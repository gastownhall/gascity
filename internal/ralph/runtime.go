// Package ralph implements the ralph tick loop for routing and checking work.
package ralph

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/convergence"
)

// CheckResult reports whether a check bead was processed and what action it caused.
type CheckResult struct {
	Processed bool
	Action    string
}

// CloseReadyWorkflowHeads closes ready workflow-head beads and records pass/fail
// outcome from their blocking logical steps.
func CloseReadyWorkflowHeads(store beads.Store) (int, error) {
	ready, err := store.Ready()
	if err != nil {
		return 0, err
	}

	workflows := make([]beads.Bead, 0, len(ready))
	for _, bead := range ready {
		if bead.Metadata["gc.kind"] == "workflow" {
			workflows = append(workflows, bead)
		}
	}

	var closed int
	var errs []string
	for _, workflow := range workflows {
		outcome, err := resolveWorkflowOutcome(store, workflow)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", workflow.ID, err))
			continue
		}
		if err := store.SetMetadata(workflow.ID, "gc.outcome", outcome); err != nil {
			errs = append(errs, fmt.Sprintf("%s: setting workflow outcome: %v", workflow.ID, err))
			continue
		}
		if err := store.Close(workflow.ID); err != nil {
			errs = append(errs, fmt.Sprintf("%s: closing workflow: %v", workflow.ID, err))
			continue
		}
		closed++
	}

	if len(errs) > 0 {
		return closed, fmt.Errorf("%d workflow close action(s) failed", len(errs))
	}
	return closed, nil
}

// ProcessCheck runs a deterministic check bead, persists the outcome, and
// either passes, appends a retry, or exhausts the Ralph loop.
func ProcessCheck(bead beads.Bead, cityPath string, store beads.Store) (CheckResult, error) {
	if bead.Metadata["gc.terminal"] == "true" {
		return CheckResult{}, nil
	}
	if bead.Metadata["gc.check_mode"] != "exec" {
		return CheckResult{}, fmt.Errorf("%s: unsupported check mode %q", bead.ID, bead.Metadata["gc.check_mode"])
	}

	attempt, err := strconv.Atoi(bead.Metadata["gc.attempt"])
	if err != nil || attempt < 1 {
		return CheckResult{}, fmt.Errorf("%s: invalid gc.attempt %q", bead.ID, bead.Metadata["gc.attempt"])
	}
	maxAttempts, err := strconv.Atoi(bead.Metadata["gc.max_attempts"])
	if err != nil || maxAttempts < 1 {
		return CheckResult{}, fmt.Errorf("%s: invalid gc.max_attempts %q", bead.ID, bead.Metadata["gc.max_attempts"])
	}

	logicalID := resolveLogicalBeadID(store, bead)
	if logicalID == "" {
		return CheckResult{}, fmt.Errorf("%s: could not resolve logical bead ID", bead.ID)
	}

	checkPath := bead.Metadata["gc.check_path"]
	if checkPath == "" {
		return CheckResult{}, fmt.Errorf("%s: missing gc.check_path", bead.ID)
	}
	workDir := ResolveInheritedMetadata(store, bead, "work_dir", "gc.work_dir")
	scriptBase := cityPath
	if workDir != "" {
		scriptBase = workDir
	}
	scriptPath, err := convergence.ResolveConditionPath(scriptBase, checkPath)
	if err != nil {
		return CheckResult{}, fmt.Errorf("%s: resolving check path: %w", bead.ID, err)
	}

	timeout := convergence.DefaultGateTimeout
	if raw := bead.Metadata["gc.check_timeout"]; raw != "" {
		parsed, parseErr := time.ParseDuration(raw)
		if parseErr != nil {
			return CheckResult{}, fmt.Errorf("%s: parsing gc.check_timeout %q: %w", bead.ID, raw, parseErr)
		}
		timeout = parsed
	}

	result := convergence.RunCondition(context.Background(), scriptPath, convergence.ConditionEnv{
		BeadID:    bead.ID,
		Iteration: attempt,
		CityPath:  cityPath,
		WorkDir:   workDir,
	}, timeout, 0)

	if err := persistCheckResult(store, bead.ID, result); err != nil {
		return CheckResult{}, fmt.Errorf("%s: persisting check result: %w", bead.ID, err)
	}

	if result.Outcome == convergence.GatePass {
		if err := store.Close(bead.ID); err != nil {
			return CheckResult{}, fmt.Errorf("%s: closing passed check: %w", bead.ID, err)
		}
		if err := store.SetMetadata(logicalID, "gc.outcome", "pass"); err != nil {
			return CheckResult{}, fmt.Errorf("%s: setting logical pass outcome: %w", logicalID, err)
		}
		if err := store.Close(logicalID); err != nil {
			return CheckResult{}, fmt.Errorf("%s: closing logical bead: %w", logicalID, err)
		}
		return CheckResult{Processed: true, Action: "pass"}, nil
	}

	if attempt >= maxAttempts {
		if err := store.SetMetadataBatch(logicalID, map[string]string{
			"gc.outcome":        "fail",
			"gc.failed_attempt": strconv.Itoa(attempt),
		}); err != nil {
			return CheckResult{}, fmt.Errorf("%s: marking logical failure: %w", logicalID, err)
		}
		if err := store.Close(bead.ID); err != nil {
			return CheckResult{}, fmt.Errorf("%s: closing failed check: %w", bead.ID, err)
		}
		if err := store.Close(logicalID); err != nil {
			return CheckResult{}, fmt.Errorf("%s: closing failed logical bead: %w", logicalID, err)
		}
		return CheckResult{Processed: true, Action: "fail"}, nil
	}

	runID, err := resolveBlockingRunID(store, bead.ID)
	if err != nil {
		return CheckResult{}, fmt.Errorf("%s: resolving run dependency: %w", bead.ID, err)
	}
	runBead, err := store.Get(runID)
	if err != nil {
		return CheckResult{}, fmt.Errorf("%s: loading run bead %s: %w", bead.ID, runID, err)
	}

	_, _, err = appendRetry(store, logicalID, runBead, bead, attempt+1)
	if err != nil {
		return CheckResult{}, fmt.Errorf("%s: appending retry: %w", bead.ID, err)
	}
	if err := store.Close(bead.ID); err != nil {
		return CheckResult{}, fmt.Errorf("%s: closing failed check after retry append: %w", bead.ID, err)
	}
	if err := store.DepRemove(logicalID, bead.ID); err != nil {
		return CheckResult{}, fmt.Errorf("%s: removing old logical blocker: %w", bead.ID, err)
	}
	return CheckResult{Processed: true, Action: "retry"}, nil
}

