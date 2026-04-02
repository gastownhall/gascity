package dispatch

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/convergence"
	"github.com/gastownhall/gascity/internal/molecule"
)

func processRalphCheck(store beads.Store, bead beads.Bead, opts ProcessOptions) (ControlResult, error) {
	if bead.Metadata["gc.terminal"] == "true" {
		return ControlResult{}, nil
	}
	if bead.Metadata["gc.check_mode"] != "exec" {
		return ControlResult{}, fmt.Errorf("%s: unsupported check mode %q", bead.ID, bead.Metadata["gc.check_mode"])
	}

	attempt, err := strconv.Atoi(bead.Metadata["gc.attempt"])
	if err != nil || attempt < 1 {
		return ControlResult{}, fmt.Errorf("%s: invalid gc.attempt %q", bead.ID, bead.Metadata["gc.attempt"])
	}
	maxAttempts, err := strconv.Atoi(bead.Metadata["gc.max_attempts"])
	if err != nil || maxAttempts < 1 {
		return ControlResult{}, fmt.Errorf("%s: invalid gc.max_attempts %q", bead.ID, bead.Metadata["gc.max_attempts"])
	}

	logicalID := resolveLogicalBeadID(store, bead)
	if logicalID == "" {
		return ControlResult{}, fmt.Errorf("%s: could not resolve logical bead ID", bead.ID)
	}

	subjectID, err := resolveBlockingSubjectID(store, bead.ID)
	if err != nil {
		return ControlResult{}, fmt.Errorf("%s: resolving subject: %w", bead.ID, err)
	}
	subject, err := store.Get(subjectID)
	if err != nil {
		return ControlResult{}, fmt.Errorf("%s: loading subject %s: %w", bead.ID, subjectID, err)
	}

	result, err := runRalphCheck(store, bead, subject, attempt, opts)
	if err != nil {
		return ControlResult{}, err
	}
	tracef("ralph check-result bead=%s logical=%s attempt=%d outcome=%s exit=%v", bead.ID, logicalID, attempt, result.Outcome, result.ExitCode)
	if err := persistCheckResult(store, bead.ID, result); err != nil {
		return ControlResult{}, fmt.Errorf("%s: persisting check result: %w", bead.ID, err)
	}

	if result.Outcome == convergence.GatePass {
		if err := setOutcomeAndClose(store, bead.ID, "pass"); err != nil {
			return ControlResult{}, fmt.Errorf("%s: closing passed check: %w", bead.ID, err)
		}
		if outputJSON := subject.Metadata["gc.output_json"]; outputJSON != "" {
			if err := store.SetMetadata(logicalID, "gc.output_json", outputJSON); err != nil {
				return ControlResult{}, fmt.Errorf("%s: propagating gc.output_json to logical bead: %w", logicalID, err)
			}
		}
		if err := setOutcomeAndClose(store, logicalID, "pass"); err != nil {
			return ControlResult{}, fmt.Errorf("%s: closing logical bead: %w", logicalID, err)
		}
		return ControlResult{Processed: true, Action: "pass"}, nil
	}

	if attempt >= maxAttempts {
		if err := store.SetMetadataBatch(logicalID, map[string]string{
			"gc.outcome":        "fail",
			"gc.failed_attempt": strconv.Itoa(attempt),
		}); err != nil {
			return ControlResult{}, fmt.Errorf("%s: marking logical failure: %w", logicalID, err)
		}
		if err := setOutcomeAndClose(store, bead.ID, "fail"); err != nil {
			return ControlResult{}, fmt.Errorf("%s: closing failed check: %w", bead.ID, err)
		}
		if err := setOutcomeAndClose(store, logicalID, "fail"); err != nil {
			return ControlResult{}, fmt.Errorf("%s: closing failed logical bead: %w", logicalID, err)
		}
		return ControlResult{Processed: true, Action: "fail"}, nil
	}

	nextAttempt := attempt + 1
	switch bead.Metadata["gc.retry_state"] {
	case "":
		tracef("ralph retry-mark-spawning bead=%s next=%d", bead.ID, nextAttempt)
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
	if bead.Metadata["gc.retry_state"] != "spawned" {
		tracef("ralph retry-append-start bead=%s next=%d", bead.ID, nextAttempt)
		if _, err := appendRalphRetry(store, logicalID, subject, bead, nextAttempt); err != nil {
			return ControlResult{}, fmt.Errorf("%s: appending retry: %w", bead.ID, err)
		}
		tracef("ralph retry-append-done bead=%s next=%d", bead.ID, nextAttempt)
		if err := store.SetMetadataBatch(bead.ID, map[string]string{
			"gc.retry_state":  "spawned",
			"gc.next_attempt": strconv.Itoa(nextAttempt),
		}); err != nil {
			return ControlResult{}, fmt.Errorf("%s: recording retry spawn complete: %w", bead.ID, err)
		}
	}
	tracef("ralph retry-finalize-start bead=%s next=%d", bead.ID, nextAttempt)
	if err := finalizeRalphRetry(store, logicalID, bead.ID); err != nil {
		return ControlResult{}, fmt.Errorf("%s: finalizing retry: %w", bead.ID, err)
	}
	tracef("ralph retry-finalize-done bead=%s next=%d", bead.ID, nextAttempt)
	return ControlResult{Processed: true, Action: "retry"}, nil
}

func runRalphCheck(store beads.Store, bead, subject beads.Bead, attempt int, opts ProcessOptions) (convergence.GateResult, error) {
	if subject.Metadata["gc.outcome"] == "fail" {
		exitCode := 1
		return convergence.GateResult{
			Outcome:   convergence.GateFail,
			ExitCode:  &exitCode,
			Stderr:    fmt.Sprintf("attempt subject %s already failed", subject.ID),
			Truncated: false,
		}, nil
	}

	checkPath := bead.Metadata["gc.check_path"]
	if checkPath == "" {
		return convergence.GateResult{}, fmt.Errorf("%s: missing gc.check_path", bead.ID)
	}
	cityPath := opts.CityPath
	if cityPath == "" {
		cityPath = resolveInheritedMetadata(store, bead, "gc.city_path")
	}
	if cityPath == "" {
		return convergence.GateResult{}, fmt.Errorf("%s: missing city path for exec check", bead.ID)
	}

	workDir := resolveInheritedMetadata(store, bead, "work_dir", "gc.work_dir")
	resolvedWorkDir := ""
	if workDir != "" {
		if filepath.IsAbs(workDir) {
			resolvedWorkDir = workDir
		} else {
			resolvedWorkDir = filepath.Join(cityPath, workDir)
		}
	}
	scriptBase := cityPath
	if resolvedWorkDir != "" {
		scriptBase = resolvedWorkDir
	}
	scriptPath, err := convergence.ResolveConditionPath(scriptBase, checkPath)
	if err != nil {
		return convergence.GateResult{}, fmt.Errorf("%s: resolving check path: %w", bead.ID, err)
	}

	timeout := convergence.DefaultGateTimeout
	if raw := bead.Metadata["gc.check_timeout"]; raw != "" {
		parsed, parseErr := time.ParseDuration(raw)
		if parseErr != nil {
			return convergence.GateResult{}, fmt.Errorf("%s: parsing gc.check_timeout %q: %w", bead.ID, raw, parseErr)
		}
		timeout = parsed
	}

	result := convergence.RunCondition(context.Background(), scriptPath, convergence.ConditionEnv{
		BeadID:    bead.ID,
		Iteration: attempt,
		CityPath:  cityPath,
		WorkDir:   resolvedWorkDir,
	}, timeout, 0)
	return result, nil
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

func appendRalphRetry(store beads.Store, logicalID string, prevSubject, prevCheck beads.Bead, nextAttempt int) (map[string]string, error) {
	var rootBeads []beads.Bead
	rootID := prevSubject.Metadata["gc.root_bead_id"]
	if rootID != "" {
		var err error
		rootBeads, err = listByWorkflowRoot(store, rootID)
		if err != nil {
			return nil, err
		}
	}

	attemptSet, err := collectRalphAttemptBeadsFromBeads(rootBeads, prevSubject)
	if err != nil {
		return nil, err
	}

	oldAttempt, _ := strconv.Atoi(prevSubject.Metadata["gc.attempt"])
	oldScopeRef := prevSubject.Metadata["gc.step_ref"]
	if oldScopeRef == "" {
		oldScopeRef = prevSubject.ID
	}
	newScopeRef := rewriteRalphAttemptRef(oldScopeRef, oldAttempt, nextAttempt)
	if newScopeRef == oldScopeRef && prevSubject.Metadata["gc.step_ref"] == "" {
		newScopeRef = fmt.Sprintf("%s.retry.%d", prevSubject.ID, nextAttempt)
	}
	if existing, err := resolveExistingRalphRetryFromBeads(store, rootBeads, logicalID, prevSubject, prevCheck, attemptSet, oldAttempt, nextAttempt, oldScopeRef, newScopeRef); err != nil {
		return nil, err
	} else if len(existing) > 0 {
		if newCheckID := existing[prevCheck.ID]; newCheckID != "" {
			if err := store.DepAdd(logicalID, newCheckID, "blocks"); err != nil {
				return nil, fmt.Errorf("restoring logical->check dep: %w", err)
			}
		}
		return existing, nil
	}
	if molecule.GraphApplyEnabled {
		if applier, ok := store.(beads.GraphApplyStore); ok {
			return appendRalphRetryViaGraphApply(store, applier, logicalID, prevSubject, prevCheck, attemptSet, oldAttempt, nextAttempt, oldScopeRef, newScopeRef)
		}
	}
	return appendRalphRetryLegacy(store, logicalID, prevSubject, prevCheck, attemptSet, oldAttempt, nextAttempt, oldScopeRef, newScopeRef)
}

func appendRalphRetryLegacy(store beads.Store, logicalID string, prevSubject, prevCheck beads.Bead, attemptSet map[string]beads.Bead, oldAttempt, nextAttempt int, oldScopeRef, newScopeRef string) (map[string]string, error) {
	mapping := make(map[string]string, len(attemptSet)+1)
	pendingAssignees := make(map[string]string, len(attemptSet)+1)

	ordered := make([]beads.Bead, 0, len(attemptSet))
	for _, bead := range attemptSet {
		ordered = append(ordered, bead)
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].CreatedAt.Equal(ordered[j].CreatedAt) {
			return ordered[i].ID < ordered[j].ID
		}
		return ordered[i].CreatedAt.Before(ordered[j].CreatedAt)
	})

	// Create the subject first so scope_ref remapping is stable for nested attempts.
	subjectMeta := cloneMetadata(prevSubject.Metadata)
	clearRetryEphemera(subjectMeta)
	subjectMeta["gc.attempt"] = strconv.Itoa(nextAttempt)
	subjectMeta["gc.retry_from"] = prevSubject.ID
	subjectMeta["gc.logical_bead_id"] = logicalID
	subjectMeta["gc.step_ref"] = rewriteRetryStepRef(prevSubject.Metadata, prevSubject.Ref, oldScopeRef, newScopeRef, oldAttempt, nextAttempt)
	if controlFor := strings.TrimSpace(subjectMeta["gc.control_for"]); controlFor != "" {
		subjectMeta["gc.control_for"] = rewriteRetryControlFor(subjectMeta, controlFor, oldScopeRef, newScopeRef, oldAttempt, nextAttempt)
	}
	newSubject, err := store.Create(beads.Bead{
		Title:       prevSubject.Title,
		Description: prevSubject.Description,
		Type:        prevSubject.Type,
		Ref:         cloneRef(subjectMeta, prevSubject.Ref),
		ParentID:    prevSubject.ParentID,
		Assignee:    "",
		Labels:      append([]string{}, prevSubject.Labels...),
		Metadata:    subjectMeta,
	})
	if err != nil {
		return nil, err
	}
	mapping[prevSubject.ID] = newSubject.ID
	if preservedAssignee := retryPreservedAssignee(prevSubject); preservedAssignee != "" {
		pendingAssignees[prevSubject.ID] = preservedAssignee
	}

	for _, old := range ordered {
		if old.ID == prevSubject.ID {
			continue
		}
		meta := cloneMetadata(old.Metadata)
		clearRetryEphemera(meta)
		meta["gc.attempt"] = strconv.Itoa(nextAttempt)
		meta["gc.retry_from"] = old.ID
		if currentScopeRef := strings.TrimSpace(meta["gc.scope_ref"]); currentScopeRef != "" {
			meta["gc.scope_ref"] = rewriteRetryScopeRef(currentScopeRef, oldScopeRef, newScopeRef, prevSubject.ID)
		}
		meta["gc.step_ref"] = rewriteRetryStepRef(meta, old.Ref, oldScopeRef, newScopeRef, oldAttempt, nextAttempt)
		if controlFor := strings.TrimSpace(meta["gc.control_for"]); controlFor != "" {
			meta["gc.control_for"] = rewriteRetryControlFor(meta, controlFor, oldScopeRef, newScopeRef, oldAttempt, nextAttempt)
		}
		created, err := store.Create(beads.Bead{
			Title:       old.Title,
			Description: old.Description,
			Type:        old.Type,
			Ref:         cloneRef(meta, old.Ref),
			ParentID:    old.ParentID,
			Assignee:    "",
			Labels:      append([]string{}, old.Labels...),
			Metadata:    meta,
		})
		if err != nil {
			return nil, err
		}
		mapping[old.ID] = created.ID
		if preservedAssignee := retryPreservedAssignee(old); preservedAssignee != "" {
			pendingAssignees[old.ID] = preservedAssignee
		}
	}

	checkMeta := cloneMetadata(prevCheck.Metadata)
	clearRetryEphemera(checkMeta)
	checkMeta["gc.attempt"] = strconv.Itoa(nextAttempt)
	checkMeta["gc.retry_from"] = prevCheck.ID
	checkMeta["gc.terminal"] = ""
	checkMeta["gc.logical_bead_id"] = logicalID
	checkMeta["gc.step_ref"] = rewriteRetryStepRef(checkMeta, prevCheck.Ref, oldScopeRef, newScopeRef, oldAttempt, nextAttempt)
	if controlFor := strings.TrimSpace(checkMeta["gc.control_for"]); controlFor != "" {
		checkMeta["gc.control_for"] = rewriteRetryControlFor(checkMeta, controlFor, oldScopeRef, newScopeRef, oldAttempt, nextAttempt)
	}
	newCheck, err := store.Create(beads.Bead{
		Title:       prevCheck.Title,
		Description: prevCheck.Description,
		Type:        prevCheck.Type,
		Ref:         cloneRef(checkMeta, prevCheck.Ref),
		ParentID:    prevCheck.ParentID,
		Assignee:    "",
		Labels:      append([]string{}, prevCheck.Labels...),
		Metadata:    checkMeta,
	})
	if err != nil {
		return nil, err
	}
	mapping[prevCheck.ID] = newCheck.ID
	if preservedAssignee := retryPreservedAssignee(prevCheck); preservedAssignee != "" {
		pendingAssignees[prevCheck.ID] = preservedAssignee
	}

	for _, old := range ordered {
		newID := mapping[old.ID]
		if newID == "" {
			continue
		}
		if remapped := remappedLogicalBeadID(mapping, old.Metadata["gc.logical_bead_id"]); remapped != "" {
			if err := store.SetMetadata(newID, "gc.logical_bead_id", remapped); err != nil {
				return nil, fmt.Errorf("remapping logical bead for retry clone %s: %w", newID, err)
			}
		}
	}
	if remapped := remappedLogicalBeadID(mapping, prevCheck.Metadata["gc.logical_bead_id"]); remapped != "" {
		if err := store.SetMetadata(newCheck.ID, "gc.logical_bead_id", remapped); err != nil {
			return nil, fmt.Errorf("remapping logical bead for retry check %s: %w", newCheck.ID, err)
		}
	}

	for _, old := range ordered {
		if err := copyRetryDeps(store, old.ID, mapping[old.ID], mapping); err != nil {
			return nil, err
		}
	}
	if err := copyRetryDeps(store, prevCheck.ID, newCheck.ID, mapping); err != nil {
		return nil, err
	}
	if err := store.DepAdd(logicalID, newCheck.ID, "blocks"); err != nil {
		return nil, fmt.Errorf("creating logical->check dep: %w", err)
	}
	for _, oldID := range sortedRetryAssigneeIDs(pendingAssignees) {
		assignee := pendingAssignees[oldID]
		newID := mapping[oldID]
		if assignee == "" || newID == "" {
			continue
		}
		if err := store.Update(newID, beads.UpdateOpts{Assignee: &assignee}); err != nil {
			return nil, fmt.Errorf("assigning retry bead %s: %w", newID, err)
		}
	}

	return mapping, nil
}

