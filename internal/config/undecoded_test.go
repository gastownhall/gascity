package config

import (
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

func TestCheckUndecodedKeysDetectsTypo(t *testing.T) { //nolint:misspell // intentional typos in test data
	typo := "prompt_" + "tempalte" //nolint:misspell // intentional typo under test
	input := "[workspace]\nname = \"test\"\n\n[[agent]]\nname = \"mayor\"\n" + typo + " = \"prompts/mayor.md\"\n"
	var cfg City
	md, err := toml.Decode(input, &cfg)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}

	warnings := CheckUndecodedKeys(md, "city.toml")
	if len(warnings) == 0 {
		t.Fatalf("expected warning for typo %s, got none", typo)
	}
	found := false
	for _, w := range warnings {
		if strings.Contains(w, typo) {
			found = true
			if !strings.Contains(w, "prompt_template") {
				t.Errorf("warning should suggest prompt_template, got: %s", w)
			}
		}
	}
	if !found {
		t.Errorf("no warning about %s in: %v", typo, warnings)
	}
}

func TestCheckUndecodedKeysNoWarningsForValidConfig(t *testing.T) {
	input := `
[workspace]
name = "test"

[[agent]]
name = "mayor"
prompt_template = "prompts/mayor.md"
`
	var cfg City
	md, err := toml.Decode(input, &cfg)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}

	warnings := CheckUndecodedKeys(md, "city.toml")
	if len(warnings) != 0 {
		t.Errorf("expected no warnings for valid config, got: %v", warnings)
	}
}

func TestCheckUndecodedKeysMultipleTypos(t *testing.T) {
	input := `
[workspace]
name = "test"

[[agent]]
name = "mayor"
promtp_template = "bad"
idel_timeout = "5m"
`
	var cfg City
	md, err := toml.Decode(input, &cfg)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}

	warnings := CheckUndecodedKeys(md, "test.toml")
	if len(warnings) < 2 {
		t.Fatalf("expected at least 2 warnings, got %d: %v", len(warnings), warnings)
	}
}

func TestCheckUndecodedKeysIncludesSource(t *testing.T) {
	input := `
[workspace]
name = "test"
bogus_field = true
`
	var cfg City
	md, err := toml.Decode(input, &cfg)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}

	warnings := CheckUndecodedKeys(md, "/path/to/city.toml")
	if len(warnings) == 0 {
		t.Fatal("expected warning for bogus_field")
	}
	if !strings.Contains(warnings[0], "/path/to/city.toml") {
		t.Errorf("warning should include source path, got: %s", warnings[0])
	}
}

func TestCheckUndecodedKeysNoSuggestionForDistantTypo(t *testing.T) {
	input := `
[workspace]
name = "test"
completely_unknown_field_xyz = "val"
`
	var cfg City
	md, err := toml.Decode(input, &cfg)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}

	warnings := CheckUndecodedKeys(md, "city.toml")
	if len(warnings) == 0 {
		t.Fatal("expected warning")
	}
	if strings.Contains(warnings[0], "did you mean") {
		t.Errorf("should not suggest for very distant key, got: %s", warnings[0])
	}
}

