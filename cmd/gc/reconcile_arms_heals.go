package main

import (
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// Arm A6's row-local heals and markers beside the timer heals (CONTRACT v5
// §4 A6). Each is a CAS through the row-write effect, which re-decides on the
// fresh row (R2) and refuses unless the row still holds the incarnation the
// pass saw, instance_token included. The heals that move state on what the
// runtime reads are the fresh kind: their effect re-proves the read under the
// runtime name lock before it writes (reconcile_effect_heal.go). No arm here
// runs for a row with an effect in flight: the pass skips it (R5), nor for a
// census-only row, which legacy never reconciles (armCensusOnly).
//
// The asleep heals act only on a runtime the inventory reads gone. A dead
// one (a corpse, or a live pane whose agent reads dead, which a booting agent
// can) waits for A12 to classify it first, so its crash, rate-limit or
// terminal screen is never lost to the heal's continuation reset (SESS-057,
// SESS-530); once A12 lands, a classified (row, runtime token) may heal here.

// A6's other reasons.
const (
	decideClaimClear      = "claim-clear"
	decideCreatingHeal    = "creating-heal"
	decideDeadRuntimeHeal = "dead-runtime-heal"
	decideAwakeHeal       = "awake-heal"
	decideStabilityClear  = "stability-clear"
	decideDetachedAt      = "detached-at"
	decideStrandedClear   = "stranded-clear"
	decideCurrentBead     = "current-bead"
)

// heal is the row write of patch as kind, at the incarnation the pass saw.
func (r *rowFacts) heal(kind, reason string, patch session.MetadataPatch) (intent, bool) {
	basis := rowBasis{Incarnation: r.row.Incarnation, InstanceToken: r.row.InstanceToken}
	return intent{Kind: kind, Reason: reason, Basis: basis, Patch: patch}, true
}

// gone reports that the row's runtime reads gone; wake that the row is Wake.
func (r *rowFacts) gone() bool { return r.entry != nil && r.entry.Liveness == livenessGone }
func (r *rowFacts) wake() bool { return r.entry != nil && r.entry.Desired == desireWake }

// aliveProbed reports the row's runtime alive with no failed process probe:
// legacy defers a row's whole lifecycle when its liveness probe errs (the
// probe-error defer, SESS-056), so no alive-gated heal or marker runs then.
func (r *rowFacts) aliveProbed() bool {
	return r.entry != nil && r.entry.Liveness.alive() && !r.probeFailed()
}

// probeFailed reports that the row's last process probe failed.
func (r *rowFacts) probeFailed() bool {
	name := strings.TrimSpace(r.row.Info.SessionName)
	return r.w.Obs.Fact(name, FactProcessAlive, r.w.Now, r.w.ObsMaxAge).Reason == obsReasonProbeIncomplete
}

// committed reports state active or awake, which S2's commit writes.
func committed(info session.Info) bool {
	state := session.State(strings.TrimSpace(info.MetadataState))
	return state == session.StateActive || state == session.StateAwake
}

// armClaimClear clears a pending_create_claim left on a committed row (v5
// P4): legacy can commit before it clears the claim, and the claim no longer
// counts as a bring-up. The keys are CommitStartedPatch's claim clear.
func armClaimClear(r *rowFacts) (intent, bool) {
	if !r.row.Info.PendingCreateClaim || !committed(r.row.Info) {
		return intent{}, false
	}
	return r.heal(intentRowHeal, decideClaimClear, session.MetadataPatch{"pending_create_claim": "", "pending_create_started_at": ""})
}

// armCreatingHeal is SESS-062's heal of a creating row, without the lease
// branch (v5 B5, scenario R53): a creating row with no pending-create claim,
// its runtime gone and not Wake, goes asleep. A pending create is A10's to
// roll back (CanRollback); a Wake row's start resolves it (S1). Legacy's
// one-minute stale window is dropped with v5's other time windows: the
// effect's fresh read under the name lock keeps the heal off a start still
// running, or one that just settled deferred with its runtime up.
func armCreatingHeal(r *rowFacts) (intent, bool) {
	info := r.row.Info
	if strings.TrimSpace(info.MetadataState) != string(session.StateCreating) || info.PendingCreateClaim || !r.gone() || r.wake() {
		return intent{}, false
	}
	return r.heal(intentRowHealFresh, decideCreatingHeal, asleepHealPatch(info))
}

// armDeadRuntimeHeal is SESS-531, legacy's heal of a committed row whose
// runtime is gone (v5.8e A6 item 4), for every sessions-leg row AL1 does not
// Drain (a census-only rig-leg row is armCensusOnly's). A gone row AL1
// Drains is undesired, and A21's orphan close takes it (C5c1b). The
// heal resets a continuation no deliberate sleep ended, so the relaunch A18
// proposes for a Wake row does not resume the crashed conversation, and it
// leaves a pool row AL1 wants asleep (quarantined, held, idle-suppressed)
// asleep with runtime-missing, which A21's pool-slot close can free; a named
// row no close arm takes would otherwise read active forever. A row still
// holding a claim is armClaimClear's first, as legacy projects it
// start-pending. A heartbeat-held row waits for A17 (C5b): healed, A21 would
// close the seat legacy respawns through its hold (SESS-602).
func armDeadRuntimeHeal(r *rowFacts) (intent, bool) {
	info := r.row.Info
	if !committed(info) || !r.gone() || r.entry.Desired == desireDrain || heartbeatHeld(info, r.w.Now) {
		return intent{}, false
	}
	return r.heal(intentRowHealFresh, decideDeadRuntimeHeal, asleepHealPatch(info))
}

// heartbeatHeld reports a live hold no sleep intent explains: SESS-602's
// heartbeat hold. The timer is trimmed, as operatorDormant's.
func heartbeatHeld(info session.Info, now time.Time) bool {
	return metadataTimeInFuture(strings.TrimSpace(info.HeldUntil), now) && strings.TrimSpace(info.SleepIntent) == ""
}

// armAwakeHeal is legacy's heal of an asleep row whose own runtime is alive
// (ProjectLifecycle's alive projection): the row reads awake again. The
// inventory's identity read must find the row's token (O2 Current); the
// effect proves it again fresh. Without it a runtime started outside the
// controller on an asleep row (`gc session attach`) stays orphaned, as S1
// reads the shape as Noop. Unlike legacy, a row an operator holds dormant is
// never woken (I15, I-STOP-3/4; operatorDormant): its runtime is the
// operator-dormant stop path's (C5a3's lost-commit stop, C6b2). That covers a
// `gc session kill` that lands between a PreWake and its provider Start,
// before any runtime exists to fence.
func armAwakeHeal(r *rowFacts) (intent, bool) {
	info := r.row.Info
	if strings.TrimSpace(info.MetadataState) != string(session.StateAsleep) || !r.aliveProbed() || r.w.Obs == nil ||
		operatorDormant(info, r.w.Now) {
		return intent{}, false
	}
	obs, ok := r.w.Obs.Observation(strings.TrimSpace(info.SessionName), r.w.Now, r.w.ObsMaxAge)
	if !ok || compareIdentity(info, obs.Identity) != identityCurrent {
		return intent{}, false
	}
	return r.heal(intentRowHealFresh, decideAwakeHeal, session.MetadataPatch{"state": string(session.StateAwake)})
}

// operatorDormant reports an asleep row an operator holds dormant: a kill
// fence, a sleep an operator owns (killed, user-hold, city-stop), a user-hold
// sleep intent (a suspend), a wait hold, or a live hold or quarantine. The
// timers are trimmed here: metadataTimeInFuture, which legacy shares, does
// not trim, and a padded timer must not read as no hold.
func operatorDormant(info session.Info, now time.Time) bool {
	switch session.SleepReason(strings.TrimSpace(info.SleepReason)) {
	case session.SleepReasonKilled, session.SleepReasonUserHold, session.SleepReasonCityStop:
		return true
	}
	return session.IsKillPendingInfo(info, now) || strings.TrimSpace(info.SleepIntent) == string(session.SleepReasonUserHold) ||
		strings.TrimSpace(info.WaitHold) != "" || metadataTimeInFuture(strings.TrimSpace(info.HeldUntil), now) ||
		metadataTimeInFuture(strings.TrimSpace(info.QuarantinedUntil), now)
}

// armStabilityClear is SESS-539/540 (clearWakeFailures, clearChurn; v5.8
// A6 item 6): a committed row alive 30s past last_woke_at clears its wake
// failures, and at 5 minutes its churn count, with legacy's patches. Its
// premise, last_woke_at unchanged, is the effect's re-decide: a wake since the
// pass stamps last_woke_at after the pass time, which proposes nothing.
func armStabilityClear(r *rowFacts) (intent, bool) {
	info := r.row.Info
	woke, err := time.Parse(time.RFC3339, info.LastWokeAt)
	if !committed(info) || !r.aliveProbed() || err != nil || r.w.Now.Sub(woke) < stabilityThreshold {
		return intent{}, false
	}
	patch := session.MetadataPatch{}
	if info.WakeAttemptsMetadata != "" && info.WakeAttemptsMetadata != "0" {
		patch["wake_attempts"] = "0"
	}
	if info.QuarantinedUntil != "" {
		patch["quarantined_until"] = ""
	}
	if info.ChurnCount != "" && info.ChurnCount != "0" && r.w.Now.Sub(woke) >= churnProductivityThreshold {
		patch["churn_count"] = "0"
	}
	if len(patch) == 0 {
		return intent{}, false
	}
	return r.heal(intentRowHeal, decideStabilityClear, patch)
}

// armDetachedAt is SESS-533/535/536 (reconcileDetachedAtInfo; v5.8 A6 item
// 7): on a committed, alive row whose resolved sleep policy is interactive,
// enabled and Full, the inventory's attach fact clears detached_at while
// attached and stamps the pass time once detached; on any other row a set
// detached_at clears. An uncertain attach on an alive row under such a
// policy, committed or not, writes nothing (SESS-534), nor does a row whose
// liveness is unknown or whose process probe failed, which legacy defers
// (SESS-056). Its premise, detached_at unchanged, is the re-decide.
func armDetachedAt(r *rowFacts) (intent, bool) {
	info, e := r.row.Info, r.entry
	if e == nil || e.Liveness == livenessUnknown || r.probeFailed() {
		return intent{}, false
	}
	set := info.DetachedAt != ""
	policy := r.w.SleepPolicies[info.ID]
	tracked := policy.Class != config.SessionSleepNonInteractive && policy.enabled() && policy.Capability == runtime.SessionSleepCapabilityFull
	obs, stamp := r.w.Observed[r.k], ""
	switch {
	case tracked && e.Liveness.alive() && obs.Uncertain:
		return intent{}, false
	case tracked && committed(info) && e.Liveness.alive() && !obs.Attached:
		if set {
			return intent{}, false
		}
		stamp = r.w.Now.UTC().Format(time.RFC3339)
	case !set:
		return intent{}, false
	}
	return r.heal(intentRowHeal, decideDetachedAt, session.MetadataPatch{"detached_at": stamp})
}

// armStrandedClear is SESS-603 (clearStrandedEventMarker): an alive row ends
// its stranding episode, so the next one ages a fresh marker.
func armStrandedClear(r *rowFacts) (intent, bool) {
	if !r.aliveProbed() || strings.TrimSpace(r.row.Info.StrandedEventEmittedAt) == "" {
		return intent{}, false
	}
	return r.heal(intentRowHeal, decideStrandedClear, session.MetadataPatch{strandedEventEmittedKey: ""})
}

// armCurrentBead is SESS-613 (recordCurrentBeadIDOnWake's backstop): an
// alive Wake row records the work it is awake for. A fresh-mode row the
// allocation says needs a fresh cycle, and which has not claimed that work
// itself, is A13's (SESS-612): legacy stamps it only on that branch's own
// terms, and an early stamp would hide the reassignment the cycle reads.
func armCurrentBead(r *rowFacts) (intent, bool) {
	e, info := r.entry, r.row.Info
	if !r.aliveProbed() || e.Desired != desireWake || e.AssignedWork == nil {
		return intent{}, false
	}
	bead := strings.TrimSpace(e.AssignedWork.BeadID)
	switch {
	case bead == "" || info.CurrentlyProcessingBeadID == bead:
		return intent{}, false
	case e.AssignedWork.RequiresFreshCycle && info.WakeMode == "fresh" && strings.TrimSpace(info.CurrentClaimBeadID) != bead:
		return intent{}, false
	}
	return r.heal(intentRowHeal, decideCurrentBead, session.MetadataPatch{session.CurrentBeadIDKey: bead})
}

// asleepHealPatch is legacy's heal patch (healStatePatchWithRollbackInfo)
// for a creating row with no claim, or a committed row, whose runtime is not
// alive: asleep, and, when the row holds a continuation that no deliberate
// sleep ended, the runtime-missing reason and the continuation reset, which a
// named mode=always row skips.
func asleepHealPatch(info session.Info) session.MetadataPatch {
	patch := session.MetadataPatch{"state": string(session.StateAsleep)}
	reason := strings.TrimSpace(info.SleepReason)
	if strings.TrimSpace(info.SessionKey) == "" && strings.TrimSpace(info.StartedConfigHash) == "" || session.SleepReasonKeepsContinuation(reason) {
		return patch
	}
	if reason == "" {
		patch["sleep_reason"] = string(session.SleepReasonRuntimeMissing)
	}
	if isNamedSessionInfo(info) && namedSessionModeInfo(info) == "always" {
		return patch
	}
	for _, key := range []string{"session_key", "started_config_hash", session.PrimedAtMetadataKey, session.PrimingAttemptedAtMetadataKey, session.PromptHashMetadataKey} {
		patch[key] = ""
	}
	patch["continuation_reset_pending"] = "true"
	return patch
}
