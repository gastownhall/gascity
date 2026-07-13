package dispatch

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/pathutil"
)

func processRetryEval(store beads.Store, bead beads.Bead, opts ProcessOptions) (ControlResult, error) {
	attempt, err := strconv.Atoi(bead.Metadata[beadmeta.AttemptMetadataKey])
	if err != nil || attempt < 1 {
		return ControlResult{}, fmt.Errorf("%s: invalid gc.attempt %q", bead.ID, bead.Metadata[beadmeta.AttemptMetadataKey])
	}
	maxAttempts, err := strconv.Atoi(bead.Metadata[beadmeta.MaxAttemptsMetadataKey])
	if err != nil || maxAttempts < 1 {
		return ControlResult{}, fmt.Errorf("%s: invalid gc.max_attempts %q", bead.ID, bead.Metadata[beadmeta.MaxAttemptsMetadataKey])
	}
	onExhausted := bead.Metadata[beadmeta.OnExhaustedMetadataKey]
	if onExhausted == "" {
		onExhausted = beadmeta.DispositionHardFail
	}

	logicalID := resolveLogicalBeadID(store, bead)
	if logicalID == "" {
		return ControlResult{}, fmt.Errorf("%s: could not resolve logical bead ID", bead.ID)
	}
	logical, err := store.Get(logicalID)
	if err != nil {
		return ControlResult{}, fmt.Errorf("%s: loading logical bead %s: %w", bead.ID, logicalID, err)
	}
	if closedBy, _ := strconv.Atoi(logical.Metadata[beadmeta.ClosedByAttemptMetadataKey]); closedBy >= attempt {
		if err := finalizeRetryEval(store, logicalID, bead.ID); err != nil {
			return ControlResult{}, fmt.Errorf("%s: finalizing stale retry eval: %w", bead.ID, err)
		}
		return ControlResult{Processed: true, Action: "noop"}, nil
	}

	subject, err := resolveRetryRunSubject(store, bead, logicalID, attempt)
	if err != nil {
		return ControlResult{}, fmt.Errorf("%s: resolving retry run subject: %w", bead.ID, err)
	}
	if subject.Status != "closed" {
		return ControlResult{}, ErrControlPending
	}

	result, err := classifyRetryAttemptWithPostconditions(store, subject, opts)
	if err != nil {
		return ControlResult{}, fmt.Errorf("%s: evaluating retry postconditions for %s: %w", bead.ID, subject.ID, err)
	}
	if err := persistRetryEvalResult(store, bead.ID, result); err != nil {
		return ControlResult{}, fmt.Errorf("%s: persisting retry eval result: %w", bead.ID, err)
	}

	switch result.Outcome {
	case "pass":
		if outputJSON := subject.Metadata[beadmeta.OutputJSONMetadataKey]; outputJSON != "" {
			if err := store.SetMetadata(logicalID, beadmeta.OutputJSONMetadataKey, outputJSON); err != nil {
				return ControlResult{}, fmt.Errorf("%s: propagating gc.output_json to logical bead: %w", logicalID, err)
			}
		}
		if err := propagateRetrySubjectMetadata(store, logicalID, subject); err != nil {
			return ControlResult{}, fmt.Errorf("%s: propagating subject metadata to logical bead: %w", logicalID, err)
		}
		if err := store.SetMetadataBatch(logicalID, map[string]string{
			beadmeta.ClosedByAttemptMetadataKey:  strconv.Itoa(attempt),
			beadmeta.FinalDispositionMetadataKey: beadmeta.DispositionPass,
		}); err != nil {
			return ControlResult{}, fmt.Errorf("%s: marking logical pass: %w", logicalID, err)
		}
		if err := setOutcomeAndClose(store, bead.ID, beadmeta.OutcomePass); err != nil {
			return ControlResult{}, fmt.Errorf("%s: closing passed eval: %w", bead.ID, err)
		}
		if err := setOutcomeAndClose(store, logicalID, beadmeta.OutcomePass); err != nil {
			return ControlResult{}, fmt.Errorf("%s: closing logical bead: %w", logicalID, err)
		}
		return ControlResult{Processed: true, Action: "pass"}, nil

	case "hard":
		if err := store.SetMetadataBatch(logicalID, map[string]string{
			beadmeta.ClosedByAttemptMetadataKey:  strconv.Itoa(attempt),
			beadmeta.FailedAttemptMetadataKey:    strconv.Itoa(attempt),
			beadmeta.FailureClassMetadataKey:     beadmeta.FailureClassHard,
			beadmeta.FailureReasonMetadataKey:    result.Reason,
			beadmeta.FinalDispositionMetadataKey: beadmeta.DispositionHardFail,
		}); err != nil {
			return ControlResult{}, fmt.Errorf("%s: marking logical hard failure: %w", logicalID, err)
		}
		if err := setOutcomeAndClose(store, bead.ID, beadmeta.OutcomeFail); err != nil {
			return ControlResult{}, fmt.Errorf("%s: closing hard-failed eval: %w", bead.ID, err)
		}
		if err := setOutcomeAndClose(store, logicalID, beadmeta.OutcomeFail); err != nil {
			return ControlResult{}, fmt.Errorf("%s: closing hard-failed logical bead: %w", logicalID, err)
		}
		return ControlResult{Processed: true, Action: "hard-fail"}, nil

	case "transient":
		if attempt >= maxAttempts {
			if onExhausted == beadmeta.DispositionSoftFail {
				if err := store.SetMetadataBatch(logicalID, map[string]string{
					beadmeta.ClosedByAttemptMetadataKey:  strconv.Itoa(attempt),
					beadmeta.FailedAttemptMetadataKey:    strconv.Itoa(attempt),
					beadmeta.FailureClassMetadataKey:     beadmeta.FailureClassTransient,
					beadmeta.FailureReasonMetadataKey:    result.Reason,
					beadmeta.FinalDispositionMetadataKey: beadmeta.DispositionSoftFail,
				}); err != nil {
					return ControlResult{}, fmt.Errorf("%s: marking logical soft-fail: %w", logicalID, err)
				}
				if err := setOutcomeAndClose(store, bead.ID, beadmeta.OutcomeFail); err != nil {
					return ControlResult{}, fmt.Errorf("%s: closing exhausted eval: %w", bead.ID, err)
				}
				if err := setOutcomeAndClose(store, logicalID, beadmeta.OutcomePass); err != nil {
					return ControlResult{}, fmt.Errorf("%s: closing soft-failed logical bead: %w", logicalID, err)
				}
				return ControlResult{Processed: true, Action: "soft-fail"}, nil
			}
			if err := store.SetMetadataBatch(logicalID, map[string]string{
				beadmeta.ClosedByAttemptMetadataKey:  strconv.Itoa(attempt),
				beadmeta.FailedAttemptMetadataKey:    strconv.Itoa(attempt),
				beadmeta.FailureClassMetadataKey:     beadmeta.FailureClassTransient,
				beadmeta.FailureReasonMetadataKey:    result.Reason,
				beadmeta.FinalDispositionMetadataKey: beadmeta.DispositionHardFail,
			}); err != nil {
				return ControlResult{}, fmt.Errorf("%s: marking exhausted logical failure: %w", logicalID, err)
			}
			if err := setOutcomeAndClose(store, bead.ID, beadmeta.OutcomeFail); err != nil {
				return ControlResult{}, fmt.Errorf("%s: closing exhausted eval: %w", bead.ID, err)
			}
			if err := setOutcomeAndClose(store, logicalID, beadmeta.OutcomeFail); err != nil {
				return ControlResult{}, fmt.Errorf("%s: closing exhausted logical bead: %w", logicalID, err)
			}
			return ControlResult{Processed: true, Action: "fail"}, nil
		}
	default:
		return ControlResult{}, fmt.Errorf("%s: unsupported retry eval outcome %q", bead.ID, result.Outcome)
	}

	nextAttempt := attempt + 1
	switch bead.Metadata[beadmeta.RetryStateMetadataKey] {
	case "":
		if err := store.SetMetadataBatch(bead.ID, map[string]string{
			beadmeta.RetryStateMetadataKey:  beadmeta.SpawnStateSpawning,
			beadmeta.NextAttemptMetadataKey: strconv.Itoa(nextAttempt),
		}); err != nil {
			if controllerSpawnBoundaryPending(store, bead.ID, err, opts) {
				return ControlResult{}, ErrControlPending
			}
			return ControlResult{}, fmt.Errorf("%s: recording retry spawn start: %w", bead.ID, err)
		}
	case beadmeta.SpawnStateSpawning:
		// Resume partial append below.
	case beadmeta.SpawnStateSpawned:
		// Resume finalization below without cloning again.
	default:
		return ControlResult{}, fmt.Errorf("%s: unsupported gc.retry_state %q", bead.ID, bead.Metadata[beadmeta.RetryStateMetadataKey])
	}

	// A routeConfig error is intentionally tolerated here: retry preserves the
	// prior attempt's already-stamped routes rather than scope-routing, so a nil
	// cfg degrades to metadata-only instead of mis-routing. Spawn/fanout
	// (control.go, fanout.go) fail closed on this error because they scope-route
	// through applyAttemptControlStepRoute.
	routeCfg, _ := opts.routeConfig()
	if beadUsesMetadataPoolRouteWithConfig(subject, routeCfg) {
		if opts.RecycleSession == nil {
			return ControlResult{}, fmt.Errorf("%s: pooled retry subject %s requires RecycleSession callback", bead.ID, subject.ID)
		}
		if bead.Metadata[beadmeta.RetrySessionRecycledMetadataKey] != "true" {
			if subject.Assignee == "" {
				return ControlResult{}, fmt.Errorf("%s: pooled retry subject %s missing assignee", bead.ID, subject.ID)
			}
			if err := opts.RecycleSession(subject); err != nil {
				return ControlResult{}, fmt.Errorf("%s: recycling pooled session %s: %w", bead.ID, subject.Assignee, err)
			}
			if err := store.SetMetadata(bead.ID, beadmeta.RetrySessionRecycledMetadataKey, "true"); err != nil {
				return ControlResult{}, fmt.Errorf("%s: recording pooled session recycle: %w", bead.ID, err)
			}
		}
	}

	if bead.Metadata[beadmeta.RetryStateMetadataKey] != beadmeta.SpawnStateSpawned {
		if err := appendRetryAttempt(store, logicalID, subject, bead, nextAttempt, routeCfg); err != nil {
			if controllerSpawnBoundaryPending(store, bead.ID, err, opts) {
				return ControlResult{}, ErrControlPending
			}
			return ControlResult{}, fmt.Errorf("%s: appending retry attempt: %w", bead.ID, err)
		}
		spawnedMetadata := map[string]string{
			beadmeta.RetryStateMetadataKey:  beadmeta.SpawnStateSpawned,
			beadmeta.NextAttemptMetadataKey: strconv.Itoa(nextAttempt),
		}
		clearControllerSpawnErrorMetadata(spawnedMetadata)
		if err := store.SetMetadataBatch(bead.ID, spawnedMetadata); err != nil {
			if controllerSpawnBoundaryPending(store, bead.ID, err, opts) {
				return ControlResult{}, ErrControlPending
			}
			return ControlResult{}, fmt.Errorf("%s: recording retry spawn complete: %w", bead.ID, err)
		}
	}

	if err := store.SetMetadataBatch(logicalID, map[string]string{
		beadmeta.RetryCountMetadataKey:       strconv.Itoa(attempt),
		beadmeta.LastFailureClassMetadataKey: beadmeta.FailureClassTransient,
		beadmeta.FailureReasonMetadataKey:    result.Reason,
	}); err != nil {
		return ControlResult{}, fmt.Errorf("%s: recording retry metadata on logical bead: %w", logicalID, err)
	}
	if err := finalizeRetryEval(store, logicalID, bead.ID); err != nil {
		return ControlResult{}, fmt.Errorf("%s: finalizing retry eval: %w", bead.ID, err)
	}
	return ControlResult{Processed: true, Action: "retry"}, nil
}