func TestEditDistance(t *testing.T) {
	tests := []struct {
		a, b string
		want int
	}{
		{"", "", 0},
		{"abc", "abc", 0},
		{"abc", "abd", 1},
		{"abc", "abcd", 1},
		{"prompt_tempalte", "prompt_template", 2}, //nolint:misspell // intentional typo
		{"idle_timeout", "idel_timeout", 2},
		{"xyz", "abc", 3},
	}
	for _, tt := range tests {
		got := editDistance(tt.a, tt.b)
		if got != tt.want {
			t.Errorf("editDistance(%q, %q) = %d, want %d", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestKnownTOMLKeysNotEmpty(t *testing.T) {
	keys := knownTOMLKeys()
	if len(keys) == 0 {
		t.Fatal("knownTOMLKeys returned empty list")
	}
	// Spot-check a few known keys.
	keySet := make(map[string]bool)
	for _, k := range keys {
		keySet[k] = true
	}
	for _, want := range []string{"name", "prompt_template", "provider", "idle_timeout", "pool"} {
		if !keySet[want] {
			t.Errorf("expected %q in known keys", want)
		}
	}
}

func TestParseWithMetaWarnings(t *testing.T) {
	input := `
[workspace]
name = "test"

[[agent]]
name = "mayor"
proivder = "claude"
`
	cfg, _, warnings, err := parseWithMeta([]byte(input), "test.toml")
	if err != nil {
		t.Fatalf("parseWithMeta: %v", err)
	}
	if cfg.Workspace.Name != "test" {
		t.Errorf("Name = %q, want %q", cfg.Workspace.Name, "test")
	}
	if len(warnings) == 0 {
		t.Fatal("expected warning for proivder typo")
	}
	if !strings.Contains(warnings[0], "proivder") {
		t.Errorf("warning should mention proivder, got: %s", warnings[0])
	}
	if !strings.Contains(warnings[0], "provider") {
		t.Errorf("warning should suggest provider, got: %s", warnings[0])
	}
}

func TestParseWithMetaNoWarningsForLegacyOrderGateAlias(t *testing.T) {
	input := `
[workspace]
name = "test"

[orders]

[[orders.overrides]]
name = "digest"
gate = "cooldown"
`
	cfg, _, warnings, err := parseWithMeta([]byte(input), "test.toml")
	if err != nil {
		t.Fatalf("parseWithMeta: %v", err)
	}
	if len(cfg.Orders.Overrides) != 1 {
		t.Fatalf("len(overrides) = %d, want 1", len(cfg.Orders.Overrides))
	}
	if cfg.Orders.Overrides[0].Trigger == nil || *cfg.Orders.Overrides[0].Trigger != "cooldown" {
		t.Fatalf("Trigger = %#v, want cooldown", cfg.Orders.Overrides[0].Trigger)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v, want none", warnings)
	}
}

func TestParseWithMetaWarnsOnAgentsAlias(t *testing.T) {
	input := `
[workspace]
name = "test"

[agents]
append_fragments = ["footer"]
`
	_, _, warnings, err := parseWithMeta([]byte(input), "test.toml")
	if err != nil {
		t.Fatalf("parseWithMeta: %v", err)
	}
	if len(warnings) == 0 {
		t.Fatal("expected warning for [agents] alias")
	}
	found := false
	for _, w := range warnings {
		if strings.Contains(w, agentsAliasWarning) {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected warning containing %q, got: %v", agentsAliasWarning, warnings)
	}
}

func TestParseWithMetaWarnsWhenCanonicalAndAliasAgentDefaultsBothPresent(t *testing.T) {
	input := `
[workspace]
name = "test"

[agent_defaults]
append_fragments = ["canonical"]

[agents]
append_fragments = ["legacy"]
`
	_, _, warnings, err := parseWithMeta([]byte(input), "test.toml")
	if err != nil {
		t.Fatalf("parseWithMeta: %v", err)
	}
	found := false
	for _, w := range warnings {
		if strings.Contains(w, "both [agent_defaults] and [agents] are present") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected mixed-table warning, got: %v", warnings)
	}
}

func TestParseWithMetaSkipsMixedTableWarningWhenCanonicalAndAliasAreDisjoint(t *testing.T) {
	input := `
[workspace]
name = "test"

[agent_defaults]
append_fragments = ["canonical"]

[agents]
allow_overlay = ["GC_HOME"]
`
	_, _, warnings, err := parseWithMeta([]byte(input), "test.toml")
	if err != nil {
		t.Fatalf("parseWithMeta: %v", err)
	}
	for _, w := range warnings {
		if strings.Contains(w, "both [agent_defaults] and [agents] are present") {
			t.Fatalf("expected no mixed-table warning for disjoint keys, got: %v", warnings)
		}
	}
	foundAlias := false
	for _, w := range warnings {
		if strings.Contains(w, agentsAliasWarning) {
			foundAlias = true
			break
		}
	}
	if !foundAlias {
		t.Fatalf("expected alias warning, got: %v", warnings)
	}
}

func TestParseWithMetaSkipsMixedTableWarningWhenOverlapIsOnlyUnsupportedFutureKeys(t *testing.T) {
	input := `
[workspace]
name = "test"

[agent_defaults]
provider = "claude"

[agents]
provider = "codex"
`
	_, _, warnings, err := parseWithMeta([]byte(input), "test.toml")
	if err != nil {
		t.Fatalf("parseWithMeta: %v", err)
	}
	for _, w := range warnings {
		if strings.Contains(w, "both [agent_defaults] and [agents] are present") {
			t.Fatalf("expected no mixed-table warning for unsupported future keys, got: %v", warnings)
		}
	}
	foundUnsupported := false
	foundAlias := false
	for _, w := range warnings {
		if strings.Contains(w, `keep using workspace.provider`) {
			foundUnsupported = true
		}
		if strings.Contains(w, agentsAliasWarning) {
			foundAlias = true
		}
	}
	if !foundUnsupported {
		t.Fatalf("expected unsupported-key guidance warning, got: %v", warnings)
	}
	if !foundAlias {
		t.Fatalf("expected alias warning, got: %v", warnings)
	}
}

func TestParseWithMetaWarnsOnUnsupportedAgentDefaultsMigrationKeys(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name: "provider",
			input: `
[workspace]
name = "test"

[agent_defaults]
provider = "claude"
`,
			want: `keep using workspace.provider`,
		},
		{
			name: "scope",
			input: `
[workspace]
name = "test"

[agent_defaults]
scope = "rig"
`,
			want: `keep setting scope per agent in agents/<name>/agent.toml`,
		},
		{
			name: "install_agent_hooks",
			input: `
[workspace]
name = "test"

[agent_defaults]
install_agent_hooks = ["hooks/gascity.json"]
`,
			want: `keep using workspace.install_agent_hooks`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, warnings, err := parseWithMeta([]byte(tt.input), "test.toml")
			if err != nil {
				t.Fatalf("parseWithMeta: %v", err)
			}
			if len(warnings) == 0 {
				t.Fatal("expected warning")
			}
			found := false
			for _, w := range warnings {
				if strings.Contains(w, tt.want) {
					found = true
				}
				if strings.Contains(w, "unknown field") {
					t.Fatalf("got generic unknown-field warning for %s: %v", tt.name, warnings)
				}
			}
			if !found {
				t.Fatalf("expected warning containing %q, got: %v", tt.want, warnings)
			}
		})
	}
}

func TestParsePackConfigWithMetaWarnsOnPackLocalUnsupportedAgentDefaultsKeys(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name: "provider",
			input: `
[pack]
name = "test"
schema = 2

[agent_defaults]
provider = "claude"
`,
			want: `keep setting provider per agent in agents/<name>/agent.toml`,
		},
		{
			name: "install_agent_hooks",
			input: `
[pack]
name = "test"
schema = 2

[agent_defaults]
install_agent_hooks = ["hooks/gascity.json"]
`,
			want: `keep setting install_agent_hooks per agent in agents/<name>/agent.toml`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, warnings, err := parsePackConfigWithMeta([]byte(tt.input), "/city/packs/test/pack.toml")
			if err != nil {
				t.Fatalf("parsePackConfigWithMeta: %v", err)
			}
			if len(warnings) == 0 {
				t.Fatal("expected warning")
			}
			found := false
			for _, w := range warnings {
				if strings.Contains(w, tt.want) {
					found = true
				}
				if strings.Contains(w, "workspace.") {
					t.Fatalf("pack warning should not point at workspace.*, got: %v", warnings)
				}
			}
			if !found {
				t.Fatalf("expected warning containing %q, got: %v", tt.want, warnings)
			}
		})
	}
}

