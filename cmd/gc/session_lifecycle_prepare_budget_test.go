package main

import (
	"context"
	"maps"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/resilience"
	gcruntime "github.com/gastownhall/gascity/internal/runtime"
)

// The cumulative prepare-time budget (ga-r7s62t, Refs #7170): one planner call
// stops preparing new candidates once the calls to prepareStartCandidateForCity
// have together spent the budget, and defers the rest to the next tick before
// any slot, lease, ticket, breaker accounting or write.

const (
	prepareBudgetOutcome = "deferred_by_prepare_budget"
	prepareBudgetReason  = "prepare_budget_spent"
	testPrepareBudget    = 60 * time.Second
)

// budgetClock is a clock.Fake safe for the async start goroutines, which read
// the clock while the executor advances it. clock.Fake has no mutex and races
// under -race.
type budgetClock struct {
	mu sync.Mutex
	f  *clock.Fake
}

func (c *budgetClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.f.Now()
}

func (c *budgetClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.f.Advance(d)
}

// prepareLatencyStore makes a session bead's prepare slow: the first Get of an
// id in latency made from inside prepareStartCandidateForCity advances the
// clock by that duration, and every such Get of an id in failGet fails. Gets from
// elsewhere in the pass (the runtime lease's fresh read, the commit) are free,
// so the cost of a prepare is exact whatever else reads the row. The first
// metadata batch written to an id in writeLatency from OUTSIDE prepare (the
// identity breaker's restart accounting) advances the clock the same way: time
// the pass spends between the budget gate and prepare, which is not prepare time.
type prepareLatencyStore struct {
	beads.Store
	clk          *budgetClock
	mu           sync.Mutex
	latency      map[string]time.Duration
	failGet      map[string]error
	charged      map[string]bool
	writeLatency map[string]time.Duration
}

// inPrepare reports whether the calling goroutine is inside
// prepareStartCandidateForCity.
func inPrepare() bool {
	pcs := make([]uintptr, 32)
	frames := runtime.CallersFrames(pcs[:runtime.Callers(2, pcs)])
	for {
		f, more := frames.Next()
		if strings.HasSuffix(f.Function, ".prepareStartCandidateForCity") || strings.HasSuffix(f.Function, ".prepareStartCandidateForCityTimed") {
			return true
		}
		if !more {
			return false
		}
	}
}

func (s *prepareLatencyStore) Get(id string) (beads.Bead, error) {
	s.mu.Lock()
	var d time.Duration
	var err error
	if inPrepare() {
		if !s.charged[id] {
			d = s.latency[id]
			s.charged[id] = true
		}
		err = s.failGet[id]
	}
	s.mu.Unlock()
	if d != 0 {
		s.clk.Advance(d)
	}
	if err != nil {
		return beads.Bead{}, err
	}
	return s.Store.Get(id)
}

// SetMetadataBatch charges the first write of an id in writeLatency made from
// outside prepareStartCandidateForCity.
func (s *prepareLatencyStore) SetMetadataBatch(id string, kvs map[string]string) error {
	s.mu.Lock()
	var d time.Duration
	if !inPrepare() {
		d = s.writeLatency[id]
		delete(s.writeLatency, id)
	}
	s.mu.Unlock()
	if d != 0 {
		s.clk.Advance(d)
	}
	return s.Store.SetMetadataBatch(id, kvs)
}

// prepareBudgetEnv is a capacityEnv whose store injects prepare latency and
// whose clock is goroutine-safe.
type prepareBudgetEnv struct {
	*capacityEnv
	lat *prepareLatencyStore
	clk *budgetClock
}

func newPrepareBudgetEnv(t *testing.T, names ...string) *prepareBudgetEnv {
	t.Helper()
	return withPrepareLatency(newCapacityEnv(t, false, names...))
}

// newGuardedPrepareBudgetEnv is newPrepareBudgetEnv over an endpoint-guarded
// capacityEnv, so a candidate on an upstream asks the endpoint for a ticket.
func newGuardedPrepareBudgetEnv(t *testing.T, names ...string) *prepareBudgetEnv {
	t.Helper()
	return withPrepareLatency(newCapacityEnv(t, true, names...))
}

func withPrepareLatency(e *capacityEnv) *prepareBudgetEnv {
	lc := &budgetClock{f: e.clk}
	lat := &prepareLatencyStore{
		Store:        e.store,
		clk:          lc,
		latency:      map[string]time.Duration{},
		failGet:      map[string]error{},
		charged:      map[string]bool{},
		writeLatency: map[string]time.Duration{},
	}
	maxWakes := 10
	e.cfg.Daemon.MaxWakesPerTick = &maxWakes
	return &prepareBudgetEnv{capacityEnv: e, lat: lat, clk: lc}
}

// candidate stores a never-started pending create for name whose prepare costs
// d of clock time.
func (e *prepareBudgetEnv) candidate(t *testing.T, name string, d time.Duration) startCandidate {
	t.Helper()
	c := e.pendingCreate(t, name, "", nil)
	e.lat.latency[c.info.ID] = d
	return c
}

// run is one planner call with the prepare budget and any extra options.
func (e *prepareBudgetEnv) run(budget time.Duration, async bool, extra []startExecutionOption, candidates ...startCandidate) int {
	return e.runWith(append(prepareBudgetOpts(budget), extra...), async, candidates...)
}

