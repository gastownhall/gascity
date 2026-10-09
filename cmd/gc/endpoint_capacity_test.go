package main

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"math/rand/v2"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/resilience"
	"github.com/gastownhall/gascity/internal/rollout/gate"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// capacityRefusal is the typed error a provider returns when the launched
// command exits 75 during startup: the tmux shape, pane text included.
func capacityRefusal(name, pane string) error {
	return &runtime.CapacityError{
		ExitCode: runtime.ExitCodeTempFail,
		Source:   runtime.CapacitySourceExitStatus,
		Err:      fmt.Errorf("%w: session %q; last pane output:\n%s", runtime.ErrSessionDiedDuringStartup, name, pane),
	}
}

// seededJitter is full jitter from a fixed-seed generator, so backoff
// deadlines are the same on every run.
func seededJitter() func(time.Duration) time.Duration {
	var mu sync.Mutex
	rng := rand.New(rand.NewPCG(1, 2))
	return func(capDur time.Duration) time.Duration {
		if capDur <= 0 {
			return 0
		}
		mu.Lock()
		defer mu.Unlock()
		return time.Duration(rng.Int64N(int64(capDur))) + 1
	}
}

// capacityEnv is a reconciler env whose endpoint guard runs on the env's fake
// clock. Tests advance the clock only between synchronous calls or after an
// awaited fact, because clock.Fake is not goroutine-safe.
type capacityEnv struct {
	*reconcilerTestEnv
	guard  *endpointCapacityGuard
	events *events.Fake
	trace  *sessionReconcilerTraceCycle
	// log collects stdout and stderr; async start goroutines write to it
	// concurrently with the executor.
	log syncBuffer
}

func newCapacityEnv(t *testing.T, guarded bool, templates ...string) *capacityEnv {
	t.Helper()
	env := newReconcilerTestEnv()
	rec := events.NewFake()
	env.rec = rec
	agents := make([]config.Agent, 0, len(templates))
	for _, tmpl := range templates {
		agents = append(agents, config.Agent{Name: tmpl})
	}
	env.cfg = &config.City{Agents: agents}
	e := &capacityEnv{reconcilerTestEnv: env, events: rec, trace: newPoolDesiredStateTestTrace(templates...)}
	if guarded {
		e.guard = newEndpointCapacityGuard(env.clk.Now)
		e.guard.registry.SetJitterForTest(seededJitter())
		env.startOptions = append(env.startOptions, withEndpointCapacityGuard(e.guard))
	}
	env.sp.StartErrors = map[string]error{}
	return e
}

// limitToOneWake sets max_wakes_per_tick to 1.
func (e *capacityEnv) limitToOneWake() {
	one := 1
	e.cfg.Daemon.MaxWakesPerTick = &one
}

// pendingCreate stores a never-started pending-create row for name, served by
// upstream, and registers it as desired. Like production it starts in
// start-pending.
func (e *capacityEnv) pendingCreate(t *testing.T, name, upstream string, extra map[string]string) startCandidate {
	t.Helper()
	meta := map[string]string{
		"session_name":          name,
		"session_name_explicit": "true",
		"pending_create_claim":  "true",
		"template":              name,
		"state":                 "start-pending",
		"generation":            "1",
		"continuation_epoch":    "1",
		"instance_token":        "tok-" + name,
	}
	maps.Copy(meta, extra)
	return e.session(t, name, upstream, meta)
}

func (e *capacityEnv) session(t *testing.T, name, upstream string, meta map[string]string) startCandidate {
	t.Helper()
	tp := TemplateParams{Command: "test-cmd", SessionName: name, TemplateName: name, Upstream: upstream}
	e.desiredState[name] = tp
	b, err := e.store.Create(beads.Bead{
		Title:    name,
		Type:     sessionBeadType,
		Labels:   []string{sessionBeadLabel, "template:" + name},
		Metadata: meta,
	})
	if err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	return startCandidate{info: e.sessionInfo(b.ID), tp: tp}
}

// refreshed re-reads a candidate's row, as the next tick's append would.
func (e *capacityEnv) refreshed(c startCandidate) startCandidate {
	c.info = e.sessionInfo(c.info.ID)
	return c
}

func (e *capacityEnv) bead(t *testing.T, id string) beads.Bead {
	t.Helper()
	b, err := e.store.Get(id)
	if err != nil {
		t.Fatalf("get %s: %v", id, err)
	}
	return b
}

func (e *capacityEnv) start(ctx context.Context, candidates ...startCandidate) int {
	return executePlannedStartsTraced(ctx, candidates, e.cfg, e.desiredState, e.sp, e.store, "", "", e.clk, e.rec, 0, &e.log, &e.log, e.trace, e.startOptions...)
}

func (e *capacityEnv) breakerStatus(k endpointKey) resilience.Status {
	return e.guard.breaker(k).Status()
}

// capacityTestEndpoint is the endpoint every "e"-upstream test session uses.
const capacityTestEndpoint endpointKey = "upstream:e"

// openEndpoint trips capacityTestEndpoint with one refusal that belongs to no
// test session.
func (e *capacityEnv) openEndpoint(t *testing.T) {
	t.Helper()
	const k = capacityTestEndpoint
	ticket, ok := e.guard.Admit(k, "outside", "outside")
	if !ok {
		t.Fatalf("Admit(%s) denied while closed", k)
	}
	ticket.Resolve(verdictCapacity)
	if st := e.breakerStatus(k); st.State != resilience.StateOpen {
		t.Fatalf("%s state = %v after a refusal, want open", k, st.State)
	}
}

// reopen re-trips capacityTestEndpoint with a fresh backoff once its probe is due, leaving it
// refusing with no probe due.
func (e *capacityEnv) reopen(t *testing.T) {
	t.Helper()
	const k = capacityTestEndpoint
	e.makeProbeDue()
	ticket, ok := e.guard.Admit(k, "outside", "outside")
	if !ok || !ticket.probe {
		t.Fatalf("Admit(%s) = (%v, probe %v), want the due probe", k, ok, ticket != nil && ticket.probe)
	}
	ticket.Resolve(verdictCapacity)
}

// makeProbeDue advances past capacityTestEndpoint's backoff cap, so the next
// admission is the probe regardless of jitter.
func (e *capacityEnv) makeProbeDue() {
	e.clk.Advance(e.breakerStatus(capacityTestEndpoint).BackoffCap)
}

func (e *capacityEnv) records(site TraceSiteCode) []SessionReconcilerTraceRecord {
	var out []SessionReconcilerTraceRecord
	for _, r := range e.trace.records {
		if r.SiteCode == site {
			out = append(out, r)
		}
	}
	return out
}

func startupHealthCount(t *testing.T, store beads.Store, name string) int {
	t.Helper()
	episode, err := sessionFrontDoor(store).LoadStartupHealthEpisode(name)
	if err != nil {
		t.Fatalf("load startup-health episode for %s: %v", name, err)
	}
	return episode.ConsecutiveCount
}

