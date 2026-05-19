package convergence

import (
	"context"
	"fmt"
)

// CreateParams holds the parameters for creating a new convergence loop.
type CreateParams struct {
	Formula           string
	Target            string
	MaxIterations     int
	GateMode          string
	GateCondition     string
	GateTimeout       string
	GateTimeoutAction string
	Title             string
	Vars              map[string]string
	CityPath          string
	EvaluatePrompt    string
}

// CreateResult holds the outcome of creating a convergence loop.
type CreateResult struct {
	BeadID      string
	FirstWispID string
}

// CreateHandler creates a new convergence loop: root bead, metadata, first
// wisp, and ConvergenceCreated event.
//
// Callers are responsible for concurrency/deadlock checks
// (CheckConcurrencyLimits, CheckNestedConvergence) BEFORE calling this.
func (h *Handler) CreateHandler(_ context.Context, params CreateParams) (CreateResult, error) {
	if params.Formula == "" {
		return CreateResult{}, fmt.Errorf("formula is required")
	}
	if params.Target == "" {
		return CreateResult{}, fmt.Errorf("target is required")
	}
	if params.MaxIterations <= 0 {
		return CreateResult{}, fmt.Errorf("max_iterations must be positive")
	}
	if params.GateMode == "" {
		params.GateMode = GateModeManual
	}

	// Validate gate config before creating any state.
	gateMeta := map[string]string{
		FieldGateMode:          params.GateMode,
		FieldGateCondition:     params.GateCondition,
		FieldGateTimeout:       params.GateTimeout,
		FieldGateTimeoutAction: params.GateTimeoutAction,
	}
	if _, err := ParseGateConfig(gateMeta); err != nil {
		return CreateResult{}, err
	}

	// Step 1: Create root bead (type=convergence, status=in_progress).
	title := params.Title
	if title == "" {
		title = "Convergence: " + params.Formula
	}
	beadID, err := h.Store.CreateConvergenceBead(title)
	if err != nil {
		return CreateResult{}, fmt.Errorf("creating convergence bead: %w", err)
	}

	// closeBead terminates the root bead on partial-create failure so the
	// reconciler does not try to resume an incomplete convergence loop.
	closeBead := func(cause error) error {
		_ = h.Store.SetMetadata(beadID, FieldState, StateTerminated)
		_ = h.Store.CloseBead(beadID)
		return cause
	}

	// Mark as creating so the reconciler can detect partial creation.
	if err := h.Store.SetMetadata(beadID, FieldState, StateCreating); err != nil {
		return CreateResult{}, closeBead(fmt.Errorf("setting creating state: %w", err))
	}

	// Step 2: Set all metadata fields.
	metaWrites := []struct{ key, value string }{
		{FieldFormula, params.Formula},
		{FieldTarget, params.Target},
		{FieldMaxIterations, EncodeInt(params.MaxIterations)},
		{FieldGateMode, params.GateMode},
		{FieldGateCondition, params.GateCondition},
		{FieldGateTimeout, params.GateTimeout},
		{FieldGateTimeoutAction, params.GateTimeoutAction},
		{FieldCityPath, params.CityPath},
		{FieldEvaluatePrompt, params.EvaluatePrompt},
		{FieldState, StateActive},
	}
	for _, mw := range metaWrites {
		if err := h.Store.SetMetadata(beadID, mw.key, mw.value); err != nil {
			return CreateResult{}, closeBead(fmt.Errorf("setting %s on convergence bead: %w", mw.key, err))
		}
	}

	// Step 3: Set template variables.
	for k, v := range params.Vars {
		if err := h.Store.SetMetadata(beadID, VarPrefix+k, v); err != nil {
			return CreateResult{}, closeBead(fmt.Errorf("setting var %q on convergence bead: %w", k, err))
		}
	}

	// Step 4: Pour first wisp with idempotency key converge:<bead-id>:iter:1.
	firstKey := IdempotencyKey(beadID, 1)
	firstWispID, err := h.Store.PourWisp(beadID, params.Formula, firstKey, params.Vars, params.EvaluatePrompt)
	if err != nil {
		return CreateResult{}, closeBead(fmt.Errorf("pouring first wisp: %w", err))
	}

	// Step 5: Set active_wisp and iteration counter.
	if err := h.Store.SetMetadata(beadID, FieldActiveWisp, firstWispID); err != nil {
		return CreateResult{}, closeBead(fmt.Errorf("setting active wisp: %w", err))
	}
	if err := h.Store.SetMetadata(beadID, FieldIteration, EncodeInt(1)); err != nil {
		return CreateResult{}, closeBead(fmt.Errorf("setting iteration: %w", err))
	}

	// Step 6: Emit ConvergenceCreated event.
	createdPayload := CreatedPayload{
		Formula:       params.Formula,
		Target:        params.Target,
		GateMode:      params.GateMode,
		MaxIterations: params.MaxIterations,
		Title:         title,
		FirstWispID:   firstWispID,
	}
	h.emitEvent(EventCreated, EventIDCreated(beadID), beadID, createdPayload)

	return CreateResult{
		BeadID:      beadID,
		FirstWispID: firstWispID,
	}, nil
}
