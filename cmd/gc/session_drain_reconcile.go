package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/rollout"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/telemetry"
)

// exactSessionDrainAdvanceCandidate is the D-DRAIN seam's guard. Like every
// other family it answers from state both writers already share, never from
// admission.Source — but unlike the durable-row families its condition lives in
// the in-memory drainTracker, because Q4 (resolved on ga-f7v2ft.110, inherited
// here) keeps drain intent there. That is the same shape D-DEADLINE takes with
// the lifecycle timer trackers: one tracker, two readers, no second opinion.
//
// The one rung that IS durable is the refusal at the top: a drain-acked
// stop-pending row belongs to the existing keyed drain-ack stop
// (session_start_reconcile.go, the isDrainAckStopPendingInfo block), which this
// family hands the stop leg to rather than reimplementing. Excluding it here is
// what makes that handover a fall-through instead of a second stop path.
func exactSessionDrainAdvanceCandidate(
	params exactSessionStartParams,
	info sessionpkg.Info,
	response sessionpkg.PersistedResponse,
) bool {
	if response.Revision == 0 || info.Closed || params.Config == nil {
		return false
	}
	// No tracker, no ack channel, no provider is no family: all three are the
	// state this handler reads and writes, so without them the row is legacy's.
	if params.DrainTracker == nil || params.DrainOps == nil || params.Provider == nil {
		return false
	}
	if strings.TrimSpace(info.SessionNameMetadata) == "" || !isKnownStateInfo(info) {
		return false
	}
	if isDrainAckStopPendingInfo(info) {
		return false
	}
	return params.DrainTracker.get(info.ID) != nil
}

