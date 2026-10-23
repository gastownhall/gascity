package main

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/workladder"
)

// The stop verb (CONTRACT v5 D3, D4, F2, F3; C6b2), A4's effect, under the
// runtime lease. Each attempt reads the runtime fresh and decides A4 again
// on it and the fresh row, so a row that left its stop is never stopped.
//
//  1. The fence, with the legs the stop's origin keeps (stopLegs): a
//     signaled request's stop-pending kill keeps L2 and L3 (L1 waived, as L2
//     reads Current; no L5, DRAIN-048/011); an operator-dormant row's
//     direct stop the full fence but L5, so an operator's suspend stops a
//     seat that holds work (J42). A fresh Foreign verdict ends the stop
//     without one: the name holds another row's runtime, the proof this
//     row's is gone (DRAIN-036/038, R52). A bead-scoped pool name is never
//     Foreign (C8.2(a), ownsName), and L2 still passes only on Current.
//  2. The Call: the fenced provider Stop of a live runtime (stopFenced), or,
//     with nothing live, C8.8 alone (confirmStopped). Stop's return decides
//     nothing; the fresh read after it does. A corpse counts as stopped, and
//     the Call removes it under the corpse exception, by exact session
//     object, when L3 passes; an attached corpse stays.
//  3. The finalize (D4): the intent's builder (Finish), else finalizePatch,
//     on the close-gate work read live (none for an idle stop). A close lands as the premise close
//     (closeSection); any other finalize is one CAS that writes its patch
//     and clears the stop request (D3 step 5). The events are legacy's:
//     session.stopped with the stop metric when a stop was performed, and
//     session.drain_acked_with_assigned_work when a drained row keeps work.

const (
	// causeDispositionRefused: the Sleep cell refused the finalize's move.
	causeDispositionRefused = "disposition-refused"
	// causeAlreadyGone: an operator-dormant stop found its runtime gone.
	causeAlreadyGone = "already-gone"
)

// stopSections are the fence and the Call, the finalize write, and the
// premise close, which only a finalize that closes reaches: a store that
// cannot close atomically still stops and finalizes every other row.
var stopSections = []section{
	called(callStop, drainBasis(probed(freshCycleProbe, stopDecideAfterProbe)), stopCall),
	called(0, probed(finalizeWorkProbe, stopFinalizeStep), passStopped),
	asStopClose(probed(finalizeWorkProbe, stopCloseStep)),
}

// stopNeeds are the stop verb's fresh reads: the runtime under its lease,
// L3 and L4 for an operator-dormant stop's full fence, and an idle stop's
// proof, its fresh activity or, for a content-clock fire, its pane.
func stopNeeds(w *World, it intent) needs {
	n := needs{Runtime: true, Lease: true, Legs: stopLegs(it) &^ (legLiveness | legToken)}
	if isIdleReason(it.Reason) {
		n.Content = w.idleView().fired[it.Key]
		n.Activity = !n.Content
	}
	return n
}

// stopLegs are the fence legs the stop's origin keeps (v5 F1, F2).
func stopLegs(it intent) fenceLegs {
	switch {
	case it.Reason == decideDormantStop || it.Kind == intentDriftRestart || it.Kind == intentDriftRelaunch: // A14's restart: the Full fence (D3)
		return legsFull
	case it.Reason == decideIdleTimeout: // a pending interaction or a claim since the pass defers it (SESS-574/576)
		return legToken | legAttach | legPending | legWork
	case it.Reason == decideIdleExhausted: // stopped over its work (SESS-575)
		return legToken | legAttach | legPending
	case it.Reason == decideRestartRequested: // A13 (v5.10u): an explicit restart stops an attached session, as legacy's
		return legToken
	}
	return legToken | legAttach
}

// drainBasis is sec with the stop's drain-basis premise.
func drainBasis(sec section) section {
	sec.Premise = premiseDrainBasis
	return sec
}

