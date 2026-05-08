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
	"github.com/gastownhall/gascity/internal/config"
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
	blockerIDs, err := controlBlockerIDs(store, bead.ID)
	if err != nil {
		return ControlResult{}, fmt.Errorf("%s: loading control blockers: %w", bead.ID, err)
	}
	source, err := resolveWorkflowStepByRefFromBeads(workflowBeads, rootID, sourceRef, workflowStepMatchOptions{PreferredIDs: blockerIDs})
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
	if strings.TrimSpace(bead.Metadata["gc.fanout_state"]) == "" {
		if err := store.SetMetadataBatch(bead.ID, map[string]string{"gc.fanout_state": "spawning"}); err != nil {
			return ControlResult{}, fmt.Errorf("%s: recording fanout spawn start: %w", bead.ID, err)
		}
	}
	fanoutSinkBlockers := fanoutSinkBlockerIDs(blockerIDs, source.ID)

	var previousSinkIDs []string
	totalCreated := 0
	for index, item := range items {
		targetRef := fanoutTargetRef(source, sourceRef, index)
		target := &formula.Step{
			ID:          targetRef,
			Title:       source.Title,
			Description: source.Description,
		}
		itemVars := materializeFanoutVars(bondVars, item, index)
		if scopeRef := strings.TrimSpace(bead.Metadata["gc.scope_ref"]); scopeRef != "" {
			if itemVars == nil {
				itemVars = make(map[string]string, 1)
			}
			itemVars["scope_ref"] = scopeRef
		}
		fragment, err := formula.CompileExpansionFragment(context.Background(), bead.Metadata["gc.bond"], opts.FormulaSearchPaths, target, itemVars)
		if err != nil {
			return ControlResult{}, fmt.Errorf("%s: compiling fragment %d: %w", bead.ID, index+1, err)
		}
		if opts.PrepareFragment != nil {
			if err := opts.PrepareFragment(fragment, source); err != nil {
				return ControlResult{}, fmt.Errorf("%s: preparing fragment %d: %w", bead.ID, index+1, err)
			}
		}
		routeFanoutFragmentSteps(fragment, bead, opts, store)
		externalDeps := expectedFragmentExternalDeps(fragment, mode, previousSinkIDs)
		existingMapping, err := resolveExistingFragmentInstanceFromBeads(store, workflowBeads, rootID, fragment, externalDeps, fragmentResumeMatchOptions{
			StepRefAliases:     fanoutLegacyStepAliases(fragment, targetRef, sourceRef, index),
			AliasScopeRef:      strings.TrimSpace(bead.Metadata["gc.scope_ref"]),
			FanoutSinkBlockers: fanoutSinkBlockers,
		})
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

func fanoutTargetRef(source beads.Bead, sourceRef string, index int) string {
	base := strings.TrimSpace(source.Metadata["gc.step_ref"])
	if base == "" {
		base = sourceRef
	}
	return base + ".item." + strconv.Itoa(index+1)
}

func controlBlockerIDs(store beads.Store, controlID string) (map[string]struct{}, error) {
	deps, err := store.DepList(controlID, "down")
	if err != nil {
		return nil, err
	}
	blockers := make(map[string]struct{}, len(deps))
	for _, dep := range deps {
		if dep.Type != "blocks" || dep.DependsOnID == "" {
			continue
		}
		blockers[dep.DependsOnID] = struct{}{}
	}
	if len(blockers) == 0 {
		return nil, nil
	}
	return blockers, nil
}

func fanoutSinkBlockerIDs(blockers map[string]struct{}, sourceID string) map[string]struct{} {
	if len(blockers) == 0 {
		return nil
	}
	sinks := make(map[string]struct{}, len(blockers))
	for blockerID := range blockers {
		if blockerID == sourceID {
			continue
		}
		sinks[blockerID] = struct{}{}
	}
	if len(sinks) == 0 {
		return nil
	}
	return sinks
}

func fanoutLegacyStepAliases(fragment *formula.FragmentRecipe, targetRef, sourceRef string, index int) map[string]string {
	if fragment == nil {
		return nil
	}
	legacyBase := strings.TrimSpace(sourceRef)
	if legacyBase == "" {
		return nil
	}
	legacyTargetRef := legacyBase + ".item." + strconv.Itoa(index+1)
	if legacyTargetRef == targetRef {
		return nil
	}

	aliases := make(map[string]string, len(fragment.Steps))
	for _, step := range fragment.Steps {
		if strings.Count(step.ID, targetRef) != 1 {
			continue
		}
		legacyID := strings.Replace(step.ID, targetRef, legacyTargetRef, 1)
		if legacyID != step.ID {
			aliases[step.ID] = legacyID
		}
	}
	if len(aliases) == 0 {
		return nil
	}
	return aliases
}

type fragmentResumeMatchOptions struct {
	StepRefAliases     map[string]string
	AliasScopeRef      string
	FanoutSinkBlockers map[string]struct{}
}

func routeFanoutFragmentSteps(fragment *formula.FragmentRecipe, control beads.Bead, opts ProcessOptions, store beads.Store) {
	if fragment == nil {
		return
	}
	executionRoute := strings.TrimSpace(control.Metadata["gc.execution_routed_to"])
	routeCfg := loadAttemptRouteConfig(opts.CityPath)
	for i := range fragment.Steps {
		step := &fragment.Steps[i]
		if step.Metadata["gc.kind"] == "spec" {
			continue
		}
		if isAttemptControlKind(step.Metadata["gc.kind"]) {
			target := strings.TrimSpace(step.Metadata["gc.execution_routed_to"])
			if target == "" {
				target = fanoutFragmentStepTarget(*step, executionRoute, routeCfg)
			}
			applyAttemptControlStepRoute(step, target, routeCfg, store)
			continue
		}
		if fanoutFragmentStepHasRoute(*step) {
			continue
		}
		target := fanoutFragmentStepTarget(*step, executionRoute, routeCfg)
		if target == "" {
			continue
		}
		applyAttemptStepRoute(step, target, routeCfg, store)
	}
}

func fanoutFragmentStepTarget(step formula.RecipeStep, executionRoute string, routeCfg *config.City) string {
	target := strings.TrimSpace(step.Metadata["gc.run_target"])
	if target == "" {
		target = strings.TrimSpace(step.Metadata["gc.routed_to"])
	}
	if target == "" {
		target = strings.TrimSpace(step.Assignee)
	}
	if target == "" {
		return executionRoute
	}
	return qualifyAttemptTargetWithSourceRoute(target, executionRoute, routeCfg)
}

func fanoutFragmentStepHasRoute(step formula.RecipeStep) bool {
	if strings.TrimSpace(step.Metadata["gc.execution_routed_to"]) != "" {
		return true
	}
	if strings.TrimSpace(step.Metadata["gc.routed_to"]) != "" {
		return true
	}
	return strings.TrimSpace(step.Assignee) != ""
}

func resolveExistingFragmentInstanceFromBeads(store beads.Store, all []beads.Bead, _ string, fragment *formula.FragmentRecipe, externalDeps []molecule.ExternalDep, opts fragmentResumeMatchOptions) (map[string]string, error) {
	if fragment == nil || len(fragment.Steps) == 0 {
		return nil, nil
	}

	expected := make(map[string]struct{}, len(fragment.Steps))
	aliasToExpected := make(map[string]string, len(opts.StepRefAliases))
	for _, step := range fragment.Steps {
		expected[step.ID] = struct{}{}
		if alias := strings.TrimSpace(opts.StepRefAliases[step.ID]); alias != "" && alias != step.ID {
			aliasToExpected[alias] = step.ID
		}
	}

	mapping := make(map[string]string, len(fragment.Steps))
	partial := make(map[string]beads.Bead, len(fragment.Steps))
	rejectedAlias := make(map[string]beads.Bead)
	usedAlias := false
	for _, bead := range all {
		if bead.Metadata["gc.partial_fragment"] == "true" {
			continue
		}
		stepRef := bead.Metadata["gc.step_ref"]
		if stepRef == "" {
			continue
		}
		matchID := stepRef
		aliasMatch := false
		if _, ok := expected[matchID]; !ok {
			matchID = aliasToExpected[stepRef]
			aliasMatch = matchID != ""
		}
		if matchID == "" {
			continue
		}
		if aliasMatch {
			scopeOwned := false
			if opts.AliasScopeRef != "" {
				beadScopeRef := strings.TrimSpace(bead.Metadata["gc.scope_ref"])
				if beadScopeRef != "" {
					if beadScopeRef != opts.AliasScopeRef {
						continue
					}
					scopeOwned = true
				}
			}
			blockerOwned := len(opts.FanoutSinkBlockers) > 0
			// Legacy aliases are only safe to reuse once current-iteration
			// ownership is proven. Without a matching scope_ref or already-wired
			// sink blockers, an open legacy fragment could still belong to an
			// older iteration that shared the same logical target.
			if !scopeOwned && !blockerOwned {
				if bead.Status != "closed" {
					rejectedAlias[bead.ID] = bead
				}
				continue
			}
			usedAlias = true
		}
		if existing := mapping[matchID]; existing != "" && existing != bead.ID {
			return nil, fmt.Errorf("duplicate fragment bead for %s (%s, %s)", matchID, existing, bead.ID)
		}
		mapping[matchID] = bead.ID
		partial[bead.ID] = bead
	}

	switch {
	case len(mapping) == 0:
		if err := discardFragmentCandidates(store, fragment.Name, rejectedAlias); err != nil {
			return nil, err
		}
		return nil, nil
	case len(mapping) != len(expected):
		if err := discardFragmentCandidates(store, fragment.Name, partial, rejectedAlias); err != nil {
			return nil, err
		}
		return nil, nil
	default:
		if usedAlias && !fragmentAliasMatchesExistingBlockers(fragment, mapping, opts.FanoutSinkBlockers) {
			if err := discardFragmentCandidates(store, fragment.Name, openFragmentBeads(partial), rejectedAlias); err != nil {
				return nil, err
			}
			return nil, nil
		}
		if len(rejectedAlias) > 0 {
			if err := discardFragmentCandidates(store, fragment.Name, rejectedAlias); err != nil {
				return nil, err
			}
		}
		complete, err := fragmentInstanceComplete(store, fragment, mapping, externalDeps)
		if err != nil {
			return nil, err
		}
		if !complete {
			if err := discardFragmentCandidates(store, fragment.Name, partial); err != nil {
				return nil, err
			}
			return nil, nil
		}
		return mapping, nil
	}
}

func fragmentAliasMatchesExistingBlockers(fragment *formula.FragmentRecipe, mapping map[string]string, blockers map[string]struct{}) bool {
	if len(blockers) == 0 {
		return true
	}
	sinkIDs := mapStepIDs(fragment.Sinks, mapping)
	if len(sinkIDs) == 0 {
		return false
	}
	for _, sinkID := range sinkIDs {
		if _, ok := blockers[sinkID]; !ok {
			return false
		}
	}
	return true
}

func discardFragmentCandidates(store beads.Store, fragmentName string, groups ...map[string]beads.Bead) error {
	candidates := make(map[string]beads.Bead)
	for _, group := range groups {
		for id, bead := range group {
			candidates[id] = bead
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	if err := discardPartialFragmentInstance(store, candidates); err != nil {
		return fmt.Errorf("recovering partial fragment instance for %s: %w", fragmentName, err)
	}
	return nil
}

func openFragmentBeads(group map[string]beads.Bead) map[string]beads.Bead {
	if len(group) == 0 {
		return nil
	}
	openOnly := make(map[string]beads.Bead)
	for id, bead := range group {
		if bead.Status == "closed" {
			continue
		}
		openOnly[id] = bead
	}
	if len(openOnly) == 0 {
		return nil
	}
	return openOnly
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
		if !fragmentRouteMetadataMatches(bead, step) {
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

func fragmentRouteMetadataMatches(bead beads.Bead, step formula.RecipeStep) bool {
	for _, key := range []string{"gc.routed_to", "gc.execution_routed_to"} {
		if strings.TrimSpace(bead.Metadata[key]) != strings.TrimSpace(step.Metadata[key]) {
			return false
		}
	}
	return true
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

type workflowStepMatchOptions struct {
	PreferredIDs map[string]struct{}
}

func resolveWorkflowStepByRefFromBeads(all []beads.Bead, rootID, stepRef string, opts workflowStepMatchOptions) (beads.Bead, error) {
	if len(opts.PreferredIDs) > 0 {
		if match, ok := findWorkflowStepByRef(all, stepRef, opts.PreferredIDs); ok {
			return match, nil
		}
	}
	if match, ok := findWorkflowStepByRef(all, stepRef, nil); ok {
		return match, nil
	}
	return beads.Bead{}, fmt.Errorf("step ref %q not found under root %s", stepRef, rootID)
}

func findWorkflowStepByRef(all []beads.Bead, stepRef string, allowedIDs map[string]struct{}) (beads.Bead, bool) {
	var suffixMatch *beads.Bead
	for _, bead := range all {
		if len(allowedIDs) > 0 {
			if _, ok := allowedIDs[bead.ID]; !ok {
				continue
			}
		}
		ref := bead.Metadata["gc.step_ref"]
		if ref == stepRef {
			return bead, true
		}
		if suffixMatch == nil && strings.HasSuffix(ref, "."+stepRef) {
			match := bead
			suffixMatch = &match
		}
	}
	if suffixMatch != nil {
		return *suffixMatch, true
	}
	return beads.Bead{}, false
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
