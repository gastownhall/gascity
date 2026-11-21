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
	"github.com/gastownhall/gascity/internal/events"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/session/sessiontest"
)

const (
	// commitBound is the longest the async start goroutine waits for the commit
	// and settle phase that follows runPreparedStartCandidate. It is spelled out
	// here, not read from the production constant, so a missing bound fails these
	// tests by hanging on the watchdog instead of by not compiling.
	commitBound = 30 * time.Second
	// commitWatchdog is far past commitBound. It only turns an unbounded wait
	// into a test failure instead of a deadlocked bubble.
	commitWatchdog = 5 * time.Minute
)

// commitGateStore is a beads.Store whose Get can hang the way a stalled store
// does, but only for the Gets the commit and settle phase makes: a hung Get
// ignores everything and returns only when release is called. The start phase
// reads the store normally, so a scenario wedges exactly the phase under test
// whatever the start phase happens to read.
type commitGateStore struct {
	beads.Store

	// hangCommit makes the commit phase's Gets hang.
	hangCommit atomic.Bool

	// commitGets counts the Gets the commit phase made, hung or not.
	commitGets atomic.Int64
	hung       atomic.Int64
	hanging    atomic.Int64

	releaseC    chan struct{}
	releaseOnce sync.Once
}

func newCommitGateStore(inner beads.Store) *commitGateStore {
	s := &commitGateStore{Store: inner, releaseC: make(chan struct{})}
	s.hangCommit.Store(true)
	return s
}

// inCommitPhase reports whether the calling goroutine is inside
// commitAsyncStartResultWithContext.
func inCommitPhase() bool {
	pcs := make([]uintptr, 64)
	frames := goruntime.CallersFrames(pcs[:goruntime.Callers(2, pcs)])
	for {
		frame, more := frames.Next()
		if strings.HasSuffix(frame.Function, ".commitAsyncStartResultWithContext") {
			return true
		}
		if !more {
			return false
		}
	}
}

func (s *commitGateStore) Get(id string) (beads.Bead, error) {
	if inCommitPhase() {
		s.commitGets.Add(1)
		if s.hangCommit.Load() {
			s.hung.Add(1)
			s.hanging.Add(1)
			defer s.hanging.Add(-1)
			<-s.releaseC
		}
	}
	return s.Store.Get(id)
}

// release lets every hung Get return. It is idempotent so a scenario can call
// it and defer it.
func (s *commitGateStore) release() {
	s.releaseOnce.Do(func() { close(s.releaseC) })
}