// runWith is one planner call with exactly the given extra options.
func (e *prepareBudgetEnv) runWith(extra []startExecutionOption, async bool, candidates ...startCandidate) int {
	opts := append([]startExecutionOption(nil), e.startOptions...)
	opts = append(opts, extra...)
	var tracker asyncStartTracker
	if async {
		opts = append(opts, withAsyncStartExecution(), withAsyncStartTracker(&tracker))
	}
	woken := executePlannedStartsTraced(context.Background(), candidates, e.cfg, e.desiredState, e.sp, e.lat, "", e.city, e.clk, e.rec, 0, &e.log, &e.log, e.trace, opts...)
	if async && !tracker.wait(hangBudget) {
		panic("async starts did not finish within the hang budget")
	}
	return woken
}

func (e *prepareBudgetEnv) deferredByBudget() []string {
	var names []string
	for _, r := range e.records(TraceSiteLifecycleStartRun) {
		if string(r.OutcomeCode) == prepareBudgetOutcome && string(r.ReasonCode) == prepareBudgetReason {
			names = append(names, r.SessionName)
		}
	}
	return names
}

// requireDeferred asserts the named candidates were deferred by the budget:
// logged on stderr and recorded as trace decisions. An async pass flushes the
// cycle's records after enqueueing its starts, so there the decisions are
// counted instead of read back.
func (e *prepareBudgetEnv) requireDeferred(t *testing.T, async bool, want ...string) {
	t.Helper()
	if async {
		e.trace.mu.Lock()
		got := e.trace.decisionCounts[string(TraceSiteLifecycleStartRun)]
		e.trace.mu.Unlock()
		if got != len(want) {
			t.Fatalf("lifecycle_start_run trace decisions = %d, want %d\n%s", got, len(want), e.log.String())
		}
	} else if got := e.deferredByBudget(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("trace deferred_by_prepare_budget = %v, want %v\n%s", got, want, e.log.String())
	}
	for _, name := range want {
		line := "session=" + name + " "
		var logged bool
		for _, l := range strings.Split(e.log.String(), "\n") {
			if strings.Contains(l, line) && strings.Contains(l, "outcome="+prepareBudgetOutcome) {
				logged = true
			}
		}
		if !logged {
			t.Fatalf("stderr has no outcome=%s line for %s:\n%s", prepareBudgetOutcome, name, e.log.String())
		}
	}
}

// requireUntouched fails when a deferred candidate was started or written.
func (e *prepareBudgetEnv) requireUntouched(t *testing.T, c startCandidate, before beads.Bead) {
	t.Helper()
	name := c.info.SessionName
	if n := e.sp.CountCalls("Start", name); n != 0 {
		t.Fatalf("deferred %s was started %d times", name, n)
	}
	after := e.bead(t, c.info.ID)
	if after.Status != before.Status || !maps.Equal(after.Metadata, before.Metadata) {
		t.Fatalf("deferred %s was written:\nbefore %v\nafter  %v", name, before.Metadata, after.Metadata)
	}
}

// Kills: no bound on the cumulative time a planner call spends preparing.
func TestExecutePlannedStarts_PrepareBudgetDefersRestOnceSpent(t *testing.T) {
	e := newPrepareBudgetEnv(t, "a", "b", "c")
	a := e.candidate(t, "a", testPrepareBudget+time.Second)
	b := e.candidate(t, "b", time.Second)
	c := e.candidate(t, "c", time.Second)
	bBefore, cBefore := e.bead(t, b.info.ID), e.bead(t, c.info.ID)

	if woken := e.run(testPrepareBudget, false, nil, a, b, c); woken != 1 {
		t.Fatalf("woken = %d, want 1 (only a is prepared)", woken)
	}
	if n := e.sp.CountCalls("Start", "a"); n != 1 {
		t.Fatalf("a Start calls = %d, want 1", n)
	}
	e.requireDeferred(t, false, "b", "c")
	e.requireUntouched(t, b, bBefore)
	e.requireUntouched(t, c, cBefore)
	if n := strings.Count(e.log.String(), "prepare budget exhausted"); n != 1 {
		t.Fatalf("summary lines = %d, want exactly one per planner call:\n%s", n, e.log.String())
	}
}

// An async batch admits candidates before preparation charges its meter.
// A slow prepare cannot make its peers wait for that charge.
func TestExecutePlannedStarts_AsyncPrepareBudgetDoesNotSerialize(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newPrepareBudgetEnv(t, "a", "b")
		a := e.candidate(t, "a", testPrepareBudget+time.Second)
		b := e.candidate(t, "b", 0)
		store := &prepareBarrierStore{Store: e.lat, id: a.info.ID, entered: make(chan struct{}), release: make(chan struct{})}
		var tracker asyncStartTracker
		defer func() {
			close(store.release)
			if !tracker.wait(hangBudget) {
				t.Error("async starts did not finish")
			}
		}()
		opts := append([]startExecutionOption(nil), e.startOptions...)
		opts = append(opts, withPrepareBudget(testPrepareBudget), withAsyncStartExecution(), withAsyncStartTracker(&tracker))
		done := make(chan int, 1)
		go func() {
			done <- executePlannedStartsTraced(context.Background(), []startCandidate{a, b}, e.cfg, e.desiredState, e.sp, store, "", e.city, e.clk, e.rec, 0, &e.log, &e.log, nil, opts...)
		}()
		synctest.Wait()
		select {
		case n := <-done:
			if n != 2 {
				t.Fatalf("enqueued = %d, want 2", n)
			}
		default:
			t.Fatal("budget serialized async prepares")
		}
		if aStarts, bStarts := e.sp.CountCalls("Start", "a"), e.sp.CountCalls("Start", "b"); aStarts != 0 || bStarts != 1 {
			t.Fatalf("Start calls while a prepares: a=%d b=%d, want 0,1", aStarts, bStarts)
		}
	})
}

