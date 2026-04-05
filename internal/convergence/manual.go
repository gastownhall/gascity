package convergence

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
)

// ApproveHandler processes an operator's approval of a convergence loop
// that is in the waiting_manual state. It terminates the loop with
// terminal_reason=approved.
//
// Idempotent: if the bead is already terminated with reason=approved,
// returns a no-op result without error.
//
// Write ordering contract: last_processed_wisp is written LAST (dedup marker).
func (h *Handler) ApproveHandler(_ context.Context, beadID, username, _ string) (HandlerResult, error) {
	meta, err := h.Store.GetMetadata(beadID)
	if err != nil {
		return HandlerResult{}, fmt.Errorf("reading bead %q metadata: %w", beadID, err)
	}

	state := meta[FieldState]
	actor := "operator:" + username

	// Idempotent: already terminated+approved is a no-op.
	if state == StateTerminated && meta[FieldTerminalReason] == TerminalApproved {
		return HandlerResult{
			Action: ActionApproved,
		}, nil
	}

	// Must be in waiting_manual state.
	if state != StateWaitingManual {
		return HandlerResult{}, fmt.Errorf(
			"cannot approve bead %q: state is %q, expected %q",
			beadID, state, StateWaitingManual,
		)
	}

	// Derive iteration count from children for event payload.
	iterationCount, err := h.deriveIterationCount(beadID)
	if err != nil {
		return HandlerResult{}, fmt.Errorf("deriving iteration count for bead %q: %w", beadID, err)
	}

	// Read the last active wisp for event payload.
	activeWisp := meta[FieldActiveWisp]
	lastProcessedWisp := meta[FieldLastProcessedWisp]
	// Use the most recent wisp reference for the event.
	eventWispID := lastProcessedWisp
	if activeWisp != "" {
		eventWispID = activeWisp
	}

	// Compute cumulative duration for terminated event.
	_, cumDur := h.computeDurations(beadID, eventWispID)

	// Write ordering: terminal_reason, terminal_actor, clear waiting_reason,
	// then state=terminated, then EventTerminated (TierCritical, before CloseBead),
	// then CloseBead, then ManualApprove (TierBestEffort), then last_processed_wisp LAST.
	if err := h.Store.SetMetadata(beadID, FieldTerminalReason, TerminalApproved); err != nil {
		return HandlerResult{}, fmt.Errorf("setting terminal reason: %w", err)
	}
	if err := h.Store.SetMetadata(beadID, FieldTerminalActor, actor); err != nil {
		return HandlerResult{}, fmt.Errorf("setting terminal actor: %w", err)
	}
	if err := h.Store.SetMetadata(beadID, FieldWaitingReason, ""); err != nil {
		return HandlerResult{}, fmt.Errorf("clearing waiting reason: %w", err)
	}
	if err := h.Store.SetMetadata(beadID, FieldState, StateTerminated); err != nil {
		return HandlerResult{}, fmt.Errorf("setting state to terminated: %w", err)
	}

	// Emit EventTerminated BEFORE CloseBead — TierCritical requires at-least-once
	// delivery, so it must be emitted while the bead is still open for reconciliation
	// replay if the controller crashes before CloseBead completes.
	termPayload := TerminatedPayload{
		TerminalReason:       TerminalApproved,
		TotalIterations:      iterationCount,
		FinalStatus:          "closed",
		Actor:                actor,
		CumulativeDurationMs: cumDur.Milliseconds(),
	}
	h.emitEvent(EventTerminated, EventIDTerminated(beadID), beadID, termPayload)

	if err := h.Store.CloseBead(beadID); err != nil {
		return HandlerResult{}, fmt.Errorf("closing bead %q: %w", beadID, err)
	}

	// Emit ManualApprove AFTER CloseBead — TierBestEffort, fire-and-forget.
	approvePayload := ManualActionPayload{
		Actor:      actor,
		PriorState: StateWaitingManual,
		NewState:   StateTerminated,
		Iteration:  iterationCount,
		WispID:     NullableString(eventWispID),
	}
	h.emitEvent(EventManualApprove, EventIDManualApprove(beadID), beadID, approvePayload)

	// last_processed_wisp LAST — dedup marker contract.
	if lastProcessedWisp != "" {
		if err := h.Store.SetMetadata(beadID, FieldLastProcessedWisp, lastProcessedWisp); err != nil {
			return HandlerResult{}, fmt.Errorf("setting last processed wisp: %w", err)
		}
	}

	return HandlerResult{
		Action:    ActionApproved,
		Iteration: iterationCount,
	}, nil
}

