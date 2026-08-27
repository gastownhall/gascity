package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/rollout"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// exactSessionOrphanCloseCandidate is the D-ORPHAN close seam's guard. It
// answers from the DURABLE ROW plus the fleet's own desired view — never from
// admission.Source — and returns the close reason legacy would have stamped, or
// "" when the row does not belong to this family.
//
// The rungs it refuses on are legacy's, in legacy's order: a desired row is
// nobody's orphan; a still-leased create belongs to PendingCreatePreserved; a
// pending-create claim in a rollback state belongs to D-STALE-CREATE, which
// legacy rolls back BEFORE it ever reaches a close; a configured named row
// whose spec is still present belongs to the preserve arm; and a drain-acked
// stop-pending row belongs to D-DRAIN.
func exactSessionOrphanCloseCandidate(
	params exactSessionStartParams,
	info sessionpkg.Info,
	response sessionpkg.PersistedResponse,
	clk clock.Clock,
) string {
	if clk == nil {
		clk = clock.Real{}
	}
	name := strings.TrimSpace(info.SessionNameMetadata)
	if response.Revision == 0 || info.Closed || name == "" || params.Config == nil {
		return ""
	}
	if !isKnownStateInfo(info) {
		return ""
	}
	if strings.TrimSpace(info.StateReason) == sessionpkg.DrainAckStopPendingReason {
		return ""
	}
	// Fail closed without a published desired view: undesiredness is the whole
	// reason this family closes anything, and an unpublished view is an absent
	// answer, not a negative one.
	if params.DesiredSessionNames == nil {
		return ""
	}
	desired := params.DesiredSessionNames()
	if desired == nil || desired[name] {
		return ""
	}
	// WD.10a sweep rule: a wake-current canonical singleton is D-WAKE's live
	// target, and no configured single-session agent generates desired-state
	// demand of its own, so undesiredness alone would reap the row an operator
	// just asked to wake.
	if wakeCurrentSingletonPreservesUndesiredRow(info, params.Config, clk.Now().UTC()) {
		return ""
	}
	if isFailedCreateSessionInfo(info) {
		if pendingCreateSessionStillLeasedInfo(info, params.Config, clk) {
			return ""
		}
		return string(sessionpkg.StateFailedCreate)
	}
	if info.PendingCreateClaim {
		return ""
	}
	if preserveConfiguredNamedSessionBeadInfo(info, params.Config, params.CityName) {
		return ""
	}
	if configuredSessionNamesWithSnapshot(params.Config, params.CityName, nil)[name] ||
		configuredNamedSessionBeadHasSpecInfo(info, params.Config, params.CityName) {
		return "suspended"
	}
	return "orphaned"
}

