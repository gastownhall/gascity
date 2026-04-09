package config

// Tests for V2 [imports.X] support in pack.toml.
// These test the new import schema parsing, binding-name stamping,
// and qualified name generation that form the foundation of #360.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/gastownhall/gascity/internal/fsys"
)

// tomlDecode wraps toml.Decode for test use.
func tomlDecode(data string, v interface{}) (toml.MetaData, error) {
	return toml.Decode(data, v)
}

// writeTestFile creates a file at dir/name with the given content.
func writeTestFile(t *testing.T, dir, name, content string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestImport_BasicLocalPath(t *testing.T) {
	dir := t.TempDir()
	cityDir := filepath.Join(dir, "city")
	packDir := filepath.Join(dir, "city", "assets", "imports", "mypk")
	os.MkdirAll(packDir, 0o755)

	writeTestFile(t, cityDir, "city.toml", `
[workspace]
name = "test"
includes = ["assets/imports/mypk"]
`)
	writeTestFile(t, packDir, "pack.toml", `
[pack]
name = "mypk"
schema = 1

[imports.helper]
source = "../helper"

[[agent]]
name = "worker"
scope = "city"
`)

	// Create the helper pack that mypk imports.
	helperDir := filepath.Join(dir, "city", "assets", "imports", "helper")
	os.MkdirAll(helperDir, 0o755)
	writeTestFile(t, helperDir, "pack.toml", `
[pack]
name = "helper"
schema = 1

[[agent]]
name = "assist"
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

	// The "worker" agent is from mypk directly (no binding name since it comes
	// via the old includes path on the city, not via [imports]).
	if !found["worker"] {
		t.Error("missing worker agent from mypk (via includes)")
	}

	// The "assist" agent should have binding name "helper" from [imports.helper].
	if !found["helper.assist"] {
		t.Errorf("missing helper.assist agent; got qualified names: %v", found)
	}
}

func TestImport_BindingNameStamped(t *testing.T) {
	dir := t.TempDir()
	cityDir := filepath.Join(dir, "city")
	packDir := filepath.Join(dir, "city", "mypk")
	os.MkdirAll(packDir, 0o755)

	writeTestFile(t, cityDir, "city.toml", `
[workspace]
name = "test"
includes = ["mypk"]
`)
	writeTestFile(t, packDir, "pack.toml", `
[pack]
name = "mypk"
schema = 1

[imports.gs]
source = "../gastown"

[[agent]]
name = "own-agent"
scope = "city"
`)

	// gastown lives at city/gastown/ so "../gastown" from city/mypk/ resolves correctly.
	gasDir := filepath.Join(dir, "city", "gastown")
	os.MkdirAll(gasDir, 0o755)
	writeTestFile(t, gasDir, "pack.toml", `
[pack]
name = "gastown"
schema = 1

[[agent]]
name = "mayor"
scope = "city"
`)

	cfg, _, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityDir, "city.toml"))
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}

	explicit := explicitAgents(cfg.Agents)
	for _, a := range explicit {
		if a.Name == "mayor" {
			if a.BindingName != "gs" {
				t.Errorf("mayor BindingName = %q, want %q", a.BindingName, "gs")
			}
			if a.PackName != "gastown" {
				t.Errorf("mayor PackName = %q, want %q", a.PackName, "gastown")
			}
			if a.QualifiedName() != "gs.mayor" {
				t.Errorf("mayor QualifiedName = %q, want %q", a.QualifiedName(), "gs.mayor")
			}
			return
		}
	}
	t.Error("mayor agent not found")
}

func TestImport_QualifiedNameWithRig(t *testing.T) {
	dir := t.TempDir()
	cityDir := filepath.Join(dir, "city")
	packDir := filepath.Join(dir, "city", "mypk")
	gasDir := filepath.Join(dir, "gastown")

	for _, d := range []string{cityDir, packDir, gasDir} {
		os.MkdirAll(d, 0o755)
	}

	writeTestFile(t, cityDir, "city.toml", `
[workspace]
name = "test"

[[rigs]]
name = "proj"
path = "/tmp/proj"
includes = ["mypk"]
`)
	writeTestFile(t, packDir, "pack.toml", `
[pack]
name = "mypk"
schema = 1

[imports.gs]
source = "../../gastown"

[[agent]]
name = "own-rig-agent"
scope = "rig"
`)
	writeTestFile(t, gasDir, "pack.toml", `
[pack]
name = "gastown"
schema = 1

[[agent]]
name = "polecat"
scope = "rig"
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

	// Rig-scoped agent from import: "proj/gs.polecat"
	if !found["proj/gs.polecat"] {
		t.Errorf("missing proj/gs.polecat; got: %v", found)
	}
}

func TestImport_ExportFlattensBinding(t *testing.T) {
	dir := t.TempDir()
	cityDir := filepath.Join(dir, "city")
	outerDir := filepath.Join(dir, "outer")
	innerDir := filepath.Join(dir, "inner")

	for _, d := range []string{cityDir, outerDir, innerDir} {
		os.MkdirAll(d, 0o755)
	}

	writeTestFile(t, cityDir, "city.toml", `
[workspace]
name = "test"
includes = ["../outer"]
`)
	// outer imports inner with export = true
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
	bindings := map[string]string{}
	for _, a := range explicit {
		found[a.QualifiedName()] = true
		bindings[a.Name] = a.BindingName
	}

	// deep-agent was re-exported from outer → its binding should be
	// flattened to "inner" (the immediate binding in outer's [imports.inner]).
	// But since the city includes outer via V1 includes (not [imports]),
	// outer itself has no binding name. The deep-agent gets binding "inner"
	// from outer's import declaration.
	if bindings["deep-agent"] != "inner" {
		t.Errorf("deep-agent BindingName = %q, want %q (flattened from export)", bindings["deep-agent"], "inner")
	}
}

func TestImport_CycleDetected(t *testing.T) {
	dir := t.TempDir()
	cityDir := filepath.Join(dir, "city")
	packA := filepath.Join(dir, "pack-a")
	packB := filepath.Join(dir, "pack-b")

	for _, d := range []string{cityDir, packA, packB} {
		os.MkdirAll(d, 0o755)
	}

	writeTestFile(t, cityDir, "city.toml", `
[workspace]
name = "test"
includes = ["../pack-a"]
`)
	writeTestFile(t, packA, "pack.toml", `
[pack]
name = "pack-a"
schema = 1

[imports.b]
source = "../pack-b"
`)
	writeTestFile(t, packB, "pack.toml", `
[pack]
name = "pack-b"
schema = 1

[imports.a]
source = "../pack-a"
`)

	_, _, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityDir, "city.toml"))
	if err == nil {
		t.Fatal("expected error for import cycle")
	}
	if !strings.Contains(err.Error(), "cycle") {
		t.Errorf("error = %q, want mention of cycle", err)
	}
}

