package config

import (
	"fmt"
	"strings"
)

// ErrProviderChainCycle is returned when a provider's base chain contains a cycle.
type ProviderChainError struct {
	Kind    string // "cycle" | "unknown_base" | "wrapper_resume_missing"
	Leaf    string
	Message string
}

func (e *ProviderChainError) Error() string { return e.Message }

// chainResolveContext is the state threaded through a chain walk.
type chainResolveContext struct {
	all        map[string]ProviderSpec // custom providers only (no built-ins)
	builtins   map[string]ProviderSpec
	visited    map[HopIdentity]bool
	chain      []HopIdentity
	chainPath  []string // human-readable chain names for error messages
	warnings   []string
}

// ResolveProviderChain walks the base chain for a custom provider and
// returns a merged ProviderSpec plus chain metadata.
//
// Rules (from engdocs/design/provider-inheritance.md):
//   - `base = nil` (absent): no chain walk; returns the spec as-is with
//     empty Chain. Phase A legacy auto-inheritance is handled by
//     lookupProvider, not here.
//   - `base = &""` (explicit opt-out): no chain walk; returns spec as-is.
//   - `base = "builtin:X"`: look up X in BuiltinProviders(). Miss → error.
//   - `base = "provider:X"`: look up X in customProviders (self-cycle on X == leaf). Miss → error.
//   - `base = "X"` (bare): custom first (self-excluded), fallthrough to
//     built-in; miss on both → error.
//   - Cycle detection keyed on (HopIdentity.Kind, HopIdentity.Name).
//   - BuiltinAncestor = first built-in hop in the walk, or "" if none.
//
// The returned ResolvedProvider carries the fully merged ProviderSpec
// (via embedded fields), BuiltinAncestor, and Chain (leaf → root).
func ResolveProviderChain(leafName string, leaf ProviderSpec, customProviders map[string]ProviderSpec) (ResolvedProvider, error) {
	ctx := &chainResolveContext{
		all:       customProviders,
		builtins:  BuiltinProviders(),
		visited:   make(map[HopIdentity]bool),
		chain:     []HopIdentity{},
		chainPath: []string{},
	}

	// The leaf itself counts as hop 0. Mark its identity as custom (leaves
	// are always authored in config — built-in-only providers come through
	// a different path).
	leafID := HopIdentity{Kind: "custom", Name: leafName}
	ctx.visited[leafID] = true
	ctx.chain = append(ctx.chain, leafID)
	ctx.chainPath = append(ctx.chainPath, leafName)

	merged, err := ctx.walkFromLeaf(leafName, leaf)
	if err != nil {
		return ResolvedProvider{}, err
	}

	// Determine BuiltinAncestor: first hop in chain with Kind == "builtin".
	// Chain is currently leaf → root (leaf at 0). Iterate from index 1
	// forward (parents) to find the first built-in.
	ancestor := ""
	for _, hop := range ctx.chain {
		if hop.Kind == "builtin" {
			ancestor = hop.Name
			break
		}
	}

	// Validate wrapper-resume: if the resolved provider has a subcommand-
	// style resume style inherited AND the leaf's Command differs from
	// the inherited ancestor's Command AND the leaf has no ResumeCommand,
	// it's a config error.
	if err := ctx.validateWrapperResume(leafName, leaf, merged); err != nil {
		return ResolvedProvider{}, err
	}

	resolvedPtr := specToResolved(leafName, &merged)
	resolvedPtr.BuiltinAncestor = ancestor
	resolvedPtr.Chain = ctx.chain
	// Kind is the legacy field; mirror BuiltinAncestor for backward compat.
	if ancestor != "" {
		resolvedPtr.Kind = ancestor
	}
	return *resolvedPtr, nil
}

