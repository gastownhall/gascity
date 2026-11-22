package main

import (
	"context"
	goruntime "runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worker"
)

// syncCommitGateStore is a beads.Store whose calls on the gated rows hang, but
// only while the caller is inside commitStartResultTraced: the sync start phase
// and the lease's own release read and write the store normally, so a scenario
// wedges exactly the per-result commit writes of the synchronous path.
type syncCommitGateStore struct {
	beads.Store

	mu    sync.Mutex
	gated map[string]bool
	// atHang is each gated row as it was when its first commit call hung.
	atHang map[string]beads.Bead

	hung     atomic.Int64
	releaseC chan struct{}
	once     sync.Once
}

func newSyncCommitGateStore(inner beads.Store) *syncCommitGateStore {
	return &syncCommitGateStore{Store: inner, gated: map[string]bool{}, atHang: map[string]beads.Bead{}, releaseC: make(chan struct{})}
}

// gate makes the commit calls on row id hang until release.
func (s *syncCommitGateStore) gate(id string) {
	s.mu.Lock()
	s.gated[id] = true
	s.mu.Unlock()
}

// release lets every hung call return. It is idempotent.
func (s *syncCommitGateStore) release() { s.once.Do(func() { close(s.releaseC) }) }

// inSyncCommit reports whether the calling goroutine is inside the per-result
// commit of the synchronous path.
func inSyncCommit() bool {
	pcs := make([]uintptr, 64)
	frames := goruntime.CallersFrames(pcs[:goruntime.Callers(3, pcs)])
	for {
		frame, more := frames.Next()
		if strings.HasSuffix(frame.Function, ".commitStartResultTraced") {
			return true
		}
		if !more {
			return false
		}
	}
}

func (s *syncCommitGateStore) stall(id string) {
	s.mu.Lock()
	gated := s.gated[id]
	s.mu.Unlock()
	if !gated || !inSyncCommit() {
		return
	}
	s.mu.Lock()
	if _, ok := s.atHang[id]; !ok {
		s.atHang[id], _ = s.Store.Get(id)
	}
	s.mu.Unlock()
	s.hung.Add(1)
	<-s.releaseC
}

func (s *syncCommitGateStore) Get(id string) (beads.Bead, error) {
	s.stall(id)
	return s.Store.Get(id)
}

func (s *syncCommitGateStore) Update(id string, opts beads.UpdateOpts) error {
	s.stall(id)
	return s.Store.Update(id, opts)
}

func (s *syncCommitGateStore) SetMetadata(id, key, value string) error {
	s.stall(id)
	return s.Store.SetMetadata(id, key, value)
}

func (s *syncCommitGateStore) SetMetadataBatch(id string, kvs map[string]string) error {
	s.stall(id)
	return s.Store.SetMetadataBatch(id, kvs)
}

// syncPlan runs the synchronous planner over cands on a goroutine of the
// bubble and fails, rather than hangs, if it has not returned by the watchdog.
// It returns the wake count and the bubble time the pass took.
func syncPlan(t *testing.T, e *capacityEnv, store beads.Store, cands ...startCandidate) (int, time.Duration) {
	t.Helper()
	begin := time.Now()
	woken := make(chan int, 1)
	go func() {
		woken <- executePlannedStartsTraced(context.Background(), cands, e.cfg, e.desiredState, e.sp, store, "", e.city, e.clk, e.rec, 0, &e.log, &e.log, nil, e.startOptions...)
	}()
	select {
	case n := <-woken:
		return n, time.Since(begin)
	case <-time.After(commitWatchdog):
		t.Fatalf("the synchronous planner had not returned %v in: its per-result commit waits on a store nothing can cancel; log: %s", commitWatchdog, e.log.String())
		return 0, 0
	}
}

