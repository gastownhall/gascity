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
	if candidates := NamedSessionBareCandidates(cfg, identity); len(candidates) == 1 {
		return candidates[0]
	}
	return nil
}

// NamedSessionBareCandidates returns the bound named sessions a dot-free
// identity denotes, in config order: the entries with a binding whose
// IdentityName is "<binding>.<identity>". It is the forward direction of
// the bare-name relation FindNamedSession resolves with — a unique
// candidate is the named session the bare identity denotes, and two or
// more mean the caller must spell the qualified form. A dotted identity
// has no candidates, and neither does a bindingless entry: its qualified
// name already is the bare form.
func NamedSessionBareCandidates(cfg *City, identity string) []*NamedSession {
	if cfg == nil || identity == "" || strings.Contains(identity, ".") {
		return nil
	}
	var candidates []*NamedSession
	for i := range cfg.NamedSessions {
		ns := &cfg.NamedSessions[i]
		if ns.BindingName == "" {
			continue
		}
		if ns.IdentityName() == ns.BindingName+"."+identity {
			candidates = append(candidates, ns)
		}
	}
	return candidates
}

// NamedSessionBareIdentity returns the bare, pre-requalification form of ns
// — the identity a row stamped before ns was bound to its rig still
// carries — or "" when ns has none. A bound entry (BindingName != "")
// whose bare leaf resolves back to itself by FindNamedSession's
// unique-candidate rule has that leaf as its bare form; a top-level entry,
// an entry with no binding, and a leaf that a top-level entry shadows or
// that two bindings share all return "". Config-level uniqueness is the
// guard, so the reverse direction (resolving a bare row to ns) stays
// unambiguous.
func NamedSessionBareIdentity(cfg *City, ns *NamedSession) string {
	leaf := namedSessionLeaf(ns)
	if leaf == "" || FindNamedSession(cfg, leaf) != ns {
		return ""
	}
	return leaf
}

// NamedSessionPriorIdentity returns the bare form a row of ns may have been
// renamed in place from when a `gc rig add --adopt` requalified it
// (gastownhall/gascity#5957), or "": ns's leaf, unless a top-level entry is
// configured under that leaf, which then owns the bare name outright. Unlike
// NamedSessionBareIdentity it does not need the leaf to stay unique among
// bindings. The rename runs only while the leaf is unique, and a binding of
// the same leaf added after it, such as a second rig importing the same pack,
// does not take the bare form from the row renamed out of it. The row's alias
// history, which the rename alone writes the leaf to, decides whether a given
// row owns it.
func NamedSessionPriorIdentity(cfg *City, ns *NamedSession) string {
	leaf := namedSessionLeaf(ns)
	if leaf == "" || cfg == nil {
		return ""
	}
	for i := range cfg.NamedSessions {
		if cfg.NamedSessions[i].QualifiedName() == leaf {
			return ""
		}
	}
	return leaf
}

// namedSessionLeaf is the bare form of a bound named session: its identity
// without the binding prefix. A bindingless entry has none, and neither does
// a dotted leaf, which no bare identity resolves to
// (NamedSessionBareCandidates).
func namedSessionLeaf(ns *NamedSession) string {
	if ns == nil || ns.BindingName == "" {
		return ""
	}
	leaf := strings.TrimPrefix(ns.IdentityName(), ns.BindingName+".")
	if strings.Contains(leaf, ".") {
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
