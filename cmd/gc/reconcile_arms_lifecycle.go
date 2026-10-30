package main

import (
	"strings"

	"github.com/gastownhall/gascity/internal/session"
)

// decideRow's restart-requested arm (CONTRACT v5 §4 A13, v5.10q; plan
// C7a-2a): a row carrying restart_requested=true, which `gc session reset`
// writes through Manager.RequestFreshRestart and `gc runtime request-restart`
// and `gc handoff` through Manager.RequestFreshRestartFenced, goes through the stop verb with the restart
// handoff finalize (legacy's restart block, session_reconciler.go). A live
// runtime is stopped first; a gone one is confirmed gone and handed off,
// and A18 starts the row a pass later (legacy starts it in the same tick).
// The runtime's own GC_RESTART_REQUESTED marker, which request-restart and
// handoff set beside the row's, triggers it too when the row write failed
// (C7a-2b): the inventory reads it, and the stop clears it. SESS-612's
// fresh-mode cycle (reconcile_arms_fresh_cycle.go) is C7a-2c.
//
// Deprecated (SG-3; the reconciler-v2 owner's ruling of 2026-10-27,
// condition 2): the session row's restart_requested is the sole restart
// authority. Since SG-3 the CLI records it first, fenced to the caller's
// incarnation, and sets GC_RESTART_REQUESTED only as a local-mode hint. This
// arm stays unchanged for one release so that older CLI and agent binaries
// mid-upgrade, which may leave only the runtime's marker, still restart.
// Remove it in the release after the one that ships SG-3 (bead ga-fz2pkq).

// A13's reasons.
const (
	decideRestartRequested   = "restart-requested"
	decideRestartPinnedClear = "restart-requested:pinned-clear"
	decideRestartMarkerClear = "restart-requested:pinned-marker-clear"
)

// armRestartRequested is A13. An operator-dormant row is A4's to stop, and
// is never restarted from under its hold; a kill fence is A2's hold, ahead
// of it; a row AL1 drains is its drain's or close's (drained). A pinned
// configured named row whose runtime is still up and which holds no
// explicit reset (continuation_reset_pending) only has the marker cleared
// (SESS-521): a collateral request never kills it. Any other row is
// stopped, or, its runtime gone, finalized, with the handoff
// (restartHandoff).
func armRestartRequested(r *rowFacts) (intent, bool) {
	info := r.row.Info
	if !r.found {
		return intent{}, false
	}
	// A clear that failed backs off its own key; while that record is live
	// the row falls through to the rest of A13 and the later arms, and the
	// clear is decided again when it releases (the #7893 review).
	if rec, ok := r.w.Backoff[markerClearBackoffKey(r.k)]; ok && rec.live(r.w.Now) {
		r.after(rec.Until.Sub(r.w.Now))
	} else if r.pinnedMarkerClear() {
		return intent{Kind: intentMarkerClear, Reason: decideRestartMarkerClear, Basis: rowBasis{Incarnation: r.row.Incarnation, InstanceToken: r.row.InstanceToken}}, true
	}
	if !r.restartPending() {
		return r.freshCycle() // SESS-612 (C7a-2c)
	}
	if r.pinnedCollateral() && r.runtimeUp() { // the row's marker: restartPending ignores the runtime's here
		return r.heal(intentRowHeal, decideRestartPinnedClear, session.MetadataPatch{"restart_requested": ""})
	}
	res, _ := r.w.Templates.lookup(info)
	it := intent{Kind: intentStop, Reason: decideRestartRequested, Basis: rowBasis{Incarnation: r.row.Incarnation, InstanceToken: r.row.InstanceToken}, Finish: restartHandoffClearKey}
	if restartKeyMints(res.TP, info) {
		it.Finish = restartHandoffMintKey
	}
	if r.freshGone() {
		it.Finalize = true
	}
	return it, true
}

// restartPending reports a row A13 restarts: restart_requested, on the row
// or on its own running runtime (runtimeRestartRequested), on a row no
// operator holds dormant and AL1 does not drain. A6's dead-runtime heal
// and its composed start leave it to A13 (the #7823 review): a heal's start
// would bring up a runtime with the marker still set, which A13 would then
// stop again, and on a young row count the restart as a death. A pinned
// configured named row with no explicit reset ignores its runtime's marker
// (SESS-521: a collateral request never kills it), so the request neither
// holds the row's other arms (SESS-612's cycle) nor fires on a later unpin
// (the #7879 review; legacy clears it).
func (r *rowFacts) restartPending() bool {
	// The runtime-marker clause is deprecated (runtimeRestartRequested).
	return (r.rowRestartRequested() || r.runtimeRestartRequested() && !r.pinnedCollateral()) && !operatorDormantStop(r.row.Info, r.w.Now) && !r.drained()
}

