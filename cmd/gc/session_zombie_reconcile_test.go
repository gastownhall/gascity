package main

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/agent"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/rollout"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// zombieTestTemplate is the single agent every fixture in this file seeds. The
// agent name, session name and template are one identity so the peek target and
// the durable row's template resolve to the same thing.
const zombieTestTemplate = "worker"

func zombieTestConfig() *config.City {
	return &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:         zombieTestTemplate,
			StartCommand: "test-cmd",
			ProcessNames: []string{"test-cmd"},
		}},
	}
}

// seedZombieSession writes the schema-v59 shape the legacy anchor seeds
// (TestReconcileSessionBeads_ZombieTerminalProviderErrorMarkedUnhealthy): an
// ACTIVE session row whose runtime session still exists while its agent process
// is gone, with a classifiable terminal-provider error in the scrollback.
//
// The two provider facts are what make it a zombie rather than an orphan: the
// runtime is Started (so it is IN the names-only running set) and Zombies marks
// its child process dead (so the second liveness bit is false).
func seedZombieSession(t *testing.T, env *reconcilerTestEnv, peek string) beads.Bead {
	t.Helper()
	bead := env.createSessionBead(zombieTestTemplate, zombieTestTemplate)
	env.markSessionActive(&bead)
	if err := env.sp.Start(context.Background(), zombieTestTemplate, runtime.Config{Command: "test-cmd"}); err != nil {
		t.Fatalf("start fixture runtime: %v", err)
	}
	env.sp.Zombies[zombieTestTemplate] = true
	env.sp.SetPeekOutput(zombieTestTemplate, peek)
	return bead
}

// zombieSweepInput builds the minimum sweep input that reaches D-ZOMBIE for one
// row. The row is DESIRED so family precedence does not hand it to D-ORPHAN
// before the zombie arm is consulted — legacy's forward pass early-continues on
// an undesired row for the same reason.
func zombieSweepInput(
	env *reconcilerTestEnv,
	provider runtime.Provider,
	info sessionpkg.Info,
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
		Desired:  map[string]TemplateParams{name: {SessionName: name, TemplateName: info.Template}},
		Clock:    &clock.Fake{Time: now},
		Trigger:  "patrol",
		Admit:    admit,
	}
}

// zombieHandlerParams builds the exact-key handler's parameters for a direct
// (controller-free) call. liveness is the tick's published fleet observation —
// the guard's scheduling filter — which production supplies from the patrol
// sweep through CityRuntime.publishSessionLiveness.
func zombieHandlerParams(
	env *reconcilerTestEnv,
	provider runtime.Provider,
	rec events.Recorder,
	liveness map[string]detectorLivenessBits,
) exactSessionStartParams {
	return exactSessionStartParams{
		Generation:      1,
		CityPath:        "test-city",
		CityName:        "test-city",
		Config:          env.cfg,
		Provider:        provider,
		Store:           env.store,
		Recorder:        rec,
		Stdout:          &env.stdout,
		Stderr:          &env.stderr,
		Clock:           env.clk,
		RolloutMode:     rollout.Require,
		SessionLiveness: func() map[string]detectorLivenessBits { return liveness },
	}
}

// countRecordedEvents counts the recorded events of one type on a fake recorder.
func countRecordedEvents(rec *events.Fake, typ string) int {
	n := 0
	for _, e := range rec.Events {
		if e.Type == typ {
			n++
		}
	}
	return n
}

func detectorConditionsFor(conds []detectorCondition, family detectorFamily) []detectorCondition {
	var out []detectorCondition
	for _, cond := range conds {
		if cond.Family == family {
			out = append(out, cond)
		}
	}
	return out
}

