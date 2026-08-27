package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/rollout"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/telemetry"
)

// reconcileExactSessionDetectorFamily is the WD handler-dispatch seam. The
// detector sweep hands an exact key to the session-start controller under a
// family admission source; this block routes that key to the family's handler.
// Each later WD slice adds EXACTLY ONE case, and two rules bind every one:
//
//   - The case's guard is a predicate over the DURABLE ROW just read, never
//     over admission.Source. The controller coalesces admissions on a key and
//     keeps the earlier source, so a source-gated arm silently routes a
//     level-triggered condition into the ordinary start path's dead end — the
//     ga-f7v2ft.125 failure, exactly.
//   - The handler re-derives its own condition from that row and refuses with
//     zero effect the moment it no longer holds. The detector's reason is a
//     scheduling hint; the row is the authority.
//
// It returns handled=false for every key no family claims, which is every key
// in a city where no act constant has flipped.
func reconcileExactSessionDetectorFamily(
	ctx context.Context,
	admission sessionStartAdmission,
	params exactSessionStartParams,
	info sessionpkg.Info,
	response sessionpkg.PersistedResponse,
	clk clock.Clock,
) (bool, exactSessionStartOwner, error) {
	if clk == nil {
		clk = clock.Real{}
	}
	// Case order mirrors legacy's own pass order. D-DUP goes first because its
	// legacy counterpart is Phase 0b, which retires duplicate named rows BEFORE
	// the forward pass runs at all: a loser row that is also undesired, or also
	// past its idle deadline, is the duplicate family's in legacy, so it must be
	// the duplicate family's here. The remaining three mirror the forward pass,
	// where the not-desired block runs before the deadline arms and
	// early-continues: an undesired row past its idle deadline is the orphan
	// family's, not the deadline family's.
	//
	// D-ORPHAN has one case for its two effect arms because their durable-row
	// guard is identical — an undesired open row — and the fact that splits them
	// (is the runtime alive) is provider I/O, not durable state. Splitting the
	// case would mean two guards that can never disagree, so whichever came
	// second would be dead code.
	//
	// D-DRIFT sits ABOVE D-DEADLINE for the same reason D-ORPHAN does: legacy's
	// forward pass runs its config-drift block before the deadline arms and
	// early-continues, so a drifted session that is also past its idle deadline
	// is the drift family's — it gets converged, not killed. Its single case
	// covers both fingerprint halves and both convergence lanes, because the
	// facts that split them (attachment, launch-only, named) are all re-derived
	// from the one row the guard already resolved.
	//
	// D-STALL sits between D-ORPHAN and D-DRIFT for the same forward-pass reason:
	// legacy evaluates its progress-stall arm before the drift block and before
	// the max-age and idle arms, and `continue`s the row past them once the
	// recycle fires. Its guard is therefore the whole decision rather than a
	// trigger rung, so a quiet row the ladder declines still reaches D-DRIFT and
	// D-DEADLINE on the same dispatch instead of being claimed and starved every
	// sweep.
	//
	// D-SLEEP goes LAST among the forward-pass families because legacy puts it
	// last: the wake/sleep decision runs in a SEPARATE phase after the entire
	// forward pass has finished, so a row any earlier family claimed — retired,
	// closed, converged, rolled back, or idle-killed — never reaches the awake
	// scan at all. Ordering it ahead of D-DEADLINE would be the sharpest version
	// of the mistake: an over-deadline row's keyed stop persists its own sleep
	// patch, and draining it as a plain no-wake row instead would race that stop
	// and stamp the wrong sleep_reason on the record ops read afterwards. Its
	// guard is also the broadest here — every awake, unpinned, unheld row — so
	// keeping it below the narrower families lets the cheaper guards
	// short-circuit first.
	//
	// D-STRANDED goes LAST, BELOW D-SLEEP. Both slices were authored claiming
	// "last" — each was the only wake/sleep-phase family on its own branch — so
	// their relative order was decided here against legacy's actual sequence
	// rather than by merge order. Legacy runs both inside ONE per-target loop in
	// the wake/sleep phase, and the sleep block comes first: the no-wake drain
	// arm sits at session_reconciler.go:4088 (`!shouldWake && alive`) and the
	// stranded pool-slot repair at :4191 (`!shouldWake && !alive && poolFreeable`).
	//
	// Today the order is not observable: the two guards are DISJOINT on the
	// durable row, because D-SLEEP requires detectorBeadAwake (state active,
	// awake, creating or start_pending) and D-STRANDED requires
	// isPoolSessionSlotFreeableInfo (state drained, or asleep with a terminal
	// sleep reason). That is exactly why it is worth pinning in the order rather
	// than leaving to chance — legacy splits these two on `alive`, which is
	// provider I/O the seam guard may not pay, so the durable-state disjointness
	// is a property of today's guards and not of the family boundary. If either
	// guard widens, this order is what keeps a row legacy would have repaired
	// from being drained as an ordinary no-wake row instead.
	//
	// D-DRAIN sits SECOND, directly below D-DUP and above every other family.
	// Legacy's Phase-0b duplicate retire is the only arm that genuinely precedes
	// drain handling; from the forward pass on, the acknowledgement decision is
	// the FIRST thing legacy does with a row. Its undesired block opens with
	// isDrainAcked (session_reconciler.go:2195) before either orphan arm, and the
	// desired path's acknowledgement block (:2548) `continue`s past progress
	// stall, drift, the deadline arms and the whole wake/sleep phase. The
	// end-of-tick advance scan (:4282) runs last only because it is a SEPARATE
	// loop over the tracker, re-walking rows the forward pass already claimed —
	// it is not a lower-precedence arm on the same row.
	//
	// The slot is also the only one that composes. Every landed family's handler
	// already refuses a row with an active drain — D-ORPHAN close
	// (session_orphan_close_reconcile.go), D-DEADLINE, D-STALE-CREATE and
	// D-STRANDED all yieldOrPark on params.DrainTracker.get(info.ID) != nil, and
	// D-SLEEP records a quiet no-change — because every one of those refusals was
	// written to mean "advancing a drain is D-DRAIN's". Placing D-DRAIN lower
	// would let those refusals swallow the key and starve the advance the moment
	// this family began acting.
	switch {
	case detectorActDup && exactSessionDuplicateNamedCandidate(params, info, response):
		owner, err := reconcileExactSessionDuplicateNamedRetire(admission, params, info, response, clk)
		return true, owner, err
	case detectorActDrain && exactSessionDrainAdvanceCandidate(params, info, response):
		owner, err := reconcileExactSessionDrainAdvance(ctx, admission, params, info, response, clk)
		return true, owner, err
	case (detectorActOrphanClose || detectorActOrphanDrain) &&
		exactSessionOrphanCloseCandidate(params, info, response, clk) != "":
		owner, err := reconcileExactSessionOrphanClose(ctx, admission, params, info, response, clk)
		return true, owner, err
	case detectorActStall && exactSessionProgressStallCandidate(params, info, response, clk):
		owner, err := reconcileExactSessionProgressStallRecycle(admission, params, info, response, clk)
		return true, owner, err
	case (detectorActDriftConverge || detectorActDriftDefer) &&
		exactSessionConfigDriftCandidate(params, info, response, clk):
		owner, err := reconcileExactSessionConfigDrift(ctx, admission, params, info, response, clk)
		return true, owner, err
	case detectorActDeadline && exactSessionDeadlineStopCandidate(params, info, response, clk.Now().UTC()):
		owner, err := reconcileExactSessionDeadlineStop(ctx, admission, params, info, response, clk)
		return true, owner, err
	case detectorActStaleCreate && exactSessionStaleCreateRollbackCandidate(params, info, response, clk):
		owner, err := reconcileExactSessionStaleCreateRollback(ctx, admission, params, info, response, clk)
		return true, owner, err
	case detectorActSleep && exactSessionSleepDrainCandidate(params, info, response, clk):
		owner, err := reconcileExactSessionSleepDrain(ctx, admission, params, info, response, clk)
		return true, owner, err
	case detectorActStranded && exactSessionStrandedRepairCandidate(params, info, response, clk):
		owner, err := reconcileExactSessionStrandedRepair(ctx, admission, params, info, response, clk)
		return true, owner, err
	}
	// D-ZOMBIE is LAST, and it is an `if` rather than a switch case. Both
	// choices are recorded §3 deltas.
	//
	// Last, because legacy's zombie arm is the one arm in the forward pass that
	// does NOT claim its row: it marks and falls through, so every sibling still
	// evaluates the same row on the same tick. A switch case returns handled and
	// therefore claims, and there is no position in legacy's textual order that
	// reproduces "claims nothing" — so the closest achievable analog is the
	// position where a claim preempts least. Placing it above D-DRIFT or
	// D-DEADLINE, where legacy's block textually sits, would starve those
	// families of a row on every sweep for the sake of an ordering that legacy
	// never actually enforces.
	//
	// An `if` rather than a case, because the guard's single liveness
	// observation has to reach the handler. This family's whole condition is
	// provider I/O, and a bool-returning case would make the handler probe a
	// second time; a second probe may disagree with the first and leave the row
	// owned by neither arm — WD.4 delta 2's rule, applied to the family that
	// meets it hardest.
	if detectorActZombie {
		if candidate, ok := exactSessionZombieMarkCandidate(params, info, response); ok {
			// The arm is unconditionally keyed-owned: every refusal inside it is
			// a zero-effect release of the key, never a legacy handback.
			return true, exactSessionStartKeyedOwner,
				reconcileExactSessionZombieMark(ctx, admission, params, info, response, candidate, clk)
		}
	}
	return false, exactSessionStartUnowned, nil
}

