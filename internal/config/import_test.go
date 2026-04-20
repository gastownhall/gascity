package config

// Tests for V2 [imports.X] support in pack.toml.
// These test the new import schema parsing, binding-name stamping,
// and qualified name generation that form the foundation of #360.

import (
	"crypto/sha256"
	"fmt"
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
	mustMkdirAll(t, filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

//nolint:unparam // test helper keeps the permission explicit at each call site.
func mustMkdirAll(t *testing.T, path string, perm os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(path, perm); err != nil {
		t.Fatalf("MkdirAll(%q): %v", path, err)
	}
}

func TestImport_BasicLocalPath(t *testing.T) {
	dir := t.TempDir()
	cityDir := filepath.Join(dir, "city")
	packDir := filepath.Join(dir, "city", "assets", "imports", "mypk")
	mustMkdirAll(t, packDir, 0o755)

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
	mustMkdirAll(t, helperDir, 0o755)
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

func TestImport_AgentDefaultsDefaultSlingFormulaInherited(t *testing.T) {
	dir := t.TempDir()
	cityDir := filepath.Join(dir, "city")
	importDir := filepath.Join(dir, "tools")
	mustMkdirAll(t, cityDir, 0o755)
	mustMkdirAll(t, importDir, 0o755)

	writeTestFile(t, cityDir, "city.toml", `
[workspace]
name = "test"
`)
	writeTestFile(t, cityDir, "pack.toml", `
[pack]
name = "test"
schema = 1

[imports.tools]
source = "../tools"
`)
	writeTestFile(t, importDir, "pack.toml", `
[pack]
name = "tools"
schema = 1

[agent_defaults]
default_sling_formula = "mol-pack-default"

[[agent]]
name = "worker"
scope = "city"
`)

	cfg, _, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityDir, "city.toml"))
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}

	for _, a := range explicitAgents(cfg.Agents) {
		if a.QualifiedName() != "tools.worker" {
			continue
		}
		if a.DefaultSlingFormula != nil {
			t.Fatalf("tools.worker DefaultSlingFormula = %v, want nil explicit override", *a.DefaultSlingFormula)
		}
		if a.InheritedDefaultSlingFormula == nil || *a.InheritedDefaultSlingFormula != "mol-pack-default" {
			got := "<nil>"
			if a.InheritedDefaultSlingFormula != nil {
				got = *a.InheritedDefaultSlingFormula
			}
			t.Fatalf("tools.worker InheritedDefaultSlingFormula = %s, want %q", got, "mol-pack-default")
		}
		if got := a.EffectiveDefaultSlingFormula(); got != "mol-pack-default" {
			t.Fatalf("tools.worker EffectiveDefaultSlingFormula() = %q, want %q", got, "mol-pack-default")
		}
		return
	}
	t.Fatalf("imported agent tools.worker not found: %+v", explicitAgents(cfg.Agents))
}

func TestImport_AgentDefaultsDefaultSlingFormulaInheritedBeatsCityDefault(t *testing.T) {
	dir := t.TempDir()
	cityDir := filepath.Join(dir, "city")
	importDir := filepath.Join(dir, "tools")
	mustMkdirAll(t, cityDir, 0o755)
	mustMkdirAll(t, importDir, 0o755)

	writeTestFile(t, cityDir, "city.toml", `
[workspace]
name = "test"
`)
	writeTestFile(t, cityDir, "pack.toml", `
[pack]
name = "test"
schema = 1

[agent_defaults]
default_sling_formula = "mol-city-default"

[imports.tools]
source = "../tools"
`)
	writeTestFile(t, importDir, "pack.toml", `
[pack]
name = "tools"
schema = 1

[agent_defaults]
default_sling_formula = "mol-pack-default"

[[agent]]
name = "worker"
scope = "city"
`)

	cfg, _, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityDir, "city.toml"))
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}

	for _, a := range explicitAgents(cfg.Agents) {
		if a.QualifiedName() != "tools.worker" {
			continue
		}
		if got := a.EffectiveDefaultSlingFormula(); got != "mol-pack-default" {
			t.Fatalf("tools.worker EffectiveDefaultSlingFormula() = %q, want %q", got, "mol-pack-default")
		}
		if a.DefaultSlingFormula != nil {
			t.Fatalf("tools.worker DefaultSlingFormula = %q, want nil when city default should not override imported pack default", *a.DefaultSlingFormula)
		}
		return
	}
	t.Fatalf("imported agent tools.worker not found: %+v", explicitAgents(cfg.Agents))
}