func TestResolvedEndpointKey_UpstreamThenLeafProviderThenBeadProvider(t *testing.T) {
	maxProvider := &config.ResolvedProvider{Name: "claude-max", BuiltinAncestor: "claude"}
	ollamaProvider := &config.ResolvedProvider{Name: "ollama-claude", BuiltinAncestor: "claude"}
	beadProvider := sessionpkg.Info{Provider: "codex", SessionName: "worker"}
	for _, tc := range []struct {
		name string
		tp   TemplateParams
		info sessionpkg.Info
		want endpointKey
	}{
		{"upstream wins over provider", TemplateParams{Upstream: " manifold ", ResolvedProvider: maxProvider}, beadProvider, "upstream:manifold"},
		{"leaf provider, not its builtin family", TemplateParams{ResolvedProvider: maxProvider}, beadProvider, "provider:claude-max"},
		{"same family, different leaf", TemplateParams{ResolvedProvider: ollamaProvider}, beadProvider, "provider:ollama-claude"},
		{"bead provider fallback", TemplateParams{SessionName: "worker"}, beadProvider, "provider:codex"},
		{"nothing known is unguarded", TemplateParams{SessionName: "worker"}, sessionpkg.Info{SessionName: "worker"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolvedEndpointKey(tc.tp, tc.info); got != tc.want {
				t.Fatalf("resolvedEndpointKey = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestExecutePlannedStarts_CapacityRefusalOpensBreakerWithoutRollback(t *testing.T) {
	e := newCapacityEnv(t, true, "sky")
	c := e.pendingCreate(t, "sky", "broker", map[string]string{"wake_attempts": "2", "session_key": "conv-1"})
	e.sp.StartErrors["sky"] = capacityRefusal("sky", "native broker tunnel is not listening")

	if woken := e.start(context.Background(), c); woken != 0 {
		t.Fatalf("woken = %d, want 0", woken)
	}

	if st := e.breakerStatus("upstream:broker"); st.State != resilience.StateOpen || st.Trips != 1 {
		t.Fatalf("breaker = %v trips=%d, want open after one refusal", st.State, st.Trips)
	}
	got := e.bead(t, c.info.ID)
	if got.Status != "open" || got.Metadata["pending_create_claim"] != "true" {
		t.Fatalf("row status=%q claim=%q, want the pending create kept open", got.Status, got.Metadata["pending_create_claim"])
	}
	if got.Metadata["last_woke_at"] != "" {
		t.Fatalf("last_woke_at = %q, want the in-flight lease released", got.Metadata["last_woke_at"])
	}
	if got.Metadata["wake_attempts"] != "2" || got.Metadata["session_key"] != "conv-1" {
		t.Fatalf("wake_attempts=%q session_key=%q, want no wake-failure accrual or conversation reset", got.Metadata["wake_attempts"], got.Metadata["session_key"])
	}
	if n := startupHealthCount(t, e.store, "sky"); n != 0 {
		t.Fatalf("startup-health episode count = %d, want 0", n)
	}
	if n := e.sp.CountCalls("Start", "sky"); n != 1 {
		t.Fatalf("provider Start calls = %d, want exactly 1 launch", n)
	}
	if n := e.sp.CountCalls("Peek", "sky"); n != 0 {
		t.Fatalf("provider Peek calls = %d, want no rate-limit peek for a capacity refusal", n)
	}
	refused := e.records(TraceSiteLifecycleStartCapacityRefused)
	if len(refused) != 1 {
		t.Fatalf("capacity_refused records = %d, want 1", len(refused))
	}
	f := refused[0].Fields
	if f["endpoint"] != "upstream:broker" || f["exit_code"] != 75 || f["source"] != runtime.CapacitySourceExitStatus ||
		f["probe"] != false || f["breaker_state_after"] != "open" || f["capacity_valve"] != false {
		t.Fatalf("capacity_refused fields = %v", f)
	}
}

func TestCommitStartResult_CapacityBeatsTerminalClassifier(t *testing.T) {
	e := newCapacityEnv(t, true, "sky")
	c := e.pendingCreate(t, "sky", "broker", nil)
	e.sp.StartErrors["sky"] = capacityRefusal("sky", "broker: quota exceeded, retry later")

	e.start(context.Background(), c)

	if recs := e.records(TraceSiteLifecycleStartTerminalProviderError); len(recs) != 0 {
		t.Fatalf("terminal provider error records = %v, want none for a capacity refusal", recs)
	}
	got := e.bead(t, c.info.ID)
	if got.Status != "open" || got.Metadata["pending_create_claim"] != "true" {
		t.Fatalf("row status=%q claim=%q, want the pending create kept open", got.Status, got.Metadata["pending_create_claim"])
	}
}

func TestCommitStartResult_CapacityWithoutGuardKeepsLegacyAccounting(t *testing.T) {
	e := newCapacityEnv(t, false, "sky")
	c := e.pendingCreate(t, "sky", "broker", nil)
	e.sp.StartErrors["sky"] = capacityRefusal("sky", "native broker tunnel is not listening")

	e.start(context.Background(), c)

	if got := e.bead(t, c.info.ID); got.Status != "closed" {
		t.Fatalf("row status = %q, want the legacy rollback without a guard", got.Status)
	}
	if n := startupHealthCount(t, e.store, "sky"); n != 1 {
		t.Fatalf("startup-health episode count = %d, want 1 (the only throttle without a breaker)", n)
	}
}

func TestExecutePlannedStarts_OpenEndpointDefersWithoutWritesOrBudget(t *testing.T) {
	e := newCapacityEnv(t, true, "template-a", "b", "c")
	e.limitToOneWake()
	e.cfg.Daemon.SessionCircuitBreaker = true
	e.cfg.Daemon.SessionCircuitBreakerMaxRestarts = intPtrCircuit(5)
	e.cfg.Daemon.SessionCircuitBreakerWindow = "30m"
	cb := breakerAt(30*time.Minute, 5)
	defer setSessionCircuitBreakerForTest(cb)()

	named := createCircuitTestNamedSessionWithIdentity(t, e.reconcilerTestEnv, "session-a", "template-a", "rig-a/session-a", "asleep")
	a := startCandidate{info: e.sessionInfo(named.ID), tp: TemplateParams{Command: "test-cmd", SessionName: "session-a", TemplateName: "template-a", Upstream: "e"}}
	b := e.pendingCreate(t, "b", "e", nil)
	c := e.pendingCreate(t, "c", "f", nil)
	e.openEndpoint(t)
	beforeA, beforeB := e.bead(t, a.info.ID), e.bead(t, b.info.ID)

	if woken := e.start(context.Background(), a, b, c); woken != 1 {
		t.Fatalf("woken = %d, want 1 (C within max_wakes=1)", woken)
	}
	if e.sp.CountCalls("Start", "c") != 1 || e.sp.CountCalls("Start", "session-a") != 0 || e.sp.CountCalls("Start", "b") != 0 {
		t.Fatalf("Start calls = %+v, want only c", e.sp.SnapshotCalls())
	}
	for _, before := range []beads.Bead{beforeA, beforeB} {
		if after := e.bead(t, before.ID); !maps.Equal(after.Metadata, before.Metadata) {
			t.Fatalf("deferred row %s changed:\nbefore %v\nafter  %v", before.ID, before.Metadata, after.Metadata)
		}
	}
	for _, snap := range cb.Snapshot(e.clk.Now().UTC()) {
		if snap.Identity == "rig-a/session-a" && snap.RestartCount != 0 {
			t.Fatalf("identity breaker restarts for deferred session-a = %d, want 0", snap.RestartCount)
		}
	}
	if !strings.Contains(e.log.String(), "outcome=deferred_by_endpoint_capacity") {
		t.Fatalf("stderr = %q, want deferred_by_endpoint_capacity", e.log.String())
	}
}

func TestExecutePlannedStarts_HalfOpenAdmitsExactlyOneProbe(t *testing.T) {
	e := newCapacityEnv(t, true, "s1", "s2", "s3")
	cands := []startCandidate{e.pendingCreate(t, "s1", "e", nil), e.pendingCreate(t, "s2", "e", nil), e.pendingCreate(t, "s3", "e", nil)}
	e.openEndpoint(t)
	e.makeProbeDue()

	e.start(context.Background(), cands...)

	starts := 0
	for _, c := range cands {
		starts += e.sp.CountCalls("Start", c.name())
	}
	if starts != 1 {
		t.Fatalf("Start calls on the half-open endpoint = %d, want exactly one probe", starts)
	}
	deferred := 0
	for _, r := range e.records(TraceSiteLifecycleStartRun) {
		if r.OutcomeCode == TraceOutcomeDeferredByEndpointCapacity {
			deferred++
		}
	}
	if deferred != 2 {
		t.Fatalf("deferred_by_endpoint_capacity records = %d, want 2", deferred)
	}
	if st := e.breakerStatus("upstream:e"); st.State != resilience.StateClosed {
		t.Fatalf("breaker = %v after a successful probe, want closed", st.State)
	}
}

// TestExecutePlannedStarts_ProbePassAdmitsNoHerd pins that a pass which
// admitted an endpoint's probe admits nothing else on that endpoint, even
// when the probe resolves before the pass reaches its later batches. A herd
// let in mid-pass is cut short by its own first refusal, so it can be one
// session refused alone after the probe's success, which is the poison valve's
// signature for a broken template.
func TestExecutePlannedStarts_ProbePassAdmitsNoHerd(t *testing.T) {
	names := []string{"s1", "s2", "s3", "s4", "s5", "s6"}
	e := newCapacityEnv(t, true, names...)
	var cands []startCandidate
	for _, n := range names {
		cands = append(cands, e.pendingCreate(t, n, "e", nil))
	}
	e.openEndpoint(t)
	e.makeProbeDue()

	e.start(context.Background(), cands...)

	starts := 0
	for _, c := range cands {
		starts += e.sp.CountCalls("Start", c.name())
	}
	if starts != 1 {
		t.Fatalf("Start calls in the probe's pass = %d, want only the probe", starts)
	}
	if st := e.breakerStatus(capacityTestEndpoint); st.State != resilience.StateClosed {
		t.Fatalf("breaker = %v after a successful probe, want closed", st.State)
	}
}

func TestExecutePlannedStarts_ProbeRotatesAmongRefusedSessions(t *testing.T) {
	e := newCapacityEnv(t, true, "s1", "s2")
	e.limitToOneWake()
	s1 := e.pendingCreate(t, "s1", "e", nil)
	s2 := e.pendingCreate(t, "s2", "e", nil)
	e.sp.StartErrors["s1"] = capacityRefusal("s1", "")
	e.sp.StartErrors["s2"] = capacityRefusal("s2", "")

	e.start(context.Background(), s1, s2) // s1 (oldest) refused while closed; s2 deferred
	if e.sp.CountCalls("Start", "s1") != 1 || e.sp.CountCalls("Start", "s2") != 0 {
		t.Fatalf("first window Start calls = %+v, want s1 only", e.sp.SnapshotCalls())
	}
	s1, s2 = e.refreshed(s1), e.refreshed(s2)
	if !wakeFairnessTime(s1).Before(wakeFairnessTime(s2)) && !wakeFairnessTime(s1).Equal(wakeFairnessTime(s2)) {
		t.Fatalf("s1 no longer sorts first by wake fairness; the test premise is broken")
	}

	e.makeProbeDue()
	e.start(context.Background(), s1, s2)
	if e.sp.CountCalls("Start", "s2") != 1 || e.sp.CountCalls("Start", "s1") != 1 {
		t.Fatalf("second window Start calls = %+v, want the probe rotated to s2", e.sp.SnapshotCalls())
	}
}

func TestExecutePlannedStarts_ProbeSuccessClosesAndCapacityReopensDoubled(t *testing.T) {
	e := newCapacityEnv(t, true, "s", "t")
	s := e.pendingCreate(t, "s", "e", nil)
	tt := e.pendingCreate(t, "t", "e", nil)
	e.sp.StartErrors["s"] = capacityRefusal("s", "")

	e.start(context.Background(), s)
	delete(e.sp.StartErrors, "s")
	e.makeProbeDue()
	e.start(context.Background(), e.refreshed(s))
	if st := e.breakerStatus("upstream:e"); st.State != resilience.StateClosed {
		t.Fatalf("breaker = %v after the probe succeeded, want closed", st.State)
	}

	e.clk.Advance(time.Minute) // inside TripDecay
	e.sp.StartErrors["t"] = capacityRefusal("t", "")
	e.start(context.Background(), tt)
	if st := e.breakerStatus("upstream:e"); st.State != resilience.StateOpen || st.Trips != 2 || st.BackoffCap != 10*time.Second {
		t.Fatalf("breaker = %v trips=%d cap=%v, want a doubled re-trip", st.State, st.Trips, st.BackoffCap)
	}
}

// cancelOnGetStore cancels a context when a given row is read, which lands
// the cancellation after admission and before the start is enqueued.
type cancelOnGetStore struct {
	beads.Store
	id     string
	cancel context.CancelFunc
}

func (s *cancelOnGetStore) Get(id string) (beads.Bead, error) {
	if id == s.id {
		s.cancel()
	}
	return s.Store.Get(id)
}

func TestExecutePlannedStarts_NotAttemptedPathsReleaseProbe(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, e *capacityEnv) (context.Context, startCandidate)
		// samePass: the pass continues after the abandoned admission, so the
		// next queued start on the endpoint must get the probe right away.
		samePass bool
	}{
		{"prepare error", func(t *testing.T, e *capacityEnv) (context.Context, startCandidate) {
			c := e.pendingCreate(t, "s", "e", nil)
			if err := e.store.SetMetadata(c.info.ID, "session_name", "not a valid name"); err != nil {
				t.Fatal(err)
			}
			return context.Background(), e.refreshed(c)
		}, true},
		{"identity breaker open", func(t *testing.T, e *capacityEnv) (context.Context, startCandidate) {
			cb := e.namedIdentityBreaker(t)
			cb.RecordRestart("rig-a/session-a", e.clk.Now().UTC())
			cb.RecordRestart("rig-a/session-a", e.clk.Now().UTC())
			return context.Background(), e.namedCandidate(t)
		}, true},
		{"identity breaker trips", func(t *testing.T, e *capacityEnv) (context.Context, startCandidate) {
			cb := e.namedIdentityBreaker(t)
			cb.RecordRestart("rig-a/session-a", e.clk.Now().UTC())
			return context.Background(), e.namedCandidate(t)
		}, true},
		{"context canceled after admission", func(t *testing.T, e *capacityEnv) (context.Context, startCandidate) {
			c := e.pendingCreate(t, "s", "e", nil)
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			e.store = &cancelOnGetStore{Store: e.store, id: c.info.ID, cancel: cancel}
			return ctx, c
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newCapacityEnv(t, true, "s", "next", "template-a")
			ctx, c := tc.setup(t, e)
			e.openEndpoint(t)
			e.makeProbeDue()

			if !tc.samePass {
				e.start(ctx, c)
				if n := e.sp.CountCalls("Start", c.name()); n != 0 {
					t.Fatalf("Start calls = %d, want the start never attempted", n)
				}
				ticket, ok := e.guard.Admit(capacityTestEndpoint, "next", "next")
				if !ok || !ticket.probe {
					t.Fatal("Admit after an abandoned probe was denied, want a new probe without waiting HalfOpenInterval")
				}
				return
			}
			next := e.pendingCreate(t, "next", "e", nil)
			e.start(ctx, c, next)
			if n := e.sp.CountCalls("Start", c.name()); n != 0 {
				t.Fatalf("Start calls for %s = %d, want the start never attempted", c.name(), n)
			}
			if n := e.sp.CountCalls("Start", "next"); n != 1 {
				t.Fatalf("Start calls for next = %d, want it to take the released probe in the same pass", n)
			}
		})
	}
}

// namedIdentityBreaker enables the per-identity circuit breaker at one
// allowed restart and installs a fresh one for the test.
func (e *capacityEnv) namedIdentityBreaker(t *testing.T) *sessionCircuitBreaker {
	t.Helper()
	e.cfg.Daemon.SessionCircuitBreaker = true
	e.cfg.Daemon.SessionCircuitBreakerMaxRestarts = intPtrCircuit(1)
	e.cfg.Daemon.SessionCircuitBreakerWindow = "30m"
	cb := breakerAt(30*time.Minute, 1)
	t.Cleanup(setSessionCircuitBreakerForTest(cb))
	return cb
}

func (e *capacityEnv) namedCandidate(t *testing.T) startCandidate {
	t.Helper()
	named := createCircuitTestNamedSessionWithIdentity(t, e.reconcilerTestEnv, "session-a", "template-a", "rig-a/session-a", "asleep")
	return startCandidate{info: e.sessionInfo(named.ID), tp: TemplateParams{Command: "test-cmd", SessionName: "session-a", TemplateName: "template-a", Upstream: "e"}}
}

// startAsync runs one async start pass and waits for every start goroutine.
func (e *capacityEnv) startAsync(t *testing.T, candidates ...startCandidate) {
	t.Helper()
	var tracker asyncStartTracker
	opts := append(append([]startExecutionOption(nil), e.startOptions...), withAsyncStartExecution(), withAsyncStartTracker(&tracker))
	executePlannedStartsTraced(context.Background(), candidates, e.cfg, e.desiredState, e.sp, e.store, "", "", e.clk, e.rec, 0, &e.log, &e.log, nil, opts...)
	if !tracker.wait(hangBudget) {
		t.Fatalf("async starts did not finish within the hang budget (%s)", hangBudget)
	}
}

// supersedingStartProvider replaces the row's instance token while Start runs,
// so the async commit finds the start stale.
type supersedingStartProvider struct {
	*runtime.Fake
	store beads.Store
	id    string
}

func (p *supersedingStartProvider) Start(ctx context.Context, name string, cfg runtime.Config) error {
	if err := p.store.SetMetadata(p.id, "instance_token", "tok-newer"); err != nil {
		return err
	}
	return p.Fake.Start(ctx, name, cfg)
}

func TestAsyncStart_ProbeResolvedBeforeCommitEvenWhenStale(t *testing.T) {
	e := newCapacityEnv(t, true, "s")
	c := e.pendingCreate(t, "s", "e", nil)
	fake := e.sp
	e.openEndpoint(t)
	e.makeProbeDue()
	provider := &supersedingStartProvider{Fake: fake, store: e.store, id: c.info.ID}

	var tracker asyncStartTracker
	opts := append(append([]startExecutionOption(nil), e.startOptions...), withAsyncStartExecution(), withAsyncStartTracker(&tracker))
	executePlannedStartsTraced(context.Background(), []startCandidate{c}, e.cfg, e.desiredState, provider, e.store, "", "", e.clk, e.rec, 0, &e.log, &e.log, nil, opts...)
	if !tracker.wait(hangBudget) {
		t.Fatalf("async start did not finish within the hang budget (%s)", hangBudget)
	}

	if !strings.Contains(e.log.String(), "ignoring stale async start result") {
		t.Fatalf("stderr = %q, want the commit to find the start stale", e.log.String())
	}
	if st := e.breakerStatus("upstream:e"); st.State != resilience.StateClosed {
		t.Fatalf("breaker = %v, want closed by the probe's success despite the stale commit", st.State)
	}
}

// panickingStartProvider panics inside provider Start.
type panickingStartProvider struct{ *runtime.Fake }

func (p *panickingStartProvider) Start(context.Context, string, runtime.Config) error {
	panic("provider exploded")
}

func TestAsyncStart_PanickingStartResolvesProbeInconclusive(t *testing.T) {
	e := newCapacityEnv(t, true, "s")
	c := e.pendingCreate(t, "s", "e", nil)
	e.openEndpoint(t)
	e.makeProbeDue()

	var tracker asyncStartTracker
	opts := append(append([]startExecutionOption(nil), e.startOptions...), withAsyncStartExecution(), withAsyncStartTracker(&tracker))
	executePlannedStartsTraced(context.Background(), []startCandidate{c}, e.cfg, e.desiredState, &panickingStartProvider{Fake: e.sp}, e.store, "", "", e.clk, e.rec, 0, &e.log, &e.log, nil, opts...)
	if !tracker.wait(hangBudget) {
		t.Fatalf("async start did not finish within the hang budget (%s)", hangBudget)
	}

	if st := e.breakerStatus("upstream:e"); st.State != resilience.StateHalfOpen {
		t.Fatalf("breaker = %v after a panicking probe, want still half-open", st.State)
	}
	if ticket, ok := e.guard.Admit("upstream:e", "next", "next"); !ok || !ticket.probe {
		t.Fatal("next Admit after a panicking probe was denied, want the probe released")
	}
}

func TestAsyncStart_WarmReuseIsNotProbeSuccess(t *testing.T) {
	e := newCapacityEnv(t, true, "s")
	c := e.session(t, "s", "e", map[string]string{
		"session_name":       "s",
		"template":           "s",
		"state":              "asleep",
		"generation":         "1",
		"continuation_epoch": "1",
		"instance_token":     "tok-s",
	})
	if err := e.sp.Start(context.Background(), "s", runtime.Config{Command: "test-cmd"}); err != nil {
		t.Fatal(err)
	}
	e.openEndpoint(t)
	e.makeProbeDue()

	e.startAsync(t, c)

	if n := e.sp.CountCalls("Start", "s"); n != 1 {
		t.Fatalf("Start calls = %d, want only the seed start (warm reuse)", n)
	}
	if st := e.breakerStatus("upstream:e"); st.State != resilience.StateHalfOpen {
		t.Fatalf("breaker = %v after a warm reuse, want still half-open", st.State)
	}
	if ticket, ok := e.guard.Admit("upstream:e", "next", "next"); !ok || !ticket.probe {
		t.Fatal("next Admit after a warm reuse was denied, want the probe released")
	}
}

// peerSuccess admits and succeeds a start of the "peer" template, closing a
// half-open endpoint.
func peerSuccess(t *testing.T, e *capacityEnv) {
	t.Helper()
	succeedAs(t, e, "peer", "peer")
}

// succeedAs admits and succeeds a start of sessionID, an instance of template.
func succeedAs(t *testing.T, e *capacityEnv, sessionID, template string) {
	t.Helper()
	ticket, ok := e.guard.Admit(capacityTestEndpoint, sessionID, template)
	if !ok {
		t.Fatalf("admission for %s denied", sessionID)
	}
	ticket.Resolve(verdictSuccess)
	e.clk.Advance(time.Second)
}

// refuseAt admits sessionID, an instance of template, and resolves its start
// as refused, then makes the next probe due. It reports whether the valve
// tripped.
func refuseAt(t *testing.T, e *capacityEnv, sessionID, template string) bool {
	t.Helper()
	ticket, ok := e.guard.Admit(capacityTestEndpoint, sessionID, template)
	if !ok {
		t.Fatalf("admission for %s denied", sessionID)
	}
	valve := ticket.Resolve(verdictCapacity)
	e.makeProbeDue()
	return valve
}

func TestCommitStartResult_CapacityValveReturnsSessionToLegacyAfterPeerSuccesses(t *testing.T) {
	t.Run("outage refusals never count", func(t *testing.T) {
		e := newCapacityEnv(t, true)
		for i := 0; i < 2*capacityBackstopRefusals; i++ {
			if refuseAt(t, e, "s", "s") {
				t.Fatalf("valve tripped on outage refusal %d with no success anywhere", i+1)
			}
		}
	})

	t.Run("refusals of other templates in between never count", func(t *testing.T) {
		e := newCapacityEnv(t, true)
		for i := 0; i < capacityBackstopCloses-1; i++ {
			peerSuccess(t, e)
			if refuseAt(t, e, "other", "other") || refuseAt(t, e, "s", "s") {
				t.Fatalf("valve tripped on round %d although another template was refused too", i+1)
			}
		}
	})

	t.Run("the first refusal never counts", func(t *testing.T) {
		e := newCapacityEnv(t, true)
		// threshold refusals, each after a peer success: the first does not
		// count, so the valve is one refusal short.
		for i := 0; i < capacityValveThreshold; i++ {
			peerSuccess(t, e)
			if refuseAt(t, e, "s", "s") {
				t.Fatalf("valve tripped on refusal %d; the first refusal must not count", i+1)
			}
		}
	})

	t.Run("refusals between peer successes return to legacy accounting", func(t *testing.T) {
		e := newCapacityEnv(t, true, "s")
		c := e.pendingCreate(t, "s", "e", nil)
		for i := 0; i < capacityValveThreshold; i++ {
			if i > 0 {
				peerSuccess(t, e)
			}
			if refuseAt(t, e, c.info.ID, "s") {
				t.Fatalf("valve tripped early on refusal %d", i+1)
			}
		}
		peerSuccess(t, e)
		e.sp.StartErrors["s"] = capacityRefusal("s", "")

		e.start(context.Background(), c)

		if got := e.bead(t, c.info.ID); got.Status != "closed" {
			t.Fatalf("row status = %q, want the legacy rollback once the valve trips", got.Status)
		}
		refused := e.records(TraceSiteLifecycleStartCapacityRefused)
		if len(refused) != 1 || refused[0].Fields["capacity_valve"] != true {
			t.Fatalf("capacity_refused records = %v, want one with capacity_valve=true", refused)
		}
		if st := e.breakerStatus(capacityTestEndpoint); st.State != resilience.StateClosed {
			t.Fatalf("breaker = %v, want the valve-tripped refusal to leave the endpoint closed", st.State)
		}
	})

	t.Run("two poisoned seats of one template are valved", func(t *testing.T) {
		e := newCapacityEnv(t, true)
		seats := []string{"seat-1", "seat-2"}
		tripped := -1
		for i := 0; i < 2*capacityValveThreshold+2 && tripped < 0; i++ {
			peerSuccess(t, e)
			if refuseAt(t, e, seats[i%2], "pool") {
				tripped = i
			}
		}
		if tripped < 0 {
			t.Fatal("two always-refused seats of one template masked each other; the valve never tripped")
		}
	})

	t.Run("replacement beads keep the count", func(t *testing.T) {
		e := newCapacityEnv(t, true)
		for i := 0; i < capacityValveThreshold; i++ {
			if i > 0 {
				peerSuccess(t, e)
			}
			if refuseAt(t, e, fmt.Sprintf("bead-%d", i), "pool") {
				t.Fatalf("valve tripped early on refusal %d", i+1)
			}
		}
		peerSuccess(t, e)
		if !refuseAt(t, e, "bead-new", "pool") {
			t.Fatal("a replacement bead of the template reset the valve count")
		}
	})

	t.Run("the template's own success clears its history", func(t *testing.T) {
		e := newCapacityEnv(t, true)
		for i := 0; i < capacityValveThreshold; i++ {
			if i > 0 {
				peerSuccess(t, e)
			}
			refuseAt(t, e, "s", "s")
		}
		succeedAs(t, e, "s2", "s") // another seat of the template got in
		for i := 0; i < capacityValveThreshold; i++ {
			peerSuccess(t, e)
			if refuseAt(t, e, "s", "s") {
				t.Fatalf("valve tripped on refusal %d after the template's own success", i+1)
			}
		}
	})

	t.Run("herd refusals after a close never feed the backstop", func(t *testing.T) {
		e := newCapacityEnv(t, true)
		for i := 0; i < 2*capacityBackstopRefusals; i++ {
			peerSuccess(t, e) // the probe closes the endpoint
			// Admitted while closed, "herd" is part of the herd a close lets
			// in; "other" then takes the due probe.
			if refuseAt(t, e, "herd", "herd") {
				t.Fatalf("valve tripped on round %d for a template refused only as part of the herd", i+1)
			}
			refuseAt(t, e, "other", "other")
		}
	})

	t.Run("herd resolution order never counts", func(t *testing.T) {
		// Each close admits a herd of "s" and one other template, all while
		// closed. Their concurrent starts resolve in arbitrary order, so "s"
		// can be refused last in one herd and first in the next, with no
		// other refusal in between. Its peers were refused in both herds.
		e := newCapacityEnv(t, true)
		for i := 0; i < 2*capacityValveThreshold+2; i++ {
			peerSuccess(t, e) // the probe closes the endpoint
			other := fmt.Sprintf("other-%d", i)
			s, ok := e.guard.Admit(capacityTestEndpoint, "s", "s")
			o, ok2 := e.guard.Admit(capacityTestEndpoint, other, other)
			if !ok || !ok2 || s.probe || o.probe {
				t.Fatalf("round %d: herd not admitted while closed", i+1)
			}
			first, second := o, s
			if i%2 == 1 {
				first, second = s, o
			}
			if first.Resolve(verdictCapacity) || second.Resolve(verdictCapacity) {
				t.Fatalf("valve tripped on round %d for a template refused only as part of a herd", i+1)
			}
			e.makeProbeDue()
		}
	})

	t.Run("backstop trips a template that never gets in under saturation", func(t *testing.T) {
		e := newCapacityEnv(t, true)
		tripped := false
		for i := 0; i < 2*capacityBackstopRefusals && !tripped; i++ {
			peerSuccess(t, e)
			refuseAt(t, e, "other", "other") // other templates are refused too
			tripped = refuseAt(t, e, "bad", "bad")
		}
		if !tripped {
			t.Fatal("a template refused on every attempt while others got in was never valved")
		}
	})
}

// TestEndpointCapacity_ValveQuietUnderSaturation runs an hour of a saturated
// pool that frees one seat every third 10s tick while forty queued sessions
// take turns asking for it. Healthy sessions refused under steady saturation
// must never be sent to legacy failure accounting, whether they share one
// template, spread over a few, or each have their own.
func TestEndpointCapacity_ValveQuietUnderSaturation(t *testing.T) {
	for _, templates := range []int{1, 8, 40} {
		t.Run(fmt.Sprintf("%d templates", templates), func(t *testing.T) {
			e := newCapacityEnv(t, true)
			const maxWakes, ticks, queued = 5, 360, 40
			next, freeSeats, refused, succeeded, valveTrips := 0, 0, 0, 0, 0
			for tick := 0; tick < ticks; tick++ {
				if tick%3 == 0 {
					freeSeats++
				}
				for i := 0; i < maxWakes; i++ {
					n := next % queued
					ticket, ok := e.guard.Admit(capacityTestEndpoint, fmt.Sprintf("s%02d", n), fmt.Sprintf("t%02d", n%templates))
					if !ok {
						break
					}
					next++
					if freeSeats > 0 {
						freeSeats--
						succeeded++
						ticket.Resolve(verdictSuccess)
						continue
					}
					refused++
					if ticket.Resolve(verdictCapacity) {
						valveTrips++
					}
				}
				e.clk.Advance(10 * time.Second)
			}
			if refused == 0 || succeeded == 0 {
				t.Fatalf("scenario not saturated: refused=%d succeeded=%d", refused, succeeded)
			}
			if valveTrips != 0 {
				t.Fatalf("valve tripped %d times under steady saturation (refused=%d succeeded=%d), want 0", valveTrips, refused, succeeded)
			}
		})
	}
}

func TestEndpointCapacityGuard_ValveHistoryExpires(t *testing.T) {
	e := newCapacityEnv(t, true)
	for i := 0; i < capacityValveThreshold; i++ {
		if i > 0 {
			peerSuccess(t, e)
		}
		refuseAt(t, e, "s", "s")
	}
	// One counted refusal short of tripping; after a quiet hour the history
	// is gone and the next refusal is a first refusal again.
	e.clk.Advance(capacityValveMemory + time.Minute)
	peerSuccess(t, e)
	if refuseAt(t, e, "s", "s") {
		t.Fatal("valve tripped on a refusal after the history expired")
	}
}

func TestEndpointCapacityGuard_CloseClearsEpisodeRefusals(t *testing.T) {
	e := newCapacityEnv(t, true)
	refuseAt(t, e, "s", "s")
	if n := e.guard.refusalsInEpisode(capacityTestEndpoint, "s"); n != 1 {
		t.Fatalf("refusals this episode = %d, want 1", n)
	}
	peerSuccess(t, e)
	if n := e.guard.refusalsInEpisode(capacityTestEndpoint, "s"); n != 0 {
		t.Fatalf("refusals after the endpoint closed = %d, want 0 (a new episode starts fresh)", n)
	}
}

func TestEndpointCapacityGuard_StaleProbeDoesNotReleaseNewerProbe(t *testing.T) {
	const k = capacityTestEndpoint
	e := newCapacityEnv(t, true)
	straggler, _ := e.guard.Admit(k, "straggler", "straggler")
	e.openEndpoint(t)
	e.makeProbeDue()
	stale, ok := e.guard.Admit(k, "p1", "p1")
	if !ok || !stale.probe {
		t.Fatal("first probe not admitted")
	}
	straggler.Resolve(verdictCapacity) // re-opens the half-open endpoint
	e.makeProbeDue()
	if current, ok := e.guard.Admit(k, "p2", "p2"); !ok || !current.probe {
		t.Fatal("second probe not admitted")
	}

	stale.Resolve(verdictInconclusive)

	if _, ok := e.guard.Admit(k, "p3", "p3"); ok {
		t.Fatal("a third start was admitted: the stale probe released the probe in flight")
	}
}

func TestCommitStartResult_CapacityRestoresConsumedExplicitWake(t *testing.T) {
	e := newCapacityEnv(t, true, "s")
	requestedAt := e.clk.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
	c := e.session(t, "s", "e", map[string]string{
		"session_name":       "s",
		"template":           "s",
		"state":              "asleep",
		"generation":         "1",
		"continuation_epoch": "1",
		"instance_token":     "tok-s",
		"wake_request":       "manual",
		"wake_requested_at":  requestedAt,
	})
	e.sp.StartErrors["s"] = capacityRefusal("s", "")

	e.start(context.Background(), c)

	got := e.bead(t, c.info.ID)
	if got.Metadata["wake_request"] != "manual" || got.Metadata["wake_requested_at"] != requestedAt {
		t.Fatalf("wake_request=%q wake_requested_at=%q, want the consumed explicit wake restored", got.Metadata["wake_request"], got.Metadata["wake_requested_at"])
	}
	if got.Metadata["last_woke_at"] != "" {
		t.Fatalf("last_woke_at = %q, want released", got.Metadata["last_woke_at"])
	}
}

func (e *capacityEnv) reconcileTraced(sessions ...beads.Bead) int {
	poolDesired := make(map[string]int)
	for _, tp := range e.desiredState {
		poolDesired[tp.TemplateName]++
	}
	cfgNames := configuredSessionNames(e.cfg, "", e.store)
	return reconcileSessionBeadsTraced(
		context.Background(), "", sessions, e.desiredState, cfgNames, e.cfg, e.sp,
		e.store, nil, nil, nil, nil, e.dt, poolDesired, false, nil, "",
		nil, e.clk, e.rec, 0, 0, &e.log, &e.log, e.trace,
		e.startOptions...,
	)
}

func TestReconcileSessionBeads_EndpointOpenSkipsWakeWithoutBudget(t *testing.T) {
	e := newCapacityEnv(t, true, "a", "c")
	e.limitToOneWake()
	a := e.pendingCreate(t, "a", "e", nil)
	c := e.pendingCreate(t, "c", "f", nil)
	e.openEndpoint(t)
	beforeA := e.bead(t, a.info.ID)

	if woken := e.reconcileTraced(beforeA, e.bead(t, c.info.ID)); woken != 1 {
		t.Fatalf("woken = %d, want 1", woken)
	}
	if e.sp.CountCalls("Start", "c") != 1 || e.sp.CountCalls("Start", "a") != 0 {
		t.Fatalf("Start calls = %+v, want only c", e.sp.SnapshotCalls())
	}
	for _, r := range e.records(TraceSiteReconcilerWakeDecision) {
		if r.SessionName == "a" && r.OutcomeCode == TraceOutcomeStartCandidate {
			t.Fatal("a on the open endpoint became a start candidate")
		}
	}
	// The ordinary per-tick heal may still touch the row; the gate must stop
	// everything a wake writes.
	after := e.bead(t, a.info.ID)
	for _, key := range []string{"last_woke_at", "generation", "instance_token", "currently_processing_bead_id"} {
		if after.Metadata[key] != beforeA.Metadata[key] {
			t.Fatalf("gated row %s = %q, want unchanged %q", key, after.Metadata[key], beforeA.Metadata[key])
		}
	}
	ticks := e.records(TraceSiteEndpointCapacityBreaker)
	if len(ticks) != 1 || ticks[0].Fields["deferred_wake"] != 1 {
		t.Fatalf("endpoint records = %v, want one counting the gated wake as deferred_wake=1", ticks)
	}
}

func TestReconcileSessionBeads_EndpointHoldDefersNeverStartedLeaseRollback(t *testing.T) {
	t.Run("held while open", func(t *testing.T) {
		e := newCapacityEnv(t, true, "a")
		a := e.pendingCreate(t, "a", "e", map[string]string{"pending_create_started_at": e.clk.Now().UTC().Format(time.RFC3339)})
		e.openEndpoint(t)
		e.clk.Advance(11 * time.Minute)
		e.reopen(t) // still refusing, no probe due

		for tick := 0; tick < 2; tick++ {
			e.reconcileTraced(e.bead(t, a.info.ID))
			requireHeldPendingCreate(t, e.bead(t, a.info.ID))
		}
		held := false
		for _, r := range e.records(TraceSiteReconcilerPendingCreate) {
			if r.OutcomeCode == TraceOutcomeDeferred && r.ReasonCode == TraceReasonEndpointCapacityOpen && r.Fields["endpoint"] == "upstream:e" {
				held = true
			}
		}
		if !held {
			t.Fatalf("rollback_pending_create records = %v, want deferred/endpoint_capacity_open", e.records(TraceSiteReconcilerPendingCreate))
		}
	})

	t.Run("held for the window after the endpoint closes", func(t *testing.T) {
		e := newCapacityEnv(t, true, "a")
		a := e.pendingCreate(t, "a", "e", map[string]string{"pending_create_started_at": e.clk.Now().UTC().Format(time.RFC3339)})
		e.clk.Advance(6 * time.Minute)
		e.openEndpoint(t)
		e.makeProbeDue()
		peerSuccess(t, e)
		e.clk.Advance(5 * time.Minute) // row 11m old, endpoint closed 5m

		e.reconcileTraced(e.bead(t, a.info.ID))

		// Held from rollback, the row then wakes normally on the closed endpoint.
		if got := e.bead(t, a.info.ID); got.Status != "open" || e.sp.CountCalls("Start", "a") != 1 {
			t.Fatalf("row status=%q Start calls=%d, want held from rollback inside the post-close window and then started", got.Status, e.sp.CountCalls("Start", "a"))
		}
	})

	t.Run("a started lease still rolls back while open", func(t *testing.T) {
		e := newCapacityEnv(t, true, "a")
		a := e.pendingCreate(t, "a", "e", map[string]string{
			"pending_create_started_at": e.clk.Now().UTC().Format(time.RFC3339),
			"last_woke_at":              e.clk.Now().UTC().Format(time.RFC3339),
		})
		e.openEndpoint(t)
		e.clk.Advance(11 * time.Minute)
		e.reopen(t)

		e.reconcileTraced(e.bead(t, a.info.ID))

		if got := e.bead(t, a.info.ID); got.Status != "closed" {
			t.Fatalf("row status = %q, want the lease-expired rollback for a started lease", got.Status)
		}
	})

	t.Run("rolls back once closed for the full window", func(t *testing.T) {
		e := newCapacityEnv(t, true, "a")
		a := e.pendingCreate(t, "a", "e", map[string]string{"pending_create_started_at": e.clk.Now().UTC().Format(time.RFC3339)})
		e.openEndpoint(t)
		e.makeProbeDue()
		probe, _ := e.guard.Admit("upstream:e", "peer", "peer")
		probe.Resolve(verdictSuccess)
		e.clk.Advance(11 * time.Minute)

		e.reconcileTraced(e.bead(t, a.info.ID))

		if got := e.bead(t, a.info.ID); got.Status != "closed" {
			t.Fatalf("row status = %q, want the lease-expired rollback after the hold window", got.Status)
		}
	})

	t.Run("not-desired row rolls back while open", func(t *testing.T) {
		// The agent is still configured but the row has left the desired
		// set: its demand is gone, so the lease rollback must not be held.
		e := newCapacityEnv(t, true, "a")
		a := e.pendingCreate(t, "a", "e", map[string]string{"pending_create_started_at": e.clk.Now().UTC().Format(time.RFC3339)})
		delete(e.desiredState, "a")
		e.openEndpoint(t)
		e.clk.Advance(11 * time.Minute)
		e.reopen(t)

		e.reconcileTraced(e.bead(t, a.info.ID))

		got := e.bead(t, a.info.ID)
		if got.Status != "closed" || got.Metadata["state"] != "failed-create" {
			t.Fatalf("not-desired row status=%q state=%q, want the lease-expired rollback regardless of the endpoint", got.Status, got.Metadata["state"])
		}
	})
}

// lockProbingRecorder reads guard state from inside Record, which deadlocks
// if an event is ever emitted while a breaker lock is held.
type lockProbingRecorder struct {
	*events.Fake
	guard *endpointCapacityGuard
}

func (r *lockProbingRecorder) Record(ev events.Event) {
	r.guard.Snapshot()
	r.Fake.Record(ev)
}

func TestEndpointCapacity_PerTickRecordAndTransitionEvent(t *testing.T) {
	e := newCapacityEnv(t, true, "a", "b")
	e.limitToOneWake()
	e.rec = &lockProbingRecorder{Fake: e.events, guard: e.guard}
	a := e.pendingCreate(t, "a", "e", nil)
	b := e.pendingCreate(t, "b", "e", nil)
	e.sp.StartErrors["a"] = capacityRefusal("a", "")

	done := make(chan struct{})
	go func() {
		defer close(done)
		e.reconcileTraced(e.bead(t, a.info.ID), e.bead(t, b.info.ID))
	}()
	awaitClose(t, done, "reconcile with a refused start")

	ticks := e.records(TraceSiteEndpointCapacityBreaker)
	if len(ticks) != 1 {
		t.Fatalf("endpoint records = %d, want one for the open endpoint", len(ticks))
	}
	tick := ticks[0]
	if tick.OutcomeCode != TraceOutcomeOpen || tick.ReasonCode != TraceReasonEndpointCapacityOpen || tick.TraceSource != TraceSourceAlwaysOn {
		t.Fatalf("endpoint record outcome=%q reason=%q source=%q", tick.OutcomeCode, tick.ReasonCode, tick.TraceSource)
	}
	for _, key := range []string{"endpoint", "state", "trips", "backoff_cap_ms", "next_probe_at", "open_since", "deferred_wake", "deferred_admit", "probe_in_flight"} {
		if _, ok := tick.Fields[key]; !ok {
			t.Fatalf("endpoint record missing field %q: %v", key, tick.Fields)
		}
	}
	if tick.Fields["endpoint"] != "upstream:e" || tick.Fields["trips"] != 1 || tick.Fields["deferred_admit"] != 1 || tick.Fields["backoff_cap_ms"] != int64(5000) {
		t.Fatalf("endpoint record fields = %v", tick.Fields)
	}

	evs, err := e.events.List(events.Filter{Type: breakerStateChangedEventType})
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 {
		t.Fatalf("breaker.state_changed events = %d, want 1", len(evs))
	}
	var payload breakerStateChangedPayload
	if err := json.Unmarshal(evs[0].Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Scope != "upstream:e" || payload.OpClass != resilience.OpClassSessionStart || payload.From != "closed" || payload.To != "open" || payload.BackoffMs <= 0 {
		t.Fatalf("breaker.state_changed payload = %+v", payload)
	}
}

// requireHeldPendingCreate asserts the row is still a claimed pending create
// with its conversation intact.
func requireHeldPendingCreate(t *testing.T, got beads.Bead) {
	t.Helper()
	state := got.Metadata["state"]
	if got.Status != "open" || got.Metadata["pending_create_claim"] != "true" || (state != "start-pending" && state != "creating") || got.Metadata["continuation_reset_pending"] != "" {
		t.Fatalf("held row status=%q claim=%q state=%q continuation_reset_pending=%q, want an open claimed pending create with no conversation reset",
			got.Status, got.Metadata["pending_create_claim"], state, got.Metadata["continuation_reset_pending"])
	}
}

func TestReconcileSessionBeads_CapacityRefusalKeepsConversationPastStaleCreating(t *testing.T) {
	e := newCapacityEnv(t, true, "s")
	c := e.session(t, "s", "e", map[string]string{
		"session_name":        "s",
		"template":            "s",
		"state":               "asleep",
		"sleep_reason":        "idle",
		"generation":          "1",
		"continuation_epoch":  "1",
		"instance_token":      "tok-s",
		"session_key":         "conv-1",
		"started_config_hash": "hash-1",
	})
	e.sp.StartErrors["s"] = capacityRefusal("s", "")
	e.start(context.Background(), c)

	e.clk.Advance(staleCreatingStateTimeout + time.Minute)
	e.reconcileTraced(e.bead(t, c.info.ID))

	got := e.bead(t, c.info.ID)
	if got.Metadata["session_key"] != "conv-1" || got.Metadata["started_config_hash"] != "hash-1" || got.Metadata["continuation_reset_pending"] != "" {
		t.Fatalf("session_key=%q started_config_hash=%q continuation_reset_pending=%q, want the conversation kept after a capacity refusal",
			got.Metadata["session_key"], got.Metadata["started_config_hash"], got.Metadata["continuation_reset_pending"])
	}
	if got.Metadata["state"] != "asleep" || got.Metadata["sleep_reason"] != "idle" {
		t.Fatalf("state=%q sleep_reason=%q, want the pre-wake state restored", got.Metadata["state"], got.Metadata["sleep_reason"])
	}
}

func TestReapStaleSessionBeads_KeepsRefusedHeldPendingCreate(t *testing.T) {
	e := newCapacityEnv(t, true, "a")
	a := e.pendingCreate(t, "a", "e", map[string]string{
		"state":                     "start-pending",
		"pending_create_started_at": e.clk.Now().UTC().Format(time.RFC3339),
	})
	e.openEndpoint(t)
	e.clk.Advance(11 * time.Minute)
	e.makeProbeDue()
	e.sp.StartErrors["a"] = capacityRefusal("a", "")
	e.start(context.Background(), e.refreshed(a))
	e.clk.Advance(2 * time.Second)

	reapStaleSessionBeads(e.store, e.sp, e.dt, nil, e.clk, &e.log)

	requireHeldPendingCreate(t, e.bead(t, a.info.ID))
}

func TestExecutePlannedStarts_ProbeRotationStaysWithinEndpointSlots(t *testing.T) {
	e := newCapacityEnv(t, true, "e1", "f0", "f1", "f2", "f3")
	e.limitToOneWake()
	e1 := e.pendingCreate(t, "e1", "e", nil) // oldest
	refuseAt(t, e, e1.info.ID, "e1")         // refused once this episode; probe now due
	for i := 0; i < 4; i++ {
		f := e.pendingCreate(t, fmt.Sprintf("f%d", i), "f", nil) // newer, on a healthy endpoint
		e.start(context.Background(), e.refreshed(e1), f)
		if e.sp.CountCalls("Start", "e1") > 0 {
			return
		}
		e.makeProbeDue()
	}
	t.Fatal("e1, the only session on the half-open endpoint, never got the probe: rotation demoted it behind another endpoint's backlog")
}

func TestReconcileSessionBeads_HalfOpenEndpointAdmitsProbe(t *testing.T) {
	e := newCapacityEnv(t, true, "a")
	a := e.pendingCreate(t, "a", "e", nil)
	e.openEndpoint(t)
	e.makeProbeDue()

	e.reconcileTraced(e.bead(t, a.info.ID))

	if n := e.sp.CountCalls("Start", "a"); n != 1 {
		t.Fatalf("Start calls = %d, want the wake gate to leave the due probe for admission", n)
	}
	if st := e.breakerStatus(capacityTestEndpoint); st.State != resilience.StateClosed {
		t.Fatalf("breaker = %v, want closed by the successful probe", st.State)
	}
}

func TestAsyncStart_CapacityRefusalTakesCapacityArm(t *testing.T) {
	e := newCapacityEnv(t, true, "s")
	c := e.pendingCreate(t, "s", "e", nil)
	e.sp.StartErrors["s"] = capacityRefusal("s", "")

	e.startAsync(t, c)

	requireHeldPendingCreate(t, e.bead(t, c.info.ID))
	if st := e.breakerStatus(capacityTestEndpoint); st.State != resilience.StateOpen {
		t.Fatalf("breaker = %v, want open after the async refusal", st.State)
	}
	if n := startupHealthCount(t, e.store, "s"); n != 0 {
		t.Fatalf("startup-health episode count = %d, want 0", n)
	}
}

func TestAsyncStart_CapacityRefusalDefersDriftRollback(t *testing.T) {
	e := newCapacityEnv(t, true, "s")
	// The row's command differs from the desired one, so the async commit
	// finds a drifted pending create it would otherwise roll back.
	c := e.pendingCreate(t, "s", "e", map[string]string{"command": "old-cmd"})
	e.sp.StartErrors["s"] = capacityRefusal("s", "")

	e.startAsync(t, c)

	if !strings.Contains(e.log.String(), "rolling back pending create for s") {
		t.Fatalf("log = %q, want the commit to see the drifted pending create", e.log.String())
	}
	got := e.bead(t, c.info.ID)
	if got.Status != "open" || got.Metadata["pending_create_claim"] != "true" {
		t.Fatalf("row status=%q claim=%q, want a refused start to hold its row like a deferral", got.Status, got.Metadata["pending_create_claim"])
	}
	if got.Metadata["state"] != "start-pending" || got.Metadata["last_woke_at"] != "" {
		t.Fatalf("state=%q last_woke_at=%q, want the drifted refusal's PreWake undone", got.Metadata["state"], got.Metadata["last_woke_at"])
	}
}

func TestCommitStartResult_ConvergedResultIgnoresCapacityArm(t *testing.T) {
	e := newCapacityEnv(t, true, "s")
	c := e.pendingCreate(t, "s", "e", nil)
	prepared, err := prepareStartCandidateForCity(c, "", "", e.cfg, e.sp, e.store, e.clk, &e.log, nil, dispatchOptionSources{})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	prepared.capacityTicket, _ = e.guard.Admit(capacityTestEndpoint, c.info.ID, c.info.ID)
	// An async commit that found a matching runtime clears err but keeps the
	// refusal flag the ticket resolution set.
	result := startResult{prepared: *prepared, outcome: TraceOutcomeSessionExistsConverged, capacityRefused: true}

	verdict := commitStartResultTraced(result, sessionFrontDoor(e.store), e.clk, e.rec, 0, &e.log, &e.log, e.trace)

	if verdict != startCommitSucceeded {
		t.Fatalf("commit verdict = %v, want a converged start committed as started", verdict)
	}
	if recs := e.records(TraceSiteLifecycleStartCapacityRefused); len(recs) != 0 {
		t.Fatalf("capacity_refused records = %v, want none for a converged start", recs)
	}
}

// midStartWriter applies a metadata patch to the row while provider Start
// runs, standing in for an operator command landing during the start window,
// then refuses the start.
type midStartWriter struct {
	*runtime.Fake
	store beads.Store
	id    string
	patch map[string]string
}

func (p *midStartWriter) Start(ctx context.Context, name string, cfg runtime.Config) error {
	p.Fake.Start(ctx, name, cfg) //nolint:errcheck // records the call; the refusal below is the result
	p.Stop(name)                 //nolint:errcheck
	if err := p.store.SetMetadataBatch(p.id, p.patch); err != nil {
		return err
	}
	return capacityRefusal(name, "")
}

// suspendPatch is what `gc session suspend` writes on a managed city.
func suspendPatch(now time.Time) map[string]string {
	return map[string]string{
		"held_until":   now.Add(indefiniteHoldDuration).UTC().Format(time.RFC3339),
		"sleep_intent": "user-hold",
		"state":        "suspended",
	}
}

func asleepSession(t *testing.T, e *capacityEnv, extra map[string]string) startCandidate {
	t.Helper()
	meta := map[string]string{
		"session_name":        "s",
		"template":            "s",
		"state":               "asleep",
		"sleep_reason":        "idle",
		"generation":          "1",
		"continuation_epoch":  "1",
		"instance_token":      "tok-s",
		"session_key":         "conv-1",
		"started_config_hash": "hash-1",
	}
	maps.Copy(meta, extra)
	return e.session(t, "s", "e", meta)
}

func requireSuspendKept(t *testing.T, got beads.Bead) {
	t.Helper()
	if got.Metadata["state"] != "suspended" || got.Metadata["sleep_intent"] != "user-hold" || got.Metadata["held_until"] == "" {
		t.Fatalf("state=%q sleep_intent=%q held_until=%q, want the suspend that landed during the start kept",
			got.Metadata["state"], got.Metadata["sleep_intent"], got.Metadata["held_until"])
	}
}

func TestCommitStartResult_CapacityRestoreKeepsSuspendDuringStart(t *testing.T) {
	t.Run("sync", func(t *testing.T) {
		e := newCapacityEnv(t, true, "s")
		c := asleepSession(t, e, nil)
		provider := &midStartWriter{Fake: e.sp, store: e.store, id: c.info.ID, patch: suspendPatch(e.clk.Now())}

		executePlannedStartsTraced(context.Background(), []startCandidate{c}, e.cfg, e.desiredState, provider, e.store, "", "", e.clk, e.rec, 0, &e.log, &e.log, e.trace, e.startOptions...)

		got := e.bead(t, c.info.ID)
		requireSuspendKept(t, got)
		if got.Metadata["last_woke_at"] != "" {
			t.Fatalf("last_woke_at = %q, want the attempt's lease released", got.Metadata["last_woke_at"])
		}
	})

	t.Run("async", func(t *testing.T) {
		e := newCapacityEnv(t, true, "s")
		c := asleepSession(t, e, nil)
		provider := &midStartWriter{Fake: e.sp, store: e.store, id: c.info.ID, patch: suspendPatch(e.clk.Now())}

		e.startAsyncWith(t, provider, c)

		// The commit refresh sees the suspend and discards the start before
		// any restore; the race that reaches the restore is the next case.
		if !strings.Contains(e.log.String(), "ignoring stale async start result for s") {
			t.Fatalf("log = %q, want the refresh to discard the suspended start", e.log.String())
		}
		requireSuspendKept(t, e.bead(t, c.info.ID))
	})

	t.Run("async, suspend after the commit refresh", func(t *testing.T) {
		e := newCapacityEnv(t, true, "s")
		c := asleepSession(t, e, nil)
		store := &writeAfterNextGetStore{Store: e.store, id: c.info.ID, patch: suspendPatch(e.clk.Now())}
		e.store = store
		provider := &armingRefuser{Fake: e.sp, arm: store.arm}

		e.startAsyncWith(t, provider, c)

		if !strings.Contains(e.log.String(), "endpoint refused (capacity)") {
			t.Fatalf("log = %q, want the refused start to reach the capacity commit arm", e.log.String())
		}
		requireSuspendKept(t, e.bead(t, c.info.ID))
	})
}

// writeAfterNextGetStore applies patch just after the first read of the row
// that follows arm, so that read sees the row as it was and every later read
// sees the write: a command landing between the async commit's refresh and
// the restore's own re-read.
type writeAfterNextGetStore struct {
	beads.Store
	id    string
	patch map[string]string
	mu    sync.Mutex
	armed bool
}

func (s *writeAfterNextGetStore) arm() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.armed = true
}

func (s *writeAfterNextGetStore) Get(id string) (beads.Bead, error) {
	b, err := s.Store.Get(id)
	s.mu.Lock()
	fire := s.armed && id == s.id
	if fire {
		s.armed = false
	}
	s.mu.Unlock()
	if fire {
		if werr := s.SetMetadataBatch(s.id, s.patch); werr != nil {
			return b, werr
		}
	}
	return b, err
}

// armingRefuser refuses every start and arms its hook just before returning.
type armingRefuser struct {
	*runtime.Fake
	arm func()
}

func (p *armingRefuser) Start(ctx context.Context, name string, cfg runtime.Config) error {
	p.Fake.Start(ctx, name, cfg) //nolint:errcheck // records the call; the refusal below is the result
	p.Stop(name)                 //nolint:errcheck
	p.arm()
	return capacityRefusal(name, "")
}

// startAsyncWith is startAsync against a wrapped provider.
func (e *capacityEnv) startAsyncWith(t *testing.T, sp runtime.Provider, candidates ...startCandidate) {
	t.Helper()
	var tracker asyncStartTracker
	opts := append(append([]startExecutionOption(nil), e.startOptions...), withAsyncStartExecution(), withAsyncStartTracker(&tracker))
	executePlannedStartsTraced(context.Background(), candidates, e.cfg, e.desiredState, sp, e.store, "", "", e.clk, e.rec, 0, &e.log, &e.log, nil, opts...)
	if !tracker.wait(hangBudget) {
		t.Fatalf("async starts did not finish within the hang budget (%s)", hangBudget)
	}
}

func TestCommitStartResult_CapacityRestoreKeepsWaitHoldDuringStart(t *testing.T) {
	e := newCapacityEnv(t, true, "s")
	c := asleepSession(t, e, nil)
	provider := &midStartWriter{Fake: e.sp, store: e.store, id: c.info.ID, patch: map[string]string{"wait_hold": "true", "sleep_intent": "wait-hold"}}

	executePlannedStartsTraced(context.Background(), []startCandidate{c}, e.cfg, e.desiredState, provider, e.store, "", "", e.clk, e.rec, 0, &e.log, &e.log, e.trace, e.startOptions...)

	got := e.bead(t, c.info.ID)
	if got.Metadata["sleep_intent"] != "wait-hold" || got.Metadata["wait_hold"] != "true" {
		t.Fatalf("sleep_intent=%q wait_hold=%q, want the wait --sleep hold kept", got.Metadata["sleep_intent"], got.Metadata["wait_hold"])
	}
	if got.Metadata["state"] != "asleep" {
		t.Fatalf("state = %q, want the untouched pre-wake state restored", got.Metadata["state"])
	}
}

func TestCommitStartResult_CapacityRestoreKeepsWakeRequestedDuringStart(t *testing.T) {
	e := newCapacityEnv(t, true, "s")
	c := asleepSession(t, e, map[string]string{"wake_request": "manual", "wake_requested_at": "2026-03-08T11:00:00Z"})
	provider := &midStartWriter{Fake: e.sp, store: e.store, id: c.info.ID, patch: map[string]string{"wake_request": "api", "wake_requested_at": "2026-03-08T12:00:01Z"}}

	executePlannedStartsTraced(context.Background(), []startCandidate{c}, e.cfg, e.desiredState, provider, e.store, "", "", e.clk, e.rec, 0, &e.log, &e.log, e.trace, e.startOptions...)

	got := e.bead(t, c.info.ID)
	if got.Metadata["wake_request"] != "api" || got.Metadata["wake_requested_at"] != "2026-03-08T12:00:01Z" {
		t.Fatalf("wake_request=%q wake_requested_at=%q, want the wake requested during the start kept", got.Metadata["wake_request"], got.Metadata["wake_requested_at"])
	}
}

func TestCommitStartResult_CapacityRestoreSkipsNewerIncarnation(t *testing.T) {
	e := newCapacityEnv(t, true, "s")
	c := asleepSession(t, e, nil)
	// A newer incarnation took the row mid-start: new token and its own
	// lease, with the same "creating" state PreWake wrote.
	provider := &midStartWriter{Fake: e.sp, store: e.store, id: c.info.ID, patch: map[string]string{"instance_token": "tok-newer"}}

	executePlannedStartsTraced(context.Background(), []startCandidate{c}, e.cfg, e.desiredState, provider, e.store, "", "", e.clk, e.rec, 0, &e.log, &e.log, e.trace, e.startOptions...)

	got := e.bead(t, c.info.ID)
	if got.Metadata["state"] != "creating" || got.Metadata["last_woke_at"] == "" {
		t.Fatalf("state=%q last_woke_at=%q, want the newer incarnation's row and lease untouched", got.Metadata["state"], got.Metadata["last_woke_at"])
	}
}

func TestCommitStartResult_CapacityRestoresPreWakeLifecycleKeys(t *testing.T) {
	e := newCapacityEnv(t, true, "s")
	c := asleepSession(t, e, map[string]string{
		"sleep_intent":               "idle-stop",
		"detached_at":                "2026-03-08T11:30:00Z",
		"continuation_reset_pending": "true",
	})
	before := e.bead(t, c.info.ID)
	e.sp.StartErrors["s"] = capacityRefusal("s", "")

	e.start(context.Background(), c)

	got := e.bead(t, c.info.ID)
	for _, key := range []string{"state", "sleep_reason", "sleep_intent", "detached_at", "continuation_reset_pending", "pending_create_started_at", "last_woke_at"} {
		if got.Metadata[key] != before.Metadata[key] {
			t.Errorf("%s = %q after the refusal, want the pre-wake %q", key, got.Metadata[key], before.Metadata[key])
		}
	}
}

func TestEndpointCapacityGuard_StragglerSuccessWhileOpenKeepsOpen(t *testing.T) {
	e := newCapacityEnv(t, true)
	straggler, _ := e.guard.Admit(capacityTestEndpoint, "straggler", "straggler")
	e.openEndpoint(t)

	straggler.Resolve(verdictSuccess)

	if st := e.breakerStatus(capacityTestEndpoint); st.State != resilience.StateOpen {
		t.Fatalf("breaker = %v, want a success admitted before the trip to leave it open", st.State)
	}
}

func TestReapStaleSessionBeads_HonorsEndpointHold(t *testing.T) {
	for _, tc := range []struct {
		name  string
		extra map[string]string
	}{
		{"claimed", map[string]string{"pending_create_claim": "true"}},
		{"claimless", map[string]string{"pending_create_claim": ""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newCapacityEnv(t, true, "a")
			// The reaper ages rows from their store CreatedAt, which is wall time.
			e.clk.Time = time.Now().UTC()
			e.cfg.Agents[0].Upstream = "e" // the reaper finds the endpoint through config
			extra := map[string]string{"state": "creating", "pending_create_started_at": e.clk.Now().UTC().Format(time.RFC3339)}
			maps.Copy(extra, tc.extra)
			a := e.pendingCreate(t, "a", "e", extra)
			e.clk.Advance(11 * time.Minute)
			e.sp.StartErrors["a"] = capacityRefusal("a", "")
			e.start(context.Background(), e.refreshed(a))
			e.clk.Advance(2 * time.Second)
			if got := e.bead(t, a.info.ID); got.Metadata["state"] != "creating" || got.Metadata["last_woke_at"] != "" {
				t.Fatalf("premise: state=%q last_woke_at=%q, want a never-started creating row", got.Metadata["state"], got.Metadata["last_woke_at"])
			}

			if n := reapStaleSessionBeads(e.store, e.sp, e.dt, endpointHoldForRows(e.cfg, e.guard), e.clk, &e.log); n != 0 {
				t.Fatalf("reaped %d rows, want the held row kept while its endpoint refuses", n)
			}
			if got := e.bead(t, a.info.ID); got.Status != "open" {
				t.Fatalf("row status = %q, want open", got.Status)
			}
			if n := reapStaleSessionBeads(e.store, e.sp, e.dt, nil, e.clk, &e.log); n != 1 {
				t.Fatalf("premise: reaped %d rows without the hold, want the row reapable", n)
			}
		})
	}

	t.Run("a started row is still reaped", func(t *testing.T) {
		e := newCapacityEnv(t, true, "a")
		e.clk.Time = time.Now().UTC()
		e.cfg.Agents[0].Upstream = "e"
		started := e.clk.Now().UTC().Format(time.RFC3339)
		a := e.pendingCreate(t, "a", "e", map[string]string{"state": "creating", "pending_create_started_at": started, "last_woke_at": started})
		e.openEndpoint(t)
		e.clk.Advance(11 * time.Minute)
		e.reopen(t)

		if n := reapStaleSessionBeads(e.store, e.sp, e.dt, endpointHoldForRows(e.cfg, e.guard), e.clk, &e.log); n != 1 {
			t.Fatalf("reaped %d rows, want the started stale row reaped regardless of the hold", n)
		}
		if got := e.bead(t, a.info.ID); got.Status != "closed" {
			t.Fatalf("row status = %q, want closed", got.Status)
		}
	})
}

// closingRefuser closes the row while provider Start runs, then refuses.
type closingRefuser struct {
	*runtime.Fake
	store beads.Store
	id    string
}

func (p *closingRefuser) Start(ctx context.Context, name string, cfg runtime.Config) error {
	p.Fake.Start(ctx, name, cfg) //nolint:errcheck // records the call; the refusal below is the result
	p.Stop(name)                 //nolint:errcheck
	if err := p.store.Close(p.id); err != nil {
		return err
	}
	return capacityRefusal(name, "")
}

func TestCommitStartResult_CapacityRestoreSkipsClosedRow(t *testing.T) {
	e := newCapacityEnv(t, true, "s")
	c := asleepSession(t, e, nil)
	provider := &closingRefuser{Fake: e.sp, store: e.store, id: c.info.ID}

	executePlannedStartsTraced(context.Background(), []startCandidate{c}, e.cfg, e.desiredState, provider, e.store, "", "", e.clk, e.rec, 0, &e.log, &e.log, e.trace, e.startOptions...)

	got := e.bead(t, c.info.ID)
	if got.Status != "closed" || got.Metadata["state"] != "creating" || got.Metadata["last_woke_at"] == "" {
		t.Fatalf("status=%q state=%q last_woke_at=%q, want a row closed during the start left exactly as closed", got.Status, got.Metadata["state"], got.Metadata["last_woke_at"])
	}
}

// seatProvider is a serving endpoint with a fixed number of free seats: a
// start succeeds while a seat is free and is refused with exit 75 otherwise.
type seatProvider struct {
	*runtime.Fake
	mu    sync.Mutex
	seats int
}

func (p *seatProvider) free(n int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.seats += n
}

func (p *seatProvider) Start(ctx context.Context, name string, cfg runtime.Config) error {
	p.mu.Lock()
	seated := p.seats > 0
	if seated {
		p.seats--
	}
	p.mu.Unlock()
	if seated {
		return p.Fake.Start(ctx, name, cfg)
	}
	p.Fake.Start(ctx, name, cfg) //nolint:errcheck // records the call; the refusal below is the result
	p.Stop(name)                 //nolint:errcheck
	return capacityRefusal(name, "")
}

// TestExecutePlannedStarts_SaturationHerdNeverValvesHealthyRows is the
// production-path regression for the backstop: under steady saturation each
// close admits a herd that saturation refuses. Twenty never-started rows,
// each its own template on one upstream, run thirty rounds of free one seat,
// probe, start, start, with admitted rows replaced. No healthy row may be sent
// to legacy accounting (rolled back).
func TestExecutePlannedStarts_SaturationHerdNeverValvesHealthyRows(t *testing.T) {
	const rows, rounds = 20, 30
	e := newCapacityEnv(t, true)
	maxWakes := 50
	e.cfg.Daemon.MaxWakesPerTick = &maxWakes
	provider := &seatProvider{Fake: e.sp}
	created := 0
	var all []string
	newRow := func() startCandidate {
		name := fmt.Sprintf("r%03d", created)
		created++
		e.cfg.Agents = append(e.cfg.Agents, config.Agent{Name: name})
		c := e.pendingCreate(t, name, "e", nil)
		all = append(all, c.info.ID)
		return c
	}
	queue := make([]startCandidate, rows)
	for i := range queue {
		queue[i] = newRow()
	}
	pass := func() {
		for i := range queue {
			queue[i] = e.refreshed(queue[i])
		}
		e.startAsyncWith(t, provider, queue...)
	}
	for round := 0; round < rounds; round++ {
		provider.free(1)
		e.makeProbeDue()
		pass() // probe
		pass() // start
		pass() // start
		for i, c := range queue {
			if e.sp.IsRunning(c.name()) {
				queue[i] = newRow()
			}
		}
	}

	rolledBack := 0
	for _, id := range all {
		if e.bead(t, id).Status == "closed" {
			rolledBack++
		}
	}
	if rolledBack != 0 {
		t.Fatalf("%d healthy rows rolled back under saturation, want 0", rolledBack)
	}
	if created == rows {
		t.Fatal("no row ever got a seat; the scenario did not exercise the endpoint closing")
	}
}

// raceStore is a conditional-write-capable MemStore that, once armed, applies
// patch right after the next read of id, and counts successful writes and
// fence conflicts from the moment it is armed.
type raceStore struct {
	*beads.MemStore
	mu               sync.Mutex
	id               string
	patch            map[string]string
	armed, counting  bool
	writes, conflict int
}

func (s *raceStore) arm() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.armed, s.counting = true, true
}

func (s *raceStore) Get(id string) (beads.Bead, error) {
	b, err := s.MemStore.Get(id)
	s.mu.Lock()
	fire := s.armed && id == s.id && s.patch != nil
	if fire {
		s.armed = false
	}
	s.mu.Unlock()
	if fire {
		if werr := s.MemStore.SetMetadataBatch(s.id, s.patch); werr != nil {
			return b, werr
		}
	}
	return b, err
}

func (s *raceStore) countWrite(err error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case !s.counting:
	case err == nil:
		s.writes++
	case beads.IsPreconditionFailed(err):
		s.conflict++
	}
	return err
}

func (s *raceStore) Update(id string, opts beads.UpdateOpts) error {
	return s.countWrite(s.MemStore.Update(id, opts))
}

func (s *raceStore) UpdateIfMatch(id string, rev int64, opts beads.UpdateOpts) error {
	return s.countWrite(s.MemStore.UpdateIfMatch(id, rev, opts))
}

func (s *raceStore) SetMetadata(id, key, value string) error {
	return s.countWrite(s.MemStore.SetMetadata(id, key, value))
}

func (s *raceStore) SetMetadataBatch(id string, kvs map[string]string) error {
	return s.countWrite(s.MemStore.SetMetadataBatch(id, kvs))
}

// openRaceStore stamps a raceStore with conditional writes on, as the store
// factory does for production stores.
func openRaceStore(t *testing.T) *raceStore {
	t.Helper()
	raw := &raceStore{MemStore: beads.NewMemStore()}
	result, err := beads.OpenStoreAtForCity(context.Background(), beads.StoreOpenOptions{
		ScopeRoot:         t.TempDir(),
		Provider:          "file",
		ConditionalWrites: gate.Auto,
		OpenFileStore:     func() (beads.Store, error) { return raw, nil },
	})
	if err != nil {
		t.Fatalf("OpenStoreAtForCity: %v", err)
	}
	if writer, _, err := beads.ResolveConditionalWriter(beads.SessionStore{Store: result.Store}); err != nil || writer == nil {
		t.Fatalf("ResolveConditionalWriter = (%v, %v), want a conditional writer", writer, err)
	}
	if result.Store != beads.Store(raw) {
		t.Fatalf("store factory wrapped the race store (%T); the hooks would be bypassed", result.Store)
	}
	return raw
}

func TestCommitStartResult_CapacityRestoreIsOneFencedWrite(t *testing.T) {
	t.Run("suspend between the restore's read and write is kept", func(t *testing.T) {
		e := newCapacityEnv(t, true, "s")
		store := openRaceStore(t)
		e.store = store
		c := asleepSession(t, e, nil)
		store.id, store.patch = c.info.ID, suspendPatch(e.clk.Now())
		provider := &armingRefuser{Fake: e.sp, arm: store.arm}

		executePlannedStartsTraced(context.Background(), []startCandidate{c}, e.cfg, e.desiredState, provider, e.store, "", "", e.clk, e.rec, 0, &e.log, &e.log, e.trace, e.startOptions...)

		if store.conflict == 0 {
			t.Fatal("the suspend did not land between the restore's read and write; the race was not exercised")
		}
		got := e.bead(t, c.info.ID)
		requireSuspendKept(t, got)
		// Re-decided from the re-read, the restore still lands for the keys
		// the suspend left alone.
		if got.Metadata["last_woke_at"] != "" || got.Metadata["sleep_reason"] != "idle" {
			t.Fatalf("last_woke_at=%q sleep_reason=%q, want the restore re-decided and applied after the conflict", got.Metadata["last_woke_at"], got.Metadata["sleep_reason"])
		}
	})

	t.Run("the restore is a single write", func(t *testing.T) {
		e := newCapacityEnv(t, true, "s")
		store := openRaceStore(t)
		e.store = store
		c := asleepSession(t, e, nil)
		store.id = c.info.ID
		provider := &armingRefuser{Fake: e.sp, arm: store.arm}

		executePlannedStartsTraced(context.Background(), []startCandidate{c}, e.cfg, e.desiredState, provider, e.store, "", "", e.clk, e.rec, 0, &e.log, &e.log, e.trace, e.startOptions...)

		if store.writes != 1 {
			t.Fatalf("writes after the refusal = %d, want the restore as exactly one write", store.writes)
		}
		if got := e.bead(t, c.info.ID); got.Metadata["state"] != "asleep" || got.Metadata["last_woke_at"] != "" {
			t.Fatalf("state=%q last_woke_at=%q, want the restore applied", got.Metadata["state"], got.Metadata["last_woke_at"])
		}
	})
}

func TestCommitStartResult_CapacityRestoreKeepsContinuationEpoch(t *testing.T) {
	e := newCapacityEnv(t, true, "s")
	// A pending continuation reset makes PreWake bump the epoch.
	c := asleepSession(t, e, map[string]string{"continuation_epoch": "3", "continuation_reset_pending": "true"})
	e.sp.StartErrors["s"] = capacityRefusal("s", "")

	e.start(context.Background(), c)

	if got := e.bead(t, c.info.ID); got.Metadata["continuation_epoch"] != "3" {
		t.Fatalf("continuation_epoch = %q after the refusal, want 3 so queued nudges stay valid", got.Metadata["continuation_epoch"])
	}
}

func TestEndpointCapacity_ForgetsEndpointsNoSessionUses(t *testing.T) {
	e := newCapacityEnv(t, true)
	e.openEndpoint(t)
	e.clk.Advance(capacityEndpointForgetAfter - time.Second)
	e.guard.recordTick(e.trace, e.rec, &e.log)
	if len(e.guard.Snapshot()) != 1 {
		t.Fatal("endpoint forgotten before capacityEndpointForgetAfter")
	}

	e.clk.Advance(2 * time.Second)
	e.guard.recordTick(e.trace, e.rec, &e.log)
	before := len(e.records(TraceSiteEndpointCapacityBreaker))
	e.guard.recordTick(e.trace, e.rec, &e.log)

	if snap := e.guard.Snapshot(); len(snap) != 0 {
		t.Fatalf("Snapshot() = %+v, want the orphaned endpoint forgotten", snap)
	}
	if after := len(e.records(TraceSiteEndpointCapacityBreaker)); after != before {
		t.Fatalf("endpoint records grew from %d to %d after the endpoint was forgotten", before, after)
	}
	if e.guard.HoldsPendingCreate(capacityTestEndpoint) {
		t.Fatal("HoldsPendingCreate = true for a forgotten endpoint, want a fresh closed one")
	}
}

func TestEndpointCapacity_KeepsEndpointsSessionsStillUse(t *testing.T) {
	for _, tc := range []struct {
		name   string
		lookup func(*endpointCapacityGuard)
	}{
		{"wake gate", func(g *endpointCapacityGuard) { g.Eligible(capacityTestEndpoint) }},
		{"hold check", func(g *endpointCapacityGuard) { g.HoldsPendingCreate(capacityTestEndpoint) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newCapacityEnv(t, true)
			e.openEndpoint(t)
			e.clk.Advance(capacityEndpointForgetAfter - time.Minute)
			tc.lookup(e.guard)
			e.clk.Advance(2 * time.Minute)

			e.guard.recordTick(e.trace, e.rec, &e.log)

			if len(e.guard.Snapshot()) != 1 {
				t.Fatalf("endpoint forgotten although a session looked it up %s ago", time.Minute)
			}
		})
	}
}