// TestExactZombieSessionMarkedUnhealthyOnceByKey is WD.11's primary RED: a
// seeded schema-v59 row that is RUNNING but NOT ALIVE is classified by the
// sweep, routed under the D-ZOMBIE admission source, and marked unhealthy
// exactly once by exact key with its terminal reason classified and
// SessionCrashed emitted. It ports the four-field assertion of
// TestReconcileSessionBeads_ZombieTerminalProviderErrorMarkedUnhealthy
// (session_reconciler_test.go) to the keyed path.
func TestExactZombieSessionMarkedUnhealthyOnceByKey(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = zombieTestConfig()
	provider := &unattendedStopProvider{Fake: env.sp}
	bead := seedZombieSession(t, env, "model_not_found: gpt-5.3-codex-spark")

	// The sweep is the producer of this key: it must classify the row into
	// D-ZOMBIE and route it under the family's own admission source.
	admitter := &recordingDetectorAdmitter{}
	in := zombieSweepInput(env, provider, env.sessionInfo(bead.ID), env.clk.Now(), admitter.admit)
	result := detectSessionConditions(context.Background(), in)
	routeDetectorConditions(in, &result)
	zombies := detectorConditionsFor(result.Conditions, detectorFamilyZombie)
	if len(zombies) != 1 || zombies[0].SessionID != bead.ID {
		t.Fatalf("sweep raised %d D-ZOMBIE conditions %v, want exactly the zombie key %q", len(zombies), zombies, bead.ID)
	}
	if len(admitter.keys) != 1 || admitter.keys[0] != bead.ID {
		t.Fatalf("sweep enqueued %v, want exactly the zombie key %q", admitter.keys, bead.ID)
	}
	if admitter.sources[0] != sessionStartAdmissionZombieMark {
		t.Fatalf("sweep enqueued under source %q, want %q", admitter.sources[0], sessionStartAdmissionZombieMark)
	}

	rec := events.NewFake()
	// The tick publishes its sweep observation; the guard is a scheduling
	// filter over it, and the handler re-observes before it writes.
	params := zombieHandlerParams(env, provider, rec, result.Liveness)
	info, response := strandedAuthoritative(t, env, bead.ID)
	handled, owner, err := reconcileExactSessionDetectorFamily(
		context.Background(),
		sessionStartAdmission{SessionID: bead.ID, Source: sessionStartAdmissionZombieMark, Version: 1},
		params, info, response, env.clk,
	)
	if !handled {
		t.Fatal("handler dispatch did not claim the zombie key")
	}
	if err != nil {
		t.Fatalf("keyed zombie mark: %v", err)
	}
	if owner != exactSessionStartKeyedOwner {
		t.Fatalf("owner = %q, want %q", owner, exactSessionStartKeyedOwner)
	}

	got, err := env.store.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get(session): %v", err)
	}
	// The legacy anchor's four fields, verbatim.
	if got.Metadata[sessionHealthStateMetadataKey] != "unhealthy" {
		t.Fatalf("session health = %q, want unhealthy", got.Metadata[sessionHealthStateMetadataKey])
	}
	if got.Metadata[sessionHealthReasonMetadataKey] != "model_not_found" {
		t.Fatalf("health reason = %q, want model_not_found", got.Metadata[sessionHealthReasonMetadataKey])
	}
	if got.Metadata[sessionDrainableMetadataKey] != boolMetadata(true) {
		t.Fatalf("drainable = %q, want true", got.Metadata[sessionDrainableMetadataKey])
	}
	if got.Metadata[sessionProviderTerminalErrorMetadataKey] != "model_not_found" {
		t.Fatalf("provider terminal error = %q, want model_not_found", got.Metadata[sessionProviderTerminalErrorMetadataKey])
	}
	if countRecordedEvents(rec, events.SessionCrashed) != 1 {
		t.Fatalf("SessionCrashed events = %d, want exactly 1", countRecordedEvents(rec, events.SessionCrashed))
	}
	markedAt := got.Metadata[sessionProviderTerminalErrorAtKey]

	// Exactly once by key: the mark is durable, so the level-triggered guard no
	// longer holds and a second admission on the same key changes nothing and
	// emits nothing.
	info, response = strandedAuthoritative(t, env, bead.ID)
	handled, _, err = reconcileExactSessionDetectorFamily(
		context.Background(),
		sessionStartAdmission{SessionID: bead.ID, Source: sessionStartAdmissionZombieMark, Version: 2},
		params, info, response, env.clk,
	)
	if err != nil {
		t.Fatalf("re-admitting marked zombie key: %v", err)
	}
	if handled {
		t.Fatal("handler dispatch claimed an already-marked zombie row; the family is not exactly-once by key")
	}
	again, err := env.store.Get(bead.ID)
	if err != nil {
		t.Fatalf("re-read marked session row: %v", err)
	}
	if again.Metadata[sessionProviderTerminalErrorAtKey] != markedAt {
		t.Fatalf("second admission re-marked the row: %q -> %q", markedAt, again.Metadata[sessionProviderTerminalErrorAtKey])
	}
	if countRecordedEvents(rec, events.SessionCrashed) != 1 {
		t.Fatalf("SessionCrashed events after second admission = %d, want still 1", countRecordedEvents(rec, events.SessionCrashed))
	}
}

