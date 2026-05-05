package config

import (
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

func TestRevision_UsesLoadedSourceSnapshot(t *testing.T) {
	dir := t.TempDir()
	cityPath := filepath.Join(dir, "city.toml")
	writeFile(t, dir, "city.toml", `[workspace]
name = "test"
`)

	cfg, prov, err := LoadWithIncludes(fsys.OSFS{}, cityPath)
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	loadedRevision := Revision(fsys.OSFS{}, prov, cfg, dir)

	writeFile(t, dir, "city.toml", `[workspace]
name = "changed"
`)
	afterWriteRevision := Revision(fsys.OSFS{}, prov, cfg, dir)
	if afterWriteRevision != loadedRevision {
		t.Fatalf("revision changed after source file write; got %q, want loaded snapshot %q", afterWriteRevision, loadedRevision)
	}

	reloadedCfg, reloadedProv, err := LoadWithIncludes(fsys.OSFS{}, cityPath)
	if err != nil {
		t.Fatalf("reloading config: %v", err)
	}
	reloadedRevision := Revision(fsys.OSFS{}, reloadedProv, reloadedCfg, dir)
	if reloadedRevision == loadedRevision {
		t.Fatal("revision did not change after reloading changed source file")
	}
}

func TestRevision_UsesLoadedSnapshotForResolvedInputs(t *testing.T) {
	tests := []struct {
		name   string
		setup  func(t *testing.T, dir string)
		mutate func(t *testing.T, dir string)
	}{
		{
			name: "fragment",
			setup: func(t *testing.T, dir string) {
				writeFile(t, dir, "city.toml", `
include = ["agents.toml"]

[workspace]
name = "test"
`)
				writeFile(t, dir, "agents.toml", `[[agent]]
name = "builder"
`)
			},
			mutate: func(t *testing.T, dir string) {
				writeFile(t, dir, "agents.toml", `[[agent]]
name = "builder-renamed"
`)
			},
		},
		{
			name: "city pack.toml",
			setup: func(t *testing.T, dir string) {
				writeFile(t, dir, "city.toml", `[workspace]
name = "test"
`)
				writeFile(t, dir, "pack.toml", `[pack]
name = "citypack"
schema = 1

[[agent]]
name = "builder"
`)
			},
			mutate: func(t *testing.T, dir string) {
				writeFile(t, dir, "pack.toml", `[pack]
name = "citypack"
schema = 1

[[agent]]
name = "builder-renamed"
`)
			},
		},
		{
			name: "implicit imports file",
			setup: func(t *testing.T, dir string) {
				gcHome := filepath.Join(dir, "gc-home")
				t.Setenv("GC_HOME", gcHome)
				writeFile(t, gcHome, "implicit-import.toml", `schema = 1

[imports.core]
source = "github.com/gastownhall/gc-core"
version = "0.1.0"
`)
				writeFile(t, dir, "city.toml", `[workspace]
name = "test"
`)
			},
			mutate: func(t *testing.T, dir string) {
				writeFile(t, filepath.Join(dir, "gc-home"), "implicit-import.toml", `schema = 1

[imports.core]
source = "github.com/gastownhall/gc-core"
version = "0.1.1"
`)
			},
		},
		{
			name: "legacy city include pack tree",
			setup: func(t *testing.T, dir string) {
				writeFile(t, dir, "city.toml", `[workspace]
name = "test"
includes = ["packs/shared"]
`)
				writeFile(t, dir, "packs/shared/pack.toml", `[pack]
name = "shared"
schema = 1

[[agent]]
name = "builder"
prompt_template = "prompts/builder.template.md"
`)
				writeFile(t, dir, "packs/shared/prompts/builder.template.md", "first prompt\n")
			},
			mutate: func(t *testing.T, dir string) {
				writeFile(t, dir, "packs/shared/prompts/builder.template.md", "second prompt\n")
			},
		},
		{
			name: "legacy rig include pack tree",
			setup: func(t *testing.T, dir string) {
				writeFile(t, dir, "city.toml", `[workspace]
name = "test"

[[rigs]]
name = "frontend"
path = "../frontend"
includes = ["packs/rig"]
`)
				writeFile(t, dir, "packs/rig/pack.toml", `[pack]
name = "rigpack"
schema = 1

[[agent]]
name = "runner"
scope = "rig"
prompt_template = "prompts/runner.template.md"
`)
				writeFile(t, dir, "packs/rig/prompts/runner.template.md", "first prompt\n")
			},
			mutate: func(t *testing.T, dir string) {
				writeFile(t, dir, "packs/rig/prompts/runner.template.md", "second prompt\n")
			},
		},
		{
			name: "packs.lock",
			setup: func(t *testing.T, dir string) {
				writeFile(t, dir, "city.toml", `[workspace]
name = "test"

[imports.shared]
source = "./packs/shared"
`)
				writeFile(t, dir, "packs/shared/pack.toml", `[pack]
name = "shared"
schema = 1
`)
				writeFile(t, dir, "packs.lock", `schema = 1

[packs."./packs/shared"]
version = "1.0.0"
commit = "aaaa"
`)
			},
			mutate: func(t *testing.T, dir string) {
				writeFile(t, dir, "packs.lock", `schema = 1

[packs."./packs/shared"]
version = "1.0.1"
commit = "bbbb"
`)
			},
		},
		{
			name: "PackV2 city import tree",
			setup: func(t *testing.T, dir string) {
				writeFile(t, dir, "city.toml", `[workspace]
name = "test"

[imports.shared]
source = "./packs/shared"
`)
				writeFile(t, dir, "packs/shared/pack.toml", `[pack]
name = "shared"
schema = 1

[[agent]]
name = "builder"
prompt_template = "prompts/builder.template.md"
`)
				writeFile(t, dir, "packs/shared/prompts/builder.template.md", "first prompt\n")
			},
			mutate: func(t *testing.T, dir string) {
				writeFile(t, dir, "packs/shared/prompts/builder.template.md", "second prompt\n")
			},
		},
		{
			name: "PackV2 rig import tree",
			setup: func(t *testing.T, dir string) {
				writeFile(t, dir, "city.toml", `[workspace]
name = "test"

[[rigs]]
name = "frontend"
path = "../frontend"

[rigs.imports.shared]
source = "./packs/shared"
`)
				writeFile(t, dir, "packs/shared/pack.toml", `[pack]
name = "shared"
schema = 1

[[agent]]
name = "runner"
scope = "rig"
prompt_template = "prompts/runner.template.md"
`)
				writeFile(t, dir, "packs/shared/prompts/runner.template.md", "first prompt\n")
			},
			mutate: func(t *testing.T, dir string) {
				writeFile(t, dir, "packs/shared/prompts/runner.template.md", "second prompt\n")
			},
		},
		{
			name: "convention discovery tree",
			setup: func(t *testing.T, dir string) {
				writeFile(t, dir, "city.toml", `[workspace]
name = "test"
`)
				writeFile(t, dir, "agents/builder/prompt.template.md", "first prompt\n")
			},
			mutate: func(t *testing.T, dir string) {
				writeFile(t, dir, "agents/builder/prompt.template.md", "second prompt\n")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			cityPath := filepath.Join(dir, "city.toml")
			tt.setup(t, dir)

			cfg, prov, err := LoadWithIncludes(fsys.OSFS{}, cityPath)
			if err != nil {
				t.Fatalf("LoadWithIncludes: %v", err)
			}
			loadedRevision := Revision(fsys.OSFS{}, prov, cfg, dir)

			tt.mutate(t, dir)
			afterWriteRevision := Revision(fsys.OSFS{}, prov, cfg, dir)
			if afterWriteRevision != loadedRevision {
				t.Fatalf("revision changed after post-load mutation; got %q, want loaded snapshot %q", afterWriteRevision, loadedRevision)
			}

			reloadedCfg, reloadedProv, err := LoadWithIncludes(fsys.OSFS{}, cityPath)
			if err != nil {
				t.Fatalf("reloading config: %v", err)
			}
			reloadedRevision := Revision(fsys.OSFS{}, reloadedProv, reloadedCfg, dir)
			if reloadedRevision == loadedRevision {
				t.Fatal("revision did not change after reloading changed input")
			}
		})
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

func TestRevision_IncludesPacksLockWhenPackV2ImportsPresent(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "city.toml", `[workspace]
name = "test"
`)
	writeFile(t, dir, "packs.lock", `schema = 1

[packs."https://example.com/shared.git"]
version = "1.0.0"
commit = "aaaa"
`)

	prov := &Provenance{
		Sources: []string{filepath.Join(dir, "city.toml")},
	}
	cfg := &City{
		Imports: map[string]Import{
			"shared": {
				Source:  "https://example.com/shared.git",
				Version: "^1.0",
			},
		},
	}

	h1 := Revision(fsys.OSFS{}, prov, cfg, dir)
	writeFile(t, dir, "packs.lock", `schema = 1

[packs."https://example.com/shared.git"]
version = "1.1.0"
commit = "bbbb"
`)
	h2 := Revision(fsys.OSFS{}, prov, cfg, dir)
	if h1 == h2 {
		t.Error("hash should change when packs.lock changes for PackV2 imports")
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

func TestWatchDirs_WithPackV2Imports(t *testing.T) {
	dir := t.TempDir()
	prov := &Provenance{
		Sources: []string{filepath.Join(dir, "city.toml")},
	}
	importDir := filepath.Join(dir, ".gc", "cache", "repos", "abc123", "packs", "base")
	cfg := &City{
		PackDirs: []string{importDir},
	}

	dirs := WatchDirs(prov, cfg, dir)

	found := false
	for _, d := range dirs {
		if d == importDir {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("watch dirs = %v, want PackV2 import dir %q", dirs, importDir)
	}
}

func TestWatchDirs_WithRigPackV2Imports(t *testing.T) {
	dir := t.TempDir()
	prov := &Provenance{
		Sources: []string{filepath.Join(dir, "city.toml")},
	}
	rigImportDir := filepath.Join(dir, ".gc", "cache", "repos", "abc123", "packs", "rig")
	cfg := &City{
		RigPackDirs: map[string][]string{
			"alpha": {rigImportDir},
		},
	}

	dirs := WatchDirs(prov, cfg, dir)

	found := false
	for _, d := range dirs {
		if d == rigImportDir {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("watch dirs = %v, want rig PackV2 import dir %q", dirs, rigImportDir)
	}
}

func TestWatchDirs_IncludesConventionDiscoveryRoots(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "agents/mayor/prompt.template.md", "prompt\n")
	writeFile(t, dir, "commands/reload/run.sh", "#!/bin/sh\n")
	writeFile(t, dir, "doctor/runtime/run.sh", "#!/bin/sh\n")

	prov := &Provenance{
		Sources: []string{filepath.Join(dir, "city.toml")},
	}

	dirs := WatchDirs(prov, &City{}, dir)
	sort.Strings(dirs)

	for _, want := range []string{
		filepath.Join(dir, "agents"),
		filepath.Join(dir, "commands"),
		filepath.Join(dir, "doctor"),
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
