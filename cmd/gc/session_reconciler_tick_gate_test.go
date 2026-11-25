package main

import (
	"context"
	"errors"
	"maps"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/worker"
)

const (
	// tickGateTestBound is the tickProbeBound these tests run under: long enough
	// that a healthy call never trips it, short enough that three of them cost
	// well under a second of wall time.
	tickGateTestBound = 150 * time.Millisecond
	// tickGateDeadline is far past every bound under test. It turns a reconcile
	// call that hangs on a wedged runtime (what main does) into a test failure
	// instead of a hung test binary.
	tickGateDeadline = 8 * time.Second
	// tickGateMargin is the slack on "returns within the bound": scheduling and
	// the store work the rest of the tick does.
	tickGateMargin = 2 * time.Second
)

// tickGateProvider is a runtime whose IsRunning and Stop can wedge per session
// the way a hung tmux subprocess does: a wedged call ignores everything and
// returns only when the test releases it, then answers normally. Every call to
// IsRunning, Stop and ClearScrollback is counted and logged in order at entry,
// wedged or not. It embeds *runtime.Fake and uses channels, not mutexes, to
// block (internal/session/AGENTS.md: synctest does not see a mutex wait).
type tickGateProvider struct {
	*runtime.Fake

	mu      sync.Mutex
	wedges  map[string]chan struct{} // "Method\x00name" -> closed to release
	calls   map[string]int           // "Method\x00name" -> entry count
	log     []string                 // "Method:name" in entry order
	touched map[string]bool          // names ever wedged, for cleanup
}

// newTickGateProvider returns a provider over a fresh Fake. Its cleanup
// releases every wedge, then waits for each wedged session's observation key to
// be released, so no abandoned goroutine outlives the test's city.
func newTickGateProvider(t *testing.T, city string) *tickGateProvider {
	t.Helper()
	p := &tickGateProvider{
		Fake:    runtime.NewFake(),
		wedges:  map[string]chan struct{}{},
		calls:   map[string]int{},
		touched: map[string]bool{},
	}
	t.Cleanup(func() {
		p.releaseAll()
		for name := range p.touchedNames() {
			drainObservationKey(t, city, name)
		}
	})
	return p
}

func tickGateKey(method, name string) string { return method + "\x00" + name }

// wedge makes every later method call on name block until released. method is
// "IsRunning" or "Stop".
func (p *tickGateProvider) wedge(method, name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.wedges[tickGateKey(method, name)] = make(chan struct{})
	p.touched[name] = true
}

func (p *tickGateProvider) releaseAll() {
	p.mu.Lock()
	chans := make([]chan struct{}, 0, len(p.wedges))
	for k, ch := range p.wedges {
		chans = append(chans, ch)
		delete(p.wedges, k)
	}
	p.mu.Unlock()
	for _, ch := range chans {
		close(ch)
	}
}

func (p *tickGateProvider) touchedNames() map[string]bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return maps.Clone(p.touched)
}

// enter records the call and returns the channel it must wait on, if wedged.
func (p *tickGateProvider) enter(method, name string) chan struct{} {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls[tickGateKey(method, name)]++
	p.log = append(p.log, method+":"+name)
	return p.wedges[tickGateKey(method, name)]
}

// count is how many times method was entered for name.
func (p *tickGateProvider) count(method, name string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls[tickGateKey(method, name)]
}

// indexOf is the position of the first "Method:name" entry in the call log, or
// -1.
func (p *tickGateProvider) indexOf(entry string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, e := range p.log {
		if e == entry {
			return i
		}
	}
	return -1
}

// IsRunning counts the call and blocks while name is wedged.
func (p *tickGateProvider) IsRunning(name string) bool {
	if ch := p.enter("IsRunning", name); ch != nil {
		<-ch
	}
	return p.Fake.IsRunning(name)
}

// Stop counts the call and blocks while name is wedged.
func (p *tickGateProvider) Stop(name string) error {
	if ch := p.enter("Stop", name); ch != nil {
		<-ch
	}
	return p.Fake.Stop(name)
}

// ClearScrollback counts the call.
func (p *tickGateProvider) ClearScrollback(name string) error {
	p.enter("ClearScrollback", name)
	return p.Fake.ClearScrollback(name)
}

