package session

import (
	"context"
	"errors"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// lockWait is the wait the scenarios below hand the acquire. It defines the
// scenario (how long the acquirer is willing to queue); it is not a hang
// budget, and the synctest bubbles that use it spend no wall time on it.
const lockWait = 30 * time.Second

// sessionMutationLockRefs reports the bookkeeping entry for id: its refcount
// and whether it is in the map at all.
func sessionMutationLockRefs(id string) (refs int, present bool) {
	sessionMutationLocksMu.Lock()
	defer sessionMutationLocksMu.Unlock()
	entry := sessionMutationLocks[id]
	if entry == nil {
		return 0, false
	}
	return entry.refs, true
}

// sessionMutationLockEntryHeld reports whether entry's lock is held, by trying
// to take it and giving it straight back when that succeeds.
func sessionMutationLockEntryHeld(entry *sessionMutationLockEntry) bool {
	select {
	case entry.sem <- struct{}{}:
		<-entry.sem
		return false
	default:
		return true
	}
}

// holdSessionMutationLock takes the lock for id on a goroutine that parks on a
// channel (durably blocked, unlike a sync.Mutex wait) and returns once it is
// held. The returned func releases it and waits for the goroutine to finish.
func holdSessionMutationLock(t *testing.T, id string) (release func()) {
	t.Helper()
	held := make(chan struct{})
	gate := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = WithSessionMutationLock(id, func() error {
			close(held)
			<-gate
			return nil
		})
	}()
	<-held
	var once sync.Once
	return func() {
		once.Do(func() { close(gate) })
		<-done
	}
}

func TestWithSessionMutationLockContextUncontended(t *testing.T) {
	const id = "lock-uncontended"
	ran := false
	err := WithSessionMutationLockContext(context.Background(), id, func() error {
		ran = true
		if refs, present := sessionMutationLockRefs(id); !present || refs != 1 {
			t.Errorf("inside fn: refs=%d present=%v, want 1 and present", refs, present)
		}
		return nil
	})
	if err != nil || !ran {
		t.Fatalf("WithSessionMutationLockContext = %v, ran=%v; want nil and ran", err, ran)
	}
	if _, present := sessionMutationLockRefs(id); present {
		t.Fatal("lock entry survived a released, uncontended acquire")
	}
}

func TestWithSessionMutationLockContextReturnsFnError(t *testing.T) {
	const id = "lock-fn-error"
	want := errors.New("fn failed")
	if got := WithSessionMutationLockContext(context.Background(), id, func() error { return want }); !errors.Is(got, want) {
		t.Fatalf("error = %v, want fn's error %v", got, want)
	}
	if _, present := sessionMutationLockRefs(id); present {
		t.Fatal("lock entry survived a failing fn")
	}
}

func TestWithSessionMutationLockContextBusyAtDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const id = "lock-busy"
		release := holdSessionMutationLock(t, id)

		ctx, cancel := context.WithTimeout(context.Background(), lockWait)
		defer cancel()
		var ran atomic.Bool
		start := time.Now()
		result := make(chan error, 1)
		go func() {
			result <- WithSessionMutationLockContext(ctx, id, func() error {
				ran.Store(true)
				return nil
			})
		}()

		synctest.Wait()
		if refs, _ := sessionMutationLockRefs(id); refs != 2 {
			t.Fatalf("refs while the acquirer waits = %d, want 2 (holder + waiter)", refs)
		}
		var err error
		select {
		case err = <-result:
		case <-time.After(2 * lockWait):
			t.Fatal("acquire still waiting after twice the wait; ctx does not bound it")
		}
		if !errors.Is(err, ErrSessionMutationBusy) || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("error = %v, want ErrSessionMutationBusy wrapping context.DeadlineExceeded", err)
		}
		if got := time.Since(start); got != lockWait {
			t.Fatalf("busy after %v, want exactly the %v wait", got, lockWait)
		}
		if ran.Load() {
			t.Fatal("fn ran although the acquire returned busy")
		}
		if refs, present := sessionMutationLockRefs(id); !present || refs != 1 {
			t.Fatalf("after busy: refs=%d present=%v, want the holder's 1 and present", refs, present)
		}

		release()
		if _, present := sessionMutationLockRefs(id); present {
			t.Fatal("lock entry survived the holder's release")
		}
	})
}

