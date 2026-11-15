package bdflags

import (
	"sort"
	"strings"
)

// TopLevelVerbs returns every top-level bd subcommand of the pinned
// inventories (internal/bdflags/inventory, bd's own cobra tree, hidden
// commands included), sorted. Aliases are not included; see TopLevelAliases.
func TopLevelVerbs() []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range CommandPaths() {
		v, _, _ := strings.Cut(p, " ")
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}

// TopLevelAliases returns the top-level alias-to-verb map of the pinned
// inventories ("new" → "create").
func TopLevelAliases() map[string]string {
	out := map[string]string{}
	for alias, target := range CommandAliases() {
		if !strings.Contains(alias, " ") {
			out[alias] = target
		}
	}
	return out
}
