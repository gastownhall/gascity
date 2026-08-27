package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/rollout"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// newExactSleepDrainParams builds the handler's params for one seeded row. The
// row is DESIRED on purpose: an undesired row belongs to D-ORPHAN, which the
// seam switch reaches first.
func newExactSleepDrainParams(env *reconcilerTestEnv, provider runtime.Provider, name string) exactSessionStartParams {
	statusWriter, _, statusWriterErr := beads.ResolveConditionalWriter(env.store)
	return exactSessionStartParams{
		Generation: 1, CityPath: "test-city", CityName: "test-city",
		Config: env.cfg, Provider: provider, Store: env.store,
		StatusWriter: statusWriter, StatusWriterError: statusWriterErr,
		Recorder: events.Discard, RolloutMode: rollout.Require,
		Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{},
		DrainTracker:        env.dt,
		DesiredSessionNames: func() map[string]bool { return map[string]bool{name: true} },
	}
}

// seedIdleSuppressedSession seeds the fixture the whole family turns on: a live,
// desired, detached interactive session whose workspace sleep window has
// elapsed, so the sleep-policy pass suppresses its wake and the awake set stops
// asking for it. It mirrors the anchor
// TestReconcileSessionBeads_PreservedRunningNamedSessionStillIdleDrains.
func seedIdleSuppressedSession(t *testing.T, env *reconcilerTestEnv) (*deadRuntimeProvider, beads.Bead) {
	const name = "worker"
	t.Helper()
	env.cfg = &config.City{
		SessionSleep: config.SessionSleepConfig{InteractiveResume: "60s"},
		Workspace:    config.Workspace{Name: "test-city"},
		Agents:       []config.Agent{{Name: name, StartCommand: "true"}},
	}
	provider := &deadRuntimeProvider{Fake: env.sp}
	if err := provider.Start(t.Context(), name, runtime.Config{}); err != nil {
		t.Fatalf("start runtime for %q: %v", name, err)
	}
	bead := env.createSessionBead(name, name)
	env.markSessionActive(&bead)
	env.setSessionMetadata(&bead, map[string]string{
		"detached_at": env.clk.Now().UTC().Add(-6 * time.Minute).Format(time.RFC3339),
	})
	env.sp.WaitForIdleErrors[name] = nil
	return provider, bead
}

