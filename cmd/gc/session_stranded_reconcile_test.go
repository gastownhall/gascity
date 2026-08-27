package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/rollout"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// strandedTestTemplate is the single pool template every fixture in this file
// seeds: agent name, session name, routed-to value and expected fallback route
// are all one identity, which is what makes the run_target assertion in the
// re-point residual test meaningful.
const strandedTestTemplate = "worker"

// seedStrandedPoolSlot writes the schema-v59 stranded-pool-slot shape the
// legacy anchor cluster seeds (strandedRepairReconcileEnv in
// session_reconciler_test.go, strandedRepairFixture in
// dead_assignee_repair_test.go): a pool-managed member whose runtime is gone,
// whose bead sits in a terminal sleep state, and which still holds an
// in_progress work bead as assignee.
//
// markerAge picks which side of strandedRepairConfirmGrace the confirmation
// marker lands on, so the same fixture serves the repair arm and the
// inside-the-window negative. workMeta seeds the work bead's routing metadata:
// an already-routed bead keeps its route, an unrouted one takes the retired
// member's fallback run_target on release.
func seedStrandedPoolSlot(
	t *testing.T,
	env *reconcilerTestEnv,
	markerAge time.Duration,
	workMeta map[string]string,
) (beads.Bead, beads.Bead) {
	t.Helper()
	bead := env.createSessionBead(strandedTestTemplate, strandedTestTemplate)
	meta := map[string]string{
		poolManagedMetadataKey: boolMetadata(true),
		"state":                "asleep",
		"sleep_reason":         "idle",
	}
	if markerAge > 0 {
		meta[strandedEventEmittedKey] = env.clk.Now().UTC().Add(-markerAge).Format(time.RFC3339)
	}
	env.setSessionMetadata(&bead, meta)

	work, err := env.store.Create(beads.Bead{
		Title:    "stranded implementation",
		Type:     "task",
		Status:   "open",
		Assignee: bead.ID,
		Metadata: workMeta,
	})
	if err != nil {
		t.Fatalf("create work bead: %v", err)
	}
	inProgress := "in_progress"
	if err := env.store.Update(work.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatalf("set work in_progress: %v", err)
	}
	if work, err = env.store.Get(work.ID); err != nil {
		t.Fatalf("re-read work bead: %v", err)
	}
	return bead, work
}

// strandedSweepInput builds the minimum sweep input that reaches D-STRANDED for
// one row, with admit as the routing seam's enqueue hook. The row is DESIRED so
// family precedence does not hand it to D-ORPHAN before the wake/sleep families
// are consulted — legacy's own forward pass early-continues on an undesired row
// for the same reason.
func strandedSweepInput(
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

func strandedTestConfig() *config.City {
	return &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              strandedTestTemplate,
			StartCommand:      "true",
			MinActiveSessions: intPtr(0),
			MaxActiveSessions: intPtr(4),
		}},
	}
}

// strandedHandlerParams builds the exact-key handler's parameters for a direct
// (controller-free) call, matching the sibling families' test shape.
func strandedHandlerParams(env *reconcilerTestEnv, provider runtime.Provider) exactSessionStartParams {
	return exactSessionStartParams{
		Generation:  1,
		CityPath:    "test-city",
		CityName:    "test-city",
		Config:      env.cfg,
		Provider:    provider,
		Store:       env.store,
		Recorder:    events.Discard,
		Stderr:      &env.stderr,
		RolloutMode: rollout.Require,
	}
}

func strandedAuthoritative(t *testing.T, env *reconcilerTestEnv, id string) (sessionpkg.Info, sessionpkg.PersistedResponse) {
	t.Helper()
	info, response, err := getAuthoritativeSessionStartPersistedRecord(env.store, id)
	if err != nil {
		t.Fatalf("authoritative read of %q: %v", id, err)
	}
	return info, response
}

