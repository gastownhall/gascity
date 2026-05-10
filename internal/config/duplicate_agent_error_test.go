package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

// TestDescribeSource exercises the per-agent descriptor that ValidateAgents
// uses when rendering duplicate-name errors. The descriptor must be non-empty
// for every reachable source category so the error never contains an empty
// quoted "" path (the visible bug ga-tpfc.1 fixes).
func TestDescribeSource(t *testing.T) {
	cityFile := "/home/u/proj/city.toml"

	tests := []struct {
		name      string
		agent     Agent
		cityFile  string
		want      string
		wantOneOf []string // any-of match when format is non-deterministic
	}{
		{
			name:     "SourceDir wins over source enum",
			agent:    Agent{Name: "mayor", SourceDir: "packs/gastown", source: sourceAutoImport},
			cityFile: cityFile,
			want:     "packs/gastown",
		},
		{
			name:     "explicit pack with SourceDir",
			agent:    Agent{Name: "mayor", SourceDir: "packs/extras", source: sourcePack},
			cityFile: cityFile,
			want:     "packs/extras",
		},
		{
			name:     "auto-import resolves to bracketed kind/path",
			agent:    Agent{Name: "mayor", BindingName: "gastown", source: sourceAutoImport},
			cityFile: cityFile,
			wantOneOf: []string{
				"<auto-import: gastown>",
				"<auto-import: ",
			},
		},
		{
			name:     "inline with cityFile renders <inline: file>",
			agent:    Agent{Name: "mayor", source: sourceInline},
			cityFile: cityFile,
			want:     "<inline: city.toml>",
		},
		{
			name:     "inline with empty cityFile renders bare <inline>",
			agent:    Agent{Name: "mayor", source: sourceInline},
			cityFile: "",
			want:     "<inline>",
		},
		{
			name:     "unknown source must not be empty",
			agent:    Agent{Name: "mayor"},
			cityFile: cityFile,
			wantOneOf: []string{
				"<unknown: name=mayor>",
				"<unknown:",
			},
		},
		{
			name:     "unknown source falls back to BindingName when present",
			agent:    Agent{Name: "polecat", BindingName: "gastown"},
			cityFile: cityFile,
			wantOneOf: []string{
				"<unknown: binding=gastown>",
				"<unknown: ",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.agent.describeSource(tc.cityFile)
			if got == "" {
				t.Fatalf("describeSource returned empty string — descriptor must always be non-empty")
			}
			if tc.want != "" && got != tc.want {
				t.Errorf("describeSource = %q, want %q", got, tc.want)
			}
			if len(tc.wantOneOf) > 0 {
				ok := false
				for _, sub := range tc.wantOneOf {
					if strings.Contains(got, sub) {
						ok = true
						break
					}
				}
				if !ok {
					t.Errorf("describeSource = %q, want it to contain one of %v", got, tc.wantOneOf)
				}
			}
		})
	}
}

// TestValidateAgents_DuplicateAutoImportRendersBracketedKind reproduces the
// ga-tpfc bug: a user pack and an auto-imported system pack both declare an
// agent of the same name. The rendered error must not contain `""`; it must
// instead point the operator at both definitions including the auto-import
// kind.
func TestValidateAgents_DuplicateAutoImportRendersBracketedKind(t *testing.T) {
	agents := []Agent{
		{Name: "mayor", SourceDir: "packs/gastown", BindingName: "gastown", source: sourcePack},
		{Name: "mayor", BindingName: "gastown", source: sourceAutoImport},
	}
	err := ValidateAgents(agents)
	if err == nil {
		t.Fatal("expected duplicate-name error")
	}
	got := err.Error()
	if strings.Contains(got, `and ""`) {
		t.Errorf(`error contains empty quoted path 'and ""'; full error: %s`, got)
	}
	if !strings.Contains(got, "packs/gastown") {
		t.Errorf("error should include the user pack source dir, got: %s", got)
	}
	if !strings.Contains(got, "<auto-import:") {
		t.Errorf(`error should include "<auto-import:" descriptor, got: %s`, got)
	}
}

