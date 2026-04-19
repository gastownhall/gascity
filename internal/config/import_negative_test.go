package config

// Negative and stress tests for the V2 import system.
// These test error paths, malformed inputs, and boundary conditions.

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

func TestImport_MalformedAgentToml(t *testing.T) {
	// A malformed agent.toml in agents/<name>/ should produce a clear error.
	dir := t.TempDir()
	packDir := filepath.Join(dir, "mypk")
	agentDir := filepath.Join(packDir, "agents", "bad")
	mustMkdirAll(t, agentDir, 0o755)

	writeTestFile(t, packDir, "pack.toml", `
[pack]
name = "mypk"
schema = 1
`)
	writeTestFile(t, agentDir, "agent.toml", `{{invalid toml`)

	cityDir := filepath.Join(dir, "city")
	mustMkdirAll(t, cityDir, 0o755)
	writeTestFile(t, cityDir, "city.toml", `
[workspace]
name = "test"
includes = ["../mypk"]
`)

	_, _, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityDir, "city.toml"))
	if err == nil {
		t.Fatal("expected error for malformed agent.toml")
	}
	if !strings.Contains(err.Error(), "agents/bad/agent.toml") {
		t.Errorf("error should mention agent.toml path; got: %v", err)
	}
}

func TestImport_InvalidPackSchemaInCityPackToml(t *testing.T) {
	// A city pack.toml with invalid schema should produce a clear error.
	dir := t.TempDir()
	cityDir := filepath.Join(dir, "city")
	mustMkdirAll(t, cityDir, 0o755)

	writeTestFile(t, cityDir, "pack.toml", `
[pack]
name = "test"
schema = 999
`)
	writeTestFile(t, cityDir, "city.toml", `
[workspace]
name = "test"
`)

	_, _, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityDir, "city.toml"))
	if err == nil {
		t.Fatal("expected error for invalid pack schema")
	}
	if !strings.Contains(err.Error(), "schema") {
		t.Errorf("error should mention schema; got: %v", err)
	}
}

func TestImport_MalformedCityPackToml(t *testing.T) {
	// A malformed city pack.toml should produce a clear error, not be
	// silently ignored.
	dir := t.TempDir()
	cityDir := filepath.Join(dir, "city")
	mustMkdirAll(t, cityDir, 0o755)

	writeTestFile(t, cityDir, "pack.toml", `{{not valid toml`)
	writeTestFile(t, cityDir, "city.toml", `
[workspace]
name = "test"
`)

	_, _, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityDir, "city.toml"))
	if err == nil {
		t.Fatal("expected error for malformed city pack.toml")
	}
	if !strings.Contains(err.Error(), "pack.toml") {
		t.Errorf("error should mention pack.toml; got: %v", err)
	}
}

func TestImport_RootPackRequirementFailure(t *testing.T) {
	dir := t.TempDir()
	cityDir := filepath.Join(dir, "city")
	mustMkdirAll(t, cityDir, 0o755)

	writeTestFile(t, cityDir, "city.toml", `
[workspace]
name = "test"
`)
	writeTestFile(t, cityDir, "pack.toml", `
[pack]
name = "test"
schema = 2

[[pack.requires]]
agent = "missing-agent"
scope = "city"
`)

	_, _, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityDir, "city.toml"))
	if err == nil {
		t.Fatal("expected error for unsatisfied root pack requirement")
	}
	if !strings.Contains(err.Error(), "missing-agent") {
		t.Errorf("error should mention required agent; got: %v", err)
	}
}