// IterateHandler processes an operator's request to continue iterating a
// convergence loop that is in the waiting_manual state. It pours a new
// wisp and transitions the loop back to active state.
//
// Write ordering contract: last_processed_wisp is NOT written here because
// the new wisp hasn't been processed yet — it will be written when the
// new wisp closes.
func (h *Handler) IterateHandler(_ context.Context, beadID, username, _ string) (HandlerResult, error) {
	meta, err := h.Store.GetMetadata(beadID)
	if err != nil {
		return HandlerResult{}, fmt.Errorf("reading bead %q metadata: %w", beadID, err)
	}

	state := meta[FieldState]

	// Must be in waiting_manual state.
	if state != StateWaitingManual {
		return HandlerResult{}, fmt.Errorf(
			"cannot iterate bead %q: state is %q, expected %q",
			beadID, state, StateWaitingManual,
		)
	}

	// Check iteration < max_iterations.
	iterationCount, err := h.deriveIterationCount(beadID)
	if err != nil {
		return HandlerResult{}, fmt.Errorf("deriving iteration count for bead %q: %w", beadID, err)
	}
	maxIterations, _ := DecodeInt(meta[FieldMaxIterations])
	if iterationCount >= maxIterations {
		return HandlerResult{}, fmt.Errorf(
			"cannot iterate bead %q: at max iterations (%d/%d)",
			beadID, iterationCount, maxIterations,
		)
	}

	actor := "operator:" + username

	// Read the last processed wisp for verdict scoping.
	lastProcessedWisp := meta[FieldLastProcessedWisp]

	// Pour next wisp with idempotency key BEFORE any state mutations.
	// If PourWisp fails, the bead stays in waiting_manual (safe to retry).
	nextIteration := iterationCount + 1
	nextKey := IdempotencyKey(beadID, nextIteration)
	formula := meta[FieldFormula]
	vars := ExtractVars(meta)
	evaluatePrompt := meta[FieldEvaluatePrompt]

	nextWispID, err := h.Store.PourWisp(beadID, formula, nextKey, vars, evaluatePrompt)
	if err != nil {
		// Check if wisp was created despite the error.
		existingID, found, lookupErr := h.Store.FindByIdempotencyKey(nextKey)
		if lookupErr == nil && found {
			nextWispID = existingID
		} else {
			return HandlerResult{}, fmt.Errorf("pouring next wisp for bead %q: %w", beadID, err)
		}
	}

	// PourWisp succeeded — now mutate state.
	// Clear verdict (scoped to last processed wisp) after PourWisp so it's
	// preserved if PourWisp fails and the operator retries.
	if lastProcessedWisp != "" && meta[FieldAgentVerdictWisp] == lastProcessedWisp {
		if err := h.Store.SetMetadata(beadID, FieldAgentVerdict, ""); err != nil {
			return HandlerResult{}, fmt.Errorf("clearing agent verdict: %w", err)
		}
		if err := h.Store.SetMetadata(beadID, FieldAgentVerdictWisp, ""); err != nil {
			return HandlerResult{}, fmt.Errorf("clearing agent verdict wisp: %w", err)
		}
	}
	// Clear waiting_reason and set state=active.
	if err := h.Store.SetMetadata(beadID, FieldWaitingReason, ""); err != nil {
		return HandlerResult{}, fmt.Errorf("clearing waiting reason: %w", err)
	}
	if err := h.Store.SetMetadata(beadID, FieldState, StateActive); err != nil {
		return HandlerResult{}, fmt.Errorf("setting state to active: %w", err)
	}

	// Set active_wisp.
	if err := h.Store.SetMetadata(beadID, FieldActiveWisp, nextWispID); err != nil {
		return HandlerResult{}, fmt.Errorf("setting active wisp: %w", err)
	}

	// Emit ConvergenceManualIterate event.
	iterPayload := ManualActionPayload{
		Actor:      actor,
		PriorState: StateWaitingManual,
		NewState:   StateActive,
		Iteration:  nextIteration,
		WispID:     NullableString(lastProcessedWisp),
		NextWispID: NullableString(nextWispID),
	}
	h.emitEvent(EventManualIterate, EventIDManualIterate(beadID, nextIteration), beadID, iterPayload)

	return HandlerResult{
		Action:     ActionIterate,
		Iteration:  nextIteration,
		NextWispID: nextWispID,
	}, nil
}

