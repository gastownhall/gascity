package config

import (
	"fmt"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

const agentsAliasWarning = "[agents] is a deprecated compatibility alias for [agent_defaults]; rewrite the table name to [agent_defaults]"

var agentDefaultsCompatibilityOverlapKeys = []string{
	"model",
	"wake_mode",
	"default_sling_formula",
	"allow_overlay",
	"allow_env_override",
	"append_fragments",
}

// CheckUndecodedKeys examines TOML metadata for keys that were present in
// the input but not mapped to any struct field. For each unknown key, it
// computes edit distance against known field names and suggests the closest
// match if one is within 2 edits. Returns a list of human-readable warnings.
func CheckUndecodedKeys(md toml.MetaData, source string) []string {
	undecoded := md.Undecoded()
	if len(undecoded) == 0 {
		return nil
	}

	known := knownTOMLKeys()
	var warnings []string
	for _, key := range undecoded {
		keyStr := key.String()
		if special, ok := specializedUndecodedWarning(source, keyStr); ok {
			warnings = append(warnings, special)
			continue
		}
		warnings = append(warnings, unknownFieldWarning(source, keyStr, known))
	}
	return warnings
}

func fatalUndecodedWarnings(md toml.MetaData, source string) []string {
	undecoded := md.Undecoded()
	if len(undecoded) == 0 {
		return nil
	}
	known := knownTOMLKeys()
	var warnings []string
	for _, key := range undecoded {
		keyStr := key.String()
		if _, ok := specializedUndecodedWarning(source, keyStr); ok {
			continue
		}
		warnings = append(warnings, unknownFieldWarning(source, keyStr, known))
	}
	return warnings
}

func unknownFieldWarning(source, key string, known []string) string {
	suggestion := suggestKey(key, known)
	w := fmt.Sprintf("%s: unknown field %q", source, key)
	if suggestion != "" {
		w += fmt.Sprintf(" (did you mean %q?)", suggestion)
	}
	return w
}

func agentDefaultsCompatibilityWarnings(md toml.MetaData, source string) []string {
	if !md.IsDefined("agents") {
		return nil
	}
	warnings := []string{fmt.Sprintf("%s: %s", source, agentsAliasWarning)}
	if md.IsDefined("agent_defaults") && agentDefaultsTablesOverlap(md) {
		warnings = append(warnings, fmt.Sprintf("%s: both [agent_defaults] and [agents] are present; canonical [agent_defaults] wins for overlapping keys", source))
	}
	return warnings
}

func agentDefaultsTablesOverlap(md toml.MetaData) bool {
	for _, key := range agentDefaultsCompatibilityOverlapKeys {
		if md.IsDefined("agent_defaults", key) && md.IsDefined("agents", key) {
			return true
		}
	}
	return false
}

func specializedUndecodedWarning(source, key string) (string, bool) {
	isPackSource := filepath.Base(source) == "pack.toml"
	switch key {
	case "agent_defaults.provider", "agents.provider":
		if isPackSource {
			return fmt.Sprintf("%s: %q is not supported in this release wave; keep setting provider per agent in agents/<name>/agent.toml", source, key), true
		}
		return fmt.Sprintf("%s: %q is not supported in this release wave; keep using workspace.provider (or set provider per agent in agents/<name>/agent.toml)", source, key), true
	case "agent_defaults.scope", "agents.scope":
		return fmt.Sprintf("%s: %q is not supported in this release wave; keep setting scope per agent in agents/<name>/agent.toml", source, key), true
	case "agent_defaults.install_agent_hooks", "agents.install_agent_hooks":
		if isPackSource {
			return fmt.Sprintf("%s: %q is not supported in this release wave; keep setting install_agent_hooks per agent in agents/<name>/agent.toml", source, key), true
		}
		return fmt.Sprintf("%s: %q is not supported in this release wave; keep using workspace.install_agent_hooks (or set install_agent_hooks per agent in agents/<name>/agent.toml)", source, key), true
	default:
		return "", false
	}
}

// suggestKey finds the closest known key to the given unknown key using
// edit distance. Returns the suggestion if the distance is <= 2, or "".
func suggestKey(unknown string, known []string) string {
	// Extract the leaf key (last component after dots).
	leaf := unknown
	if idx := strings.LastIndex(unknown, "."); idx >= 0 {
		leaf = unknown[idx+1:]
	}

	bestKey := ""
	bestDist := 3 // only suggest if distance <= 2
	for _, k := range known {
		d := editDistance(leaf, k)
		if d < bestDist {
			bestDist = d
			bestKey = k
		}
	}
	if bestKey == leaf {
		return ""
	}
	return bestKey
}

// editDistance computes the Levenshtein distance between two strings.
func editDistance(a, b string) int {
	la, lb := len(a), len(b)
	if la == 0 {
		return lb
	}
	if lb == 0 {
		return la
	}
	// Single-row DP.
	prev := make([]int, lb+1)
	for j := 0; j <= lb; j++ {
		prev[j] = j
	}
	for i := 1; i <= la; i++ {
		curr := make([]int, lb+1)
		curr[0] = i
		for j := 1; j <= lb; j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			ins := curr[j-1] + 1
			del := prev[j] + 1
			sub := prev[j-1] + cost
			curr[j] = min(ins, min(del, sub))
		}
		prev = curr
	}
	return prev[lb]
}

// knownTOMLKeys returns a deduplicated, sorted list of all TOML key names
// used across the config structs. Built via reflection on struct tags.
func knownTOMLKeys() []string {
	seen := make(map[string]bool)
	types := []reflect.Type{
		reflect.TypeOf(City{}),
		reflect.TypeOf(Workspace{}),
		reflect.TypeOf(Agent{}),
		reflect.TypeOf(Rig{}),
		reflect.TypeOf(ProviderSpec{}),
		reflect.TypeOf(AgentPatch{}),
		reflect.TypeOf(AgentOverride{}),
		reflect.TypeOf(BeadsConfig{}),
		reflect.TypeOf(SessionConfig{}),
		reflect.TypeOf(MailConfig{}),
		reflect.TypeOf(EventsConfig{}),
		reflect.TypeOf(DoltConfig{}),
		reflect.TypeOf(FormulasConfig{}),
		reflect.TypeOf(DaemonConfig{}),
		reflect.TypeOf(OrdersConfig{}),
		reflect.TypeOf(APIConfig{}),
		reflect.TypeOf(ConvergenceConfig{}),
		reflect.TypeOf(Service{}),
		reflect.TypeOf(ServiceWorkflowConfig{}),
		reflect.TypeOf(ServiceProcessConfig{}),
		reflect.TypeOf(AgentDefaults{}),
		reflect.TypeOf(packConfig{}),
		reflect.TypeOf(PackMeta{}),
		reflect.TypeOf(Import{}),
		reflect.TypeOf(NamedSession{}),
		reflect.TypeOf(PackRequirement{}),
		reflect.TypeOf(PackDoctorEntry{}),
		reflect.TypeOf(PackCommandEntry{}),
		reflect.TypeOf(PackGlobal{}),
		reflect.TypeOf(packDefaults{}),
		reflect.TypeOf(packRigDefaults{}),
	}
	for _, t := range types {
		collectTOMLTags(t, seen)
	}
	keys := make([]string, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// collectTOMLTags extracts TOML key names from struct tags.
func collectTOMLTags(t reflect.Type, seen map[string]bool) {
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := f.Tag.Get("toml")
		if tag == "" || tag == "-" {
			continue
		}
		// Parse "name,omitempty" → "name"
		name, _, _ := strings.Cut(tag, ",")
		if name != "" {
			seen[name] = true
		}
	}
}