func resolveRetryRunSubject(store beads.Store, eval beads.Bead, logicalID string, attempt int) (beads.Bead, error) {
	if rootID := strings.TrimSpace(eval.Metadata[beadmeta.RootBeadIDMetadataKey]); rootID != "" && logicalID != "" && attempt > 0 {
		all, err := listByWorkflowRoot(store, rootID)
		if err != nil {
			return beads.Bead{}, err
		}
		attemptStr := strconv.Itoa(attempt)
		for _, candidate := range all {
			if candidate.Metadata[beadmeta.KindMetadataKey] != beadmeta.KindRetryRun {
				continue
			}
			if candidate.Metadata[beadmeta.LogicalBeadIDMetadataKey] != logicalID {
				continue
			}
			if candidate.Metadata[beadmeta.AttemptMetadataKey] != attemptStr {
				continue
			}
			return candidate, nil
		}
	}

	subjectID, err := resolveBlockingSubjectID(store, eval.ID)
	if err != nil {
		return beads.Bead{}, err
	}
	return store.Get(subjectID)
}

type retryEvalResult struct {
	Outcome string
	Reason  string
}

func classifyRetryAttempt(subject beads.Bead) retryEvalResult {
	outcome := strings.TrimSpace(subject.Metadata[beadmeta.OutcomeMetadataKey])
	switch outcome {
	case beadmeta.OutcomePass:
		if strings.TrimSpace(subject.Metadata[beadmeta.FailureClassMetadataKey]) != "" || strings.TrimSpace(subject.Metadata[beadmeta.FailureReasonMetadataKey]) != "" {
			return retryEvalResult{Outcome: "transient", Reason: "pass_with_failure_metadata"}
		}
		if strings.TrimSpace(subject.Metadata[beadmeta.OutputJSONRequiredMetadataKey]) == "true" {
			rawOutput := strings.TrimSpace(subject.Metadata[beadmeta.OutputJSONMetadataKey])
			if rawOutput == "" {
				return retryEvalResult{Outcome: "transient", Reason: "missing_required_output_json"}
			}
			if !json.Valid([]byte(rawOutput)) {
				return retryEvalResult{Outcome: "transient", Reason: "invalid_required_output_json"}
			}
		}
		return retryEvalResult{Outcome: "pass"}
	case beadmeta.OutcomeFail:
		switch strings.TrimSpace(subject.Metadata[beadmeta.FailureClassMetadataKey]) {
		case beadmeta.FailureClassTransient:
			return retryEvalResult{Outcome: "transient", Reason: retryFailureReason(subject)}
		case beadmeta.FailureClassHard, "":
			return retryEvalResult{Outcome: "hard", Reason: retryFailureReason(subject)}
		default:
			return retryEvalResult{Outcome: "transient", Reason: "unknown_failure_class"}
		}
	case "":
		return retryEvalResult{Outcome: "transient", Reason: "missing_outcome"}
	default:
		return retryEvalResult{Outcome: "transient", Reason: "invalid_outcome_value"}
	}
}

