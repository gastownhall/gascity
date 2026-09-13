package main

import (
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

// skill_visibility.go holds the session-front-door-injected half of `gc skill
// list`: the resolvers that take the typed *session.Store rather than a raw
// store. The command root (cmd_skill.go) opens the city store and constructs
// the front door; these leaves receive it, so this file is store-free
// (frontDoorStoreFreeFiles). Session-bead access reaches the raw session-class
// store the front door wraps via sessFront.Store().Store — the same underlying
// store, so behavior is unchanged.

func listVisibleSkillEntries(cityPath string, cfg *config.City, sessFront *session.Store, agentName, sessionID string) ([]visibilityEntry, error) {
	entries := discoverSkillEntries(cityPath, "city")
	// Legacy implicit-import compatibility packs may still contribute
	// shared skills on upgraded installs. Keep surfacing them here while
	// the compatibility path exists; builtin pack skills arrive through
	// the composed imports and are not part of this listing.
	entries = append(entries, discoverBootstrapSkillEntries()...)
	if strings.TrimSpace(agentName) == "" && strings.TrimSpace(sessionID) == "" {
		entries = append(entries, discoverImportedSkillEntries(sharedSkillCatalogInputs(cfg, currentRigContext(cfg)))...)
		sortVisibilityEntries(entries)
		return entries, nil
	}
	agent, err := resolveVisibilityAgent(cityPath, cfg, sessFront, agentName, sessionID)
	if err != nil {
		return nil, err
	}
	// Every agent sees the entire shared catalog plus its own agent-local
	// skills. No attachment filtering.
	entries = append(entries, discoverImportedSkillEntries(sharedSkillCatalogInputs(cfg, agentRigScopeName(agent, cfg.Rigs)))...)
	entries = append(entries, discoverAgentSkillEntries(agentAssetRoot(cityPath, agent), agent.Name, "agent")...)
	sortVisibilityEntries(entries)
	return entries, nil
}

func resolveVisibilityAgent(cityPath string, cfg *config.City, sessFront *session.Store, agentName, sessionID string) (*config.Agent, error) {
	switch {
	case strings.TrimSpace(agentName) != "":
		resolved, ok := resolveAgentIdentity(cfg, agentName, currentRigContext(cfg))
		if !ok {
			return nil, fmt.Errorf("unknown agent %q", agentName)
		}
		template := resolveAgentTemplate(resolved.QualifiedName(), cfg)
		agent := findAgentByTemplate(cfg, template)
		if agent == nil {
			return nil, fmt.Errorf("unknown agent %q", agentName)
		}
		return agent, nil
	case strings.TrimSpace(sessionID) != "":
		if !sessFront.Backed() {
			return nil, fmt.Errorf("session store unavailable")
		}
		store := sessFront.Store().Store
		id, err := resolveSessionIDAllowClosedWithConfig(cityPath, cfg, store, sessionID)
		if err != nil {
			return nil, err
		}
		info, err := sessFront.Get(id)
		if err != nil {
			// Name the user-supplied identifier, not the resolved bead id.
			return nil, fmt.Errorf("loading session %q: %w", sessionID, err)
		}
		template := normalizedSessionTemplateInfo(info, cfg)
		if template == "" {
			template = strings.TrimSpace(info.AgentName)
		}
		template = resolveAgentTemplate(template, cfg)
		agent := findAgentByTemplate(cfg, template)
		if agent == nil {
			return nil, fmt.Errorf("session %q maps to unknown agent template %q", sessionID, template)
		}
		return agent, nil
	default:
		return nil, nil
	}
}
