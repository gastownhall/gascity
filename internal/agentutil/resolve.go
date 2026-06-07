// Package agentutil provides agent resolution and pool expansion for
// Gas City. It lives in agentutil (not agent) to avoid an import cycle
// with internal/config.
package agentutil

import (
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	workdirutil "github.com/gastownhall/gascity/internal/workdir"
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
	if a, ok := ResolveQualifiedRigScopedTemplate(cfg, input); ok {
		return a, true
	}

	// Step 2b: qualified pool instance — "rig/polecat-2" or
	// "binding.polecat-2" matches the corresponding pool template.
	if opts.AllowPoolMembers && strings.ContainsAny(input, "/.") {
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
	if a, ok := ResolveQualifiedRigScopedTemplate(cfg, input); ok {
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
	for _, a := range cfg.Agents {
		if config.AgentMatchesIdentity(&a, identity) {
			return a, true
		}
	}
	return config.Agent{}, false
}

// ResolveQualifiedRigScopedTemplate resolves "rig/name" against a generic
// scope="rig" template that applies to every configured rig. It returns a
// synthetic rig-bound copy so downstream code sees the concrete identity.
func ResolveQualifiedRigScopedTemplate(cfg *config.City, identity string) (config.Agent, bool) {
	if cfg == nil || !strings.Contains(identity, "/") {
		return config.Agent{}, false
	}
	dir, name := config.ParseQualifiedName(identity)
	if dir == "" || name == "" || !hasRig(cfg, dir) {
		return config.Agent{}, false
	}

	var match *config.Agent
	for i := range cfg.Agents {
		a := &cfg.Agents[i]
		if a.Name != name || a.Dir != "" || a.Scope != "rig" {
			continue
		}
		if match != nil {
			return config.Agent{}, false
		}
		match = a
	}
	if match == nil {
		return config.Agent{}, false
	}
	return DeepCopyAgent(match, match.Name, dir), true
}

func hasRig(cfg *config.City, name string) bool {
	for _, rig := range cfg.Rigs {
		if rig.Name == name {
			return true
		}
	}
	return false
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

// NormalizePoolRouteTarget collapses a slot-suffixed pool target qualified
// name (e.g. "myrig/polecat-2") back to its base pool qualified name
// ("myrig/polecat"). Slinging to a slot-suffixed target expresses a
// load-balancing hint, not a hard pin: every slot in a pool shares the base
// template, and pool work_query / nudgers key on that base via exact match.
// Recording the slot-suffixed value in gc.routed_to therefore leaves the bead
// structurally invisible to the pool. Normalizing at the routing write site
// keeps slot-suffixed slings reachable by any slot.
//
// A target is collapsed only when it is exactly base.QualifiedName()+"-N" for
// a configured multi-session pool agent and N is a valid slot (>=1, and within
// the agent's max when bounded) — the inverse of resolvePoolInstanceQualified.
// Any other target (base names, non-pool agents, out-of-range or non-numeric
// suffixes, unknown agents) is returned unchanged.
func NormalizePoolRouteTarget(cfg *config.City, target string) string {
	if cfg == nil || target == "" {
		return target
	}
	for i := range cfg.Agents {
		a := cfg.Agents[i]
		if !IsMultiSessionAgent(&a) {
			continue
		}
		base := a.QualifiedName()
		prefix := base + "-"
		if !strings.HasPrefix(target, prefix) {
			continue
		}
		suffix := target[len(prefix):]
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
		return base
	}
	return target
}

// AgentReachesWorkflowStore reports whether an agent's scale_check can read
// beads from storeRef. storeRef uses the workflow store format: "city:<name>"
// for the HQ store and "rig:<name>" for rig stores.
func AgentReachesWorkflowStore(storeRef string, agentCfg *config.Agent, cityPath string, cfg *config.City) bool {
	if cfg == nil || agentCfg == nil {
		return true
	}
	// City-scoped agents are cross-store eligible: a city-wide singleton
	// legitimately serves work in ANY store (vp-kvp). Without this exemption the
	// cross-store route guard (validateBuiltInRouteStoreReachable) would
	// false-positive on a legitimate route to a city-scoped target and refuse
	// it — the very dead-drop stages ii/iii exist to remove. Rig-scoped agents
	// stay single-store, so all existing reachability is unchanged.
	if AgentIsCrossStoreEligible(agentCfg) {
		return true
	}
	agentRig := workdirutil.ConfiguredRigName(cityPath, *agentCfg, cfg.Rigs)
	if agentRig == "" {
		return strings.HasPrefix(storeRef, "city:")
	}
	return storeRef == "rig:"+agentRig
}

// AgentIsCrossStoreEligible reports whether an agent may discover and serve
// work in ANY store, not just its configured rig. City-scoped agents are
// cross-store eligible: a city-wide singleton legitimately serves per-rig
// routed work (vp-kvp — "scope determines discovery breadth"). Centralized
// here so domain packages and the CLI share one definition.
func AgentIsCrossStoreEligible(agentCfg *config.Agent) bool {
	return agentCfg != nil && strings.TrimSpace(agentCfg.Scope) == "city"
}

// AgentReachableStoreLabel returns the workflow store ref an agent's
// scale_check reads, for use in cross-store routing diagnostics.
func AgentReachableStoreLabel(agentCfg *config.Agent, cityPath, cityName string, cfg *config.City) string {
	if cfg == nil || agentCfg == nil {
		return ""
	}
	agentRig := workdirutil.ConfiguredRigName(cityPath, *agentCfg, cfg.Rigs)
	if agentRig == "" {
		cn := strings.TrimSpace(cityName)
		if cn == "" {
			cn = "city"
		}
		return "city:" + cn
	}
	return "rig:" + agentRig
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
	return a.SupportsExpandedSessionIdentities()
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