func classifyRetryAttemptWithPostconditions(store beads.Store, subject beads.Bead, opts ProcessOptions) (retryEvalResult, error) {
	result := classifyRetryAttempt(subject)
	if result.Outcome != "pass" {
		return result, nil
	}
	reason, err := validateRequiredArtifacts(store, subject, opts.RequiredArtifactStat)
	if err != nil {
		return retryEvalResult{}, err
	}
	if reason != "" {
		return retryEvalResult{Outcome: "transient", Reason: reason}, nil
	}
	return result, nil
}

func validateRequiredArtifacts(store beads.Store, subject beads.Bead, stat func(string) (os.FileInfo, error)) (string, error) {
	if stat == nil {
		stat = os.Stat
	}
	for _, rawPath := range requiredArtifactTemplates(subject.Metadata) {
		path, worktree, reason, err := resolveRequiredArtifactPath(store, subject, rawPath)
		if err != nil {
			return "", err
		}
		if reason != "" {
			return reason, nil
		}
		info, err := stat(path)
		if err != nil {
			if os.IsNotExist(err) {
				return "missing_required_artifact", nil
			}
			return "unreadable_required_artifact", nil
		}
		contained, err := requiredArtifactTargetInWorktree(worktree, path)
		if err != nil {
			return "", err
		}
		if !contained {
			return "required_artifact_outside_worktree", nil
		}
		if info.IsDir() || info.Size() == 0 {
			return "empty_required_artifact", nil
		}
	}
	return "", nil
}

