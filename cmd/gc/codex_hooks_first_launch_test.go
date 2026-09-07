package main

import (
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// A fresh workspace must include overlay-owned hooks in its first fingerprint,
// so runtime staging cannot make the next reconcile tick drain the new session.
func TestCodexOverlayFirstLaunchFingerprintStableWithoutHookInstaller(t *testing.T) {
	cityDir, workDir := t.TempDir(), t.TempDir()
	overlayDir := seedCodexOverlay(t)
	base := "builtin:codex"
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents:    []config.Agent{{Name: "assistant", Provider: "custom-codex", WorkDir: workDir}},
		Providers: map[string]config.ProviderSpec{
			"custom-codex": {Base: &base, Command: "/bin/echo", ResumeCommand: "/bin/echo resume {{.SessionKey}}"},
		},
		PackOverlayDirs: []string{overlayDir},
	}
	bp := newAgentBuildParams("test-city", cityDir, cfg, runtime.NewFake(), time.Now().UTC(), nil, io.Discard)
	prepareTemplateResolution(bp, &cfg.Agents[0], "assistant", io.Discard)
	start := runtime.Config{WorkDir: workDir, ProviderName: "codex", ProviderOverlayName: "custom-codex", PackOverlayDirs: cfg.PackOverlayDirs}
	start.CopyFiles = stageHookFiles(nil, cityDir, workDir, []string{"codex"})
	before := runtime.CoreFingerprint(start)
	if err := runtime.StageSessionWorkDirWithWarnings(start, io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(workDir, ".codex", "hooks.json")); err != nil {
		t.Fatalf("runtime did not stage the actual Codex hook overlay: %v", err)
	}
	prepareTemplateResolution(bp, &cfg.Agents[0], "assistant", io.Discard)
	start.CopyFiles = stageHookFiles(nil, cityDir, workDir, []string{"codex"})
	if after := runtime.CoreFingerprint(start); after != before {
		t.Fatalf("first runtime staging changed the prepared fingerprint: before=%s after=%s", before, after)
	}
}
