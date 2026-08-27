package main

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/rollout"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

func orphanDrainAdmission(id string) sessionStartAdmission {
	return sessionStartAdmission{SessionID: id, Source: sessionStartAdmissionOrphanDrain}
}

// startLiveOrphan seeds one undesired row whose runtime is genuinely running,
// which is the single fact that splits the live-orphan drain arm from the
// dead-orphan close arm.
func startLiveOrphan(t *testing.T, env *reconcilerTestEnv, provider *deadRuntimeProvider, name string) beads.Bead {
	t.Helper()
	if err := provider.Start(t.Context(), name, runtime.Config{}); err != nil {
		t.Fatalf("start runtime for %q: %v", name, err)
	}
	bead := env.createSessionBead(name, name)
	env.markSessionActive(&bead)
	return bead
}

// TestExactOrphanLiveRowDrainsOnceByKey is WD.4's primary RED: a live row
// outside the desired set begins exactly ONE keyed drain against the same
// canonical bead, through the existing drain library, with the enqueue-only
// begin semantics preserved — the interrupt is still deferred to the advance
// loop, so the one-tick rescue window survives. It is the keyed re-point of
// TestReconcileSessionBeads_OrphanSessionDrained.
func TestExactOrphanLiveRowDrainsOnceByKey(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents:    []config.Agent{{Name: "other"}},
	}
	provider := &deadRuntimeProvider{Fake: env.sp}
	bead := startLiveOrphan(t, env, provider, "orphan")

	info, response, err := getAuthoritativeSessionStartPersistedRecord(env.store, bead.ID)
	if err != nil {
		t.Fatalf("authoritative read: %v", err)
	}
	before, err := env.store.Get(bead.ID)
	if err != nil {
		t.Fatal(err)
	}
	params := newExactOrphanCloseParams(env, provider, map[string]bool{})
	params.DrainTracker = env.dt

	handled, owner, err := reconcileExactSessionDetectorFamily(
		t.Context(), orphanDrainAdmission(bead.ID), params, info, response, env.clk)
	if !handled {
		t.Fatal("the D-ORPHAN seam did not claim a live undesired row")
	}
	if err != nil || owner != exactSessionStartKeyedOwner {
		t.Fatalf("drain handler returned owner=%v err=%v, want keyed ownership and no error", owner, err)
	}

	state := env.dt.get(bead.ID)
	if state == nil {
		t.Fatal("no drain intent recorded in the in-memory tracker; Q4 keeps drain intent there")
	}
	if state.reason != "orphaned" {
		t.Fatalf("drain reason = %q, want %q", state.reason, "orphaned")
	}
	// Enqueue-only begin (session_wake.go:203-233): the drain is recorded, the
	// runtime is untouched this tick, and nothing durable is written. The
	// one-tick rescue window is a behavior, not an accident.
	if !provider.IsRunning("orphan") {
		t.Fatal("the drain begin stopped the runtime; begin is enqueue-only and the interrupt is deferred")
	}
	for _, call := range env.sp.Calls {
		if call.Method == "Interrupt" || call.Method == "Stop" {
			t.Fatalf("drain begin issued %q on the provider; the interrupt belongs to the advance loop", call.Method)
		}
	}
	after, err := env.store.Get(bead.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != before.Status || len(after.Metadata) != len(before.Metadata) {
		t.Fatalf("drain begin mutated the durable row: before=%#v after=%#v", before.Metadata, after.Metadata)
	}

	// Exactly once against the same canonical bead, proven where production
	// enforces it: the sweep's family precedence hands a row with drain intent to
	// D-DRAIN, so the ORPHAN family never re-enqueues the key it just drained. No
	// treadmill, no second intent.
	//
	// Before WD.6 that was visible as an empty admitter, because D-DRAIN did not
	// act and nobody claimed the row. Now the key IS carried back in — under
	// drain_advance, which is the whole point of the family: legacy's end-of-tick
	// advance scan re-walks exactly these rows to drive the drain it began. The
	// invariant is unchanged, so it is asserted on the SOURCE rather than on
	// silence, and asserting the source is strictly the stronger form — an empty
	// admitter could never have caught an orphan-sourced re-enqueue arriving
	// beside a legitimate one.
	admitter := &recordingDetectorAdmitter{}
	in := orphanSweepInput(env, provider, env.sessionInfo(bead.ID), map[string]TemplateParams{}, env.clk.Now().UTC(), admitter.admit)
	in.Drains = env.dt
	in.SuspendDeferrals = newDetectorSuspendDeferralTracker()
	result := detectSessionConditions(context.Background(), in)
	routeDetectorConditions(in, &result)
	for i, source := range admitter.sources {
		if source != sessionStartAdmissionDrainAdvance {
			t.Fatalf("the sweep re-enqueued %q under %q for a row whose drain is already in flight; only D-DRAIN's advance may carry it back",
				admitter.keys[i], source)
		}
	}
	for _, cond := range result.Conditions {
		if cond.Family == detectorFamilyOrphan {
			t.Fatalf("the orphan family re-claimed a draining row: %#v", cond)
		}
	}

	// And if some other admission does carry the key back in, no second intent
	// is recorded: the drain begun above is the only one.
	firstStartedAt := state.startedAt
	env.clk.Time = env.clk.Time.Add(time.Minute)
	if _, _, err := reconcileExactSessionDetectorFamily(
		t.Context(), orphanDrainAdmission(bead.ID), params, info, response, env.clk); err == nil {
		t.Fatal("a re-admitted draining row was not refused; advancing a drain is D-DRAIN's")
	}
	again := env.dt.get(bead.ID)
	if again == nil || !again.startedAt.Equal(firstStartedAt) || again.reason != "orphaned" {
		t.Fatalf("the re-admission restarted the drain: %#v", again)
	}
}