// lockedLog is an io.Writer that tolerates the abandoned commit writing while
// the start goroutine logs the abandonment.
type lockedLog struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (l *lockedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *lockedLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// hungGetReporter is a test store that can say how many calls are hung on it.
type hungGetReporter interface{ hungNow() int64 }

func (s *commitGateStore) hungNow() int64 { return s.hanging.Load() }

// commitPhaseRun is one async start enqueued the way the reconciler enqueues
// it, with the counters the commit-phase scenarios assert on.
type commitPhaseRun struct {
	limiter   *asyncStartLimiter
	released  atomic.Int64
	finished  atomic.Int64
	followUps atomic.Int64
	finishedC chan struct{}
	stderr    *lockedLog
	rec       *events.Fake
	// releaseLease gives back the start lease the test took for the session.
	releaseLease func()
}

// startLeaseShape builds the lease an async start is enqueued with. It returns
// the prepared start carrying the lease, which the start itself ends, and a
// cleanup that gives the lease back at the end of the test if the start did not.
type startLeaseShape func(t *testing.T, city string, item preparedStart) (preparedStart, func())

// flockOnlyLease is the lease the post-start tests hold: the name's flock and
// nothing on the row, released by the test.
func flockOnlyLease(t *testing.T, city string, item preparedStart) (preparedStart, func()) {
	t.Helper()
	return leasedStart(t, city, item)
}

// productionLease is the lease the reconciler's start pass gives an async start
// (executePlannedStartsTraced): the name's flock taken against store, then the
// row's record taken just before PreWake, ended by the start. Releasing it
// reads and writes the store.
func productionLease(store beads.Store) startLeaseShape {
	return func(t *testing.T, city string, item preparedStart) (preparedStart, func()) {
		t.Helper()
		lease, dropLease, err := tryRuntimeLease(store, city, startLeaseName(item.candidate), "", 0)
		if err != nil {
			t.Fatalf("tryRuntimeLease: %v", err)
		}
		if err := lease.TakeRecord(sessionFrontDoor(store), sessionpkg.RuntimeLeaseRequest{
			City: city, Name: item.candidate.info.SessionName, ID: item.candidate.info.ID, TTL: sessionpkg.RuntimeLeaseTTL(postStartTimeout),
		}); err != nil {
			dropLease()
			t.Fatalf("TakeRecord: %v", err)
		}
		item.candidate.lease = lease
		// A fixture start that skipped PreWake runs under the incarnation its
		// row records, as leasedStart does: a runtime-only start refuses one
		// that was not minted.
		if !item.candidate.inc.Valid() && item.candidate.info.ID != "" {
			item.candidate.inc = sessionpkg.RecordedIncarnationForTest(item.candidate.info)
		}
		return item, dropLease
	}
}

// enqueueCommitPhaseStart reserves a slot and enqueues one async start of the
// fixture session on ctx, the controller's context, under the flock-only lease.
func enqueueCommitPhaseStart(ctx context.Context, t *testing.T, city string, item preparedStart, sp *postStartProvider, store beads.Store, clk clock.Clock, waiter startStabilityWaiter) *commitPhaseRun {
	t.Helper()
	return enqueueCommitPhaseStartShaped(ctx, t, city, item, sp, store, clk, waiter, flockOnlyLease)
}

// enqueueCommitPhaseStartShaped is enqueueCommitPhaseStart under the lease
// shape the caller picks.
func enqueueCommitPhaseStartShaped(ctx context.Context, t *testing.T, city string, item preparedStart, sp *postStartProvider, store beads.Store, clk clock.Clock, waiter startStabilityWaiter, shape startLeaseShape) *commitPhaseRun {
	t.Helper()
	run := &commitPhaseRun{
		limiter:   newAsyncStartLimiter(1),
		finishedC: make(chan struct{}),
		stderr:    &lockedLog{},
		rec:       events.NewFake(),
	}
	slot, ok, reason := reserveAsyncStartSlot(context.Background(), run.limiter)
	if !ok {
		t.Fatalf("reserve async start slot: %s", reason)
	}
	item, cleanup := shape(t, city, item)
	var cleanupOnce sync.Once
	run.releaseLease = func() { cleanupOnce.Do(cleanup) }
	t.Cleanup(run.releaseLease)
	enqueuePreparedStartWaveForCity(
		ctx,
		[]asyncPreparedStart{{
			item: item,
			release: func() {
				run.released.Add(1)
				slot()
			},
			done: func() {
				run.finished.Add(1)
				close(run.finishedC)
			},
		}},
		city, sp, store, nil, clk, run.rec, postStartTimeout, 1, ioDiscard{}, run.stderr, nil,
		func() { run.followUps.Add(1) },
		waiter, immediateSessionStaleKeyDetectionWaiter, nil,
	)
	return run
}

// awaitFinished fails the test if the async start has not finished within
// commitWatchdog of fake time, and returns the fake time it took.
func (r *commitPhaseRun) awaitFinished(t *testing.T, sp *postStartProvider, store hungGetReporter, begin time.Time) time.Duration {
	t.Helper()
	select {
	case <-r.finishedC:
		return time.Since(begin)
	case <-time.After(commitWatchdog):
		t.Fatalf("the async start had not finished %v in and still holds its slot: the commit phase after runPreparedStartCandidate is unbounded (the bound is %v); %d store calls hung, provider calls hung: %s", commitWatchdog, commitBound, store.hungNow(), sp.hungIn())
		return 0
	}
}

func (r *commitPhaseRun) inFlight() int {
	r.limiter.mu.Lock()
	defer r.limiter.mu.Unlock()
	return r.limiter.inFlight
}

// The async start goroutine holds its asyncStartLimiter slot until the commit
// finishes, so a commit that waits forever on the store holds it forever. The
// bound on the commit phase gives the slot back and still runs the follow-up.
func TestEnqueuePreparedStartWaveReleasesItsSlotWhenTheCommitHangs(t *testing.T) {
	testCity := t.TempDir()
	workDir := t.TempDir()
	synctest.Test(t, func(t *testing.T) {
		clk := &clock.Fake{Time: time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)}
		inner, bead, _, item := postStartBeadItem(t, clk, workDir)
		store := newCommitGateStore(inner)
		sp := newPostStartProvider()
		defer sp.release()
		defer store.release()
		begin := time.Now()

		run := enqueueCommitPhaseStart(context.Background(), t, testCity, item, sp, store, clk, immediateStartStabilityWaiter)
		elapsed := run.awaitFinished(t, sp, store, begin)

		if store.hung.Load() != 1 {
			t.Fatalf("%d store Gets hung, want exactly the commit's refresh read: the scenario no longer reaches the commit", store.hung.Load())
		}
		if elapsed < commitBound || elapsed > commitBound+time.Second {
			t.Fatalf("the async start finished %v in, want it to give up the commit at the %v bound", elapsed, commitBound)
		}
		if got := run.released.Load(); got != 1 {
			t.Fatalf("slot released %d times, want 1", got)
		}
		if got := run.inFlight(); got != 0 {
			t.Fatalf("asyncStartLimiter has %d starts in flight, want the slot back", got)
		}
		if got := run.followUps.Load(); got != 1 {
			t.Fatalf("follow-up ran %d times, want 1: the abandoned commit must request another pass", got)
		}
		if !strings.Contains(run.stderr.String(), "commit_unavailable") {
			t.Fatalf("log = %q, want a commit_unavailable lifecycle line for the abandoned commit", run.stderr.String())
		}
		// The waiter writes nothing: the row is exactly what the start phase
		// left, and the runtime is not touched. The age of the lease is the
		// repair path's input (SESSION-RUNTIME-017).
		got := mustGetBead(t, inner, bead.ID)
		if got.Status != "open" || got.Metadata["pending_create_claim"] != "true" {
			t.Fatalf("session after the abandoned commit: status=%q pending_create_claim=%q, want it open and still claimed", got.Status, got.Metadata["pending_create_claim"])
		}
		if got := sp.CountCalls("Stop", "worker"); got != 0 {
			t.Fatalf("Stop calls = %d, want 0: expiry is not a shutdown, the waiter must not tear the runtime down", got)
		}
		if len(run.rec.Events) != 0 {
			t.Fatalf("lifecycle events = %#v, want none from an abandoned commit", run.rec.Events)
		}

		// The abandoned commit's Get returns late. The goroutine that waited
		// for it is gone, so nothing may be reported or released twice. Run
		// under -race: the closure and the waiter share no writable state.
		store.release()
		synctest.Wait()
		if run.released.Load() != 1 || run.finished.Load() != 1 || run.followUps.Load() != 1 {
			t.Fatalf("after the late return: released=%d finished=%d followUps=%d, want 1 each", run.released.Load(), run.finished.Load(), run.followUps.Load())
		}
		if got := store.hanging.Load(); got != 0 {
			t.Fatalf("%d store Gets still hung after release", got)
		}
	})
}