// TestExactStrandedPoolSlotRepairsOnceByKey is WD.14's primary RED: a seeded
// v59 pool slot whose confirmation window has elapsed is handed to the
// session-start controller under the D-STRANDED admission source, repaired
// exactly once by exact key through the existing repair helpers — the assigned
// work is unassigned and reopened, the session bead is closed — and left
// terminal so a second admission on the same key is a zero-effect no-op.
func TestExactStrandedPoolSlotRepairsOnceByKey(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = strandedTestConfig()
	provider := &unattendedStopProvider{Fake: env.sp}
	bead, work := seedStrandedPoolSlot(t, env, strandedRepairConfirmGrace+time.Minute,
		map[string]string{beadmeta.RoutedToMetadataKey: strandedTestTemplate})

	cr := newExactDeadlineRuntime(t, env, provider, nil, nil, events.NewFake())
	admit := cr.detectorAdmitFunc()
	if admit == nil {
		t.Fatal("detectorAdmitFunc() = nil under keyed ownership; the sweep has no enqueue seam")
	}

	// The sweep is the producer of this key: it must classify the row into
	// D-STRANDED and route it under the family's own admission source.
	admitter := &recordingDetectorAdmitter{}
	in := strandedSweepInput(env, provider, env.sessionInfo(bead.ID), env.clk.Now(), admitter.admit)
	result := detectSessionConditions(context.Background(), in)
	routeDetectorConditions(in, &result)
	if len(admitter.keys) != 1 || admitter.keys[0] != bead.ID {
		t.Fatalf("sweep enqueued %v, want exactly the stranded key %q", admitter.keys, bead.ID)
	}
	if admitter.sources[0] != sessionStartAdmissionStrandedRepair {
		t.Fatalf("sweep enqueued under source %q, want %q", admitter.sources[0], sessionStartAdmissionStrandedRepair)
	}

	if outcome, err := admit(bead.ID, sessionStartAdmissionStrandedRepair); err != nil || outcome == sessionStartAdmissionOverflow {
		t.Fatalf("admitting stranded key: outcome=%q err=%v", outcome, err)
	}
	awaitCond(t, func() bool {
		stored, err := env.store.Get(bead.ID)
		return err == nil && stored.Status == "closed"
	}, "keyed stranded pool-slot repair")

	gotWork, err := env.store.Get(work.ID)
	if err != nil {
		t.Fatalf("read repaired work: %v", err)
	}
	if gotWork.Status != "open" {
		t.Fatalf("work status = %q, want open (reopened by repair)", gotWork.Status)
	}
	if gotWork.Assignee != "" {
		t.Fatalf("work assignee = %q, want empty (unassigned by repair)", gotWork.Assignee)
	}
	stored, err := env.store.Get(bead.ID)
	if err != nil {
		t.Fatalf("read closed session row: %v", err)
	}
	// NOTE (recorded as a §3 delta): the repair's close reason is
	// strandedRepairCloseReason, NOT the preserved sleep_reason. §3's D-STRANDED
	// prose folded the sibling clean-close arm's sleep_reason preservation into
	// this arm; the reused helper has always stamped its own forensic reason,
	// and "reuse the repair helpers unchanged" wins.
	if want := sessionpkg.CanonicalCloseReason(strandedRepairCloseReason); stored.Metadata["close_reason"] != want {
		t.Fatalf("close_reason = %q, want %q", stored.Metadata["close_reason"], want)
	}
	closedAt := stored.Metadata["closed_at"]

	// Exactly once by key: the level-triggered condition no longer holds, so a
	// second admission on the same key changes nothing.
	if outcome, err := admit(bead.ID, sessionStartAdmissionStrandedRepair); err != nil || outcome == sessionStartAdmissionOverflow {
		t.Fatalf("re-admitting repaired key: outcome=%q err=%v", outcome, err)
	}
	awaitCond(t, func() bool { return !cr.sessionStartController.ownsStrandedRepair(bead.ID) }, "stranded admission drain")
	again, err := env.store.Get(bead.ID)
	if err != nil {
		t.Fatalf("re-read closed session row: %v", err)
	}
	if again.Metadata["closed_at"] != closedAt {
		t.Fatalf("second admission repaired the row again: closed_at %q -> %q", closedAt, again.Metadata["closed_at"])
	}
}