// drainObservationKey waits until the lifecycle observation key of name in city
// is free: the abandoned call under it has returned.
func drainObservationKey(t *testing.T, city, name string) {
	t.Helper()
	deadline := time.NewTimer(tickGateDeadline)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_, err := worker.ObserveBounded(ctx, postStartObservationKey(city, name), func(context.Context) (struct{}, error) {
			return struct{}{}, nil
		})
		cancel()
		if !errors.Is(err, worker.ErrObservationOutstanding) {
			return
		}
		select {
		case <-tick.C:
		case <-deadline.C:
			t.Errorf("observation key of %q never released after the wedge was lifted", name)
			return
		}
	}
}

// setTickProbeBound shrinks tickProbeBound to tickGateTestBound for the test and restores it. Tests
// that call it must not be parallel.
func setTickProbeBound(t *testing.T) {
	t.Helper()
	old := tickProbeBound
	tickProbeBound = tickGateTestBound
	t.Cleanup(func() { tickProbeBound = old })
}

// runWithDeadline runs fn on its own goroutine and returns how long it took. A
// call that outlives tickGateDeadline fails the test: it is a reconcile call
// held by a wedged runtime.
func runWithDeadline(t *testing.T, what string, fn func()) time.Duration {
	t.Helper()
	done := make(chan struct{})
	start := time.Now()
	go func() {
		defer close(done)
		fn()
	}()
	timer := time.NewTimer(tickGateDeadline)
	defer timer.Stop()
	select {
	case <-done:
		return time.Since(start)
	case <-timer.C:
		t.Fatalf("%s did not return within %s: the tick is held by a runtime that does not answer", what, tickGateDeadline)
		return 0
	}
}

// tickGateEnv builds a reconciler env whose provider is a tickGateProvider and
// whose bound is shrunk.
func tickGateEnv(t *testing.T) (*reconcilerTestEnv, *tickGateProvider) {
	t.Helper()
	setTickProbeBound(t)
	env := newReconcilerTestEnv(t)
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	return env, newTickGateProvider(t, env.city)
}

// addRunning registers name as a desired, running, active session and returns
// its bead.
func (e *reconcilerTestEnv) addRunning(t *testing.T, sp *tickGateProvider, name string) beads.Bead {
	t.Helper()
	e.addDesired(name, "worker", false)
	if err := sp.Start(context.Background(), name, runtime.Config{Command: "test-cmd"}); err != nil {
		t.Fatalf("start %s: %v", name, err)
	}
	b := e.createSessionBead(name, "worker")
	e.markSessionActive(&b)
	if err := sp.SetMeta(name, "GC_SESSION_ID", b.ID); err != nil {
		t.Fatalf("SetMeta(GC_SESSION_ID) %s: %v", name, err)
	}
	return b
}

// addDown registers name as a desired session that is not running and is
// waiting to be woken.
func (e *reconcilerTestEnv) addDown(name string) beads.Bead {
	e.addDesired(name, "worker", false)
	b := e.createSessionBead(name, "worker")
	e.markSessionCreating(&b)
	return b
}

// tickWith runs one reconcile tick over sp for the beads' current store state.
func (e *reconcilerTestEnv) tickWith(t *testing.T, sp runtime.Provider, it idleTracker, opts []startExecutionOption, sessions ...beads.Bead) (woken int, elapsed time.Duration) {
	t.Helper()
	fresh := make([]beads.Bead, 0, len(sessions))
	for _, s := range sessions {
		b, err := e.store.Get(s.ID)
		if err != nil {
			t.Fatalf("Get(%s): %v", s.ID, err)
		}
		fresh = append(fresh, b)
	}
	poolDesired := make(map[string]int)
	for _, tp := range e.desiredState {
		if tp.TemplateName != "" {
			poolDesired[tp.TemplateName]++
		}
	}
	cfgNames := configuredSessionNames(e.cfg, "", e.store)
	opts = append(append([]startExecutionOption(nil), e.startOptions...), opts...)
	elapsed = runWithDeadline(t, "reconcile tick", func() {
		woken = reconcileSessionBeadsAtPath(
			context.Background(), e.city, fresh, e.desiredState, cfgNames, e.cfg, sp,
			e.store, nil, nil, nil, nil, e.dt, poolDesired, false, nil, "",
			it, e.clk, e.rec, 0, 0, &e.stdout, &e.stderr,
			opts...,
		)
	})
	return woken, elapsed
}

