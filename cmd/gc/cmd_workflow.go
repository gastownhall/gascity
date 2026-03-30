package main

import (
	"fmt"
	"io"
	"maps"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/formula"
	"github.com/gastownhall/gascity/internal/workflow"
	"github.com/spf13/cobra"
)

func newWorkflowCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "workflow",
		Short: "Run explicit graph-first workflow control beads",
	}
	cmd.AddCommand(
		newWorkflowControlCmd(stdout, stderr),
		newWorkflowServeCmd(stdout, stderr),
		newWorkflowDeleteCmd(stdout, stderr),
	)
	return cmd
}

func newWorkflowControlCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "control <bead-id>",
		Short: "Execute a graph.v2 control bead in the current city",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if err := runWorkflowControl(args[0], stdout, stderr); err != nil {
				fmt.Fprintf(stderr, "gc workflow control: %v\n", err) //nolint:errcheck
				return errExit
			}
			return nil
		},
	}
	return cmd
}

func runWorkflowControl(beadID string, stdout, _ io.Writer) error {
	cityPath, err := resolveCity()
	if err != nil {
		return err
	}

	readDoltPort(cityPath)
	store, err := openStoreAtForCity(cityPath, cityPath)
	if err != nil {
		return fmt.Errorf("opening workflow store: %w", err)
	}

	bead, err := store.Get(beadID)
	if err != nil {
		return fmt.Errorf("loading bead %s: %w", beadID, err)
	}

	opts := workflow.ProcessOptions{CityPath: cityPath}
	switch bead.Metadata["gc.kind"] {
	case "check", "fanout":
		cfg, err := loadCityConfig(cityPath)
		if err != nil {
			return err
		}
		opts.FormulaSearchPaths = workflowFormulaSearchPaths(cfg, bead)
		opts.PrepareFragment = func(fragment *formula.FragmentRecipe, source beads.Bead) error {
			return decorateDynamicFragmentRecipe(fragment, source, store, cfg.Workspace.Name, cityPath, cfg)
		}
	}

	result, err := workflow.ProcessControl(store, bead, opts)
	if err != nil {
		return err
	}
	if result.Processed {
		fmt.Fprintf(stdout, "workflow control: bead=%s action=%s", beadID, result.Action) //nolint:errcheck
		if result.Created > 0 {
			fmt.Fprintf(stdout, " created=%d", result.Created) //nolint:errcheck
		}
		if result.Skipped > 0 {
			fmt.Fprintf(stdout, " skipped=%d", result.Skipped) //nolint:errcheck
		}
		fmt.Fprintln(stdout) //nolint:errcheck
	}
	return nil
}

func workflowFormulaSearchPaths(cfg *config.City, bead beads.Bead) []string {
	if cfg == nil {
		return nil
	}
	routedTo := workflowExecutionRoute(bead)
	if routedTo == "" {
		return cfg.FormulaLayers.City
	}
	rigName, _ := config.ParseQualifiedName(routedTo)
	if paths := cfg.FormulaLayers.SearchPaths(rigName); len(paths) > 0 {
		return paths
	}
	return cfg.FormulaLayers.City
}

