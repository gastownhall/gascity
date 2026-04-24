package config

import (
	"fmt"
	"strings"
)

// ProviderChainError reports a provider base-chain resolution failure.
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
	chainSpecs []ProviderSpec // raw spec per hop, parallel to chain
	chainPath  []string       // human-readable chain names for error messages
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
	ctx.chainSpecs = append(ctx.chainSpecs, leaf)
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
	resolvedPtr.Provenance = buildProviderProvenance(ctx, customProviders)
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
	ctx.chainSpecs = append(ctx.chainSpecs, parentSpec)
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

// buildProviderProvenance walks the chain (root → leaf) a second time
// to compute per-field attribution. For each scalar field that has a
// non-zero value at some layer, the "most specific wins" rule of
// MergeProviderOverBuiltin means the leaf-most non-zero value wins;
// provenance records that leaf's layer.
//
// For additive maps (Env, OptionDefaults, PermissionModes), provenance
// is tracked per-key: each key's layer is the leaf-most layer that set
// that key in the raw (unmerged) spec.
//
// Runs in O(chain_depth × field_count) — negligible for typical configs.
func buildProviderProvenance(ctx *chainResolveContext, customProviders map[string]ProviderSpec) ProviderProvenance {
	_ = customProviders // kept for signature stability; specs come from ctx.chainSpecs
	prov := ProviderProvenance{
		Chain:       append([]HopIdentity(nil), ctx.chain...),
		FieldLayer:  make(map[string]string),
		MapKeyLayer: make(map[string]map[string]string),
	}
	// Walk leaf → root; record the FIRST layer (leaf-most) that sets
	// each scalar field. For additive maps, record per-key leaf-most.
	for i, hop := range ctx.chain {
		spec := ctx.chainSpecs[i]
		layer := provenanceSource(hop)
		recordScalarProvenance(spec, layer, prov.FieldLayer)
		recordMapProvenance(spec, layer, prov.MapKeyLayer)
	}
	return prov
}

// recordScalarProvenance marks fields that have a non-zero value in
// `spec` as sourced from `layer`, but only if they haven't been marked
// by an earlier (more specific) hop already.
func recordScalarProvenance(spec ProviderSpec, layer string, into map[string]string) {
	set := func(key, value string) {
		if value == "" {
			return
		}
		if _, already := into[key]; already {
			return
		}
		into[key] = layer
	}
	setInt := func(key string, value int) {
		if value == 0 {
			return
		}
		if _, already := into[key]; already {
			return
		}
		into[key] = layer
	}
	setBool := func(key string, value *bool) {
		if value == nil {
			return
		}
		if _, already := into[key]; already {
			return
		}
		into[key] = layer
	}
	setSlice := func(key string, value []string) {
		if value == nil {
			return
		}
		if _, already := into[key]; already {
			return
		}
		into[key] = layer
	}

	set("display_name", spec.DisplayName)
	set("command", spec.Command)
	setSlice("args", spec.Args)
	setSlice("args_append", spec.ArgsAppend)
	set("prompt_mode", spec.PromptMode)
	set("prompt_flag", spec.PromptFlag)
	setInt("ready_delay_ms", spec.ReadyDelayMs)
	set("ready_prompt_prefix", spec.ReadyPromptPrefix)
	setSlice("process_names", spec.ProcessNames)
	set("path_check", spec.PathCheck)
	setBool("supports_acp", spec.SupportsACP)
	setBool("supports_hooks", spec.SupportsHooks)
	setBool("emits_permission_warning", spec.EmitsPermissionWarning)
	set("instructions_file", spec.InstructionsFile)
	set("resume_flag", spec.ResumeFlag)
	set("resume_style", spec.ResumeStyle)
	set("resume_command", spec.ResumeCommand)
	set("session_id_flag", spec.SessionIDFlag)
	set("title_model", spec.TitleModel)
	set("options_schema_merge", spec.OptionsSchemaMerge)
	setSlice("print_args", spec.PrintArgs)
}

// recordMapProvenance marks each key of each additive map as sourced
// from `layer`, preserving earlier (more specific) attributions.
func recordMapProvenance(spec ProviderSpec, layer string, into map[string]map[string]string) {
	recordKeys := func(field string, m map[string]string) {
		if len(m) == 0 {
			return
		}
		existing, ok := into[field]
		if !ok {
			existing = make(map[string]string)
			into[field] = existing
		}
		for k := range m {
			if _, already := existing[k]; already {
				continue
			}
			existing[k] = layer
		}
	}
	recordKeys("env", spec.Env)
	recordKeys("permission_modes", spec.PermissionModes)
	recordKeys("option_defaults", spec.OptionDefaults)
}