// noDriftBeadItem is postStartBeadItem with the row's command equal to the
// command the start launches, so a late commit of the start finds no drift to
// roll back: left alone it commits the start.
func noDriftBeadItem(t *testing.T, clk clock.Clock, workDir string) (beads.Store, beads.Bead, preparedStart) {
	t.Helper()
	store := beads.NewMemStore()
	bead, _ := createStartRecoveryFixture(t, store, clk, workDir)
	if err := store.SetMetadata(bead.ID, "command", "claude --resume resume-key"); err != nil {
		t.Fatalf("SetMetadata command: %v", err)
	}
	bead = mustGetBead(t, store, bead.ID)
	return store, bead, postStartItem(sessiontest.SeedBead(t, bead), workDir)
}

// Expiry of the commit bound is not a shutdown: the shutdown branch inside the
// commit is keyed on the controller's context, and the abandoned commit keeps
// running on it. A healthy start whose commit outlives the bound and then
// completes, with the controller still up, commits the start like any other:
// the row leaves its pending create and the runtime is left running.
func TestAbandonedCommitRunsToCompletionOnTheControllerContext(t *testing.T) {
	testCity := t.TempDir()
	workDir := t.TempDir()
	synctest.Test(t, func(t *testing.T) {
		clk := &clock.Fake{Time: time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)}
		inner, bead, item := noDriftBeadItem(t, clk, workDir)
		store := newCommitGateStore(inner)
		sp := newPostStartProvider()
		defer sp.release()
		defer store.release()
		begin := time.Now()

		run := enqueueCommitPhaseStart(context.Background(), t, testCity, item, sp, store, clk, immediateStartStabilityWaiter)
		run.awaitFinished(t, sp, store, begin)
		if got := sp.CountCalls("Stop", "worker"); got != 0 {
			t.Fatalf("Stop calls = %d at expiry, want 0: the waiter must not tear the runtime down", got)
		}
		if at := mustGetBead(t, inner, bead.ID); at.Metadata["pending_create_claim"] != "true" {
			t.Fatalf("pending_create_claim = %q at expiry, want the abandoned commit to have written nothing yet", at.Metadata["pending_create_claim"])
		}
		store.release()
		synctest.Wait()

		got := mustGetBead(t, inner, bead.ID)
		if got.Metadata["pending_create_claim"] != "" || got.Metadata["state"] != "active" {
			t.Fatalf("session after the late commit returned: state=%q pending_create_claim=%q, want the start committed (active, claim cleared) on the controller's context", got.Metadata["state"], got.Metadata["pending_create_claim"])
		}
		if !sp.IsRunning("worker") || sp.CountCalls("Stop", "worker") != 0 {
			t.Fatalf("runtime running=%v stops=%d after the late commit, want the healthy runtime left running", sp.IsRunning("worker"), sp.CountCalls("Stop", "worker"))
		}
	})
}