// TestExactZombieDetectionIgnoresAliveSession is the first negative: an alive
// session raises no D-ZOMBIE condition and the handler dispatch refuses it with
// zero effect, so a healthy fleet pays no mark.
func TestExactZombieDetectionIgnoresAliveSession(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = zombieTestConfig()
	provider := &unattendedStopProvider{Fake: env.sp}
	bead := seedZombieSession(t, env, "model_not_found: gpt-5.3-codex-spark")
	// Alive: the child process is back, so the second liveness bit is true.
	delete(env.sp.Zombies, zombieTestTemplate)

	admitter := &recordingDetectorAdmitter{}
	in := zombieSweepInput(env, provider, env.sessionInfo(bead.ID), env.clk.Now(), admitter.admit)
	result := detectSessionConditions(context.Background(), in)
	routeDetectorConditions(in, &result)
	if got := detectorConditionsFor(result.Conditions, detectorFamilyZombie); len(got) != 0 {
		t.Fatalf("alive session raised D-ZOMBIE conditions %v, want none", got)
	}

	before, err := env.store.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get(session): %v", err)
	}
	rec := events.NewFake()
	info, response := strandedAuthoritative(t, env, bead.ID)
	handled, _, err := reconcileExactSessionDetectorFamily(
		context.Background(),
		sessionStartAdmission{SessionID: bead.ID, Source: sessionStartAdmissionZombieMark, Version: 1},
		zombieHandlerParams(env, provider, rec, result.Liveness), info, response, env.clk,
	)
	if err != nil {
		t.Fatalf("dispatching an alive row: %v", err)
	}
	if handled {
		t.Fatal("handler dispatch claimed an alive row for D-ZOMBIE")
	}
	after, err := env.store.Get(bead.ID)
	if err != nil {
		t.Fatalf("re-read alive session row: %v", err)
	}
	if after.Metadata[sessionHealthStateMetadataKey] != before.Metadata[sessionHealthStateMetadataKey] {
		t.Fatalf("alive row health mutated: %q -> %q",
			before.Metadata[sessionHealthStateMetadataKey], after.Metadata[sessionHealthStateMetadataKey])
	}
	if countRecordedEvents(rec, events.SessionCrashed) != 0 {
		t.Fatalf("SessionCrashed events for an alive row = %d, want 0", countRecordedEvents(rec, events.SessionCrashed))
	}
}

