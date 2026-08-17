package convergence

import (
	"context"
	"errors"
	"fmt"

	"github.com/gastownhall/gascity/internal/beads"
)

// ReconcileAction names the outcome of reconciling a single bead. The values
// are stable and surface verbatim in ReconcileReport details.
type ReconcileAction string

// ReconcileAction values describing what a recovery path did.
const (
	ActionNoAction          ReconcileAction = "no_action"
	ActionCompletedTerminal ReconcileAction = "completed_terminal"
	ActionAdoptedWisp       ReconcileAction = "adopted_wisp"
	ActionPouredWisp        ReconcileAction = "poured_wisp"
	ActionRepairedState     ReconcileAction = "repaired_state"
)

// ReconcileDetail records the outcome of reconciling a single bead.
type ReconcileDetail struct {
	BeadID string
	Action ReconcileAction
	Error  error // nil if successful
}

// ReconcileReport summarizes a full reconciliation pass.
type ReconcileReport struct {
	Scanned   int
	Recovered int
	Errors    int
	Details   []ReconcileDetail
}

// Reconciler performs startup reconciliation for convergence beads that
// were in-progress when the controller crashed.  It inspects each bead's
// metadata, determines which step of the convergence algorithm was
// interrupted, and completes or repairs the state so normal processing
// can resume.
type Reconciler struct {
	Handler *Handler // reuse the handler's Store and Emitter
}

// ReconcileBeads reconciles a set of convergence beads identified by ID.
// The caller (controller startup) is responsible for finding the bead IDs
// — typically all beads whose status is "in_progress" and that carry
// convergence metadata.
//
// Errors on individual beads are captured in the report; the scan
// continues through the full list.
func (r *Reconciler) ReconcileBeads(ctx context.Context, beadIDs []string) (ReconcileReport, error) {
	report := ReconcileReport{
		Scanned: len(beadIDs),
	}

	for _, id := range beadIDs {
		detail := r.reconcileBead(ctx, id)
		report.Details = append(report.Details, detail)
		if detail.Error != nil {
			report.Errors++
		} else if detail.Action != ActionNoAction {
			report.Recovered++
		}
	}

	return report, nil
}

// reconcileBead inspects a single convergence bead and performs whatever
// recovery action is needed.  It never returns an error directly —
// the (action, error) result of the chosen recovery path is wrapped into
// the returned ReconcileDetail at this single site.
func (r *Reconciler) reconcileBead(ctx context.Context, beadID string) ReconcileDetail {
	action, err := r.reconcileState(ctx, beadID)
	return ReconcileDetail{BeadID: beadID, Action: action, Error: err}
}

// reconcileState reads the bead's convergence state and dispatches to the
// recovery path for that state, returning the resulting (action, error).
func (r *Reconciler) reconcileState(ctx context.Context, beadID string) (ReconcileAction, error) {
	meta, err := r.Handler.Store.GetMetadata(beadID)
	if err != nil {
		return ActionNoAction, fmt.Errorf("reading metadata: %w", err)
	}

	state := meta[FieldState]

	switch state {
	case "":
		// Path 1a: Missing/empty state — the bead was created but the
		// convergence loop never started (or the state write was lost).
		return r.reconcileMissingState(ctx, beadID, meta)

	case StateCreating:
		// Path 1b: Creation was interrupted. Terminate the partial bead.
		return r.reconcileCreating(beadID)

	case StateTerminated:
		// Path 2: state=terminated but bead still in_progress — the
		// terminal transition started but CloseBead was not reached.
		return r.reconcileTerminatedNotClosed(beadID, meta)

	case StateWaitingManual:
		// Path 3: state=waiting_manual.
		return r.reconcileWaitingManual(beadID, meta)

	case StateWaitingTrigger:
		// Path 3t: state=waiting_trigger. No wisp is in flight while waiting
		// on the external trigger, so recovery only completes an interrupted
		// stop; otherwise the controller tick re-evaluates the trigger.
		return r.reconcileWaitingTrigger(beadID, meta)

	case StateActive:
		// Path 4: state=active.
		return r.reconcileActive(ctx, beadID, meta)

	default:
		return ActionNoAction, fmt.Errorf("unknown convergence state %q", state)
	}
}

// --- Path 1: Missing/empty state ---