// metadataOf reads the persisted metadata of id.
func (e *reconcilerTestEnv) metadataOf(t *testing.T, id string) map[string]string {
	t.Helper()
	b, err := e.store.Get(id)
	if err != nil {
		t.Fatalf("Get(%s): %v", id, err)
	}
	return maps.Clone(b.Metadata)
}

// unansweredLines is the stderr lines that say a session's runtime did not
// answer.
func unansweredLines(stderr string) []string {
	var out []string
	for _, line := range strings.Split(stderr, "\n") {
		if strings.Contains(line, "did not answer") {
			out = append(out, line)
		}
	}
	return out
}

// 1. A session whose runtime does not answer is skipped for the tick; its
// neighbors are reconciled as usual, and the tick is bounded.
func TestReconcileTickGate_WedgedSessionSkippedOthersReconciled(t *testing.T) {
	env, sp := tickGateEnv(t)
	a := env.addDown("sess-a")
	b := env.addRunning(t, sp, "sess-b")
	c := env.addDown("sess-c")
	sp.wedge("IsRunning", "sess-b")
	beforeB := env.metadataOf(t, b.ID)

	woken, elapsed := env.tickWith(t, sp, nil, nil, a, b, c)

	if limit := tickGateTestBound + tickGateMargin; elapsed > limit {
		t.Fatalf("tick took %s, want at most %s (one bound plus margin)", elapsed, limit)
	}
	if woken != 2 {
		t.Errorf("woken = %d, want 2 (sess-a and sess-c)", woken)
	}
	for _, name := range []string{"sess-a", "sess-c"} {
		if got := sp.CountCalls("Start", name); got != 1 {
			t.Errorf("Start(%s) calls = %d, want 1: a healthy neighbor of a wedged session is reconciled as usual", name, got)
		}
	}
	if after := env.metadataOf(t, b.ID); !maps.Equal(after, beforeB) {
		t.Errorf("wedged sess-b's bead was written: before=%#v after=%#v", beforeB, after)
	}
	for _, method := range []string{"Start", "Stop", "Kill"} {
		if got := sp.CountCalls(method, "sess-b"); got != 0 {
			t.Errorf("%s(sess-b) calls = %d, want 0: unavailable means deferred, never dead (SESSION-RUNTIME-009)", method, got)
		}
	}
	lines := unansweredLines(env.stderr.String())
	if len(lines) != 1 || !strings.Contains(lines[0], "sess-b") {
		t.Errorf("did-not-answer lines = %q, want exactly one, for sess-b", lines)
	}
}

// 2. While the session's key is still outstanding, the next tick neither waits
// nor calls the provider again.
func TestReconcileTickGate_SecondTickFindsKeyOutstanding(t *testing.T) {
	env, sp := tickGateEnv(t)
	b := env.addRunning(t, sp, "sess-b")
	sp.wedge("IsRunning", "sess-b")

	env.tickWith(t, sp, nil, nil, b)
	if got := sp.count("IsRunning", "sess-b"); got != 1 {
		t.Fatalf("first tick: IsRunning(sess-b) = %d, want 1", got)
	}
	env.stderr.Reset()
	beforeB := env.metadataOf(t, b.ID)

	_, elapsed := env.tickWith(t, sp, nil, nil, b)

	if elapsed > tickGateTestBound/2 {
		t.Errorf("second tick took %s, want well under the %s bound: the key is outstanding, there is nothing to wait for", elapsed, tickGateTestBound)
	}
	if got := sp.count("IsRunning", "sess-b"); got != 1 {
		t.Errorf("IsRunning(sess-b) = %d after the second tick, want still 1: no second call onto a wedged leaf", got)
	}
	if lines := unansweredLines(env.stderr.String()); len(lines) != 1 || !strings.Contains(lines[0], "sess-b") {
		t.Errorf("second tick did-not-answer lines = %q, want exactly one, for sess-b", lines)
	}
	if after := env.metadataOf(t, b.ID); !maps.Equal(after, beforeB) {
		t.Errorf("sess-b's bead was written on the second tick: before=%#v after=%#v", beforeB, after)
	}
}

