// Package dispatch implements workflow execution, fan-out, and lifecycle management.
package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/formula"
	"github.com/gastownhall/gascity/internal/molecule"
)

var fanoutVarPattern = regexp.MustCompile(`\{([^}]+)\}`)

func processFanout(store beads.Store, bead beads.Bead, opts ProcessOptions) (ControlResult, error) {
	switch bead.Metadata["gc.fanout_state"] {
	case "spawned":
		outcome, err := resolveFinalizeOutcome(store, bead.ID)
		if err != nil {
			if errors.Is(err, errFinalizePending) {
				return ControlResult{}, nil
			}
			return ControlResult{}, fmt.Errorf("%s: resolving fanout outcome: %w", bead.ID, err)
		}
		if err := setOutcomeAndClose(store, bead.ID, outcome); err != nil {
			return ControlResult{}, fmt.Errorf("%s: closing fanout: %w", bead.ID, err)
		}
		closedBead, err := store.Get(bead.ID)
		if err != nil {
			return ControlResult{}, fmt.Errorf("%s: reloading closed fanout: %w", bead.ID, err)
		}
		scopeResult, err := reconcileTerminalScopedMember(store, closedBead)
		if err != nil {
			return ControlResult{}, err
		}
		return ControlResult{Processed: true, Action: "fanout-" + outcome, Skipped: scopeResult.Skipped}, nil
	case "", "spawning":
		// Continue below. "spawning" means a previous attempt may have created
		// some or all child fragments before the control bead could persist its
		// terminal spawned state.
	default:
		return ControlResult{}, fmt.Errorf("%s: unsupported gc.fanout_state %q", bead.ID, bead.Metadata["gc.fanout_state"])
	}

	rootID := bead.Metadata["gc.root_bead_id"]
	if rootID == "" {
		return ControlResult{}, fmt.Errorf("%s: missing gc.root_bead_id", bead.ID)
	}
	workflowBeads, err := listByWorkflowRoot(store, rootID)
	if err != nil {
		return ControlResult{}, fmt.Errorf("%s: listing workflow beads: %w", bead.ID, err)
	}

	sourceRef := bead.Metadata["gc.control_for"]
	if sourceRef == "" {
		return ControlResult{}, fmt.Errorf("%s: missing gc.control_for", bead.ID)
	}
	source, err := resolveWorkflowStepByRefFromBeads(workflowBeads, rootID, sourceRef)
	if err != nil {
		return ControlResult{}, fmt.Errorf("%s: resolving source step %q: %w", bead.ID, sourceRef, err)
	}
	if source.Metadata["gc.outcome"] == "fail" {
		if err := setOutcomeAndClose(store, bead.ID, "fail"); err != nil {
			return ControlResult{}, fmt.Errorf("%s: closing failed fanout: %w", bead.ID, err)
		}
		closedBead, err := store.Get(bead.ID)
		if err != nil {
			return ControlResult{}, fmt.Errorf("%s: reloading failed fanout: %w", bead.ID, err)
		}
		scopeResult, err := reconcileTerminalScopedMember(store, closedBead)
		if err != nil {
			return ControlResult{}, err
		}
		return ControlResult{Processed: true, Action: "fanout-fail", Skipped: scopeResult.Skipped}, nil
	}

	items, err := resolveFanoutItems(source, bead.Metadata["gc.for_each"])
	if err != nil {
		return ControlResult{}, fmt.Errorf("%s: resolving items: %w", bead.ID, err)
	}
	if len(items) == 0 {
		if err := setOutcomeAndClose(store, bead.ID, "pass"); err != nil {
			return ControlResult{}, fmt.Errorf("%s: closing empty fanout: %w", bead.ID, err)
		}
		closedBead, err := store.Get(bead.ID)
		if err != nil {
			return ControlResult{}, fmt.Errorf("%s: reloading empty fanout: %w", bead.ID, err)
		}
		scopeResult, err := reconcileTerminalScopedMember(store, closedBead)
		if err != nil {
			return ControlResult{}, err
		}
		return ControlResult{Processed: true, Action: "fanout-empty", Skipped: scopeResult.Skipped}, nil
	}
	if len(opts.FormulaSearchPaths) == 0 {
		return ControlResult{}, fmt.Errorf("%s: missing formula search paths", bead.ID)
	}

	bondVars, err := parseFanoutVars(bead.Metadata["gc.bond_vars"])
	if err != nil {
		return ControlResult{}, fmt.Errorf("%s: parsing gc.bond_vars: %w", bead.ID, err)
	}
	mode := bead.Metadata["gc.fanout_mode"]
	if mode == "" {
		mode = "parallel"
	}
	if bead.Metadata["gc.fanout_state"] == "" {
		if err := store.SetMetadataBatch(bead.ID, map[string]string{"gc.fanout_state": "spawning"}); err != nil {
			return ControlResult{}, fmt.Errorf("%s: recording fanout spawn start: %w", bead.ID, err)
		}
	}

	var previousSinkIDs []string
	totalCreated := 0
	for index, item := range items {
		targetRef := sourceRef + ".item." + strconv.Itoa(index+1)
		target := &formula.Step{
			ID:          targetRef,
			Title:       source.Title,
			Description: source.Description,
		}
		itemVars := materializeFanoutVars(bondVars, item, index)
		fragment, err := formula.CompileExpansionFragment(context.Background(), bead.Metadata["gc.bond"], opts.FormulaSearchPaths, target, itemVars)
		if err != nil {
			return ControlResult{}, fmt.Errorf("%s: compiling fragment %d: %w", bead.ID, index+1, err)
		}
		if opts.PrepareFragment != nil {
			if err := opts.PrepareFragment(fragment, source); err != nil {
				return ControlResult{}, fmt.Errorf("%s: preparing fragment %d: %w", bead.ID, index+1, err)
			}
		}
		externalDeps := expectedFragmentExternalDeps(fragment, mode, previousSinkIDs)
		existingMapping, err := resolveExistingFragmentInstanceFromBeads(store, workflowBeads, rootID, fragment, externalDeps)
		if err != nil {
			return ControlResult{}, fmt.Errorf("%s: resuming fragment %d: %w", bead.ID, index+1, err)
		}

		var idMapping map[string]string
		if len(existingMapping) > 0 {
			idMapping = existingMapping
		} else {
			inst, err := molecule.InstantiateFragment(context.Background(), store, fragment, molecule.FragmentOptions{
				RootID:       rootID,
				Vars:         itemVars,
				ExternalDeps: externalDeps,
			})
			if err != nil {
				return ControlResult{}, fmt.Errorf("%s: instantiating fragment %d: %w", bead.ID, index+1, err)
			}
			totalCreated += inst.Created
			idMapping = inst.IDMapping
		}

		sinkIDs := mapStepIDs(fragment.Sinks, idMapping)
		for _, sinkID := range sinkIDs {
			if err := store.DepAdd(bead.ID, sinkID, "blocks"); err != nil {
				return ControlResult{}, fmt.Errorf("%s: wiring fanout blocker: %w", bead.ID, err)
			}
		}
		if len(sinkIDs) > 0 {
			previousSinkIDs = sinkIDs
		}
	}

	if err := store.SetMetadataBatch(bead.ID, map[string]string{
		"gc.fanout_state":  "spawned",
		"gc.spawned_count": strconv.Itoa(len(items)),
	}); err != nil {
		return ControlResult{}, fmt.Errorf("%s: recording fanout state: %w", bead.ID, err)
	}
	return ControlResult{Processed: true, Action: "fanout-spawn", Created: totalCreated}, nil
}