func (r *Reconciler) reconcileMissingState(ctx context.Context, beadID string, meta map[string]string) (ReconcileAction, error) {
	// Check if there is already a wisp for iteration 1 (idempotency key
	// lookup).
	key1 := IdempotencyKey(beadID, 1)
	existingID, found, err := r.Handler.Store.FindByIdempotencyKey(key1)
	if err != nil {
		return ActionNoAction, fmt.Errorf("looking up iter-1 wisp: %w", err)
	}

	if found {
		// Wisp exists — adopt it, but check if it's already closed.
		wispInfo, err := r.Handler.Store.GetBead(existingID)
		if err != nil {
			return ActionAdoptedWisp, fmt.Errorf("reading wisp %q info: %w", existingID, err)
		}

		if err := r.Handler.Store.SetMetadata(beadID, FieldActiveWisp, existingID); err != nil {
			return ActionAdoptedWisp, fmt.Errorf("setting active_wisp: %w", err)
		}
		// Set iteration to match the adopted wisp: 1 if closed (we know
		// iteration 1 exists), 0 if still open (HandleWispClosed will
		// derive the correct count when it fires).
		adoptedIteration := 0
		if wispInfo.Status == "closed" {
			adoptedIteration = 1
		}
		if err := r.Handler.Store.SetMetadata(beadID, FieldIteration, EncodeInt(adoptedIteration)); err != nil {
			return ActionAdoptedWisp, fmt.Errorf("setting iteration: %w", err)
		}
		if err := r.Handler.Store.SetMetadata(beadID, FieldState, StateActive); err != nil {
			return ActionAdoptedWisp, fmt.Errorf("setting state: %w", err)
		}

		// If the adopted wisp is already closed, replay the transition
		// so the convergence loop doesn't stall in active with a dead wisp.
		if wispInfo.Status == "closed" {
			if _, err := r.Handler.HandleWispClosed(ctx, beadID, existingID); err != nil {
				return ActionAdoptedWisp, fmt.Errorf("replaying wisp_closed for adopted wisp %q: %w", existingID, err)
			}
		}

		return ActionAdoptedWisp, nil
	}

	// No wisp exists — pour the first one.
	formula := meta[FieldFormula]
	vars := ExtractVars(meta)
	evaluatePrompt := meta[FieldEvaluatePrompt]

	wispID, err := r.Handler.Store.PourWisp(beadID, formula, key1, vars, evaluatePrompt)
	if err != nil {
		return ActionPouredWisp, fmt.Errorf("pouring first wisp: %w", err)
	}

	if err := r.Handler.Store.SetMetadata(beadID, FieldActiveWisp, wispID); err != nil {
		return ActionPouredWisp, fmt.Errorf("setting active_wisp: %w", err)
	}
	if err := r.Handler.Store.SetMetadata(beadID, FieldIteration, EncodeInt(0)); err != nil {
		return ActionPouredWisp, fmt.Errorf("setting iteration: %w", err)
	}
	if err := r.Handler.Store.SetMetadata(beadID, FieldState, StateActive); err != nil {
		return ActionPouredWisp, fmt.Errorf("setting state: %w", err)
	}

	return ActionPouredWisp, nil
}

// --- Path 1b: state=creating (partial creation) ---

func (r *Reconciler) reconcileCreating(beadID string) (ReconcileAction, error) {
	if err := r.Handler.Store.SetMetadata(beadID, FieldTerminalReason, TerminalPartialCreation); err != nil {
		return ActionCompletedTerminal, fmt.Errorf("setting terminal_reason: %w", err)
	}
	if err := r.Handler.Store.SetMetadata(beadID, FieldTerminalActor, "recovery"); err != nil {
		return ActionCompletedTerminal, fmt.Errorf("setting terminal_actor: %w", err)
	}
	if err := r.Handler.Store.SetMetadata(beadID, FieldState, StateTerminated); err != nil {
		return ActionCompletedTerminal, fmt.Errorf("setting state to terminated: %w", err)
	}
	if err := r.Handler.Store.CloseBead(beadID, CloseReasonReconcileDone); err != nil {
		return ActionCompletedTerminal, fmt.Errorf("closing bead: %w", err)
	}
	return ActionCompletedTerminal, nil
}

// --- Path 2: state=terminated but bead not closed ---

