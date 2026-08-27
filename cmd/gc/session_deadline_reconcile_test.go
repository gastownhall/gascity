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

// newExactDeadlineRuntime builds a keyed city runtime whose session-start
// controller is live and whose D-DEADLINE trackers are seeded, so a detector
// admission reaches the real handler rather than a test double.
func newExactDeadlineRuntime(
	t *testing.T,
	env *reconcilerTestEnv,
	provider runtime.Provider,
	it idleTracker,
	mat maxSessionAgeTracker,
	rec events.Provider,
) *CityRuntime {
	t.Helper()
	cs := coherentSessionStartControllerStateForTest(env.cfg, provider, env.store, rollout.Require)
	if rec != nil {
		cs.eventProv = rec
	}
	cr := &CityRuntime{
		cityPath: "test-city",
		cityName: "test-city",
		cfg:      env.cfg,
		sp:       provider,
		cs:       cs,
		rec:      events.Discard,
		stdout:   &bytes.Buffer{},
		stderr:   &bytes.Buffer{},
		it:       it,
		mat:      mat,
	}
	if err := cr.ensureSessionStartController(t.Context(), newSessionBeadSnapshotFromInfos(nil)); err != nil {
		t.Fatalf("ensure session-start controller: %v", err)
	}
	t.Cleanup(cr.stopSessionStartController)
	return cr
}

// deadlineSweepInput builds the minimum sweep input that reaches D-DEADLINE for
// one row, with admit as the routing seam's enqueue hook.
func deadlineSweepInput(
	env *reconcilerTestEnv,
	provider runtime.Provider,
	info sessionpkg.Info,
	it idleTracker,
	mat maxSessionAgeTracker,
	now time.Time,
	admit func(string, sessionStartAdmissionSource) (sessionStartAdmissionOutcome, error),
) detectorSweepInput {
	name := info.SessionNameMetadata
	return detectorSweepInput{
		CityPath: "test-city",
		CityName: "test-city",
		Cfg:      env.cfg,
		Provider: provider,
		Rows:     []sessionpkg.ReconcileSession{{Info: info}},
		// Desired, so the row reaches D-DEADLINE: family precedence routes an
		// undesired row to D-ORPHAN and evaluates it no further.
		Desired: map[string]TemplateParams{name: {SessionName: name, TemplateName: info.Template}},
		Idle:    it,
		MaxAge:  mat,
		Clock:   &clock.Fake{Time: now},
		Trigger: "patrol",
		Admit:   admit,
	}
}

type recordingDetectorAdmitter struct {
	keys    []string
	sources []sessionStartAdmissionSource
}

func (r *recordingDetectorAdmitter) admit(id string, source sessionStartAdmissionSource) (sessionStartAdmissionOutcome, error) {
	r.keys = append(r.keys, id)
	r.sources = append(r.sources, source)
	return sessionStartAdmissionAccepted, nil
}

// TestExactDeadlineIdleSessionStopsOnceByKey is WD.2's primary RED: a seeded
// row past its idle deadline, handed to the session-start controller under the
// D-DEADLINE admission source, is stopped exactly once with one token-bound
// unattended stop, and its sleep_reason lands durably before the key is
// released. It is the keyed re-point of
// TestReconcileSessionBeads_IdleTimeoutStopsAndStaysAsleep.
func TestExactDeadlineIdleSessionStopsOnceByKey(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker", StartCommand: "true"}}}
	provider := &unattendedStopProvider{Fake: env.sp}
	bead := env.createSessionBead("worker", "worker")
	env.markSessionActive(&bead)
	env.setSessionMetadata(&bead, map[string]string{
		"pending_create_claim": "true",
		"sleep_intent":         "idle-stop-pending",
	})
	if err := provider.Start(t.Context(), "worker", runtime.Config{}); err != nil {
		t.Fatalf("start runtime: %v", err)
	}
	if err := provider.SetMeta("worker", "GC_INSTANCE_TOKEN", "test-token"); err != nil {
		t.Fatalf("set runtime token: %v", err)
	}

	it := newFakeIdleTracker()
	it.idle["worker"] = true
	rec := events.NewFake()
	cr := newExactDeadlineRuntime(t, env, provider, it, nil, rec)

	admit := cr.detectorAdmitFunc()
	if admit == nil {
		t.Fatal("detectorAdmitFunc() = nil under keyed ownership; the sweep has no enqueue seam")
	}
	outcome, err := admit(bead.ID, sessionStartAdmissionDeadline)
	if err != nil || outcome == sessionStartAdmissionOverflow {
		t.Fatalf("admitting deadline key: outcome=%q err=%v", outcome, err)
	}

	awaitCond(t, func() bool { return !provider.IsRunning("worker") }, "keyed idle-deadline stop")
	awaitCond(t, func() bool { return env.sessionInfo(bead.ID).SleepReason == "idle-timeout" }, "durable idle-timeout sleep reason")

	after := env.sessionInfo(bead.ID)
	if after.Closed || after.MetadataState != string(sessionpkg.StateAsleep) {
		t.Fatalf("durable row = closed:%v state:%q, want an open asleep row", after.Closed, after.MetadataState)
	}
	stored, err := env.store.Get(bead.ID)
	if err != nil {
		t.Fatalf("read stopped row: %v", err)
	}
	if stored.Metadata["last_woke_at"] != "" || stored.Metadata["pending_create_claim"] != "" || stored.Metadata["sleep_intent"] != "" {
		t.Fatalf("sleep patch did not clear the wake markers: %#v", stored.Metadata)
	}
	if calls := provider.stopSnapshot(); len(calls) != 1 || calls[0].name != "worker" || calls[0].expectedToken != "test-token" {
		t.Fatalf("unattended stop calls = %#v, want exactly one token-bound stop", calls)
	}
	fired := 0
	for _, e := range rec.Events {
		if e.Type == events.SessionIdleKilled {
			fired++
		}
	}
	if fired != 1 {
		t.Fatalf("SessionIdleKilled events = %d, want exactly 1", fired)
	}
}