func TestImport_TransitiveDefault(t *testing.T) {
	// By default, imports are transitive: if A imports B and B imports C,
	// importing A gives you C's agents too.
	dir := t.TempDir()
	cityDir := filepath.Join(dir, "city")
	packA := filepath.Join(dir, "pack-a")
	packB := filepath.Join(dir, "pack-b")
	packC := filepath.Join(dir, "pack-c")

	for _, d := range []string{cityDir, packA, packB, packC} {
		os.MkdirAll(d, 0o755)
	}

	writeTestFile(t, cityDir, "city.toml", `
[workspace]
name = "test"
includes = ["../pack-a"]
`)
	writeTestFile(t, packA, "pack.toml", `
[pack]
name = "pack-a"
schema = 1

[imports.b]
source = "../pack-b"
`)
	writeTestFile(t, packB, "pack.toml", `
[pack]
name = "pack-b"
schema = 1

[imports.c]
source = "../pack-c"
`)
	writeTestFile(t, packC, "pack.toml", `
[pack]
name = "pack-c"
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
	found := false
	for _, a := range explicit {
		if a.Name == "deep-agent" {
			found = true
			// Should have binding from the chain.
			if a.BindingName != "c" {
				t.Errorf("deep-agent BindingName = %q, want %q", a.BindingName, "c")
			}
			break
		}
	}
	if !found {
		t.Error("deep-agent not found — transitive imports should be on by default")
	}
}

func TestImport_ParseImportStruct(t *testing.T) {
	// Test that the Import struct parses correctly from TOML.
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`
[workspace]
name = "test"
`)
	fs.Files["/city/pack-a/pack.toml"] = []byte(`
[pack]
name = "pack-a"
schema = 1

[imports.gastown]
source = "./gs"
version = "^1.2"
export = true
shadow = "silent"
`)

	// Just verify the TOML parses — this test doesn't need pack resolution.
	// We'll parse manually rather than going through LoadWithIncludes.
	data := `
[pack]
name = "test-pack"
schema = 1

[imports.gastown]
source = "./gs"
version = "^1.2"
export = true
shadow = "silent"

[imports.util]
source = "../util"
transitive = false
`
	var tc struct {
		Pack    PackMeta          `toml:"pack"`
		Imports map[string]Import `toml:"imports,omitempty"`
	}
	if _, err := tomlDecode(data, &tc); err != nil {
		t.Fatalf("TOML parse: %v", err)
	}

	if len(tc.Imports) != 2 {
		t.Fatalf("len(Imports) = %d, want 2", len(tc.Imports))
	}

	gs := tc.Imports["gastown"]
	if gs.Source != "./gs" {
		t.Errorf("gastown.Source = %q, want %q", gs.Source, "./gs")
	}
	if gs.Version != "^1.2" {
		t.Errorf("gastown.Version = %q, want %q", gs.Version, "^1.2")
	}
	if !gs.Export {
		t.Error("gastown.Export should be true")
	}
	if gs.Shadow != "silent" {
		t.Errorf("gastown.Shadow = %q, want %q", gs.Shadow, "silent")
	}
	if !gs.ImportIsTransitive() {
		t.Error("gastown should be transitive by default (nil)")
	}

	util := tc.Imports["util"]
	if util.Source != "../util" {
		t.Errorf("util.Source = %q, want %q", util.Source, "../util")
	}
	if util.ImportIsTransitive() {
		t.Error("util should not be transitive (explicit false)")
	}
}

func TestImport_CityLevelImports(t *testing.T) {
	// A city declares [imports.X] directly in city.toml.
	// The loader should resolve them and produce agents with binding names.
	dir := t.TempDir()
	cityDir := filepath.Join(dir, "city")
	gasDir := filepath.Join(dir, "gastown")
	maintDir := filepath.Join(dir, "maint")

	for _, d := range []string{cityDir, gasDir, maintDir} {
		os.MkdirAll(d, 0o755)
	}

	writeTestFile(t, cityDir, "city.toml", `
[workspace]
name = "test"

[imports.gastown]
source = "../gastown"

[imports.maint]
source = "../maint"

[[agent]]
name = "mayor"
scope = "city"
`)
	writeTestFile(t, gasDir, "pack.toml", `
[pack]
name = "gastown"
schema = 1

[[agent]]
name = "polecat"
scope = "city"
`)
	writeTestFile(t, maintDir, "pack.toml", `
[pack]
name = "maintenance"
schema = 1

[[agent]]
name = "dog"
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

	// City's own agent has no binding.
	if !found["mayor"] {
		t.Errorf("missing mayor; got: %v", found)
	}
	// Import agents have binding names.
	if !found["gastown.polecat"] {
		t.Errorf("missing gastown.polecat; got: %v", found)
	}
	if !found["maint.dog"] {
		t.Errorf("missing maint.dog; got: %v", found)
	}
}

func TestImport_CityLevelImportsWithRig(t *testing.T) {
	// City-level imports should produce city-scoped agents only.
	// Rig-scoped agents from imports should not appear at city level.
	dir := t.TempDir()
	cityDir := filepath.Join(dir, "city")
	gasDir := filepath.Join(dir, "gastown")

	for _, d := range []string{cityDir, gasDir} {
		os.MkdirAll(d, 0o755)
	}

	writeTestFile(t, cityDir, "city.toml", `
[workspace]
name = "test"

[imports.gs]
source = "../gastown"
`)
	writeTestFile(t, gasDir, "pack.toml", `
[pack]
name = "gastown"
schema = 1

[[agent]]
name = "mayor"
scope = "city"

[[agent]]
name = "polecat"
scope = "rig"
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

	// City-scoped import agent should appear.
	if !found["gs.mayor"] {
		t.Errorf("missing gs.mayor; got: %v", found)
	}
	// Rig-scoped import agent should NOT appear at city level.
	if found["gs.polecat"] {
		t.Error("gs.polecat should not appear at city level (scope=rig)")
	}
}

func TestImport_CityImportsCoexistWithIncludes(t *testing.T) {
	// V1 includes and V2 imports should work together in the same city.
	dir := t.TempDir()
	cityDir := filepath.Join(dir, "city")
	includeDir := filepath.Join(dir, "city", "old-pack")
	importDir := filepath.Join(dir, "new-pack")

	for _, d := range []string{cityDir, includeDir, importDir} {
		os.MkdirAll(d, 0o755)
	}

	writeTestFile(t, cityDir, "city.toml", `
[workspace]
name = "test"
includes = ["old-pack"]

[imports.newpk]
source = "../new-pack"
`)
	writeTestFile(t, includeDir, "pack.toml", `
[pack]
name = "old-pack"
schema = 1

[[agent]]
name = "old-agent"
scope = "city"
`)
	writeTestFile(t, importDir, "pack.toml", `
[pack]
name = "new-pack"
schema = 1

[[agent]]
name = "new-agent"
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

	// V1 include agent: no binding name.
	if !found["old-agent"] {
		t.Errorf("missing old-agent from V1 include; got: %v", found)
	}
	// V2 import agent: has binding name.
	if !found["newpk.new-agent"] {
		t.Errorf("missing newpk.new-agent from V2 import; got: %v", found)
	}
}

func TestImport_RigLevelImports(t *testing.T) {
	// Rigs can declare [imports.X] to get rig-scoped agents with
	// qualified names like "proj/gastown.polecat".
	dir := t.TempDir()
	cityDir := filepath.Join(dir, "city")
	gasDir := filepath.Join(dir, "gastown")

	for _, d := range []string{cityDir, gasDir} {
		os.MkdirAll(d, 0o755)
	}

	writeTestFile(t, cityDir, "city.toml", `
[workspace]
name = "test"

[[rigs]]
name = "proj"
path = "/tmp/proj"

[rigs.imports.gs]
source = "../gastown"
`)
	writeTestFile(t, gasDir, "pack.toml", `
[pack]
name = "gastown"
schema = 1

[[agent]]
name = "polecat"
scope = "rig"

[[agent]]
name = "mayor"
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

	// Rig-scoped agent should appear with binding + rig prefix.
	if !found["proj/gs.polecat"] {
		t.Errorf("missing proj/gs.polecat; got: %v", found)
	}
	// City-scoped agent should NOT appear from rig import.
	if found["gs.mayor"] || found["proj/gs.mayor"] {
		t.Error("city-scoped mayor should not appear from rig-level import")
	}
}

func TestImport_RigImportsCoexistWithIncludes(t *testing.T) {
	// V1 rig includes and V2 rig imports should work together.
	dir := t.TempDir()
	cityDir := filepath.Join(dir, "city")
	oldPack := filepath.Join(dir, "city", "old-pack")
	newPack := filepath.Join(dir, "new-pack")

	for _, d := range []string{cityDir, oldPack, newPack} {
		os.MkdirAll(d, 0o755)
	}

	writeTestFile(t, cityDir, "city.toml", `
[workspace]
name = "test"

[[rigs]]
name = "proj"
path = "/tmp/proj"
includes = ["old-pack"]

[rigs.imports.newpk]
source = "../new-pack"
`)
	writeTestFile(t, oldPack, "pack.toml", `
[pack]
name = "old-pack"
schema = 1

[[agent]]
name = "old-agent"
scope = "rig"
`)
	writeTestFile(t, newPack, "pack.toml", `
[pack]
name = "new-pack"
schema = 1

[[agent]]
name = "new-agent"
scope = "rig"
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

	// V1 include: no binding name.
	if !found["proj/old-agent"] {
		t.Errorf("missing proj/old-agent from V1 include; got: %v", found)
	}
	// V2 import: has binding name.
	if !found["proj/newpk.new-agent"] {
		t.Errorf("missing proj/newpk.new-agent from V2 import; got: %v", found)
	}
}

func TestImport_ShadowWarningEmitted(t *testing.T) {
	// When a city-local agent has the same bare name as an imported agent,
	// a shadow warning should be emitted.
	dir := t.TempDir()
	cityDir := filepath.Join(dir, "city")
	gasDir := filepath.Join(dir, "gastown")

	for _, d := range []string{cityDir, gasDir} {
		os.MkdirAll(d, 0o755)
	}

	writeTestFile(t, cityDir, "city.toml", `
[workspace]
name = "test"

[imports.gastown]
source = "../gastown"

[[agent]]
name = "mayor"
scope = "city"
`)
	writeTestFile(t, gasDir, "pack.toml", `
[pack]
name = "gastown"
schema = 1

[[agent]]
name = "mayor"
scope = "city"
`)

	_, prov, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityDir, "city.toml"))
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}

	// Should have a shadow warning.
	found := false
	for _, w := range prov.Warnings {
		if strings.Contains(w, "shadows") && strings.Contains(w, "mayor") && strings.Contains(w, "gastown") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected shadow warning for mayor; warnings = %v", prov.Warnings)
	}
}