// exactSessionDeadline names which lifecycle timer fired for one durable row.
type exactSessionDeadline struct {
	Site   TraceSiteCode
	MaxAge bool
}

// exactSessionDeadlineTriggered re-derives the D-DEADLINE trigger rung from the
// durable row and the fleet's own timer trackers. It is the seam's dispatch
// predicate and the handler's pre-stop re-verification, so both answer from the
// same source. Legacy's arm order is preserved: max-session-age is consulted
// before idle-timeout.
func exactSessionDeadlineTriggered(params exactSessionStartParams, info sessionpkg.Info, now time.Time) []exactSessionDeadline {
	name := strings.TrimSpace(info.SessionNameMetadata)
	if name == "" || info.Closed || !detectorBeadAwake(info) {
		return nil
	}
	template := normalizedSessionTemplateInfo(info, params.Config)
	if template == "" {
		template = info.Template
	}
	var fired []exactSessionDeadline
	if params.MaxSessionAgeTracker != nil {
		if completeAt, ok := parseRFC3339Metadata(info.CreationCompleteAt); ok &&
			params.MaxSessionAgeTracker.shouldRestart(name, template, completeAt, now) {
			fired = append(fired, exactSessionDeadline{Site: TraceSiteReconcilerMaxSessionAge, MaxAge: true})
		}
	}
	if params.IdleTracker != nil && params.IdleTracker.checkIdle(name, template, params.Provider, now) {
		fired = append(fired, exactSessionDeadline{Site: TraceSiteReconcilerIdleTimeout})
	}
	return fired
}