func resolveExistingFragmentInstanceFromBeads(store beads.Store, all []beads.Bead, _ string, fragment *formula.FragmentRecipe, externalDeps []molecule.ExternalDep) (map[string]string, error) {
	if fragment == nil || len(fragment.Steps) == 0 {
		return nil, nil
	}

	expected := make(map[string]struct{}, len(fragment.Steps))
	for _, step := range fragment.Steps {
		expected[step.ID] = struct{}{}
	}

	mapping := make(map[string]string, len(fragment.Steps))
	partial := make(map[string]beads.Bead, len(fragment.Steps))
	for _, bead := range all {
		if bead.Metadata["gc.partial_fragment"] == "true" {
			continue
		}
		stepRef := bead.Metadata["gc.step_ref"]
		if stepRef == "" {
			continue
		}
		if _, ok := expected[stepRef]; !ok {
			continue
		}
		if existing := mapping[stepRef]; existing != "" && existing != bead.ID {
			return nil, fmt.Errorf("duplicate fragment bead for %s (%s, %s)", stepRef, existing, bead.ID)
		}
		mapping[stepRef] = bead.ID
		partial[bead.ID] = bead
	}

	switch {
	case len(mapping) == 0:
		return nil, nil
	case len(mapping) != len(expected):
		if err := discardPartialFragmentInstance(store, partial); err != nil {
			return nil, fmt.Errorf("recovering partial fragment instance for %s: %w", fragment.Name, err)
		}
		return nil, nil
	default:
		complete, err := fragmentInstanceComplete(store, fragment, mapping, externalDeps)
		if err != nil {
			return nil, err
		}
		if !complete {
			if err := discardPartialFragmentInstance(store, partial); err != nil {
				return nil, fmt.Errorf("recovering incompletely wired fragment instance for %s: %w", fragment.Name, err)
			}
			return nil, nil
		}
		return mapping, nil
	}
}

