package main

import (
	"context"
	"os"
	goruntime "runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/session/sessiontest"
)

// waiterCallStore counts the store calls the async start goroutine itself makes
// once runPreparedStartCandidate has returned: the waiter of the bounded commit
// (SESSION-RUNTIME-017). The commit closure runs on a goroutine of its own, and
// the start phase runs under runPreparedStartCandidate, so neither counts. The
// waiter makes none.
type waiterCallStore struct {
	beads.Store

	calls atomic.Int64
	mu    sync.Mutex
	by    []string
}

// inWaiterAfterStart reports whether the calling goroutine is an async start
// goroutine outside runPreparedStartCandidate.
func inWaiterAfterStart() bool {
	pcs := make([]uintptr, 64)
	frames := goruntime.CallersFrames(pcs[:goruntime.Callers(3, pcs)])
	inEnqueue := false
	for {
		frame, more := frames.Next()
		switch {
		case strings.Contains(frame.Function, "runPreparedStartCandidate"):
			return false
		case strings.Contains(frame.Function, "enqueuePreparedStartWaveForCity"):
			inEnqueue = true
		}
		if !more {
			return inEnqueue
		}
	}
}

func (s *waiterCallStore) note(verb string) {
	if inWaiterAfterStart() {
		s.calls.Add(1)
		s.mu.Lock()
		s.by = append(s.by, verb)
		s.mu.Unlock()
	}
}

func (s *waiterCallStore) waiterCalls() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Join(s.by, ", ")
}

func (s *waiterCallStore) hungNow() int64 {
	if r, ok := s.Store.(hungGetReporter); ok {
		return r.hungNow()
	}
	return 0
}

func (s *waiterCallStore) Get(id string) (beads.Bead, error) { s.note("Get"); return s.Store.Get(id) }
func (s *waiterCallStore) Update(id string, opts beads.UpdateOpts) error {
	s.note("Update")
	return s.Store.Update(id, opts)
}

func (s *waiterCallStore) SetMetadata(id, k, v string) error {
	s.note("SetMetadata")
	return s.Store.SetMetadata(id, k, v)
}

func (s *waiterCallStore) SetMetadataBatch(id string, kvs map[string]string) error {
	s.note("SetMetadataBatch")
	return s.Store.SetMetadataBatch(id, kvs)
}

func (s *waiterCallStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	s.note("List")
	return s.Store.List(q)
}
func (s *waiterCallStore) Close(id string) error { s.note("Close"); return s.Store.Close(id) }

// leaseFree reports whether the runtime name's flock is free, taking and
// dropping it.
func leaseFree(t *testing.T, city, name string) bool {
	t.Helper()
	_, drop, err := tryRuntimeLease(nil, city, name, "", 0)
	if err != nil {
		return false
	}
	drop()
	return true
}