// reconcileExactSessionOrphanClose closes one undesired dead row, or one
// expired failed-create row, by exact key. Absence is PROVEN, never assumed:
// the D2 fresh-liveness observation must complete and must report the runtime
// both not running and not alive before any close is attempted. An incomplete
// observation is a typed refusal with zero effect — legacy's fail-closed
// behavior at the CloseOrphan/CloseFailedCreate liveness-error arms, kept
// rather than replaced.
//
// The close itself reuses closeSessionBeadIfReachableStoreUnassigned verbatim
// (DETECTOR.md §3, D-ORPHAN): it re-queries the reachable store for assigned
// work at close time, and lands the terminal metadata and the status close in
// ONE store transaction, so a row that reports closed always carries its
// terminal state (ga-igcny0.1.1 / ga-f7v2ft.78.6). The fence this handler adds
// on top is the authoritative pre-close match on revision, instance token and
// name, plus a post-close terminal witness: an effect is recorded only when the
// durable row actually shows it.
func reconcileExactSessionOrphanClose(
	ctx context.Context,
	admission sessionStartAdmission,
	params exactSessionStartParams,
	info sessionpkg.Info,
	response sessionpkg.PersistedResponse,
	clk clock.Clock,
) (exactSessionStartOwner, error) {
	if clk == nil {
		clk = clock.Real{}
	}
	stderr := params.Stderr
	if stderr == nil {
		stderr = io.Discard
	}
	yieldOrPark := func(cause error) (exactSessionStartOwner, error) {
		if params.RolloutMode == rollout.Auto {
			return exactSessionStartLegacyOwner, fmt.Errorf("%w: %w", errSessionStartLegacyFallbackRequired, cause)
		}
		return exactSessionStartKeyedOwner, cause
	}
	reason := exactSessionOrphanCloseCandidate(params, info, response, clk)
	if reason == "" {
		return exactSessionStartKeyedOwner, nil
	}
	if params.DrainTracker != nil && params.DrainTracker.get(info.ID) != nil {
		return yieldOrPark(errors.New("exact orphan close target has an active legacy drain"))
	}
	if _, ok := params.Provider.(runtime.FreshLivenessObserver); !ok {
		return yieldOrPark(errors.New("exact orphan close provider cannot prove fresh liveness"))
	}

	name := strings.TrimSpace(info.SessionNameMetadata)
	site := TraceSiteReconcilerCloseOrphan
	if reason == string(sessionpkg.StateFailedCreate) {
		site = TraceSiteReconcilerCloseFailedCreate
	}

	latest, latestResponse, readErr := getAuthoritativeSessionStartPersistedRecord(params.Store, info.ID)
	if readErr != nil || latestResponse.Revision != response.Revision || latest.Closed ||
		strings.TrimSpace(latest.InstanceToken) != strings.TrimSpace(info.InstanceToken) ||
		strings.TrimSpace(latest.SessionNameMetadata) != name ||
		exactSessionOrphanCloseCandidate(params, latest, latestResponse, clk) != reason {
		return exactSessionStartKeyedOwner, nil
	}

	processNames := drainAckStopPendingProcessNames(params.Config, latest)
	liveness := runtime.ObserveFreshLiveness(params.Provider, runtime.LivenessTarget{
		SessionID:            latest.ID,
		SessionName:          name,
		ProcessNames:         processNames,
		IncarnationStartedAt: drainAckIncarnationStartedAt(latest),
	})
	// Scan completeness proves ABSENCE; a positive observation is decisive on its
	// own. The positive arm below is not a no-op — it is the hand-off that gets a
	// live undesired session drained — and a live pane withholds the very
	// tmux-absence license (TmuxSessionProvenAbsent) the /proc sweep needs, so an
	// unconditional Complete gate parked every live undesired row above its own
	// hand-off, forever, on any busy host (ga-bxa8r). The destructive close keeps
	// its proof obligation: it is reachable only from the negative arm, which
	// still demands a COMPLETE observation.
	if liveness.Running || liveness.Alive {
		// A live undesired row is the DRAIN arm's (WD.4). It is handed over from
		// INSIDE this observation rather than released for a second admission:
		// the two arms split on exactly one fact — is the runtime alive — and a
		// second probe could disagree with the first, leaving the row owned by
		// neither arm. The drain arm records its own outcome at legacy's Orphaned
		// site; a live failed-create row is nobody's drain target and keeps the
		// kept-open refusal here.
		if exactSessionOrphanDrainReasons(reason) {
			return reconcileExactSessionOrphanDrain(ctx, admission, params, latest, reason, clk)
		}
		recordExactSessionOrphanCloseTrace(params, admission, latest, site, reason, TraceOutcomeKeptOpen, 0, false)
		return exactSessionStartKeyedOwner, nil
	}
	if !liveness.Complete {
		// Absence is not proven, and the close below is the destructive step.
		// Refuse with zero effect rather than close a bead whose runtime may still
		// be alive behind a transient provider blip (#3872-family); the condition
		// is level-triggered and re-detected.
		return yieldOrPark(errors.New("exact orphan close liveness observation is incomplete"))
	}

	if ctx != nil && ctx.Err() != nil {
		return exactSessionStartKeyedOwner, nil
	}

	closeStartedAt := time.Now()
	if !closeSessionBeadIfReachableStoreUnassigned(
		params.CityPath, params.Config, params.Store, params.RigStores, latest,
		reason, clk.Now().UTC(), stderr, false,
	) {
		// The reachable-store work guard refused, or the close transaction failed.
		// Either way nothing durable changed; the next sweep re-detects.
		recordExactSessionOrphanCloseTrace(params, admission, latest, site, reason, TraceOutcomeKeptOpen, time.Since(closeStartedAt), false)
		return exactSessionStartKeyedOwner, nil
	}
	witness, witnessResponse, witnessErr := getAuthoritativeSessionStartPersistedRecord(params.Store, latest.ID)
	if witnessErr != nil {
		return exactSessionStartKeyedOwner, fmt.Errorf("witnessing exact orphan close of %q: %w", latest.ID, witnessErr)
	}
	if !witness.Closed || witnessResponse.Metadata["close_reason"] != sessionpkg.CanonicalCloseReason(reason) {
		return exactSessionStartKeyedOwner, fmt.Errorf(
			"closing exact orphan session %q: the authoritative row is not terminal after close", latest.ID)
	}
	if params.DrainTracker != nil {
		params.DrainTracker.clearSuspendDeferral(latest.ID)
	}
	recordExactSessionOrphanCloseTrace(params, admission, witness, site, reason, TraceOutcomeClosed, time.Since(closeStartedAt), true)
	return exactSessionStartKeyedOwner, nil
}

// recordExactSessionOrphanCloseTrace fires the SAME legacy trace site the fleet
// arm fires, with effect_owner=keyed and the honest effect_applied. The WD.15
// parity join reads exactly these fields to separate the legacy, keyed, and
// detector-shadow populations on a shared cycle.
func recordExactSessionOrphanCloseTrace(
	params exactSessionStartParams,
	admission sessionStartAdmission,
	info sessionpkg.Info,
	site TraceSiteCode,
	reason string,
	outcome TraceOutcomeCode,
	duration time.Duration,
	applied bool,
) {
	if params.Trace == nil || site == "" || reason == "" {
		return
	}
	cycle := params.Trace.BeginCycle(TraceTickTriggerControl, "exact_session_orphan_close", time.Now().UTC(), params.Config)
	if cycle == nil {
		return
	}
	template := normalizedSessionTemplateInfo(info, params.Config)
	if template == "" {
		template = info.Template
	}
	cycle.recordKeyedEffect(
		site,
		TraceReasonCode(reason),
		outcome,
		"exact_session_orphan_close",
		template,
		info.ID,
		info.SessionNameMetadata,
		duration,
		map[string]any{
			"admission":         string(admission.Source),
			"admission_version": admission.Version,
			"generation":        params.Generation,
			"instance_token":    info.InstanceToken,
			"close_reason":      sessionpkg.CanonicalCloseReason(reason),
			"effect_owner":      detectorKeyedEffectOwner,
			"effect_applied":    applied,
		},
	)
	if err := cycle.End(TraceCompletionCompleted, nil); err != nil && params.Stderr != nil {
		fmt.Fprintf(params.Stderr, "session reconciler: recording exact orphan close trace: %v\n", err) //nolint:errcheck // tracing is observational
	}
}
