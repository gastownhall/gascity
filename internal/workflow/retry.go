package workflow

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
)

func processRetryEval(store beads.Store, bead beads.Bead, opts ProcessOptions) (ControlResult, error) {
	attempt, err := strconv.Atoi(bead.Metadata["gc.attempt"])
	if err != nil || attempt < 1 {
		return ControlResult{}, fmt.Errorf("%s: invalid gc.attempt %q", bead.ID, bead.Metadata["gc.attempt"])
	}
	maxAttempts, err := strconv.Atoi(bead.Metadata["gc.max_attempts"])
	if err != nil || maxAttempts < 1 {
		return ControlResult{}, fmt.Errorf("%s: invalid gc.max_attempts %q", bead.ID, bead.Metadata["gc.max_attempts"])
	}
	onExhausted := bead.Metadata["gc.on_exhausted"]
	if onExhausted == "" {
		onExhausted = "hard_fail"
	}

	logicalID := resolveLogicalBeadID(store, bead)
	if logicalID == "" {
		return ControlResult{}, fmt.Errorf("%s: could not resolve logical bead ID", bead.ID)
	}
	logical, err := store.Get(logicalID)
	if err != nil {
		return ControlResult{}, fmt.Errorf("%s: loading logical bead %s: %w", bead.ID, logicalID, err)
	}
	if closedBy, _ := strconv.Atoi(logical.Metadata["gc.closed_by_attempt"]); closedBy >= attempt {
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

	result := classifyRetryAttempt(subject)
	if err := persistRetryEvalResult(store, bead.ID, result); err != nil {
		return ControlResult{}, fmt.Errorf("%s: persisting retry eval result: %w", bead.ID, err)
	}

	switch result.Outcome {
	case "pass":
		if outputJSON := subject.Metadata["gc.output_json"]; outputJSON != "" {
			if err := store.SetMetadata(logicalID, "gc.output_json", outputJSON); err != nil {
				return ControlResult{}, fmt.Errorf("%s: propagating gc.output_json to logical bead: %w", logicalID, err)
			}
		}
		if err := propagateRetrySubjectMetadata(store, logicalID, subject); err != nil {
			return ControlResult{}, fmt.Errorf("%s: propagating subject metadata to logical bead: %w", logicalID, err)
		}
		if err := store.SetMetadataBatch(logicalID, map[string]string{
			"gc.closed_by_attempt": strconv.Itoa(attempt),
			"gc.final_disposition": "pass",
		}); err != nil {
			return ControlResult{}, fmt.Errorf("%s: marking logical pass: %w", logicalID, err)
		}
		if err := setOutcomeAndClose(store, bead.ID, "pass"); err != nil {
			return ControlResult{}, fmt.Errorf("%s: closing passed eval: %w", bead.ID, err)
		}
		if err := setOutcomeAndClose(store, logicalID, "pass"); err != nil {
			return ControlResult{}, fmt.Errorf("%s: closing logical bead: %w", logicalID, err)
		}
		return ControlResult{Processed: true, Action: "pass"}, nil

	case "hard":
		if err := store.SetMetadataBatch(logicalID, map[string]string{
			"gc.closed_by_attempt": strconv.Itoa(attempt),
			"gc.failed_attempt":    strconv.Itoa(attempt),
			"gc.failure_class":     "hard",
			"gc.failure_reason":    result.Reason,
			"gc.final_disposition": "hard_fail",
		}); err != nil {
			return ControlResult{}, fmt.Errorf("%s: marking logical hard failure: %w", logicalID, err)
		}
		if err := setOutcomeAndClose(store, bead.ID, "fail"); err != nil {
			return ControlResult{}, fmt.Errorf("%s: closing hard-failed eval: %w", bead.ID, err)
		}
		if err := setOutcomeAndClose(store, logicalID, "fail"); err != nil {
			return ControlResult{}, fmt.Errorf("%s: closing hard-failed logical bead: %w", logicalID, err)
		}
		return ControlResult{Processed: true, Action: "hard-fail"}, nil

	case "transient":
		if attempt >= maxAttempts {
			if onExhausted == "soft_fail" {
				if err := store.SetMetadataBatch(logicalID, map[string]string{
					"gc.closed_by_attempt": strconv.Itoa(attempt),
					"gc.failed_attempt":    strconv.Itoa(attempt),
					"gc.failure_class":     "transient",
					"gc.failure_reason":    result.Reason,
					"gc.final_disposition": "soft_fail",
				}); err != nil {
					return ControlResult{}, fmt.Errorf("%s: marking logical soft-fail: %w", logicalID, err)
				}
				if err := setOutcomeAndClose(store, bead.ID, "fail"); err != nil {
					return ControlResult{}, fmt.Errorf("%s: closing exhausted eval: %w", bead.ID, err)
				}
				if err := setOutcomeAndClose(store, logicalID, "pass"); err != nil {
					return ControlResult{}, fmt.Errorf("%s: closing soft-failed logical bead: %w", logicalID, err)
				}
				return ControlResult{Processed: true, Action: "soft-fail"}, nil
			}
			if err := store.SetMetadataBatch(logicalID, map[string]string{
				"gc.closed_by_attempt": strconv.Itoa(attempt),
				"gc.failed_attempt":    strconv.Itoa(attempt),
				"gc.failure_class":     "transient",
				"gc.failure_reason":    result.Reason,
				"gc.final_disposition": "hard_fail",
			}); err != nil {
				return ControlResult{}, fmt.Errorf("%s: marking exhausted logical failure: %w", logicalID, err)
			}
			if err := setOutcomeAndClose(store, bead.ID, "fail"); err != nil {
				return ControlResult{}, fmt.Errorf("%s: closing exhausted eval: %w", bead.ID, err)
			}
			if err := setOutcomeAndClose(store, logicalID, "fail"); err != nil {
				return ControlResult{}, fmt.Errorf("%s: closing exhausted logical bead: %w", logicalID, err)
			}
			return ControlResult{Processed: true, Action: "fail"}, nil
		}
	default:
		return ControlResult{}, fmt.Errorf("%s: unsupported retry eval outcome %q", bead.ID, result.Outcome)
	}

	nextAttempt := attempt + 1
	switch bead.Metadata["gc.retry_state"] {
	case "":
		if err := store.SetMetadataBatch(bead.ID, map[string]string{
			"gc.retry_state":  "spawning",
			"gc.next_attempt": strconv.Itoa(nextAttempt),
		}); err != nil {
			return ControlResult{}, fmt.Errorf("%s: recording retry spawn start: %w", bead.ID, err)
		}
	case "spawning":
		// Resume partial append below.
	case "spawned":
		// Resume finalization below without cloning again.
	default:
		return ControlResult{}, fmt.Errorf("%s: unsupported gc.retry_state %q", bead.ID, bead.Metadata["gc.retry_state"])
	}

	if isPoolAssigned(subject) {
		if opts.RecycleSession == nil {
			return ControlResult{}, fmt.Errorf("%s: pooled retry subject %s requires RecycleSession callback", bead.ID, subject.ID)
		}
		if bead.Metadata["gc.retry_session_recycled"] != "true" {
			if subject.Assignee == "" {
				return ControlResult{}, fmt.Errorf("%s: pooled retry subject %s missing assignee", bead.ID, subject.ID)
			}
			if err := opts.RecycleSession(subject); err != nil {
				return ControlResult{}, fmt.Errorf("%s: recycling pooled session %s: %w", bead.ID, subject.Assignee, err)
			}
			if err := store.SetMetadata(bead.ID, "gc.retry_session_recycled", "true"); err != nil {
				return ControlResult{}, fmt.Errorf("%s: recording pooled session recycle: %w", bead.ID, err)
			}
		}
	}

	if bead.Metadata["gc.retry_state"] != "spawned" {
		if err := appendRetryAttempt(store, logicalID, subject, bead, nextAttempt); err != nil {
			return ControlResult{}, fmt.Errorf("%s: appending retry attempt: %w", bead.ID, err)
		}
		if err := store.SetMetadataBatch(bead.ID, map[string]string{
			"gc.retry_state":  "spawned",
			"gc.next_attempt": strconv.Itoa(nextAttempt),
		}); err != nil {
			return ControlResult{}, fmt.Errorf("%s: recording retry spawn complete: %w", bead.ID, err)
		}
	}

	if err := store.SetMetadataBatch(logicalID, map[string]string{
		"gc.retry_count":        strconv.Itoa(attempt),
		"gc.last_failure_class": "transient",
		"gc.failure_reason":     result.Reason,
	}); err != nil {
		return ControlResult{}, fmt.Errorf("%s: recording retry metadata on logical bead: %w", logicalID, err)
	}
	if err := finalizeRetryEval(store, logicalID, bead.ID); err != nil {
		return ControlResult{}, fmt.Errorf("%s: finalizing retry eval: %w", bead.ID, err)
	}
	return ControlResult{Processed: true, Action: "retry"}, nil
}

func resolveRetryRunSubject(store beads.Store, eval beads.Bead, logicalID string, attempt int) (beads.Bead, error) {
	if rootID := strings.TrimSpace(eval.Metadata["gc.root_bead_id"]); rootID != "" && logicalID != "" && attempt > 0 {
		all, err := listByWorkflowRoot(store, rootID)
		if err != nil {
			return beads.Bead{}, err
		}
		attemptStr := strconv.Itoa(attempt)
		for _, candidate := range all {
			if candidate.Metadata["gc.kind"] != "retry-run" {
				continue
			}
			if candidate.Metadata["gc.logical_bead_id"] != logicalID {
				continue
			}
			if candidate.Metadata["gc.attempt"] != attemptStr {
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
	outcome := strings.TrimSpace(subject.Metadata["gc.outcome"])
	switch outcome {
	case "pass":
		if strings.TrimSpace(subject.Metadata["gc.failure_class"]) != "" || strings.TrimSpace(subject.Metadata["gc.failure_reason"]) != "" {
			return retryEvalResult{Outcome: "hard", Reason: "invalid_worker_result_contract"}
		}
		return retryEvalResult{Outcome: "pass"}
	case "fail":
		switch strings.TrimSpace(subject.Metadata["gc.failure_class"]) {
		case "transient":
			return retryEvalResult{Outcome: "transient", Reason: retryFailureReason(subject)}
		case "hard", "":
			return retryEvalResult{Outcome: "hard", Reason: retryFailureReason(subject)}
		default:
			return retryEvalResult{Outcome: "hard", Reason: "invalid_worker_result_contract"}
		}
	case "":
		return retryEvalResult{Outcome: "hard", Reason: "invalid_worker_result_contract"}
	default:
		return retryEvalResult{Outcome: "hard", Reason: "invalid_worker_result_contract"}
	}
}

func retryFailureReason(subject beads.Bead) string {
	reason := strings.TrimSpace(subject.Metadata["gc.failure_reason"])
	if reason == "" {
		return "unspecified"
	}
	return reason
}

func persistRetryEvalResult(store beads.Store, beadID string, result retryEvalResult) error {
	batch := map[string]string{
		"gc.failure_reason": result.Reason,
	}
	switch result.Outcome {
	case "pass":
		batch["gc.outcome"] = "pass"
		batch["gc.failure_class"] = ""
	case "transient":
		batch["gc.outcome"] = "fail"
		batch["gc.failure_class"] = "transient"
	default:
		batch["gc.outcome"] = "fail"
		batch["gc.failure_class"] = "hard"
	}
	return store.SetMetadataBatch(beadID, batch)
}

func propagateRetrySubjectMetadata(store beads.Store, logicalID string, subject beads.Bead) error {
	batch := map[string]string{}
	for key, value := range subject.Metadata {
		if key == "" || strings.HasPrefix(key, "gc.") {
			continue
		}
		batch[key] = value
	}
	if len(batch) == 0 {
		return nil
	}
	return store.SetMetadataBatch(logicalID, batch)
}

func appendRetryAttempt(store beads.Store, logicalID string, prevRun, prevEval beads.Bead, nextAttempt int) error {
	oldAttempt, err := strconv.Atoi(prevRun.Metadata["gc.attempt"])
	if err != nil || oldAttempt < 1 {
		return fmt.Errorf("%s: invalid gc.attempt %q", prevRun.ID, prevRun.Metadata["gc.attempt"])
	}
	rootID := prevRun.Metadata["gc.root_bead_id"]
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
		nextRun, err = store.Create(retryAttemptBead(prevRun, logicalID, runRef, nextAttempt))
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

func retryAttemptBead(prev beads.Bead, logicalID, stepRef string, attempt int) beads.Bead {
	meta := cloneMetadata(prev.Metadata)
	clearRetryEphemera(meta)
	meta["gc.attempt"] = strconv.Itoa(attempt)
	meta["gc.retry_from"] = prev.ID
	meta["gc.step_ref"] = stepRef
	meta["gc.logical_bead_id"] = logicalID
	return beads.Bead{
		Title:       prev.Title,
		Description: prev.Description,
		Type:        prev.Type,
		Assignee:    retryPreservedAssignee(prev),
		From:        prev.From,
		ParentID:    prev.ParentID,
		Ref:         stepRef,
		Labels:      append([]string{}, prev.Labels...),
		Metadata:    meta,
	}
}

func retryEvalBead(prev beads.Bead, logicalID, stepRef string, attempt int) beads.Bead {
	meta := cloneMetadata(prev.Metadata)
	clearRetryEphemera(meta)
	meta["gc.attempt"] = strconv.Itoa(attempt)
	meta["gc.retry_from"] = prev.ID
	meta["gc.step_ref"] = stepRef
	meta["gc.logical_bead_id"] = logicalID
	return beads.Bead{
		Title:       prev.Title,
		Description: prev.Description,
		Type:        prev.Type,
		From:        prev.From,
		ParentID:    prev.ParentID,
		Ref:         stepRef,
		Labels:      append([]string{}, prev.Labels...),
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
	return setOutcomeAndClose(store, evalID, "fail")
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
	if ref := strings.TrimSpace(bead.Metadata["gc.step_ref"]); ref != "" {
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

func isPoolAssigned(bead beads.Bead) bool {
	for _, label := range bead.Labels {
		if strings.HasPrefix(label, "pool:") {
			return true
		}
	}
	return false
}