// exactSessionDeadlineStopCandidate is the seam's guard. A durable blocker
// keeps the row out of the family entirely — a user-hold row belongs to the
// suspend arm above, and a quarantined row belongs to nobody.
//
// The blocker is asked PER FIRED TIMER, not once for the row, because the two
// timers do not take the same set (deadlineTimerBlockerInfo). A row-wide guard
// lets the idle ladder's pin exemption swallow the age restart as well, and the
// age restart is the credential refresh — so one unblocked deadline is enough to
// admit the row and let decideExactSessionDeadline apply each arm's own rung.
func exactSessionDeadlineStopCandidate(params exactSessionStartParams, info sessionpkg.Info, response sessionpkg.PersistedResponse, now time.Time) bool {
	if response.Revision == 0 || (params.IdleTracker == nil && params.MaxSessionAgeTracker == nil) {
		return false
	}
	for _, deadline := range exactSessionDeadlineTriggered(params, info, now) {
		if deadlineTimerBlockerInfo(info, now, deadline.MaxAge) == "" {
			return true
		}
	}
	return false
}

// reconcileExactSessionDeadlineStop stops one over-deadline session by exact
// key. It reuses ga-f7v2ft.102's stop machinery verbatim — the D2 capability
// pair, the token-bound unattended stop, and the fresh-death confirmation — and
// adds the one thing a deadline stop needs that a suspend stop does not: the
// sleep patch lands BEFORE the key is released, so a same-tick D-WAKE cannot
// respawn the incarnation this handler just killed.
func reconcileExactSessionDeadlineStop(
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
	recorder := params.Recorder
	if recorder == nil {
		recorder = events.Discard
	}
	yieldOrPark := func(cause error) (exactSessionStartOwner, error) {
		if params.RolloutMode == rollout.Auto {
			return exactSessionStartLegacyOwner, fmt.Errorf("%w: %w", errSessionStartLegacyFallbackRequired, cause)
		}
		return exactSessionStartKeyedOwner, cause
	}
	if params.DrainTracker != nil && params.DrainTracker.get(info.ID) != nil {
		return yieldOrPark(errors.New("exact over-deadline session has an active legacy drain"))
	}
	if _, ok := params.Provider.(runtime.FreshLivenessObserver); !ok {
		return yieldOrPark(errors.New("exact over-deadline session provider cannot prove fresh liveness"))
	}
	if _, ok := params.Provider.(runtime.UnattendedSessionStopper); !ok {
		return yieldOrPark(errors.New("exact over-deadline session provider cannot prove unattended stop"))
	}
	// The sleep patch is the whole point of the ordering guarantee, so an
	// AMBIGUOUS writer must never enter the provider. A merely absent
	// conditional writer is not ambiguous: conditional writes are gated per
	// store and off by default, and the legacy arm this replaces writes the
	// same patch through the plain front door. persistExactSessionDeadlineSleep
	// takes the CAS fence when it exists and the front door when it does not.
	if params.StatusWriterError != nil {
		return yieldOrPark(fmt.Errorf("exact over-deadline sleep writer: %w", params.StatusWriterError))
	}

	now := clk.Now().UTC()
	name := strings.TrimSpace(info.SessionNameMetadata)
	decision, deadline, ok := decideExactSessionDeadline(params, info, clk, now)
	if !ok {
		// Every rung below the trigger is a defer: blocker, pending interaction,
		// or open assigned work. Record it and release the key with zero effect;
		// the condition is level-triggered and re-detected next sweep.
		recordExactSessionDeadlineTrace(params, admission, info, deadline.Site, decision, 0, false)
		return exactSessionStartKeyedOwner, nil
	}

	processNames := drainAckStopPendingProcessNames(params.Config, info)
	incarnationStartedAt := drainAckIncarnationStartedAt(info)
	liveness := runtime.ObserveFreshLiveness(params.Provider, runtime.LivenessTarget{
		SessionID:            info.ID,
		SessionName:          name,
		ProcessNames:         processNames,
		IncarnationStartedAt: incarnationStartedAt,
	})
	// Scan completeness proves ABSENCE; a positive observation is decisive on its
	// own. This arm's stop is destructive BY INTENT — the target is killed
	// precisely because it is alive — and that is exactly what made the
	// unconditional gate a wedge rather than a safety net: a live pane withholds
	// the tmux-absence license (TmuxSessionProvenAbsent) that lets the /proc sweep
	// clear post-incarnation strangers, so on a busy host Complete is unreachable
	// for the very targets this family exists to stop, and the max-age kill —
	// which is the fleet's credential refresh — silently never fired (ga-bxa8r).
	//
	// Completeness was never what fenced identity here. Stopping the WRONG
	// incarnation is refused by the revision + instance-token + name re-read
	// below, by the token-bound unattended stop, and by
	// confirmDrainAckRuntimeDeadCompletion's COMPLETE proven-dead confirm — which
	// stays, and is satisfiable, because once the pane is gone the license IS
	// granted. So the destructive path still ends on a complete proof; it just no
	// longer demands one it cannot obtain before acting.
	if !liveness.Running && !liveness.Alive {
		if !liveness.Complete {
			// Dead cannot be told apart from unobserved, and this arm's own
			// no-op hand-off would silently retire a deadline the row still
			// owes. Fail closed; the condition is level-triggered.
			return yieldOrPark(errors.New("exact over-deadline session liveness observation is incomplete"))
		}
		// Nothing to stop. A durably-awake row with a dead runtime is D-ORPHAN's
		// and D-SLEEP's condition, not this family's.
		return exactSessionStartKeyedOwner, nil
	}

	latest, latestResponse, readErr := getAuthoritativeSessionStartPersistedRecord(params.Store, info.ID)
	if readErr != nil || latestResponse.Revision != response.Revision || latest.Closed ||
		strings.TrimSpace(latest.InstanceToken) != strings.TrimSpace(info.InstanceToken) ||
		strings.TrimSpace(latest.SessionNameMetadata) != name ||
		!exactSessionDeadlineStopCandidate(params, latest, latestResponse, clk.Now().UTC()) {
		return exactSessionStartKeyedOwner, nil
	}
	if params.DrainTracker != nil && params.DrainTracker.get(info.ID) != nil {
		return yieldOrPark(errors.New("exact over-deadline session entered an active legacy drain before stop"))
	}

	stopStartedAt := time.Now()
	if stopErr := workerStopUnattendedSessionByIDWithConfig(params.CityPath, params.Store, params.Provider, params.Config, info.ID, info.InstanceToken); stopErr != nil {
		return exactSessionStartKeyedOwner, fmt.Errorf("stopping exact over-deadline session %q: %w", info.ID, stopErr)
	}
	if completion := confirmDrainAckRuntimeDeadCompletion(params.CityPath, params.Store, params.Provider, params.Config, info.ID, name, info.InstanceToken, processNames, stderr, incarnationStartedAt, true); completion != drainAckAsyncStopConfirmed {
		return exactSessionStartKeyedOwner, fmt.Errorf("confirming exact over-deadline session %q stopped: %v", info.ID, completion)
	}
	_ = params.Provider.ClearScrollback(name) //nolint:errcheck // scrollback clearing is best-effort, matching the legacy arm

	if err := persistExactSessionDeadlineSleep(params, info, latestResponse.Revision, sessionpkg.SleepPatch(clk.Now().UTC(), decision.SleepReason)); err != nil {
		return exactSessionStartKeyedOwner, err
	}

	subject := strings.TrimSpace(info.AgentName)
	if subject == "" {
		subject = name
	}
	if deadline.MaxAge {
		recorder.Record(events.Event{Type: events.SessionMaxAgeKilled, Actor: "gc", Subject: subject})
		telemetry.RecordAgentMaxAgeKill(ctx, subject)
	} else {
		recorder.Record(events.Event{Type: events.SessionIdleKilled, Actor: "gc", Subject: subject})
		telemetry.RecordAgentIdleKill(ctx, subject)
	}
	recordExactSessionDeadlineTrace(params, admission, info, deadline.Site, decision, time.Since(stopStartedAt), true)
	return exactSessionStartKeyedOwner, nil
}

