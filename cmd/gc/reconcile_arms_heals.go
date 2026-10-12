package main

import (
	"maps"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worker/transcript"
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
	return r.healClass(kind, reason, patch, session.DispositionPatch{})
}

// healClass is heal with the disposition cells' keys class beside patch.
func (r *rowFacts) healClass(kind, reason string, patch session.MetadataPatch, class session.DispositionPatch) (intent, bool) {
	basis := rowBasis{Incarnation: r.row.Incarnation, InstanceToken: r.row.InstanceToken}
	return intent{Kind: kind, Reason: reason, Basis: basis, Patch: patch, Class: class}, true
}

// freshGone is gone for a fresh kind's arm: it rests the decision on the
// runtime, and on the effect's fresh read it is that read's absence.
func (r *rowFacts) freshGone() bool {
	r.rests |= restRuntime
	if r.fresh != nil {
		return r.fresh.Class == rtAbsent
	}
	return r.gone()
}

// freshIdentity is the row's runtime identity for a fresh kind's arm (A3's
// rekey): the inventory's read on the pass's facts; on the effect's fresh
// read, the identity read on one object across its brackets, else unknown.
func (r *rowFacts) freshIdentity() runtimeIdentity {
	r.rests |= restRuntime
	if r.fresh == nil {
		return r.w.Observed[r.k].Identity
	}
	if !r.fresh.Same {
		return runtimeIdentity{}
	}
	return r.fresh.Identity
}

