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
	tests := []struct {
		name         string
		agent        Agent
		want         string
		wantContains []string
	}{
		{
			name:  "auto-import with SourceDir renders kind and path",
			agent: Agent{Name: "mayor", SourceDir: "packs/gastown", BindingName: "gastown", source: sourceAutoImport},
			wantContains: []string{
				"<auto-import: gastown>",
				"packs/gastown",
			},
		},
		{
			name:  "explicit pack with SourceDir renders kind and path",
			agent: Agent{Name: "mayor", SourceDir: "packs/extras", BindingName: "extras", source: sourcePack},
			wantContains: []string{
				"<pack: extras>",
				"packs/extras",
			},
		},
		{
			name:  "pack without binding keeps SourceDir",
			agent: Agent{Name: "mayor", SourceDir: "packs/extras", source: sourcePack},
			want:  "packs/extras",
		},
		{
			name:  "auto-import resolves to bracketed kind",
			agent: Agent{Name: "mayor", BindingName: "gastown", source: sourceAutoImport},
			wantContains: []string{
				"<auto-import: gastown>",
				"<auto-import: ",
			},
		},
		{
			name:  "inline with empty SourceDir renders bare <inline>",
			agent: Agent{Name: "mayor", source: sourceInline},
			want:  "<inline>",
		},
		{
			name:  "inline fragment with SourceDir renders path",
			agent: Agent{Name: "mayor", SourceDir: "fragments/agents.toml", source: sourceInline},
			want:  "fragments/agents.toml",
		},
		{
			name:  "unknown source must not be empty",
			agent: Agent{Name: "mayor"},
			wantContains: []string{
				"<unknown: name=mayor>",
				"<unknown:",
			},
		},
		{
			name:  "unknown source falls back to BindingName when present",
			agent: Agent{Name: "polecat", BindingName: "gastown"},
			wantContains: []string{
				"<unknown: binding=gastown>",
				"<unknown: ",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.agent.describeSource()
			if got == "" {
				t.Fatalf("describeSource returned empty string — descriptor must always be non-empty")
			}
			if tc.want != "" && got != tc.want {
				t.Errorf("describeSource = %q, want %q", got, tc.want)
			}
			if len(tc.wantContains) > 0 {
				missing := []string{}
				for _, sub := range tc.wantContains {
					if !strings.Contains(got, sub) {
						missing = append(missing, sub)
					}
				}
				if len(missing) > 0 {
					t.Errorf("describeSource = %q, missing substrings %v", got, missing)
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

func writeDuplicateAgentSourceTestFile(t *testing.T, root, rel, data string) {
	t.Helper()
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", path, err)
	}
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatalf("WriteFile(%s): %v", path, err)
	}
}

func TestValidateAgents_LoadedPackCollisionRendersPackBinding(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())

	dir := t.TempDir()
	writeDuplicateAgentSourceTestFile(t, dir, "city.toml", `
[workspace]
name = "test-city"

[imports.tools]
source = "./packs/tools"

[[agent]]
name = "worker"
`)
	writeDuplicateAgentSourceTestFile(t, dir, "packs/tools/pack.toml", `
[pack]
name = "tools"
schema = 1

[[agent]]
name = "worker"
scope = "city"
`)

	cfg, _, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(dir, "city.toml"))
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}

	err = ValidateAgents(cfg.Agents)
	if err == nil {
		t.Fatal("expected duplicate-name error")
	}
	got := err.Error()
	if strings.Contains(got, `""`) {
		t.Fatalf(`error contains empty quoted ""; full error: %s`, got)
	}
	if !strings.Contains(got, "<pack: tools>") {
		t.Fatalf("error should include pack binding provenance, got: %s", got)
	}
	if !strings.Contains(got, filepath.Join(dir, "packs/tools")) {
		t.Fatalf("error should include pack source dir, got: %s", got)
	}
	if !strings.Contains(got, "<inline>") {
		t.Fatalf("error should include inline provenance, got: %s", got)
	}
}

func TestValidateAgents_DefaultRigImportCollisionRendersAutoImportBinding(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())

	dir := t.TempDir()
	cityDir := filepath.Join(dir, "city")
	packDir := filepath.Join(dir, "gastown")
	writeDuplicateAgentSourceTestFile(t, cityDir, "city.toml", `
[workspace]
name = "test-city"

[[rigs]]
name = "proj"
path = "/tmp/proj"

[rigs.imports.gs]
source = "../gastown"

[[agent]]
name = "worker"
dir = "proj"
`)
	writeDuplicateAgentSourceTestFile(t, cityDir, "pack.toml", `
[pack]
name = "test-city"
schema = 2

[defaults.rig.imports.gs]
source = "../gastown"
`)
	writeDuplicateAgentSourceTestFile(t, packDir, "pack.toml", `
[pack]
name = "gastown"
schema = 1

[[agent]]
name = "worker"
scope = "rig"
`)

	cfg, _, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityDir, "city.toml"))
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}

	var imported *Agent
	for i := range cfg.Agents {
		a := &cfg.Agents[i]
		if a.Dir == "proj" && a.Name == "worker" && a.BindingName == "gs" {
			imported = a
			break
		}
	}
	if imported == nil {
		t.Fatal("imported rig agent not found")
	}
	if imported.source != sourceAutoImport {
		t.Fatalf("imported source = %v, want sourceAutoImport", imported.source)
	}
	if imported.SourceDir == "" {
		t.Fatal("imported source dir is empty; test must use the real pack-loaded shape")
	}

	err = ValidateAgents(cfg.Agents)
	if err == nil {
		t.Fatal("expected duplicate-name error")
	}
	got := err.Error()
	if strings.Contains(got, `""`) {
		t.Fatalf(`error contains empty quoted ""; full error: %s`, got)
	}
	if !strings.Contains(got, "<auto-import: gs>") {
		t.Fatalf("error should include auto-import binding provenance, got: %s", got)
	}
	if !strings.Contains(got, packDir) {
		t.Fatalf("error should include auto-import source dir, got: %s", got)
	}
	if !strings.Contains(got, "<inline>") {
		t.Fatalf("error should include inline provenance, got: %s", got)
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
