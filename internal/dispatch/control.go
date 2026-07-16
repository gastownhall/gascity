package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/convergence"
	"github.com/gastownhall/gascity/internal/formula"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/molecule"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/storeref"
)

// attemptDisposition is the normalized outcome of a closed attempt/iteration,
// shared by the retry and ralph control loops.
type attemptDisposition int

const (
	// attemptPass closes the control as passed.
	attemptPass attemptDisposition = iota
	// attemptHardFail closes the control as a terminal hard failure regardless
	// of attempts remaining (only the retry classifier produces this).
	attemptHardFail
	// attemptContinue spawns the next attempt when attempts remain, or disposes
	// of the exhausted control via the strategy when max_attempts is reached.
	attemptContinue
)

// attemptEvaluation is the strategy-produced classification of a closed
// attempt/iteration bead: its disposition plus the values recorded in the
// attempt log and (for hard/exhaust closures) the failure reason.
type attemptEvaluation struct {
	disposition attemptDisposition
	logOutcome  string // value recorded in the attempt log
	logDetail   string // detail recorded in the attempt log (reason/stderr)
	reason      string // failure reason stamped on terminal metadata
}

// controlAttemptStrategy is the per-kind seam over the shared attempt loop.
// The two live implementations (retry, ralph) differ only in how they classify
// a closed attempt, what extra metadata a pass carries, and how an exhausted
// attempt is disposed. kind/subjectNoun/missingNoun carry the control-kind
// trace and error wording (control kinds, not role names).
type controlAttemptStrategy struct {
	kind        string // "retry" | "ralph" — trace text only
	subjectNoun string // "attempt" | "iteration" — error/trace text
	missingNoun string // "no attempt found" | "no iteration found"
	evaluate    func(store beads.Store, bead, attempt beads.Bead, attemptNum int, opts ProcessOptions) (attemptEvaluation, error)
	onPass      func(closeMetadata map[string]string, attempt beads.Bead)
	exhaust     func(store beads.Store, beadID string, attemptNum int, reason, attemptLog string) (ControlResult, error)
}

// processRetryControl handles a retry control bead when it becomes ready
// (its blocking dep on the latest attempt has resolved).
func processRetryControl(store beads.Store, bead beads.Bead, opts ProcessOptions) (ControlResult, error) {
	onExhausted := bead.Metadata[beadmeta.OnExhaustedMetadataKey]
	if onExhausted == "" {
		onExhausted = beadmeta.DispositionHardFail
	}
	strategy := controlAttemptStrategy{
		kind:        "retry",
		subjectNoun: "attempt",
		missingNoun: "no attempt found",
		evaluate:    evaluateRetryAttempt,
		onPass: func(closeMetadata map[string]string, attempt beads.Bead) {
			copyNonGCMetadata(closeMetadata, attempt.Metadata)
		},
		exhaust: func(store beads.Store, beadID string, attemptNum int, reason, attemptLog string) (ControlResult, error) {
			return handleRetryExhaustion(store, beadID, attemptNum, reason, onExhausted, attemptLog)
		},
	}
	return processAttemptControl(store, bead, opts, strategy)
}

// processRalphControl handles a ralph control bead when it becomes ready.
func processRalphControl(store beads.Store, bead beads.Bead, opts ProcessOptions) (ControlResult, error) {
	strategy := controlAttemptStrategy{
		kind:        "ralph",
		subjectNoun: "iteration",
		missingNoun: "no iteration found",
		evaluate:    evaluateRalphIteration,
		exhaust: func(store beads.Store, beadID string, iterationNum int, _, attemptLog string) (ControlResult, error) {
			closeMetadata := map[string]string{
				beadmeta.AttemptLogMetadataKey:    attemptLog,
				beadmeta.OutcomeMetadataKey:       beadmeta.OutcomeFail,
				beadmeta.FailedAttemptMetadataKey: strconv.Itoa(iterationNum),
			}
			clearControllerSpawnErrorMetadata(closeMetadata)
			if err := updateMetadataAndClose(store, beadID, closeMetadata); err != nil {
				return ControlResult{}, fmt.Errorf("%s: closing exhausted: %w", beadID, err)
			}
			return ControlResult{Processed: true, Action: "fail"}, nil
		},
	}
	return processAttemptControl(store, bead, opts, strategy)
}

// processAttemptControl is the shared retry/ralph control loop: parse
// max_attempts, find the latest attempt, quarantine a malformed graph, drive a
// pending attempt to convergence, then classify the closed attempt via the
// strategy and pass / hard-fail / spawn-next / exhaust accordingly. The three
// per-kind seams live in controlAttemptStrategy.
func processAttemptControl(store beads.Store, bead beads.Bead, opts ProcessOptions, strategy controlAttemptStrategy) (ControlResult, error) {
	maxAttempts, err := strconv.Atoi(bead.Metadata[beadmeta.MaxAttemptsMetadataKey])
	if err != nil || maxAttempts < 1 {
		return ControlResult{}, fmt.Errorf("%s: invalid gc.max_attempts %q", bead.ID, bead.Metadata[beadmeta.MaxAttemptsMetadataKey])
	}

	// Find the most recent attempt.
	attempt, err := findLatestAttempt(store, bead)
	if err != nil {
		return ControlResult{}, fmt.Errorf("%s: finding latest %s: %w", bead.ID, strategy.subjectNoun, err)
	}
	if attempt.ID == "" {
		// A control with no attempt sub-DAG cannot become valid by waiting —
		// the graph is malformed (missing seed or a seed attach marked
		// molecule_failed). Classify for the dispatcher quarantine instead of
		// fataling the serve loop, which crash-looped all dispatch for the rig.
		// See gastownhall/gascity#2798.
		opts.tracef("process-control bead=%s kind=%s quarantine reason=no_%s_found root=%s",
			bead.ID, strategy.kind, strategy.subjectNoun, bead.Metadata[beadmeta.RootBeadIDMetadataKey])
		return ControlResult{}, fmt.Errorf("%w: %s: %s", ErrControlGraphMalformed, bead.ID, strategy.missingNoun)
	}
	if attempt.Status != "closed" {
		return ensurePendingAttemptConverges(store, bead, attempt, strategy, opts)
	}

	attemptNum, _ := strconv.Atoi(attempt.Metadata[beadmeta.AttemptMetadataKey])
	eval, err := strategy.evaluate(store, bead, attempt, attemptNum, opts)
	if err != nil {
		return ControlResult{}, err
	}
	attemptLog, err := appendAttemptLogValue(bead.Metadata[beadmeta.AttemptLogMetadataKey], attemptNum, eval.logOutcome, eval.logDetail, opts.tracef)
	if err != nil {
		return ControlResult{}, fmt.Errorf("%s: recording attempt log: %w", bead.ID, err)
	}

	switch eval.disposition {
	case attemptPass:
		closeMetadata := map[string]string{
			beadmeta.AttemptLogMetadataKey: attemptLog,
			beadmeta.OutcomeMetadataKey:    beadmeta.OutcomePass,
		}
		clearControllerSpawnErrorMetadata(closeMetadata)
		if outputJSON := attempt.Metadata[beadmeta.OutputJSONMetadataKey]; outputJSON != "" {
			closeMetadata[beadmeta.OutputJSONMetadataKey] = outputJSON
		}
		if strategy.onPass != nil {
			strategy.onPass(closeMetadata, attempt)
		}
		if err := updateMetadataAndClose(store, bead.ID, closeMetadata); err != nil {
			return ControlResult{}, fmt.Errorf("%s: closing passed: %w", bead.ID, err)
		}
		scopeResult, err := reconcileClosedScopeMemberWithOptions(store, bead.ID, opts)
		if err != nil {
			return ControlResult{}, fmt.Errorf("%s: reconciling enclosing scope: %w", bead.ID, err)
		}
		return ControlResult{Processed: true, Action: "pass", Skipped: scopeResult.Skipped}, nil

	case attemptHardFail:
		closeMetadata := map[string]string{
			beadmeta.AttemptLogMetadataKey:       attemptLog,
			beadmeta.OutcomeMetadataKey:          beadmeta.OutcomeFail,
			beadmeta.FailedAttemptMetadataKey:    strconv.Itoa(attemptNum),
			beadmeta.FailureClassMetadataKey:     beadmeta.FailureClassHard,
			beadmeta.FailureReasonMetadataKey:    eval.reason,
			beadmeta.FinalDispositionMetadataKey: beadmeta.DispositionHardFail,
		}
		clearControllerSpawnErrorMetadata(closeMetadata)
		if err := updateMetadataAndClose(store, bead.ID, closeMetadata); err != nil {
			return ControlResult{}, fmt.Errorf("%s: closing hard-failed: %w", bead.ID, err)
		}
		scopeResult, err := reconcileClosedScopeMemberWithOptions(store, bead.ID, opts)
		if err != nil {
			return ControlResult{}, fmt.Errorf("%s: reconciling enclosing scope: %w", bead.ID, err)
		}
		return ControlResult{Processed: true, Action: "hard-fail", Skipped: scopeResult.Skipped}, nil

	case attemptContinue:
		if attemptNum >= maxAttempts {
			exhaustedResult, err := strategy.exhaust(store, bead.ID, attemptNum, eval.reason, attemptLog)
			if err != nil {
				return ControlResult{}, err
			}
			scopeResult, err := reconcileClosedScopeMemberWithOptions(store, bead.ID, opts)
			if err != nil {
				return ControlResult{}, fmt.Errorf("%s: reconciling enclosing scope: %w", bead.ID, err)
			}
			exhaustedResult.Skipped += scopeResult.Skipped
			return exhaustedResult, nil
		}

		// Spawn next attempt.
		spawnMetadata := map[string]string{beadmeta.AttemptLogMetadataKey: attemptLog}
		clearControllerSpawnErrorMetadata(spawnMetadata)
		if err := store.SetMetadataBatch(bead.ID, spawnMetadata); err != nil {
			if controllerSpawnBoundaryPending(store, bead.ID, err, opts) {
				return ControlResult{}, ErrControlPending
			}
			return ControlResult{}, fmt.Errorf("%s: recording attempt log: %w", bead.ID, err)
		}
		nextAttempt := attemptNum + 1
		if err := spawnNextAttempt(context.Background(), store, bead, nextAttempt, opts); err != nil {
			if markControllerSpawnError(store, bead.ID, err, opts) {
				return ControlResult{}, ErrControlPending
			}
			return ControlResult{}, fmt.Errorf("%s: spawning %s %d: %w", bead.ID, strategy.subjectNoun, nextAttempt, err)
		}

		return ControlResult{Processed: true, Action: "retry", Created: 1}, nil

	default:
		return ControlResult{}, fmt.Errorf("%s: unsupported attempt disposition", bead.ID)
	}
}

