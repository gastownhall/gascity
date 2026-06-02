package convergence

import (
	"context"
	"fmt"
)

// TriggerConfig holds the external-trigger configuration for a convergence
// loop. A trigger gates *when* iterations are poured: the entry (iteration 1)
// and every subsequent iteration wait until the trigger condition exits 0.
// This lets converge replace out-of-band sweepers that poll for an external
// precondition between passes.
type TriggerConfig struct {
	Mode      string // "" (none) or "event"
	Condition string // path to trigger condition script (required when Mode=="event")
}

// Enabled reports whether an external trigger gates this loop.
func (tc TriggerConfig) Enabled() bool {
	return tc.Mode == TriggerEvent
}

// ParseTriggerConfig extracts trigger configuration from convergence metadata.
// An empty trigger mode is valid and means "no trigger" (default wisp-close
// iteration semantic). When the mode is "event", a condition path is required.
func ParseTriggerConfig(meta map[string]string) (TriggerConfig, error) {
	mode := meta[FieldTrigger]
	condition := meta[FieldTriggerCondition]

	switch mode {
	case TriggerNone:
		return TriggerConfig{Mode: TriggerNone}, nil
	case TriggerEvent:
		if condition == "" {
			return TriggerConfig{}, fmt.Errorf("parsing trigger config: trigger mode %q requires a trigger condition path", TriggerEvent)
		}
		return TriggerConfig{Mode: TriggerEvent, Condition: condition}, nil
	default:
		return TriggerConfig{}, fmt.Errorf("parsing trigger config: invalid trigger mode %q", mode)
	}
}

// HandleTrigger evaluates the trigger condition for a loop sitting in the
// waiting_trigger state and, when the condition exits 0, advances the loop by
// pouring and activating the next iteration's wisp. A non-zero exit (or
// timeout/error) keeps the loop waiting; the controller re-evaluates on the
// next tick as beads change.
//
// This is the tick-side counterpart to HandleWispClosed. Like that handler it
// assumes single-writer-per-bead concurrency, which the controller event loop
// provides.
func (h *Handler) HandleTrigger(ctx context.Context, rootBeadID string) (HandlerResult, error) {
	meta, err := h.Store.GetMetadata(rootBeadID)
	if err != nil {
		return HandlerResult{}, fmt.Errorf("reading root bead metadata: %w", err)
	}

	// Guard: only act on loops genuinely waiting on a trigger.
	if meta[FieldState] != StateWaitingTrigger {
		return HandlerResult{Action: ActionSkipped}, nil
	}

	triggerConfig, err := ParseTriggerConfig(meta)
	if err != nil {
		return HandlerResult{}, fmt.Errorf("parsing trigger config: %w", err)
	}
	if !triggerConfig.Enabled() {
		return HandlerResult{}, fmt.Errorf("bead %q is in %s but has no trigger configured", rootBeadID, StateWaitingTrigger)
	}

	// Evaluate the trigger condition with the same sandboxed env contract as
	// gate conditions. The gate timeout (or its default) bounds the trigger.
	gateConfig, err := ParseGateConfig(meta)
	if err != nil {
		return HandlerResult{}, fmt.Errorf("parsing gate config: %w", err)
	}
	closedIterations, err := h.deriveIterationCount(rootBeadID)
	if err != nil {
		return HandlerResult{}, fmt.Errorf("deriving iteration count: %w", err)
	}

	cityPath := meta[FieldCityPath]
	nextIteration := closedIterations + 1

	// Defense in depth: a healthy loop only enters waiting_trigger when the
	// gate already confirmed iteration < max_iterations, so this is
	// unreachable in normal flow. Refuse loudly rather than pour an
	// over-limit wisp if corrupt state ever lands here.
	if maxIter, ok := DecodeInt(meta[FieldMaxIterations]); ok && maxIter > 0 && nextIteration > maxIter {
		return HandlerResult{}, fmt.Errorf("trigger-gated loop %q at iteration %d exceeds max_iterations %d; refusing to advance", rootBeadID, nextIteration, maxIter)
	}

	env := ConditionEnv{
		BeadID:      rootBeadID,
		Iteration:   nextIteration,
		CityPath:    cityPath,
		StorePath:   h.StorePath,
		DocPath:     meta[VarPrefix+"doc_path"],
		ArtifactDir: ArtifactDirFor(cityPath, rootBeadID, nextIteration),
	}
	if maxIter, ok := DecodeInt(meta[FieldMaxIterations]); ok {
		env.MaxIterations = maxIter
	}

	result := RunCondition(ctx, triggerConfig.Condition, env, gateConfig.Timeout, 0)
	if result.Outcome != GatePass {
		// Trigger not satisfied — keep waiting. Re-evaluated next tick.
		return HandlerResult{Action: ActionSkipped}, nil
	}

	return h.advanceFromTrigger(rootBeadID, meta, nextIteration)
}

// advanceFromTrigger pours and activates the next iteration's wisp after the
// trigger condition passes, transitioning the loop back to the active state.
// The next wisp's idempotency key makes the pour crash-safe: a re-pour after a
// crash returns the existing wisp rather than duplicating it.
func (h *Handler) advanceFromTrigger(rootBeadID string, meta map[string]string, nextIteration int) (HandlerResult, error) {
	nextKey := IdempotencyKey(rootBeadID, nextIteration)
	formula := meta[FieldFormula]
	vars := ExtractVars(meta)
	evaluatePrompt := meta[FieldEvaluatePrompt]

	nextWispID, err := h.Store.PourWisp(rootBeadID, formula, nextKey, vars, evaluatePrompt)
	if err != nil {
		existingID, found, lookupErr := h.Store.FindByIdempotencyKey(nextKey)
		if lookupErr == nil && found {
			nextWispID = existingID
		} else {
			return HandlerResult{}, fmt.Errorf("pouring wisp for iteration %d: %w", nextIteration, err)
		}
	}
	if err := h.Store.ActivateWisp(nextWispID); err != nil {
		return HandlerResult{}, fmt.Errorf("activating wisp %q: %w", nextWispID, err)
	}

	if err := h.Store.SetMetadata(rootBeadID, FieldIteration, EncodeInt(nextIteration)); err != nil {
		return HandlerResult{}, fmt.Errorf("setting iteration: %w", err)
	}
	if err := h.Store.SetMetadata(rootBeadID, FieldActiveWisp, nextWispID); err != nil {
		return HandlerResult{}, fmt.Errorf("setting active wisp: %w", err)
	}
	if err := h.Store.SetMetadata(rootBeadID, FieldState, StateActive); err != nil {
		return HandlerResult{}, fmt.Errorf("setting state to active: %w", err)
	}

	// Emit a ConvergenceIteration event recording the trigger-driven advance.
	iterPayload := IterationPayload{
		Iteration:  nextIteration,
		WispID:     meta[FieldLastProcessedWisp],
		GateMode:   meta[FieldGateMode],
		Action:     string(ActionIterate),
		NextWispID: NullableString(nextWispID),
	}
	h.emitEvent(EventIteration, EventIDIteration(rootBeadID, nextIteration), rootBeadID, iterPayload)

	return HandlerResult{
		Action:     ActionIterate,
		Iteration:  nextIteration,
		NextWispID: nextWispID,
	}, nil
}