func TestWithSessionMutationLockContextAbandonedWaiterNeverRuns(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const id = "lock-abandoned"
		release := holdSessionMutationLock(t, id)

		ctx, cancel := context.WithTimeout(context.Background(), lockWait)
		defer cancel()
		var ran atomic.Bool
		result := make(chan error, 1)
		go func() {
			result <- WithSessionMutationLockContext(ctx, id, func() error {
				ran.Store(true)
				return nil
			})
		}()
		if err := <-result; !errors.Is(err, ErrSessionMutationBusy) {
			t.Fatalf("error = %v, want ErrSessionMutationBusy", err)
		}

		release()
		synctest.Wait()
		if ran.Load() {
			t.Fatal("an abandoned waiter ran fn after the holder released")
		}
		freshRan := false
		fresh, freshCancel := context.WithTimeout(context.Background(), lockWait)
		defer freshCancel()
		if err := WithSessionMutationLockContext(fresh, id, func() error { freshRan = true; return nil }); err != nil || !freshRan {
			t.Fatalf("fresh acquire = %v, ran=%v; want it to succeed at once (the abandoned wait must not leave the lock held)", err, freshRan)
		}
		if _, present := sessionMutationLockRefs(id); present {
			t.Fatal("lock entry survived once every user released")
		}
	})
}

// doneCtxAcquireIterations repeats the done-context acquire because the
// select that follows the try-first send picks at random among ready cases:
// without the try-first send a done context loses about half the time, so a
// single shot would let a mutant that drops it survive.
const doneCtxAcquireIterations = 64

func TestWithSessionMutationLockContextDoneContextStillAcquiresFreeLock(t *testing.T) {
	const id = "lock-done-ctx"
	for i := 0; i < doneCtxAcquireIterations; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		ran := false
		if err := WithSessionMutationLockContext(ctx, id, func() error { ran = true; return nil }); err != nil || !ran {
			t.Fatalf("iteration %d: done ctx on a free lock = %v, ran=%v; want it to acquire (ctx bounds only the wait)", i, err, ran)
		}
		if _, present := sessionMutationLockRefs(id); present {
			t.Fatalf("iteration %d: lock entry survived the acquire", i)
		}
	}
}

func TestWithSessionMutationLockContextDoneContextBusyWhenHeld(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const id = "lock-done-held"
		release := holdSessionMutationLock(t, id)
		defer release()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		ran := false
		err := WithSessionMutationLockContext(ctx, id, func() error { ran = true; return nil })
		if !errors.Is(err, ErrSessionMutationBusy) || !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want ErrSessionMutationBusy wrapping context.Canceled", err)
		}
		if ran {
			t.Fatal("fn ran although the lock was held")
		}
	})
}

func TestWithSessionMutationLockContextFIFO(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const id = "lock-fifo"
		release := holdSessionMutationLock(t, id)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var (
			mu    sync.Mutex
			order []string
			wg    sync.WaitGroup
		)
		wait := func(name string) {
			wg.Add(1)
			go func() {
				defer wg.Done()
				err := WithSessionMutationLockContext(ctx, id, func() error {
					mu.Lock()
					order = append(order, name)
					mu.Unlock()
					return nil
				})
				if err != nil {
					t.Errorf("waiter %s: %v", name, err)
				}
			}()
			// Both waiters are durably blocked on the lock's channel once
			// Wait returns, so the second queues strictly after the first.
			synctest.Wait()
		}
		wait("first")
		wait("second")

		release()
		wg.Wait()
		if len(order) != 2 || order[0] != "first" || order[1] != "second" {
			t.Fatalf("acquire order = %v, want [first second]", order)
		}
		if _, present := sessionMutationLockRefs(id); present {
			t.Fatal("lock entry survived once every user released")
		}
	})
}