func fragmentInstanceComplete(store beads.Store, fragment *formula.FragmentRecipe, mapping map[string]string, externalDeps []molecule.ExternalDep) (bool, error) {
	if fragment == nil {
		return false, fmt.Errorf("fragment is nil")
	}
	stepByID := make(map[string]formula.RecipeStep, len(fragment.Steps))
	for _, step := range fragment.Steps {
		stepByID[step.ID] = step
	}
	for _, step := range fragment.Steps {
		beadID := mapping[step.ID]
		if beadID == "" {
			return false, nil
		}
		bead, err := store.Get(beadID)
		if err != nil {
			return false, err
		}
		if bead.Assignee != step.Assignee {
			return false, nil
		}
	}

	for _, dep := range fragment.Deps {
		if dep.Type == "parent-child" {
			continue
		}
		fromID := mapping[dep.StepID]
		toID := mapping[dep.DependsOnID]
		if fromID == "" || toID == "" {
			return false, nil
		}
		deps, err := store.DepList(fromID, "down")
		if err != nil {
			return false, err
		}
		found := false
		for _, existing := range deps {
			if existing.Type == dep.Type && existing.DependsOnID == toID {
				found = true
				break
			}
		}
		if !found {
			ok, err := fragmentDepSatisfiedDynamically(store, stepByID, dep, mapping)
			if err != nil {
				return false, err
			}
			if !ok {
				return false, nil
			}
		}
	}

	for _, dep := range externalDeps {
		fromID := mapping[dep.StepID]
		if fromID == "" || dep.DependsOnID == "" {
			return false, nil
		}
		depType := dep.Type
		if depType == "" {
			depType = "blocks"
		}
		found, err := beadHasDep(store, fromID, dep.DependsOnID, depType)
		if err != nil {
			return false, err
		}
		if !found {
			return false, nil
		}
	}

	return true, nil
}

func expectedFragmentExternalDeps(fragment *formula.FragmentRecipe, mode string, previousSinkIDs []string) []molecule.ExternalDep {
	if fragment == nil || mode != "sequential" || len(previousSinkIDs) == 0 {
		return nil
	}
	externalDeps := make([]molecule.ExternalDep, 0, len(fragment.Entries)*len(previousSinkIDs))
	for _, entryID := range fragment.Entries {
		for _, prevSinkID := range previousSinkIDs {
			externalDeps = append(externalDeps, molecule.ExternalDep{
				StepID:      entryID,
				DependsOnID: prevSinkID,
				Type:        "blocks",
			})
		}
	}
	return externalDeps
}

func beadHasDep(store beads.Store, fromID, toID, depType string) (bool, error) {
	deps, err := store.DepList(fromID, "down")
	if err != nil {
		return false, err
	}
	for _, dep := range deps {
		if dep.Type == depType && dep.DependsOnID == toID {
			return true, nil
		}
	}
	return false, nil
}

func fragmentDepSatisfiedDynamically(store beads.Store, stepByID map[string]formula.RecipeStep, dep formula.RecipeDep, mapping map[string]string) (bool, error) {
	fromStep, ok := stepByID[dep.StepID]
	if !ok {
		return false, nil
	}
	toStep, ok := stepByID[dep.DependsOnID]
	if !ok {
		return false, nil
	}
	if dep.Type != "blocks" || fromStep.Metadata["gc.kind"] != "ralph" || toStep.Metadata["gc.kind"] != "check" {
		return false, nil
	}

	logicalID := mapping[dep.StepID]
	if logicalID == "" {
		return false, nil
	}
	deps, err := store.DepList(logicalID, "down")
	if err != nil {
		return false, err
	}
	for _, existing := range deps {
		if existing.Type != "blocks" {
			continue
		}
		check, err := store.Get(existing.DependsOnID)
		if err != nil {
			return false, err
		}
		if check.Metadata["gc.kind"] != "check" {
			continue
		}
		if check.Metadata["gc.logical_bead_id"] == logicalID {
			return true, nil
		}
	}
	return false, nil
}

func discardPartialFragmentInstance(store beads.Store, partial map[string]beads.Bead) error {
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
				"gc.outcome":          "skipped",
				"gc.partial_fragment": "true",
			}); err != nil {
				return err
			}
			if bead.Status != "closed" {
				if err := store.Close(id); err != nil {
					return fmt.Errorf("closing partial fragment bead %s: %w", id, err)
				}
			}
			delete(pending, id)
			progress = true
		}
		if progress {
			continue
		}
		return fmt.Errorf("unable to discard partial fragment beads: %v", sortedPendingFragmentIDs(pending))
	}

	return nil
}