func appendRalphRetryViaGraphApply(store beads.Store, applier beads.GraphApplyStore, logicalID string, prevSubject, prevCheck beads.Bead, attemptSet map[string]beads.Bead, oldAttempt, nextAttempt int, oldScopeRef, newScopeRef string) (map[string]string, error) {
	ordered := make([]beads.Bead, 0, len(attemptSet))
	for _, bead := range attemptSet {
		ordered = append(ordered, bead)
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].CreatedAt.Equal(ordered[j].CreatedAt) {
			return ordered[i].ID < ordered[j].ID
		}
		return ordered[i].CreatedAt.Before(ordered[j].CreatedAt)
	})

	attemptIDs := make(map[string]bool, len(attemptSet)+1)
	for _, bead := range ordered {
		attemptIDs[bead.ID] = true
	}
	attemptIDs[prevCheck.ID] = true

	plan := &beads.GraphApplyPlan{
		CommitMessage: fmt.Sprintf("gc: ralph retry %s attempt %d", logicalID, nextAttempt),
		Nodes:         make([]beads.GraphApplyNode, 0, len(attemptSet)+1),
		Edges:         make([]beads.GraphApplyEdge, 0, len(attemptSet)*2),
	}

	plan.Nodes = append(plan.Nodes, buildRalphRetryGraphNode(prevSubject, logicalID, oldScopeRef, newScopeRef, oldAttempt, nextAttempt, attemptIDs))
	for _, old := range ordered {
		if old.ID == prevSubject.ID {
			continue
		}
		plan.Nodes = append(plan.Nodes, buildRalphRetryGraphNode(old, logicalID, oldScopeRef, newScopeRef, oldAttempt, nextAttempt, attemptIDs))
	}
	plan.Nodes = append(plan.Nodes, buildRalphRetryGraphNode(prevCheck, logicalID, oldScopeRef, newScopeRef, oldAttempt, nextAttempt, attemptIDs))

	for _, old := range ordered {
		if err := appendRalphRetryGraphEdges(plan, store, old.ID, attemptIDs); err != nil {
			return nil, err
		}
	}
	if err := appendRalphRetryGraphEdges(plan, store, prevCheck.ID, attemptIDs); err != nil {
		return nil, err
	}
	plan.Edges = append(plan.Edges, beads.GraphApplyEdge{
		FromID: logicalID,
		ToKey:  prevCheck.ID,
		Type:   "blocks",
	})

	tracef("ralph retry-graph-apply-start logical=%s next=%d nodes=%d edges=%d", logicalID, nextAttempt, len(plan.Nodes), len(plan.Edges))
	applied, err := applier.ApplyGraphPlan(context.Background(), plan)
	if err != nil {
		return nil, err
	}
	if err := beads.ValidateGraphApplyResult(plan, applied); err != nil {
		return nil, err
	}
	tracef("ralph retry-graph-apply-done logical=%s next=%d nodes=%d", logicalID, nextAttempt, len(applied.IDs))

	mapping := make(map[string]string, len(applied.IDs))
	for oldID, newID := range applied.IDs {
		mapping[oldID] = newID
	}
	return mapping, nil
}

