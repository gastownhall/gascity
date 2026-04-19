package api

import (
	"strings"

	"github.com/gastownhall/gascity/internal/config"
)

// resolveSessionTemplateAgent resolves only configured templates.
//
// The API intentionally has no ambient rig-context shortcut. Bare names only
// resolve for city-scoped templates; rig-scoped templates require fully
// qualified identities (for example "corp/maya"). Session creation may layer
// its own compatibility fallback above this stricter resolver.
func resolveSessionTemplateAgent(cfg *config.City, input string) (config.Agent, bool) {
	if a, ok := findAgentByQualifiedTemplate(cfg, input); ok {
		return a, true
	}
	if strings.Contains(input, "/") {
		return config.Agent{}, false
	}

	var matches []config.Agent
	for _, a := range cfg.Agents {
		if a.Dir == "" && a.Name == input {
			matches = append(matches, a)
		}
	}
	if len(matches) == 1 {
		return matches[0], true
	}
	return config.Agent{}, false
}

func findUniqueAgentTemplateByBareName(cfg *config.City, input string) (config.Agent, bool) {
	if strings.Contains(input, "/") {
		return config.Agent{}, false
	}
	var matches []config.Agent
	for _, a := range cfg.Agents {
		if a.Name == input {
			matches = append(matches, a)
		}
	}
	if len(matches) == 1 {
		return matches[0], true
	}
	return config.Agent{}, false
}

func findAgentByQualifiedTemplate(cfg *config.City, identity string) (config.Agent, bool) {
	for _, a := range cfg.Agents {
		if config.AgentMatchesIdentity(&a, identity) {
			return a, true
		}
	}
	return config.Agent{}, false
}
