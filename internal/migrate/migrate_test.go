package migrate

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

// TestDropRedundantControlDispatcherNamedSession pins the stale-city cleanup:
// gc doctor --fix (via migrate.Apply) drops the auto-created control-dispatcher
// named session (bare or core-qualified backing template) that older gc init
// versions injected, leaves user-defined named sessions untouched, and is
// idempotent.
func TestDropRedundantControlDispatcherNamedSession(t *testing.T) {
	t.Parallel()
	in := []config.NamedSession{
		{Name: "control-dispatcher", Template: "control-dispatcher", Mode: "on_demand"},      // pre-1.3 bare (the stale case)
		{Name: "mayor", Template: "mayor", Mode: "always"},                                   // user-defined: keep
		{Name: "control-dispatcher", Template: "core.control-dispatcher", Mode: "on_demand"}, // rc1/rc2 qualified
	}
	out, removed := dropRedundantControlDispatcherNamedSession(in)
	if removed != 2 {
		t.Fatalf("removed = %d, want 2", removed)
	}
	if len(out) != 1 || out[0].Name != "mayor" {
		t.Fatalf("remaining = %+v, want only the user-defined mayor session", out)
	}
	// Idempotent: a second pass removes nothing.
	if out2, removed2 := dropRedundantControlDispatcherNamedSession(out); removed2 != 0 || len(out2) != 1 {
		t.Fatalf("second pass removed=%d len=%d, want 0 removed / 1 remaining", removed2, len(out2))
	}
	// User-authored control-dispatcher sessions express intent and must be
	// left alone: a non-core template, an always mode, or an explicit
	// scope/dir all disqualify the auto-created match.
	keep := []config.NamedSession{
		{Name: "control-dispatcher", Template: "myrig/custom-dispatcher", Mode: "always"},                  // custom template
		{Name: "control-dispatcher", Template: "core.control-dispatcher", Mode: "always"},                  // always mode
		{Name: "control-dispatcher", Template: "core.control-dispatcher", Mode: "on_demand", Scope: "rig"}, // explicit scope
	}
	if out, n := dropRedundantControlDispatcherNamedSession(keep); n != 0 || len(out) != len(keep) {
		t.Fatalf("dropped a user-authored control-dispatcher session (removed=%d); all must be kept", n)
	}
}

