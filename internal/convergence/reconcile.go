package convergence

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
)

// ReconcileDetail records the outcome of reconciling a single bead.
type ReconcileDetail struct {
	BeadID string
	Action string // "completed_terminal", "adopted_wisp", "poured_wisp", "repaired_state", "no_action"
	Error  error  // nil if successful
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
		} else if detail.Action != "no_action" {
			report.Recovered++
		}
	}

	return report, nil
}

// reconcileBead inspects a single convergence bead and performs whatever
// recovery action is needed.  It never returns an error directly —
// errors are captured in the returned ReconcileDetail.
func (r *Reconciler) reconcileBead(ctx context.Context, beadID string) ReconcileDetail {
	meta, err := r.Handler.Store.GetMetadata(beadID)
	if err != nil {
		return ReconcileDetail{BeadID: beadID, Action: "no_action", Error: fmt.Errorf("reading metadata: %w", err)}
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

	case StateActive:
		// Path 4: state=active.
		return r.reconcileActive(ctx, beadID, meta)

	default:
		return ReconcileDetail{
			BeadID: beadID, Action: "no_action",
			Error: fmt.Errorf("unknown convergence state %q", state),
		}
	}
}

// --- Path 1: Missing/empty state ---

func (r *Reconciler) reconcileMissingState(ctx context.Context, beadID string, meta map[string]string) ReconcileDetail {
	// Check if there is already a wisp for iteration 1 (idempotency key
	// lookup).
	key1 := IdempotencyKey(beadID, 1)
	existingID, found, err := r.Handler.Store.FindByIdempotencyKey(key1)
	if err != nil {
		return ReconcileDetail{
			BeadID: beadID, Action: "no_action",
			Error: fmt.Errorf("looking up iter-1 wisp: %w", err),
		}
	}

	if found {
		// Wisp exists — adopt it, but check if it's already closed.
		wispInfo, err := r.Handler.Store.GetBead(existingID)
		if err != nil {
			return ReconcileDetail{
				BeadID: beadID, Action: "adopted_wisp",
				Error: fmt.Errorf("reading wisp %q info: %w", existingID, err),
			}
		}

		if err := r.Handler.Store.SetMetadata(beadID, FieldActiveWisp, existingID); err != nil {
			return ReconcileDetail{
				BeadID: beadID, Action: "adopted_wisp",
				Error: fmt.Errorf("setting active_wisp: %w", err),
			}
		}
		// Set iteration to match the adopted wisp: 1 if closed (we know
		// iteration 1 exists), 0 if still open (HandleWispClosed will
		// derive the correct count when it fires).
		adoptedIteration := 0
		if wispInfo.Status == "closed" {
			adoptedIteration = 1
		}
		if err := r.Handler.Store.SetMetadata(beadID, FieldIteration, EncodeInt(adoptedIteration)); err != nil {
			return ReconcileDetail{
				BeadID: beadID, Action: "adopted_wisp",
				Error: fmt.Errorf("setting iteration: %w", err),
			}
		}
		if err := r.Handler.Store.SetMetadata(beadID, FieldState, StateActive); err != nil {
			return ReconcileDetail{
				BeadID: beadID, Action: "adopted_wisp",
				Error: fmt.Errorf("setting state: %w", err),
			}
		}

		// If the adopted wisp is already closed, replay the transition
		// so the convergence loop doesn't stall in active with a dead wisp.
		if wispInfo.Status == "closed" {
			if _, err := r.Handler.HandleWispClosed(ctx, beadID, existingID); err != nil {
				return ReconcileDetail{
					BeadID: beadID, Action: "adopted_wisp",
					Error: fmt.Errorf("replaying wisp_closed for adopted wisp %q: %w", existingID, err),
				}
			}
		}

		return ReconcileDetail{BeadID: beadID, Action: "adopted_wisp"}
	}

	// No wisp exists — pour the first one.
	formula := meta[FieldFormula]
	vars := ExtractVars(meta)
	evaluatePrompt := meta[FieldEvaluatePrompt]

	wispID, err := r.Handler.Store.PourWisp(beadID, formula, key1, vars, evaluatePrompt)
	if err != nil {
		return ReconcileDetail{
			BeadID: beadID, Action: "poured_wisp",
			Error: fmt.Errorf("pouring first wisp: %w", err),
		}
	}

	if err := r.Handler.Store.SetMetadata(beadID, FieldActiveWisp, wispID); err != nil {
		return ReconcileDetail{
			BeadID: beadID, Action: "poured_wisp",
			Error: fmt.Errorf("setting active_wisp: %w", err),
		}
	}
	if err := r.Handler.Store.SetMetadata(beadID, FieldIteration, EncodeInt(0)); err != nil {
		return ReconcileDetail{
			BeadID: beadID, Action: "poured_wisp",
			Error: fmt.Errorf("setting iteration: %w", err),
		}
	}
	if err := r.Handler.Store.SetMetadata(beadID, FieldState, StateActive); err != nil {
		return ReconcileDetail{
			BeadID: beadID, Action: "poured_wisp",
			Error: fmt.Errorf("setting state: %w", err),
		}
	}

	return ReconcileDetail{BeadID: beadID, Action: "poured_wisp"}
}