// TestValidateAgents_DuplicateInlineNoEmptyQuotes ensures that two inline
// (city.toml [[agent]]) agents with the same name produce an error with no
// empty quoted paths and a non-empty descriptor for each side.
func TestValidateAgents_DuplicateInlineNoEmptyQuotes(t *testing.T) {
	agents := []Agent{
		{Name: "polecat", source: sourceInline},
		{Name: "polecat", source: sourceInline},
	}
	err := ValidateAgents(agents)
	if err == nil {
		t.Fatal("expected duplicate-name error")
	}
	got := err.Error()
	if strings.Contains(got, `""`) {
		t.Errorf(`error contains empty quoted "" — descriptors must always be non-empty; full error: %s`, got)
	}
	if !strings.Contains(got, "<inline") {
		t.Errorf(`error should include "<inline" descriptor for inline agents, got: %s`, got)
	}
}

// TestLoadWithIncludes_StampsInlineSource asserts that agents declared via
// inline [[agent]] blocks in city.toml carry source=sourceInline after load.
func TestLoadWithIncludes_StampsInlineSource(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())

	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`
[workspace]
name = "test-city"

[[agent]]
name = "mayor"
scope = "city"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, _, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityDir, "city.toml"))
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}

	var mayor *Agent
	for i := range cfg.Agents {
		if cfg.Agents[i].Name == "mayor" && cfg.Agents[i].Dir == "" {
			mayor = &cfg.Agents[i]
			break
		}
	}
	if mayor == nil {
		t.Fatalf("mayor agent not found in cfg.Agents")
	}
	if mayor.source != sourceInline {
		t.Errorf("mayor.source = %v, want sourceInline", mayor.source)
	}
}

// TestApplyAgentPatch_PreservesSource asserts the source stamp survives a
// patch application — the architecture pins "stamp once at discovery, no
// re-stamping" and patches must respect that.
func TestApplyAgentPatch_PreservesSource(t *testing.T) {
	strVal := func(s string) *string { return &s }
	agent := Agent{Name: "mayor", source: sourceAutoImport, BindingName: "gastown"}
	patch := AgentPatch{Name: "mayor", PromptTemplate: strVal("prompts/new.md")}
	applyAgentPatchFields(&agent, &patch)
	if agent.source != sourceAutoImport {
		t.Errorf("agent.source after patch = %v, want sourceAutoImport (preserved)", agent.source)
	}
}

// TestApplyAgentOverride_PreservesSource asserts the source stamp survives
// an override application.
func TestApplyAgentOverride_PreservesSource(t *testing.T) {
	strVal := func(s string) *string { return &s }
	agent := Agent{Name: "polecat", source: sourceInline}
	override := AgentOverride{Agent: "polecat", PromptTemplate: strVal("prompts/p.md")}
	applyAgentOverride(&agent, &override)
	if agent.source != sourceInline {
		t.Errorf("agent.source after override = %v, want sourceInline (preserved)", agent.source)
	}
}

// TestFormatDuplicateAgentError_LayoutMatrix sweeps over the 3×3
// (a.layout, b.layout) matrix from ga-9ogb §7 and asserts the
// migration-guidance variant fires only on the (V1Inline, V2Convention)
// pair (in either order). The other seven cells fall through to the
// generic format.
func TestFormatDuplicateAgentError_LayoutMatrix(t *testing.T) {
	const migrationHeadline = "pack v1/v2 layout collision"

	layouts := []agentLayout{layoutUnknown, layoutV1Inline, layoutV2Convention}
	for _, la := range layouts {
		for _, lb := range layouts {
			a := Agent{Name: "mayor", SourceDir: "packs/a", layout: la}
			b := Agent{Name: "mayor", SourceDir: "packs/b", layout: lb}
			err := formatDuplicateAgentError(a, b)
			if err == nil {
				t.Errorf("formatDuplicateAgentError returned nil for layouts (%v, %v)", la, lb)
				continue
			}
			got := err.Error()
			isV1V2 := (la == layoutV1Inline && lb == layoutV2Convention) ||
				(la == layoutV2Convention && lb == layoutV1Inline)
			hasMigration := strings.Contains(got, migrationHeadline)
			if isV1V2 && !hasMigration {
				t.Errorf("layouts (%v, %v): expected migration variant (%q), got: %s",
					la, lb, migrationHeadline, got)
			}
			if !isV1V2 && hasMigration {
				t.Errorf("layouts (%v, %v): expected generic variant, got migration: %s",
					la, lb, got)
			}
			if !isV1V2 && !strings.Contains(got, "duplicate name") {
				t.Errorf("layouts (%v, %v): expected generic 'duplicate name' wording, got: %s",
					la, lb, got)
			}
			if strings.Contains(got, `""`) {
				t.Errorf(`layouts (%v, %v): error contains empty quoted "": %s`, la, lb, got)
			}
		}
	}
}

// TestFormatDuplicateAgentError_MigrationHeadlinePinned pins the exact
// first-line headline for the migration variant. Body prose may evolve
// without test churn, but the headline is the stable signal log
// scrapers and operators key on.
func TestFormatDuplicateAgentError_MigrationHeadlinePinned(t *testing.T) {
	a := Agent{Name: "mayor", SourceDir: "packs/gastown", layout: layoutV1Inline}
	b := Agent{Name: "mayor", BindingName: "gastown", layout: layoutV2Convention}
	err := formatDuplicateAgentError(a, b)
	if err == nil {
		t.Fatal("expected migration error")
	}
	headline := strings.SplitN(err.Error(), "\n", 2)[0]
	want := `agent "mayor": pack v1/v2 layout collision`
	if headline != want {
		t.Errorf("migration headline = %q, want %q", headline, want)
	}
}

// TestFormatDuplicateAgentError_MigrationContainsBothSources asserts
// the migration error names both the v1 and v2 source descriptors and
// the migration-guide doc path. Order-independent: the helper accepts
// (v1, v2) or (v2, v1).
func TestFormatDuplicateAgentError_MigrationContainsBothSources(t *testing.T) {
	v1 := Agent{Name: "mayor", SourceDir: "packs/gastown", layout: layoutV1Inline}
	v2 := Agent{Name: "mayor", BindingName: "gastown", source: sourceAutoImport, layout: layoutV2Convention}

	for _, order := range []struct {
		a, b Agent
	}{
		{v1, v2},
		{v2, v1},
	} {
		err := formatDuplicateAgentError(order.a, order.b)
		if err == nil {
			t.Fatal("expected migration error")
		}
		got := err.Error()
		if !strings.Contains(got, "v1 source: ") {
			t.Errorf(`expected "v1 source: " line, got: %s`, got)
		}
		if !strings.Contains(got, "v2 source: ") {
			t.Errorf(`expected "v2 source: " line, got: %s`, got)
		}
		if !strings.Contains(got, "[[agent]] mayor") {
			t.Errorf(`expected "[[agent]] mayor" reference on the v1 line, got: %s`, got)
		}
		if !strings.Contains(got, "agents/mayor/agent.toml") {
			t.Errorf(`expected "agents/mayor/agent.toml" reference on the v2 line, got: %s`, got)
		}
		if !strings.Contains(got, "docs/packv2/migration.mdx") {
			t.Errorf(`expected migration doc link "docs/packv2/migration.mdx", got: %s`, got)
		}
	}
}

// TestMigrationGuideDocPathExists protects the migration diagnostic from
// pointing operators at documentation that is not present in the repository.
func TestMigrationGuideDocPathExists(t *testing.T) {
	path := filepath.Clean(filepath.Join("..", "..", migrationGuideDocPath))
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("migration guide %q is missing: %v", migrationGuideDocPath, err)
	}
	if info.IsDir() {
		t.Fatalf("migration guide %q resolves to a directory", migrationGuideDocPath)
	}
}

// TestValidateAgents_V1V2LayoutCollisionRepro reproduces the bug ga-9ogb
// fixes: a v1 [[agent]] block in one pack + a v2 agents/<name>/agent.toml
// in another pack both declare "mayor". Today (pre-fix) the operator
// gets the same generic duplicate-name error as a v2-vs-v2 conflict.
// Post-fix, the migration headline fires.
func TestValidateAgents_V1V2LayoutCollisionRepro(t *testing.T) {
	agents := []Agent{
		{Name: "mayor", SourceDir: "packs/userpack", layout: layoutV1Inline, source: sourcePack},
		{Name: "mayor", BindingName: "gastown", source: sourceAutoImport, layout: layoutV2Convention},
	}
	err := ValidateAgents(agents)
	if err == nil {
		t.Fatal("expected duplicate-name error")
	}
	got := err.Error()
	if !strings.Contains(got, "pack v1/v2 layout collision") {
		t.Errorf("expected migration headline, got: %s", got)
	}
}

// TestValidateAgents_CityPackV1CollisionWithImportedV2Convention asserts
// the root city pack.toml path stamps v1 [[agent]] blocks with layout
// provenance before imported v2 convention agents are validated.
func TestValidateAgents_CityPackV1CollisionWithImportedV2Convention(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	dir := t.TempDir()
	cityDir := filepath.Join(dir, "city")
	importDir := filepath.Join(dir, "sys")
	if err := os.MkdirAll(cityDir, 0o755); err != nil {
		t.Fatal(err)
	}
	agentDir := filepath.Join(importDir, "agents", "worker")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`
