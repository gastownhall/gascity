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

// agentNotFoundMsg returns a user-friendly error string for when an agent
// name is not found. Includes "did you mean?" and available agents list.
func agentNotFoundMsg(prefix, input string, cfg *config.City) string {
	names := availableAgentNames(cfg)
	hint := suggestSimilar(input, names)
	if hint != "" {
		return fmt.Sprintf("%s: agent %q not found in city.toml%s", prefix, input, hint)
	}
	return fmt.Sprintf("%s: agent %q not found in city.toml%s", prefix, input, formatAvailable("agents", names))
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
