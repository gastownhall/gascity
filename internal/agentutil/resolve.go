// Package agentutil provides agent resolution and pool expansion for
// Gas City. It lives in agentutil (not agent) to avoid an import cycle
// with internal/config.
package agentutil

import (
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
)

// ResolveOpts controls the behavior of ResolveAgent.
type ResolveOpts struct {
	// UseAmbientRig enables contextual rig preference. When set along with
	// RigContext, bare names try the rig-scoped agent first before falling
	// back to literal/bare-name resolution.
	UseAmbientRig bool

	// RigContext is the rig directory name for contextual resolution.
	// Only used when UseAmbientRig is true.
	RigContext string

	// TemplateOnly restricts resolution to configured templates only.
	// No ambient rig qualification, no pool-member synthesis.
	// Used by API session creation.
	TemplateOnly bool

	// AllowPoolMembers enables pool instance synthesis (e.g., "polecat-2"
	// matching pool "polecat"). Used by dispatch modes (CLI + API sling).
	AllowPoolMembers bool
}

// ResolveAgent resolves an agent input string to a config.Agent using
// options-driven behavior that serves three resolution modes:
//
//   - CLI dispatch: UseAmbientRig=true, AllowPoolMembers=true
//   - API sling dispatch: AllowPoolMembers=true (no ambient rig)
//   - API session creation: TemplateOnly=true
func ResolveAgent(cfg *config.City, input string, opts ResolveOpts) (config.Agent, bool) {
	if opts.TemplateOnly {
		return resolveTemplate(cfg, input)
	}

	// Step 1: contextual rig match (bare name + rig context).
	if opts.UseAmbientRig && !strings.Contains(input, "/") && opts.RigContext != "" {
		if a, ok := findAgentByQualified(cfg, opts.RigContext+"/"+input); ok {
			return a, true
		}
	}

	// Step 2: literal match (qualified or city-scoped).
	if a, ok := findAgentByQualified(cfg, input); ok {
		return a, true
	}

	// Step 2b: qualified pool instance — "rig/polecat-2" matches pool "rig/polecat".
	if opts.AllowPoolMembers && strings.Contains(input, "/") {
		if a, ok := resolvePoolInstanceQualified(cfg, input); ok {
			return a, true
		}
	}

	// Step 3: unambiguous bare name — scan all agents by Name (ignoring Dir).
	if !strings.Contains(input, "/") {
		var matches []config.Agent
		for _, a := range cfg.Agents {
			if a.Name == input {
				matches = append(matches, a)
				continue
			}
			// Pool instance: "polecat-2" matches pool "polecat".
			if opts.AllowPoolMembers {
				if inst, ok := matchPoolInstanceBare(a, input); ok {
					matches = append(matches, inst)
				}
			}
		}
		if len(matches) == 1 {
			return matches[0], true
		}
	}

	return config.Agent{}, false
}

// resolveTemplate resolves only configured templates (no pool members,
// no ambient rig). Bare names must be city-unique.
func resolveTemplate(cfg *config.City, input string) (config.Agent, bool) {
	if a, ok := findAgentByQualified(cfg, input); ok {
		return a, true
	}
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

// findAgentByQualified looks up an agent by its exact qualified identity.
func findAgentByQualified(cfg *config.City, identity string) (config.Agent, bool) {
	dir, name := config.ParseQualifiedName(identity)
	for _, a := range cfg.Agents {
		if a.Dir == dir && a.Name == name {
			return a, true
		}
	}
	return config.Agent{}, false
}

// resolvePoolInstanceQualified handles qualified pool instance names like
// "rig/polecat-2" by matching against each pool agent.
func resolvePoolInstanceQualified(cfg *config.City, input string) (config.Agent, bool) {
	for _, a := range cfg.Agents {
		if !IsMultiSessionAgent(&a) {
			continue
		}
		prefix := a.QualifiedName() + "-"
		if !strings.HasPrefix(input, prefix) {
			continue
		}
		suffix := input[len(prefix):]
		n, err := strconv.Atoi(suffix)
		if err != nil || n < 1 {
			continue
		}
		if !a.HasUnlimitedSessionCapacity() {
			maxSess := a.EffectiveMaxActiveSessions()
			if maxSess == nil || n > *maxSess {
				continue
			}
		}
		return DeepCopyAgent(&a, a.Name+"-"+suffix, a.Dir), true
	}
	return config.Agent{}, false
}

// matchPoolInstanceBare checks if a bare input matches a multi-session
// agent's instance pattern (e.g., "polecat-2" matches "polecat").
func matchPoolInstanceBare(a config.Agent, input string) (config.Agent, bool) {
	if !IsMultiSessionAgent(&a) {
		return config.Agent{}, false
	}
	prefix := a.Name + "-"
	if !strings.HasPrefix(input, prefix) {
		return config.Agent{}, false
	}
	suffix := input[len(prefix):]
	n, err := strconv.Atoi(suffix)
	if err != nil || n < 1 {
		return config.Agent{}, false
	}
	if !a.HasUnlimitedSessionCapacity() {
		maxSess := a.EffectiveMaxActiveSessions()
		if maxSess == nil || n > *maxSess {
			return config.Agent{}, false
		}
	}
	return DeepCopyAgent(&a, input, a.Dir), true
}

// IsMultiSessionAgent reports whether a config agent supports multiple
// concurrent sessions.
func IsMultiSessionAgent(a *config.Agent) bool {
	return a != nil && a.SupportsInstanceExpansion()
}

// DeepCopyAgent creates a deep copy of a config.Agent with a new name and dir.
func DeepCopyAgent(src *config.Agent, name, dir string) config.Agent {
	dst := *src
	dst.Name = name
	dst.Dir = dir
	// Deep-copy slices and maps to prevent aliasing.
	if src.PreStart != nil {
		dst.PreStart = append([]string(nil), src.PreStart...)
	}
	if src.Args != nil {
		dst.Args = append([]string(nil), src.Args...)
	}
	if src.ProcessNames != nil {
		dst.ProcessNames = append([]string(nil), src.ProcessNames...)
	}
	if src.Env != nil {
		dst.Env = make(map[string]string, len(src.Env))
		for k, v := range src.Env {
			dst.Env[k] = v
		}
	}
	if src.OptionDefaults != nil {
		dst.OptionDefaults = make(map[string]string, len(src.OptionDefaults))
		for k, v := range src.OptionDefaults {
			dst.OptionDefaults[k] = v
		}
	}
	if src.SessionSetup != nil {
		dst.SessionSetup = append([]string(nil), src.SessionSetup...)
	}
	if src.SessionLive != nil {
		dst.SessionLive = append([]string(nil), src.SessionLive...)
	}
	if src.NamepoolNames != nil {
		dst.NamepoolNames = append([]string(nil), src.NamepoolNames...)
	}
	return dst
}