// freshOwnAlive reports the row's own runtime alive, its identity Current
// (O2), for a fresh kind's arm: on the pass's facts, alive with no failed
// process probe and the inventory's identity read; on the effect's fresh
// read, alive across its identity read and that identity.
func (r *rowFacts) freshOwnAlive() bool {
	r.rests |= restRuntime
	info := r.row.Info
	if r.fresh != nil {
		return r.fresh.Alive() && compareIdentity(info, r.fresh.Identity) == identityCurrent
	}
	if !r.aliveProbed() || r.w.Obs == nil {
		return false
	}
	obs, ok := r.w.Obs.Observation(strings.TrimSpace(info.SessionName), r.w.Now, r.w.ObsMaxAge)
	return ok && compareIdentity(info, obs.Identity) == identityCurrent
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
	if strings.TrimSpace(info.MetadataState) != string(session.StateCreating) || info.PendingCreateClaim || r.wake() || !r.freshGone() {
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
// start-pending. The heal carries the death's accrual (deadRuntimePatch);
// decideRow composes it with the row's restart (startHealed).
func armDeadRuntimeHeal(r *rowFacts) (intent, bool) {
	info := r.row.Info
	if r.entry == nil || !committed(info) || r.entry.Desired == desireDrain || !r.freshGone() {
		return intent{}, false
	}
	patch, class, _ := deadRuntimePatch(r, true)
	return r.healClass(intentRowHealFresh, decideDeadRuntimeHeal, patch, class)
}

// deadRuntimePatch is legacy's write for a committed row whose runtime is
// gone, in legacy's order: the state heal (SESS-531), then the death's
// accrual (SESS-537 a wake failure inside the stability threshold, SESS-538
// churn before the productivity threshold, or a productive death's churn
// clear), each edge-triggered by clearing last_woke_at. A drain the row asked
// for is no crash. The provider-screen lane (rate limit, terminal error) is
// C5a3's classification.
//
// accrued reports a wake failure or churn recorded, after which legacy skips
// the row for the tick ("record, fold, continue"): A18 starts it on the next
// pass, as legacy's next tick does (a stranded seat's in this one:
// asleepStranded).
//
// With heal false the patch is the death's accrual alone, the row left
// committed: a stranded seat's record of its death while its restart
// cannot run (startHealed).
func deadRuntimePatch(r *rowFacts, heal bool) (patch session.MetadataPatch, class session.DispositionPatch, accrued bool) {
	info := r.row.Info
	patch = session.MetadataPatch{}
	if heal {
		patch = asleepHealPatch(info)
	}
	if strings.TrimSpace(info.SleepIntent) == sleepIntentIdle {
		// SESS-532: an idle stop was pending, so the death is the idle
		// sleep it asked for, which accrues nothing. Legacy heals the state
		// first (its continuation reset), then writes SleepPatch(idle) over
		// it, which keeps the pre-drain sleep_policy_fingerprint, with the
		// disposition's Sleep cell.
		maps.Copy(patch, session.SleepPatch(r.w.Now, drainIdle))
		return patch, session.CellInfo(info, session.ActorController, session.DispEvent{Kind: session.EvSleep}, r.w.Now), false
	}
	_, draining := activeStop(r.row)
	until := r.w.Now.Add(defaultQuarantineDuration)
	var accrual session.ExitAccrual
	switch session.DecideSessionExit(session.ExitFacts{
		SubprocessProvider: r.w.Env != nil && r.w.Env.Cfg != nil && r.w.Env.Cfg.Session.Provider == "subprocess",
		DrainPending:       draining, PendingCreateClaim: info.PendingCreateClaim, SleepReason: info.SleepReason, LastWokeAt: info.LastWokeAt,
		StartUncommitted: strings.TrimSpace(info.PendingCreateStartedAt) != "",
		Now:              r.w.Now, StabilityThreshold: stabilityThreshold, ProductivityThreshold: churnProductivityThreshold,
	}) {
	case session.ExitRapidCrash:
		// Legacy resets the conversation unless a probe finds its transcript:
		// the PreWake's stale-key clear probes a keyed one; nothing can probe
		// the others, so their reset is written here.
		if folded := foldInfo(info, patch); (strings.TrimSpace(folded.SessionKey) != "" || strings.TrimSpace(folded.StartedConfigHash) != "") &&
			!transcript.ProviderHasKeyedTranscript(sessionTranscriptProvider(nil, info)) {
			maps.Copy(patch, session.ConversationResetPatch(true))
		}
		attempts, _ := strconv.Atoi(info.WakeAttemptsMetadata)
		accrual = session.WakeFailureAccrualPatch(attempts, defaultMaxWakeAttempts, until)
	case session.ExitChurn:
		if strings.TrimSpace(foldInfo(info, patch).SessionKey) != "" {
			maps.Copy(patch, session.ConversationResetPatch(false))
		}
		cycles, _ := strconv.Atoi(info.ChurnCount)
		accrual = session.ChurnAccrualPatch(cycles, defaultMaxChurnCycles, until)
	case session.ExitProductiveDeath:
		if info.ChurnCount != "" && info.ChurnCount != "0" {
			patch["churn_count"] = "0"
		}
		return patch, class, false
	default:
		return patch, class, false
	}
	maps.Copy(patch, accrual.Patch)
	class = accrual.Class(info, r.w.Now)
	patch["last_woke_at"] = ""
	return patch, class, true
}

// heartbeatHeld reports SESS-602's heartbeat hold (Disposition.HeartbeatHeld).
func heartbeatHeld(info session.Info, now time.Time) bool {
	return session.DispositionOfInfo(info).HeartbeatHeld(now)
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
	if strings.TrimSpace(info.MetadataState) != string(session.StateAsleep) || operatorDormant(info, r.w.Now) || !r.freshOwnAlive() {
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
	var class session.DispositionPatch
	if info.QuarantinedUntil != "" {
		// The Quarantine cell with no deadline: the quarantine ends.
		class = session.CellInfo(info, session.ActorController, session.DispEvent{Kind: session.EvQuarantine}, r.w.Now)
	}
	if info.ChurnCount != "" && info.ChurnCount != "0" && r.w.Now.Sub(woke) >= churnProductivityThreshold {
		patch["churn_count"] = "0"
	}
	if len(patch) == 0 && class.Len() == 0 {
		return intent{}, false
	}
	return r.healClass(intentRowHeal, decideStabilityClear, patch, class)
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
	maps.Copy(patch, session.ConversationResetPatch(true))
	return patch
}