// A commit that outlives the bound is neither refused nor torn down by the
// waiter. The waiter makes no store call, so a lease it dropped (a Release
// reads and writes the row) would be the very store call the bound exists to
// avoid; the lease stays with the commit that is running, and the name stays
// busy to every other holder until it returns. When the store answers, the late
// commit lands under its own lease: the row is committed, the runtime is left
// running and exactly one session.woke is recorded, then the lease is released
// and the name is takeable (ga-hlod480, SESSION-RUNTIME-017).
func TestLateHealthyCommitKeepsItsLeaseAndTheWaiterTouchesNothing(t *testing.T) {
	testCity := t.TempDir()
	workDir := t.TempDir()
	synctest.Test(t, func(t *testing.T) {
		clk := &clock.Fake{Time: time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)}
		inner, bead, item := noDriftBeadItem(t, clk, workDir)
		gate := newCommitGateStore(inner)
		probe := &waiterCallStore{Store: gate}
		sp := newPostStartProvider()
		defer sp.release()
		defer gate.release()
		begin := time.Now()

		run := enqueueCommitPhaseStartShaped(context.Background(), t, testCity, item, sp, probe, clk, immediateStartStabilityWaiter, productionLease(inner))
		run.awaitFinished(t, sp, probe, begin)

		if run.released.Load() != 1 || run.finished.Load() != 1 || run.followUps.Load() != 1 || run.inFlight() != 0 {
			t.Fatalf("at expiry: released=%d finished=%d followUps=%d inFlight=%d, want the slot, the done and the follow-up each once",
				run.released.Load(), run.finished.Load(), run.followUps.Load(), run.inFlight())
		}
		if log := run.stderr.String(); !strings.Contains(log, "commit_unavailable") || !strings.Contains(log, leaseRetainedByCommit) {
			t.Fatalf("log = %q, want commit_unavailable naming %q", log, leaseRetainedByCommit)
		}
		if n := probe.calls.Load(); n != 0 {
			t.Fatalf("the waiter made %d store calls after the start phase (%s), want 0", n, probe.waiterCalls())
		}
		name := startLeaseName(item.candidate)
		if leaseFree(t, testCity, name) {
			t.Fatalf("the name's flock was free at expiry, want it held by the commit that is still running")
		}
		parked := mustGetBead(t, inner, bead.ID)
		clk.Advance(commitBound)
		if deferred, written := recoverRunningPendingCreate(testCity, sessiontest.SeedBead(t, parked), item.candidate.tp, &config.City{}, inner, sp, clk, run.stderr, nil); !deferred || written != nil {
			t.Fatalf("recoverRunningPendingCreate = %v, %v while the commit holds the lease, want a silent deferral (true, nil)", deferred, written)
		}
		requireBeadUnchanged(t, inner, parked)

		gate.release()
		synctest.Wait()

		got := mustGetBead(t, inner, bead.ID)
		if got.Metadata["pending_create_claim"] != "" || got.Metadata["state"] != "active" {
			t.Fatalf("after the late commit: state=%q pending_create_claim=%q, want the start committed", got.Metadata["state"], got.Metadata["pending_create_claim"])
		}
		if !sp.IsRunning("worker") || sp.CountCalls("Stop", "worker") != 0 {
			t.Fatalf("runtime running=%v stops=%d after the late commit, want it left running", sp.IsRunning("worker"), sp.CountCalls("Stop", "worker"))
		}
		woke := 0
		for _, e := range run.rec.Events {
			if e.Type == events.SessionWoke {
				woke++
			}
		}
		if woke != 1 {
			t.Fatalf("%d session.woke events, want exactly one from the late commit", woke)
		}
		if strings.Contains(run.stderr.String(), "stale_async_start") {
			t.Fatalf("log = %q, want the late commit accepted, not refused as stale", run.stderr.String())
		}
		if got.Metadata[sessionpkg.RuntimeLeaseHolderKey] != "" {
			t.Fatalf("lease holder = %q after the commit returned, want the record cleared", got.Metadata[sessionpkg.RuntimeLeaseHolderKey])
		}
		if !leaseFree(t, testCity, name) {
			t.Fatalf("the name's flock is still held after the late commit returned")
		}
		if n := probe.calls.Load(); n != 0 {
			t.Fatalf("the waiter made %d store calls (%s), want 0", n, probe.waiterCalls())
		}
	})
}

