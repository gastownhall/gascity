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
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/resilience"
	"github.com/gastownhall/gascity/internal/runtime"
)

// wedgedStore is a beads.Store that stalls the way a wedged Dolt or bd does:
// once armed, every read and write the commit and settle phase makes, and every
// one the release of the start's lease makes, hangs until release. The start
// phase is left alone so a scenario wedges what comes after provider.Start and
// nothing earlier. Hanging the lease release as well as the commit is what a
// commit-only gate cannot show.
type wedgedStore struct {
	beads.Store

	armed atomic.Bool

	// hung counts the calls that ever hung, hanging the ones hung right now.
	hung    atomic.Int64
	hanging atomic.Int64

	mu     sync.Mutex
	hungBy []string
	// snapshotID names the row whose state arm keeps; snapshot is that state.
	snapshotID string
	snapshot   beads.Bead

	releaseC    chan struct{}
	releaseOnce sync.Once
}

func newWedgedStore(inner beads.Store) *wedgedStore {
	return &wedgedStore{Store: inner, releaseC: make(chan struct{})}
}

// arm starts hanging the callers after the start phase, and keeps the row as
// it is at that moment.
func (s *wedgedStore) arm() {
	s.mu.Lock()
	if s.snapshotID != "" {
		s.snapshot, _ = s.Store.Get(s.snapshotID)
	}
	s.mu.Unlock()
	s.armed.Store(true)
}

// release lets every hung call return. It is idempotent so a scenario can call
// it and defer it.
func (s *wedgedStore) release() { s.releaseOnce.Do(func() { close(s.releaseC) }) }

func (s *wedgedStore) hungNow() int64 { return s.hanging.Load() }

// atArm is the row as it was when the store was armed.
func (s *wedgedStore) atArm() beads.Bead {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snapshot
}

// callers names what has hung on the store, for failure messages.
func (s *wedgedStore) callers() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Join(s.hungBy, ", ")
}

// inCommitOrRelease reports whether the calling goroutine is the commit and
// settle phase or the release of the start's lease, the two things that run
// after the start phase. The start phase's own reads, which run on goroutines
// of their own, are not.
func inCommitOrRelease() bool {
	pcs := make([]uintptr, 64)
	frames := goruntime.CallersFrames(pcs[:goruntime.Callers(3, pcs)])
	for {
		frame, more := frames.Next()
		if strings.HasSuffix(frame.Function, ".commitAsyncStartResultWithContext") || strings.Contains(frame.Function, "RuntimeLease).release") {
			return true
		}
		if !more {
			return false
		}
	}
}

// stall hangs the caller when the store is wedged for it.
func (s *wedgedStore) stall(verb string) {
	if !s.armed.Load() || !inCommitOrRelease() {
		return
	}
	s.hung.Add(1)
	s.hanging.Add(1)
	defer s.hanging.Add(-1)
	s.mu.Lock()
	s.hungBy = append(s.hungBy, verb)
	s.mu.Unlock()
	<-s.releaseC
}

func (s *wedgedStore) Get(id string) (beads.Bead, error) {
	s.stall("Get")
	return s.Store.Get(id)
}

func (s *wedgedStore) Update(id string, opts beads.UpdateOpts) error {
	s.stall("Update")
	return s.Store.Update(id, opts)
}

func (s *wedgedStore) SetMetadata(id, key, value string) error {
	s.stall("SetMetadata")
	return s.Store.SetMetadata(id, key, value)
}

func (s *wedgedStore) SetMetadataBatch(id string, kvs map[string]string) error {
	s.stall("SetMetadataBatch")
	return s.Store.SetMetadataBatch(id, kvs)
}