// The first candidate of a planner call is always prepared, even when the
// budget is smaller than any single prepare.
func TestExecutePlannedStarts_PrepareBudgetAlwaysAdmitsFirstCandidate(t *testing.T) {
	e := newPrepareBudgetEnv(t, "a")
	a := e.candidate(t, "a", 10*time.Minute)

	if woken := e.run(time.Nanosecond, false, nil, a); woken != 1 {
		t.Fatalf("woken = %d, want 1: the first candidate is always admitted", woken)
	}
	if got := e.deferredByBudget(); len(got) != 0 {
		t.Fatalf("deferred = %v, want none", got)
	}
}

// Kills: a budget that defers work that fit.
func TestExecutePlannedStarts_PrepareBudgetWithinBudgetStartsEverything(t *testing.T) {
	names := []string{"a", "b", "c", "d", "e"}
	e := newPrepareBudgetEnv(t, names...)
	var candidates []startCandidate
	for _, n := range names {
		candidates = append(candidates, e.candidate(t, n, time.Second))
	}

	if woken := e.run(testPrepareBudget, false, nil, candidates...); woken != len(names) {
		t.Fatalf("woken = %d, want %d", woken, len(names))
	}
	if got := e.deferredByBudget(); len(got) != 0 {
		t.Fatalf("deferred = %v, want none within budget", got)
	}
	if strings.Contains(e.log.String(), "prepare budget exhausted") {
		t.Fatalf("summary logged within budget:\n%s", e.log.String())
	}
}

// The spend carries across the dependency-recheck batches of one planner call.
func TestExecutePlannedStarts_PrepareBudgetCarriesAcrossBatches(t *testing.T) {
	names := []string{"a", "b", "c", "d", "e", "f"}
	e := newPrepareBudgetEnv(t, names...)
	var candidates []startCandidate
	for _, n := range names {
		// Three candidates per batch; the first batch spends the budget.
		candidates = append(candidates, e.candidate(t, n, 30*time.Second))
	}

	woken := e.run(testPrepareBudget, false, nil, candidates...)
	if woken < 2 || woken > 3 {
		t.Fatalf("woken = %d, want the candidates prepared before the budget ran out", woken)
	}
	if len(e.deferredByBudget()) != len(names)-woken {
		t.Fatalf("deferred = %v, want the remaining %d", e.deferredByBudget(), len(names)-woken)
	}
	for _, n := range e.deferredByBudget() {
		if n == "a" || n == "b" {
			t.Fatalf("%s was deferred but fit the budget", n)
		}
	}
}

// The spend carries across dependency waves in sync mode: b waits for a to be
// running, a's slow prepare spends the budget, so b's wave defers.
func TestExecutePlannedStarts_PrepareBudgetCarriesAcrossWaves(t *testing.T) {
	e := newPrepareBudgetEnv(t, "a", "b")
	e.cfg.Agents = []config.Agent{{Name: "a"}, {Name: "b", DependsOn: []string{"a"}}}
	a := e.candidate(t, "a", testPrepareBudget+time.Second)
	b := e.candidate(t, "b", time.Second)
	bBefore := e.bead(t, b.info.ID)

	if woken := e.run(testPrepareBudget, false, nil, a, b); woken != 1 {
		t.Fatalf("woken = %d, want 1", woken)
	}
	e.requireDeferred(t, false, "b")
	e.requireUntouched(t, b, bBefore)
}

// A prepare that fails still cost its time.
func TestExecutePlannedStarts_PrepareBudgetCountsSlowFailingPrepare(t *testing.T) {
	e := newPrepareBudgetEnv(t, "a", "b")
	a := e.candidate(t, "a", testPrepareBudget+time.Second)
	e.lat.failGet[a.info.ID] = os.ErrDeadlineExceeded
	b := e.candidate(t, "b", time.Second)
	bBefore := e.bead(t, b.info.ID)

	if woken := e.run(testPrepareBudget, false, nil, a, b); woken != 0 {
		t.Fatalf("woken = %d, want 0 (a's prepare failed, b deferred)", woken)
	}
	if !strings.Contains(e.log.String(), "session=a template=a outcome=failed") {
		t.Fatalf("a's failed prepare not logged:\n%s", e.log.String())
	}
	e.requireDeferred(t, false, "b")
	e.requireUntouched(t, b, bBefore)
}