// walkFromLeaf does the recursive merge: resolve parent (if any), then
// merge leaf over parent.
func (ctx *chainResolveContext) walkFromLeaf(name string, spec ProviderSpec) (ProviderSpec, error) {
	if spec.Base == nil {
		// No base declared — this is a chain root. Return as-is.
		return spec, nil
	}
	baseValue := strings.TrimSpace(*spec.Base)
	if baseValue == "" {
		// Explicit empty opt-out — no inheritance.
		return spec, nil
	}

	parentKind, parentName, err := classifyBase(baseValue)
	if err != nil {
		return ProviderSpec{}, &ProviderChainError{
			Kind:    "unknown_base",
			Leaf:    ctx.chainPath[0],
			Message: fmt.Sprintf("provider %q has invalid base %q: %v", name, baseValue, err),
		}
	}

	// Resolve parent spec.
	parentSpec, resolvedKind, err := ctx.lookupBase(name, baseValue, parentKind, parentName)
	if err != nil {
		return ProviderSpec{}, err
	}

	parentID := HopIdentity{Kind: resolvedKind, Name: parentName}
	if ctx.visited[parentID] {
		// Cycle: we've seen this identity before.
		cyclePath := append([]string{}, ctx.chainPath...)
		cyclePath = append(cyclePath, formatHopName(parentID))
		return ProviderSpec{}, &ProviderChainError{
			Kind:    "cycle",
			Leaf:    ctx.chainPath[0],
			Message: fmt.Sprintf("provider %q has inheritance cycle: %s", ctx.chainPath[0], strings.Join(cyclePath, " → ")),
		}
	}
	ctx.visited[parentID] = true
	ctx.chain = append(ctx.chain, parentID)
	ctx.chainPath = append(ctx.chainPath, formatHopName(parentID))

	// Recurse: resolve the parent's own chain first.
	parentMerged, err := ctx.walkFromLeaf(parentName, parentSpec)
	if err != nil {
		return ProviderSpec{}, err
	}

	// Merge leaf over parent (parent is the "base", leaf is the "city").
	return MergeProviderOverBuiltin(parentMerged, spec), nil
}

// lookupBase resolves a base reference to a ProviderSpec and confirms its
// identity kind.
func (ctx *chainResolveContext) lookupBase(leafName, baseValue, parentKind, parentName string) (ProviderSpec, string, error) {
	// Self-exclusion: when resolving bare name, skip the leaf itself.
	// Note: leafName here is the hop currently being resolved (the owner
	// of the `base` field we're following), not the original walk leaf.
	switch parentKind {
	case "builtin":
		if spec, ok := ctx.builtins[parentName]; ok {
			return spec, "builtin", nil
		}
		return ProviderSpec{}, "", &ProviderChainError{
			Kind:    "unknown_base",
			Leaf:    ctx.chainPath[0],
			Message: fmt.Sprintf("provider %q has unknown base %q: no built-in with that name exists", leafName, baseValue),
		}
	case "provider":
		if parentName == leafName {
			// Self-cycle via provider: prefix — distinct from unknown-base.
			return ProviderSpec{}, "", &ProviderChainError{
				Kind:    "cycle",
				Leaf:    ctx.chainPath[0],
				Message: fmt.Sprintf("provider %q has inheritance cycle: self-reference via %q", leafName, baseValue),
			}
		}
		if spec, ok := ctx.all[parentName]; ok {
			return spec, "custom", nil
		}
		return ProviderSpec{}, "", &ProviderChainError{
			Kind:    "unknown_base",
			Leaf:    ctx.chainPath[0],
			Message: fmt.Sprintf("provider %q has unknown base %q: no custom provider with that name", leafName, baseValue),
		}
	case "bare":
		// Bare name: custom first (self-excluded), then built-in.
		if parentName != leafName {
			if spec, ok := ctx.all[parentName]; ok {
				return spec, "custom", nil
			}
		}
		if spec, ok := ctx.builtins[parentName]; ok {
			return spec, "builtin", nil
		}
		// If no built-in and bare name equals leaf name with no custom
		// alternative, it's a self-cycle (user wrote base = "foo" in
		// [providers.foo] with no built-in foo).
		if parentName == leafName {
			return ProviderSpec{}, "", &ProviderChainError{
				Kind:    "cycle",
				Leaf:    ctx.chainPath[0],
				Message: fmt.Sprintf("provider %q has inheritance cycle: self-reference via bare name %q (no built-in of that name exists)", leafName, baseValue),
			}
		}
		return ProviderSpec{}, "", &ProviderChainError{
			Kind:    "unknown_base",
			Leaf:    ctx.chainPath[0],
			Message: fmt.Sprintf("provider %q has unknown base %q (no custom provider or built-in with that name)", leafName, baseValue),
		}
	}
	return ProviderSpec{}, "", fmt.Errorf("internal: unknown parent kind %q", parentKind)
}