// asStopClose is sec as the stop's premise-close section.
func asStopClose(sec section) section {
	sec.Premise = premiseClose
	return sec
}

// stopPlan is what the fence hands the Call.
type stopPlan struct {
	name    string
	verdict fenceVerdict // a live runtime's fence; zero for nothing live
	names   []string
	foreign bool // a fresh Foreign verdict: finalize without a stop
	corpse  *corpseObject
	now     time.Time
	idle    bool // an idle-timeout stop clears the pane's scrollback after it (SESS-576)
	restart bool // A13's restart clears the runtime's restart marker after it (C7a-2b)
	// stalled is the claim an execution-stalled drain was requested for (the
	// backstop's marker), which a confirmed stop gives back (v5.10x).
	stalled stalledClaim
}

// stalledClaim is the claim an execution-stalled stop gives back: its bead
// and the seat that holds it. started is the seat's record that a turn began
// it (stalledClaimOf), which keeps it unless it is on a dispatch hold.
type stalledClaim struct {
	beadID  string
	row     session.Info
	started bool
}

// stalledClaimOf is the claim an execution-stalled stop of row gives back,
// and whether the seat records that a turn began it (mc-358yo, PHASE1-2
// §6.5). It runs with the work ladder off only: with it on, the stop is the
// ladder's recycle, which gives back nothing (§8 W3), and the ladder releases
// an unstarted claim itself.
//
// The execution backstop requests the stop, and its own record is
// execution_claim_started at the row's current instance token (startedValue),
// the stamp that already vetoes its drain, written from a step_started of
// the claim or from its work_started entry at the row's current generation
// (workStartedAtGen). Only that keeps the claim: work_started read whole
// names every claim the hook handed out, at any generation, and a stamp from
// an earlier incarnation names one the seat was woken on and never ran, so
// keeping either would leave the seat asleep holding the claim, the
// assigned-work wake would restart it on it, and the backstop would drain it
// again: the v5.10x loop.
func stalledClaimOf(row session.Info, meta map[string]string) stalledClaim {
	beadID := strings.TrimSpace(meta[executionClaimNudgeWorkKey])
	if beadID == "" {
		return stalledClaim{}
	}
	started := strings.TrimSpace(meta[executionClaimStartedKey]) == strings.TrimSpace(meta["instance_token"])+"|"+beadID
	return stalledClaim{beadID: beadID, row: row, started: started}
}

// corpseObject is the corpse a confirmed stop removes: its leaf and the
// session object the read saw.
type corpseObject struct {
	leaf runtime.Provider
	name string
	live runtime.Liveness
}

// stopped is the Call's outcome, which the finalize reads.
type stopped struct {
	Confirmed bool
	Performed bool   // the fence passed: a provider Stop was issued, or the passed runtime had already left the name
	Reason    string // why not confirmed
}