// TestExactDeadlineOverAgeSessionStopsOnceByKey is the merged MaxSessionAge arm
// (DETECTOR.md R1): the same detector and the same handler, differing only in
// deadline source and reason. Keyed re-point of
// TestReconcileSessionBeads_MaxSessionAgeKillsAgedSession.
func TestExactDeadlineOverAgeSessionStopsOnceByKey(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "witness", MaxSessionAge: "5h", StartCommand: "true"}}}
	provider := &unattendedStopProvider{Fake: env.sp}
	bead := env.createSessionBead("witness", "witness")
	env.markSessionActive(&bead)
	env.setSessionMetadata(&bead, map[string]string{
		"creation_complete_at": time.Now().UTC().Add(-6 * time.Hour).Format(time.RFC3339),
	})
	if err := provider.Start(t.Context(), "witness", runtime.Config{}); err != nil {
		t.Fatalf("start runtime: %v", err)
	}
	if err := provider.SetMeta("witness", "GC_INSTANCE_TOKEN", "test-token"); err != nil {
		t.Fatalf("set runtime token: %v", err)
	}

	mat := newMaxSessionAgeTracker()
	mat.setConfig("witness", 5*time.Hour, 0)
	rec := events.NewFake()
	cr := newExactDeadlineRuntime(t, env, provider, nil, mat, rec)

	if outcome, err := cr.detectorAdmitFunc()(bead.ID, sessionStartAdmissionDeadline); err != nil || outcome == sessionStartAdmissionOverflow {
		t.Fatalf("admitting deadline key: outcome=%q err=%v", outcome, err)
	}

	awaitCond(t, func() bool { return !provider.IsRunning("witness") }, "keyed max-age stop")
	awaitCond(t, func() bool { return env.sessionInfo(bead.ID).SleepReason == "max-session-age" }, "durable max-session-age sleep reason")

	if calls := provider.stopSnapshot(); len(calls) != 1 || calls[0].expectedToken != "test-token" {
		t.Fatalf("unattended stop calls = %#v, want exactly one token-bound stop", calls)
	}
	fired := false
	for _, e := range rec.Events {
		if e.Type == events.SessionMaxAgeKilled {
			fired = true
		}
	}
	if !fired {
		t.Fatal("expected SessionMaxAgeKilled event on the keyed path")
	}
}

// TestExactDeadlineSleepLandsBeforeHandlerReturns pins the ordering the whole
// family exists for: the sleep patch is durable by the time the handler
// returns. The controller deletes an admission only AFTER callReconcile
// returns, so "before return" IS "before key release" — a D-WAKE admission on
// the same key cannot see an awake row and respawn the incarnation this handler
// just killed.
func TestExactDeadlineSleepLandsBeforeHandlerReturns(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker", StartCommand: "true"}}}
	provider := &unattendedStopProvider{Fake: env.sp}
	bead := env.createSessionBead("worker", "worker")
	env.markSessionActive(&bead)
	if err := provider.Start(t.Context(), "worker", runtime.Config{}); err != nil {
		t.Fatalf("start runtime: %v", err)
	}
	if err := provider.SetMeta("worker", "GC_INSTANCE_TOKEN", "test-token"); err != nil {
		t.Fatalf("set runtime token: %v", err)
	}
	it := newFakeIdleTracker()
	it.idle["worker"] = true

	info, response, err := getAuthoritativeSessionStartPersistedRecord(env.store, bead.ID)
	if err != nil {
		t.Fatalf("authoritative read: %v", err)
	}
	statusWriter, _, statusWriterErr := beads.ResolveConditionalWriter(env.store)
	params := exactSessionStartParams{
		Generation: 1, CityPath: "test-city", CityName: "test-city",
		Config: env.cfg, Provider: provider, Store: env.store,
		StatusWriter: statusWriter, StatusWriterError: statusWriterErr,
		Recorder: events.Discard, RolloutMode: rollout.Require, IdleTracker: it,
	}
	owner, err := reconcileExactSessionDeadlineStop(
		t.Context(),
		sessionStartAdmission{SessionID: bead.ID, Source: sessionStartAdmissionDeadline},
		params, info, response, env.clk,
	)
	if err != nil || owner != exactSessionStartKeyedOwner {
		t.Fatalf("handler returned owner=%v err=%v, want keyed ownership and no error", owner, err)
	}
	if got := env.sessionInfo(bead.ID); got.SleepReason != "idle-timeout" || got.MetadataState != string(sessionpkg.StateAsleep) {
		t.Fatalf("sleep had not landed when the handler returned: state=%q sleep_reason=%q", got.MetadataState, got.SleepReason)
	}
	if provider.IsRunning("worker") {
		t.Fatal("handler returned with the runtime still alive")
	}
}

