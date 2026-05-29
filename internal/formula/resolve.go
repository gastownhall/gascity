package formula

import (
	"os"
	"path/filepath"
)

// extOrder is the within-layer extension precedence used by Resolve:
// plain TOML beats infixed TOML beats legacy JSON. JSON is included here
// because Resolve drives the in-process parser, which still loads
// .formula.json formulas. ResolveAll deliberately excludes JSON (its caller
// stages symlinks the bd CLI consumes — TOML-only).
var extOrder = []string{CanonicalTOMLExt, LegacyTOMLExt, FormulaExtJSON}

// Resolve returns the path of the highest-priority layer that contains a
// formula by this name. layers are ordered lowest→highest priority,
// matching ComputeFormulaLayers; the highest-priority layer present wins
// (last-wins). Within a single layer, plain .toml beats infixed
// .formula.toml beats legacy .formula.json.
//
// Returns ("", false) if no layer contains the formula.
//
// Resolve consults the local filesystem directly. For ref-stable
// resolution (see #2030) use ResolveWithSource.
func Resolve(layers []string, name string) (string, bool) {
	return ResolveWithSource(FSSource{}, layers, name)
}

// ResolveWithSource is Resolve, parameterized by Source. Layers are
// probed via src.Stat; the first hit (highest priority + canonical
// extension order) wins.
func ResolveWithSource(src Source, layers []string, name string) (string, bool) {
	if src == nil {
		src = FSSource{}
	}
	for i := len(layers) - 1; i >= 0; i-- {
		for _, ext := range extOrder {
			path := filepath.Join(layers[i], name+ext)
			if src.Stat(path) {
				return path, true
			}
		}
	}
	return "", false
}

// ResolveAll returns name→winning-path for every TOML formula reachable
// across layers. Same precedence rules as Resolve: layers ordered
// lowest→highest priority (last-wins across layers), plain .toml beats
// infixed .formula.toml within a layer.
//
// JSON formulas are excluded — they are loader-only fallback and not
// suitable for symlink staging by callers that consume this map.
//
// ResolveAll consults the local filesystem directly. For ref-stable
// resolution (see #2030) use ResolveAllWithSource.
func ResolveAll(layers []string) map[string]string {
	return ResolveAllWithSource(FSSource{}, layers)
}

// ResolveAllWithSource is ResolveAll, parameterized by Source.
//
// The absolute-path emission semantic of the legacy ResolveAll is
// preserved when Source is the filesystem; for non-filesystem Sources
// the returned path is the rig-relative layer path joined with the
// entry name. Callers that stage symlinks (cmd/gc/formula_resolve.go)
// require filesystem-backed sources; ref-stable callers consume the
// returned map for content reads via ResolveWithSource + ParseFile and
// do not require absolute working-tree paths.
func ResolveAllWithSource(src Source, layers []string) map[string]string {
	if src == nil {
		src = FSSource{}
	}
	_, fsBacked := src.(FSSource)
	winners := make(map[string]string)
	for _, layerDir := range layers {
		entries, err := src.ListDir(layerDir)
		if err != nil {
			continue
		}
		// Resolve within-layer winners first so plain .toml beats an infixed
		// sibling regardless of ListDir order, then merge into the
		// cross-layer winners map (overwriting lower layers).
		layerPick := make(map[string]string)
		layerLegacy := make(map[string]bool)
		for _, entry := range entries {
			name, ok := TrimTOMLFilename(entry)
			if !ok {
				continue
			}
			legacy := entry == name+LegacyTOMLExt
			if _, exists := layerPick[name]; exists && legacy && !layerLegacy[name] {
				continue // Plain .toml already picked in this layer — skip infixed sibling.
			}
			full := filepath.Join(layerDir, entry)
			if fsBacked {
				if abs, absErr := filepath.Abs(full); absErr == nil {
					full = abs
				} else if _, statErr := os.Stat(full); statErr != nil {
					continue
				}
			}
			layerPick[name] = full
			layerLegacy[name] = legacy
		}
		for name, full := range layerPick {
			winners[name] = full
		}
	}
	return winners
}
