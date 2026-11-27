package main

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/resilience"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// prepareFailingStore fails every Get made from inside
// prepareStartCandidateForCity with err: every row's, or only the rows whose
// ids are in only when it is set.
type prepareFailingStore struct {
	beads.Store
	err  error
	only map[string]bool
}

func (s *prepareFailingStore) Get(id string) (beads.Bead, error) {
	if inPrepare() && (s.only == nil || s.only[id]) {
		return beads.Bead{}, s.err
	}
	return s.Store.Get(id)
}

// prepareMoveStore gates a row move into the window between a start's two
// in-prepare reads: the first in-prepare Get of id (the locked capture) passes
// through; the second (the mint's fresh re-read) blocks until unblock,
// announcing itself by closing mintRead, so a patch applied on the real store
// while it blocks lands after the capture but before the mint compares.
type prepareMoveStore struct {
	beads.Store
	id       string
	mintRead chan struct{}
	once     sync.Once
	unblock  chan struct{}
	rowReads int
}

func (s *prepareMoveStore) Get(id string) (beads.Bead, error) {
	if inPrepare() && id == s.id {
		s.rowReads++
		if s.rowReads == 2 {
			s.once.Do(func() { close(s.mintRead) })
			<-s.unblock
		}
	}
	return s.Store.Get(id)
}

// asyncPrepareTrace arms a detail trace over a real tracer rooted in the
// test city, so reconciler.start.execute records flushed by async start
// goroutines can be read back from the durable trace.
type asyncPrepareTrace struct {
	tracer *SessionReconcilerTracer
	cycle  *sessionReconcilerTraceCycle
	city   string
}

func newAsyncPrepareTrace(t *testing.T, e *capacityEnv, templates ...string) *asyncPrepareTrace {
	t.Helper()
	tracer := newSessionReconcilerTracer(e.city, "async-prepare-town", io.Discard)
	if !tracer.Enabled() {
		t.Fatal("tracer should be enabled")
	}
	now := e.clk.Now().UTC()
	for _, template := range templates {
		if _, err := tracer.armStore.upsertArm(TraceArm{
			ScopeType:      TraceArmScopeTemplate,
			ScopeValue:     template,
			Source:         TraceArmSourceManual,
			Level:          TraceModeDetail,
			ArmedAt:        now,
			ExpiresAt:      now.Add(15 * time.Minute),
			LastExtendedAt: now,
			UpdatedAt:      now,
		}); err != nil {
			t.Fatalf("upsert trace arm for %s: %v", template, err)
		}
	}
	cycle := tracer.BeginCycle(TraceTickTriggerPatrol, "test", now, e.cfg)
	if cycle == nil {
		t.Fatal("BeginCycle returned nil")
	}
	cycle.syncArms(now, e.cfg)
	t.Cleanup(func() { _ = tracer.Close() })
	return &asyncPrepareTrace{tracer: tracer, cycle: cycle, city: e.city}
}

// startExecuteRecords returns this trace's durable reconciler.start.execute
// operation records.
func (a *asyncPrepareTrace) startExecuteRecords(t *testing.T) []SessionReconcilerTraceRecord {
	t.Helper()
	records, err := ReadTraceRecords(traceCityRuntimeDir(a.city), TraceFilter{TraceID: a.cycle.traceID})
	if err != nil {
		t.Fatalf("ReadTraceRecords: %v", err)
	}
	var ops []SessionReconcilerTraceRecord
	for _, rec := range records {
		if rec.RecordType == TraceRecordOperation && rec.SiteCode == TraceSiteLifecycleStartRun {
			ops = append(ops, rec)
		}
	}
	return ops
}

// assertAsyncStartReservationsReleased checks what every admitted async start
// must leave behind when it exits without a Start: no provider Start, the
// tracker drained, the registry empty, a fresh slot reservation and a runtime
// lease on the name both acquirable, and the row's in-flight marker clear.
func assertAsyncStartReservationsReleased(t *testing.T, e *capacityEnv, tracker *asyncStartTracker, limiter *asyncStartLimiter, name, id string) {
	t.Helper()
	if n := e.sp.CountCalls("Start", name); n != 0 {
		t.Fatalf("Start calls = %d, want 0", n)
	}
	if tracker.busy() {
		t.Fatal("tracker still busy after the async start exited")
	}
	if tracker.startInFlight(name) {
		t.Fatalf("registry still holds %s after the async start exited", name)
	}
	if _, ok, _ := limiter.reserve(context.Background()); !ok {
		t.Fatalf("async start slot for %s was not released", name)
	}
	if _, release, err := tryRuntimeLease(nil, e.city, name, "", 0); err != nil {
		t.Fatalf("runtime name %s is not takeable after the async start exited: %v", name, err)
	} else {
		release()
	}
	if b := e.bead(t, id); b.Metadata["last_woke_at"] != "" {
		t.Fatalf("last_woke_at = %q, want empty after the failed start", b.Metadata["last_woke_at"])
	}
}

// waitAsyncStartGoroutines waits for every start goroutine tracker launched to
// return, past its done count: a follow-up request comes after it.
func waitAsyncStartGoroutines(t *testing.T, tracker *asyncStartTracker) {
	t.Helper()
	if err := tracker.waitLaunchedStarts(context.Background(), hangBudget); err != nil {
		t.Fatal(err)
	}
}

// followUpProbe stands in for the controller's follow-up request. It counts
// the requests and records, at each, what the pass it asks for would find of
// the start's session: whether it is held out of admission, and what the start
// has not given back yet (its admitted-start registry entry, its async slot,
// its runtime name's flock).
type followUpProbe struct {
	tracker  *asyncStartTracker
	limiter  *asyncStartLimiter
	clk      clock.Clock
	city, id string
	name     string

	mu         sync.Mutex
	requests   int
	held       bool
	unreleased []string
}

func (p *followUpProbe) request() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.requests++
	_, p.held = p.tracker.preparationHeldUntil(p.id, p.clk.Now())
	if p.tracker.startInFlight(p.name) {
		p.unreleased = append(p.unreleased, "its admitted-start registry entry")
	}
	if release, ok, _ := p.limiter.reserve(context.Background()); ok {
		release()
	} else {
		p.unreleased = append(p.unreleased, "its async start slot")
	}
	if _, release, err := tryRuntimeLease(nil, p.city, p.name, "", 0); err == nil {
		release()
	} else {
		p.unreleased = append(p.unreleased, "its runtime name's flock")
	}
}

// assertFollowUps checks the start requested want follow-up passes, and that
// each came after it gave everything back, with its session held out of
// admission so that pass cannot admit it again.
func (p *followUpProbe) assertFollowUps(t *testing.T, want int) {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.requests != want {
		t.Fatalf("follow-up passes requested = %d, want %d", p.requests, want)
	}
	if want == 0 {
		return
	}
	if !p.held {
		t.Fatal("the session was not held out of admission when its failed start requested its follow-up pass: that pass would admit it again")
	}
	if len(p.unreleased) > 0 {
		t.Fatalf("the failed start requested its follow-up pass before it gave back %s", strings.Join(p.unreleased, ", "))
	}
}

