package config

import (
	"strings"

	"github.com/gastownhall/gascity/internal/agent"
)

// FindNamedSession returns the configured named session for the provided
// identity, or nil when the identity is not reserved. Matches by fully
// qualified name first. When no exact match is found and the identity has
// no binding prefix, falls back to matching the bare template/name against
// V2 bindings so callers can say "mayor" instead of "gastown.mayor".
// Returns nil when multiple bindings would match — the caller must
// disambiguate with the fully qualified form.
func FindNamedSession(cfg *City, identity string) *NamedSession {
	if cfg == nil || identity == "" {
		return nil
	}
	for i := range cfg.NamedSessions {
		if cfg.NamedSessions[i].QualifiedName() == identity {
			return &cfg.NamedSessions[i]
		}
	}
	// V2 bare-name fallback: only when identity has no binding prefix.
	if strings.Contains(identity, ".") {
		return nil
	}
	var match *NamedSession
	for i := range cfg.NamedSessions {
		ns := &cfg.NamedSessions[i]
		if ns.BindingName == "" {
			continue
		}
		if ns.IdentityName() == ns.BindingName+"."+identity {
			if match != nil {
				// Ambiguous — user must spell out the qualified name.
				return nil
			}
			match = ns
		}
	}
	return match
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