func TestImport_ShadowWarningSuppressed(t *testing.T) {
	// When shadow = "silent" is set on the import, no warning should be emitted.
	dir := t.TempDir()
	cityDir := filepath.Join(dir, "city")
	gasDir := filepath.Join(dir, "gastown")

	for _, d := range []string{cityDir, gasDir} {
		os.MkdirAll(d, 0o755)
	}

	writeTestFile(t, cityDir, "city.toml", `
[workspace]
name = "test"

[imports.gastown]
source = "../gastown"
shadow = "silent"

[[agent]]
name = "mayor"
scope = "city"
`)
	writeTestFile(t, gasDir, "pack.toml", `
[pack]
name = "gastown"
schema = 1

[[agent]]
name = "mayor"
scope = "city"
`)

	_, prov, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityDir, "city.toml"))
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}

	// Should NOT have a shadow warning.
	for _, w := range prov.Warnings {
		if strings.Contains(w, "shadows") && strings.Contains(w, "mayor") {
			t.Errorf("shadow warning should be suppressed with shadow=silent; got: %s", w)
		}
	}
}

func TestImport_DiamondDAGNoCycle(t *testing.T) {
	// A→B, A→C, B→D, C→D should NOT be a cycle error.
	dir := t.TempDir()
	cityDir := filepath.Join(dir, "city")
	for _, name := range []string{"city", "b", "c", "d"} {
		os.MkdirAll(filepath.Join(dir, name), 0o755)
	}

	writeTestFile(t, cityDir, "city.toml", `
[workspace]
name = "test"

[imports.b]
source = "../b"

[imports.c]
source = "../c"
`)
	writeTestFile(t, filepath.Join(dir, "b"), "pack.toml", `
[pack]
name = "b"
schema = 1

[imports.d]
source = "../d"
`)
	writeTestFile(t, filepath.Join(dir, "c"), "pack.toml", `
[pack]
name = "c"
schema = 1

[imports.d]
source = "../d"
`)
	writeTestFile(t, filepath.Join(dir, "d"), "pack.toml", `
[pack]
name = "d"
schema = 1

[[agent]]
name = "shared"
scope = "city"
`)

	cfg, _, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityDir, "city.toml"))
	if err != nil {
		t.Fatalf("diamond DAG should not error: %v", err)
	}

	// shared agent should be present (from D via both B and C).
	explicit := explicitAgents(cfg.Agents)
	found := false
	for _, a := range explicit {
		if a.Name == "shared" {
			found = true
			break
		}
	}
	if !found {
		t.Error("shared agent from diamond dep D not found")
	}
}