// TestExactStrandedRepairHealsAckedMemberRepointResidual is the round-5 AC
// addendum's producer, end to end. Legacy's buildDesiredState re-points a
// trigger bead onto a pool member its per-tick snapshot still shows as active;
// the member had already acknowledged its drain, so it finishes draining and
// stops with the newly bound work still assigned to it (ga-f7v2ft.131).
// poolTriggerRepointSuperseded narrowed that window to the ack → stop-pending
// stamp and declared the pre-stamp remainder irreducible there — so the
// RESIDUAL heal is this family's.
//
// The heal must also keep ga-f7v2ft.117's premise intact: the reopened work has
// to become re-detectable by the census, which means landing back in the routed
// ready population — open, unassigned, and carrying a run target — not merely
// losing its assignee.
func TestExactStrandedRepairHealsAckedMemberRepointResidual(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = strandedTestConfig()
	provider := &unattendedStopProvider{Fake: env.sp}

	// The residual shape: the work carries NO route of its own — it reached the
	// member through the trigger binding, not through a routed-work read — so
	// only the retired member's fallback route can put it back in the census.
	bead, work := seedStrandedPoolSlot(t, env, strandedRepairConfirmGrace+time.Minute, nil)
	// The legacy re-point: the trigger binding names the newly bound work, and
	// the member reached its terminal state through an ACKNOWLEDGED drain.
	env.setSessionMetadata(&bead, map[string]string{
		beadmeta.TriggerBeadIDMetadataKey: work.ID,
		"state":                           "drained",
		"sleep_reason":                    "drained",
	})

	cr := newExactDeadlineRuntime(t, env, provider, nil, nil, events.NewFake())
	admit := cr.detectorAdmitFunc()
	if admit == nil {
		t.Fatal("detectorAdmitFunc() = nil under keyed ownership")
	}

	admitter := &recordingDetectorAdmitter{}
	in := strandedSweepInput(env, provider, env.sessionInfo(bead.ID), env.clk.Now(), admitter.admit)
	result := detectSessionConditions(context.Background(), in)
	routeDetectorConditions(in, &result)
	if len(admitter.keys) != 1 || admitter.sources[0] != sessionStartAdmissionStrandedRepair {
		t.Fatalf("the acked-member re-point residual was not recognized: keys=%v sources=%v", admitter.keys, admitter.sources)
	}

	if outcome, err := admit(bead.ID, sessionStartAdmissionStrandedRepair); err != nil || outcome == sessionStartAdmissionOverflow {
		t.Fatalf("admitting residual key: outcome=%q err=%v", outcome, err)
	}
	awaitCond(t, func() bool {
		stored, err := env.store.Get(bead.ID)
		return err == nil && stored.Status == "closed"
	}, "keyed heal of the acked-member re-point residual")

	gotWork, err := env.store.Get(work.ID)
	if err != nil {
		t.Fatalf("read healed work: %v", err)
	}
	if gotWork.Status != "open" || gotWork.Assignee != "" {
		t.Fatalf("healed work status=%q assignee=%q, want open/unassigned", gotWork.Status, gotWork.Assignee)
	}
	// .117's census re-detection premise: the work must land somewhere the
	// routed-work view can see it again. An unrouted release would drop it out
	// of the census entirely and the strand would simply move.
	if got := gotWork.Metadata[beadmeta.RunTargetMetadataKey]; got != strandedTestTemplate {
		t.Fatalf("healed work run_target = %q, want the retired member's fallback route %q", got, strandedTestTemplate)
	}
}