// TestExactSleepNoWakeSessionDrainsOnceByKey is WD.5's primary RED: an alive
// session the awake set no longer wants is drained exactly once by exact key,
// with markIdleSleepPending recorded BEFORE the drain begins and the drain
// library's enqueue-only begin semantics preserved (the interrupt stays with
// the advance loop, so the one-tick rescue window survives).
func TestExactSleepNoWakeSessionDrainsOnceByKey(t *testing.T) {
	env := newReconcilerTestEnv()
	provider, bead := seedIdleSuppressedSession(t, env)

	info, response, err := getAuthoritativeSessionStartPersistedRecord(env.store, bead.ID)
	if err != nil {
		t.Fatalf("authoritative read: %v", err)
	}
	params := newExactSleepDrainParams(env, provider, "worker")

	// Leg 1 — no probe yet, so the handler launches one and defers with zero
	// effect. The probe launch is the handler's; the budget is the detector's.
	handled, owner, err := reconcileExactSessionDetectorFamily(
		t.Context(), sleepDrainAdmission(bead.ID), params, info, response, env.clk)
	if !handled {
		t.Fatal("the D-SLEEP seam did not claim a live no-wake row")
	}
	if err != nil || owner != exactSessionStartKeyedOwner {
		t.Fatalf("probe leg returned owner=%v err=%v, want keyed ownership and no error", owner, err)
	}
	if env.dt.get(bead.ID) != nil {
		t.Fatal("the handler drained before the idle probe confirmed the session was idle")
	}
	waitForIdleProbeReady(t, env.dt, bead.ID)

	// Leg 2 — the probe came back idle, so the same key marks the pending sleep
	// intent and begins the drain.
	handled, owner, err = reconcileExactSessionDetectorFamily(
		t.Context(), sleepDrainAdmission(bead.ID), params, info, response, env.clk)
	if !handled || err != nil || owner != exactSessionStartKeyedOwner {
		t.Fatalf("drain leg: handled=%v owner=%v err=%v", handled, owner, err)
	}
	state := env.dt.get(bead.ID)
	if state == nil {
		t.Fatal("no drain intent recorded in the in-memory tracker; Q4 keeps drain intent there")
	}
	if state.reason != "idle" {
		t.Fatalf("drain reason = %q, want %q", state.reason, "idle")
	}
	stored, err := env.store.Get(bead.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Metadata["sleep_intent"] != "idle-stop-pending" {
		t.Fatalf("sleep_intent = %q, want idle-stop-pending recorded before the drain began", stored.Metadata["sleep_intent"])
	}
	if stored.Status != "open" {
		t.Fatalf("row status = %q, want open; the drain begin is enqueue-only", stored.Status)
	}
	if !provider.IsRunning("worker") {
		t.Fatal("the drain begin stopped the runtime; the interrupt belongs to the advance loop")
	}
	for _, call := range env.sp.SnapshotCalls() {
		if call.Method == "Interrupt" || call.Method == "Stop" {
			t.Fatalf("drain begin issued %q on the provider; begin is enqueue-only", call.Method)
		}
	}

	// Exactly once: a re-admitted draining row records no second intent.
	firstStartedAt := state.startedAt
	env.clk.Time = env.clk.Time.Add(time.Minute)
	if _, _, err := reconcileExactSessionDetectorFamily(
		t.Context(), sleepDrainAdmission(bead.ID), params, info, response, env.clk); err != nil {
		t.Fatalf("re-admitting a draining row: %v", err)
	}
	again := env.dt.get(bead.ID)
	if again == nil || !again.startedAt.Equal(firstStartedAt) || again.reason != "idle" {
		t.Fatalf("the re-admission restarted the drain: %#v", again)
	}
}

func sleepDrainAdmission(id string) sessionStartAdmission {
	return sessionStartAdmission{SessionID: id, Source: sessionStartAdmissionSleepDrain}
}

// sleepSweepInput builds the minimum sweep input that reaches D-SLEEP for the
// given rows. Every row is DESIRED: family precedence routes an undesired row to
// D-ORPHAN and evaluates it no further.
func sleepSweepInput(
	env *reconcilerTestEnv,
	provider runtime.Provider,
	infos []sessionpkg.Info,
	now time.Time,
	admit func(string, sessionStartAdmissionSource) (sessionStartAdmissionOutcome, error),
) detectorSweepInput {
	rows := make([]sessionpkg.ReconcileSession, 0, len(infos))
	desired := make(map[string]TemplateParams, len(infos))
	for _, info := range infos {
		rows = append(rows, sessionpkg.ReconcileSession{Info: info})
		name := info.SessionNameMetadata
		desired[name] = TemplateParams{SessionName: name, TemplateName: info.Template}
	}
	return detectorSweepInput{
		CityPath:   "test-city",
		CityName:   "test-city",
		Cfg:        env.cfg,
		Provider:   provider,
		Rows:       rows,
		Desired:    desired,
		Drains:     env.dt,
		IdleProbes: newDetectorIdleProbeCursor(),
		Clock:      &clock.Fake{Time: now},
		Trigger:    "patrol",
		Admit:      admit,
	}
}

func sleepConditionsFor(result detectorSweepResult, id string) []detectorCondition {
	var out []detectorCondition
	for _, cond := range result.Conditions {
		if cond.Family == detectorFamilySleep && cond.SessionID == id {
			out = append(out, cond)
		}
	}
	return out
}

// TestDetectorSleepIdleProbeBudgetIsRoundRobinAcrossSweeps is the third AC
// negative: the per-tick idle-probe budget still bounds how many probes may be
// launched, the round-robin cursor means no session is starved across sweeps,
// and no session is granted a slot twice in one cycle. The budget is the one
// half of the probe that stays detector-side — it is a fleet-shaped rate limit,
// and the launch itself is the handler's.
func TestDetectorSleepIdleProbeBudgetIsRoundRobinAcrossSweeps(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		SessionSleep: config.SessionSleepConfig{InteractiveResume: "60s"},
		Workspace:    config.Workspace{Name: "test-city"},
	}
	provider := &deadRuntimeProvider{Fake: env.sp}
	names := []string{"w1", "w2", "w3", "w4", "w5"}
	var infos []sessionpkg.Info
	for _, name := range names {
		env.cfg.Agents = append(env.cfg.Agents, config.Agent{Name: name, StartCommand: "true"})
		if err := provider.Start(t.Context(), name, runtime.Config{}); err != nil {
			t.Fatalf("start runtime for %q: %v", name, err)
		}
		bead := env.createSessionBead(name, name)
		env.markSessionActive(&bead)
		env.setSessionMetadata(&bead, map[string]string{
			"detached_at": env.clk.Now().UTC().Add(-6 * time.Minute).Format(time.RFC3339),
		})
		infos = append(infos, env.sessionInfo(bead.ID))
	}

	cursor := newDetectorIdleProbeCursor()
	sweep := func() []string {
		admitter := &recordingDetectorAdmitter{}
		in := sleepSweepInput(env, provider, infos, env.clk.Now().UTC(), admitter.admit)
		in.IdleProbes = cursor
		result := detectSessionConditions(context.Background(), in)
		routeDetectorConditions(in, &result)
		for _, cond := range result.Conditions {
			if cond.Family != detectorFamilySleep || cond.AdmissionSource == "" {
				continue
			}
			if cond.Fields["predicted_effect"] != "idle_probe" {
				t.Fatalf("routed sleep condition %#v is not a probe slot", cond)
			}
		}
		return admitter.keys
	}

	first := sweep()
	if len(first) != maxIdleSleepProbesPerTick {
		t.Fatalf("first sweep granted %d probe slots (%v), want the per-tick ceiling %d", len(first), first, maxIdleSleepProbesPerTick)
	}
	seen := map[string]int{}
	for _, key := range first {
		seen[key]++
		if seen[key] > 1 {
			t.Fatalf("session %q was granted two probe slots in one cycle: %v", key, first)
		}
	}
	second := sweep()
	if len(second) != maxIdleSleepProbesPerTick {
		t.Fatalf("second sweep granted %d probe slots (%v), want %d", len(second), second, maxIdleSleepProbesPerTick)
	}
	for _, key := range second {
		seen[key]++
	}
	if len(seen) != len(names) {
		t.Fatalf("round robin starved a session: after two sweeps only %d of %d were granted a slot (%v)", len(seen), len(names), seen)
	}

	// The ceiling counts probes ALREADY in flight, so a fleet mid-probe grants
	// fewer new slots rather than stacking a second wave on top.
	if probe := env.dt.startIdleProbe(infos[0].ID); probe == nil {
		t.Fatal("seeding an in-flight probe")
	}
	if got := len(sweep()); got != maxIdleSleepProbesPerTick-1 {
		t.Fatalf("sweep granted %d slots with one probe in flight, want %d", got, maxIdleSleepProbesPerTick-1)
	}
}