// TestExactOrphanDrainToCloseRelay is the WD.3/WD.4 handoff proven end to end in
// one test. A live undesired row is refused by the close arm as possibly-alive
// and drained by this slice's arm; once the drain has converged and the runtime
// is provably dead, the close arm — already landed at WD.3 — closes the same
// canonical bead. Neither arm acts twice and neither acts on the other's row.
func TestExactOrphanDrainToCloseRelay(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents:    []config.Agent{{Name: "other"}},
	}
	provider := &deadRuntimeProvider{Fake: env.sp}
	bead := startLiveOrphan(t, env, provider, "orphan")

	info, response, err := getAuthoritativeSessionStartPersistedRecord(env.store, bead.ID)
	if err != nil {
		t.Fatalf("authoritative read: %v", err)
	}
	params := newExactOrphanCloseParams(env, provider, map[string]bool{})
	params.DrainTracker = env.dt

	// Leg 1 — the row WD.3's close arm releases as possibly-alive is drained here.
	if _, _, err := reconcileExactSessionDetectorFamily(
		t.Context(), orphanCloseAdmission(bead.ID), params, info, response, env.clk); err != nil {
		t.Fatalf("close-arm admission on a live row: %v", err)
	}
	if state := env.dt.get(bead.ID); state == nil {
		t.Fatal("the close arm released a possibly-alive row and no drain arm picked it up")
	}
	if stored, _ := env.store.Get(bead.ID); stored.Status == "closed" {
		t.Fatal("the close arm closed a live row instead of releasing it to the drain arm")
	}

	// Leg 2 — the drain converges: the runtime exits and the intent is retired,
	// which is D-DRAIN's advance (WD.6) and legacy's today. The relay under test
	// is what the two ORPHAN arms do on either side of it.
	if err := provider.Stop("orphan"); err != nil {
		t.Fatalf("stop the drained runtime: %v", err)
	}
	env.dt.remove(bead.ID)

	// Leg 3 — with absence now provable, the landed close arm closes the same key.
	latest, latestResponse, err := getAuthoritativeSessionStartPersistedRecord(env.store, bead.ID)
	if err != nil {
		t.Fatalf("authoritative re-read: %v", err)
	}
	handled, owner, err := reconcileExactSessionDetectorFamily(
		t.Context(), orphanCloseAdmission(bead.ID), params, latest, latestResponse, env.clk)
	if !handled || err != nil || owner != exactSessionStartKeyedOwner {
		t.Fatalf("close leg of the relay: handled=%v owner=%v err=%v", handled, owner, err)
	}
	stored, err := env.store.Get(bead.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != "closed" {
		t.Fatalf("row status after the relay = %q, want closed", stored.Status)
	}
	if want := sessionpkg.CanonicalCloseReason("orphaned"); stored.Metadata["close_reason"] != want {
		t.Fatalf("close_reason after the relay = %q, want %q", stored.Metadata["close_reason"], want)
	}
}