// requiredArtifactTemplates keeps the singular key as one opaque path; only
// the plural key is parsed as a comma/newline-delimited list.
func requiredArtifactTemplates(metadata map[string]string) []string {
	var result []string
	if raw := strings.TrimSpace(metadata[beadmeta.RequiredArtifactMetadataKey]); raw != "" {
		result = append(result, raw)
	}
	raw := strings.TrimSpace(metadata[beadmeta.RequiredArtifactsMetadataKey])
	if raw == "" {
		return result
	}
	for _, part := range strings.FieldsFunc(raw, func(r rune) bool {
		return r == '\n' || r == ','
	}) {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}

func resolveRequiredArtifactPath(store beads.Store, subject beads.Bead, rawPath string) (string, string, string, error) {
	rootID := strings.TrimSpace(subject.Metadata[beadmeta.RootBeadIDMetadataKey])
	attempt := strings.TrimSpace(subject.Metadata[beadmeta.AttemptMetadataKey])
	worktree := strings.TrimSpace(subject.Metadata["work_dir"])

	if worktree == "" {
		resolvedWorktree, reason, err := resolveRequiredArtifactWorktree(store, rootID)
		if err != nil {
			return "", "", "", err
		}
		if reason != "" {
			return "", "", reason, nil
		}
		worktree = resolvedWorktree
	}
	if worktree == "" {
		return "", "", "missing_required_artifact_context", nil
	}

	path := rawPath
	path = strings.ReplaceAll(path, "{worktree}", worktree)
	path = strings.ReplaceAll(path, "{root}", rootID)
	path = strings.ReplaceAll(path, "{root_id}", rootID)
	path = strings.ReplaceAll(path, "{attempt}", attempt)
	if strings.Contains(path, "{") || strings.Contains(path, "}") {
		return "", "", "unresolved_required_artifact_template", nil
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(worktree, path)
	}
	path = filepath.Clean(path)
	contained, err := requiredArtifactPathInWorktree(worktree, path)
	if err != nil {
		return "", "", "", err
	}
	if !contained {
		return "", "", "required_artifact_outside_worktree", nil
	}
	return path, worktree, "", nil
}

func requiredArtifactPathInWorktree(worktree, path string) (bool, error) {
	absWorktree, err := filepath.Abs(filepath.Clean(worktree))
	if err != nil {
		return false, fmt.Errorf("resolving required artifact worktree path %q: %w", worktree, err)
	}
	absPath, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return false, fmt.Errorf("resolving required artifact path %q: %w", path, err)
	}
	return pathutil.PathWithin(absWorktree, absPath), nil
}