// TestExactZombieIsNotAbsenceFromTheRunningSet is the family-boundary negative
// the design calls out by name: a session merely ABSENT from the names-only
// running set is D-ORPHAN's (or D-WAKE's), never D-ZOMBIE's. A zombie IS in the
// running set — that is why the sweep keys on the two-bit probe rather than on
// set membership, and why the naive "not in running set" condition fails the
// legacy anchor.
func TestExactZombieIsNotAbsenceFromTheRunningSet(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = zombieTestConfig()
	provider := &unattendedStopProvider{Fake: env.sp}
	bead := env.createSessionBead(zombieTestTemplate, zombieTestTemplate)
	env.markSessionActive(&bead)
	// Deliberately NOT started: the row is absent from the running set, so both
	// liveness bits are false.
	env.sp.SetPeekOutput(zombieTestTemplate, "model_not_found: gpt-5.3-codex-spark")

	admitter := &recordingDetectorAdmitter{}
	in := zombieSweepInput(env, provider, env.sessionInfo(bead.ID), env.clk.Now(), admitter.admit)
	result := detectSessionConditions(context.Background(), in)
	routeDetectorConditions(in, &result)
	if got := detectorConditionsFor(result.Conditions, detectorFamilyZombie); len(got) != 0 {
		t.Fatalf("an absent runtime raised D-ZOMBIE conditions %v; absence is D-ORPHAN's, not this family's", got)
	}
	for _, source := range admitter.sources {
		if source == sessionStartAdmissionZombieMark {
			t.Fatal("an absent runtime was enqueued under the D-ZOMBIE admission source")
		}
	}

	info, response := strandedAuthoritative(t, env, bead.ID)
	handled, _, err := reconcileExactSessionDetectorFamily(
		context.Background(),
		sessionStartAdmission{SessionID: bead.ID, Source: sessionStartAdmissionZombieMark, Version: 1},
		zombieHandlerParams(env, provider, events.NewFake(), result.Liveness), info, response, env.clk,
	)
	if err != nil {
		t.Fatalf("dispatching an absent row: %v", err)
	}
	if handled {
		t.Fatal("handler dispatch claimed an absent row for D-ZOMBIE; the family boundary is not held")
	}
}

// TestExactZombieGuardCostsNoProbeWithoutAPublishedView pins the cost shape the
// guard exists to protect. D-ZOMBIE's condition is pure provider I/O, so a guard
// that probed would put one provider call on the ordinary start path for EVERY
// admitted key. Instead it reads the fleet observation the patrol sweep already
// paid for; with nothing published it declines, and it makes no provider call
// doing so.
//
// Declining is the fail-safe direction, not a hole: the condition is
// level-triggered, so the next sweep publishes and the next admission acts.
func TestExactZombieGuardCostsNoProbeWithoutAPublishedView(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = zombieTestConfig()
	provider := &unattendedStopProvider{Fake: env.sp}
	bead := seedZombieSession(t, env, "model_not_found: gpt-5.3-codex-spark")
	info, response := strandedAuthoritative(t, env, bead.ID)

	env.sp.Calls = nil
	params := zombieHandlerParams(env, provider, events.NewFake(), nil)
	params.SessionLiveness = nil
	if _, ok := exactSessionZombieMarkCandidate(params, info, response); ok {
		t.Fatal("the guard claimed a key with no published fleet view; it must decline rather than probe")
	}
	if calls := env.sp.SnapshotCalls(); len(calls) != 0 {
		t.Fatalf("guard made %d provider calls with no published view, want 0: %#v", len(calls), calls)
	}

	// A published view that never probed this row answers the same way.
	params.SessionLiveness = func() map[string]detectorLivenessBits { return map[string]detectorLivenessBits{} }
	if _, ok := exactSessionZombieMarkCandidate(params, info, response); ok {
		t.Fatal("the guard claimed a key the fleet view never probed")
	}
	if calls := env.sp.SnapshotCalls(); len(calls) != 0 {
		t.Fatalf("guard made %d provider calls against an unprobed row, want 0: %#v", len(calls), calls)
	}
}