[workspace]
name = "test"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "pack.toml"), []byte(`
[pack]
name = "city"
schema = 1

[imports.sys]
source = "../sys"

[[agent]]
name = "worker"
scope = "city"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(importDir, "pack.toml"), []byte(`
[pack]
name = "sys"
schema = 1
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "agent.toml"), []byte(`
scope = "city"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, _, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityDir, "city.toml"))
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	err = ValidateAgents(cfg.Agents)
	if err == nil {
		t.Fatal("expected duplicate-name error")
	}
	got := err.Error()
	if !strings.Contains(got, "pack v1/v2 layout collision") {
		t.Fatalf("expected migration diagnostic, got: %s", got)
	}
}

// TestValidateAgents_FallbackSuppressesV1V2Migration asserts that when
// one side of the v1/v2 pair carries fallback=true, the fallback agent
// is removed by resolveFallbackAgents BEFORE ValidateAgents runs, so
// no migration error fires. Pinned via the full LoadWithIncludes path
// because resolveFallbackAgents is composition-layer machinery.
func TestValidateAgents_FallbackSuppressesV1V2Migration(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	cityDir := t.TempDir()

	// User pack: v1 [[agent]] mayor with fallback=true.
	userPack := filepath.Join(cityDir, "packs", "user")
	if err := os.MkdirAll(userPack, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(userPack, "pack.toml"), []byte(`
[pack]
name = "user"
schema = 1

[[agent]]
name = "mayor"
scope = "city"
fallback = true
`), 0o644); err != nil {
		t.Fatal(err)
	}

	// System pack: v2 agents/mayor/agent.toml.
	sysPack := filepath.Join(cityDir, "packs", "sys")
	mayorDir := filepath.Join(sysPack, "agents", "mayor")
	if err := os.MkdirAll(mayorDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sysPack, "pack.toml"), []byte(`
[pack]
name = "sys"
schema = 1
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mayorDir, "agent.toml"), []byte(`
scope = "city"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`
[workspace]
name = "test"

[pack]
name = "city"
schema = 1
includes = ["packs/user", "packs/sys"]
`), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, _, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityDir, "city.toml"))
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v — fallback should suppress the v1/v2 collision", err)
	}
	if err := ValidateAgents(cfg.Agents); err != nil {
		t.Fatalf("ValidateAgents: %v — fallback should suppress the v1/v2 collision", err)
	}
}

// TestValidateAgents_NoEmptyQuotesAcrossAllSourceCombos sweeps over all
// source-enum × SourceDir-empty/present combinations and asserts that no
// duplicate-agent error rendered by ValidateAgents contains an empty quoted
// "" path. This is the fallback-suppression invariant the architecture
// pins.
func TestValidateAgents_NoEmptyQuotesAcrossAllSourceCombos(t *testing.T) {
	sources := []agentSource{sourceUnknown, sourceInline, sourcePack, sourceAutoImport}
	srcDirs := []string{"", "packs/base"}

	for _, sa := range sources {
		for _, sb := range sources {
			for _, da := range srcDirs {
				for _, db := range srcDirs {
					a := Agent{Name: "worker", source: sa, SourceDir: da, BindingName: "gastown"}
					b := Agent{Name: "worker", source: sb, SourceDir: db, BindingName: "gastown"}
					err := ValidateAgents([]Agent{a, b})
					if err == nil {
						t.Errorf("expected duplicate-name error for source=(%v,%v), srcDir=(%q,%q)", sa, sb, da, db)
						continue
					}
					if strings.Contains(err.Error(), `""`) {
						t.Errorf(`empty quoted "" in error for source=(%v,%v), srcDir=(%q,%q): %s`, sa, sb, da, db, err.Error())
					}
				}
			}
		}
	}
}
