package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/rollout"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// sessionSleepIntentIdleStopPending is the durable marker an idle drain writes
// before it begins, so a crash between the mark and the stop recovers into a
// normal idle sleep rather than into a drain with no record.
const sessionSleepIntentIdleStopPending = "idle-stop-pending"

// exactSessionSleepDrainCandidate is the D-SLEEP seam's guard. It answers from
// the DURABLE ROW alone — never from admission.Source — and returns true when
// the row is one this family may reason about at all.
//
// The rungs it refuses on are legacy's, in legacy's order: a row that does not
// durably claim to be awake has nothing to drain; a drain-acked stop-pending row
// belongs to D-DRAIN; a pinned row or one carrying a durable wake cause is still
// wanted; and a live session held only by a future `held_until` with no
// sleep_intent is running `gc runtime heartbeat` (#3994) — the keep-alive escape
// becomes a refusal here rather than a mid-pass branch.
//
// The last rung is what keeps the guard NARROW. Every rung above it is shared
// with the ordinary start path — an awake, unpinned, unheld row is the normal
// shape of a running session — so a guard that stopped there would claim every
// admission on every live key and divert it out of the start path. The row must
// additionally carry a durable sleep intent, or resolve to a sleep policy whose
// window has already elapsed. On a city that configures no sleep at all the
// policy resolution is pure config and short-circuits before any provider read,
// so the family costs such a city nothing and claims none of its rows.
//
// Two rungs are deliberately still the handler's: the pending-interaction probe
// and the attachment probe are provider I/O the seam must not pay for every
// admission on every key, and both would only ever REFUSE a row this guard
// admits.
func exactSessionSleepDrainCandidate(
	params exactSessionStartParams,
	info sessionpkg.Info,
	response sessionpkg.PersistedResponse,
	clk clock.Clock,
) bool {
	if clk == nil {
		clk = clock.Real{}
	}
	if response.Revision == 0 || info.Closed || params.Config == nil {
		return false
	}
	// No tracker is no family: drain intent and idle-probe state both live there,
	// so without one there is nothing for this handler to record or read, and the
	// row belongs to legacy.
	if params.DrainTracker == nil || params.Provider == nil {
		return false
	}
	if strings.TrimSpace(info.SessionNameMetadata) == "" {
		return false
	}
	if !isKnownStateInfo(info) || !detectorBeadAwake(info) {
		return false
	}
	if strings.TrimSpace(info.StateReason) == sessionpkg.DrainAckStopPendingReason {
		return false
	}
	if strings.TrimSpace(info.PinAwake) == "true" {
		return false
	}
	lifecycle := sessionpkg.ProjectLifecycle(exactSessionSleepLifecycleInput(info, clk))
	if lifecycle.HasWakeCause(sessionpkg.WakeCausePendingCreate) ||
		lifecycle.HasWakeCause(sessionpkg.WakeCauseExplicit) ||
		lifecycle.HasWakeCause(sessionpkg.WakeCausePinned) {
		return false
	}
	if strings.TrimSpace(info.SleepIntent) != "" {
		// A durable intent IS the no-wake answer, and the #3994 keep-alive escape
		// only ever covers a hold with no intent behind it.
		return true
	}
	if lifecycleTimerBlockerInfo(info, clk.Now()) == "user_hold" {
		return false
	}
	policy := resolveSessionSleepPolicyInfo(info, params.Config, params.Provider)
	return configWakeSuppressedInfo(info, policy, params.Provider, clk)
}

func exactSessionSleepLifecycleInput(info sessionpkg.Info, clk clock.Clock) sessionpkg.LifecycleInput {
	in := sessionpkg.LifecycleInputFromInfo(info)
	in.Now = clk.Now()
	return in
}