// TestExactStrandedRepairRefusesInsideConfirmationWindow is the confirmation
// window negative: a slot whose CURRENT stranding episode is younger than
// strandedRepairConfirmGrace is untouched — the detector raises the deferred
// arm and never enqueues, and the handler refuses with zero writes and zero
// provider calls even when some other admission carries the key in.
func TestExactStrandedRepairRefusesInsideConfirmationWindow(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = strandedTestConfig()
	provider := &unattendedStopProvider{Fake: env.sp}
	bead, work := seedStrandedPoolSlot(t, env, time.Second,
		map[string]string{beadmeta.RoutedToMetadataKey: strandedTestTemplate})

	admitter := &recordingDetectorAdmitter{}
	in := strandedSweepInput(env, provider, env.sessionInfo(bead.ID), env.clk.Now(), admitter.admit)
	result := detectSessionConditions(context.Background(), in)
	routeDetectorConditions(in, &result)
	if len(admitter.keys) != 0 {
		t.Fatalf("sweep enqueued %v inside the confirmation window; want zero enqueues", admitter.keys)
	}
	deferred := 0
	for _, cond := range result.Conditions {
		if cond.Family != detectorFamilyStranded {
			continue
		}
		deferred++
		if cond.Outcome != TraceOutcomeDeferredConfirm {
			t.Fatalf("in-window stranded condition outcome = %q, want %q", cond.Outcome, TraceOutcomeDeferredConfirm)
		}
	}
	if deferred != 1 {
		t.Fatalf("stranded conditions = %d, want exactly 1 deferred record for the parity join", deferred)
	}

	before := detectorStoreFingerprint(t, env.store)
	callsBefore := len(env.sp.SnapshotCalls())
	params := strandedHandlerParams(env, provider)
	info, response := strandedAuthoritative(t, env, bead.ID)
	if exactSessionStrandedRepairCandidate(params, info, response, env.clk) {
		t.Fatal("the seam guard claimed a row inside its confirmation window")
	}
	if _, err := reconcileExactSessionStrandedRepair(context.Background(),
		sessionStartAdmission{SessionID: bead.ID, Source: sessionStartAdmissionStrandedRepair},
		params, info, response, env.clk); err != nil {
		t.Fatalf("in-window handler must refuse silently, got %v", err)
	}
	if after := detectorStoreFingerprint(t, env.store); after != before {
		t.Fatal("in-window refusal wrote to the store")
	}
	if got := len(env.sp.SnapshotCalls()); got != callsBefore {
		t.Fatalf("in-window refusal made %d provider call(s)", got-callsBefore)
	}
	gotWork, _ := env.store.Get(work.ID)
	if gotWork.Status != "in_progress" || gotWork.Assignee != bead.ID {
		t.Fatalf("work must stay claimed, got status=%q assignee=%q", gotWork.Status, gotWork.Assignee)
	}
}

// TestExactStrandedRepairRefusesLiveMember covers both healthy-slot negatives.
// A member that is merely SLOW — its runtime is still up, it just has not
// finished the work — must never have its claim cleared: the detector skips it
// on the liveness bit, and the handler refuses on its own fresh-liveness
// observation even when a marker from an earlier episode survives on the row.
func TestExactStrandedRepairRefusesLiveMember(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = strandedTestConfig()
	provider := &unattendedStopProvider{Fake: env.sp}
	bead, work := seedStrandedPoolSlot(t, env, strandedRepairConfirmGrace+time.Minute,
		map[string]string{beadmeta.RoutedToMetadataKey: strandedTestTemplate})
	// Slow, not stopped: the bead is awake and the runtime is up.
	env.setSessionMetadata(&bead, map[string]string{
		"state":        "active",
		"sleep_reason": "",
		"last_woke_at": env.clk.Now().UTC().Format(time.RFC3339),
	})
	if err := provider.Start(t.Context(), strandedTestTemplate, runtime.Config{}); err != nil {
		t.Fatalf("start runtime: %v", err)
	}

	admitter := &recordingDetectorAdmitter{}
	in := strandedSweepInput(env, provider, env.sessionInfo(bead.ID), env.clk.Now(), admitter.admit)
	result := detectSessionConditions(context.Background(), in)
	routeDetectorConditions(in, &result)
	for i, source := range admitter.sources {
		if source == sessionStartAdmissionStrandedRepair {
			t.Fatalf("sweep enqueued a live member for stranded repair: key=%q", admitter.keys[i])
		}
	}
	for _, cond := range result.Conditions {
		if cond.Family == detectorFamilyStranded {
			t.Fatalf("a live member raised a D-STRANDED condition: %+v", cond)
		}
	}

	before := detectorStoreFingerprint(t, env.store)
	params := strandedHandlerParams(env, provider)
	info, response := strandedAuthoritative(t, env, bead.ID)
	if exactSessionStrandedRepairCandidate(params, info, response, env.clk) {
		t.Fatal("the seam guard claimed a live member's slot")
	}
	if _, err := reconcileExactSessionStrandedRepair(context.Background(),
		sessionStartAdmission{SessionID: bead.ID, Source: sessionStartAdmissionStrandedRepair},
		params, info, response, env.clk); err != nil {
		t.Fatalf("live-member handler must refuse silently, got %v", err)
	}
	if after := detectorStoreFingerprint(t, env.store); after != before {
		t.Fatal("live-member refusal wrote to the store")
	}
	gotWork, _ := env.store.Get(work.ID)
	if gotWork.Status != "in_progress" || gotWork.Assignee != bead.ID {
		t.Fatalf("a slow member's work must stay claimed, got status=%q assignee=%q", gotWork.Status, gotWork.Assignee)
	}
}

