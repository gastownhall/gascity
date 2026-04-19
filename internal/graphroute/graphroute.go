// Package graphroute decorates compiled formula recipes with graph.v2
// routing metadata. It resolves step assignments to agents, handles
// control dispatcher routing, and manages graph step binding resolution.
package graphroute

import (
	"fmt"
	"maps"
	"strings"

	"github.com/gastownhall/gascity/internal/agentutil"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/formula"
	"github.com/gastownhall/gascity/internal/session"
)

// GraphExecutionRouteMetaKey is the metadata key for the execution route.
const GraphExecutionRouteMetaKey = "gc.execution_routed_to"

// AgentResolver resolves an agent name to a config.Agent.
type AgentResolver interface {
	ResolveAgent(cfg *config.City, name, rigContext string) (config.Agent, bool)
}

// DirectSessionResolver optionally materializes or resolves a direct
// assignee target to a concrete session bead ID.
type DirectSessionResolver func(store beads.Store, cityName, cityPath string, cfg *config.City, target, rigContext string) (string, bool, error)

// Deps provides the narrow dependencies needed for graph routing.
type Deps struct {
	Resolver              AgentResolver
	CityPath              string
	DirectSessionResolver DirectSessionResolver
}

// GraphRouteBinding captures how a graph.v2 step is routed to an agent.
type GraphRouteBinding struct {
	QualifiedName string
	SessionName   string
	// DirectSessionID bypasses config routing and assigns the step to a
	// concrete session bead ID. When set, gc.routed_to is intentionally
	// omitted because execution already targets a specific session.
	DirectSessionID string
	MetadataOnly    bool
}

type graphStepTarget struct {
	value        string
	fromAssignee bool
}

// IsControlDispatcherKind reports whether a gc.kind value is a control-
// dispatcher kind (routed to the control dispatcher agent).
func IsControlDispatcherKind(kind string) bool {
	switch kind {
	case "check", "fanout", "retry-eval", "scope-check", "workflow-finalize", "retry", "ralph":
		return true
	default:
		return false
	}
}

// IsWorkflowTopologyKind reports whether a gc.kind value identifies a
// workflow-topology step (root workflow, scope latch, or formula spec).
// Routing never lands on these — they exist to structure the graph, not
// to be claimed by an agent.
func IsWorkflowTopologyKind(kind string) bool {
	switch kind {
	case "workflow", "scope", "spec":
		return true
	default:
		return false
	}
}

// IsCompiledGraphWorkflow reports whether a compiled recipe is a graph.v2
// workflow.
func IsCompiledGraphWorkflow(recipe *formula.Recipe) bool {
	if recipe == nil || len(recipe.Steps) == 0 {
		return false
	}
	root := recipe.Steps[0]
	return root.Metadata["gc.kind"] == "workflow" && root.Metadata["gc.formula_contract"] == "graph.v2"
}

// GraphWorkflowRouteVars builds the route variable map by merging recipe
// defaults with user-provided variables.
func GraphWorkflowRouteVars(recipe *formula.Recipe, provided map[string]string) map[string]string {
	routeVars := make(map[string]string, len(provided))
	if recipe != nil {
		for name, def := range recipe.Vars {
			if def != nil && def.Default != nil {
				routeVars[name] = *def.Default
			}
		}
	}
	maps.Copy(routeVars, provided)
	return routeVars
}

// GraphRouteRigContext extracts the rig context (directory prefix) from
// a qualified agent name like "rig/agent".
func GraphRouteRigContext(route string) string {
	route = strings.TrimSpace(route)
	if route == "" {
		return ""
	}
	idx := strings.LastIndex(route, "/")
	if idx <= 0 {
		return ""
	}
	return route[:idx]
}

// GraphStepRouteTarget extracts the route target from a step's direct-session
// assignee or gc.run_target metadata, applying variable substitution.
func GraphStepRouteTarget(step *formula.RecipeStep, routeVars map[string]string) string {
	return parseGraphStepRouteTarget(step, routeVars).value
}

func parseGraphStepRouteTarget(step *formula.RecipeStep, routeVars map[string]string) graphStepTarget {
	if step == nil {
		return graphStepTarget{}
	}
	target := strings.TrimSpace(formula.Substitute(step.Assignee, routeVars))
	if target != "" {
		return graphStepTarget{value: target, fromAssignee: true}
	}
	if step.Metadata == nil {
		return graphStepTarget{}
	}
	return graphStepTarget{value: strings.TrimSpace(formula.Substitute(step.Metadata["gc.run_target"], routeVars))}
}

