package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/rollout"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// exactSessionOrphanDrainReasons are the two close reasons the family drains
// rather than closes. Legacy's drain arm carries exactly these
// (session_reconciler.go:2225), and both are non-cancelable drain reasons
// (session_wake.go drainReasonCancelable), which is what makes the drain
// terminal rather than advisory. A live failed-create row is nobody's drain
// target: legacy's failed-create arm is dead-only, so such a row keeps the
// close arm's kept-open refusal.
func exactSessionOrphanDrainReasons(reason string) bool {
	return reason == "orphaned" || reason == "suspended"
}

// exactSessionActiveUseDeferralReason answers A6 — attached-user safety, a
// KEEP invariant of the whole redesign (DESIGN.md §2) — for every keyed arm
// whose effect is a drain. A drain's first act on the next tick is an interrupt
// (Ctrl-C) into the pane, so beginning one against a session a human is attached
// to, or one whose user is waiting on a pending interaction, interrupts a person
// mid-sentence. D-ORPHAN's drain arm (WD.4) and D-SLEEP's (WD.5) share it
// deliberately: one rung, one spelling, so the two families cannot drift on what
// counts as an engaged human.
//
// It leads with the same two positive-use signals namedSessionActiveUseReasonInfo
// leads with, and deliberately stops there: that helper's remaining rungs
// (activity_unknown, recent_activity) are config-drift POLICY, and adopting them
// here would defer orphan drains indefinitely on every provider that cannot
// report activity. Returning "" means no human is engaged.
func exactSessionActiveUseDeferralReason(
	params exactSessionStartParams,
	info sessionpkg.Info,
	name string,
	clk clock.Clock,
) string {
	if params.Provider == nil || name == "" {
		return ""
	}
	if pendingInteractionKeepsAwakeInfo(info, params.Provider, name, clk) {
		return "pending_interaction"
	}
	if exactSessionUserAttached(params.Provider, name) {
		return "attached"
	}
	return ""
}

// exactSessionUserAttached is the attachment rung itself, named once so every
// keyed arm that owes A6 spells it the same way. D-ORPHAN's and D-SLEEP's drain
// arms reach it through exactSessionActiveUseDeferralReason above; D-DRAIN's
// third cancel arm calls it directly, because at that point in the advance the
// pending-interaction probe has already been paid by cancel arm 1 and re-paying
// it would buy a second provider read per key per tick for an answer the handler
// already has.
//
// IsRunning is deliberately not re-checked the way sessionAttachedForWakeReason
// checks it. Every caller has already proven fresh liveness for the key before it
// asks, so the extra probe would only add provider I/O to a question already
// answered.
func exactSessionUserAttached(sp runtime.Provider, name string) bool {
	if sp == nil || strings.TrimSpace(name) == "" {
		return false
	}
	return sp.IsAttached(name)
}

// reconcileExactSessionOrphanDrain begins the keyed drain of ONE live undesired
// row. It is reached from inside the close arm's fresh-liveness observation
// rather than under a second admission on the same key: the two arms split on
// exactly one fact — is the runtime alive — and a second probe could disagree
// with the first, leaving the row owned by neither arm. One observation, one
// owner.
//
// The drain itself is the existing library (beginSessionDrainInfo,
// session_wake.go:203-233), not a second drain engine, and its ENQUEUE-ONLY
// begin semantics are preserved verbatim: the interrupt is deferred to the next
// tick's advance, which is what gives a falsely-orphaned session one full tick
// to be rescued. Intent stays in the in-memory drainTracker (Q4, resolved on
// ga-f7v2ft.110): drains reset on controller restart, exactly today's
// semantics, and legacy's advance loop reads the same tracker.
//
// The named spec-absence confirmation window is NOT re-derived here. Like
// undesiredness, it is fleet-tick state no per-key predicate can answer — it
// counts consecutive sweeps — so the detector withholds the enqueue until the
// window elapses and this handler never sees a key inside it.
func reconcileExactSessionOrphanDrain(
	ctx context.Context,
	admission sessionStartAdmission,
	params exactSessionStartParams,
	info sessionpkg.Info,
	reason string,
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
	name := strings.TrimSpace(info.SessionNameMetadata)
	if !detectorActOrphanDrain || !exactSessionOrphanDrainReasons(reason) {
		recordExactSessionOrphanDrainTrace(params, admission, info, reason, TraceOutcomeKeptOpen, 0, false, nil)
		return exactSessionStartKeyedOwner, nil
	}
	if params.DrainTracker == nil {
		return yieldOrPark(errors.New("exact orphan drain has no tracker to record drain intent in"))
	}
	// The same D2 pair the routing seam screens on. A drain's terminal effect is
	// an unattended, token-bound stop behind a fresh-death proof, so a provider
	// that cannot pay for that must not accumulate an intent it can never retire.
	if !detectorProviderStopCapable(params.Provider) {
		return yieldOrPark(errors.New("exact orphan drain provider cannot prove fresh liveness and an unattended stop"))
	}
	if deferral := exactSessionActiveUseDeferralReason(params, info, name, clk); deferral != "" {
		recordExactSessionOrphanDrainTrace(params, admission, info, reason, TraceOutcomeDeferred, 0, false, map[string]any{
			"active_use": deferral,
		})
		return exactSessionStartKeyedOwner, nil
	}
	if ctx != nil && ctx.Err() != nil {
		return exactSessionStartKeyedOwner, nil
	}

	beganAt := time.Now()
	if !beginSessionDrainInfo(info, params.Provider, params.DrainTracker, reason, clk, defaultDrainTimeout) {
		// A drain is already in flight for this key. Advancing it is D-DRAIN's,
		// and the sweep's family precedence hands the row there from the next
		// cycle on, so this is a quiet no-op rather than a refusal.
		recordExactSessionOrphanDrainTrace(params, admission, info, reason, TraceOutcomeNoChange, time.Since(beganAt), false, nil)
		return exactSessionStartKeyedOwner, nil
	}
	// Witness the intent the same way the close arm witnesses its terminal row:
	// effect_applied is claimed only against observed state, never against a
	// call that returned.
	state := params.DrainTracker.get(info.ID)
	if state == nil || state.reason != reason {
		return exactSessionStartKeyedOwner, fmt.Errorf(
			"beginning exact orphan drain of %q: drain intent is absent after begin", info.ID)
	}
	recordExactSessionOrphanDrainTrace(params, admission, info, reason, TraceOutcomeDrain, time.Since(beganAt), true, map[string]any{
		"drain_timeout_seconds": int64(state.deadline.Sub(state.startedAt).Seconds()),
	})
	return exactSessionStartKeyedOwner, nil
}

// recordExactSessionOrphanDrainTrace fires the SAME legacy trace site the fleet
// drain arm fires (TraceSiteReconcilerOrphaned) with effect_owner=keyed and the
// honest effect_applied, so the WD.15 parity join can put the keyed drain beside
// legacy's on one cycle.
func recordExactSessionOrphanDrainTrace(
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
	cycle := params.Trace.BeginCycle(TraceTickTriggerControl, "exact_session_orphan_drain", time.Now().UTC(), params.Config)
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
		TraceSiteReconcilerOrphaned,
		TraceReasonCode(reason),
		outcome,
		"exact_session_orphan_drain",
		template,
		info.ID,
		info.SessionNameMetadata,
		duration,
		fields,
	)
	if err := cycle.End(TraceCompletionCompleted, nil); err != nil && params.Stderr != nil {
		fmt.Fprintf(params.Stderr, "session reconciler: recording exact orphan drain trace: %v\n", err) //nolint:errcheck // tracing is observational
	}
}
