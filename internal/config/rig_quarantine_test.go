//go:build !windows

package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

const quarantineBrokenSource = "https://github.com/example/broken.git"

// rigQuarantineFixture is a city with a healthy rig "good" and a rig "bad"
// whose second import is a locked remote pack served from the shared repo
// cache. The fixture's cache starts valid; each test breaks it its own way.
type rigQuarantineFixture struct {
	cityDir     string
	cacheDir    string
	badLocalDir string
}

func (f rigQuarantineFixture) cityToml() string {
	return filepath.Join(f.cityDir, "city.toml")
}

// newRigQuarantineFixture writes the fixture. Rig "bad" imports a local pack
// "alpha" before the locked remote "broken" (imports load in name order), so
// by the time "broken" fails, "alpha" has already contributed an agent, a pack
// command and a formula dir that a quarantine must not leave behind.
func newRigQuarantineFixture(t *testing.T) rigQuarantineFixture {
	t.Helper()
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	t.Setenv("HOME", home)
	t.Setenv("GC_HOME", filepath.Join(home, ".gc"))

	cityDir := filepath.Join(dir, "city")
	mustMkdirAll(t, cityDir, 0o755)

	seedDir := filepath.Join(dir, "seed")
	mustMkdirAll(t, seedDir, 0o755)
	writeTestFile(t, seedDir, "pack.toml", `
[pack]
name = "broken"
schema = 1

[[agent]]
name = "brokenworker"
scope = "rig"
`)
	for _, args := range [][]string{
		{"init", "-q"},
		{"add", "pack.toml"},
		{"-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-q", "-m", "seed"},
	} {
		if _, err := runRepoCacheGit(seedDir, args...); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}
	commit, err := runRepoCacheGit(seedDir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("git rev-parse: %v", err)
	}
	commit = strings.TrimSpace(commit)
	cacheDir := filepath.Join(home, ".gc", "cache", "repos", RepoCacheKey(quarantineBrokenSource, commit))
	mustMkdirAll(t, filepath.Dir(cacheDir), 0o755)
	if err := os.Rename(seedDir, cacheDir); err != nil {
		t.Fatal(err)
	}

	goodPack := filepath.Join(dir, "goodpack")
	writeTestFile(t, goodPack, "pack.toml", `
[pack]
name = "goodpack"
schema = 1

[[agent]]
name = "worker"
scope = "rig"
`)

	badLocal := filepath.Join(dir, "alpha")
	writeTestFile(t, badLocal, "pack.toml", `
[pack]
name = "alpha"
schema = 1

[[agent]]
name = "helper"
scope = "rig"
`)
	writeTestFile(t, badLocal, "formulas/mol-alpha.toml", "formula = \"mol-alpha\"\n")
	writeTestFile(t, badLocal, "commands/status/run.sh", "#!/bin/sh\n")

	writeTestFile(t, cityDir, "city.toml", `
[workspace]
name = "test"

[[rigs]]
name = "good"
path = "/tmp/good"
prefix = "gd"

[rigs.imports.tools]
source = "../goodpack"

[[rigs]]
name = "bad"
path = "/tmp/bad"
prefix = "bd"

[rigs.imports.alpha]
source = "../alpha"

[rigs.imports.broken]
source = "https://github.com/example/broken.git"
version = "^1.0"
`)
	writeTestFile(t, cityDir, "packs.lock", fmt.Sprintf(`
schema = 1

[packs."https://github.com/example/broken.git"]
version = "1.0.0"
commit = "%s"
fetched = "2026-04-10T00:00:00Z"
`, commit))
	t.Cleanup(ResetRemoteCacheValidationCache)
	return rigQuarantineFixture{cityDir: cityDir, cacheDir: cacheDir, badLocalDir: badLocal}
}

// dirtyCache leaves a local edit in the shared cache, tripping the
// dirty-cache gate (issue #6656 trigger 3).
func (f rigQuarantineFixture) dirtyCache(t *testing.T) {
	t.Helper()
	writeTestFile(t, f.cacheDir, "pack.toml", "[pack]\nname = \"tampered\"\nschema = 1\n")
}

func loadIsolated(t *testing.T, f rigQuarantineFixture) (*City, *Provenance) {
	t.Helper()
	cfg, prov, err := LoadWithIncludesOptions(fsys.OSFS{}, f.cityToml(), LoadOptions{IsolateRigPackFailures: true})
	if err != nil {
		t.Fatalf("LoadWithIncludesOptions(isolate): one rig's broken import failed the city load: %v", err)
	}
	return cfg, prov
}