// decideExactSessionDeadline runs the existing pure ladders over facts gathered
// per key. Legacy gathers the same facts fleet-wide; the only difference is that
// here the pending probe and the reachable-store scan are paid once, for one
// session that actually hit a deadline.
func decideExactSessionDeadline(
	params exactSessionStartParams,
	info sessionpkg.Info,
	clk clock.Clock,
	now time.Time,
) (sessionpkg.TimerDecision, exactSessionDeadline, bool) {
	name := strings.TrimSpace(info.SessionNameMetadata)
	var lastDecision sessionpkg.TimerDecision
	var lastDeadline exactSessionDeadline
	for _, deadline := range exactSessionDeadlineTriggered(params, info, now) {
		decide := sessionpkg.DecideIdleTimeout
		hasAssignedWork := sessionHasAwakeAssignedWorkForReachableStore
		if deadline.MaxAge {
			decide = sessionpkg.DecideMaxSessionAge
			hasAssignedWork = sessionHasOpenAssignedWorkForReachableStore
		}
		facts := sessionpkg.TimerFacts{Triggered: true, Blocker: deadlineTimerBlockerInfo(info, now, deadline.MaxAge)}
		dec := decide(facts)
		for dec.Action == sessionpkg.TimerActionGatherPending ||
			dec.Action == sessionpkg.TimerActionGatherAssignedWork ||
			dec.Action == sessionpkg.TimerActionGatherMinFloor {
			switch dec.Action {
			case sessionpkg.TimerActionGatherPending:
				facts.Pending = sessionpkg.PendingNo
				if pendingInteractionKeepsAwakeInfo(info, params.Provider, name, clk) {
					facts.Pending = sessionpkg.PendingYes
				}
			case sessionpkg.TimerActionGatherAssignedWork:
				// Fail closed on a store blip, exactly as the fleet arms do: a
				// session that may still hold in-flight work is not killed.
				has, err := hasAssignedWork(params.CityPath, params.Config, params.Store, params.RigStores, info)
				if err != nil {
					has = true
				}
				facts.AssignedWork = sessionpkg.AssignedWorkNone
				if has {
					facts.AssignedWork = sessionpkg.AssignedWorkHas
				}
			case sessionpkg.TimerActionGatherMinFloor:
				facts.MinFloor = sessionpkg.MinFloorNo
				if exactSessionDeadlineMinFloorExempt(params, info) {
					facts.MinFloor = sessionpkg.MinFloorYes
				}
			}
			dec = decide(facts)
		}
		// The consecutive same-bead assigned-work backstop travels with the
		// ladder (ga-nllza6). Without it here, an acting D-DEADLINE would defer a
		// wedged session forever: legacy yields the key, so its own backstop
		// never sees the defer.
		if !deadline.MaxAge && params.AssignedWorkDeferTracker != nil {
			if dec.Action == sessionpkg.TimerActionDefer && dec.TraceReason == string(TraceReasonAssignedWork) {
				if params.AssignedWorkDeferTracker.recordDefer(name, normalizedSessionTemplateInfo(info, params.Config), strings.TrimSpace(info.CurrentlyProcessingBeadID)) {
					dec = sessionpkg.DecideAssignedWorkExhausted()
				}
			} else {
				params.AssignedWorkDeferTracker.reset(name)
			}
		}
		if dec.Action == sessionpkg.TimerActionStop {
			return dec, deadline, true
		}
		lastDecision, lastDeadline = dec, deadline
	}
	return lastDecision, lastDeadline, false
}