// A deferral is not a failure: the deferred candidate's bead row and in-memory
// info do not move, while the same candidate within budget does write (the
// contrast that proves the check is not vacuous). This test uses an unnamed
// candidate in a breaker-disabled harness; the breaker accounting and the
// named-session row are covered by ..._DeferralRecordsNoBreakerRestart.
func TestExecutePlannedStarts_PrepareBudgetDeferralWritesNothingButStartDoes(t *testing.T) {
	deferred := newPrepareBudgetEnv(t, "a", "b")
	a := deferred.candidate(t, "a", testPrepareBudget+time.Second)
	b := deferred.candidate(t, "b", time.Second)
	bBefore := deferred.bead(t, b.info.ID)
	inMemory := b.info
	deferred.run(testPrepareBudget, false, nil, a, b)
	deferred.requireUntouched(t, b, bBefore)
	if b.info.State != inMemory.State || b.info.Generation != inMemory.Generation {
		t.Fatalf("in-memory candidate mutated: %+v", b.info)
	}

	within := newPrepareBudgetEnv(t, "a", "b")
	wa := within.candidate(t, "a", time.Second)
	wb := within.candidate(t, "b", time.Second)
	wbBefore := within.bead(t, wb.info.ID)
	within.run(testPrepareBudget, false, nil, wa, wb)
	if after := within.bead(t, wb.info.ID); maps.Equal(after.Metadata, wbBefore.Metadata) {
		t.Fatal("contrast: a start within budget wrote nothing, so the unchanged-row assertion proves nothing")
	}
}

// The gate sits after the on-death and dependency gates: a candidate they
// already defer keeps its outcome when the budget is also spent.
func TestExecutePlannedStarts_PrepareBudgetGateOrder(t *testing.T) {
	e := newPrepareBudgetEnv(t, "a", "hooked")
	gate := newOnDeathGate()
	gate.enqueue([]deathEdge{{name: "hooked"}})
	a := e.candidate(t, "a", testPrepareBudget+time.Second)
	hooked := e.candidate(t, "hooked", time.Second)

	e.run(testPrepareBudget, false, []startExecutionOption{withOnDeathGate(gate)}, a, hooked)

	if got := e.deferredByBudget(); len(got) != 0 {
		t.Fatalf("budget deferred %v; the on-death gate must keep its outcome", got)
	}
	if !strings.Contains(e.log.String(), "session=hooked template=hooked outcome=deferred_by_on_death_hook") {
		t.Fatalf("on-death deferral lost:\n%s", e.log.String())
	}
}

func TestExecutePlannedStarts_PrepareBudgetKeepsDependencyBlockedOutcome(t *testing.T) {
	e := newPrepareBudgetEnv(t, "a", "blocked")
	e.cfg.Agents = []config.Agent{{Name: "a"}, {Name: "blocked", DependsOn: []string{"missing-dep"}}, {Name: "missing-dep"}}
	a := e.candidate(t, "a", testPrepareBudget+time.Second)
	blocked := e.candidate(t, "blocked", time.Second)

	e.run(testPrepareBudget, false, nil, a, blocked)

	if got := e.deferredByBudget(); len(got) != 0 {
		t.Fatalf("budget deferred %v; the dependency gate must keep its outcome", got)
	}
	if !strings.Contains(e.log.String(), "session=blocked template=blocked outcome=blocked_on_dependencies") {
		t.Fatalf("dependency block lost:\n%s", e.log.String())
	}
}

// The option is opt-in: omitted, zero or negative leaves prepare unbounded,
// and the meter's edge: spent exactly at the limit defers, a nanosecond under
// does not.
func TestExecutePlannedStarts_PrepareBudgetOptInAndEdges(t *testing.T) {
	cases := []struct {
		name       string
		opts       func(*prepareBudgetEnv) []startExecutionOption
		wantWoken  int
		wantDefers []string
	}{
		{"omitted", func(*prepareBudgetEnv) []startExecutionOption { return nil }, 2, nil},
		{"zero", func(*prepareBudgetEnv) []startExecutionOption { return prepareBudgetOpts(0) }, 2, nil},
		{"negative", func(*prepareBudgetEnv) []startExecutionOption { return prepareBudgetOpts(-time.Second) }, 2, nil},
		{"exactly at the limit defers", func(*prepareBudgetEnv) []startExecutionOption { return prepareBudgetOpts(testPrepareBudget) }, 1, []string{"b"}},
		{"just under the limit admits", func(*prepareBudgetEnv) []startExecutionOption {
			return prepareBudgetOpts(testPrepareBudget + time.Nanosecond)
		}, 2, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newPrepareBudgetEnv(t, "a", "b")
			a := e.candidate(t, "a", testPrepareBudget)
			b := e.candidate(t, "b", 0)

			if woken := e.runWith(tc.opts(e), false, a, b); woken != tc.wantWoken {
				t.Fatalf("woken = %d, want %d", woken, tc.wantWoken)
			}
			if got := e.deferredByBudget(); strings.Join(got, ",") != strings.Join(tc.wantDefers, ",") {
				t.Fatalf("deferred = %v, want %v", got, tc.wantDefers)
			}
		})
	}
}

// A clock that moves backwards during prepare charges nothing: the spend
// never goes negative and cannot cancel an earlier charge.
func TestExecutePlannedStarts_PrepareBudgetIgnoresBackwardsClock(t *testing.T) {
	e := newPrepareBudgetEnv(t, "a", "b", "c")
	a := e.candidate(t, "a", testPrepareBudget-time.Second)
	b := e.candidate(t, "b", -time.Hour)
	c := e.candidate(t, "c", 2*time.Second)

	e.run(testPrepareBudget, false, nil, a, b, c)

	// a spent just under the budget; b's rewind must not refund it, so c runs
	// (spent < limit) and nothing is deferred before c; c itself is admitted.
	if got := e.deferredByBudget(); len(got) != 0 {
		t.Fatalf("deferred = %v, want none: spent stayed under the limit", got)
	}
	if n := e.sp.CountCalls("Start", "c"); n != 1 {
		t.Fatalf("c Start calls = %d, want 1", n)
	}
}