func buildRalphRetryGraphNode(old beads.Bead, logicalID, oldScopeRef, newScopeRef string, oldAttempt, nextAttempt int, attemptIDs map[string]bool) beads.GraphApplyNode {
	meta := cloneMetadata(old.Metadata)
	clearRetryEphemera(meta)
	meta["gc.attempt"] = strconv.Itoa(nextAttempt)
	meta["gc.retry_from"] = old.ID
	if currentScopeRef := strings.TrimSpace(meta["gc.scope_ref"]); currentScopeRef != "" {
		meta["gc.scope_ref"] = rewriteRetryScopeRef(currentScopeRef, oldScopeRef, newScopeRef, old.ID)
	}
	meta["gc.step_ref"] = rewriteRetryStepRef(meta, old.Ref, oldScopeRef, newScopeRef, oldAttempt, nextAttempt)
	if controlFor := strings.TrimSpace(meta["gc.control_for"]); controlFor != "" {
		meta["gc.control_for"] = rewriteRetryControlFor(meta, controlFor, oldScopeRef, newScopeRef, oldAttempt, nextAttempt)
	}
	metadataRefs := map[string]string(nil)
	if oldLogicalID := strings.TrimSpace(old.Metadata["gc.logical_bead_id"]); oldLogicalID != "" {
		if attemptIDs[oldLogicalID] {
			metadataRefs = make(map[string]string, 1)
			metadataRefs["gc.logical_bead_id"] = oldLogicalID
			delete(meta, "gc.logical_bead_id")
		} else {
			meta["gc.logical_bead_id"] = oldLogicalID
		}
	} else if kind := meta["gc.kind"]; kind == "scope" || kind == "check" {
		meta["gc.logical_bead_id"] = logicalID
	}
	parentKey := ""
	parentID := old.ParentID
	if attemptIDs[old.ParentID] {
		parentKey = old.ParentID
		parentID = ""
	}
	return beads.GraphApplyNode{
		Key:               old.ID,
		Title:             old.Title,
		Description:       old.Description,
		Type:              old.Type,
		Assignee:          retryPreservedAssignee(old),
		AssignAfterCreate: retryPreservedAssignee(old) != "",
		From:              old.From,
		Labels:            append([]string{}, old.Labels...),
		Metadata:          meta,
		MetadataRefs:      metadataRefs,
		ParentKey:         parentKey,
		ParentID:          parentID,
	}
}