func TestMigrateCityCommonCase(t *testing.T) {
	t.Parallel()

	cityDir := t.TempDir()
	writeFile(t, cityDir, "city.toml", `
[workspace]
name = "legacy-city"
includes = ["../packs/gastown", "../packs/qa-team"]
default_rig_includes = ["../packs/z-pack", "../packs/a-pack"]

[[agent]]
name = "mayor"
scope = "city"
provider = "claude"
prompt_template = "prompts/mayor.md"
overlay_dir = "overlays/mayor"
namepool = "namepools/mayor.txt"

[[agent]]
name = "worker"
prompt_template = "prompts/worker.md"
`)
	writeFile(t, cityDir, "prompts/mayor.md", "You are {{.Agent}}.\n")
	writeFile(t, cityDir, "prompts/worker.md", "You are a worker.\n")
	writeFile(t, cityDir, "overlays/mayor/CLAUDE.md", "city overlay\n")
	writeFile(t, cityDir, "namepools/mayor.txt", "Ada\nGrace\n")

	if _, err := Apply(cityDir, Options{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	packToml := readFile(t, filepath.Join(cityDir, "pack.toml"))
	if !strings.Contains(packToml, "[pack]") {
		t.Fatalf("pack.toml missing [pack]:\n%s", packToml)
	}
	if !strings.Contains(packToml, "name = \"legacy-city\"") {
		t.Fatalf("pack.toml missing derived pack name:\n%s", packToml)
	}
	if !strings.Contains(packToml, "[imports.gastown]") {
		t.Fatalf("pack.toml missing gastown import:\n%s", packToml)
	}
	if !strings.Contains(packToml, "source = \"../packs/gastown\"") {
		t.Fatalf("pack.toml missing gastown source:\n%s", packToml)
	}
	cityToml := readFile(t, filepath.Join(cityDir, "city.toml"))
	for _, line := range []string{
		"[defaults.rig.imports.a-pack]",
		"source = \"../packs/a-pack\"",
		"[defaults.rig.imports.z-pack]",
		"source = \"../packs/z-pack\"",
	} {
		if !strings.Contains(cityToml, line) {
			t.Fatalf("city.toml missing migrated default-rig imports %q:\n%s", line, cityToml)
		}
	}
	if strings.Contains(cityToml, "[[agent]]") {
		t.Fatalf("city.toml still contains [[agent]]:\n%s", cityToml)
	}
	if strings.Contains(cityToml, "\nincludes =") {
		t.Fatalf("city.toml still contains workspace.includes:\n%s", cityToml)
	}
	if strings.Contains(cityToml, "default_rig_includes") {
		t.Fatalf("city.toml should drop legacy workspace.default_rig_includes:\n%s", cityToml)
	}

	mayorAgentToml := readFile(t, filepath.Join(cityDir, "agents", "mayor", "agent.toml"))
	if !strings.Contains(mayorAgentToml, "scope = \"city\"") {
		t.Fatalf("mayor agent.toml missing scope:\n%s", mayorAgentToml)
	}
	if !strings.Contains(mayorAgentToml, "provider = \"claude\"") {
		t.Fatalf("mayor agent.toml missing provider:\n%s", mayorAgentToml)
	}
	if strings.Contains(mayorAgentToml, "prompt_template") {
		t.Fatalf("mayor agent.toml still contains prompt_template:\n%s", mayorAgentToml)
	}
	if strings.Contains(mayorAgentToml, "overlay_dir") {
		t.Fatalf("mayor agent.toml still contains overlay_dir:\n%s", mayorAgentToml)
	}
	if strings.Contains(mayorAgentToml, "namepool") {
		t.Fatalf("mayor agent.toml still contains namepool:\n%s", mayorAgentToml)
	}
	if strings.Contains(mayorAgentToml, "fallback") {
		t.Fatalf("mayor agent.toml still contains fallback:\n%s", mayorAgentToml)
	}

	if got := readFile(t, filepath.Join(cityDir, "agents", "mayor", "prompt.template.md")); !strings.Contains(got, "{{.Agent}}") {
		t.Fatalf("expected templated mayor prompt to be moved, got:\n%s", got)
	}
	if got := readFile(t, filepath.Join(cityDir, "agents", "worker", "prompt.md")); !strings.Contains(got, "worker") {
		t.Fatalf("expected plain worker prompt to be moved, got:\n%s", got)
	}
	if got := readFile(t, filepath.Join(cityDir, "agents", "mayor", "overlay", "CLAUDE.md")); !strings.Contains(got, "city overlay") {
		t.Fatalf("expected overlay to move, got:\n%s", got)
	}
	if got := readFile(t, filepath.Join(cityDir, "agents", "mayor", "namepool.txt")); !strings.Contains(got, "Ada") {
		t.Fatalf("expected namepool to move, got:\n%s", got)
	}

	if _, err := os.Stat(filepath.Join(cityDir, "prompts", "mayor.md")); !os.IsNotExist(err) {
		t.Fatalf("expected mayor prompt source to be removed, err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(cityDir, "overlays", "mayor")); !os.IsNotExist(err) {
		t.Fatalf("expected overlay source to be removed, err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(cityDir, "namepools", "mayor.txt")); !os.IsNotExist(err) {
		t.Fatalf("expected namepool source to be removed, err=%v", err)
	}
}

func TestMigrateDefaultRigIncludesLoadAfterMigration(t *testing.T) {
	t.Parallel()

	cityDir := t.TempDir()
	writeFile(t, cityDir, "city.toml", `
[workspace]
name = "legacy-city"
default_rig_includes = ["../packs/z-pack", "../packs/a-pack"]
`)

	if _, err := Apply(cityDir, Options{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	cityToml := readFile(t, filepath.Join(cityDir, "city.toml"))
	for _, line := range []string{
		"[defaults.rig.imports.a-pack]",
		`source = "../packs/a-pack"`,
		"[defaults.rig.imports.z-pack]",
		`source = "../packs/z-pack"`,
	} {
		if !strings.Contains(cityToml, line) {
			t.Fatalf("city.toml missing migrated default-rig imports %q:\n%s", line, cityToml)
		}
	}
	if strings.Contains(cityToml, "default_rig_includes") {
		t.Fatalf("city.toml should drop legacy workspace.default_rig_includes:\n%s", cityToml)
	}

	cfg, _, err := config.LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityDir, "city.toml"))
	if err != nil {
		t.Fatalf("LoadWithIncludes after migration: %v", err)
	}
	if len(cfg.DefaultRigImports) != 2 {
		t.Fatalf("len(DefaultRigImports) = %d, want 2", len(cfg.DefaultRigImports))
	}
	if cfg.DefaultRigImports["z-pack"].Source != "../packs/z-pack" {
		t.Fatalf("DefaultRigImports[z-pack].Source = %q, want %q", cfg.DefaultRigImports["z-pack"].Source, "../packs/z-pack")
	}
	if cfg.DefaultRigImports["a-pack"].Source != "../packs/a-pack" {
		t.Fatalf("DefaultRigImports[a-pack].Source = %q, want %q", cfg.DefaultRigImports["a-pack"].Source, "../packs/a-pack")
	}
}

func TestMigrateRemovesPacksAfterMigratingNamedIncludes(t *testing.T) {
	t.Parallel()

	cityDir := t.TempDir()
	writeFile(t, cityDir, "city.toml", `
[workspace]
name = "legacy-city"
includes = ["team"]
default_rig_includes = ["ops"]

[packs.team]
source = "https://example.com/team.git"
path = "pack"
ref = "v1"

[packs.ops]
source = "../packs/ops"
`)

	if _, err := Apply(cityDir, Options{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	packToml := readFile(t, filepath.Join(cityDir, "pack.toml"))
	for _, want := range []string{
		"[imports.team]",
		`source = "https://example.com/team.git//pack#v1"`,
	} {
		if !strings.Contains(packToml, want) {
			t.Fatalf("pack.toml missing %q after named pack migration:\n%s", want, packToml)
		}
	}

	cityToml := readFile(t, filepath.Join(cityDir, "city.toml"))
	for _, want := range []string{
		"[defaults.rig.imports.ops]",
		`source = "../packs/ops"`,
	} {
		if !strings.Contains(cityToml, want) {
			t.Fatalf("city.toml missing %q after named pack migration:\n%s", want, cityToml)
		}
	}

	if strings.Contains(cityToml, "[packs.") || strings.Contains(cityToml, "[packs]") {
		t.Fatalf("city.toml still contains migrated [packs] entries:\n%s", cityToml)
	}
}

func TestMigrateKeepsPacksStillReferencedByRigIncludes(t *testing.T) {
	t.Parallel()

	cityDir := t.TempDir()
	writeFile(t, cityDir, "city.toml", `
[workspace]
name = "legacy-city"
includes = ["team"]

[[rigs]]
name = "app"
path = "app"
includes = ["team"]

[packs.team]
source = "../packs/team"
`)

	if _, err := Apply(cityDir, Options{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	cityToml := readFile(t, filepath.Join(cityDir, "city.toml"))
	if !strings.Contains(cityToml, "[packs.team]") {
		t.Fatalf("city.toml removed pack still referenced by rig includes:\n%s", cityToml)
	}
	cfg, err := config.Load(fsys.OSFS{}, filepath.Join(cityDir, "city.toml"))
	if err != nil {
		t.Fatalf("Load migrated city: %v", err)
	}
	if len(cfg.Workspace.LegacyIncludes()) != 0 {
		t.Fatalf("workspace legacy includes = %v, want none", cfg.Workspace.LegacyIncludes())
	}
}

func TestMigrateUsesSiteBoundWorkspaceNameForPackFallback(t *testing.T) {
	t.Parallel()

	cityDir := t.TempDir()
	writeFile(t, cityDir, "city.toml", `
	[workspace]
	includes = ["../packs/gastown"]
	`)
	writeFile(t, cityDir, ".gc/site.toml", `
	workspace_name = "site-city"
	`)

	if _, err := Apply(cityDir, Options{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	packToml := readFile(t, filepath.Join(cityDir, "pack.toml"))
	if !strings.Contains(packToml, "name = \"site-city\"") {
		t.Fatalf("pack.toml missing site-bound pack name fallback:\n%s", packToml)
	}
}

func TestMigrateRejectsUnknownExistingPackTomlFields(t *testing.T) {
	t.Parallel()

	cityDir := t.TempDir()
	writeFile(t, cityDir, "city.toml", `
[workspace]
name = "legacy-city"
includes = ["../packs/gastown"]
`)
	writeFile(t, cityDir, "pack.toml", `
[pack]
name = "legacy-city"
schema = 2
legacy_unknown = "silently dropped before strict migration validation"
`)

	beforeCity := readFile(t, filepath.Join(cityDir, "city.toml"))
	beforePack := readFile(t, filepath.Join(cityDir, "pack.toml"))

	_, err := Apply(cityDir, Options{})
	if err == nil {
		t.Fatal("expected Apply to reject unknown pack.toml field")
	}
	if !strings.Contains(err.Error(), `unknown field "pack.legacy_unknown"`) {
		t.Fatalf("error = %v, want unknown field detail for pack.legacy_unknown", err)
	}
	if got := readFile(t, filepath.Join(cityDir, "city.toml")); got != beforeCity {
		t.Fatalf("city.toml changed after validation failure:\n%s", got)
	}
	if got := readFile(t, filepath.Join(cityDir, "pack.toml")); got != beforePack {
		t.Fatalf("pack.toml changed after validation failure:\n%s", got)
	}
}

func TestMigrateRejectsUnknownCityTomlKeys(t *testing.T) {
	t.Parallel()

	cityDir := t.TempDir()
	writeFile(t, cityDir, "city.toml", `
[workspace]
name = "legacy-city"
includes = ["../packs/gastown"]

[agent_defaults]
future_unknown = "written by a newer gc, silently dropped by this rewrite"
`)

	beforeCity := readFile(t, filepath.Join(cityDir, "city.toml"))

	_, err := Apply(cityDir, Options{})
	if err == nil {
		t.Fatal("expected Apply to refuse a city.toml with unknown keys")
	}
	if !strings.Contains(err.Error(), "agent_defaults.future_unknown") {
		t.Fatalf("error = %v, want the unknown key named", err)
	}
	if !strings.Contains(err.Error(), "refusing to rewrite") {
		t.Fatalf("error = %v, want key-loss refusal with remediation", err)
	}
	if got := readFile(t, filepath.Join(cityDir, "city.toml")); got != beforeCity {
		t.Fatalf("city.toml changed after refusal:\n%s", got)
	}
}

func TestMigrateMovesExistingRootDefaultRigImportsToCityToml(t *testing.T) {
	t.Parallel()

	cityDir := t.TempDir()
	writeFile(t, cityDir, "city.toml", `
[workspace]
name = "legacy-city"
includes = ["../packs/new-pack"]
`)
	writeFile(t, cityDir, "pack.toml", `
[pack]
name = "legacy-city"
schema = 2

[defaults.rig.imports.z-pack]
source = "../packs/z-pack"

[defaults.rig.imports.a-pack]
source = "../packs/a-pack"
`)

	if _, err := Apply(cityDir, Options{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	packToml := readFile(t, filepath.Join(cityDir, "pack.toml"))
	if !strings.Contains(packToml, "[imports.new-pack]") {
		t.Fatalf("pack.toml missing migrated workspace include:\n%s", packToml)
	}
	if strings.Contains(packToml, "[defaults.rig.imports.") {
		t.Fatalf("pack.toml should not retain default rig imports:\n%s", packToml)
	}
	cityToml := readFile(t, filepath.Join(cityDir, "city.toml"))
	for _, want := range []string{
		"[defaults.rig.imports.a-pack]",
		`source = "../packs/a-pack"`,
		"[defaults.rig.imports.z-pack]",
		`source = "../packs/z-pack"`,
	} {
		if !strings.Contains(cityToml, want) {
			t.Fatalf("city.toml missing migrated root default rig import %q:\n%s", want, cityToml)
		}
	}
}

func TestMigrateMovesPackV2RejectedSurfacesThenLoads(t *testing.T) {
	t.Parallel()

	cityDir := t.TempDir()
	writeFile(t, cityDir, "city.toml", `
[workspace]
name = "legacy-city"

[[rigs]]
name = "app"

[formulas]
dir = "city-formulas"

[providers.local]
command = "true"
`)
	writeFile(t, cityDir, "pack.toml", `
[pack]
name = "legacy-city"
schema = 2

[agent_defaults]
default_sling_formula = "mol-canonical"

[agents]
append_fragments = ["legacy-footer"]

[formulas]
dir = "legacy-formulas"

[defaults.rig.imports.ops]
source = "../packs/ops"

[[patches.rigs]]
name = "app"
prefix = "ga"

[[patches.providers]]
name = "local"
command = "false"

[[agent]]
name = "worker"
provider = "local"
`)

	report, err := Apply(cityDir, Options{})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	packToml := readFile(t, filepath.Join(cityDir, "pack.toml"))
	for _, forbidden := range []string{
		"[agent_defaults]",
		"[agents]",
		"[formulas]",
		"[defaults.rig.imports.",
		"[[patches.rigs]]",
		"[[patches.providers]]",
	} {
		if strings.Contains(packToml, forbidden) {
			t.Fatalf("pack.toml still contains rejected surface %q:\n%s", forbidden, packToml)
		}
	}

	cityToml := readFile(t, filepath.Join(cityDir, "city.toml"))
	for _, want := range []string{
		"[agent_defaults]",
		`default_sling_formula = "mol-canonical"`,
		`append_fragments = ["legacy-footer"]`,
		"[defaults.rig.imports.ops]",
		"[[patches.rigs]]",
		`prefix = "ga"`,
		"[[patches.providers]]",
		`command = "false"`,
	} {
		if !strings.Contains(cityToml, want) {
			t.Fatalf("city.toml missing migrated surface %q:\n%s", want, cityToml)
		}
	}
	if strings.Contains(cityToml, "[formulas]") {
		t.Fatalf("city.toml should not gain unsupported [formulas].dir:\n%s", cityToml)
	}
	if len(report.Warnings) == 0 || !strings.Contains(strings.Join(report.Warnings, "\n"), "formulas.dir") {
		t.Fatalf("expected formulas.dir migration warning, got %v", report.Warnings)
	}

	if _, _, err := config.LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityDir, "city.toml")); err != nil {
		t.Fatalf("LoadWithIncludes after migration: %v", err)
	}
}

// TestMigratePreservesRootPackUpstreams guards the migrate packFile round-trip
// for pack-level [upstreams]. The normal loader accepts [upstreams.<name>] in a
// city's own pack.toml (a valid authoring surface, mirroring [providers]), so gc
// migrate / gc doctor --fix must not fail it on the undecoded-key gate, and the
// table — including its nested [upstreams.<name>.env] block — must survive the
// pack.toml rewrite rather than being silently dropped.
func TestMigratePreservesRootPackUpstreams(t *testing.T) {
	t.Parallel()

	cityDir := t.TempDir()
	writeFile(t, cityDir, "city.toml", `
[workspace]
name = "legacy-city"
`)
	// [agent_defaults] is migrated out to city.toml, forcing a pack.toml rewrite
	// so the round-trip through marshalPackFile is exercised, not just the decode
	// gate.
	writeFile(t, cityDir, "pack.toml", `
[pack]
name = "legacy-city"
schema = 2

[agent_defaults]
default_sling_formula = "mol-canonical"

[upstreams.groq]
description = "Groq OpenAI-compatible gateway"
base_url = "https://api.groq.com/openai/v1"
api_key = "$GROQ_API_KEY"

[upstreams.groq.env]
GROQ_EXTRA = "$GROQ_EXTRA"
`)

	if _, err := Apply(cityDir, Options{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	packToml := readFile(t, filepath.Join(cityDir, "pack.toml"))
	for _, want := range []string{
		"[upstreams.groq]",
		`description = "Groq OpenAI-compatible gateway"`,
		`base_url = "https://api.groq.com/openai/v1"`,
		`api_key = "$GROQ_API_KEY"`,
		"[upstreams.groq.env]",
		`GROQ_EXTRA = "$GROQ_EXTRA"`,
	} {
		if !strings.Contains(packToml, want) {
			t.Fatalf("rewritten pack.toml missing preserved upstream %q:\n%s", want, packToml)
		}
	}

	// The migrated city must still compose cleanly with the preserved upstream.
	if _, _, err := config.LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityDir, "city.toml")); err != nil {
		t.Fatalf("LoadWithIncludes after migration: %v", err)
	}
}

func TestMigrateMovesPackAgentDefaultsProvider(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		pack string
		want []string
	}{
		{
			name: "canonical provider only",
			pack: `
[agent_defaults]
provider = "codex"
`,
			want: []string{
				"[agent_defaults]",
				`provider = "codex"`,
			},
		},
		{
			name: "canonical provider mixed with model",
			pack: `
[agent_defaults]
provider = "codex"
model = "gpt-5"
`,
			want: []string{
				`provider = "codex"`,
				`model = "gpt-5"`,
			},
		},
		{
			name: "legacy agents provider only",
			pack: `
[agents]
provider = "claude"
`,
			want: []string{
				"[agent_defaults]",
				`provider = "claude"`,
			},
		},
		{
			name: "legacy agents provider fills canonical defaults",
			pack: `
[agent_defaults]
model = "gpt-5"

[agents]
provider = "claude"
`,
			want: []string{
				`provider = "claude"`,
				`model = "gpt-5"`,
			},
		},
		{
			name: "canonical provider beats legacy agents alias",
			pack: `
[agent_defaults]
provider = "codex"

[agents]
provider = "claude"
wake_mode = "resume"
`,
			want: []string{
				`provider = "codex"`,
				`wake_mode = "resume"`,
			},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cityDir := t.TempDir()
			writeFile(t, cityDir, "city.toml", `
[workspace]
name = "legacy-city"
`)
			writeFile(t, cityDir, "pack.toml", `
[pack]
name = "legacy-city"
schema = 2
`+tt.pack)

			if _, err := Apply(cityDir, Options{}); err != nil {
				t.Fatalf("Apply: %v", err)
			}

			cityToml := readFile(t, filepath.Join(cityDir, "city.toml"))
			for _, want := range tt.want {
				if !strings.Contains(cityToml, want) {
					t.Fatalf("city.toml missing migrated provider default %q:\n%s", want, cityToml)
				}
			}

			packToml := readFile(t, filepath.Join(cityDir, "pack.toml"))
			for _, forbidden := range []string{"[agent_defaults]", "[agents]"} {
				if strings.Contains(packToml, forbidden) {
					t.Fatalf("pack.toml still contains %s after migration:\n%s", forbidden, packToml)
				}
			}
		})
	}
}

// TestMigrateMovesPackAgentDefaultsUpstream guards the AgentDefaults.Upstream
// field through pack→city migration. The "upstream only" case proves
// isZeroAgentDefaults no longer treats an upstream-only defaults block as zero
// (else the whole merge is skipped and the city loses its upstream default);
// the mixed and legacy-alias cases prove mergeMigratedAgentDefaults and
// mergeAgentDefaultsAliasForMigration both carry Upstream through.
func TestMigrateMovesPackAgentDefaultsUpstream(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		pack string
		want []string
	}{
		{
			name: "upstream only",
			pack: `
[agent_defaults]
upstream = "bedrock"
`,
			want: []string{
				"[agent_defaults]",
				`upstream = "bedrock"`,
			},
		},
		{
			name: "upstream mixed with provider",
			pack: `
[agent_defaults]
provider = "codex"
upstream = "bedrock"
`,
			want: []string{
				`provider = "codex"`,
				`upstream = "bedrock"`,
			},
		},
		{
			name: "legacy agents upstream fills canonical defaults",
			pack: `
[agent_defaults]
provider = "codex"

[agents]
upstream = "bedrock"
`,
			want: []string{
				`provider = "codex"`,
				`upstream = "bedrock"`,
			},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cityDir := t.TempDir()
			writeFile(t, cityDir, "city.toml", `
[workspace]
name = "legacy-city"
`)
			writeFile(t, cityDir, "pack.toml", `
[pack]
name = "legacy-city"
schema = 2
`+tt.pack)

			if _, err := Apply(cityDir, Options{}); err != nil {
				t.Fatalf("Apply: %v", err)
			}

			cityToml := readFile(t, filepath.Join(cityDir, "city.toml"))
			for _, want := range tt.want {
				if !strings.Contains(cityToml, want) {
					t.Fatalf("city.toml missing migrated upstream default %q:\n%s", want, cityToml)
				}
			}

			packToml := readFile(t, filepath.Join(cityDir, "pack.toml"))
			for _, forbidden := range []string{"[agent_defaults]", "[agents]"} {
				if strings.Contains(packToml, forbidden) {
					t.Fatalf("pack.toml still contains %s after migration:\n%s", forbidden, packToml)
				}
			}
		})
	}
}

// TestMigrateWritesAgentTomlForUpstreamOnlyAgent guards isZeroAgentConfig: an
// agent whose only non-default field is upstream must still get its per-agent
// agent.toml written. If isZeroAgentConfig omits cfg.Upstream it judges the
// agent "zero", skips the write, and the per-agent upstream selection is lost.
func TestMigrateWritesAgentTomlForUpstreamOnlyAgent(t *testing.T) {
	t.Parallel()

	cityDir := t.TempDir()
	writeFile(t, cityDir, "city.toml", `
[workspace]
name = "legacy-city"

[[agent]]
name = "worker"
upstream = "bedrock"
`)

	if _, err := Apply(cityDir, Options{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	agentToml := readFile(t, filepath.Join(cityDir, "agents", "worker", "agent.toml"))
	if !strings.Contains(agentToml, `upstream = "bedrock"`) {
		t.Fatalf("worker agent.toml missing upstream:\n%s", agentToml)
	}
}

func TestMigrateCreatesFreshBindingWhenExistingImportHasNonDefaultSemantics(t *testing.T) {
	t.Parallel()

	cityDir := t.TempDir()
	writeFile(t, cityDir, "city.toml", `
[workspace]
name = "legacy-city"
includes = ["../packs/gastown"]
`)
	writeFile(t, cityDir, "pack.toml", `
[pack]
name = "legacy-city"
schema = 2

[imports.gastown]
source = "../packs/gastown"
transitive = false
`)

	if _, err := Apply(cityDir, Options{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	packToml := readFile(t, filepath.Join(cityDir, "pack.toml"))
	if !strings.Contains(packToml, "[imports.gastown]") || !strings.Contains(packToml, "transitive = false") {
		t.Fatalf("pack.toml should preserve the existing non-default binding:\n%s", packToml)
	}
	if !strings.Contains(packToml, "[imports.gastown-2]") {
		t.Fatalf("pack.toml should add a fresh default binding instead of reusing the non-default one:\n%s", packToml)
	}
}

func TestMigrateCreatesFreshDefaultRigBindingWhenExistingImportHasNonDefaultSemantics(t *testing.T) {
	t.Parallel()

	cityDir := t.TempDir()
	writeFile(t, cityDir, "city.toml", `
[workspace]
name = "legacy-city"
default_rig_includes = ["../packs/gastown"]
`)
	writeFile(t, cityDir, "pack.toml", `
[pack]
name = "legacy-city"
schema = 2

[defaults.rig.imports.gastown]
source = "../packs/gastown"
shadow = "silent"
`)

	if _, err := Apply(cityDir, Options{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	cityToml := readFile(t, filepath.Join(cityDir, "city.toml"))
	if !strings.Contains(cityToml, "[defaults.rig.imports.gastown]") || !strings.Contains(cityToml, `shadow = "silent"`) {
		t.Fatalf("city.toml should preserve the existing non-default default-rig binding:\n%s", cityToml)
	}
	if !strings.Contains(cityToml, "[defaults.rig.imports.gastown-2]") {
		t.Fatalf("city.toml should add a fresh default-rig binding instead of reusing the non-default one:\n%s", cityToml)
	}
}

func TestMigrateDropsLegacyCityDefaultRigIncludesWhenPackAlreadyCanonical(t *testing.T) {
	t.Parallel()

	cityDir := t.TempDir()
	writeFile(t, cityDir, "city.toml", `
[workspace]
name = "legacy-city"
default_rig_includes = ["../packs/z-pack", "../packs/a-pack"]
`)
	writeFile(t, cityDir, "pack.toml", `
# existing formatting must survive no-op pack migration
[pack]
name = "legacy-city"
schema = 2

[defaults.rig.imports.z-pack]
source = "../packs/z-pack"

[defaults.rig.imports.a-pack]
source = "../packs/a-pack"
`)

	beforeCity := readFile(t, filepath.Join(cityDir, "city.toml"))
	if _, err := Apply(cityDir, Options{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := readFile(t, filepath.Join(cityDir, "pack.toml")); strings.Contains(got, "[defaults.rig.imports.") {
		t.Fatalf("pack.toml should drop default rig imports:\n%s", got)
	}
	afterCity := readFile(t, filepath.Join(cityDir, "city.toml"))
	if afterCity == beforeCity {
		t.Fatalf("city.toml should drop legacy default_rig_includes when pack.toml is already canonical:\n%s", afterCity)
	}
	if strings.Contains(afterCity, "default_rig_includes") {
		t.Fatalf("city.toml should remove default_rig_includes after migration:\n%s", afterCity)
	}
}

func TestMigrateDryRunDoesNotWrite(t *testing.T) {
	t.Parallel()

	cityDir := t.TempDir()
	writeFile(t, cityDir, "city.toml", `
[workspace]
name = "legacy-city"
includes = ["../packs/gastown"]

[[agent]]
name = "mayor"
prompt_template = "prompts/mayor.md"
`)
	writeFile(t, cityDir, "prompts/mayor.md", "hello\n")

	beforeCity := readFile(t, filepath.Join(cityDir, "city.toml"))

	report, err := Apply(cityDir, Options{DryRun: true})
	if err != nil {
		t.Fatalf("Apply(dry-run): %v", err)
	}
	if len(report.Changes) == 0 {
		t.Fatal("expected dry-run changes, got none")
	}
	if got := readFile(t, filepath.Join(cityDir, "city.toml")); got != beforeCity {
		t.Fatalf("dry-run rewrote city.toml:\n%s", got)
	}
	if _, err := os.Stat(filepath.Join(cityDir, "pack.toml")); !os.IsNotExist(err) {
		t.Fatalf("dry-run should not create pack.toml, err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(cityDir, "agents")); !os.IsNotExist(err) {
		t.Fatalf("dry-run should not create agents dir, err=%v", err)
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	t.Parallel()

	cityDir := t.TempDir()
	writeFile(t, cityDir, "city.toml", `
[workspace]
name = "legacy-city"

[[agent]]
name = "mayor"
prompt_template = "prompts/mayor.md"
`)
	writeFile(t, cityDir, "prompts/mayor.md", "hello\n")

	if _, err := Apply(cityDir, Options{}); err != nil {
		t.Fatalf("first Apply: %v", err)
	}

	beforePack := readFile(t, filepath.Join(cityDir, "pack.toml"))
	beforeCity := readFile(t, filepath.Join(cityDir, "city.toml"))

	report, err := Apply(cityDir, Options{})
	if err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	if len(report.Changes) != 0 {
		t.Fatalf("expected no changes on second run, got %v", report.Changes)
	}
	if got := readFile(t, filepath.Join(cityDir, "pack.toml")); got != beforePack {
		t.Fatalf("pack.toml changed on second run:\n%s", got)
	}
	if got := readFile(t, filepath.Join(cityDir, "city.toml")); got != beforeCity {
		t.Fatalf("city.toml changed on second run:\n%s", got)
	}
}

func TestMigrateSharedPromptCopiesInsteadOfMoving(t *testing.T) {
	t.Parallel()

	cityDir := t.TempDir()
	writeFile(t, cityDir, "city.toml", `
[workspace]
name = "legacy-city"

[[agent]]
name = "mayor"
prompt_template = "prompts/shared.md"

[[agent]]
name = "worker"
prompt_template = "prompts/shared.md"
`)
	writeFile(t, cityDir, "prompts/shared.md", "shared prompt\n")

	if _, err := Apply(cityDir, Options{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if _, err := os.Stat(filepath.Join(cityDir, "prompts", "shared.md")); err != nil {
		t.Fatalf("shared prompt should be retained for multi-use sources: %v", err)
	}
	for _, name := range []string{"mayor", "worker"} {
		got := readFile(t, filepath.Join(cityDir, "agents", name, "prompt.md"))
		if !strings.Contains(got, "shared prompt") {
			t.Fatalf("%s prompt missing copied content:\n%s", name, got)
		}
	}
}

func TestMigrateResolvesPackRegistryIncludeSources(t *testing.T) {
	t.Parallel()

	cityDir := t.TempDir()
	writeFile(t, cityDir, "city.toml", `
[workspace]
name = "legacy-city"
includes = ["gastown"]

[packs.gastown]
source = "https://github.com/example/gastown.git"
ref = "main"
path = "packs/gastown"
`)

	if _, err := Apply(cityDir, Options{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	packToml := readFile(t, filepath.Join(cityDir, "pack.toml"))
	if !strings.Contains(packToml, "[imports.gastown]") {
		t.Fatalf("pack.toml missing gastown import:\n%s", packToml)
	}
	if !strings.Contains(packToml, "source = \"https://github.com/example/gastown/tree/main/packs/gastown\"") {
		t.Fatalf("pack.toml missing converted pack source:\n%s", packToml)
	}
}

func TestMigratePackAgentsYieldToCityAgents(t *testing.T) {
	t.Parallel()

	cityDir := t.TempDir()
	writeFile(t, cityDir, "city.toml", `
[workspace]
name = "legacy-city"

[[agent]]
name = "mayor"
provider = "codex"
prompt_template = "prompts/mayor.md"
`)
	writeFile(t, cityDir, "pack.toml", `
[pack]
name = "legacy-city"
schema = 1

[[agent]]
name = "mayor"
provider = "claude"
`)
	writeFile(t, cityDir, "prompts/mayor.md", "hello\n")

	if _, err := Apply(cityDir, Options{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	agentToml := readFile(t, filepath.Join(cityDir, "agents", "mayor", "agent.toml"))
	if !strings.Contains(agentToml, "provider = \"codex\"") {
		t.Fatalf("expected city agent to win over pack agent:\n%s", agentToml)
	}
	packToml := readFile(t, filepath.Join(cityDir, "pack.toml"))
	if strings.Contains(packToml, "[[agent]]") {
		t.Fatalf("pack.toml still contains [[agent]] after migration:\n%s", packToml)
	}
}

func TestMigrateValidatesAssetsBeforeWriting(t *testing.T) {
	t.Parallel()

	cityDir := t.TempDir()
	writeFile(t, cityDir, "city.toml", `
[workspace]
name = "legacy-city"
includes = ["../packs/gastown"]

[[agent]]
name = "mayor"
prompt_template = "prompts/missing.md"
`)

	beforeCity := readFile(t, filepath.Join(cityDir, "city.toml"))

	_, err := Apply(cityDir, Options{})
	if err == nil {
		t.Fatal("expected Apply to fail for missing prompt_template")
	}
	if !strings.Contains(err.Error(), "prompt_template") {
		t.Fatalf("expected prompt_template error, got %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(cityDir, "pack.toml")); !os.IsNotExist(statErr) {
		t.Fatalf("pack.toml should not be written on validation failure, err=%v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(cityDir, "agents")); !os.IsNotExist(statErr) {
		t.Fatalf("agents dir should not be created on validation failure, err=%v", statErr)
	}
	if got := readFile(t, filepath.Join(cityDir, "city.toml")); got != beforeCity {
		t.Fatalf("city.toml changed after validation failure:\n%s", got)
	}
}

func TestAgentConfigFromAgentCoversPersistedFields(t *testing.T) {
	t.Parallel()

	trueVal := true
	intVal := 42
	formula := "mol-work"
	src := config.Agent{
		Name:                   "worker",
		Description:            "test agent description",
		Dir:                    "demo",
		WorkDir:                ".gc/agents/worker",
		TmuxAlias:              "worker--{{.CityName}}",
		Scope:                  "city",
		Suspended:              true,
		PreStart:               []string{"pre-cmd"},
		PromptTemplate:         "prompts/worker.md",
		Nudge:                  "nudge text",
		Session:                "acp",
		Provider:               "claude",
		Upstream:               "anthropic",
		StartCommand:           "claude --dangerously",
		Lifecycle:              config.AgentLifecycleOneShot,
		Args:                   []string{"--arg1"},
		PromptMode:             "flag",
		PromptFlag:             "--prompt",
		ReadyDelayMs:           &intVal,
		ReadyPromptPrefix:      "ready>",
		ProcessNames:           []string{"claude"},
		EmitsPermissionWarning: &trueVal,
		Env:                    map[string]string{"K": "V"},
		OptionDefaults:         map[string]string{"effort": "max"},
		MaxActiveSessions:      intPtr(5),
		MinActiveSessions:      intPtr(1),
		ScaleCheck:             "echo 3",
		DrainTimeout:           "10m",
		OnBoot:                 "echo boot",
		OnDeath:                "echo death",
		Namepool:               "names.txt",
		WorkQuery:              "bd ready",
		WorkQueryUnfiltered:    true,
		SlingQuery:             "bd update {}",
		IdleTimeout:            "15m",
		MaxSessionAge:          "5h",
		MaxSessionAgeJitter:    "15m",
		SleepAfterIdle:         "30s",
		InstallAgentHooks:      []string{"claude"},
		HooksInstalled:         &trueVal,
		InjectAssignedSkills:   &trueVal,
		SessionSetup:           []string{"setup-cmd"},
		SessionSetupScript:     "scripts/setup.sh",
		SessionLive:            []string{"live-cmd"},
		OverlayDir:             "overlays/test",
		DefaultSlingFormula:    &formula,
		InjectFragments:        []string{"frag1"},
		AppendFragments:        []string{"append1"},
		Attach:                 &trueVal,
		DependsOn:              []string{"other-agent"},
		ResumeCommand:          "claude --resume {{.SessionKey}} --dangerously",
		WakeMode:               "fresh",
		MouseMode:              "on",
	}

	omitted := map[string]bool{
		"Name":                         true,
		"PromptTemplate":               true,
		"Namepool":                     true,
		"NamepoolNames":                true,
		"OverlayDir":                   true,
		"SourceDir":                    true,
		"InheritedProvider":            true,
		"InheritedDefaultSlingFormula": true,
		"InheritedAppendFragments":     true,
		"Implicit":                     true,
		"SleepAfterIdleSource":         true,
		"PoolName":                     true,
		"BindingName":                  true,
		"PackName":                     true,
		// Runtime-only provenance consumed inside internal/config.
		"source": true,
		"layout": true,
		// v0.15.1 tombstones — still on Agent but intentionally not propagated
		// by migrate (removed in v0.16).
		"Skills":       true,
		"MCP":          true,
		"SharedSkills": true,
		"SharedMCP":    true,
		"SkillsDir":    true, // runtime-only (discovered from agents/<n>/skills/)
		"MCPDir":       true, // runtime-only (discovered from agents/<n>/mcp/)
	}

	cfgFields := make(map[string]bool)
	cfgType := reflect.TypeOf(agentFile{})
	for i := 0; i < cfgType.NumField(); i++ {
		cfgFields[cfgType.Field(i).Name] = true
	}

	sv := reflect.ValueOf(src)
	st := sv.Type()
	for i := 0; i < st.NumField(); i++ {
		fname := st.Field(i).Name
		if omitted[fname] {
			continue
		}
		if sv.Field(i).IsZero() {
			t.Fatalf("Agent field %q is zero in test data — add it to the migration field-sync test", fname)
		}
		if !cfgFields[fname] {
			t.Fatalf("persisted Agent field %q is missing from agentFile — update migration output or explicitly omit it", fname)
		}
	}

	cfg := agentConfigFromAgent(src)
	cv := reflect.ValueOf(cfg)
	for i := 0; i < cfgType.NumField(); i++ {
		fname := cfgType.Field(i).Name
		if cv.Field(i).IsZero() {
			t.Fatalf("agentConfigFromAgent did not populate field %q", fname)
		}
	}
}

func intPtr(v int) *int {
	return &v
}

func writeFile(t *testing.T, root, rel, contents string) {
	t.Helper()
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(strings.TrimLeft(contents, "\n")), 0o644); err != nil {
		t.Fatalf("WriteFile(%q): %v", path, err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", path, err)
	}
	return string(data)
}