// Design item 7: a named session the prepare budget defers records no breaker
// restart, in memory or persisted; the same session within budget does. The
// contrast is what makes a gate placed after the accounting fail.
func TestExecutePlannedStarts_PrepareBudgetDeferralRecordsNoBreakerRestart(t *testing.T) {
	cases := []struct {
		name         string
		slowLatency  time.Duration
		wantRestarts int
		wantWritten  bool
	}{
		{"deferred by the budget", testPrepareBudget + time.Second, 0, false},
		{"within budget (contrast)", time.Second, 1, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newPrepareBudgetEnv(t, "a", "template-a")
			cb := e.namedIdentityBreaker(t)
			a := e.candidate(t, "a", tc.slowLatency)
			named := e.namedCandidate(t)
			e.lat.latency[named.info.ID] = time.Second
			before := e.bead(t, named.info.ID)

			e.run(testPrepareBudget, false, nil, a, named)

			restarts := 0
			for _, snap := range cb.Snapshot(e.clk.Now().UTC()) {
				if snap.Identity == "rig-a/session-a" {
					restarts = snap.RestartCount
				}
			}
			if restarts != tc.wantRestarts {
				t.Fatalf("in-memory breaker restarts for the named session = %d, want %d\nlog:\n%s", restarts, tc.wantRestarts, e.log.String())
			}
			after := e.bead(t, named.info.ID)
			if written := !maps.Equal(after.Metadata, before.Metadata); written != tc.wantWritten {
				t.Fatalf("named row written = %v, want %v\nbefore %v\nafter  %v", written, tc.wantWritten, before.Metadata, after.Metadata)
			}
			if !tc.wantWritten {
				if n := e.sp.CountCalls("Start", "session-a"); n != 0 {
					t.Fatalf("deferred named session started %d times", n)
				}
			}
		})
	}
}

// Only prepareStartCandidateForCity is charged: time the pass spends between
// the gate and prepare (here the named session's breaker write) is not prepare
// time and must not use up the budget.
func TestExecutePlannedStarts_PrepareBudgetChargesOnlyPrepare(t *testing.T) {
	e := newPrepareBudgetEnv(t, "b", "template-a")
	e.namedIdentityBreaker(t)
	named := e.namedCandidate(t)
	e.lat.latency[named.info.ID] = time.Second
	e.lat.writeLatency[named.info.ID] = testPrepareBudget + time.Second
	b := e.candidate(t, "b", time.Second)

	if woken := e.run(testPrepareBudget, false, nil, named, b); woken != 2 {
		t.Fatalf("woken = %d, want 2: the breaker write is not prepare time\n%s", woken, e.log.String())
	}
	if got := e.deferredByBudget(); len(got) != 0 {
		t.Fatalf("deferred = %v, want none: only prepare is charged", got)
	}
}

// Design item 2: the budget gate sits before endpoint-capacity admission. On a
// half-open endpoint whose probe the slow first candidate took, the second
// candidate on the same endpoint is deferred by the BUDGET (no ticket asked
// for); a gate placed after admission would report it deferred by endpoint
// capacity.
func TestExecutePlannedStarts_PrepareBudgetDeferralTakesNoCapacityTicket(t *testing.T) {
	setup := func(t *testing.T) (*prepareBudgetEnv, startCandidate, startCandidate) {
		e := newGuardedPrepareBudgetEnv(t, "a", "b")
		a := e.pendingCreate(t, "a", "e", nil)
		e.lat.latency[a.info.ID] = testPrepareBudget + time.Second
		b := e.pendingCreate(t, "b", "e", nil)
		e.lat.latency[b.info.ID] = time.Second
		e.openEndpoint(t)
		e.makeProbeDue()
		return e, a, b
	}
	capacityDeferred := func(e *prepareBudgetEnv) []string {
		var names []string
		for _, r := range e.records(TraceSiteLifecycleStartRun) {
			if r.OutcomeCode == TraceOutcomeDeferredByEndpointCapacity {
				names = append(names, r.SessionName)
			}
		}
		return names
	}

	t.Run("budget spent", func(t *testing.T) {
		e, a, b := setup(t)
		e.run(testPrepareBudget, false, nil, a, b)
		if n := e.sp.CountCalls("Start", "a"); n != 1 {
			t.Fatalf("Start calls for the probe taker a = %d, want 1\n%s", n, e.log.String())
		}
		e.requireDeferred(t, false, "b")
		if got := capacityDeferred(e); len(got) != 0 {
			t.Fatalf("deferred_by_endpoint_capacity for %v, want none (the budget gate precedes admission)\n%s", got, e.log.String())
		}
	})
	t.Run("no budget (contrast)", func(t *testing.T) {
		e, a, b := setup(t)
		e.runWith(nil, false, a, b)
		if got := capacityDeferred(e); len(got) != 1 || got[0] != "b" {
			t.Fatalf("deferred_by_endpoint_capacity = %v, want [b]: the scenario must expose the admission order\n%s", got, e.log.String())
		}
	})
}