// TestDetectorSleepUserHoldKeepAliveNeverEnqueues is the first AC negative: a
// live session held only by a future held_until with no sleep_intent is running
// `gc runtime heartbeat` through a long, silent operation (#3994). It is never
// enqueued and never drains — the escape becomes a detection-side non-enqueue
// rather than a mid-pass branch — and the handler refuses it too, so no other
// admission can carry it into a drain.
func TestDetectorSleepUserHoldKeepAliveNeverEnqueues(t *testing.T) {
	env := newReconcilerTestEnv()
	provider, bead := seedIdleSuppressedSession(t, env)
	env.setSessionMetadata(&bead, map[string]string{
		"held_until": env.clk.Now().UTC().Add(30 * time.Minute).Format(time.RFC3339),
	})

	admitter := &recordingDetectorAdmitter{}
	in := sleepSweepInput(env, provider, []sessionpkg.Info{env.sessionInfo(bead.ID)}, env.clk.Now().UTC(), admitter.admit)
	result := detectSessionConditions(context.Background(), in)
	routeDetectorConditions(in, &result)

	if len(admitter.keys) != 0 {
		t.Fatalf("a heartbeat keep-alive session enqueued %v; want zero enqueues", admitter.keys)
	}
	conds := sleepConditionsFor(result, bead.ID)
	if len(conds) != 1 || conds[0].Reason != detectorReasonSleepKeepAlive {
		t.Fatalf("keep-alive row raised %#v, want one %q record", conds, detectorReasonSleepKeepAlive)
	}
	if conds[0].Outcome == TraceOutcomeDrain {
		t.Fatal("the keep-alive arm predicted a drain; it must predict no effect so it can never route")
	}

	info, response, err := getAuthoritativeSessionStartPersistedRecord(env.store, bead.ID)
	if err != nil {
		t.Fatalf("authoritative read: %v", err)
	}
	params := newExactSleepDrainParams(env, provider, "worker")
	handled, _, err := reconcileExactSessionDetectorFamily(
		t.Context(), sleepDrainAdmission(bead.ID), params, info, response, env.clk)
	if handled || err != nil {
		t.Fatalf("the seam claimed a heartbeat keep-alive row: handled=%v err=%v", handled, err)
	}
	if env.dt.get(bead.ID) != nil {
		t.Fatal("a heartbeat keep-alive session was drained")
	}
}