func requiredArtifactTargetInWorktree(worktree, path string) (bool, error) {
	resolvedWorktree, err := filepath.EvalSymlinks(filepath.Clean(worktree))
	if err != nil {
		return false, fmt.Errorf("resolving required artifact worktree symlinks %q: %w", worktree, err)
	}
	resolvedPath, err := filepath.EvalSymlinks(filepath.Clean(path))
	if err != nil {
		if os.IsNotExist(err) {
			return true, nil
		}
		return false, fmt.Errorf("resolving required artifact path symlinks %q: %w", path, err)
	}
	return requiredArtifactPathInWorktree(resolvedWorktree, resolvedPath)
}

func resolveRequiredArtifactWorktree(store beads.Store, rootID string) (string, string, error) {
	if rootID == "" {
		return "", "missing_required_artifact_context", nil
	}
	root, err := store.Get(rootID)
	if errors.Is(err, beads.ErrNotFound) {
		return "", "missing_required_artifact_context", nil
	}
	if err != nil {
		return "", "", fmt.Errorf("loading required artifact workflow root %s: %w", rootID, markTransientControllerBoundaryError(err))
	}
	sourceID := strings.TrimSpace(root.Metadata[beadmeta.SourceBeadIDMetadataKey])
	if sourceID == "" {
		sourceID = strings.TrimSpace(root.Metadata[beadmeta.InputConvoyIDMetadataKey])
	}
	if sourceID == "" {
		return "", "missing_required_artifact_context", nil
	}
	source, err := store.Get(sourceID)
	if errors.Is(err, beads.ErrNotFound) {
		return "", "missing_required_artifact_context", nil
	}
	if err != nil {
		return "", "", fmt.Errorf("loading required artifact source bead %s: %w", sourceID, markTransientControllerBoundaryError(err))
	}
	worktree := strings.TrimSpace(source.Metadata["work_dir"])
	if worktree == "" {
		return "", "missing_required_artifact_context", nil
	}
	return worktree, "", nil
}