// ApplyGraphRouteBinding sets the routing metadata on a recipe step.
func ApplyGraphRouteBinding(step *formula.RecipeStep, binding GraphRouteBinding) {
	if binding.DirectSessionID != "" {
		delete(step.Metadata, "gc.routed_to")
		step.Assignee = binding.DirectSessionID
		return
	}
	step.Metadata["gc.routed_to"] = binding.QualifiedName
	if binding.MetadataOnly {
		step.Assignee = ""
		return
	}
	step.Assignee = binding.SessionName
}

// AssignGraphStepRoute applies routing to a step, optionally diverting
// control steps to the control dispatcher.
func AssignGraphStepRoute(step *formula.RecipeStep, executionBinding GraphRouteBinding, controlBinding *GraphRouteBinding) {
	if controlBinding != nil {
		if executionBinding.QualifiedName != "" {
			step.Metadata[GraphExecutionRouteMetaKey] = executionBinding.QualifiedName
		} else {
			delete(step.Metadata, GraphExecutionRouteMetaKey)
		}
		ApplyGraphRouteBinding(step, *controlBinding)
		return
	}
	delete(step.Metadata, GraphExecutionRouteMetaKey)
	ApplyGraphRouteBinding(step, executionBinding)
}

// WorkflowExecutionRouteFromMeta extracts the execution route from bead metadata.
func WorkflowExecutionRouteFromMeta(meta map[string]string) string {
	if meta == nil {
		return ""
	}
	if routedTo := strings.TrimSpace(meta[GraphExecutionRouteMetaKey]); routedTo != "" {
		return routedTo
	}
	return strings.TrimSpace(meta["gc.routed_to"])
}

// WorkflowExecutionRoute extracts the execution route from a bead.
func WorkflowExecutionRoute(bead beads.Bead) string {
	return WorkflowExecutionRouteFromMeta(bead.Metadata)
}

// ControlDispatcherBinding resolves the graph routing binding for the
// control dispatcher agent.
func ControlDispatcherBinding(store beads.Store, cityName string, cfg *config.City, rigContext string, deps Deps) (GraphRouteBinding, error) {
	if cfg == nil {
		return GraphRouteBinding{}, fmt.Errorf("control-dispatcher route requires config")
	}
	if deps.Resolver == nil {
		return GraphRouteBinding{}, fmt.Errorf("ResolveAgent not configured")
	}
	agentCfg, ok := deps.Resolver.ResolveAgent(cfg, config.ControlDispatcherAgentName, rigContext)
	if !ok {
		return GraphRouteBinding{}, fmt.Errorf("control-dispatcher agent %q not found", config.ControlDispatcherAgentName)
	}
	binding := GraphRouteBinding{QualifiedName: agentCfg.QualifiedName()}
	if agentutil.IsMultiSessionAgent(&agentCfg) {
		return binding, nil
	}
	sn := agentutil.LookupSessionName(store, cityName, agentCfg.QualifiedName(), cfg.Workspace.SessionTemplate)
	if sn == "" {
		return GraphRouteBinding{}, fmt.Errorf("could not resolve session name for %q", agentCfg.QualifiedName())
	}
	binding.SessionName = sn
	return binding, nil
}

// ResolveGraphStepBinding resolves the routing binding for a graph step
// (without route variables).
func ResolveGraphStepBinding(stepID string, stepByID map[string]*formula.RecipeStep, stepAlias map[string]string, depsByStep map[string][]string, cache map[string]GraphRouteBinding, resolving map[string]bool, fallback GraphRouteBinding, rigContext string, store beads.Store, cityName string, cfg *config.City, deps Deps) (GraphRouteBinding, error) {
	return ResolveGraphStepBindingWithVars(stepID, stepByID, stepAlias, depsByStep, cache, resolving, nil, fallback, rigContext, store, cityName, cfg, deps)
}