// TestDetectorSleepWakeEligibleRowsNeverEnqueue is the second AC negative, in
// both of its halves. A session the awake set still wants raises no sleep
// condition at all; and a session whose sleep window HAS elapsed but whose wake
// suppression is overridden by demand — the ConfigSuppressed pass's own last
// rung — is likewise never enqueued and never drained.
func TestDetectorSleepWakeEligibleRowsNeverEnqueue(t *testing.T) {
	env := newReconcilerTestEnv()
	provider, bead := seedIdleSuppressedSession(t, env)
	work, err := env.store.Create(beads.Bead{Title: "work", Status: "in_progress", Assignee: bead.ID})
	if err != nil {
		t.Fatalf("seeding assigned work: %v", err)
	}
	assigned := []beads.Bead{work}

	// Half one: the row is in the awake set (it holds assigned work) and its
	// sleep window has NOT elapsed.
	env.setSessionMetadata(&bead, map[string]string{
		"detached_at": env.clk.Now().UTC().Add(-5 * time.Second).Format(time.RFC3339),
	})
	admitter := &recordingDetectorAdmitter{}
	in := sleepSweepInput(env, provider, []sessionpkg.Info{env.sessionInfo(bead.ID)}, env.clk.Now().UTC(), admitter.admit)
	in.AssignedWorkBeads = assigned
	in.ReadyAssignedFlags = []bool{true}
	result := detectSessionConditions(context.Background(), in)
	routeDetectorConditions(in, &result)
	if conds := sleepConditionsFor(result, bead.ID); len(conds) != 0 {
		t.Fatalf("a wake-eligible row raised %#v; want no sleep condition", conds)
	}
	if len(admitter.keys) != 0 {
		t.Fatalf("a wake-eligible row enqueued %v; want zero enqueues", admitter.keys)
	}

	// Half two: the window HAS elapsed, so the sleep-policy pass suppresses —
	// but assigned work overrides that suppression for every sleep class, which
	// is the pass's own last rung.
	env.setSessionMetadata(&bead, map[string]string{
		"detached_at": env.clk.Now().UTC().Add(-6 * time.Minute).Format(time.RFC3339),
	})
	admitter = &recordingDetectorAdmitter{}
	in = sleepSweepInput(env, provider, []sessionpkg.Info{env.sessionInfo(bead.ID)}, env.clk.Now().UTC(), admitter.admit)
	in.AssignedWorkBeads = assigned
	in.ReadyAssignedFlags = []bool{true}
	result = detectSessionConditions(context.Background(), in)
	routeDetectorConditions(in, &result)
	if conds := sleepConditionsFor(result, bead.ID); len(conds) != 0 {
		t.Fatalf("a demand-overridden row raised %#v; want no sleep condition", conds)
	}
	if len(admitter.keys) != 0 {
		t.Fatalf("a demand-overridden row enqueued %v; want zero enqueues", admitter.keys)
	}

	// And the handler refuses it too, on its own authority: the live
	// reachable-store re-query is the one wake rung it re-pays per key.
	info, response, err := getAuthoritativeSessionStartPersistedRecord(env.store, bead.ID)
	if err != nil {
		t.Fatalf("authoritative read: %v", err)
	}
	params := newExactSleepDrainParams(env, provider, "worker")
	if _, _, err := reconcileExactSessionDetectorFamily(
		t.Context(), sleepDrainAdmission(bead.ID), params, info, response, env.clk); err != nil {
		t.Fatalf("assigned-work deferral: %v", err)
	}
	if env.dt.get(bead.ID) != nil {
		t.Fatal("a session still holding awake assigned work was drained")
	}
}