// TestExactDeadlineMinFloorExemptsTheFloorAndStopsAboveIt pins DecideIdleTimeout's
// keep-warm floor rung (sc-5mtyhy) on the keyed path.
//
// The rung is a gather, and an unanswered gather is not a defer: the ladder
// returns holding TimerActionGatherMinFloor, which is neither Stop nor Defer, so
// the handler treats EVERY idle deadline as declined and no keyed idle stop ever
// fires. That is why the above-floor half is not optional — it is the control
// that separates "the rung is answered" from "the family stopped stopping", and
// it fails against a ladder that simply defers whatever it does not understand.
func TestExactDeadlineMinFloorExemptsTheFloorAndStopsAboveIt(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{
		Name: "worker", StartCommand: "true", MinActiveSessions: intPtr(1),
	}}}
	provider := &unattendedStopProvider{Fake: env.sp}
	it := newFakeIdleTracker()

	beadsByName := map[string]beads.Bead{}
	for _, name := range []string{"worker-a", "worker-b"} {
		bead := env.createSessionBead(name, "worker")
		env.markSessionActive(&bead)
		if err := provider.Start(t.Context(), name, runtime.Config{}); err != nil {
			t.Fatalf("start runtime %s: %v", name, err)
		}
		if err := provider.SetMeta(name, "GC_INSTANCE_TOKEN", "test-token"); err != nil {
			t.Fatalf("set runtime token %s: %v", name, err)
		}
		it.idle[name] = true
		beadsByName[name] = bead
	}

	// The floor is ranked by bead ID, which the store mints; read the ranking
	// off the beads themselves rather than assuming creation order.
	floorName, elasticName := "worker-a", "worker-b"
	if beadsByName[elasticName].ID < beadsByName[floorName].ID {
		floorName, elasticName = elasticName, floorName
	}
	floor, elastic := beadsByName[floorName], beadsByName[elasticName]

	statusWriter, _, statusWriterErr := beads.ResolveConditionalWriter(env.store)
	params := exactSessionStartParams{
		Generation: 1, CityPath: "test-city", CityName: "test-city",
		Config: env.cfg, Provider: provider, Store: env.store,
		StatusWriter: statusWriter, StatusWriterError: statusWriterErr,
		Recorder: events.Discard, RolloutMode: rollout.Require, IdleTracker: it,
	}
	stop := func(t *testing.T, bead beads.Bead) {
		t.Helper()
		info, response, err := getAuthoritativeSessionStartPersistedRecord(env.store, bead.ID)
		if err != nil {
			t.Fatalf("authoritative read: %v", err)
		}
		owner, err := reconcileExactSessionDeadlineStop(
			t.Context(),
			sessionStartAdmission{SessionID: bead.ID, Source: sessionStartAdmissionDeadline},
			params, info, response, env.clk,
		)
		if err != nil || owner != exactSessionStartKeyedOwner {
			t.Fatalf("handler returned owner=%v err=%v, want keyed ownership and no error", owner, err)
		}
	}

	stop(t, floor)
	if !provider.IsRunning(floorName) {
		t.Fatal("the min_active_sessions floor member was idle-killed; the keep-warm rung must defer it")
	}
	if got := env.sessionInfo(floor.ID); got.SleepReason != "" || got.MetadataState != "active" {
		t.Fatalf("floor member row = state:%q sleep_reason:%q, want an untouched active row", got.MetadataState, got.SleepReason)
	}

	stop(t, elastic)
	if provider.IsRunning(elasticName) {
		t.Fatal("the above-floor elastic session must still idle-reclaim")
	}
	if got := env.sessionInfo(elastic.ID); got.SleepReason != "idle-timeout" {
		t.Fatalf("above-floor row sleep_reason = %q, want idle-timeout", got.SleepReason)
	}
}

// TestDetectorDeadlineNotPassedNeitherEnqueuesNorStops is the negative: a live
// row whose deadline has NOT elapsed produces zero enqueues from the sweep, and
// the handler refuses with zero effect even if some other admission carries the
// key into the seam.
func TestDetectorDeadlineNotPassedNeitherEnqueuesNorStops(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker", StartCommand: "true"}}}
	provider := &unattendedStopProvider{Fake: env.sp}
	bead := env.createSessionBead("worker", "worker")
	env.markSessionActive(&bead)
	if err := provider.Start(t.Context(), "worker", runtime.Config{}); err != nil {
		t.Fatalf("start runtime: %v", err)
	}
	if err := provider.SetMeta("worker", "GC_INSTANCE_TOKEN", "test-token"); err != nil {
		t.Fatalf("set runtime token: %v", err)
	}

	it := newFakeIdleTracker() // nothing registered: the deadline has not elapsed
	admitter := &recordingDetectorAdmitter{}
	in := deadlineSweepInput(env, provider, env.sessionInfo(bead.ID), it, nil, time.Now().UTC(), admitter.admit)
	result := detectSessionConditions(context.Background(), in)
	routeDetectorConditions(in, &result)
	for _, cond := range result.Conditions {
		if cond.Family == detectorFamilyDeadline {
			t.Fatalf("sweep raised a deadline condition for a row inside its deadline: %#v", cond)
		}
	}
	if len(admitter.keys) != 0 {
		t.Fatalf("sweep enqueued %v with no deadline elapsed; want zero enqueues", admitter.keys)
	}

	rec := events.NewFake()
	cr := newExactDeadlineRuntime(t, env, provider, it, nil, rec)
	if outcome, err := cr.detectorAdmitFunc()(bead.ID, sessionStartAdmissionDeadline); err != nil || outcome == sessionStartAdmissionOverflow {
		t.Fatalf("admitting deadline key: outcome=%q err=%v", outcome, err)
	}
	awaitCond(t, func() bool { return !cr.sessionStartController.ownsDeadlineStop(bead.ID) }, "deadline admission drain")
	if calls := provider.stopSnapshot(); len(calls) != 0 {
		t.Fatalf("unattended stop calls = %#v, want zero for a row inside its deadline", calls)
	}
	if got := env.sessionInfo(bead.ID); got.SleepReason != "" || got.MetadataState != string(sessionpkg.StateActive) {
		t.Fatalf("durable row mutated by a refused deadline handler: state=%q sleep_reason=%q", got.MetadataState, got.SleepReason)
	}
}

// TestLegacyDeadlineArmsYieldToKeyedOwnedRow is the coexistence-doctrine RED.
// Both writers fire off the SAME tracker in the SAME tick, so an acting
// D-DEADLINE beside a non-yielding legacy is a guaranteed double stop, not a
// race. The legacy idle arm must yield while the keyed controller owns the key.
func TestLegacyDeadlineArmsYieldToKeyedOwnedRow(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	env.addDesired("worker", "worker", true)
	session := env.createSessionBead("worker", "worker")
	env.markSessionActive(&session)
	if err := env.sp.SetMeta("worker", "GC_SESSION_ID", session.ID); err != nil {
		t.Fatalf("SetMeta(GC_SESSION_ID): %v", err)
	}

	it := newFakeIdleTracker()
	it.idle["worker"] = true
	cfgNames := configuredSessionNames(env.cfg, "", env.store)
	reconcileSessionBeads(
		context.Background(), []beads.Bead{session}, env.desiredState, cfgNames,
		env.cfg, env.sp, env.store, nil, nil, nil, env.dt, map[string]int{}, false, nil, "",
		it, env.clk, env.rec, 0, 0, &env.stdout, &env.stderr,
		withLegacyDeadlineStopExclusion(func(info sessionpkg.Info) bool { return info.ID == session.ID }),
	)

	if !env.sp.IsRunning("worker") {
		t.Fatal("legacy idle-timeout arm stopped a session the keyed D-DEADLINE handler owns")
	}
	stored, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Metadata["sleep_reason"] == "idle-timeout" {
		t.Fatalf("legacy wrote the keyed handler's sleep patch: %#v", stored.Metadata)
	}
}

