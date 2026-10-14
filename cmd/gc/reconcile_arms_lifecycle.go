package main

import (
	"strings"

	"github.com/gastownhall/gascity/internal/session"
)

// decideRow's restart-requested arm (CONTRACT v5 §4 A13, v5.10q; plan
// C7a-2a): a row carrying restart_requested=true, which `gc session reset`,
// `gc runtime request-restart` and `gc handoff` write through
// Manager.RequestFreshRestart, goes through the stop verb with the restart
// handoff finalize (legacy's restart block, session_reconciler.go). A live
// runtime is stopped first; a gone one is confirmed gone and handed off,
// and A18 starts the row a pass later (legacy starts it in the same tick).
// The runtime's own GC_RESTART_REQUESTED marker, and SESS-612's fresh-mode
// cycle, are post-release (C7a-2b, C7a-2c).

// A13's reasons.
const (
	decideRestartRequested   = "restart-requested"
	decideRestartPinnedClear = "restart-requested:pinned-clear"
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
	if !r.found || !r.restartPending() {
		return intent{}, false
	}
	if pinnedConfiguredNamedSessionKillProtected(info) && strings.TrimSpace(info.ContinuationResetPending) != "true" && r.runtimeUp() {
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

// restartPending reports a row A13 restarts: restart_requested on a row
// no operator holds dormant and AL1 does not drain. A6's dead-runtime heal
// and its composed start leave it to A13 (the #7823 review): a heal's start
// would bring up a runtime with the marker still set, which A13 would then
// stop again, and on a young row count the restart as a death.
func (r *rowFacts) restartPending() bool {
	return strings.TrimSpace(r.row.Info.RestartRequested) == "true" && !operatorDormantStop(r.row.Info, r.w.Now) && !r.drained()
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