func retryPreservedAssignee(bead beads.Bead) string {
	if bead.Assignee == "" {
		return ""
	}
	for _, label := range bead.Labels {
		if strings.HasPrefix(label, "pool:") {
			return ""
		}
	}
	return bead.Assignee
}

func appendRalphRetryGraphEdges(plan *beads.GraphApplyPlan, store beads.Store, oldID string, attemptIDs map[string]bool) error {
	deps, err := store.DepList(oldID, "down")
	if err != nil {
		return err
	}
	for _, dep := range deps {
		edge := beads.GraphApplyEdge{
			FromKey: oldID,
			Type:    dep.Type,
		}
		if attemptIDs[dep.DependsOnID] {
			edge.ToKey = dep.DependsOnID
		} else {
			edge.ToID = dep.DependsOnID
		}
		plan.Edges = append(plan.Edges, edge)
	}
	return nil
}

func finalizeRalphRetry(store beads.Store, logicalID, checkID string) error {
	if err := store.DepRemove(logicalID, checkID); err != nil {
		return err
	}
	check, err := store.Get(checkID)
	if err != nil {
		return err
	}
	if check.Status == "closed" {
		return nil
	}
	return setOutcomeAndClose(store, checkID, "fail")
}

func collectRalphAttemptBeads(store beads.Store, subject beads.Bead) (map[string]beads.Bead, error) {
	if subject.Metadata["gc.kind"] != "scope" {
		return map[string]beads.Bead{subject.ID: subject}, nil
	}
	rootID := subject.Metadata["gc.root_bead_id"]
	if rootID == "" {
		return nil, fmt.Errorf("%s: missing gc.root_bead_id", subject.ID)
	}
	all, err := listByWorkflowRoot(store, rootID)
	if err != nil {
		return nil, err
	}
	return collectRalphAttemptBeadsFromBeads(all, subject)
}

