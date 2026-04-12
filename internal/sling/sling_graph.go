package sling

// This file provides backward-compatible exports for graph routing
// types and functions that have moved to internal/graphroute.
// Callers should migrate to importing graphroute directly.

import (
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/formula"
	"github.com/gastownhall/gascity/internal/graphroute"
)

// GraphRouteBinding is an alias for graphroute.GraphRouteBinding.
type GraphRouteBinding = graphroute.GraphRouteBinding

// GraphExecutionRouteMetaKey is an alias for graphroute.GraphExecutionRouteMetaKey.
const GraphExecutionRouteMetaKey = graphroute.GraphExecutionRouteMetaKey

// IsCompiledGraphWorkflow delegates to graphroute.
func IsCompiledGraphWorkflow(recipe *formula.Recipe) bool {
	return graphroute.IsCompiledGraphWorkflow(recipe)
}

// IsControlDispatcherKind delegates to graphroute.
func IsControlDispatcherKind(kind string) bool {
	return graphroute.IsControlDispatcherKind(kind)
}

// GraphRouteRigContext delegates to graphroute.
func GraphRouteRigContext(route string) string {
	return graphroute.GraphRouteRigContext(route)
}

// GraphWorkflowRouteVars delegates to graphroute.
func GraphWorkflowRouteVars(recipe *formula.Recipe, provided map[string]string) map[string]string {
	return graphroute.GraphWorkflowRouteVars(recipe, provided)
}

// ApplyGraphRouteBinding delegates to graphroute.
func ApplyGraphRouteBinding(step *formula.RecipeStep, binding GraphRouteBinding) {
	graphroute.ApplyGraphRouteBinding(step, binding)
}

// AssignGraphStepRoute delegates to graphroute.
func AssignGraphStepRoute(step *formula.RecipeStep, executionBinding GraphRouteBinding, controlBinding *GraphRouteBinding) {
	graphroute.AssignGraphStepRoute(step, executionBinding, controlBinding)
}

// WorkflowExecutionRouteFromMeta delegates to graphroute.
func WorkflowExecutionRouteFromMeta(meta map[string]string) string {
	return graphroute.WorkflowExecutionRouteFromMeta(meta)
}

// WorkflowExecutionRoute delegates to graphroute.
func WorkflowExecutionRoute(bead beads.Bead) string {
	return graphroute.WorkflowExecutionRoute(bead)
}

// ApplyGraphRouting delegates to graphroute with sling deps adapted.
func ApplyGraphRouting(recipe *formula.Recipe, a *config.Agent, routedTo string, vars map[string]string, sourceBeadID, scopeKind, scopeRef, storeRef string, store beads.Store, cityName string, cfg *config.City, deps SlingDeps) error {
	gdeps := graphroute.Deps{CityPath: deps.CityPath}
	if deps.Resolver != nil {
		gdeps.Resolver = deps.Resolver
	}
	if deps.DirectSessionResolver != nil {
		gdeps.DirectSessionResolver = deps.DirectSessionResolver
	}
	return graphroute.ApplyGraphRouting(recipe, a, routedTo, vars, sourceBeadID, scopeKind, scopeRef, storeRef, store, cityName, cfg, gdeps)
}

// ControlDispatcherBinding delegates to graphroute with sling deps adapted.
func ControlDispatcherBinding(store beads.Store, cityName string, cfg *config.City, rigContext string, deps SlingDeps) (GraphRouteBinding, error) {
	gdeps := graphroute.Deps{CityPath: deps.CityPath}
	if deps.Resolver != nil {
		gdeps.Resolver = deps.Resolver
	}
	if deps.DirectSessionResolver != nil {
		gdeps.DirectSessionResolver = deps.DirectSessionResolver
	}
	return graphroute.ControlDispatcherBinding(store, cityName, cfg, rigContext, gdeps)
}

// ResolveGraphStepBindingWithVars delegates to graphroute.
func ResolveGraphStepBindingWithVars(stepID string, stepByID map[string]*formula.RecipeStep, stepAlias map[string]string, depsByStep map[string][]string, cache map[string]GraphRouteBinding, resolving map[string]bool, routeVars map[string]string, fallback GraphRouteBinding, rigContext string, store beads.Store, cityName string, cfg *config.City, deps SlingDeps) (GraphRouteBinding, error) {
	gdeps := graphroute.Deps{CityPath: deps.CityPath}
	if deps.Resolver != nil {
		gdeps.Resolver = deps.Resolver
	}
	if deps.DirectSessionResolver != nil {
		gdeps.DirectSessionResolver = deps.DirectSessionResolver
	}
	return graphroute.ResolveGraphStepBindingWithVars(stepID, stepByID, stepAlias, depsByStep, cache, resolving, routeVars, fallback, rigContext, store, cityName, cfg, gdeps)
}

// DecorateGraphWorkflowRecipe delegates to graphroute.
func DecorateGraphWorkflowRecipe(recipe *formula.Recipe, routeVars map[string]string, sourceBeadID, scopeKind, scopeRef, rootStoreRef, routedTo, sessionName string, store beads.Store, cityName string, cfg *config.City, deps SlingDeps) error {
	gdeps := graphroute.Deps{CityPath: deps.CityPath}
	if deps.Resolver != nil {
		gdeps.Resolver = deps.Resolver
	}
	if deps.DirectSessionResolver != nil {
		gdeps.DirectSessionResolver = deps.DirectSessionResolver
	}
	return graphroute.DecorateGraphWorkflowRecipe(recipe, routeVars, sourceBeadID, scopeKind, scopeRef, rootStoreRef, routedTo, sessionName, store, cityName, cfg, gdeps)
}
