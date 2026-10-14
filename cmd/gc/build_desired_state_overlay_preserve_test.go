package main

import (
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/hooks"
	"github.com/gastownhall/gascity/internal/runtime"
)

// The reconcile tick stages the per-provider overlay over the work directory on
// every pass. It had no version check and wrote no backup, so a managed hook
// file that internal/hooks considers current was silently reverted (#5554).
// This exercises the real reconcile path end to end.
func TestPrepareTemplateResolution_PreservesCurrentManagedHook(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "myrig")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(rig): %v", err)
	}

	rel := filepath.Join(".opencode", "plugins", "gascity.js")
	overlayDir := filepath.Join(cityDir, "packs", "myrig", "overlay")
	bundled := filepath.Join(overlayDir, "per-provider", "opencode", rel)
	if err := os.MkdirAll(filepath.Dir(bundled), 0o755); err != nil {
		t.Fatalf("MkdirAll(overlay): %v", err)
	}
	if err := os.WriteFile(bundled, []byte("// bundled overlay copy\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(bundled): %v", err)
	}

	// Install the real managed plugin into the work directory, then confirm the
	// policy layer regards it as current before asserting staging respects that.
	installed := filepath.Join(rigDir, rel)
	if err := os.MkdirAll(filepath.Dir(installed), 0o755); err != nil {
		t.Fatalf("MkdirAll(workdir): %v", err)
	}
	if err := hooks.Install(fsys.OSFS{}, cityDir, rigDir, []string{"opencode"}); err != nil {
		t.Fatalf("hooks.Install: %v", err)
	}
	current, err := os.ReadFile(installed)
	if err != nil {
		t.Fatalf("ReadFile(installed): %v", err)
	}
	if !hooks.PreserveManagedFile(rel, current) {
		t.Fatalf("fixture is not a current managed plugin; test would pass vacuously")
	}

	// A second overlay layer supplying the same managed path. A current local
	// file must survive every layer of the pass, not just the first.
	lateDir := filepath.Join(cityDir, "packs", "myrig", "overlay-late")
	writeOverlayLayerFile(t, lateDir, rel, "// late overlay copy\n")

	base := "builtin:opencode"
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:     "worker",
			Provider: "oc-local",
			Scope:    "rig",
			Dir:      "myrig",
		}},
		Providers:      map[string]config.ProviderSpec{"oc-local": {Base: &base, Command: "/bin/echo"}},
		Rigs:           []config.Rig{{Name: "myrig", Path: rigDir}},
		RigOverlayDirs: map[string][]string{"myrig": {overlayDir, lateDir}},
	}

	bp := newAgentBuildParams("test-city", cityDir, cfg, runtime.NewFake(), time.Now().UTC(), nil, io.Discard)
	prepareTemplateResolution(bp, &cfg.Agents[0], "myrig/worker", io.Discard)

	got, err := os.ReadFile(installed)
	if err != nil {
		t.Fatalf("ReadFile(after stage): %v", err)
	}
	if string(got) == "// bundled overlay copy\n" || string(got) == "// late overlay copy\n" {
		t.Fatal("reconcile staging reverted a current managed hook file")
	}
	if string(got) != string(current) {
		t.Fatalf("current managed hook file was modified by staging:\n%s", got)
	}
}

// writeOverlayLayerFile materializes one per-provider overlay source supplying
// rel with the given content, for tests that stage several layers in order.
func writeOverlayLayerFile(t *testing.T, overlayDir, rel, content string) {
	t.Helper()
	dst := filepath.Join(overlayDir, "per-provider", "opencode", rel)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", overlayDir, err)
	}
	if err := os.WriteFile(dst, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%s): %v", dst, err)
	}
}

// Preservation must not invert overlay precedence on the reconcile path. When
// the core pack ships the current managed plugin and a city override pack
// layers its own copy on top, the override has to win on a work directory that
// did not already hold the file — otherwise the first layer's write makes the
// destination look "current" to the predicate and permanently pins the path.
func TestPrepareTemplateResolution_LaterOverlayLayerOverridesManagedHook(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "myrig")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(rig): %v", err)
	}

	rel := filepath.Join(".opencode", "plugins", "gascity.js")

	// Layer one ships the genuine current managed plugin, so the preserve
	// predicate reports it as current the moment it lands.
	seedDir := t.TempDir()
	if err := hooks.Install(fsys.OSFS{}, cityDir, seedDir, []string{"opencode"}); err != nil {
		t.Fatalf("hooks.Install: %v", err)
	}
	current, err := os.ReadFile(filepath.Join(seedDir, rel))
	if err != nil {
		t.Fatalf("ReadFile(seed): %v", err)
	}
	if !hooks.PreserveManagedFile(rel, current) {
		t.Fatalf("seed is not a current managed plugin; test would pass vacuously")
	}

	coreDir := filepath.Join(cityDir, "packs", "myrig", "core")
	writeOverlayLayerFile(t, coreDir, rel, string(current))
	overrideDir := filepath.Join(cityDir, "packs", "myrig", "override")
	writeOverlayLayerFile(t, overrideDir, rel, "// city override copy\n")

	// The work directory starts without the managed file, so nothing here
	// pre-exists the staging pass.
	if _, err := os.Stat(filepath.Join(rigDir, rel)); !os.IsNotExist(err) {
		t.Fatalf("work dir unexpectedly already holds %s", rel)
	}

	base := "builtin:opencode"
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:     "worker",
			Provider: "oc-local",
			Scope:    "rig",
			Dir:      "myrig",
		}},
		Providers:      map[string]config.ProviderSpec{"oc-local": {Base: &base, Command: "/bin/echo"}},
		Rigs:           []config.Rig{{Name: "myrig", Path: rigDir}},
		RigOverlayDirs: map[string][]string{"myrig": {coreDir, overrideDir}},
	}

	bp := newAgentBuildParams("test-city", cityDir, cfg, runtime.NewFake(), time.Now().UTC(), nil, io.Discard)
	prepareTemplateResolution(bp, &cfg.Agents[0], "myrig/worker", io.Discard)

	got, err := os.ReadFile(filepath.Join(rigDir, rel))
	if err != nil {
		t.Fatalf("ReadFile(after stage): %v", err)
	}
	if string(got) != "// city override copy\n" {
		t.Fatalf("later overlay layer was suppressed by managed-hook preservation:\ngot:  %q\nwant: %q",
			got, "// city override copy\n")
	}
}