// TestLegacyMaxAgeArmYieldsToKeyedOwnedRow is the same doctrine on the merged
// max-session-age arm.
func TestLegacyMaxAgeArmYieldsToKeyedOwnedRow(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "witness", MaxSessionAge: "5h"}}}
	env.addDesired("witness", "witness", true)
	session := env.createSessionBead("witness", "witness")
	env.markSessionActive(&session)
	env.setSessionMetadata(&session, map[string]string{
		"creation_complete_at": env.clk.Now().Add(-6 * time.Hour).UTC().Format(time.RFC3339),
	})
	tr := newMaxSessionAgeTracker()
	tr.setConfig("witness", 5*time.Hour, 0)

	cfgNames := configuredSessionNames(env.cfg, "", env.store)
	reconcileSessionBeadsTraced(
		context.Background(), "", []beads.Bead{session}, env.desiredState, cfgNames, env.cfg, env.sp,
		env.store, nil, nil, nil, nil, env.dt, map[string]int{}, false, nil, "",
		nil, env.clk, env.rec, 0, 0, &env.stdout, &env.stderr, nil,
		withMaxSessionAgeTracker(tr),
		withLegacyDeadlineStopExclusion(func(info sessionpkg.Info) bool { return info.ID == session.ID }),
	)

	if !env.sp.IsRunning("witness") {
		t.Fatal("legacy max-session-age arm stopped a session the keyed D-DEADLINE handler owns")
	}
	stored, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Metadata["sleep_reason"] == "max-session-age" {
		t.Fatalf("legacy wrote the keyed handler's sleep patch: %#v", stored.Metadata)
	}
}

// TestDetectorDeadlineUserHoldNeverEnqueues ports the legacy user-hold negative
// (session_reconciler_test.go:9609) to the keyed path: a held row raises a
// traced deferral, never an enqueue, so the handler is never entered and makes
// zero provider calls and zero writes. The quarantine blocker is the same rung.
func TestDetectorDeadlineUserHoldNeverEnqueues(t *testing.T) {
	for _, tc := range []struct {
		name    string
		key     string
		blocker string
	}{
		{name: "user hold", key: "held_until", blocker: "user_hold"},
		{name: "quarantine", key: "quarantined_until", blocker: "quarantine"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newReconcilerTestEnv()
			env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
			provider := &unattendedStopProvider{Fake: env.sp}
			bead := env.createSessionBead("worker", "worker")
			env.markSessionActive(&bead)
			now := time.Now().UTC()
			env.setSessionMetadata(&bead, map[string]string{tc.key: now.Add(100 * time.Hour).Format(time.RFC3339)})
			if err := provider.Start(t.Context(), "worker", runtime.Config{}); err != nil {
				t.Fatalf("start runtime: %v", err)
			}

			it := newFakeIdleTracker()
			it.idle["worker"] = true
			admitter := &recordingDetectorAdmitter{}
			in := deadlineSweepInput(env, provider, env.sessionInfo(bead.ID), it, nil, now, admitter.admit)
			result := detectSessionConditions(context.Background(), in)
			routeDetectorConditions(in, &result)

			if len(admitter.keys) != 0 {
				t.Fatalf("blocked row enqueued %v; want zero enqueues", admitter.keys)
			}
			deferred := false
			for _, cond := range result.Conditions {
				if cond.Family != detectorFamilyDeadline {
					continue
				}
				if cond.Outcome == TraceOutcomeStop {
					t.Fatalf("blocked row predicted a stop: %#v", cond)
				}
				if cond.Fields["blocker"] == tc.blocker {
					deferred = true
				}
			}
			if !deferred {
				t.Fatalf("no traced %s deferral recorded; conditions=%#v", tc.blocker, result.Conditions)
			}
			if calls := provider.stopSnapshot(); len(calls) != 0 {
				t.Fatalf("blocked row produced provider stops: %#v", calls)
			}
		})
	}
}

// TestDetectorDeadlineRefusesD2IncapableProviderWithoutTreadmill is the second
// negative: a provider that cannot prove fresh liveness or unattended stop
// yields a traced refusal and no enqueue, and repeating the sweep does not form
// a re-enqueue treadmill (DETECTOR.md §2, detection-side D2 screen).
func TestDetectorDeadlineRefusesD2IncapableProviderWithoutTreadmill(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	bead := env.createSessionBead("worker", "worker")
	env.markSessionActive(&bead)
	if err := env.sp.Start(t.Context(), "worker", runtime.Config{}); err != nil {
		t.Fatalf("start runtime: %v", err)
	}
	if _, ok := runtime.Provider(env.sp).(runtime.UnattendedSessionStopper); ok {
		t.Fatal("fixture provider is D2-capable; the refusal arm cannot be observed")
	}

	it := newFakeIdleTracker()
	it.idle["worker"] = true
	admitter := &recordingDetectorAdmitter{}
	in := deadlineSweepInput(env, env.sp, env.sessionInfo(bead.ID), it, nil, time.Now().UTC(), admitter.admit)

	refusals := 0
	for range 2 {
		result := detectSessionConditions(context.Background(), in)
		routeDetectorConditions(in, &result)
		for _, cond := range result.Conditions {
			if cond.Family == detectorFamilyDeadline && cond.AdmissionOutcome == detectorAdmissionRefusedProviderIncapable {
				refusals++
			}
		}
	}
	if refusals != 2 {
		t.Fatalf("traced D2 refusals = %d over two sweeps, want 2", refusals)
	}
	if len(admitter.keys) != 0 {
		t.Fatalf("D2-incapable provider enqueued %v; want zero enqueues and no treadmill", admitter.keys)
	}
}

