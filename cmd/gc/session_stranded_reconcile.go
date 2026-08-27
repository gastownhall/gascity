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

// exactSessionStrandedRepairCandidate is the D-STRANDED seam's guard and the
// handler's own pre-mutation re-verification, so the two can never disagree
// about which slot is stranded. It answers from the DURABLE ROW plus one live
// assigned-work query, never from admission.Source.
//
// The rungs are legacy's dead-pool arm, in legacy's order, minus the two facts
// no per-key predicate owns:
//
//   - pool-managed and in a freeable terminal sleep state — legacy's
//     isPoolManagedSessionInfo / isPoolSessionSlotFreeableInfo pair;
//   - a CONFIRMED stranding episode, i.e. a stranded_event_emitted_at marker
//     aged past strandedRepairConfirmGrace. The marker is durable and shared
//     with legacy, so both paths read one window rather than two counters;
//   - still holding open assigned work. Without it the row belongs to the
//     sibling clean-close arm, which stamps the preserved sleep_reason as its
//     close reason and is still legacy's for this wave.
//
// Non-liveness is deliberately NOT a rung here: it is provider I/O, so the
// handler pays it once, per key, with a fresh-liveness observation. A store
// error on the assigned-work query fails CLOSED — not this family — because the
// effect below clears a work claim, and a blip must never be allowed to clear a
// live one. Legacy's own read-error branch treats the row as stranded and then
// defers inside the repair; refusing here is the strictly safer direction and
// the condition is level-triggered.
func exactSessionStrandedRepairCandidate(
	params exactSessionStartParams,
	info sessionpkg.Info,
	response sessionpkg.PersistedResponse,
	clk clock.Clock,
) bool {
	if clk == nil {
		clk = clock.Real{}
	}
	// Revision 0 is the legacy-at-0 residual (DETECTOR.md §3b): a row no
	// conditional writer has ever fenced. Refuse it and let the first
	// unconditional write self-heal it.
	if params.Store == nil || params.Config == nil || response.Revision == 0 {
		return false
	}
	if info.Closed || strings.TrimSpace(info.SessionNameMetadata) == "" || !isKnownStateInfo(info) {
		return false
	}
	if !isPoolManagedSessionInfo(info) || !isPoolSessionSlotFreeableInfo(info) {
		return false
	}
	if !exactSessionStrandedEpisodeConfirmed(info, clk.Now().UTC()) {
		return false
	}
	has, err := sessionHasOpenAssignedWorkForReachableStore(params.CityPath, params.Config, params.Store, params.RigStores, info)
	return err == nil && has
}

// exactSessionStrandedEpisodeConfirmed mirrors repairStrandedPoolWorkerBead's
// own window test exactly, so the guard can never schedule a repair the reused
// helper would decline to make.
func exactSessionStrandedEpisodeConfirmed(info sessionpkg.Info, now time.Time) bool {
	since := strings.TrimSpace(info.StrandedEventEmittedAt)
	if since == "" {
		return false
	}
	first := parseRFC3339OrZero(since)
	return !first.IsZero() && now.Sub(first) >= strandedRepairConfirmGrace
}