// ensurePendingAttemptConverges drives a not-yet-closed attempt toward
// convergence: it re-adds the blocking dep, syncs the control epoch to a
// recovered attempt, and closes any generated spec beads, returning
// ErrControlPending. Each store boundary error is classified through the
// controller spawn boundary so transient failures stay open for retry.
func ensurePendingAttemptConverges(store beads.Store, bead, attempt beads.Bead, strategy controlAttemptStrategy, opts ProcessOptions) (ControlResult, error) {
	if err := ensureBlockingDependency(store, bead.ID, attempt.ID); err != nil {
		if controllerSpawnBoundaryPending(store, bead.ID, err, opts) {
			return ControlResult{}, ErrControlPending
		}
		return ControlResult{}, fmt.Errorf("%s: blocking on pending %s %s: %w", bead.ID, strategy.subjectNoun, attempt.ID, err)
	}
	if err := syncControlEpochToAttempt(store, bead, attempt); err != nil {
		if controllerSpawnBoundaryPending(store, bead.ID, err, opts) {
			return ControlResult{}, ErrControlPending
		}
		return ControlResult{}, fmt.Errorf("%s: advancing recovered %s epoch for %s: %w", bead.ID, strategy.subjectNoun, attempt.ID, err)
	}
	if err := closeGeneratedSpecBeadsForAttempt(store, bead, attempt); err != nil {
		if controllerSpawnBoundaryPending(store, bead.ID, err, opts) {
			return ControlResult{}, ErrControlPending
		}
		return ControlResult{}, fmt.Errorf("%s: closing generated spec beads for pending %s %s: %w", bead.ID, strategy.subjectNoun, attempt.ID, err)
	}
	return ControlResult{}, ErrControlPending
}

// evaluateRetryAttempt classifies a closed retry attempt via its worker-result
// postconditions. classifyRetryAttempt only emits pass/hard/transient, so the
// default branch is defensive.
func evaluateRetryAttempt(store beads.Store, bead, attempt beads.Bead, _ int, opts ProcessOptions) (attemptEvaluation, error) {
	result, err := classifyRetryAttemptWithPostconditions(store, attempt, opts)
	if err != nil {
		return attemptEvaluation{}, fmt.Errorf("%s: evaluating retry postconditions for %s: %w", bead.ID, attempt.ID, err)
	}
	eval := attemptEvaluation{logOutcome: result.Outcome, logDetail: result.Reason, reason: result.Reason}
	switch result.Outcome {
	case "pass":
		eval.disposition = attemptPass
	case "hard":
		eval.disposition = attemptHardFail
	case "transient":
		eval.disposition = attemptContinue
	default:
		return attemptEvaluation{}, fmt.Errorf("%s: unsupported outcome %q", bead.ID, result.Outcome)
	}
	return eval, nil
}

// evaluateRalphIteration propagates the iteration's non-gc metadata onto the
// ralph control, reloads the control so the check sees the updated values, and
// runs the check script. A GatePass closes the control; anything else spawns
// the next iteration or exhausts.
func evaluateRalphIteration(store beads.Store, bead, iteration beads.Bead, iterationNum int, opts ProcessOptions) (attemptEvaluation, error) {
	// Propagate non-gc metadata from the iteration to the ralph control BEFORE
	// running the check. This makes the iteration's output (e.g.,
	// review.verdict) visible on the ralph bead for check scripts that read
	// $GC_BEAD_ID metadata.
	if err := propagateRetrySubjectMetadata(store, bead.ID, iteration); err != nil {
		return attemptEvaluation{}, fmt.Errorf("%s: propagating iteration metadata: %w", bead.ID, err)
	}
	// Reload the control bead after propagation so the check sees updated values.
	reloaded, err := store.Get(bead.ID)
	if err != nil {
		return attemptEvaluation{}, fmt.Errorf("%s: reloading after propagation: %w", bead.ID, err)
	}
	// The control bead carries the check config (gc.check_path etc), and the
	// iteration is the subject whose output is being checked.
	checkResult, err := runRalphCheck(store, reloaded, iteration, iterationNum, opts)
	if err != nil {
		return attemptEvaluation{}, fmt.Errorf("%s: running check: %w", bead.ID, err)
	}
	eval := attemptEvaluation{logOutcome: checkResult.Outcome, logDetail: checkResult.Stderr}
	if checkResult.Outcome == convergence.GatePass {
		eval.disposition = attemptPass
	} else {
		eval.disposition = attemptContinue
	}
	return eval, nil
}

func ensureBlockingDependency(store beads.Store, issueID, dependsOnID string) error {
	deps, err := store.DepList(issueID, "down")
	if err != nil {
		return err
	}
	for _, dep := range deps {
		if dep.DependsOnID == dependsOnID && dep.Type == "blocks" {
			return nil
		}
	}
	return store.DepAdd(issueID, dependsOnID, "blocks")
}

func controllerSpawnBoundaryPending(store beads.Store, beadID string, err error, opts ProcessOptions) bool {
	if err == nil {
		return false
	}
	return markControllerSpawnError(store, beadID, err, opts)
}

func syncControlEpochToAttempt(store beads.Store, control, attempt beads.Bead) error {
	current, err := strconv.Atoi(strings.TrimSpace(control.Metadata[beadmeta.ControlEpochMetadataKey]))
	if err != nil || current < 1 {
		return nil
	}
	attemptNum, err := strconv.Atoi(strings.TrimSpace(attempt.Metadata[beadmeta.AttemptMetadataKey]))
	if err != nil || attemptNum <= current {
		return nil
	}
	writer, _, resolveErr := beads.ResolveConditionalWriter(store)
	if resolveErr != nil {
		return fmt.Errorf("syncing control epoch on %s: %w", control.ID, resolveErr)
	}
	if writer == nil {
		return store.SetMetadata(control.ID, beadmeta.ControlEpochMetadataKey, strconv.Itoa(attemptNum))
	}
	// Bounded to one re-issue from a fresh read: losing the CAS is benign
	// (another processor advanced the epoch first), but a conflict that keeps
	// recurring with a still-stale epoch is cross-key revision interference
	// and must surface as transient rather than loop (level-triggered passes
	// re-enter).
	const syncAttempts = 2
	expected := current
	for attempt := 1; attempt <= syncAttempts; attempt++ {
		ok, casErr := writer.CompareAndSetMetadataKey(control.ID, beadmeta.ControlEpochMetadataKey,
			strconv.Itoa(expected), strconv.Itoa(attemptNum))
		if ok {
			return nil
		}
		if casErr != nil && !beads.IsPreconditionFailed(casErr) {
			return fmt.Errorf("syncing control epoch on %s: %w", control.ID, casErr)
		}
		refreshed, getErr := store.Get(control.ID)
		if getErr != nil {
			return getErr
		}
		refreshedEpoch, err := strconv.Atoi(strings.TrimSpace(refreshed.Metadata[beadmeta.ControlEpochMetadataKey]))
		if err != nil || refreshedEpoch >= attemptNum {
			return nil
		}
		expected = refreshedEpoch
	}
	return fmt.Errorf("syncing control epoch on %s: conditional advance kept conflicting below attempt %d", control.ID, attemptNum)
}