func retryFailureReason(subject beads.Bead) string {
	reason := strings.TrimSpace(subject.Metadata[beadmeta.FailureReasonMetadataKey])
	if reason == "" {
		return "unspecified"
	}
	return reason
}

func persistRetryEvalResult(store beads.Store, beadID string, result retryEvalResult) error {
	batch := map[string]string{
		beadmeta.FailureReasonMetadataKey: result.Reason,
	}
	// result.Outcome is the internal retryEvalResult domain {pass, transient,
	// hard} produced by classifyRetryAttempt, not the gc.outcome /
	// gc.failure_class vocabularies it maps onto below. Match it raw so the two
	// vocabularies cannot silently drift into a miscompare.
	switch result.Outcome {
	case "pass":
		batch[beadmeta.OutcomeMetadataKey] = beadmeta.OutcomePass
		batch[beadmeta.FailureClassMetadataKey] = ""
	case "transient":
		batch[beadmeta.OutcomeMetadataKey] = beadmeta.OutcomeFail
		batch[beadmeta.FailureClassMetadataKey] = beadmeta.FailureClassTransient
	default:
		batch[beadmeta.OutcomeMetadataKey] = beadmeta.OutcomeFail
		batch[beadmeta.FailureClassMetadataKey] = beadmeta.FailureClassHard
	}
	return store.SetMetadataBatch(beadID, batch)
}

func propagateRetrySubjectMetadata(store beads.Store, logicalID string, subject beads.Bead) error {
	batch := map[string]string{}
	for key, value := range subject.Metadata {
		if key == "" || strings.HasPrefix(key, beadmeta.Namespace) {
			continue
		}
		batch[key] = value
	}
	if len(batch) == 0 {
		return nil
	}
	return store.SetMetadataBatch(logicalID, batch)
}

func appendRetryAttempt(store beads.Store, logicalID string, prevRun, prevEval beads.Bead, nextAttempt int, routeCfg *config.City) error {
	oldAttempt, err := strconv.Atoi(prevRun.Metadata[beadmeta.AttemptMetadataKey])
	if err != nil || oldAttempt < 1 {
		return fmt.Errorf("%s: invalid gc.attempt %q", prevRun.ID, prevRun.Metadata[beadmeta.AttemptMetadataKey])
	}
	rootID := prevRun.Metadata[beadmeta.RootBeadIDMetadataKey]
	if rootID == "" {
		return fmt.Errorf("%s: missing gc.root_bead_id", prevRun.ID)
	}

	runRef := rewriteRetryAttemptRef(stepRefForRetryBead(prevRun), oldAttempt, nextAttempt)
	evalRef := rewriteRetryAttemptRef(stepRefForRetryBead(prevEval), oldAttempt, nextAttempt)
	if runRef == "" || evalRef == "" {
		return fmt.Errorf("%s: could not derive retry step refs", prevRun.ID)
	}

	all, err := listByWorkflowRoot(store, rootID)
	if err != nil {
		return err
	}
	var nextRun, nextEval beads.Bead
	for _, candidate := range all {
		switch stepRefForRetryBead(candidate) {
		case runRef:
			nextRun = candidate
		case evalRef:
			nextEval = candidate
		}
	}

	if nextRun.ID == "" {
		nextRun, err = store.Create(retryAttemptBead(prevRun, logicalID, runRef, nextAttempt, routeCfg))
		if err != nil {
			return fmt.Errorf("creating retry run bead: %w", err)
		}
	}
	if nextEval.ID == "" {
		nextEval, err = store.Create(retryEvalBead(prevEval, logicalID, evalRef, nextAttempt))
		if err != nil {
			return fmt.Errorf("creating retry eval bead: %w", err)
		}
	}

	if err := ensureDep(store, nextEval.ID, nextRun.ID, "blocks"); err != nil {
		return fmt.Errorf("wiring retry eval -> run: %w", err)
	}
	if err := ensureDep(store, logicalID, nextEval.ID, "blocks"); err != nil {
		return fmt.Errorf("wiring logical -> retry eval: %w", err)
	}
	return nil
}