// 3. Three consecutive abandoned waits skip the rest of the tick without
// waiting again: a provider-wide wedge costs three bounds, not N.
func TestReconcileTickGate_AbandonBudgetSkipsRemainingSessions(t *testing.T) {
	env, sp := tickGateEnv(t)
	names := []string{"sess-1", "sess-2", "sess-3", "sess-4", "sess-5"}
	var sessions []beads.Bead
	before := map[string]map[string]string{}
	for _, name := range names {
		b := env.addRunning(t, sp, name)
		sp.wedge("IsRunning", name)
		sessions = append(sessions, b)
		before[b.ID] = env.metadataOf(t, b.ID)
	}

	_, elapsed := env.tickWith(t, sp, nil, nil, sessions...)

	if limit := 3*tickGateTestBound + tickGateMargin; elapsed > limit {
		t.Errorf("tick took %s, want at most %s (three bounds plus margin), not one bound per session", elapsed, limit)
	}
	called, untouched := 0, 0
	for _, name := range names {
		switch got := sp.count("IsRunning", name); got {
		case 0:
			untouched++
		case 1:
			called++
		default:
			t.Errorf("IsRunning(%s) = %d, want at most 1", name, got)
		}
	}
	if called != 3 || untouched != 2 {
		t.Errorf("sessions probed = %d, skipped without a call = %d; want 3 and 2: the budget is three consecutive abandoned waits", called, untouched)
	}
	for _, b := range sessions {
		if after := env.metadataOf(t, b.ID); !maps.Equal(after, before[b.ID]) {
			t.Errorf("bead %s was written: before=%#v after=%#v", b.ID, before[b.ID], after)
		}
	}
	for _, name := range names {
		for _, method := range []string{"Start", "Stop", "Kill"} {
			if got := sp.CountCalls(method, name); got != 0 {
				t.Errorf("%s(%s) calls = %d, want 0", method, name, got)
			}
		}
	}
}

// 3b. A pattern with no three CONSECUTIVE abandoned waits does not trip the
// budget: an answer resets the counter, so every session gets its own probe.
func TestTickProbeGate_AnswerResetsAbandonBudget(t *testing.T) {
	setTickProbeBound(t)
	city := t.TempDir()
	sp := newTickGateProvider(t, city)
	for _, name := range []string{"w1", "w2", "w3"} {
		sp.wedge("IsRunning", name)
	}
	for _, name := range []string{"a1", "a2"} {
		if err := sp.Start(context.Background(), name, runtime.Config{}); err != nil {
			t.Fatalf("start %s: %v", name, err)
		}
	}
	gate := newTickProbeGate(context.Background(), city, sp)

	want := []struct {
		name string
		may  bool
	}{{"w1", false}, {"a1", true}, {"w2", false}, {"a2", true}, {"w3", false}}
	runWithDeadline(t, "gate over a non-consecutive pattern", func() {
		for _, w := range want {
			if got := gate.mayReconcile(w.name); got != w.may {
				t.Errorf("mayReconcile(%s) = %v, want %v", w.name, got, w.may)
			}
		}
	})
	for _, w := range want {
		if got := sp.count("IsRunning", w.name); got != 1 {
			t.Errorf("IsRunning(%s) = %d, want 1: wedged, answered, wedged, answered, wedged never reaches three in a row", w.name, got)
		}
	}
}

// 3c. After three consecutive abandoned waits the gate refuses the next session
// without calling the provider, even one that would answer.
func TestTickProbeGate_ThreeConsecutiveAbandonsRefuseWithoutProbe(t *testing.T) {
	setTickProbeBound(t)
	city := t.TempDir()
	sp := newTickGateProvider(t, city)
	for _, name := range []string{"w1", "w2", "w3"} {
		sp.wedge("IsRunning", name)
	}
	if err := sp.Start(context.Background(), "healthy", runtime.Config{}); err != nil {
		t.Fatalf("start healthy: %v", err)
	}
	gate := newTickProbeGate(context.Background(), city, sp)

	runWithDeadline(t, "gate over three wedged sessions", func() {
		for _, name := range []string{"w1", "w2", "w3"} {
			if gate.mayReconcile(name) {
				t.Errorf("mayReconcile(%s) = true, want false: the runtime did not answer", name)
			}
		}
		if gate.mayReconcile("healthy") {
			t.Error("mayReconcile(healthy) = true after three consecutive abandoned waits, want false: the budget skips the rest of the tick")
		}
	})
	if got := sp.count("IsRunning", "healthy"); got != 0 {
		t.Errorf("IsRunning(healthy) = %d, want 0: the exhausted budget makes no provider call", got)
	}
}

