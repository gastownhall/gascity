package convergence

import (
	"context"
	"fmt"
)

// RetryResult holds the outcome of RetryHandler.
type RetryResult struct {
	NewBeadID   string
	FirstWispID string
	Iteration   int // always 1
}

// RetryHandler creates a new convergence loop from a terminated one.
// It copies configuration (formula, gate settings, template variables)
// from the source bead and pours the first wisp of the new loop.
//
// The source bead must be in terminated state with a terminal_reason
// other than "approved" (approved loops cannot be retried).
func (h *Handler) RetryHandler(_ context.Context, sourceBeadID, _ string, maxIterations int) (RetryResult, error) {
	// Step 1: Read source bead metadata.
	meta, err := h.Store.GetMetadata(sourceBeadID)
	if err != nil {
		return RetryResult{}, fmt.Errorf("reading source bead %q metadata: %w", sourceBeadID, err)
	}

	// Step 2: Verify source is terminated.
	if meta[FieldState] != StateTerminated {
		return RetryResult{}, fmt.Errorf(
			"cannot retry bead %q: state is %q, expected %q",
			sourceBeadID, meta[FieldState], StateTerminated,
		)
	}

	// Step 3: Verify source was not approved.
	if meta[FieldTerminalReason] == TerminalApproved {
		return RetryResult{}, fmt.Errorf(
			"cannot retry bead %q: terminal_reason is %q (approved loops cannot be retried)",
			sourceBeadID, TerminalApproved,
		)
	}

	// Step 4: Read source configuration.
	formula := meta[FieldFormula]
	target := meta[FieldTarget]
	gateMode := meta[FieldGateMode]
	gateCondition := meta[FieldGateCondition]
	gateTimeout := meta[FieldGateTimeout]
	gateTimeoutAction := meta[FieldGateTimeoutAction]
	cityPath := meta[FieldCityPath]
	evaluatePrompt := meta[FieldEvaluatePrompt]
	vars := ExtractVars(meta)

	// Step 4b: Validate gate config from source bead before creating state.
	gateMeta := map[string]string{
		FieldGateMode:          gateMode,
		FieldGateCondition:     gateCondition,
		FieldGateTimeout:       gateTimeout,
		FieldGateTimeoutAction: gateTimeoutAction,
	}
	if _, err := ParseGateConfig(gateMeta); err != nil {
		return RetryResult{}, fmt.Errorf("source bead %q has invalid gate config: %w", sourceBeadID, err)
	}

	// Step 5: Create new root bead.
	title := "Retry of " + sourceBeadID
	newBeadID, err := h.Store.CreateConvergenceBead(title)
	if err != nil {
		return RetryResult{}, fmt.Errorf("creating convergence bead: %w", err)
	}

	// closeBead terminates the root bead on partial-create failure so the
	// reconciler does not try to resume an incomplete convergence loop.
	closeBead := func(cause error) error {
		_ = h.Store.SetMetadata(newBeadID, FieldState, StateTerminated)
		_ = h.Store.CloseBead(newBeadID)
		return cause
	}

	// Mark as creating so the reconciler can detect partial creation.
	if err := h.Store.SetMetadata(newBeadID, FieldState, StateCreating); err != nil {
		return RetryResult{}, closeBead(fmt.Errorf("setting creating state: %w", err))
	}

	// Step 6: Set metadata on new bead.
	metaWrites := []struct{ key, value string }{
		{FieldFormula, formula},
		{FieldTarget, target},
		{FieldGateMode, gateMode},
		{FieldGateCondition, gateCondition},
		{FieldGateTimeout, gateTimeout},
		{FieldGateTimeoutAction, gateTimeoutAction},
		{FieldMaxIterations, EncodeInt(maxIterations)},
		{FieldCityPath, cityPath},
		{FieldEvaluatePrompt, evaluatePrompt},
		{FieldRetrySource, sourceBeadID},
		{FieldState, StateActive},
	}
	for _, mw := range metaWrites {
		if err := h.Store.SetMetadata(newBeadID, mw.key, mw.value); err != nil {
			return RetryResult{}, closeBead(fmt.Errorf("setting %s on new bead: %w", mw.key, err))
		}
	}

	// Step 7: Copy template variables.
	for k, v := range vars {
		if err := h.Store.SetMetadata(newBeadID, VarPrefix+k, v); err != nil {
			return RetryResult{}, closeBead(fmt.Errorf("copying var %q to new bead: %w", k, err))
		}
	}

	// Step 8: Pour first wisp.
	firstKey := IdempotencyKey(newBeadID, 1)
	firstWispID, err := h.Store.PourWisp(newBeadID, formula, firstKey, vars, evaluatePrompt)
	if err != nil {
		return RetryResult{}, closeBead(fmt.Errorf("pouring first wisp for retry bead %q: %w", newBeadID, err))
	}

	// Step 9: Set active_wisp and iteration counter.
	if err := h.Store.SetMetadata(newBeadID, FieldActiveWisp, firstWispID); err != nil {
		return RetryResult{}, closeBead(fmt.Errorf("setting active wisp on new bead: %w", err))
	}
	if err := h.Store.SetMetadata(newBeadID, FieldIteration, EncodeInt(1)); err != nil {
		return RetryResult{}, closeBead(fmt.Errorf("setting iteration on new bead: %w", err))
	}

	// Step 10: Emit ConvergenceCreated event with retry_source.
	createdPayload := CreatedPayload{
		Formula:       formula,
		Target:        target,
		GateMode:      gateMode,
		MaxIterations: maxIterations,
		Title:         title,
		FirstWispID:   firstWispID,
		RetrySource:   &sourceBeadID,
	}
	h.emitEvent(EventCreated, EventIDCreated(newBeadID), newBeadID, createdPayload)

	return RetryResult{
		NewBeadID:   newBeadID,
		FirstWispID: firstWispID,
		Iteration:   1,
	}, nil
}