// A commit that is refused (an observation of the session is outstanding) never
// ran, so the lease is the waiter's: it abandons it, dropping the flock only. The
// store may be hung or panicking and the slot, the follow-up and done() still
// complete, the row is exactly what the start phase left, and the name is
// takeable at once, without waiting for the record to lapse.
func TestRefusedCommitAbandonsTheLeaseWithoutTouchingTheStore(t *testing.T) {
	testCity := t.TempDir()
	workDir := t.TempDir()
	for _, tc := range []struct {
		name string
		wrap func(beads.Store) beads.Store
	}{
		{"wedged", func(s beads.Store) beads.Store { return s }},
		{"panicking", func(s beads.Store) beads.Store { return &panicAfterStartStore{Store: s} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				clk := &clock.Fake{Time: time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)}
				inner, bead, _, item := postStartBeadItem(t, clk, workDir)
				wedge := newWedgedStore(inner)
				wedge.snapshotID = bead.ID
				probe := &waiterCallStore{Store: tc.wrap(wedge)}
				sp := newPostStartProvider()
				defer sp.release()
				defer wedge.release()
				wedged := make(chan struct{})
				defer close(wedged)
				occupy := startStabilityWaiter(func(ctx context.Context, name string) bool {
					wctx, cancel := context.WithTimeout(context.Background(), time.Second)
					defer cancel()
					_, _ = observeSessionBounded(wctx, testCity, "worker", func() (int, error) { <-wedged; return 0, nil })
					return immediateStartStabilityWaiter(ctx, name)
				})
				begin := time.Now()

				run := enqueueCommitPhaseStartShaped(context.Background(), t, testCity, item, sp, probe, clk, occupy, func(t *testing.T, city string, item preparedStart) (preparedStart, func()) {
					item, cleanup := productionLease(wedge)(t, city, item)
					wedge.arm()
					return item, cleanup
				})
				elapsed := run.awaitFinished(t, sp, probe, begin)

				if elapsed >= commitBound {
					t.Fatalf("the async start took %v, want the refusal at once", elapsed)
				}
				if run.released.Load() != 1 || run.finished.Load() != 1 || run.followUps.Load() != 1 || run.inFlight() != 0 {
					t.Fatalf("released=%d finished=%d followUps=%d inFlight=%d, want each once and the slot back", run.released.Load(), run.finished.Load(), run.followUps.Load(), run.inFlight())
				}
				if log := run.stderr.String(); !strings.Contains(log, "commit_unavailable") || !strings.Contains(log, leaseAbandoned) {
					t.Fatalf("log = %q, want commit_unavailable naming %q", log, leaseAbandoned)
				}
				if n := probe.calls.Load(); n != 0 {
					t.Fatalf("the waiter made %d store calls (%s), want 0", n, probe.waiterCalls())
				}
				requireBeadUnchanged(t, inner, wedge.atArm())
				name := startLeaseName(item.candidate)
				if !leaseFree(t, testCity, name) {
					t.Fatalf("the name's flock is held after the refused commit, want it abandoned at once")
				}
				// The record is left to lapse; the next acquirer of the row's lease
				// proves the flock it names is gone and takes it without waiting
				// for the record to expire, where the host can prove it.
				if _, err := os.Stat("/proc/sys/kernel/random/boot_id"); err == nil {
					next, drop, err := tryRuntimeLease(inner, testCity, name, bead.ID, sessionpkg.RuntimeLeaseTTL(postStartTimeout))
					if err != nil {
						t.Fatalf("taking the row's lease after the abandon: %v, want it takeable at once", err)
					}
					drop()
					_ = next
				}
			})
		})
	}
}

// panicAfterStartStore panics on any call made by the start goroutine after the
// start phase, so a waiter that touches the store fails loudly where a hung
// store would only stall it.
type panicAfterStartStore struct{ beads.Store }

func (s *panicAfterStartStore) Get(id string) (beads.Bead, error) {
	if inWaiterAfterStart() {
		panic("the waiter read the store after the start phase")
	}
	return s.Store.Get(id)
}

func (s *panicAfterStartStore) SetMetadataBatch(id string, kvs map[string]string) error {
	if inWaiterAfterStart() {
		panic("the waiter wrote the store after the start phase")
	}
	return s.Store.SetMetadataBatch(id, kvs)
}

// hangingWatchStore hangs the reads a lease Watch makes, as a stalled store does.
type hangingWatchStore struct {
	beads.Store
	release  chan struct{}
	hanging  atomic.Int64
	released sync.Once
}