// TestExactZombieHandlerRefusesARecoveredRow proves the published view is a
// SCHEDULING filter and not authority. It is up to one patrol old, so a row that
// recovered in between must not inherit the dead incarnation's mark: the handler
// makes its own observation first and refuses with zero effect.
func TestExactZombieHandlerRefusesARecoveredRow(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = zombieTestConfig()
	provider := &unattendedStopProvider{Fake: env.sp}
	bead := seedZombieSession(t, env, "model_not_found: gpt-5.3-codex-spark")
	in := zombieSweepInput(env, provider, env.sessionInfo(bead.ID), env.clk.Now(), nil)
	result := detectSessionConditions(context.Background(), in)
	if !exactSessionObservedZombie(zombieHandlerParams(env, provider, events.NewFake(), result.Liveness), bead.ID) {
		t.Fatal("precondition: the sweep should have observed this row as a zombie")
	}

	// The agent process came back between the sweep and the admission.
	delete(env.sp.Zombies, zombieTestTemplate)

	rec := events.NewFake()
	info, response := strandedAuthoritative(t, env, bead.ID)
	handled, _, err := reconcileExactSessionDetectorFamily(
		context.Background(),
		sessionStartAdmission{SessionID: bead.ID, Source: sessionStartAdmissionZombieMark, Version: 1},
		zombieHandlerParams(env, provider, rec, result.Liveness), info, response, env.clk,
	)
	if err != nil {
		t.Fatalf("dispatching a recovered row: %v", err)
	}
	if !handled {
		t.Fatal("the guard should still claim the key; the REFUSAL is the handler's, off its own fresh observation")
	}
	got, err := env.store.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get(session): %v", err)
	}
	if got.Metadata[sessionProviderTerminalErrorMetadataKey] != "" {
		t.Fatalf("a recovered row was marked from a stale fleet view: %q",
			got.Metadata[sessionProviderTerminalErrorMetadataKey])
	}
	if n := countRecordedEvents(rec, events.SessionCrashed); n != 0 {
		t.Fatalf("SessionCrashed events for a recovered row = %d, want 0", n)
	}
}

// TestLegacyZombieCaptureYieldsToTheKeyedOwner pins the family's legacy yield.
// While the keyed controller holds the key, legacy's zombie-capture block
// performs no effect of its own: unlike the stranded and stall seams this arm
// has no observational half to keep, since every step below its
// `running && !alive` test is an effect, and a duplicated SessionCrashed is
// exactly the alarm ops read one-per-incarnation.
//
// The assertion is the EVENT, not the health cluster, and that distinction is a
// recorded finding. legacy's exit-classification lane (checkRateLimitStability,
// session_reconciler.go's desired-branch call below the zombie block) is a
// SIBLING writer of the same terminal-error cluster for the same dead row, and
// it does not yield — it also owns rate-limit quarantine, which nothing here
// replaces. The two writes are content-identical because both classify the same
// peek, so the row converges either way; only the crash event is uniquely this
// arm's.
func TestLegacyZombieCaptureYieldsToTheKeyedOwner(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = zombieTestConfig()
	rec := events.NewFake()
	env.rec = rec
	env.desiredState[zombieTestTemplate] = TemplateParams{
		Command:      "test-cmd",
		SessionName:  zombieTestTemplate,
		TemplateName: zombieTestTemplate,
		Hints:        agent.StartupHints{ProcessNames: []string{"test-cmd"}},
	}
	bead := seedZombieSession(t, env, "model_not_found: gpt-5.3-codex-spark")

	env.startOptions = append(env.startOptions,
		withLegacyZombieMarkExclusion(func(sessionpkg.Info) bool { return true }))
	env.reconcile([]beads.Bead{bead})

	if n := countRecordedEvents(rec, events.SessionCrashed); n != 0 {
		t.Fatalf("legacy emitted %d SessionCrashed events for a keyed-owned zombie, want 0", n)
	}

	// Control: with no keyed owner the very same fixture DOES fire the crash
	// event, which is what proves the yield above did the withholding.
	env2 := newReconcilerTestEnv()
	env2.cfg = zombieTestConfig()
	controlRec := events.NewFake()
	env2.rec = controlRec
	env2.desiredState[zombieTestTemplate] = env.desiredState[zombieTestTemplate]
	control := seedZombieSession(t, env2, "model_not_found: gpt-5.3-codex-spark")
	env2.reconcile([]beads.Bead{control})
	if n := countRecordedEvents(controlRec, events.SessionCrashed); n != 1 {
		t.Fatalf("control SessionCrashed events = %d, want 1; the yield assertion proves nothing", n)
	}
	stored, err := env2.store.Get(control.ID)
	if err != nil {
		t.Fatalf("Get(control session): %v", err)
	}
	if stored.Metadata[sessionProviderTerminalErrorMetadataKey] != "model_not_found" {
		t.Fatalf("control provider terminal error = %q, want model_not_found", stored.Metadata[sessionProviderTerminalErrorMetadataKey])
	}
}

