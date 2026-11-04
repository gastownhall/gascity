package config

import "slices"

// FreshBuiltinCommandFamily proves only the configured launch command's
// provenance, never that a session or runtime is fresh. The fresh-create caller
// must separately exclude startup input, resume, reset and prior incarnations.
// Schema-resolved options may decorate the launch; opaque command/argv escapes
// cannot acquire this provenance.
func FreshBuiltinCommandFamily(city *City, resolved *ResolvedProvider) string {
	if city == nil || resolved == nil || city.Workspace.StartCommand != "" {
		return ""
	}
	family := resolved.BuiltinAncestor
	if family != "claude" && family != "codex" {
		return ""
	}
	// Only fixed arguments from the owned builtin catalog may accompany a
	// fresh launch. Selectable schema options are resolved separately; an
	// empty Codex override or any extra argv cannot gain bootstrap authority.
	builtin := BuiltinProviders()[family]
	if !slices.Equal(resolved.Args, builtin.Args) {
		return ""
	}
	spec, ok := city.Providers[resolved.Name]
	if !ok {
		return ""
	}
	declared, err := ResolveProviderChain(resolved.Name, spec, city.Providers)
	if err != nil || declared.Provenance.FieldLayer["command"] != BasePrefixBuiltin+family || resolved.Command != declared.Command || !slices.Equal(declared.Args, builtin.Args) {
		return ""
	}
	return family
}