// 4. The gate's answer is only "did it answer": with nothing wedged, a session
// that is down is woken and a healthy one is left alone, as today.
func TestReconcileTickGate_AnswerValueIgnored(t *testing.T) {
	env, sp := tickGateEnv(t)
	down := env.addDown("sess-down")
	healthy := env.addRunning(t, sp, "sess-healthy")
	startsBefore := sp.CountCalls("Start", "sess-healthy")

	woken, _ := env.tickWith(t, sp, nil, nil, down, healthy)

	if got := sp.CountCalls("Start", "sess-down"); got != 1 || woken < 1 {
		t.Errorf("down session: Start calls = %d, woken = %d; want it woken as today", got, woken)
	}
	if got := sp.CountCalls("Start", "sess-healthy"); got != startsBefore {
		t.Errorf("Start(sess-healthy) = %d, want %d: a running session is not restarted", got, startsBefore)
	}
	if got := sp.CountCalls("Stop", "sess-healthy"); got != 0 {
		t.Errorf("Stop(sess-healthy) = %d, want 0", got)
	}
	if lines := unansweredLines(env.stderr.String()); len(lines) != 0 {
		t.Errorf("did-not-answer lines with nothing wedged = %q, want none", lines)
	}
}

// 5. The drain advance skips a session the gate holds, and advances the rest.
func TestAdvanceSessionDrains_GateWedgedSessionSkipped(t *testing.T) {
	setTickProbeBound(t)
	city := t.TempDir()
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}
	sp := newTickGateProvider(t, city)
	store := beads.NewMemStore()
	dt := newDrainTracker()

	newDrain := func(name string) (beads.Bead, *drainState) {
		b, err := store.Create(beads.Bead{
			Title:  name,
			Type:   sessionBeadType,
			Labels: []string{sessionBeadLabel},
			Metadata: map[string]string{
				"session_name":        name,
				"template":            "worker",
				"provider":            "claude",
				"work_dir":            t.TempDir(),
				"generation":          "3",
				"state":               "active",
				"wake_mode":           "fresh",
				"session_key":         "keep-" + name,
				"started_config_hash": "keep-hash",
				"last_woke_at":        now.Add(-time.Minute).Format(time.RFC3339),
			},
		})
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		ds := &drainState{startedAt: now.Add(-10 * time.Second), deadline: now.Add(20 * time.Second), reason: "idle", generation: 3}
		dt.set(b.ID, ds)
		return b, ds
	}
	// drain-wedged has a runtime that does not answer. drain-gone has no runtime,
	// so its drain completes as today.
	wedged, wedgedDS := newDrain("drain-wedged")
	gone, _ := newDrain("drain-gone")
	if err := sp.Start(context.Background(), "drain-wedged", runtime.Config{}); err != nil {
		t.Fatalf("start drain-wedged: %v", err)
	}
	sp.wedge("IsRunning", "drain-wedged")
	beforeWedged, _ := store.Get(wedged.ID)

	gate := newTickProbeGate(context.Background(), city, sp)
	runWithDeadline(t, "gate probes", func() {
		if gate.mayReconcile("drain-wedged") {
			t.Error("mayReconcile(drain-wedged) = true, want false: its runtime does not answer")
		}
		if !gate.mayReconcile("drain-gone") {
			t.Error("mayReconcile(drain-gone) = false, want true: the fake answers (not running)")
		}
	})
	probesAfterGate := sp.count("IsRunning", "drain-wedged")
	stopsBefore := sp.count("Stop", "drain-wedged")

	dt.tickGate = gate
	runWithDeadline(t, "drain advance", func() {
		advanceSessionDrainsWithSessionsTraced(city, dt, sp, store, infoLookupFromBeadLookup(func(id string) *beads.Bead {
			got, err := store.Get(id)
			if err != nil {
				return nil
			}
			return &got
		}), map[string]wakeEvaluation{}, &config.City{}, clk, nil)
	})

	if got := dt.get(wedged.ID); got != wedgedDS {
		t.Errorf("wedged session's drain entry = %p, want it retained (%p): the next tick resumes it", got, wedgedDS)
	}
	if got := sp.count("IsRunning", "drain-wedged"); got != probesAfterGate {
		t.Errorf("IsRunning(drain-wedged) = %d, want %d: the drain pass makes no probe of a session the gate holds", got, probesAfterGate)
	}
	if got := sp.count("Stop", "drain-wedged"); got != stopsBefore {
		t.Errorf("Stop(drain-wedged) = %d, want %d", got, stopsBefore)
	}
	if after, _ := store.Get(wedged.ID); !maps.Equal(after.Metadata, beforeWedged.Metadata) {
		t.Errorf("wedged session's bead was written: before=%#v after=%#v", beforeWedged.Metadata, after.Metadata)
	}
	if got := dt.get(gone.ID); got != nil {
		t.Errorf("drain-gone's drain entry = %p, want it advanced to completion as today", got)
	}
}