// A pin is a blocker for exactly ONE lifecycle timer.
//
// Main added "pinned" as a third lifecycleTimerBlockerInfo value and, in the
// same change, narrowed it back out of the age timer (maxSessionAgeBlockerInfo,
// session_reconciler.go:72). The reason is that a max-age stop is not a kill:
// SleepPatch records state=asleep with sleep_reason=max_session_age and leaves
// pin_awake alone, and ComputeAwakeSet's durable pin override re-wakes an asleep
// pinned row on the next tick. So the stop IS the credential refresh the timer
// exists to perform, and exempting it skips the refresh without saving the
// session — permanently, because a pin never self-clears the way a hold or a
// quarantine does.
//
// The keyed path took the unnarrowed blocker at both of its D-DEADLINE seams.
// These three tests pin the per-timer split from both directions: the age
// restart must still happen for a pinned row, and the idle kill must still not.

// TestExactDeadlinePinnedSessionStillStopsForAge is the keyed handler's RED. It
// covers both halves of the seam at once — the family guard
// (exactSessionDeadlineStopCandidate) and the ladder's own blocker rung inside
// decideExactSessionDeadline — because either one still taking the pin leaves
// the session running past its age deadline.
func TestExactDeadlinePinnedSessionStillStopsForAge(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "witness", MaxSessionAge: "5h", StartCommand: "true"}}}
	provider := &unattendedStopProvider{Fake: env.sp}
	bead := env.createSessionBead("witness", "witness")
	env.markSessionActive(&bead)
	env.setSessionMetadata(&bead, map[string]string{
		"pin_awake":            "true",
		"creation_complete_at": time.Now().UTC().Add(-6 * time.Hour).Format(time.RFC3339),
	})
	if err := provider.Start(t.Context(), "witness", runtime.Config{}); err != nil {
		t.Fatalf("start runtime: %v", err)
	}
	if err := provider.SetMeta("witness", "GC_INSTANCE_TOKEN", "test-token"); err != nil {
		t.Fatalf("set runtime token: %v", err)
	}

	mat := newMaxSessionAgeTracker()
	mat.setConfig("witness", 5*time.Hour, 0)
	cr := newExactDeadlineRuntime(t, env, provider, nil, mat, events.NewFake())

	if outcome, err := cr.detectorAdmitFunc()(bead.ID, sessionStartAdmissionDeadline); err != nil || outcome == sessionStartAdmissionOverflow {
		t.Fatalf("admitting deadline key: outcome=%q err=%v", outcome, err)
	}

	awaitCond(t, func() bool { return !provider.IsRunning("witness") }, "keyed max-age stop for a pinned row")
	awaitCond(t, func() bool { return env.sessionInfo(bead.ID).SleepReason == "max-session-age" },
		"durable max-session-age sleep reason for a pinned row")
}

// TestExactDeadlinePinnedSessionKeepsIdleExemption is the control that keeps the
// narrowing honest. Dropping the blocker outright would pass the test above and
// still be wrong: an operator's pin has to go on suppressing the idle kill,
// which is the half of #4648 that survived review unchanged.
func TestExactDeadlinePinnedSessionKeepsIdleExemption(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker", StartCommand: "true"}}}
	provider := &unattendedStopProvider{Fake: env.sp}
	bead := env.createSessionBead("worker", "worker")
	env.markSessionActive(&bead)
	env.setSessionMetadata(&bead, map[string]string{"pin_awake": "true"})
	if err := provider.Start(t.Context(), "worker", runtime.Config{}); err != nil {
		t.Fatalf("start runtime: %v", err)
	}

	it := newFakeIdleTracker()
	it.idle["worker"] = true
	cr := newExactDeadlineRuntime(t, env, provider, it, nil, events.NewFake())

	if outcome, err := cr.detectorAdmitFunc()(bead.ID, sessionStartAdmissionDeadline); err != nil || outcome == sessionStartAdmissionOverflow {
		t.Fatalf("admitting deadline key: outcome=%q err=%v", outcome, err)
	}

	if calls := provider.stopSnapshot(); len(calls) != 0 {
		t.Fatalf("pinned row was idle-killed: %#v", calls)
	}
	if !provider.IsRunning("worker") {
		t.Fatal("pinned row stopped running; the idle exemption is gone")
	}
}

// TestDetectorDeadlinePinnedRowDefersIdleButRoutesMaxAge is the sweep's RED. The
// detector captured the blocker ONCE per row and the shared closure applied it
// to both arms, so a pinned row recorded deadline_deferred for max-age and
// detectorAdmissionSourceFor — which only enqueues on TraceOutcomeStop — never
// routed the stop. Both arms fire here, and they must disagree.
func TestDetectorDeadlinePinnedRowDefersIdleButRoutesMaxAge(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker", MaxSessionAge: "5h"}}}
	provider := &unattendedStopProvider{Fake: env.sp}
	bead := env.createSessionBead("worker", "worker")
	env.markSessionActive(&bead)
	now := time.Now().UTC()
	env.setSessionMetadata(&bead, map[string]string{
		"pin_awake":            "true",
		"creation_complete_at": now.Add(-6 * time.Hour).Format(time.RFC3339),
	})
	if err := provider.Start(t.Context(), "worker", runtime.Config{}); err != nil {
		t.Fatalf("start runtime: %v", err)
	}

	it := newFakeIdleTracker()
	it.idle["worker"] = true
	mat := newMaxSessionAgeTracker()
	mat.setConfig("worker", 5*time.Hour, 0)
	admitter := &recordingDetectorAdmitter{}
	in := deadlineSweepInput(env, provider, env.sessionInfo(bead.ID), it, mat, now, admitter.admit)
	result := detectSessionConditions(context.Background(), in)
	routeDetectorConditions(in, &result)

	idleDeferred, ageStop := false, false
	for _, cond := range result.Conditions {
		if cond.Family != detectorFamilyDeadline {
			continue
		}
		switch cond.Site {
		case TraceSiteReconcilerIdleTimeout:
			if cond.Outcome == TraceOutcomeStop {
				t.Fatalf("pinned row predicted an idle stop: %#v", cond)
			}
			idleDeferred = cond.Fields["blocker"] == "pinned"
		case TraceSiteReconcilerMaxSessionAge:
			if cond.Outcome != TraceOutcomeStop {
				t.Fatalf("pinned row did not predict its age stop: %#v", cond)
			}
			ageStop = true
		}
	}
	if !idleDeferred {
		t.Fatalf("no traced pin deferral on the idle arm; conditions=%#v", result.Conditions)
	}
	if !ageStop {
		t.Fatalf("no predicted max-age stop on a pinned row; conditions=%#v", result.Conditions)
	}
	if len(admitter.keys) != 1 || admitter.keys[0] != bead.ID {
		t.Fatalf("max-age enqueues = %v, want exactly the pinned row's key", admitter.keys)
	}
}