func TestImport_SameNameDifferentBindings(t *testing.T) {
	// Two imports both define "mayor" — should NOT collide because they
	// have different binding names (gs.mayor vs maint.mayor).
	dir := t.TempDir()
	cityDir := filepath.Join(dir, "city")
	for _, name := range []string{"city", "gastown", "maint"} {
		os.MkdirAll(filepath.Join(dir, name), 0o755)
	}

	writeTestFile(t, cityDir, "city.toml", `
[workspace]
name = "test"

[imports.gs]
source = "../gastown"

[imports.maint]
source = "../maint"
`)
	writeTestFile(t, filepath.Join(dir, "gastown"), "pack.toml", `
[pack]
name = "gastown"
schema = 1

[[agent]]
name = "mayor"
scope = "city"
`)
	writeTestFile(t, filepath.Join(dir, "maint"), "pack.toml", `
[pack]
name = "maint"
schema = 1

[[agent]]
name = "mayor"
scope = "city"
`)

	cfg, _, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityDir, "city.toml"))
	if err != nil {
		t.Fatalf("same name under different bindings should not error: %v", err)
	}

	explicit := explicitAgents(cfg.Agents)
	found := map[string]bool{}
	for _, a := range explicit {
		found[a.QualifiedName()] = true
	}
	if !found["gs.mayor"] {
		t.Errorf("missing gs.mayor; got: %v", found)
	}
	if !found["maint.mayor"] {
		t.Errorf("missing maint.mayor; got: %v", found)
	}
}

