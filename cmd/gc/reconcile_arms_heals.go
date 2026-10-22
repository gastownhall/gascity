package main

import (
	"maps"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
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
// The asleep heals act on a runtime the inventory reads gone, or on a dead
// one (a corpse, or a live pane whose agent reads dead) once A12 classified
// its (row, runtime token), so its crash, rate-limit or terminal screen is
// never lost to the heal's continuation reset (SESS-057, SESS-530). A
// classified zombie heals too, its rapid crash counted, as legacy heals it
// (CONTRACT v5.9m).

// A6's other reasons.
const (
	decideClaimClear      = "claim-clear"
	decideCreatingHeal    = "creating-heal"
	decideDeadRuntimeHeal = "dead-runtime-heal"
	decideSquatterHeal    = "squatter-heal"
	decideAwakeHeal       = "awake-heal"
	decideSatisfiedWake   = "satisfied-wake-clear"
	decideSuspendHeal     = "soft-suspend-heal"
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

// freshGoneOrClassified is freshGone for A6's asleep heals, which take a
// dead runtime A12 classified too (CONTRACT A6, v5.9m): on the effect's
// fresh read, a dead object read on one session object whose token A12
// classified. A4 and A14 keep freshGone: a classified zombie is no gone
// runtime to finalize a stop over or repair drift on.
func (r *rowFacts) freshGoneOrClassified() bool {
	if r.freshGone() {
		return true
	}
	if r.fresh != nil {
		dead := r.fresh.Class == rtCorpse || r.fresh.Class == rtZombie
		return dead && r.fresh.Same && r.classified(r.fresh.Identity)
	}
	return r.entry != nil && r.entry.Liveness == livenessDead && r.classified(r.w.Observed[r.k].Identity)
}

// classified reports that A12 classified the row's dead runtime id (the
// start's recycle records its own, which no heal takes: CONTRACT A6).
func (r *rowFacts) classified(id runtimeIdentity) bool {
	c, ok := r.w.Classified[r.k]
	return ok && !c.Recycle && c.classifies(id)
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
	if strings.TrimSpace(info.MetadataState) != string(session.StateCreating) || info.PendingCreateClaim || r.wake() || !r.freshGoneOrClassified() {
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
// decideRow composes it with the row's restart (startHealed). A row with an
// acked request is A11's (ackedRequest).
func armDeadRuntimeHeal(r *rowFacts) (intent, bool) {
	info := r.row.Info
	if r.entry == nil || !committed(info) || r.entry.Desired == desireDrain || r.ackedRequest() || r.restartPending() || !r.freshGoneOrClassified() {
		return intent{}, false
	}
	patch, class, _, ev := deadRuntimePatch(r, true)
	it, ok := r.healClass(intentRowHealFresh, decideDeadRuntimeHeal, patch, class)
	it.Event = ev
	return it, ok
}

// ackedRequest reports a row whose stop request is acked and that A11
// signals: one carrying a request, a controller reason or the operator's
// drain (CONTRACT v5.10k, §12.2 row 68), or a bare ack off the floor
// (v5.10k-1). The dead-runtime heal does not take it, nor the squatter heal
// over a Foreign squatter under a name it does not own (v5.10k-2), nor does
// any A6 heal carry A18's restart (decideRow), so its runtime's death or
// squatter is no restart. A11 signals it at once (D2's ack row) and A4's verb finalizes it
// by D3's gone or Foreign path, a drain's exit, as legacy finalizes an ack
// read off a dead pane. A floor member's bare ack is row 66's.
func (r *rowFacts) ackedRequest() bool {
	req, ok := activeStop(r.row)
	floor := r.entry != nil && r.entry.Floor != nil
	return ok && req.Phase == stopAcked && (req.Reason != "" || req.Operator || !floor)
}

// armSquatterHeal is A6 item 4's occupied case (CONTRACT v5.10c, §12.2 #23,
// J40 A/B): a committed row whose name a runtime that is not its own holds
// (squatted) goes asleep with runtime-missing, for every sessions-leg row AL1
// does not Drain, so the row is not credited with the squatter. It is not a
// crash: no death accrues and the conversation is kept, so a squatter can
// neither quarantine the seat nor wipe its conversation. It stops nothing.
// The row stays down while the name is held (AL1's name-occupied, S1's
// refusals) and A18 starts it once the name frees. A row with an acked
// request is A11's (ackedRequest) where D3 finalizes over the squatter: a
// Foreign one, under a name the row does not own (v5.10k-2). A bead-scoped
// name's squatter reads as the row's own (C8.2(a)), and an Ownerless one
// holds A9 and refuses L2, so either stop would only refuse: those park.
func armSquatterHeal(r *rowFacts) (intent, bool) {
	info := r.row.Info
	acked := r.ackedRequest() && !ownsName(info, strings.TrimSpace(info.SessionName)) && compareIdentity(info, r.freshIdentity()) == identityForeign
	if r.entry == nil || !committed(info) || r.entry.Desired == desireDrain || acked || !r.freshSquatted() {
		return intent{}, false
	}
	patch := session.MetadataPatch{"state": string(session.StateAsleep)}
	if strings.TrimSpace(info.SleepReason) == "" {
		patch["sleep_reason"] = string(session.SleepReasonRuntimeMissing)
	}
	return r.heal(intentRowHealFresh, decideSquatterHeal, patch)
}

// freshSquatted reports the row's name held by a runtime that is not its own
// (squatter), for a fresh kind's arm: on the pass's facts, a name the
// inventory reads occupied or Ownerless; on the effect's fresh read, the
// identity read on one present object across its brackets. An Ownerless one
// must read alive (CONTRACT v5.10c-1): the row's own exiting runtime reads
// Ownerless briefly on acp and subprocess, and its death is the dead-runtime
// heal's to count.
func (r *rowFacts) freshSquatted() bool {
	r.rests |= restRuntime
	info := r.row.Info
	if r.fresh != nil {
		return squatterProven(info, r.fresh)
	}
	o := r.w.Observed[r.k]
	if r.entry == nil || r.entry.Liveness != livenessOccupied && o.Reason != observeReasonOwnerless {
		return false
	}
	v, ok := squatter(info, o.Identity)
	return ok && (v == identityForeign || r.inventoryAlive())
}

// squatterProven is the squatter heal's fresh proof (CONTRACT v5.10c,
// v5.10c-1): one object across the read's brackets, Foreign, or Ownerless
// under a tokened row and alive.
func squatterProven(row session.Info, rt *txRuntime) bool {
	v, ok := squatter(row, rt.Identity)
	return rt.Same && ok && (v == identityForeign || rt.Alive())
}

// inventoryAlive reports the row's runtime running with its agent not read
// dead on the pass's facts (O1's alive, whoever owns it).
func (r *rowFacts) inventoryAlive() bool {
	name := strings.TrimSpace(r.row.Info.SessionName)
	return r.w.Obs.Fact(name, FactRunning, r.w.Now, r.w.ObsMaxAge).Value == ObsYes &&
		r.w.Obs.Fact(name, FactProcessAlive, r.w.Now, r.w.ObsMaxAge).Value != ObsNo
}

// squatter reports a runtime identity that proves the row's own runtime gone
// (CONTRACT v5.10c), with its verdict: Foreign, as A4/D3 read it; or
// Ownerless while the row carries a token, since every admitted runtime is
// started with RuntimeEnv and only a leaf that writes it atomically with
// liveness reads Ownerless (O2, X3; any other leaf's identity is never
// Known, so Unknown).
func squatter(row session.Info, rt runtimeIdentity) (identityVerdict, bool) {
	switch v := compareIdentity(row, rt); v {
	case identityForeign:
		return v, true
	case identityOwnerless:
		return v, strings.TrimSpace(row.InstanceToken) != ""
	default:
		return v, false
	}
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
//
// ev is what the write records once it lands: a committed manual
// conversation's session.conversation_reset{reason:resume-failed}, when a
// rapid crash of a resume launch resets it by the resume backstop.
func deadRuntimePatch(r *rowFacts, heal bool) (patch session.MetadataPatch, class session.DispositionPatch, accrued bool, ev *events.Event) {
	info := r.row.Info
	patch = session.MetadataPatch{}
	if heal {
		patch = asleepHealPatch(info)
	}
	if session.DispositionOfInfo(info).IdleStop {
		// SESS-532: an idle stop was pending, so the death is the idle
		// sleep it asked for, which accrues nothing. Legacy heals the state
		// first (its continuation reset), then writes SleepPatch(idle) over
		// it, which keeps the pre-drain sleep_policy_fingerprint, with the
		// disposition's Sleep cell.
		maps.Copy(patch, session.SleepPatch(r.w.Now, drainIdle))
		session.KeepQuarantineReason(patch, session.DispositionOfInfo(info), info.SleepReason, r.w.Now) // a quarantine's reason dominates (mc-slkxd)
		return patch, session.CellInfo(info, session.ActorController, session.DispEvent{Kind: session.EvSleep}, r.w.Now), false, nil
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
		// Legacy resets the conversation unless it is a committed manual one
		// or a probe finds its transcript: the PreWake's stale-key clear
		// probes a keyed one; nothing can probe the others, so their reset is
		// written here. A kept one meets the resume backstop (resumeBackstop):
		// a rapid crash of a resume launch counts, and the bound resets it here.
		// recordWakeFailure and failureKeepsConversation keep by the same
		// rule; change the three together.
		attempts, _ := strconv.Atoi(info.WakeAttemptsMetadata)
		if folded := foldInfo(info, patch); strings.TrimSpace(folded.SessionKey) != "" || strings.TrimSpace(folded.StartedConfigHash) != "" {
			manual := session.HasStartedManualConversation(folded)
			kept := manual || transcript.ProviderHasKeyedTranscript(sessionTranscriptProvider(nil, info))
			failures, _ := strconv.Atoi(folded.ResumeFailures)
			backstop, count := resumeBackstop(failures, kept, resumeFailure{counted: folded.ResumeLaunch == "true"})
			if !kept || backstop {
				maps.Copy(patch, session.ConversationResetPatch(true))
				count = 0
			}
			maps.Copy(patch, resumeFailuresPatch(folded, count))
			if backstop && manual {
				e := resumeFailedConversationResetEvent(folded, sessionAgentMetricIdentityInfo(folded, worldCfg(r.w)))
				ev = &e
			}
		}
		accrual = session.WakeFailureAccrualPatch(attempts, defaultMaxWakeAttempts, until)
	case session.ExitChurn:
		if folded := foldInfo(info, patch); strings.TrimSpace(folded.SessionKey) != "" && !session.HasStartedManualConversation(folded) {
			maps.Copy(patch, session.ConversationResetPatch(false))
		}
		cycles, _ := strconv.Atoi(info.ChurnCount)
		accrual = session.ChurnAccrualPatch(cycles, defaultMaxChurnCycles, until)
	case session.ExitProductiveDeath:
		if info.ChurnCount != "" && info.ChurnCount != "0" {
			patch["churn_count"] = "0"
		}
		return patch, class, false, nil
	default:
		return patch, class, false, nil
	}
	maps.Copy(patch, accrual.Over(info, r.w.Now)) // a live quarantine keeps its reason (v5.9z)
	class = accrual.Class(info, r.w.Now)
	patch["last_woke_at"] = ""
	return patch, class, true, ev
}

// worldCfg is w's city config, nil without one.
func worldCfg(w *World) *config.City {
	if w.Env == nil {
		return nil
	}
	return w.Env.Cfg
}

// heartbeatHeld reports SESS-602's heartbeat hold (Disposition.HeartbeatHeld).
func heartbeatHeld(info session.Info, now time.Time) bool {
	return session.DispositionOfInfo(info).HeartbeatHeld(now)
}

// armSoftSuspendHeal is legacy's state heal of a soft-suspended row whose
// runtime is gone (CONTRACT v5.9u): the row reads asleep, its suspended_at
// left as legacy leaves it, and decideRow composes the heal with the row's
// restart (startHealed), so its assigned work wakes it in the same pass, as
// legacy heals and wakes in one tick. A live one is armAwakeHeal's.
func armSoftSuspendHeal(r *rowFacts) (intent, bool) {
	if !softSuspended(r.row.Info, r.w.Now) || !r.freshGoneOrClassified() {
		return intent{}, false
	}
	return r.heal(intentRowHealFresh, decideSuspendHeal, session.MetadataPatch{"state": string(session.StateAsleep)})
}

// armAwakeHeal is legacy's heal of an asleep, drained or soft-suspended row
// whose own runtime is alive (ProjectLifecycle's alive projection, which
// reconciles drained as it does asleep; CONTRACT v5.9u, A6 item 5; a drained
// row the A14 generator's shifted seed 296 left alive, mc-i7efm):
// the row reads awake again. The inventory's identity read must find the
// row's token (O2 Current); the effect proves it again fresh. Without it a
// runtime started outside the controller on an asleep row (`gc session
// attach`) stays orphaned, as S1 reads the shape as Noop. Unlike legacy, a
// row an operator holds dormant is never woken (I15, I-STOP-3/4;
// Disposition.OperatorDormant): its runtime is the operator-dormant stop
// path's (C5a3's lost-commit stop, C6b2). That covers a `gc session kill`
// that lands between a PreWake and its provider Start, before any runtime
// exists to fence.
func armAwakeHeal(r *rowFacts) (intent, bool) {
	info := r.row.Info
	state := session.State(strings.TrimSpace(info.MetadataState))
	asleep := (state == session.StateAsleep || state == session.StateDrained) && !session.DispositionOfInfo(info).OperatorDormant(r.w.Now)
	if !asleep && !softSuspended(info, r.w.Now) || !r.freshOwnAlive() {
		return intent{}, false
	}
	patch := session.MetadataPatch{"state": string(session.StateAwake)}
	if wakeRequested(info) { // the heal satisfies the wake: D7 rule 2 consumes it in the same CAS
		maps.Copy(patch, session.ClearWakeRequestPatch())
	}
	return r.heal(intentRowHealFresh, decideAwakeHeal, patch)
}

// wakeRequested reports a pending explicit wake on info (D7:
// wake_request=explicit), whatever the row's state.
func wakeRequested(info session.Info) bool {
	return strings.TrimSpace(info.WakeRequest) == string(session.WakeCauseExplicit)
}

// armSatisfiedWake is A6's satisfied-wake clear (CONTRACT v5.7 D7 rule 2):
// a committed row the pass read with a pending explicit wake, whose own
// runtime is alive and Current, has had its wake satisfied, so one CAS
// clears {wake_request, wake_requested_at}, while wake_requested_at still
// equals the value the pass read (the fresh heal's premise holds the
// lifecycle facts, wake_requested_at among them). Left standing, the residue
// keeps the row wanted and never idle-sleeping, and comes back after a later
// runtime-missing heal to restart it (§12.2 #26; J21 C). An asleep row's
// wake is the awake heal's; a row an operator holds dormant is never
// touched (I15); and a row with an active stop request keeps its wake, which
// A19 reads to cancel a requested drain (DRAIN-047).
func armSatisfiedWake(r *rowFacts) (intent, bool) {
	info := r.row.Info
	if _, stopping := activeStop(r.row); stopping {
		return intent{}, false // the wake is A19's to cancel a requested drain with (DRAIN-047), not satisfied
	}
	if !committed(info) || !wakeRequested(info) || session.DispositionOfInfo(info).OperatorDormant(r.w.Now) || !r.freshOwnAlive() {
		return intent{}, false
	}
	return r.heal(intentRowHealFresh, decideSatisfiedWake, session.ClearWakeRequestPatch())
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
	maps.Copy(patch, resumeFailuresPatch(info, 0)) // a stable run ends the resume backstop's run, as legacy's clearWakeFailures
	var class session.DispositionPatch
	if info.QuarantinedUntil != "" {
		// The Quarantine cell with no deadline: the quarantine ends.
		class = session.CellInfo(info, session.ActorController, session.DispEvent{Kind: session.EvQuarantine}, r.w.Now)
	}
	if info.ChurnCount != "" && info.ChurnCount != "0" && r.w.Now.Sub(woke) >= churnProductivityThreshold {
		patch["churn_count"] = "0"
	}
	switch session.SleepReason(strings.TrimSpace(info.SleepReason)) {
	case session.SleepReasonQuarantine, session.SleepReasonRateLimit:
		// §12.2 #57: a late quarantine an awake row's commit kept ends here
		// with its sleep reason, as legacy's commit wipes it, so a later
		// death reads as a crash, not a deliberate sleep (mc-bwvzv).
		patch["sleep_reason"] = ""
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
// named mode=always row and a committed manual conversation skip
// (session.PreserveConversationOnRuntimeLoss).
func asleepHealPatch(info session.Info) session.MetadataPatch {
	patch := session.MetadataPatch{"state": string(session.StateAsleep)}
	reason := strings.TrimSpace(info.SleepReason)
	if strings.TrimSpace(info.SessionKey) == "" && strings.TrimSpace(info.StartedConfigHash) == "" || session.SleepReasonKeepsContinuation(reason) {
		return patch
	}
	if reason == "" {
		patch["sleep_reason"] = string(session.SleepReasonRuntimeMissing)
	}
	if session.PreserveConversationOnRuntimeLoss(info) {
		return patch
	}
	maps.Copy(patch, session.ConversationResetPatch(true))
	return patch
}
