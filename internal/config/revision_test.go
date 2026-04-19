package config

import (
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

func TestRevision_Deterministic(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "city.toml", `[workspace]
name = "test"
`)

	prov := &Provenance{
		Sources: []string{filepath.Join(dir, "city.toml")},
	}

	h1 := Revision(fsys.OSFS{}, prov, &City{}, dir)
	h2 := Revision(fsys.OSFS{}, prov, &City{}, dir)
	if h1 != h2 {
		t.Errorf("not deterministic: %q vs %q", h1, h2)
	}
	if h1 == "" {
		t.Error("hash should not be empty")
	}
}

func TestRevision_ChangesOnFileModification(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "city.toml", `[workspace]
name = "test"
`)

	prov := &Provenance{
		Sources: []string{filepath.Join(dir, "city.toml")},
	}

	h1 := Revision(fsys.OSFS{}, prov, &City{}, dir)

	writeFile(t, dir, "city.toml", `[workspace]
name = "changed"
`)

	h2 := Revision(fsys.OSFS{}, prov, &City{}, dir)
	if h1 == h2 {
		t.Error("hash should change when file content changes")
	}
}

func TestRevision_IncludesFragments(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "city.toml", `[workspace]
name = "test"
`)
	writeFile(t, dir, "agents.toml", `[[agent]]
name = "mayor"
`)

	prov := &Provenance{
		Sources: []string{
			filepath.Join(dir, "city.toml"),
			filepath.Join(dir, "agents.toml"),
		},
	}

	h1 := Revision(fsys.OSFS{}, prov, &City{}, dir)

	// Change fragment.
	writeFile(t, dir, "agents.toml", `[[agent]]
name = "worker"
`)

	h2 := Revision(fsys.OSFS{}, prov, &City{}, dir)
	if h1 == h2 {
		t.Error("hash should change when fragment changes")
	}
}

func TestRevision_IncludesPack(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "city.toml", `[workspace]
name = "test"
`)
	writeFile(t, dir, "packs/gt/pack.toml", `[pack]
name = "gastown"
schema = 1
`)

	prov := &Provenance{
		Sources: []string{filepath.Join(dir, "city.toml")},
	}
	cfg := &City{Rigs: []Rig{{Name: "hw", Path: "/hw", Includes: []string{"packs/gt"}}}}

	h1 := Revision(fsys.OSFS{}, prov, cfg, dir)

	// Change pack file.
	writeFile(t, dir, "packs/gt/pack.toml", `[pack]
name = "gastown-v2"
schema = 1
`)

	h2 := Revision(fsys.OSFS{}, prov, cfg, dir)
	if h1 == h2 {
		t.Error("hash should change when pack file changes")
	}
}

func TestRevision_IncludesCityPack(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "city.toml", `[workspace]
name = "test"
`)
	writeFile(t, dir, "packs/shared/agents.toml", `[[agent]]
name = "worker"
`)

	prov := &Provenance{
		Sources: []string{filepath.Join(dir, "city.toml")},
	}
	cfg := &City{Workspace: Workspace{Includes: []string{"packs/shared"}}}

	h1 := Revision(fsys.OSFS{}, prov, cfg, dir)

	writeFile(t, dir, "packs/shared/agents.toml", `[[agent]]
name = "worker-v2"
`)

	h2 := Revision(fsys.OSFS{}, prov, cfg, dir)
	if h1 == h2 {
		t.Error("hash should change when city pack file changes")
	}
}

func TestRevision_IncludesConventionDiscoveredCityAgents(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "city.toml", `[workspace]
name = "test"
`)
	writeFile(t, dir, "agents/mayor/prompt.template.md", "first prompt\n")

	prov := &Provenance{
		Sources: []string{filepath.Join(dir, "city.toml")},
	}

	h1 := Revision(fsys.OSFS{}, prov, &City{}, dir)
	writeFile(t, dir, "agents/mayor/prompt.template.md", "second prompt\n")
	h2 := Revision(fsys.OSFS{}, prov, &City{}, dir)
	if h1 == h2 {
		t.Error("hash should change when convention-discovered city agent files change")
	}
}