func (r *Reconciler) reconcileTerminatedNotClosed(beadID string, meta map[string]string) (ReconcileAction, error) {
	// Check if the bead is actually already closed.
	beadInfo, err := r.Handler.Store.GetBead(beadID)
	if err != nil {
		return ActionNoAction, fmt.Errorf("reading bead info: %w", err)
	}
	if beadInfo.Status == "closed" {
		// Already fully terminated.
		return ActionNoAction, nil
	}

	// Backfill terminal_actor if missing.
	if err := r.backfillTerminalActor(beadID, meta); err != nil {
		return ActionCompletedTerminal, fmt.Errorf("backfilling terminal_actor: %w", err)
	}

	// Derive iteration count and cumulative duration for the terminated event
	// from a single child fetch (best-effort: zeros on error).
	children, _ := r.Handler.Store.Children(beadID)
	stats := childStats(children, beadID)

	// Emit ConvergenceTerminated (recovery).
	reason := meta[FieldTerminalReason]
	if reason == "" {
		reason = TerminalNoConvergence // safe default
	}
	actor := meta[FieldTerminalActor]
	if actor == "" {
		actor = "recovery"
	}

	termPayload := TerminatedPayload{
		TerminalReason:       reason,
		TotalIterations:      stats.ClosedCount,
		FinalStatus:          "closed",
		Actor:                actor,
		CumulativeDurationMs: stats.CumulativeDur.Milliseconds(),
	}
	r.emitRecoveryEvent(EventTerminated, EventIDTerminated(beadID), beadID, termPayload)

	// Close the bead.
	if err := r.Handler.Store.CloseBead(beadID, CloseReasonReconcileDone); err != nil {
		return ActionCompletedTerminal, fmt.Errorf("closing bead: %w", err)
	}

	return ActionCompletedTerminal, nil
}

// --- Path 3: state=waiting_manual ---

func (r *Reconciler) reconcileWaitingManual(beadID string, meta map[string]string) (ReconcileAction, error) {
	terminalReason := meta[FieldTerminalReason]
	waitingReason := meta[FieldWaitingReason]

	// Sub-path A: terminal_reason set — a stop was requested but the
	// terminal transition didn't complete.
	if terminalReason != "" {
		return r.completeTerminalTransition(beadID, meta)
	}

	// Sub-path B: waiting_reason set, no terminal_reason — genuine hold.
	if waitingReason != "" {
		// Re-emit ConvergenceWaitingManual (TierRecoverable) so that
		// event consumers learn the bead is waiting even if the original
		// event was lost in a crash.
		iteration, _ := DecodeInt(meta[FieldIteration])
		wispID := meta[FieldLastProcessedWisp]
		// One child fetch feeds both the cumulative duration (best-effort:
		// zero on error) and the last_processed_wisp repair below.
		children, childErr := r.Handler.Store.Children(beadID)
		stats := childStats(children, beadID)
		wmPayload := WaitingManualPayload{
			Iteration:            iteration,
			WispID:               wispID,
			GateMode:             meta[FieldGateMode],
			Reason:               waitingReason,
			CumulativeDurationMs: stats.CumulativeDur.Milliseconds(),
		}
		r.emitRecoveryEvent(EventWaitingManual, EventIDWaitingManual(beadID, iteration), beadID, wmPayload)

		// Repair last_processed_wisp if needed: find the highest-iteration
		// closed wisp and ensure last_processed_wisp points to it.
		// S31 single-fetch: childErr/stats come from the Children() call above;
		// no redundant re-fetch. S33 flattened (action, error) return shape.
		if childErr != nil {
			return ActionNoAction, fmt.Errorf("listing children: %w", childErr)
		}
		if stats.HighestClosedFound && meta[FieldLastProcessedWisp] != stats.HighestClosed.ID {
			if err := r.Handler.Store.SetMetadata(beadID, FieldLastProcessedWisp, stats.HighestClosed.ID); err != nil {
				return ActionRepairedState, fmt.Errorf("repairing last_processed_wisp: %w", err)
			}
			return ActionRepairedState, nil
		}
		return ActionNoAction, nil
	}

	// Sub-path C: no waiting_reason, no terminal_reason — orphaned state.
	// Check for any orphaned closed wisps that need processing. For now
	// just repair the waiting_reason so the loop is in a known state.
	children, err := r.Handler.Store.Children(beadID)
	if err != nil {
		return ActionNoAction, fmt.Errorf("listing children: %w", err)
	}
	if childStats(children, beadID).HighestClosedFound {
		// There are closed wisps but no waiting_reason — set a default.
		if err := r.Handler.Store.SetMetadata(beadID, FieldWaitingReason, WaitManual); err != nil {
			return ActionRepairedState, fmt.Errorf("setting default waiting_reason: %w", err)
		}
		return ActionRepairedState, nil
	}

	return ActionNoAction, nil
}