// seedPendingIdleDeadlineRow seeds the ONE row that reaches DecideIdleTimeout's
// PendingYes rung: a live, desired session past its idle deadline with a
// structured interaction outstanding. That rung is the only producer of
// TimerDecision.CancelDrain and TimerDecision.SkipWakePass
// (internal/session/lifecycle_timers.go), so it is the only fixture that can
// exercise the two fields the keyed arm does not read (ga-f7v2ft.181).
func seedPendingIdleDeadlineRow(t *testing.T, env *reconcilerTestEnv) (*unattendedStopProvider, beads.Bead, exactSessionStartParams) {
	t.Helper()
	const name = "worker"
	env.cfg = &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents:    []config.Agent{{Name: name, StartCommand: "true"}},
	}
	provider := &unattendedStopProvider{Fake: env.sp}
	if err := provider.Start(t.Context(), name, runtime.Config{}); err != nil {
		t.Fatalf("start runtime: %v", err)
	}
	provider.SetPendingInteraction(name, &runtime.PendingInteraction{})
	bead := env.createSessionBead(name, name)
	env.markSessionActive(&bead)

	it := newFakeIdleTracker()
	it.idle[name] = true
	statusWriter, _, statusWriterErr := beads.ResolveConditionalWriter(env.store)
	params := exactSessionStartParams{
		Generation: 1, CityPath: "test-city", CityName: "test-city",
		Config: env.cfg, Provider: provider, Store: env.store,
		StatusWriter: statusWriter, StatusWriterError: statusWriterErr,
		Recorder: events.Discard, RolloutMode: rollout.Require,
		Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{},
		IdleTracker:         it,
		DrainTracker:        env.dt,
		DrainOps:            newDrainOps(provider),
		DesiredSessionNames: func() map[string]bool { return map[string]bool{name: true} },
	}
	return provider, bead, params
}

// TestKeyedDeadlineCancelDrainIsUnreachableByConstruction is ga-f7v2ft.181's
// CancelDrain half, asserted rather than assumed.
//
// DecideIdleTimeout's PendingYes rung sets CancelDrain, and the keyed arm never
// reads it. That is safe only because reconcileExactSessionDeadlineStop yields
// the whole row the moment params.DrainTracker holds an entry for it — and it
// does so BEFORE decideExactSessionDeadline runs, which is the only caller of
// that ladder in the tree. So wherever a CancelDrain=true decision can exist,
// there is no drain to cancel, and legacy's own consumer
// (cancelSessionDrainInfo → cancelSessionDrainIfInfo) returns false on a tracker
// with no entry for the key.
//
// Both legs are needed. The first proves the field IS produced here — an
// unreachable rung would make the whole exemption vacuous. The second proves the
// guard, not some unrelated refusal, is what withholds the row: the SAME fixture
// with a tracked drain must fail DIFFERENTLY, by yielding instead of deferring.
func TestKeyedDeadlineCancelDrainIsUnreachableByConstruction(t *testing.T) {
	t.Run("the rung fires and there is nothing to cancel", func(t *testing.T) {
		env := newReconcilerTestEnv()
		_, bead, params := seedPendingIdleDeadlineRow(t, env)
		info := env.sessionInfo(bead.ID)

		decision, _, ok := decideExactSessionDeadline(params, info, env.clk, env.clk.Now().UTC())
		if ok {
			t.Fatalf("decision = %+v, want the pending-interaction defer rather than a stop", decision)
		}
		if !decision.CancelDrain || !decision.SkipWakePass {
			t.Fatalf("decision = %+v, want CancelDrain and SkipWakePass set.\n"+
				"If this rung stopped producing them the exemption is vacuous and must be re-derived, not kept.", decision)
		}
		if params.DrainTracker.get(bead.ID) != nil {
			t.Fatal("a drain was tracked for a row the handler would have yielded; the guard's premise does not hold")
		}
		if cancelSessionDrainInfo(info, params.Provider, params.DrainTracker) {
			t.Fatal("legacy's own consumer canceled something here; the keyed arm's omission is then a real dropped effect")
		}
	})

	t.Run("a tracked drain yields the row before the ladder runs", func(t *testing.T) {
		env := newReconcilerTestEnv()
		provider, bead, params := seedPendingIdleDeadlineRow(t, env)
		now := env.clk.Now()
		params.DrainTracker.set(bead.ID, &drainState{
			startedAt:  now.Add(-10 * time.Second),
			deadline:   now.Add(defaultDrainTimeout),
			reason:     "idle",
			generation: 1,
		})
		info, response, err := getAuthoritativeSessionStartPersistedRecord(env.store, bead.ID)
		if err != nil {
			t.Fatalf("authoritative read: %v", err)
		}

		owner, err := reconcileExactSessionDeadlineStop(t.Context(), sessionStartAdmission{
			SessionID: bead.ID, Source: sessionStartAdmissionDeadline, Version: 3,
		}, params, info, response, env.clk)
		if err == nil {
			t.Fatal("the handler ran the ladder on a row carrying an active drain; the CancelDrain guard is not total")
		}
		if owner != exactSessionStartKeyedOwner {
			t.Fatalf("owner = %v, want keyed under rollout.Require", owner)
		}
		if params.DrainTracker.get(bead.ID) == nil {
			t.Fatal("the yield canceled the drain; advancing a drain is D-DRAIN's, not this family's")
		}
		if !provider.IsRunning("worker") {
			t.Fatal("the yield stopped the runtime")
		}
	})
}

