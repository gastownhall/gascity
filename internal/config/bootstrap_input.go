package config

// FreshBuiltinCommandFamily proves only the configured launch command's
// provenance, never that a session or runtime is fresh. The fresh-create caller
// must separately exclude startup input, resume, reset and prior incarnations.
// Schema-resolved options may decorate the launch; opaque command/argv escapes
// cannot acquire this provenance.
func FreshBuiltinCommandFamily(city *City, resolved *ResolvedProvider) string {
	if city == nil || resolved == nil || city.Workspace.StartCommand != "" || len(resolved.Args) != 0 {
		return ""
	}
	family := resolved.BuiltinAncestor
	if family != "claude" && family != "codex" {
		return ""
	}
	spec, ok := city.Providers[resolved.Name]
	if !ok {
		return ""
	}
	declared, err := ResolveProviderChain(resolved.Name, spec, city.Providers)
	if err != nil || declared.Provenance.FieldLayer["command"] != BasePrefixBuiltin+family || resolved.Command != declared.Command {
		return ""
	}
	return family
}