func TestWatchDirs_ConfigOnly(t *testing.T) {
	dir := t.TempDir()
	prov := &Provenance{
		Sources: []string{filepath.Join(dir, "city.toml")},
	}

	dirs := WatchDirs(prov, &City{}, dir)
	if len(dirs) != 1 {
		t.Fatalf("got %d dirs, want 1", len(dirs))
	}
	if dirs[0] != dir {
		t.Errorf("dir = %q, want %q", dirs[0], dir)
	}
}

func TestWatchDirs_WithFragments(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "conf/agents.toml", "")

	prov := &Provenance{
		Sources: []string{
			filepath.Join(dir, "city.toml"),
			filepath.Join(dir, "conf", "agents.toml"),
		},
	}

	dirs := WatchDirs(prov, &City{}, dir)
	sort.Strings(dirs)

	expected := []string{dir, filepath.Join(dir, "conf")}
	sort.Strings(expected)

	if len(dirs) != 2 {
		t.Fatalf("got %d dirs, want 2: %v", len(dirs), dirs)
	}
	for i := range expected {
		if dirs[i] != expected[i] {
			t.Errorf("dirs[%d] = %q, want %q", i, dirs[i], expected[i])
		}
	}
}

func TestWatchDirs_WithPack(t *testing.T) {
	dir := t.TempDir()
	prov := &Provenance{
		Sources: []string{filepath.Join(dir, "city.toml")},
	}
	cfg := &City{Rigs: []Rig{{Name: "hw", Path: "/hw", Includes: []string{"packs/gt"}}}}

	dirs := WatchDirs(prov, cfg, dir)

	// Should include city dir + pack dir.
	if len(dirs) != 2 {
		t.Fatalf("got %d dirs, want 2: %v", len(dirs), dirs)
	}

	found := false
	for _, d := range dirs {
		if d == filepath.Join(dir, "packs", "gt") {
			found = true
		}
	}
	if !found {
		t.Errorf("pack dir not in watch list: %v", dirs)
	}
}

func TestWatchDirs_WithPackConventionRoots(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "packs/gt/formulas/mol-test.formula.toml", "formula = \"mol-test\"\n")
	writeFile(t, dir, "packs/gt/orders/test.order.toml", "name = \"test\"\n")

	prov := &Provenance{
		Sources: []string{filepath.Join(dir, "city.toml")},
	}
	cfg := &City{Rigs: []Rig{{Name: "hw", Path: "/hw", Includes: []string{"packs/gt"}}}}

	dirs := WatchDirs(prov, cfg, dir)
	for _, want := range []string{
		filepath.Join(dir, "packs", "gt"),
	} {
		found := false
		for _, got := range dirs {
			if got == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("watch dirs = %v, want %q present", dirs, want)
		}
	}
}

func TestWatchTargets_WithPackConventionRoots(t *testing.T) {
	dir := t.TempDir()
	packDir := filepath.Join(dir, "packs", "gt")
	writeFile(t, dir, "packs/gt/formulas/mol-test.formula.toml", "formula = \"mol-test\"\n")
	writeFile(t, dir, "packs/gt/orders/test.order.toml", "name = \"test\"\n")

	prov := &Provenance{
		Sources: []string{filepath.Join(dir, "city.toml")},
	}
	cfg := &City{Rigs: []Rig{{Name: "hw", Path: "/hw", Includes: []string{"packs/gt"}}}}

	targets := WatchTargets(prov, cfg, dir)
	assertWatchTarget(t, targets, dir, false, true)
	assertWatchTarget(t, targets, packDir, true, false)
	assertNoWatchTarget(t, targets, filepath.Join(packDir, "formulas"))
	assertNoWatchTarget(t, targets, filepath.Join(packDir, "orders"))
}

func TestWatchDirs_WithCityPack(t *testing.T) {
	dir := t.TempDir()
	prov := &Provenance{
		Sources: []string{filepath.Join(dir, "city.toml")},
	}
	cfg := &City{Workspace: Workspace{Includes: []string{"packs/shared"}}}

	dirs := WatchDirs(prov, cfg, dir)

	found := false
	for _, d := range dirs {
		if d == filepath.Join(dir, "packs", "shared") {
			found = true
		}
	}
	if !found {
		t.Errorf("city pack dir not in watch list: %v", dirs)
	}
}

