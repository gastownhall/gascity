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

// A14's restart in place and relaunch (CONTRACT v5 §4; C7b2-2), boot-gated,
// under the name's flock, the runtime lease's record taken only past the
// deferral (LeaseAt), so a deferral writes nothing. A row first defers on
// its use, read fresh: a pending interaction (but under a wait hold, a hold
// or a quarantine, legacy's pendingInteractionHoldInfo) or an attach refuses
// (SESS-553/558), and so does work on an ordinary seat (SESS-559), read
// live; the refusal backs off the drift's own key (driftBackoffKey, the
// D3 amendment), pacing the drift, not the row. A named row also defers on
// its activity (SESS-555, legacy's shouldDeferNamedSessionConfigDrift): one it cannot
// read for 2m, recent activity for 30s, each bound stamped
// config_drift_deferred_{at,key} on first sight of its drift key. Then a
// relaunch (SESS-556/560) prepares the row's start config, re-reads L3, and
// relaunches its agent in the warm box, its core baseline moved to the
// config it runs (baselineRelaunch); one that fails falls back to the
// config-drift drain's begin for an ordinary row, and to the restart for a
// named one: the stop verb's full fence and Call (stopDecide, stopCall), then
// the restart's reset to start-pending (legacy's
// resetConfiguredNamedSessionForConfigDriftInfo), which the next pass
// starts.

// Restart causes.
const (
	causeDriftDeferred = "drift-deferred:" // the named row is in use; the reason follows
	causeRelaunchSkew  = "relaunch-skew"   // the prepared config no longer reads launch-only
)

var (
	driftRestartSections  = append([]section{{Decide: driftDeferStep}}, driftStopSections...)
	driftRelaunchSections = append([]section{
		{Decide: driftDeferStep},
		called(0, section{Decide: relaunchStep}, relaunchCall),
		{Premise: premiseBaseline, Decide: relaunchResultStep}, // a heartbeat or claim during the relaunch does not hold its baseline
	}, driftStopSections...)
	driftStopSections = []section{
		called(callStop, section{Premise: premiseDrainBasis, Decide: stopDecide}, stopCall),
		{Decide: driftRestartStep},
	}
)

// driftRestartNeeds are the restart's and the relaunch's fresh reads: the
// runtime, L3, L4 and the agent's activity; the lease record past the
// deferral section, before any Call (driftLeaseAt).
var driftRestartNeeds = needs{Runtime: true, Lease: true, LeaseAt: driftLeaseAt, Legs: legAttach | legPending, Activity: true}

// driftLeaseAt is the first section past the deferral.
const driftLeaseAt = 1

// driftNeeds adds L5 for an ordinary row's relaunch, whose seat holds no
// relaunch or fallback drain over work (SESS-559); a named restart reads
// no work.
func driftNeeds(w *World, it intent) needs {
	n := driftRestartNeeds
	if it.Kind == intentDriftRelaunch && w.Census != nil {
		if row, ok := w.Census.Rows[it.Key]; ok && !isNamedSessionInfo(row.Info) {
			n.Legs |= legWork
		}
	}
	return n
}

// driftDeferStep is the use deferral: a pending interaction or an attach,
// read fresh, refuses, and so does work on an ordinary row's seat, read
// live (SESS-559); a named row's activity defers it, bounded.
func driftDeferStep(v txView) txStep {
	a := v.Fence.Activity
	switch {
	case v.Fence.Pending != "" && !pendingExempt(v.Row, v.Now):
		return txStep{Refuse: causeDriftDeferred + v.Fence.Pending}
	case v.Fence.Attach != "":
		return txStep{Refuse: causeDriftDeferred + v.Fence.Attach}
	case !isNamedSessionInfo(v.Row) && v.Fence.workHeld():
		return txStep{Refuse: causeHasWork}
	case !isNamedSessionInfo(v.Row): // an ordinary row's relaunch: its other guards are the arm's
	case a == nil || !a.Reportable:
		return driftBoundedDefer(v, "activity_unknown", namedSessionActivityThreshold)
	case errors.Is(a.Err, runtime.ErrRuntimeUnavailable) || errors.Is(a.Err, errProbeExpired):
		return txStep{Refuse: fenceLivenessUnknown, Err: a.Err}
	case a.Err == nil && !a.Last.IsZero() && v.Now.Sub(a.Last) < namedSessionActivityThreshold:
		return driftBoundedDefer(v, "recent_activity", namedSessionRecentActivityConfigDriftDeferralLimit)
	}
	return txStep{}
}

