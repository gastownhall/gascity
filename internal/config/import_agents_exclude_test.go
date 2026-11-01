package config

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

func TestImportAgentsExcludeParseAndRoundTrip(t *testing.T) {
	cfg, err := Parse([]byte(`[workspace]
name = "test"

[imports.shared]
source = "../shared"
agents_exclude = ["worker", "unused"]
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := cfg.Imports["shared"].AgentsExclude; len(got) != 2 || got[0] != "worker" || got[1] != "unused" {
		t.Fatalf("AgentsExclude = %#v, want [worker unused]", got)
	}
	data, err := cfg.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	got, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse(Marshal output): %v\n%s", err, data)
	}
	if got := got.Imports["shared"].AgentsExclude; len(got) != 2 || got[0] != "worker" || got[1] != "unused" {
		t.Fatalf("round-tripped AgentsExclude = %#v, want [worker unused]", got)
	}
}

func TestImportAgentsExcludeDisablesDefaultImportReuse(t *testing.T) {
	if !(&Import{Source: "../shared"}).HasDefaultOptionSemantics() {
		t.Fatal("unrestricted import should have default option semantics")
	}
	if (&Import{Source: "../shared", AgentsExclude: []string{"worker"}}).HasDefaultOptionSemantics() {
		t.Fatal("agent-narrowed import must not have default option semantics")
	}

	target := map[string]Import{
		"shared": {Source: "../shared", AgentsExclude: []string{"worker"}},
	}
	if !AddLegacyImports(target, []string{"../shared"}, nil) {
		t.Fatal("AddLegacyImports should add a distinct unrestricted import")
	}
	if _, ok := target["shared-2"]; !ok {
		t.Fatalf("imports = %#v, want narrowed shared plus unrestricted shared-2", target)
	}
}

func TestImportAgentsExcludeCityPreservesSiblingContentAndRemovesNamedSession(t *testing.T) {
	dir := t.TempDir()
	cityDir := filepath.Join(dir, "city")
	packDir := filepath.Join(dir, "shared")
	writeTestFile(t, cityDir, "city.toml", `[workspace]
name = "test"

[imports.shared]
source = "../shared"
agents_exclude = ["worker"]
`)
	writeTestFile(t, packDir, "pack.toml", `[pack]
name = "shared"
schema = 1

[[agent]]
name = "worker"
scope = "city"
start_command = "true"

[[agent]]
name = "keeper"
scope = "city"
start_command = "true"

[[named_session]]
name = "worker-alias"
template = "worker"
scope = "city"

[[named_session]]
name = "keeper-alias"
template = "keeper"
scope = "city"

[providers.shared]
command = "true"

[global]
session_live = ["echo retained"]
`)
	cfg, prov, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityDir, "city.toml"))
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	if prov == nil {
		t.Fatal("LoadWithIncludes returned nil provenance")
	}
	for _, agent := range cfg.Agents {
		if agent.Name == "worker" {
			t.Fatalf("excluded worker survived: %#v", agent)
		}
		if agent.Name == "keeper" && agent.BindingName != "shared" {
			t.Fatalf("keeper BindingName = %q, want shared", agent.BindingName)
		}
	}
	if len(cfg.NamedSessions) != 1 || cfg.NamedSessions[0].Template != "keeper" {
		t.Fatalf("NamedSessions = %#v, want only keeper session", cfg.NamedSessions)
	}
	if _, ok := cfg.Providers["shared"]; !ok {
		t.Fatalf("provider from import was not retained: %#v", cfg.Providers)
	}
	if len(cfg.PackGlobals) == 0 || len(cfg.PackGlobals[0].SessionLive) != 1 {
		t.Fatalf("pack globals were not retained: %#v", cfg.PackGlobals)
	}
}

func TestImportAgentsExcludeRig(t *testing.T) {
	dir := t.TempDir()
	cityDir := filepath.Join(dir, "city")
	packDir := filepath.Join(dir, "shared")
	writeTestFile(t, cityDir, "city.toml", `[workspace]
name = "test"

[[rigs]]
name = "app"
path = "/tmp/app"

[rigs.imports.shared]
source = "../shared"
agents_exclude = ["worker"]
`)
	writeTestFile(t, packDir, "pack.toml", `[pack]
name = "shared"
schema = 1

[[agent]]
name = "worker"
scope = "rig"
start_command = "true"

[[agent]]
name = "keeper"
scope = "rig"
start_command = "true"

[[named_session]]
template = "worker"
scope = "rig"

[[named_session]]
template = "keeper"
scope = "rig"
`)
	cfg, _, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityDir, "city.toml"))
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	if len(cfg.Agents) != 1 || cfg.Agents[0].Name != "keeper" || cfg.Agents[0].QualifiedName() != "app/shared.keeper" {
		t.Fatalf("rig agents = %#v, want app/shared.keeper only", cfg.Agents)
	}
	if len(cfg.NamedSessions) != 1 || cfg.NamedSessions[0].Template != "keeper" {
		t.Fatalf("rig named sessions = %#v, want keeper only", cfg.NamedSessions)
	}
}

func TestImportAgentsExcludeIsEdgeLocalWithCachedSource(t *testing.T) {
	dir := t.TempDir()
	cityDir := filepath.Join(dir, "city")
	packDir := filepath.Join(dir, "shared")
	writeTestFile(t, cityDir, "city.toml", `[workspace]
name = "test"

[imports.first]
source = "../shared"
agents_exclude = ["worker"]

[imports.second]
source = "../shared"
agents_exclude = ["keeper"]
`)
	writeTestFile(t, packDir, "pack.toml", `[pack]
name = "shared"
schema = 1

[[agent]]
name = "worker"
scope = "city"
start_command = "true"

[[agent]]
name = "keeper"
scope = "city"
start_command = "true"
`)
	cfg, _, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityDir, "city.toml"))
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	seen := map[string]bool{}
	for _, agent := range cfg.Agents {
		seen[agent.QualifiedName()] = true
	}
	if !seen["first.keeper"] || seen["first.worker"] || !seen["second.worker"] || seen["second.keeper"] {
		t.Fatalf("edge-local exclusions produced agents %v", seen)
	}
}

func TestImportAgentsExcludeNestedAndTransitiveFalse(t *testing.T) {
	dir := t.TempDir()
	cityDir := filepath.Join(dir, "city")
	outerDir := filepath.Join(dir, "outer")
	innerDir := filepath.Join(dir, "inner")
	writeTestFile(t, cityDir, "city.toml", `[workspace]
name = "test"

[imports.outer]
source = "../outer"
agents_exclude = ["deep"]
`)
	writeTestFile(t, outerDir, "pack.toml", `[pack]
name = "outer"
schema = 1

[imports.inner]
source = "../inner"
export = true
`)
	writeTestFile(t, outerDir, "agents/keeper/agent.toml", "start_command = \"true\"\nscope = \"city\"\n")
	writeTestFile(t, innerDir, "pack.toml", `[pack]
name = "inner"
schema = 1

[[agent]]
name = "deep"
scope = "city"
start_command = "true"

[[named_session]]
template = "deep"
scope = "city"
`)
	cfg, prov, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityDir, "city.toml"))
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	for _, agent := range cfg.Agents {
		if agent.Name == "deep" {
			t.Fatalf("nested excluded agent survived: %#v", agent)
		}
	}
	for _, session := range cfg.NamedSessions {
		if session.Template == "deep" {
			t.Fatalf("nested excluded session survived: %#v", session)
		}
	}
	if len(prov.Warnings) != 0 {
		// The selector matches the nested agent before the edge-level filter;
		// no unmatched-selector warning is expected in this case.
		for _, warning := range prov.Warnings {
			if strings.Contains(warning, "agents_exclude") && strings.Contains(warning, "deep") {
				t.Fatalf("matched selector produced warning: %q", warning)
			}
		}
	}
}

func TestImportAgentsExcludeInvalidAndUnmatched(t *testing.T) {
	_, _, _, err := filterImportedAgentsByName(
		[]Agent{{Name: "keeper"}}, nil, []string{"bad.name"}, `city import "shared"`)
	if err == nil || !strings.Contains(err.Error(), "agents_exclude") {
		t.Fatalf("invalid selector error = %v, want agents_exclude validation", err)
	}
	_, _, warnings, err := filterImportedAgentsByName(
		[]Agent{{Name: "keeper"}}, nil, []string{"worker"}, `city import "shared"`)
	if err != nil {
		t.Fatalf("unmatched selector error = %v, want warning/no-op", err)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "matched no imported agent") {
		t.Fatalf("unmatched warnings = %v, want one warning", warnings)
	}
}

func TestImportAgentsExcludeKeepsDependsOnValidation(t *testing.T) {
	kept, _, _, err := filterImportedAgentsByName([]Agent{
		{Name: "keeper", DependsOn: []string{"worker"}},
		{Name: "worker"},
	}, nil, []string{"worker"}, `city import "shared"`)
	if err != nil {
		t.Fatalf("filterImportedAgentsByName: %v", err)
	}
	if err := ValidateAgents(kept); err == nil || !strings.Contains(err.Error(), "depends_on") {
		t.Fatalf("ValidateAgents = %v, want missing dependency error", err)
	}
}
