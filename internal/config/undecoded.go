package config

import (
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

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
		// Skip deeply nested keys — we only warn on section-level and
		// field-level keys, not on sub-sub-fields of tables.
		suggestion := suggestKey(keyStr, known)
		w := fmt.Sprintf("%s: unknown field %q", source, keyStr)
		if suggestion != "" {
			w += fmt.Sprintf(" (did you mean %q?)", suggestion)
		}
		warnings = append(warnings, w)
	}
	return warnings
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