func markControllerSpawnError(store beads.Store, beadID string, err error, opts ProcessOptions) bool {
	metadata := map[string]string{
		beadmeta.ControllerErrorMetadataKey: err.Error(),
	}
	if IsTransientControllerError(err) && !isPartialAttemptAttachError(err) {
		metadata[beadmeta.ControllerErrorClassMetadataKey] = beadmeta.FailureClassTransient
		metadata[beadmeta.ControllerRetryableMetadataKey] = "true"
		if writeErr := store.SetMetadataBatch(beadID, metadata); writeErr != nil {
			opts.tracef("controller-spawn-error bead=%s recording transient failure metadata failed err=%v", beadID, writeErr)
		}
		return true
	}

	metadata[beadmeta.ControllerErrorClassMetadataKey] = beadmeta.FailureClassHard
	metadata[beadmeta.ControllerRetryableMetadataKey] = ""
	metadata[beadmeta.FinalDispositionMetadataKey] = beadmeta.DispositionControllerError
	if writeErr := store.SetMetadataBatch(beadID, metadata); writeErr != nil {
		opts.tracef("controller-spawn-error bead=%s recording hard failure metadata failed err=%v", beadID, writeErr)
	}
	if closeErr := setOutcomeAndClose(store, beadID, beadmeta.OutcomeFail); closeErr != nil {
		opts.tracef("controller-spawn-error bead=%s closing failed bead failed err=%v", beadID, closeErr)
	}
	// Reconcile any enclosing scope so a controller_error terminal closure
	// does not leave the scope body stalled.
	if _, scopeErr := reconcileClosedScopeMemberWithOptions(store, beadID, opts); scopeErr != nil {
		opts.tracef("controller-spawn-error bead=%s reconciling enclosing scope failed err=%v", beadID, scopeErr)
	}
	return false
}

func clearControllerSpawnErrorMetadata(metadata map[string]string) {
	metadata[beadmeta.ControllerErrorMetadataKey] = ""
	metadata[beadmeta.ControllerErrorClassMetadataKey] = ""
	metadata[beadmeta.ControllerRetryableMetadataKey] = ""
}

func isPartialAttemptAttachError(err error) bool {
	var partial *partialAttemptAttachError
	return errors.As(err, &partial)
}

var errTransientControllerBoundary = errors.New("transient controller boundary error")

func markTransientControllerBoundaryError(err error) error {
	if err == nil || errors.Is(err, errTransientControllerBoundary) {
		return err
	}
	return fmt.Errorf("%w: %w", errTransientControllerBoundary, err)
}

// IsTransientControllerError is the dispatch/store transient classifier for
// control spawn and spawn-state update boundaries. Prefer typed checks when
// callers expose them; the string fallback covers wrapped Dolt/MySQL/tmux
// messages that arrive through the bead store CLI boundary.
func IsTransientControllerError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	if errors.Is(err, errTransientControllerBoundary) {
		return true
	}
	// Conditional-write contention and capability loss are level-triggered
	// re-entry classes, never terminal dispositions: exhaustion means the
	// store could not get a clean shot (re-enter and retry), and a runtime
	// unsupported latch means the next resolve degrades (auto) or refuses
	// (require) — neither is a broken control. A require refusal
	// (ConditionalWritesRequiredError) is deliberately NOT here: it is a
	// persistent policy refusal and stays hard/fail-closed.
	if beads.IsCASRetriesExhausted(err) || beads.IsConditionalWriteUnsupported(err) {
		return true
	}
	msg := strings.ToLower(err.Error())
	if isTransientWorkQueryFailure(msg) {
		return true
	}
	transientNeedles := []string{
		"i/o timeout",
		"context deadline exceeded",
		"invalid connection",
		"connection refused",
		"connection reset by peer",
		"broken pipe",
		"bad connection",
		"server has gone away",
		"too many connections",
		"lock wait timeout",
		"deadlock found",
		"database is locked",
		"database table is locked",
		"sqlite_busy",
		// bd's client-side Dolt breaker fails fast while the server is down.
		// These errors are recoverable, so a long-running control dispatcher
		// must keep sweeping rather than exit permanently during the outage.
		"dolt circuit breaker is open",
		"server appears down, failing fast",
		"dolt server unreachable",
	}
	for _, needle := range transientNeedles {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	return false
}