// ResolveGraphStepBindingWithVars resolves the routing binding for a graph
// step with variable substitution support.
func ResolveGraphStepBindingWithVars(stepID string, stepByID map[string]*formula.RecipeStep, stepAlias map[string]string, depsByStep map[string][]string, cache map[string]GraphRouteBinding, resolving map[string]bool, routeVars map[string]string, fallback GraphRouteBinding, rigContext string, store beads.Store, cityName string, cfg *config.City, deps Deps) (GraphRouteBinding, error) {
	if aliased, ok := stepAlias[stepID]; ok {
		stepID = aliased
	}
	if binding, ok := cache[stepID]; ok {
		return binding, nil
	}
	if resolving[stepID] {
		return GraphRouteBinding{}, fmt.Errorf("graph.v2 routing cycle while resolving %s", stepID)
	}
	step := stepByID[stepID]
	if step == nil {
		return fallback, nil
	}
	resolving[stepID] = true
	defer delete(resolving, stepID)

	target := parseGraphStepRouteTarget(step, routeVars)
	if target.value == "" {
		switch step.Metadata["gc.kind"] {
		case "scope-check":
			controlTarget := strings.TrimSpace(step.Metadata["gc.control_for"])
			if controlTarget != "" {
				binding, err := ResolveGraphStepBindingWithVars(controlTarget, stepByID, stepAlias, depsByStep, cache, resolving, routeVars, fallback, rigContext, store, cityName, cfg, deps)
				if err != nil {
					return GraphRouteBinding{}, err
				}
				cache[stepID] = binding
				return binding, nil
			}
		case "fanout":
			controlTarget := strings.TrimSpace(step.Metadata["gc.control_for"])
			if controlTarget != "" {
				binding, err := ResolveGraphStepBindingWithVars(controlTarget, stepByID, stepAlias, depsByStep, cache, resolving, routeVars, fallback, rigContext, store, cityName, cfg, deps)
				if err != nil {
					return GraphRouteBinding{}, err
				}
				cache[stepID] = binding
				return binding, nil
			}
		case "workflow-finalize":
			cache[stepID] = fallback
			return fallback, nil
		case "retry-eval":
			var subjectID string
			for _, depID := range depsByStep[step.ID] {
				depStep := stepByID[depID]
				if depStep == nil {
					continue
				}
				switch depStep.Metadata["gc.kind"] {
				case "retry-run", "run":
					subjectID = depID
				}
				if subjectID != "" {
					break
				}
			}
			if subjectID == "" && len(depsByStep[step.ID]) > 0 {
				subjectID = depsByStep[step.ID][0]
			}
			if subjectID != "" {
				binding, err := ResolveGraphStepBindingWithVars(subjectID, stepByID, stepAlias, depsByStep, cache, resolving, routeVars, fallback, rigContext, store, cityName, cfg, deps)
				if err != nil {
					return GraphRouteBinding{}, err
				}
				cache[stepID] = binding
				return binding, nil
			}
		case "check":
			var resolved GraphRouteBinding
			found := false
			for _, depID := range depsByStep[step.ID] {
				if depID == "" {
					continue
				}
				binding, err := ResolveGraphStepBindingWithVars(depID, stepByID, stepAlias, depsByStep, cache, resolving, routeVars, fallback, rigContext, store, cityName, cfg, deps)
				if err != nil {
					return GraphRouteBinding{}, err
				}
				if !found {
					resolved = binding
					found = true
					continue
				}
				if binding != resolved {
					return GraphRouteBinding{}, fmt.Errorf("step %s: inconsistent control routing between deps (%+v vs %+v)", stepID, resolved, binding)
				}
			}
			if found {
				cache[stepID] = resolved
				return resolved, nil
			}
		}
		cache[stepID] = fallback
		return fallback, nil
	}

	if cfg == nil {
		return GraphRouteBinding{}, fmt.Errorf("graph.v2 routing for %s requires config", stepID)
	}
	if deps.Resolver == nil {
		return GraphRouteBinding{}, fmt.Errorf("ResolveAgent not configured")
	}
	if target.fromAssignee {
		if binding, ok, err := resolveGraphDirectSessionBinding(store, cityName, cfg, target.value, rigContext, deps); err != nil {
			return GraphRouteBinding{}, fmt.Errorf("step %s: %w", stepID, err)
		} else if ok {
			cache[stepID] = binding
			return binding, nil
		}
		return GraphRouteBinding{}, fmt.Errorf("step %s: assignee target %q did not resolve to a concrete session; use gc.run_target for config routing", stepID, target.value)
	}
	agentCfg, ok := deps.Resolver.ResolveAgent(cfg, target.value, rigContext)
	if !ok {
		return GraphRouteBinding{}, fmt.Errorf("step %s: unknown graph.v2 target %q", stepID, target.value)
	}
	binding := GraphRouteBinding{QualifiedName: agentCfg.QualifiedName()}
	if agentutil.IsMultiSessionAgent(&agentCfg) {
		binding.MetadataOnly = true
		cache[stepID] = binding
		return binding, nil
	}
	sn := agentutil.LookupSessionName(store, cityName, agentCfg.QualifiedName(), cfg.Workspace.SessionTemplate)
	if sn == "" {
		return GraphRouteBinding{}, fmt.Errorf("step %s: could not resolve session name for %q", stepID, agentCfg.QualifiedName())
	}
	binding.SessionName = sn
	cache[stepID] = binding
	return binding, nil
}