// reconcileExactSessionDrainAdvance advances ONE in-flight drain by exact key:
// it discovers the acknowledgement, applies the cancel arms, and drives the
// drain to its terminal state. It is the keyed form of the fleet drain scan
// (advanceSessionDrainsWithSessionsTraced, session_wake.go) plus the ack read
// legacy pays per session in its forward pass.
//
// Ack discovery is HANDLER-side by recorded architect decision (DETECTOR.md §3,
// D-DRAIN): the tracker cannot tell awaiting-ack from acked — every ack read is
// a provider GetMeta — so the sweep enqueues on tracker state alone and this
// handler pays the one read for the one key it holds.
//
// Exactly ONE effect lands per invocation, drawn from the existing drain
// library and nothing else: the stale-generation clear, completeDrain, one of
// the two cancel arms, the stop-pending transition (after which the keyed
// drain-ack stop owns the row), the deferred acknowledgement write, or the
// timeout stop. The arms are evaluated in the library's own order, because that
// order is load-bearing: the pending-interaction and wake-based cancels run
// before the timeout path so a drain a person has re-engaged is never
// force-stopped.
func reconcileExactSessionDrainAdvance(
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
	state := params.DrainTracker.get(info.ID)
	if state == nil {
		// Re-derived away between admission and dispatch. The tracker is the
		// authority and the detector's reason was only a hint.
		return exactSessionStartKeyedOwner, nil
	}
	name := strings.TrimSpace(info.SessionNameMetadata)
	// The same D2 pair the routing seam screens on. Every terminal arm below
	// ends in an unattended, token-bound stop behind a fresh-death proof, so a
	// provider that cannot pay for that must not advance an intent it can never
	// retire.
	if !detectorProviderStopCapable(params.Provider) {
		return yieldOrPark(errors.New("exact drain advance provider cannot prove fresh liveness and an unattended stop"))
	}

	// Stale generation: the session was re-woken under a new incarnation, so this
	// drain is about a session that no longer exists. Clear it — never stop it.
	generation, _ := strconv.Atoi(strings.TrimSpace(info.Generation))
	if generation != state.generation {
		params.DrainTracker.clearIdleProbe(info.ID)
		if state.ackSet {
			_ = clearReconcilerDrainAckMetadata(params.Provider, name)
		}
		params.DrainTracker.remove(info.ID)
		recordExactSessionDrainTrace(params, admission, info, state.reason,
			TraceSiteDrainStale, TraceReasonStaleGeneration, TraceOutcomeCancel, 0, true, map[string]any{
				"drain_generation":   state.generation,
				"session_generation": generation,
			})
		return exactSessionStartKeyedOwner, nil
	}

	liveness := runtime.ObserveFreshLiveness(params.Provider, runtime.LivenessTarget{
		SessionID:            info.ID,
		SessionName:          name,
		ProcessNames:         drainAckStopPendingProcessNames(params.Config, info),
		IncarnationStartedAt: drainAckIncarnationStartedAt(info),
	})
	// Scan completeness proves ABSENCE; a positive observation is decisive on
	// its own. A draining session's pane is usually still alive, and a live
	// pane withholds the tmux-absence license that would complete the /proc
	// sweep — an unconditional Complete gate therefore parked the advance
	// before it could even send its deferred drain signal (ga-i20db field
	// follow-up). Only the dead-completion below writes asleep, and it (like
	// the timeout stop's confirm) demands a COMPLETE proven-dead observation.
	if !liveness.Running && !liveness.Alive {
		if !liveness.Complete {
			// Absence is not proven. The fleet scan treats an unreadable probe as
			// "exited" and completes the drain; a keyed completion writes asleep
			// onto a row whose agent may still be working, so this refuses
			// instead. The condition is level-triggered and re-detected next
			// sweep.
			return yieldOrPark(errors.New("exact drain advance liveness observation is incomplete"))
		}
		latest, ok := rereadExactSessionDrainRow(params, info, response, name)
		if !ok {
			return exactSessionStartKeyedOwner, nil
		}
		completedAt := time.Now()
		completeDrain(latest, sessionFrontDoor(params.Store), state, clk)
		params.DrainTracker.clearIdleProbe(info.ID)
		params.DrainTracker.remove(info.ID)
		telemetry.RecordDrainTransition(context.Background(), name, state.reason, "complete")
		recordExactSessionDrainTrace(params, admission, info, state.reason,
			TraceSiteDrainComplete, TraceReasonCode(state.reason), TraceOutcomeComplete, time.Since(completedAt), true, map[string]any{
				"drain_started_at": state.startedAt,
			})
		return exactSessionStartKeyedOwner, nil
	}

	if ctx != nil && ctx.Err() != nil {
		return exactSessionStartKeyedOwner, nil
	}

	// Cancel arm 1 — a person is waiting on a prompt. A6 (DESIGN.md §2) in its
	// cancel form: the fleet scan reaches the same conclusion through the wake
	// evaluation's WakePending reason, which is fleet state no per-key predicate
	// can re-derive, so the handler re-pays the probe the reason was built from.
	if pendingDrainReasonCancelable(state.reason) &&
		pendingInteractionKeepsAwakeInfo(info, params.Provider, name, clk) {
		if cancelSessionDrainForPendingInfo(info, params.Provider, params.DrainTracker) {
			recordExactSessionDrainTrace(params, admission, info, state.reason,
				TraceSiteDrainCancel, TraceReasonCode(state.reason), TraceOutcomeCancelPending, 0, true, nil)
			return exactSessionStartKeyedOwner, nil
		}
	}

	// Cancel arm 2 — the session acquired assigned work while draining. Same
	// substitution: the fleet scan reads eval.Reason == "assigned-work" off the
	// awake set; the handler re-pays the one reachable-store query that verdict
	// was derived from, and fails CLOSED exactly as the fleet arms do.
	if assignedWorkDrainReasonCancelable(state.reason) {
		hasWork, workErr := sessionHasAwakeAssignedWorkForReachableStore(params.CityPath, params.Config, params.Store, params.RigStores, info)
		if workErr != nil || hasWork {
			if workErr != nil {
				fmt.Fprintf(stderr, "session reconciler: checking assigned work for exact draining %s: %v\n", name, workErr) //nolint:errcheck // matches the fleet arm's diagnostic
			}
			if cancelSessionDrainForAssignedWorkInfo(info, params.Provider, params.DrainTracker) ||
				cancelRecoveredDrainForAssignedWorkInfo(info, params.Provider, name) {
				_ = params.DrainOps.clearDrain(name)
				recordExactSessionDrainTrace(params, admission, info, state.reason,
					TraceSiteDrainCancel, TraceReasonCode(state.reason), TraceOutcomeCancelAssignedWork, 0, true, nil)
				return exactSessionStartKeyedOwner, nil
			}
		}
	}

	// Cancel arm 3 — the session reacquired SOME reason to be awake. Arms 1 and 2
	// cover WakePending and WakeWork; this one covers the other seven reasons in
	// the vocabulary (config, create, session, keep-warm, attached, wait, pin),
	// which is why it cannot be a per-key predicate: "any wake reason" is a fleet
	// verdict over pool counts, named and routed demand and the ready-wait set,
	// exactly the shape D-ORPHAN's undesiredness takes. The handler therefore
	// reads the sweep's own published projection rather than re-deriving it, and
	// an absent view or an absent key declines — the fleet scan's behavior for a
	// row missing from wakeEvals.
	//
	// It sits ABOVE the acknowledgement discovery for the reason the fleet scan
	// puts it above its timeout: a session someone has re-engaged must never be
	// carried into the stop leg. The cancel itself is the shared library's
	// (cancelSessionDrainInfo), the same one arms 1 and 2 route through, so the
	// clear-ack/clear-probe/retire sequence has one implementation.
	//
	// ATTACHMENT is the one reason in that list the published projection
	// structurally cannot carry, so the arm pays for it the way every other keyed
	// arm pays for A6: handler-side, for one key. detectorAwakeSet leaves
	// AwakeInput.AttachedSessions empty on purpose — attachment is provider I/O
	// the sweep refuses to pay fleet-wide (DETECTOR.md §3b, D-SLEEP "probe/pending
	// arms unpredicted") — so ComputeAwakeSet never reaches its "attached" rung,
	// awakeSetToWakeEvals never emits WakeAttached, and a published-view-only arm
	// would force-stop a session a human is sitting in. The re-pay is the same
	// rung D-ORPHAN's and D-SLEEP's drain arms take through
	// exactSessionActiveUseDeferralReason, and it is ordered second so the free
	// map read decides before any provider read is bought.
	if drainReasonCancelable(state.reason) {
		source := ""
		switch {
		case exactSessionDrainReacquiredWake(params, info.ID):
			source = "published_wake_view"
		case exactSessionUserAttached(params.Provider, name):
			source = "attachment_probe"
		}
		if source != "" && cancelSessionDrainInfo(info, params.Provider, params.DrainTracker) {
			recordExactSessionDrainTrace(params, admission, info, state.reason,
				TraceSiteDrainCancel, TraceReasonCode(state.reason), TraceOutcomeCancel, 0, true, map[string]any{
					"wake_cancel_source": source,
				})
			return exactSessionStartKeyedOwner, nil
		}
	}

	// Handler-side ack discovery. This single GetMeta is the read the sweep
	// refuses to pay fleet-wide, and it is what lets detection stay zero-read.
	acked, ackErr := params.DrainOps.isDrainAcked(name)
	if ackErr != nil {
		return yieldOrPark(fmt.Errorf("reading exact drain acknowledgement for %q: %w", info.ID, ackErr))
	}
	if acked {
		latest, ok := rereadExactSessionDrainRow(params, info, response, name)
		if !ok {
			return exactSessionStartKeyedOwner, nil
		}
		markedAt := time.Now()
		updated, marked := markDrainAckStopPending(latest, sessionFrontDoor(params.Store), clk, stderr)
		if !marked {
			return exactSessionStartKeyedOwner, fmt.Errorf(
				"marking exact drain acknowledgement stop-pending for %q: the transition did not persist", info.ID)
		}
		// The tracker entry retires with the transition, exactly as the fleet arm
		// retires it: from here the row is the keyed drain-ack stop's, which owns
		// the atomic close and the async stop (A5), and this family must not
		// re-detect a key it has handed over.
		clearDrainTrackerForStopPending(info.ID, params.DrainTracker)
		recordExactSessionDrainTrace(params, admission, info, state.reason,
			TraceSiteReconcilerDrainAck, TraceReasonAcknowledged, TraceOutcomeStopPending, time.Since(markedAt), true, map[string]any{
				"state_reason": strings.TrimSpace(updated.StateReason),
			})
		return exactSessionStartKeyedOwner, nil
	}

	// Deferred drain signal. The interrupt was retired from this path years ago
	// (session_wake.go: "no Ctrl-C keystroke injection into the pane"), so the
	// signal IS this metadata write, and deferring it one advance is what gives a
	// falsely-drained session a full cycle to be rescued.
	if !state.ackSet {
		if err := setReconcilerDrainAckMetadata(params.Provider, name, state); err != nil {
			recordExactSessionDrainTrace(params, admission, info, state.reason,
				TraceSiteReconcilerDrainAck, TraceReasonCode(state.reason), TraceOutcomeRetry, 0, false, map[string]any{
					"deferred_signal": true,
					"error":           err.Error(),
				})
			return exactSessionStartKeyedOwner, fmt.Errorf(
				"setting exact drain acknowledgement for %q: %w", info.ID, err)
		}
		state.ackSet = true
		state.followUp = true
		recordExactSessionDrainTrace(params, admission, info, state.reason,
			TraceSiteReconcilerDrainAck, TraceReasonCode(state.reason), TraceOutcomeSuccess, 0, true, map[string]any{
				"deferred_signal": true,
				"field":           "GC_DRAIN_ACK",
			})
		return exactSessionStartKeyedOwner, nil
	}

	// Timeout. Reached only after every cancel arm declined, which is the fleet
	// scan's ordering and the reason a re-engaged session is never force-stopped.
	if clk.Now().After(state.deadline) {
		latest, ok := rereadExactSessionDrainRow(params, info, response, name)
		if !ok {
			return exactSessionStartKeyedOwner, nil
		}
		stoppedAt := time.Now()
		if stopErr := verifiedStop(latest, params.Store, params.Provider, params.Config); stopErr != nil {
			if errors.Is(stopErr, errTokenMismatch) {
				// A different incarnation owns the runtime; this drain is stale.
				params.DrainTracker.clearIdleProbe(info.ID)
				params.DrainTracker.remove(info.ID)
			}
			recordExactSessionDrainTrace(params, admission, info, state.reason,
				TraceSiteDrainTimeout, TraceReasonCode(state.reason), TraceOutcomeRetry, time.Since(stoppedAt), false, map[string]any{
					"error": stopErr.Error(),
				})
			return exactSessionStartKeyedOwner, nil
		}
		confirm := runtime.ObserveFreshLiveness(params.Provider, runtime.LivenessTarget{
			SessionID:            latest.ID,
			SessionName:          name,
			ProcessNames:         drainAckStopPendingProcessNames(params.Config, latest),
			IncarnationStartedAt: drainAckIncarnationStartedAt(latest),
		})
		if !confirm.Complete || confirm.Running || confirm.Alive {
			// Still up, or absence unproven. Keep the drain for the next advance
			// rather than write asleep onto a live row.
			recordExactSessionDrainTrace(params, admission, info, state.reason,
				TraceSiteDrainTimeout, TraceReasonCode(state.reason), TraceOutcomeRetry, time.Since(stoppedAt), false, map[string]any{
					"liveness_complete": confirm.Complete,
				})
			return exactSessionStartKeyedOwner, nil
		}
		completeDrain(latest, sessionFrontDoor(params.Store), state, clk)
		params.DrainTracker.clearIdleProbe(info.ID)
		params.DrainTracker.remove(info.ID)
		telemetry.RecordDrainTransition(context.Background(), name, state.reason, "timeout")
		recordExactSessionDrainTrace(params, admission, info, state.reason,
			TraceSiteDrainTimeout, TraceReasonCode(state.reason), TraceOutcomeComplete, time.Since(stoppedAt), true, nil)
		return exactSessionStartKeyedOwner, nil
	}

	// Still draining, still inside its budget, acknowledgement still outstanding.
	recordExactSessionDrainTrace(params, admission, info, state.reason,
		TraceSiteReconcilerDrainAck, TraceReasonCode(state.reason), TraceOutcomeNoChange, 0, false, nil)
	return exactSessionStartKeyedOwner, nil
}