// TestExactStrandedRepairRefusesWhenTheRuntimeIsStillUp is the OTHER slow-member
// shape, and the one only the handler can catch. The bead says asleep and its
// confirmation window has elapsed, so every durable rung passes and the sweep
// enqueues — the sweep cannot know better, because it probes liveness for
// bead-awake rows only. Legacy answers this from its fleet-wide !alive bit; the
// keyed arm pays a fresh-liveness observation per key instead, and refuses with
// zero effect rather than clear the claim of a worker that is still running.
func TestExactStrandedRepairRefusesWhenTheRuntimeIsStillUp(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = strandedTestConfig()
	provider := &unattendedStopProvider{Fake: env.sp}
	bead, work := seedStrandedPoolSlot(t, env, strandedRepairConfirmGrace+time.Minute,
		map[string]string{beadmeta.RoutedToMetadataKey: strandedTestTemplate})
	if err := provider.Start(t.Context(), strandedTestTemplate, runtime.Config{}); err != nil {
		t.Fatalf("start runtime: %v", err)
	}

	// The sweep cannot see this: an asleep row is never liveness-probed, so the
	// key IS enqueued and the handler is the only guard left.
	admitter := &recordingDetectorAdmitter{}
	in := strandedSweepInput(env, provider, env.sessionInfo(bead.ID), env.clk.Now(), admitter.admit)
	result := detectSessionConditions(context.Background(), in)
	routeDetectorConditions(in, &result)
	if len(admitter.keys) != 1 || admitter.sources[0] != sessionStartAdmissionStrandedRepair {
		t.Fatalf("expected the sweep to enqueue this row blind: keys=%v sources=%v", admitter.keys, admitter.sources)
	}

	before := detectorStoreFingerprint(t, env.store)
	params := strandedHandlerParams(env, provider)
	info, response := strandedAuthoritative(t, env, bead.ID)
	if !exactSessionStrandedRepairCandidate(params, info, response, env.clk) {
		t.Fatal("the durable rungs must all pass here; otherwise this test does not reach the liveness rung")
	}
	if _, err := reconcileExactSessionStrandedRepair(context.Background(),
		sessionStartAdmission{SessionID: bead.ID, Source: sessionStartAdmissionStrandedRepair},
		params, info, response, env.clk); err != nil {
		t.Fatalf("live-runtime handler must refuse silently, got %v", err)
	}
	if after := detectorStoreFingerprint(t, env.store); after != before {
		t.Fatal("the handler cleared a live worker's claim on a row whose durable state lied")
	}
	gotWork, _ := env.store.Get(work.ID)
	if gotWork.Status != "in_progress" || gotWork.Assignee != bead.ID {
		t.Fatalf("a running member's work must stay claimed, got status=%q assignee=%q", gotWork.Status, gotWork.Assignee)
	}
	stored, _ := env.store.Get(bead.ID)
	if stored.Status == "closed" {
		t.Fatal("the handler closed a slot whose runtime is still up")
	}
}

// seedStrandedLiveRuntimeUnderIncompleteScan is the ga-bxa8r fixture: the
// slow-member shape only the handler can catch (an asleep row whose runtime is
// still up), observed through a provider whose alive-target completeness is
// structurally unlicensable.
func seedStrandedLiveRuntimeUnderIncompleteScan(
	t *testing.T,
	env *reconcilerTestEnv,
) (*aliveIncompleteStopProvider, beads.Bead, beads.Bead, exactSessionStartParams) {
	t.Helper()
	env.cfg = strandedTestConfig()
	provider := &aliveIncompleteStopProvider{unattendedStopProvider: &unattendedStopProvider{Fake: env.sp}}
	bead, work := seedStrandedPoolSlot(t, env, strandedRepairConfirmGrace+time.Minute,
		map[string]string{beadmeta.RoutedToMetadataKey: strandedTestTemplate})
	if err := provider.Start(t.Context(), strandedTestTemplate, runtime.Config{}); err != nil {
		t.Fatalf("start runtime: %v", err)
	}
	return provider, bead, work, strandedHandlerParams(env, provider)
}