// ResolveInheritedMetadata walks parent/root links to find the first matching
// metadata key on a bead or its workflow context.
func ResolveInheritedMetadata(store beads.Store, bead beads.Bead, keys ...string) string {
	current := bead
	visited := map[string]struct{}{}
	for {
		for _, key := range keys {
			if value := current.Metadata[key]; value != "" {
				return value
			}
		}
		if current.ParentID == "" {
			rootID := current.Metadata["gc.root_bead_id"]
			if rootID == "" {
				return ""
			}
			if _, seen := visited[rootID]; seen {
				return ""
			}
			parent, err := store.Get(rootID)
			if err != nil {
				return ""
			}
			visited[rootID] = struct{}{}
			current = parent
			continue
		}
		if _, seen := visited[current.ParentID]; seen {
			return ""
		}
		parent, err := store.Get(current.ParentID)
		if err != nil {
			return ""
		}
		visited[current.ParentID] = struct{}{}
		current = parent
	}
}

func resolveWorkflowOutcome(store beads.Store, workflow beads.Bead) (string, error) {
	deps, err := store.DepList(workflow.ID, "down")
	if err != nil {
		return "", err
	}

	outcome := "pass"
	for _, dep := range deps {
		if dep.Type != "blocks" {
			continue
		}
		blocker, err := store.Get(dep.DependsOnID)
		if err != nil {
			return "", err
		}
		if blocker.Status != "closed" {
			return "", fmt.Errorf("workflow blocker %s is still open", blocker.ID)
		}
		if blocker.Metadata["gc.outcome"] == "fail" {
			outcome = "fail"
		}
	}
	return outcome, nil
}

func persistCheckResult(store beads.Store, beadID string, result convergence.GateResult) error {
	batch := map[string]string{
		"gc.outcome":     result.Outcome,
		"gc.stdout":      result.Stdout,
		"gc.stderr":      result.Stderr,
		"gc.duration_ms": strconv.FormatInt(result.Duration.Milliseconds(), 10),
		"gc.truncated":   strconv.FormatBool(result.Truncated),
	}
	if result.ExitCode != nil {
		batch["gc.exit_code"] = strconv.Itoa(*result.ExitCode)
	} else {
		batch["gc.exit_code"] = ""
	}
	return store.SetMetadataBatch(beadID, batch)
}