// StopHandler processes an operator's request to stop a convergence loop.
// The loop can be in active or waiting_manual state. It terminates the loop
// with terminal_reason=stopped.
//
// Enhanced stop sequence:
//  1. Validate state (active or waiting_manual)
//  2. Drain completed iteration — if active wisp is already closed, process it
//     through HandleWispClosed first to avoid discarding a legitimate iteration
//  3. Force-close active wisp — if still open after drain, force-close it
//  4. Derive iteration count (after force-close so count is accurate)
//  5. Clear stale verdicts — prevent interrupted wisp's verdict from leaking
//  6. Write terminal state metadata
//     7a. Emit synthetic ConvergenceIteration for force-closed wisp BEFORE CloseBead (TierCritical)
//     7b. Emit EventTerminated BEFORE CloseBead (TierCritical)
//  8. CloseBead
//  9. Emit ManualStop AFTER CloseBead (TierBestEffort)
//  10. Write last_processed_wisp LAST (dedup marker)
//
// Idempotent: if the bead is already terminated with reason=stopped,
// returns a no-op result without error.
//
// Write ordering contract: last_processed_wisp is written LAST (dedup marker).
func (h *Handler) StopHandler(ctx context.Context, beadID, username, _ string) (HandlerResult, error) {
	meta, err := h.Store.GetMetadata(beadID)
	if err != nil {
		return HandlerResult{}, fmt.Errorf("reading bead %q metadata: %w", beadID, err)
	}

	state := meta[FieldState]
	actor := "operator:" + username

	// Idempotent: already terminated+stopped is a no-op.
	if state == StateTerminated && meta[FieldTerminalReason] == TerminalStopped {
		return HandlerResult{
			Action: ActionStopped,
		}, nil
	}

	// Must be active or waiting_manual.
	if state != StateActive && state != StateWaitingManual {
		return HandlerResult{}, fmt.Errorf(
			"cannot stop bead %q: state is %q, expected %q or %q",
			beadID, state, StateActive, StateWaitingManual,
		)
	}

	activeWisp := meta[FieldActiveWisp]
	lastProcessedWisp := meta[FieldLastProcessedWisp]
	forceClosedWisp := false

	// Step 2: Drain completed iteration — if the active wisp is already closed,
	// process it through HandleWispClosed before stopping. This prevents
	// discarding a legitimately completed iteration.
	if activeWisp != "" {
		wispInfo, err := h.Store.GetBead(activeWisp)
		if err != nil {
			if !errors.Is(err, beads.ErrNotFound) {
				return HandlerResult{}, fmt.Errorf("reading active wisp %q: %w", activeWisp, err)
			}
			recoveredWisp, found, recoverErr := h.recoverCurrentActiveWisp(beadID, lastProcessedWisp)
			if recoverErr != nil {
				return HandlerResult{}, recoverErr
			}
			if !found {
				activeWisp = ""
			} else {
				activeWisp = recoveredWisp.ID
				wispInfo = recoveredWisp
			}
		}

		if activeWisp != "" && wispInfo.Status == "closed" {
			// Drain: process the completed wisp through the normal handler.
			_, drainErr := h.HandleWispClosed(ctx, beadID, activeWisp)
			if drainErr != nil {
				return HandlerResult{}, fmt.Errorf("draining completed wisp %q: %w", activeWisp, drainErr)
			}

			// Re-read metadata after drain — HandleWispClosed may have terminated
			// the loop (gate passed or max iterations reached).
			meta, err = h.Store.GetMetadata(beadID)
			if err != nil {
				return HandlerResult{}, fmt.Errorf("re-reading metadata after drain: %w", err)
			}
			if meta[FieldState] == StateTerminated {
				// HandleWispClosed already terminated the loop — stop is a no-op.
				return HandlerResult{
					Action: ActionStopped,
				}, nil
			}
			// Update local vars from refreshed metadata.
			state = meta[FieldState]
			activeWisp = meta[FieldActiveWisp]
			lastProcessedWisp = meta[FieldLastProcessedWisp]
		}
	}

	// Step 3: Force-close active wisp if still open.
	if activeWisp != "" {
		wispInfo, err := h.Store.GetBead(activeWisp)
		if err != nil {
			if !errors.Is(err, beads.ErrNotFound) {
				return HandlerResult{}, fmt.Errorf("reading active wisp %q for force-close: %w", activeWisp, err)
			}
			recoveredWisp, found, recoverErr := h.recoverCurrentActiveWisp(beadID, lastProcessedWisp)
			if recoverErr != nil {
				return HandlerResult{}, recoverErr
			}
			if !found {
				activeWisp = ""
			} else {
				activeWisp = recoveredWisp.ID
				wispInfo = recoveredWisp
			}
		}
		if activeWisp != "" && wispInfo.Status != "closed" {
			if err := h.Store.CloseBead(activeWisp); err != nil {
				return HandlerResult{}, fmt.Errorf("force-closing active wisp %q: %w", activeWisp, err)
			}
			forceClosedWisp = true
		}
	}

	// Step 4: Derive iteration count from children (after force-close so
	// the count includes the force-closed wisp).
	iterationCount, err := h.deriveIterationCount(beadID)
	if err != nil {
		return HandlerResult{}, fmt.Errorf("deriving iteration count for bead %q: %w", beadID, err)
	}

	// Step 5: Clear stale verdicts — prevent an interrupted wisp's verdict
	// from leaking into a future retry.
	if err := h.Store.SetMetadata(beadID, FieldAgentVerdict, ""); err != nil {
		return HandlerResult{}, fmt.Errorf("clearing stale agent verdict: %w", err)
	}
	if err := h.Store.SetMetadata(beadID, FieldAgentVerdictWisp, ""); err != nil {
		return HandlerResult{}, fmt.Errorf("clearing stale agent verdict wisp: %w", err)
	}

	// Use the best available wisp reference for event payloads.
	eventWispID := lastProcessedWisp
	if activeWisp != "" {
		eventWispID = activeWisp
	}

	// Compute cumulative duration for terminated event.
	_, cumDur := h.computeDurations(beadID, eventWispID)

	// Step 6: Write ordering: terminal_reason, terminal_actor, clear waiting_reason,
	// then state=terminated.
	if err := h.Store.SetMetadata(beadID, FieldTerminalReason, TerminalStopped); err != nil {
		return HandlerResult{}, fmt.Errorf("setting terminal reason: %w", err)
	}
	if err := h.Store.SetMetadata(beadID, FieldTerminalActor, actor); err != nil {
		return HandlerResult{}, fmt.Errorf("setting terminal actor: %w", err)
	}
	if err := h.Store.SetMetadata(beadID, FieldWaitingReason, ""); err != nil {
		return HandlerResult{}, fmt.Errorf("clearing waiting reason: %w", err)
	}
	if err := h.Store.SetMetadata(beadID, FieldState, StateTerminated); err != nil {
		return HandlerResult{}, fmt.Errorf("setting state to terminated: %w", err)
	}

	// Step 7a: Emit synthetic ConvergenceIteration for force-closed wisp
	// BEFORE CloseBead — TierCritical requires at-least-once delivery.
	if forceClosedWisp && activeWisp != "" {
		wispIteration := iterationCount // force-closed wisp is the latest
		iterDur, synthCumDur := h.computeDurations(beadID, activeWisp)
		gateMode := meta[FieldGateMode]
		if gateMode == "" {
			gateMode = GateModeManual
		}
		synthPayload := IterationPayload{
			Iteration:            wispIteration,
			WispID:               activeWisp,
			Action:               string(ActionStopped),
			GateMode:             gateMode,
			IterationDurationMs:  iterDur.Milliseconds(),
			CumulativeDurationMs: synthCumDur.Milliseconds(),
		}
		h.emitEvent(EventIteration, EventIDIteration(beadID, wispIteration), beadID, synthPayload)
	}

	// Step 7b: Emit EventTerminated BEFORE CloseBead — TierCritical requires
	// at-least-once delivery, so it must be emitted while the bead is still
	// open for reconciliation replay if the controller crashes.
	termPayload := TerminatedPayload{
		TerminalReason:       TerminalStopped,
		TotalIterations:      iterationCount,
		FinalStatus:          "closed",
		Actor:                actor,
		CumulativeDurationMs: cumDur.Milliseconds(),
	}
	h.emitEvent(EventTerminated, EventIDTerminated(beadID), beadID, termPayload)

	// Step 8: CloseBead.
	if err := h.Store.CloseBead(beadID); err != nil {
		return HandlerResult{}, fmt.Errorf("closing bead %q: %w", beadID, err)
	}

	// Step 9: Emit ManualStop AFTER CloseBead — TierBestEffort, fire-and-forget.
	stopPayload := ManualActionPayload{
		Actor:      actor,
		PriorState: state,
		NewState:   StateTerminated,
		Iteration:  iterationCount,
		WispID:     NullableString(eventWispID),
	}
	h.emitEvent(EventManualStop, EventIDManualStop(beadID), beadID, stopPayload)

	// Step 10: last_processed_wisp LAST — dedup marker contract.
	// After force-close, the force-closed wisp becomes the highest closed wisp.
	finalLPW := lastProcessedWisp
	if forceClosedWisp && activeWisp != "" {
		finalLPW = activeWisp
	}
	if finalLPW != "" {
		if err := h.Store.SetMetadata(beadID, FieldLastProcessedWisp, finalLPW); err != nil {
			return HandlerResult{}, fmt.Errorf("setting last processed wisp: %w", err)
		}
	}

	return HandlerResult{
		Action:    ActionStopped,
		Iteration: iterationCount,
	}, nil
}