// assertPreparationHold checks whether the session with bead ID id is held
// out of async admission now.
func assertPreparationHold(t *testing.T, tracker *asyncStartTracker, id string, now time.Time, want bool) {
	t.Helper()
	if _, held := tracker.preparationHeldUntil(id, now); held != want {
		t.Fatalf("session %s held out of admission = %t, want %t", id, held, want)
	}
}

// TestAsyncPrepareFailureReleasesReservations: an admitted async start whose
// preparation fails, or whose context ends before provider Start, records no
// Start, releases its slot, flock, ticket, tracker count and registry entry,
// clears the row's in-flight marker (except a superseded refusal), and leaves a
// failed (or context_canceled) lifecycle line plus a durable
// reconciler.start.execute record. A failed preparation holds its session out
// of async admission and then, with everything given back, requests one
// follow-up pass; a canceled start holds nothing and requests none.
func TestAsyncPrepareFailureReleasesReservations(t *testing.T) {
	for _, sub := range []string{"store_error", "user_held", "superseded", "canceled_after_prepare"} {
		t.Run(sub, func(t *testing.T) {
			e := newCapacityEnv(t, false, "worker-a")
			tr := newAsyncPrepareTrace(t, e, "worker-a")
			c := e.pendingCreate(t, "worker-a", "", nil)

			limiter := newAsyncStartLimiter(1)
			var tracker asyncStartTracker
			probe := &followUpProbe{tracker: &tracker, limiter: limiter, clk: e.clk, city: e.city, id: c.info.ID, name: "worker-a"}
			opts := append([]startExecutionOption(nil), e.startOptions...)
			opts = append(opts, withAsyncStartExecution(), withAsyncStartTracker(&tracker), withAsyncStartLimiter(limiter),
				withAsyncStartFollowUp(probe.request))

			wantOutcome := "failed"
			wantCode := TraceOutcomeFailed
			wantFollowUps := 1
			store := e.store
			var release func()
			var gate chan struct{}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch sub {
			case "store_error":
				store = &prepareFailingStore{Store: e.store, err: errors.New("store unreachable during prepare")}
			case "superseded":
				moved := &prepareMoveStore{Store: e.store, id: c.info.ID, mintRead: make(chan struct{}), unblock: make(chan struct{})}
				store = moved
				gate = moved.mintRead
				var unblockOnce sync.Once
				release = func() { unblockOnce.Do(func() { close(moved.unblock) }) }
				defer release()
			default:
				blocking := &prepareBlockingStore{Store: e.store, entered: make(chan struct{}), unblock: make(chan struct{})}
				store = blocking
				gate = blocking.entered
				var unblockOnce sync.Once
				release = func() { unblockOnce.Do(func() { close(blocking.unblock) }) }
				defer release()
			}

			returned := make(chan int, 1)
			go func() {
				returned <- executePlannedStartsTraced(ctx, []startCandidate{c}, e.cfg, e.desiredState, e.sp, store, "", e.city, e.clk, e.rec, 0, &e.log, &e.log, tr.cycle, opts...)
			}()
			if release != nil {
				select {
				case <-gate:
				case <-time.After(hangBudget):
					release()
					t.Fatal("preparation never reached the gate")
				}
				switch sub {
				case "user_held":
					if err := sessionFrontDoor(e.store).ApplyPatch(c.info.ID, sessionpkg.MetadataPatch{"sleep_intent": "user-hold"}); err != nil {
						t.Fatalf("holding the row: %v", err)
					}
				case "superseded":
					if err := sessionFrontDoor(e.store).ApplyPatch(c.info.ID, sessionpkg.MetadataPatch{"generation": "99"}); err != nil {
						t.Fatalf("moving the row: %v", err)
					}
				case "canceled_after_prepare":
					wantOutcome = "context_canceled"
					wantCode = TraceOutcomeContextCanceled
					wantFollowUps = 0
					cancel()
				}
				release()
			}
			if woken := <-returned; woken != 1 {
				t.Fatalf("planner call woken = %d, want 1 (the start was admitted and enqueued)", woken)
			}
			if !tracker.wait(hangBudget) {
				t.Fatal("async start did not finish within the hang budget")
			}
			waitAsyncStartGoroutines(t, &tracker)
			assertAsyncStartReservationsReleased(t, e, &tracker, limiter, "worker-a", c.info.ID)
			probe.assertFollowUps(t, wantFollowUps)
			assertPreparationHold(t, &tracker, c.info.ID, e.clk.Now(), wantFollowUps == 1)

			if got := e.log.String(); !strings.Contains(got, "session=worker-a") || !strings.Contains(got, "outcome="+wantOutcome) {
				t.Fatalf("lifecycle log lacks the %s outcome line:\n%s", wantOutcome, got)
			}
			ops := tr.startExecuteRecords(t)
			found := false
			for _, op := range ops {
				if op.OutcomeCode == wantCode && op.SessionName == "worker-a" {
					found = true
				}
			}
			if !found {
				t.Fatalf("no durable reconciler.start.execute record with outcome %s for worker-a among %d records", wantOutcome, len(ops))
			}
		})
	}

	// The exits below sit between the goroutine's launch and provider Start,
	// where the planner has no seam (goStart is a plain go statement). They
	// drive the enqueue directly.
	t.Run("canceled_before_prepare", func(t *testing.T) {
		e := newCapacityEnv(t, false, "worker-a")
		tr := newAsyncPrepareTrace(t, e, "worker-a")
		d := newDirectAdmission(t, e, "worker-a")

		// A context that is already done when the start goroutine first runs
		// releases everything without calling prepare at all.
		var prepareCalled atomic.Bool
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		d.enqueue(ctx, t, e, tr, func(startCandidate) (*preparedStart, prepareTimings, error) {
			prepareCalled.Store(true)
			return nil, prepareTimings{}, errors.New("prepare must not run")
		})
		d.assertDropOutcome(t, "context_canceled", false)
		if prepareCalled.Load() {
			t.Fatal("prepare ran though the context was already done")
		}
		assertAsyncStartReservationsReleased(t, e, d.tracker, d.limiter, "worker-a", d.candidate.info.ID)
		d.probe.assertFollowUps(t, 0)
		assertPreparationHold(t, d.tracker, d.candidate.info.ID, e.clk.Now(), false)
		if got := e.log.String(); !strings.Contains(got, "outcome=context_canceled") {
			t.Fatalf("lifecycle log lacks the context_canceled outcome line:\n%s", got)
		}
		d.assertTraced(t, tr, TraceOutcomeContextCanceled)
	})

	// A panic inside preparation is contained on the start goroutine, which
	// has no tick-level recover: it ends as one panic_recovered start that
	// releases everything, never a provider Start.
	t.Run("panic_in_prepare", func(t *testing.T) {
		e := newCapacityEnv(t, false, "worker-a")
		tr := newAsyncPrepareTrace(t, e, "worker-a")
		d := newDirectAdmission(t, e, "worker-a")

		d.enqueue(context.Background(), t, e, tr, func(startCandidate) (*preparedStart, prepareTimings, error) {
			panic("prepare exploded")
		})
		d.assertDropOutcome(t, "panic_recovered", false)
		assertAsyncStartReservationsReleased(t, e, d.tracker, d.limiter, "worker-a", d.candidate.info.ID)
		d.probe.assertFollowUps(t, 1)
		assertPreparationHold(t, d.tracker, d.candidate.info.ID, e.clk.Now(), true)
		if got := e.log.String(); !strings.Contains(got, "session=worker-a") || !strings.Contains(got, "outcome=panic_recovered") {
			t.Fatalf("lifecycle log lacks the panic_recovered outcome line:\n%s", got)
		}
		d.assertTraced(t, tr, TraceOutcomePanicRecovered)
	})
}