// TestDetectorOrphanNamedSuspendDeferredUntilWindowElapses is the named
// deferred-confirm negative. A named row whose configured spec vanished is
// untouched — no enqueue, no drain intent, zero writes — until the detector's
// own confirmation window elapses, and the counter it advances is the
// DETECTOR's: legacy's drainTracker counter is never touched by the keyed path.
func TestDetectorOrphanNamedSuspendDeferredUntilWindowElapses(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "other"}}}
	provider := &deadRuntimeProvider{Fake: env.sp}
	bead := startLiveOrphan(t, env, provider, "mayor")
	env.setSessionMetadata(&bead, map[string]string{sessionpkg.NamedSessionMetadataKey: "true"})

	deferrals := newDetectorSuspendDeferralTracker()
	before, err := env.store.Get(bead.ID)
	if err != nil {
		t.Fatal(err)
	}

	for tick := 1; tick < namedSuspendConfirmTicks; tick++ {
		admitter := &recordingDetectorAdmitter{}
		in := orphanSweepInput(env, provider, env.sessionInfo(bead.ID), map[string]TemplateParams{}, env.clk.Now().UTC(), admitter.admit)
		in.Drains = env.dt
		in.SuspendDeferrals = deferrals
		result := detectSessionConditions(context.Background(), in)
		routeDetectorConditions(in, &result)

		if len(admitter.keys) != 0 {
			t.Fatalf("tick %d enqueued %v inside the deferred-confirm window", tick, admitter.keys)
		}
		deferred := false
		for _, cond := range result.Conditions {
			if cond.Family != detectorFamilyOrphan {
				continue
			}
			if cond.Outcome == TraceOutcomeDrain {
				t.Fatalf("tick %d predicted a drain inside the deferred-confirm window: %#v", tick, cond)
			}
			if cond.Reason == detectorReasonOrphanSuspendDeferred && cond.Outcome == TraceOutcomeDeferredConfirm {
				deferred = true
			}
		}
		if !deferred {
			t.Fatalf("tick %d recorded no deferred-confirm condition; conditions=%#v", tick, result.Conditions)
		}
		if env.dt.get(bead.ID) != nil {
			t.Fatalf("tick %d recorded drain intent inside the deferred-confirm window", tick)
		}
		if after, _ := env.store.Get(bead.ID); len(after.Metadata) != len(before.Metadata) || after.Status != before.Status {
			t.Fatalf("tick %d wrote to the durable row: before=%#v after=%#v", tick, before.Metadata, after.Metadata)
		}
	}

	// The window elapses on the confirming tick and only then is the key enqueued.
	admitter := &recordingDetectorAdmitter{}
	in := orphanSweepInput(env, provider, env.sessionInfo(bead.ID), map[string]TemplateParams{}, env.clk.Now().UTC(), admitter.admit)
	in.Drains = env.dt
	in.SuspendDeferrals = deferrals
	result := detectSessionConditions(context.Background(), in)
	routeDetectorConditions(in, &result)
	if len(admitter.keys) != 1 || admitter.keys[0] != bead.ID {
		t.Fatalf("confirming tick enqueued %v, want exactly the orphan key", admitter.keys)
	}
	if admitter.sources[0] != sessionStartAdmissionOrphanDrain {
		t.Fatalf("confirming tick admitted under %q, want %q", admitter.sources[0], sessionStartAdmissionOrphanDrain)
	}

	// The counter lives ONLY in the detector: legacy's drainTracker window is
	// still at zero, so its first bump returns one.
	if n := env.dt.bumpSuspendDeferral(bead.ID); n != 1 {
		t.Fatalf("legacy drainTracker suspend counter = %d after the keyed window, want an untouched 0 (first bump returns 1)", n)
	}
}