// TestExactZombieMarkRefusesUnclassifiableScrollback proves the handler applies
// no mark when the scrollback carries no recognizable terminal-provider error:
// legacy stamps the health cluster only from a classified reason, and the keyed
// arm must not invent one. The SessionCrashed forensic event still fires,
// exactly as legacy fires it outside the reason check.
func TestExactZombieMarkRefusesUnclassifiableScrollback(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = zombieTestConfig()
	provider := &unattendedStopProvider{Fake: env.sp}
	bead := seedZombieSession(t, env, "just some ordinary scrollback")

	rec := events.NewFake()
	in := zombieSweepInput(env, provider, env.sessionInfo(bead.ID), env.clk.Now(), nil)
	result := detectSessionConditions(context.Background(), in)
	info, response := strandedAuthoritative(t, env, bead.ID)
	handled, _, err := reconcileExactSessionDetectorFamily(
		context.Background(),
		sessionStartAdmission{SessionID: bead.ID, Source: sessionStartAdmissionZombieMark, Version: 1},
		zombieHandlerParams(env, provider, rec, result.Liveness), info, response, env.clk,
	)
	if err != nil {
		t.Fatalf("keyed zombie mark on unclassifiable scrollback: %v", err)
	}
	if !handled {
		t.Fatal("handler dispatch did not claim a running-and-not-alive row")
	}
	got, err := env.store.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get(session): %v", err)
	}
	if got.Metadata[sessionProviderTerminalErrorMetadataKey] != "" {
		t.Fatalf("provider terminal error = %q, want empty (nothing to classify)",
			got.Metadata[sessionProviderTerminalErrorMetadataKey])
	}
	if got.Metadata[sessionHealthStateMetadataKey] == "unhealthy" {
		t.Fatal("an unclassifiable zombie was marked unhealthy; the reason is what licenses the mark")
	}
	if countRecordedEvents(rec, events.SessionCrashed) != 1 {
		t.Fatalf("SessionCrashed events = %d, want exactly 1 (legacy fires it outside the reason check)",
			countRecordedEvents(rec, events.SessionCrashed))
	}
}