// directAdmission is one admitted async start built as the pass hands it to
// the enqueue: its slot, runtime lease, tracker count and registry entry are
// taken; drop releases them and logs the lifecycle line as the pass's does,
// and undoPrepared and drop record their calls.
type directAdmission struct {
	candidate startCandidate
	admitted  asyncAdmittedStart
	tracker   *asyncStartTracker
	limiter   *asyncStartLimiter
	// dropped receives drop's outcome; undone is closed if undoPrepared runs.
	dropped chan string
	undone  chan struct{}
	// probe is the start's follow-up request.
	probe *followUpProbe
}

func newDirectAdmission(t *testing.T, e *capacityEnv, name string) *directAdmission {
	t.Helper()
	c := e.pendingCreate(t, name, "", nil)
	tracker := &asyncStartTracker{}
	countDone, ok := tracker.start()
	if !ok {
		t.Fatal("tracker start refused")
	}
	unmark := tracker.markStartInFlight(name)
	done := func() { unmark(); countDone() }
	limiter := newAsyncStartLimiter(1)
	slot, reserved, _ := limiter.reserve(context.Background())
	if !reserved {
		t.Fatal("slot reserve refused")
	}
	lease, dropLease, err := tryRuntimeLease(e.store, e.city, name, "", 0)
	if err != nil {
		t.Fatalf("tryRuntimeLease: %v", err)
	}
	c.lease = lease
	var slotOnce sync.Once
	release := func() { slotOnce.Do(func() { dropLease(); slot() }) }
	d := &directAdmission{
		candidate: c,
		tracker:   tracker,
		limiter:   limiter,
		dropped:   make(chan string, 1),
		undone:    make(chan struct{}),
		probe:     &followUpProbe{tracker: tracker, limiter: limiter, clk: e.clk, city: e.city, id: c.info.ID, name: name},
	}
	d.admitted = asyncAdmittedStart{
		candidate: c,
		undoPrepared: func(error, ...startPhaseTimings) {
			close(d.undone)
		},
		drop: func(outcome string, err error, phases ...startPhaseTimings) {
			release()
			if outcome != "" {
				logLifecycleOutcome(&e.log, "start", 1, name, name, outcome, time.Time{}, time.Time{}, err, phases...)
			}
			d.dropped <- outcome
			done()
		},
		release: release,
		done:    done,
		tracker: tracker,
	}
	return d
}

// enqueue hands the admission to the enqueue with prepare, checks it was
// reported start_enqueued, and waits for its goroutine to return.
func (d *directAdmission) enqueue(ctx context.Context, t *testing.T, e *capacityEnv, tr *asyncPrepareTrace, prepare func(startCandidate) (*preparedStart, prepareTimings, error)) {
	t.Helper()
	results := enqueuePreparedStartWaveForCity(ctx, []asyncAdmittedStart{d.admitted}, e.city, e.sp, e.store, e.cfg, e.clk, e.rec, prepare, 0, 1, io.Discard, &e.log, tr.cycle, d.probe.request, nil, nil, nil)
	if len(results) != 1 || results[0].outcome != TraceOutcomeStartEnqueued {
		t.Fatalf("results = %+v, want one start_enqueued result", results)
	}
	if !d.tracker.wait(hangBudget) {
		t.Fatal("async start did not finish within the hang budget")
	}
	waitAsyncStartGoroutines(t, d.tracker)
}

// assertDropOutcome checks drop ran with outcome, and whether undoPrepared ran.
func (d *directAdmission) assertDropOutcome(t *testing.T, want string, wantUndo bool) {
	t.Helper()
	select {
	case outcome := <-d.dropped:
		if outcome != want {
			t.Fatalf("drop outcome = %q, want %q", outcome, want)
		}
	default:
		t.Fatal("drop was not called")
	}
	undone := false
	select {
	case <-d.undone:
		undone = true
	default:
	}
	if undone != wantUndo {
		t.Fatalf("undoPrepared ran = %v, want %v", undone, wantUndo)
	}
}

// assertTraced checks the durable trace holds the start's
// reconciler.start.execute record with outcome.
func (d *directAdmission) assertTraced(t *testing.T, tr *asyncPrepareTrace, want TraceOutcomeCode) {
	t.Helper()
	for _, op := range tr.startExecuteRecords(t) {
		if op.OutcomeCode == want && op.SessionName == d.candidate.name() {
			return
		}
	}
	t.Fatalf("no durable reconciler.start.execute record with outcome %s for %s", want, d.candidate.name())
}