// stopDecide is A4 decided again on the fresh row and runtime, then the
// fence. It refuses a stop A4 no longer decides, or one the fence holds.
func stopDecide(v txView) txStep {
	w := v.World.withRow(v.It.Key, v.Row, v.Meta).withRuntime(v.It.Key, v.RT)
	// The stop's, or A14's restart that runs it; an idle stop's reason, and
	// only an idle stop's, must hold too, so the idle finalize never applies
	// to another stop (a stop request's verb turns finalize on its own).
	if fresh, _ := decideRow(&w, v.Alloc, v.It.Key); fresh.Kind != v.It.Kind || (isIdleReason(fresh.Reason) || isIdleReason(v.It.Reason)) && fresh.Reason != v.It.Reason {
		if dormantGone(v) {
			return txStep{Done: true, Cause: causeAlreadyGone} // the transaction notes the absence (noteAbsence)
		}
		return txStep{Refuse: causeRedecided}
	}
	name := strings.TrimSpace(v.Row.SessionName)
	plan := stopPlan{name: name, names: processNamesFor(v.World, v.Row), now: v.Now, idle: isIdleReason(v.It.Reason), restart: v.It.Reason == decideRestartRequested}
	if stopReason(v.It) == executionStalledDrainReason && x7Releases(v.World, v.Meta) {
		plan.stalled = stalledClaimOf(v.Row, v.Meta)
	}
	switch rt := v.RT; rt.Class {
	case rtAbsent:
		return txStep{Pass: plan}
	case rtCorpse:
		if rt.Same {
			plan.corpse = &corpseObject{leaf: rt.leaf, name: name, live: rt.Again}
		}
		return txStep{Pass: plan}
	case rtAlive, rtZombie:
	default:
		return txStep{Refuse: fenceLivenessUnknown}
	}
	// A bead-scoped pool name embeds this row's ID, so it is never Foreign
	// (C8.2(a), ownsName); L2 still passes only on Current (I3).
	if foreignUnowned(v) {
		plan.foreign = true
		return txStep{Pass: plan}
	}
	legs := stopLegs(v.It)
	if plan.idle && pendingExempt(v.Row, v.Now) { // the ladder's wait-hold exemption, legacy's pendingInteractionHoldInfo
		legs &^= legPending
	}
	fv := fenceDestructive(v.RT, v.Fence, fenceRequest{Row: v.Row, Legs: legs})
	if !fv.Proceed {
		return txStep{Refuse: fv.Reason}
	}
	if plan.idle && !idleReproved(v) {
		return txStep{Refuse: causeIdleUnproven}
	}
	plan.verdict = fv
	return txStep{Pass: plan}
}

// dormantGone reports an operator-dormant stop whose fresh read finds
// nothing under the name: the runtime is already gone (a kill's own stop
// finished after the pass read it alive). It settles as a no-op, with no row
// backoff; the transaction notes the absence, as for every effect's absent
// read, so the next pass decides the row on it (CONTRACT v5.10d (a),
// v5.10g). Any other redecided stop keeps its backoff.
func dormantGone(v txView) bool {
	return v.It.Reason == decideDormantStop && v.RT != nil && v.RT.Class == rtAbsent
}

// stopCall stops a live runtime through its fence, or, with nothing live,
// confirms C8.8 alone; a confirmed corpse is then removed when L3 passes.
func stopCall(ctx context.Context, c txCaps, p stopPlan) (stopped, error) {
	sp, now := c.stop.sp, c.stop.clock.Now
	switch {
	case p.foreign:
		return stopped{Confirmed: true}, nil
	case p.verdict.Proceed:
		v, ok := stopFenced(ctx, sp, p.verdict, p.names, now)
		if ok && v.Proceed && p.idle {
			_ = sp.ClearScrollback(p.name) // legacy's, best effort
		}
		if ok {
			clearRestartMarker(sp, p)
			releaseStalledClaim(c.release, p.stalled)
		}
		return stopped{Confirmed: ok, Performed: v.Proceed, Reason: v.Reason}, nil
	}
	if p.name == "" || !confirmStopped(ctx, sp, p.names, p.name, p.now) {
		return stopped{Reason: fenceStopUnconfirmed}, nil
	}
	if k := p.corpse; k != nil {
		removeCorpse(ctx, *k, now())
	}
	clearRestartMarker(sp, p)
	releaseStalledClaim(c.release, p.stalled)
	return stopped{Confirmed: true}, nil
}

// workLadderOn reports whether w's city runs the work ladder ([daemon]
// work_ladder). With it on, an execution-stalled stop is the ladder's
// recycle, which keeps a started claim on the seat (PHASE1-2 §6.5): the
// ladder releases an unstarted claim itself, by a CAS proving it unstarted,
// and never asks a stop to.
func workLadderOn(w *World) bool {
	return w == nil || w.Env == nil || w.Env.Cfg == nil || w.Env.Cfg.Daemon.WorkLadderEnabled()
}

