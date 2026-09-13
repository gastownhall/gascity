package main

import (
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/materialize"
)

func sharedSkillCatalogInputs(cfg *config.City, rigName string) []config.DiscoveredSkillCatalog {
	if cfg == nil {
		return nil
	}
	var catalogs []config.DiscoveredSkillCatalog
	if rigName != "" && cfg.RigPackSkills != nil {
		catalogs = append(catalogs, cfg.RigPackSkills[rigName]...)
	}
	catalogs = append(catalogs, cfg.PackSkills...)
	return catalogs
}

func loadSharedSkillCatalog(cfg *config.City, rigName string) (materialize.CityCatalog, error) {
	if cfg == nil {
		return materialize.CityCatalog{}, nil
	}
	return materialize.LoadCityCatalog(cfg.PackSkillsDir, sharedSkillCatalogInputs(cfg, rigName)...)
}

// agentRigScopeName returns the configured rig name that should
// contribute rig-local shared skills for this agent. Only actual
// rig-scoped agents attached to a declared rig get a non-empty name.
// City-scoped agents, and inline agents whose Dir is just a working
// directory hint, must not pull in rig catalogs solely because
// agent.Dir happens to match a rig name.
func agentRigScopeName(agent *config.Agent, rigs []config.Rig) string {
	if agent == nil {
		return ""
	}
	if strings.TrimSpace(agent.Scope) == "city" {
		return ""
	}
	dir := strings.TrimSpace(agent.Dir)
	if dir == "" {
		return ""
	}
	for _, rig := range rigs {
		if rig.Name == dir {
			return rig.Name
		}
	}
	return ""
}