// TestAsyncPrepareSupersededBreakerUndo: an async start's superseded-PreWake
// refusal lands a whole prepare after the restart was recorded, so the undo
// takes the restart back only while the identity's restart accounting is
// unchanged since the record: a progress observation or a manual Reset in
// between wins. A pass that observes no progress there (no assigned work, a
// first signature, an unchanged one) and a trim of restarts that aged out of
// the window do not block the undo. The row's breaker metadata follows.
func TestAsyncPrepareSupersededBreakerUndo(t *testing.T) {
	const identity = "rig-a/session-a"
	for _, tc := range []struct {
		name string
		// seed runs before the admission; interfere runs in the window after
		// the admission recorded its restart and before the refusal. now is
		// the admission's time.
		seed, interfere func(cb *sessionCircuitBreaker, now time.Time)
		// wantRestarts is the identity's restart count once the start ended,
		// read 2s after the admission; -1 wants no entry at all.
		wantRestarts int
		// wantKept wants the admission's restart to stay counted.
		wantKept bool
	}{
		{name: "restores_without_interference", wantRestarts: -1},
		{
			// A pass over an identity with no assigned work observes "".
			name: "restores_after_no_progress_pass",
			interfere: func(cb *sessionCircuitBreaker, now time.Time) {
				cb.ObserveProgressSignature(identity, "", now.Add(time.Second))
			},
			wantRestarts: -1,
		},
		{
			// A first signature primes the comparison and is no progress.
			name: "restores_after_first_signature",
			interfere: func(cb *sessionCircuitBreaker, now time.Time) {
				cb.ObserveProgressSignature(identity, "sig-1", now.Add(time.Second))
			},
			wantRestarts: -1,
		},
		{
			name: "restores_after_unchanged_signature",
			seed: func(cb *sessionCircuitBreaker, now time.Time) {
				cb.ObserveProgressSignature(identity, "sig-1", now.Add(-time.Minute))
			},
			interfere: func(cb *sessionCircuitBreaker, now time.Time) {
				cb.ObserveProgressSignature(identity, "sig-1", now.Add(time.Second))
			},
			wantRestarts: 0,
		},
		{
			// A read in the window (here a status snapshot) trims a restart
			// that aged out of the rolling window off the history; that is
			// no new restart.
			name: "restores_after_expired_restart_trimmed",
			seed: func(cb *sessionCircuitBreaker, now time.Time) {
				cb.RecordRestart(identity, now.Add(-30*time.Minute+time.Second))
			},
			interfere: func(cb *sessionCircuitBreaker, now time.Time) {
				cb.Snapshot(now.Add(2 * time.Second))
			},
			wantRestarts: 0,
		},
		{
			// Two different signatures: the first only primes the breaker's
			// expectation, the second is the progress it records.
			name: "keeps_newer_observation",
			interfere: func(cb *sessionCircuitBreaker, now time.Time) {
				cb.ObserveProgressSignature(identity, "sig-1", now)
				cb.ObserveProgressSignature(identity, "sig-2", now)
			},
			wantRestarts: 1,
			wantKept:     true,
		},
		{
			// One restart from before the admission, so an undo that ignored
			// the Reset would show it coming back.
			name: "keeps_after_reset",
			seed: func(cb *sessionCircuitBreaker, now time.Time) {
				cb.RecordRestart(identity, now.Add(-time.Minute))
			},
			interfere: func(cb *sessionCircuitBreaker, _ time.Time) {
				cb.Reset(identity)
			},
			wantRestarts: -1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newCapacityEnv(t, false, "template-a")
			e.cfg.Daemon.SessionCircuitBreaker = true
			e.cfg.Daemon.SessionCircuitBreakerMaxRestarts = intPtrCircuit(5)
			e.cfg.Daemon.SessionCircuitBreakerWindow = "30m"
			cb := breakerAt(30*time.Minute, 5)
			setSessionCircuitBreakerForTest(t, cb)
			now := e.clk.Now().UTC()
			if tc.seed != nil {
				tc.seed(cb, now)
			}

			named := createCircuitTestNamedSessionWithIdentity(t, e.reconcilerTestEnv, "session-a", "template-a", identity, "asleep")
			c := startCandidate{info: e.sessionInfo(named.ID), tp: TemplateParams{Command: "test-cmd", SessionName: "session-a", TemplateName: "template-a"}}
			moved := &prepareMoveStore{Store: e.store, id: named.ID, mintRead: make(chan struct{}), unblock: make(chan struct{})}
			var unblockOnce sync.Once
			release := func() { unblockOnce.Do(func() { close(moved.unblock) }) }
			defer release()

			var tracker asyncStartTracker
			opts := append([]startExecutionOption(nil), e.startOptions...)
			opts = append(opts, withAsyncStartExecution(), withAsyncStartTracker(&tracker))

			returned := make(chan int, 1)
			go func() {
				returned <- executePlannedStartsTraced(context.Background(), []startCandidate{c}, e.cfg, e.desiredState, e.sp, moved, "", e.city, e.clk, e.rec, 0, &e.log, &e.log, e.trace, opts...)
			}()
			select {
			case <-moved.mintRead:
			case <-time.After(hangBudget):
				release()
				t.Fatal("the mint's fresh re-read never reached the store")
			}
			// The admission has recorded the restart, and the refusal is one
			// fresh read away: this window is after the record and before the
			// undo.
			if tc.interfere != nil {
				tc.interfere(cb, now)
			}
			if err := sessionFrontDoor(e.store).ApplyPatch(c.info.ID, sessionpkg.MetadataPatch{"generation": "99"}); err != nil {
				t.Fatalf("moving the row: %v", err)
			}
			release()
			if woken := <-returned; woken != 1 {
				t.Fatalf("planner call woken = %d, want 1", woken)
			}
			if !tracker.wait(hangBudget) {
				t.Fatal("async start did not finish within the hang budget")
			}
			if n := e.sp.CountCalls("Start", "session-a"); n != 0 {
				t.Fatalf("Start calls = %d, want 0", n)
			}
			var snap *CircuitBreakerSnapshot
			for _, s := range cb.Snapshot(now.Add(2 * time.Second)) {
				if s.Identity == identity {
					s := s
					snap = &s
				}
			}
			switch {
			case tc.wantRestarts < 0 && snap != nil:
				t.Fatalf("breaker entry %+v after the superseded refusal, want none: the undo took back the admission's restart, or the Reset stands", *snap)
			case tc.wantRestarts >= 0 && snap == nil:
				t.Fatalf("no breaker entry after the superseded refusal, want one with %d restart(s)", tc.wantRestarts)
			case tc.wantRestarts >= 0 && snap.RestartCount != tc.wantRestarts:
				t.Fatalf("restart count = %d, want %d", snap.RestartCount, tc.wantRestarts)
			}
			if tc.wantKept && snap.LastProgress.IsZero() {
				t.Fatal("the progress observation that landed between admission and refusal was reverted")
			}
			// The row's breaker metadata follows the entry the undo left.
			persisted := e.bead(t, named.ID).Metadata[sessionCircuitRestartsMetadata]
			if kept := strings.Contains(persisted, formatCircuitTime(now)); kept != tc.wantKept {
				t.Fatalf("persisted restart history %q holds the admission's restart = %t, want %t", persisted, kept, tc.wantKept)
			}
		})
	}
}