func decorateDynamicFragmentRecipe(fragment *formula.FragmentRecipe, source beads.Bead, store beads.Store, cityName, cityPath string, cfg *config.City) error {
	if fragment == nil {
		return fmt.Errorf("fragment recipe is nil")
	}
	defaultRoute, err := graphFallbackBindingForBead(source, store, cityName, cityPath, cfg)
	if err != nil {
		return err
	}
	controlRoute, err := workflowControlBinding(store, cityName, cityPath, cfg)
	if err != nil {
		return err
	}

	for i := range fragment.Steps {
		step := &fragment.Steps[i]
		if step.Metadata == nil {
			step.Metadata = make(map[string]string)
		} else {
			step.Metadata = maps.Clone(step.Metadata)
		}
		step.Metadata["gc.dynamic_fragment"] = "true"
		propagateDynamicScopeMetadata(step, source)
	}
	formula.ApplyFragmentRecipeGraphControls(fragment)

	stepByID := make(map[string]*formula.RecipeStep, len(fragment.Steps))
	stepAlias := make(map[string]string, len(fragment.Steps))
	for i := range fragment.Steps {
		stepByID[fragment.Steps[i].ID] = &fragment.Steps[i]
		if short, ok := strings.CutPrefix(fragment.Steps[i].ID, fragment.Name+"."); ok {
			stepAlias[short] = fragment.Steps[i].ID
		}
	}
	depsByStep := make(map[string][]string, len(fragment.Deps))
	for _, dep := range fragment.Deps {
		if dep.Type != "blocks" && dep.Type != "waits-for" && dep.Type != "conditional-blocks" {
			continue
		}
		depsByStep[dep.StepID] = append(depsByStep[dep.StepID], dep.DependsOnID)
	}
	bindingCache := make(map[string]graphRouteBinding, len(fragment.Steps))
	resolving := make(map[string]bool, len(fragment.Steps))
	routingRigContext := graphRouteRigContext(defaultRoute.qualifiedName)
	for i := range fragment.Steps {
		step := &fragment.Steps[i]
		switch step.Metadata["gc.kind"] {
		case "workflow", "scope":
			continue
		}
		binding, err := resolveGraphStepBinding(step.ID, stepByID, stepAlias, depsByStep, bindingCache, resolving, defaultRoute, routingRigContext, store, cityName, cityPath, cfg)
		if err != nil {
			return err
		}
		if isWorkflowControlKind(step.Metadata["gc.kind"]) {
			assignGraphStepRoute(step, binding, &controlRoute)
			continue
		}
		assignGraphStepRoute(step, binding, nil)
	}
	return nil
}

func graphFallbackBindingForBead(source beads.Bead, store beads.Store, _, cityPath string, cfg *config.City) (graphRouteBinding, error) {
	routedTo := workflowExecutionRoute(source)
	if routedTo == "" {
		return graphRouteBinding{sessionName: source.Assignee}, nil
	}
	if cfg == nil {
		return graphRouteBinding{}, fmt.Errorf("graph.v2 routing for %s requires config", source.ID)
	}

	agentCfg, ok := resolveAgentIdentity(cfg, routedTo, graphRouteRigContext(routedTo))
	if !ok {
		return graphRouteBinding{}, fmt.Errorf("unknown graph.v2 fallback target %q on %s", routedTo, source.ID)
	}

	binding := graphRouteBinding{qualifiedName: agentCfg.QualifiedName()}
	if agentCfg.IsPool() {
		binding.label = "pool:" + agentCfg.QualifiedName()
		return binding, nil
	}
	if source.Assignee != "" {
		binding.sessionName = source.Assignee
		return binding, nil
	}
	sn, err := ensureSessionForTemplate(cityPath, cfg, store, agentCfg.QualifiedName(), io.Discard)
	if err != nil {
		return graphRouteBinding{}, err
	}
	binding.sessionName = sn
	return binding, nil
}

func propagateDynamicScopeMetadata(step *formula.RecipeStep, source beads.Bead) {
	if step == nil {
		return
	}
	if step.Metadata == nil {
		step.Metadata = make(map[string]string)
	}
	if scopeRef := strings.TrimSpace(source.Metadata["gc.scope_ref"]); scopeRef != "" && step.Metadata["gc.scope_ref"] == "" {
		step.Metadata["gc.scope_ref"] = scopeRef
	}
	if onFail := strings.TrimSpace(source.Metadata["gc.on_fail"]); onFail != "" && step.Metadata["gc.on_fail"] == "" {
		step.Metadata["gc.on_fail"] = onFail
	}
	for _, key := range []string{"gc.step_id", "gc.ralph_step_id", "gc.attempt"} {
		if value := strings.TrimSpace(source.Metadata[key]); value != "" && step.Metadata[key] == "" {
			step.Metadata[key] = value
		}
	}
	if step.Metadata["gc.scope_ref"] == "" || step.Metadata["gc.scope_role"] != "" {
		return
	}
	switch step.Metadata["gc.kind"] {
	case "scope":
		return
	case "scope-check", "workflow-finalize", "fanout", "check":
		step.Metadata["gc.scope_role"] = "control"
		return
	default:
		step.Metadata["gc.scope_role"] = "member"
	}
}