// TestExactSleepDrainDefersAttachedSession is the A6 negative. Attached-user
// safety is a KEEP invariant of the whole redesign (DESIGN.md §2), and the
// D-SLEEP drain arm can reach an attached session because the sweep feeds
// ComputeAwakeSet an EMPTY attachment map by design — attachment is provider I/O
// and stays handler-side. So the rung has to be here, exactly as WD.4 put it on
// the orphan drain: zero effect, level-triggered, and it proceeds once the human
// detaches.
func TestExactSleepDrainDefersAttachedSession(t *testing.T) {
	env := newReconcilerTestEnv()
	provider, bead := seedIdleSuppressedSession(t, env)
	env.sp.SetAttached("worker", true)

	info, response, err := getAuthoritativeSessionStartPersistedRecord(env.store, bead.ID)
	if err != nil {
		t.Fatalf("authoritative read: %v", err)
	}
	before, err := env.store.Get(bead.ID)
	if err != nil {
		t.Fatal(err)
	}
	params := newExactSleepDrainParams(env, provider, "worker")

	handled, owner, err := reconcileExactSessionDetectorFamily(
		t.Context(), sleepDrainAdmission(bead.ID), params, info, response, env.clk)
	if !handled {
		t.Fatal("the seam did not claim a live no-wake row")
	}
	if err != nil || owner != exactSessionStartKeyedOwner {
		t.Fatalf("attached deferral returned owner=%v err=%v, want a quiet keyed refusal", owner, err)
	}
	if env.dt.get(bead.ID) != nil {
		t.Fatal("an attached session was drained; A6 forbids interrupting an attached human")
	}
	if _, probing := env.dt.idleProbe(bead.ID); probing {
		t.Fatal("an attached session was probed; the deferral is above the probe rung")
	}
	after, err := env.store.Get(bead.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != before.Status || len(after.Metadata) != len(before.Metadata) {
		t.Fatalf("the attached deferral wrote to the durable row: before=%#v after=%#v", before.Metadata, after.Metadata)
	}

	// Level-triggered: once the human detaches the drain proceeds — one leg to
	// launch the probe, one to consume it.
	env.sp.SetAttached("worker", false)
	if _, _, err := reconcileExactSessionDetectorFamily(
		t.Context(), sleepDrainAdmission(bead.ID), params, info, response, env.clk); err != nil {
		t.Fatalf("probe leg after detach: %v", err)
	}
	waitForIdleProbeReady(t, env.dt, bead.ID)
	if _, _, err := reconcileExactSessionDetectorFamily(
		t.Context(), sleepDrainAdmission(bead.ID), params, info, response, env.clk); err != nil {
		t.Fatalf("drain leg after detach: %v", err)
	}
	if env.dt.get(bead.ID) == nil {
		t.Fatal("the drain did not resume after the human detached")
	}
}

// TestDetectorSleepD2IncapableProviderRefuses proves the destructive-family
// screen covers D-SLEEP: a provider that cannot prove fresh liveness and an
// unattended stop yields a traced refusal with no enqueue, refused every sweep
// rather than re-enqueued on a 30-second treadmill.
func TestDetectorSleepD2IncapableProviderRefuses(t *testing.T) {
	env := newReconcilerTestEnv()
	_, bead := seedIdleSuppressedSession(t, env)

	admitter := &recordingDetectorAdmitter{}
	// env.sp is the bare fake: neither FreshLivenessObserver nor
	// UnattendedSessionStopper — exactly the D2-incapable shape.
	in := sleepSweepInput(env, env.sp, []sessionpkg.Info{env.sessionInfo(bead.ID)}, env.clk.Now().UTC(), admitter.admit)
	result := detectSessionConditions(context.Background(), in)
	routeDetectorConditions(in, &result)

	if len(admitter.keys) != 0 {
		t.Fatalf("a D2-incapable provider enqueued %v; want zero enqueues", admitter.keys)
	}
	refused := false
	for _, cond := range sleepConditionsFor(result, bead.ID) {
		if cond.Outcome != TraceOutcomeDrain {
			continue
		}
		if cond.AdmissionOutcome != detectorAdmissionRefusedProviderIncapable {
			t.Fatalf("sleep condition admission outcome = %q, want %q", cond.AdmissionOutcome, detectorAdmissionRefusedProviderIncapable)
		}
		refused = true
	}
	if !refused {
		t.Fatalf("no traced sleep refusal for a D2-incapable provider; conditions=%#v", result.Conditions)
	}
	if env.dt.get(bead.ID) != nil {
		t.Fatal("a D2-incapable provider recorded drain intent")
	}
}

// TestDetectorSleepFleetOnlyNoWakeRecordsAndNeverEnqueues pins this slice's
// narrowing. Legacy's last reason rung — plain "no-wake-reason" — is a FLEET
// verdict no per-key predicate can re-derive, and the seam's rule is that the
// handler answers from the row rather than from the detector's reason. Such rows
// therefore record for the parity join and stay legacy's this wave.
func TestDetectorSleepFleetOnlyNoWakeRecordsAndNeverEnqueues(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents:    []config.Agent{{Name: "worker", StartCommand: "true", MinActiveSessions: intPtr(0)}},
	}
	provider := &deadRuntimeProvider{Fake: env.sp}
	if err := provider.Start(t.Context(), "worker", runtime.Config{}); err != nil {
		t.Fatalf("start runtime: %v", err)
	}
	bead := env.createSessionBead("worker", "worker")
	env.markSessionActive(&bead)

	admitter := &recordingDetectorAdmitter{}
	in := sleepSweepInput(env, provider, []sessionpkg.Info{env.sessionInfo(bead.ID)}, env.clk.Now().UTC(), admitter.admit)
	result := detectSessionConditions(context.Background(), in)
	routeDetectorConditions(in, &result)

	if len(admitter.keys) != 0 {
		t.Fatalf("a fleet-only no-wake row enqueued %v; want zero enqueues", admitter.keys)
	}
	conds := sleepConditionsFor(result, bead.ID)
	if len(conds) != 1 || conds[0].Reason != detectorReasonNoWakeFleetOnly {
		t.Fatalf("fleet-only row raised %#v, want one %q record", conds, detectorReasonNoWakeFleetOnly)
	}
	if conds[0].Outcome == TraceOutcomeDrain {
		t.Fatal("the fleet-only arm predicted a drain; only a re-derivable reason may route")
	}
}

