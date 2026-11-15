package main

import (
	"context"
	"maps"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/resilience"
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
		if strings.HasSuffix(f.Function, ".prepareStartCandidateForCity") {
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
	for _, async := range []bool{false, true} {
		name := "sync"
		if async {
			name = "async"
		}
		t.Run(name, func(t *testing.T) {
			e := newPrepareBudgetEnv(t, "a", "b", "c")
			a := e.candidate(t, "a", testPrepareBudget+time.Second)
			b := e.candidate(t, "b", time.Second)
			c := e.candidate(t, "c", time.Second)
			bBefore, cBefore := e.bead(t, b.info.ID), e.bead(t, c.info.ID)

			if woken := e.run(testPrepareBudget, async, nil, a, b, c); woken != 1 {
				t.Fatalf("woken = %d, want 1 (only a is prepared)", woken)
			}
			if n := e.sp.CountCalls("Start", "a"); n != 1 {
				t.Fatalf("a Start calls = %d, want 1", n)
			}
			e.requireDeferred(t, async, "b", "c")
			e.requireUntouched(t, b, bBefore)
			e.requireUntouched(t, c, cBefore)
			if n := strings.Count(e.log.String(), "prepare budget exhausted"); n != 1 {
				t.Fatalf("summary lines = %d, want exactly one per planner call:\n%s", n, e.log.String())
			}
		})
	}
}

// The slot, tracker entry, lease and capacity ticket of an async start are all
// taken after the gate, so a deferred candidate holds none of them.
func TestExecutePlannedStarts_PrepareBudgetDeferralTakesNoAsyncSlotOrLease(t *testing.T) {
	e := newPrepareBudgetEnv(t, "a", "b")
	a := e.candidate(t, "a", testPrepareBudget)
	b := e.candidate(t, "b", time.Second)
	limiter := newAsyncStartLimiter(1)

	e.run(testPrepareBudget, true, []startExecutionOption{withAsyncStartLimiter(limiter)}, a, b)

	e.requireDeferred(t, true, "b")
	// a's start released its slot; with b never reserving one the limiter is
	// empty and a one-slot limiter admitted a without ever refusing b for a
	// full slot.
	if strings.Contains(e.log.String(), "session=b template=b outcome=deferred_by_async_start_limit") {
		t.Fatalf("b was refused a slot instead of being deferred by the budget:\n%s", e.log.String())
	}
	limiter.mu.Lock()
	inFlight := limiter.inFlight
	limiter.mu.Unlock()
	if inFlight != 0 {
		t.Fatalf("async slots in flight = %d, want 0", inFlight)
	}
	if rec := e.sessionInfo(b.info.ID); rec.SessionName != "b" {
		t.Fatalf("b row unreadable: %+v", rec)
	}
	if _, err := os.Stat(e.city + "/.gc/runtime-leases"); err == nil {
		entries, _ := os.ReadDir(e.city + "/.gc/runtime-leases")
		for _, ent := range entries {
			if strings.Contains(ent.Name(), "b") && !strings.Contains(ent.Name(), "a") {
				t.Fatalf("b took a runtime lease: %s", ent.Name())
			}
		}
	}
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
// never defers or reads the clock, and spent exactly at the limit defers.
func TestPrepareMeter(t *testing.T) {
	cand := startCandidate{tp: TemplateParams{TemplateName: "w", SessionName: "w"}}
	f := &clock.Fake{Time: time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)}

	t.Run("nil clock falls back to the wall clock", func(t *testing.T) {
		m := &prepareMeter{limit: time.Hour}
		m.begin()
		m.end()
		if m.spent < 0 || m.spent >= time.Hour {
			t.Fatalf("spent = %s, want a small non-negative wall-clock charge", m.spent)
		}
	})
	t.Run("backwards clock charges nothing", func(t *testing.T) {
		m := &prepareMeter{limit: time.Second, clk: f}
		m.begin()
		f.Advance(-time.Minute)
		m.end()
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