// reconcileExactSessionStrandedRepair repairs one stranded pool slot by exact
// key. It adds no second repair implementation: the effect is
// repairStrandedPoolWorkerBead — the fleet pass's own helper — which unassigns
// and reopens the stranded work through unclaimWorkAssignedToRetiredSessionInfo
// (stamping the retired member's fallback run_target so the routed-work census
// can re-drive it) and only then closes the session bead. Narrowing the feed to
// one key is what turns a fleet phase into one fenced effect.
//
// That helper's ordering is load-bearing and inherited unchanged: a failed
// unassign leaves the session bead OPEN and reports no repair, so a stale
// assignee is never masked behind a "repaired" close. The worktree prune runs
// LAST and its own safety gates may refuse it — pruning is disk reclamation,
// never the step that decides whether the slot is freed.
//
// The one thing the keyed arm adds that the fleet arm cannot is proof: the
// runtime's absence is observed freshly for this key before any claim is
// cleared, and the pre-effect re-read fences on revision, instance token and
// name. A live member — one that is merely slow — is refused with zero effect.
func reconcileExactSessionStrandedRepair(
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
	if !exactSessionStrandedRepairCandidate(params, info, response, clk) {
		// Re-derived away between admission and dispatch, or carried in by some
		// other admission. Release the key with zero effect; the row is the
		// authority and the detector's reason was only a hint.
		return exactSessionStartKeyedOwner, nil
	}
	if params.DrainTracker != nil && params.DrainTracker.get(info.ID) != nil {
		return yieldOrPark(errors.New("exact stranded pool slot has an active legacy drain"))
	}
	if _, ok := params.Provider.(runtime.FreshLivenessObserver); !ok {
		return yieldOrPark(errors.New("exact stranded pool slot provider cannot prove fresh liveness"))
	}

	name := strings.TrimSpace(info.SessionNameMetadata)
	liveness := runtime.ObserveFreshLiveness(params.Provider, runtime.LivenessTarget{
		SessionID:            info.ID,
		SessionName:          name,
		ProcessNames:         drainAckStopPendingProcessNames(params.Config, info),
		IncarnationStartedAt: drainAckIncarnationStartedAt(info),
	})
	// Scan completeness proves ABSENCE; a positive observation is decisive on its
	// own. A live member holds a pane, and a live pane withholds the very
	// tmux-absence license (TmuxSessionProvenAbsent) the /proc sweep needs to
	// clear post-incarnation strangers — so an unconditional Complete gate parked
	// exactly the rows this rung exists to protect (ga-bxa8r). The refusal below
	// is the whole point: it is non-destructive, and a running worker keeps its
	// claim on the strength of being seen running.
	if liveness.Running || liveness.Alive {
		// The member is slow, not stopped. Legacy's marker survives a tick it
		// never observed alive, so this is the rung that keeps a running worker's
		// claim: record the refusal and leave the row alone.
		recordExactSessionStrandedRepairTrace(params, admission, info, TraceOutcomeKeptOpen, 0, false)
		return exactSessionStartKeyedOwner, nil
	}
	if !liveness.Complete {
		// Absence is not proven, and the repair below IS the destructive step.
		// Refuse rather than clear a claim a live member may still be working;
		// the condition is level-triggered.
		return yieldOrPark(errors.New("exact stranded pool slot liveness observation is incomplete"))
	}

	latest, latestResponse, readErr := getAuthoritativeSessionStartPersistedRecord(params.Store, info.ID)
	if readErr != nil || latestResponse.Revision != response.Revision || latest.Closed ||
		strings.TrimSpace(latest.InstanceToken) != strings.TrimSpace(info.InstanceToken) ||
		strings.TrimSpace(latest.SessionNameMetadata) != name ||
		!exactSessionStrandedRepairCandidate(params, latest, latestResponse, clk) {
		return exactSessionStartKeyedOwner, nil
	}
	if ctx != nil && ctx.Err() != nil {
		return exactSessionStartKeyedOwner, nil
	}

	repairStartedAt := time.Now()
	if !repairStrandedPoolWorkerBead(params.CityPath, params.Config, params.Store, params.RigStores, latest,
		retiredSessionFallbackRouteInfo(latest), clk, stderr) {
		// An unassign did not land, so the helper left the bead open on purpose.
		// Report the refusal instead of claiming a repair; the next sweep
		// re-detects while the episode's marker is still aged.
		recordExactSessionStrandedRepairTrace(params, admission, latest, TraceOutcomeKeptOpen, time.Since(repairStartedAt), false)
		return exactSessionStartKeyedOwner, nil
	}
	witness, _, witnessErr := getAuthoritativeSessionStartPersistedRecord(params.Store, latest.ID)
	if witnessErr != nil {
		return exactSessionStartKeyedOwner, fmt.Errorf("witnessing exact stranded repair of %q: %w", latest.ID, witnessErr)
	}
	if !witness.Closed {
		return exactSessionStartKeyedOwner, fmt.Errorf(
			"repairing exact stranded pool slot %q: the authoritative row is not terminal after close", latest.ID)
	}
	// Disk reclamation, last and non-blocking: the slot is already free.
	pruneAgentHomeWorktreeIfSafeInfo(latest, params.CityPath, params.Config, stderr)
	recordExactSessionStrandedRepairTrace(params, admission, witness, TraceOutcomeClosed, time.Since(repairStartedAt), true)
	return exactSessionStartKeyedOwner, nil
}

// recordExactSessionStrandedRepairTrace fires the SAME site the sweep's shadow
// arm fires, with effect_owner=keyed and the honest effect_applied, so the WD.15
// parity join can separate the legacy, keyed and detector-shadow populations on
// a shared cycle.
//
// The site is the WakeSleep PHASE constant, not a decision site of this arm's
// own: legacy's stranded repair emits no decision record at all (its only trace
// is the phase timing at session_reconciler.go's recordPhase call), so WD.1
// seated the family there and this handler stays with it. Recorded as a §3 delta
// rather than invented here.
func recordExactSessionStrandedRepairTrace(
	params exactSessionStartParams,
	admission sessionStartAdmission,
	info sessionpkg.Info,
	outcome TraceOutcomeCode,
	duration time.Duration,
	applied bool,
) {
	if params.Trace == nil {
		return
	}
	cycle := params.Trace.BeginCycle(TraceTickTriggerControl, "exact_session_stranded_repair", time.Now().UTC(), params.Config)
	if cycle == nil {
		return
	}
	template := normalizedSessionTemplateInfo(info, params.Config)
	if template == "" {
		template = info.Template
	}
	cycle.recordKeyedEffect(
		TraceSiteSessionReconcileWakeSleep,
		detectorReasonStrandedPoolSlot,
		outcome,
		"exact_session_stranded_repair",
		template,
		info.ID,
		info.SessionNameMetadata,
		duration,
		map[string]any{
			"admission":         string(admission.Source),
			"admission_version": admission.Version,
			"generation":        params.Generation,
			"instance_token":    info.InstanceToken,
			"close_reason":      strandedRepairCloseReason,
			"effect_owner":      detectorKeyedEffectOwner,
			"effect_applied":    applied,
		},
	)
	if err := cycle.End(TraceCompletionCompleted, nil); err != nil && params.Stderr != nil {
		fmt.Fprintf(params.Stderr, "session reconciler: recording exact stranded repair trace: %v\n", err) //nolint:errcheck // tracing is observational
	}
}