func collectRalphAttemptBeadsFromBeads(all []beads.Bead, subject beads.Bead) (map[string]beads.Bead, error) {
	out := map[string]beads.Bead{
		subject.ID: subject,
	}
	if subject.Metadata["gc.kind"] != "scope" {
		return out, nil
	}
	scopeRef := subject.Metadata["gc.step_ref"]
	if scopeRef == "" {
		scopeRef = subject.ID
	}
	for _, bead := range all {
		if bead.Metadata["gc.dynamic_fragment"] == "true" {
			continue
		}
		if matchesRalphRetryScope(bead.Metadata["gc.scope_ref"], scopeRef, subject.ID) {
			out[bead.ID] = bead
		}
	}
	return out, nil
}

func matchesRalphRetryScope(beadScopeRef, scopeRef, subjectID string) bool {
	beadScopeRef = strings.TrimSpace(beadScopeRef)
	if beadScopeRef == "" {
		return false
	}
	if beadScopeRef == scopeRef || beadScopeRef == subjectID {
		return true
	}
	return scopeRef != "" && strings.HasSuffix(scopeRef, "."+beadScopeRef)
}

func rewriteRetryScopeRef(beadScopeRef, oldScopeRef, newScopeRef, subjectID string) string {
	if !matchesRalphRetryScope(beadScopeRef, oldScopeRef, subjectID) {
		return beadScopeRef
	}
	return newScopeRef
}

