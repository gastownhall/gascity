package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

func writePackLifecycleScript(t *testing.T, packDir, name string) string {
	t.Helper()
	dir := filepath.Join(packDir, "lifecycle")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir lifecycle: %v", err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

func writePackManifest(t *testing.T, packDir, packName string) {
	t.Helper()
	if err := os.MkdirAll(packDir, 0o755); err != nil {
		t.Fatalf("mkdir pack: %v", err)
	}
	body := "[pack]\nname = \"" + packName + "\"\n"
	if err := os.WriteFile(filepath.Join(packDir, packFile), []byte(body), 0o644); err != nil {
		t.Fatalf("write pack.toml: %v", err)
	}
}

func TestDiscoverPackLifecycleHooksFindsBothEvents(t *testing.T) {
	packDir := t.TempDir()
	stop := writePackLifecycleScript(t, packDir, "city-stop.sh")
	start := writePackLifecycleScript(t, packDir, "city-start.sh")

	hooks := DiscoverPackLifecycleHooks(fsys.OSFS{}, packDir, "gasburger")
	if len(hooks) != 2 {
		t.Fatalf("hooks = %d, want 2 (%+v)", len(hooks), hooks)
	}
	// Events are returned in LifecycleEvents order: start before stop.
	if hooks[0].Event != LifecycleEventCityStart || hooks[0].Script != start {
		t.Errorf("hooks[0] = %+v, want city-start %s", hooks[0], start)
	}
	if hooks[1].Event != LifecycleEventCityStop || hooks[1].Script != stop {
		t.Errorf("hooks[1] = %+v, want city-stop %s", hooks[1], stop)
	}
	for _, h := range hooks {
		if h.PackName != "gasburger" {
			t.Errorf("PackName = %q, want gasburger", h.PackName)
		}
		if h.PackDir != packDir {
			t.Errorf("PackDir = %q, want %q", h.PackDir, packDir)
		}
	}
}

func TestDiscoverPackLifecycleHooksIgnoresUnknownAndNonRegularEntries(t *testing.T) {
	packDir := t.TempDir()
	writePackLifecycleScript(t, packDir, "city-stop.sh")
	writePackLifecycleScript(t, packDir, "rig-add.sh")
	if err := os.MkdirAll(filepath.Join(packDir, "lifecycle", "city-start.sh"), 0o755); err != nil {
		t.Fatalf("mkdir city-start.sh dir: %v", err)
	}

	hooks := DiscoverPackLifecycleHooks(fsys.OSFS{}, packDir, "gasburger")
	if len(hooks) != 1 {
		t.Fatalf("hooks = %+v, want only city-stop", hooks)
	}
	if hooks[0].Event != LifecycleEventCityStop {
		t.Errorf("event = %q, want %q", hooks[0].Event, LifecycleEventCityStop)
	}
}

func TestDiscoverPackLifecycleHooksNoLifecycleDir(t *testing.T) {
	if hooks := DiscoverPackLifecycleHooks(fsys.OSFS{}, t.TempDir(), "gasburger"); hooks != nil {
		t.Fatalf("hooks = %+v, want nil", hooks)
	}
}

func TestLoadPackLifecycleHooksDedupesAndNamesPacks(t *testing.T) {
	root := t.TempDir()
	packA := filepath.Join(root, "a")
	packB := filepath.Join(root, "b")
	writePackManifest(t, packA, "alpha")
	writePackManifest(t, packB, "beta")
	writePackLifecycleScript(t, packA, "city-stop.sh")
	writePackLifecycleScript(t, packB, "city-stop.sh")

	hooks := LoadPackLifecycleHooks(fsys.OSFS{}, []string{packA, packB, packA}, LifecycleEventCityStop)
	if len(hooks) != 2 {
		t.Fatalf("hooks = %d, want 2 (%+v)", len(hooks), hooks)
	}
	if hooks[0].PackName != "alpha" || hooks[1].PackName != "beta" {
		t.Errorf("pack names = %q, %q; want alpha, beta", hooks[0].PackName, hooks[1].PackName)
	}
}

func TestLoadPackLifecycleHooksFiltersByEvent(t *testing.T) {
	packDir := t.TempDir()
	writePackManifest(t, packDir, "alpha")
	writePackLifecycleScript(t, packDir, "city-start.sh")
	writePackLifecycleScript(t, packDir, "city-stop.sh")

	hooks := LoadPackLifecycleHooks(fsys.OSFS{}, []string{packDir}, LifecycleEventCityStart)
	if len(hooks) != 1 || hooks[0].Event != LifecycleEventCityStart {
		t.Fatalf("hooks = %+v, want a single city-start hook", hooks)
	}
}

func TestLoadPackLifecycleHooksSkipsDirsWithoutPackManifest(t *testing.T) {
	packDir := t.TempDir()
	writePackLifecycleScript(t, packDir, "city-stop.sh")

	if hooks := LoadPackLifecycleHooks(fsys.OSFS{}, []string{packDir}, LifecycleEventCityStop); len(hooks) != 0 {
		t.Fatalf("hooks = %+v, want none without pack.toml", hooks)
	}
}

func TestLoadPackLifecycleHooksRejectsUnknownEvent(t *testing.T) {
	packDir := t.TempDir()
	writePackManifest(t, packDir, "alpha")
	writePackLifecycleScript(t, packDir, "city-stop.sh")

	if hooks := LoadPackLifecycleHooks(fsys.OSFS{}, []string{packDir}, "rig-add"); len(hooks) != 0 {
		t.Fatalf("hooks = %+v, want none for an unknown event", hooks)
	}
}