func isTransientWorkQueryFailure(msg string) bool {
	if !strings.Contains(msg, "running work query") {
		return false
	}
	workQueryNeedles := []string{
		"signal: killed",
		"signal: terminated",
		"exit status 137",
		"exit status 143",
		"timed out after",
	}
	for _, needle := range workQueryNeedles {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	return false
}

func handleRetryExhaustion(store beads.Store, beadID string, attemptNum int, reason, onExhausted, attemptLog string) (ControlResult, error) {
	if onExhausted == beadmeta.DispositionSoftFail {
		closeMetadata := map[string]string{
			beadmeta.AttemptLogMetadataKey:       attemptLog,
			beadmeta.OutcomeMetadataKey:          beadmeta.OutcomePass,
			beadmeta.FailedAttemptMetadataKey:    strconv.Itoa(attemptNum),
			beadmeta.FailureClassMetadataKey:     beadmeta.FailureClassTransient,
			beadmeta.FailureReasonMetadataKey:    reason,
			beadmeta.FinalDispositionMetadataKey: beadmeta.DispositionSoftFail,
		}
		clearControllerSpawnErrorMetadata(closeMetadata)
		if err := updateMetadataAndClose(store, beadID, closeMetadata); err != nil {
			return ControlResult{}, fmt.Errorf("%s: closing soft-failed: %w", beadID, err)
		}
		return ControlResult{Processed: true, Action: "soft-fail"}, nil
	}

	closeMetadata := map[string]string{
		beadmeta.AttemptLogMetadataKey:       attemptLog,
		beadmeta.OutcomeMetadataKey:          beadmeta.OutcomeFail,
		beadmeta.FailedAttemptMetadataKey:    strconv.Itoa(attemptNum),
		beadmeta.FailureClassMetadataKey:     beadmeta.FailureClassTransient,
		beadmeta.FailureReasonMetadataKey:    reason,
		beadmeta.FinalDispositionMetadataKey: beadmeta.DispositionHardFail,
	}
	clearControllerSpawnErrorMetadata(closeMetadata)
	if err := updateMetadataAndClose(store, beadID, closeMetadata); err != nil {
		return ControlResult{}, fmt.Errorf("%s: closing exhausted: %w", beadID, err)
	}
	return ControlResult{Processed: true, Action: "fail"}, nil
}

// spawnNextAttempt deserializes the frozen step spec, builds an attempt recipe,
// and calls molecule.Attach to graft it onto the control bead.
func spawnNextAttempt(ctx context.Context, store beads.Store, control beads.Bead, attemptNum int, opts ProcessOptions) error {
	specJSON := control.Metadata[beadmeta.SourceStepSpecMetadataKey]
	if specJSON == "" {
		// New path: look up the spec bead.
		spec, err := findSpecBead(store, control)
		if err != nil {
			return fmt.Errorf("control bead %s: finding spec bead: %w", control.ID, err)
		}
		specJSON = spec.Description
	}

	var step formula.Step
	if err := json.Unmarshal([]byte(specJSON), &step); err != nil {
		return fmt.Errorf("deserializing step spec: %w", err)
	}

	recipe := buildAttemptRecipe(&step, control, attemptNum)

	// Attach bypasses graph compile routing, so spawned attempts need their
	// execution lane restored manually. Prefer each step's explicit target when
	// available, and only inherit the parent execution lane as a fallback.
	executionRoute := strings.TrimSpace(control.Metadata[beadmeta.ExecutionRoutedToMetadataKey])
	executionRigContext := strings.TrimSpace(control.Metadata[beadmeta.ExecutionRigContextMetadataKey])
	routeCfg, err := opts.routeConfig()
	if err != nil {
		return fmt.Errorf("loading attempt route config: %w", err)
	}
	rootStoreRef := strings.TrimSpace(control.Metadata[beadmeta.RootStoreRefMetadataKey])
	for i := range recipe.Steps {
		if recipe.Steps[i].Metadata[beadmeta.KindMetadataKey] == beadmeta.KindSpec {
			continue
		}
		if executionRigContext != "" && strings.TrimSpace(recipe.Steps[i].Metadata[beadmeta.ExecutionRigContextMetadataKey]) == "" {
			if recipe.Steps[i].Metadata == nil {
				recipe.Steps[i].Metadata = make(map[string]string)
			}
			recipe.Steps[i].Metadata[beadmeta.ExecutionRigContextMetadataKey] = executionRigContext
		}
		if rootStoreRef != "" {
			if recipe.Steps[i].Metadata == nil {
				recipe.Steps[i].Metadata = make(map[string]string)
			}
			// The parent graph owns attached attempts. Ignore stale fragment
			// metadata so routing and molecule.Attach's persisted store ref agree.
			recipe.Steps[i].Metadata[beadmeta.RootStoreRefMetadataKey] = rootStoreRef
		}
		target := strings.TrimSpace(recipe.Steps[i].Metadata[beadmeta.RunTargetMetadataKey])
		if target == "" {
			target = strings.TrimSpace(recipe.Steps[i].Metadata[beadmeta.RoutedToMetadataKey])
		}
		if target == "" {
			target = strings.TrimSpace(recipe.Steps[i].Assignee)
		}
		if target == "" {
			target = executionRoute
		} else {
			target = qualifyAttemptTargetWithSourceRoute(target, executionRoute, routeCfg)
		}
		if isAttemptControlKind(recipe.Steps[i].Metadata[beadmeta.KindMetadataKey]) {
			if err := applyAttemptControlStepRoute(&recipe.Steps[i], target, routeCfg, store); err != nil {
				return fmt.Errorf("routing attempt control step %s: %w", recipe.Steps[i].ID, err)
			}
			continue
		}
		if target == "" {
			continue
		}
		applyAttemptStepRoute(&recipe.Steps[i], target, routeCfg, store)
	}

	epoch := 0
	if raw := control.Metadata[beadmeta.ControlEpochMetadataKey]; raw != "" {
		epoch, _ = strconv.Atoi(raw)
	}

	result, err := molecule.Attach(ctx, store, recipe, control.ID, molecule.AttachOptions{
		IdempotencyKey: fmt.Sprintf("%s:attempt:%d", control.ID, attemptNum),
		ExpectedEpoch:  epoch,
	})
	if err != nil {
		// An epoch conflict is a ROUTINE convergence signal under the CAS-last
		// fence: another processor won this attempt, and the next
		// level-triggered pass re-enters and converges on the winner through
		// findExistingAttach. It must classify transient — the partial-attach
		// hard path below exists for genuinely broken (crash-partial)
		// attempts, and routing a normal fence loser there terminally closes
		// the shared control, making the promised convergence impossible.
		if errors.Is(err, molecule.ErrEpochConflict) {
			return markTransientControllerBoundaryError(fmt.Errorf("attach epoch conflict on %s attempt %d (fence lost; converging next pass): %w", control.ID, attemptNum, err))
		}
		failedRootID, lookupErr := failedAttemptAttachRootID(store, control, attemptNum)
		if lookupErr != nil {
			return &failedAttemptAttachLookupError{lookupErr: lookupErr, err: err}
		}
		if failedRootID != "" {
			return &partialAttemptAttachError{rootID: failedRootID, err: err}
		}
		return err
	}
	if err := closeAttachedSpecBeads(store, recipe, result); err != nil {
		return err
	}
	return nil
}

type partialAttemptAttachError struct {
	rootID string
	err    error
}

func (e *partialAttemptAttachError) Error() string {
	return fmt.Sprintf("partial attempt attach %s is marked molecule_failed: %v", e.rootID, e.err)
}

func (e *partialAttemptAttachError) Unwrap() error {
	return e.err
}

type failedAttemptAttachLookupError struct {
	lookupErr error
	err       error
}

func (e *failedAttemptAttachLookupError) Error() string {
	return fmt.Sprintf("checking failed attempt attach state: %v; original attach error: %v", e.lookupErr, e.err)
}

func (e *failedAttemptAttachLookupError) Unwrap() []error {
	return []error{e.lookupErr, e.err}
}

func failedAttemptAttachRootID(store beads.Store, control beads.Bead, attemptNum int) (string, error) {
	rootID := control.Metadata[beadmeta.RootBeadIDMetadataKey]
	if rootID == "" {
		rootID = control.ID
	}
	matches, err := beads.HandlesFor(store).Live.List(beads.ListQuery{
		Metadata: map[string]string{
			beadmeta.IdempotencyKeyMetadataKey: fmt.Sprintf("%s:attempt:%d", control.ID, attemptNum),
			beadmeta.RootBeadIDMetadataKey:     rootID,
			beadmeta.MoleculeFailedMetadataKey: "true",
		},
	})
	if err != nil {
		return "", err
	}
	if len(matches) == 0 {
		return "", nil
	}
	return matches[0].ID, nil
}

func qualifyAttemptTargetWithSourceRoute(target, sourceRoute string, cfg *config.City) string {
	target = strings.TrimSpace(target)
	if target == "" || strings.Contains(target, "/") || cfg == nil {
		return target
	}
	sourceRoute = strings.TrimSpace(sourceRoute)
	slash := strings.IndexByte(sourceRoute, '/')
	if slash <= 0 {
		return target
	}
	candidate := sourceRoute[:slash] + "/" + target
	if config.FindAgent(cfg, candidate) != nil || config.FindNamedSession(cfg, candidate) != nil {
		return candidate
	}
	return target
}

// buildAttemptRecipe constructs a minimal formula.Recipe for one attempt
// from the frozen step spec.
func buildAttemptRecipe(step *formula.Step, control beads.Bead, attemptNum int) *formula.Recipe {
	// stepID is the bare logical ID for metadata grouping.
	stepID := control.Metadata[beadmeta.StepIDMetadataKey]
	if stepID == "" {
		stepID = control.ID
	}
	// stepRef is the fully namespaced ref (e.g., mol-demo-v2.self-review)
	// so Attach-created beads match the same namespace as compiler-created ones.
	stepRef := control.Metadata[beadmeta.StepRefMetadataKey]
	if stepRef == "" {
		stepRef = stepID
	}

	var attemptPrefix string
	if step.Ralph != nil {
		attemptPrefix = fmt.Sprintf("%s.iteration.%d", stepRef, attemptNum)
	} else {
		attemptPrefix = fmt.Sprintf("%s.attempt.%d", stepRef, attemptNum)
	}

	// Root step for the attempt sub-DAG.
	// For ralph iterations with children, the root is a scope bead.
	// For simple retries, it's the work bead itself (no wrapper).
	rootKind := beadmeta.KindTask
	if step.Ralph != nil && len(step.Children) > 0 {
		rootKind = beadmeta.KindScope
	}
	rootMeta := make(map[string]string, len(step.Metadata))
	// Preserve formula-specified retry metadata such as required artifacts.
	for k, v := range step.Metadata {
		rootMeta[k] = v
	}
	rootMeta[beadmeta.KindMetadataKey] = rootKind
	rootMeta[beadmeta.AttemptMetadataKey] = strconv.Itoa(attemptNum)
	rootMeta[beadmeta.StepIDMetadataKey] = stepID
	rootMeta[beadmeta.StepRefMetadataKey] = attemptPrefix
	// gc.control_for is the durable lineage pointer back to the control bead.
	// Written AFTER the step.Metadata copy loop so a formula-authored value
	// cannot shadow it. control.ID is a real store bead ID for top-level mints
	// and the control's namespaced step ref for nested seeds
	// (buildNestedControlSeed) — both are covered by findLatestAttempt's
	// identity set.
	rootMeta[beadmeta.ControlForMetadataKey] = control.ID
	if step.OnComplete != nil {
		rootMeta[beadmeta.OutputJSONRequiredMetadataKey] = "true"
	}
	// Ralph iterations need scope metadata for grouping.
	if rootKind == beadmeta.KindScope {
		rootMeta[beadmeta.ScopeRoleMetadataKey] = beadmeta.ScopeRoleBody
		rootMeta[beadmeta.ScopeNameMetadataKey] = stepID
		rootMeta[beadmeta.RalphStepIDMetadataKey] = stepID
	}
	rootStep := formula.RecipeStep{
		ID:       attemptPrefix,
		Title:    step.Title,
		Type:     step.Type,
		IsRoot:   true,
		Labels:   append([]string{}, step.Labels...),
		Assignee: step.Assignee,
		Metadata: rootMeta,
	}
	if step.Type == "" {
		rootStep.Type = "task"
	}

	recipe := &formula.Recipe{
		Name:  attemptPrefix,
		Steps: []formula.RecipeStep{rootStep},
	}
	var fanoutSteps []formula.RecipeStep
	var fanoutDeps []formula.RecipeDep
	var nestedSeedSteps []formula.RecipeStep
	var nestedSeedDeps []formula.RecipeDep

	// For steps with children (scoped ralph), add children as sub-steps.
	// Children may have retry/ralph config — propagate their metadata
	// so the beads get the correct gc.kind for logical grouping.
	if len(step.Children) > 0 {
		// Collect top-level child IDs so the scope bead blocks on them.
		var topChildIDs []string
		for _, child := range step.Children {
			topChildIDs = append(topChildIDs, attemptPrefix+"."+child.ID)
		}
		// Wire scope → children: scope closes when all children close.
		for _, cid := range topChildIDs {
			recipe.Deps = append(recipe.Deps, formula.RecipeDep{
				StepID:      attemptPrefix,
				DependsOnID: cid,
				Type:        "blocks",
			})
		}

		for _, child := range step.Children {
			childID := attemptPrefix + "." + child.ID
			childMeta := map[string]string{
				beadmeta.AttemptMetadataKey:     strconv.Itoa(attemptNum),
				beadmeta.StepRefMetadataKey:     childID,
				beadmeta.StepIDMetadataKey:      child.ID,
				beadmeta.ScopeRefMetadataKey:    attemptPrefix,
				beadmeta.RalphStepIDMetadataKey: stepID,
				beadmeta.ScopeRoleMetadataKey:   beadmeta.ScopeRoleMember,
				beadmeta.OnFailMetadataKey:      "abort_scope",
			}
			// Copy formula-defined metadata from the child step.
			for k, v := range child.Metadata {
				if _, exists := childMeta[k]; !exists {
					childMeta[k] = v
				}
			}
			if child.OnComplete != nil {
				childMeta[beadmeta.OutputJSONRequiredMetadataKey] = "true"
			}
			// Derive gc.kind and control metadata from retry/ralph config.
			if child.Retry != nil {
				childMeta[beadmeta.KindMetadataKey] = beadmeta.KindRetry
				childMeta[beadmeta.MaxAttemptsMetadataKey] = strconv.Itoa(child.Retry.MaxAttempts)
				childMeta[beadmeta.ControlEpochMetadataKey] = "1"
				if child.Retry.OnExhausted != "" {
					childMeta[beadmeta.OnExhaustedMetadataKey] = child.Retry.OnExhausted
				} else {
					childMeta[beadmeta.OnExhaustedMetadataKey] = beadmeta.DispositionHardFail
				}
				// Emit a spec bead for the nested retry so it can spawn
				// its own attempts without oversized metadata.
				if step := newSpecRecipeStep(childID, child); step != nil {
					recipe.Steps = append(recipe.Steps, *step)
				}
			}
			if child.Ralph != nil {
				childMeta[beadmeta.KindMetadataKey] = beadmeta.KindRalph
				childMeta[beadmeta.MaxAttemptsMetadataKey] = strconv.Itoa(child.Ralph.MaxAttempts)
				childMeta[beadmeta.ControlEpochMetadataKey] = "1"
				if child.Ralph.Check != nil {
					childMeta[beadmeta.CheckModeMetadataKey] = child.Ralph.Check.Mode
					childMeta[beadmeta.CheckPathMetadataKey] = child.Ralph.Check.Path
					childMeta[beadmeta.CheckTimeoutMetadataKey] = child.Ralph.Check.Timeout
					if child.Timeout != "" {
						childMeta[beadmeta.StepTimeoutMetadataKey] = child.Timeout
					}
				}
				if step := newSpecRecipeStep(childID, child); step != nil {
					recipe.Steps = append(recipe.Steps, *step)
				}
				// Seed the nested ralph's first iteration. At compile time
				// expandNestedRalph seeds iteration.1; the re-spawn path must do
				// the same so the inner ralph control finds a valid iteration on
				// every outer iteration, not just the first. Without this seed,
				// processRalphControl's findLatestAttempt returns empty and fatals
				// ("no iteration found"), crash-looping all dispatch for the rig.
				// See gastownhall/gascity#2798.
				seedSteps, seedDeps := buildNestedControlSeed(child, childID)
				nestedSeedSteps = append(nestedSeedSteps, seedSteps...)
				nestedSeedDeps = append(nestedSeedDeps, seedDeps...)
			}
			// Drain children are themselves control beads: re-apply the
			// compiler's drain contract (gc.kind=drain + gc.drain_* keys) so
			// re-spawned iterations keep the shape minted by flattenSteps.
			// Validation forbids combining drain with retry/ralph, so this
			// never overwrites the nested-control kinds above.
			formula.ApplyDrainControlMetadata(childMeta, child.Drain)
			childStep := formula.RecipeStep{
				ID:          childID,
				Title:       child.Title,
				Description: child.Description,
				Type:        child.Type,
				Labels:      append([]string{}, child.Labels...),
				Assignee:    child.Assignee,
				Metadata:    childMeta,
			}
			if childStep.Type == "" {
				childStep.Type = "task"
			}
			recipe.Steps = append(recipe.Steps, childStep)
			if fanoutStep, fanoutDep, ok := buildAttemptRecipeFanoutControl(childStep, child.OnComplete); ok {
				fanoutSteps = append(fanoutSteps, fanoutStep)
				fanoutDeps = append(fanoutDeps, fanoutDep)
			}
			// No parent-child dep to the iteration scope — it creates a
			// deadlock (scope waits for children, children wait for scope).
			// Children are associated with the iteration via gc.scope_ref
			// metadata, and their execution order comes from blocks deps.

			// Wire inter-child deps.
			for _, need := range child.Needs {
				needID := attemptPrefix + "." + need
				recipe.Deps = append(recipe.Deps, formula.RecipeDep{
					StepID:      childID,
					DependsOnID: needID,
					Type:        "blocks",
				})
			}
		}
	}

	applyAttemptRecipeScopeChecks(recipe)
	recipe.Steps = append(recipe.Steps, fanoutSteps...)
	recipe.Deps = append(recipe.Deps, fanoutDeps...)
	// Nested-control seed steps are appended after the outer scope-check pass so
	// their own scope-checks (already applied by the recursive buildAttemptRecipe
	// call) are not double-processed against the outer iteration scope.
	recipe.Steps = append(recipe.Steps, nestedSeedSteps...)
	recipe.Deps = append(recipe.Deps, nestedSeedDeps...)

	return recipe
}

// buildNestedControlSeed builds the first-iteration sub-DAG for a nested ralph
// control re-created during an outer ralph re-spawn. It mirrors the compile-time
// seeding performed by expandNestedRalph so the inner control starts in a valid
// state on every outer iteration. childID is the fully namespaced ID of the inner
// control bead (for example "mol.outer.iteration.2.inner"). The returned steps and
// deps must be merged after the caller's scope-check pass, since the seed already
// carries its own scope-checks. See gastownhall/gascity#2798.
func buildNestedControlSeed(child *formula.Step, childID string) ([]formula.RecipeStep, []formula.RecipeDep) {
	synthetic := beads.Bead{
		ID: childID,
		Metadata: map[string]string{
			beadmeta.StepIDMetadataKey:  child.ID,
			beadmeta.StepRefMetadataKey: childID,
		},
	}
	seed := buildAttemptRecipe(child, synthetic, 1)
	// buildAttemptRecipe marks the seed's root step with IsRoot=true, but once
	// these steps are merged into the outer attempt recipe they are no longer
	// roots — the outer recipe already owns its root at Steps[0]. molecule.Attach
	// applies the attach-root overrides (Type="molecule", Ref, ParentID) to ANY
	// IsRoot step and maps it as an attach root, so a leftover IsRoot on the
	// nested seed would corrupt the iteration bead's type/ref/parent and break
	// dependency wiring. Clear it on every returned seed step. RootStep() below
	// returns Steps[0] regardless of the flag, so the blocks dep wiring is
	// unaffected. See gastownhall/gascity#2798.
	for i := range seed.Steps {
		seed.Steps[i].IsRoot = false
	}
	deps := append([]formula.RecipeDep{}, seed.Deps...)
	if root := seed.RootStep(); root != nil {
		// The inner control blocks on its first iteration, exactly as the
		// compile-time control.Needs wiring does.
		deps = append(deps, formula.RecipeDep{
			StepID:      childID,
			DependsOnID: root.ID,
			Type:        "blocks",
		})
	}
	return seed.Steps, deps
}

func buildAttemptRecipeFanoutControl(source formula.RecipeStep, onComplete *formula.OnCompleteSpec) (formula.RecipeStep, formula.RecipeDep, bool) {
	if onComplete == nil {
		return formula.RecipeStep{}, formula.RecipeDep{}, false
	}
	sourceRef := source.Metadata[beadmeta.StepRefMetadataKey]
	if sourceRef == "" {
		sourceRef = source.ID
	}
	meta := map[string]string{
		beadmeta.KindMetadataKey:       beadmeta.KindFanout,
		beadmeta.ControlForMetadataKey: sourceRef,
		beadmeta.ForEachMetadataKey:    onComplete.ForEach,
		beadmeta.BondMetadataKey:       onComplete.Bond,
		beadmeta.FanoutModeMetadataKey: beadmeta.FanoutModeParallel,
	}
	if onComplete.Sequential {
		meta[beadmeta.FanoutModeMetadataKey] = beadmeta.FanoutModeSequential
	}
	if len(onComplete.Vars) > 0 {
		if data, err := json.Marshal(onComplete.Vars); err == nil {
			meta[beadmeta.BondVarsMetadataKey] = string(data)
		}
	}
	for _, key := range []string{beadmeta.ScopeRefMetadataKey, beadmeta.OnFailMetadataKey, beadmeta.StepIDMetadataKey, beadmeta.RalphStepIDMetadataKey, beadmeta.AttemptMetadataKey} {
		if value := source.Metadata[key]; value != "" {
			meta[key] = value
		}
	}
	// Control infrastructure is never a scope member: stamp the control role
	// explicitly (mirroring minted scope-checks) instead of inheriting the
	// host step's role (see the identical stamp in formula's applyGraphControls).
	if meta[beadmeta.ScopeRefMetadataKey] != "" {
		meta[beadmeta.ScopeRoleMetadataKey] = beadmeta.ScopeRoleControl
	}
	control := formula.RecipeStep{
		ID:       source.ID + "-fanout",
		Title:    "Expand fanout for " + source.Title,
		Type:     "task",
		Metadata: meta,
	}
	dep := formula.RecipeDep{
		StepID:      control.ID,
		DependsOnID: source.ID,
		Type:        "blocks",
	}
	return control, dep, true
}

// applyAttemptRecipeScopeChecks re-mints paired scope-check controls for the
// scoped steps of a re-spawned attempt recipe, mirroring the compile-time
// shape injected by formula.ApplyGraphControls: each scope-check blocks on
// its subject step, and deps that waited on the subject are rewritten to
// wait on the scope-check instead.
func applyAttemptRecipeScopeChecks(recipe *formula.Recipe) {
	if recipe == nil || len(recipe.Steps) == 0 {
		return
	}

	existingStepIDs := make(map[string]struct{}, len(recipe.Steps))
	replacements := make(map[string]string)
	controls := make([]formula.RecipeStep, 0)
	controlDeps := make([]formula.RecipeDep, 0)
	for _, step := range recipe.Steps {
		existingStepIDs[step.ID] = struct{}{}
	}

	for _, step := range recipe.Steps {
		if !attemptRecipeStepNeedsScopeCheck(step) {
			continue
		}
		controlID := step.ID + "-scope-check"
		if _, exists := existingStepIDs[controlID]; exists {
			continue
		}

		replacements[step.ID] = controlID
		meta := map[string]string{
			beadmeta.KindMetadataKey:       beadmeta.KindScopeCheck,
			beadmeta.ScopeRefMetadataKey:   step.Metadata[beadmeta.ScopeRefMetadataKey],
			beadmeta.ScopeRoleMetadataKey:  beadmeta.ScopeRoleControl,
			beadmeta.ControlForMetadataKey: step.ID,
		}
		for _, key := range []string{beadmeta.StepIDMetadataKey, beadmeta.RalphStepIDMetadataKey, beadmeta.AttemptMetadataKey, beadmeta.OnFailMetadataKey} {
			if value := step.Metadata[key]; value != "" {
				meta[key] = value
			}
		}
		controls = append(controls, formula.RecipeStep{
			ID:       controlID,
			Title:    "Finalize scope for " + step.Title,
			Type:     "task",
			Metadata: meta,
		})
		controlDeps = append(controlDeps, formula.RecipeDep{
			StepID:      controlID,
			DependsOnID: step.ID,
			Type:        "blocks",
		})
	}

	if len(controls) == 0 {
		return
	}

	for i := range recipe.Deps {
		if replacement, ok := replacements[recipe.Deps[i].DependsOnID]; ok {
			recipe.Deps[i].DependsOnID = replacement
		}
	}
	recipe.Steps = append(recipe.Steps, controls...)
	recipe.Deps = append(recipe.Deps, controlDeps...)
}

func attemptRecipeStepNeedsScopeCheck(step formula.RecipeStep) bool {
	if step.Metadata[beadmeta.ScopeRefMetadataKey] == "" {
		return false
	}
	if step.Metadata[beadmeta.ScopeRoleMetadataKey] == beadmeta.ScopeRoleTeardown {
		return false
	}
	return !beadmeta.IsScopeCheckExemptKind(step.Metadata[beadmeta.KindMetadataKey])
}

// loadAttemptRouteConfigE loads the city.toml used for attempt-time routing.
// An empty cityPath yields (nil, nil) — routing legitimately runs metadata-only
// when no city config is present. A genuine parse failure is returned rather
// than swallowed so callers (via ProcessOptions.routeConfig) can surface it.
func loadAttemptRouteConfigE(cityPath string) (*config.City, error) {
	if strings.TrimSpace(cityPath) == "" {
		return nil, nil
	}
	cfg, _, err := config.LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		return nil, fmt.Errorf("loading attempt-route config from %s: %w", cityPath, err)
	}
	return cfg, nil
}