func copyRetryDeps(store beads.Store, oldID, newID string, mapping map[string]string) error {
	deps, err := store.DepList(oldID, "down")
	if err != nil {
		return err
	}
	for _, dep := range deps {
		if dep.Type != "blocks" && dep.Type != "waits-for" && dep.Type != "conditional-blocks" {
			continue
		}
		targetID := dep.DependsOnID
		if mapped, ok := mapping[targetID]; ok {
			targetID = mapped
		} else {
			target, err := store.Get(dep.DependsOnID)
			if err != nil {
				return err
			}
			if target.Metadata["gc.dynamic_fragment"] == "true" {
				continue
			}
		}
		if err := store.DepAdd(newID, targetID, dep.Type); err != nil {
			return fmt.Errorf("copying dep %s->%s (%s): %w", newID, targetID, dep.Type, err)
		}
	}
	return nil
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
			switch candidate.Metadata["gc.kind"] {
			case "ralph", "retry":
				return candidate.ID
			}
		}
	}
	if rootID := bead.Metadata["gc.root_bead_id"]; rootID != "" {
		// Build candidate refs: scope-check controlled ref first (most specific),
		// then logicalStepRefForAttemptBead (may trim attempt patterns).
		var candidates []string
		if controlledRef := scopeCheckControlledStepRef(bead); controlledRef != "" {
			candidates = append(candidates, controlledRef)
		}
		if logicalRef := logicalStepRefForAttemptBead(bead); logicalRef != "" {
			alreadyHave := false
			for _, c := range candidates {
				if c == logicalRef {
					alreadyHave = true
					break
				}
			}
			if !alreadyHave {
				candidates = append(candidates, logicalRef)
			}
		}
		if len(candidates) > 0 {
			all, listErr := listByWorkflowRoot(store, rootID)
			if listErr == nil {
				for _, ref := range candidates {
					for _, candidate := range all {
						switch candidate.Metadata["gc.kind"] {
						case "ralph", "retry":
						default:
							continue
						}
						candidateRef := strings.TrimSpace(candidate.Metadata["gc.step_ref"])
						if candidateRef == "" {
							candidateRef = strings.TrimSpace(candidate.Ref)
						}
						if candidateRef == ref {
							return candidate.ID
						}
					}
				}
			}
		}
	}
	return ""
}

func logicalStepRefForAttemptBead(bead beads.Bead) string {
	stepRef := strings.TrimSpace(bead.Metadata["gc.step_ref"])
	if stepRef == "" {
		stepRef = strings.TrimSpace(bead.Ref)
	}
	if stepRef == "" {
		return ""
	}
	kind := strings.TrimSpace(bead.Metadata["gc.kind"])
	normalized := stepRef
	if kind == "scope-check" && strings.HasSuffix(normalized, "-scope-check") {
		normalized = strings.TrimSuffix(normalized, "-scope-check")
	}
	attempt := strings.TrimSpace(bead.Metadata["gc.attempt"])
	if trimmed, ok := trimAttemptStepRefForKind(normalized, kind, attempt); ok {
		return trimmed
	}
	// For scope-check beads, prefer trimming attempt patterns from the
	// normalized ref (e.g., .eval.1 from a nested retry scope-check) to
	// resolve to the logical retry/ralph step. Fall back to normalized ref
	// for flat scope-checks that don't have attempt patterns.
	if kind == "scope-check" && normalized != stepRef {
		if trimmed, ok := trimRightmostAttemptStepRef(normalized); ok {
			return trimmed
		}
		return normalized
	}
	if trimmed, ok := trimRightmostAttemptStepRef(normalized); ok {
		return trimmed
	}
	return ""
}

func scopeCheckControlledStepRef(bead beads.Bead) string {
	if strings.TrimSpace(bead.Metadata["gc.kind"]) != "scope-check" {
		return ""
	}
	stepRef := strings.TrimSpace(bead.Metadata["gc.step_ref"])
	if stepRef == "" {
		stepRef = strings.TrimSpace(bead.Ref)
	}
	if stepRef == "" || !strings.HasSuffix(stepRef, "-scope-check") {
		return ""
	}
	return strings.TrimSuffix(stepRef, "-scope-check")
}

func trimAttemptStepRefForKind(stepRef, kind, attempt string) (string, bool) {
	if attempt == "" {
		return "", false
	}
	switch kind {
	case "run", "scope", "retry-run":
		return trimAttemptStepRefSuffix(stepRef, ".run."+attempt)
	case "check":
		return trimAttemptStepRefSuffix(stepRef, ".check."+attempt)
	case "retry-eval":
		return trimAttemptStepRefSuffix(stepRef, ".eval."+attempt)
	default:
		return "", false
	}
}

func trimRightmostAttemptStepRef(stepRef string) (string, bool) {
	best := -1
	for _, prefix := range []string{".run.", ".check.", ".eval.", ".iteration.", ".attempt."} {
		if idx := strings.LastIndex(stepRef, prefix); idx > best {
			best = idx
		}
	}
	if best <= 0 {
		return "", false
	}
	return stepRef[:best], true
}

func trimAttemptStepRefSuffix(stepRef, suffix string) (string, bool) {
	if suffix == "" || !strings.HasSuffix(stepRef, suffix) {
		return "", false
	}
	return strings.TrimSuffix(stepRef, suffix), true
}