// TestExactStrandedRepairKeepsLiveMemberOpenOnIncompleteScan is ga-bxa8r's
// D-STRANDED specimen, and the 4714a8a00d shape exactly: the POSITIVE arm here
// is the non-destructive keep-open refusal that protects a slow worker's claim,
// while the destructive repair lives on the negative arm. A live member
// withholds the tmux-absence license, so the unconditional Complete gate parked
// this row instead of recording its refusal — and under daemon.session_reconciler
// = auto that park is a permanent legacy-fallback treadmill on exactly the rows
// the keyed arm exists to protect.
//
// A positive observation is decisive: the member is running, its claim stays,
// and no repair is even considered.
func TestExactStrandedRepairKeepsLiveMemberOpenOnIncompleteScan(t *testing.T) {
	env := newReconcilerTestEnv()
	_, bead, work, params := seedStrandedLiveRuntimeUnderIncompleteScan(t, env)
	info, response := strandedAuthoritative(t, env, bead.ID)
	if !exactSessionStrandedRepairCandidate(params, info, response, env.clk) {
		t.Fatal("the durable rungs must all pass here; otherwise this test never reaches the liveness rung")
	}

	before := detectorStoreFingerprint(t, env.store)
	if _, err := reconcileExactSessionStrandedRepair(context.Background(),
		sessionStartAdmission{SessionID: bead.ID, Source: sessionStartAdmissionStrandedRepair},
		params, info, response, env.clk); err != nil {
		t.Fatalf("a live member's incomplete scan parked the stranded refusal: %v", err)
	}
	if after := detectorStoreFingerprint(t, env.store); after != before {
		t.Fatal("the keep-open refusal wrote to the store")
	}
	gotWork, _ := env.store.Get(work.ID)
	if gotWork.Status != "in_progress" || gotWork.Assignee != bead.ID {
		t.Fatalf("a running member's work must stay claimed, got status=%q assignee=%q", gotWork.Status, gotWork.Assignee)
	}
	if stored, _ := env.store.Get(bead.ID); stored.Status == "closed" {
		t.Fatal("the handler closed a slot whose runtime is still up")
	}
}

// TestExactStrandedRepairUnprovenAbsenceStillParks is the fail-closed control,
// and it fails DIFFERENTLY: the same fixture with the runtime gone and the scan
// still unlicensable. Here the destructive repair IS the next step, dead cannot
// be told apart from unobserved, and the handler must refuse rather than clear a
// claim a live member may still hold behind an unreadable probe.
func TestExactStrandedRepairUnprovenAbsenceStillParks(t *testing.T) {
	env := newReconcilerTestEnv()
	provider, bead, work, params := seedStrandedLiveRuntimeUnderIncompleteScan(t, env)
	provider.alwaysIncomplete = true
	if err := provider.Stop(strandedTestTemplate); err != nil {
		t.Fatalf("stop runtime: %v", err)
	}
	info, response := strandedAuthoritative(t, env, bead.ID)

	before := detectorStoreFingerprint(t, env.store)
	_, err := reconcileExactSessionStrandedRepair(context.Background(),
		sessionStartAdmission{SessionID: bead.ID, Source: sessionStartAdmissionStrandedRepair},
		params, info, response, env.clk)
	if err == nil || !strings.Contains(err.Error(), "liveness observation is incomplete") {
		t.Fatalf("err = %v, want the incomplete-liveness park for a negative unproven observation", err)
	}
	if after := detectorStoreFingerprint(t, env.store); after != before {
		t.Fatal("an unproven absence repaired the slot")
	}
	gotWork, _ := env.store.Get(work.ID)
	if gotWork.Status != "in_progress" || gotWork.Assignee != bead.ID {
		t.Fatalf("an unproven absence released the work, got status=%q assignee=%q", gotWork.Status, gotWork.Assignee)
	}
}