// Under the lease production hands an async start, a record on the row fences
// the commit: a commit that finds the record gone or not its own is stale, is
// refused, and has its runtime stopped. The waiter that gave up on a commit
// therefore must not release that lease, or the commit that lands afterward
// finds the record cleared and stops a healthy runtime. The commit's own
// goroutine releases the lease when it returns; until then the name's flock
// stays held and another start of the session defers on it
// (SESSION-RUNTIME-017).
func TestAbandonedCommitLandsUnderTheProductionLeaseAndThenReleasesIt(t *testing.T) {
	testCity := t.TempDir()
	workDir := t.TempDir()
	synctest.Test(t, func(t *testing.T) {
		clk := &clock.Fake{Time: time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)}
		inner, bead, item := noDriftBeadItem(t, clk, workDir)
		store := newCommitGateStore(inner)
		sp := newPostStartProvider()
		defer sp.release()
		defer store.release()
		begin := time.Now()

		run := enqueueCommitPhaseStartShaped(context.Background(), t, testCity, item, sp, store, clk, immediateStartStabilityWaiter, productionLease(inner))
		run.awaitFinished(t, sp, store, begin)
		if got := run.inFlight(); got != 0 {
			t.Fatalf("asyncStartLimiter has %d starts in flight at expiry, want the slot back", got)
		}
		name := startLeaseName(item.candidate)
		if _, drop, err := tryRuntimeLease(inner, testCity, name, "", 0); err == nil {
			drop()
			t.Fatalf("the name's flock was free at expiry, want it held by the abandoned commit's start so no second start runs under it")
		}
		store.release()
		synctest.Wait()

		got := mustGetBead(t, inner, bead.ID)
		if got.Metadata["pending_create_claim"] != "" || got.Metadata["state"] != "active" {
			t.Fatalf("session after the late commit returned: state=%q pending_create_claim=%q, want the start committed (active, claim cleared)", got.Metadata["state"], got.Metadata["pending_create_claim"])
		}
		if !sp.IsRunning("worker") || sp.CountCalls("Stop", "worker") != 0 {
			t.Fatalf("runtime running=%v stops=%d after the late commit, want the healthy runtime left running: the late commit was refused as stale", sp.IsRunning("worker"), sp.CountCalls("Stop", "worker"))
		}
		if strings.Contains(run.stderr.String(), "stale_async_start") {
			t.Fatalf("log = %q, want the late commit accepted, not refused as stale", run.stderr.String())
		}
		_, drop, err := tryRuntimeLease(inner, testCity, name, "", 0)
		if err != nil {
			t.Fatalf("the name's flock is still held after the late commit returned: %v", err)
		}
		drop()
	})
}