func resolveGraphDirectSessionBinding(store beads.Store, cityName string, cfg *config.City, target, rigContext string, deps Deps) (GraphRouteBinding, bool, error) {
	target = strings.TrimSpace(target)
	if store == nil || target == "" {
		return GraphRouteBinding{}, false, nil
	}
	if deps.DirectSessionResolver != nil {
		id, ok, err := deps.DirectSessionResolver(store, cityName, deps.CityPath, cfg, target, rigContext)
		if err != nil {
			return GraphRouteBinding{}, false, err
		}
		if ok {
			return GraphRouteBinding{DirectSessionID: id}, true, nil
		}
	}
	// Exact session bead IDs are unambiguous and must win even when they
	// collide with a config target name.
	if id, err := session.ResolveSessionIDByExactID(store, target); err == nil {
		if bead, getErr := store.Get(id); getErr == nil && session.IsSessionBeadOrRepairable(bead) && bead.Status != "closed" {
			return GraphRouteBinding{DirectSessionID: bead.ID}, true, nil
		}
	}
	if cfg != nil && deps.Resolver != nil {
		if _, ok := deps.Resolver.ResolveAgent(cfg, target, rigContext); ok {
			return GraphRouteBinding{}, false, nil
		}
	}
	if id, err := session.ResolveSessionID(store, target); err == nil {
		if bead, getErr := store.Get(id); getErr == nil && session.IsSessionBeadOrRepairable(bead) && bead.Status != "closed" {
			return GraphRouteBinding{DirectSessionID: bead.ID}, true, nil
		}
	}
	return GraphRouteBinding{}, false, nil
}

// DecorateGraphWorkflowRecipe applies routing metadata to all steps in a
// graph.v2 workflow recipe.
func DecorateGraphWorkflowRecipe(recipe *formula.Recipe, routeVars map[string]string, sourceBeadID, scopeKind, scopeRef, rootStoreRef, routedTo, sessionName string, store beads.Store, cityName string, cfg *config.City, deps Deps) error {
	if recipe == nil {
		return fmt.Errorf("workflow recipe is nil")
	}
	defaultRoute := GraphRouteBinding{QualifiedName: routedTo}
	if sessionName != "" {
		defaultRoute.SessionName = sessionName
	} else {
		defaultRoute.MetadataOnly = true
	}
	routingRigContext := GraphRouteRigContext(defaultRoute.QualifiedName)
	controlRoute, err := ControlDispatcherBinding(store, cityName, cfg, routingRigContext, deps)
	if err != nil {
		return err
	}
	stepByID := make(map[string]*formula.RecipeStep, len(recipe.Steps))
	stepAlias := make(map[string]string, len(recipe.Steps))
	for i := range recipe.Steps {
		stepByID[recipe.Steps[i].ID] = &recipe.Steps[i]
		if short, ok := strings.CutPrefix(recipe.Steps[i].ID, recipe.Name+"."); ok {
			stepAlias[short] = recipe.Steps[i].ID
		}
	}
	depsByStep := make(map[string][]string, len(recipe.Deps))
	for _, dep := range recipe.Deps {
		if dep.Type != "blocks" && dep.Type != "waits-for" && dep.Type != "conditional-blocks" {
			continue
		}
		depsByStep[dep.StepID] = append(depsByStep[dep.StepID], dep.DependsOnID)
	}
	bindingCache := make(map[string]GraphRouteBinding, len(recipe.Steps))
	resolvingSet := make(map[string]bool, len(recipe.Steps))
	for i := range recipe.Steps {
		step := &recipe.Steps[i]
		if step.Metadata == nil {
			step.Metadata = make(map[string]string)
		} else {
			step.Metadata = maps.Clone(step.Metadata)
		}
		if rootStoreRef != "" {
			step.Metadata["gc.root_store_ref"] = rootStoreRef
		}
		if step.IsRoot {
			step.Metadata["gc.run_target"] = routedTo
			if sourceBeadID != "" {
				step.Metadata["gc.source_bead_id"] = sourceBeadID
				if rootStoreRef != "" {
					step.Metadata["gc.source_store_ref"] = rootStoreRef
				}
			}
			if scopeKind != "" {
				step.Metadata["gc.scope_kind"] = scopeKind
			}
			if scopeRef != "" {
				step.Metadata["gc.scope_ref"] = scopeRef
			}
			continue
		}
		if IsWorkflowTopologyKind(step.Metadata["gc.kind"]) {
			continue
		}
		binding, err := ResolveGraphStepBindingWithVars(step.ID, stepByID, stepAlias, depsByStep, bindingCache, resolvingSet, routeVars, defaultRoute, routingRigContext, store, cityName, cfg, deps)
		if err != nil {
			return err
		}
		if IsControlDispatcherKind(step.Metadata["gc.kind"]) {
			AssignGraphStepRoute(step, binding, &controlRoute)
			continue
		}
		AssignGraphStepRoute(step, binding, nil)
	}
	return nil
}

