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