// exactSessionDeadlineMinFloorExempt answers DecideIdleTimeout's keep-warm floor
// rung (sc-5mtyhy) for one key: is this row one of the deterministic
// min_active_sessions floor members, which the idle path defers instead of
// killing and cold-recreating every tick.
//
// The fleet arm ranks the floor off the tick's coherent infoByID snapshot. A
// keyed admission has no tick, so it re-reads the inventory through the same
// front door the stall family's floor rung uses and hands it to the SAME
// predicate the fleet arm calls, so the two sides cannot answer differently for
// the same row. The store read is paid only once a configured floor exists —
// the overwhelmingly common minFloor==0 template short-circuits above it, so an
// ordinary idle admission still costs nothing.
//
// A store failure fails CLOSED (exempt), matching the assigned-work gather
// beside it: an unreadable fleet must not idle-kill a session that may be the
// one holding its pool floor warm.
func exactSessionDeadlineMinFloorExempt(params exactSessionStartParams, info sessionpkg.Info) bool {
	template := normalizedSessionTemplateInfo(info, params.Config)
	cfgAgent := findAgentByTemplate(params.Config, template)
	if cfgAgent == nil || cfgAgent.EffectiveMinActiveSessions() <= 0 {
		return false
	}
	rows, err := sessionFrontDoor(params.Store).ListAllForReconcile(sessionpkg.ListAllOptions{})
	if err != nil {
		return true
	}
	infoByID := make(map[string]sessionpkg.Info, len(rows))
	for i := range rows {
		infoByID[rows[i].Info.ID] = rows[i].Info
	}
	return isMinFloorExemptIdleSession(infoByID, params.Config, template, info.ID)
}

