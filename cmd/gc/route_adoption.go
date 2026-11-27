package main

import (
	"errors"
	"fmt"
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
	// No inheritance: a rig adopts only through its own unrouted_work_target.
	for _, rig := range cfg.Rigs {
		prefix := strings.ToLower(strings.TrimSpace(rig.EffectivePrefix()))
		if prefix == "" {
			continue
		}
		if _, taken := scopes[prefix]; taken {
			// The HQ prefix wins, exactly as targetless sling resolves it.
			continue
		}
		scopes[prefix] = unroutedAdoptionScope{label: "rig " + rig.Name, route: adoptionRouteFor(cfg, rig.UnroutedWorkTarget)}
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

// adoptionRefusingMetadataKeys mark a bead as workflow or control topology,
// as already carrying a deferred or execution route, or as claimed by gc at
// some point. Adoption never routes such a bead: its route, if any, is owned
// by the machinery that stamped the key.
var adoptionRefusingMetadataKeys = []string{
	beadmeta.KindMetadataKey,
	beadmeta.RoutedToMetadataKey,
	beadmeta.RunTargetMetadataKey,
	beadmeta.ExecutionRoutedToMetadataKey,
	beadmeta.DeferredRoutedToMetadataKey,
	beadmeta.DeferredExecutionRoutedToMetadataKey,
	beadmeta.InstantiatingMetadataKey,
	beadmeta.ClaimedAtMetadataKey,
}

// adoptableUnroutedWork is the single structural predicate for work the
// route-recovery lane may adopt and the unrouted-ready-work doctor check
// reports: open, unassigned, non-ephemeral work that carries no route of any
// kind, was never claimed, is not quarantined, and that the Tier 3 serving
// rules (demandRowServable, ready exclusions) would hand to a worker.
func adoptableUnroutedWork(b beads.Bead) bool {
	if b.Status != "open" || strings.TrimSpace(b.Assignee) != "" || b.Ephemeral {
		return false
	}
	for _, key := range adoptionRefusingMetadataKeys {
		if strings.TrimSpace(b.Metadata[key]) != "" {
			return false
		}
	}
	if beads.IsReadyExcludedBead(b) || !demandRowServable(b) {
		return false
	}
	return !isRouteRecoveryQuarantined(b)
}

// validateUnroutedWorkTargets checks every configured unrouted_work_target:
// it must resolve to a configured agent through resolveAgentIdentity, the
// resolver gc sling uses, and that agent must use built-in routing, since a custom sling_query
// routes some other way one gc.routed_to write cannot reproduce. Each error
// names the scope, unrouted_work_target and the value.
func validateUnroutedWorkTargets(cfg *config.City) error {
	if cfg == nil {
		return nil
	}
	var errs []error
	check := func(scope, target string) {
		target = strings.TrimSpace(target)
		if target == "" {
			return
		}
		agent, ok := resolveAgentIdentity(cfg, target, "")
		switch {
		case !ok:
			errs = append(errs, fmt.Errorf("%s: unrouted_work_target %q names no configured agent", scope, target))
		case isCustomSlingQuery(agent):
			errs = append(errs, fmt.Errorf("%s: unrouted_work_target %q names agent %q, which has a custom sling_query; adoption needs built-in sling routing", scope, target, agent.QualifiedName()))
		}
	}
	check("workspace", cfg.Workspace.UnroutedWorkTarget)
	for _, rig := range cfg.Rigs {
		check(fmt.Sprintf("rig %q", rig.Name), rig.UnroutedWorkTarget)
	}
	return errors.Join(errs...)
}