func resolveInheritedMetadata(store beads.Store, bead beads.Bead, keys ...string) string {
	current := bead
	visited := map[string]struct{}{}
	for {
		for _, key := range keys {
			if value := current.Metadata[key]; value != "" {
				return value
			}
		}
		if parentID := current.ParentID; parentID != "" {
			if _, seen := visited[parentID]; !seen {
				parent, err := store.Get(parentID)
				if err == nil {
					visited[parentID] = struct{}{}
					current = parent
					continue
				}
			}
		}
		rootID := current.Metadata["gc.root_bead_id"]
		if rootID != "" && current.ID != rootID {
			if _, seen := visited[rootID]; !seen {
				parent, err := store.Get(rootID)
				if err == nil {
					visited[rootID] = struct{}{}
					current = parent
					continue
				}
			}
		}
		return ""
	}
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
		"gc.outcome",
		"gc.exit_code",
		"gc.stdout",
		"gc.stderr",
		"gc.output_json",
		"gc.duration_ms",
		"gc.truncated",
		"gc.terminal",
		"gc.failed_attempt",
		"gc.fanout_state",
		"gc.spawned_count",
		"gc.retry_state",
		"gc.next_attempt",
		"gc.partial_retry",
		"gc.failure_class",
		"gc.failure_reason",
		"gc.final_disposition",
		"gc.closed_by_attempt",
		"gc.last_failure_class",
		"gc.retry_session_recycled",
		"review.verdict",
		"design_review.verdict",
		"code_review.verdict",
	} {
		delete(meta, key)
	}
}

func cloneRef(meta map[string]string, fallback string) string {
	if meta != nil && meta["gc.step_ref"] != "" {
		return meta["gc.step_ref"]
	}
	return fallback
}

func rewriteRetryStepRef(meta map[string]string, fallbackRef, oldScopeRef, newScopeRef string, oldAttempt, nextAttempt int) string {
	stepRef := fallbackRef
	if meta != nil && meta["gc.step_ref"] != "" {
		stepRef = meta["gc.step_ref"]
	}
	if stepRef == "" {
		return ""
	}
	if stepRef == oldScopeRef {
		return newScopeRef
	}
	if oldScopeRef != "" && strings.HasPrefix(stepRef, oldScopeRef+".") {
		return newScopeRef + strings.TrimPrefix(stepRef, oldScopeRef)
	}
	return rewriteRalphAttemptRef(stepRef, oldAttempt, nextAttempt)
}

func rewriteRetryControlRef(controlFor, oldScopeRef, newScopeRef string, oldAttempt, nextAttempt int) string {
	return rewriteRetryStepRef(map[string]string{"gc.step_ref": controlFor}, controlFor, oldScopeRef, newScopeRef, oldAttempt, nextAttempt)
}

func rewriteRetryControlFor(meta map[string]string, controlFor, oldScopeRef, newScopeRef string, oldAttempt, nextAttempt int) string {
	if kind := strings.TrimSpace(meta["gc.kind"]); kind == "scope-check" {
		if stepRef := strings.TrimSpace(meta["gc.step_ref"]); strings.HasSuffix(stepRef, "-scope-check") {
			return strings.TrimSuffix(stepRef, "-scope-check")
		}
	}
	return rewriteRetryControlRef(controlFor, oldScopeRef, newScopeRef, oldAttempt, nextAttempt)
}

func remappedLogicalBeadID(mapping map[string]string, raw string) string {
	logicalID := strings.TrimSpace(raw)
	if logicalID == "" {
		return ""
	}
	if mapped := mapping[logicalID]; mapped != "" {
		return mapped
	}
	return logicalID
}

func resolveExistingRalphRetryFromBeads(store beads.Store, all []beads.Bead, logicalID string, prevSubject, prevCheck beads.Bead, attemptSet map[string]beads.Bead, oldAttempt, nextAttempt int, oldScopeRef, newScopeRef string) (map[string]string, error) {
	rootID := prevSubject.Metadata["gc.root_bead_id"]
	if rootID == "" {
		return nil, fmt.Errorf("%s: missing gc.root_bead_id", prevSubject.ID)
	}

	expected := make(map[string]string, len(attemptSet)+1)
	expected[rewriteRetryStepRef(prevSubject.Metadata, prevSubject.Ref, oldScopeRef, newScopeRef, oldAttempt, nextAttempt)] = prevSubject.ID
	for _, old := range attemptSet {
		if old.ID == prevSubject.ID {
			continue
		}
		expected[rewriteRetryStepRef(old.Metadata, old.Ref, oldScopeRef, newScopeRef, oldAttempt, nextAttempt)] = old.ID
	}
	expected[rewriteRetryStepRef(prevCheck.Metadata, prevCheck.Ref, oldScopeRef, newScopeRef, oldAttempt, nextAttempt)] = prevCheck.ID

	mapping := make(map[string]string, len(expected))
	partial := make(map[string]beads.Bead, len(expected))
	for _, bead := range all {
		if bead.Metadata["gc.partial_retry"] == "true" {
			continue
		}
		stepRef := bead.Metadata["gc.step_ref"]
		if stepRef == "" {
			continue
		}
		oldID, ok := expected[stepRef]
		if !ok {
			continue
		}
		if existing := mapping[oldID]; existing != "" && existing != bead.ID {
			return nil, fmt.Errorf("duplicate retry bead for %s (%s, %s)", stepRef, existing, bead.ID)
		}
		mapping[oldID] = bead.ID
		partial[bead.ID] = bead
	}

	switch {
	case len(mapping) == 0:
		return nil, nil
	case len(mapping) != len(expected):
		if err := discardPartialRalphRetry(store, partial); err != nil {
			return nil, fmt.Errorf("recovering partial retry append for %s: %w", prevSubject.ID, err)
		}
		return nil, nil
	default:
		complete, err := ralphRetryAppendComplete(store, logicalID, prevCheck.ID, attemptSet, mapping)
		if err != nil {
			return nil, err
		}
		if !complete {
			if err := discardPartialRalphRetry(store, partial); err != nil {
				return nil, fmt.Errorf("recovering incompletely wired retry append for %s: %w", prevSubject.ID, err)
			}
			return nil, nil
		}
		return mapping, nil
	}
}

