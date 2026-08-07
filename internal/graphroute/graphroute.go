// Package graphroute decorates compiled formula recipes with graph.v2
// routing metadata. It resolves step assignments to agents, handles
// control dispatcher routing, and manages graph step binding resolution.
package graphroute

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/gastownhall/gascity/internal/agentutil"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/formula"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/storeref"
)

const (
	// GraphExecutionRouteMetaKey is the metadata key for the execution route.
	GraphExecutionRouteMetaKey = beadmeta.ExecutionRoutedToMetadataKey
	// GraphExecutionRigContextMetaKey preserves the formula-layer rig context
	// for control beads whose execution route is a concrete session ID.
	GraphExecutionRigContextMetaKey = beadmeta.ExecutionRigContextMetadataKey
)

// poolWorkflowContinuationGroup is the continuation group value stamped on
// pool-routed graph.v2 steps so preassignHookContinuationGroup keeps all steps
// of a molecule on the same pool slot (fixes #2978).
const poolWorkflowContinuationGroup = "pool-workflow"

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
	RigContext      string
	MetadataOnly    bool
}

type graphStepTarget struct {
	value        string
	fromAssignee bool
}

// IsControlDispatcherKind reports whether a gc.kind value is a control-
// dispatcher kind (routed to the control dispatcher agent). This is exactly
// beadmeta.ControlKinds: every kind the ProcessControl switch executes is
// routed to the control dispatcher.
func IsControlDispatcherKind(kind string) bool {
	return beadmeta.IsControlKind(kind)
}

// IsWorkflowTopologyKind reports whether a gc.kind value identifies a
// workflow-topology step (root workflow, scope latch, or formula spec).
// Routing never lands on these — they exist to structure the graph, not
// to be claimed by an agent.
func IsWorkflowTopologyKind(kind string) bool {
	return slices.Contains(beadmeta.WorkflowTopologyKinds, kind)
}