func TestWatchDirs_IncludesConventionDiscoveryRoots(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "agents/sample-agent/prompt.template.md", "prompt\n")
	writeFile(t, dir, "commands/reload/run.sh", "#!/bin/sh\n")
	writeFile(t, dir, "doctor/runtime/run.sh", "#!/bin/sh\n")
	writeFile(t, dir, "formulas/reload.formula.toml", "formula = \"reload\"\n")
	writeFile(t, dir, "orders/reload.order.toml", "name = \"reload\"\n")
	writeFile(t, dir, "template-fragments/footer.template.md", "{{ define \"footer\" }}ok{{ end }}\n")
	writeFile(t, dir, "skills/review/SKILL.md", "# review\n")
	writeFile(t, dir, "mcp/review.toml", "command = [\"review\"]\n")

	prov := &Provenance{
		Sources: []string{filepath.Join(dir, "city.toml")},
	}

	dirs := WatchDirs(prov, &City{}, dir)
	sort.Strings(dirs)

	for _, want := range []string{
		filepath.Join(dir, "agents"),
		filepath.Join(dir, "commands"),
		filepath.Join(dir, "doctor"),
		filepath.Join(dir, "formulas"),
		filepath.Join(dir, "orders"),
		filepath.Join(dir, "template-fragments"),
		filepath.Join(dir, "skills"),
		filepath.Join(dir, "mcp"),
	} {
		found := false
		for _, got := range dirs {
			if got == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("watch dirs = %v, want %q present", dirs, want)
		}
	}
}

func TestWatchTargets_IncludesConventionDiscoveryRoots(t *testing.T) {
	dir := t.TempDir()
	for _, rel := range []string{
		"agents/sample-agent/prompt.template.md",
		"commands/reload/run.sh",
		"doctor/runtime/run.sh",
		"formulas/reload.formula.toml",
		"orders/reload.order.toml",
		"template-fragments/footer.template.md",
		"skills/review/SKILL.md",
		"mcp/review.toml",
	} {
		writeFile(t, dir, rel, "x\n")
	}
	prov := &Provenance{
		Sources: []string{filepath.Join(dir, "city.toml")},
	}

	targets := WatchTargets(prov, &City{}, dir)
	assertWatchTarget(t, targets, dir, false, true)
	for _, name := range ConventionDiscoveryDirNames() {
		assertWatchTarget(t, targets, filepath.Join(dir, name), true, false)
	}
}

// Regression for gastownhall/gascity#779:
// WatchTargets must include v2-resolved PackDirs / RigPackDirs, not only v1
// include refs, or cities composing packs via [imports.X] get no fsnotify
// coverage for imported pack trees.
func TestWatchTargets_Regression779_IncludesV2ResolvedPackDirs(t *testing.T) {
	dir := t.TempDir()
	cityPack := filepath.Join(dir, "imported", "city-pack")
	rigPack := filepath.Join(dir, "imported", "rig-pack")
	writeFile(t, cityPack, "formulas/city.formula.toml", "formula = \"city\"\n")
	writeFile(t, rigPack, "skills/rig/SKILL.md", "# rig\n")

	prov := &Provenance{
		Sources: []string{filepath.Join(dir, "city.toml")},
	}
	cfg := &City{
		PackDirs: []string{cityPack},
		RigPackDirs: map[string][]string{
			"api-server": {rigPack},
		},
		Rigs: []Rig{{Name: "api-server", Path: "/srv/api"}},
	}

	targets := WatchTargets(prov, cfg, dir)
	assertWatchTarget(t, targets, cityPack, true, false)
	assertNoWatchTarget(t, targets, filepath.Join(cityPack, "formulas"))
	assertWatchTarget(t, targets, rigPack, true, false)
	assertNoWatchTarget(t, targets, filepath.Join(rigPack, "skills"))
}