// TestAsyncPrepareFailingEveryTimeIsNotReadmittedByItsFollowUp: a preparation
// that fails on every attempt requests one follow-up pass per failure, and
// that pass does not admit it again: its session is held out of async
// admission for one patrol interval. The controller runs a pass at once for
// each follow-up a start requests (a key-less Enqueue; tick_debounce defaults
// to 0), so a failure re-admitted by the pass it requested would fail again
// and request the next: a tight loop of reconcile passes, each with lease
// writes and a durable trace flush, that with the session circuit breaker on
// also spends the named session's restart window on a transient store error.
// The loop below is that controller, bounded so a regression fails instead of
// spinning. The first pass once the hold has ended admits it again.
func TestAsyncPrepareFailingEveryTimeIsNotReadmittedByItsFollowUp(t *testing.T) {
	e := newCapacityEnv(t, false, "worker-a")
	c := e.pendingCreate(t, "worker-a", "", nil)
	store := &prepareFailingStore{Store: e.store, err: errors.New("store unreachable during prepare")}

	var tracker asyncStartTracker
	var requested atomic.Int64
	opts := append([]startExecutionOption(nil), e.startOptions...)
	opts = append(opts, withAsyncStartExecution(), withAsyncStartTracker(&tracker),
		withAsyncStartFollowUp(func() { requested.Add(1) }))
	pass := func() int {
		t.Helper()
		woken := executePlannedStartsTraced(context.Background(), []startCandidate{e.refreshed(c)}, e.cfg, e.desiredState, e.sp, store, "", e.city, e.clk, e.rec, 0, &e.log, &e.log, e.trace, opts...)
		// The tracker stays open for the next pass: wait on the goroutines,
		// not on wait, which stops the tracker admitting.
		waitAsyncStartGoroutines(t, &tracker)
		return woken
	}

	const maxPasses = 4
	admissions, passes := 0, 0
	for passes < maxPasses {
		passes++
		before := requested.Load()
		admissions += pass()
		if requested.Load() == before {
			break
		}
	}
	log := e.log.String()
	if admissions != 1 {
		t.Fatalf("admissions = %d over the controller's passes, want 1: the failed preparation's own follow-up re-admitted it\n%s", admissions, log)
	}
	if got := requested.Load(); got != 1 || passes != 2 {
		t.Fatalf("follow-up passes requested = %d over %d passes, want 1 over 2: the failure requests one, and the pass it runs requests none\n%s", got, passes, log)
	}
	if n := e.sp.CountCalls("Start", "worker-a"); n != 0 {
		t.Fatalf("Start calls = %d, want 0", n)
	}
	if got := strings.Count(log, "session=worker-a template=worker-a outcome=failed"); got != 1 {
		t.Fatalf("failed outcome lines = %d, want 1\n%s", got, log)
	}
	if got := strings.Count(log, "session=worker-a template=worker-a outcome="+string(TraceOutcomeDeferredByPrepareFailure)); got != 1 {
		t.Fatalf("%s lines = %d, want 1 (the follow-up pass)\n%s", TraceOutcomeDeferredByPrepareFailure, got, log)
	}

	// Until the hold ends, every pass leaves the session alone.
	hold := e.cfg.Daemon.PatrolIntervalDuration()
	e.clk.Advance(hold - time.Second)
	if woken := pass(); woken != 0 {
		t.Fatalf("pass inside the hold woken = %d, want 0", woken)
	}
	e.clk.Advance(time.Second)
	if woken := pass(); woken != 1 {
		t.Fatalf("pass once the hold ended woken = %d, want 1\n%s", woken, e.log.String())
	}
	if got := strings.Count(e.log.String(), "session=worker-a template=worker-a outcome=failed"); got != 2 {
		t.Fatalf("failed outcome lines = %d, want 2 once the hold ended\n%s", got, e.log.String())
	}
	if got := requested.Load(); got != 2 {
		t.Fatalf("follow-up passes requested = %d, want 2: the second failure requests its own", got)
	}
}

// TestAsyncPrepareFailedDependencyBatchDoesNotStrandTheWave: an async pass
// yields after a batch holding a candidate whose template has depends_on, and
// counts on the batch's starts to request the follow-up pass that runs the
// rest of the wave. When every admitted start of that batch fails its
// preparation, the failures request that pass, and it starts the healthy
// candidate behind them without admitting the failed ones again. The loop is
// the controller, which runs a pass for each follow-up a start requests,
// bounded so a regression fails instead of spinning.
func TestAsyncPrepareFailedDependencyBatchDoesNotStrandTheWave(t *testing.T) {
	e := newCapacityEnv(t, false, "db", "app1", "app2", "app3", "w1")
	for i := range e.cfg.Agents {
		if strings.HasPrefix(e.cfg.Agents[i].Name, "app") {
			e.cfg.Agents[i].DependsOn = []string{"db"}
		}
	}
	// db is alive, so every candidate is in one wave; the apps are created
	// first, so wake fairness (creation time) puts all three in the first
	// batch and w1 in the next.
	if err := e.sp.Start(context.Background(), "db", runtime.Config{Command: "test-cmd"}); err != nil {
		t.Fatal(err)
	}
	failing := map[string]bool{}
	var candidates []startCandidate
	for _, name := range []string{"app1", "app2", "app3", "w1"} {
		c := e.pendingCreate(t, name, "", nil)
		if name != "w1" {
			failing[c.info.ID] = true
		}
		candidates = append(candidates, c)
	}
	store := &prepareFailingStore{Store: e.store, err: errors.New("work_dir unreadable during prepare"), only: failing}

	var tracker asyncStartTracker
	var requested atomic.Int64
	opts := append([]startExecutionOption(nil), e.startOptions...)
	opts = append(opts, withAsyncStartExecution(), withAsyncStartTracker(&tracker),
		withAsyncStartFollowUp(func() { requested.Add(1) }))

	const maxPasses = 4
	var woken []int
	for pass := 1; pass <= maxPasses; pass++ {
		before := requested.Load()
		// The controller offers what is still not running.
		var offered []startCandidate
		for _, c := range candidates {
			if !e.sp.IsRunning(c.name()) {
				offered = append(offered, e.refreshed(c))
			}
		}
		woken = append(woken, executePlannedStartsTraced(context.Background(), offered, e.cfg, e.desiredState, e.sp, store, "", e.city, e.clk, e.rec, 0, &e.log, &e.log, e.trace, opts...))
		waitAsyncStartGoroutines(t, &tracker)
		if requested.Load() == before {
			break
		}
	}
	log := e.log.String()
	if len(woken) == 0 || woken[0] != defaultStartDependencyRecheckBatchSize {
		t.Fatalf("first pass woken = %v, want the %d dependency-sensitive admissions it yields after\n%s", woken, defaultStartDependencyRecheckBatchSize, log)
	}
	if n := e.sp.CountCalls("Start", "w1"); n != 1 {
		t.Fatalf("w1 Start calls = %d over passes woken %v, want 1: the failed batch stranded the rest of its wave\n%s", n, woken, log)
	}
	for _, name := range []string{"app1", "app2", "app3"} {
		if n := e.sp.CountCalls("Start", name); n != 0 {
			t.Fatalf("%s Start calls = %d, want 0", name, n)
		}
		if got := strings.Count(log, "session="+name+" template="+name+" outcome=failed"); got != 1 {
			t.Fatalf("%s failed outcome lines = %d, want 1: the pass its failure requested admitted it again\n%s", name, got, log)
		}
		if !strings.Contains(log, "session="+name+" template="+name+" outcome="+string(TraceOutcomeDeferredByPrepareFailure)) {
			t.Fatalf("no deferred_by_prepare_failure line for %s, held after its failed preparation\n%s", name, log)
		}
	}
}