// exactSessionSleepDrainReason re-derives the no-wake reason legacy's awake-scan
// drain arm would stamp (session_reconciler.go:3973-3985), from durable intent
// plus the per-key sleep-policy pass. It returns "" when the row's no-wake
// verdict is not re-derivable per key.
//
// That "" is load-bearing and is why the family's effect arm is narrower than
// legacy's. Legacy's last rung — plain "no-wake-reason" — means "ComputeAwakeSet
// found no reason to be awake", which is a FLEET verdict over pool counts, named
// and routed demand, and the ready-wait set. No per-key predicate can re-derive
// it, and the seam's rule is that the handler answers from the row, not from the
// detector's reason. Rather than trust a reason it cannot check, this slice
// leaves those rows to legacy: the detector records them and never enqueues them
// (see detectSleep). What IS re-derivable per key — a durable sleep_intent, and
// the workspace sleep-policy suppression — is exactly what the family acts on.
func exactSessionSleepDrainReason(
	params exactSessionStartParams,
	info sessionpkg.Info,
	policy resolvedSessionSleepPolicy,
	clk clock.Clock,
) string {
	intent := strings.TrimSpace(info.SleepIntent)
	switch {
	case intent == sessionSleepIntentIdleStopPending:
		return "idle"
	case intent != "":
		return intent
	}
	name := strings.TrimSpace(info.SessionNameMetadata)
	// Legacy's own suppression rungs, minus the fleet demand override: an alive
	// session with a ready pending interaction is a person waiting on a prompt,
	// so it is never suppressed into a sleep.
	if pendingInteractionReady(params.Provider, name) {
		return ""
	}
	if !configWakeSuppressedInfo(info, policy, params.Provider, clk) {
		return ""
	}
	return "idle"
}