// Under the lease production hands an async start, releasing the lease reads the
// row live and writes the fenced clear of the record before it drops the flock
// (RuntimeLease.Release). When the store is wedged for everyone, as a stalled
// Dolt is, that release hangs too, so a waiter that runs it before freeing the
// slot pins the slot behind the very store the commit bound gave up on. The
// abandon path (SESSION-RUNTIME-017) must free the slot, run the follow-up and
// finish without any store I/O of its own.
func TestAbandonedCommitFreesTheSlotUnderTheProductionLeaseWhenTheStoreIsWedged(t *testing.T) {
	testCity := t.TempDir()
	workDir := t.TempDir()
	synctest.Test(t, func(t *testing.T) {
		clk := &clock.Fake{Time: time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)}
		inner, bead, _, item := postStartBeadItem(t, clk, workDir)
		store := newWedgedStore(inner)
		store.snapshotID = bead.ID
		sp := newPostStartProvider()
		defer sp.release()
		defer store.release()
		begin := time.Now()

		// Take the production lease first: it writes the row's record, which the
		// wedge must not stall.
		run := enqueueCommitPhaseStartShaped(context.Background(), t, testCity, item, sp, store, clk, immediateStartStabilityWaiter, func(t *testing.T, city string, item preparedStart) (preparedStart, func(), func()) {
			item, dropLease, cleanup := productionLease(store)(t, city, item)
			store.arm()
			return item, dropLease, cleanup
		})
		elapsed := run.awaitFinished(t, sp, store, begin)

		if store.hung.Load() == 0 {
			t.Fatalf("no store call hung: the scenario no longer reaches the wedge")
		}
		if elapsed < commitBound || elapsed > commitBound+time.Second {
			t.Fatalf("the async start finished %v in, want it to give up the commit at the %v bound and free the slot then; store calls hung: %s; log: %s", elapsed, commitBound, store.callers(), run.stderr.String())
		}
		if got := run.inFlight(); got != 0 {
			t.Fatalf("asyncStartLimiter has %d starts in flight, want the slot back although the store is wedged (hung: %s)", got, store.callers())
		}
		if got := run.followUps.Load(); got != 1 {
			t.Fatalf("follow-up ran %d times, want 1 although the store is wedged (hung: %s)", got, store.callers())
		}
		if !strings.Contains(run.stderr.String(), "commit_unavailable") {
			t.Fatalf("log = %q, want a commit_unavailable lifecycle line for the abandoned commit", run.stderr.String())
		}
		requireBeadUnchanged(t, inner, store.atArm())
	})
}

// The same contract when the commit is refused rather than timed out: an
// observation of the session is already outstanding, so the commit never runs,
// and the waiter still frees the slot without touching a wedged store.
func TestRefusedCommitFreesTheSlotUnderTheProductionLeaseWhenTheStoreIsWedged(t *testing.T) {
	testCity := t.TempDir()
	workDir := t.TempDir()
	synctest.Test(t, func(t *testing.T) {
		clk := &clock.Fake{Time: time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)}
		inner, bead, _, item := postStartBeadItem(t, clk, workDir)
		store := newWedgedStore(inner)
		store.snapshotID = bead.ID
		sp := newPostStartProvider()
		defer sp.release()
		defer store.release()
		wedged := make(chan struct{})
		defer close(wedged)
		occupy := startStabilityWaiter(func(ctx context.Context, name string) bool {
			wctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_, _ = observeSessionBounded(wctx, testCity, "worker", func() (int, error) { <-wedged; return 0, nil })
			return immediateStartStabilityWaiter(ctx, name)
		})
		begin := time.Now()

		run := enqueueCommitPhaseStartShaped(context.Background(), t, testCity, item, sp, store, clk, occupy, func(t *testing.T, city string, item preparedStart) (preparedStart, func(), func()) {
			item, dropLease, cleanup := productionLease(store)(t, city, item)
			store.arm()
			return item, dropLease, cleanup
		})
		elapsed := run.awaitFinished(t, sp, store, begin)

		if elapsed >= commitBound {
			t.Fatalf("the async start took %v, want the commit refused at once and the slot freed then; store calls hung: %s", elapsed, store.callers())
		}
		if got := run.inFlight(); got != 0 {
			t.Fatalf("asyncStartLimiter has %d starts in flight, want the slot back although the store is wedged (hung: %s)", got, store.callers())
		}
		if got := run.followUps.Load(); got != 1 {
			t.Fatalf("follow-up ran %d times, want 1 although the store is wedged (hung: %s)", got, store.callers())
		}
		if !strings.Contains(run.stderr.String(), "commit_unavailable") {
			t.Fatalf("log = %q, want a commit_unavailable lifecycle line", run.stderr.String())
		}
		requireBeadUnchanged(t, inner, store.atArm())
	})
}

// A failed start whose rollback has to read the runtime's identity and stop it
// reaches two more provider calls the commit cannot cancel. With both hung, and
// the store answering, the bound still gives the slot back, runs the follow-up
// and writes nothing from the waiter.
func TestAbandonedCommitOfAFailedStartFreesTheSlotWhenTheRuntimeCallsHang(t *testing.T) {
	testCity := t.TempDir()
	workDir := t.TempDir()
	synctest.Test(t, func(t *testing.T) {
		clk := &clock.Fake{Time: time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)}
		inner, bead, _, item := postStartBeadItem(t, clk, workDir)
		store := newCommitGateStore(inner)
		store.hangCommit.Store(false)
		sp := lateFailureProvider()
		sp.hangStopInCommit.Store(true)
		sp.hangGetMetaInCommit.Store(true)
		var before beads.Bead
		var once sync.Once
		sp.onHang = func() { once.Do(func() { before = mustGetBead(t, inner, bead.ID) }) }
		defer sp.release()
		defer store.release()
		begin := time.Now()

		run := enqueueCommitPhaseStart(context.Background(), t, testCity, item, sp, store, clk, immediateStartStabilityWaiter)
		elapsed := run.awaitFinished(t, sp, store, begin)

		if sp.hangs.Load() == 0 {
			t.Fatalf("no provider call hung: the scenario no longer reaches the commit's runtime calls")
		}
		if elapsed > commitBound+postStartBound+time.Second {
			t.Fatalf("the async start finished %v in, want it back within the %v commit bound; provider calls hung: %s", elapsed, commitBound, sp.hungIn())
		}
		if got := run.inFlight(); got != 0 {
			t.Fatalf("asyncStartLimiter has %d starts in flight, want the slot back (hung: %s)", got, sp.hungIn())
		}
		if got := run.followUps.Load(); got != 1 {
			t.Fatalf("follow-up ran %d times, want 1", got)
		}
		if !strings.Contains(run.stderr.String(), "commit_unavailable") {
			t.Fatalf("log = %q, want a commit_unavailable lifecycle line for the abandoned commit", run.stderr.String())
		}
		requireBeadUnchanged(t, inner, before)
		releaseAndSettle(t, sp)
	})
}