// tickKillCase is one of the three tick kills.
type tickKillCase struct {
	name string
	// setup builds the session and returns it with the idle tracker the tick
	// needs (nil when none) and the tick's extra options.
	setup func(t *testing.T, env *reconcilerTestEnv, sp *tickGateProvider) (beads.Bead, idleTracker, []startExecutionOption)
	// event is the event the site records after a successful kill ("" for none).
	event string
	// clearsScrollback is whether the site clears scrollback after the kill.
	clearsScrollback bool
	// errLine is the stderr text of the site's kill-error arm.
	errLine string
	// folded reports whether the site's post-kill fold reached the store.
	folded func(meta map[string]string) bool
}

func tickKillCases() []tickKillCase {
	const sessionName = "sess-kill"
	return []tickKillCase{
		{
			name: "restart-requested",
			setup: func(t *testing.T, env *reconcilerTestEnv, sp *tickGateProvider) (beads.Bead, idleTracker, []startExecutionOption) {
				env.cfg = &config.City{
					Workspace:     config.Workspace{Name: "test-city"},
					Agents:        []config.Agent{{Name: "worker", StartCommand: "true", MaxActiveSessions: intPtr(2)}},
					NamedSessions: []config.NamedSession{{Template: "worker", Mode: "on_demand"}},
				}
				name := config.NamedSessionRuntimeName(env.cfg.Workspace.Name, env.cfg.Workspace, "worker")
				env.desiredState[name] = TemplateParams{Command: "true", SessionName: name, TemplateName: "worker"}
				if err := sp.Start(context.Background(), name, runtime.Config{Command: "true"}); err != nil {
					t.Fatalf("start session: %v", err)
				}
				b := env.createSessionBead(name, "worker")
				env.markSessionActive(&b)
				env.setSessionMetadata(&b, map[string]string{
					namedSessionMetadataKey:      "true",
					namedSessionIdentityMetadata: "worker",
					namedSessionModeMetadata:     "on_demand",
					"restart_requested":          "true",
					"session_key":                "original-key",
					"started_config_hash":        "hash-before-restart",
					"pending_create_claim":       "true",
				})
				if err := sp.SetMeta(name, "GC_SESSION_ID", b.ID); err != nil {
					t.Fatalf("SetMeta(GC_SESSION_ID): %v", err)
				}
				return b, nil, nil
			},
			errLine: "stopping restart-requested",
			folded:  func(meta map[string]string) bool { return meta["restart_requested"] != "true" },
		},
		{
			name: "max-age",
			setup: func(t *testing.T, env *reconcilerTestEnv, sp *tickGateProvider) (beads.Bead, idleTracker, []startExecutionOption) {
				env.cfg = &config.City{Agents: []config.Agent{{Name: "worker", MaxSessionAge: "5h"}}}
				b := env.addRunning(t, sp, sessionName)
				env.setSessionMetadata(&b, map[string]string{
					"creation_complete_at": env.clk.Now().Add(-6 * time.Hour).UTC().Format(time.RFC3339),
				})
				tr := newMaxSessionAgeTracker()
				tr.setConfig(sessionName, 5*time.Hour, 0)
				return b, nil, []startExecutionOption{withMaxSessionAgeTracker(tr)}
			},
			event:            events.SessionMaxAgeKilled,
			clearsScrollback: true,
			errLine:          "stopping aged",
			folded:           func(meta map[string]string) bool { return meta["sleep_reason"] != "" },
		},
		{
			name: "idle",
			setup: func(t *testing.T, env *reconcilerTestEnv, sp *tickGateProvider) (beads.Bead, idleTracker, []startExecutionOption) {
				b := env.addRunning(t, sp, sessionName)
				it := newFakeIdleTracker()
				it.idle[sessionName] = true
				return b, it, nil
			},
			event:            events.SessionIdleKilled,
			clearsScrollback: true,
			errLine:          "stopping idle",
			folded:           func(meta map[string]string) bool { return meta["sleep_reason"] != "" },
		},
	}
}

