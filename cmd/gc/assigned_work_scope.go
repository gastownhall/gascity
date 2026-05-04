package main

import (
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

func assignedWorkStoreRefForAgent(cityPath string, cfg *config.City, agentCfg *config.Agent) string {
	if cfg == nil || agentCfg == nil {
		return ""
	}
	return configuredRigName(cityPath, agentCfg, cfg.Rigs)
}

func assignedWorkIndexReachableFromAgent(cityPath string, cfg *config.City, agentCfg *config.Agent, storeRefs []string, index int) bool {
	if len(storeRefs) == 0 {
		return true
	}
	if index < 0 || index >= len(storeRefs) {
		return false
	}
	return storeRefs[index] == assignedWorkStoreRefForAgent(cityPath, cfg, agentCfg)
}

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
		assigneeToSessionBeadID[sb.ID] = sb.ID
		if sessionName := strings.TrimSpace(sb.Metadata["session_name"]); sessionName != "" {
			assigneeToSessionBeadID[sessionName] = sb.ID
		}
		if identity := strings.TrimSpace(sb.Metadata["configured_named_identity"]); identity != "" {
			assigneeToSessionBeadID[identity] = sb.ID
		}
	}
	filtered := make([]beads.Bead, 0, len(assignedWorkBeads))
	for i, wb := range assignedWorkBeads {
		template := strings.TrimSpace(wb.Metadata["gc.routed_to"])
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
		storeRef := assignedWorkStoreRefForAgent(cityPath, cfg, agentCfg)
		add(sb.ID, storeRef)
		add(sb.Metadata["session_name"], storeRef)
		add(sb.Metadata["configured_named_identity"], storeRef)
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
		if refs := reachableRefsByAssignee[assignee]; refs != nil {
			if _, ok := refs[assignedWorkStoreRefs[i]]; ok {
				filtered = append(filtered, wb)
			}
		}
	}
	return filtered
}
