package main

import (
	"maps"
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
// version-mismatched stored hash is A8's to rebaseline, never A14's to
// repair or drain. A live drift's RunLive (SESS-565) and an asleep named
// row's repair (SESS-566/568) are C7b2-1's; a named row's restart in place
// and relaunch (SESS-556/557) and a launch-only relaunch (SESS-560) come
// with the stop verb (C7b2-2), and until then A14 leaves those rows alone.

// Drift decide reasons.
const (
	decideDriftResolved = "drift-resolved"          // SESS-562
	decideDriftAttached = "drift-deferred:attached" // SESS-552/553
	decideDriftPinned   = "drift-deferred:pinned"   // SESS-555
	decideDriftRepair   = "drift-repair"            // SESS-566/568
	decideLiveApply     = "live-apply"              // SESS-565
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
// (SESS-562), then a live drift has its session_live run again (SESS-565).
// A named row whose runtime reads gone has its drift repaired (SESS-566).
func armConfigDrift(r *rowFacts) (intent, bool) {
	e, info := r.entry, r.row.Info
	stored := strings.TrimSpace(info.StartedConfigHash)
	if e == nil || !e.InDesired || stored == "" {
		return intent{}, false
	}
	cfg, core, ok := r.w.coreHash(info)
	if !ok {
		return intent{}, false
	}
	if e.Liveness != livenessAlive {
		return r.driftRepair(stored, core)
	}
	if stored == core {
		if info.ConfigDriftDeferredAt+info.ConfigDriftDeferredKey+info.AttachedConfigDriftDeferredAt+info.AttachedConfigDriftDeferredKey != "" {
			return r.heal(intentRowHeal, decideDriftResolved, session.MetadataPatch{
				namedSessionConfigDriftDeferredAtMetadata: "", namedSessionConfigDriftDeferredKeyMetadata: "",
				sessionAttachedConfigDriftDeferredAtMetadata: "", sessionAttachedConfigDriftDeferredKeyMetadata: "",
			})
		}
		return r.liveApply(cfg)
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

// liveApply is SESS-565: a live hash of this binary's version that differs
// from the template's (A8, decided first, backfills an empty one with no
// session_live and rebaselines another version's) has session_live run
// again on the row's own runtime, then the live hashes recorded.
func (r *rowFacts) liveApply(cfg runtime.Config) (intent, bool) {
	stored, live := r.row.Info.StartedLiveHash, runtime.LiveFingerprint(cfg)
	if stored == live || stored == "" && len(cfg.SessionLive) == 0 || !r.freshOwnAlive() {
		return intent{}, false
	}
	return r.heal(intentLiveApply, decideLiveApply, liveHashPatch(live))
}

// liveHashPatch records live as the row's live and started live hashes.
func liveHashPatch(live string) session.MetadataPatch {
	return session.MetadataPatch{"live_hash": live, "started_live_hash": live}
}

// driftRepair is SESS-566/568: a named row whose runtime reads gone (fresh,
// in the effect) and whose stored hash, of this binary's version, drifted
// is reset asleep: its baseline and deferral stamps cleared, its
// conversation reset, its key rotated as its provider can (the effect mints
// a new one). Not a version artifact, A8's rebaseline alone (SESS-567);
// not an in-place restart still leased in creating, whose preserved hash
// names the conversation it resumes. A row the start arms start once
// repaired gets that start, the repair in its PreWake's CAS (startHealed),
// as legacy repairs and then starts it: its key cleared, so the PreWake's
// set-once fill mints the fresh conversation's, never resuming the old one
// on the drifted config.
func (r *rowFacts) driftRepair(stored, core string) (intent, bool) {
	info, now := r.row.Info, r.w.Now
	if stored == core || !isNamedSessionInfo(info) || runtime.IsLegacyOrMismatchedVersion(stored) ||
		pendingResumePreservingNamedRestartAt(info, now, r.w.startupTimeout()) || !r.freshGone() {
		return intent{}, false
	}
	res, _ := r.w.Templates.lookup(info)
	patch := session.ConfigDriftAsleepResetPatch("")
	if !restartKeyMints(res.TP, info) {
		patch["session_key"] = "" // the start launches bare, its provider minting its own
	}
	maps.Copy(patch, session.MetadataPatch{
		namedSessionConfigDriftDeferredAtMetadata: "", namedSessionConfigDriftDeferredKeyMetadata: "",
		sessionAttachedConfigDriftDeferredAtMetadata: "", sessionAttachedConfigDriftDeferredKeyMetadata: "",
	})
	repair, _ := r.heal(intentDriftRepair, decideDriftRepair, patch) // its own kind: a fresh read that decides another heal refuses

	repair.Key = r.k
	composed := repair
	composed.Patch = maps.Clone(patch)
	composed.Patch["session_key"] = ""
	if start := r.startHealed(composed); start.Kind == intentStart {
		return start, true
	}
	return repair, true
}

// pendingResumePreservingNamedRestartAt is
// pendingResumePreservingNamedRestartInfo at now, reading no clock: an
// in-place restart leased in creating, which keeps its key and hash.
func pendingResumePreservingNamedRestartAt(i session.Info, now time.Time, startupTimeout time.Duration) bool {
	switch session.State(strings.TrimSpace(i.MetadataState)) {
	case session.StateStartPending, session.StateCreating:
	default:
		return false
	}
	_, started := parseRFC3339Metadata(i.PendingCreateStartedAt)
	return i.PendingCreateClaim && strings.TrimSpace(i.SessionKey) != "" && strings.TrimSpace(i.StartedConfigHash) != "" &&
		started && pendingCreateLeaseActiveAt(i, now, startupTimeout)
}

// restartKeyMints is freshRestartSessionKeyInfo's decision, pure: a
// provider that takes a session id gets a new key (the effect mints it);
// any other has its key cleared.
func restartKeyMints(tp TemplateParams, info session.Info) bool {
	if p := tp.ResolvedProvider; p != nil && strings.TrimSpace(p.SessionIDFlag) != "" {
		return true
	}
	if p := tp.ResolvedProvider; p != nil && strings.TrimSpace(p.ResumeFlag)+strings.TrimSpace(p.ResumeCommand)+strings.TrimSpace(p.ResumeStyle) != "" {
		return false
	}
	return strings.TrimSpace(info.SessionIDFlag) != ""
}

// startupTimeout is the env's startup_timeout, config's default without one.
func (w *World) startupTimeout() time.Duration {
	if w.Env != nil && w.Env.Cfg != nil {
		if d := w.Env.Cfg.Session.StartupTimeoutDuration(); d > 0 {
			return d
		}
	}
	return 60 * time.Second
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
