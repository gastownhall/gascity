package main

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// A14's restart in place and relaunch (CONTRACT v5 §4; C7b2-2), boot-gated
// and under the runtime lease. A named row first defers on its use, read
// fresh (SESS-555, legacy's shouldDeferNamedSessionConfigDrift): a pending
// interaction or an attach, unbounded; an activity it cannot read for 2m,
// recent activity for 30s, each bound stamped config_drift_deferred_{at,key}
// on first sight of its drift key. Then a relaunch (SESS-556/560) prepares
// the row's start config and relaunches its agent in the warm box, its core
// baseline moved to the config it runs (baselineRelaunch); one that fails
// falls back to the drain for an ordinary row, and to the restart for a
// named one: the stop verb's fence and Call (stopDecide, stopCall), then the
// restart's reset to start-pending (legacy's
// resetConfiguredNamedSessionForConfigDriftInfo), which the next pass
// starts.

// Restart causes.
const (
	causeDriftDeferred = "drift-deferred:" // the named row is in use; the reason follows
	causeRelaunchSkew  = "relaunch-skew"   // the prepared config no longer reads launch-only
)

// Named drift deferral bounds (legacy's namedSessionActivityThreshold and
// namedSessionRecentActivityConfigDriftDeferralLimit).
const (
	driftActivityRecent       = 2 * time.Minute
	driftActivityUnknownBound = 2 * time.Minute
	driftRecentActivityBound  = 30 * time.Second
)

var (
	driftRestartSections  = append([]section{{Decide: driftDeferStep}}, driftStopSections...)
	driftRelaunchSections = append([]section{
		{Decide: driftDeferStep},
		called(0, section{Decide: relaunchStep}, relaunchCall),
		{Decide: relaunchResultStep},
	}, driftStopSections...)
	driftStopSections = []section{
		called(callStop, section{Premise: premiseDrainBasis, Decide: stopDecide}, stopCall),
		{Decide: driftRestartStep},
	}
)

// driftRestartNeeds are the restart's fresh reads: the runtime under its
// lease, L3 and L4, and the agent's activity.
var driftRestartNeeds = needs{Runtime: true, Lease: true, Legs: legAttach | legPending, Activity: true}

// driftDeferStep is a named row's use deferral; an ordinary row's relaunch
// passes it, its guards the arm's.
func driftDeferStep(v txView) txStep {
	if !isNamedSessionInfo(v.Row) {
		return txStep{}
	}
	a := v.Fence.Activity
	switch {
	case v.Fence.Pending != "":
		return txStep{Done: true, Cause: causeDriftDeferred + v.Fence.Pending}
	case v.Fence.Attach != "":
		return txStep{Done: true, Cause: causeDriftDeferred + v.Fence.Attach}
	case a == nil || !a.Reportable:
		return driftBoundedDefer(v, "activity_unknown", driftActivityUnknownBound)
	case errors.Is(a.Err, runtime.ErrRuntimeUnavailable) || errors.Is(a.Err, errProbeExpired):
		return txStep{Refuse: fenceLivenessUnknown, Err: a.Err}
	case a.Err == nil && !a.Last.IsZero() && v.Now.Sub(a.Last) < driftActivityRecent:
		return driftBoundedDefer(v, "recent_activity", driftRecentActivityBound)
	}
	return txStep{}
}

// driftBoundedDefer is legacy's boundedNamedSessionConfigDriftDeferral: the
// stamp on first sight of the drift key (or of an unreadable stamp), a
// deferral inside limit, and the restart past it.
func driftBoundedDefer(v txView, reason string, limit time.Duration) txStep {
	key := driftKeyOf(v)
	at, ok := parseRFC3339Metadata(v.Row.ConfigDriftDeferredAt)
	switch {
	case v.Row.ConfigDriftDeferredKey != key || !ok:
		return txStep{Write: driftStamp(namedSessionConfigDriftDeferredAtMetadata, namedSessionConfigDriftDeferredKeyMetadata, key, v.Now), Done: true, Cause: causeDriftDeferred + reason}
	case v.Now.Sub(at) < limit:
		return txStep{Done: true, Cause: causeDriftDeferred + reason}
	}
	return txStep{}
}

// driftKeyOf is the row's drift key, its stored and current core hashes.
func driftKeyOf(v txView) string {
	_, core, _ := v.World.coreHash(v.Row)
	return strings.TrimSpace(v.Row.StartedConfigHash) + ":" + core
}

// relaunchPlan is what the relaunch Call runs.
type relaunchPlan struct {
	w     *World
	row   session.Info
	tp    TemplateParams
	hash  runtime.Config // the hash-form config, whose baseline a relaunch writes
	name  string
	named bool
}

// relaunchStep hands the Call the relaunch A14 still decides on the fresh
// row and runtime read.
func relaunchStep(v txView) txStep {
	w := v.World.withRow(v.It.Key, v.Row, v.Meta).withRuntime(v.It.Key, v.RT)
	if fresh, _ := decideRow(&w, v.Alloc, v.It.Key); fresh.Kind != v.It.Kind {
		return txStep{Refuse: causeRedecided}
	}
	res, _ := v.World.Templates.lookup(v.Row)
	cfg, _, _ := v.World.coreHash(v.Row)
	return txStep{Pass: relaunchPlan{w: v.World, row: v.Row, tp: res.TP, hash: cfg, name: strings.TrimSpace(v.Row.SessionName), named: isNamedSessionInfo(v.Row)}}
}

// relaunched is the relaunch Call's outcome.
type relaunched struct {
	OK    bool
	Named bool
	Hash  runtime.Config
	Err   error
}

