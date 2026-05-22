package formula

import (
	"os"
	"path/filepath"
)

// extOrder is the within-layer extension precedence used by Resolve:
// canonical TOML beats legacy infixed TOML beats legacy JSON. JSON is
// included here because Resolve drives the in-process parser, which still
// loads .formula.json formulas. ResolveAll deliberately excludes JSON
// (its caller stages symlinks the bd CLI consumes — TOML-only).
var extOrder = []string{CanonicalTOMLExt, LegacyTOMLExt, FormulaExtJSON}

// Resolve returns the path of the highest-priority layer that contains a
// formula by this name. layers are ordered lowest→highest priority,
// matching ComputeFormulaLayers; the highest-priority layer present wins
// (last-wins). Within a single layer, canonical .toml beats legacy
// .formula.toml beats legacy .formula.json.
//
// Returns ("", false) if no layer contains the formula.
func Resolve(layers []string, name string) (string, bool) {
	for i := len(layers) - 1; i >= 0; i-- {
		for _, ext := range extOrder {
			path := filepath.Join(layers[i], name+ext)
			if _, err := os.Stat(path); err == nil {
				return path, true
			}
		}
	}
	return "", false
}

// ResolveAll returns name→winning-path for every TOML formula reachable
// across layers. Same precedence rules as Resolve: layers ordered
// lowest→highest priority (last-wins across layers), canonical beats
// legacy within a layer.
//
// JSON formulas are excluded — they are loader-only fallback and not
// suitable for symlink staging by callers that consume this map.
func ResolveAll(layers []string) map[string]string {
	winners := make(map[string]string)
	for _, layerDir := range layers {
		entries, err := os.ReadDir(layerDir)
		if err != nil {
			continue
		}
		// Resolve within-layer winners first so canonical beats legacy
		// sibling regardless of ReadDir order, then merge into the
		// cross-layer winners map (overwriting lower layers).
		layerPick := make(map[string]string)
		layerLegacy := make(map[string]bool)
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			name, ok := TrimTOMLFilename(e.Name())
			if !ok {
				continue
			}
			legacy := e.Name() == name+LegacyTOMLExt
			if _, exists := layerPick[name]; exists && legacy && !layerLegacy[name] {
				continue // Canonical already picked in this layer — skip legacy sibling.
			}
			abs, err := filepath.Abs(filepath.Join(layerDir, e.Name()))
			if err != nil {
				continue
			}
			layerPick[name] = abs
			layerLegacy[name] = legacy
		}
		for name, abs := range layerPick {
			winners[name] = abs
		}
	}
	return winners
}