func newWorkflowDeleteCmd(stdout, stderr io.Writer) *cobra.Command {
	var force bool
	var deleteBeads bool
	cmd := &cobra.Command{
		Use:   "delete <workflow-id>",
		Short: "Close and optionally delete a workflow and all its beads",
		Long: `Close all open beads in a workflow, then optionally delete them.

Searches all stores (city + rigs) for the workflow root and all beads
with matching gc.root_bead_id. Without --force, shows a preview.

By default, beads are closed with gc.outcome=skipped. Use --delete to
also remove them from the store via bd delete --force.`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdWorkflowDelete(args[0], force, deleteBeads, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVarP(&force, "force", "f", false, "Actually close/delete (without this, shows preview)")
	cmd.Flags().BoolVar(&deleteBeads, "delete", false, "Also delete beads from the store after closing")
	return cmd
}

func cmdWorkflowDelete(workflowID string, force, deleteBeads bool, stdout, stderr io.Writer) int {
	cityPath, err := resolveCity()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "gc workflow delete: %v\n", err)
		return 1
	}
	readDoltPort(cityPath)
	cfg, err := loadCityConfig(cityPath)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "gc workflow delete: %v\n", err)
		return 1
	}

	type storeMatch struct {
		store   beads.Store
		ids     []string
		label   string
		rigPath string // for shelling out to bd delete
	}
	var matches []storeMatch

	if cityStore, err := openStoreAtForCity(cityPath, cityPath); err == nil {
		ids := findWorkflowBeadIDs(cityStore, workflowID)
		if len(ids) > 0 {
			matches = append(matches, storeMatch{store: cityStore, ids: ids, label: "city", rigPath: cityPath})
		}
	}
	for _, rig := range cfg.Rigs {
		rigStore, err := openStoreAtForCity(rig.Path, cityPath)
		if err != nil {
			continue
		}
		ids := findWorkflowBeadIDs(rigStore, workflowID)
		if len(ids) > 0 {
			matches = append(matches, storeMatch{store: rigStore, ids: ids, label: "rig:" + rig.Name, rigPath: rig.Path})
		}
	}

	total := 0
	for _, m := range matches {
		total += len(m.ids)
	}
	if total == 0 {
		_, _ = fmt.Fprintf(stderr, "gc workflow delete: no beads found for workflow %s\n", workflowID)
		return 1
	}

	openCount := 0
	for _, m := range matches {
		for _, id := range m.ids {
			if b, err := m.store.Get(id); err == nil && b.Status != "closed" {
				openCount++
			}
		}
	}

	action := "close"
	if deleteBeads {
		action = "delete"
	}
	_, _ = fmt.Fprintf(stdout, "Workflow %s: %d beads (%d open) — %s\n", workflowID, total, openCount, action)
	for _, m := range matches {
		_, _ = fmt.Fprintf(stdout, "  %s: %d beads\n", m.label, len(m.ids))
	}

	if !force {
		_, _ = fmt.Fprintln(stdout, "\nDry run. Use --force to proceed.")
		return 0
	}

	// Phase 1: Batch close all open beads with gc.outcome=skipped.
	closed := 0
	for _, m := range matches {
		n, _ := m.store.CloseAll(m.ids, map[string]string{"gc.outcome": "skipped"})
		closed += n
	}
	_, _ = fmt.Fprintf(stdout, "Closed %d open beads\n", closed)

	if !deleteBeads {
		return 0
	}

	// Phase 2: Delete via bd delete --force (handles dep cleanup, events, etc.)
	deleted := 0
	for _, m := range matches {
		runner := bdCommandRunnerForCity(cityPath)
		for _, id := range m.ids {
			if _, err := runner(m.rigPath, "bd", "delete", id, "--force"); err != nil {
				_, _ = fmt.Fprintf(stderr, "  delete %s: %v\n", id, err)
				continue
			}
			deleted++
		}
	}
	_, _ = fmt.Fprintf(stdout, "Deleted %d beads\n", deleted)
	return 0
}

func findWorkflowBeadIDs(store beads.Store, workflowID string) []string {
	all, err := store.List()
	if err != nil {
		return nil
	}
	var ids []string
	for _, b := range all {
		if b.ID == workflowID || b.Metadata["gc.root_bead_id"] == workflowID {
			ids = append(ids, b.ID)
		}
	}
	return ids
}