func inLeaseWatch() bool {
	pcs := make([]uintptr, 32)
	frames := goruntime.CallersFrames(pcs[:goruntime.Callers(3, pcs)])
	for {
		frame, more := frames.Next()
		if strings.Contains(frame.Function, "RuntimeLease).Watch") {
			return true
		}
		if !more {
			return false
		}
	}
}

func (s *hangingWatchStore) Get(id string) (beads.Bead, error) {
	if inLeaseWatch() {
		s.hanging.Add(1)
		defer s.hanging.Add(-1)
		<-s.release
	}
	return s.Store.Get(id)
}

func (s *hangingWatchStore) unblock() { s.released.Do(func() { close(s.release) }) }

// The lease watch of a start ends when the commit takes over, not on the waiter:
// stopping it waits for a read in flight, and a lease read that hangs on the store
// must not hold the slot (ga-hlod480, T-E).
func TestHungLeaseWatchReadDoesNotHoldTheSlot(t *testing.T) {
	prev := startLeaseWatchEvery
	startLeaseWatchEvery = time.Second
	t.Cleanup(func() { startLeaseWatchEvery = prev })
	testCity := t.TempDir()
	workDir := t.TempDir()
	synctest.Test(t, func(t *testing.T) {
		clk := &clock.Fake{Time: time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)}
		inner, _, item := noDriftBeadItem(t, clk, workDir)
		hang := &hangingWatchStore{Store: inner, release: make(chan struct{})}
		probe := &waiterCallStore{Store: inner}
		sp := newPostStartProvider()
		defer sp.release()
		defer hang.unblock()
		// The provider start outlasts one watch interval, so a read is in flight
		// when the start ends.
		slow := startStabilityWaiter(func(ctx context.Context, name string) bool {
			select {
			case <-time.After(3 * time.Second):
			case <-ctx.Done():
			}
			return immediateStartStabilityWaiter(ctx, name)
		})
		begin := time.Now()

		run := enqueueCommitPhaseStartShaped(context.Background(), t, testCity, item, sp, probe, clk, slow, productionLease(hang))
		elapsed := run.awaitFinished(t, sp, probe, begin)

		if hang.hanging.Load() == 0 {
			t.Fatalf("no lease watch read hung: the scenario no longer reaches the watch")
		}
		if elapsed > 3*time.Second+commitBound+time.Second {
			t.Fatalf("the async start finished %v in, want the slot back within the %v bound of the commit although a lease watch read is hung", elapsed, commitBound)
		}
		if run.released.Load() != 1 || run.followUps.Load() != 1 || run.inFlight() != 0 {
			t.Fatalf("released=%d followUps=%d inFlight=%d, want the slot back and the follow-up run", run.released.Load(), run.followUps.Load(), run.inFlight())
		}
		hang.unblock()
		synctest.Wait()
	})
}

// commitHandoff has one winner. The waiter that abandons first leaves a
// closure that finds it lost, whatever the order the two arrive in.
func TestCommitHandoffHasExactlyOneWinner(t *testing.T) {
	city := t.TempDir()
	newLease := func(t *testing.T) *sessionpkg.RuntimeLease {
		l, drop, err := tryRuntimeLease(nil, city, "worker", "", 0)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(drop)
		return l
	}
	t.Run("closure first", func(t *testing.T) {
		var h commitHandoff
		l := newLease(t)
		if !h.claim() {
			t.Fatal("the closure lost an uncontested claim")
		}
		if h.abandon(l) {
			t.Fatal("the waiter abandoned a lease the commit owns")
		}
		if leaseFree(t, city, "worker") {
			t.Fatal("the flock was dropped from under the running commit")
		}
	})
	t.Run("waiter first", func(t *testing.T) {
		var h commitHandoff
		l := newLease(t)
		if !h.abandon(l) {
			t.Fatal("the waiter lost an uncontested abandon")
		}
		if h.claim() {
			t.Fatal("the closure claimed a lease the waiter abandoned")
		}
		if !leaseFree(t, city, "worker") {
			t.Fatal("the flock is still held after the abandon")
		}
	})
	t.Run("raced", func(t *testing.T) {
		for i := 0; i < 200; i++ {
			var h commitHandoff
			l := newLease(t)
			var claimed, abandoned bool
			var wg sync.WaitGroup
			wg.Add(2)
			go func() { defer wg.Done(); claimed = h.claim() }()
			go func() { defer wg.Done(); abandoned = h.abandon(l) }()
			wg.Wait()
			if claimed == abandoned {
				t.Fatalf("iteration %d: claim=%v abandon=%v, want exactly one winner", i, claimed, abandoned)
			}
			l.Release()
		}
	})
	t.Run("a nil lease", func(t *testing.T) {
		var h commitHandoff
		if !h.abandon(nil) || h.claim() {
			t.Fatal("a start with no lease still hands off once")
		}
	})
}