// x7Releases reports whether an execution-stalled stop of the row whose
// metadata is meta gives back the execution backstop's claim (X7): only
// with the ladder rolled back, and only a marker the backstops wrote for the
// row's own incarnation, and not when the row's work_ladder cell records a
// recycle at the row's generation (the switch flipped mid-recycle), or is
// unreadable: each way the stop is, or may be, the ladder's, which keeps the
// claim.
func x7Releases(w *World, meta map[string]string) bool {
	token := strings.TrimSpace(meta["instance_token"])
	if workLadderOn(w) || token == "" || strings.TrimSpace(meta[executionClaimNudgeTokenKey]) != token {
		return false
	}
	// A recycle the ladder asked for at this generation, with the switch
	// flipped to off before the stop landed, is still the ladder's; so is
	// one a cell it cannot read might record (keep the work).
	cell, err := workladder.DecodeLadder(meta[session.WorkLadderKey])
	return err == nil && (cell.V == 0 || cell.RecycledGen != rowGeneration(meta))
}

// releaseStalledClaim gives back, after a confirmed stop, the claim an
// execution-stalled drain was requested for (v5.10x): read live on the
// seat's legs, and released only while still in_progress and assigned to
// this seat (ReleaseWorkBead fences its write on both). The finalize's work
// read then finds the seat free, so it closes, and a fresh seat takes the
// claim: the drain's convergence. Without it the finalize kept the seat
// asleep holding the claim, the assigned-work wake restarted it on the same
// claim, and the backstop drained it again, in a loop (maintainer-city,
// build 42dd71f94d). Best effort: a claim it cannot release stays, as before.
func releaseStalledClaim(r *closeReleases, c stalledClaim) {
	if r == nil || c.beadID == "" {
		return
	}
	scope := releaseScope(c.row, r.cfg)
	hits, err := seatWorkFor(r.legs, scope).match(seatWorkQuery{scope: scope, statuses: []string{"in_progress"}}, false)
	if err != nil {
		fmt.Fprintf(r.stderr, "execution-stalled stop: reading %s's claim %s: %v\n", c.row.ID, c.beadID, err) //nolint:errcheck // best-effort
	}
	for _, hit := range hits {
		if hit.bead.ID != c.beadID {
			continue
		}
		if c.started && !beadmeta.HasDispatchHold(hit.bead.Labels) {
			fmt.Fprintf(r.stderr, "execution-stalled stop: %s keeps claim %s: the seat records that a turn began it\n", c.row.ID, c.beadID) //nolint:errcheck // best-effort
			continue
		}
		// The stranded repair's policy: the claim's route kept, its release
		// fenced on its status and assignee.
		out := releaseListed([]seatWorkHit{hit}, releaseStrandedRepair, releaseSeatOfInfo(c.row), r.cfg, ReleaseOpts{})
		logRelease(r.stderr, "execution-stalled", c.row.ID, out, nil)
	}
}

// clearRestartMarker clears the runtime's restart marker after A13's
// confirmed stop, best effort, as legacy's clearRestartRequested: a tmux
// session takes its marker with it, but a corpse the stop leaves, or a
// sidecar that outlives its process, keeps it.
func clearRestartMarker(sp runtime.Provider, p stopPlan) {
	if p.restart && p.name != "" {
		_ = sp.RemoveMeta(p.name, "GC_RESTART_REQUESTED")
	}
}

// removeCorpse removes a confirmed corpse under the corpse exception (v5
// F2): L3 kept, by exact session object, which the kill re-checks
// server-side. An attached corpse, or a refused kill, stays.
func removeCorpse(ctx context.Context, k corpseObject, now time.Time) {
	killer, ok := k.leaf.(runtime.SessionObjectKiller)
	if !ok {
		return
	}
	if reason, _ := attachLeg(ctx, k.leaf, k.name, false, now); reason != "" {
		return
	}
	_, _ = killer.KillCorpseObject(k.name, k.live.ObjectID, k.live.ObjectCreated)
}

