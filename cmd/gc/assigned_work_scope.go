package main

import (
	"strings"

	"github.com/gastownhall/gascity/internal/agentutil"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
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
	sessionBeads []beads.Bead,
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
	for _, sb := range sessionBeads {
		if sb.Status == "closed" {
			continue
		}
		template := normalizedSessionTemplate(sb, cfg)
		if template == "" {
			template = strings.TrimSpace(sb.Metadata["template"])
		}
		if template != "" {
			sessionBeadTemplate[sb.ID] = template
		}
		for _, id := range sessionBeadAssigneeIdentities(sb) {
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
// identities because session wake decisions are per concrete session owner.
func filterAssignedWorkBeadsForSessionWake(
	cfg *config.City,
	cityPath string,
	sessionBeads []beads.Bead,
	assignedWorkBeads []beads.Bead,
	assignedWorkStoreRefs []string,
) []beads.Bead {
	if len(assignedWorkBeads) == 0 || len(assignedWorkStoreRefs) == 0 {
		return assignedWorkBeads
	}
	if cfg == nil {
		return assignedWorkBeads
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
	for _, sb := range sessionBeads {
		if sb.Status == "closed" {
			continue
		}
		template := normalizedSessionTemplate(sb, cfg)
		if template == "" {
			template = strings.TrimSpace(sb.Metadata["template"])
		}
		agentCfg := findAgentByTemplate(cfg, template)
		if agentCfg == nil {
			continue
		}
		if agentIsCrossStoreEligible(agentCfg) {
			for _, id := range sessionBeadAssigneeIdentities(sb) {
				crossStore[strings.TrimSpace(id)] = struct{}{}
			}
			crossStore[strings.TrimSpace(template)] = struct{}{}
			continue
		}
		storeRef := assignedWorkStoreRefForAgent(cityPath, cfg, agentCfg)
		for _, id := range sessionBeadAssigneeIdentities(sb) {
			add(id, storeRef)
		}
		add(template, storeRef)
	}

	filtered := make([]beads.Bead, 0, len(assignedWorkBeads))
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
			continue
		}
		if refs := reachableRefsByAssignee[assignee]; refs != nil {
			if _, ok := refs[assignedWorkStoreRefs[i]]; ok {
				filtered = append(filtered, wb)
			}
		}
	}
	return filtered
}