// exactSessionDrainReacquiredWake answers the fleet half of D-DRAIN's third
// cancel arm for one key: does the tick's own wake evaluation give this session a
// reason to be awake?
//
// It is the exact predicate the fleet scan applies inline
// (`eval, ok := wakeEvals[info.ID]; ok && len(eval.Reasons) > 0`), read off the
// projection the detector sweep published for the same cycle. Everything
// missing — no accessor, no published view, no entry for the key — answers false,
// which declines the cancel and leaves the drain to its remaining arms. That is
// the fleet scan's own answer for a row its wakeEvals does not carry, and it is
// the direction that cannot invent a rescue for a session nothing wants awake.
func exactSessionDrainReacquiredWake(params exactSessionStartParams, sessionID string) bool {
	if params.SessionWakeEvaluations == nil {
		return false
	}
	eval, ok := params.SessionWakeEvaluations()[sessionID]
	return ok && len(eval.Reasons) > 0
}

// rereadExactSessionDrainRow re-reads the authoritative row and fences it on the
// revision, the instance token, and the session name the admission was
// dispatched against. Every durable or destructive arm above runs it first: the
// liveness probe and the ack read are provider I/O, and a wake, a suspend or a
// replacement incarnation can land while they run.
func rereadExactSessionDrainRow(
	params exactSessionStartParams,
	info sessionpkg.Info,
	response sessionpkg.PersistedResponse,
	name string,
) (sessionpkg.Info, bool) {
	latest, latestResponse, readErr := getAuthoritativeSessionStartPersistedRecord(params.Store, info.ID)
	if readErr != nil || latestResponse.Revision != response.Revision || latest.Closed ||
		strings.TrimSpace(latest.InstanceToken) != strings.TrimSpace(info.InstanceToken) ||
		strings.TrimSpace(latest.SessionNameMetadata) != name ||
		!exactSessionDrainAdvanceCandidate(params, latest, latestResponse) {
		return sessionpkg.Info{}, false
	}
	return latest, true
}