// ApplyGraphRouting decorates a compiled recipe with routing metadata.
// For graph.v2 workflows it delegates to DecorateGraphWorkflowRecipe. For
// legacy [[steps]] recipes it stamps gc.routed_to on every non-root step
// so EffectiveWorkQuery tier-3 and pool scale_check can see the work
// (fixes #796). Returns early with no effect when cfg is nil.
func ApplyGraphRouting(recipe *formula.Recipe, a *config.Agent, routedTo string, vars map[string]string, sourceBeadID, scopeKind, scopeRef, storeRef string, store beads.Store, cityName string, cfg *config.City, deps Deps) error {
	if recipe == nil || cfg == nil {
		return nil
	}

	// Legacy path runs before agent resolution: it needs only the routedTo
	// string, and skipping the ResolveAgent call avoids a config-map lookup
	// and Agent deep-copy on every controller tick that dispatches a legacy
	// order.
	if !IsCompiledGraphWorkflow(recipe) {
		stampLegacyRecipeRouting(recipe, routedTo)
		return nil
	}

	// Resolve agent if not provided (order dispatch path).
	if a == nil {
		rigContext := GraphRouteRigContext(routedTo)
		baseName := routedTo
		if i := strings.LastIndex(routedTo, "/"); i >= 0 {
			baseName = routedTo[i+1:]
		}
		if deps.Resolver == nil {
			return nil
		}
		resolved, ok := deps.Resolver.ResolveAgent(cfg, baseName, rigContext)
		if !ok {
			return nil
		}
		a = &resolved
	}

	var sessionName string
	if !agentutil.IsMultiSessionAgent(a) {
		sessionName = agentutil.LookupSessionName(store, cityName, a.QualifiedName(), cfg.Workspace.SessionTemplate)
		if sessionName == "" {
			return fmt.Errorf("could not resolve session name for %q", a.QualifiedName())
		}
	}
	routeVars := GraphWorkflowRouteVars(recipe, vars)
	return DecorateGraphWorkflowRecipe(recipe, routeVars, sourceBeadID, scopeKind, scopeRef, storeRef, routedTo, sessionName, store, cityName, cfg, deps)
}

// stampLegacyRecipeRouting mirrors the graph.v2 path in ApplyGraphRouteBinding:
// routing is set unconditionally on every non-root, non-topology step. The
// root bead is excluded because InstantiateSlingFormula stamps it via the
// SlingResult path.
func stampLegacyRecipeRouting(recipe *formula.Recipe, routedTo string) {
	routedTo = strings.TrimSpace(routedTo)
	if recipe == nil || routedTo == "" {
		return
	}
	for i := range recipe.Steps {
		step := &recipe.Steps[i]
		if step.IsRoot {
			continue
		}
		if IsWorkflowTopologyKind(step.Metadata["gc.kind"]) {
			continue
		}
		if step.Metadata == nil {
			step.Metadata = make(map[string]string, 1)
		}
		step.Metadata["gc.routed_to"] = routedTo
	}
}
