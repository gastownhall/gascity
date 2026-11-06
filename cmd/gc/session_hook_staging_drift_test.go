package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/bootstrap/packs/core"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// codexHookStagingCity builds a city whose codex agent declares NO
// install_agent_hooks, so nothing but overlay staging writes the work dir's
// .codex/hooks.json. The pack overlay carries the real embedded core codex
// hooks document, as a city with the core pack does in production.
func codexHookStagingCity(t *testing.T) (cityPath string, cfg *config.City) {
	t.Helper()
	cityPath = t.TempDir()
	packOverlay := filepath.Join(cityPath, "packs", "core", "overlay")
	data, err := core.PackFS.ReadFile("overlay/per-provider/codex/.codex/hooks.json")
	if err != nil {
		t.Fatalf("read embedded codex hooks overlay: %v", err)
	}
	overlayHook := filepath.Join(packOverlay, "per-provider", "codex", ".codex", "hooks.json")
	if err := os.MkdirAll(filepath.Dir(overlayHook), 0o755); err != nil {
		t.Fatalf("mkdir codex overlay: %v", err)
	}
	if err := os.WriteFile(overlayHook, data, 0o644); err != nil {
		t.Fatalf("write codex overlay: %v", err)
	}
	cfg = &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Providers: map[string]config.ProviderSpec{
			"codex": {Command: "echo", PromptMode: "none"},
		},
		PackOverlayDirs: []string{packOverlay},
		Agents: []config.Agent{{
			Name:              "run-operator",
			Provider:          "codex",
			MaxActiveSessions: intPtr(1),
			ScaleCheck:        "echo 1",
			WorkDir:           "worker",
		}},
	}
	return cityPath, cfg
}

func onlyDesiredSession(t *testing.T, state map[string]TemplateParams) (string, TemplateParams) {
	t.Helper()
	if len(state) != 1 {
		t.Fatalf("desired state size = %d, want 1: %#v", len(state), state)
	}
	for name, tp := range state {
		return name, tp
	}
	return "", TemplateParams{}
}

// TestConfigDrift_SessionStartHookStagingIsNotDrift reproduces the fresh-worker
// config-drift drain: a codex session starts, the session-start overlay staging
// writes the work dir's .codex/hooks.json (a file GC itself generates), and the
// next tick's CopyFiles probe finds a hook file the started fingerprint never
// saw. The started fingerprint and the next tick's fingerprint must agree, and
// the reconciler must not drain the session for it.
func TestConfigDrift_SessionStartHookStagingIsNotDrift(t *testing.T) {
	cityPath, cfg := codexHookStagingCity(t)

	first := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), nil, io.Discard)
	sessionName, firstTP := onlyDesiredSession(t, first.State)
	startedCfg := templateParamsToConfig(firstTP)
	startedHash := runtime.CoreFingerprint(startedCfg)

	// The session start: every runtime provider stages the provider overlay
	// (pack overlays + agent overlay) into the work dir before the process
	// launches (tmux.stageStartFiles, runtime.StageSessionWorkDir).
	if err := runtime.StageSessionWorkDir(startedCfg); err != nil {
		t.Fatalf("StageSessionWorkDir: %v", err)
	}
	workHook := filepath.Join(startedCfg.WorkDir, ".codex", "hooks.json")
	if _, err := os.Stat(workHook); err != nil {
		t.Fatalf("session-start staging did not write %s: %v", workHook, err)
	}

	second := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), nil, io.Discard)
	secondTP, ok := second.State[sessionName]
	if !ok {
		t.Fatalf("second desired state missing %q: %#v", sessionName, second.State)
	}
	currentCfg := templateParamsToConfig(secondTP)
	if got := runtime.CoreFingerprint(currentCfg); got != startedHash {
		t.Errorf("core fingerprint moved after GC staged its own hook file: started=%s current=%s drifted=%v\nstarted CopyFiles=%#v\ncurrent CopyFiles=%#v",
			startedHash, got, runtime.CoreFingerprintDriftFields(runtime.CoreFingerprintBreakdown(startedCfg), currentCfg),
			startedCfg.CopyFiles, currentCfg.CopyFiles)
	}

	// The reconciler's drift check over the same two ticks.
	env := newReconcilerTestEnv(t)
	env.cfg = cfg
	env.desiredState[sessionName] = secondTP
	if err := env.sp.Start(context.Background(), sessionName, startedCfg); err != nil {
		t.Fatalf("Start: %v", err)
	}
	session := env.createSessionBead(sessionName, secondTP.TemplateName)
	env.markSessionActive(&session)
	env.setSessionMetadata(&session, map[string]string{
		"started_config_hash": startedHash,
		"started_live_hash":   runtime.LiveFingerprint(startedCfg),
		"live_hash":           runtime.LiveFingerprint(startedCfg),
	})
	env.reconcile([]beads.Bead{session})

	if ds := env.dt.get(session.ID); ds != nil && ds.reason == "config-drift" {
		t.Fatalf("fresh codex session drained for config-drift after GC staged its own hooks file: %+v\nstderr=%s", ds, env.stderr.String())
	}
	if strings.Contains(env.stderr.String(), "config-drift "+sessionName) {
		t.Fatalf("reconciler reported config drift for GC-staged hook file:\n%s", env.stderr.String())
	}
}

// TestConfigDrift_RealHookContentChangeStillDrifts guards the other half of the
// invariant: once the hook file is part of the started fingerprint, an edit to
// its content is still a real config change.
func TestConfigDrift_RealHookContentChangeStillDrifts(t *testing.T) {
	cityPath, cfg := codexHookStagingCity(t)

	first := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), nil, io.Discard)
	sessionName, firstTP := onlyDesiredSession(t, first.State)
	startedCfg := templateParamsToConfig(firstTP)
	startedHash := runtime.CoreFingerprint(startedCfg)
	if err := runtime.StageSessionWorkDir(startedCfg); err != nil {
		t.Fatalf("StageSessionWorkDir: %v", err)
	}

	workHook := filepath.Join(startedCfg.WorkDir, ".codex", "hooks.json")
	edited := `{"hooks":{"SessionStart":[{"matcher":"","hooks":[{"type":"command","command":"operator-edited"}]}]}}`
	if err := os.WriteFile(workHook, []byte(edited), 0o644); err != nil {
		t.Fatalf("edit %s: %v", workHook, err)
	}
	// A real edit to the pack overlay is what changes what GC stages.
	overlayHook := filepath.Join(cfg.PackOverlayDirs[0], "per-provider", "codex", ".codex", "hooks.json")
	if err := os.WriteFile(overlayHook, []byte(edited), 0o644); err != nil {
		t.Fatalf("edit %s: %v", overlayHook, err)
	}

	second := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), nil, io.Discard)
	secondTP, ok := second.State[sessionName]
	if !ok {
		t.Fatalf("second desired state missing %q", sessionName)
	}
	if got := runtime.CoreFingerprint(templateParamsToConfig(secondTP)); got == startedHash {
		t.Fatalf("core fingerprint unchanged after a real hook content change: %s", got)
	}
}