// A commit that hangs on the sync path must not hold the pass: the reconcile
// returns at the commit bound, the hung result counts no wake, writes no
// rollback, leaves the pending-start lease alone and logs commit_unavailable,
// and the next result in the same pass is still committed (SESSION-RUNTIME-035).
func TestExecutePlannedStartsSyncReturnsAtTheBoundWhenACommitHangsAndCommitsTheNextResult(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newCapacityEnv(t, false, "s1", "s2")
		c1 := e.pendingCreate(t, "s1", "e", nil)
		c2 := e.pendingCreate(t, "s2", "e", nil)
		store := newSyncCommitGateStore(e.store)
		store.gate(c1.info.ID)
		defer store.release()

		woken, elapsed := syncPlan(t, e, store, c1, c2)

		if store.hung.Load() == 0 {
			t.Fatalf("no commit call hung: the scenario no longer reaches the sync commit; log: %s", e.log.String())
		}
		if elapsed < commitBound || elapsed > commitBound+time.Second {
			t.Fatalf("the pass returned %v in, want it to give up the hung commit at the %v bound", elapsed, commitBound)
		}
		if woken != 1 {
			t.Fatalf("woken = %d, want 1: the hung result counts no wake and the next result still commits; log: %s", woken, e.log.String())
		}
		log := e.log.String()
		if !strings.Contains(log, "outcome=commit_unavailable") || !strings.Contains(log, "session=s1") {
			t.Fatalf("log = %q, want a commit_unavailable lifecycle line naming s1", log)
		}
		if got := e.sp.CountCalls("Stop", "s1"); got != 0 {
			t.Fatalf("Stop calls for s1 = %d, want 0: an unavailable commit rolls nothing back", got)
		}
		s1 := mustGetBead(t, e.store, c1.info.ID).Metadata
		if s1["pending_create_claim"] != "true" || s1["last_woke_at"] == "" {
			t.Fatalf("s1 pending_create_claim=%q last_woke_at=%q, want the claim and the pending-start lease left as they were", s1["pending_create_claim"], s1["last_woke_at"])
		}
		if s2 := mustGetBead(t, e.store, c2.info.ID).Metadata; s2["pending_create_claim"] == "true" {
			t.Fatalf("s2 pending_create_claim = true, want the next result committed after the unavailable one")
		}
	})
}

// A late commit of the abandoned result finishes on its own copy after the pass
// has gone on: with the gate released the closure runs to its end while the log
// and the store are shared, which -race must find free of unsynchronized state
// the waiter also touches.
func TestExecutePlannedStartsSyncLateCommitAfterExpiryRunsOnItsOwnCopy(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newCapacityEnv(t, false, "s1", "s2")
		c1 := e.pendingCreate(t, "s1", "e", nil)
		c2 := e.pendingCreate(t, "s2", "e", nil)
		store := newSyncCommitGateStore(e.store)
		store.gate(c1.info.ID)
		defer store.release()

		syncPlan(t, e, store, c1, c2)
		store.release()
		synctest.Wait()
		_ = e.log.String()
		if !strings.Contains(e.log.String(), "outcome=commit_unavailable") {
			t.Fatalf("log = %q, want the commit_unavailable line", e.log.String())
		}
	})
}

// A commit that answers inside the bound behaves as it does unbounded: every
// result commits, each counts its wake, and nothing is logged as unavailable.
func TestExecutePlannedStartsSyncHealthyCommitsAreUnchanged(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newCapacityEnv(t, false, "s1", "s2")
		c1 := e.pendingCreate(t, "s1", "e", nil)
		c2 := e.pendingCreate(t, "s2", "e", nil)

		woken, elapsed := syncPlan(t, e, e.store, c1, c2)

		if woken != 2 {
			t.Fatalf("woken = %d, want 2; log: %s", woken, e.log.String())
		}
		if elapsed >= commitBound {
			t.Fatalf("a healthy pass took %v, want well under the %v bound", elapsed, commitBound)
		}
		if strings.Contains(e.log.String(), "commit_unavailable") {
			t.Fatalf("log = %q, want no commit_unavailable for a healthy commit", e.log.String())
		}
		for _, c := range []startCandidate{c1, c2} {
			if got := mustGetBead(t, e.store, c.info.ID).Metadata["pending_create_claim"]; got == "true" {
				t.Fatalf("%s pending_create_claim = true, want the start committed", c.tp.SessionName)
			}
		}
	})
}

func syncCommitTestResult(name string) startResult {
	cand := startCandidate{info: sessionpkg.Info{SessionNameMetadata: name}}
	cand.tp.TemplateName = "tpl"
	return startResult{prepared: preparedStart{candidate: cand}, started: time.Now(), finished: time.Now()}
}

