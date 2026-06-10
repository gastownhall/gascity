package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/convergence"
	"github.com/gastownhall/gascity/internal/formula"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/molecule"
	"github.com/gastownhall/gascity/internal/session"
)

// processRetryControl handles a retry control bead when it becomes ready
// (its blocking dep on the latest attempt has resolved).
func processRetryControl(store beads.Store, bead beads.Bead, opts ProcessOptions) (ControlResult, error) {
	maxAttempts, err := strconv.Atoi(bead.Metadata[beadmeta.MaxAttemptsMetadataKey])
	if err != nil || maxAttempts < 1 {
		return ControlResult{}, fmt.Errorf("%s: invalid gc.max_attempts %q", bead.ID, bead.Metadata[beadmeta.MaxAttemptsMetadataKey])
	}
	onExhausted := bead.Metadata[beadmeta.OnExhaustedMetadataKey]
	if onExhausted == "" {
		onExhausted = "hard_fail"
	}

	// Find the most recent attempt.
	attempt, err := findLatestAttempt(store, bead)
	if err != nil {
		return ControlResult{}, fmt.Errorf("%s: finding latest attempt: %w", bead.ID, err)
	}
	if attempt.ID == "" {
		// A retry control with no attempt sub-DAG cannot become valid by
		// waiting — the graph is malformed (missing seed or a seed attach
		// marked molecule_failed). Classify for the dispatcher quarantine
		// instead of fataling the serve loop. See gastownhall/gascity#2798.
		opts.tracef("process-control bead=%s kind=retry quarantine reason=no_attempt_found root=%s",
			bead.ID, bead.Metadata[beadmeta.RootBeadIDMetadataKey])
		return ControlResult{}, fmt.Errorf("%w: %s: no attempt found", ErrControlGraphMalformed, bead.ID)
	}
	if attempt.Status != "closed" {
		if err := ensureBlockingDependency(store, bead.ID, attempt.ID); err != nil {
			if controllerSpawnBoundaryPending(store, bead.ID, err, opts) {
				return ControlResult{}, ErrControlPending
			}
			return ControlResult{}, fmt.Errorf("%s: blocking on pending attempt %s: %w", bead.ID, attempt.ID, err)
		}
		if err := syncControlEpochToAttempt(store, bead, attempt); err != nil {
			if controllerSpawnBoundaryPending(store, bead.ID, err, opts) {
				return ControlResult{}, ErrControlPending
			}
			return ControlResult{}, fmt.Errorf("%s: advancing recovered attempt epoch for %s: %w", bead.ID, attempt.ID, err)
		}
		if err := closeGeneratedSpecBeadsForAttempt(store, bead, attempt); err != nil {
			if controllerSpawnBoundaryPending(store, bead.ID, err, opts) {
				return ControlResult{}, ErrControlPending
			}
			return ControlResult{}, fmt.Errorf("%s: closing generated spec beads for pending attempt %s: %w", bead.ID, attempt.ID, err)
		}
		return ControlResult{}, ErrControlPending
	}

	attemptNum, _ := strconv.Atoi(attempt.Metadata[beadmeta.AttemptMetadataKey])
	result, err := classifyRetryAttemptWithPostconditions(store, attempt, opts)
	if err != nil {
		return ControlResult{}, fmt.Errorf("%s: evaluating retry postconditions for %s: %w", bead.ID, attempt.ID, err)
	}
	attemptLog, err := appendAttemptLogValue(bead.Metadata[beadmeta.AttemptLogMetadataKey], attemptNum, result.Outcome, result.Reason)
	if err != nil {
		return ControlResult{}, fmt.Errorf("%s: recording attempt log: %w", bead.ID, err)
	}

	switch result.Outcome {
	case "pass":
		closeMetadata := map[string]string{
			beadmeta.AttemptLogMetadataKey: attemptLog,
			beadmeta.OutcomeMetadataKey:    "pass",
		}
		clearControllerSpawnErrorMetadata(closeMetadata)
		if outputJSON := attempt.Metadata[beadmeta.OutputJSONMetadataKey]; outputJSON != "" {
			closeMetadata[beadmeta.OutputJSONMetadataKey] = outputJSON
		}
		copyNonGCMetadata(closeMetadata, attempt.Metadata)
		if err := updateMetadataAndClose(store, bead.ID, closeMetadata); err != nil {
			return ControlResult{}, fmt.Errorf("%s: closing passed: %w", bead.ID, err)
		}
		scopeResult, err := reconcileClosedScopeMemberWithOptions(store, bead.ID, opts)
		if err != nil {
			return ControlResult{}, fmt.Errorf("%s: reconciling enclosing scope: %w", bead.ID, err)
		}
		return ControlResult{Processed: true, Action: "pass", Skipped: scopeResult.Skipped}, nil

	case "hard":
		closeMetadata := map[string]string{
			beadmeta.AttemptLogMetadataKey:       attemptLog,
			beadmeta.OutcomeMetadataKey:          "fail",
			beadmeta.FailedAttemptMetadataKey:    strconv.Itoa(attemptNum),
			beadmeta.FailureClassMetadataKey:     "hard",
			beadmeta.FailureReasonMetadataKey:    result.Reason,
			beadmeta.FinalDispositionMetadataKey: "hard_fail",
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

	case "transient":
		if attemptNum >= maxAttempts {
			exhaustedResult, err := handleRetryExhaustion(store, bead.ID, attemptNum, result.Reason, onExhausted, attemptLog)
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
			return ControlResult{}, fmt.Errorf("%s: spawning attempt %d: %w", bead.ID, nextAttempt, err)
		}

		return ControlResult{Processed: true, Action: "retry", Created: 1}, nil

	default:
		return ControlResult{}, fmt.Errorf("%s: unsupported outcome %q", bead.ID, result.Outcome)
	}
}

// processRalphControl handles a ralph control bead when it becomes ready.
func processRalphControl(store beads.Store, bead beads.Bead, opts ProcessOptions) (ControlResult, error) {
	maxAttempts, err := strconv.Atoi(bead.Metadata[beadmeta.MaxAttemptsMetadataKey])
	if err != nil || maxAttempts < 1 {
		return ControlResult{}, fmt.Errorf("%s: invalid gc.max_attempts %q", bead.ID, bead.Metadata[beadmeta.MaxAttemptsMetadataKey])
	}

	// Find the most recent iteration.
	iteration, err := findLatestAttempt(store, bead)
	if err != nil {
		return ControlResult{}, fmt.Errorf("%s: finding latest iteration: %w", bead.ID, err)
	}
	if iteration.ID == "" {
		// A ralph control with no iteration sub-DAG cannot become valid by
		// waiting — the graph is malformed (missing first-iteration seed or
		// a seed attach marked molecule_failed). Classify for the dispatcher
		// quarantine instead of fataling the serve loop, which crash-looped
		// all dispatch for the rig. See gastownhall/gascity#2798.
		opts.tracef("process-control bead=%s kind=ralph quarantine reason=no_iteration_found root=%s",
			bead.ID, bead.Metadata[beadmeta.RootBeadIDMetadataKey])
		return ControlResult{}, fmt.Errorf("%w: %s: no iteration found", ErrControlGraphMalformed, bead.ID)
	}
	if iteration.Status != "closed" {
		if err := ensureBlockingDependency(store, bead.ID, iteration.ID); err != nil {
			if controllerSpawnBoundaryPending(store, bead.ID, err, opts) {
				return ControlResult{}, ErrControlPending
			}
			return ControlResult{}, fmt.Errorf("%s: blocking on pending iteration %s: %w", bead.ID, iteration.ID, err)
		}
		if err := syncControlEpochToAttempt(store, bead, iteration); err != nil {
			if controllerSpawnBoundaryPending(store, bead.ID, err, opts) {
				return ControlResult{}, ErrControlPending
			}
			return ControlResult{}, fmt.Errorf("%s: advancing recovered iteration epoch for %s: %w", bead.ID, iteration.ID, err)
		}
		if err := closeGeneratedSpecBeadsForAttempt(store, bead, iteration); err != nil {
			if controllerSpawnBoundaryPending(store, bead.ID, err, opts) {
				return ControlResult{}, ErrControlPending
			}
			return ControlResult{}, fmt.Errorf("%s: closing generated spec beads for pending iteration %s: %w", bead.ID, iteration.ID, err)
		}
		return ControlResult{}, ErrControlPending
	}

	iterationNum, _ := strconv.Atoi(iteration.Metadata[beadmeta.AttemptMetadataKey])

	// Propagate non-gc metadata from the iteration to the ralph control
	// BEFORE running the check. This makes the iteration's output (e.g.,
	// review.verdict) visible on the ralph bead for check scripts that
	// read $GC_BEAD_ID metadata.
	if err := propagateRetrySubjectMetadata(store, bead.ID, iteration); err != nil {
		return ControlResult{}, fmt.Errorf("%s: propagating iteration metadata: %w", bead.ID, err)
	}
	// Reload the bead after metadata propagation so the check sees updated values.
	bead, err = store.Get(bead.ID)
	if err != nil {
		return ControlResult{}, fmt.Errorf("%s: reloading after propagation: %w", bead.ID, err)
	}

	// Run check script. The control bead carries the check config (gc.check_path etc),
	// and the iteration is the subject whose output is being checked.
	checkResult, err := runRalphCheck(store, bead, iteration, iterationNum, opts)
	if err != nil {
		return ControlResult{}, fmt.Errorf("%s: running check: %w", bead.ID, err)
	}

	attemptLog, err := appendAttemptLogValue(bead.Metadata[beadmeta.AttemptLogMetadataKey], iterationNum, checkResult.Outcome, checkResult.Stderr)
	if err != nil {
		return ControlResult{}, fmt.Errorf("%s: recording attempt log: %w", bead.ID, err)
	}

	if checkResult.Outcome == convergence.GatePass {
		closeMetadata := map[string]string{
			beadmeta.AttemptLogMetadataKey: attemptLog,
			beadmeta.OutcomeMetadataKey:    "pass",
		}
		clearControllerSpawnErrorMetadata(closeMetadata)
		if outputJSON := iteration.Metadata[beadmeta.OutputJSONMetadataKey]; outputJSON != "" {
			closeMetadata[beadmeta.OutputJSONMetadataKey] = outputJSON
		}
		if err := updateMetadataAndClose(store, bead.ID, closeMetadata); err != nil {
			return ControlResult{}, fmt.Errorf("%s: closing passed: %w", bead.ID, err)
		}
		scopeResult, err := reconcileClosedScopeMemberWithOptions(store, bead.ID, opts)
		if err != nil {
			return ControlResult{}, fmt.Errorf("%s: reconciling enclosing scope: %w", bead.ID, err)
		}
		return ControlResult{Processed: true, Action: "pass", Skipped: scopeResult.Skipped}, nil
	}

	if iterationNum >= maxAttempts {
		closeMetadata := map[string]string{
			beadmeta.AttemptLogMetadataKey:    attemptLog,
			beadmeta.OutcomeMetadataKey:       "fail",
			beadmeta.FailedAttemptMetadataKey: strconv.Itoa(iterationNum),
		}
		clearControllerSpawnErrorMetadata(closeMetadata)
		if err := updateMetadataAndClose(store, bead.ID, closeMetadata); err != nil {
			return ControlResult{}, fmt.Errorf("%s: closing exhausted: %w", bead.ID, err)
		}
		scopeResult, err := reconcileClosedScopeMemberWithOptions(store, bead.ID, opts)
		if err != nil {
			return ControlResult{}, fmt.Errorf("%s: reconciling enclosing scope: %w", bead.ID, err)
		}
		return ControlResult{Processed: true, Action: "fail", Skipped: scopeResult.Skipped}, nil
	}

	// Spawn next iteration.
	spawnMetadata := map[string]string{beadmeta.AttemptLogMetadataKey: attemptLog}
	clearControllerSpawnErrorMetadata(spawnMetadata)
	if err := store.SetMetadataBatch(bead.ID, spawnMetadata); err != nil {
		if controllerSpawnBoundaryPending(store, bead.ID, err, opts) {
			return ControlResult{}, ErrControlPending
		}
		return ControlResult{}, fmt.Errorf("%s: recording attempt log: %w", bead.ID, err)
	}
	nextIteration := iterationNum + 1
	if err := spawnNextAttempt(context.Background(), store, bead, nextIteration, opts); err != nil {
		if markControllerSpawnError(store, bead.ID, err, opts) {
			return ControlResult{}, ErrControlPending
		}
		return ControlResult{}, fmt.Errorf("%s: spawning iteration %d: %w", bead.ID, nextIteration, err)
	}

	return ControlResult{Processed: true, Action: "retry", Created: 1}, nil
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
	return store.SetMetadata(control.ID, beadmeta.ControlEpochMetadataKey, strconv.Itoa(attemptNum))
}

func markControllerSpawnError(store beads.Store, beadID string, err error, opts ProcessOptions) bool {
	metadata := map[string]string{
		beadmeta.ControllerErrorMetadataKey: err.Error(),
	}
	if IsTransientControllerError(err) && !isPartialAttemptAttachError(err) {
		metadata[beadmeta.ControllerErrorClassMetadataKey] = "transient"
		metadata[beadmeta.ControllerRetryableMetadataKey] = "true"
		_ = store.SetMetadataBatch(beadID, metadata)
		return true
	}

	metadata[beadmeta.ControllerErrorClassMetadataKey] = "hard"
	metadata[beadmeta.ControllerRetryableMetadataKey] = ""
	metadata[beadmeta.FinalDispositionMetadataKey] = "controller_error"
	_ = store.SetMetadataBatch(beadID, metadata)
	_ = setOutcomeAndClose(store, beadID, "fail")
	// Reconcile any enclosing scope so a controller_error terminal closure
	// does not leave the scope body stalled.
	_, _ = reconcileClosedScopeMemberWithOptions(store, beadID, opts)
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
	msg := strings.ToLower(err.Error())
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
	}
	for _, needle := range transientNeedles {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	return false
}

func handleRetryExhaustion(store beads.Store, beadID string, attemptNum int, reason, onExhausted, attemptLog string) (ControlResult, error) {
	if onExhausted == "soft_fail" {
		closeMetadata := map[string]string{
			beadmeta.AttemptLogMetadataKey:       attemptLog,
			beadmeta.OutcomeMetadataKey:          "pass",
			beadmeta.FailedAttemptMetadataKey:    strconv.Itoa(attemptNum),
			beadmeta.FailureClassMetadataKey:     "transient",
			beadmeta.FailureReasonMetadataKey:    reason,
			beadmeta.FinalDispositionMetadataKey: "soft_fail",
		}
		clearControllerSpawnErrorMetadata(closeMetadata)
		if err := updateMetadataAndClose(store, beadID, closeMetadata); err != nil {
			return ControlResult{}, fmt.Errorf("%s: closing soft-failed: %w", beadID, err)
		}
		return ControlResult{Processed: true, Action: "soft-fail"}, nil
	}

	closeMetadata := map[string]string{
		beadmeta.AttemptLogMetadataKey:       attemptLog,
		beadmeta.OutcomeMetadataKey:          "fail",
		beadmeta.FailedAttemptMetadataKey:    strconv.Itoa(attemptNum),
		beadmeta.FailureClassMetadataKey:     "transient",
		beadmeta.FailureReasonMetadataKey:    reason,
		beadmeta.FinalDispositionMetadataKey: "hard_fail",
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
	routeCfg := loadAttemptRouteConfig(opts.CityPath)
	for i := range recipe.Steps {
		if recipe.Steps[i].Metadata[beadmeta.KindMetadataKey] == "spec" {
			continue
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
			applyAttemptControlStepRoute(&recipe.Steps[i], target, routeCfg, store)
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
			"molecule_failed":                  "true",
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
	rootKind := "task"
	if step.Ralph != nil && len(step.Children) > 0 {
		rootKind = "scope"
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
	if step.OnComplete != nil {
		rootMeta[beadmeta.OutputJSONRequiredMetadataKey] = "true"
	}
	// Ralph iterations need scope metadata for grouping.
	if rootKind == "scope" {
		rootMeta[beadmeta.ScopeRoleMetadataKey] = "body"
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
				beadmeta.ScopeRoleMetadataKey:   "member",
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
				childMeta[beadmeta.KindMetadataKey] = "retry"
				childMeta[beadmeta.MaxAttemptsMetadataKey] = strconv.Itoa(child.Retry.MaxAttempts)
				childMeta[beadmeta.ControlEpochMetadataKey] = "1"
				if child.Retry.OnExhausted != "" {
					childMeta[beadmeta.OnExhaustedMetadataKey] = child.Retry.OnExhausted
				} else {
					childMeta[beadmeta.OnExhaustedMetadataKey] = "hard_fail"
				}
				// Emit a spec bead for the nested retry so it can spawn
				// its own attempts without oversized metadata.
				if step := newSpecRecipeStep(childID, child); step != nil {
					recipe.Steps = append(recipe.Steps, *step)
				}
			}
			if child.Ralph != nil {
				childMeta[beadmeta.KindMetadataKey] = "ralph"
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
		beadmeta.KindMetadataKey:       "fanout",
		beadmeta.ControlForMetadataKey: sourceRef,
		beadmeta.ForEachMetadataKey:    onComplete.ForEach,
		beadmeta.BondMetadataKey:       onComplete.Bond,
		beadmeta.FanoutModeMetadataKey: "parallel",
	}
	if onComplete.Sequential {
		meta[beadmeta.FanoutModeMetadataKey] = "sequential"
	}
	if len(onComplete.Vars) > 0 {
		if data, err := json.Marshal(onComplete.Vars); err == nil {
			meta[beadmeta.BondVarsMetadataKey] = string(data)
		}
	}
	for _, key := range []string{beadmeta.ScopeRefMetadataKey, beadmeta.ScopeRoleMetadataKey, beadmeta.OnFailMetadataKey, beadmeta.StepIDMetadataKey, beadmeta.RalphStepIDMetadataKey, beadmeta.AttemptMetadataKey} {
		if value := source.Metadata[key]; value != "" {
			meta[key] = value
		}
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
			beadmeta.KindMetadataKey:       "scope-check",
			beadmeta.ScopeRefMetadataKey:   step.Metadata[beadmeta.ScopeRefMetadataKey],
			beadmeta.ScopeRoleMetadataKey:  "control",
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
	if step.Metadata[beadmeta.ScopeRoleMetadataKey] == "teardown" {
		return false
	}
	switch step.Metadata[beadmeta.KindMetadataKey] {
	case "scope", "scope-check", "workflow-finalize", "fanout", "check", "spec":
		return false
	default:
		return true
	}
}

func loadAttemptRouteConfig(cityPath string) *config.City {
	if strings.TrimSpace(cityPath) == "" {
		return nil
	}
	cfg, _, err := config.LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		return nil
	}
	return cfg
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

func applyAttemptControlStepRoute(step *formula.RecipeStep, executionTarget string, cfg *config.City, store beads.Store) {
	if step.Metadata == nil {
		step.Metadata = make(map[string]string)
	}
	resolvedExecutionTarget := strings.TrimSpace(executionTarget)
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

	controlTarget := controlDispatcherTargetForExecutionTarget(resolvedExecutionTarget)
	if assignee, ok := resolveAttemptControlAssignee(controlTarget, cfg, store); ok {
		delete(step.Metadata, beadmeta.RoutedToMetadataKey)
		step.Assignee = assignee
		return
	}

	step.Metadata[beadmeta.RoutedToMetadataKey] = controlTarget
	step.Assignee = ""
}

func controlDispatcherTargetForExecutionTarget(executionTarget string) string {
	executionTarget = strings.TrimSpace(executionTarget)
	if slash := strings.IndexByte(executionTarget, '/'); slash > 0 {
		return executionTarget[:slash] + "/" + config.ControlDispatcherAgentName
	}
	return config.ControlDispatcherAgentName
}

func resolveAttemptControlAssignee(target string, cfg *config.City, store beads.Store) (string, bool) {
	target = strings.TrimSpace(target)
	if target == "" {
		return "", false
	}
	if binding, ok := resolveAttemptRouteBinding(target, cfg, store); ok {
		if binding.directSessionID != "" {
			return binding.directSessionID, true
		}
		if binding.sessionName != "" {
			return binding.sessionName, true
		}
	}
	if cfg != nil {
		if named := config.FindNamedSession(cfg, target); named != nil {
			if spec, ok := session.FindNamedSessionSpec(cfg, cfg.EffectiveCityName(), named.QualifiedName()); ok && spec.SessionName != "" {
				return spec.SessionName, true
			}
		}
		if agentCfg := config.FindAgent(cfg, target); agentCfg != nil {
			if sessionName := config.NamedSessionRuntimeName(cfg.EffectiveCityName(), cfg.Workspace, agentCfg.QualifiedName()); sessionName != "" {
				return sessionName, true
			}
		}
	}
	return "", false
}

func isAttemptControlKind(kind string) bool {
	switch kind {
	case "check", "fanout", "retry-eval", "scope-check", "workflow-finalize", "retry", "ralph":
		return true
	default:
		return false
	}
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
				if spec.SessionName != "" {
					return attemptRouteBinding{sessionName: spec.SessionName}, true
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

func beadUsesMetadataPoolRoute(bead beads.Bead, cityPath string) bool {
	return beadUsesMetadataPoolRouteWithConfig(bead, loadAttemptRouteConfig(cityPath))
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
		if b.Metadata[beadmeta.KindMetadataKey] != "spec" {
			continue
		}
		if stepRef != "" && b.Metadata[beadmeta.SpecForRefMetadataKey] == stepRef {
			return b, nil
		}
	}
	for _, b := range all {
		if b.Metadata[beadmeta.KindMetadataKey] == "spec" && b.Metadata[beadmeta.SpecForMetadataKey] == stepID {
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
			beadmeta.KindMetadataKey:       "spec",
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
		if step.Metadata[beadmeta.KindMetadataKey] != "spec" {
			continue
		}
		beadID := result.IDMapping[step.ID]
		if beadID != "" {
			if err := setOutcomeAndClose(store, beadID, "pass"); err != nil {
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
		if bead.Status == "closed" || bead.Metadata[beadmeta.KindMetadataKey] != "spec" {
			continue
		}
		ref := strings.TrimSpace(bead.Metadata[beadmeta.StepRefMetadataKey])
		if ref == "" {
			ref = strings.TrimSpace(bead.Ref)
		}
		if !strings.HasPrefix(ref, attemptRef+".") {
			continue
		}
		if err := setOutcomeAndClose(store, bead.ID, "pass"); err != nil {
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
		if bead.Status == "closed" || bead.Metadata[beadmeta.KindMetadataKey] != "spec" {
			continue
		}
		ref := strings.TrimSpace(bead.Metadata[beadmeta.StepRefMetadataKey])
		if ref == "" {
			ref = strings.TrimSpace(bead.Ref)
		}
		if _, ok := wanted[ref]; !ok {
			continue
		}
		if err := setOutcomeAndClose(store, bead.ID, "pass"); err != nil {
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
	return strings.TrimSpace(bead.Metadata["molecule_failed"]) == "true"
}

// findLatestAttempt finds the most recent attempt/iteration child of a control bead.
// Matches by gc.step_ref pattern: the attempt's step_ref ends with
// .attempt.N or .iteration.N where the prefix matches the control's step_ref.
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

func latestAttemptFromCandidates(control beads.Bead, candidates []beads.Bead) beads.Bead {
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
		switch kind {
		case "scope-check", "workflow-finalize", "fanout", "check", "retry-eval", "retry", "ralph", "workflow":
			continue
		case "scope":
			if controlKind != "ralph" {
				continue
			}
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
	return latest
}

// appendAttemptLog records a retry/ralph decision to the control bead's
// gc.attempt_log metadata.
func appendAttemptLog(store beads.Store, controlID string, attempt int, outcome, reason string) error {
	control, err := store.Get(controlID)
	if err != nil {
		return err
	}
	logJSON, err := appendAttemptLogValue(control.Metadata[beadmeta.AttemptLogMetadataKey], attempt, outcome, reason)
	if err != nil {
		return err
	}
	return store.SetMetadata(controlID, beadmeta.AttemptLogMetadataKey, logJSON)
}

func appendAttemptLogValue(existing string, attempt int, outcome, reason string) (string, error) {
	var log []map[string]string
	if existing != "" {
		_ = json.Unmarshal([]byte(existing), &log)
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
	return store.Update(beadID, beads.UpdateOpts{
		Status:   &status,
		Metadata: metadata,
	})
}

// Note: listByWorkflowRoot, setOutcomeAndClose, propagateRetrySubjectMetadata,
// classifyRetryAttempt, retryPreservedAssignee, and runRalphCheck are defined
// in runtime.go, retry.go, and ralph.go respectively.