// TestDetectorNamedSuspendDeferralTrackerIsBoundedAndConsecutive pins the two
// properties the moved counter must keep: it counts CONSECUTIVE candidacy (a
// tick that does not re-raise the condition restarts the window), which is what
// bounds it to the live fleet rather than growing with every row ever seen.
func TestDetectorNamedSuspendDeferralTrackerIsBoundedAndConsecutive(t *testing.T) {
	tr := newDetectorSuspendDeferralTracker()
	if n := tr.bump("ga-a"); n != 1 {
		t.Fatalf("first bump = %d, want 1", n)
	}
	tr.prune()
	if n := tr.bump("ga-a"); n != 2 {
		t.Fatalf("consecutive bump = %d, want 2", n)
	}
	tr.bump("ga-b")
	tr.prune()
	// A sweep that raises the condition for ga-b only must drop ga-a's window.
	tr.bump("ga-b")
	tr.prune()
	if n := tr.bump("ga-a"); n != 1 {
		t.Fatalf("bump after a non-confirming sweep = %d, want a restarted window of 1", n)
	}
	if n := tr.bump("ga-b"); n != 3 {
		t.Fatalf("ga-b consecutive bump = %d, want 3", n)
	}
}

// TestDetectorOrphanDrainRequiresConfirmationStateToCount is the fail-closed
// half of the moved counter: a sweep with no confirmation state (the read-only
// `gc start` and control-dispatcher call sites, which publish no cross-tick
// state) defers a named row forever rather than draining it on its first
// spec-absent tick.
func TestDetectorOrphanDrainRequiresConfirmationStateToCount(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "other"}}}
	provider := &deadRuntimeProvider{Fake: env.sp}
	bead := startLiveOrphan(t, env, provider, "mayor")
	env.setSessionMetadata(&bead, map[string]string{sessionpkg.NamedSessionMetadataKey: "true"})

	admitter := &recordingDetectorAdmitter{}
	in := orphanSweepInput(env, provider, env.sessionInfo(bead.ID), map[string]TemplateParams{}, env.clk.Now().UTC(), admitter.admit)
	in.Drains = env.dt
	result := detectSessionConditions(context.Background(), in)
	routeDetectorConditions(in, &result)

	if len(admitter.keys) != 0 {
		t.Fatalf("a sweep with no confirmation state enqueued %v; want zero", admitter.keys)
	}
	for _, cond := range result.Conditions {
		if cond.Family == detectorFamilyOrphan && cond.Outcome == TraceOutcomeDrain {
			t.Fatalf("a sweep with no confirmation state predicted a named drain: %#v", cond)
		}
	}
}

// TestDetectorOrphanLiveRowRoutesUnderDrainSource is the routing half of the
// seam: an unnamed live orphan is admitted under the DRAIN source, not the
// close source, so each arm's legacy yield answers for its own family.
func TestDetectorOrphanLiveRowRoutesUnderDrainSource(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "other"}}}
	provider := &deadRuntimeProvider{Fake: env.sp}
	bead := startLiveOrphan(t, env, provider, "orphan")

	admitter := &recordingDetectorAdmitter{}
	in := orphanSweepInput(env, provider, env.sessionInfo(bead.ID), map[string]TemplateParams{}, env.clk.Now().UTC(), admitter.admit)
	in.Drains = env.dt
	in.SuspendDeferrals = newDetectorSuspendDeferralTracker()
	result := detectSessionConditions(context.Background(), in)
	routeDetectorConditions(in, &result)

	if len(admitter.keys) != 1 || admitter.keys[0] != bead.ID {
		t.Fatalf("live orphan enqueued %v, want exactly the orphan key", admitter.keys)
	}
	if admitter.sources[0] != sessionStartAdmissionOrphanDrain {
		t.Fatalf("live orphan admitted under %q, want %q", admitter.sources[0], sessionStartAdmissionOrphanDrain)
	}
	drainArm := false
	for _, cond := range result.Conditions {
		if cond.Family != detectorFamilyOrphan {
			continue
		}
		if cond.Outcome == TraceOutcomeDrain {
			drainArm = true
			if cond.Site != TraceSiteReconcilerOrphaned {
				t.Fatalf("drain arm recorded at %q, want the legacy Orphaned site", cond.Site)
			}
			if cond.AdmissionSource != sessionStartAdmissionOrphanDrain {
				t.Fatalf("drain condition carries admission source %q", cond.AdmissionSource)
			}
		}
	}
	if !drainArm {
		t.Fatalf("no drain arm raised for a live orphan; conditions=%#v", result.Conditions)
	}
}