func TestImport_RootPackDirectServiceRejected(t *testing.T) {
	dir := t.TempDir()
	cityDir := filepath.Join(dir, "city")
	mustMkdirAll(t, cityDir, 0o755)

	writeTestFile(t, cityDir, "city.toml", `
[workspace]
name = "test"
`)
	writeTestFile(t, cityDir, "pack.toml", `
[pack]
name = "test"
schema = 2

[[service]]
name = "ui"
publish_mode = "direct"
`)

	_, _, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityDir, "city.toml"))
	if err == nil {
		t.Fatal("expected error for direct service in root pack.toml")
	}
	if !strings.Contains(err.Error(), "city pack.toml") || !strings.Contains(err.Error(), "publish_mode=direct") {
		t.Errorf("error should mention city pack.toml direct service; got: %v", err)
	}
}

func TestImport_TransitiveFalseWithExport(t *testing.T) {
	// transitive=false on an import should suppress its nested deps
	// even if the nested pack uses export=true internally.
	dir := t.TempDir()
	cityDir := filepath.Join(dir, "city")
	outerDir := filepath.Join(dir, "outer")
	innerDir := filepath.Join(dir, "inner")
	deepDir := filepath.Join(dir, "deep")

	for _, d := range []string{cityDir, outerDir, innerDir, deepDir} {
		mustMkdirAll(t, d, 0o755)
	}

	writeTestFile(t, cityDir, "city.toml", `
[workspace]
name = "test"

[imports.outer]
source = "../outer"
transitive = false
`)
	writeTestFile(t, outerDir, "pack.toml", `
[pack]
name = "outer"
schema = 1

[imports.inner]
source = "../inner"
export = true

[[agent]]
name = "outer-agent"
scope = "city"
`)
	writeTestFile(t, innerDir, "pack.toml", `
[pack]
name = "inner"
schema = 1

[imports.deep]
source = "../deep"

[[agent]]
name = "inner-agent"
scope = "city"
`)
	writeTestFile(t, deepDir, "pack.toml", `
[pack]
name = "deep"
schema = 1

[[agent]]
name = "deep-agent"
scope = "city"
`)

	cfg, _, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityDir, "city.toml"))
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}

	explicit := explicitAgents(cfg.Agents)
	found := map[string]bool{}
	for _, a := range explicit {
		found[a.QualifiedName()] = true
	}

	// outer-agent should be present (direct from outer).
	if !found["outer.outer-agent"] {
		t.Errorf("missing outer.outer-agent; got: %v", found)
	}
	// inner-agent and deep-agent should NOT be present (transitive=false
	// on the city's import of outer suppresses all nested deps).
	for qn := range found {
		if strings.Contains(qn, "inner-agent") || strings.Contains(qn, "deep-agent") {
			t.Errorf("transitive=false should block nested agents; got: %v", found)
			break
		}
	}
}

func TestImport_DeeplyNestedChain(t *testing.T) {
	// A→B→C→D→E: five-level import chain should work.
	dir := t.TempDir()
	cityDir := filepath.Join(dir, "city")
	packs := []string{"a", "b", "c", "d", "e"}

	mustMkdirAll(t, cityDir, 0o755)
	for _, name := range packs {
		mustMkdirAll(t, filepath.Join(dir, name), 0o755)
	}

	writeTestFile(t, cityDir, "city.toml", `
[workspace]
name = "test"

[imports.a]
source = "../a"
`)

	for i, name := range packs {
		var importLine string
		if i < len(packs)-1 {
			next := packs[i+1]
			importLine = "[imports." + next + "]\nsource = \"../" + next + "\"\n"
		}
		writeTestFile(t, filepath.Join(dir, name), "pack.toml", `
[pack]
name = "`+name+`"
schema = 1

`+importLine+`

[[agent]]
name = "`+name+`-agent"
scope = "city"
`)
	}

	cfg, _, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityDir, "city.toml"))
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}

	explicit := explicitAgents(cfg.Agents)
	found := map[string]bool{}
	for _, a := range explicit {
		found[a.Name] = true
	}

	for _, name := range packs {
		agentName := name + "-agent"
		if !found[agentName] {
			t.Errorf("missing %s from deep chain; got: %v", agentName, found)
		}
	}
}