// --- Path 3t: state=waiting_trigger ---

func (r *Reconciler) reconcileWaitingTrigger(beadID string, meta map[string]string) (ReconcileAction, error) {
	// A stop requested while waiting on the trigger may have crashed before
	// the terminal transition completed.
	if meta[FieldTerminalReason] != "" {
		return r.completeTerminalTransition(beadID, meta)
	}
	// Otherwise nothing to repair: no wisp is in flight and the controller
	// tick re-evaluates the trigger condition.
	return ActionNoAction, nil
}

// --- Path 4: state=active ---

func (r *Reconciler) reconcileActive(ctx context.Context, beadID string, meta map[string]string) (ReconcileAction, error) {
	// Sub-path A: terminal_reason set — a stop was requested while active
	// but the transition crashed before completing.
	if meta[FieldTerminalReason] != "" {
		return r.completeTerminalTransition(beadID, meta)
	}

	// Sub-path B: Check active_wisp status.
	activeWispID := meta[FieldActiveWisp]
	recoveredActiveWisp := false

	if activeWispID != "" {
		wispInfo, err := r.Handler.Store.GetBead(activeWispID)
		if err != nil {
			if !errors.Is(err, beads.ErrNotFound) {
				return ActionNoAction, fmt.Errorf("reading active wisp %q: %w", activeWispID, err)
			}
			recoveredWisp, found, recoverErr := r.Handler.recoverCurrentActiveWisp(beadID, meta[FieldLastProcessedWisp])
			if recoverErr != nil {
				return ActionNoAction, recoverErr
			}
			if !found {
				// A crashed loop can leave active_wisp pointing at a bead that
				// was later cleaned up. Treat that as stale recovery state and
				// rebuild the chain from surviving children below.
				activeWispID = ""
			} else {
				activeWispID = recoveredWisp.ID
				wispInfo = recoveredWisp
				recoveredActiveWisp = true
			}
		}
		if activeWispID != "" {
			if recoveredActiveWisp {
				if err := r.Handler.Store.SetMetadata(beadID, FieldActiveWisp, activeWispID); err != nil {
					return ActionRepairedState, fmt.Errorf("setting recovered active wisp %q: %w", activeWispID, err)
				}
			}
			switch wispInfo.Status {
			case "open", "in_progress":
				// Wisp still running — nothing to do.
				if recoveredActiveWisp {
					return ActionRepairedState, nil
				}
				return ActionNoAction, nil

			case "closed":
				// Wisp is closed. Check if it was already processed.
				lastProcessed := meta[FieldLastProcessedWisp]
				if lastProcessed == activeWispID {
					// Already processed — check if the commit completed.
					// The commit was done because last_processed_wisp is
					// set (it is always the last write). Nothing to do.
					return ActionNoAction, nil
				}

				// Closed but not processed — replay the wisp_closed event.
				if _, err := r.Handler.HandleWispClosed(ctx, beadID, activeWispID); err != nil {
					return ActionRepairedState, fmt.Errorf("replaying wisp_closed for %q: %w", activeWispID, err)
				}
				return ActionRepairedState, nil

			default:
				return ActionNoAction, fmt.Errorf("active wisp %q has unexpected status %q", activeWispID, wispInfo.Status)
			}
		}
	}

	// active_wisp is empty — derive iteration from children and pour or
	// adopt the next wisp.
	children, err := r.Handler.Store.Children(beadID)
	if err != nil {
		return ActionNoAction, fmt.Errorf("listing children: %w", err)
	}

	closedIter := childStats(children, beadID).ClosedCount
	nextIter := closedIter + 1
	nextKey := IdempotencyKey(beadID, nextIter)

	var wispID string
	action := ActionAdoptedWisp

	if pendingID := r.Handler.validPendingNextWisp(beadID, nextKey, meta[FieldPendingNextWisp]); pendingID != "" {
		wispID = pendingID
	} else {
		// Check if a wisp for the next iteration already exists.
		existingID, found, err := r.Handler.Store.FindByIdempotencyKey(nextKey)
		if err != nil {
			return ActionNoAction, fmt.Errorf("looking up next wisp: %w", err)
		}

		if found {
			wispID = existingID
		} else {
			// Pour the next wisp.
			formula := meta[FieldFormula]
			vars := ExtractVars(meta)
			evaluatePrompt := meta[FieldEvaluatePrompt]

			wispID, err = r.Handler.Store.PourWisp(beadID, formula, nextKey, vars, evaluatePrompt)
			if err != nil {
				return ActionPouredWisp, fmt.Errorf("pouring wisp for iter %d: %w", nextIter, err)
			}
			action = ActionPouredWisp
		}
	}

	if err := r.Handler.Store.ActivateWisp(wispID); err != nil {
		return action, fmt.Errorf("activating wisp %q: %w", wispID, err)
	}

	if err := r.Handler.Store.SetMetadata(beadID, FieldActiveWisp, wispID); err != nil {
		return action, fmt.Errorf("setting active_wisp: %w", err)
	}
	_ = r.Handler.Store.SetMetadata(beadID, FieldPendingNextWisp, "")

	return action, nil
}