// pendingExempt reports legacy's pending-interaction exceptions
// (pendingInteractionHoldInfo): a wait hold, or a hold or quarantine
// blocking the row.
func pendingExempt(row session.Info, now time.Time) bool {
	if session.DispositionOfInfo(row).Wait {
		return true
	}
	in := session.LifecycleInputFromInfo(row)
	in.Runtime, in.Now = session.RuntimeFacts{Observed: true, Alive: true, Pending: true}, now
	view := session.ProjectLifecycle(in)
	return view.HasBlocker(session.BlockerHeld) || view.HasBlocker(session.BlockerQuarantined)
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
	leaf  runtime.Provider // the routed leaf, whose L3 the Call re-reads
	now   time.Time
}

// relaunchStep hands the Call the relaunch A14 still decides on the fresh
// row and runtime read, and that the fence's legs this section read under
// the lease record pass (v5.9g; STALE-READ 7.2): the deferral's reads came
// before the record, so a pending interaction, an attach or work that lands
// between them holds the Relaunch here. The legs are driftNeeds' (L5 on an
// ordinary row only), less L4 where legacy's pending exemption applies, as
// the deferral reads it.
func relaunchStep(v txView) txStep {
	legs := driftNeeds(v.World, v.It).Legs
	if pendingExempt(v.Row, v.Now) {
		legs &^= legPending
	}
	if fv := fenceDestructive(v.RT, v.Fence, fenceRequest{Row: v.Row, Legs: legs}); !fv.Proceed {
		return txStep{Refuse: causeDriftDeferred + fv.Reason}
	}
	w := v.World.withRow(v.It.Key, v.Row, v.Meta).withRuntime(v.It.Key, v.RT)
	if fresh, _ := decideRow(&w, v.Alloc, v.It.Key); fresh.Kind != v.It.Kind {
		return txStep{Refuse: causeRedecided}
	}
	res, _ := v.World.Templates.lookup(v.Row)
	cfg, _, _ := v.World.coreHash(v.Row)
	return txStep{Pass: relaunchPlan{w: v.World, row: v.Row, tp: res.TP, hash: cfg, name: strings.TrimSpace(v.Row.SessionName), named: isNamedSessionInfo(v.Row), leaf: v.RT.leaf, now: v.Now}}
}

// relaunched is the relaunch Call's outcome.
type relaunched struct {
	OK    bool
	Named bool
	Hash  runtime.Config
	Err   error
	Held  string // L3, re-read just before Relaunch, holds it: no relaunch and no fallback
}