// --- Path 1b: state=creating (partial creation) ---

func (r *Reconciler) reconcileCreating(beadID string) ReconcileDetail {
	if err := r.Handler.Store.SetMetadata(beadID, FieldTerminalReason, TerminalPartialCreation); err != nil {
		return ReconcileDetail{
			BeadID: beadID, Action: "completed_terminal",
			Error: fmt.Errorf("setting terminal_reason: %w", err),
		}
	}
	if err := r.Handler.Store.SetMetadata(beadID, FieldTerminalActor, "recovery"); err != nil {
		return ReconcileDetail{
			BeadID: beadID, Action: "completed_terminal",
			Error: fmt.Errorf("setting terminal_actor: %w", err),
		}
	}
	if err := r.Handler.Store.SetMetadata(beadID, FieldState, StateTerminated); err != nil {
		return ReconcileDetail{
			BeadID: beadID, Action: "completed_terminal",
			Error: fmt.Errorf("setting state to terminated: %w", err),
		}
	}
	if err := r.Handler.Store.CloseBead(beadID, CloseReasonReconcileDone); err != nil {
		return ReconcileDetail{
			BeadID: beadID, Action: "completed_terminal",
			Error: fmt.Errorf("closing bead: %w", err),
		}
	}
	return ReconcileDetail{BeadID: beadID, Action: "completed_terminal"}
}

// --- Path 2: state=terminated but bead not closed ---

func (r *Reconciler) reconcileTerminatedNotClosed(beadID string, meta map[string]string) ReconcileDetail {
	// Check if the bead is actually already closed.
	beadInfo, err := r.Handler.Store.GetBead(beadID)
	if err != nil {
		return ReconcileDetail{
			BeadID: beadID, Action: "no_action",
			Error: fmt.Errorf("reading bead info: %w", err),
		}
	}
	if beadInfo.Status == "closed" {
		// Already fully terminated.
		return ReconcileDetail{BeadID: beadID, Action: "no_action"}
	}

	// Backfill terminal_actor if missing.
	if err := r.backfillTerminalActor(beadID, meta); err != nil {
		return ReconcileDetail{
			BeadID: beadID, Action: "completed_terminal",
			Error: fmt.Errorf("backfilling terminal_actor: %w", err),
		}
	}

	// Derive total iterations for the terminated event.
	totalIterations, _ := r.deriveIterationFromChildrenViaStore(beadID)

	// Emit ConvergenceTerminated (recovery).
	reason := meta[FieldTerminalReason]
	if reason == "" {
		reason = TerminalNoConvergence // safe default
	}
	actor := meta[FieldTerminalActor]
	if actor == "" {
		actor = "recovery"
	}

	// Compute cumulative duration (best-effort).
	cumDur := r.cumulativeDuration(beadID)

	termPayload := TerminatedPayload{
		TerminalReason:       reason,
		TotalIterations:      totalIterations,
		FinalStatus:          "closed",
		Actor:                actor,
		CumulativeDurationMs: cumDur,
	}
	r.emitRecoveryEvent(EventTerminated, EventIDTerminated(beadID), beadID, termPayload)

	// Close the bead.
	if err := r.Handler.Store.CloseBead(beadID, CloseReasonReconcileDone); err != nil {
		return ReconcileDetail{
			BeadID: beadID, Action: "completed_terminal",
			Error: fmt.Errorf("closing bead: %w", err),
		}
	}

	return ReconcileDetail{BeadID: beadID, Action: "completed_terminal"}
}

