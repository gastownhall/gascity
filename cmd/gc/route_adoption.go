package main

import (
	"errors"
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/agentutil"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
	workdirutil "github.com/gastownhall/gascity/internal/workdir"
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
	// adopts nothing.
	route string
	// err is why the scope's unrouted_work_target cannot adopt, or nil.
	// validateUnroutedWorkTargets refuses such a config at start and reload,
	// so only a config read around them (gc doctor before a reload) carries
	// one; the scope then adopts nothing.
	err error
}

// unroutedAdoptionPolicy maps a bead's owning store scope, found by its ID
// prefix exactly as targetless `gc sling` finds it, to that scope's adoption
// route. The zero value adopts nothing.
type unroutedAdoptionPolicy struct {
	cfg *config.City
	// scopes is keyed by the lower-cased bead prefix.
	scopes map[string]unroutedAdoptionScope
}

// unroutedAdoptionPolicyFor builds the policy from one config snapshot of the
// city at cityPath. A nil config yields the zero policy.
func unroutedAdoptionPolicyFor(cfg *config.City, cityPath string) unroutedAdoptionPolicy {
	if cfg == nil {
		return unroutedAdoptionPolicy{}
	}
	scopes := map[string]unroutedAdoptionScope{}
	if hq := strings.ToLower(strings.TrimSpace(config.EffectiveHQPrefix(cfg))); hq != "" {
		route, err := resolveUnroutedWorkTarget(cfg, cityPath, cityWorkflowStoreRef(cfg, cityPath), cfg.Workspace.UnroutedWorkTarget)
		scopes[hq] = unroutedAdoptionScope{label: "city", route: route, err: err}
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
		route, err := resolveUnroutedWorkTarget(cfg, cityPath, "rig:"+rig.Name, rig.UnroutedWorkTarget)
		scopes[prefix] = unroutedAdoptionScope{label: "rig " + rig.Name, route: route, err: err}
	}
	return unroutedAdoptionPolicy{cfg: cfg, scopes: scopes}
}

// cityWorkflowStoreRef is the city (HQ) store's workflow store ref,
// "city:<name>", as agentutil.AgentReachesWorkflowStore reads it.
func cityWorkflowStoreRef(cfg *config.City, cityPath string) string {
	return "city:" + workdirutil.CityName(cityPath, cfg)
}

// resolveUnroutedWorkTarget resolves one scope's unrouted_work_target to the
// gc.routed_to value adoption stamps, which is the value `gc sling <target>
// <id>` writes: resolveAgentIdentity resolves target to an agent exactly as
// sling resolves an explicit target (with no rig context, since the controller
// has no working directory), and built-in sling writes that agent's routing
// identity with any pool slot collapsed (sling_core.go RouteRequest.Target,
// cmd_sling.go cliBeadRouter.Route). Resolving the agent first is what turns
// a unique bare name, a binding-less V2 name or a bare pool-instance name into
// the route the agent's pool claims.
//
// storeRef names the scope's bead store ("city:<name>" or "rig:<name>"). The
// agent must read it, as sling's cross-store guard requires: a pool discovers
// work only in the stores it reads, so a route to a pool that cannot read the
// scope's store parks the work for good.
//
// It returns "" and no error for an empty target, and an error naming the
// target when it names no configured agent, names one with a custom
// sling_query (which routes some way one gc.routed_to write cannot
// reproduce), or names one that cannot read storeRef.
func resolveUnroutedWorkTarget(cfg *config.City, cityPath, storeRef, target string) (string, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return "", nil
	}
	agent, ok := resolveAgentIdentity(cfg, target, "")
	switch {
	case !ok:
		return "", fmt.Errorf("unrouted_work_target %q names no configured agent", target)
	case isCustomSlingQuery(agent):
		return "", fmt.Errorf("unrouted_work_target %q names agent %q, which has a custom sling_query; adoption needs built-in sling routing", target, agent.QualifiedName())
	case !agentutil.AgentReachesWorkflowStore(storeRef, &agent, cityPath, cfg):
		reads := agentutil.AgentReachableStoreLabel(&agent, cityPath, workdirutil.CityName(cityPath, cfg), cfg)
		return "", fmt.Errorf("unrouted_work_target %q names agent %q, whose pool reads %s, not this scope's %s; name an agent that reads it (one in this scope, or one with scope = \"city\")", target, agent.QualifiedName(), reads, storeRef)
	}
	return agentutil.NormalizePoolRouteTarget(cfg, agentutil.RoutedToIdentity(&agent)), nil
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
// unrouted work or its owning scope adopts nothing.
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
//
// It must also be work-class (coordclass.ClassWork). On a city that relocates
// no class, gc's own coordination beads live in the work store looking like
// work: a queued-nudge shadow is an open, unassigned chore, and an extmsg
// binding or delivery an open, unassigned task. The serving rules never had to
// exclude them, because nothing routes them; adopting one would hand a live
// nudge or conversation binding to the pool to claim and close.
func adoptableUnroutedWork(b beads.Bead) bool {
	if b.Status != "open" || strings.TrimSpace(b.Assignee) != "" || b.Ephemeral {
		return false
	}
	for _, key := range adoptionRefusingMetadataKeys {
		if strings.TrimSpace(b.Metadata[key]) != "" {
			return false
		}
	}
	if beads.IsReadyExcludedBead(b) || !demandRowServable(b) || coordclass.Classify(b).IsInfrastructure() {
		return false
	}
	return !isRouteRecoveryQuarantined(b)
}

// validateUnroutedWorkTargets checks every configured unrouted_work_target
// with resolveUnroutedWorkTarget, the resolver adoption itself uses, against
// its scope's store: the city store for the workspace target, the rig's own
// store for a rig's. Each error names the scope, unrouted_work_target and the
// value.
func validateUnroutedWorkTargets(cfg *config.City, cityPath string) error {
	if cfg == nil {
		return nil
	}
	var errs []error
	if _, err := resolveUnroutedWorkTarget(cfg, cityPath, cityWorkflowStoreRef(cfg, cityPath), cfg.Workspace.UnroutedWorkTarget); err != nil {
		errs = append(errs, fmt.Errorf("workspace: %w", err))
	}
	for _, rig := range cfg.Rigs {
		if _, err := resolveUnroutedWorkTarget(cfg, cityPath, "rig:"+rig.Name, rig.UnroutedWorkTarget); err != nil {
			errs = append(errs, fmt.Errorf("rig %q: %w", rig.Name, err))
		}
	}
	return errors.Join(errs...)
}
