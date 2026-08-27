package main

import (
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// legacyPreWakeValidatorTemplateParams is the minimal template every case in
// this file starts through the legacy wave.
func legacyPreWakeValidatorTemplateParams(name string) TemplateParams {
	return TemplateParams{
		Command:      "test-cmd",
		SessionName:  name,
		TemplateName: "worker",
	}
}

// legacyPreWakeValidatorConfig resolves "worker" to a dependency-free agent, the
// row class classifyExactSessionStartOwnership hands to the keyed owner once a
// wake cause is present.
func legacyPreWakeValidatorConfig() *config.City {
	return &config.City{Agents: []config.Agent{{Name: "worker"}}}
}

// keyedReconciliationActive stands in for the installed keyed-ownership seam.
// It answers false for the SNAPSHOT the candidate was decided on — exactly the
// production shape, where the seam is evaluated pre-window on stale info and
// cannot exclude a row whose wake cause does not exist yet.
func keyedReconciliationActive(sessionpkg.Info) bool { return false }

// TestLegacyStartPreWakeValidatorSkipsRespawnAfterKill is the ga-l1j53 P2 red
// for sub-race A. `gc session kill` writes SleepPatch(now, "killed") after the
// worker handle kills the runtime, and for a manual session no wake cause is
// synthesized. A tick whose metadata snapshot predates that patch still sees the
// row desired-awake and dead, so it emits a start candidate; the keyed_start_owner
// seam cannot exclude it, because a post-kill/pre-wake row has no wake cause at
// all. Without a prepare-time re-validation the legacy wave then respawns the row
// the operator just killed, and `gc session kill` has no durable meaning for any
// session whose sleep policy is off.
func TestLegacyStartPreWakeValidatorSkipsRespawnAfterKill(t *testing.T) {
	env := newReconcilerTestEnv()
	bead := env.createSessionBead("worker-adhoc-kill", "worker")
	env.setSessionMetadata(&bead, map[string]string{
		"state":          "active",
		"session_origin": "manual",
		"last_woke_at":   env.clk.Now().UTC().Format(time.RFC3339),
	})
	// The candidate is decided on this snapshot.
	snapshot := env.sessionInfo(bead.ID)

	// The kill lands mid-tick, after the snapshot and before prepare.
	killedAt := env.clk.Now().UTC().Add(time.Second)
	if err := sessionFrontDoor(env.store).ApplyPatch(bead.ID, sessionpkg.SleepPatch(killedAt, "killed")); err != nil {
		t.Fatalf("apply kill sleep patch: %v", err)
	}

	prepared, err := prepareStartCandidateForCity(
		startCandidate{info: snapshot, tp: legacyPreWakeValidatorTemplateParams("worker-adhoc-kill")},
		"", "", legacyPreWakeValidatorConfig(), env.sp, env.store, env.clk, io.Discard, nil,
		keyedReconciliationActive,
	)
	if !errors.Is(err, errPreWakeSuperseded) || prepared != nil {
		t.Fatalf("prepare on a killed-since-snapshot row = %v (prepared=%v), want errPreWakeSuperseded", err, prepared != nil)
	}

	latest := env.sessionInfo(bead.ID)
	if latest.MetadataState != string(sessionpkg.StateAsleep) || latest.SleepReason != "killed" {
		t.Fatalf("killed row = state %q sleep_reason %q, want asleep/killed: the stale tick respawned it",
			latest.MetadataState, latest.SleepReason)
	}
	if latest.InstanceToken != snapshot.InstanceToken || latest.Generation != snapshot.Generation {
		t.Fatalf("killed row rotated its incarnation: token %q->%q generation %q->%q",
			snapshot.InstanceToken, latest.InstanceToken, snapshot.Generation, latest.Generation)
	}
}

// TestLegacyStartPreWakeValidatorSkipsKeyedOwnedWakeWindow is the ga-l1j53 P2
// red for sub-race B. Once wake_request=explicit is durable the row is keyed-
// owned, but a legacy candidate already in flight enters the SHARED start wave
// beside the keyed admission. Both re-read the row and both commit a fresh
// incarnation, so the durable instance_token ends up naming neither runtime. The
// legacy entrant must re-classify on the CURRENT row and abort.
func TestLegacyStartPreWakeValidatorSkipsKeyedOwnedWakeWindow(t *testing.T) {
	env := newReconcilerTestEnv()
	bead := env.createSessionBead("worker-adhoc-wake", "worker")
	env.setSessionMetadata(&bead, map[string]string{
		"session_origin":    "manual",
		"wake_request":      "explicit",
		"wake_requested_at": env.clk.Now().UTC().Format(time.RFC3339),
	})
	current := env.sessionInfo(bead.ID)
	if _, _, owner := classifyExactSessionStartOwnership(current, legacyPreWakeValidatorConfig(), env.clk.Now().UTC()); owner != exactSessionStartKeyedOwner {
		t.Fatalf("test premise: ownership = %v, want keyed", owner)
	}

	prepared, err := prepareStartCandidateForCity(
		startCandidate{info: current, tp: legacyPreWakeValidatorTemplateParams("worker-adhoc-wake")},
		"", "", legacyPreWakeValidatorConfig(), env.sp, env.store, env.clk, io.Discard, nil,
		keyedReconciliationActive,
	)
	if !errors.Is(err, errPreWakeSuperseded) || prepared != nil {
		t.Fatalf("prepare inside the keyed wake window = %v (prepared=%v), want errPreWakeSuperseded", err, prepared != nil)
	}
	latest := env.sessionInfo(bead.ID)
	if latest.WakeRequest != "explicit" || latest.InstanceToken != current.InstanceToken {
		t.Fatalf("legacy entrant consumed the keyed wake: wake_request %q token %q->%q",
			latest.WakeRequest, current.InstanceToken, latest.InstanceToken)
	}
}

// TestLegacyStartPreWakeValidatorLeavesLegacyOnlyCitiesStarting guards the
// ownership arm's gate. Where no keyed-ownership seam is installed, legacy is the
// ONLY starter: skipping a keyed-CLASSIFIED row there would strand every explicit
// wake forever.
func TestLegacyStartPreWakeValidatorLeavesLegacyOnlyCitiesStarting(t *testing.T) {
	env := newReconcilerTestEnv()
	bead := env.createSessionBead("worker-adhoc-legacy", "worker")
	env.setSessionMetadata(&bead, map[string]string{
		"session_origin":    "manual",
		"wake_request":      "explicit",
		"wake_requested_at": env.clk.Now().UTC().Format(time.RFC3339),
	})
	current := env.sessionInfo(bead.ID)

	prepared, err := prepareStartCandidateForCity(
		startCandidate{info: current, tp: legacyPreWakeValidatorTemplateParams("worker-adhoc-legacy")},
		"", "", legacyPreWakeValidatorConfig(), env.sp, env.store, env.clk, io.Discard, nil,
		nil,
	)
	if err != nil || prepared == nil {
		t.Fatalf("prepare with no keyed seam installed = %v (prepared=%v), want the legacy start to proceed", err, prepared != nil)
	}
	latest := env.sessionInfo(bead.ID)
	if latest.InstanceToken == current.InstanceToken || latest.MetadataState != string(sessionpkg.StateCreating) {
		t.Fatalf("legacy-only start did not rotate: token %q->%q state %q",
			current.InstanceToken, latest.InstanceToken, latest.MetadataState)
	}
}

// TestLegacyStartPreWakeValidatorStartsUnchangedPoolFloorRefill is the negative
// the drift compare must not break: a min-floor refill of a genuinely sleeping
// pool member whose row did NOT change since the snapshot still starts. The
// premise check is freshness only — it must never grow into a fleet-demand
// judgment, and "killed" is deliberately not read through IsDeliberateSleepReason.
func TestLegacyStartPreWakeValidatorStartsUnchangedPoolFloorRefill(t *testing.T) {
	env := newReconcilerTestEnv()
	bead := env.createSessionBead("worker-1", "worker")
	env.setSessionMetadata(&bead, map[string]string{
		"pool_slot":    "1",
		"pool_managed": "true",
		"sleep_reason": string(sessionpkg.SleepReasonIdleTimeout),
	})
	snapshot := env.sessionInfo(bead.ID)
	if !isPoolManagedSessionInfo(snapshot) {
		t.Fatalf("test premise: %+v is not a pool-managed session", snapshot)
	}

	prepared, err := prepareStartCandidateForCity(
		startCandidate{info: snapshot, tp: legacyPreWakeValidatorTemplateParams("worker-1")},
		"", "", legacyPreWakeValidatorConfig(), env.sp, env.store, env.clk, io.Discard, nil,
		keyedReconciliationActive,
	)
	if err != nil || prepared == nil {
		t.Fatalf("floor refill of an unchanged sleeping pool member = %v (prepared=%v), want a start", err, prepared != nil)
	}
	latest := env.sessionInfo(bead.ID)
	if latest.InstanceToken == snapshot.InstanceToken || latest.MetadataState != string(sessionpkg.StateCreating) {
		t.Fatalf("floor refill did not rotate the incarnation: token %q->%q state %q",
			snapshot.InstanceToken, latest.InstanceToken, latest.MetadataState)
	}
}

// TestLegacyStartPreWakeValidatorSkipsMidIncarnationStart pins the third arm:
// a fresh `creating` row that already carries last_woke_at is another writer's
// rotation in flight. Starting on top of it mints the second incarnation the
// split-brain is made of. A STALE creating row still starts — that is the
// existing respawn contract, not a race. Both legs run with no keyed seam so the
// ownership arm cannot mask the result.
func TestLegacyStartPreWakeValidatorSkipsMidIncarnationStart(t *testing.T) {
	env := newReconcilerTestEnv()
	bead := env.createSessionBead("worker-adhoc-inflight", "worker")
	env.setSessionMetadata(&bead, map[string]string{
		"state":                     "creating",
		"session_origin":            "manual",
		"last_woke_at":              env.clk.Now().UTC().Format(time.RFC3339),
		"pending_create_claim":      "true",
		"pending_create_started_at": env.clk.Now().UTC().Format(time.RFC3339),
	})
	snapshot := env.sessionInfo(bead.ID)

	prepared, err := prepareStartCandidateForCity(
		startCandidate{info: snapshot, tp: legacyPreWakeValidatorTemplateParams("worker-adhoc-inflight")},
		"", "", legacyPreWakeValidatorConfig(), env.sp, env.store, env.clk, io.Discard, nil,
		nil,
	)
	if !errors.Is(err, errPreWakeSuperseded) || prepared != nil {
		t.Fatalf("prepare on a mid-incarnation row = %v (prepared=%v), want errPreWakeSuperseded", err, prepared != nil)
	}

	// Age the in-flight create past the stale-creating window: the respawn is
	// then legitimate and must proceed.
	env.clk.Time = env.clk.Time.Add(staleCreatingStateTimeout + time.Minute)
	stale := env.sessionInfo(bead.ID)
	prepared, err = prepareStartCandidateForCity(
		startCandidate{info: stale, tp: legacyPreWakeValidatorTemplateParams("worker-adhoc-inflight")},
		"", "", legacyPreWakeValidatorConfig(), env.sp, env.store, env.clk, io.Discard, nil,
		nil,
	)
	if err != nil || prepared == nil {
		t.Fatalf("prepare on a stale-creating row = %v (prepared=%v), want the respawn to proceed", err, prepared != nil)
	}
	if strings.TrimSpace(env.sessionInfo(bead.ID).InstanceToken) == strings.TrimSpace(stale.InstanceToken) {
		t.Fatalf("stale-creating respawn did not rotate the incarnation")
	}
}