// Controller shutdown is not the commit bound. A commit that is waiting when
// the controller's context is canceled is still waited for: the wait is not
// abandoned, so the shutdown branch inside the commit runs and the shutdown
// accounting (waitForAsyncStarts) sees the start finish when its work does.
func TestControllerShutdownDoesNotAbandonTheCommitWait(t *testing.T) {
	testCity := t.TempDir()
	workDir := t.TempDir()
	synctest.Test(t, func(t *testing.T) {
		clk := &clock.Fake{Time: time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)}
		inner, _, _, item := postStartBeadItem(t, clk, workDir)
		store := newCommitGateStore(inner)
		sp := newPostStartProvider()
		defer sp.release()
		defer store.release()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		begin := time.Now()

		run := enqueueCommitPhaseStart(ctx, t, testCity, item, sp, store, clk, immediateStartStabilityWaiter)
		<-time.After(5 * time.Second)
		if store.hanging.Load() != 1 {
			t.Fatalf("%d store Gets hung 5s in, want the commit's refresh read", store.hanging.Load())
		}
		cancel()
		<-time.After(time.Second)
		select {
		case <-run.finishedC:
			t.Fatalf("the async start finished %v in, 1s after shutdown began with its commit still waiting on the store: the wait was abandoned", time.Since(begin))
		default:
		}
		if strings.Contains(run.stderr.String(), "commit_unavailable") {
			t.Fatalf("log = %q, want no commit_unavailable line: shutdown is not an expiry", run.stderr.String())
		}

		store.release()
		select {
		case <-run.finishedC:
		case <-time.After(commitWatchdog):
			t.Fatalf("the async start had not finished after the store answered")
		}
		if got := run.released.Load(); got != 1 {
			t.Fatalf("slot released %d times, want 1", got)
		}
		if strings.Contains(run.stderr.String(), "commit_unavailable") {
			t.Fatalf("log = %q, want no commit_unavailable line: the commit ran to completion", run.stderr.String())
		}
	})
}