// reconcileExactSessionSleepDrain drains ONE alive session the awake set no
// longer wants, by exact key, and launches that session's idle probe when the
// drain still needs one.
//
// Both rungs live behind one key on purpose. The probe is not a second effect:
// it is the confirmation the idle drain waits on, and legacy runs it in the same
// pass for the same session (selectIdleProbeTargets / shouldBeginIdleDrainInfo).
// What moves is WHERE each half runs — the round-robin per-sweep budget stays
// detector-side because it is a fleet-shaped rate limit, and the WaitForIdle
// launch comes here, so a fleet-wide tick no longer pays for it
// (DETECTOR.md §3, D-SLEEP).
//
// The drain itself is the existing library (beginSessionDrainInfo,
// session_wake.go:203-233), not a second drain engine, and its enqueue-only
// begin semantics are preserved verbatim: the interrupt is deferred to the next
// tick's advance, which is what gives a session one full tick to be rescued.
// Intent stays in the in-memory drainTracker (Q4, resolved on ga-f7v2ft.110).
func reconcileExactSessionSleepDrain(
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
	yieldOrPark := func(cause error) (exactSessionStartOwner, error) {
		if params.RolloutMode == rollout.Auto {
			return exactSessionStartLegacyOwner, fmt.Errorf("%w: %w", errSessionStartLegacyFallbackRequired, cause)
		}
		return exactSessionStartKeyedOwner, cause
	}
	if params.DrainTracker == nil {
		return yieldOrPark(errors.New("exact sleep drain has no tracker to record drain intent in"))
	}
	if params.DrainTracker.get(info.ID) != nil {
		// A drain is already in flight for this key. Advancing it is D-DRAIN's,
		// and the sweep's family precedence hands the row there from the next
		// cycle on, so this is a quiet no-op rather than a refusal.
		recordExactSessionSleepDrainTrace(params, admission, info, "", TraceOutcomeNoChange, 0, false, nil)
		return exactSessionStartKeyedOwner, nil
	}
	// The same D2 pair the routing seam screens on. A drain's terminal effect is
	// an unattended, token-bound stop behind a fresh-death proof, so a provider
	// that cannot pay for that must not accumulate an intent it can never retire.
	if !detectorProviderStopCapable(params.Provider) {
		return yieldOrPark(errors.New("exact sleep drain provider cannot prove fresh liveness and an unattended stop"))
	}

	name := strings.TrimSpace(info.SessionNameMetadata)
	policy := resolveSessionSleepPolicyInfo(info, params.Config, params.Provider)
	reason := exactSessionSleepDrainReason(params, info, policy, clk)
	if reason == "" {
		// The row still has a wake reason, or its no-wake verdict is the fleet's
		// alone. Either way this handler applies nothing.
		recordExactSessionSleepDrainTrace(params, admission, info, "", TraceOutcomeSkipped, 0, false, nil)
		return exactSessionStartKeyedOwner, nil
	}

	// A6 — attached-user safety, a KEEP invariant of the whole redesign
	// (DESIGN.md §2). The drain's first act on the next tick is an interrupt into
	// the pane, so it must never begin against a session a person is engaged
	// with. The refusal is level-triggered: the drain proceeds once they detach.
	if deferral := exactSessionActiveUseDeferralReason(params, info, name, clk); deferral != "" {
		recordExactSessionSleepDrainTrace(params, admission, info, reason, TraceOutcomeDeferred, 0, false, map[string]any{
			"active_use": deferral,
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
	// its own. This family's candidate is definitionally ALIVE — a live pane
	// withholds the very tmux-absence license (TmuxSessionProvenAbsent) that
	// would let the /proc sweep clear post-incarnation strangers — so gating
	// the positive path on Complete wedged every idle wake-suppressed session
	// on a busy host permanently (ga-i20db field follow-up: tr-j82xw,
	// su-h9kaad, or-b24cs, pl-65t6r). The begin a positive observation
	// licenses is enqueue-only; the terminal stop stays behind its own
	// fresh-death proof in the advance.
	if !liveness.Running && !liveness.Alive {
		if !liveness.Complete {
			// Dead cannot be told apart from unobserved. Fail closed.
			return yieldOrPark(errors.New("exact sleep drain liveness observation is incomplete"))
		}
		// Nothing to drain. A durably-awake row with a dead runtime is D-ORPHAN's
		// condition and the pool-slot free's, not this family's.
		recordExactSessionSleepDrainTrace(params, admission, info, reason, TraceOutcomeSkipped, 0, false, nil)
		return exactSessionStartKeyedOwner, nil
	}

	// Awake assigned work is the one wake rung this handler re-pays live, because
	// it is the rung a stale snapshot is most likely to be wrong about and the one
	// whose cost is a single reachable-store query. Fail closed exactly as the
	// fleet arms do: a session that may still hold in-flight work is not drained.
	hasWork, workErr := sessionHasAwakeAssignedWorkForReachableStore(params.CityPath, params.Config, params.Store, params.RigStores, info)
	if workErr != nil || hasWork {
		recordExactSessionSleepDrainTrace(params, admission, info, reason, TraceOutcomeDeferred, 0, false, map[string]any{
			"assigned_work": true,
		})
		return exactSessionStartKeyedOwner, nil
	}

	if ctx != nil && ctx.Err() != nil {
		return exactSessionStartKeyedOwner, nil
	}

	// Re-read the authoritative row before anything durable happens. The gathers
	// above are provider I/O, and a wake, a suspend or a replacement incarnation
	// can land while they run; everything below writes sleep_intent and records
	// drain intent against the row, so it must be THIS row.
	latest, latestResponse, readErr := getAuthoritativeSessionStartPersistedRecord(params.Store, info.ID)
	if readErr != nil || latestResponse.Revision != response.Revision || latest.Closed ||
		strings.TrimSpace(latest.InstanceToken) != strings.TrimSpace(info.InstanceToken) ||
		strings.TrimSpace(latest.SessionNameMetadata) != name ||
		!exactSessionSleepDrainCandidate(params, latest, latestResponse, clk) {
		return exactSessionStartKeyedOwner, nil
	}

	intent := strings.TrimSpace(latest.SleepIntent)
	if reason == "idle" && intent != sessionSleepIntentIdleStopPending {
		if !shouldBeginIdleDrainInfo(latest, wakeEvaluation{Policy: policy}, params.DrainTracker, params.Provider) {
			// Not yet confirmed idle. Launch the probe HERE — the detector granted
			// this key its slot out of the fleet-shaped per-sweep budget, and the
			// launch is the only half of the probe that was ever per-session.
			launched := launchIdleProbeForSession(ctx, params.DrainTracker, params.Provider, clk, info.ID, name)
			recordExactSessionSleepDrainTrace(params, admission, info, reason, TraceOutcomeDeferredConfirm, 0, false, map[string]any{
				"idle_probe_launched": launched,
			})
			return exactSessionStartKeyedOwner, nil
		}
		// The pending intent lands BEFORE the drain begins, exactly as the fleet
		// arm orders it: a crash between the two must recover into a normal idle
		// sleep (recoverPendingIdleSleepInfo), never into a drain with no record.
		if markIdleSleepPendingInfo(latest, sessionFrontDoor(params.Store)) == nil {
			return exactSessionStartKeyedOwner, fmt.Errorf(
				"marking exact idle sleep pending for %q: the intent did not persist", info.ID)
		}
	}

	beganAt := time.Now()
	if !beginSessionDrainInfo(latest, params.Provider, params.DrainTracker, reason, clk, defaultDrainTimeout) {
		recordExactSessionSleepDrainTrace(params, admission, info, reason, TraceOutcomeNoChange, time.Since(beganAt), false, nil)
		return exactSessionStartKeyedOwner, nil
	}
	// Witness the intent rather than the call: effect_applied is claimed only
	// against observed state.
	state := params.DrainTracker.get(info.ID)
	if state == nil || state.reason != reason {
		return exactSessionStartKeyedOwner, fmt.Errorf(
			"beginning exact sleep drain of %q: drain intent is absent after begin", info.ID)
	}
	if params.Stdout != nil {
		fmt.Fprintf(params.Stdout, "Draining session '%s': %s\n", name, reason) //nolint:errcheck // operator log, matching the fleet arm
	}
	recordExactSessionSleepDrainTrace(params, admission, info, reason, TraceOutcomeDrain, time.Since(beganAt), true, map[string]any{
		"drain_timeout_seconds": int64(state.deadline.Sub(state.startedAt).Seconds()),
		"sleep_intent":          intent,
	})
	return exactSessionStartKeyedOwner, nil
}

// recordExactSessionSleepDrainTrace fires the SAME legacy trace site the fleet
// awake-scan arm fires (TraceSiteReconcilerDrainDecision) with effect_owner=keyed
// and the honest effect_applied, so the WD.15 parity join can put the keyed drain
// beside legacy's on one cycle.
func recordExactSessionSleepDrainTrace(
	params exactSessionStartParams,
	admission sessionStartAdmission,
	info sessionpkg.Info,
	reason string,
	outcome TraceOutcomeCode,
	duration time.Duration,
	applied bool,
	extra map[string]any,
) {
	if params.Trace == nil || reason == "" {
		return
	}
	cycle := params.Trace.BeginCycle(TraceTickTriggerControl, "exact_session_sleep_drain", time.Now().UTC(), params.Config)
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
		"drain_reason":      reason,
		"effect_owner":      detectorKeyedEffectOwner,
		"effect_applied":    applied,
	}
	for k, v := range extra {
		fields[k] = v
	}
	cycle.recordKeyedEffect(
		TraceSiteReconcilerDrainDecision,
		TraceReasonCode(reason),
		outcome,
		"exact_session_sleep_drain",
		template,
		info.ID,
		info.SessionNameMetadata,
		duration,
		fields,
	)
	if err := cycle.End(TraceCompletionCompleted, nil); err != nil && params.Stderr != nil {
		fmt.Fprintf(params.Stderr, "session reconciler: recording exact sleep drain trace: %v\n", err) //nolint:errcheck // tracing is observational
	}
}

// exactSessionSleepPolicyProbeGated reports whether an idle drain for this row
// must wait on a WaitForIdle confirmation before it may begin. It is the shared
// predicate: the detector uses it to decide which keys need a probe slot, and
// the handler's own gate (shouldBeginIdleDrainInfo) enforces the same rule, so
// the two sides cannot disagree about which rows the budget applies to.
func exactSessionSleepPolicyProbeGated(policy resolvedSessionSleepPolicy) bool {
	return policy.enabled() && policy.Class != config.SessionSleepNonInteractive
}