// relaunchCall is legacy's relaunchAgentForLaunchDrift past its guards: the
// row's start config prepared over the read-only stores, the anti-skew gate
// (still launch-only, its baseline the hash form's), a key the preparation
// would mint refused (a warm relaunch must not resume a conversation never
// created), L3 read again just before it (C8.7, as stopFenced does), then
// Relaunch. An attach holds it; any failure is the fallback's.
func relaunchCall(ctx context.Context, c txCaps, p relaunchPlan) (relaunched, error) {
	out := relaunched{Named: p.named, Hash: p.hash}
	r, ok := p.leaf.(runtime.RelaunchProvider) // the leaf the section fenced, never the composite, whose router re-routes by name (C8.7)
	if !ok {
		out.Err = runtime.ErrRelaunchUnsupported
		return out, nil
	}
	prepared, err := prepareRow(c.reads, p.w, p.row, p.tp, transcriptOf(c.reads, p.w, p.row, p.tp))
	switch {
	case err != nil:
		out.Err = err
	case prepared.coreHash != runtime.CoreFingerprint(p.hash) || !prepared.provisionMatches(p.row.StartedProvisionHash) ||
		prepared.launchMatches(p.row.StartedLaunchHash): // the core partitions into provision and launch: a matching core is the hash form's launch too
		out.Err = errors.New(causeRelaunchSkew)
	case strings.TrimSpace(p.row.SessionKey) == "" && p.tp.ResolvedProvider != nil && strings.TrimSpace(p.tp.ResolvedProvider.SessionIDFlag) != "":
		out.Err = errors.New("relaunch would mint a speculative resume key") // legacy's preparation mints one here, then falls back
	default:
		if out.Held, _ = attachLeg(ctx, p.leaf, p.name, false, p.now); out.Held != "" {
			return out, nil
		}
		out.Err = r.Relaunch(ctx, p.name, prepared.cfg)
	}
	out.OK = out.Err == nil
	return out, nil
}

// relaunchResultStep writes a relaunch's core baseline and legacy's
// session.updated; a failed one falls back: an ordinary row's config-drift
// drain begins in this write, through the begin's own checks, a named row
// goes on to the restart. An attach found just before it refuses.
func relaunchResultStep(v txView) txStep {
	out, err := callResult[relaunched](v)
	res, _ := v.World.Templates.lookup(v.Row)
	switch {
	case err != nil:
		return txStep{Fail: causeCallError, Err: err}
	case out.Held != "":
		return txStep{Refuse: out.Held}
	case out.OK:
		step := txStep{Write: baselinePatch(out.Hash, baselineRelaunch), Done: true}
		step.Facts.Events = []events.Event{{Type: events.SessionUpdated, Actor: "gc", Subject: res.TP.DisplayName(), Message: "agent relaunched (launch-only config change)"}}
		return step
	case out.Named:
		return txStep{}
	}
	row := v.World.Census.reread(v.It.Key, v.Row, v.Meta) // the fresh row with its stop keys
	if _, active := activeStop(row); active {
		return txStep{Refuse: causeStopActive}
	}
	// The fallback rests on the drift the pass decided, on the row read
	// before the relaunch: an operator's kill or hold since then makes the
	// row another arm's, and the fresh row decides (CONTRACT v5.10e;
	// the A14 generator's seeds 784, 958, 554, 683, 368, 238, 74, mc-i7efm).
	w := v.World.withRow(v.It.Key, v.Row, v.Meta).withRuntime(v.It.Key, v.RT)
	if fresh, _ := decideRow(&w, v.Alloc, v.It.Key); fresh.Kind != v.It.Kind {
		return txStep{Refuse: causeRedecided}
	}
	step := txStep{Write: stopBeginPatch(row, drainConfigDrift, v.Now), Done: true}
	step.Facts.Events = []events.Event{{Type: events.SessionDraining, Actor: "gc", Subject: res.TP.DisplayName(), Message: "config drift detected", SessionID: v.Row.ID}}
	return drainBeginChecks(v, drainConfigDrift, step)
}

// causeStopActive: a stop request stands; the fallback drain begins none.
const causeStopActive = "stop-active"

// driftRestartStep, on a confirmed stop that its own runtime read still
// finds gone (goneNow), resets the row to start-pending for its restart,
// with legacy's session.draining.
func driftRestartStep(v txView) txStep {
	out, err := callResult[stopped](v)
	switch {
	case err != nil:
		return txStep{Fail: causeCallError, Err: err}
	case !out.Confirmed:
		return txStep{Refuse: out.Reason}
	case !goneNow(v):
		return txStep{Refuse: causeRuntimePresent}
	}
	res, _ := v.World.Templates.lookup(v.Row)
	step := txStep{Write: driftRestartPatch(res.TP, v.Row, v.Now), Done: true} // legacy's drift kill records no stop metric
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