// killTargetName is the runtime name the case's session runs under.
func killTargetName(env *reconcilerTestEnv, c tickKillCase) string {
	if c.name == "restart-requested" {
		return config.NamedSessionRuntimeName(env.cfg.Workspace.Name, env.cfg.Workspace, "worker")
	}
	return "sess-kill"
}

func tickGateRecorded(rec *events.Fake, typ string) bool {
	for _, e := range rec.Events {
		if e.Type == typ {
			return true
		}
	}
	return false
}

// 6. A tick kill is one bounded observation: a Stop that does not return is a
// failed kill, taking the site's existing error arm, and the scrollback clear
// that follows a kill never runs.
func TestReconcileTickGate_WedgedKillIsFailedKill(t *testing.T) {
	for _, c := range tickKillCases() {
		t.Run(c.name, func(t *testing.T) {
			env, sp := tickGateEnv(t)
			rec := events.NewFake()
			env.rec = rec
			b, it, opts := c.setup(t, env, sp)
			name := killTargetName(env, c)
			sp.wedge("Stop", name)
			before := env.metadataOf(t, b.ID)

			_, elapsed := env.tickWith(t, sp, it, opts, b)

			if limit := tickGateTestBound + tickGateMargin; elapsed > limit {
				t.Fatalf("tick took %s, want at most %s: the kill is held to the bound", elapsed, limit)
			}
			if got := sp.count("Stop", name); got != 1 {
				t.Fatalf("Stop(%s) = %d, want 1: the case must reach the site's kill", name, got)
			}
			if !strings.Contains(env.stderr.String(), c.errLine) {
				t.Errorf("stderr lacks %q: the site's kill-error arm did not run; stderr=%q", c.errLine, env.stderr.String())
			}
			if c.event != "" && tickGateRecorded(rec, c.event) {
				t.Errorf("%s recorded for a kill that did not answer", c.event)
			}
			if got := sp.count("ClearScrollback", name); got != 0 {
				t.Errorf("ClearScrollback(%s) = %d, want 0: kill and clear are one observation, serial", name, got)
			}
			after := env.metadataOf(t, b.ID)
			if c.folded(after) {
				t.Errorf("the post-kill fold was applied to a kill that did not answer: before=%#v after=%#v", before, after)
			}
			if after["last_woke_at"] != before["last_woke_at"] {
				t.Errorf("last_woke_at changed %q -> %q: no re-wake clear for a failed kill", before["last_woke_at"], after["last_woke_at"])
			}

			// The key is held until the wedge lifts: a second tick neither waits
			// nor stops again.
			env.stderr.Reset()
			_, second := env.tickWith(t, sp, it, opts, b)
			if second > tickGateTestBound/2 {
				t.Errorf("second tick took %s, want well under the %s bound", second, tickGateTestBound)
			}
			if got := sp.count("Stop", name); got != 1 {
				t.Errorf("Stop(%s) = %d after the second tick, want still 1", name, got)
			}
		})
	}
}

// 7. Nothing wedged: each tick kill is the kill, then the scrollback clear,
// then the event and the fold, as today.
func TestReconcileTickGate_KillHappyPathUnchanged(t *testing.T) {
	for _, c := range tickKillCases() {
		t.Run(c.name, func(t *testing.T) {
			env, sp := tickGateEnv(t)
			rec := events.NewFake()
			env.rec = rec
			b, it, opts := c.setup(t, env, sp)
			name := killTargetName(env, c)

			env.tickWith(t, sp, it, opts, b)

			if got := sp.count("Stop", name); got != 1 {
				t.Fatalf("Stop(%s) = %d, want 1; stderr=%q", name, got, env.stderr.String())
			}
			if c.clearsScrollback {
				stop, cleared := sp.indexOf("Stop:"+name), sp.indexOf("ClearScrollback:"+name)
				if cleared < 0 || cleared < stop {
					t.Errorf("call order: Stop at %d, ClearScrollback at %d; want the kill, then the clear", stop, cleared)
				}
			}
			if c.event != "" && !tickGateRecorded(rec, c.event) {
				t.Errorf("%s not recorded after a successful kill", c.event)
			}
			if !c.folded(env.metadataOf(t, b.ID)) {
				t.Errorf("the post-kill fold did not land: %#v", env.metadataOf(t, b.ID))
			}
			if lines := unansweredLines(env.stderr.String()); len(lines) != 0 {
				t.Errorf("did-not-answer lines on the happy path = %q, want none", lines)
			}
		})
	}
}