// TestKeyedDeadlineSkipWakePassIsStructural is ga-f7v2ft.181's SkipWakePass
// half. Legacy's `continue` withholds the session from that tick's wake/sleep
// pass; the keyed handler has no wake pass of its own, so the equivalent
// guarantee has to come from whatever decides that no OTHER family drains the
// row. Two independent things do, and the three legs below take them apart one
// variable at a time:
//
//	deadline fires + pending  → D-DEADLINE claims the key (it sits above D-SLEEP
//	                            in reconcileExactSessionDetectorFamily), so the
//	                            sleep family never runs. The decision carrying
//	                            SkipWakePass can only be produced inside
//	                            reconcileExactSessionDeadlineStop, which only runs
//	                            once that claim has happened, so the withholding is
//	                            total for the dispatch — and checkIdle is a pure
//	                            read, so the claim is stable across dispatches
//	                            within a tick.
//	deadline withdrawn        → D-SLEEP takes the row and DEFERS on A6 active use
//	  + pending                 (exactSessionActiveUseDeferralReason →
//	                            "pending_interaction"). The protection survives
//	                            even if the claim ordering ever changed.
//	both withdrawn            → D-SLEEP drains. Without this the two legs above
//	                            would be satisfied by a row nothing was ever going
//	                            to drain.
func TestKeyedDeadlineSkipWakePassIsStructural(t *testing.T) {
	seed := func(t *testing.T, deadlineFires, pending bool) (*unattendedStopProvider, beads.Bead, exactSessionStartParams) {
		t.Helper()
		env := newReconcilerTestEnv()
		provider, bead, params := seedPendingIdleDeadlineRow(t, env)
		env.setSessionMetadata(&bead, map[string]string{"sleep_intent": sessionSleepIntentIdleStopPending})
		if !deadlineFires {
			it, ok := params.IdleTracker.(*fakeIdleTracker)
			if !ok {
				t.Fatalf("IdleTracker = %T, want the fixture's fake", params.IdleTracker)
			}
			it.idle["worker"] = false
		}
		if !pending {
			provider.SetPendingInteraction("worker", nil)
		}
		info, response, err := getAuthoritativeSessionStartPersistedRecord(env.store, bead.ID)
		if err != nil {
			t.Fatalf("authoritative read: %v", err)
		}
		if !exactSessionSleepDrainCandidate(params, info, response, env.clk) {
			t.Fatal("the fixture is not a D-SLEEP candidate, so nothing here proves anything withheld it")
		}
		handled, owner, err := reconcileExactSessionDetectorFamily(t.Context(), sessionStartAdmission{
			SessionID: bead.ID, Source: sessionStartAdmissionDeadline, Version: 3,
		}, params, info, response, env.clk)
		if !handled || err != nil || owner != exactSessionStartKeyedOwner {
			t.Fatalf("dispatch: handled=%v owner=%v err=%v", handled, owner, err)
		}
		return provider, bead, params
	}

	t.Run("a fired deadline claims the row above the sleep family", func(t *testing.T) {
		provider, bead, params := seed(t, true, true)
		if state := params.DrainTracker.get(bead.ID); state != nil {
			t.Fatalf("drain = %+v: the sleep family drained a row the idle ladder deferred for a pending interaction.\n"+
				"D-DEADLINE must claim the row above D-SLEEP, which is what legacy's SkipWakePass buys (ga-f7v2ft.181).", state)
		}
		if !provider.IsRunning("worker") {
			t.Fatal("the pending-interaction defer stopped the runtime")
		}
	})

	t.Run("without the deadline the sleep family still defers on the pending interaction", func(t *testing.T) {
		_, bead, params := seed(t, false, true)
		if state := params.DrainTracker.get(bead.ID); state != nil {
			t.Fatalf("drain = %+v, want the A6 active-use deferral: a person is waiting on a prompt", state)
		}
	})

	t.Run("control: with neither the deadline nor the interaction, the row drains", func(t *testing.T) {
		_, bead, params := seed(t, false, false)
		if params.DrainTracker.get(bead.ID) == nil {
			t.Fatal("the sleep family never drains this fixture, so the two legs above withhold nothing")
		}
	})
}

// aliveIncompleteStopProvider reproduces the tmux provider's structural
// asymmetry (internal/runtime/tmux/state_cache.go ObserveFreshLiveness):
// Complete = cacheComplete && scanComplete, and scanComplete can only clear
// unreadable strangers on a busy host once the tmux-absence license
// (TmuxSessionProvenAbsent = cacheComplete && !panePresent) is granted. A LIVE
// target holds a pane, so it withholds that license and its observation is
// positive-but-INCOMPLETE on every sweep, forever. The moment the pane is gone
// the license is granted and the very same probe returns a COMPLETE proven-dead
// observation — which is exactly what this family's post-stop confirm demands.
//
// alwaysIncomplete withholds the license even on absence, which is the
// fail-closed control's shape: a NEGATIVE observation that proves nothing.
type aliveIncompleteStopProvider struct {
	*unattendedStopProvider
	alwaysIncomplete bool
}

func (p *aliveIncompleteStopProvider) ObserveFreshLiveness(target runtime.LivenessTarget) runtime.Liveness {
	running := p.IsRunning(target.SessionName)
	return runtime.Liveness{Running: running, Alive: running, Complete: !running && !p.alwaysIncomplete}
}

