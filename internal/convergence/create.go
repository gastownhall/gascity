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
	// Trigger gates iteration pours on an external condition. Empty means no
	// trigger (default wisp-close iteration semantic).
	Trigger string
	// TriggerCondition is the path to the trigger condition script. Required
	// when Trigger == "event".
	TriggerCondition string
	// Rig names the rig whose bead store owns this convergence loop.
	// Empty means the city/HQ store. The loop physically lives in
	// whichever store the handler is bound to; Rig is persisted as
	// metadata so status/list and audit can report the owning scope.
	Rig string
	// RetrySource, when non-empty, marks this loop as a retry of a
	// terminated source loop. It changes the partial-create rollback close
	// reason and stamps FieldRetrySource metadata plus the retry_source
	// event payload. Empty means a fresh (non-retry) create.
	RetrySource string
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

	// Validate trigger config before creating any state.
	triggerConfig, err := ParseTriggerConfig(map[string]string{
		FieldTrigger:          params.Trigger,
		FieldTriggerCondition: params.TriggerCondition,
	})
	if err != nil {
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
	closeReason := CloseReasonCreateRollback
	if params.RetrySource != "" {
		closeReason = CloseReasonRetryRollback
	}
	closeBead := func(cause error) error {
		_ = h.Store.SetMetadata(beadID, FieldState, StateTerminated)
		_ = h.Store.CloseBead(beadID, closeReason)
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
		{FieldRig, params.Rig},
		{FieldEvaluatePrompt, params.EvaluatePrompt},
		{FieldTrigger, params.Trigger},
		{FieldTriggerCondition, params.TriggerCondition},
	}
	if params.RetrySource != "" {
		metaWrites = append(metaWrites, struct{ key, value string }{FieldRetrySource, params.RetrySource})
	}
	for _, mw := range metaWrites {
		if err := h.Store.SetMetadata(beadID, mw.key, mw.value); err != nil {
			return CreateResult{}, closeBead(fmt.Errorf("setting %s on convergence bead: %w", mw.key, err))
		}
	}

	// retrySource is stamped on the created event when this is a retry so
	// downstream observers can trace the lineage to the source loop.
	var retrySource *string
	if params.RetrySource != "" {
		retrySource = &params.RetrySource
	}

	// Step 3: Set template variables.
	for k, v := range params.Vars {
		if err := h.Store.SetMetadata(beadID, VarPrefix+k, v); err != nil {
			return CreateResult{}, closeBead(fmt.Errorf("setting var %q on convergence bead: %w", k, err))
		}
	}

	// Entry gate: when an external trigger gates the loop, defer pouring the
	// first wisp until the trigger condition passes. The controller tick
	// evaluates the trigger and pours iteration 1 via HandleTrigger.
	if triggerConfig.Enabled() {
		if err := h.Store.SetMetadata(beadID, FieldIteration, EncodeInt(0)); err != nil {
			return CreateResult{}, closeBead(fmt.Errorf("setting iteration: %w", err))
		}
		if err := h.Store.SetMetadata(beadID, FieldState, StateWaitingTrigger); err != nil {
			return CreateResult{}, closeBead(fmt.Errorf("setting waiting_trigger state: %w", err))
		}
		createdPayload := CreatedPayload{
			Formula:       params.Formula,
			Target:        params.Target,
			GateMode:      params.GateMode,
			MaxIterations: params.MaxIterations,
			Title:         title,
			RetrySource:   retrySource,
		}
		h.emitEvent(EventCreated, EventIDCreated(beadID), beadID, createdPayload)
		return CreateResult{BeadID: beadID}, nil
	}

	// No trigger: activate immediately and pour the first wisp.
	if err := h.Store.SetMetadata(beadID, FieldState, StateActive); err != nil {
		return CreateResult{}, closeBead(fmt.Errorf("setting active state: %w", err))
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
		RetrySource:   retrySource,
	}
	h.emitEvent(EventCreated, EventIDCreated(beadID), beadID, createdPayload)

	return CreateResult{
		BeadID:      beadID,
		FirstWispID: firstWispID,
	}, nil
}
