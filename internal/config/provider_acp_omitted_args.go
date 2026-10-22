package config

// acpOmittedArgs returns the raw provider-layer args an ACP launch would
// drop: the args and args_append tokens declared by layers strictly more
// specific than the leaf-most layer that sets acp_args. layers are the raw,
// as-authored specs ordered leaf (index 0) to root.
//
// The rules:
//   - No layer sets acp_args → nil: the ACP launch then falls back to Args
//     (ACPCommandString), so nothing is dropped.
//   - Same-layer args and acp_args are never omitted: an author declaring
//     both on one layer composed them deliberately.
//   - Args at or below the acp_args layer are not omitted either: the
//     acp_args layer overrides them knowingly.
//   - Raw tokens, not schema-normalized ones: normalizeProviderLayerArgsForSchema
//     lifts a curated model flag into option_defaults and strips it from
//     Args, so normalized tokens would hide the drop the ACP launch performs.
//
// Token order: the leaf-most qualifying args first, then qualifying
// args_append from root to leaf.
func acpOmittedArgs(layers []ProviderSpec) []string {
	acpLayer := -1
	for i := range layers {
		if layers[i].ACPArgs != nil {
			acpLayer = i
			break
		}
	}
	if acpLayer < 0 {
		return nil
	}
	var omitted []string
	for i := 0; i < acpLayer; i++ {
		if layers[i].Args != nil {
			omitted = append(omitted, layers[i].Args...)
			break
		}
	}
	for i := acpLayer - 1; i >= 0; i-- {
		if layers[i].ArgsAppend != nil {
			omitted = append(omitted, layers[i].ArgsAppend...)
		}
	}
	return omitted
}