// seedAliveIncompleteMaxAgeRow seeds the specimen ga-bxa8r asked for: a session
// that is ALIVE, six hours past a five-hour max_session_age, on a host whose
// alive-target completeness is unlicensable.
func seedAliveIncompleteMaxAgeRow(t *testing.T, env *reconcilerTestEnv) (*aliveIncompleteStopProvider, beads.Bead, exactSessionStartParams) {
	t.Helper()
	const name = "witness"
	env.cfg = &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents:    []config.Agent{{Name: name, MaxSessionAge: "5h", StartCommand: "true"}},
	}
	provider := &aliveIncompleteStopProvider{unattendedStopProvider: &unattendedStopProvider{Fake: env.sp}}
	if err := provider.Start(t.Context(), name, runtime.Config{}); err != nil {
		t.Fatalf("start runtime: %v", err)
	}
	if err := provider.SetMeta(name, "GC_INSTANCE_TOKEN", "test-token"); err != nil {
		t.Fatalf("set runtime token: %v", err)
	}
	bead := env.createSessionBead(name, name)
	env.markSessionActive(&bead)
	env.setSessionMetadata(&bead, map[string]string{
		"creation_complete_at": env.clk.Now().UTC().Add(-6 * time.Hour).Format(time.RFC3339),
	})
	mat := newMaxSessionAgeTracker()
	mat.setConfig(name, 5*time.Hour, 0)
	statusWriter, _, statusWriterErr := beads.ResolveConditionalWriter(env.store)
	params := exactSessionStartParams{
		Generation: 1, CityPath: "test-city", CityName: "test-city",
		Config: env.cfg, Provider: provider, Store: env.store,
		StatusWriter: statusWriter, StatusWriterError: statusWriterErr,
		Recorder: events.Discard, RolloutMode: rollout.Require,
		Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{},
		MaxSessionAgeTracker: mat,
		DrainTracker:         env.dt,
		DrainOps:             newDrainOps(provider),
		DesiredSessionNames:  func() map[string]bool { return map[string]bool{name: true} },
	}
	return provider, bead, params
}

// TestExactDeadlineMaxAgeStopsAliveSessionOnIncompleteScan is ga-bxa8r's
// specimen. D-DEADLINE's positive arm is the only destructive-BY-INTENT arm in
// the family set the ga-i20db doctrine covers: the target is stopped precisely
// BECAUSE it is alive. Gating that stop on scan completeness therefore demanded
// a proof a live target can never supply, and the max-age kill — which is the
// fleet's credential refresh — silently never fired under keyed ownership.
//
// Nothing about the destructive obligation is relaxed. Identity is fenced where
// it is actually decidable: the revision + instance-token + name re-read below
// the observation, the token-bound unattended stop, and
// confirmDrainAckRuntimeDeadCompletion's COMPLETE proven-dead confirm, which
// this fixture can satisfy only after the pane is gone.
func TestExactDeadlineMaxAgeStopsAliveSessionOnIncompleteScan(t *testing.T) {
	env := newReconcilerTestEnv()
	provider, bead, params := seedAliveIncompleteMaxAgeRow(t, env)
	info, response, err := getAuthoritativeSessionStartPersistedRecord(env.store, bead.ID)
	if err != nil {
		t.Fatalf("authoritative read: %v", err)
	}
	if !exactSessionDeadlineStopCandidate(params, info, response, env.clk.Now().UTC()) {
		t.Fatal("the fixture never fires its max-age deadline, so nothing here proves the gate withheld the stop")
	}

	owner, err := reconcileExactSessionDeadlineStop(
		t.Context(),
		sessionStartAdmission{SessionID: bead.ID, Source: sessionStartAdmissionDeadline},
		params, info, response, env.clk,
	)
	if err != nil {
		t.Fatalf("an alive session's incomplete scan parked the max-age stop: %v", err)
	}
	if owner != exactSessionStartKeyedOwner {
		t.Fatalf("owner = %v, want keyed ownership", owner)
	}
	if provider.IsRunning("witness") {
		t.Fatal("the over-age runtime is still alive; the max-age stop never fired")
	}
	if calls := provider.stopSnapshot(); len(calls) != 1 || calls[0].expectedToken != "test-token" {
		t.Fatalf("unattended stop calls = %#v, want exactly one token-bound stop", calls)
	}
	if got := env.sessionInfo(bead.ID); got.SleepReason != "max-session-age" || got.MetadataState != string(sessionpkg.StateAsleep) {
		t.Fatalf("durable row = state:%q sleep_reason:%q, want the max-session-age sleep patch", got.MetadataState, got.SleepReason)
	}
}

// TestExactDeadlineDeadIncompleteObservationStillParks is the fail-closed
// control for the specimen above, and it must fail DIFFERENTLY: same fixture,
// same unlicensable-completeness provider, but the runtime is already gone. A
// NEGATIVE incomplete observation cannot tell dead apart from unobserved, so the
// handler must still refuse with zero effect rather than write a sleep patch
// onto a row whose agent may still be working behind an unreadable probe.
func TestExactDeadlineDeadIncompleteObservationStillParks(t *testing.T) {
	env := newReconcilerTestEnv()
	provider, bead, params := seedAliveIncompleteMaxAgeRow(t, env)
	provider.alwaysIncomplete = true
	if err := provider.Stop("witness"); err != nil {
		t.Fatalf("stop runtime: %v", err)
	}
	info, response, err := getAuthoritativeSessionStartPersistedRecord(env.store, bead.ID)
	if err != nil {
		t.Fatalf("authoritative read: %v", err)
	}

	owner, err := reconcileExactSessionDeadlineStop(
		t.Context(),
		sessionStartAdmission{SessionID: bead.ID, Source: sessionStartAdmissionDeadline},
		params, info, response, env.clk,
	)
	if err == nil || !strings.Contains(err.Error(), "liveness observation is incomplete") {
		t.Fatalf("err = %v, want the incomplete-liveness park for a negative unproven observation", err)
	}
	if owner != exactSessionStartKeyedOwner {
		t.Fatalf("owner = %v, want keyed ownership", owner)
	}
	if calls := provider.stopSnapshot(); len(calls) != 0 {
		t.Fatalf("unattended stop calls = %#v, want zero effect on an unproven absence", calls)
	}
	if got := env.sessionInfo(bead.ID); got.SleepReason != "" || got.MetadataState != string(sessionpkg.StateActive) {
		t.Fatalf("durable row = state:%q sleep_reason:%q, want an untouched active row", got.MetadataState, got.SleepReason)
	}
}