// The helper hands back the commit's own verdict, succeeded or stale, when the
// commit answers, and the commit works on a copy: what it does to its result
// is not visible to the caller.
func TestCommitSyncStartBoundedReturnsTheCommitVerdictOnACopy(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for _, want := range []startCommitVerdict{startCommitSucceeded, startCommitStale, startCommitFailed} {
			result := syncCommitTestResult("s")
			var log lockedLog
			got, answered := commitSyncStartBounded(context.Background(), t.TempDir(), result, 1, &log, func(r startResult) startCommitVerdict {
				r.outcome = TraceOutcomeDeferred
				return want
			})
			if !answered || got != want {
				t.Fatalf("verdict = %v answered = %v, want %v answered", got, answered, want)
			}
			if result.outcome == TraceOutcomeDeferred {
				t.Fatal("the commit's write to its result is visible to the caller: it must run on a copy")
			}
			if log.String() != "" {
				t.Fatalf("log = %q, want nothing for an answered commit", log.String())
			}
		}
	})
}

// The key is the async path's: while an observation of the same session in the
// same city is outstanding the commit is refused without running and the call
// returns at once; another session is unaffected.
func TestCommitSyncStartBoundedSharesTheObservationKeyWithTheAsyncPath(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		city := t.TempDir()
		hold := make(chan struct{})
		defer close(hold)
		started := make(chan struct{})
		go func() {
			_, _ = worker.ObserveBounded(context.Background(), postStartObservationKey(city, "s"), func(context.Context) (struct{}, error) {
				close(started)
				<-hold
				return struct{}{}, nil
			})
		}()
		<-started

		var ran atomic.Int64
		var log lockedLog
		begin := time.Now()
		_, answered := commitSyncStartBounded(context.Background(), city, syncCommitTestResult("s"), 1, &log, func(startResult) startCommitVerdict {
			ran.Add(1)
			return startCommitSucceeded
		})
		if answered || ran.Load() != 0 {
			t.Fatalf("answered = %v ran = %d, want the commit refused without running", answered, ran.Load())
		}
		if took := time.Since(begin); took != 0 {
			t.Fatalf("refusal took %v, want it at once", took)
		}
		if !strings.Contains(log.String(), "commit_unavailable") {
			t.Fatalf("log = %q, want commit_unavailable", log.String())
		}

		got, answered := commitSyncStartBounded(context.Background(), city, syncCommitTestResult("other"), 1, &lockedLog{}, func(startResult) startCommitVerdict {
			ran.Add(1)
			return startCommitSucceeded
		})
		if !answered || got != startCommitSucceeded || ran.Load() != 1 {
			t.Fatalf("other session: verdict %v answered %v ran %d, want it committed", got, answered, ran.Load())
		}
	})
}

// A nil context is treated as Background: no panic, and the bound still holds.
func TestCommitSyncStartBoundedToleratesANilContext(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var ctx context.Context
		got, answered := commitSyncStartBounded(ctx, t.TempDir(), syncCommitTestResult("s"), 1, &lockedLog{}, func(startResult) startCommitVerdict {
			return startCommitSucceeded
		})
		if !answered || got != startCommitSucceeded {
			t.Fatalf("verdict %v answered %v, want the commit to answer under a nil ctx", got, answered)
		}
	})
}

// Cancellation is not expiry: the wait ends on the bound only, so a commit that
// outlives a canceled ctx is still waited for, as on the async path.
func TestCommitSyncStartBoundedDoesNotEndOnACanceledContext(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		got, answered := commitSyncStartBounded(ctx, t.TempDir(), syncCommitTestResult("s"), 1, &lockedLog{}, func(startResult) startCommitVerdict {
			return startCommitSucceeded
		})
		if !answered || got != startCommitSucceeded {
			t.Fatalf("verdict %v answered %v, want a canceled ctx not to abandon the commit", got, answered)
		}
	})
}

// A commit that does not answer inside the bound is given up on at the bound,
// with commit_unavailable logged and the caller's result untouched.
func TestCommitSyncStartBoundedGivesUpAtTheBound(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		defer close(release)
		var log lockedLog
		begin := time.Now()
		_, answered := commitSyncStartBounded(context.Background(), t.TempDir(), syncCommitTestResult("s"), 1, &log, func(startResult) startCommitVerdict {
			<-release
			return startCommitSucceeded
		})
		if answered {
			t.Fatal("answered = true for a commit that never returned")
		}
		if took := time.Since(begin); took != commitBound {
			t.Fatalf("gave up after %v, want exactly the %v bound", took, commitBound)
		}
		if !strings.Contains(log.String(), "outcome=commit_unavailable") {
			t.Fatalf("log = %q, want commit_unavailable", log.String())
		}
	})
}
