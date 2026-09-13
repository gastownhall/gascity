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
func (h *Handler) RetryHandler(ctx context.Context, sourceBeadID, _ string, maxIterations int) (RetryResult, error) {
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

	// Step 4: Validate gate config from source bead before creating state.
	// CreateHandler re-validates, but doing it here first preserves the
	// source-scoped error message and the "no bead created on invalid source"
	// guarantee that retry callers rely on.
	gateMeta := map[string]string{
		FieldGateMode:          meta[FieldGateMode],
		FieldGateCondition:     meta[FieldGateCondition],
		FieldGateTimeout:       meta[FieldGateTimeout],
		FieldGateTimeoutAction: meta[FieldGateTimeoutAction],
	}
	if _, err := ParseGateConfig(gateMeta); err != nil {
		return RetryResult{}, fmt.Errorf("source bead %q has invalid gate config: %w", sourceBeadID, err)
	}

	// Step 5: Map the source configuration onto CreateParams and delegate to
	// CreateHandler. This is the single create path: bead create, rollback,
	// StateCreating marker, metadata, first-wisp pour, and the created event
	// all live in CreateHandler. Trigger fields carry forward so a retried
	// trigger-gated loop keeps its entry gate (previously dropped here).
	result, err := h.CreateHandler(ctx, CreateParams{
		Formula:           meta[FieldFormula],
		Target:            meta[FieldTarget],
		MaxIterations:     maxIterations,
		GateMode:          meta[FieldGateMode],
		GateCondition:     meta[FieldGateCondition],
		GateTimeout:       meta[FieldGateTimeout],
		GateTimeoutAction: meta[FieldGateTimeoutAction],
		Title:             "Retry of " + sourceBeadID,
		Vars:              ExtractVars(meta),
		CityPath:          meta[FieldCityPath],
		EvaluatePrompt:    meta[FieldEvaluatePrompt],
		Trigger:           meta[FieldTrigger],
		TriggerCondition:  meta[FieldTriggerCondition],
		Rig:               meta[FieldRig],
		RetrySource:       sourceBeadID,
	})
	if err != nil {
		return RetryResult{}, err
	}

	return RetryResult{
		NewBeadID:   result.BeadID,
		FirstWispID: result.FirstWispID,
		Iteration:   1,
	}, nil
}