func applyAttemptStepRoute(step *formula.RecipeStep, target string, cfg *config.City, store beads.Store) {
	if step.Metadata == nil {
		step.Metadata = make(map[string]string)
	}
	if binding, ok := resolveAttemptRouteBinding(target, cfg, store); ok {
		if binding.directSessionID != "" {
			delete(step.Metadata, beadmeta.RoutedToMetadataKey)
			delete(step.Metadata, beadmeta.ExecutionRoutedToMetadataKey)
			step.Labels = removeAttemptPoolLabels(step.Labels)
			step.Assignee = binding.directSessionID
			return
		}
		if binding.qualifiedName != "" {
			step.Metadata[beadmeta.RoutedToMetadataKey] = binding.qualifiedName
			step.Metadata[beadmeta.ExecutionRoutedToMetadataKey] = binding.qualifiedName
		} else {
			delete(step.Metadata, beadmeta.RoutedToMetadataKey)
			delete(step.Metadata, beadmeta.ExecutionRoutedToMetadataKey)
		}
		step.Labels = removeAttemptPoolLabels(step.Labels)
		if binding.metadataOnly {
			step.Assignee = ""
			return
		}
		step.Assignee = binding.sessionName
		return
	}

	// Target not found in config — route via metadata only and clear assignee
	// to avoid stale routing. Work discovery relies on gc.routed_to (tier 3).
	step.Metadata[beadmeta.RoutedToMetadataKey] = target
	step.Metadata[beadmeta.ExecutionRoutedToMetadataKey] = target
	step.Labels = removeAttemptPoolLabels(step.Labels)
	step.Assignee = ""
}