func TestImport_BindingNameStamped(t *testing.T) {
	dir := t.TempDir()
	cityDir := filepath.Join(dir, "city")
	packDir := filepath.Join(dir, "city", "mypk")
	mustMkdirAll(t, packDir, 0o755)

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
	mustMkdirAll(t, gasDir, 0o755)
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
		mustMkdirAll(t, d, 0o755)
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
		mustMkdirAll(t, d, 0o755)
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
		mustMkdirAll(t, d, 0o755)
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
		mustMkdirAll(t, d, 0o755)
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

func TestImport_CityTomlImportsWarnWhenPackTomlExists(t *testing.T) {
	// When pack.toml exists, city.toml imports should produce a warning
	// guiding the user to move them to pack.toml. Without pack.toml,
	// city.toml imports work normally (backward compatibility).
	dir := t.TempDir()
	cityDir := filepath.Join(dir, "city")
	gasDir := filepath.Join(dir, "gastown")
	for _, d := range []string{cityDir, gasDir} {
		mustMkdirAll(t, d, 0o755)
	}

	writeTestFile(t, gasDir, "pack.toml", `
[pack]
name = "gastown"
schema = 1
`)
	// City with BOTH pack.toml and city.toml imports.
	writeTestFile(t, cityDir, "pack.toml", `
[pack]
name = "test"
schema = 1
`)
	writeTestFile(t, cityDir, "city.toml", `
[workspace]
name = "test"

[imports.gastown]
source = "../gastown"
`)

	_, prov, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityDir, "city.toml"))
	if err != nil {
		t.Fatalf("city.toml imports should work (with warning), got error: %v", err)
	}
	// Should produce a warning about moving imports to pack.toml.
	found := false
	for _, w := range prov.Warnings {
		if strings.Contains(w, "pack.toml") && strings.Contains(w, "imports") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected warning about city.toml imports when pack.toml exists; warnings: %v", prov.Warnings)
	}
}