// startCapacityRefusedAsync runs one async start pass of a session whose
// provider Start is refused for capacity, on store, and returns once every
// start goroutine finished or the watchdog expired.
func startCapacityRefusedAsync(t *testing.T, e *capacityEnv, sp runtime.Provider, store beads.Store, c startCandidate) bool {
	t.Helper()
	var tracker asyncStartTracker
	opts := append(append([]startExecutionOption(nil), e.startOptions...), withAsyncStartExecution(), withAsyncStartTracker(&tracker))
	executePlannedStartsTraced(context.Background(), []startCandidate{c}, e.cfg, e.desiredState, sp, store, "", e.city, e.clk, e.rec, 0, &e.log, &e.log, nil, opts...)
	return tracker.wait(commitWatchdog)
}

// occupyingRefuser refuses Start for capacity, after leaving an observation of
// the session outstanding the way an abandoned post-start wait does.
type occupyingRefuser struct {
	*runtime.Fake
	city   string
	wedged chan struct{}
}

func (p *occupyingRefuser) Start(ctx context.Context, name string, cfg runtime.Config) error {
	p.Fake.Start(ctx, name, cfg) //nolint:errcheck // records the call; the refusal below is the result
	p.Stop(name)                 //nolint:errcheck
	wctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, _ = observeSessionBounded(wctx, p.city, name, func() (int, error) { <-p.wedged; return 0, nil })
	return capacityRefusal(name, "")
}

// The endpoint verdict of a refused start is resolved before the commit, outside
// the commit bound, so a commit that wedges cannot lose it or leave the
// endpoint's ticket admitted. The breaker opens on the refusal even though the
// commit never answered.
func TestAsyncStart_CapacityVerdictIsRecordedWhenTheCommitHangs(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newCapacityEnv(t, true, "s")
		c := e.pendingCreate(t, "s", "e", nil)
		e.sp.StartErrors["s"] = capacityRefusal("s", "")
		gate := newCommitGateStore(e.store)
		defer gate.release()

		if !startCapacityRefusedAsync(t, e, e.sp, gate, c) {
			t.Fatalf("the async start had not finished %v in: the commit pins the slot", commitWatchdog)
		}

		if gate.hung.Load() != 1 {
			t.Fatalf("%d store Gets hung, want exactly the commit's refresh read: log = %q", gate.hung.Load(), e.log.String())
		}
		if !strings.Contains(e.log.String(), "commit_unavailable") {
			t.Fatalf("log = %q, want the commit abandoned at its bound", e.log.String())
		}
		if st := e.breakerStatus(capacityTestEndpoint); st.State != resilience.StateOpen {
			t.Fatalf("breaker = %v, want open: the refusal's verdict was resolved before the commit", st.State)
		}
	})
}

// The same when the commit is refused without running because an observation of
// the session is already outstanding: the verdict was resolved before the
// refusal, so the endpoint is not left admitted behind a commit that never ran.
func TestAsyncStart_CapacityVerdictIsRecordedWhenTheCommitIsRefused(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newCapacityEnv(t, true, "s")
		c := e.pendingCreate(t, "s", "e", nil)
		wedged := make(chan struct{})
		defer close(wedged)
		sp := &occupyingRefuser{Fake: e.sp, city: e.city, wedged: wedged}

		if !startCapacityRefusedAsync(t, e, sp, e.store, c) {
			t.Fatalf("the async start had not finished %v in", commitWatchdog)
		}

		if !strings.Contains(e.log.String(), "commit_unavailable") {
			t.Fatalf("log = %q, want the commit refused while an observation of the session is outstanding", e.log.String())
		}
		if st := e.breakerStatus(capacityTestEndpoint); st.State != resilience.StateOpen {
			t.Fatalf("breaker = %v, want open: the refusal's verdict was resolved before the commit was refused", st.State)
		}
	})
}
