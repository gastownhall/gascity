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
	cityRoot := "/home/u/proj"
	cityFile := "/home/u/proj/city.toml"

	tests := []struct {
		name      string
		agent     Agent
		cityRoot  string
		cityFile  string
		want      string
		wantOneOf []string // any-of match when format is non-deterministic
	}{
		{
			name:     "SourceDir wins over source enum",
			agent:    Agent{Name: "mayor", SourceDir: "packs/gastown", source: sourceAutoImport},
			cityRoot: cityRoot,
			cityFile: cityFile,
			want:     "packs/gastown",
		},
		{
			name:     "explicit pack with SourceDir",
			agent:    Agent{Name: "mayor", SourceDir: "packs/extras", source: sourcePack},
			cityRoot: cityRoot,
			cityFile: cityFile,
			want:     "packs/extras",
		},
		{
			name:     "auto-import resolves to bracketed kind/path",
			agent:    Agent{Name: "mayor", BindingName: "gastown", source: sourceAutoImport},
			cityRoot: cityRoot,
			cityFile: cityFile,
			wantOneOf: []string{
				"<auto-import: gastown>",
				"<auto-import: ",
			},
		},
		{
			name:     "inline with cityFile renders <inline: file>",
			agent:    Agent{Name: "mayor", source: sourceInline},
			cityRoot: cityRoot,
			cityFile: cityFile,
			want:     "<inline: city.toml>",
		},
		{
			name:     "inline with empty cityFile renders bare <inline>",
			agent:    Agent{Name: "mayor", source: sourceInline},
			cityRoot: "",
			cityFile: "",
			want:     "<inline>",
		},
		{
			name:     "unknown source must not be empty",
			agent:    Agent{Name: "mayor"},
			cityRoot: cityRoot,
			cityFile: cityFile,
			wantOneOf: []string{
				"<unknown: name=mayor>",
				"<unknown:",
			},
		},
		{
			name:     "unknown source falls back to BindingName when present",
			agent:    Agent{Name: "polecat", BindingName: "gastown"},
			cityRoot: cityRoot,
			cityFile: cityFile,
			wantOneOf: []string{
				"<unknown: binding=gastown>",
				"<unknown: ",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.agent.describeSource(tc.cityRoot, tc.cityFile)
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

// TestValidateAgents_NoEmptyQuotesAcrossAllSourceCombos sweeps over all
// source-enum × SourceDir-empty/present combinations and asserts that no
// duplicate-agent error rendered by ValidateAgents contains an empty quoted
// "" path. This is the fallback-suppression invariant the architecture
// pins.
func TestValidateAgents_NoEmptyQuotesAcrossAllSourceCombos(t *testing.T) {
	sources := []agentSource{0, sourceInline, sourcePack, sourceAutoImport}
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