// assertBadRigQuarantined checks the AC-1 shape: the healthy rig loads, the
// quarantined rig keeps its city.toml declaration but contributes no pack
// content, and the quarantine is reported once, by name.
func assertBadRigQuarantined(t *testing.T, f rigQuarantineFixture, cfg *City, wantErr ...string) {
	t.Helper()
	hasGoodWorker := false
	for _, a := range cfg.Agents {
		if a.Dir == "good" && a.Name == "worker" {
			hasGoodWorker = true
		}
		if a.Dir == "bad" {
			t.Errorf("agent %q from quarantined rig present", a.QualifiedName())
		}
	}
	if !hasGoodWorker {
		t.Errorf("healthy rig agent good/worker missing")
	}
	for _, c := range cfg.PackCommands {
		if strings.HasPrefix(c.PackDir, f.badLocalDir) || strings.HasPrefix(c.PackDir, f.cacheDir) {
			t.Errorf("pack command %q from quarantined rig's packs present (pack dir %s)", c.Name, c.PackDir)
		}
	}
	for _, fd := range cfg.FormulaLayers.Rigs["bad"] {
		if strings.HasPrefix(fd, f.badLocalDir) || strings.HasPrefix(fd, f.cacheDir) {
			t.Errorf("formula dir %s from quarantined rig's packs present", fd)
		}
	}
	if dirs := cfg.RigPackDirs["bad"]; len(dirs) != 0 {
		t.Errorf("RigPackDirs[bad] = %v, want none", dirs)
	}

	var bad *Rig
	for i := range cfg.Rigs {
		if cfg.Rigs[i].Name == "bad" {
			bad = &cfg.Rigs[i]
		}
	}
	if bad == nil {
		t.Fatalf("cfg.Rigs no longer declares quarantined rig %q", "bad")
	}
	if bad.Path != "/tmp/bad" || bad.Prefix != "bd" {
		t.Errorf("quarantined rig declaration = path %q prefix %q, want /tmp/bad bd", bad.Path, bad.Prefix)
	}

	if len(cfg.QuarantinedRigs) != 1 {
		t.Fatalf("QuarantinedRigs = %+v, want exactly one entry", cfg.QuarantinedRigs)
	}
	q := cfg.QuarantinedRigs[0]
	if q.Rig != "bad" {
		t.Errorf("QuarantinedRigs[0].Rig = %q, want bad", q.Rig)
	}
	msg := q.Err.Error()
	for _, want := range append([]string{`import "broken"`}, wantErr...) {
		if !strings.Contains(msg, want) {
			t.Errorf("QuarantinedRigs[0].Err = %q, want it to mention %q", msg, want)
		}
	}
	if !cfg.RigQuarantined("bad") || cfg.RigQuarantined("good") {
		t.Errorf("RigQuarantined(bad)=%v RigQuarantined(good)=%v, want true false", cfg.RigQuarantined("bad"), cfg.RigQuarantined("good"))
	}

	var quarantineWarnings []string
	for _, w := range cfg.LoadWarnings {
		if IsRigQuarantineWarning(w) {
			quarantineWarnings = append(quarantineWarnings, w)
		}
	}
	if len(quarantineWarnings) != 1 || !strings.Contains(quarantineWarnings[0], `rig "bad"`) {
		t.Errorf("quarantine warnings = %q, want one naming rig %q (all warnings: %q)", quarantineWarnings, "bad", cfg.LoadWarnings)
	}
}

func TestLoadIsolateRigPackFailuresQuarantinesDirtyCacheRig(t *testing.T) {
	f := newRigQuarantineFixture(t)
	f.dirtyCache(t)

	cfg, prov := loadIsolated(t, f)
	assertBadRigQuarantined(t, f, cfg, "has local worktree changes", `gc import install`)

	quarantineInProv := 0
	for _, w := range prov.Warnings {
		if IsRigQuarantineWarning(w) {
			quarantineInProv++
		}
	}
	if quarantineInProv != 1 {
		t.Errorf("provenance carries %d quarantine warnings, want 1: %q", quarantineInProv, prov.Warnings)
	}
	if err := cfg.QuarantineError(); err == nil || !strings.Contains(err.Error(), `rig "bad"`) {
		t.Errorf("QuarantineError() = %v, want an error naming rig %q", err, "bad")
	}
}

func TestLoadIsolateRigPackFailuresHealthyCityQuarantinesNothing(t *testing.T) {
	f := newRigQuarantineFixture(t)

	cfg, _ := loadIsolated(t, f)
	if len(cfg.QuarantinedRigs) != 0 || cfg.QuarantineError() != nil {
		t.Fatalf("healthy city quarantined %+v", cfg.QuarantinedRigs)
	}
	found := false
	for _, a := range cfg.Agents {
		if a.Dir == "bad" && a.Name == "brokenworker" {
			found = true
		}
	}
	if !found {
		t.Errorf("healthy remote import agent bad/brokenworker missing")
	}
}

// A city [[patches.agent]] aimed at the quarantined rig cannot match: its
// agents were withheld. It is withheld with them instead of failing the load,
// while a typo aimed at a healthy rig still fails.
func TestLoadIsolateRigPackFailuresWithholdsPatchesForQuarantinedRig(t *testing.T) {
	f := newRigQuarantineFixture(t)
	f.dirtyCache(t)
	appendCityToml(t, f, `
[[patches.agent]]
dir = "bad"
name = "helper"
nudge = "hi"
`)
	cfg, _ := loadIsolated(t, f)
	if len(cfg.QuarantinedRigs) != 1 || cfg.QuarantinedRigs[0].Rig != "bad" {
		t.Fatalf("QuarantinedRigs = %+v, want rig bad", cfg.QuarantinedRigs)
	}
	named := false
	for _, w := range cfg.LoadWarnings {
		if IsRigQuarantineWarning(w) && strings.Contains(w, "bad/helper") {
			named = true
		}
	}
	if !named {
		t.Errorf("quarantine warning does not name the withheld patch bad/helper: %q", cfg.LoadWarnings)
	}

	appendCityToml(t, f, `
[[patches.agent]]
dir = "good"
name = "wroker"
nudge = "hi"
`)
	if _, _, err := LoadWithIncludesOptions(fsys.OSFS{}, f.cityToml(), LoadOptions{IsolateRigPackFailures: true}); err == nil {
		t.Fatal("typo patch aimed at healthy rig loaded; want a hard error")
	}
}

func appendCityToml(t *testing.T, f rigQuarantineFixture, extra string) {
	t.Helper()
	data, err := os.ReadFile(f.cityToml())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.cityToml(), append(data, []byte(extra)...), 0o644); err != nil {
		t.Fatal(err)
	}
}