// zombieUnarmedTraceRecords drives the keyed zombie handler against a city whose
// trace detail is UNARMED — the shipping default, and the only configuration a
// released opt-in reconciler is observed in — and returns everything that
// survived to the trace store, plus the session it acted on.
//
// peek is the fixture's scrollback: a classifiable terminal error licenses the
// mark and the handler APPLIES an effect; anything else leaves the row untouched
// and the handler only refuses.
func zombieUnarmedTraceRecords(t *testing.T, peek string) ([]SessionReconcilerTraceRecord, string) {
	t.Helper()
	env := newReconcilerTestEnv()
	env.cfg = zombieTestConfig()
	provider := &unattendedStopProvider{Fake: env.sp}
	bead := seedZombieSession(t, env, peek)

	in := zombieSweepInput(env, provider, env.sessionInfo(bead.ID), env.clk.Now(), nil)
	result := detectSessionConditions(context.Background(), in)

	cityPath := t.TempDir()
	trace := newSessionReconcilerTraceManager(cityPath, "test-city", io.Discard)
	t.Cleanup(func() { _ = trace.Close() })

	params := zombieHandlerParams(env, provider, events.NewFake(), result.Liveness)
	params.CityPath = cityPath
	params.Trace = trace

	info, response := strandedAuthoritative(t, env, bead.ID)
	handled, _, err := reconcileExactSessionDetectorFamily(
		context.Background(),
		sessionStartAdmission{SessionID: bead.ID, Source: sessionStartAdmissionZombieMark, Version: 1},
		params, info, response, env.clk,
	)
	if err != nil {
		t.Fatalf("keyed zombie mark on an unarmed city: %v", err)
	}
	if !handled {
		t.Fatal("handler dispatch did not claim the zombie key")
	}

	records, readErr := ReadTraceRecords(traceCityRuntimeDir(cityPath), TraceFilter{})
	if readErr != nil {
		t.Fatalf("read unarmed trace store: %v", readErr)
	}
	return records, bead.ID
}

// TestKeyedAppliedEffectPersistsOnAnUnarmedFleet is ga-f7v2ft.161's RED. An
// APPLIED effect record is the load-bearing proof that the opt-in keyed engine
// acted on a row — the answer to "how do I know the opt-in is working?" — and it
// used to be detail-gated like every other thing a handler emits, so a fleet
// that armed nothing threw the proof away as it was written. The 2026-08-24 soak
// census measured the ratio the gate exists for: 59,486 condition records in the
// window against a handful of applied effects, 59,447 of them discarded.
//
// So the APPLIED record, and only the APPLIED record, moves to the always-on
// tier — the treatment pool_allocation.materialize already gets from
// RecordControllerOperation. The control is the same handler on the same fixture
// with nothing to classify: it applies nothing, stays gated, and leaves no
// record behind. Lifting the effect must not lift the volume with it.
func TestKeyedAppliedEffectPersistsOnAnUnarmedFleet(t *testing.T) {
	records, sessionID := zombieUnarmedTraceRecords(t, "model_not_found: gpt-5.3-codex-spark")

	var applied []SessionReconcilerTraceRecord
	for _, record := range records {
		if record.SiteCode == TraceSiteReconcilerTerminalProviderError {
			applied = append(applied, record)
		}
	}
	if len(applied) != 1 {
		t.Fatalf("keyed applied-effect records on an unarmed city = %d, want exactly 1 (records=%#v)", len(applied), records)
	}
	effect := applied[0]
	if effect.Fields["effect_owner"] != detectorKeyedEffectOwner || effect.Fields["effect_applied"] != true {
		t.Fatalf("applied effect = %#v, want the keyed ownership stamp and an honest applied flag", effect)
	}
	if effect.TraceMode != TraceModeBaseline || effect.TraceSource != TraceSourceAlwaysOn {
		t.Fatalf("applied effect tier = %q/%q, want baseline/always_on", effect.TraceMode, effect.TraceSource)
	}
	if effect.SessionBeadID != sessionID || effect.Template != zombieTestTemplate {
		t.Fatalf("applied effect identity = %q/%q, want %q/%q: an always-on record still has to join per-session",
			effect.SessionBeadID, effect.Template, sessionID, zombieTestTemplate)
	}

	refusals, _ := zombieUnarmedTraceRecords(t, "just some ordinary scrollback")
	for _, record := range refusals {
		if record.SiteCode == TraceSiteReconcilerTerminalProviderError {
			t.Fatalf("an unapplied keyed refusal escaped the detail gate on an unarmed city: %#v", record)
		}
	}
}
