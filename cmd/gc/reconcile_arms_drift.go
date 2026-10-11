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
// first. A named row's restart in place and relaunch (SESS-556/557), a
// launch-only relaunch (SESS-560), a live drift's RunLive (SESS-565) and
// an asleep named row's repair (SESS-566/568) come with their effects in
// C7b2; until then A14 leaves those rows alone.

// Drift decide reasons.
const (
	decideDriftResolved = "drift-resolved"          // SESS-562
	decideDriftAttached = "drift-deferred:attached" // SESS-552/553
	decideDriftPinned   = "drift-deferred:pinned"   // SESS-555
)

// armConfigDrift is A14 on an alive desired row whose stored config hash
// differs from this generation's (a missing one never drifts, #127):
//   - attached, or its attach unknown (SESS-552/553): the attached
//     deferral stamp, throttled; A19 cancels a config-drift request;
//   - attached-deferred under 5 minutes for this drift (SESS-554): defer;
//   - a pinned named row (SESS-555): the deferral stamp, and never a
//     restart; another named row is C7b2's (its restart or relaunch);
//   - a pending interaction (SESS-558) or open assigned work (SESS-559):
//     defer, A19's lenses canceling a cancelable drain;
//   - a launch-only drift its provider can relaunch is C7b2's (SESS-560);
//   - otherwise the config-drift drain (SESS-561), unless a stop request is
//     active already.
//
// An alive row whose drift resolved has its deferral keys cleared
// (SESS-562).
func armConfigDrift(r *rowFacts) (intent, bool) {
	e, info := r.entry, r.row.Info
	stored := strings.TrimSpace(info.StartedConfigHash)
	if e == nil || !e.InDesired || e.Liveness != livenessAlive || stored == "" {
		return intent{}, false
	}
	cfg, core, ok := r.w.coreHash(info)
	if !ok {
		return intent{}, false
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
	_, relaunchable := sp.(runtime.RelaunchProvider) // a conjoined runtime takes the full path
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
	case isNamedSessionInfo(info), launchOnly: // C7b2's restart or relaunch
		return intent{}, false
	case containsWakeReason(e.WakeReasons, WakePending) || e.AssignedWork != nil:
		return intent{}, false
	}
	if _, active := activeStop(r.row); active || !r.freshOwnAlive() { // the begin reads the runtime fresh
		return intent{}, false
	}
	res, _ := r.w.Templates.lookup(info)
	return r.drainBegin(drainConfigDrift, &events.Event{Type: events.SessionDraining, Actor: "gc", Subject: res.TP.DisplayName(), Message: "config drift detected", SessionID: info.ID})
}

// armExecutionStalled is A16: the execution backstop's drain request for
// the row's generation, compared as integers (C8's planner message) becomes a stop request with
// reason execution-stalled, which no lens cancels (D2).
func armExecutionStalled(r *rowFacts) (intent, bool) {
	req, posted := r.w.ExecutionStalled[r.row.Info.ID]
	gen, _ := strconv.ParseInt(strings.TrimSpace(req.Generation), 10, 64) // an empty generation reads 0, as legacy's
	if !posted || gen != r.row.Incarnation || r.entry == nil || r.entry.Liveness != livenessAlive {
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