// TestDetectorOrphanLiveAssignedWorkNeverEnqueuesOrDrains ports
// TestReconcileSessionBeads_OrphanDrainLiveAssignedWorkStaysOpen to the keyed
// path: a LIVE orphan still holding open assigned work is never enqueued and
// never drained, so no interrupt can reach an agent mid-tool-call.
func TestDetectorOrphanLiveAssignedWorkNeverEnqueuesOrDrains(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "other"}}}
	provider := &deadRuntimeProvider{Fake: env.sp}
	bead := startLiveOrphan(t, env, provider, "orphan")

	admitter := &recordingDetectorAdmitter{}
	in := orphanSweepInput(env, provider, env.sessionInfo(bead.ID), map[string]TemplateParams{}, env.clk.Now().UTC(), admitter.admit)
	in.Drains = env.dt
	in.SuspendDeferrals = newDetectorSuspendDeferralTracker()
	in.AssignedWorkBeads = []beads.Bead{{ID: "ga-work-1", Status: "in_progress", Assignee: bead.ID}}
	result := detectSessionConditions(context.Background(), in)
	routeDetectorConditions(in, &result)

	if len(admitter.keys) != 0 {
		t.Fatalf("a live orphan holding assigned work enqueued %v; want zero enqueues", admitter.keys)
	}
	for _, cond := range result.Conditions {
		if cond.Family == detectorFamilyOrphan && cond.Outcome == TraceOutcomeDrain {
			t.Fatalf("a live orphan holding assigned work predicted a drain: %#v", cond)
		}
	}
	if env.dt.get(bead.ID) != nil {
		t.Fatal("a live orphan holding assigned work recorded drain intent")
	}
}

// TestDetectorOrphanDrainD2IncapableProviderRefuses is the third AC negative: a
// provider that cannot prove fresh liveness and an unattended stop yields a
// traced refusal with no enqueue — refused every sweep, never a re-enqueue
// treadmill.
func TestDetectorOrphanDrainD2IncapableProviderRefuses(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "other"}}}
	if err := env.sp.Start(t.Context(), "orphan", runtime.Config{}); err != nil {
		t.Fatalf("start runtime: %v", err)
	}
	bead := env.createSessionBead("orphan", "orphan")
	env.markSessionActive(&bead)

	admitter := &recordingDetectorAdmitter{}
	// env.sp is the bare fake: it implements neither FreshLivenessObserver nor
	// UnattendedSessionStopper, which is exactly the D2-incapable shape.
	in := orphanSweepInput(env, env.sp, env.sessionInfo(bead.ID), map[string]TemplateParams{}, env.clk.Now().UTC(), admitter.admit)
	in.Drains = env.dt
	in.SuspendDeferrals = newDetectorSuspendDeferralTracker()
	result := detectSessionConditions(context.Background(), in)
	routeDetectorConditions(in, &result)

	if len(admitter.keys) != 0 {
		t.Fatalf("a D2-incapable provider enqueued %v; want zero enqueues", admitter.keys)
	}
	refused := false
	for _, cond := range result.Conditions {
		if cond.Family == detectorFamilyOrphan && cond.Outcome == TraceOutcomeDrain {
			if cond.AdmissionOutcome != detectorAdmissionRefusedProviderIncapable {
				t.Fatalf("drain condition admission outcome = %q, want %q", cond.AdmissionOutcome, detectorAdmissionRefusedProviderIncapable)
			}
			refused = true
		}
	}
	if !refused {
		t.Fatalf("no traced drain refusal for a D2-incapable provider; conditions=%#v", result.Conditions)
	}
	if env.dt.get(bead.ID) != nil {
		t.Fatal("a D2-incapable provider recorded drain intent")
	}
}