func TestImport_ManyImports(t *testing.T) {
	// A city with 20 imports should work without issues.
	dir := t.TempDir()
	cityDir := filepath.Join(dir, "city")
	mustMkdirAll(t, cityDir, 0o755)

	var importLines []string
	for i := 0; i < 20; i++ {
		name := "pack" + string(rune('a'+i))
		packDir := filepath.Join(dir, name)
		mustMkdirAll(t, packDir, 0o755)
		writeTestFile(t, packDir, "pack.toml", `
[pack]
name = "`+name+`"
schema = 1

[[agent]]
name = "agent"
scope = "city"
`)
		importLines = append(importLines, "[imports."+name+"]\nsource = \"../"+name+"\"")
	}

	writeTestFile(t, cityDir, "city.toml", `
[workspace]
name = "test"

`+strings.Join(importLines, "\n\n"))

	cfg, _, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityDir, "city.toml"))
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}

	explicit := explicitAgents(cfg.Agents)
	if len(explicit) != 20 {
		t.Errorf("expected 20 agents from 20 imports, got %d", len(explicit))
	}
}

func TestImport_EmptyImportSource(t *testing.T) {
	// An import with empty source should produce a clear error.
	dir := t.TempDir()
	cityDir := filepath.Join(dir, "city")
	mustMkdirAll(t, cityDir, 0o755)

	writeTestFile(t, cityDir, "city.toml", `
[workspace]
name = "test"

[imports.bad]
source = ""
`)

	_, _, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityDir, "city.toml"))
	if err == nil {
		t.Fatal("expected error for empty import source")
	}
}