// recordDrainAckAdmissionBoundTrace records the two observations ga-f7v2ft.112
// ruling 1b owes at the drain-ack admission: the throttled consecutive-refusal
// diagnostic, and the one event fired when a retained obligation outlives the
// drain's own deadline and gives the row back. It fires at the DrainAck family's
// legacy site so `gc trace` shows the release beside the drain it bounded.
func (cr *CityRuntime) recordDrainAckAdmissionBoundTrace(
	cfg *config.City,
	result sessionStartReconcileResult,
	outcome TraceOutcomeCode,
) {
	if cr == nil || cr.trace == nil {
		return
	}
	cycle := cr.trace.BeginCycle(TraceTickTriggerControl, "exact_session_drain_ack_admission", time.Now().UTC(), cfg)
	if cycle == nil {
		return
	}
	fields := traceRecordPayload{
		"session_id":         result.Admission.SessionID,
		"admission":          string(result.Admission.Source),
		"admission_version":  result.Admission.Version,
		"drain_ack_refusals": result.DrainAckRefusals,
		"effect_owner":       detectorKeyedEffectOwner,
		"effect_applied":     outcome == TraceOutcomeDeadlineExceeded,
	}
	if result.Err != nil {
		fields["error"] = result.Err.Error()
	}
	cycle.RecordDecision(TraceSiteReconcilerDrainAck, TraceReasonAcknowledged, outcome, "", result.Admission.SessionID, fields)
	if err := cycle.End(TraceCompletionCompleted, nil); err != nil {
		fmt.Fprintf(cr.sessionStartStderr(), "%s: recording drain-ack admission bound trace: %v\n", cr.sessionStartLogPrefix(), err) //nolint:errcheck // tracing is observational
	}
}