func TestWithSessionMutationLockContextStress(t *testing.T) {
	const (
		id         = "lock-stress"
		goroutines = 64
		attempts   = 200
	)
	var (
		inside   atomic.Int32
		overlaps atomic.Int32
		ran      atomic.Int32
		busy     atomic.Int32
		wg       sync.WaitGroup
	)
	for g := range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rng := rand.New(rand.NewPCG(uint64(g), 1))
			for range attempts {
				ctx, cancel := context.WithTimeout(context.Background(), time.Duration(rng.IntN(200))*time.Microsecond)
				err := WithSessionMutationLockContext(ctx, id, func() error {
					if inside.Add(1) != 1 {
						overlaps.Add(1)
					}
					ran.Add(1)
					inside.Add(-1)
					return nil
				})
				cancel()
				switch {
				case err == nil:
				case errors.Is(err, ErrSessionMutationBusy):
					busy.Add(1)
				default:
					t.Errorf("unexpected error: %v", err)
				}
			}
		}()
	}
	wg.Wait()
	if overlaps.Load() != 0 {
		t.Fatalf("%d fn runs overlapped another; the lock is not exclusive", overlaps.Load())
	}
	if got := ran.Load() + busy.Load(); got != goroutines*attempts {
		t.Fatalf("ran+busy = %d, want %d: every attempt either runs fn or reports busy", got, goroutines*attempts)
	}
	if _, present := sessionMutationLockRefs(id); present {
		t.Fatal("lock entry leaked after the stress run")
	}
}

// busyStoreFixture opens a pending-create row on a backend that provides the
// atomic close (RollbackPendingCreateAtomically refuses without one before it
// ever reaches the lock), and returns the Store, the raw store and the row's
// Info. It opens outside any synctest bubble.
func busyStoreFixture(t *testing.T) (*Store, beads.Store, Info) {
	t.Helper()
	backing, _ := fencedAtomicCloseBackends(t)[0].open(t)
	_, info := seedRollbackPendingSession(t, backing)
	return NewStore(beads.SessionStore{Store: backing}), backing, info
}

func TestStoreLockedMethodsReturnBusyWithoutWriting(t *testing.T) {
	type op struct {
		name string
		call func(ctx context.Context, front *Store, info Info) error
	}
	ops := []op{
		{"CommitStartedIfCurrent", func(ctx context.Context, front *Store, info Info) error {
			_, err := front.CommitStartedIfCurrent(ctx, info, MetadataPatch{"state": "active"})
			return err
		}},
		{"WithPendingCreateRollback", func(ctx context.Context, front *Store, info Info) error {
			_, err := front.WithPendingCreateRollback(ctx, info, func() error {
				t.Error("rollback fn ran although the lock was busy")
				return nil
			})
			return err
		}},
		{"RollbackPendingCreateAtomically", func(ctx context.Context, front *Store, info Info) error {
			_, _, err := front.RollbackPendingCreateAtomically(ctx, info, rollbackClosePatch(), nil)
			return err
		}},
	}
	for _, tc := range ops {
		t.Run(tc.name, func(t *testing.T) {
			front, backing, info := busyStoreFixture(t)
			synctest.Test(t, func(t *testing.T) {
				release := holdSessionMutationLock(t, info.ID)
				defer release()
				ctx, cancel := context.WithTimeout(context.Background(), lockWait)
				defer cancel()

				err := tc.call(ctx, front, info)
				if !errors.Is(err, ErrSessionMutationBusy) || !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("%s = %v, want ErrSessionMutationBusy wrapping context.DeadlineExceeded", tc.name, err)
				}
			})
			got, err := backing.Get(info.ID)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if got.Status != "open" || got.Metadata["state"] != string(StateCreating) || got.Metadata["pending_create_claim"] != "true" {
				t.Fatalf("row after busy = status %q state %q claim %q, want it untouched (open, creating, claimed)",
					got.Status, got.Metadata["state"], got.Metadata["pending_create_claim"])
			}
		})
	}
}