// relaunchCall is legacy's relaunchAgentForLaunchDrift past its guards: the
// row's start config prepared over the read-only stores, the anti-skew gate
// (still launch-only, its baseline the hash form's), a key the preparation
// would mint refused (a warm relaunch must not resume a conversation never
// created), then Relaunch. Any failure is the fallback's.
func relaunchCall(ctx context.Context, c txCaps, p relaunchPlan) (relaunched, error) {
	out := relaunched{Named: p.named, Hash: p.hash}
	r, ok := c.start.sp.(runtime.RelaunchProvider)
	if !ok {
		out.Err = runtime.ErrRelaunchUnsupported
		return out, nil
	}
	transcript := transcriptOf(c.reads, p.w, p.row, p.tp)
	sources := dispatchOptionSources{claimedWork: reachableClaimedWorkProbe(seatWorkFor(c.reads.legs, releaseScope(p.row, p.w.Env.Cfg)))}
	prepared, _, err := buildPreparedStartWithTranscript(startCandidate{info: p.row, tp: p.tp}, p.w.CityPath, p.w.Env.Cfg, c.reads.city, taskWorkDirs(p.w), sources, &transcript)
	switch {
	case err != nil:
		out.Err = err
	case prepared.coreHash != runtime.CoreFingerprint(p.hash) || prepared.provisionHash != p.row.StartedProvisionHash ||
		prepared.launchHash == p.row.StartedLaunchHash || prepared.launchHash != runtime.LaunchFingerprint(p.hash):
		out.Err = errors.New(causeRelaunchSkew)
	case strings.TrimSpace(p.row.SessionKey) == "" && p.tp.ResolvedProvider != nil && strings.TrimSpace(p.tp.ResolvedProvider.SessionIDFlag) != "":
		out.Err = errors.New("relaunch would mint a speculative resume key") // legacy's preparation mints one here, then falls back
	default:
		out.Err = r.Relaunch(ctx, p.name, prepared.cfg)
	}
	out.OK = out.Err == nil
	return out, nil
}

// relaunchResultStep writes a relaunch's core baseline and legacy's
// session.updated; a failed one falls back: an ordinary row's config-drift
// drain begins in this write, a named row goes on to the restart.
func relaunchResultStep(v txView) txStep {
	out, err := callResult[relaunched](v)
	res, _ := v.World.Templates.lookup(v.Row)
	switch {
	case err != nil:
		return txStep{Fail: causeCallError, Err: err}
	case out.OK:
		step := txStep{Write: baselinePatch(out.Hash, baselineRelaunch), Done: true}
		step.Facts.Events = []events.Event{{Type: events.SessionUpdated, Actor: "gc", Subject: res.TP.DisplayName(), Message: "agent relaunched (launch-only config change)"}}
		return step
	case out.Named:
		return txStep{}
	}
	if _, active := activeStop(newCensusRow(v.It.Key, v.Row)); active {
		return txStep{Done: true, Cause: "stop-active"}
	}
	step := txStep{Write: stopBeginPatch(newCensusRow(v.It.Key, v.Row), drainConfigDrift, v.Now), Done: true}
	step.Facts.Events = []events.Event{{Type: events.SessionDraining, Actor: "gc", Subject: res.TP.DisplayName(), Message: "config drift detected", SessionID: v.Row.ID}}
	return step
}

// driftRestartStep, on a confirmed stop, resets the row to start-pending
// for its restart, with legacy's session.draining.
func driftRestartStep(v txView) txStep {
	out, err := callResult[stopped](v)
	switch {
	case err != nil:
		return txStep{Fail: causeCallError, Err: err}
	case !out.Confirmed:
		return txStep{Refuse: out.Reason}
	}
	res, _ := v.World.Templates.lookup(v.Row)
	step := txStep{Write: driftRestartPatch(res.TP, v.Row, v.Now), Done: true}
	if out.Performed {
		step.Facts.Stopped = &agentStop{Name: strings.TrimSpace(v.Row.SessionName), Identity: sessionAgentMetricIdentityInfo(v.Row, v.World.Env.Cfg)}
	}
	step.Facts.Events = []events.Event{{Type: events.SessionDraining, Actor: "gc", Subject: res.TP.DisplayName(), Message: "config drift detected", SessionID: v.Row.ID}}
	return step
}

// driftRestartPatch is legacy's restart-in-place reset to start-pending: a
// resumable conversation (its key and stored hash) is kept, so the start
// resumes it; otherwise the key is rotated as the provider takes one, or
// cleared; the deferral stamps clear.
func driftRestartPatch(tp TemplateParams, row session.Info, now time.Time) session.MetadataPatch {
	key, hash := strings.TrimSpace(row.SessionKey), strings.TrimSpace(row.StartedConfigHash)
	preserve, next, clearKey := key != "" && hash != "", key, false
	if !preserve {
		next, clearKey = "", !restartKeyMints(tp, row)
		if !clearKey {
			next, _ = session.GenerateSessionKey() // a mint error leaves the key, as legacy's
		}
	}
	patch := session.ConfigDriftRestartPatch(next, now)
	if clearKey {
		patch["session_key"] = ""
	}
	if preserve {
		patch["started_config_hash"] = hash
	}
	for _, k := range []string{namedSessionConfigDriftDeferredAtMetadata, namedSessionConfigDriftDeferredKeyMetadata, sessionAttachedConfigDriftDeferredAtMetadata, sessionAttachedConfigDriftDeferredKeyMetadata} {
		patch[k] = ""
	}
	return patch
}