// persistExactSessionDeadlineSleep lands the sleep patch BEFORE the key is
// released, so a D-WAKE admission on the same key cannot respawn the
// incarnation this handler just killed. Where the store offers conditional
// writes the patch is fenced on the revision the pre-stop reread proved, with
// one bounded retry: the stop already happened, so a lost CAS must not leave
// the row claiming awake. Where it does not, the patch goes through the same
// session front door the legacy arm writes it through.
func persistExactSessionDeadlineSleep(params exactSessionStartParams, info sessionpkg.Info, revision int64, patch sessionpkg.MetadataPatch) error {
	if params.StatusWriter == nil {
		if err := sessionFrontDoor(params.Store).ApplyPatch(info.ID, patch); err != nil {
			return fmt.Errorf("persisting exact over-deadline sleep for %q: %w", info.ID, err)
		}
		return nil
	}
	err := params.StatusWriter.UpdateIfMatch(info.ID, revision, beads.UpdateOpts{Metadata: patch})
	if err == nil {
		return nil
	}
	latest, latestResponse, readErr := getAuthoritativeSessionStartPersistedRecord(params.Store, info.ID)
	if readErr != nil || latest.Closed || latestResponse.Revision == 0 ||
		strings.TrimSpace(latest.InstanceToken) != strings.TrimSpace(info.InstanceToken) {
		return fmt.Errorf("persisting exact over-deadline sleep for %q: %w", info.ID, err)
	}
	if retryErr := params.StatusWriter.UpdateIfMatch(info.ID, latestResponse.Revision, beads.UpdateOpts{Metadata: patch}); retryErr != nil {
		return fmt.Errorf("persisting exact over-deadline sleep for %q: %w", info.ID, retryErr)
	}
	return nil
}