// Design item 2, direct form: the deferred candidate asked the endpoint for no
// ticket, so the due half-open probe is still there for the next planner call.
// A gate placed after admission leaves b holding (and dropping) the probe.
func TestExecutePlannedStarts_PrepareBudgetDeferralLeavesDueProbeUntaken(t *testing.T) {
	e := newGuardedPrepareBudgetEnv(t, "a", "b")
	a := e.candidate(t, "a", testPrepareBudget+time.Second) // no upstream: no ticket
	b := e.pendingCreate(t, "b", "e", nil)
	e.lat.latency[b.info.ID] = time.Second
	e.openEndpoint(t)
	e.makeProbeDue()

	e.run(testPrepareBudget, false, nil, a, b)

	e.requireDeferred(t, false, "b")
	if st := e.breakerStatus(capacityTestEndpoint); st.State != resilience.StateOpen {
		t.Fatalf("endpoint state after the deferral = %v, want open (b must not have been admitted as the probe)\n%s", st.State, e.log.String())
	}
	ticket, ok := e.guard.Admit(capacityTestEndpoint, "later", "later")
	if !ok || ticket == nil || !ticket.probe {
		t.Fatalf("the due probe was not available to the next planner call (ok=%v)\n%s", ok, e.log.String())
	}
	ticket.Resolve(verdictCapacity)
}

// The deferral record carries the spend and the limit (AC3, AC7), and the one
// summary line on stderr names the spend where it says spent=.
func TestExecutePlannedStarts_PrepareBudgetDeferralPayload(t *testing.T) {
	e := newPrepareBudgetEnv(t, "a", "b")
	a := e.candidate(t, "a", testPrepareBudget+time.Second)
	b := e.candidate(t, "b", time.Second)

	e.run(testPrepareBudget, false, nil, a, b)

	e.requireDeferred(t, false, "b")
	var found int
	for _, r := range e.records(TraceSiteLifecycleStartRun) {
		if string(r.OutcomeCode) != prepareBudgetOutcome {
			continue
		}
		found++
		if got, want := r.Fields["spent_ms"], (testPrepareBudget + time.Second).Milliseconds(); got != want {
			t.Fatalf("trace spent_ms = %v (%T), want %v", got, got, want)
		}
		if got, want := r.Fields["budget_ms"], testPrepareBudget.Milliseconds(); got != want {
			t.Fatalf("trace budget_ms = %v (%T), want %v", got, got, want)
		}
	}
	if found != 1 {
		t.Fatalf("deferral records = %d, want 1", found)
	}
	if want := "(spent=1m1s, budget=1m0s)"; !strings.Contains(e.log.String(), want) {
		t.Fatalf("stderr summary lacks %q:\n%s", want, e.log.String())
	}
}

// The option is wired at the two controller reconcile sites and not at the
// one-shot `gc start`.
func TestPrepareBudgetWiring(t *testing.T) {
	const wiring = "withPrepareBudget(defaultStartPrepareBudget)"
	rt, err := os.ReadFile("city_runtime.go")
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(rt), wiring); n != 2 {
		t.Fatalf("city_runtime.go wires the prepare budget %d times, want 2 (reconcileStartOptions, controlDispatcherTick)", n)
	}
	start, err := os.ReadFile("cmd_start.go")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(start), "withPrepareBudget") {
		t.Fatal("cmd_start.go (one-shot gc start) must not opt in to the prepare budget")
	}
}

// The harness charges the clock inside prepare itself, which is what the
// budget tests rely on.
func TestPrepareLatencyStore_ChargesInsidePrepare(t *testing.T) {
	e := newPrepareBudgetEnv(t, "a")
	a := e.candidate(t, "a", 7*time.Second)
	before := e.clk.Now()

	if _, err := prepareStartCandidateForCity(a, e.city, "", e.cfg, e.sp, e.lat, e.clk, &e.log, nil, dispatchOptionSources{}); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if got := e.clk.Now().Sub(before); got != 7*time.Second {
		t.Fatalf("prepare advanced the clock by %s, want exactly 7s", got)
	}
}

// The meter's own edges, beside the behavior tests: a nil clock falls back to
// the wall clock, a backwards clock charges nothing, a limit of zero or less
// never defers, and spent exactly at the limit defers.
func TestPrepareMeter(t *testing.T) {
	cand := startCandidate{tp: TemplateParams{TemplateName: "w", SessionName: "w"}}
	f := &clock.Fake{Time: time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)}

	t.Run("nil clock falls back to the wall clock", func(t *testing.T) {
		m := &prepareMeter{limit: time.Hour}
		began := m.begin()
		m.end(began)
		if m.spent < 0 || m.spent >= time.Hour {
			t.Fatalf("spent = %s, want a small non-negative wall-clock charge", m.spent)
		}
	})
	t.Run("backwards clock charges nothing", func(t *testing.T) {
		m := &prepareMeter{limit: time.Second, clk: f}
		began := m.begin()
		f.Advance(-time.Minute)
		m.end(began)
		if m.spent != 0 {
			t.Fatalf("spent = %s, want 0", m.spent)
		}
	})
	t.Run("limit zero or less never defers", func(t *testing.T) {
		for _, limit := range []time.Duration{0, -time.Second} {
			m := &prepareMeter{limit: limit, clk: f, spent: time.Hour}
			if m.deferIfSpent(ioDiscard{}, nil, 0, cand, &config.City{}) {
				t.Fatalf("limit %s deferred", limit)
			}
		}
	})
	t.Run("exactly at the limit defers, just under does not", func(t *testing.T) {
		m := &prepareMeter{limit: time.Minute, clk: f, spent: time.Minute - time.Nanosecond}
		if m.deferIfSpent(ioDiscard{}, nil, 0, cand, &config.City{}) {
			t.Fatal("deferred just under the limit")
		}
		m.spent = time.Minute
		if !m.deferIfSpent(ioDiscard{}, nil, 0, cand, &config.City{}) {
			t.Fatal("did not defer exactly at the limit")
		}
	})
}