// TestLegacySleepDrainArmYieldsToKeyedOwnedRow is the coexistence-doctrine RED
// for legacy's awake-scan no-wake arm. Both writers record drain intent in the
// same in-memory tracker and consume the same idle probe on the same tick, so an
// acting D-SLEEP beside a non-yielding legacy double-begins by construction —
// or, worse, legacy consumes the very confirmation the keyed handler waits on.
func TestLegacySleepDrainArmYieldsToKeyedOwnedRow(t *testing.T) {
	env := newReconcilerTestEnv()
	_, session := seedLegacyIdleDrainSession(t, env)

	cfgNames := configuredSessionNames(env.cfg, "", env.store)
	reconcileSessionBeads(
		context.Background(), []beads.Bead{session}, env.desiredState, cfgNames,
		env.cfg, env.sp, env.store, nil, nil, nil, env.dt, map[string]int{}, false, nil, "",
		nil, env.clk, env.rec, 0, 0, &env.stdout, &env.stderr,
		withLegacySleepDrainExclusion(func(info sessionpkg.Info) bool { return info.ID == session.ID }),
	)

	if state := env.dt.get(session.ID); state != nil {
		t.Fatalf("legacy no-wake arm began a drain the keyed handler owns: %#v", state)
	}
	stored, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Metadata["sleep_intent"] == "idle-stop-pending" {
		t.Fatal("legacy marked idle-stop-pending on a row the keyed handler owns")
	}
	if probe, ok := env.dt.idleProbe(session.ID); !ok || !probe.ready {
		t.Fatal("legacy consumed the idle probe the keyed handler is waiting on")
	}
}

// TestLegacySleepDrainArmStillDrainsUnownedRows is the other half of the yield:
// the exclusion is per-key, so a row the keyed controller does NOT own still
// drains on the legacy arm. A yield that stood down fleet-wide would silently
// disable idle sleep everywhere.
func TestLegacySleepDrainArmStillDrainsUnownedRows(t *testing.T) {
	env := newReconcilerTestEnv()
	_, session := seedLegacyIdleDrainSession(t, env)

	cfgNames := configuredSessionNames(env.cfg, "", env.store)
	reconcileSessionBeads(
		context.Background(), []beads.Bead{session}, env.desiredState, cfgNames,
		env.cfg, env.sp, env.store, nil, nil, nil, env.dt, map[string]int{}, false, nil, "",
		nil, env.clk, env.rec, 0, 0, &env.stdout, &env.stderr,
		withLegacySleepDrainExclusion(func(sessionpkg.Info) bool { return false }),
	)

	state := env.dt.get(session.ID)
	if state == nil {
		t.Fatal("legacy no-wake arm stood down for a row no keyed handler owns")
	}
	if state.reason != "idle" {
		t.Fatalf("legacy drain reason = %q, want idle", state.reason)
	}
}

// seedLegacyIdleDrainSession seeds the ANCHOR's fixture —
// TestReconcileSessionBeads_PreservedRunningNamedSessionStillIdleDrains
// (session_reconciler_test.go) — and runs one legacy tick, so the row arrives
// with its idle probe already ready and the next tick's legacy arm would begin
// the drain. It is an on_demand named session on purpose: that is what gives the
// row a wake reason for ComputeAwakeSet to withdraw on its own idle window,
// which is the exact shape the acting keyed family has to be able to take over.
func seedLegacyIdleDrainSession(t *testing.T, env *reconcilerTestEnv) (string, beads.Bead) {
	t.Helper()
	env.cfg = &config.City{
		SessionSleep:  config.SessionSleepConfig{InteractiveResume: "60s"},
		Workspace:     config.Workspace{Name: "test-city"},
		Agents:        []config.Agent{{Name: "worker", StartCommand: "true", MaxActiveSessions: intPtr(2)}},
		NamedSessions: []config.NamedSession{{Template: "worker", Mode: "on_demand"}},
	}
	name := config.NamedSessionRuntimeName(env.cfg.Workspace.Name, env.cfg.Workspace, "worker")
	env.addDesired(name, "worker", true)
	session := env.createSessionBead(name, "worker")
	env.markSessionActive(&session)
	env.setSessionMetadata(&session, map[string]string{
		namedSessionMetadataKey:      "true",
		namedSessionIdentityMetadata: "worker",
		namedSessionModeMetadata:     "on_demand",
		"detached_at":                env.clk.Now().UTC().Add(-6 * time.Minute).Format(time.RFC3339),
	})
	env.sp.WaitForIdleErrors[name] = nil
	idleGate := make(chan struct{}) // see waitForIdleProbeReady godoc
	env.sp.WaitForIdleGates[name] = idleGate

	cfgNames := configuredSessionNames(env.cfg, "", env.store)
	reconcileSessionBeads(
		context.Background(), []beads.Bead{session}, env.desiredState, cfgNames,
		env.cfg, env.sp, env.store, nil, nil, nil, env.dt, map[string]int{}, false, nil, "",
		nil, env.clk, env.rec, 0, 0, &env.stdout, &env.stderr,
	)
	close(idleGate)
	waitForIdleProbeReady(t, env.dt, session.ID)
	return name, session
}