func ralphRetryAppendComplete(store beads.Store, logicalID, prevCheckID string, attemptSet map[string]beads.Bead, mapping map[string]string) (bool, error) {
	newCheckID := mapping[prevCheckID]
	if newCheckID == "" {
		return false, nil
	}

	for _, old := range attemptSet {
		newID := mapping[old.ID]
		if newID == "" {
			return false, nil
		}
		if ok, err := copiedDepsPresent(store, old.ID, newID, mapping); err != nil {
			return false, err
		} else if !ok {
			return false, nil
		}
	}
	if ok, err := copiedDepsPresent(store, prevCheckID, newCheckID, mapping); err != nil {
		return false, err
	} else if !ok {
		return false, nil
	}
	for _, old := range attemptSet {
		newID := mapping[old.ID]
		if newID == "" {
			return false, nil
		}
		newBead, err := store.Get(newID)
		if err != nil {
			return false, err
		}
		if newBead.Assignee != old.Assignee {
			return false, nil
		}
	}
	newCheck, err := store.Get(newCheckID)
	if err != nil {
		return false, err
	}
	oldCheck, err := store.Get(prevCheckID)
	if err != nil {
		return false, err
	}
	if newCheck.Assignee != oldCheck.Assignee {
		return false, nil
	}

	deps, err := store.DepList(logicalID, "down")
	if err != nil {
		return false, err
	}
	for _, dep := range deps {
		if dep.Type == "blocks" && dep.DependsOnID == newCheckID {
			return true, nil
		}
	}
	return false, nil
}

func copiedDepsPresent(store beads.Store, oldID, newID string, mapping map[string]string) (bool, error) {
	oldDeps, err := store.DepList(oldID, "down")
	if err != nil {
		return false, err
	}
	newDeps, err := store.DepList(newID, "down")
	if err != nil {
		return false, err
	}
	for _, oldDep := range oldDeps {
		if oldDep.Type != "blocks" && oldDep.Type != "waits-for" && oldDep.Type != "conditional-blocks" {
			continue
		}
		targetID := oldDep.DependsOnID
		if mapped, ok := mapping[targetID]; ok {
			targetID = mapped
		} else {
			target, err := store.Get(oldDep.DependsOnID)
			if err != nil {
				return false, err
			}
			if target.Metadata["gc.dynamic_fragment"] == "true" {
				continue
			}
		}
		found := false
		for _, newDep := range newDeps {
			if newDep.Type == oldDep.Type && newDep.DependsOnID == targetID {
				found = true
				break
			}
		}
		if !found {
			return false, nil
		}
	}
	return true, nil
}

func discardPartialRalphRetry(store beads.Store, partial map[string]beads.Bead) error {
	if len(partial) == 0 {
		return nil
	}

	pending := make(map[string]beads.Bead, len(partial))
	for id, bead := range partial {
		pending[id] = bead
	}

	for len(pending) > 0 {
		progress := false
		for _, id := range sortedPendingFragmentIDs(pending) {
			if !canDiscardPartialFragmentBead(store, id, pending) {
				continue
			}
			bead := pending[id]
			if err := detachIncomingDeps(store, id); err != nil {
				return err
			}
			if err := store.SetMetadataBatch(id, map[string]string{
				"gc.outcome":       "skipped",
				"gc.partial_retry": "true",
			}); err != nil {
				return err
			}
			if bead.Status != "closed" {
				if err := store.Close(id); err != nil {
					return fmt.Errorf("closing partial retry bead %s: %w", id, err)
				}
			}
			delete(pending, id)
			progress = true
		}
		if progress {
			continue
		}
		return fmt.Errorf("unable to discard partial retry beads: %v", sortedPendingFragmentIDs(pending))
	}

	return nil
}

func sortedRetryAssigneeIDs(pending map[string]string) []string {
	ids := make([]string, 0, len(pending))
	for id := range pending {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func rewriteRalphAttemptRef(ref string, oldAttempt, nextAttempt int) string {
	if ref == "" || oldAttempt < 1 || nextAttempt < 1 {
		return ref
	}
	if rewritten, ok := rewriteAttemptSegment(ref, "run", oldAttempt, nextAttempt); ok {
		return rewritten
	}
	if rewritten, ok := rewriteAttemptSegment(ref, "check", oldAttempt, nextAttempt); ok {
		return rewritten
	}
	return ref
}

func rewriteAttemptSegment(ref, kind string, oldAttempt, nextAttempt int) (string, bool) {
	needle := "." + kind + "." + strconv.Itoa(oldAttempt)
	index := strings.LastIndex(ref, needle)
	if index < 0 {
		return "", false
	}
	end := index + len(needle)
	if end < len(ref) && ref[end] != '.' {
		return "", false
	}
	replacement := "." + kind + "." + strconv.Itoa(nextAttempt)
	return ref[:index] + replacement + ref[end:], true
}