func TestImport_RootPackRemoteImportFromLockfileCache(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	t.Setenv("HOME", home)

	cityDir := filepath.Join(dir, "city")
	mustMkdirAll(t, cityDir, 0o755)

	source := "https://github.com/example/gastown.git"
	commit := "abc123def456"
	cacheKey := fmt.Sprintf("%x", sha256.Sum256([]byte(source+commit)))
	cacheDir := filepath.Join(home, ".gc", "cache", "repos", cacheKey)
	mustMkdirAll(t, filepath.Join(cacheDir, ".git"), 0o755)

	writeTestFile(t, cityDir, "city.toml", `
[workspace]
name = "test"
`)
	writeTestFile(t, cityDir, "pack.toml", `
[pack]
name = "test"
schema = 1

[imports.gastown]
source = "https://github.com/example/gastown.git"
version = "^1.2"
`)
	writeTestFile(t, cityDir, "packs.lock", `
schema = 1

[packs."https://github.com/example/gastown.git"]
version = "1.2.3"
commit = "abc123def456"
fetched = "2026-04-10T00:00:00Z"
`)
	writeTestFile(t, cacheDir, "pack.toml", `
[pack]
name = "gastown"
schema = 1

[[agent]]
name = "polecat"
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
	if !found["gastown.polecat"] {
		t.Errorf("missing gastown.polecat; got: %v", found)
	}
}

func TestImport_RootPackRemoteImportMissingSharedCacheFails(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	t.Setenv("HOME", home)

	cityDir := filepath.Join(dir, "city")
	mustMkdirAll(t, cityDir, 0o755)

	writeTestFile(t, cityDir, "city.toml", `
[workspace]
name = "test"
`)
	writeTestFile(t, cityDir, "pack.toml", `
[pack]
name = "test"
schema = 1

[imports.gastown]
source = "https://github.com/example/gastown.git"
version = "^1.2"
`)
	writeTestFile(t, cityDir, "packs.lock", `
schema = 1

[packs."https://github.com/example/gastown.git"]
version = "1.2.3"
commit = "abc123def456"
fetched = "2026-04-10T00:00:00Z"
`)

	_, _, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityDir, "city.toml"))
	if err == nil {
		t.Fatal("expected missing shared cache error")
	}
	if !strings.Contains(err.Error(), "locked but not cached") || !strings.Contains(err.Error(), `run "gc import install"`) {
		t.Fatalf("error = %v, want locked-but-not-cached install hint", err)
	}
}

func TestImport_RootPackRemoteImportMissingLockfileSuggestsInstall(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	t.Setenv("HOME", home)

	cityDir := filepath.Join(dir, "city")
	mustMkdirAll(t, cityDir, 0o755)

	writeTestFile(t, cityDir, "city.toml", `
[workspace]
name = "test"
`)
	writeTestFile(t, cityDir, "pack.toml", `
[pack]
name = "test"
schema = 1

[imports.gastown]
source = "https://github.com/example/gastown.git"
version = "^1.2"
`)

	_, _, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityDir, "city.toml"))
	if err == nil {
		t.Fatal("expected missing lockfile error")
	}
	if !strings.Contains(err.Error(), "missing packs.lock") || !strings.Contains(err.Error(), `run "gc import install"`) {
		t.Fatalf("error = %v, want missing packs.lock install hint", err)
	}
}

func TestImport_RootPackRemoteSubpathImportFromLockfileCache(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	t.Setenv("HOME", home)

	cityDir := filepath.Join(dir, "city")
	mustMkdirAll(t, cityDir, 0o755)

	commit := "abc123def456"
	cacheKey := fmt.Sprintf("%x", sha256.Sum256([]byte("file:///tmp/repo.git"+commit)))
	cacheDir := filepath.Join(home, ".gc", "cache", "repos", cacheKey)
	mustMkdirAll(t, filepath.Join(cacheDir, ".git"), 0o755)
	mustMkdirAll(t, filepath.Join(cacheDir, "packs", "base"), 0o755)

	writeTestFile(t, cityDir, "city.toml", `
[workspace]
name = "test"
`)
	writeTestFile(t, cityDir, "pack.toml", `
[pack]
name = "test"
schema = 1

[imports.base]
source = "file:///tmp/repo.git//packs/base"
version = "^1.2"
`)
	writeTestFile(t, cityDir, "packs.lock", `
schema = 1

[packs."file:///tmp/repo.git//packs/base"]
version = "1.2.3"
commit = "abc123def456"
fetched = "2026-04-10T00:00:00Z"
`)
	writeTestFile(t, filepath.Join(cacheDir, "packs", "base"), "pack.toml", `
[pack]
name = "base"
schema = 1

[[agent]]
name = "scout"
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
	if !found["base.scout"] {
		t.Errorf("missing base.scout; got: %v", found)
	}
}

func TestImport_RootPackGitHubTreeImportFromLockfileCache(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	t.Setenv("HOME", home)

	cityDir := filepath.Join(dir, "city")
	mustMkdirAll(t, cityDir, 0o755)

	commit := "abc123def456"
	cacheKey := fmt.Sprintf("%x", sha256.Sum256([]byte("https://github.com/example/repo.git"+commit)))
	cacheDir := filepath.Join(home, ".gc", "cache", "repos", cacheKey)
	mustMkdirAll(t, filepath.Join(cacheDir, ".git"), 0o755)
	mustMkdirAll(t, filepath.Join(cacheDir, "packs", "base"), 0o755)

	writeTestFile(t, cityDir, "city.toml", `
[workspace]
name = "test"
`)
	writeTestFile(t, cityDir, "pack.toml", `
[pack]
name = "test"
schema = 1

[imports.base]
source = "https://github.com/example/repo/tree/main/packs/base"
version = "^1.2"
`)
	writeTestFile(t, cityDir, "packs.lock", `
schema = 1

[packs."https://github.com/example/repo/tree/main/packs/base"]
version = "1.2.3"
commit = "abc123def456"
fetched = "2026-04-10T00:00:00Z"
`)
	writeTestFile(t, filepath.Join(cacheDir, "packs", "base"), "pack.toml", `
[pack]
name = "base"
schema = 1

[[agent]]
name = "scout"
scope = "city"
`)

	cfg, _, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityDir, "city.toml"))
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}

	found := map[string]bool{}
	for _, a := range explicitAgents(cfg.Agents) {
		found[a.QualifiedName()] = true
	}
	if !found["base.scout"] {
		t.Errorf("missing base.scout; got: %v", found)
	}
}