// TestExactSleepAnchorPreservedRunningNamedSessionDrainsKeyed is the AC's
// re-pointed anchor. The very fixture legacy's
// TestReconcileSessionBeads_PreservedRunningNamedSessionStillIdleDrains drains
// is drained by the KEYED handler instead, on the same canonical bead, with the
// pending intent recorded first — and the detector routes it under the family's
// own admission source rather than leaving it to the fleet loop.
func TestExactSleepAnchorPreservedRunningNamedSessionDrainsKeyed(t *testing.T) {
	env := newReconcilerTestEnv()
	name, session := seedLegacyIdleDrainSession(t, env)
	env.dt.clearIdleProbe(session.ID)

	provider := &deadRuntimeProvider{Fake: env.sp}
	admitter := &recordingDetectorAdmitter{}
	in := sleepSweepInput(env, provider, []sessionpkg.Info{env.sessionInfo(session.ID)}, env.clk.Now().UTC(), admitter.admit)
	result := detectSessionConditions(context.Background(), in)
	routeDetectorConditions(in, &result)
	if len(admitter.keys) != 1 || admitter.keys[0] != session.ID {
		t.Fatalf("the anchor row enqueued %v, want exactly its own key", admitter.keys)
	}
	if admitter.sources[0] != sessionStartAdmissionSleepDrain {
		t.Fatalf("the anchor row was admitted under %q, want %q", admitter.sources[0], sessionStartAdmissionSleepDrain)
	}

	info, response, err := getAuthoritativeSessionStartPersistedRecord(env.store, session.ID)
	if err != nil {
		t.Fatalf("authoritative read: %v", err)
	}
	params := newExactSleepDrainParams(env, provider, name)
	if _, _, err := reconcileExactSessionDetectorFamily(
		t.Context(), sleepDrainAdmission(session.ID), params, info, response, env.clk); err != nil {
		t.Fatalf("probe leg: %v", err)
	}
	waitForIdleProbeReady(t, env.dt, session.ID)
	if _, _, err := reconcileExactSessionDetectorFamily(
		t.Context(), sleepDrainAdmission(session.ID), params, info, response, env.clk); err != nil {
		t.Fatalf("drain leg: %v", err)
	}

	state := env.dt.get(session.ID)
	if state == nil {
		t.Fatal("expected a keyed idle drain for the preserved running named session")
	}
	if state.reason != "idle" {
		t.Fatalf("drain reason = %q, want idle", state.reason)
	}
	stored, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != "open" {
		t.Fatalf("status = %q, want open", stored.Status)
	}
	if stored.Metadata["sleep_intent"] != "idle-stop-pending" {
		t.Fatalf("sleep_intent = %q, want idle-stop-pending", stored.Metadata["sleep_intent"])
	}
}

// TestExactSleepDrainAdmissionIsOwnedAndYielded closes the ownership loop and
// states this slice's ownership-semantics decision: D-SLEEP takes its OWN
// SIBLING predicate rather than widening ownsOrphanDrain. ownsOrphanDrain
// answers "is a D-ORPHAN drain in flight for this key", which is false for every
// sleep admission; one predicate serving both drain families would make legacy's
// orphan drain stand down for sleep-owned rows and legacy's idle sleep stand
// down for orphan-owned ones — the silent-disable trap WD.2 recorded when it
// declined to reuse sessionStartLegacyExclusionPredicate.
func TestExactSleepDrainAdmissionIsOwnedAndYielded(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	provider := &deadRuntimeProvider{Fake: env.sp}
	bead := env.createSessionBead("worker", "worker")

	cs := coherentSessionStartControllerStateForTest(env.cfg, provider, env.store, rollout.Require)
	cr := &CityRuntime{
		cityPath: "test-city", cityName: "test-city", cfg: env.cfg, sp: provider, cs: cs,
		rec: events.Discard, stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{},
	}
	if err := cr.ensureSessionStartController(t.Context(), newSessionBeadSnapshotFromInfos(nil)); err != nil {
		t.Fatalf("ensure session-start controller: %v", err)
	}
	t.Cleanup(cr.stopSessionStartController)

	controller := cr.sessionStartController
	if _, err := controller.Admit(bead.ID, sessionStartAdmissionSleepDrain); err != nil {
		t.Fatalf("admitting sleep-drain key: %v", err)
	}
	if controller.ownsOrphanDrain(bead.ID) {
		t.Fatal("ownsOrphanDrain() answered true for a sleep-drain admission; legacy's orphan drain would be silently disabled")
	}
	if controller.ownsOrphanClose(bead.ID) {
		t.Fatal("ownsOrphanClose() answered true for a sleep-drain admission; legacy's close arm would be silently disabled")
	}
	if controller.ownsDeadlineStop(bead.ID) {
		t.Fatal("ownsDeadlineStop() answered true for a sleep-drain admission; legacy's idle kill would be silently disabled")
	}
	if controller.ownsDuplicateNamedRetire(bead.ID) {
		t.Fatal("ownsDuplicateNamedRetire() answered true for a sleep-drain admission")
	}
	awaitCond(t, func() bool { return !controller.ownsSleepDrain(bead.ID) }, "sleep-drain admission drain")
}