// recordExactSessionDeadlineTrace fires the SAME legacy trace site the fleet arm
// fires, with effect_owner=keyed and the honest effect_applied. The WD.15 parity
// join reads exactly these fields to separate the legacy, keyed, and
// detector-shadow populations on a shared cycle.
func recordExactSessionDeadlineTrace(
	params exactSessionStartParams,
	admission sessionStartAdmission,
	info sessionpkg.Info,
	site TraceSiteCode,
	decision sessionpkg.TimerDecision,
	duration time.Duration,
	applied bool,
) {
	if params.Trace == nil || site == "" || decision.Action == sessionpkg.TimerActionNone {
		return
	}
	cycle := params.Trace.BeginCycle(TraceTickTriggerControl, "exact_session_deadline_stop", time.Now().UTC(), params.Config)
	if cycle == nil {
		return
	}
	template := normalizedSessionTemplateInfo(info, params.Config)
	reason, outcome := timerTraceCodes(decision)
	cycle.recordKeyedEffect(
		site,
		reason,
		outcome,
		"exact_session_deadline_stop",
		template,
		info.ID,
		info.SessionNameMetadata,
		duration,
		map[string]any{
			"admission":         string(admission.Source),
			"admission_version": admission.Version,
			"generation":        params.Generation,
			"instance_token":    info.InstanceToken,
			"sleep_reason":      decision.SleepReason,
			"effect_owner":      detectorKeyedEffectOwner,
			"effect_applied":    applied,
		},
	)
	if err := cycle.End(TraceCompletionCompleted, nil); err != nil && params.Stderr != nil {
		fmt.Fprintf(params.Stderr, "session reconciler: recording exact deadline stop trace: %v\n", err) //nolint:errcheck // tracing is observational
	}
}
