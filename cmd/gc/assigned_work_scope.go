package main

import (
	"strings"

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

// openSessionReachableStoreRef returns the store-ref under which an open session
// bead owns assigned work, for makeOpenSessionStoreRefIndex. A cross-store
// eligible (city-scoped) session federates across every store (vp-kvp), so it is
// indexed under crossStoreOpenSessionStoreRef — a wildcard openSessionOwnsWork
// matches against any work store-ref. This mirrors the cross-store ownership the
// demand and session-wake filters already grant (filterAssignedWorkBeadsForSessionWake);
// without it the release path strands a live city-scoped holder's rig-routed
// work and a backup worker is minted on the same bead (#3453). A session whose
// template/agent cannot be resolved falls back to unresolvedOpenSessionStoreRef
// (also a wildcard), preserving the legacy keep-on-match fail-safe; every other
// session stays scoped to its configured rig's store-ref.
func openSessionReachableStoreRef(cityPath string, cfg *config.City, session beads.Bead) string {
	agentCfg := sessionAgentConfig(cfg, session)
	if agentCfg == nil {
		return unresolvedOpenSessionStoreRef
	}
	if agentIsCrossStoreEligible(agentCfg) {
		return crossStoreOpenSessionStoreRef
	}
	return assignedWorkStoreRefForAgent(cityPath, cfg, agentCfg)
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

// filterAssignedWorkBeadsForPoolDemand resolves work through the routed
// backing template because pool scale decisions are per agent template.
func filterAssignedWorkBeadsForPoolDemand(
	cfg *config.City,
	cityPath string,
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
	filtered := make([]beads.Bead, 0, len(assignedWorkBeads))
	for i, wb := range assignedWorkBeads {
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
		agentCfg := findAgentByTemplate(cfg, template)
		if agentCfg == nil {
			continue
		}
		if assignedWorkIndexReachableFromAgent(cityPath, cfg, agentCfg, assignedWorkStoreRefs, i) {
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
func filterAssignedWorkBeadsForSessionWake(
	cfg *config.City,
	cityPath string,
	sessionInfos []sessionpkg.Info,
	assignedWorkBeads []beads.Bead,
	assignedWorkStoreRefs []string,
) ([]beads.Bead, []string) {
	if len(assignedWorkBeads) == 0 || len(assignedWorkStoreRefs) == 0 {
		return assignedWorkBeads, assignedWorkStoreRefs
	}
	if cfg == nil {
		return assignedWorkBeads, assignedWorkStoreRefs
	}
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
		}
		add(template, storeRef)
	}

	filtered := make([]beads.Bead, 0, len(assignedWorkBeads))
	filteredRefs := make([]string, 0, len(assignedWorkBeads))
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
			filtered = append(filtered, wb)
			filteredRefs = append(filteredRefs, assignedWorkStoreRefs[i])
			continue
		}
		if refs := reachableRefsByAssignee[assignee]; refs != nil {
			if _, ok := refs[assignedWorkStoreRefs[i]]; ok {
				filtered = append(filtered, wb)
				filteredRefs = append(filteredRefs, assignedWorkStoreRefs[i])
			}
		}
	}
	return filtered, filteredRefs
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