// finalizeWork is the finalize's live work read: the close-gate form (v5.7
// A9), which excludes the seat's own drain step. free is proven by a clean
// read only: an error, a read that expired or panicked (the zero value),
// counts as work, so a lost read never closes the seat. anomaly is the
// work bead legacy's drain_acked_with_assigned_work names
// (drainAckClaimableAnomalyBead), when the row keeps work.
type finalizeWork struct {
	free    bool
	anomaly *beads.Bead
}

// finalizeWorkProbe reads the row's work in the close-gate form. An idle
// stop's finalize reads none (mc-ensxp): A15's builders sleep the row
// whatever its work and never close it, and the idle kill's events name no
// kept bead; its claim check is the fence's L5, read before the stop.
func finalizeWorkProbe(_ context.Context, r effectReads, v txView) (finalizeWork, error) {
	if isIdleReason(v.It.Reason) {
		return finalizeWork{}, nil
	}
	if r.legs.unusable() != nil || v.World.Env == nil {
		return finalizeWork{}, errNoReadStore
	}
	// One seat's read, live, over the pass's city work legs.
	sw := seatWorkFor(r.legs, releaseScope(v.Row, v.World.Env.Cfg))
	has, err := sessionHasOpenAssignedWorkForReachableStoreForCloseGate(sw, v.Row)
	w := finalizeWork{free: !has && err == nil}
	if has {
		if b, found, _ := drainAckClaimableAnomalyBead(sw, v.Row, v.Now); found {
			w.anomaly = &b
		}
	}
	return w, err
}

// stopChoice is the finalize's choice on the fresh row: the intent's
// builder, else finalizePatch. A work read that erred, expired or panicked
// (probeErr, or the zero value) is work.
func stopChoice(v txView, work finalizeWork, probeErr error) finalizeChoice {
	f := finalizeFacts{
		Now: v.Now, CloseIfUnassigned: closeIfUnassigned(v), HasWork: !work.free || probeErr != nil,
		OperatorHeld: session.DispositionOfInfo(v.Row).KeepsSlot(),
		HonoredWake:  honoredWake(v),
		FreshWake:    v.Row.WakeMode == "fresh", RestartRequested: v.Row.RestartRequested == "true",
	}
	if v.Alloc != nil && v.Alloc.Snapshot != nil {
		if e := v.Alloc.Snapshot.Entries[v.It.Key]; e != nil && e.AssignedWork != nil {
			f.AssignedBead = strings.TrimSpace(e.AssignedWork.BeadID)
		}
	}
	switch {
	case v.It.Finish != nil:
		return v.It.Finish(f)
	case v.It.Reason == decideDormantStop:
		return dormantChoice(v.Row, f)
	case stopReason(v.It) == drainIdle:
		return idleFinish(f)
	case stopReason(v.It) == idleRespawnDrainReason:
		return idleRespawnFinish(f)
	}
	return finalizePatch(f)
}

// closeIfUnassigned is legacy's finalize close condition
// (finalizeDrainAckStopPendingSessions): a seat the allocation no longer
// wants, or any pool seat.
func closeIfUnassigned(v txView) bool {
	var desired bool
	if v.Alloc != nil && v.Alloc.Snapshot != nil {
		e := v.Alloc.Snapshot.Entries[v.It.Key]
		desired = e != nil && e.InDesired
	}
	return !desired || isPoolManagedSessionInfo(v.Row)
}

// dormantChoice is an operator-dormant stop's finalize: finalizePatch, as
// legacy completes the drain of a managed suspend, written through the Sleep
// cell, which keeps the operator's hold; never a close, as the operator's
// state is the row's. A killed row keeps sleep_reason=killed.
func dormantChoice(row session.Info, f finalizeFacts) finalizeChoice {
	f.CloseIfUnassigned = false
	c := finalizePatch(f)
	c.Patch["drain_at"] = f.Now.UTC().Format(time.RFC3339) // legacy's drain stamps it, and its completion keeps it
	if session.SleepReason(strings.TrimSpace(row.SleepReason)) == session.SleepReasonKilled {
		c.Patch["sleep_reason"] = string(session.SleepReasonKilled)
	}
	return c
}

