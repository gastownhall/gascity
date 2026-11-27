package main

import (
	"strings"

	"github.com/gastownhall/gascity/internal/agentutil"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// Route adoption: the route-recovery lane's second route source.
//
// A bead made with plain `bd create` carries no gc.routed_to and no carried
// route, so every pool-facing surface (the Tier 3 work query, the reconciler
// count-form, the hook claim predicate, controller demand) fails closed on it
// by design, and carriedPoolRoute has nothing to restore. An operator who
// wants such work served names one agent per store scope with
// unrouted_work_target; the lane then compare-and-sets gc.routed_to to the
// value built-in `gc sling <target> <id>` writes. Go applies the configured
// route and never infers one: a scope with no target adopts nothing.

// unroutedAdoptionScope is one store scope's adoption policy.
type unroutedAdoptionScope struct {
	// label names the scope the way the lane's leg labels do: "city" or
	// "rig <name>".
	label string
	// route is the gc.routed_to value adoption stamps, or "" when the scope
	// sets no unrouted_work_target.
	route string
}

// unroutedAdoptionPolicy maps a bead's owning store scope, found by its ID
// prefix exactly as targetless `gc sling` finds it, to that scope's adoption
// route. The zero value adopts nothing.
type unroutedAdoptionPolicy struct {
	cfg *config.City
	// scopes is keyed by the lower-cased bead prefix.
	scopes map[string]unroutedAdoptionScope
}

// unroutedAdoptionPolicyFor builds the policy from one config snapshot. A nil
// config yields the zero policy.
func unroutedAdoptionPolicyFor(cfg *config.City) unroutedAdoptionPolicy {
	if cfg == nil {
		return unroutedAdoptionPolicy{}
	}
	scopes := map[string]unroutedAdoptionScope{}
	if hq := strings.ToLower(strings.TrimSpace(config.EffectiveHQPrefix(cfg))); hq != "" {
		scopes[hq] = unroutedAdoptionScope{label: "city", route: adoptionRouteFor(cfg, cfg.Workspace.UnroutedWorkTarget)}
	}
	return unroutedAdoptionPolicy{cfg: cfg, scopes: scopes}
}

// adoptionRouteFor is the gc.routed_to value built-in sling writes for target
// (cmd_sling.go cliBeadRouter.Route), or "" for an empty target.
func adoptionRouteFor(cfg *config.City, target string) string {
	target = strings.TrimSpace(target)
	if target == "" {
		return ""
	}
	return agentutil.NormalizePoolRouteTarget(cfg, target)
}

// scopeFor returns the scope owning beadID, by prefix.
func (p unroutedAdoptionPolicy) scopeFor(beadID string) (unroutedAdoptionScope, bool) {
	if len(p.scopes) == 0 {
		return unroutedAdoptionScope{}, false
	}
	prefix := strings.ToLower(strings.TrimSpace(beadPrefix(p.cfg, beadID)))
	if prefix == "" {
		return unroutedAdoptionScope{}, false
	}
	sc, ok := p.scopes[prefix]
	return sc, ok
}

// routeFor returns the adoption route for b, or "" when b is not adoptable
// unrouted work or its owning scope sets no target.
func (p unroutedAdoptionPolicy) routeFor(b beads.Bead) string {
	if !adoptableUnroutedWork(b) {
		return ""
	}
	sc, ok := p.scopeFor(b.ID)
	if !ok {
		return ""
	}
	return sc.route
}

// adoptableUnroutedWork is the single structural predicate for work the
// route-recovery lane may adopt and the unrouted-ready-work doctor check
// reports: open, unassigned work that carries no route of any kind.
func adoptableUnroutedWork(b beads.Bead) bool {
	if b.Status != "open" || strings.TrimSpace(b.Assignee) != "" {
		return false
	}
	return strings.TrimSpace(b.Metadata[beadmeta.RoutedToMetadataKey]) == "" &&
		strings.TrimSpace(b.Metadata[beadmeta.RunTargetMetadataKey]) == ""
}