// classifyBase parses a raw base string into a (kind, name) pair.
// Returns "builtin", "provider", or "bare" for kind.
func classifyBase(baseValue string) (kind, name string, err error) {
	switch {
	case strings.HasPrefix(baseValue, BasePrefixBuiltin):
		suffix := strings.TrimPrefix(baseValue, BasePrefixBuiltin)
		if suffix == "" {
			return "", "", fmt.Errorf("empty suffix after %q prefix", BasePrefixBuiltin)
		}
		return "builtin", suffix, nil
	case strings.HasPrefix(baseValue, BasePrefixProvider):
		suffix := strings.TrimPrefix(baseValue, BasePrefixProvider)
		if suffix == "" {
			return "", "", fmt.Errorf("empty suffix after %q prefix", BasePrefixProvider)
		}
		return "provider", suffix, nil
	case strings.Contains(baseValue, ":"):
		return "", "", fmt.Errorf("unknown namespace prefix (only %q and %q are reserved)", BasePrefixBuiltin, BasePrefixProvider)
	default:
		return "bare", baseValue, nil
	}
}

// validateWrapperResume implements the "wrapper descendants of subcommand-
// style resume providers must declare resume_command" rule.
func (ctx *chainResolveContext) validateWrapperResume(leafName string, leaf, merged ProviderSpec) error {
	// If leaf already declares ResumeCommand, we're fine.
	if strings.TrimSpace(leaf.ResumeCommand) != "" {
		return nil
	}
	// If merged (inherited) ResumeStyle is not subcommand-style, we're fine.
	// Today the only subcommand style is "subcommand"; a data-driven check
	// here would compare against a registry. Keep a simple literal for now.
	if merged.ResumeStyle != "subcommand" {
		return nil
	}
	// Find the inherited built-in's Command to compare against the leaf's.
	// Walk chain looking for the first non-leaf hop to get the inherited
	// command. If inherited.Command == leaf.Command, this isn't a wrapper;
	// regular resume behavior applies.
	if leaf.Command == "" {
		// Leaf inherits command wholesale — definitely not a wrapper.
		return nil
	}
	inheritedCommand := ""
	// Resolve parent spec to look at its Command. Use the resolved merged
	// result minus the leaf's explicit override.
	// merged.Command is the leaf's own Command if set, else inherited.
	// To find inherited: we compare merged.Command against leaf.Command.
	// If they differ, the leaf overrode; inherited is the chain's ancestor.
	// A simple way: look up the nearest builtin ancestor and read its Command.
	for _, hop := range ctx.chain[1:] {
		if hop.Kind == "builtin" {
			if b, ok := ctx.builtins[hop.Name]; ok {
				inheritedCommand = b.Command
				break
			}
		}
	}
	if inheritedCommand == "" {
		// No built-in ancestor; the subcommand-style resume came from a
		// custom provider. Best effort: compare against merged's pre-leaf
		// command. Skip the check to avoid false positives.
		return nil
	}
	if leaf.Command == inheritedCommand {
		return nil // not a wrapper
	}
	return &ProviderChainError{
		Kind: "wrapper_resume_missing",
		Leaf: leafName,
		Message: fmt.Sprintf(
			"provider %q wraps a subcommand-style resume provider (%s) but does not declare `resume_command`. Wrapper providers must specify their own resume invocation (e.g. resume_command = %q).",
			leafName, inheritedCommand,
			"aimux run "+inheritedCommand+" -- resume {{.SessionKey}}"),
	}
}

// formatHopName renders a HopIdentity as a human-readable string with
// namespace prefix for error messages.
func formatHopName(id HopIdentity) string {
	if id.Kind == "builtin" {
		return BasePrefixBuiltin + id.Name
	}
	return id.Name
}