func TestImport_FullV2KitchenSink(t *testing.T) {
	// Exercises all V2 features simultaneously:
	// - pack.toml as definition layer
	// - city.toml as deployment layer
	// - Convention-based agent discovery (agents/ dirs)
	// - [imports.X] with qualified names
	// - Transitive imports (default)
	// - transitive=false on one import
	// - export=true re-export with flattening
	// - Shadow warning (city agent masks import)
	// - Rig imports
	// - Named session from import
	// - depends_on with binding rewrite
	dir := t.TempDir()
	cityDir := filepath.Join(dir, "city")
	gasDir := filepath.Join(dir, "gastown")
	utilDir := filepath.Join(dir, "util")
	privateDir := filepath.Join(dir, "private")

	for _, d := range []string{
		cityDir,
		gasDir,
		filepath.Join(gasDir, "agents", "mayor"),
		filepath.Join(gasDir, "agents", "polecat"),
		utilDir,
		privateDir,
	} {
		mustMkdirAll(t, d, 0o755)
	}

	// City pack.toml: definition with imports.
	writeTestFile(t, cityDir, "pack.toml", `
[pack]
name = "kitchen-sink"
schema = 1

[imports.gs]
source = "../gastown"

[imports.priv]
source = "../private"
transitive = false

[[agent]]
name = "mayor"
scope = "city"
`)
	// City city.toml: deployment with rig.
	writeTestFile(t, cityDir, "city.toml", `
[workspace]
name = "kitchen-sink"
provider = "claude"

[[rigs]]
name = "api"
path = "/tmp/api"

[rigs.imports.gs]
source = "../gastown"
`)
	// Gastown pack: has agents/ dirs, imports util with export, named session.
	writeTestFile(t, gasDir, "pack.toml", `
[pack]
name = "gastown"
schema = 1

[imports.util]
source = "../util"
export = true

[[named_session]]
template = "mayor"
mode = "always"
scope = "city"
`)
	writeTestFile(t, filepath.Join(gasDir, "agents", "mayor"), "agent.toml", `
scope = "city"
provider = "claude"
`)
	writeTestFile(t, filepath.Join(gasDir, "agents", "mayor"), "prompt.md", `Gastown mayor.`)
	writeTestFile(t, filepath.Join(gasDir, "agents", "polecat"), "agent.toml", `
scope = "rig"
depends_on = ["db"]
`)
	writeTestFile(t, filepath.Join(gasDir, "agents", "polecat"), "prompt.md", `Polecat.`)

	// Util pack: provides a rig-scoped db agent (transitive through gastown).
	writeTestFile(t, utilDir, "pack.toml", `
[pack]
name = "util"
schema = 1

[[agent]]
name = "db"
scope = "rig"
`)
	// Private pack: has a nested dep that should be blocked by transitive=false.
	writeTestFile(t, privateDir, "pack.toml", `
[pack]
name = "private"
schema = 1

[imports.util]
source = "../util"

[[agent]]
name = "secret"
scope = "city"
`)

	cfg, prov, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityDir, "city.toml"))
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}

	explicit := explicitAgents(cfg.Agents)
	qnames := map[string]bool{}
	for _, a := range explicit {
		qnames[a.QualifiedName()] = true
	}

	// City's own mayor (no binding).
	if !qnames["mayor"] {
		t.Errorf("missing city mayor; got: %v", qnames)
	}
	// Gastown mayor from import (convention-discovered).
	if !qnames["gs.mayor"] {
		t.Errorf("missing gs.mayor; got: %v", qnames)
	}
	// Shadow warning for mayor.
	hasShadow := false
	for _, w := range prov.Warnings {
		if strings.Contains(w, "shadows") && strings.Contains(w, "mayor") {
			hasShadow = true
		}
	}
	if !hasShadow {
		t.Errorf("expected shadow warning for mayor; warnings: %v", prov.Warnings)
	}
	// Rig-scoped polecat from gastown under rig "api".
	if !qnames["api/gs.polecat"] {
		t.Errorf("missing api/gs.polecat; got: %v", qnames)
	}
	// Rig-scoped db from util (transitive through gastown export).
	if !qnames["api/gs.db"] {
		t.Errorf("missing api/gs.db; got: %v", qnames)
	}
	// Private secret agent should be present.
	if !qnames["priv.secret"] {
		t.Errorf("missing priv.secret; got: %v", qnames)
	}
	// Private's transitive dep (util.db) should NOT be present at city
	// level because transitive=false. (It would be rig-scoped anyway,
	// but let's verify no unexpected agents leak through.)
	for qn := range qnames {
		if strings.Contains(qn, "priv.db") {
			t.Errorf("priv.db should not appear (transitive=false); got: %v", qnames)
		}
	}
	// Named session from gastown import (references mayor, which is city-scoped).
	nsFound := false
	for _, ns := range cfg.NamedSessions {
		if ns.Template == "mayor" && ns.BindingName == "gs" {
			nsFound = true
			if ns.QualifiedName() != "gs.mayor" {
				t.Errorf("named session QN = %q, want %q", ns.QualifiedName(), "gs.mayor")
			}
		}
	}
	if !nsFound {
		t.Error("named session gs.mayor not found")
	}
	// Polecat depends_on should be rewritten to "api/gs.db".
	for _, a := range explicit {
		if a.Name == "polecat" && a.Dir == "api" {
			if len(a.DependsOn) != 1 || a.DependsOn[0] != "api/gs.db" {
				t.Errorf("polecat DependsOn = %v, want [api/gs.db]", a.DependsOn)
			}
		}
	}
}

func TestImport_RigImportRejectsServices(t *testing.T) {
	// Services from rig imports should be rejected (city-scoped only).
	dir := t.TempDir()
	cityDir := filepath.Join(dir, "city")
	packDir := filepath.Join(dir, "svcpack")

	for _, d := range []string{cityDir, packDir} {
		mustMkdirAll(t, d, 0o755)
	}

	writeTestFile(t, cityDir, "city.toml", `
[workspace]
name = "test"

[[rigs]]
name = "proj"
path = "/tmp/proj"

[rigs.imports.svc]
source = "../svcpack"
`)
	writeTestFile(t, packDir, "pack.toml", `
[pack]
name = "svcpack"
schema = 1

[[service]]
name = "webhook"
kind = "workflow"

[[agent]]
name = "worker"
scope = "rig"
`)

	_, _, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityDir, "city.toml"))
	if err == nil {
		t.Fatal("expected error for service in rig import")
	}
	if !strings.Contains(err.Error(), "service") {
		t.Errorf("error should mention service; got: %v", err)
	}
}