func applyAttemptControlStepRoute(step *formula.RecipeStep, executionTarget string, cfg *config.City, store beads.Store) error {
	if step.Metadata == nil {
		step.Metadata = make(map[string]string)
	}
	resolvedExecutionTarget := strings.TrimSpace(executionTarget)
	rigContext := strings.TrimSpace(step.Metadata[beadmeta.ExecutionRigContextMetadataKey])
	scopeKnown := rigContext != ""
	if storeRigContext, scoped := storeref.ScopeRigContext(step.Metadata[beadmeta.RootStoreRefMetadataKey]); scoped {
		rigContext = storeRigContext
		scopeKnown = true
	}
	if binding, ok := resolveAttemptRouteBinding(executionTarget, cfg, store); ok {
		switch {
		case binding.qualifiedName != "":
			resolvedExecutionTarget = binding.qualifiedName
			step.Metadata[beadmeta.ExecutionRoutedToMetadataKey] = binding.qualifiedName
		case executionTarget != "":
			// Direct session delivery still executes via the named/session target,
			// but control beads themselves must remain on control-dispatcher.
			step.Metadata[beadmeta.ExecutionRoutedToMetadataKey] = executionTarget
		default:
			delete(step.Metadata, beadmeta.ExecutionRoutedToMetadataKey)
		}
	} else if executionTarget != "" {
		step.Metadata[beadmeta.ExecutionRoutedToMetadataKey] = executionTarget
	} else {
		delete(step.Metadata, beadmeta.ExecutionRoutedToMetadataKey)
	}
	step.Labels = removeAttemptPoolLabels(step.Labels)

	controlTarget, err := controlDispatcherTargetForExecutionTarget(resolvedExecutionTarget, rigContext, scopeKnown, cfg)
	if err != nil {
		delete(step.Metadata, beadmeta.RoutedToMetadataKey)
		step.Assignee = ""
		return err
	}
	step.Metadata[beadmeta.RoutedToMetadataKey] = controlTarget
	step.Assignee = ""
	return nil
}

func controlDispatcherTargetForExecutionTarget(executionTarget, rigContext string, scopeKnown bool, cfg *config.City) (string, error) {
	executionTarget = strings.TrimSpace(executionTarget)
	rigContext = strings.TrimSpace(rigContext)
	if !scopeKnown {
		if slash := strings.IndexByte(executionTarget, '/'); slash > 0 {
			rigContext = executionTarget[:slash]
		}
	}
	// Select the deterministic dispatcher in the same scope as the graph store.
	// This keeps attempt-time control re-routing in lockstep with graph.v2
	// decoration and with the dispatcher's store-scoped claim loop.
	if agentCfg, ok := config.ControlDispatcherForScope(cfg, rigContext); ok {
		return agentCfg.QualifiedName(), nil
	}
	if rigContext != "" {
		return "", fmt.Errorf("control-dispatcher agent for rig %q not found", rigContext)
	}
	return "", fmt.Errorf("city control-dispatcher agent %q not found", config.ControlDispatcherAgentName)
}

// isAttemptControlKind reports whether an Attach-path recipe step should be
// routed to the control dispatcher rather than a worker: exactly the kinds
// the dispatcher's ProcessControl switch executes (beadmeta.ControlKinds).
// Pinned to the authoritative set by TestIsAttemptControlKindMatchesControlKinds.
func isAttemptControlKind(kind string) bool {
	return beadmeta.IsControlKind(kind)
}

// latestAttemptCandidateIsControlInfrastructure reports whether a bead kind
// is control infrastructure (never selectable as the latest-attempt work
// bead): every control kind plus the workflow topology root. Scope beads are
// deliberately NOT included — for ralph controls, scope beads ARE the
// iterations, and the caller handles that case.
func latestAttemptCandidateIsControlInfrastructure(kind string) bool {
	return beadmeta.IsControlKind(kind) || kind == beadmeta.KindWorkflow
}

type attemptRouteBinding struct {
	qualifiedName   string
	metadataOnly    bool
	sessionName     string
	directSessionID string
}

func resolveAttemptRouteBinding(target string, cfg *config.City, store beads.Store) (attemptRouteBinding, bool) {
	if strings.TrimSpace(target) == "" {
		return attemptRouteBinding{}, false
	}
	if cfg != nil {
		if named := config.FindNamedSession(cfg, target); named != nil {
			if spec, ok := session.FindNamedSessionSpec(cfg, cfg.EffectiveCityName(), named.QualifiedName()); ok {
				if store != nil {
					if candidates, err := session.NamedSessionResolutionCandidates(store, spec); err == nil {
						if bead, found := session.FindCanonicalNamedSessionBead(candidates, spec); found {
							return attemptRouteBinding{directSessionID: bead.ID}, true
						}
					}
				}
			}
			return attemptRouteBinding{
				qualifiedName: named.QualifiedName(),
				metadataOnly:  true,
			}, true
		}

		if agentCfg := config.FindAgent(cfg, target); agentCfg != nil {
			binding := attemptRouteBinding{qualifiedName: agentCfg.QualifiedName()}
			if isAttemptMultiSessionTarget(agentCfg.QualifiedName(), cfg) {
				binding.metadataOnly = true
				return binding, true
			}
			binding.sessionName = config.NamedSessionRuntimeName(cfg.EffectiveCityName(), cfg.Workspace, agentCfg.QualifiedName())
			return binding, true
		}
	}
	if store != nil {
		if id, err := session.ResolveSessionID(store, target); err == nil {
			return attemptRouteBinding{directSessionID: id}, true
		}
	}

	return attemptRouteBinding{}, false
}

