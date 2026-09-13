package bootstrap

import (
	"sort"

	"github.com/gastownhall/gascity/internal/config"
)

// CollidesWithBootstrapPack reports whether any bootstrap pack name is
// shadowed by an explicit [imports.<name>] entry declared by the city.
//
// It is a pure predicate — no I/O, no side effects. Callers supply the
// user's explicit imports map and the set of bootstrap pack names being
// installed; the returned slice lists colliding names in sorted order.
// An empty slice means no collision.
//
// The v0.15.1 hotfix adds explicit collision detection (both at
// gc init / gc import install write time and during city composition) on
// top of the previously-silent shadowing behavior; see
// engdocs/proposals/skill-materialization.md for the rationale.
func CollidesWithBootstrapPack(userImports map[string]config.Import, bootstrapNames []string) []string {
	if len(userImports) == 0 || len(bootstrapNames) == 0 {
		return nil
	}
	var collisions []string
	seen := make(map[string]struct{}, len(bootstrapNames))
	for _, name := range bootstrapNames {
		if name == "" {
			continue
		}
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		if _, exists := userImports[name]; exists {
			collisions = append(collisions, name)
		}
	}
	sort.Strings(collisions)
	return collisions
}

// PackNames returns the current list of bootstrap pack names
// (from BootstrapPacks). Exposed for callers that need the bootstrap
// name set for a collision check without depending on the Entry layout.
func PackNames() []string {
	names := make([]string, 0, len(BootstrapPacks))
	for _, entry := range BootstrapPacks {
		names = append(names, entry.Name)
	}
	return names
}