// TestAsyncPrepareFailureHoldKeepsTheWakeBudgetForOthers: an async admission
// spends a wake before it prepares, and a failed preparation leaves its
// session at the front of wake-fairness order. Its hold keeps it out of the
// passes that follow the failure, so the candidate its admission deferred by
// the wake budget starts on the next pass instead of losing the budget to it
// on every pass; the next pass after the hold ends admits it again.
func TestAsyncPrepareFailureHoldKeepsTheWakeBudgetForOthers(t *testing.T) {
	e := newCapacityEnv(t, false, "f1", "h1")
	e.limitToOneWake()
	f1 := e.pendingCreate(t, "f1", "", nil)
	h1 := e.pendingCreate(t, "h1", "", nil)
	store := &prepareFailingStore{Store: e.store, err: errors.New("work_dir unreadable during prepare"), only: map[string]bool{f1.info.ID: true}}

	var tracker asyncStartTracker
	opts := append([]startExecutionOption(nil), e.startOptions...)
	opts = append(opts, withAsyncStartExecution(), withAsyncStartTracker(&tracker))
	pass := func(candidates ...startCandidate) int {
		t.Helper()
		var offered []startCandidate
		for _, c := range candidates {
			offered = append(offered, e.refreshed(c))
		}
		woken := executePlannedStartsTraced(context.Background(), offered, e.cfg, e.desiredState, e.sp, store, "", e.city, e.clk, e.rec, 0, &e.log, &e.log, e.trace, opts...)
		waitAsyncStartGoroutines(t, &tracker)
		return woken
	}

	if woken := pass(f1, h1); woken != 1 {
		t.Fatalf("first pass woken = %d, want 1 (f1 spends the one wake)", woken)
	}
	if got := e.log.String(); !strings.Contains(got, "session=h1 template=h1 outcome=deferred_by_wake_budget") {
		t.Fatalf("h1 was not deferred by the wake budget f1's admission spent:\n%s", got)
	}
	// The next pass, whatever runs it, finds f1 held.
	if woken := pass(f1, h1); woken != 1 {
		t.Fatalf("second pass woken = %d, want 1 (h1)", woken)
	}
	log := e.log.String()
	if n := e.sp.CountCalls("Start", "h1"); n != 1 {
		t.Fatalf("h1 Start calls = %d, want 1: the failing f1 took the wake budget again\n%s", n, log)
	}
	if got := strings.Count(log, "session=f1 template=f1 outcome=failed"); got != 1 {
		t.Fatalf("f1 failed outcome lines = %d, want 1 while it is held\n%s", got, log)
	}
	if !strings.Contains(log, "session=f1 template=f1 outcome="+string(TraceOutcomeDeferredByPrepareFailure)) {
		t.Fatalf("no deferred_by_prepare_failure line for the held f1:\n%s", log)
	}

	// One patrol interval after the failure the hold has ended.
	e.clk.Advance(e.cfg.Daemon.PatrolIntervalDuration())
	if woken := pass(f1); woken != 1 {
		t.Fatalf("pass after the hold woken = %d, want 1: f1 is admitted again once its hold ends\n%s", woken, e.log.String())
	}
	if got := strings.Count(e.log.String(), "session=f1 template=f1 outcome=failed"); got != 2 {
		t.Fatalf("f1 failed outcome lines = %d, want 2 after the hold ended\n%s", got, e.log.String())
	}
}

// TestAsyncStartTrackerPreparationHold: a failed preparation's hold is keyed
// by the session's bead ID and holds until its end, exclusive. A hold that
// ended is forgotten by the next read of it or the next hold recorded, and a
// nil tracker or an empty ID records and holds nothing.
func TestAsyncStartTrackerPreparationHold(t *testing.T) {
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	end := now.Add(time.Minute)
	var tracker asyncStartTracker
	if tracker.holdFailedPreparation(" ", now, end) {
		t.Fatal("an empty id recorded a hold")
	}
	if !tracker.holdFailedPreparation("gc-1", now, end) {
		t.Fatal("the hold of gc-1 was not recorded")
	}
	if until, held := tracker.preparationHeldUntil("gc-1", end.Add(-time.Nanosecond)); !held || !until.Equal(end) {
		t.Fatalf("gc-1 just before its end: held = %t until %s, want held until %s", held, until, end)
	}
	if _, held := tracker.preparationHeldUntil("gc-2", now); held {
		t.Fatal("gc-2 is held by gc-1's hold")
	}
	if _, held := tracker.preparationHeldUntil("gc-1", end); held {
		t.Fatal("gc-1 is still held at its end")
	}
	if n := len(tracker.preparationHolds); n != 0 {
		t.Fatalf("holds kept = %d after the read of an ended one, want 0", n)
	}
	// A hold recorded later forgets the ones that ended by then, read or not.
	tracker.holdFailedPreparation("gc-1", now, end)
	tracker.holdFailedPreparation("gc-3", end, end.Add(time.Minute))
	if _, kept := tracker.preparationHolds["gc-1"]; kept || len(tracker.preparationHolds) != 1 {
		t.Fatalf("holds after a later one = %v, want only gc-3's", tracker.preparationHolds)
	}
	var none *asyncStartTracker
	if none.holdFailedPreparation("gc-1", now, end) {
		t.Fatal("a nil tracker recorded a hold")
	}
	if _, held := none.preparationHeldUntil("gc-1", now); held {
		t.Fatal("a nil tracker holds a session")
	}
}

// TestAsyncPrepareFailureHoldIsOnePatrolInterval: the hold is the city's
// patrol interval, the patrol default without a config.
func TestAsyncPrepareFailureHoldIsOnePatrolInterval(t *testing.T) {
	if got := asyncPrepareFailureHold(nil); got != 30*time.Second {
		t.Fatalf("hold without a config = %s, want the 30s patrol default", got)
	}
	cfg := &config.City{Daemon: config.DaemonConfig{PatrolInterval: "15s"}}
	if got := asyncPrepareFailureHold(cfg); got != 15*time.Second {
		t.Fatalf("hold = %s, want the city's 15s patrol interval", got)
	}
}

