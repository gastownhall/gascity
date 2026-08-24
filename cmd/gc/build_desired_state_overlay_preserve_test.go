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
		RigOverlayDirs: map[string][]string{"myrig": {overlayDir}},
	}

	bp := newAgentBuildParams("test-city", cityDir, cfg, runtime.NewFake(), time.Now().UTC(), nil, io.Discard)
	prepareTemplateResolution(bp, &cfg.Agents[0], "myrig/worker", io.Discard)

	got, err := os.ReadFile(installed)
	if err != nil {
		t.Fatalf("ReadFile(after stage): %v", err)
	}
	if string(got) == "// bundled overlay copy\n" {
		t.Fatal("reconcile staging reverted a current managed hook file")
	}
	if string(got) != string(current) {
		t.Fatalf("current managed hook file was modified by staging:\n%s", got)
	}
}