// --- Path 3: state=waiting_manual ---

func (r *Reconciler) reconcileWaitingManual(beadID string, meta map[string]string) ReconcileDetail {
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
		cumDur := r.cumulativeDuration(beadID)
		wmPayload := WaitingManualPayload{
			Iteration:            iteration,
			WispID:               wispID,
			GateMode:             meta[FieldGateMode],
			Reason:               waitingReason,
			CumulativeDurationMs: cumDur,
		}
		r.emitRecoveryEvent(EventWaitingManual, EventIDWaitingManual(beadID, iteration), beadID, wmPayload)

		// Repair last_processed_wisp if needed: find the highest-iteration
		// closed wisp and ensure last_processed_wisp points to it.
		children, err := r.Handler.Store.Children(beadID)
		if err != nil {
			return ReconcileDetail{
				BeadID: beadID, Action: "no_action",
				Error: fmt.Errorf("listing children: %w", err),
			}
		}
		highestWisp, _, found := highestClosedWisp(children, beadID)
		if found && meta[FieldLastProcessedWisp] != highestWisp.ID {
			if err := r.Handler.Store.SetMetadata(beadID, FieldLastProcessedWisp, highestWisp.ID); err != nil {
				return ReconcileDetail{
					BeadID: beadID, Action: "repaired_state",
					Error: fmt.Errorf("repairing last_processed_wisp: %w", err),
				}
			}
			return ReconcileDetail{BeadID: beadID, Action: "repaired_state"}
		}
		return ReconcileDetail{BeadID: beadID, Action: "no_action"}
	}

	// Sub-path C: no waiting_reason, no terminal_reason — orphaned state.
	// Check for any orphaned closed wisps that need processing. For now
	// just repair the waiting_reason so the loop is in a known state.
	children, err := r.Handler.Store.Children(beadID)
	if err != nil {
		return ReconcileDetail{
			BeadID: beadID, Action: "no_action",
			Error: fmt.Errorf("listing children: %w", err),
		}
	}
	_, _, found := highestClosedWisp(children, beadID)
	if found {
		// There are closed wisps but no waiting_reason — set a default.
		if err := r.Handler.Store.SetMetadata(beadID, FieldWaitingReason, WaitManual); err != nil {
			return ReconcileDetail{
				BeadID: beadID, Action: "repaired_state",
				Error: fmt.Errorf("setting default waiting_reason: %w", err),
			}
		}
		return ReconcileDetail{BeadID: beadID, Action: "repaired_state"}
	}

	return ReconcileDetail{BeadID: beadID, Action: "no_action"}
}

// --- Path 4: state=active ---

