package main

import (
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// decideRow's drift arms (CONTRACT v5 §4, A14 and A16; plan C7b1): config
// drift on an alive row, in legacy's order (session_reconciler.go's drift
// block), and the execution backstop's drain request (NUDGE-022). A
// version-mismatched stored hash never reaches A14: A8 rebaselines it
// first. The restart and relaunch rows propose C7b2's kinds, which have no
// effect until it lands (no-effect).

// Drift decide reasons.
const (
	decideDriftResolved = "drift-resolved"          // SESS-562
	decideDriftAttached = "drift-deferred:attached" // SESS-552/553
	decideDriftPinned   = "drift-deferred:pinned"   // SESS-555
	decideDriftRestart  = "drift-restart"           // SESS-557, C7b2's
	decideDriftRelaunch = "drift-relaunch"          // SESS-556/560, C7b2's
	decideDriftRepair   = "drift-repair"            // SESS-566/568, C7b2's
	decideLiveApply     = "live-apply"              // SESS-565, C7b2's
)

// armConfigDrift is A14 on an alive desired row whose stored config hash
// differs from this generation's (a missing one never drifts, #127):
//   - attached, or its attach unknown (SESS-552/553): the attached
//     deferral stamp, throttled; A19 cancels a config-drift request;
//   - attached-deferred under 5 minutes for this drift (SESS-554): defer;
//   - a pinned named row (SESS-555): the deferral stamp, and never a
//     restart; another named row restarts in place, or relaunches on a
//     launch-only drift its provider can relaunch (C7b2's kinds, whose
//     effect reads its activity);
//   - a pending interaction (SESS-558) or open assigned work (SESS-559):
//     defer, A19's lenses canceling a cancelable drain;
//   - a launch-only drift its provider can relaunch relaunches
//     (SESS-556/560, C7b2's kind);
//   - otherwise the config-drift drain (SESS-561), unless a stop request is
//     active already.
//
// A row whose drift resolved has its deferral keys cleared (SESS-562), and
// a live drift runs its session_live again (SESS-565, C7b2's kind). A named
// row that is not alive has its drift repaired (SESS-566/568, C7b2's kind),
// but not while creating or start-pending, where a restart in place may
// still hold its resume (legacy's pendingResumePreservingNamedRestartInfo).
func armConfigDrift(r *rowFacts) (intent, bool) {
	e, info := r.entry, r.row.Info
	stored := strings.TrimSpace(info.StartedConfigHash)
	if e == nil || !e.InDesired || stored == "" {
		return intent{}, false
	}
	cfg, core, ok := r.w.coreHash(info)
	switch {
	case !ok:
		return intent{}, false
	case e.Liveness != livenessAlive:
		if stored == core || !isNamedSessionInfo(info) || creating(info) || session.State(strings.TrimSpace(info.MetadataState)) == session.StateStartPending {
			return intent{}, false
		}
		return r.heal(intentDriftRepair, decideDriftRepair, nil) // an asleep named row's repair
	case stored == core && info.StartedLiveHash != runtime.LiveFingerprint(cfg) && (info.StartedLiveHash != "" || len(cfg.SessionLive) > 0) &&
		!runtime.IsLegacyOrMismatchedVersion(info.StartedLiveHash):
		return r.heal(intentLiveApply, decideLiveApply, nil) // a live drift's RunLive
	}
	if stored == core {
		if info.ConfigDriftDeferredAt+info.ConfigDriftDeferredKey+info.AttachedConfigDriftDeferredAt+info.AttachedConfigDriftDeferredKey == "" {
			return intent{}, false
		}
		return r.heal(intentRowHeal, decideDriftResolved, session.MetadataPatch{
			namedSessionConfigDriftDeferredAtMetadata: "", namedSessionConfigDriftDeferredKeyMetadata: "",
			sessionAttachedConfigDriftDeferredAtMetadata: "", sessionAttachedConfigDriftDeferredKeyMetadata: "",
		})
	}
	key, now, obs := stored+":"+core, r.w.Now, r.w.Observed[r.k]
	var sp runtime.Provider
	if r.w.Env != nil {
		sp = r.w.Env.SP
	}
	_, relaunchable := sp.(runtime.RelaunchProvider) // a conjoined runtime restarts instead
	launchOnly := relaunchable && info.StartedProvisionHash != "" && info.StartedLaunchHash != "" &&
		info.StartedProvisionHash == runtime.ProvisionFingerprint(cfg) && info.StartedLaunchHash != runtime.LaunchFingerprint(cfg)
	switch {
	case obs.Attached || obs.Uncertain:
		if driftStampFresh(info.AttachedConfigDriftDeferredKey, info.AttachedConfigDriftDeferredAt, key, now, sessionAttachedConfigDriftRefreshInterval) {
			return intent{}, false
		}
		return r.heal(intentRowHeal, decideDriftAttached, driftStamp(sessionAttachedConfigDriftDeferredAtMetadata, sessionAttachedConfigDriftDeferredKeyMetadata, key, now))
	case attachedConfigDriftDeferredAt(info, now, key):
		return intent{}, false
	case pinnedConfiguredNamedSessionKillProtected(info):
		if info.ConfigDriftDeferredKey == key && info.ConfigDriftDeferredAt != "" {
			return intent{}, false
		}
		return r.heal(intentRowHeal, decideDriftPinned, driftStamp(namedSessionConfigDriftDeferredAtMetadata, namedSessionConfigDriftDeferredKeyMetadata, key, now))
	case isNamedSessionInfo(info) && launchOnly:
		return r.heal(intentDriftRelaunch, decideDriftRelaunch, nil)
	case isNamedSessionInfo(info):
		return r.heal(intentDriftRestart, decideDriftRestart, nil)
	case containsWakeReason(e.WakeReasons, WakePending) || e.AssignedWork != nil || e.OpenWork != nil:
		return intent{}, false
	case launchOnly:
		return r.heal(intentDriftRelaunch, decideDriftRelaunch, nil)
	}
	if _, active := activeStop(r.row); active || !r.freshOwnAlive() { // the begin reads the runtime fresh
		return intent{}, false
	}
	res, _ := r.w.Templates.lookup(info)
	return r.drainBegin(drainConfigDrift, &events.Event{Type: events.SessionDraining, Actor: "gc", Subject: res.TP.DisplayName(), Message: "config drift detected", SessionID: info.ID})
}

// armExecutionStalled is A16: the execution backstop's drain request for
// the row's generation (C8's planner message) becomes a stop request with
// reason execution-stalled, which no lens cancels (D2).
func armExecutionStalled(r *rowFacts) (intent, bool) {
	req, posted := r.w.ExecutionStalled[r.row.Info.ID]
	if !posted || req.Generation != strconv.FormatInt(r.row.Incarnation, 10) || r.entry == nil || r.entry.Liveness != livenessAlive {
		return intent{}, false
	}
	if _, active := activeStop(r.row); active {
		return intent{}, false
	}
	return r.drainBegin(executionStalledDrainReason, nil)
}

// drainBegin is the stop request for reason by CAS (C6a2's begin effect),
// recording ev once it lands.
func (r *rowFacts) drainBegin(reason string, ev *events.Event) (intent, bool) {
	it, ok := r.heal(intentDrainBegin, decideDrainBegin+reason, stopBeginPatch(r.row, reason, r.w.Now))
	it.Event = ev
	return it, ok
}

// driftStampFresh reports a deferral stamp for key no older than limit.
func driftStampFresh(storedKey, at, key string, now time.Time, limit time.Duration) bool {
	t, ok := parseRFC3339Metadata(at)
	return ok && storedKey == key && !t.After(now) && now.Sub(t) < limit
}

// driftStamp writes a deferral stamp for key at now.
func driftStamp(atKey, keyKey, key string, now time.Time) session.MetadataPatch {
	return session.MetadataPatch{atKey: now.UTC().Format(time.RFC3339), keyKey: key}
}
