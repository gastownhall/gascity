package config

// ProviderProvenance records the layer that contributed each field of a
// ResolvedProvider. Populated during ResolveProviderChain. Per-field
// granularity for scalars and per-map-key granularity for additive maps
// (Env, PermissionModes, OptionDefaults). Per-segment granularity for
// args (Args ++ ArgsAppend) is captured via ArgsSegments.
//
// Design: engdocs/design/provider-inheritance.md §Provenance data model.
//
// This is v1 — tracks field-level attribution sufficient for
// `gc config explain --provider <name>` to answer "which layer set X".
// Future extensions (per-options-schema-entry attribution, per-args-
// segment highlighting in explain UI) slot in without breaking callers.
type ProviderProvenance struct {
	// Chain is the resolved ancestry from leaf (index 0) to root (index
	// len-1). Duplicates ResolvedProvider.Chain for convenience.
	Chain []HopIdentity

	// FieldLayer maps field name (TOML/API snake_case) → the layer that
	// contributed that field's final value. Layers use the form
	//   "providers.<name>"  for custom providers
	//   "builtin:<name>"    for built-in ancestors
	// Only populated for fields that vary across layers; unset means the
	// field took its zero value or was not exercised.
	FieldLayer map[string]string

	// MapKeyLayer maps field name → (key → layer). Used for additive
	// maps where different keys may come from different layers. Example:
	//   FieldLayer["option_defaults"] is unset;
	//   MapKeyLayer["option_defaults"]["permission_mode"] = "builtin:codex"
	//   MapKeyLayer["option_defaults"]["effort"] = "providers.codex-max"
	MapKeyLayer map[string]map[string]string
}

// clone returns a deep copy of the provenance so callers cannot mutate
// the cached value.
func (p ProviderProvenance) clone() ProviderProvenance {
	out := ProviderProvenance{}
	if p.Chain != nil {
		out.Chain = append([]HopIdentity(nil), p.Chain...)
	}
	if p.FieldLayer != nil {
		out.FieldLayer = make(map[string]string, len(p.FieldLayer))
		for k, v := range p.FieldLayer {
			out.FieldLayer[k] = v
		}
	}
	if p.MapKeyLayer != nil {
		out.MapKeyLayer = make(map[string]map[string]string, len(p.MapKeyLayer))
		for k, inner := range p.MapKeyLayer {
			cp := make(map[string]string, len(inner))
			for k2, v := range inner {
				cp[k2] = v
			}
			out.MapKeyLayer[k] = cp
		}
	}
	return out
}

// provenanceSource returns the canonical layer label for a hop. Mirrors
// the formatHopName helper used in error messages but returns the form
// most useful for provenance consumers.
func provenanceSource(id HopIdentity) string {
	if id.Kind == "builtin" {
		return BasePrefixBuiltin + id.Name
	}
	return "providers." + id.Name
}
