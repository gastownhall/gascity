package config

// acpOmittedArgs returns the provider-layer args an ACP launch would drop:
// the args the layers strictly more specific than the leaf-most layer that
// sets acp_args contribute to the merged Args. layers are the
// schema-normalized specs, as the merge sees them, ordered leaf (index 0) to
// root.
//
// The rules:
//   - No layer sets acp_args → nil: the ACP launch then falls back to Args
//     (ACPCommandString), so nothing is dropped.
//   - The contribution replays the merge (MergeProviderOverBuiltin) over
//     those layers, root to leaf: args replaces, args_append extends. An
//     args_append a more specific args replaces reaches neither launch.
//   - Args at or below the acp_args layer, the same layer included, are not
//     omitted: the acp_args author overrides them knowingly.
//   - Schema-managed flags are not omitted: normalization lifts them into
//     option defaults, which both launches render (ProviderTemplateCommand),
//     so args = ["-m", "<curated model>"] resolves the transport its
//     option_defaults spelling does.
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
	for i := acpLayer - 1; i >= 0; i-- {
		if layers[i].Args != nil {
			omitted = append([]string(nil), layers[i].Args...)
		}
		omitted = append(omitted, layers[i].ArgsAppend...)
	}
	return omitted
}

// ACPDroppedArgs returns the declared args the ACP launch of rp drops for
// agent, the agent ResolveProvider resolved rp for: the args the CLI launch
// carries that the ACP launch, which takes its args from ACPArgs alone, does
// not. An agent's args replace the provider's on the CLI launch, as Args
// holds them normalized (mergeAgentOverrides), so they are what an agent
// that declares args drops; otherwise ACPOmittedArgs is. Schema-managed
// flags are never dropped: both launches carry them as option defaults. Nil
// when nothing is dropped, and when ACPArgs is nil, because the ACP launch
// then uses Args.
func (rp *ResolvedProvider) ACPDroppedArgs(agent *Agent) []string {
	if rp == nil || rp.ACPArgs == nil {
		return nil
	}
	dropped := rp.ACPOmittedArgs
	if agent != nil && len(agent.Args) > 0 {
		dropped = rp.Args
	}
	if len(dropped) == 0 {
		return nil
	}
	return append([]string(nil), dropped...)
}