func (h *Handler) recoverCurrentActiveWisp(beadID, lastProcessedWisp string) (BeadInfo, bool, error) {
	children, err := h.Store.Children(beadID)
	if err != nil {
		return BeadInfo{}, false, fmt.Errorf("listing children for stale active wisp recovery: %w", err)
	}

	nextIter := 0
	haveNextIter := false
	if lastProcessedWisp != "" {
		lastProcessedInfo, err := h.Store.GetBead(lastProcessedWisp)
		if err != nil {
			if !errors.Is(err, beads.ErrNotFound) {
				return BeadInfo{}, false, fmt.Errorf("reading last processed wisp %q: %w", lastProcessedWisp, err)
			}
		} else if iter, ok := ParseIterationFromKey(lastProcessedInfo.IdempotencyKey); ok {
			nextIter = iter + 1
			haveNextIter = true
		}
	}

	if haveNextIter {
		nextKey := IdempotencyKey(beadID, nextIter)
		candidateID, found, err := h.Store.FindByIdempotencyKey(nextKey)
		if err != nil {
			return BeadInfo{}, false, fmt.Errorf("looking up replacement active wisp %q: %w", nextKey, err)
		}
		if found {
			wispInfo, err := h.Store.GetBead(candidateID)
			if err != nil {
				if errors.Is(err, beads.ErrNotFound) {
					return BeadInfo{}, false, nil
				}
				return BeadInfo{}, false, fmt.Errorf("reading replacement active wisp %q: %w", candidateID, err)
			}
			return wispInfo, true, nil
		}
		return BeadInfo{}, false, nil
	}

	var bestOpen BeadInfo
	bestOpenIter := -1
	var bestClosed BeadInfo
	bestClosedIter := -1
	prefix := IdempotencyKeyPrefix(beadID)
	for _, child := range children {
		if !strings.HasPrefix(child.IdempotencyKey, prefix) {
			continue
		}
		iter, ok := ParseIterationFromKey(child.IdempotencyKey)
		if !ok {
			continue
		}
		switch child.Status {
		case "open", "in_progress":
			if iter > bestOpenIter {
				bestOpen = child
				bestOpenIter = iter
			}
		case "closed":
			if iter > bestClosedIter {
				bestClosed = child
				bestClosedIter = iter
			}
		}
	}
	if bestOpenIter >= 0 {
		return bestOpen, true, nil
	}
	if bestClosedIter >= 0 {
		return bestClosed, true, nil
	}
	return BeadInfo{}, false, nil
}