// TestAsyncStartLeavesTheRegistryBeforeItsFollowUp: a start that ran removes
// its admitted-start registry entry before it requests its follow-up pass, so
// the pass it asks for sees the committed dependency as it is: an entry still
// in place would read the dependency as in flight and keep its dependents
// blocked_on_dependencies until a later patrol or event.
func TestAsyncStartLeavesTheRegistryBeforeItsFollowUp(t *testing.T) {
	e := newCapacityEnv(t, false, "db", "app")
	for i := range e.cfg.Agents {
		if e.cfg.Agents[i].Name == "app" {
			e.cfg.Agents[i].DependsOn = []string{"db"}
		}
	}
	db := e.pendingCreate(t, "db", "", nil)

	var tracker asyncStartTracker
	var registered, dependencyAlive bool
	followedUp := make(chan struct{})
	opts := append([]startExecutionOption(nil), e.startOptions...)
	opts = append(opts, withAsyncStartExecution(), withAsyncStartTracker(&tracker),
		withAsyncStartFollowUp(func() {
			// What the requested pass's dependency check reads.
			registered = tracker.startInFlight("db")
			dependencyAlive = allDependenciesAliveForTemplateWithClock("app", e.cfg, e.desiredState, e.sp, "", e.store, e.clk, tracker.startInFlight)
			close(followedUp)
		}))
	if woken := executePlannedStartsTraced(context.Background(), []startCandidate{db}, e.cfg, e.desiredState, e.sp, e.store, "", e.city, e.clk, e.rec, 0, &e.log, &e.log, e.trace, opts...); woken != 1 {
		t.Fatalf("woken = %d, want 1", woken)
	}
	select {
	case <-followedUp:
	case <-time.After(hangBudget):
		t.Fatal("the start requested no follow-up within the hang budget")
	}
	waitAsyncStartGoroutines(t, &tracker)
	if n := e.sp.CountCalls("Start", "db"); n != 1 {
		t.Fatalf("db Start calls = %d, want 1", n)
	}
	if registered {
		t.Fatal("db was still in the admitted-start registry when its follow-up was requested")
	}
	if !dependencyAlive {
		t.Fatalf("the follow-up pass would find app's dependency db not alive after db committed\n%s", e.log.String())
	}
}

// TestAsyncPrepareFailedProbeStillDefersSameEndpoint: a pass that admitted
// an open endpoint's one probe defers the endpoint's other candidate in that
// pass whether or not the probe's off-tick preparation succeeds, and the
// failed probe hands its ticket back: the endpoint keeps a probe to give, so
// the next pass is not wedged behind an admission that never reached Start.
func TestAsyncPrepareFailedProbeStillDefersSameEndpoint(t *testing.T) {
	e := newCapacityEnv(t, true, "s1", "s2")
	cands := []startCandidate{e.pendingCreate(t, "s1", "e", nil), e.pendingCreate(t, "s2", "e", nil)}
	e.openEndpoint(t)
	e.makeProbeDue()

	var failed asyncStartTracker
	opts := append([]startExecutionOption(nil), e.startOptions...)
	opts = append(opts, withAsyncStartExecution(), withAsyncStartTracker(&failed))
	store := &prepareFailingStore{Store: e.store, err: errors.New("store unreachable during prepare")}

	if woken := executePlannedStartsTraced(context.Background(), cands, e.cfg, e.desiredState, e.sp, store, "", e.city, e.clk, e.rec, 0, &e.log, &e.log, nil, opts...); woken != 1 {
		t.Fatalf("woken = %d, want 1 (only the probe is admitted)", woken)
	}
	if !failed.wait(hangBudget) {
		t.Fatal("async start did not finish within the hang budget")
	}
	starts := e.sp.CountCalls("Start", "s1") + e.sp.CountCalls("Start", "s2")
	if starts != 0 {
		t.Fatalf("Start calls = %+v, want none: the probe's preparation failed and the other candidate was deferred", e.sp.SnapshotCalls())
	}
	// The deferral was decided on the tick, before the probe prepared, so it
	// stands whatever the preparation does.
	got := e.log.String()
	for _, want := range []string{
		"session=s2 template=s2 outcome=deferred_by_endpoint_capacity",
		"session=s1 template=s1 outcome=start_enqueued",
		"session=s1 template=s1 outcome=failed",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("lifecycle log lacks %q:\n%s", want, got)
		}
	}
	// The probe never reached Start, so its ticket was abandoned as not
	// attempted: that is no capacity evidence, and the half-open endpoint
	// keeps its probe to give.
	if st := e.breakerStatus(capacityTestEndpoint); st.State != resilience.StateHalfOpen {
		t.Fatalf("breaker = %v after the failed probe's preparation, want half-open", st.State)
	}

	e.startAsync(t, e.refreshed(cands[0]), e.refreshed(cands[1]))
	starts = e.sp.CountCalls("Start", "s1") + e.sp.CountCalls("Start", "s2")
	if starts != 1 {
		t.Fatalf("Start calls on the half-open endpoint = %d, want exactly one probe", starts)
	}
	if st := e.breakerStatus(capacityTestEndpoint); st.State != resilience.StateClosed {
		t.Fatalf("breaker = %v after a successful probe, want closed", st.State)
	}
}

// prepareTimedStore advances the test clock by step on every raw store read
// or write made from inside prepareStartCandidateForCity, so each preparation
// phase costs time a trace field can carry. err fails those row reads
// instead.
type prepareTimedStore struct {
	beads.Store
	clk  *clock.Fake
	step time.Duration
	err  error
}

// toll charges one in-prepare store call to the clock.
func (s *prepareTimedStore) toll() {
	if inPrepare() {
		s.clk.Advance(s.step)
	}
}

func (s *prepareTimedStore) Get(id string) (beads.Bead, error) {
	if !inPrepare() {
		return s.Store.Get(id)
	}
	s.toll()
	if s.err != nil {
		return beads.Bead{}, s.err
	}
	return s.Store.Get(id)
}

func (s *prepareTimedStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	s.toll()
	return s.Store.List(query)
}

func (s *prepareTimedStore) Update(id string, opts beads.UpdateOpts) error {
	s.toll()
	return s.Store.Update(id, opts)
}

func (s *prepareTimedStore) SetMetadataBatch(id string, kvs map[string]string) error {
	s.toll()
	return s.Store.SetMetadataBatch(id, kvs)
}

// traceFieldMS reads a millisecond trace field from a record, in memory
// (int64) or read back from the durable trace (float64). ok is false when
// the record does not carry the field.
func traceFieldMS(t *testing.T, rec SessionReconcilerTraceRecord, field string) (value int64, ok bool) {
	t.Helper()
	v, ok := rec.Fields[field]
	if !ok {
		return 0, false
	}
	switch n := v.(type) {
	case int64:
		return n, true
	case float64:
		return int64(n), true
	}
	t.Fatalf("trace field %s = %v (%T), want a number", field, v, v)
	return 0, true
}

