package config

import (
	"strings"

	"github.com/gastownhall/gascity/internal/agent"
)

// FindNamedSession returns the configured named session for the provided
// qualified identity, or nil when the identity is not reserved.
func FindNamedSession(cfg *City, identity string) *NamedSession {
	if cfg == nil || identity == "" {
		return nil
	}
	for i := range cfg.NamedSessions {
		if cfg.NamedSessions[i].QualifiedName() == identity {
			return &cfg.NamedSessions[i]
		}
	}
	return nil
}

// FindAgent returns the configured agent template for the provided qualified
// identity, or nil when the template does not exist.
func FindAgent(cfg *City, identity string) *Agent {
	if cfg == nil || identity == "" {
		return nil
	}
	for i := range cfg.Agents {
		if cfg.Agents[i].QualifiedName() == identity {
			return &cfg.Agents[i]
		}
	}
	return nil
}

// EffectiveCityName returns the name used for deterministic runtime naming.
// When workspace.name is omitted, callers may pass a fallback derived from the
// city path; loaded configs also carry ResolvedWorkspaceName for this purpose.
func EffectiveCityName(cfg *City, fallback string) string {
	if cfg != nil {
		if name := strings.TrimSpace(cfg.Workspace.Name); name != "" {
			return name
		}
		if name := strings.TrimSpace(cfg.ResolvedWorkspaceName); name != "" {
			return name
		}
	}
	return strings.TrimSpace(fallback)
}

// EffectiveCityName returns the effective deterministic naming prefix for the
// loaded config. It is empty only when neither workspace.name nor a derived
// city-root fallback is available.
func (c *City) EffectiveCityName() string {
	return EffectiveCityName(c, "")
}

// NamedSessionRuntimeName returns the deterministic runtime session_name for a
// configured named session identity under the current city naming policy.
func NamedSessionRuntimeName(cityName string, workspace Workspace, identity string) string {
	return agent.SessionNameFor(cityName, identity, workspace.SessionTemplate)
}