// The hand-off, not the lease, keeps a closure that starts after the waiter gave
// up from committing: Abandon leaves the lease's record, which still names the
// lease (HoldsMeta ignores the released flag), so a late closure that did not
// check would commit. It checks, commits nothing and leaves the row as it was.
func TestLosingClosureCommitsNothingAgainstAnAbandonedRecordBearingLease(t *testing.T) {
	testCity := t.TempDir()
	workDir := t.TempDir()
	synctest.Test(t, func(t *testing.T) {
		clk := &clock.Fake{Time: time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)}
		inner, bead, item := noDriftBeadItem(t, clk, workDir)
		item, cleanup := productionLease(inner)(t, testCity, item)
		defer cleanup()
		lease := item.candidate.lease
		held := mustGetBead(t, inner, bead.ID)
		if !lease.HoldsMeta(held.Metadata) {
			t.Fatal("the production lease does not record itself on the row")
		}
		sp := newPostStartProvider()
		defer sp.release()
		committed := 0
		commit := func() bool {
			committed++
			return commitAsyncStartResultWithContext(context.Background(), startResult{prepared: item, started: time.Now(), finished: time.Now()}, sp, inner, clk, events.Discard, 1, ioDiscard{}, ioDiscard{}, nil)
		}

		var h commitHandoff
		if !h.abandon(lease) {
			t.Fatal("the waiter lost an uncontested abandon")
		}
		after := mustGetBead(t, inner, bead.ID)
		if !lease.HoldsMeta(after.Metadata) {
			t.Fatal("HoldsMeta is false after Abandon: the record no longer proves the CAS necessary")
		}
		got, err := runHandedOffCommit(&h, lease, func() { t.Error("a losing closure ended the watch") }, commit)
		if got || err != nil || committed != 0 {
			t.Fatalf("a losing closure returned (%v, %v) and ran the commit %d times, want (false, nil) and 0", got, err, committed)
		}
		requireBeadUnchanged(t, inner, held)
	})
}

// A closure that wins the hand-off releases its lease on every exit, a panic
// included, and the panic still reaches the waiter's caller.
func TestWinningClosureReleasesTheLeaseWhenTheCommitPanics(t *testing.T) {
	city := t.TempDir()
	lease, drop, err := tryRuntimeLease(nil, city, "worker", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer drop()
	var h commitHandoff
	func() {
		defer func() {
			if recover() == nil {
				t.Error("the commit's panic was swallowed")
			}
		}()
		_, _ = runHandedOffCommit(&h, lease, nil, func() bool { panic("commit failed") })
	}()
	if !leaseFree(t, city, "worker") {
		t.Fatal("the flock is still held after the commit panicked")
	}
	// The unwind of the waiter finds the lease the commit owned and touches nothing.
	if h.abandon(lease) {
		t.Fatal("the waiter's unwind abandoned a lease the commit owned")
	}
}