// honoredWake is D7 rule 4's honored wake: the row holds a pending
// explicit wake, and AL1 honored it (the pass wants the row awake).
func honoredWake(v txView) bool {
	if strings.TrimSpace(v.Row.WakeRequest) != string(session.WakeCauseExplicit) || v.Alloc == nil || v.Alloc.Snapshot == nil {
		return false
	}
	e := v.Alloc.Snapshot.Entries[v.It.Key]
	return e != nil && e.WakeHonored
}

// stopReason is a stop intent's drain reason.
func stopReason(it intent) string {
	if r, ok := strings.CutPrefix(it.Reason, decideStopFinalize); ok {
		return r
	}
	return strings.TrimPrefix(it.Reason, decideStopVerb)
}

// idleRespawnFinish is an idle-respawn drain's finalize (v5 D2, C6d): the
// row sleeps and is never closed, so resume-on-ready starts it fresh for the
// bead it was awake for. It is legacy's completion of a drain that keeps
// work (finalizePatch with work: CompleteDrainPatch(idle), through the Sleep
// cell), whatever the live work read finds; a row that keeps work never
// closes.
func idleRespawnFinish(f finalizeFacts) finalizeChoice {
	f.HasWork = true
	return finalizePatch(f)
}

// idleFinish is an idle drain's finalize (D4's idle builder): legacy's idle
// sleep, SleepPatch(idle), which keeps the sleep_policy_fingerprint the
// begin wrote (the idle latch, D2); never a close.
func idleFinish(f finalizeFacts) finalizeChoice {
	return finalizeChoice{Patch: session.SleepPatch(f.Now, drainIdle), AsSleep: true}
}

// stopCloseStep is the premise close of a confirmed stop whose finalize
// closes; one that no longer closes (work assigned since) refuses, and the
// next pass finalizes it again, as does one whose own runtime read finds a
// runtime under the name again (goneNow).
func stopCloseStep(v txView, work finalizeWork, probeErr error) txStep {
	out, _ := callResult[stopped](v)
	switch {
	case !goneNow(v):
		return txStep{Refuse: causeRuntimePresent}
	case !stopChoice(v, work, probeErr).CloseSeat:
		return txStep{Refuse: causeRedecided}
	}
	terminal := session.ClosePatch(v.Now, "drained")
	maps.Copy(terminal, stopEndPatch()) // a closed row keeps no stop request
	return txStep{Terminal: terminal, Facts: stopFacts(v, out, nil), Done: true}
}

// passStopped hands the stop's outcome to the premise close.
func passStopped(_ context.Context, _ txCaps, out stopped) (stopped, error) { return out, nil }

// stopFinalizeStep, on a confirmed stop, writes the finalize's patch and
// clears the stop request in one CAS, or, for a finalize that closes, hands
// the stop's outcome to the premise close. The Call's C8.8 confirms its own
// stop; the finalize rests on this section's own read too (goneNow), so a
// runtime under the name since is never finalized drained.
func stopFinalizeStep(v txView, work finalizeWork, probeErr error) txStep {
	out, err := callResult[stopped](v)
	switch {
	case err != nil:
		return txStep{Fail: causeCallError, Err: err}
	case !out.Confirmed:
		return txStep{Refuse: out.Reason}
	case !goneNow(v):
		return txStep{Refuse: causeRuntimePresent}
	}
	choice := stopChoice(v, work, probeErr)
	if choice.CloseSeat {
		return txStep{Pass: out}
	}
	// The choice's lifecycle patch and, for a sleep, the Sleep cell's move on
	// the fresh row in the same CAS, as legacy's ApplySleep splits it: the
	// cell keeps an operator's hold, a wait's intent and an intent it cannot
	// read, and clears the idle stop's transient.
	patch := maps.Clone(choice.Patch)                                                             // a builder may hand back a shared map
	if req, ok := activeStop(v.World.Census.reread(v.It.Key, v.Row, v.Meta)); ok || req.Residue { // the fresh row's stop keys, a residue's too (D4)
		maps.Copy(patch, stopEndPatch())
	}
	var kept *beads.Bead
	var class session.DispositionPatch
	from := session.DecodeDisposition(v.Meta)
	session.KeepQuarantineReason(patch, from, v.Row.SleepReason, v.Now) // a late quarantine's reason dominates (mc-slkxd)
	if choice.AsSleep {
		to, err := from.Apply(session.ActorController, session.DispEvent{Kind: session.EvSleep}, session.State(v.Row.MetadataState), v.Now)
		if err != nil {
			return txStep{Refuse: causeDispositionRefused, Err: err}
		}
		class, kept = to.Changed(), work.anomaly // the tx mints the cell's move into this write
	}
	return txStep{Write: patch, Class: class, Facts: stopFacts(v, out, kept), Done: true}
}

