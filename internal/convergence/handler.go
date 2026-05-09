package convergence

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// IdempotencyKeyPrefix returns the prefix for all convergence wisp keys
// belonging to a root bead.
func IdempotencyKeyPrefix(beadID string) string {
	return "converge:" + beadID + ":iter:"
}

// IdempotencyKey returns the idempotency key for a specific iteration.
// Iteration is 1-based.
func IdempotencyKey(beadID string, iteration int) string {
	return fmt.Sprintf("converge:%s:iter:%d", beadID, iteration)
}

// ParseIterationFromKey extracts the iteration number from an idempotency
// key of the form "converge:<bead-id>:iter:<N>". Returns 0, false if the
// key doesn't match the expected format.
func ParseIterationFromKey(key string) (int, bool) {
	// Find last ":iter:" and parse the number after it.
	const marker = ":iter:"
	idx := strings.LastIndex(key, marker)
	if idx < 0 {
		return 0, false
	}
	numStr := key[idx+len(marker):]
	n, err := strconv.Atoi(numStr)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// Canonical close_reason strings for convergence-handler-driven closes.
// Every CloseBead caller uses one of these so bd's
// validation.on-close=error validator (which rejects close_reason of
// <20 chars) accepts the close. The reason also lands in the closed
// bead's metadata for audit.
const (
	CloseReasonCreateRollback  = "convergence: bead-create rollback after error"
	CloseReasonRetryRollback   = "convergence: retry-create rollback after error"
	CloseReasonManualApprove   = "convergence: iteration closed by manual approve"
	CloseReasonManualSupersede = "convergence: active wisp superseded during manual stop"
	CloseReasonManualStop      = "convergence: iteration closed by manual stop"
	CloseReasonReconcileDone   = "convergence reconcile: terminated-state bead closed"
	CloseReasonHandlerCleanup  = "convergence: terminated state observed; closing root"
	CloseReasonHandlerRoot     = "convergence: workflow handler closing root after terminate"
)

// BeadInfo holds the minimal bead information needed by the handler.
type BeadInfo struct {
	ID             string
	Status         string // "open", "in_progress", "closed"
	ParentID       string
	IdempotencyKey string
	CreatedAt      time.Time
	ClosedAt       time.Time // zero if not closed
}

// Store abstracts bead operations needed by the convergence handler.
// The bead store adapter implements this interface.
type Store interface {
	// GetBead returns basic info about a bead. Missing beads must be
	// reported with an error that wraps beads.ErrNotFound so recovery code
	// can distinguish stale references from transient store failures.
	GetBead(id string) (BeadInfo, error)

	// GetMetadata returns all metadata for a bead.
	GetMetadata(id string) (map[string]string, error)

	// SetMetadata writes a single metadata key-value pair.
	SetMetadata(id, key, value string) error

	// CloseBead sets a bead's status to "closed" and stamps reason as
	// the bead's close_reason metadata. reason must be >=20 chars to
	// satisfy bd's validation.on-close=error validator. Use one of the
	// CloseReason* constants below for canonical wording.
	CloseBead(id, reason string) error

	// DeleteBead permanently removes a bead. Used to burn discarded
	// speculative wisps so they are not counted as completed iterations.
	DeleteBead(id string) error

	// Children returns child beads of a parent.
	Children(parentID string) ([]BeadInfo, error)

	// PourWisp creates a new convergence wisp with an idempotency key.
	// If a wisp with this key already exists, returns the existing wisp's ID.
	PourWisp(parentID, formula, idempotencyKey string, vars map[string]string, evaluatePrompt string) (string, error)

	// PourSpeculativeWisp creates a hidden/unassigned convergence wisp that can
	// be activated after a nonterminal gate outcome adopts it.
	PourSpeculativeWisp(parentID, formula, idempotencyKey string, vars map[string]string, evaluatePrompt string) (string, error)

	// ActivateWisp publishes a previously speculative wisp for agent work.
	ActivateWisp(id string) error

	// FindByIdempotencyKey looks up a wisp by its idempotency key.
	FindByIdempotencyKey(key string) (string, bool, error)

	// CountActiveConvergenceLoops counts active convergence loops targeting
	// the given agent. Used for nested convergence prevention.
	CountActiveConvergenceLoops(targetAgent string) (int, error)

	// CreateConvergenceBead creates a new convergence root bead and returns its ID.
	CreateConvergenceBead(title string) (string, error)
}

// HandlerAction describes the outcome of processing a wisp_closed event.
type HandlerAction string

// HandlerAction values describing the outcome of wisp_closed processing.
const (
	ActionIterate       HandlerAction = "iterate"
	ActionApproved      HandlerAction = "approved"
	ActionNoConvergence HandlerAction = "no_convergence"
	ActionWaitingManual HandlerAction = "waiting_manual"
	ActionStopped       HandlerAction = "stopped"
	ActionSkipped       HandlerAction = "skipped"
)

// HandlerResult holds the outcome of HandleWispClosed.
type HandlerResult struct {
	Action        HandlerAction
	Iteration     int    // this handler's iteration number (from wisp key)
	GateOutcome   string // gate evaluation result (pass/fail/timeout/error)
	NextWispID    string // populated if Action == ActionIterate
	WaitingReason string // populated if Action == ActionWaitingManual
}

// Handler processes convergence wisp_closed events. It implements the 9-step
// algorithm from the Controller Behavior spec section.
//
// IMPORTANT: Handler assumes single-writer-per-bead concurrency. Only one
// goroutine may call HandleWispClosed (or any manual handler) for a given
// root bead at a time. The controller event loop provides this guarantee.
// Violating this assumption can cause stale-read races on metadata snapshots.
type Handler struct {
	Store   Store
	Emitter EventEmitter
	Clock   func() time.Time // injectable for testing; defaults to time.Now
}

// HandleWispClosed processes a wisp_closed event for a convergence root bead.
// This is the core 9-step algorithm from the spec.
//
// Crash safety: the next wisp is speculatively poured BEFORE gate evaluation
// (step 3b). If the outcome is terminal or waiting_manual, the speculative
// wisp is burned. If the process crashes after the pour but before the burn,
// the reconciler finds the speculative wisp via pending_next_wisp or
// FindByIdempotencyKey and adopts it.
func (h *Handler) HandleWispClosed(ctx context.Context, rootBeadID, wispID string) (HandlerResult, error) {
	now := h.clock()

	// Read root bead metadata.
	meta, err := h.Store.GetMetadata(rootBeadID)
	if err != nil {
		return HandlerResult{}, fmt.Errorf("reading root bead metadata: %w", err)
	}

	// Step 1: Guard check.
	state := meta[FieldState]
	if state == StateTerminated {
		_ = h.Store.CloseBead(rootBeadID, CloseReasonHandlerCleanup) // best-effort cleanup
		return HandlerResult{Action: ActionSkipped}, nil
	}

	// Step 2: Dedup check (monotonic).
	wispInfo, err := h.Store.GetBead(wispID)
	if err != nil {
		return HandlerResult{}, fmt.Errorf("reading wisp info: %w", err)
	}
	wispIteration, ok := ParseIterationFromKey(wispInfo.IdempotencyKey)
	if !ok {
		return HandlerResult{}, fmt.Errorf("parsing iteration from wisp key %q", wispInfo.IdempotencyKey)
	}

	lastProcessedIteration := 0
	if lpw := meta[FieldLastProcessedWisp]; lpw != "" {
		lpwInfo, err := h.Store.GetBead(lpw)
		if err != nil {
			// Graceful degradation: if the last-processed wisp is missing
			// or corrupted, treat it as unprocessed (iteration 0) so the
			// loop can continue rather than permanently blocking.
			lastProcessedIteration = 0
		} else if n, ok := ParseIterationFromKey(lpwInfo.IdempotencyKey); ok {
			lastProcessedIteration = n
		}
	}
	if wispIteration <= lastProcessedIteration {
		return HandlerResult{Action: ActionSkipped}, nil
	}

	// Step 3: Derive iteration.
	globalIteration, err := h.deriveIterationCount(rootBeadID)
	if err != nil {
		return HandlerResult{}, fmt.Errorf("deriving iteration count: %w", err)
	}
	storedIteration, _ := DecodeInt(meta[FieldIteration])
	if globalIteration != storedIteration {
		// Log warning: stored disagrees with derived. Use derived.
		if err := h.Store.SetMetadata(rootBeadID, FieldIteration, EncodeInt(globalIteration)); err != nil {
			return HandlerResult{}, fmt.Errorf("updating iteration count: %w", err)
		}
	}

	maxIterations, _ := DecodeInt(meta[FieldMaxIterations])

	// Parse gate config before creating speculative work. Invalid config or
	// deterministic manual-waiting paths must not leave behind a successor wisp.
	gateConfig, err := ParseGateConfig(meta)
	if err != nil {
		return HandlerResult{}, fmt.Errorf("parsing gate config: %w", err)
	}

	nextIteration := wispIteration + 1
	nextKey := IdempotencyKey(rootBeadID, nextIteration)

	gateOutcomeWisp := meta[FieldGateOutcomeWisp]
	skipGateEval := gateOutcomeWisp == wispID
	if !skipGateEval && gateConfig.Mode == GateModeCondition && gateConfig.Condition == "" {
		if pending := h.validPendingNextWisp(rootBeadID, nextKey, meta[FieldPendingNextWisp]); pending != "" {
			if burnErr := h.burnSpeculativeWisp(rootBeadID, pending); burnErr != nil {
				return HandlerResult{}, fmt.Errorf("gate mode is %q but no condition path configured; additionally burning pending wisp: %w", GateModeCondition, burnErr)
			}
		}
		return HandlerResult{}, fmt.Errorf("gate mode is %q but no condition path configured", GateModeCondition)
	}

	// Step 3b: Speculative pour - create the next wisp BEFORE gate evaluation
	// so that a crash between gate eval and commit cannot break the chain.
	// If the outcome is terminal or waiting_manual, we burn this wisp.
	speculativeWispID := h.validPendingNextWisp(rootBeadID, nextKey, meta[FieldPendingNextWisp])
	var speculativePourErr error
	needsManualWithoutGate := gateConfig.Mode == GateModeManual ||
		(gateConfig.Mode == GateModeHybrid && HybridNeedsManual(gateConfig))
	if wispIteration < maxIterations && !needsManualWithoutGate && speculativeWispID == "" {
		formula := meta[FieldFormula]
		vars := ExtractVars(meta)
		evaluatePrompt := meta[FieldEvaluatePrompt]
		speculativeWispID, err = h.Store.PourSpeculativeWisp(rootBeadID, formula, nextKey, vars, evaluatePrompt)
		if err != nil {
			existingID, found, lookupErr := h.Store.FindByIdempotencyKey(nextKey)
			if lookupErr == nil && found {
				speculativeWispID = existingID
			} else {
				speculativePourErr = err
			}
		}
		if speculativeWispID != "" {
			if err := h.Store.SetMetadata(rootBeadID, FieldPendingNextWisp, speculativeWispID); err != nil {
				if burnErr := h.burnSpeculativeWisp(rootBeadID, speculativeWispID); burnErr != nil {
					return HandlerResult{}, fmt.Errorf("setting pending next wisp: %w; additionally burning speculative wisp: %w", err, burnErr)
				}
				return HandlerResult{}, fmt.Errorf("setting pending next wisp: %w", err)
			}
		}
	}

	// Step 4: Gate evaluation (idempotent).
	var gateResult GateResult

	if skipGateEval {
		// Replay: use persisted outcome.
		gateResult.Outcome = meta[FieldGateOutcome]
		if ec := meta[FieldGateExitCode]; ec != "" {
			if code, ok := DecodeInt(ec); ok {
				gateResult.ExitCode = &code
			}
		}
		if rc, ok := DecodeInt(meta[FieldGateRetryCount]); ok {
			gateResult.RetryCount = rc
		}
		gateResult.Stdout = meta[FieldGateStdout]
		gateResult.Stderr = meta[FieldGateStderr]
		if ms := meta[FieldGateDurationMs]; ms != "" {
			if msVal, ok := DecodeInt(ms); ok {
				gateResult.Duration = time.Duration(msVal) * time.Millisecond
			}
		}
		gateResult.Truncated = meta[FieldGateTruncated] == "true"
	} else {
		// Manual mode: no gate evaluation, transition to waiting_manual.
		if gateConfig.Mode == GateModeManual {
			if err := h.burnSpeculativeWisp(rootBeadID, speculativeWispID); err != nil {
				return HandlerResult{}, fmt.Errorf("burning speculative wisp before waiting_manual: %w", err)
			}
			return h.transitionToWaitingManual(rootBeadID, wispID, wispIteration,
				gateConfig, GateResult{}, WaitManual, "", meta, now)
		}

		// Hybrid mode with no condition: fallback to manual.
		if gateConfig.Mode == GateModeHybrid && HybridNeedsManual(gateConfig) {
			if err := h.burnSpeculativeWisp(rootBeadID, speculativeWispID); err != nil {
				return HandlerResult{}, fmt.Errorf("burning speculative wisp before waiting_manual: %w", err)
			}
			return h.transitionToWaitingManual(rootBeadID, wispID, wispIteration,
				gateConfig, GateResult{}, WaitHybridNoCondition, "", meta, now)
		}

		// Read agent verdict (only if scoped to this wisp).
		verdict := ""
		if meta[FieldAgentVerdictWisp] == wispID {
			verdict = NormalizeVerdict(meta[FieldAgentVerdict])
		} else {
			verdict = VerdictBlock // no verdict or mismatched wisp
		}

		// Run gate evaluation.
		gateResult = h.evaluateGate(ctx, gateConfig, meta, wispID, wispIteration, verdict, rootBeadID)
	}

	// Step 5: Persist gate outcome.
	if !skipGateEval {
		if err := h.persistGateOutcome(rootBeadID, wispID, gateResult); err != nil {
			if burnErr := h.burnSpeculativeWisp(rootBeadID, speculativeWispID); burnErr != nil {
				return HandlerResult{}, fmt.Errorf("persisting gate outcome: %w; additionally burning speculative wisp: %w", err, burnErr)
			}
			return HandlerResult{}, fmt.Errorf("persisting gate outcome: %w", err)
		}
	}

	// Step 6: Record iteration note (audit trail).
	// Notes are informational — errors don't block control flow.

	// Step 7: Prepare outcome.
	// Check for timeout with manual action first.
	if gateResult.Outcome == GateTimeout && gateConfig.TimeoutAction == TimeoutActionManual {
		if err := h.burnSpeculativeWisp(rootBeadID, speculativeWispID); err != nil {
			return HandlerResult{}, fmt.Errorf("burning speculative wisp before waiting_manual: %w", err)
		}
		return h.transitionToWaitingManual(rootBeadID, wispID, wispIteration,
			gateConfig, gateResult, WaitTimeout, gateResult.Outcome, meta, now)
	}

	// Determine if terminal.
	isTerminal := false
	terminalReason := ""

	switch {
	case gateResult.Outcome == GatePass:
		isTerminal = true
		terminalReason = TerminalApproved
	case gateResult.Outcome == GateTimeout && gateConfig.TimeoutAction == TimeoutActionTerminate:
		isTerminal = true
		terminalReason = TerminalNoConvergence
	case wispIteration >= maxIterations:
		// At max iterations with non-pass outcome.
		isTerminal = true
		terminalReason = TerminalNoConvergence
	}

	if !isTerminal {
		if speculativePourErr != nil {
			return h.handleSlingFailure(rootBeadID, wispID, wispIteration,
				gateConfig, gateResult, meta, now)
		}
		// Iterate: clear verdict and use speculatively poured wisp.
		return h.iterate(ctx, rootBeadID, wispID, wispIteration, gateConfig, gateResult, meta, now, speculativeWispID)
	}

	// Terminal transition - burn the speculative wisp if one was poured.
	if err := h.burnSpeculativeWisp(rootBeadID, speculativeWispID); err != nil {
		return HandlerResult{}, fmt.Errorf("burning speculative wisp before terminal transition: %w", err)
	}
	return h.terminate(rootBeadID, wispID, wispIteration, gateConfig, gateResult,
		terminalReason, "controller", globalIteration, meta, now)
}

// transitionToWaitingManual handles the transition to waiting_manual state.
// This covers manual mode, hybrid-no-condition, and timeout-with-manual-action.
func (h *Handler) transitionToWaitingManual(
	rootBeadID, wispID string,
	iteration int,
	gateConfig GateConfig,
	gateResult GateResult,
	reason string,
	gateOutcome string,
	meta map[string]string,
	_ time.Time,
) (HandlerResult, error) {
	// Build action string for iteration event.
	action := string(ActionWaitingManual)

	// Compute durations.
	iterDur, cumDur := h.computeDurations(rootBeadID, wispID)

	// Build gate outcome pointer for events.
	var gateOutcomePtr *string
	if gateOutcome != "" {
		gateOutcomePtr = &gateOutcome
	}

	// Read verdict for event payload.
	verdict := ""
	if meta[FieldAgentVerdictWisp] == wispID {
		verdict = NormalizeVerdict(meta[FieldAgentVerdict])
	}

	// Step 8: Emit ConvergenceIteration event.
	iterPayload := IterationPayload{
		Iteration:            iteration,
		WispID:               wispID,
		AgentVerdict:         verdict,
		GateMode:             gateConfig.Mode,
		GateOutcome:          gateOutcomePtr,
		GateResult:           GateResultToPayload(gateResult),
		GateRetryCount:       gateResult.RetryCount,
		Action:               action,
		WaitingReason:        NullableString(reason),
		IterationDurationMs:  iterDur.Milliseconds(),
		CumulativeDurationMs: cumDur.Milliseconds(),
	}
	h.emitEvent(EventIteration, EventIDIteration(rootBeadID, iteration), rootBeadID, iterPayload)

	// Step 9: Commit point — write state changes.
	// Write last_processed_wisp LAST (write ordering contract):
	// it is the dedup/idempotency marker — if the process crashes before
	// this write, recovery re-processes this wisp rather than skipping it.
	if err := h.Store.SetMetadata(rootBeadID, FieldActiveWisp, ""); err != nil {
		return HandlerResult{}, fmt.Errorf("clearing active wisp: %w", err)
	}
	if err := h.Store.SetMetadata(rootBeadID, FieldWaitingReason, reason); err != nil {
		return HandlerResult{}, fmt.Errorf("setting waiting reason: %w", err)
	}
	if err := h.Store.SetMetadata(rootBeadID, FieldState, StateWaitingManual); err != nil {
		return HandlerResult{}, fmt.Errorf("setting state to waiting_manual: %w", err)
	}
	if err := h.Store.SetMetadata(rootBeadID, FieldLastProcessedWisp, wispID); err != nil {
		return HandlerResult{}, fmt.Errorf("setting last processed wisp: %w", err)
	}

	// Emit ConvergenceWaitingManual event.
	wmPayload := WaitingManualPayload{
		Iteration:            iteration,
		WispID:               wispID,
		AgentVerdict:         verdict,
		GateMode:             gateConfig.Mode,
		GateOutcome:          gateOutcomePtr,
		GateResult:           GateResultToPayload(gateResult),
		Reason:               reason,
		IterationDurationMs:  iterDur.Milliseconds(),
		CumulativeDurationMs: cumDur.Milliseconds(),
	}
	h.emitEvent(EventWaitingManual, EventIDWaitingManual(rootBeadID, iteration), rootBeadID, wmPayload)

	return HandlerResult{
		Action:        ActionWaitingManual,
		Iteration:     iteration,
		GateOutcome:   gateOutcome,
		WaitingReason: reason,
	}, nil
}

// iterate clears verdict and adopts the speculatively poured wisp (or pours
// a new one as fallback). The speculative wisp was created in step 3b of
// HandleWispClosed before gate evaluation, ensuring crash safety.
func (h *Handler) iterate(
	_ context.Context,
	rootBeadID, wispID string,
	iteration int,
	gateConfig GateConfig,
	gateResult GateResult,
	meta map[string]string,
	now time.Time,
	speculativeWispID string,
) (HandlerResult, error) {
	// Clear verdict for next iteration (only if verdict belongs to this wisp).
	if meta[FieldAgentVerdictWisp] == wispID {
		if err := h.Store.SetMetadata(rootBeadID, FieldAgentVerdict, ""); err != nil {
			return HandlerResult{}, fmt.Errorf("clearing agent verdict: %w", err)
		}
		if err := h.Store.SetMetadata(rootBeadID, FieldAgentVerdictWisp, ""); err != nil {
			return HandlerResult{}, fmt.Errorf("clearing agent verdict wisp: %w", err)
		}
	}

	// Adopt speculatively poured wisp, or pour a new one as fallback.
	nextIteration := iteration + 1
	nextKey := IdempotencyKey(rootBeadID, nextIteration)

	var nextWispID string
	if speculativeWispID != "" {
		// Speculative wisp was pre-poured in step 3b — adopt it.
		nextWispID = speculativeWispID
	} else {
		// Fallback: pour now (e.g., at max iterations boundary or error).
		formula := meta[FieldFormula]
		vars := ExtractVars(meta)
		evaluatePrompt := meta[FieldEvaluatePrompt]
		var pourErr error
		nextWispID, pourErr = h.Store.PourWisp(rootBeadID, formula, nextKey, vars, evaluatePrompt)
		if pourErr != nil {
			existingID, found, lookupErr := h.Store.FindByIdempotencyKey(nextKey)
			if lookupErr == nil && found {
				nextWispID = existingID
			} else {
				return h.handleSlingFailure(rootBeadID, wispID, iteration,
					gateConfig, gateResult, meta, now)
			}
		}
	}
	if err := h.Store.ActivateWisp(nextWispID); err != nil {
		return HandlerResult{}, fmt.Errorf("activating next wisp %q: %w", nextWispID, err)
	}

	// Compute durations.
	iterDur, cumDur := h.computeDurations(rootBeadID, wispID)

	// Read verdict for event payload.
	verdict := ""
	if meta[FieldAgentVerdictWisp] == wispID {
		verdict = NormalizeVerdict(meta[FieldAgentVerdict])
	}

	// Step 8: Emit ConvergenceIteration event.
	gateOutcome := gateResult.Outcome
	iterPayload := IterationPayload{
		Iteration:            iteration,
		WispID:               wispID,
		AgentVerdict:         verdict,
		GateMode:             gateConfig.Mode,
		GateOutcome:          NullableString(gateOutcome),
		GateResult:           GateResultToPayload(gateResult),
		GateRetryCount:       gateResult.RetryCount,
		Action:               string(ActionIterate),
		NextWispID:           NullableString(nextWispID),
		IterationDurationMs:  iterDur.Milliseconds(),
		CumulativeDurationMs: cumDur.Milliseconds(),
	}
	h.emitEvent(EventIteration, EventIDIteration(rootBeadID, iteration), rootBeadID, iterPayload)

	// Step 9: Commit point.
	// Write last_processed_wisp LAST — it is the dedup marker.
	if err := h.Store.SetMetadata(rootBeadID, FieldActiveWisp, nextWispID); err != nil {
		return HandlerResult{}, fmt.Errorf("setting active wisp: %w", err)
	}
	if err := h.Store.SetMetadata(rootBeadID, FieldLastProcessedWisp, wispID); err != nil {
		return HandlerResult{}, fmt.Errorf("setting last processed wisp: %w", err)
	}
	// Clear pending_next_wisp after the dedup marker commits. If this best-effort
	// cleanup fails, validPendingNextWisp will self-heal on the next entry.
	_ = h.Store.SetMetadata(rootBeadID, FieldPendingNextWisp, "")

	return HandlerResult{
		Action:      ActionIterate,
		Iteration:   iteration,
		GateOutcome: gateOutcome,
		NextWispID:  nextWispID,
	}, nil
}

// terminate handles the terminal transition (approved or no_convergence).
func (h *Handler) terminate(
	rootBeadID, wispID string,
	iteration int,
	gateConfig GateConfig,
	gateResult GateResult,
	reason, actor string,
	globalIteration int,
	meta map[string]string,
	_ time.Time,
) (HandlerResult, error) {
	// Compute durations.
	iterDur, cumDur := h.computeDurations(rootBeadID, wispID)

	// Map terminal reason to action string.
	action := reason // "approved" or "no_convergence"

	// Read verdict for event payload.
	verdict := ""
	if meta[FieldAgentVerdictWisp] == wispID {
		verdict = NormalizeVerdict(meta[FieldAgentVerdict])
	}

	gateOutcome := gateResult.Outcome

	// Step 8: Emit ConvergenceIteration event.
	iterPayload := IterationPayload{
		Iteration:            iteration,
		WispID:               wispID,
		AgentVerdict:         verdict,
		GateMode:             gateConfig.Mode,
		GateOutcome:          NullableString(gateOutcome),
		GateResult:           GateResultToPayload(gateResult),
		GateRetryCount:       gateResult.RetryCount,
		Action:               action,
		IterationDurationMs:  iterDur.Milliseconds(),
		CumulativeDurationMs: cumDur.Milliseconds(),
	}
	h.emitEvent(EventIteration, EventIDIteration(rootBeadID, iteration), rootBeadID, iterPayload)

	// Emit ConvergenceTerminated event.
	termPayload := TerminatedPayload{
		TerminalReason:       reason,
		TotalIterations:      globalIteration,
		FinalStatus:          "closed",
		Actor:                actor,
		CumulativeDurationMs: cumDur.Milliseconds(),
	}
	h.emitEvent(EventTerminated, EventIDTerminated(rootBeadID), rootBeadID, termPayload)

	// Step 9: Commit point.
	// Write terminal_reason and terminal_actor BEFORE state=terminated,
	// then last_processed_wisp LAST — it is the dedup marker.
	if err := h.Store.SetMetadata(rootBeadID, FieldTerminalReason, reason); err != nil {
		return HandlerResult{}, fmt.Errorf("setting terminal reason: %w", err)
	}
	if err := h.Store.SetMetadata(rootBeadID, FieldTerminalActor, actor); err != nil {
		return HandlerResult{}, fmt.Errorf("setting terminal actor: %w", err)
	}
	if err := h.Store.SetMetadata(rootBeadID, FieldState, StateTerminated); err != nil {
		return HandlerResult{}, fmt.Errorf("setting state to terminated: %w", err)
	}
	if err := h.Store.CloseBead(rootBeadID, CloseReasonHandlerRoot); err != nil {
		return HandlerResult{}, fmt.Errorf("closing root bead: %w", err)
	}
	if err := h.Store.SetMetadata(rootBeadID, FieldLastProcessedWisp, wispID); err != nil {
		return HandlerResult{}, fmt.Errorf("setting last processed wisp: %w", err)
	}

	return HandlerResult{
		Action:      HandlerAction(action),
		Iteration:   iteration,
		GateOutcome: gateOutcome,
	}, nil
}

// handleSlingFailure transitions to waiting_manual when PourWisp fails.
func (h *Handler) handleSlingFailure(
	rootBeadID, wispID string,
	iteration int,
	gateConfig GateConfig,
	gateResult GateResult,
	meta map[string]string,
	now time.Time,
) (HandlerResult, error) {
	// Delegate to transitionToWaitingManual which handles all state writes
	// including FieldWaitingReason as part of its commit sequence.
	return h.transitionToWaitingManual(rootBeadID, wispID, iteration,
		gateConfig, gateResult, WaitSlingFailure, gateResult.Outcome, meta, now)
}

// evaluateGate runs the gate evaluation based on gate mode.
func (h *Handler) evaluateGate(
	ctx context.Context,
	gateConfig GateConfig,
	meta map[string]string,
	wispID string,
	iteration int,
	verdict string,
	rootBeadID string,
) GateResult {
	retryBudget := 0
	if gateConfig.TimeoutAction == TimeoutActionRetry {
		retryBudget = MaxGateRetries
	}

	cityPath := meta[FieldCityPath] // set during create
	env := ConditionEnv{
		BeadID:      rootBeadID,
		Iteration:   iteration,
		CityPath:    cityPath,
		WispID:      wispID,
		DocPath:     meta[VarPrefix+"doc_path"],
		ArtifactDir: ArtifactDirFor(cityPath, rootBeadID, iteration),
	}

	// Compute durations for environment.
	iterDur, cumDur := h.computeDurations(rootBeadID, wispID)
	env.IterationDurationMs = iterDur.Milliseconds()
	env.CumulativeDurationMs = cumDur.Milliseconds()

	maxIter, _ := DecodeInt(meta[FieldMaxIterations])
	env.MaxIterations = maxIter

	switch gateConfig.Mode {
	case GateModeCondition:
		return RunCondition(ctx, gateConfig.Condition, env, gateConfig.Timeout, retryBudget)
	case GateModeHybrid:
		return EvaluateHybrid(ctx, gateConfig, env, verdict)
	default:
		// Should not reach here (manual mode handled earlier).
		return GateResult{Outcome: GateError, Stderr: "unexpected gate mode: " + gateConfig.Mode}
	}
}

// persistGateOutcome writes gate results to bead metadata (step 5).
// Persists the full result for replay fidelity: stdout, stderr, duration,
// and truncated flag are needed to reconstruct event payloads after crash recovery.
func (h *Handler) persistGateOutcome(rootBeadID, wispID string, result GateResult) error {
	if err := h.Store.SetMetadata(rootBeadID, FieldGateOutcome, result.Outcome); err != nil {
		return err
	}
	exitCode := ""
	if result.ExitCode != nil {
		exitCode = EncodeInt(*result.ExitCode)
	}
	if err := h.Store.SetMetadata(rootBeadID, FieldGateExitCode, exitCode); err != nil {
		return err
	}
	if err := h.Store.SetMetadata(rootBeadID, FieldGateRetryCount, EncodeInt(result.RetryCount)); err != nil {
		return err
	}
	if err := h.Store.SetMetadata(rootBeadID, FieldGateStdout, result.Stdout); err != nil {
		return err
	}
	if err := h.Store.SetMetadata(rootBeadID, FieldGateStderr, result.Stderr); err != nil {
		return err
	}
	if err := h.Store.SetMetadata(rootBeadID, FieldGateDurationMs, strconv.FormatInt(result.Duration.Milliseconds(), 10)); err != nil {
		return err
	}
	truncated := ""
	if result.Truncated {
		truncated = "true"
	}
	if err := h.Store.SetMetadata(rootBeadID, FieldGateTruncated, truncated); err != nil {
		return err
	}
	// Write gate_outcome_wisp LAST — this is the idempotency marker.
	return h.Store.SetMetadata(rootBeadID, FieldGateOutcomeWisp, wispID)
}

// deriveIterationCount counts closed child wisps with convergence idempotency
// key prefix.
func (h *Handler) deriveIterationCount(rootBeadID string) (int, error) {
	children, err := h.Store.Children(rootBeadID)
	if err != nil {
		return 0, err
	}
	prefix := IdempotencyKeyPrefix(rootBeadID)
	count := 0
	for _, child := range children {
		if strings.HasPrefix(child.IdempotencyKey, prefix) && child.Status == "closed" {
			count++
		}
	}
	return count, nil
}

// computeDurations computes iteration and cumulative durations.
// Returns zero durations on error (best-effort).
func (h *Handler) computeDurations(rootBeadID, wispID string) (iterDur, cumDur time.Duration) {
	if wispID != "" {
		wispInfo, err := h.Store.GetBead(wispID)
		if err == nil && !wispInfo.ClosedAt.IsZero() && !wispInfo.CreatedAt.IsZero() {
			iterDur = wispInfo.ClosedAt.Sub(wispInfo.CreatedAt)
		}
	}

	// Cumulative: sum durations of all closed convergence children.
	children, err := h.Store.Children(rootBeadID)
	if err != nil {
		return iterDur, 0
	}
	prefix := IdempotencyKeyPrefix(rootBeadID)
	for _, child := range children {
		if strings.HasPrefix(child.IdempotencyKey, prefix) && child.Status == "closed" &&
			!child.ClosedAt.IsZero() && !child.CreatedAt.IsZero() {
			cumDur += child.ClosedAt.Sub(child.CreatedAt)
		}
	}
	return iterDur, cumDur
}

// emitEvent emits a convergence event through the EventEmitter.
func (h *Handler) emitEvent(eventType, eventID, beadID string, payload any) {
	if h.Emitter == nil {
		return
	}
	h.Emitter.Emit(eventType, eventID, beadID, MarshalPayload(payload), false)
}

// clock returns the current time, using the injected Clock or time.Now.
func (h *Handler) clock() time.Time {
	if h.Clock != nil {
		return h.Clock()
	}
	return time.Now()
}

// burnSpeculativeWisp deletes a speculatively poured wisp and clears the
// pending_next_wisp metadata field. Called when the gate outcome is terminal
// or waiting_manual and the speculative wisp is not needed.
func (h *Handler) burnSpeculativeWisp(rootBeadID, speculativeWispID string) error {
	if speculativeWispID == "" {
		return nil
	}
	if err := h.deleteBeadSubtree(speculativeWispID); err != nil {
		return err
	}
	_ = h.Store.SetMetadata(rootBeadID, FieldPendingNextWisp, "")
	return nil
}

func (h *Handler) deleteBeadSubtree(id string) error {
	children, err := h.Store.Children(id)
	if err != nil {
		return fmt.Errorf("listing children for delete %q: %w", id, err)
	}
	for _, child := range children {
		if err := h.deleteBeadSubtree(child.ID); err != nil {
			return err
		}
	}
	if err := h.Store.DeleteBead(id); err != nil {
		return fmt.Errorf("deleting bead %q: %w", id, err)
	}
	return nil
}

func (h *Handler) validPendingNextWisp(rootBeadID, nextKey, pendingID string) string {
	if pendingID == "" {
		return ""
	}
	info, err := h.Store.GetBead(pendingID)
	if err != nil || info.ParentID != rootBeadID || info.IdempotencyKey != nextKey || info.Status == "closed" {
		_ = h.Store.SetMetadata(rootBeadID, FieldPendingNextWisp, "")
		return ""
	}
	return pendingID
}

// CheckNestedConvergence validates that creating a new convergence loop
// from callingAgent targeting targetAgent would not cause a self-deadlock.
// Returns an error only if callingAgent == targetAgent AND the agent already
// has an active convergence loop (self-targeting deadlock). Multiple
// concurrent loops targeting different agents are allowed — use
// CheckConcurrencyLimits for per-agent caps.
func CheckNestedConvergence(store Store, callingAgent, targetAgent string) error {
	if callingAgent != targetAgent {
		return nil // cross-agent convergence is always safe from deadlock
	}
	count, err := store.CountActiveConvergenceLoops(targetAgent)
	if err != nil {
		return fmt.Errorf("checking nested convergence: %w", err)
	}
	if count > 0 {
		return fmt.Errorf("cannot create convergence loop targeting %q: "+
			"agent is currently executing a convergence wisp. "+
			"Self-targeting convergence would deadlock", targetAgent)
	}
	return nil
}

// CheckConcurrencyLimits validates that creating a new convergence loop
// would not exceed the per-agent limit.
//
// NOTE: City-wide max_total enforcement is deferred. The config exposes
// max_total for forward compatibility, but this function only checks
// per-agent limits. Total enforcement requires a store method that counts
// all active loops across all agents, which will be added in a later wave.
func CheckConcurrencyLimits(store Store, targetAgent string, maxPerAgent int) error {
	agentCount, err := store.CountActiveConvergenceLoops(targetAgent)
	if err != nil {
		return fmt.Errorf("checking concurrency limits: %w", err)
	}
	if agentCount >= maxPerAgent {
		return fmt.Errorf("cannot create convergence loop targeting %q: "+
			"per-agent limit reached (%d/%d active loops)", targetAgent, agentCount, maxPerAgent)
	}
	return nil
}