func (r *Reconciler) reconcileActive(ctx context.Context, beadID string, meta map[string]string) ReconcileDetail {
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
				return ReconcileDetail{
					BeadID: beadID, Action: "no_action",
					Error: fmt.Errorf("reading active wisp %q: %w", activeWispID, err),
				}
			}
			recoveredWisp, found, recoverErr := r.Handler.recoverCurrentActiveWisp(beadID, meta[FieldLastProcessedWisp])
			if recoverErr != nil {
				return ReconcileDetail{
					BeadID: beadID, Action: "no_action",
					Error: recoverErr,
				}
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
					return ReconcileDetail{
						BeadID: beadID, Action: "repaired_state",
						Error: fmt.Errorf("setting recovered active wisp %q: %w", activeWispID, err),
					}
				}
			}
			switch wispInfo.Status {
			case "open", "in_progress":
				// Wisp still running — nothing to do.
				action := "no_action"
				if recoveredActiveWisp {
					action = "repaired_state"
				}
				return ReconcileDetail{BeadID: beadID, Action: action}

			case "closed":
				// Wisp is closed. Check if it was already processed.
				lastProcessed := meta[FieldLastProcessedWisp]
				if lastProcessed == activeWispID {
					// Already processed — check if the commit completed.
					// The commit was done because last_processed_wisp is
					// set (it is always the last write). Nothing to do.
					return ReconcileDetail{BeadID: beadID, Action: "no_action"}
				}

				// Closed but not processed — replay the wisp_closed event.
				result, err := r.Handler.HandleWispClosed(ctx, beadID, activeWispID)
				if err != nil {
					return ReconcileDetail{
						BeadID: beadID, Action: "repaired_state",
						Error: fmt.Errorf("replaying wisp_closed for %q: %w", activeWispID, err),
					}
				}
				_ = result
				return ReconcileDetail{BeadID: beadID, Action: "repaired_state"}

			default:
				return ReconcileDetail{
					BeadID: beadID, Action: "no_action",
					Error: fmt.Errorf("active wisp %q has unexpected status %q", activeWispID, wispInfo.Status),
				}
			}
		}
	}

	// active_wisp is empty — derive iteration from children and pour or
	// adopt the next wisp.
	children, err := r.Handler.Store.Children(beadID)
	if err != nil {
		return ReconcileDetail{
			BeadID: beadID, Action: "no_action",
			Error: fmt.Errorf("listing children: %w", err),
		}
	}

	closedIter := deriveIterationFromChildren(children, beadID)
	nextIter := closedIter + 1
	nextKey := IdempotencyKey(beadID, nextIter)

	var wispID string
	action := "adopted_wisp"

	if pendingID := r.Handler.validPendingNextWisp(beadID, nextKey, meta[FieldPendingNextWisp]); pendingID != "" {
		wispID = pendingID
	} else {
		// Check if a wisp for the next iteration already exists.
		existingID, found, err := r.Handler.Store.FindByIdempotencyKey(nextKey)
		if err != nil {
			return ReconcileDetail{
				BeadID: beadID, Action: "no_action",
				Error: fmt.Errorf("looking up next wisp: %w", err),
			}
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
				return ReconcileDetail{
					BeadID: beadID, Action: "poured_wisp",
					Error: fmt.Errorf("pouring wisp for iter %d: %w", nextIter, err),
				}
			}
			action = "poured_wisp"
		}
	}

	if err := r.Handler.Store.ActivateWisp(wispID); err != nil {
		return ReconcileDetail{
			BeadID: beadID, Action: action,
			Error: fmt.Errorf("activating wisp %q: %w", wispID, err),
		}
	}

	if err := r.Handler.Store.SetMetadata(beadID, FieldActiveWisp, wispID); err != nil {
		return ReconcileDetail{
			BeadID: beadID, Action: action,
			Error: fmt.Errorf("setting active_wisp: %w", err),
		}
	}
	_ = r.Handler.Store.SetMetadata(beadID, FieldPendingNextWisp, "")

	return ReconcileDetail{BeadID: beadID, Action: action}
}

// --- Shared helpers ---