func TestImport_RigImportRequirementFailure(t *testing.T) {
	// When an imported rig pack requires an agent that doesn't exist,
	// it should produce a clear error.
	dir := t.TempDir()
	cityDir := filepath.Join(dir, "city")
	packDir := filepath.Join(dir, "reqpack")

	for _, d := range []string{cityDir, packDir} {
		mustMkdirAll(t, d, 0o755)
	}

	writeTestFile(t, cityDir, "city.toml", `
[workspace]
name = "test"

[[rigs]]
name = "proj"
path = "/tmp/proj"

[rigs.imports.req]
source = "../reqpack"
`)
	writeTestFile(t, packDir, "pack.toml", `
[pack]
name = "reqpack"
schema = 1

[[pack.requires]]
agent = "missing-agent"
scope = "rig"

[[agent]]
name = "worker"
scope = "rig"
`)

	_, _, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityDir, "city.toml"))
	if err == nil {
		t.Fatal("expected error for unsatisfied pack requirement")
	}
	if !strings.Contains(err.Error(), "missing-agent") {
		t.Errorf("error should mention required agent; got: %v", err)
	}
}

func TestImport_PackMissingName(t *testing.T) {
	// A pack with no [pack].name should produce a clear error.
	dir := t.TempDir()
	cityDir := filepath.Join(dir, "city")
	packDir := filepath.Join(dir, "noname")

	for _, d := range []string{cityDir, packDir} {
		mustMkdirAll(t, d, 0o755)
	}

	writeTestFile(t, cityDir, "city.toml", `
[workspace]
name = "test"

[imports.bad]
source = "../noname"
`)
	writeTestFile(t, packDir, "pack.toml", `
[pack]
schema = 1
`)

	_, _, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityDir, "city.toml"))
	if err == nil {
		t.Fatal("expected error for pack with no name")
	}
	if !strings.Contains(err.Error(), "name") {
		t.Errorf("error should mention name; got: %v", err)
	}
}

func TestImport_AgentDiscoveryWithNoPromptOrToml(t *testing.T) {
	// An agents/<name>/ directory with neither prompt.md nor agent.toml
	// should still create an agent (minimal discovery).
	dir := t.TempDir()
	packDir := filepath.Join(dir, "mypk")
	agentDir := filepath.Join(packDir, "agents", "minimal")
	mustMkdirAll(t, agentDir, 0o755)

	writeTestFile(t, packDir, "pack.toml", `
[pack]
name = "mypk"
schema = 1
`)
	// Create a file inside the agent dir (not prompt.md or agent.toml)
	// to prove the dir exists but has no standard files.
	writeTestFile(t, agentDir, "notes.txt", "just a note")

	cityDir := filepath.Join(dir, "city")
	mustMkdirAll(t, cityDir, 0o755)
	writeTestFile(t, cityDir, "city.toml", `
[workspace]
name = "test"
includes = ["../mypk"]
`)

	cfg, _, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityDir, "city.toml"))
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}

	explicit := explicitAgents(cfg.Agents)
	found := false
	for _, a := range explicit {
		if a.Name == "minimal" {
			found = true
			if a.PromptTemplate != "" {
				t.Errorf("minimal agent should have no prompt template, got %q", a.PromptTemplate)
			}
			break
		}
	}
	if !found {
		t.Error("minimal agent should still be discovered from empty agents/ subdir")
	}
}