func TestWatchTargets_SkipsEmptyPackDirsWithoutCWDConventionLeak(t *testing.T) {
	originalWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	cwd := t.TempDir()
	if err := os.Mkdir(filepath.Join(cwd, "agents"), 0o755); err != nil {
		t.Fatalf("Mkdir cwd agents: %v", err)
	}
	if err := os.Chdir(cwd); err != nil {
		t.Fatalf("Chdir temp cwd: %v", err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(originalWD); err != nil {
			t.Fatalf("restore cwd: %v", err)
		}
	})

	dir := t.TempDir()
	prov := &Provenance{
		Sources: []string{filepath.Join(dir, "city.toml")},
	}
	cfg := &City{
		Workspace: Workspace{Includes: []string{""}},
		PackDirs:  []string{""},
		Rigs:      []Rig{{Name: "api", Path: "/api", Includes: []string{""}}},
		RigPackDirs: map[string][]string{
			"api": {""},
		},
	}

	targets := WatchTargets(prov, cfg, dir)
	assertNoWatchTarget(t, targets, "")
	assertNoWatchTarget(t, targets, "agents")
}

// Regression for gastownhall/gascity#779:
// WatchDirs iterated only the v1 Includes slices and ignored v2-resolved
// PackDirs / RigPackDirs, so cities composing packs via [imports.X] or
// [rigs.imports.X] got zero fsnotify coverage for imported pack trees.
// Hot reload was silently broken for v2 layouts.
func TestWatchDirs_Regression779_IncludesV2ResolvedPackDirs(t *testing.T) {
	dir := t.TempDir()
	prov := &Provenance{
		Sources: []string{filepath.Join(dir, "city.toml")},
	}
	cityPack := filepath.Join(dir, "imported", "city-pack")
	rigPack := filepath.Join(dir, "imported", "rig-pack")

	cfg := &City{
		PackDirs: []string{cityPack},
		RigPackDirs: map[string][]string{
			"api-server": {rigPack},
		},
		Rigs: []Rig{{Name: "api-server", Path: "/srv/api"}},
	}

	dirs := WatchDirs(prov, cfg, dir)
	sort.Strings(dirs)

	for _, want := range []string{cityPack, rigPack} {
		found := false
		for _, got := range dirs {
			if got == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("watch dirs = %v, want %q present (v2 resolved pack dir, gascity#779)", dirs, want)
		}
	}
}

// Regression for gastownhall/gascity#779:
// Revision hashed only v1 Includes-based pack content. Cities using v2
// [imports.X] saw no revision change when imported pack files were edited,
// so the reconciler never detected the change.
func TestRevision_Regression779_HashesV2ResolvedPackDirs(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "city.toml", `[workspace]
name = "test"
`)
	packAbs := filepath.Join(dir, "imported", "support")
	writeFile(t, packAbs, "agents/helper/prompt.template.md", "first prompt\n")

	prov := &Provenance{
		Sources: []string{filepath.Join(dir, "city.toml")},
	}
	cfg := &City{PackDirs: []string{packAbs}}

	h1 := Revision(fsys.OSFS{}, prov, cfg, dir)

	writeFile(t, packAbs, "agents/helper/prompt.template.md", "second prompt\n")

	h2 := Revision(fsys.OSFS{}, prov, cfg, dir)
	if h1 == h2 {
		t.Errorf("Revision did not change when v2-imported pack content changed (gascity#779)")
	}
}

func TestWatchDirs_Deduplicates(t *testing.T) {
	dir := t.TempDir()
	prov := &Provenance{
		Sources: []string{
			filepath.Join(dir, "city.toml"),
			filepath.Join(dir, "agents.toml"),
		},
	}

	dirs := WatchDirs(prov, &City{}, dir)
	if len(dirs) != 1 {
		t.Errorf("got %d dirs, want 1 (deduplicated): %v", len(dirs), dirs)
	}
}

func assertWatchTarget(t *testing.T, targets []WatchTarget, path string, recursive, discoverConventions bool) {
	t.Helper()
	for _, target := range targets {
		if target.Path == path {
			if target.Recursive != recursive || target.DiscoverConventions != discoverConventions {
				t.Fatalf("target %q = {Recursive:%v DiscoverConventions:%v}, want {Recursive:%v DiscoverConventions:%v}; all targets: %#v",
					path, target.Recursive, target.DiscoverConventions, recursive, discoverConventions, targets)
			}
			return
		}
	}
	t.Fatalf("target %q not found in %#v", path, targets)
}

func assertNoWatchTarget(t *testing.T, targets []WatchTarget, path string) {
	t.Helper()
	for _, target := range targets {
		if target.Path == path {
			t.Fatalf("target %q unexpectedly found in %#v", path, targets)
		}
	}
}
