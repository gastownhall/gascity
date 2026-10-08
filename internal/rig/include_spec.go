package rig

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

// includeBindingPattern is the grammar of an explicit --include binding: the
// character set config's legacy binding sanitizer emits (so an explicit binding
// is one gc could have derived), starting with a letter or digit. It excludes
// ':', '/', '.', '@', '~' and '\', so no URL, scp-style remote, or path prefix
// can parse as a binding.
var includeBindingPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)

// includeSpec is one parsed --include token.
type includeSpec struct {
	Token   string // trimmed raw token, for messages
	Binding string // explicit binding; "" derives it from Source
	Source  string // pack source or pack name (canonicalized after resolveIncludeSpecs)
}

// parseIncludeSpecs splits each --include token into an optional explicit
// binding and its source. A token is read as "<binding>=<source>" only when
// the left side matches includeBindingPattern and the whole token does not
// already resolve as a source (a [packs] key, a remote, or a local pack
// directory literally named with "="), so an include that works today keeps
// its meaning. A binding with an empty source is an error.
func parseIncludeSpecs(fs fsys.FS, cityPath string, includes []string, packs map[string]config.PackSource) ([]includeSpec, error) {
	specs := make([]includeSpec, 0, len(includes))
	for _, token := range includes {
		spec := includeSpec{Token: token, Source: token}
		binding, source, found := strings.Cut(token, "=")
		if found && includeBindingPattern.MatchString(binding) && !includeSourceResolves(fs, cityPath, token, packs) {
			source = strings.TrimSpace(source)
			if source == "" {
				return nil, fmt.Errorf("--include %q names binding %q but no pack source; write --include %s=<source>", token, binding, binding)
			}
			spec.Binding = binding
			spec.Source = source
		}
		specs = append(specs, spec)
	}
	return specs, nil
}

// resolveIncludeSpecs parses the --include tokens and canonicalizes and
// validates every source through resolveIncludeSources, so a named include
// takes exactly the builtin/registry rewrite and the unresolvable-pack check an
// unnamed one does.
func resolveIncludeSpecs(fs fsys.FS, cityPath string, includes []string, packs map[string]config.PackSource, resolveRegistryPack func(string) (string, bool)) ([]includeSpec, error) {
	specs, err := parseIncludeSpecs(fs, cityPath, includes, packs)
	if err != nil {
		return nil, err
	}
	canonical, err := resolveIncludeSources(fs, cityPath, includeSpecSources(specs), packs, resolveRegistryPack)
	if err != nil {
		return nil, err
	}
	for i := range specs {
		specs[i].Source = canonical[i]
	}
	return specs, nil
}

// includeSpecSources returns the source of each spec, index-aligned.
func includeSpecSources(specs []includeSpec) []string {
	sources := make([]string, len(specs))
	for i, spec := range specs {
		sources[i] = spec.Source
	}
	return sources
}

// includeBindingClaim records one --include that wants a binding.
type includeBindingClaim struct {
	label  string
	source string
}

// bindIncludeSpecs converts resolved specs into bound imports. Unnamed specs
// go through the legacy converter unchanged (source-basename bindings, "-N"
// suffixes between derived names); a named spec keeps the converter's source
// mapping but takes its explicit binding. An explicit binding claimed by
// another include for a different source is an error; claims that agree on
// both binding and source collapse to one import.
func bindIncludeSpecs(specs []includeSpec, packs map[string]config.PackSource) ([]config.BoundImport, error) {
	var unnamedSources []string
	var named []config.BoundImport
	claims := make(map[string][]includeBindingClaim)
	for _, spec := range specs {
		if spec.Binding == "" {
			unnamedSources = append(unnamedSources, spec.Source)
			continue
		}
		bound := config.BoundImportsFromLegacySources([]string{spec.Source}, packs)[0]
		bound.Binding = spec.Binding
		claims[spec.Binding] = appendIncludeClaim(claims[spec.Binding], includeBindingClaim{label: spec.Token, source: bound.Import.Source})
		if !slices.ContainsFunc(named, func(b config.BoundImport) bool { return b.Binding == bound.Binding }) {
			named = append(named, bound)
		}
	}
	derived := config.BoundImportsFromLegacySources(unnamedSources, packs)
	out := named
	for _, bound := range derived {
		if existing, ok := claims[bound.Binding]; ok {
			claims[bound.Binding] = appendIncludeClaim(existing, includeBindingClaim{label: bound.Import.Source, source: bound.Import.Source})
			continue
		}
		out = append(out, bound)
	}
	if err := includeBindingConflict(claims); err != nil {
		return nil, err
	}
	return sortedBoundImports(out), nil
}

// appendIncludeClaim adds claim unless an identical one is already recorded.
func appendIncludeClaim(claims []includeBindingClaim, claim includeBindingClaim) []includeBindingClaim {
	if slices.Contains(claims, claim) {
		return claims
	}
	return append(claims, claim)
}

// includeBindingConflict returns an error for the first binding, in sorted
// order, whose claims disagree on the import source.
func includeBindingConflict(claims map[string][]includeBindingClaim) error {
	bindings := make([]string, 0, len(claims))
	for binding := range claims {
		bindings = append(bindings, binding)
	}
	slices.Sort(bindings)
	for _, binding := range bindings {
		list := claims[binding]
		if !slices.ContainsFunc(list, func(c includeBindingClaim) bool { return c.source != list[0].source }) {
			continue
		}
		labels := make([]string, 0, len(list))
		for _, claim := range list {
			labels = append(labels, strconv.Quote(claim.label))
		}
		return fmt.Errorf("--include binding %q is claimed by more than one pack (%s); each binding must name one pack, so choose another with <binding>=<source>", binding, strings.Join(labels, ", "))
	}
	return nil
}

// composeExplicitRigImports binds the explicit --include specs and resolves
// them. A fresh add with imports goes through Deps.ResolveIncludeImports when
// the caller provides it (gc import add parity: version constraints and lock
// entries for remote sources); a re-add, an add without includes, or a caller
// without the seam keeps the bundled-only Deps.ComposePacks hardening.
func composeExplicitRigImports(deps Deps, specs []includeSpec, reAdd bool) ([]config.BoundImport, func() error, error) {
	bound, err := bindIncludeSpecs(specs, deps.Cfg.Packs)
	if err != nil {
		return nil, nil, err
	}
	if !reAdd && len(bound) > 0 && deps.ResolveIncludeImports != nil {
		resolved, commit, err := deps.ResolveIncludeImports(deps.CityPath, bound)
		if err != nil {
			return nil, nil, fmt.Errorf("resolving rig imports: %w", err)
		}
		return resolved, commit, nil
	}
	pinned, commit, err := deps.ComposePacks(deps.CityPath, bound)
	if err != nil {
		return nil, nil, fmt.Errorf("installing bundled rig imports: %w", err)
	}
	return pinned, commit, nil
}