// prepareBarrierStore holds the first prepare read before PreWake writes.
// Reads made by the tick (dependency checks and runtime-lease admission) pass.
type prepareBarrierStore struct {
	beads.Store
	id      string
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *prepareBarrierStore) Get(id string) (beads.Bead, error) {
	if id == s.id && inPrepare() {
		s.once.Do(func() {
			close(s.entered)
			<-s.release
		})
	}
	return s.Store.Get(id)
}

// Preparation must not hold the tick, and its slot and lease must exclude a
// second attempt even before PreWake has published the in-flight marker.
func TestExecutePlannedStarts_AsyncPrepareOffTick(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newPrepareBudgetEnv(t, "a")
		a := e.candidate(t, "a", 0)
		store := &prepareBarrierStore{Store: e.lat, id: a.info.ID, entered: make(chan struct{}), release: make(chan struct{})}
		var tracker asyncStartTracker
		defer func() {
			close(store.release)
			if !tracker.wait(hangBudget) {
				t.Error("async starts did not finish")
			}
		}()
		limiter := newAsyncStartLimiter(2)
		opts := append([]startExecutionOption(nil), e.startOptions...)
		opts = append(opts, withAsyncStartExecution(), withAsyncStartTracker(&tracker), withAsyncStartLimiter(limiter))
		run := func() int {
			return executePlannedStartsTraced(context.Background(), []startCandidate{a}, e.cfg, e.desiredState, e.sp, store, "", e.city, e.clk, e.rec, 0, &e.log, &e.log, nil, opts...)
		}
		done := make(chan int, 1)
		go func() { done <- run() }()
		synctest.Wait()
		select {
		case <-store.entered:
		default:
			t.Fatal("prepare did not reach the barrier")
		}
		select {
		case n := <-done:
			if n != 1 {
				t.Fatalf("enqueued = %d, want 1", n)
			}
		default:
			t.Fatal("tick is waiting for prepare before enqueueing")
		}
		if got := e.bead(t, a.info.ID).Metadata["last_woke_at"]; got != "" {
			t.Fatalf("PreWake landed while prepare was blocked: %q", got)
		}
		limiter.mu.Lock()
		inFlight := limiter.inFlight
		limiter.mu.Unlock()
		if inFlight != 1 || !tracker.busy() {
			t.Fatalf("prepare lost its slot or tracker: slots=%d busy=%v", inFlight, tracker.busy())
		}
		// A second free slot cannot admit the same runtime name on another tick.
		if n := run(); n != 0 {
			t.Fatalf("duplicate enqueued = %d, want 0", n)
		}
		if got := e.sp.CountCalls("Start", "a"); got != 0 {
			t.Fatalf("Start calls = %d before PreWake", got)
		}
	})
}

// A dependent waits in this pass and the next while its prerequisite is still
// preparing. Once that preparation commits it remains eligible to start.
func TestExecutePlannedStarts_AsyncPrepareDependencyWaits(t *testing.T) {
	for _, live := range []bool{false, true} {
		name := "absent runtime"
		if live {
			name = "existing runtime"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				e := newPrepareBudgetEnv(t, "a", "b")
				e.cfg.Agents[1].DependsOn = []string{"a"}
				a, b := e.candidate(t, "a", 0), e.candidate(t, "b", 0)
				if live {
					if err := e.sp.Start(context.Background(), "a", gcruntime.Config{}); err != nil {
						t.Fatal(err)
					}
				}
				store := &prepareBarrierStore{Store: e.lat, id: a.info.ID, entered: make(chan struct{}), release: make(chan struct{})}
				var once sync.Once
				release := func() { once.Do(func() { close(store.release) }) }
				var tracker asyncStartTracker
				defer func() {
					release()
					if !tracker.wait(hangBudget) {
						t.Error("async starts did not finish")
					}
				}()
				opts := append([]startExecutionOption(nil), e.startOptions...)
				opts = append(opts, withAsyncStartExecution(), withAsyncStartTracker(&tracker))
				run := func(cs ...startCandidate) int {
					return executePlannedStartsTraced(context.Background(), cs, e.cfg, e.desiredState, e.sp, store, "", e.city, e.clk, e.rec, 0, &e.log, &e.log, nil, opts...)
				}
				done := make(chan int, 1)
				go func() { done <- run(a, b) }()
				synctest.Wait()
				select {
				case n := <-done:
					if n != 1 {
						t.Fatalf("enqueued = %d, want prerequisite only", n)
					}
				default:
					t.Fatal("tick waited for dependency prepare")
				}
				if n := run(b); n != 0 {
					t.Fatalf("dependent enqueued = %d while prepare is blocked", n)
				}
				if row := e.bead(t, b.info.ID); row.Status == "closed" || row.Metadata["last_woke_at"] != "" {
					t.Fatalf("waiting dependency was closed or prepared: %+v", row)
				}
				release()
				synctest.Wait()
				if tracker.busy() {
					t.Fatal("prerequisite prepare did not finish")
				}
				if n := run(b); n != 1 {
					t.Fatalf("dependent enqueued = %d after prerequisite committed", n)
				}
				synctest.Wait()
				if got := e.sp.CountCalls("Start", "b"); got != 1 {
					t.Fatalf("dependent starts = %d, want 1", got)
				}
			})
		})
	}
}

