package rig

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/gastownhall/gascity/internal/builtinpacks"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/packregistry"
)

func formatBoundImports(imports []config.BoundImport) string {
	parts := make([]string, 0, len(imports))
	for _, bound := range sortedBoundImports(imports) {
		part := bound.Binding
		if source := strings.TrimSpace(bound.Import.Source); source != "" {
			part += "=" + source
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, ", ")
}

// canonicalizePackIncludes rewrites --include tokens that name a pack (rather
// than a path) to that pack's canonical remote source. Builtin packs compose
// from the user-global repo cache and registry packs live behind a catalog;
// neither is registered in [packs], so a bare "<name>", "packs/<name>", or
// scoped "<owner>/<name>" token would otherwise be persisted as the
// non-resolvable literal "./<token>", breaking pack expansion citywide
// (gascity#3137 for builtins, registry-sfn for registry packs).
//
// A token is a pack-NAME candidate only when it matches the registry
// pack-name grammar (packregistry.ValidatePackName). "Contains a slash" is
// not a usable proxy for "this is a path" any more: community registry packs
// publish under scoped owner/pack names, which are names that contain a
// slash. The grammar also excludes every path/URL shape that matters here —
// absolute paths, "../", "./", dots, underscores, uppercase, "://", "git@" —
// so a real local or remote import is never mistaken for a name.
//
// Resolution precedence, most explicit first:
//
//  1. a [packs] entry keyed by the raw token or the packs/-stripped name: an
//     explicit reference keeps its configured source;
//  2. a real local pack in the city (<city>/<token>/pack.toml): a local
//     import is not a pack-name reference;
//  3. a bundled builtin pack matching the packs/-stripped name;
//  4. a pack of that exact name in a cached registry catalog;
//  5. otherwise the token is left verbatim for local/remote path handling.
//
// Local content therefore wins over the registry (2 before 4): a directory
// the user can see and edit must never be silently swapped for a fetched
// pack. Builtins win over the registry (3 before 4) so a published pack can
// never shadow a name shipped in the binary.
func canonicalizePackIncludes(fs fsys.FS, cityPath string, includes []string, packs map[string]config.PackSource, resolveRegistryPack func(string) (string, bool)) []string {
	out := make([]string, len(includes))
	for i, inc := range includes {
		out[i] = inc
		raw := filepath.ToSlash(strings.TrimSpace(inc))
		tok := strings.TrimPrefix(raw, "./")
		name := tok
		// pathDeclared marks the two spellings that say "path" outright: an
		// explicit "./" prefix, and the documented city-local "packs/<name>"
		// form. Builtin canonicalization accepts both (unchanged), but neither
		// may be reinterpreted as a registry pack name.
		pathDeclared := tok != raw
		if rest, ok := strings.CutPrefix(tok, "packs/"); ok {
			name = rest
			pathDeclared = true
		}
		if name == "" {
			continue
		}
		// Don't shadow an explicitly configured [packs] reference: a token
		// that names a registered pack keeps its configured source.
		if _, ok := packs[tok]; ok {
			continue
		}
		if _, ok := packs[name]; ok {
			continue
		}
		// A token that resolves to a real local pack in the city is a local
		// import, not a pack-name reference.
		if !filepath.IsAbs(tok) {
			if _, err := fs.Stat(filepath.Join(cityPath, filepath.FromSlash(tok), "pack.toml")); err == nil {
				continue
			}
		}
		if source, ok := builtinpacks.CanonicalImportSource(name); ok {
			out[i] = source
			continue
		}
		if source, ok := registryPackIncludeSource(fs, cityPath, tok, pathDeclared, resolveRegistryPack); ok {
			out[i] = source
		}
	}
	return out
}

// registryPackIncludeSource resolves tok as a registry pack name, the last
// resort in the canonicalizePackIncludes precedence chain.
//
// Two deliberate refusals keep it from changing what an already-working
// include means:
//
//   - a token that declares itself a path — "./<path>", or the documented
//     city-local "packs/<name>" form — is never treated as a registry name,
//     so a registry owner segment named "packs" cannot reinterpret existing
//     include lists and "./x" always means the directory x;
//   - anything already present on disk at that path wins, even a directory
//     with no pack.toml (a looser test than the builtin step above, which is
//     frozen for compatibility). New behavior gets the conservative rule: the
//     registry never shadows local content.
func registryPackIncludeSource(fs fsys.FS, cityPath, tok string, pathDeclared bool, resolveRegistryPack func(string) (string, bool)) (string, bool) {
	if resolveRegistryPack == nil || pathDeclared {
		return "", false
	}
	if err := packregistry.ValidatePackName(tok); err != nil {
		return "", false
	}
	// The grammar rejects absolute paths and "..", so joining tok onto the
	// city path cannot escape it.
	if _, err := fs.Stat(filepath.Join(cityPath, filepath.FromSlash(tok))); err == nil {
		return "", false
	}
	return resolveRegistryPack(tok)
}

func boundImportsFromImportMap(imports map[string]config.Import) []config.BoundImport {
	if len(imports) == 0 {
		return nil
	}
	bindings := make([]string, 0, len(imports))
	for binding := range imports {
		bindings = append(bindings, binding)
	}
	slices.Sort(bindings)
	bound := make([]config.BoundImport, 0, len(bindings))
	for _, binding := range bindings {
		bound = append(bound, config.BoundImport{
			Binding: binding,
			Import:  imports[binding],
		})
	}
	return bound
}

func effectiveRigBoundImports(rig *config.Rig, packs map[string]config.PackSource) ([]config.BoundImport, error) {
	if rig == nil {
		return nil, nil
	}
	legacy := config.BoundImportsFromLegacySources(rig.Includes, packs)
	return MergeBoundImports(boundImportsFromImportMap(rig.Imports), legacy)
}

func composeDefaultRigImports(root []config.BoundImport, legacyIncludes []string, packs map[string]config.PackSource) []config.BoundImport {
	if len(root) == 0 {
		return config.BoundImportsFromLegacySources(legacyIncludes, packs)
	}
	target := make(map[string]config.Import, len(root)+len(legacyIncludes))
	order := make([]string, 0, len(root)+len(legacyIncludes))
	for _, bound := range root {
		if _, exists := target[bound.Binding]; !exists {
			order = append(order, bound.Binding)
		}
		target[bound.Binding] = bound.Import
	}
	order, _ = config.AddOrderedLegacyImports(target, order, legacyIncludes, packs)
	out := make([]config.BoundImport, 0, len(order))
	for _, binding := range order {
		imp, ok := target[binding]
		if !ok {
			continue
		}
		out = append(out, config.BoundImport{Binding: binding, Import: imp})
	}
	return out
}

func sortedBoundImports(imports []config.BoundImport) []config.BoundImport {
	if len(imports) == 0 {
		return nil
	}
	sorted := append([]config.BoundImport(nil), imports...)
	slices.SortFunc(sorted, func(a, b config.BoundImport) int {
		if a.Binding != b.Binding {
			return strings.Compare(a.Binding, b.Binding)
		}
		return strings.Compare(a.Import.Source, b.Import.Source)
	})
	return sorted
}

// MergeBoundImports is for already-bound import sets. Legacy default-rig
// includes use composeDefaultRigImports so binding collisions can be
// uniquified with the migration policy.
func MergeBoundImports(primary, secondary []config.BoundImport) ([]config.BoundImport, error) {
	if len(primary) == 0 && len(secondary) == 0 {
		return nil, nil
	}
	merged := make([]config.BoundImport, 0, len(primary)+len(secondary))
	seenByBinding := make(map[string]config.Import, len(primary)+len(secondary))
	appendImport := func(bound config.BoundImport) error {
		if prior, exists := seenByBinding[bound.Binding]; exists {
			if prior.Source == bound.Import.Source {
				return nil
			}
			return fmt.Errorf("binding %q maps to both %q and %q", bound.Binding, prior.Source, bound.Import.Source)
		}
		seenByBinding[bound.Binding] = bound.Import
		merged = append(merged, bound)
		return nil
	}
	for _, bound := range primary {
		if err := appendImport(bound); err != nil {
			return nil, err
		}
	}
	for _, bound := range secondary {
		if err := appendImport(bound); err != nil {
			return nil, err
		}
	}
	return sortedBoundImports(merged), nil
}

func boundImportsMap(imports []config.BoundImport) map[string]config.Import {
	if len(imports) == 0 {
		return nil
	}
	out := make(map[string]config.Import, len(imports))
	for _, bound := range imports {
		out[bound.Binding] = bound.Import
	}
	return out
}