// TestExactOrphanDrainDesiredRowNeitherEnqueuesNorDrains is the zero-effect
// negative: a DESIRED row raises no orphan condition and the drain arm refuses
// it even when some other admission carries the key into the seam.
func TestExactOrphanDrainDesiredRowNeitherEnqueuesNorDrains(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	provider := &deadRuntimeProvider{Fake: env.sp}
	bead := startLiveOrphan(t, env, provider, "worker")

	desired := map[string]TemplateParams{"worker": {SessionName: "worker", TemplateName: "worker"}}
	admitter := &recordingDetectorAdmitter{}
	in := orphanSweepInput(env, provider, env.sessionInfo(bead.ID), desired, env.clk.Now().UTC(), admitter.admit)
	in.Drains = env.dt
	in.SuspendDeferrals = newDetectorSuspendDeferralTracker()
	result := detectSessionConditions(context.Background(), in)
	routeDetectorConditions(in, &result)
	for _, cond := range result.Conditions {
		if cond.Family == detectorFamilyOrphan {
			t.Fatalf("sweep raised an orphan condition for a desired live row: %#v", cond)
		}
	}
	if len(admitter.keys) != 0 {
		t.Fatalf("sweep enqueued %v for a desired row; want zero enqueues", admitter.keys)
	}

	info, response, err := getAuthoritativeSessionStartPersistedRecord(env.store, bead.ID)
	if err != nil {
		t.Fatalf("authoritative read: %v", err)
	}
	params := newExactOrphanCloseParams(env, provider, map[string]bool{"worker": true})
	params.DrainTracker = env.dt
	handled, owner, err := reconcileExactSessionDetectorFamily(
		t.Context(), orphanDrainAdmission(bead.ID), params, info, response, env.clk)
	if handled || err != nil || owner != exactSessionStartUnowned {
		t.Fatalf("desired row entered the drain seam: handled=%v owner=%v err=%v", handled, owner, err)
	}
	if env.dt.get(bead.ID) != nil {
		t.Fatal("desired row was drained by the keyed orphan handler")
	}
}

// TestExactOrphanDrainDefersAttachedSession is the A6 negative: attached-user
// safety is a KEEP invariant of the whole redesign (DESIGN.md §2), so the keyed
// drain arm never begins a drain against a session a human is attached to. The
// refusal is zero-effect and level-triggered — the drain proceeds on a later
// tick once the human leaves.
func TestExactOrphanDrainDefersAttachedSession(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "other"}}}
	provider := &deadRuntimeProvider{Fake: env.sp}
	bead := startLiveOrphan(t, env, provider, "orphan")
	env.sp.SetAttached("orphan", true)

	info, response, err := getAuthoritativeSessionStartPersistedRecord(env.store, bead.ID)
	if err != nil {
		t.Fatalf("authoritative read: %v", err)
	}
	before, err := env.store.Get(bead.ID)
	if err != nil {
		t.Fatal(err)
	}
	params := newExactOrphanCloseParams(env, provider, map[string]bool{})
	params.DrainTracker = env.dt

	handled, owner, err := reconcileExactSessionDetectorFamily(
		t.Context(), orphanDrainAdmission(bead.ID), params, info, response, env.clk)
	if !handled {
		t.Fatal("the seam did not claim a live undesired row")
	}
	if err != nil || owner != exactSessionStartKeyedOwner {
		t.Fatalf("attached deferral returned owner=%v err=%v, want a quiet keyed refusal", owner, err)
	}
	if env.dt.get(bead.ID) != nil {
		t.Fatal("an attached session was drained; A6 forbids interrupting an attached human")
	}
	after, err := env.store.Get(bead.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != before.Status || len(after.Metadata) != len(before.Metadata) {
		t.Fatalf("the attached deferral wrote to the durable row: before=%#v after=%#v", before.Metadata, after.Metadata)
	}
	if !provider.IsRunning("orphan") {
		t.Fatal("the attached deferral stopped the runtime")
	}

	// Level-triggered: the drain proceeds once the human detaches.
	env.sp.SetAttached("orphan", false)
	if _, _, err := reconcileExactSessionDetectorFamily(
		t.Context(), orphanDrainAdmission(bead.ID), params, info, response, env.clk); err != nil {
		t.Fatalf("drain after detach: %v", err)
	}
	if env.dt.get(bead.ID) == nil {
		t.Fatal("the drain did not resume after the human detached")
	}
}

