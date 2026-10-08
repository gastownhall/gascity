package main

import (
	"strings"

	"github.com/gastownhall/gascity/internal/session"
)

// Arm A6's row-local heals and markers beside the timer heals (CONTRACT v5
// §4 A6). Each is a CAS through the row-write effect, which re-decides on the
// fresh row (R2) and refuses unless the row still holds the incarnation the
// pass saw, instance_token included. The two heals that write asleep because
// the runtime reads gone or dead are the fresh kind: their effect re-proves
// that under the runtime name lock before it writes (reconcile_effect_heal.go).
// No arm here runs for a row with an effect in flight: the pass skips it (R5).

// A6's other reasons.
const (
	decideClaimClear    = "claim-clear"
	decideCreatingHeal  = "creating-heal"
	decideDeadNamedHeal = "dead-named-heal"
	decideStrandedClear = "stranded-clear"
	decideCurrentBead   = "current-bead"
)

// heal is the row write of patch as kind, at the incarnation the pass saw.
func (r *rowFacts) heal(kind, reason string, patch session.MetadataPatch) (intent, bool) {
	basis := rowBasis{Incarnation: r.row.Incarnation, InstanceToken: r.row.InstanceToken}
	return intent{Kind: kind, Reason: reason, Basis: basis, Patch: patch}, true
}

// notAliveUnwanted: the row's runtime reads gone or dead, and it is not Wake.
func (r *rowFacts) notAliveUnwanted() bool {
	return r.entry != nil && r.entry.Liveness.startCandidate() && r.entry.Desired != desireWake
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
// its runtime gone or dead and not Wake, goes asleep. A pending create is
// A10's to roll back (CanRollback); a Wake row's start resolves it (S1).
// Legacy's one-minute stale window is dropped with v5's other time windows:
// the effect's fresh read under the name lock keeps the heal off a start
// still running, or one that just settled deferred with its runtime up.
func armCreatingHeal(r *rowFacts) (intent, bool) {
	info := r.row.Info
	if strings.TrimSpace(info.MetadataState) != string(session.StateCreating) || info.PendingCreateClaim || !r.notAliveUnwanted() {
		return intent{}, false
	}
	return r.heal(intentRowHealFresh, decideCreatingHeal, asleepHealPatch(info))
}

// armDeadNamedHeal is legacy's heal of a committed row whose runtime is not
// alive, kept for a named row that is not Wake (v5.2 A6): no close arm takes
// a named row, so it would otherwise read active forever. A row still holding
// a claim is armClaimClear's first, as legacy projects it start-pending.
func armDeadNamedHeal(r *rowFacts) (intent, bool) {
	info := r.row.Info
	if !committed(info) || !isNamedSessionInfo(info) || !r.notAliveUnwanted() {
		return intent{}, false
	}
	return r.heal(intentRowHealFresh, decideDeadNamedHeal, asleepHealPatch(info))
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
// alive Wake row records the work it is awake for. A fresh-mode row that the
// allocation says needs a fresh cycle is A13's (SESS-612): legacy stamps it
// only on that branch's own terms, and an early stamp would hide the
// reassignment the cycle reads.
func armCurrentBead(r *rowFacts) (intent, bool) {
	e := r.entry
	if e == nil || !e.Liveness.alive() || e.Desired != desireWake || e.AssignedWork == nil {
		return intent{}, false
	}
	bead := strings.TrimSpace(e.AssignedWork.BeadID)
	switch {
	case bead == "" || r.row.Info.CurrentlyProcessingBeadID == bead:
		return intent{}, false
	case e.AssignedWork.RequiresFreshCycle && r.row.Info.WakeMode == "fresh":
		return intent{}, false
	}
	return r.heal(intentRowHeal, decideCurrentBead, session.MetadataPatch{session.CurrentBeadIDKey: bead})
}

// continuationSleepReasons are the sleep reasons after which a runtime that
// went missing keeps its continuation (legacy's shouldResetContinuation).
var continuationSleepReasons = map[session.SleepReason]bool{
	session.SleepReasonIdle: true, session.SleepReasonIdleTimeout: true, session.SleepReasonNoWakeReason: true,
	session.SleepReasonConfigDrift: true, session.SleepReasonDrained: true, session.SleepReasonCityStop: true,
	session.SleepReasonUserHold: true, session.SleepReasonWaitHold: true, session.SleepReasonRateLimit: true,
	session.SleepReasonRuntimeMissing: true,
}

// asleepHealPatch is legacy's heal patch (healStatePatchWithRollbackInfo)
// for a creating row with no claim, or a committed row, whose runtime is not
// alive: asleep, and, when the row holds a continuation that no deliberate
// sleep ended, the runtime-missing reason and the continuation reset, which a
// named mode=always row skips.
func asleepHealPatch(info session.Info) session.MetadataPatch {
	patch := session.MetadataPatch{"state": string(session.StateAsleep)}
	reason := strings.TrimSpace(info.SleepReason)
	if strings.TrimSpace(info.SessionKey) == "" && strings.TrimSpace(info.StartedConfigHash) == "" || continuationSleepReasons[session.SleepReason(reason)] {
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