// --- Shared helpers ---

// completeTerminalTransition finishes a terminal transition that was
// interrupted.  Used by both Path 3A and Path 4A.
func (r *Reconciler) completeTerminalTransition(beadID string, meta map[string]string) (ReconcileAction, error) {
	// Backfill terminal_actor if missing.
	if err := r.backfillTerminalActor(beadID, meta); err != nil {
		return ActionCompletedTerminal, fmt.Errorf("backfilling terminal_actor: %w", err)
	}

	reason := meta[FieldTerminalReason]
	actor := meta[FieldTerminalActor]
	if actor == "" {
		actor = "recovery"
	}

	// One child fetch feeds the iteration count, cumulative duration, and the
	// last_processed_wisp repair below. The intervening writes only touch the
	// parent bead's metadata/status, not its children, so the highest closed
	// wisp is unchanged by the time we write it (best-effort: zeros on error).
	children, _ := r.Handler.Store.Children(beadID)
	stats := childStats(children, beadID)

	termPayload := TerminatedPayload{
		TerminalReason:       reason,
		TotalIterations:      stats.ClosedCount,
		FinalStatus:          "closed",
		Actor:                actor,
		CumulativeDurationMs: stats.CumulativeDur.Milliseconds(),
	}
	r.emitRecoveryEvent(EventTerminated, EventIDTerminated(beadID), beadID, termPayload)

	// Write state=terminated if not already set.
	if meta[FieldState] != StateTerminated {
		if err := r.Handler.Store.SetMetadata(beadID, FieldState, StateTerminated); err != nil {
			return ActionCompletedTerminal, fmt.Errorf("setting state to terminated: %w", err)
		}
	}

	// Close the bead.
	if err := r.Handler.Store.CloseBead(beadID, CloseReasonReconcileDone); err != nil {
		return ActionCompletedTerminal, fmt.Errorf("closing bead: %w", err)
	}

	// Write last_processed_wisp if there is a highest closed wisp
	// (write ordering: always last).
	if stats.HighestClosedFound {
		_ = r.Handler.Store.SetMetadata(beadID, FieldLastProcessedWisp, stats.HighestClosed.ID)
	}

	return ActionCompletedTerminal, nil
}

// backfillTerminalActor sets terminal_actor to "recovery" if it is
// missing from the metadata.
func (r *Reconciler) backfillTerminalActor(beadID string, meta map[string]string) error {
	if meta[FieldTerminalActor] != "" {
		return nil
	}
	return r.Handler.Store.SetMetadata(beadID, FieldTerminalActor, "recovery")
}

// deriveIterationFromChildren counts closed convergence wisps among the
// children of beadID. Thin accessor over childStats for the closed-wisp count.
func deriveIterationFromChildren(children []BeadInfo, beadID string) int {
	return childStats(children, beadID).ClosedCount
}

// highestClosedWisp finds the closed convergence wisp with the highest
// iteration number among the children of beadID. Thin accessor over childStats.
func highestClosedWisp(children []BeadInfo, beadID string) (BeadInfo, int, bool) {
	s := childStats(children, beadID)
	return s.HighestClosed, s.HighestClosedIter, s.HighestClosedFound
}

// emitRecoveryEvent emits a convergence event with the recovery flag
// set to true, signaling to downstream consumers that this event was
// generated during startup reconciliation rather than normal operation.
func (r *Reconciler) emitRecoveryEvent(eventType, eventID, beadID string, payload any) {
	if r.Handler.Emitter == nil {
		return
	}
	r.Handler.Emitter.Emit(eventType, eventID, beadID, MarshalPayload(r.Handler.withEventRig(beadID, payload)), true)
}