// completeTerminalTransition finishes a terminal transition that was
// interrupted.  Used by both Path 3A and Path 4A.
func (r *Reconciler) completeTerminalTransition(beadID string, meta map[string]string) ReconcileDetail {
	// Backfill terminal_actor if missing.
	if err := r.backfillTerminalActor(beadID, meta); err != nil {
		return ReconcileDetail{
			BeadID: beadID, Action: "completed_terminal",
			Error: fmt.Errorf("backfilling terminal_actor: %w", err),
		}
	}

	reason := meta[FieldTerminalReason]
	actor := meta[FieldTerminalActor]
	if actor == "" {
		actor = "recovery"
	}

	totalIterations, _ := r.deriveIterationFromChildrenViaStore(beadID)
	cumDur := r.cumulativeDuration(beadID)

	termPayload := TerminatedPayload{
		TerminalReason:       reason,
		TotalIterations:      totalIterations,
		FinalStatus:          "closed",
		Actor:                actor,
		CumulativeDurationMs: cumDur,
	}
	r.emitRecoveryEvent(EventTerminated, EventIDTerminated(beadID), beadID, termPayload)

	// Write state=terminated if not already set.
	if meta[FieldState] != StateTerminated {
		if err := r.Handler.Store.SetMetadata(beadID, FieldState, StateTerminated); err != nil {
			return ReconcileDetail{
				BeadID: beadID, Action: "completed_terminal",
				Error: fmt.Errorf("setting state to terminated: %w", err),
			}
		}
	}

	// Close the bead.
	if err := r.Handler.Store.CloseBead(beadID, CloseReasonReconcileDone); err != nil {
		return ReconcileDetail{
			BeadID: beadID, Action: "completed_terminal",
			Error: fmt.Errorf("closing bead: %w", err),
		}
	}

	// Write last_processed_wisp if there is a highest closed wisp
	// (write ordering: always last).
	children, err := r.Handler.Store.Children(beadID)
	if err == nil {
		if hw, _, found := highestClosedWisp(children, beadID); found {
			_ = r.Handler.Store.SetMetadata(beadID, FieldLastProcessedWisp, hw.ID)
		}
	}

	return ReconcileDetail{BeadID: beadID, Action: "completed_terminal"}
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
// children of beadID. This is the same logic as Handler.deriveIterationCount
// but operates on a pre-fetched child list.
func deriveIterationFromChildren(children []BeadInfo, beadID string) int {
	prefix := IdempotencyKeyPrefix(beadID)
	count := 0
	for _, child := range children {
		if strings.HasPrefix(child.IdempotencyKey, prefix) && child.Status == "closed" {
			count++
		}
	}
	return count
}

// highestClosedWisp finds the closed convergence wisp with the highest
// iteration number among the children of beadID.
func highestClosedWisp(children []BeadInfo, beadID string) (BeadInfo, int, bool) {
	prefix := IdempotencyKeyPrefix(beadID)
	var best BeadInfo
	bestIter := -1
	found := false

	for _, child := range children {
		if !strings.HasPrefix(child.IdempotencyKey, prefix) {
			continue
		}
		if child.Status != "closed" {
			continue
		}
		iter, ok := ParseIterationFromKey(child.IdempotencyKey)
		if !ok {
			continue
		}
		if iter > bestIter {
			best = child
			bestIter = iter
			found = true
		}
	}

	return best, bestIter, found
}

// deriveIterationFromChildrenViaStore fetches children from the store
// and delegates to deriveIterationFromChildren.
func (r *Reconciler) deriveIterationFromChildrenViaStore(beadID string) (int, error) {
	children, err := r.Handler.Store.Children(beadID)
	if err != nil {
		return 0, err
	}
	return deriveIterationFromChildren(children, beadID), nil
}

// cumulativeDuration computes the cumulative duration across all closed
// convergence wisps (best-effort, returns 0 on error).
func (r *Reconciler) cumulativeDuration(beadID string) int64 {
	children, err := r.Handler.Store.Children(beadID)
	if err != nil {
		return 0
	}
	prefix := IdempotencyKeyPrefix(beadID)
	var total int64
	for _, child := range children {
		if strings.HasPrefix(child.IdempotencyKey, prefix) && child.Status == "closed" &&
			!child.ClosedAt.IsZero() && !child.CreatedAt.IsZero() {
			total += child.ClosedAt.Sub(child.CreatedAt).Milliseconds()
		}
	}
	return total
}

// emitRecoveryEvent emits a convergence event with the recovery flag
// set to true, signaling to downstream consumers that this event was
// generated during startup reconciliation rather than normal operation.
func (r *Reconciler) emitRecoveryEvent(eventType, eventID, beadID string, payload any) {
	if r.Handler.Emitter == nil {
		return
	}
	r.Handler.Emitter.Emit(eventType, eventID, beadID, MarshalPayload(payload), true)
}
