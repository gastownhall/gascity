package main

import (
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
)

// suggestSimilar returns a "did you mean X?" hint for the closest match
// in candidates to input, using Levenshtein distance. Returns "" if no
// candidate is close enough (distance > len(input)/2).
func suggestSimilar(input string, candidates []string) string {
	if len(candidates) == 0 {
		return ""
	}
	best := ""
	bestDist := len(input)/2 + 1 // threshold: must be within half the input length
	for _, c := range candidates {
		if c == input {
			// Defense-in-depth: never echo the input back as a hint. If the
			// caller's lookup said "not found" yet a candidate equals the
			// input, the lookup itself is wrong — surfacing the same string
			// as a suggestion just hides that bug.
			continue
		}
		d := levenshtein(strings.ToLower(input), strings.ToLower(c))
		if d < bestDist {
			bestDist = d
			best = c
		}
	}
	if best == "" {
		return ""
	}
	return fmt.Sprintf("; did you mean %q?", best)
}

// availableAgentNames returns all configured agent qualified names.
func availableAgentNames(cfg *config.City) []string {
	names := make([]string, 0, len(cfg.Agents))
	for _, a := range cfg.Agents {
		names = append(names, a.QualifiedName())
	}
	return names
}

// availableRigNames returns all configured rig names.
func availableRigNames(cfg *config.City) []string {
	names := make([]string, 0, len(cfg.Rigs))
	for _, r := range cfg.Rigs {
		names = append(names, r.Name)
	}
	return names
}

// formatAvailable returns a short suffix listing available names, e.g.
// "; available: mayor, worker". Returns "" if the list is empty.
// Truncates at 5 names with "..." to avoid wall-of-text errors.
func formatAvailable(label string, names []string) string {
	if len(names) == 0 {
		return ""
	}
	show := names
	suffix := ""
	if len(show) > 5 {
		show = show[:5]
		suffix = ", ..."
	}
	return fmt.Sprintf("; available %s: %s%s", label, strings.Join(show, ", "), suffix)
}

// importBindingOf extracts the pack-import binding from a (possibly
// rig-qualified) agent target. "rig/gc.run-operator" and "gc.run-operator"
// both yield "gc"; a bare name ("mayor") or pool instance ("rig/polecat-2")
// yields "" because it carries no binding.
func importBindingOf(input string) string {
	name := input
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	if i := strings.Index(name, "."); i > 0 {
		return name[:i]
	}
	return ""
}

// declaredImportSource returns the authored source for a pack-import binding
// declared anywhere in the city config — city-scope [imports.*], the
// [defaults.rig.imports.*] table, or any rig's [rigs.imports.*] — and whether
// the binding was found. It reads only what the city itself declares, so no
// pack or role name is hard-coded.
func declaredImportSource(cfg *config.City, binding string) (string, bool) {
	if cfg == nil || binding == "" {
		return "", false
	}
	if imp, ok := cfg.Imports[binding]; ok {
		return imp.Source, true
	}
	if imp, ok := cfg.Defaults.Rig.Imports[binding]; ok {
		return imp.Source, true
	}
	if imp, ok := cfg.DefaultRigImports[binding]; ok {
		return imp.Source, true
	}
	for _, r := range cfg.Rigs {
		if imp, ok := r.Imports[binding]; ok {
			return imp.Source, true
		}
	}
	return "", false
}

// uninstalledImportHint returns a remediation hint when a not-found target is
// qualified by a pack-import binding that the city declares but whose agents
// are not installed here — the case behind gascity#3832, where a fresh city
// imports the formulas pack but the roles those formulas route to live in a
// declared-but-not-yet-installed pack. The fix is to install the declared pack,
// not to pick a different agent name, so we point at `gc import install`.
// Returns "" when the binding is unknown (caller falls back to the
// available-agents list) or when an agent with that binding is already
// composed (then the miss is a wrong/typo'd name, not a missing pack).
func uninstalledImportHint(input string, cfg *config.City) string {
	binding := importBindingOf(input)
	if binding == "" || cfg == nil {
		return ""
	}
	src, ok := declaredImportSource(cfg, binding)
	if !ok {
		return ""
	}
	for _, a := range cfg.Agents {
		if importBindingOf(a.QualifiedName()) == binding {
			return "" // pack is installed; the agent name itself is wrong
		}
	}
	return fmt.Sprintf("\n  the %q pack is imported (%s) but its agents are not installed here; run `gc import install`", binding, src)
}

// agentNotFoundMsg returns a user-friendly error string for when an agent
// name is not found. When the target names a declared-but-uninstalled pack
// import it surfaces the `gc import install` repair; otherwise it falls back to
// a "did you mean?" hint and the available-agents list.
func agentNotFoundMsg(prefix, input string, cfg *config.City) string {
	base := fmt.Sprintf("%s: agent %q not found in city.toml", prefix, input)
	if hint := uninstalledImportHint(input, cfg); hint != "" {
		return base + hint
	}
	names := availableAgentNames(cfg)
	if hint := suggestSimilar(input, names); hint != "" {
		return base + hint
	}
	return base + formatAvailable("agents", names)
}

// rigNotFoundMsg returns a user-friendly error string for when a rig
// name is not found. Includes "did you mean?" and available rigs list.
func rigNotFoundMsg(prefix, input string, cfg *config.City) string {
	names := availableRigNames(cfg)
	hint := suggestSimilar(input, names)
	if hint != "" {
		return fmt.Sprintf("%s: rig %q not found in city.toml%s", prefix, input, hint)
	}
	return fmt.Sprintf("%s: rig %q not found in city.toml%s", prefix, input, formatAvailable("rigs", names))
}

// levenshtein computes the edit distance between two strings.
func levenshtein(a, b string) int {
	la, lb := len(a), len(b)
	if la == 0 {
		return lb
	}
	if lb == 0 {
		return la
	}
	// Single-row DP.
	prev := make([]int, lb+1)
	for j := range prev {
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
			curr[j] = min(curr[j-1]+1, min(prev[j]+1, prev[j-1]+cost))
		}
		prev = curr
	}
	return prev[lb]
}
