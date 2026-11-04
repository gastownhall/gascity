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

// The default native Codex now has fixed isolation argv. It is not a startup
// prompt and must retain authority through the actual catalog resolver, while
// arbitrary or inconsistent argument shapes remain unsupported.
func TestFreshBuiltinCommandFamilyAcceptsOnlyExactBuiltinIsolationArgs(t *testing.T) {
	base := BasePrefixBuiltin + "codex"
	for _, test := range []struct {
		name string
		spec ProviderSpec
		want string
	}{
		{name: "inherited isolation", spec: ProviderSpec{Base: &base}, want: "codex"},
		{name: "typed options", spec: ProviderSpec{Base: &base, OptionDefaults: map[string]string{"model": "gpt-6.1-sol", "effort": "medium", "permission_mode": "unrestricted"}}, want: "codex"},
		{name: "extra input", spec: ProviderSpec{Base: &base, ArgsAppend: []string{"initial user task"}}},
		{name: "duplicated isolation", spec: ProviderSpec{Base: &base, ArgsAppend: []string{"--no-daemon"}}},
		{name: "cleared arguments", spec: ProviderSpec{Base: &base, Args: []string{}}},
		{name: "replacement input", spec: ProviderSpec{Base: &base, Args: []string{"initial user task"}}},
		{name: "explicit same command", spec: ProviderSpec{Base: &base, Command: "codex"}},
		{name: "wrapper", spec: ProviderSpec{Base: &base, Command: "opaque-launcher", ResumeCommand: "opaque-launcher resume {{.SessionKey}}"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			city := &City{Providers: map[string]ProviderSpec{"alias": test.spec}}
			resolved, err := ResolveProvider(&Agent{Provider: "alias"}, &city.Workspace, city.Providers, func(name string) (string, error) { return "/qualified/bin/" + name, nil })
			if err != nil {
				t.Fatal(err)
			}
			if got := FreshBuiltinCommandFamily(city, resolved); got != test.want {
				t.Fatalf("family = %q, want %q; actual catalog args=%v", got, test.want, resolved.Args)
			}
		})
	}
}

func TestFreshBuiltinCommandFamilyRefusesResolvedAndDeclaredArgvDrift(t *testing.T) {
	base := BasePrefixBuiltin + "codex"
	city := &City{Providers: map[string]ProviderSpec{"alias": {Base: &base}}}
	resolved, err := ResolveProvider(&Agent{Provider: "alias"}, &city.Workspace, city.Providers, func(name string) (string, error) { return "/qualified/bin/" + name, nil })
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{nil, {"--no-daemon", "initial user task"}, {"--no-daemon", "--no-daemon"}} {
		copyProvider := *resolved
		copyProvider.Args = args
		if got := FreshBuiltinCommandFamily(city, &copyProvider); got != "" {
			t.Fatalf("mutated effective argv granted authority: args=%v family=%q", args, got)
		}
	}
	// A plausible resolved argv cannot override an independently declared
	// startup prompt or a cleared isolation policy in the owned city catalog.
	for _, args := range [][]string{{}, {"initial user task"}} {
		otherCity := &City{Providers: map[string]ProviderSpec{"alias": {Base: &base, Args: args}}}
		if got := FreshBuiltinCommandFamily(otherCity, resolved); got != "" {
			t.Fatalf("resolved/declared argv mismatch granted authority: declared=%v family=%q", args, got)
		}
	}
}