func TestExecutePlannedStarts_AsyncPrepareFailureReleasesReservations(t *testing.T) {
	for _, cancelPrepare := range []bool{false, true} {
		name := "error"
		if cancelPrepare {
			name = "canceled"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				e := newPrepareBudgetEnv(t, "a")
				a := e.candidate(t, "a", 7*time.Millisecond)
				if !cancelPrepare {
					e.lat.failGet[a.info.ID] = os.ErrDeadlineExceeded
				}
				store := &prepareBarrierStore{Store: e.lat, id: a.info.ID, entered: make(chan struct{}), release: make(chan struct{})}
				var once sync.Once
				release := func() { once.Do(func() { close(store.release) }) }
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				var tracker asyncStartTracker
				defer func() {
					release()
					if !tracker.wait(hangBudget) {
						t.Error("async starts did not finish")
					}
				}()
				limiter := newAsyncStartLimiter(1)
				opts := append([]startExecutionOption(nil), e.startOptions...)
				opts = append(opts, withAsyncStartExecution(), withAsyncStartTracker(&tracker), withAsyncStartLimiter(limiter))
				done := make(chan int, 1)
				go func() {
					done <- executePlannedStartsTraced(ctx, []startCandidate{a}, e.cfg, e.desiredState, e.sp, store, "", e.city, e.clk, e.rec, 0, &e.log, &e.log, e.trace, opts...)
				}()
				synctest.Wait()
				select {
				case n := <-done:
					if n != 1 {
						t.Fatalf("enqueued = %d, want 1", n)
					}
				default:
					t.Fatal("tick waited for prepare")
				}
				if cancelPrepare {
					cancel()
				}
				release()
				synctest.Wait()
				if tracker.busy() || limiter.inFlight != 0 {
					t.Fatalf("prepare leaked reservations: busy=%v slots=%d", tracker.busy(), limiter.inFlight)
				}
				if got := e.sp.CountCalls("Start", "a"); got != 0 {
					t.Fatalf("Start calls = %d after failed or canceled prepare", got)
				}
				lease, drop, err := tryRuntimeLease(store, e.city, "a", "", 0)
				if err != nil {
					t.Fatalf("prepare leaked its name lease: %v", err)
				}
				drop()
				if lease == nil {
					t.Fatal("lease reacquisition returned no lease")
				}
				if row := e.bead(t, a.info.ID); row.Status == "closed" || row.Metadata["last_woke_at"] != "" || row.Metadata["pending_create_claim"] != "true" {
					t.Fatalf("prepare failure lost the pending create or left it in flight: %+v", row)
				}
				if !cancelPrepare && !strings.Contains(e.log.String(), "session=a template=a outcome=failed") {
					t.Fatalf("prepare failure lost its outcome: %s", e.log.String())
				}
			})
		})
	}
}

func TestExecutePlannedStarts_FastPrepareTraceTimings(t *testing.T) {
	for _, async := range []bool{false, true} {
		name := "sync"
		if async {
			name = "async"
		}
		t.Run(name, func(t *testing.T) {
			e := newPrepareBudgetEnv(t, "a")
			a := e.candidate(t, "a", 7*time.Millisecond)
			// Async completion flushes the cycle. Read the persisted typed
			// records so the proof also covers timings surviving that flush.
			var traceStore *SessionReconcilerTraceStore
			if async {
				var err error
				traceStore, err = newSessionReconcilerTraceStore(e.city, ioDiscard{})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := traceStore.Close(); err != nil {
						t.Error(err)
					}
				})
				e.trace.tracer.store = traceStore
			}
			if n := e.run(testPrepareBudget, async, nil, a); n != 1 {
				t.Fatalf("starts = %d, want 1", n)
			}
			records := e.records(TraceSiteLifecycleStartRun)
			if async {
				var err error
				records, err = ReadTraceRecords(traceStore.rootDir, TraceFilter{})
				if err != nil {
					t.Fatal(err)
				}
			}
			var found bool
			for _, r := range records {
				if r.SiteCode != TraceSiteLifecycleStartRun || r.RecordType != TraceRecordOperation || r.OutcomeCode == TraceOutcomeStartEnqueued {
					continue
				}
				found = true
				for _, field := range []string{"lock_get_ms", "prewake_ms", "named_snapshot_ms", "build_ms", "spent_ms"} {
					if _, ok := r.Fields[field]; !ok {
						t.Errorf("fast prepare missing trace field %s: %v", field, r.Fields)
					}
				}
				if got := r.Fields["lock_get_ms"]; got != int64(7) && got != float64(7) {
					t.Errorf("lock_get_ms = %v, want 7", got)
				}
				if got := r.Fields["spent_ms"]; got != int64(7) && got != float64(7) {
					t.Errorf("spent_ms = %v, want 7", got)
				}
			}
			if !found {
				t.Fatal("no start execution trace result")
			}
		})
	}
}