// aliveIncompleteObservationProvider reproduces the wedged-fleet shape from
// the mc-enterprise6i soak (ga-i20db follow-up): the target session's pane is
// ALIVE — which is exactly why it is a sleep-drain candidate — so the
// tmux-absence license (TmuxSessionProvenAbsent = cacheComplete &&
// !panePresent) is definitionally unavailable, the /proc sweep cannot clear
// post-incarnation strangers, and the observation reports positive liveness
// with Complete=false on every sweep, forever.
type aliveIncompleteObservationProvider struct {
	*runtime.Fake
	observed int
}

func (p *aliveIncompleteObservationProvider) ObserveFreshLiveness(target runtime.LivenessTarget) runtime.Liveness {
	p.observed++
	running := p.IsRunning(target.SessionName)
	return runtime.Liveness{Running: running, Alive: running, Complete: false}
}

func (p *aliveIncompleteObservationProvider) StopUnattendedSession(name, _ string) error {
	return p.Stop(name)
}

// TestExactSleepDrainAliveSessionProceedsOnIncompleteScan is the field wedge
// (tr-j82xw / su-h9kaad / or-b24cs / pl-65t6r, 2026-08-23): an alive, idle,
// wake-suppressed session whose liveness observation is POSITIVE but
// incomplete. Scan completeness exists to prove ABSENCE; a positive
// observation is decisive on its own, and the drain begin it licenses is
// enqueue-only (the interrupt stays with the advance, the terminal stop stays
// behind its own fresh-death proof). Parking here wedged the four sessions
// permanently, because a live pane withholds the very license that would
// complete the scan.
func TestExactSleepDrainAliveSessionProceedsOnIncompleteScan(t *testing.T) {
	const name = "worker"
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		SessionSleep: config.SessionSleepConfig{InteractiveResume: "60s"},
		Workspace:    config.Workspace{Name: "test-city"},
		Agents:       []config.Agent{{Name: name, StartCommand: "true"}},
	}
	provider := &aliveIncompleteObservationProvider{Fake: env.sp}
	if err := provider.Start(t.Context(), name, runtime.Config{}); err != nil {
		t.Fatalf("start runtime for %q: %v", name, err)
	}
	bead := env.createSessionBead(name, name)
	env.markSessionActive(&bead)
	env.setSessionMetadata(&bead, map[string]string{
		"detached_at": env.clk.Now().UTC().Add(-6 * time.Minute).Format(time.RFC3339),
	})
	env.sp.WaitForIdleErrors[name] = nil

	info, response, err := getAuthoritativeSessionStartPersistedRecord(env.store, bead.ID)
	if err != nil {
		t.Fatalf("authoritative read: %v", err)
	}
	params := newExactSleepDrainParams(env, provider, name)

	handled, owner, err := reconcileExactSessionDetectorFamily(
		t.Context(), sleepDrainAdmission(bead.ID), params, info, response, env.clk)
	if !handled {
		t.Fatal("the D-SLEEP seam did not claim a live no-wake row")
	}
	if err != nil {
		t.Fatalf("an alive session's incomplete scan parked the drain begin: %v", err)
	}
	if owner != exactSessionStartKeyedOwner {
		t.Fatalf("owner = %v, want keyed ownership", owner)
	}
	if provider.observed == 0 {
		t.Fatal("the handler never observed liveness; the test proves nothing")
	}
	waitForIdleProbeReady(t, env.dt, bead.ID)

	handled, _, err = reconcileExactSessionDetectorFamily(
		t.Context(), sleepDrainAdmission(bead.ID), params, info, response, env.clk)
	if !handled || err != nil {
		t.Fatalf("drain leg: handled=%v err=%v", handled, err)
	}
	if env.dt.get(bead.ID) == nil {
		t.Fatal("no drain intent recorded; the positive observation must license the enqueue-only begin")
	}
}

// TestExactSleepDrainDeadIncompleteObservationStillParks is the fail-closed
// control for the test above: when the observation is NEGATIVE and incomplete,
// dead cannot be told apart from unobserved, so the handler must still refuse.
func TestExactSleepDrainDeadIncompleteObservationStillParks(t *testing.T) {
	env := newReconcilerTestEnv()
	provider, bead := seedIdleSuppressedSession(t, env)
	provider.incomplete = true

	info, response, err := getAuthoritativeSessionStartPersistedRecord(env.store, bead.ID)
	if err != nil {
		t.Fatalf("authoritative read: %v", err)
	}
	params := newExactSleepDrainParams(env, provider, "worker")

	handled, _, err := reconcileExactSessionDetectorFamily(
		t.Context(), sleepDrainAdmission(bead.ID), params, info, response, env.clk)
	if !handled {
		t.Fatal("the D-SLEEP seam did not claim the row")
	}
	if err == nil || !strings.Contains(err.Error(), "liveness observation is incomplete") {
		t.Fatalf("err = %v, want the incomplete-liveness park for a negative unproven observation", err)
	}
	if env.dt.get(bead.ID) != nil {
		t.Fatal("an unproven observation recorded drain intent")
	}
}
