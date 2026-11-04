package config

import "testing"

func TestFreshBuiltinCommandFamilyUsesCatalogProvenanceNotSchemaOptions(t *testing.T) {
	for _, family := range []string{"claude", "codex"} {
		t.Run(family, func(t *testing.T) {
			base := BasePrefixBuiltin + family
			city := &City{Providers: map[string]ProviderSpec{"alias": {Base: &base}}}
			resolved, err := ResolveProvider(&Agent{Provider: "alias"}, &city.Workspace, city.Providers, func(s string) (string, error) { return "/qualified/bin/" + s, nil })
			if err != nil {
				t.Fatal(err)
			}
			if got := FreshBuiltinCommandFamily(city, resolved); got != family {
				t.Fatalf("inherited builtin = %q, want %q", got, family)
			}
			if _, _, err := ResolveOptions(resolved.OptionsSchema, map[string]string{"effort": "medium", "permission_mode": "unrestricted"}, resolved.EffectiveDefaults); err != nil {
				t.Fatal(err)
			}
			if got := FreshBuiltinCommandFamily(city, resolved); got != family {
				t.Fatalf("schema settings lost provenance: %q", got)
			}
		})
	}
}

func TestFreshBuiltinCommandFamilyRefusesOpaqueAndUnprovenCommands(t *testing.T) {
	base := BasePrefixBuiltin + "codex"
	for name, spec := range map[string]ProviderSpec{
		"opaque command": {Base: &base, Command: "opaque-launcher", ResumeCommand: "opaque-launcher resume {{.SessionKey}}"},
		"opaque args":    {Base: &base, Args: []string{"initial user task"}},
	} {
		t.Run(name, func(t *testing.T) {
			city := &City{Providers: map[string]ProviderSpec{"alias": spec}}
			resolved, err := ResolveProvider(&Agent{Provider: "alias"}, &city.Workspace, city.Providers, func(s string) (string, error) { return "/qualified/bin/" + s, nil })
			if err != nil {
				t.Fatal(err)
			}
			if got := FreshBuiltinCommandFamily(city, resolved); got != "" {
				t.Fatalf("opaque configuration granted family %q", got)
			}
		})
	}
	city := &City{Providers: map[string]ProviderSpec{"alias": {Base: &base}}}
	resolved, err := ResolveProvider(&Agent{Provider: "alias"}, &city.Workspace, city.Providers, func(s string) (string, error) { return "/qualified/bin/" + s, nil })
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"nil city", "nil provider", "workspace command", "different command", "missing catalog"} {
		t.Run(mode, func(t *testing.T) {
			copyCity := *city
			copyProvider := *resolved
			c, p := &copyCity, &copyProvider
			switch mode {
			case "nil city":
				c = nil
			case "nil provider":
				p = nil
			case "workspace command":
				c.Workspace.StartCommand = "codex"
			case "different command":
				p.Command = "codex initial task"
			case "missing catalog":
				c.Providers = nil
			}
			if got := FreshBuiltinCommandFamily(c, p); got != "" {
				t.Fatalf("unproven command granted family %q", got)
			}
		})
	}
}
