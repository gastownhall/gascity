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
	candidates := NamedSessionBareCandidates(cfg, identity)
	if len(candidates) != 1 {
		// None, or ambiguous: the user must spell out the qualified name.
		return nil
	}
	return candidates[0]
}

// NamedSessionBareCandidates returns, in config order, the bound named
// sessions a dot-free identity names by FindNamedSession's bare-name rule:
// the entries with a binding whose IdentityName is "binding.identity",
// whatever their Dir. It returns nil for an identity carrying a binding
// prefix. More than one candidate means the bare identity is ambiguous.
func NamedSessionBareCandidates(cfg *City, identity string) []*NamedSession {
	if cfg == nil || identity == "" || strings.Contains(identity, ".") {
		return nil
	}
	var out []*NamedSession
	for i := range cfg.NamedSessions {
		ns := &cfg.NamedSessions[i]
		if ns.BindingName == "" {
			continue
		}
		if ns.IdentityName() == ns.BindingName+"."+identity {
			out = append(out, ns)
		}
	}
	return out
}

// NamedSessionBareIdentity returns the bare identity that FindNamedSession
// resolves to ns: its name without the binding prefix, when ns has a
// binding and that bare name denotes ns alone. It returns "" when ns has no
// binding, when a top-level entry claims the bare name exactly, or when
// another binding shares it.
func NamedSessionBareIdentity(cfg *City, ns *NamedSession) string {
	if ns == nil || ns.BindingName == "" {
		return ""
	}
	leaf := strings.TrimPrefix(ns.IdentityName(), ns.BindingName+".")
	if FindNamedSession(cfg, leaf) != ns {
		return ""
	}
	return leaf
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
	dir, name := ParseQualifiedName(identity)
	if dir == "" || name == "" {
		return nil
	}
	if !hasRigNamed(cfg, dir) {
		return nil
	}
	var match *Agent
	for i := range cfg.Agents {
		a := &cfg.Agents[i]
		if a.Name != name || a.Dir != "" || a.Scope != "rig" {
			continue
		}
		if match != nil {
			return nil
		}
		match = a
	}
	if match == nil {
		return nil
	}
	synth := *match
	synth.Dir = dir
	return &synth
}

func hasRigNamed(cfg *City, name string) bool {
	for _, rig := range cfg.Rigs {
		if rig.Name == name {
			return true
		}
	}
	return false
}

// EffectiveCityName returns the name used for deterministic runtime naming.
// Loaded configs should populate ResolvedWorkspaceName with the effective
// site-bound/declared/basename result; raw parsed configs may still rely on
// workspace.name or the provided fallback.
func EffectiveCityName(cfg *City, fallback string) string {
	if cfg != nil {
		if name := strings.TrimSpace(cfg.ResolvedWorkspaceName); name != "" {
			return name
		}
		if name := strings.TrimSpace(cfg.Workspace.Name); name != "" {
			return name
		}
	}
	return strings.TrimSpace(fallback)
}

// EffectiveCityName returns the effective deterministic naming prefix for the
// loaded config. It is empty only when neither site-bound/legacy workspace
// identity nor a derived city-root fallback is available.
func (c *City) EffectiveCityName() string {
	return EffectiveCityName(c, "")
}

// NamedSessionRuntimeName returns the deterministic runtime session_name for a
// configured named session identity under the current city naming policy.
func NamedSessionRuntimeName(cityName string, workspace Workspace, identity string) string {
	return agent.SessionNameFor(cityName, identity, workspace.SessionTemplate)
}