func TestImport_RootPackRejectsUnknownFields(t *testing.T) {
	dir := t.TempDir()
	cityDir := filepath.Join(dir, "city")
	packDir := filepath.Join(dir, "base")

	for _, d := range []string{cityDir, packDir} {
		mustMkdirAll(t, d, 0o755)
	}

	writeTestFile(t, cityDir, "city.toml", `
[workspace]
name = "test"
`)
	writeTestFile(t, cityDir, "pack.toml", `
[pack]
name = "test"
schema = 1

[imports.base]
sorce = "../base"
`)
	writeTestFile(t, packDir, "pack.toml", `
[pack]
name = "base"
schema = 1
`)

	_, _, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityDir, "city.toml"))
	if err == nil {
		t.Fatal("expected unknown-field error for root pack.toml")
	}
	if !strings.Contains(err.Error(), `unknown field "imports.base.sorce"`) {
		t.Fatalf("error = %v, want unknown-field message", err)
	}
	if !strings.Contains(err.Error(), `did you mean "source"`) {
		t.Fatalf("error = %v, want suggestion", err)
	}
}

func TestImport_RootPackImportsWithRig(t *testing.T) {
	// Root-pack imports should produce city-scoped agents only.
	// Rig-scoped agents from imports should not appear at city level.
	dir := t.TempDir()
	cityDir := filepath.Join(dir, "city")
	gasDir := filepath.Join(dir, "gastown")

	for _, d := range []string{cityDir, gasDir} {
		mustMkdirAll(t, d, 0o755)
	}

	writeTestFile(t, cityDir, "city.toml", `
[workspace]
name = "test"
`)
	writeTestFile(t, cityDir, "pack.toml", `
[pack]
name = "test"
schema = 1

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

func TestImport_RootPackImportsCoexistWithIncludes(t *testing.T) {
	// V1 includes and root-pack V2 imports should work together in the same city.
	dir := t.TempDir()
	cityDir := filepath.Join(dir, "city")
	includeDir := filepath.Join(dir, "city", "old-pack")
	importDir := filepath.Join(dir, "new-pack")

	for _, d := range []string{cityDir, includeDir, importDir} {
		mustMkdirAll(t, d, 0o755)
	}

	writeTestFile(t, cityDir, "city.toml", `
[workspace]
name = "test"
includes = ["old-pack"]
`)
	writeTestFile(t, cityDir, "pack.toml", `
[pack]
name = "test"
schema = 1

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
		mustMkdirAll(t, d, 0o755)
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
		mustMkdirAll(t, d, 0o755)
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
		mustMkdirAll(t, d, 0o755)
	}

	writeTestFile(t, cityDir, "city.toml", `
[workspace]
name = "test"
`)
	writeTestFile(t, cityDir, "pack.toml", `
[pack]
name = "test"
schema = 1

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
		mustMkdirAll(t, d, 0o755)
	}

	writeTestFile(t, cityDir, "city.toml", `
[workspace]
name = "test"
`)
	writeTestFile(t, cityDir, "pack.toml", `
[pack]
name = "test"
schema = 1

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
		mustMkdirAll(t, filepath.Join(dir, name), 0o755)
	}

	writeTestFile(t, cityDir, "city.toml", `
[workspace]
name = "test"
`)
	writeTestFile(t, cityDir, "pack.toml", `
[pack]
name = "test"
schema = 1

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
		mustMkdirAll(t, filepath.Join(dir, name), 0o755)
	}

	writeTestFile(t, cityDir, "city.toml", `
[workspace]
name = "test"
`)
	writeTestFile(t, cityDir, "pack.toml", `
[pack]
name = "test"
schema = 1

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
		mustMkdirAll(t, filepath.Join(dir, name), 0o755)
	}

	writeTestFile(t, cityDir, "city.toml", `
[workspace]
name = "test"
`)
	writeTestFile(t, cityDir, "pack.toml", `
[pack]
name = "test"
schema = 1

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

func TestImport_MissingRootPackImportIsFatal(t *testing.T) {
	// A typo in root pack.toml [imports.X].source should be a hard error.
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
schema = 1

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

func TestImport_PackTomlAsDefinitionLayer(t *testing.T) {
	// When a city has both pack.toml and city.toml, the loader should
	// read pack.toml as the definition layer (imports, agents, providers)
	// and city.toml as the deployment layer (rigs, overrides).
	dir := t.TempDir()
	cityDir := filepath.Join(dir, "city")
	helperDir := filepath.Join(dir, "city", "assets", "helper")

	for _, d := range []string{cityDir, helperDir} {
		mustMkdirAll(t, d, 0o755)
	}

	// pack.toml: definition layer — imports and agents.
	writeTestFile(t, cityDir, "pack.toml", `
[pack]
name = "my-city"
schema = 1

[imports.helper]
source = "./assets/helper"

[[agent]]
name = "pack-agent"
scope = "city"
`)

	// city.toml: deployment layer — rigs, workspace name.
	writeTestFile(t, cityDir, "city.toml", `
[workspace]
name = "my-city"

[[agent]]
name = "city-agent"
scope = "city"
`)

	writeTestFile(t, helperDir, "pack.toml", `
[pack]
name = "helper"
schema = 1

[[agent]]
name = "assist"
scope = "city"
`)

	cfg, prov, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityDir, "city.toml"))
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}

	explicit := explicitAgents(cfg.Agents)
	found := map[string]bool{}
	for _, a := range explicit {
		found[a.QualifiedName()] = true
	}

	// Agent from pack.toml.
	if !found["pack-agent"] {
		t.Errorf("missing pack-agent from pack.toml; got: %v", found)
	}
	// Agent from city.toml.
	if !found["city-agent"] {
		t.Errorf("missing city-agent from city.toml; got: %v", found)
	}
	// Imported agent from pack.toml's [imports].
	if !found["helper.assist"] {
		t.Errorf("missing helper.assist from pack.toml import; got: %v", found)
	}
	// Provenance should include pack.toml as a source.
	packFound := false
	for _, src := range prov.Sources {
		if strings.HasSuffix(src, "pack.toml") {
			packFound = true
			break
		}
	}
	if !packFound {
		t.Errorf("pack.toml not in provenance sources: %v", prov.Sources)
	}
}

func TestImport_DependsOnRewriteWithBinding(t *testing.T) {
	// When agent gs.worker depends on "db", the rewritten dep should be
	// "gs.db" (matching the qualified name of a sibling in the same import).
	dir := t.TempDir()
	cityDir := filepath.Join(dir, "city")
	packDir := filepath.Join(dir, "mypk")

	for _, d := range []string{cityDir, packDir} {
		mustMkdirAll(t, d, 0o755)
	}

	writeTestFile(t, cityDir, "city.toml", `
[workspace]
name = "test"

[[rigs]]
name = "proj"
path = "/tmp/proj"

[rigs.imports.gs]
source = "../mypk"
`)
	writeTestFile(t, packDir, "pack.toml", `
[pack]
name = "mypk"
schema = 1

[[agent]]
name = "worker"
scope = "rig"
depends_on = ["db"]

[[agent]]
name = "db"
scope = "rig"
`)

	cfg, _, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityDir, "city.toml"))
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}

	explicit := explicitAgents(cfg.Agents)
	for _, a := range explicit {
		if a.Name == "worker" && a.Dir == "proj" {
			if len(a.DependsOn) != 1 {
				t.Fatalf("worker DependsOn = %v, want 1 entry", a.DependsOn)
			}
			// Should be qualified with binding: "proj/gs.db"
			want := "proj/gs.db"
			if a.DependsOn[0] != want {
				t.Errorf("worker DependsOn[0] = %q, want %q", a.DependsOn[0], want)
			}
			return
		}
	}
	t.Error("worker agent not found under rig proj")
}

func TestImport_NamedSessionBindingStamped(t *testing.T) {
	// Named sessions from imports should get BindingName stamped.
	dir := t.TempDir()
	cityDir := filepath.Join(dir, "city")
	packDir := filepath.Join(dir, "mypk")

	for _, d := range []string{cityDir, packDir} {
		mustMkdirAll(t, d, 0o755)
	}

	writeTestFile(t, cityDir, "city.toml", `
[workspace]
name = "test"

[imports.gs]
source = "../mypk"

[[agent]]
name = "mayor"
scope = "city"
`)
	writeTestFile(t, packDir, "pack.toml", `
[pack]
name = "mypk"
schema = 1

[[agent]]
name = "polecat"
scope = "city"

[[named_session]]
template = "polecat"
mode = "always"
scope = "city"
`)

	cfg, _, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityDir, "city.toml"))
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}

	for _, ns := range cfg.NamedSessions {
		if ns.Template == "polecat" {
			if ns.BindingName != "gs" {
				t.Errorf("NamedSession BindingName = %q, want %q", ns.BindingName, "gs")
			}
			if ns.QualifiedName() != "gs.polecat" {
				t.Errorf("NamedSession QualifiedName = %q, want %q", ns.QualifiedName(), "gs.polecat")
			}
			return
		}
	}
	t.Error("polecat named session not found")
}

func TestImport_RootNamedSessionCanTargetImportedTemplate(t *testing.T) {
	dir := t.TempDir()
	cityDir := filepath.Join(dir, "city")
	packDir := filepath.Join(dir, "mypk")

	for _, d := range []string{cityDir, packDir} {
		mustMkdirAll(t, d, 0o755)
	}

	writeTestFile(t, cityDir, "city.toml", `
[workspace]
name = "test"

[imports.gs]
source = "../mypk"

[[named_session]]
name = "witness"
template = "gs.polecat"
mode = "always"
`)
	writeTestFile(t, packDir, "pack.toml", `
[pack]
name = "mypk"
schema = 1

[[agent]]
name = "polecat"
scope = "city"
`)

	cfg, _, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityDir, "city.toml"))
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}

	named := FindNamedSession(cfg, "witness")
	if named == nil {
		t.Fatal("FindNamedSession(witness) = nil")
	}
	if named.TemplateQualifiedName() != "gs.polecat" {
		t.Fatalf("TemplateQualifiedName() = %q, want %q", named.TemplateQualifiedName(), "gs.polecat")
	}
	if got := FindAgent(cfg, named.TemplateQualifiedName()); got == nil {
		t.Fatalf("FindAgent(%q) = nil", named.TemplateQualifiedName())
	}
}

func TestImport_ReExportNestedPreservesInnerBinding(t *testing.T) {
	// outer exports inner, inner imports util (not exported).
	// Agents from inner should be flattened to outer's binding.
	// Agents from util (transitive through inner) should keep inner's binding
	// since inner did NOT export util.
	dir := t.TempDir()
	cityDir := filepath.Join(dir, "city")
	outerDir := filepath.Join(dir, "outer")
	innerDir := filepath.Join(dir, "inner")
	utilDir := filepath.Join(dir, "util")

	for _, d := range []string{cityDir, outerDir, innerDir, utilDir} {
		mustMkdirAll(t, d, 0o755)
	}

	writeTestFile(t, cityDir, "city.toml", `
[workspace]
name = "test"

[imports.out]
source = "../outer"
`)
	writeTestFile(t, outerDir, "pack.toml", `
[pack]
name = "outer"
schema = 1

[imports.inn]
source = "../inner"
export = true
`)
	writeTestFile(t, innerDir, "pack.toml", `
[pack]
name = "inner"
schema = 1

[imports.ut]
source = "../util"

[[agent]]
name = "inner-agent"
scope = "city"
`)
	writeTestFile(t, utilDir, "pack.toml", `
[pack]
name = "util"
schema = 1

[[agent]]
name = "util-agent"
scope = "city"
`)

	cfg, _, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityDir, "city.toml"))
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}

	explicit := explicitAgents(cfg.Agents)
	bindings := map[string]string{}
	for _, a := range explicit {
		bindings[a.Name] = a.BindingName
	}

	// inner-agent should be "out" — the city imported outer as "out",
	// so all agents from outer's closure get binding "out".
	if bindings["inner-agent"] != "out" {
		t.Errorf("inner-agent binding = %q, want %q", bindings["inner-agent"], "out")
	}

	// util-agent also gets "out" because the city's binding overrides
	// all nested bindings. The city sees everything through its import.
	if bindings["util-agent"] != "out" {
		t.Errorf("util-agent binding = %q, want %q (city binding overrides nested)", bindings["util-agent"], "out")
	}
}

func TestImport_SamePackTwoBindings(t *testing.T) {
	// The same pack imported under two different bindings should
	// produce agents with both bindings (cache returns copies).
	dir := t.TempDir()
	cityDir := filepath.Join(dir, "city")
	packDir := filepath.Join(dir, "shared")

	for _, d := range []string{cityDir, packDir} {
		mustMkdirAll(t, d, 0o755)
	}

	writeTestFile(t, cityDir, "city.toml", `
[workspace]
name = "test"

[imports.alpha]
source = "../shared"

[imports.beta]
source = "../shared"
`)
	writeTestFile(t, packDir, "pack.toml", `
[pack]
name = "shared"
schema = 1

[[agent]]
name = "worker"
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

	if !found["alpha.worker"] {
		t.Errorf("missing alpha.worker; got: %v", found)
	}
	if !found["beta.worker"] {
		t.Errorf("missing beta.worker; got: %v", found)
	}
}

func TestLoadPackWithCache_ReturnsDetachedCopies(t *testing.T) {
	dir := t.TempDir()
	cityRoot := filepath.Join(dir, "city")
	packDir := filepath.Join(dir, "shared")
	for _, d := range []string{cityRoot, packDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	writeTestFile(t, packDir, "pack.toml", `
[pack]
name = "shared"
schema = 1

[providers.helper]
command = "helper"
args = ["--base"]
env = { TOKEN = "base" }
option_defaults = { mode = "plan" }

[[agent]]
name = "worker"
scope = "city"
pre_start = ["echo pre"]
env = { AGENT = "base" }
ready_delay_ms = 5
install_agent_hooks = ["hook-a"]
session_setup = ["echo setup"]
inject_fragments = ["frag-a"]

[[service]]
name = "bridge"
kind = "proxy_process"

[service.process]
command = ["./scripts/start-bridge.sh"]
health_path = "/healthz"

[global]
session_live = ["echo global"]
`)

	cache := &packLoadCache{results: map[string]*packLoadResult{}}
	topoPath := filepath.Join(packDir, "pack.toml")

	agents, namedSessions, providers, services, topoDirs, requires, globals, err := loadPackWithCache(
		fsys.OSFS{}, topoPath, packDir, cityRoot, "", nil, cache)
	if err != nil {
		t.Fatalf("loadPackWithCache first pass: %v", err)
	}
	if len(agents) != 1 || len(namedSessions) != 0 || len(providers) != 1 || len(services) != 1 || len(topoDirs) != 1 || len(requires) != 0 || len(globals) != 1 {
		t.Fatalf("unexpected first-pass shape: agents=%d namedSessions=%d providers=%d services=%d topoDirs=%d requires=%d globals=%d",
			len(agents), len(namedSessions), len(providers), len(services), len(topoDirs), len(requires), len(globals))
	}

	agents[0].Env["AGENT"] = "mutated"
	agents[0].PreStart[0] = "mutated"
	*agents[0].ReadyDelayMs = 99
	agents[0].InstallAgentHooks[0] = "mutated"
	providers["helper"].Env["TOKEN"] = "mutated"
	providers["helper"].Args[0] = "mutated"
	providers["helper"].OptionDefaults["mode"] = "mutated"
	services[0].Process.Command[0] = "mutated"
	globals[0].SessionLive[0] = "mutated"

	agents2, namedSessions2, providers2, services2, topoDirs2, requires2, globals2, err := loadPackWithCache(
		fsys.OSFS{}, topoPath, packDir, cityRoot, "", nil, cache)
	if err != nil {
		t.Fatalf("loadPackWithCache second pass: %v", err)
	}
	if len(agents2) != 1 || len(namedSessions2) != 0 || len(providers2) != 1 || len(services2) != 1 || len(topoDirs2) != 1 || len(requires2) != 0 || len(globals2) != 1 {
		t.Fatalf("unexpected second-pass shape: agents=%d namedSessions=%d providers=%d services=%d topoDirs=%d requires=%d globals=%d",
			len(agents2), len(namedSessions2), len(providers2), len(services2), len(topoDirs2), len(requires2), len(globals2))
	}

	if got := agents2[0].Env["AGENT"]; got != "base" {
		t.Errorf("cached agent env leaked mutation: got %q, want %q", got, "base")
	}
	if got := agents2[0].PreStart[0]; got != "echo pre" {
		t.Errorf("cached agent pre_start leaked mutation: got %q, want %q", got, "echo pre")
	}
	if agents2[0].ReadyDelayMs == nil || *agents2[0].ReadyDelayMs != 5 {
		t.Errorf("cached agent ready_delay_ms leaked mutation: got %v, want 5", agents2[0].ReadyDelayMs)
	}
	if got := agents2[0].InstallAgentHooks[0]; got != "hook-a" {
		t.Errorf("cached agent install_agent_hooks leaked mutation: got %q, want %q", got, "hook-a")
	}
	if got := providers2["helper"].Env["TOKEN"]; got != "base" {
		t.Errorf("cached provider env leaked mutation: got %q, want %q", got, "base")
	}
	if got := providers2["helper"].Args[0]; got != "--base" {
		t.Errorf("cached provider args leaked mutation: got %q, want %q", got, "--base")
	}
	if got := providers2["helper"].OptionDefaults["mode"]; got != "plan" {
		t.Errorf("cached provider option_defaults leaked mutation: got %q, want %q", got, "plan")
	}
	if got := services2[0].Process.Command[0]; got != "./scripts/start-bridge.sh" {
		t.Errorf("cached service process.command leaked mutation: got %q, want %q", got, "./scripts/start-bridge.sh")
	}
	if got := globals2[0].SessionLive[0]; got != "echo global" {
		t.Errorf("cached global session_live leaked mutation: got %q, want %q", got, "echo global")
	}
}

func TestImport_HiddenDirsSkippedInAgentDiscovery(t *testing.T) {
	// Directories starting with . or _ should not be discovered as agents.
	dir := t.TempDir()
	packDir := filepath.Join(dir, "mypk")
	agentsDir := filepath.Join(packDir, "agents")

	mustMkdirAll(t, filepath.Join(agentsDir, ".hidden"), 0o755)
	mustMkdirAll(t, filepath.Join(agentsDir, "_internal"), 0o755)
	mustMkdirAll(t, filepath.Join(agentsDir, "real-agent"), 0o755)

	writeTestFile(t, packDir, "pack.toml", `
[pack]
name = "mypk"
schema = 1
`)
	writeTestFile(t, filepath.Join(agentsDir, ".hidden"), "prompt.md", `hidden`)
	writeTestFile(t, filepath.Join(agentsDir, "_internal"), "prompt.md", `internal`)
	writeTestFile(t, filepath.Join(agentsDir, "real-agent"), "prompt.md", `real`)

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
	for _, a := range explicit {
		if a.Name == ".hidden" || a.Name == "_internal" {
			t.Errorf("hidden/underscore dir discovered as agent: %q", a.Name)
		}
	}
	found := false
	for _, a := range explicit {
		if a.Name == "real-agent" {
			found = true
		}
	}
	if !found {
		t.Error("real-agent not discovered")
	}
}

func TestAgentMatchesIdentity(t *testing.T) {
	tests := []struct {
		name     string
		agent    Agent
		identity string
		want     bool
	}{
		{
			name:     "bare name match",
			agent:    Agent{Name: "mayor"},
			identity: "mayor",
			want:     true,
		},
		{
			name:     "V1 dir/name match",
			agent:    Agent{Name: "polecat", Dir: "proj"},
			identity: "proj/polecat",
			want:     true,
		},
		{
			name:     "V2 binding.name match",
			agent:    Agent{Name: "mayor", BindingName: "gastown"},
			identity: "gastown.mayor",
			want:     true,
		},
		{
			name:     "V2 dir/binding.name match",
			agent:    Agent{Name: "polecat", BindingName: "gastown", Dir: "proj"},
			identity: "proj/gastown.polecat",
			want:     true,
		},
		{
			name:     "bare name does NOT match V2 agent with binding",
			agent:    Agent{Name: "mayor", BindingName: "gastown"},
			identity: "mayor",
			want:     false,
		},
		{
			name:     "no match",
			agent:    Agent{Name: "mayor", BindingName: "gastown"},
			identity: "maint.mayor",
			want:     false,
		},
		{
			name:     "wrong dir",
			agent:    Agent{Name: "polecat", Dir: "proj"},
			identity: "other/polecat",
			want:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := AgentMatchesIdentity(&tt.agent, tt.identity)
			if got != tt.want {
				t.Errorf("AgentMatchesIdentity(%v, %q) = %v, want %v", tt.agent.QualifiedName(), tt.identity, got, tt.want)
			}
		})
	}
}

func TestQualifiedName_WithBindingName(t *testing.T) {
	tests := []struct {
		name   string
		agent  Agent
		wantQN string
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