// pinnedMarkerClear reports legacy's pinned skip of the runtime's own
// restart marker (SESS-521, mc-nm6yt): a pinned configured named row with no
// explicit reset, no row marker (A13's row-marker clear comes first), no
// operator hold and no drain, whose own running runtime carries the marker,
// has the marker cleared and keeps its runtime. Kept, the marker would fire
// the old request after an unpin. It rests on the runtime (freshOwnAlive),
// which the clear's effect reads fresh.
func (r *rowFacts) pinnedMarkerClear() bool {
	if !r.pinnedCollateral() || r.rowRestartRequested() || operatorDormantStop(r.row.Info, r.w.Now) || r.drained() || !r.runtimeRestartRequested() {
		return false
	}
	return r.freshOwnAlive()
}

// pinnedCollateral reports a pinned configured named row holding no
// explicit reset, which a restart request does not kill (SESS-521).
func (r *rowFacts) pinnedCollateral() bool {
	return pinnedConfiguredNamedSessionKillProtected(r.row.Info) && strings.TrimSpace(r.row.Info.ContinuationResetPending) != "true"
}

// rowRestartRequested reports the row's restart_requested marker.
func (r *rowFacts) rowRestartRequested() bool {
	return strings.TrimSpace(r.row.Info.RestartRequested) == "true"
}

// runtimeRestartRequested reports the runtime's own GC_RESTART_REQUESTED
// marker, as the pass's inventory read it (C7a-2b), on the row's Current
// runtime while it runs: alive, or a zombie whose pane is up, as legacy
// reads it only on a running runtime. A corpse's marker is never read, so
// a corpse the stop leaves (an attached one) restarts nothing again.
//
// Deprecated (SG-3; the reconciler-v2 owner's ruling of 2026-10-27,
// condition 2): the session row's restart_requested is the sole restart
// authority. Since SG-3 the CLI records it first, fenced to the caller's
// incarnation, and sets GC_RESTART_REQUESTED only as a local-mode hint. This
// arm stays unchanged for one release so that older CLI and agent binaries
// mid-upgrade, which may leave only the runtime's marker, still restart.
// Remove it in the release after the one that ships SG-3 (bead ga-fz2pkq).
func (r *rowFacts) runtimeRestartRequested() bool {
	if r.entry == nil || r.w.Obs == nil {
		return false
	}
	info := r.row.Info
	obs, ok := r.w.Obs.Observation(strings.TrimSpace(info.SessionName), r.w.Now, r.w.ObsMaxAge)
	running := r.entry.Liveness.alive() || r.entry.Liveness == livenessDead && obs.Running.Value == ObsYes
	return ok && running && obs.RestartRequested && compareIdentity(info, obs.Identity) == identityCurrent
}

// drained reports a row AL1 drains (orphaned, suspended, over its pool's
// demand): its drain or close settles it, and its finalize consumes the
// marker; a restart would only bring it back (the handoff's reset wakes
// it). Legacy never restarts an orphaned row, which has no template.
func (r *rowFacts) drained() bool { return r.entry != nil && r.entry.Desired == desireDrain }

// runtimeUp reports the pass's runtime as running, a dead pane included,
// which legacy's pinned skip reads (running or alive).
func (r *rowFacts) runtimeUp() bool {
	return r.entry != nil && (r.entry.Liveness.alive() || r.entry.Liveness == livenessDead)
}

// restartHandoffMintKey is the restart's finalize for a provider that
// takes a session id: a new key (legacy's freshRestartSessionKeyInfo; a
// mint error leaves the key, as legacy's does).
func restartHandoffMintKey(f finalizeFacts) finalizeChoice {
	key, _ := session.GenerateSessionKey()
	return finalizeChoice{Patch: restartHandoff(key, false, f)}
}

// restartHandoffClearKey is the restart's finalize for any other provider:
// the key cleared, so the next start is a fresh conversation.
func restartHandoffClearKey(f finalizeFacts) finalizeChoice {
	return finalizeChoice{Patch: restartHandoff("", true, f)}
}

// restartHandoff is legacy's handoff (RestartRequestPatch, at f.Now): restart_requested
// consumed, the continuation reset committed, started_config_hash and
// last_woke_at cleared so the next start is a first start that crash and
// churn trackers do not count; the key rotated or cleared; and the row
// marked asleep (ga-2fpf9z), as its runtime is confirmed gone. Never a
// close, and no disposition move: the restart is no sleep.
func restartHandoff(key string, clearKey bool, f finalizeFacts) session.MetadataPatch {
	patch := session.RestartRequestPatchAt(key, f.Now)
	if clearKey {
		patch["session_key"] = ""
	}
	patch["state"] = string(session.StateAsleep)
	return patch
}