// TestDetectStrandedSuppressedOnPartialStore is the global-guard negative: on a
// degraded store view the family raises no condition at all, so no record can
// carry a repair the handler would then have to decline.
func TestDetectStrandedSuppressedOnPartialStore(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = strandedTestConfig()
	provider := &unattendedStopProvider{Fake: env.sp}
	bead, _ := seedStrandedPoolSlot(t, env, strandedRepairConfirmGrace+time.Minute,
		map[string]string{beadmeta.RoutedToMetadataKey: strandedTestTemplate})

	admitter := &recordingDetectorAdmitter{}
	in := strandedSweepInput(env, provider, env.sessionInfo(bead.ID), env.clk.Now(), admitter.admit)
	in.StoreQueryPartial = true
	result := detectSessionConditions(context.Background(), in)
	routeDetectorConditions(in, &result)
	for _, cond := range result.Conditions {
		if cond.Family == detectorFamilyStranded {
			t.Fatalf("a partial store view still emitted a D-STRANDED record: %+v", cond)
		}
	}
	if len(admitter.keys) != 0 {
		t.Fatalf("partial store view enqueued %v", admitter.keys)
	}
	if result.SuppressedByPartialStore == 0 {
		t.Fatal("suppression was not counted; the family must fail closed before the condition exists")
	}
}

// TestExactStrandedRepairClosesBeadWhenWorktreeUnsafe pins the ordering the AC
// asks for: pruning is the LAST step and never the blocking one. A worker_dir
// the safety gates refuse to remove is left intact while the work is still
// reopened and the session bead still closes.
func TestExactStrandedRepairClosesBeadWhenWorktreeUnsafe(t *testing.T) {
	fx := newPruneFixture(t)
	unsafe := &fakeGitProbe{isRepo: true, hasUncommitted: true}
	fx.setProbe(fx.workerDir, unsafe)
	fx.cfg.Agents = []config.Agent{{Name: strandedTestTemplate, StartCommand: "true", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(4)}}

	env := newReconcilerTestEnv()
	env.cfg = fx.cfg
	provider := &unattendedStopProvider{Fake: env.sp}
	bead, work := seedStrandedPoolSlot(t, env, strandedRepairConfirmGrace+time.Minute,
		map[string]string{beadmeta.RoutedToMetadataKey: strandedTestTemplate})
	env.setSessionMetadata(&bead, map[string]string{"worker_dir": fx.workerDir})

	params := strandedHandlerParams(env, provider)
	params.CityPath = fx.cityPath
	info, response := strandedAuthoritative(t, env, bead.ID)
	if !exactSessionStrandedRepairCandidate(params, info, response, env.clk) {
		t.Fatal("the seam guard rejected a confirmed stranded slot")
	}
	if _, err := reconcileExactSessionStrandedRepair(context.Background(),
		sessionStartAdmission{SessionID: bead.ID, Source: sessionStartAdmissionStrandedRepair},
		params, info, response, env.clk); err != nil {
		t.Fatalf("repair with an unsafe worktree: %v", err)
	}

	stored, _ := env.store.Get(bead.ID)
	if stored.Status != "closed" {
		t.Fatalf("session status = %q, want closed — pruning must never block the close", stored.Status)
	}
	gotWork, _ := env.store.Get(work.ID)
	if gotWork.Status != "open" || gotWork.Assignee != "" {
		t.Fatalf("work status=%q assignee=%q, want open/unassigned", gotWork.Status, gotWork.Assignee)
	}
	if unsafe.removeInvoked {
		t.Fatal("an unsafe worker_dir was pruned; the safety gates must leave it intact")
	}
	assertWorktreeStaleMarker(t, fx.workerDir, "", "uncommitted-work")
}