// goneNow is a gone-proof from the current attempt's own runtime read
// (STALE-READ M1, E1), never one a Call relays from an earlier section:
// nothing under the name, a corpse, or a Foreign runtime under a name the
// row does not own (foreignUnowned), which the stop leaves alone.
func goneNow(v txView) bool {
	if v.RT == nil {
		return false
	}
	switch v.RT.Class {
	case rtAbsent, rtCorpse:
		return true
	case rtAlive, rtZombie:
		return foreignUnowned(v)
	}
	return false
}

// foreignUnowned reports the read's runtime as another row's, read on one
// object, under a name this row does not own: a bead-scoped pool name
// embeds this row's ID, so it is never Foreign (C8.2(a), ownsName).
func foreignUnowned(v txView) bool {
	return !ownsName(v.Row, strings.TrimSpace(v.Row.SessionName)) && v.RT.Same && v.RT.Identity.Known && compareIdentity(v.Row, v.RT.Identity) == identityForeign
}

// stopFacts are legacy's events for a finalized stop (SESS-033/034,
// DRAIN-051/054).
func stopFacts(v txView, out stopped, kept *beads.Bead) effectFacts {
	if isIdleReason(v.It.Reason) { // legacy's idle kill: session.idle_killed and its metric (SESS-576)
		res, _ := v.World.Templates.lookup(v.Row)
		name := res.TP.DisplayName()
		return effectFacts{IdleKilled: name, Events: []events.Event{{Type: events.SessionIdleKilled, Actor: "gc", Subject: name}}}
	}
	template := cmp.Or(normalizedSessionTemplateInfo(v.Row, v.World.Env.Cfg), v.Row.Template)
	f := effectFacts{}
	if out.Performed {
		f.Stopped = &agentStop{Name: strings.TrimSpace(v.Row.SessionName), Identity: sessionAgentMetricIdentityInfo(v.Row, v.World.Env.Cfg)}
	}
	f.Events = append(f.Events, events.Event{
		Type: events.SessionStopped, Actor: "gc", Subject: template, Message: "drain acknowledged by agent",
		SessionID: v.Row.ID, Payload: api.SessionLifecyclePayloadJSON(v.Row.ID, template, "drain acknowledged"),
	})
	if kept != nil {
		f.Events = append(f.Events, events.Event{
			Type: events.SessionDrainAckedWithAssignedWork, Actor: "gc", Subject: template,
			Message: "session drain-acked while still assigned to work bead", SessionID: v.Row.ID,
			Payload: api.SessionDrainAckedWithAssignedWorkPayloadJSON(v.Row.ID, kept.ID, template, kept.Status, "drain_acked_with_assigned_work"),
		})
	}
	return f
}

// agentStop is a performed stop, for legacy's stop metric
// (telemetry.RecordAgentStop): one the fence passed, recorded as before
// mc-rq4d0 also when the runtime had left the name and no Stop was issued.
type agentStop struct{ Name, Identity string }
