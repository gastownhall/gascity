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
fallback = true

[[agent]]
name = "worker"
prompt_template = "prompts/worker.md"
`)
	writeFile(t, cityDir, "prompts/mayor.md", "You are {{.Agent}}.\n")
	writeFile(t, cityDir, "prompts/worker.md", "You are a worker.\n")
	writeFile(t, cityDir, "overlays/mayor/CLAUDE.md", "city overlay\n")
	writeFile(t, cityDir, "namepools/mayor.txt", "Ada\nGrace\n")

	report, err := Apply(cityDir, Options{})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if len(report.Warnings) == 0 {
		t.Fatal("expected fallback warning, got none")
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
	for _, line := range []string{
		"[defaults.rig.imports.z-pack]",
		"source = \"../packs/z-pack\"",
		"[defaults.rig.imports.a-pack]",
		"source = \"../packs/a-pack\"",
	} {
		if !strings.Contains(packToml, line) {
			t.Fatalf("pack.toml missing migrated default-rig imports %q:\n%s", line, packToml)
		}
	}
	if strings.Index(packToml, "[defaults.rig.imports.z-pack]") > strings.Index(packToml, "[defaults.rig.imports.a-pack]") {
		t.Fatalf("pack.toml reordered migrated default-rig imports:\n%s", packToml)
	}

	cityToml := readFile(t, filepath.Join(cityDir, "city.toml"))
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

	packToml := readFile(t, filepath.Join(cityDir, "pack.toml"))
	for _, line := range []string{
		"[defaults.rig.imports.z-pack]",
		`source = "../packs/z-pack"`,
		"[defaults.rig.imports.a-pack]",
		`source = "../packs/a-pack"`,
	} {
		if !strings.Contains(packToml, line) {
			t.Fatalf("pack.toml missing migrated default-rig imports %q:\n%s", line, packToml)
		}
	}
	cityToml := readFile(t, filepath.Join(cityDir, "city.toml"))
	if strings.Contains(cityToml, "default_rig_includes") {
		t.Fatalf("city.toml should drop legacy workspace.default_rig_includes:\n%s", cityToml)
	}

	cfg, _, err := config.LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityDir, "city.toml"))
	if err != nil {
		t.Fatalf("LoadWithIncludes after migration: %v", err)
	}
	if len(cfg.Workspace.DefaultRigIncludes) != 2 {
		t.Fatalf("len(DefaultRigIncludes) = %d, want 2", len(cfg.Workspace.DefaultRigIncludes))
	}
	if cfg.Workspace.DefaultRigIncludes[0] != "../packs/z-pack" {
		t.Fatalf("DefaultRigIncludes[0] = %q, want %q", cfg.Workspace.DefaultRigIncludes[0], "../packs/z-pack")
	}
	if cfg.Workspace.DefaultRigIncludes[1] != "../packs/a-pack" {
		t.Fatalf("DefaultRigIncludes[1] = %q, want %q", cfg.Workspace.DefaultRigIncludes[1], "../packs/a-pack")
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

func TestMigratePreservesExistingRootDefaultRigImportOrder(t *testing.T) {
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
	zIndex := strings.Index(packToml, "[defaults.rig.imports.z-pack]")
	aIndex := strings.Index(packToml, "[defaults.rig.imports.a-pack]")
	if zIndex < 0 || aIndex < 0 {
		t.Fatalf("pack.toml missing root default rig imports:\n%s", packToml)
	}
	if zIndex > aIndex {
		t.Fatalf("pack.toml reordered default rig imports:\n%s", packToml)
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

	packToml := readFile(t, filepath.Join(cityDir, "pack.toml"))
	if !strings.Contains(packToml, "[defaults.rig.imports.gastown]") || !strings.Contains(packToml, `shadow = "silent"`) {
		t.Fatalf("pack.toml should preserve the existing non-default default-rig binding:\n%s", packToml)
	}
	if !strings.Contains(packToml, "[defaults.rig.imports.gastown-2]") {
		t.Fatalf("pack.toml should add a fresh default-rig binding instead of reusing the non-default one:\n%s", packToml)
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

	beforePack := readFile(t, filepath.Join(cityDir, "pack.toml"))
	beforeCity := readFile(t, filepath.Join(cityDir, "city.toml"))
	if _, err := Apply(cityDir, Options{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := readFile(t, filepath.Join(cityDir, "pack.toml")); got != beforePack {
		t.Fatalf("pack.toml changed without pack migration changes:\n%s", got)
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
	if !strings.Contains(packToml, "source = \"https://github.com/example/gastown.git//packs/gastown#main\"") {
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
		Scope:                  "city",
		Suspended:              true,
		PreStart:               []string{"pre-cmd"},
		PromptTemplate:         "prompts/worker.md",
		Nudge:                  "nudge text",
		Session:                "acp",
		Provider:               "claude",
		StartCommand:           "claude --dangerously",
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
		SlingQuery:             "bd update {}",
		IdleTimeout:            "15m",
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
		Fallback:               true,
		DependsOn:              []string{"other-agent"},
		ResumeCommand:          "claude --resume {{.SessionKey}} --dangerously",
		WakeMode:               "fresh",
	}

	omitted := map[string]bool{
		"Name":                         true,
		"PromptTemplate":               true,
		"Namepool":                     true,
		"NamepoolNames":                true,
		"OverlayDir":                   true,
		"SourceDir":                    true,
		"InheritedDefaultSlingFormula": true,
		"InheritedAppendFragments":     true,
		"Implicit":                     true,
		"Fallback":                     true,
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