func TestImport_TransitiveFalseBlocksNested(t *testing.T) {
	// A imports B with transitive=false. B imports C.
	// C's agents should NOT appear.
	dir := t.TempDir()
	cityDir := filepath.Join(dir, "city")
	for _, name := range []string{"city", "b", "c"} {
		os.MkdirAll(filepath.Join(dir, name), 0o755)
	}

	writeTestFile(t, cityDir, "city.toml", `
[workspace]
name = "test"

[imports.b]
source = "../b"
transitive = false
`)
	writeTestFile(t, filepath.Join(dir, "b"), "pack.toml", `
[pack]
name = "b"
schema = 1

[imports.c]
source = "../c"

[[agent]]
name = "direct"
scope = "city"
`)
	writeTestFile(t, filepath.Join(dir, "c"), "pack.toml", `
[pack]
name = "c"
schema = 1

[[agent]]
name = "transitive"
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

	// Direct agent from B should be present.
	if !found["b.direct"] {
		t.Errorf("missing b.direct; got: %v", found)
	}
	// Transitive agent from C should NOT be present.
	for qn := range found {
		if strings.Contains(qn, "transitive") {
			t.Errorf("transitive agent should be blocked by transitive=false; got: %v", found)
		}
	}
}

func TestImport_MissingCityImportIsFatal(t *testing.T) {
	// A typo in [imports.X].source should be a hard error, not silently skipped.
	dir := t.TempDir()
	cityDir := filepath.Join(dir, "city")
	os.MkdirAll(cityDir, 0o755)

	writeTestFile(t, cityDir, "city.toml", `
[workspace]
name = "test"

[imports.typo]
source = "../nonexistent"
`)

	_, _, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityDir, "city.toml"))
	if err == nil {
		t.Fatal("expected error for missing import source")
	}
	if !strings.Contains(err.Error(), "typo") {
		t.Errorf("error should mention the binding name; got: %v", err)
	}
}

func TestQualifiedName_WithBindingName(t *testing.T) {
	tests := []struct {
		name        string
		agent       Agent
		wantQN      string
	}{
		{
			name:   "bare name, no binding, no dir",
			agent:  Agent{Name: "mayor"},
			wantQN: "mayor",
		},
		{
			name:   "with dir, no binding",
			agent:  Agent{Name: "mayor", Dir: "proj"},
			wantQN: "proj/mayor",
		},
		{
			name:   "with binding, no dir",
			agent:  Agent{Name: "mayor", BindingName: "gastown"},
			wantQN: "gastown.mayor",
		},
		{
			name:   "with binding and dir",
			agent:  Agent{Name: "polecat", BindingName: "gastown", Dir: "proj"},
			wantQN: "proj/gastown.polecat",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.agent.QualifiedName()
			if got != tt.wantQN {
				t.Errorf("QualifiedName() = %q, want %q", got, tt.wantQN)
			}
		})
	}
}