// IsCompiledGraphWorkflow reports whether a compiled recipe is a graph.v2
// workflow.
func IsCompiledGraphWorkflow(recipe *formula.Recipe) bool {
	if recipe == nil || len(recipe.Steps) == 0 {
		return false
	}
	root := recipe.Steps[0]
	return root.Metadata[beadmeta.KindMetadataKey] == beadmeta.KindWorkflow && root.Metadata[beadmeta.FormulaContractMetadataKey] == beadmeta.FormulaContractGraphV2
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

func graphBindingRigContext(binding GraphRouteBinding) string {
	if rigContext := strings.TrimSpace(binding.RigContext); rigContext != "" {
		return rigContext
	}
	return GraphRouteRigContext(binding.QualifiedName)
}

func graphBindingHasExecutionContext(binding GraphRouteBinding) bool {
	return strings.TrimSpace(binding.QualifiedName) != "" ||
		strings.TrimSpace(binding.SessionName) != "" ||
		strings.TrimSpace(binding.DirectSessionID) != "" ||
		strings.TrimSpace(binding.RigContext) != ""
}

func graphDirectSessionRigContext(target, rigContext string, bead beads.Bead) string {
	if rigContext = strings.TrimSpace(rigContext); rigContext != "" {
		return rigContext
	}
	if rigContext = GraphRouteRigContext(target); rigContext != "" {
		return rigContext
	}
	for _, candidate := range []string{
		bead.Metadata[session.NamedSessionIdentityMetadata],
		bead.Metadata["alias"],
		bead.Metadata["template"],
	} {
		if rigContext = GraphRouteRigContext(candidate); rigContext != "" {
			return rigContext
		}
	}
	return ""
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
	return graphStepTarget{value: strings.TrimSpace(formula.Substitute(step.Metadata[beadmeta.RunTargetMetadataKey], routeVars))}
}

// ApplyGraphRouteBinding sets the routing metadata on a recipe step.
func ApplyGraphRouteBinding(step *formula.RecipeStep, binding GraphRouteBinding) {
	// Clear any prior session back-references so the metadata always matches
	// the current binding when a step is re-decorated (#2843).
	delete(step.Metadata, beadmeta.SessionNameMetadataKey)
	delete(step.Metadata, beadmeta.SessionIDMetadataKey)
	if binding.DirectSessionID != "" {
		delete(step.Metadata, beadmeta.RoutedToMetadataKey)
		// Durably record the bound session so consumers (e.g. the dashboard
		// run-detail session/diff views) can resolve the step's session after
		// the transient Assignee is cleared on close. (#2843)
		step.Metadata[beadmeta.SessionIDMetadataKey] = binding.DirectSessionID
		step.Assignee = binding.DirectSessionID
		return
	}
	step.Metadata[beadmeta.RoutedToMetadataKey] = binding.QualifiedName
	if binding.MetadataOnly {
		// Pool-routed step: stamp continuation group so preassignHookContinuationGroup
		// pre-assigns all molecule steps to the claiming slot, preventing scatter
		// across pool slots (fixes #2978).
		step.Metadata[beadmeta.ContinuationGroupMetadataKey] = poolWorkflowContinuationGroup
		step.Metadata[beadmeta.SessionAffinityMetadataKey] = "require"
		step.Assignee = ""
		return
	}
	if binding.SessionName != "" {
		// Durable session back-reference for single-session agents (#2843).
		// Pool agents resolve MetadataOnly above and bind a concrete session
		// only when a slot claims the step — out of scope for route-time.
		step.Metadata[beadmeta.SessionNameMetadataKey] = binding.SessionName
	}
	step.Assignee = binding.SessionName
}

// ApplyGraphControlRouteBinding routes control steps to the store-scoped
// control-dispatcher config queue. Direct session assignment is reserved for
// already-existing concrete session owners, not future on-demand sessions.
func ApplyGraphControlRouteBinding(step *formula.RecipeStep, binding GraphRouteBinding) {
	// Clear any prior session back-references so the metadata matches the
	// current binding when a control step is re-decorated (#2843).
	delete(step.Metadata, beadmeta.SessionNameMetadataKey)
	delete(step.Metadata, beadmeta.SessionIDMetadataKey)
	// Clear the retired rig→city fallback marker when an existing recipe is
	// re-decorated. Route identity now always matches the graph store scope.
	delete(step.Metadata, beadmeta.ControlDispatcherFallbackMetadataKey)
	if binding.QualifiedName != "" {
		step.Metadata[beadmeta.RoutedToMetadataKey] = binding.QualifiedName
	} else {
		delete(step.Metadata, beadmeta.RoutedToMetadataKey)
	}
	step.Assignee = ""
}

// AssignGraphStepRoute applies routing to a step, optionally diverting
// control steps to the control dispatcher.
func AssignGraphStepRoute(step *formula.RecipeStep, executionBinding GraphRouteBinding, controlBinding *GraphRouteBinding) {
	if controlBinding != nil {
		switch {
		case executionBinding.QualifiedName != "":
			step.Metadata[GraphExecutionRouteMetaKey] = executionBinding.QualifiedName
		case executionBinding.DirectSessionID != "":
			step.Metadata[GraphExecutionRouteMetaKey] = executionBinding.DirectSessionID
		default:
			delete(step.Metadata, GraphExecutionRouteMetaKey)
		}
		if rigContext := graphBindingRigContext(executionBinding); rigContext != "" {
			step.Metadata[GraphExecutionRigContextMetaKey] = rigContext
		} else {
			delete(step.Metadata, GraphExecutionRigContextMetaKey)
		}
		ApplyGraphControlRouteBinding(step, *controlBinding)
		return
	}
	delete(step.Metadata, GraphExecutionRouteMetaKey)
	delete(step.Metadata, GraphExecutionRigContextMetaKey)
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
	return strings.TrimSpace(meta[beadmeta.RoutedToMetadataKey])
}

// WorkflowExecutionRoute extracts the execution route from a bead.
func WorkflowExecutionRoute(bead beads.Bead) string {
	return WorkflowExecutionRouteFromMeta(bead.Metadata)
}

// ControlDispatcherBinding resolves the graph routing binding for the control
// dispatcher in the graph's store scope. Runtime health does not change route
// identity: desired-state reconciliation starts or recovers the configured
// dispatcher for that scope.
func ControlDispatcherBinding(store beads.Store, cityName string, cfg *config.City, rigContext string, deps Deps) (GraphRouteBinding, error) {
	return resolveControlDispatcherBinding(store, cityName, cfg, rigContext, deps)
}

// resolveControlDispatcherBinding resolves the control-dispatcher binding for a
// graph store scope.
func resolveControlDispatcherBinding(_ beads.Store, _ string, cfg *config.City, rigContext string, deps Deps) (GraphRouteBinding, error) {
	if cfg == nil {
		return GraphRouteBinding{}, fmt.Errorf("control-dispatcher route requires config")
	}
	if deps.Resolver == nil {
		return GraphRouteBinding{}, fmt.Errorf("ResolveAgent not configured")
	}
	// Primary lookup: find the deterministic control-dispatcher directly, by
	// behavior (IsDeterministicControlDispatcher) rather than bare-name string
	// match. Since 9fa6b7fec the dispatcher ships bound (core.control-dispatcher),
	// so AgentMatchesIdentity rejects the bare-name fallback for it and the
	// per-rig fleet makes the bare-name scan ambiguous — both break a
	// Resolver-based lookup. ControlDispatcherForScope selects the configured
	// dispatcher whose scope matches the graph store.
	if agentCfg, ok := config.ControlDispatcherForScope(cfg, rigContext); ok {
		return GraphRouteBinding{QualifiedName: agentutil.RoutedToIdentity(&agentCfg), MetadataOnly: true}, nil
	}
	// Fallback for configs without a deterministic dispatcher (e.g. a plain
	// control-dispatcher agent carrying no convoy-control StartCommand): defer
	// to the name-based resolver path, preserving the rig-context preference.
	agentCfg, ok := deps.Resolver.ResolveAgent(cfg, config.ControlDispatcherAgentName, rigContext)
	if !ok || strings.TrimSpace(agentCfg.Dir) != strings.TrimSpace(rigContext) {
		if strings.TrimSpace(rigContext) != "" {
			return GraphRouteBinding{}, fmt.Errorf("control-dispatcher agent for rig %q not found", strings.TrimSpace(rigContext))
		}
		return GraphRouteBinding{}, fmt.Errorf("city control-dispatcher agent %q not found", config.ControlDispatcherAgentName)
	}
	return GraphRouteBinding{QualifiedName: agentutil.RoutedToIdentity(&agentCfg), MetadataOnly: true}, nil
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
		return GraphRouteBinding{}, fmt.Errorf("formulas v2 routing cycle while resolving %s", stepID)
	}
	step := stepByID[stepID]
	if step == nil {
		return fallback, nil
	}
	resolving[stepID] = true
	defer delete(resolving, stepID)

	target := parseGraphStepRouteTarget(step, routeVars)
	if target.value == "" {
		switch step.Metadata[beadmeta.KindMetadataKey] {
		case "scope-check":
			controlTarget := strings.TrimSpace(step.Metadata[beadmeta.ControlForMetadataKey])
			if controlTarget != "" {
				binding, err := ResolveGraphStepBindingWithVars(controlTarget, stepByID, stepAlias, depsByStep, cache, resolving, routeVars, fallback, rigContext, store, cityName, cfg, deps)
				if err != nil {
					return GraphRouteBinding{}, err
				}
				cache[stepID] = binding
				return binding, nil
			}
		case "fanout":
			controlTarget := strings.TrimSpace(step.Metadata[beadmeta.ControlForMetadataKey])
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
				switch depStep.Metadata[beadmeta.KindMetadataKey] {
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
		return GraphRouteBinding{}, fmt.Errorf("formulas v2 routing for %s requires config", stepID)
	}
	if deps.Resolver == nil {
		return GraphRouteBinding{}, fmt.Errorf("ResolveAgent not configured")
	}
	if target.fromAssignee {
		if binding, ok, err := ResolveGraphDirectSessionBinding(store, cityName, cfg, target.value, rigContext, deps); err != nil {
			return GraphRouteBinding{}, fmt.Errorf("step %s: %w", stepID, err)
		} else if ok {
			cache[stepID] = binding
			return binding, nil
		}
		return GraphRouteBinding{}, fmt.Errorf("step %s: assignee target %q did not resolve to a concrete session; use gc.run_target for config routing", stepID, target.value)
	}
	agentCfg, ok := deps.Resolver.ResolveAgent(cfg, target.value, rigContext)
	if !ok {
		return GraphRouteBinding{}, fmt.Errorf("step %s: unknown formulas v2 target %q", stepID, target.value)
	}
	binding := GraphRouteBinding{QualifiedName: agentutil.RoutedToIdentity(&agentCfg)}
	if agentCfg.SupportsInstanceExpansion() {
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

// ResolveGraphDirectSessionBinding resolves a direct-session route target to a
// concrete session bead, returning ok=false when the target is not a
// direct-session reference (so the caller can fall back to config-agent
// routing). Exact session bead IDs win over config target names; config-named
// sessions are materialized through deps.DirectSessionResolver. This is the
// canonical implementation shared by the graphroute library path and the CLI
// projection in cmd/gc.
func ResolveGraphDirectSessionBinding(store beads.Store, cityName string, cfg *config.City, target, rigContext string, deps Deps) (GraphRouteBinding, bool, error) {
	target = strings.TrimSpace(target)
	if store == nil || target == "" {
		return GraphRouteBinding{}, false, nil
	}
	// Exact session bead IDs are unambiguous and must win even when they
	// collide with a config target name.
	if id, err := session.ResolveSessionIDByExactID(store, target); err == nil {
		if bead, getErr := store.Get(id); getErr == nil && session.IsSessionBeadOrRepairable(bead) && bead.Status != "closed" {
			return GraphRouteBinding{DirectSessionID: bead.ID, RigContext: graphDirectSessionRigContext(target, rigContext, bead)}, true, nil
		}
	}
	if deps.DirectSessionResolver != nil {
		id, ok, err := deps.DirectSessionResolver(store, cityName, deps.CityPath, cfg, target, rigContext)
		if err != nil {
			return GraphRouteBinding{}, false, err
		}
		if ok {
			binding := GraphRouteBinding{DirectSessionID: id, RigContext: strings.TrimSpace(rigContext)}
			if binding.RigContext == "" {
				binding.RigContext = GraphRouteRigContext(target)
			}
			if binding.RigContext == "" {
				if bead, getErr := store.Get(id); getErr == nil {
					binding.RigContext = graphDirectSessionRigContext(target, rigContext, bead)
				}
			}
			return binding, true, nil
		}
	}
	if cfg != nil && deps.Resolver != nil {
		if _, ok := deps.Resolver.ResolveAgent(cfg, target, rigContext); ok {
			return GraphRouteBinding{}, false, nil
		}
	}
	if id, err := session.ResolveSessionID(store, target); err == nil {
		if bead, getErr := store.Get(id); getErr == nil && session.IsSessionBeadOrRepairable(bead) && bead.Status != "closed" {
			return GraphRouteBinding{DirectSessionID: bead.ID, RigContext: graphDirectSessionRigContext(target, rigContext, bead)}, true, nil
		}
	}
	return GraphRouteBinding{}, false, nil
}

// DecorateGraphWorkflowRecipe applies routing metadata to all steps in a
// graph.v2 workflow recipe.
func DecorateGraphWorkflowRecipe(recipe *formula.Recipe, routeVars map[string]string, sourceBeadID, scopeKind, scopeRef, rootStoreRef, routedTo, sessionName string, store beads.Store, cityName string, cfg *config.City, deps Deps) error {
	defaultRoute := GraphRouteBinding{QualifiedName: routedTo}
	if sessionName != "" {
		defaultRoute.SessionName = sessionName
	} else {
		defaultRoute.MetadataOnly = true
	}
	return DecorateGraphWorkflowRecipeWithDefaultBinding(recipe, routeVars, sourceBeadID, scopeKind, scopeRef, rootStoreRef, defaultRoute, store, cityName, cfg, deps)
}

// DecorateGraphWorkflowRecipeWithDefaultBinding applies routing metadata to all
// steps in a graph.v2 workflow recipe using a pre-resolved default route.
func DecorateGraphWorkflowRecipeWithDefaultBinding(recipe *formula.Recipe, routeVars map[string]string, sourceBeadID, scopeKind, scopeRef, rootStoreRef string, defaultRoute GraphRouteBinding, store beads.Store, cityName string, cfg *config.City, deps Deps) error {
	if recipe == nil {
		return fmt.Errorf("workflow recipe is nil")
	}
	routedTo := strings.TrimSpace(defaultRoute.QualifiedName)
	rootSessionName := strings.TrimSpace(defaultRoute.SessionName)
	executionRigContext := graphBindingRigContext(defaultRoute)
	controlRigContext := executionRigContext
	if storeRigContext, scoped := storeref.ScopeRigContext(rootStoreRef); scoped {
		controlRigContext = storeRigContext
		// A graph with no default execution binding (for example a no-pool rig
		// order whose workers all declare gc.run_target) resolves bare targets in
		// its owning scope. Otherwise the default binding defines the execution
		// context, which may intentionally differ from graph ownership.
		if !graphBindingHasExecutionContext(defaultRoute) {
			executionRigContext = storeRigContext
		}
	}
	controlRoute, err := ControlDispatcherBinding(store, cityName, cfg, controlRigContext, deps)
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
			step.Metadata[beadmeta.RootStoreRefMetadataKey] = rootStoreRef
		}
		if step.IsRoot {
			// gc.routed_to is the canonical (and sole) persisted delivery key
			// every runtime demand/claim/scale reader consults; the workflow root
			// must carry it to be claimable, exactly like its own child steps and
			// every legacy bead. Without it a pool-routed root is spawned-for by
			// scale_check but never claimed by the worker, then idle-reaped
			// (fixes #2763; gc.run_target retired as a wire field — ga-eld2x).
			step.Metadata[beadmeta.RoutedToMetadataKey] = routedTo
			delete(step.Metadata, beadmeta.RunTargetMetadataKey)
			if rootSessionName != "" {
				// Durable session back-reference on the run root for
				// single-session agents (#2843). Empty for pool agents.
				step.Metadata[beadmeta.SessionNameMetadataKey] = rootSessionName
			}
			if sourceBeadID != "" {
				step.Metadata[beadmeta.SourceBeadIDMetadataKey] = sourceBeadID
				if rootStoreRef != "" {
					step.Metadata[beadmeta.SourceStoreRefMetadataKey] = rootStoreRef
				}
			}
			if scopeKind != "" {
				step.Metadata[beadmeta.ScopeKindMetadataKey] = scopeKind
			}
			if scopeRef != "" {
				step.Metadata[beadmeta.ScopeRefMetadataKey] = scopeRef
			}
			continue
		}
		if IsWorkflowTopologyKind(step.Metadata[beadmeta.KindMetadataKey]) {
			continue
		}
		binding, err := ResolveGraphStepBindingWithVars(step.ID, stepByID, stepAlias, depsByStep, bindingCache, resolvingSet, routeVars, defaultRoute, executionRigContext, store, cityName, cfg, deps)
		if err != nil {
			return err
		}
		if IsControlDispatcherKind(step.Metadata[beadmeta.KindMetadataKey]) {
			AssignGraphStepRoute(step, binding, &controlRoute)
			continue
		}
		AssignGraphStepRoute(step, binding, nil)
	}
	return nil
}

// ApplyGraphRouting decorates a compiled recipe with routing metadata.
// For graph.v2 workflows it delegates to DecorateGraphWorkflowRecipe. For
// standalone legacy [[steps]] recipes it stamps gc.routed_to on every
// non-root step so EffectiveWorkQuery tier-3 and pool scale_check can see
// the work (fixes #796). Attached legacy formulas intentionally stay on the
// molecule_id flow: only the source bead is routed, and the internal molecule
// steps remain private instructions for the assignee. Pool demand for attached
// legacy formulas comes from the already-routed source bead via the ready and
// in_progress tiers; the molecule count is only for standalone routed roots.
// Returns early with no effect when cfg is nil.
func ApplyGraphRouting(recipe *formula.Recipe, a *config.Agent, routedTo string, vars map[string]string, sourceBeadID, scopeKind, scopeRef, storeRef string, store beads.Store, cityName string, cfg *config.City, deps Deps) error {
	if recipe == nil || cfg == nil {
		return nil
	}

	// Legacy path runs before agent resolution: it needs only the routedTo
	// string, and skipping the ResolveAgent call avoids a config-map lookup
	// and Agent deep-copy on every controller tick that dispatches a legacy
	// order.
	if !IsCompiledGraphWorkflow(recipe) {
		if strings.TrimSpace(sourceBeadID) != "" {
			return nil
		}
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
	if !a.SupportsInstanceExpansion() {
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
//
// Per-step gc.run_target is the formula author's compile-time routing intent:
// when a step declares a target via gc.run_target, the stamper resolves it into
// gc.routed_to (the sole persisted routing key — ga-eld2x) instead of the
// convoy-wide default. Without this, the blanket routedTo clobbers per-step
// targets and every child looks routed to the convoy entry agent (see adaf6ec /
// PR #2386).
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
		if IsWorkflowTopologyKind(step.Metadata[beadmeta.KindMetadataKey]) {
			continue
		}
		if step.Metadata == nil {
			step.Metadata = make(map[string]string, 1)
		}
		target := routedTo
		if perStep := strings.TrimSpace(step.Metadata[beadmeta.RunTargetMetadataKey]); perStep != "" {
			target = perStep
		}
		step.Metadata[beadmeta.RoutedToMetadataKey] = target
	}
}