func appendRetry(store beads.Store, logicalID string, prevRun, prevCheck beads.Bead, nextAttempt int) (string, string, error) {
	runMeta := cloneMetadata(prevRun.Metadata)
	clearRetryEphemera(runMeta)
	runMeta["gc.attempt"] = strconv.Itoa(nextAttempt)
	runMeta["gc.routed_to"] = ""
	runMeta["gc.retry_from"] = prevRun.ID
	runMeta["gc.logical_bead_id"] = logicalID
	newRun, err := store.Create(beads.Bead{
		Title:       prevRun.Title,
		Description: prevRun.Description,
		Type:        prevRun.Type,
		ParentID:    prevRun.ParentID,
		Labels:      append([]string{}, prevRun.Labels...),
		Metadata:    runMeta,
	})
	if err != nil {
		return "", "", err
	}

	runDeps, err := store.DepList(prevRun.ID, "down")
	if err != nil {
		return "", "", fmt.Errorf("listing prior run deps: %w", err)
	}
	for _, dep := range runDeps {
		if dep.Type != "blocks" && dep.Type != "waits-for" && dep.Type != "conditional-blocks" {
			continue
		}
		if err := store.DepAdd(newRun.ID, dep.DependsOnID, dep.Type); err != nil {
			return "", "", fmt.Errorf("copying run dep %s->%s: %w", newRun.ID, dep.DependsOnID, err)
		}
	}

	checkMeta := cloneMetadata(prevCheck.Metadata)
	clearRetryEphemera(checkMeta)
	checkMeta["gc.attempt"] = strconv.Itoa(nextAttempt)
	checkMeta["gc.retry_from"] = prevCheck.ID
	checkMeta["gc.terminal"] = ""
	checkMeta["gc.logical_bead_id"] = logicalID
	newCheck, err := store.Create(beads.Bead{
		Title:       prevCheck.Title,
		Description: prevCheck.Description,
		Type:        prevCheck.Type,
		ParentID:    prevCheck.ParentID,
		Labels:      append([]string{}, prevCheck.Labels...),
		Metadata:    checkMeta,
	})
	if err != nil {
		return "", "", err
	}

	if err := store.DepAdd(newCheck.ID, newRun.ID, "blocks"); err != nil {
		return "", "", fmt.Errorf("creating check->run dep: %w", err)
	}
	if err := store.DepAdd(logicalID, newCheck.ID, "blocks"); err != nil {
		return "", "", fmt.Errorf("creating logical->check dep: %w", err)
	}

	return newRun.ID, newCheck.ID, nil
}

func resolveBlockingRunID(store beads.Store, checkID string) (string, error) {
	deps, err := store.DepList(checkID, "down")
	if err != nil {
		return "", err
	}
	for _, dep := range deps {
		if dep.Type == "blocks" {
			return dep.DependsOnID, nil
		}
	}
	return "", fmt.Errorf("no blocking run dep found")
}

func resolveLogicalBeadID(store beads.Store, bead beads.Bead) string {
	if bead.Metadata["gc.logical_bead_id"] != "" {
		return bead.Metadata["gc.logical_bead_id"]
	}

	deps, err := store.DepList(bead.ID, "up")
	if err == nil {
		for _, dep := range deps {
			if dep.Type != "blocks" {
				continue
			}
			candidate, getErr := store.Get(dep.IssueID)
			if getErr != nil {
				continue
			}
			if candidate.Metadata["gc.kind"] == "ralph" {
				return candidate.ID
			}
		}
	}
	return ""
}

func cloneMetadata(meta map[string]string) map[string]string {
	if len(meta) == 0 {
		return nil
	}
	out := make(map[string]string, len(meta))
	for k, v := range meta {
		out[k] = v
	}
	return out
}

func clearRetryEphemera(meta map[string]string) {
	if meta == nil {
		return
	}
	for _, key := range []string{
		"gc.routed_to",
		"gc.outcome",
		"gc.exit_code",
		"gc.stdout",
		"gc.stderr",
		"gc.duration_ms",
		"gc.truncated",
		"gc.terminal",
	} {
		delete(meta, key)
	}
}