func canDiscardPartialFragmentBead(store beads.Store, beadID string, pending map[string]beads.Bead) bool {
	deps, err := store.DepList(beadID, "up")
	if err != nil {
		return false
	}
	for _, dep := range deps {
		if dep.Type != "blocks" {
			continue
		}
		if _, blocked := pending[dep.IssueID]; blocked {
			return false
		}
	}
	return true
}

func sortedPendingFragmentIDs(pending map[string]beads.Bead) []string {
	ids := make([]string, 0, len(pending))
	for id := range pending {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func detachIncomingDeps(store beads.Store, beadID string) error {
	deps, err := store.DepList(beadID, "up")
	if err != nil {
		return err
	}
	for _, dep := range deps {
		if err := store.DepRemove(dep.IssueID, beadID); err != nil {
			return fmt.Errorf("removing incoming dep %s -> %s: %w", dep.IssueID, beadID, err)
		}
	}
	return nil
}

func resolveWorkflowStepByRefFromBeads(all []beads.Bead, rootID, stepRef string) (beads.Bead, error) {
	var suffixMatch *beads.Bead
	for _, bead := range all {
		ref := bead.Metadata["gc.step_ref"]
		if ref == stepRef {
			return bead, nil
		}
		if suffixMatch == nil && strings.HasSuffix(ref, "."+stepRef) {
			match := bead
			suffixMatch = &match
		}
	}
	if suffixMatch != nil {
		return *suffixMatch, nil
	}
	return beads.Bead{}, fmt.Errorf("step ref %q not found under root %s", stepRef, rootID)
}

func resolveFanoutItems(source beads.Bead, forEach string) ([]interface{}, error) {
	if !strings.HasPrefix(forEach, "output.") {
		return nil, fmt.Errorf("for_each must start with output. (got %q)", forEach)
	}
	raw := source.Metadata["gc.output_json"]
	if raw == "" {
		return nil, fmt.Errorf("source bead %s is missing gc.output_json (required by on_complete fanout; producer must set metadata before close)", source.ID)
	}
	var output interface{}
	if err := json.Unmarshal([]byte(raw), &output); err != nil {
		return nil, fmt.Errorf("parsing gc.output_json: %w", err)
	}

	current := output
	for _, part := range strings.Split(strings.TrimPrefix(forEach, "output."), ".") {
		obj, ok := current.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("output path %q does not resolve to an array", forEach)
		}
		current = obj[part]
	}

	values, ok := current.([]interface{})
	if !ok {
		return nil, fmt.Errorf("output path %q is not an array", forEach)
	}
	return values, nil
}

func parseFanoutVars(raw string) (map[string]string, error) {
	if raw == "" {
		return nil, nil
	}
	var vars map[string]string
	if err := json.Unmarshal([]byte(raw), &vars); err != nil {
		return nil, err
	}
	return vars, nil
}

func materializeFanoutVars(spec map[string]string, item interface{}, index int) map[string]string {
	if len(spec) == 0 {
		return nil
	}
	vars := make(map[string]string, len(spec))
	for key, template := range spec {
		vars[key] = substituteFanoutTemplate(template, item, index)
	}
	return vars
}

func substituteFanoutTemplate(template string, item interface{}, index int) string {
	return fanoutVarPattern.ReplaceAllStringFunc(template, func(match string) string {
		token := fanoutVarPattern.FindStringSubmatch(match)[1]
		switch {
		case token == "index":
			return strconv.Itoa(index)
		case token == "item":
			return fmt.Sprintf("%v", item)
		case strings.HasPrefix(token, "item."):
			if value, ok := lookupItemValue(item, strings.TrimPrefix(token, "item.")); ok {
				return fmt.Sprintf("%v", value)
			}
			return ""
		default:
			return match
		}
	})
}

func lookupItemValue(item interface{}, path string) (interface{}, bool) {
	current := item
	for _, part := range strings.Split(path, ".") {
		obj, ok := current.(map[string]interface{})
		if !ok {
			return nil, false
		}
		current, ok = obj[part]
		if !ok {
			return nil, false
		}
	}
	return current, true
}

func mapStepIDs(stepIDs []string, idMapping map[string]string) []string {
	if len(stepIDs) == 0 {
		return nil
	}
	mapped := make([]string, 0, len(stepIDs))
	for _, stepID := range stepIDs {
		if beadID := idMapping[stepID]; beadID != "" {
			mapped = append(mapped, beadID)
		}
	}
	return mapped
}