// TestStartTraceCarriesPrepareTimings: a start's final-outcome
// reconciler.start.execute record carries its preparation's phase timings,
// read back from the durable trace, in async and sync mode whether the
// preparation succeeded or failed. A phase that did not run is absent, a
// configured prepare budget adds the pass's charge, and the lifecycle log's
// phases segment shows the phases that ran.
func TestStartTraceCarriesPrepareTimings(t *testing.T) {
	const step = 3 * time.Millisecond
	const tracePhases = "lock_get_ms prewake_ms named_snapshot_ms build_ms"
	for _, tc := range []struct {
		name        string
		async       bool
		fails       bool
		noBudget    bool
		woken       int
		starts      int
		wantOutcome TraceOutcomeCode
	}{
		{name: "async_success", async: true, woken: 1, starts: 1, wantOutcome: TraceOutcomeSuccess},
		{name: "async_failure", async: true, fails: true, woken: 1, wantOutcome: TraceOutcomeFailed},
		{name: "sync_success", woken: 1, starts: 1, wantOutcome: TraceOutcomeSuccess},
		{name: "sync_success_without_budget", noBudget: true, woken: 1, starts: 1, wantOutcome: TraceOutcomeSuccess},
		{name: "sync_failure", fails: true, wantOutcome: TraceOutcomeFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newCapacityEnv(t, false, "template-a")
			tr := newAsyncPrepareTrace(t, e, "template-a")
			timed := &prepareTimedStore{Store: e.store, clk: e.clk, step: step}
			// The row re-read is preparation's first store call, so a store
			// error ends it inside its first phase.
			ran := map[string]bool{"lock_get_ms": true}
			if tc.fails {
				timed.err = errors.New("store unreachable during prepare")
			} else {
				ran = map[string]bool{"lock_get_ms": true, "prewake_ms": true, "named_snapshot_ms": true, "build_ms": true}
			}
			opts := append([]startExecutionOption(nil), e.startOptions...)
			var tracker asyncStartTracker
			if tc.async {
				opts = append(opts, withAsyncStartExecution(), withAsyncStartTracker(&tracker))
			} else if !tc.noBudget {
				opts = append(opts, withPrepareBudget(time.Hour))
			}

			woken := executePlannedStartsTraced(context.Background(), []startCandidate{e.namedCandidate(t)}, e.cfg, e.desiredState, e.sp, timed, "", e.city, e.clk, e.rec, 0, &e.log, &e.log, tr.cycle, opts...)
			if woken != tc.woken {
				t.Fatalf("woken = %d, want %d", woken, tc.woken)
			}
			if tc.async {
				if !tracker.wait(hangBudget) {
					t.Fatal("async start did not finish within the hang budget")
				}
			} else if err := tr.cycle.End(TraceCompletionCompleted, traceRecordPayload{"phase": "tick"}); err != nil {
				t.Fatalf("ending the trace cycle: %v", err)
			}
			if n := e.sp.CountCalls("Start", "session-a"); n != tc.starts {
				t.Fatalf("Start calls = %d, want %d", n, tc.starts)
			}

			var finals []SessionReconcilerTraceRecord
			for _, rec := range tr.startExecuteRecords(t) {
				if rec.OutcomeCode != TraceOutcomeStartEnqueued {
					finals = append(finals, rec)
				}
			}
			if len(finals) != 1 {
				t.Fatalf("final-outcome reconciler.start.execute records = %d, want 1", len(finals))
			}
			rec := finals[0]
			if rec.OutcomeCode != tc.wantOutcome || rec.SessionName != "session-a" {
				t.Fatalf("final record = %s %s, want %s session-a", rec.OutcomeCode, rec.SessionName, tc.wantOutcome)
			}

			// A phase is on the record exactly when it ran, and its cost is
			// whole store calls; prepare_ms is their sum.
			var total int64
			for _, phase := range strings.Fields(tracePhases) {
				value, ok := traceFieldMS(t, rec, phase)
				if ok != ran[phase] {
					t.Fatalf("%s on the record = %t, want %t", phase, ok, ran[phase])
				}
				if !ok {
					continue
				}
				if value < step.Milliseconds() {
					t.Fatalf("%s = %d, want at least the %s one in-prepare store call costs", phase, value, step)
				}
				total += value
			}
			if got, ok := traceFieldMS(t, rec, "prepare_ms"); !ok || got != total {
				t.Fatalf("prepare_ms = %d (on the record %t), want %d", got, ok, total)
			}
			// A synchronous preparation charges the budget as it runs, so the
			// pass's charge is this start's; an async one charges nothing, and
			// a pass without a budget reports no charge.
			budgeted := !tc.async && !tc.noBudget
			spent, ok := traceFieldMS(t, rec, "spent_ms")
			if ok != budgeted || (budgeted && spent != total) {
				t.Fatalf("spent_ms = %d (on the record %t), want on the record %t", spent, ok, budgeted)
			}

			// The lifecycle line's phases segment names the same phases.
			wantLine := "session=session-a template=template-a outcome=" + string(tc.wantOutcome)
			for _, line := range strings.Split(e.log.String(), "\n") {
				if !strings.Contains(line, wantLine) {
					continue
				}
				for _, phase := range []string{"lock_get", "prewake", "named_snapshot", "build"} {
					if _, onRecord := rec.Fields[phase+"_ms"]; strings.Contains(line, phase+"=") != onRecord {
						t.Fatalf("lifecycle line reports %s= %t, want %t: %s", phase, strings.Contains(line, phase+"="), onRecord, line)
					}
				}
				return
			}
			t.Fatalf("no lifecycle line for %s:\n%s", wantLine, e.log.String())
		})
	}
}

// TestPrepareTimingsRecordPhasesThatRan: an unnamed session's preparation on a
// clock that does not move runs the locked re-read, PreWake and build in no
// measurable time and never refreshes a named session, so its record carries
// lock_get_ms, prewake_ms and build_ms as 0 and no named_snapshot_ms.
func TestPrepareTimingsRecordPhasesThatRan(t *testing.T) {
	e := newCapacityEnv(t, false, "worker-a")
	c := e.pendingCreate(t, "worker-a", "", nil)
	if refreshesNamedStartCandidate(c, e.cfg) {
		t.Fatal("test candidate is a configured named session; want an unnamed one")
	}
	prepared, timings, err := prepareStartCandidateForCity(c, "", "", e.cfg, nil, e.store, e.clk, io.Discard, nil, dispatchOptionSources{})
	if err != nil || prepared == nil {
		t.Fatalf("prepareStartCandidateForCity = %v, %v; want a prepared start", prepared, err)
	}
	fields := traceRecordPayload{}
	addPrepareTimingFields(fields, timings, nil)
	for _, field := range []string{"lock_get_ms", "prewake_ms", "build_ms", "prepare_ms"} {
		if got, ok := fields[field]; !ok || got != int64(0) {
			t.Fatalf("%s = %v (on the record %t), want 0: the phase ran in no clock time", field, got, ok)
		}
	}
	if got, ok := fields["named_snapshot_ms"]; ok {
		t.Fatalf("named_snapshot_ms = %v on the record, want absent: no named refresh ran", got)
	}
}
