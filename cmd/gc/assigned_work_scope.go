package main

import (
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/agentutil"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

func assignedWorkStoreRefForAgent(cityPath string, cfg *config.City, agentCfg *config.Agent) string {
	if cfg == nil || agentCfg == nil {
		return ""
	}
	return configuredRigName(cityPath, agentCfg, cfg.Rigs)
}

// agentIsCrossStoreEligible reports whether an agent may discover and serve work
// in ANY store, not just its configured rig. City-scoped agents are cross-store
// eligible: a city-wide singleton legitimately serves per-rig routed work
// (vp-kvp — "scope determines discovery breadth"). Rig-scoped agents stay
// single-store, so their reachability and all existing behavior are unchanged.
func agentIsCrossStoreEligible(agentCfg *config.Agent) bool {
	return agentutil.AgentIsCrossStoreEligible(agentCfg)
}

// sessionAgentConfig resolves the agent config backing a session bead from its
// template metadata, or nil when neither the template nor a backing agent can be
// resolved.
func sessionAgentConfig(cfg *config.City, session beads.Bead) *config.Agent {
	if cfg == nil {
		return nil
	}
	template := normalizedSessionTemplate(session, cfg)
	if template == "" {
		template = strings.TrimSpace(session.Metadata["template"])
	}
	if template == "" {
		template = strings.TrimSpace(session.Metadata["common_name"])
	}
	if template == "" {
		return nil
	}
	return findAgentByTemplate(cfg, template)
}

// sessionAgentConfigInfo is the session.Info form of sessionAgentConfig: it
// resolves the backing agent from the typed template/common_name Info fields
// instead of cracking the raw bead, staying byte-identical to the raw form
// (TestSessionClassifierInfoEquivalence pins it).
func sessionAgentConfigInfo(cfg *config.City, info sessionpkg.Info) *config.Agent {
	if cfg == nil {
		return nil
	}
	template := normalizedSessionTemplateInfo(info, cfg)
	if template == "" {
		template = strings.TrimSpace(info.Template)
	}
	if template == "" {
		template = strings.TrimSpace(info.CommonName)
	}
	if template == "" {
		return nil
	}
	return findAgentByTemplate(cfg, template)
}

// openSessionReachableStoreRefInfo returns the store-refs under which an open
// session bead owns assigned work, for makeOpenSessionStoreRefIndex. The SESSION
// side reads typed session.Info (WI-5 W3 per-parameter split); the refs come
// from the residency resolver.
//
// A cross-store eligible (city-scoped) session federates across every store
// (vp-kvp), so it is indexed under crossStoreOpenSessionStoreRef — a wildcard
// openSessionOwnsWork matches against any work store-ref. This mirrors the
// cross-store ownership the demand and session-wake filters already grant
// (filterAssignedWorkBeadsForSessionWake); without it the release path strands a
// live city-scoped holder's rig-routed work and a backup worker is minted on the
// same bead (#3453). A session whose template/agent cannot be resolved falls back
// to unresolvedOpenSessionStoreRef (also a wildcard), preserving the legacy
// keep-on-match fail-safe.
//
// Every other session is its configured rig's store-ref PLUS the refs a claim
// can be recorded under whatever the holder's rig scope (assignedWorkClaimRefs:
// the leading work arm and every relocated class binding). That addition is the
// counterweight to this slice widening what the release path can SEE: claim-time
// class routing writes a rig-scoped agent's claim into the binding, the
// orphan-release scan now reads that leg, and an index that still answered
// "this holder owns only its rig" would let the scan reap a LIVE worker's claim
// — claim loss, not a missed wake. It is the same widening the wake filter
// already applies to the same identities (ga-whzrt), which is what makes the two
// mechanisms answer one question instead of two.
// claimRefs is the city-level answer from assignedWorkClaimRefs, resolved once
// by the caller because it is a property of the CITY and this runs per session.
func openSessionReachableStoreRefInfo(cityPath string, cfg *config.City, claimRefs []string, info sessionpkg.Info) []string {
	agentCfg := sessionAgentConfigInfo(cfg, info)
	if agentCfg == nil {
		return []string{unresolvedOpenSessionStoreRef}
	}
	if agentIsCrossStoreEligible(agentCfg) {
		return []string{crossStoreOpenSessionStoreRef}
	}
	return append([]string{assignedWorkStoreRefForAgent(cityPath, cfg, agentCfg)}, claimRefs...)
}

func assignedWorkIndexReachableFromAgent(cityPath string, cfg *config.City, agentCfg *config.Agent, storeRefs []string, index int) bool {
	if len(storeRefs) == 0 {
		return true
	}
	if index < 0 || index >= len(storeRefs) {
		return false
	}
	// City-scoped agents federate across all stores (vp-kvp): a city-wide
	// singleton's work may live in any rig store, so gating it to its own
	// configured rig is the cross-store dead-drop this fixes.
	if agentIsCrossStoreEligible(agentCfg) {
		return true
	}
	return storeRefs[index] == assignedWorkStoreRefForAgent(cityPath, cfg, agentCfg)
}

// assignedWorkIndexReachableFromAgentOnClaimRefs extends the rig-equality test
// with the refs a claim can be RECORDED under whatever the holder's rig scope:
// the leading work arm plus every relocated class binding (assignedWorkClaimRefs).
//
// This is the demand-side twin of the widening filterAssignedWorkBeadsForSessionWake
// already applies (ga-whzrt). The two filters answer one question from opposite
// ends — "is this holder still working?" and "does this work still need a
// worker?" — so a ref the wake side accepts and the demand side rejects is a city
// that drains a slot and then refuses to refill it.
//
// Relocating a coordination class is what made that reachable. The equality test
// is against the agent's configured RIG, so once graph-resident work carries a
// "class:*" ref no rig name can equal, in-progress demand for a rig-scoped pool
// agent became structurally invisible. It stayed hidden because scale_check
// supplies demand independently while the work is still ready; only once the
// work is CLAIMED is the resume tier the sole remaining source, which is why the
// symptom is a session that dies holding a claim and is never replaced.
//
// claimRefs must come from assignedWorkRelocatedClaimRefs, which answers empty
// for a city that relocates nothing: the widening covers relocated class
// bindings only, never another rig's ref, so rig isolation and every
// single-store city are exactly what they were.
func assignedWorkIndexReachableFromAgentOnClaimRefs(
	cityPath string,
	cfg *config.City,
	agentCfg *config.Agent,
	storeRefs []string,
	index int,
	claimRefs []string,
) bool {
	if assignedWorkIndexReachableFromAgent(cityPath, cfg, agentCfg, storeRefs, index) {
		return true
	}
	if index < 0 || index >= len(storeRefs) {
		return false
	}
	for _, ref := range claimRefs {
		if storeRefs[index] == ref {
			return true
		}
	}
	return false
}

// filterAssignedWorkBeadsForPoolDemand resolves work through the routed
// backing template because pool scale decisions are per agent template.
// leading is the store this arm was handed; it resolves the claim refs, and is
// a property of the CITY so it is read once rather than per bead.
func filterAssignedWorkBeadsForPoolDemand(
	cfg *config.City,
	cityPath string,
	leading beads.Store,
	sessionInfos []sessionpkg.Info,
	assignedWorkBeads []beads.Bead,
	assignedWorkStoreRefs []string,
) []beads.Bead {
	if len(assignedWorkBeads) == 0 || len(assignedWorkStoreRefs) == 0 {
		return assignedWorkBeads
	}
	if cfg == nil {
		return assignedWorkBeads
	}
	claimRefs := assignedWorkRelocatedClaimRefs(cityPath, cfg, leading)
	assigneeToSessionBeadID := make(map[string]string)
	sessionBeadTemplate := make(map[string]string)
	for _, sb := range sessionInfos {
		if sb.Closed {
			continue
		}
		template := normalizedSessionTemplateInfo(sb, cfg)
		if template == "" {
			template = strings.TrimSpace(sb.Template)
		}
		if template != "" {
			sessionBeadTemplate[sb.ID] = template
		}
		for _, id := range sessionBeadAssigneeIdentitiesInfo(sb) {
			assigneeToSessionBeadID[id] = sb.ID
		}
	}
	now := time.Now().UTC()
	filtered := make([]beads.Bead, 0, len(assignedWorkBeads))
	for i, wb := range assignedWorkBeads {
		// A deferred bead is deliberately parked (future defer_until) and is
		// invisible to bd ready, so scale_check reports zero demand for it.
		// But this pool-demand pass draws from a raw List(status=open) that
		// still returns deferred beads. A deferred bead that retains a stale
		// gc.routed_to would otherwise count as poolDesired=1 with no matching
		// ready work, driving the reconciler to spawn an ephemeral session that
		// immediately orphan-drains — a spawn/drain loop that only ends when
		// the bead's defer elapses or the route is stripped by hand. Excluding
		// deferred beads here mirrors bd ready's server-side filter so gc's
		// internal demand agrees with the shim.
		if beads.IsDeferred(wb, now) {
			continue
		}
		template := routedToOrLegacyWorkflowTarget(wb)
		if template == "" {
			if sessionBeadID := assigneeToSessionBeadID[strings.TrimSpace(wb.Assignee)]; sessionBeadID != "" {
				template = sessionBeadTemplate[sessionBeadID]
				if template == "" && len(cfg.Agents) == 1 {
					template = cfg.Agents[0].QualifiedName()
				}
			}
		}
		if template == "" {
			continue
		}
		template = agentutil.NormalizePoolRouteTarget(cfg, template)
		agentCfg := findAgentByTemplate(cfg, template)
		if agentCfg == nil {
			continue
		}
		if assignedWorkIndexReachableFromAgentOnClaimRefs(cityPath, cfg, agentCfg, assignedWorkStoreRefs, i, claimRefs) {
			filtered = append(filtered, wb)
		}
	}
	return filtered
}

// filterAssignedWorkBeadsForSessionWake resolves work through assignment
// identities because session wake decisions are per concrete session owner. It
// returns the filtered beads plus their store refs, index-aligned, so callers
// can resolve store-scoped wake-demand readiness (storeScopedBeadKey) for the
// surviving beads without re-deriving each bead's originating store.
//
// The reconcile paths take the store-aware form below, which also carries the
// legs the rows were read through; this two-value form is for callers that only
// read the surviving rows.
func filterAssignedWorkBeadsForSessionWake(
	cfg *config.City,
	cityPath string,
	leading beads.Store,
	sessionInfos []sessionpkg.Info,
	assignedWorkBeads []beads.Bead,
	assignedWorkStoreRefs []string,
) ([]beads.Bead, []string) {
	kept, keptRefs, _ := filterAssignedWorkBeadsForSessionWakeWithStores(cfg, cityPath, leading, sessionInfos, assignedWorkBeads, assignedWorkStoreRefs, nil)
	return kept, keptRefs
}

// filterAssignedWorkBeadsForSessionWakeWithStores is the store-aware form: it
// projects the index-aligned snapshot stores through the same filter, so a
// caller that must WRITE to a surviving row can do it through the leg the census
// read the row through rather than re-deriving an owner from gc.routed_to (which
// names a work ledger a binding-resident row does not live in — ga-b0o6a).
//
// assignedWorkStores must be as long as assignedWorkBeads; a slice of any other
// length is dropped rather than partially applied, and the returned stores are
// then nil.
func filterAssignedWorkBeadsForSessionWakeWithStores(
	cfg *config.City,
	cityPath string,
	leading beads.Store,
	sessionInfos []sessionpkg.Info,
	assignedWorkBeads []beads.Bead,
	assignedWorkStoreRefs []string,
	assignedWorkStores []beads.Store,
) ([]beads.Bead, []string, []beads.Store) {
	if len(assignedWorkStores) != len(assignedWorkBeads) {
		assignedWorkStores = nil
	}
	if len(assignedWorkBeads) == 0 || len(assignedWorkStoreRefs) == 0 {
		return assignedWorkBeads, assignedWorkStoreRefs, assignedWorkStores
	}
	if cfg == nil {
		return assignedWorkBeads, assignedWorkStoreRefs, assignedWorkStores
	}
	claimRefs := assignedWorkClaimRefs(cityPath, cfg, leading)
	reachableRefsByAssignee := make(map[string]map[string]struct{})
	// crossStore identities belong to city-scoped (cross-store-eligible) agents
	// and are reachable from ANY store (vp-kvp). They bypass the per-ref match.
	crossStore := make(map[string]struct{})
	add := func(identifier, storeRef string) {
		identifier = strings.TrimSpace(identifier)
		if identifier == "" {
			return
		}
		refs := reachableRefsByAssignee[identifier]
		if refs == nil {
			refs = make(map[string]struct{})
			reachableRefsByAssignee[identifier] = refs
		}
		refs[storeRef] = struct{}{}
	}

	for i := range cfg.NamedSessions {
		identity := cfg.NamedSessions[i].QualifiedName()
		spec, ok := findNamedSessionSpec(cfg, "", identity)
		if !ok {
			continue
		}
		if agentIsCrossStoreEligible(spec.Agent) {
			crossStore[strings.TrimSpace(identity)] = struct{}{}
			continue
		}
		add(identity, assignedWorkStoreRefForAgent(cityPath, cfg, spec.Agent))
	}
	for _, sb := range sessionInfos {
		if sb.Closed {
			continue
		}
		template := normalizedSessionTemplateInfo(sb, cfg)
		if template == "" {
			template = strings.TrimSpace(sb.Template)
		}
		agentCfg := findAgentByTemplate(cfg, template)
		if agentCfg == nil {
			continue
		}
		if agentIsCrossStoreEligible(agentCfg) {
			for _, id := range sessionBeadAssigneeIdentitiesInfo(sb) {
				crossStore[strings.TrimSpace(id)] = struct{}{}
			}
			crossStore[strings.TrimSpace(template)] = struct{}{}
			continue
		}
		storeRef := assignedWorkStoreRefForAgent(cityPath, cfg, agentCfg)
		for _, id := range sessionBeadAssigneeIdentitiesInfo(sb) {
			add(id, storeRef)
			// A claim this session HOLDS can also live on the leading work arm
			// or in a relocated class binding, whatever the owning agent's rig
			// scope: on a split city the binding is where claim-time class
			// routing writes the assignee (claim_class_route.go), and even on a
			// single-store city the agent's own hook fan-out reaches the city
			// store (appendCityHookStore) — so a bead it can claim was being
			// dropped here for a store it demonstrably reads. Dropping it is
			// what left a claim-holder with AwakeDecision{Reason:""} and drained
			// it down the no-wake-reason arm while it owned in-progress work
			// (ga-whzrt).
			//
			// The refs come from the resolver rather than from a constant, so a
			// city mid-rollout is readable by ONE filter: the census records a
			// binding-resident claim under "" while the reconciler's leading arm
			// is the binding, and under the binding's own "class:*" ref once it
			// is a distinct census leg. Both are in the set.
			//
			// This widens COLLECTION only. The match is still this session's own
			// exact assignee identity, so no bead belonging to anyone else
			// becomes visible; the template key below deliberately does NOT get
			// the extra arms, because a template match is a scope statement
			// rather than an ownership one.
			for _, ref := range claimRefs {
				add(id, ref)
			}
		}
		add(template, storeRef)
	}

	filtered := make([]beads.Bead, 0, len(assignedWorkBeads))
	filteredRefs := make([]string, 0, len(assignedWorkBeads))
	var filteredStores []beads.Store
	if assignedWorkStores != nil {
		filteredStores = make([]beads.Store, 0, len(assignedWorkBeads))
	}
	keep := func(i int, wb beads.Bead) {
		filtered = append(filtered, wb)
		filteredRefs = append(filteredRefs, assignedWorkStoreRefs[i])
		if filteredStores != nil {
			filteredStores = append(filteredStores, assignedWorkStores[i])
		}
	}
	for i, wb := range assignedWorkBeads {
		if i >= len(assignedWorkStoreRefs) {
			continue
		}
		assignee := strings.TrimSpace(wb.Assignee)
		if assignee == "" {
			continue
		}
		if _, ok := crossStore[assignee]; ok {
			// City-scoped assignee: reachable from any store (vp-kvp).
			keep(i, wb)
			continue
		}
		if refs := reachableRefsByAssignee[assignee]; refs != nil {
			if _, ok := refs[assignedWorkStoreRefs[i]]; ok {
				keep(i, wb)
			}
		}
	}
	return filtered, filteredRefs, filteredStores
}

// readyAssignedFlagsForBeads resolves the store-scoped wake-demand readiness of
// each assigned-work bead into a slice index-aligned with beadList. Readiness is
// keyed by (store ref, bead ID) because AssignedWorkBeads can carry the same
// bead ID from independent city and rig stores; a plain ID lookup would let a
// ready bead in one store mark a blocked open bead with the same ID in another
// store as ready and reintroduce the awake-demand hang. storeRefs must be the
// refs returned alongside beadList by filterAssignedWorkBeadsForSessionWake. A
// bead whose store ref is unavailable resolves to not-ready, matching the
// nil-map default the awake bridge applied before readiness was store-scoped.
func readyAssignedFlagsForBeads(readyAssigned map[storeScopedBeadKey]bool, beadList []beads.Bead, storeRefs []string) []bool {
	if len(beadList) == 0 {
		return nil
	}
	flags := make([]bool, len(beadList))
	for i := range beadList {
		if i >= len(storeRefs) {
			continue
		}
		flags[i] = readyAssigned[storeScopedBeadKey{StoreRef: storeRefs[i], ID: beadList[i].ID}]
	}
	return flags
}
