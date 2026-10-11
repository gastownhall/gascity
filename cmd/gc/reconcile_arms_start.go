package main

import (
	"maps"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

// decideRow's start arms (CONTRACT v5 §4, S1; plan C5b): A10's adopt and
// A17's heartbeat respawn and A18's start, whose effects are C5a1's adopt
// and bringUp, and A10's rollback (C5a2). A10's StaleSelf exit waits for
// C5a3's effect. A committed row
// whose claim outlived its commit (START-037, SESS-542) is A6's claim
// clear, not an adopt: S1 leaves a committed row alone.

// Start decide reasons. A held start names its gate.
const (
	decideAdopt             = "adopt"
	decideRollback          = "rollback"
	decideStart             = "start"
	decideHeartbeatRespawn  = "heartbeat-respawn"         // A17, SESS-602
	decideStartAccrued      = "start-held:death-accrual"  // SESS-537/538: legacy restarts the next tick
	decideStartFailedCreate = "start-held:failed-create"  // SESS-605
	decideStartQuarantine   = "start-held:quarantine"     // SESS-606
	decideStartEpisode      = "start-held:startup-health" // SESS-607, #46
	decideStartProviderRed  = "start-held:provider-red"   // SESS-610, GUAR-034
)

// armAdopt is A10's adopt: an uncommitted intent (a creating row) whose own
// runtime reads alive and Current is committed, never launched again.
func armAdopt(r *rowFacts) (intent, bool) {
	if !creating(r.row.Info) {
		return intent{}, false
	}
	if rested := r.rests; !r.freshOwnAlive() {
		r.rests = rested // the next arm rests on its own reads
		return intent{}, false
	}
	basis := rowBasis{Incarnation: r.row.Incarnation, InstanceToken: r.row.InstanceToken}
	return intent{Kind: intentAdopt, Reason: decideAdopt, Basis: basis}, true
}

// armRollback is A10's rollback (v5 §4 A10, C3): a pending create the
// allocation reads past its never-started deadline with nothing running
// (its rollback candidate), undesired (a Drain row, C5b-2) with its runtime
// gone or dead, or under a name another row's runtime holds. The effect reads
// the runtime fresh and attributes it (C3, O2, C8.2(a)).
func armRollback(r *rowFacts) (intent, bool) {
	e := r.entry
	if !r.row.Info.PendingCreateClaim || e == nil {
		return intent{}, false
	}
	switch {
	case e.Desired == desireNone && e.Reason == reasonRollbackCandidate:
	case e.Desired == desireDrain && e.Liveness.startCandidate():
	case e.Liveness == livenessOccupied:
	default:
		return intent{}, false
	}
	r.rests |= restRuntime
	basis := rowBasis{Incarnation: r.row.Incarnation, InstanceToken: r.row.InstanceToken}
	return intent{Kind: intentRollback, Reason: decideRollback, Basis: basis}, true
}

// armHeartbeatRespawn is A17 (SESS-602; v5.9i): a row the allocation does
// not wake because an agent heartbeat holds it (held_until with no intent),
// whose runtime is gone or dead and which still has assigned work, is
// restarted through its hold; its PreWake exempts that hold for this intent
// alone. The start gates still apply.
// TODO(R7): Disposition tells the heartbeat hold apart.
func armHeartbeatRespawn(r *rowFacts) (intent, bool) {
	e := r.entry
	if e == nil || e.Desired == desireWake || e.Desired == desireDrain || e.AssignedWork == nil || !e.Liveness.startCandidate() || !heartbeatHeld(r.row.Info, r.w.Now) {
		return intent{}, false
	}
	return r.startIntent(decideHeartbeatRespawn)
}

// armStart is A18: a row the allocation wants awake whose runtime is gone or
// dead gets a bringUp (startIntent).
func armStart(r *rowFacts) (intent, bool) {
	e := r.entry
	if e == nil || e.Desired != desireWake || !e.Liveness.startCandidate() {
		return intent{}, false
	}
	return r.startIntent(decideStart)
}

// restart is the start arms' decision on the row, for A6's dead-runtime heal
// to take: A17's, else A18's, with the reads it rests on.
func (r *rowFacts) restart() (intent, bool) {
	rests := r.rests
	defer func() { r.rests = rests }() // A6 records only what it takes
	it, ok := armHeartbeatRespawn(r)
	if !ok {
		it, ok = armStart(r)
	}
	it.Rests = r.rests
	return it, ok
}

// startIntent is a bringUp for the row, ranked oldest wake first
// (START-009), unless a gate holds it, in legacy's order: failed-create, a
// quarantine, the startup-health episode's quarantine (#46), a red provider
// (unknown health fails open). A timed hold asks for a pass at its end. The
// endpoint breaker is admission's (the intent's Endpoint). A committed row
// whose runtime is gone carries A6's dead-runtime patch, so a productive
// death restarts in the same pass, as legacy's does; one that accrues a
// wake failure or churn is recorded first and restarts the next pass, as
// legacy's, but for a stranded seat (asleepStranded). A desired pending
// create a crash interrupted launches again on its own bead (S7).
func (r *rowFacts) startIntent(reason string) (intent, bool) {
	e, info := r.entry, r.row.Info
	var patch, kept session.MetadataPatch
	if committed(info) && r.gone() {
		var accrued bool
		if patch, accrued = deadRuntimePatch(r, true); accrued && !r.asleepStranded() {
			return intent{Reason: decideStartAccrued}, true // A6 records the death first
		}
		info, kept = foldInfo(info, patch), patch
		if r.asleepStranded() {
			kept, _ = deadRuntimePatch(r, false)
		}
	}
	now := r.w.Now
	if isFailedCreateSessionInfo(info) {
		return intent{Reason: decideStartFailedCreate}, true
	}
	if until, ok := parseRFC3339Metadata(info.QuarantinedUntil); ok && now.Before(until) {
		r.after(requeueUntil(now, until))
		return intent{Reason: decideStartQuarantine}, true
	}
	name := strings.TrimSpace(info.SessionName)
	if until := r.w.Episodes[startupHealthEpisodeKey(info, name)].QuarantinedUntil; now.Before(until) {
		r.after(requeueUntil(now, until))
		return intent{Reason: decideStartEpisode}, true
	}
	res, _ := r.w.Templates.lookup(r.row.Info)
	if p := res.TP.ResolvedProvider; p != nil {
		if healthy, present := r.w.ProviderHealth.check(p.Name); present && !healthy {
			return intent{Reason: decideStartProviderRed}, true
		}
	}
	r.rests |= restRuntime // bringUp reads the runtime fresh and decides on it (S1)
	basis := rowBasis{Incarnation: r.row.Incarnation, InstanceToken: r.row.InstanceToken}
	return intent{Kind: intentStart, Reason: reason, Basis: basis, Endpoint: e.Endpoint, Rank: wakeFairnessTime(startCandidate{info: r.row.Info}), Patch: patch, Kept: kept}, true
}

// startHealed is heal, an A6 row write, or the start the start arms (A17,
// A18) propose for the row once healed, carrying the heal into its
// PreWake's CAS: a heal that frees a wanted row (an expired kill fence or
// hold, a dead runtime) restarts it in the same pass, as legacy heals and
// wakes in one tick. A dead runtime's start decides on the unhealed row,
// whose death startIntent accrues. The start keeps heal (intent.Heal) for
// when it cannot run; a stranded seat's death keeps only its accrual.
func (r *rowFacts) startHealed(heal intent) intent {
	healed := *r
	if heal.Reason != decideDeadRuntimeHeal {
		healed.row.Info = foldInfo(r.row.Info, heal.Patch)
	}
	it, ok := healed.restart()
	if !ok || it.Kind != intentStart {
		return heal
	}
	patch := maps.Clone(heal.Patch)
	maps.Copy(patch, it.Patch)
	it.Patch, it.Key = patch, heal.Key
	if heal.Reason != decideDeadRuntimeHeal || !r.asleepStranded() {
		// The whole heal stays, the items A6 folded beside a dead runtime's
		// (detached_at) included; a stranded seat keeps its death's accrual
		// alone (startIntent's Kept).
		kept := maps.Clone(heal.Patch)
		maps.Copy(kept, it.Kept)
		it.Kept = kept
	}
	switch {
	case heal.Reason != decideDeadRuntimeHeal || !r.asleepStranded():
		it.Heal = &heal
	default: // a stranded seat records its death alone and stays committed, so it is recorded once
		if accrual, accrued := deadRuntimePatch(r, false); accrued {
			recorded := heal
			recorded.Patch = accrual
			it.Heal = &recorded
		}
	}
	return it
}

// asleepStranded reports a persistent pool seat with no work assigned, which
// AL1 never wakes asleep (reusablePoolSessionInfo); legacy's pool-slot close
// replaces it. Until A21 does (C5c1), its death restarts it in place, and
// the fallback that leaves it asleep for good is not its (staging J23).
func (r *rowFacts) asleepStranded() bool {
	info := r.row.Info
	if !info.PoolManaged || info.ConfiguredNamedSession || r.entry == nil || r.entry.AssignedWork != nil || r.w.Env == nil {
		return false
	}
	agent := findAgentByTemplate(r.w.Env.Cfg, r.entry.Template)
	return agent != nil && agent.Lifecycle != config.AgentLifecycleOneShot
}
