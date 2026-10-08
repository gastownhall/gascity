package main

import (
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/session"
)

// Arm A6's row-local heals and markers beside the timer heals (CONTRACT v5
// §4 A6). Each is a CAS through the row-write effect, which re-decides on the
// fresh row (R2) and refuses unless the row still holds the incarnation the
// pass saw, instance_token included. The heals that move state on what the
// runtime reads are the fresh kind: their effect re-proves the read under the
// runtime name lock before it writes (reconcile_effect_heal.go). No arm here
// runs for a row with an effect in flight: the pass skips it (R5).
//
// The asleep heals act only on a runtime the inventory reads gone. A dead
// one (a corpse, or a live pane whose agent reads dead, which a booting agent
// can) waits for A12 to classify it first, so its crash, rate-limit or
// terminal screen is never lost to the heal's continuation reset (SESS-057,
// SESS-530); once A12 lands, a classified (row, runtime token) may heal here.

// A6's other reasons.
const (
	decideClaimClear    = "claim-clear"
	decideCreatingHeal  = "creating-heal"
	decideDeadNamedHeal = "dead-named-heal"
	decideCrashHeal     = "crash-heal"
	decideAwakeHeal     = "awake-heal"
	decideStrandedClear = "stranded-clear"
	decideCurrentBead   = "current-bead"
)

// heal is the row write of patch as kind, at the incarnation the pass saw.
func (r *rowFacts) heal(kind, reason string, patch session.MetadataPatch) (intent, bool) {
	basis := rowBasis{Incarnation: r.row.Incarnation, InstanceToken: r.row.InstanceToken}
	return intent{Kind: kind, Reason: reason, Basis: basis, Patch: patch}, true
}

// gone reports that the row's runtime reads gone; wake that the row is Wake.
func (r *rowFacts) gone() bool { return r.entry != nil && r.entry.Liveness == livenessGone }
func (r *rowFacts) wake() bool { return r.entry != nil && r.entry.Desired == desireWake }

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

// armDeadNamedHeal is legacy's heal of a committed row whose runtime is
// gone, kept for a named row that is not Wake (v5.2 A6): no close arm takes a
// named row, so it would otherwise read active forever. A row still holding a
// claim is armClaimClear's first, as legacy projects it start-pending.
func armDeadNamedHeal(r *rowFacts) (intent, bool) {
	info := r.row.Info
	if !committed(info) || !isNamedSessionInfo(info) || !r.gone() || r.wake() {
		return intent{}, false
	}
	return r.heal(intentRowHealFresh, decideDeadNamedHeal, asleepHealPatch(info))
}

// armCrashHeal is SESS-531, legacy's heal on its desired path: a committed
// Wake row whose runtime is gone goes asleep with legacy's patch, which
// resets a continuation no deliberate sleep ended, so the relaunch A18
// proposes next does not resume the crashed conversation.
func armCrashHeal(r *rowFacts) (intent, bool) {
	if !committed(r.row.Info) || !r.gone() || !r.wake() {
		return intent{}, false
	}
	return r.heal(intentRowHealFresh, decideCrashHeal, asleepHealPatch(r.row.Info))
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
	if strings.TrimSpace(info.MetadataState) != string(session.StateAsleep) || r.entry == nil || !r.entry.Liveness.alive() || r.w.Obs == nil ||
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
// sleep intent (a suspend), a wait hold, or a live hold or quarantine.
func operatorDormant(info session.Info, now time.Time) bool {
	switch session.SleepReason(strings.TrimSpace(info.SleepReason)) {
	case session.SleepReasonKilled, session.SleepReasonUserHold, session.SleepReasonCityStop:
		return true
	}
	return session.IsKillPendingInfo(info, now) || strings.TrimSpace(info.SleepIntent) == string(session.SleepReasonUserHold) ||
		strings.TrimSpace(info.WaitHold) != "" || metadataTimeInFuture(info.HeldUntil, now) || metadataTimeInFuture(info.QuarantinedUntil, now)
}

// armStrandedClear is SESS-603 (clearStrandedEventMarker): an alive row ends
// its stranding episode, so the next one ages a fresh marker.
func armStrandedClear(r *rowFacts) (intent, bool) {
	if r.entry == nil || !r.entry.Liveness.alive() || strings.TrimSpace(r.row.Info.StrandedEventEmittedAt) == "" {
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
	if e == nil || !e.Liveness.alive() || e.Desired != desireWake || e.AssignedWork == nil {
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