// recordExactSessionDrainTrace fires the SAME legacy trace sites the fleet drain
// scan and the forward-pass acknowledgement block fire — DrainStale,
// DrainComplete, DrainCancel, DrainTimeout and ReconcilerDrainAck — with
// effect_owner=keyed and the honest effect_applied, so the WD.15 parity join can
// put the keyed advance beside legacy's on one cycle.
func recordExactSessionDrainTrace(
	params exactSessionStartParams,
	admission sessionStartAdmission,
	info sessionpkg.Info,
	drainReason string,
	site TraceSiteCode,
	reason TraceReasonCode,
	outcome TraceOutcomeCode,
	duration time.Duration,
	applied bool,
	extra map[string]any,
) {
	if params.Trace == nil {
		return
	}
	cycle := params.Trace.BeginCycle(TraceTickTriggerControl, "exact_session_drain_advance", time.Now().UTC(), params.Config)
	if cycle == nil {
		return
	}
	template := normalizedSessionTemplateInfo(info, params.Config)
	if template == "" {
		template = info.Template
	}
	fields := map[string]any{
		"admission":         string(admission.Source),
		"admission_version": admission.Version,
		"generation":        params.Generation,
		"instance_token":    info.InstanceToken,
		"drain_reason":      drainReason,
		"effect_owner":      detectorKeyedEffectOwner,
		"effect_applied":    applied,
	}
	for k, v := range extra {
		fields[k] = v
	}
	cycle.recordKeyedEffect(
		site,
		reason,
		outcome,
		"exact_session_drain_advance",
		template,
		info.ID,
		info.SessionNameMetadata,
		duration,
		fields,
	)
	if err := cycle.End(TraceCompletionCompleted, nil); err != nil && params.Stderr != nil {
		fmt.Fprintf(params.Stderr, "session reconciler: recording exact drain advance trace: %v\n", err) //nolint:errcheck // tracing is observational
	}
}