func retryAttemptBead(prev beads.Bead, logicalID, stepRef string, attempt int, routeCfg *config.City) beads.Bead {
	meta := cloneMetadata(prev.Metadata)
	clearRetryEphemera(meta)
	assignee := retryPreservedAssigneeWithConfig(prev, routeCfg)
	if assignee == "" {
		clearSessionAffinityMetadata(meta)
	}
	meta[beadmeta.AttemptMetadataKey] = strconv.Itoa(attempt)
	meta[beadmeta.RetryFromMetadataKey] = prev.ID
	meta[beadmeta.StepRefMetadataKey] = stepRef
	meta[beadmeta.LogicalBeadIDMetadataKey] = logicalID
	return beads.Bead{
		Title:       prev.Title,
		Description: prev.Description,
		Type:        prev.Type,
		Assignee:    assignee,
		From:        prev.From,
		ParentID:    prev.ParentID,
		Ref:         stepRef,
		Labels:      removeAttemptPoolLabels(prev.Labels),
		Metadata:    meta,
	}
}

func retryEvalBead(prev beads.Bead, logicalID, stepRef string, attempt int) beads.Bead {
	meta := cloneMetadata(prev.Metadata)
	clearRetryEphemera(meta)
	clearSessionAffinityMetadata(meta)
	meta[beadmeta.AttemptMetadataKey] = strconv.Itoa(attempt)
	meta[beadmeta.RetryFromMetadataKey] = prev.ID
	meta[beadmeta.StepRefMetadataKey] = stepRef
	meta[beadmeta.LogicalBeadIDMetadataKey] = logicalID
	return beads.Bead{
		Title:       prev.Title,
		Description: prev.Description,
		Type:        prev.Type,
		From:        prev.From,
		ParentID:    prev.ParentID,
		Ref:         stepRef,
		Labels:      removeAttemptPoolLabels(prev.Labels),
		Metadata:    meta,
	}
}

func finalizeRetryEval(store beads.Store, logicalID, evalID string) error {
	if logicalID != "" {
		if err := store.DepRemove(logicalID, evalID); err != nil {
			return err
		}
	}
	eval, err := store.Get(evalID)
	if err != nil {
		return err
	}
	if eval.Status == "closed" {
		return nil
	}
	return setOutcomeAndClose(store, evalID, beadmeta.OutcomeFail)
}

func ensureDep(store beads.Store, issueID, dependsOnID, depType string) error {
	deps, err := store.DepList(issueID, "down")
	if err != nil {
		return err
	}
	for _, dep := range deps {
		if dep.DependsOnID == dependsOnID && dep.Type == depType {
			return nil
		}
	}
	return store.DepAdd(issueID, dependsOnID, depType)
}

func stepRefForRetryBead(bead beads.Bead) string {
	if ref := strings.TrimSpace(bead.Metadata[beadmeta.StepRefMetadataKey]); ref != "" {
		return ref
	}
	return strings.TrimSpace(bead.Ref)
}

func rewriteRetryAttemptRef(ref string, oldAttempt, nextAttempt int) string {
	if ref == "" || oldAttempt < 1 || nextAttempt < 1 {
		return ref
	}
	if rewritten, ok := rewriteAttemptSegment(ref, "run", oldAttempt, nextAttempt); ok {
		return rewritten
	}
	if rewritten, ok := rewriteAttemptSegment(ref, "eval", oldAttempt, nextAttempt); ok {
		return rewritten
	}
	return ref
}