func routedAttemptTarget(bead beads.Bead) string {
	if bead.Metadata == nil {
		return ""
	}
	if target := strings.TrimSpace(bead.Metadata[beadmeta.ExecutionRoutedToMetadataKey]); target != "" {
		return target
	}
	return strings.TrimSpace(bead.Metadata[beadmeta.RoutedToMetadataKey])
}

func isAttemptMultiSessionTarget(target string, cfg *config.City) bool {
	if cfg == nil || strings.TrimSpace(target) == "" {
		return false
	}
	agentCfg := config.FindAgent(cfg, target)
	return agentCfg != nil && agentCfg.SupportsInstanceExpansion()
}

func beadUsesMetadataPoolRouteWithConfig(bead beads.Bead, cfg *config.City) bool {
	if isAttemptMultiSessionTarget(routedAttemptTarget(bead), cfg) {
		return true
	}
	// Legacy fallback: check pool labels on the bead. This function is always
	// called on the previous attempt's bead (which retains its original labels),
	// not on the newly cloned bead (which has pool labels stripped).
	for _, label := range bead.Labels {
		if strings.HasPrefix(label, "pool:") {
			return true
		}
	}
	return false
}

func removeAttemptPoolLabels(labels []string) []string {
	if len(labels) == 0 {
		return labels
	}
	out := make([]string, 0, len(labels))
	for _, label := range labels {
		if strings.HasPrefix(label, "pool:") {
			continue
		}
		out = append(out, label)
	}
	return out
}

// findSpecBead locates the spec bead for a control (retry/ralph) bead.
// The spec bead has gc.kind=spec and gc.spec_for matching the control's
// step ID, under the same workflow root.
func findSpecBead(store beads.Store, control beads.Bead) (beads.Bead, error) {
	rootID := control.Metadata[beadmeta.RootBeadIDMetadataKey]
	if rootID == "" {
		return beads.Bead{}, fmt.Errorf("missing gc.root_bead_id")
	}
	stepID := control.Metadata[beadmeta.StepIDMetadataKey]
	if stepID == "" {
		stepID = control.Metadata[beadmeta.StepRefMetadataKey]
	}
	if stepID == "" {
		return beads.Bead{}, fmt.Errorf("missing gc.step_id")
	}
	stepRef := control.Metadata[beadmeta.StepRefMetadataKey]

	all, err := listByWorkflowRoot(store, rootID)
	if err != nil {
		return beads.Bead{}, err
	}
	for _, b := range all {
		if b.Metadata[beadmeta.KindMetadataKey] != beadmeta.KindSpec {
			continue
		}
		if stepRef != "" && b.Metadata[beadmeta.SpecForRefMetadataKey] == stepRef {
			return b, nil
		}
	}
	for _, b := range all {
		if b.Metadata[beadmeta.KindMetadataKey] == beadmeta.KindSpec && b.Metadata[beadmeta.SpecForMetadataKey] == stepID {
			return b, nil
		}
	}
	return beads.Bead{}, fmt.Errorf("no spec bead found for step %q under root %s", stepID, rootID)
}

// newSpecRecipeStep builds a spec recipe step for a nested retry/ralph child.
// Returns nil if marshaling fails.
func newSpecRecipeStep(childID string, child *formula.Step) *formula.RecipeStep {
	specJSON, err := json.Marshal(child)
	if err != nil {
		return nil
	}
	return &formula.RecipeStep{
		ID:          childID + ".spec",
		Title:       "Step spec for " + child.Title,
		Type:        "spec",
		Description: string(specJSON),
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:       beadmeta.KindSpec,
			beadmeta.SpecForMetadataKey:    child.ID,
			beadmeta.SpecForRefMetadataKey: childID,
		},
	}
}

func closeAttachedSpecBeads(store beads.Store, recipe *formula.Recipe, result *molecule.AttachResult) error {
	if recipe == nil || len(recipe.Steps) == 0 || result == nil {
		return nil
	}
	var fallbackRefs []string
	for _, step := range recipe.Steps {
		if step.Metadata[beadmeta.KindMetadataKey] != beadmeta.KindSpec {
			continue
		}
		beadID := result.IDMapping[step.ID]
		if beadID != "" {
			if err := setOutcomeAndClose(store, beadID, beadmeta.OutcomePass); err != nil {
				return fmt.Errorf("closing spec bead %s: %w", beadID, err)
			}
			continue
		}
		if ref := recipeStepRef(step); ref != "" {
			fallbackRefs = append(fallbackRefs, ref)
		}
	}
	if len(fallbackRefs) > 0 && result.WorkflowRootID != "" {
		if err := closeSpecBeadsByRefs(store, result.WorkflowRootID, fallbackRefs); err != nil {
			return err
		}
	}
	return nil
}

func closeGeneratedSpecBeadsForAttempt(store beads.Store, control, attempt beads.Bead) error {
	attemptRef := strings.TrimSpace(attempt.Metadata[beadmeta.StepRefMetadataKey])
	if attemptRef == "" {
		attemptRef = strings.TrimSpace(attempt.Ref)
	}
	if attemptRef == "" {
		return nil
	}
	rootID := control.Metadata[beadmeta.RootBeadIDMetadataKey]
	if rootID == "" {
		rootID = control.ID
	}
	all, err := listByWorkflowRoot(store, rootID)
	if err != nil {
		return err
	}
	for _, bead := range all {
		if bead.Status == "closed" || bead.Metadata[beadmeta.KindMetadataKey] != beadmeta.KindSpec {
			continue
		}
		ref := strings.TrimSpace(bead.Metadata[beadmeta.StepRefMetadataKey])
		if ref == "" {
			ref = strings.TrimSpace(bead.Ref)
		}
		if !strings.HasPrefix(ref, attemptRef+".") {
			continue
		}
		if err := setOutcomeAndClose(store, bead.ID, beadmeta.OutcomePass); err != nil {
			return fmt.Errorf("closing spec bead %s: %w", bead.ID, err)
		}
	}
	return nil
}

func closeSpecBeadsByRefs(store beads.Store, rootID string, refs []string) error {
	wanted := make(map[string]struct{}, len(refs))
	for _, ref := range refs {
		if ref = strings.TrimSpace(ref); ref != "" {
			wanted[ref] = struct{}{}
		}
	}
	if len(wanted) == 0 {
		return nil
	}
	all, err := listByWorkflowRoot(store, rootID)
	if err != nil {
		return err
	}
	for _, bead := range all {
		if bead.Status == "closed" || bead.Metadata[beadmeta.KindMetadataKey] != beadmeta.KindSpec {
			continue
		}
		ref := strings.TrimSpace(bead.Metadata[beadmeta.StepRefMetadataKey])
		if ref == "" {
			ref = strings.TrimSpace(bead.Ref)
		}
		if _, ok := wanted[ref]; !ok {
			continue
		}
		if err := setOutcomeAndClose(store, bead.ID, beadmeta.OutcomePass); err != nil {
			return fmt.Errorf("closing spec bead %s: %w", bead.ID, err)
		}
	}
	return nil
}

func recipeStepRef(step formula.RecipeStep) string {
	if ref := strings.TrimSpace(step.Metadata[beadmeta.StepRefMetadataKey]); ref != "" {
		return ref
	}
	return strings.TrimSpace(step.ID)
}

func isFailedPartialMolecule(bead beads.Bead) bool {
	return strings.TrimSpace(bead.Metadata[beadmeta.MoleculeFailedMetadataKey]) == "true"
}

// findLatestAttempt finds the most recent attempt/iteration child of a control
// bead. It lists beads under the workflow root and, on empty result, walks the
// control's blocks-dependencies; both feed latestAttemptFromCandidates, which
// matches the durable gc.control_for lineage stamp (with a legacy ref-string
// fallback for pre-S38 molecules) and returns the max gc.attempt.
func findLatestAttempt(store beads.Store, control beads.Bead) (beads.Bead, error) {
	rootID := control.Metadata[beadmeta.RootBeadIDMetadataKey]
	if rootID == "" {
		rootID = control.ID
	}

	all, err := listByWorkflowRoot(store, rootID)
	if err == nil {
		latest := latestAttemptFromCandidates(control, all)
		if latest.ID != "" {
			return latest, nil
		}
	}

	latest, depErr := latestAttemptFromDependencies(store, control)
	if depErr != nil {
		if err != nil {
			return beads.Bead{}, fmt.Errorf("%w; dependency fallback: %w", err, depErr)
		}
		return beads.Bead{}, depErr
	}
	if latest.ID != "" {
		return latest, nil
	}
	if err != nil {
		return beads.Bead{}, err
	}
	return beads.Bead{}, nil
}

func latestAttemptFromDependencies(store beads.Store, control beads.Bead) (beads.Bead, error) {
	deps, err := store.DepList(control.ID, "down")
	if err != nil {
		return beads.Bead{}, err
	}
	candidates := make([]beads.Bead, 0, len(deps))
	for _, dep := range deps {
		candidate, err := store.Get(dep.DependsOnID)
		if err != nil {
			return beads.Bead{}, err
		}
		candidates = append(candidates, candidate)
	}
	return latestAttemptFromCandidates(control, candidates), nil
}