// TestLegacyOrphanDrainArmYieldsToKeyedOwnedRow is the coexistence-doctrine RED
// for the legacy Orphaned DRAIN arm. Both writers read the same in-memory
// tracker on the same tick, so an acting keyed drain beside a non-yielding
// legacy double-begins by construction; legacy must stand down while the keyed
// controller owns the key.
func TestLegacyOrphanDrainArmYieldsToKeyedOwnedRow(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "other"}}}
	if err := env.sp.Start(t.Context(), "orphan", runtime.Config{}); err != nil {
		t.Fatalf("start runtime: %v", err)
	}
	session := env.createSessionBead("orphan", "orphan")
	env.markSessionActive(&session)

	cfgNames := configuredSessionNames(env.cfg, "", env.store)
	reconcileSessionBeads(
		context.Background(), []beads.Bead{session}, env.desiredState, cfgNames,
		env.cfg, env.sp, env.store, nil, nil, nil, env.dt, map[string]int{}, false, nil, "",
		nil, env.clk, env.rec, 0, 0, &env.stdout, &env.stderr,
		withLegacyOrphanDrainExclusion(func(info sessionpkg.Info) bool { return info.ID == session.ID }),
	)

	if state := env.dt.get(session.ID); state != nil {
		t.Fatalf("legacy Orphaned drain arm began a drain the keyed handler owns: %#v", state)
	}
}

// TestLegacyOrphanDrainArmStillDrainsUnownedRows is the other half of the
// yield: the exclusion is per-key, so a row the keyed controller does NOT own
// still drains on the legacy arm. A yield that stood down fleet-wide would
// silently disable orphan drains everywhere.
func TestLegacyOrphanDrainArmStillDrainsUnownedRows(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "other"}}}
	if err := env.sp.Start(t.Context(), "orphan", runtime.Config{}); err != nil {
		t.Fatalf("start runtime: %v", err)
	}
	session := env.createSessionBead("orphan", "orphan")
	env.markSessionActive(&session)

	cfgNames := configuredSessionNames(env.cfg, "", env.store)
	reconcileSessionBeads(
		context.Background(), []beads.Bead{session}, env.desiredState, cfgNames,
		env.cfg, env.sp, env.store, nil, nil, nil, env.dt, map[string]int{}, false, nil, "",
		nil, env.clk, env.rec, 0, 0, &env.stdout, &env.stderr,
		withLegacyOrphanDrainExclusion(func(sessionpkg.Info) bool { return false }),
	)

	if env.dt.get(session.ID) == nil {
		t.Fatal("legacy Orphaned drain arm stood down for a row no keyed handler owns")
	}
}

// TestExactOrphanDrainAdmissionIsOwnedAndYielded closes the ownership loop, and
// states this slice's ownership-semantics decision: the drain arm gets its OWN
// sibling predicate rather than widening ownsOrphanClose. ownsOrphanClose
// answers "is a D-ORPHAN CLOSE in flight for this key", which is false for
// every drain admission; a single predicate serving both arms would make
// legacy's close arm stand down for rows the drain arm owns and vice versa.
func TestExactOrphanDrainAdmissionIsOwnedAndYielded(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "other"}}}
	provider := &deadRuntimeProvider{Fake: env.sp}
	bead := env.createSessionBead("orphan", "orphan")

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
	if _, err := controller.Admit(bead.ID, sessionStartAdmissionOrphanDrain); err != nil {
		t.Fatalf("admitting orphan-drain key: %v", err)
	}
	if controller.ownsOrphanClose(bead.ID) {
		t.Fatal("ownsOrphanClose() answered true for an orphan-drain admission; legacy's close arm would be silently disabled")
	}
	if controller.ownsDeadlineStop(bead.ID) {
		t.Fatal("ownsDeadlineStop() answered true for an orphan-drain admission; legacy's idle kill would be silently disabled")
	}
	awaitCond(t, func() bool { return !controller.ownsOrphanDrain(bead.ID) }, "orphan-drain admission drain")
}