func TestParsePackConfigWithMetaAllowsKnownPackMetadata(t *testing.T) {
	input := `
[pack]
name = "core"
version = "0.1.0"
schema = 2
requires_gc = ">=0.14.0"
`

	cfg, warnings, err := parsePackConfigWithMeta([]byte(input), "/city/.gc/system/packs/core/pack.toml")
	if err != nil {
		t.Fatalf("parsePackConfigWithMeta: %v", err)
	}
	if cfg.Pack.Version != "0.1.0" {
		t.Fatalf("Pack.Version = %q, want %q", cfg.Pack.Version, "0.1.0")
	}
	if cfg.Pack.RequiresGC != ">=0.14.0" {
		t.Fatalf("Pack.RequiresGC = %q, want %q", cfg.Pack.RequiresGC, ">=0.14.0")
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v, want none", warnings)
	}
}

func TestParseWithMetaWarnsForUnknownOrderOverrideKey(t *testing.T) {
	input := `
[workspace]
name = "test"

[orders]

[[orders.overrides]]
name = "digest"
triger = "cooldown"
`
	cfg, _, warnings, err := parseWithMeta([]byte(input), "test.toml")
	if err != nil {
		t.Fatalf("parseWithMeta: %v", err)
	}
	if len(cfg.Orders.Overrides) != 1 {
		t.Fatalf("len(overrides) = %d, want 1", len(cfg.Orders.Overrides))
	}
	if len(warnings) == 0 {
		t.Fatal("warnings = nil, want unknown-key warning")
	}
	if !strings.Contains(warnings[0], "triger") {
		t.Fatalf("warning = %q, want triger key", warnings[0])
	}
}