// latestAttemptFromCandidates selects the control's latest attempt/iteration
// root among candidates.
//
// Primary path (S38): match the durable gc.control_for lineage stamp against
// the control's identity set — one string equality plus an integer max, no ref
// parsing. Every attempt/iteration root minted since S38 carries this stamp
// (buildAttemptRecipe and the compile-time first-attempt seeds). When no
// candidate carries a matching stamp (in-flight molecules minted before S38),
// it falls back to the deprecated ref-string cascade.
func latestAttemptFromCandidates(control beads.Bead, candidates []beads.Bead) beads.Bead {
	identity := controlIdentitySet(control)

	var latest beads.Bead
	latestAttempt := 0
	for _, b := range candidates {
		if isFailedPartialMolecule(b) {
			continue
		}
		// Skip beads that are control infrastructure, not actual work. On the
		// primary path only this control's own attempt roots carry its identity,
		// so no scope-unless-ralph skip is needed (see legacy fallback).
		if latestAttemptCandidateIsControlInfrastructure(b.Metadata[beadmeta.KindMetadataKey]) {
			continue
		}
		cf := strings.TrimSpace(b.Metadata[beadmeta.ControlForMetadataKey])
		if cf == "" || !identity[cf] {
			continue
		}
		attemptNum, _ := strconv.Atoi(b.Metadata[beadmeta.AttemptMetadataKey])
		if attemptNum > latestAttempt {
			latestAttempt = attemptNum
			latest = b
		}
	}
	if latest.ID != "" {
		return latest
	}
	return latestAttemptFromCandidatesLegacyRefSurgery(control, candidates)
}

// controlIdentitySet returns the non-empty members of the control's identity:
// its store bead ID plus its namespaced step ref and bare step id. A
// gc.control_for stamp equal to any member points at this control (bead-ID
// stamps come from runtime top-level mints; step-ref/step-id stamps come from
// compile-time and nested seeds — see S38).
func controlIdentitySet(control beads.Bead) map[string]bool {
	identity := make(map[string]bool, 3)
	for _, v := range []string{
		control.ID,
		control.Metadata[beadmeta.StepRefMetadataKey],
		control.Metadata[beadmeta.StepIDMetadataKey],
	} {
		if v = strings.TrimSpace(v); v != "" {
			identity[v] = true
		}
	}
	return identity
}

// legacyAttemptLineageHits counts attempt-lineage recoveries served by the
// deprecated pre-S38 ref-string cascade rather than the gc.control_for stamp.
// It is an in-process test hook, not a production operator surface: the
// deletion gate for the legacy cascade (S38 Phase 4) is enforced by the
// shadow-parity tests proving the primary stamp path subsumes the cascade,
// with this counter asserted to stay at zero over post-S38 candidate shapes.
// Package-level counter (not an event type) per the S38 trace-observability
// note; wire it to a trace/metric before relying on it in production.
var legacyAttemptLineageHits int64

// legacyAttemptLineageHitCount reports the number of attempt-lineage recoveries
// served by the deprecated ref-string cascade. In-process test hook.
func legacyAttemptLineageHitCount() int64 {
	return atomic.LoadInt64(&legacyAttemptLineageHits)
}

// latestAttemptFromCandidatesLegacyRefSurgery recovers attempt lineage by
// parsing dotted step refs through a four-stage cascade.
//
// Deprecated: remove after the release following S38 — serves only molecules
// minted before the gc.control_for stamp existed. New attempts resolve on the
// primary equality path in latestAttemptFromCandidates.
func latestAttemptFromCandidatesLegacyRefSurgery(control beads.Bead, candidates []beads.Bead) beads.Bead {
	controlRef := control.Metadata[beadmeta.StepRefMetadataKey]
	if controlRef == "" {
		controlRef = control.ID
	}

	var latest beads.Bead
	latestAttempt := 0
	controlKind := control.Metadata[beadmeta.KindMetadataKey]
	for _, b := range candidates {
		if isFailedPartialMolecule(b) {
			continue
		}
		// Skip beads that are control infrastructure, not actual work.
		// For ralph controls, scope beads ARE the iterations — don't skip them.
		kind := b.Metadata[beadmeta.KindMetadataKey]
		if latestAttemptCandidateIsControlInfrastructure(kind) {
			continue
		}
		if kind == beadmeta.KindScope && controlKind != "ralph" {
			continue
		}

		ref := b.Metadata[beadmeta.StepRefMetadataKey]
		if ref == "" {
			continue
		}

		// Match: attempt ref starts with the control's ref + ".attempt." or ".iteration."
		isAttempt := strings.HasPrefix(ref, controlRef+".attempt.") ||
			strings.HasPrefix(ref, controlRef+".iteration.")
		// Also match by step_id (ralph parent ID).
		stepID := control.Metadata[beadmeta.StepIDMetadataKey]
		if !isAttempt && stepID != "" {
			isAttempt = strings.HasPrefix(ref, stepID+".attempt.") ||
				strings.HasPrefix(ref, stepID+".iteration.")
		}
		// Also match short refs from nested retries inside ralphs where the
		// step_ref is the bare child ID + ".attempt.N" (not fully namespaced).
		// Try progressively shorter suffixes of the control's step_ref.
		if !isAttempt {
			// First: extract after ".iteration.N." for compose.expand children
			// whose short refs include multi-segment IDs (e.g., "review-pipeline.review-codex").
			for _, marker := range []string{".iteration.", ".attempt."} {
				if idx := strings.LastIndex(controlRef, marker); idx >= 0 {
					rest := controlRef[idx+len(marker):]
					if dotIdx := strings.IndexByte(rest, '.'); dotIdx >= 0 {
						childRef := rest[dotIdx+1:]
						if childRef != "" {
							isAttempt = strings.HasPrefix(ref, childRef+".attempt.") ||
								strings.HasPrefix(ref, childRef+".iteration.")
						}
					}
				}
				if isAttempt {
					break
				}
			}
		}
		// Fallback: last dot segment (handles single-segment child IDs).
		if !isAttempt {
			if lastDot := strings.LastIndex(controlRef, "."); lastDot >= 0 {
				shortRef := controlRef[lastDot+1:]
				isAttempt = strings.HasPrefix(ref, shortRef+".attempt.") ||
					strings.HasPrefix(ref, shortRef+".iteration.")
			}
		}
		if !isAttempt {
			continue
		}

		attemptNum, _ := strconv.Atoi(b.Metadata[beadmeta.AttemptMetadataKey])
		if attemptNum > latestAttempt {
			latestAttempt = attemptNum
			latest = b
		}
	}
	if latest.ID != "" {
		atomic.AddInt64(&legacyAttemptLineageHits, 1)
	}
	return latest
}

// appendAttemptLog records a retry/ralph decision to the control bead's
// gc.attempt_log metadata.
func appendAttemptLog(store beads.Store, controlID string, attempt int, outcome, reason string) error {
	control, err := store.Get(controlID)
	if err != nil {
		return err
	}
	logJSON, err := appendAttemptLogValue(control.Metadata[beadmeta.AttemptLogMetadataKey], attempt, outcome, reason, nil)
	if err != nil {
		return err
	}
	return store.SetMetadata(controlID, beadmeta.AttemptLogMetadataKey, logJSON)
}

func appendAttemptLogValue(existing string, attempt int, outcome, reason string, tracef func(string, ...any)) (string, error) {
	var log []map[string]string
	if existing != "" {
		if err := json.Unmarshal([]byte(existing), &log); err != nil {
			// A corrupt audit history cannot be recovered, so we start fresh —
			// but surface the reset instead of silently discarding the log.
			if tracef != nil {
				tracef("attempt-log corrupt, resetting history existing=%q err=%v", existing, err)
			}
			log = nil
		}
	}

	entry := map[string]string{
		"attempt": strconv.Itoa(attempt),
		"outcome": outcome,
	}
	if reason != "" {
		entry["reason"] = reason
	}

	var action string
	switch outcome {
	case "pass":
		action = "close"
	case "hard":
		action = "hard-fail"
	case "transient":
		action = "retry"
	default:
		action = outcome
	}
	entry["action"] = action

	if len(log) > 0 {
		last := log[len(log)-1]
		if last["attempt"] == entry["attempt"] && last["outcome"] == entry["outcome"] && last["action"] == entry["action"] {
			log[len(log)-1] = entry
		} else {
			log = append(log, entry)
		}
	} else {
		log = append(log, entry)
	}
	logJSON, err := json.Marshal(log)
	if err != nil {
		return "", err
	}

	return string(logJSON), nil
}

func copyNonGCMetadata(dst, src map[string]string) {
	for key, value := range src {
		if key == "" || strings.HasPrefix(key, beadmeta.Namespace) {
			continue
		}
		dst[key] = value
	}
}

func updateMetadataAndClose(store beads.Store, beadID string, metadata map[string]string) error {
	status := "closed"
	if err := store.Update(beadID, beads.UpdateOpts{
		Status:   &status,
		Metadata: metadata,
	}); err != nil {
		return err
	}
	bead, err := store.Get(beadID)
	if err != nil {
		return fmt.Errorf("verifying close of %s: %w", beadID, err)
	}
	if bead.Status == "closed" {
		return nil
	}
	return store.Close(beadID)
}

// Note: listByWorkflowRoot, setOutcomeAndClose, propagateRetrySubjectMetadata,
// classifyRetryAttempt, retryPreservedAssigneeWithConfig, and runRalphCheck are
// defined in runtime.go, retry.go, and ralph.go respectively.