// TestLegacyStrandedRepairYieldsToKeyedOwnedRow states this slice's ownership
// decision and proves it: the legacy dead-pool repair arm stands down for any
// row the keyed controller currently holds an admission for. The diagnostic
// above the yield keeps firing, because the marker it stamps IS this family's
// entry condition — only the destructive repair yields.
func TestLegacyStrandedRepairYieldsToKeyedOwnedRow(t *testing.T) {
	env, session, work, _ := strandedRepairReconcileEnv(t)
	runStrandedReconcileTick(t, env, []beads.Bead{session})
	env.clk.Time = env.clk.Time.Add(strandedRepairConfirmGrace + time.Minute)
	confirmed, _ := env.store.Get(session.ID)

	runStrandedReconcileTick(t, env, []beads.Bead{confirmed},
		withLegacyStrandedRepairExclusion(func(info sessionpkg.Info) bool { return info.ID == session.ID }))

	gotWork, _ := env.store.Get(work.ID)
	if gotWork.Status != "in_progress" || gotWork.Assignee != session.ID {
		t.Fatalf("legacy repaired work the keyed D-STRANDED handler owns: status=%q assignee=%q", gotWork.Status, gotWork.Assignee)
	}
	gotSession, _ := env.store.Get(session.ID)
	if gotSession.Status == "closed" {
		t.Fatalf("legacy closed a row the keyed D-STRANDED handler owns: %#v", gotSession.Metadata)
	}
}

// TestLegacyStrandedRepairStillRepairsUnownedRows is the other half of the
// doctrine: the exclusion is narrow. A row the keyed controller does NOT own
// still repairs through legacy for the whole WD wave, so installing the bridge
// cannot silently disable fleet repair.
func TestLegacyStrandedRepairStillRepairsUnownedRows(t *testing.T) {
	env, session, work, _ := strandedRepairReconcileEnv(t)
	runStrandedReconcileTick(t, env, []beads.Bead{session})
	env.clk.Time = env.clk.Time.Add(strandedRepairConfirmGrace + time.Minute)
	confirmed, _ := env.store.Get(session.ID)

	runStrandedReconcileTick(t, env, []beads.Bead{confirmed},
		withLegacyStrandedRepairExclusion(func(sessionpkg.Info) bool { return false }))

	gotWork, _ := env.store.Get(work.ID)
	if gotWork.Status != "open" || gotWork.Assignee != "" {
		t.Fatalf("legacy left an unowned stranded slot claimed: status=%q assignee=%q", gotWork.Status, gotWork.Assignee)
	}
	gotSession, _ := env.store.Get(session.ID)
	if gotSession.Status != "closed" {
		t.Fatalf("legacy left an unowned stranded session open: %#v", gotSession.Metadata)
	}
}

// TestStrandedRepairYieldIsNotSourceGated records WHY the yield answers on any
// in-flight admission rather than on the D-STRANDED source. The seam guards on
// the durable row, and the controller coalesces admissions on a key while
// keeping the EARLIER source — and a stranded pool member is routinely already
// held by a pool wake when the sweep finds it (that is exactly how the
// acked-member re-point residual arrives). A source-gated yield would let the
// keyed handler repair through the coalesced admission while legacy raced it at
// the same work beads: the ga-f7v2ft.125 hole on legacy's side.
func TestStrandedRepairYieldIsNotSourceGated(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = strandedTestConfig()
	provider := &unattendedStopProvider{Fake: env.sp}
	bead, work := seedStrandedPoolSlot(t, env, strandedRepairConfirmGrace+time.Minute,
		map[string]string{beadmeta.RoutedToMetadataKey: strandedTestTemplate})

	cr := newExactDeadlineRuntime(t, env, provider, nil, nil, events.NewFake())
	admit := cr.detectorAdmitFunc()
	if outcome, err := admit(bead.ID, sessionStartAdmissionAntiEntropy); err != nil || outcome == sessionStartAdmissionOverflow {
		t.Fatalf("admitting under a foreign source: outcome=%q err=%v", outcome, err)
	}
	awaitCond(t, func() bool {
		stored, err := env.store.Get(bead.ID)
		return err == nil && stored.Status == "closed"
	}, "keyed repair reached through a coalesced admission")

	gotWork, _ := env.store.Get(work.ID)
	if gotWork.Status != "open" || gotWork.Assignee != "" {
		t.Fatalf("a coalesced admission did not repair: status=%q assignee=%q", gotWork.Status, gotWork.Assignee)
	}
}