// A hung commit is retried every reconcile tick. Without a cap each tick would
// abandon one more store read; the contract is at most one outstanding
// observation of the session, shared with the post-start bounds.
func TestRepeatedStartsLeaveOneAbandonedCommitPerSession(t *testing.T) {
	testCity := t.TempDir()
	workDir := t.TempDir()
	synctest.Test(t, func(t *testing.T) {
		clk := &clock.Fake{Time: time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)}
		inner, _, _, item := postStartBeadItem(t, clk, workDir)
		store := newCommitGateStore(inner)
		sp := newPostStartProvider()
		defer sp.release()
		defer store.release()
		const attempts = 100
		begin := time.Now()

		for i := 0; i < attempts; i++ {
			sp.beginAttempt(t, "worker")
			// The same session each tick: the first start wedges its commit,
			// the later ones must find that commit outstanding and give up
			// without spawning another.
			run := enqueueCommitPhaseStart(context.Background(), t, testCity, item, sp, store, clk, immediateStartStabilityWaiter)
			run.awaitFinished(t, sp, store, time.Now())
			run.releaseLease()
			if got := run.inFlight(); got != 0 {
				t.Fatalf("attempt %d: asyncStartLimiter has %d starts in flight, want the slot back", i, got)
			}
		}

		if got := store.hung.Load(); got != 1 {
			t.Fatalf("%d store Gets hung over %d ticks, want 1: ticks that find an abandoned commit outstanding must not start another", got, attempts)
		}
		if got := store.hanging.Load(); got > 1 {
			t.Fatalf("%d abandoned store Gets hung at once, want at most 1", got)
		}
		if elapsed := time.Since(begin); elapsed > commitBound+postStartBound {
			t.Fatalf("%d ticks took %v of fake time, want only the first to wait (at most %v)", attempts, elapsed, commitBound)
		}
	})
}

// An observation of the session that is already outstanding (an R3a zombie
// holds the key) means the commit cannot be bounded safely: it is refused
// without running, immediately, and the waiter writes nothing. The row keeps
// its pending claim and the lease ages out through the reconciler's repair
// paths, the alive-row gate and the stale-lease rollback.
func TestCommitIsRefusedWithoutRunningWhenTheSessionObservationIsOutstanding(t *testing.T) {
	testCity := t.TempDir()
	workDir := t.TempDir()
	synctest.Test(t, func(t *testing.T) {
		clk := &clock.Fake{Time: time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)}
		inner, bead, _, item := postStartBeadItem(t, clk, workDir)
		store := newCommitGateStore(inner)
		store.hangCommit.Store(false)
		sp := newPostStartProvider()
		defer sp.release()
		defer store.release()
		wedged := make(chan struct{})
		defer close(wedged)
		// The waiter runs after provider.Start. Wedging an observation of the
		// session there is an R3a zombie holding the key before the commit.
		occupy := startStabilityWaiter(func(ctx context.Context, name string) bool {
			wctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_, _ = observeSessionBounded(wctx, testCity, "worker", func() (int, error) { <-wedged; return 0, nil })
			return immediateStartStabilityWaiter(ctx, name)
		})
		begin := time.Now()

		run := enqueueCommitPhaseStart(context.Background(), t, testCity, item, sp, store, clk, occupy)
		elapsed := run.awaitFinished(t, sp, store, begin)
		commitGets := store.commitGets.Load()

		if elapsed >= commitBound {
			t.Fatalf("the async start took %v, want the commit refused at once, not after the %v bound", elapsed, commitBound)
		}
		if commitGets != 0 {
			t.Fatalf("%d store Gets by the commit, want 0: the commit must not run while an observation of the session is outstanding", commitGets)
		}
		if !strings.Contains(run.stderr.String(), "commit_unavailable") {
			t.Fatalf("log = %q, want a commit_unavailable lifecycle line", run.stderr.String())
		}
		if got := run.followUps.Load(); got != 1 {
			t.Fatalf("follow-up ran %d times, want 1", got)
		}
		if got := run.inFlight(); got != 0 {
			t.Fatalf("asyncStartLimiter has %d starts in flight, want the slot back", got)
		}
		got := mustGetBead(t, inner, bead.ID)
		if got.Status != "open" || got.Metadata["pending_create_claim"] != "true" || got.Metadata["last_woke_at"] == "" {
			t.Fatalf("session after the refused commit: status=%q pending_create_claim=%q last_woke_at=%q, want it open, claimed and still leased: the waiter must not clear the lease", got.Status, got.Metadata["pending_create_claim"], got.Metadata["last_woke_at"])
		}
		if got := sp.CountCalls("Stop", "worker"); got != 0 {
			t.Fatalf("Stop calls = %d, want 0", got)
		}
	})
}
