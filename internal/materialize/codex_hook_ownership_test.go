package materialize

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/hooks"
	"github.com/gastownhall/gascity/internal/runtime"
)

func TestResolveConfiguredCodexHookOwnershipFollowsConfiguredHomeNotSessionKind(t *testing.T) {
	cityDir := t.TempDir()
	overlayDir := filepath.Join(cityDir, "overlay")
	overlayHookDir := filepath.Join(overlayDir, "per-provider", "codex", ".codex")
	if err := os.MkdirAll(overlayHookDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(overlay): %v", err)
	}
	if err := os.WriteFile(filepath.Join(overlayHookDir, "hooks.json"), []byte(`{"hooks":{"UserPromptSubmit":[{"hooks":[{"type":"command","command":"printf configured"}]}]}}`), 0o644); err != nil {
		t.Fatalf("WriteFile(overlay): %v", err)
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:     "worker",
			Provider: "codex",
			WorkDir:  filepath.Join(cityDir, "homes", "{{.Agent}}"),
		}},
		NamedSessions:   []config.NamedSession{{Name: "operator", Template: "worker"}},
		PackOverlayDirs: []string{overlayDir},
	}
	hints := runtime.Config{
		ProviderName:    "codex",
		PackOverlayDirs: []string{overlayDir},
	}
	layers, err := hooks.ReadCodexHookOverlayLayers(overlayDir, []string{"codex"})
	if err != nil {
		t.Fatalf("ReadCodexHookOverlayLayers: %v", err)
	}

	baseHome := filepath.Join(cityDir, "homes", "worker")
	namedHome := filepath.Join(cityDir, "homes", "operator")
	isolated := filepath.Join(cityDir, ".gc", "worktrees", "task")
	for _, home := range []string{baseHome, namedHome, isolated} {
		if _, err := hooks.ReconcileCodexHooks(fsys.OSFS{}, cityDir, home, layers); err != nil {
			t.Fatalf("ReconcileCodexHooks(%s): %v", home, err)
		}
	}

	for _, tc := range []struct {
		name    string
		workDir string
		want    bool
	}{
		{name: "agent home", workDir: baseHome, want: true},
		{name: "named-session home", workDir: namedHome, want: true},
		{name: "isolated worktree", workDir: isolated},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ResolveConfiguredCodexHookOwnership(cityDir, cfg, "worker", tc.workDir, hints, nil)
			if owned := got[CodexManagedMergeablePath] != ""; owned != tc.want {
				t.Fatalf("ownership = %v, digest=%q, want %v", got, got[CodexManagedMergeablePath], tc.want)
			}
		})
	}
}

func TestResolveConfiguredCodexHookOwnershipConfiguredHomeFailsClosed(t *testing.T) {
	cityDir := t.TempDir()
	home := filepath.Join(cityDir, "home")
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents:    []config.Agent{{Name: "worker", Provider: "codex", WorkDir: home}},
	}
	hints := runtime.Config{ProviderName: "codex"}

	assertBlocked := func(t *testing.T) {
		t.Helper()
		got := ResolveConfiguredCodexHookOwnership(cityDir, cfg, "worker", home, hints, nil)
		if digest, ok := got[CodexManagedMergeablePath]; !ok || digest != "" {
			t.Fatalf("ownership = %v, want fail-closed empty digest", got)
		}
	}

	t.Run("missing", func(t *testing.T) { assertBlocked(t) })
	t.Run("malformed", func(t *testing.T) {
		if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(home, ".codex", "hooks.json"), []byte(`{`), 0o644); err != nil {
			t.Fatal(err)
		}
		assertBlocked(t)
	})
	t.Run("unconverged", func(t *testing.T) {
		if err := os.WriteFile(filepath.Join(home, ".codex", "hooks.json"), []byte(`{"hooks":{}}`), 0o644); err != nil {
			t.Fatal(err)
		}
		assertBlocked(t)
	})
	t.Run("non-regular", func(t *testing.T) {
		otherHome := filepath.Join(cityDir, "other-home")
		otherCfg := &config.City{
			Workspace: config.Workspace{Name: "test-city"},
			Agents:    []config.Agent{{Name: "worker", Provider: "codex", WorkDir: otherHome}},
		}
		if err := os.MkdirAll(filepath.Join(otherHome, ".codex", "hooks.json"), 0o755); err != nil {
			t.Fatal(err)
		}
		got := ResolveConfiguredCodexHookOwnership(cityDir, otherCfg, "worker", otherHome, hints, nil)
		if digest, ok := got[CodexManagedMergeablePath]; !ok || digest != "" {
			t.Fatalf("ownership = %v, want non-regular file to fail closed", got)
		}
	})
}

func TestApplyVerifiedMergeableOwnershipRequiresCurrentCanonicalCopy(t *testing.T) {
	workDir := t.TempDir()
	hookPath := filepath.Join(workDir, ".codex", "hooks.json")
	if err := os.MkdirAll(filepath.Dir(hookPath), 0o755); err != nil {
		t.Fatal(err)
	}
	data := []byte(`{"hooks":{}}`)
	if err := os.WriteFile(hookPath, data, 0o644); err != nil {
		t.Fatal(err)
	}
	verified := map[string]string{CodexManagedMergeablePath: runtime.HashPathContent(hookPath)}
	cfg := runtime.Config{WorkDir: workDir, CopyFiles: []runtime.CopyEntry{{Src: "legacy", RelDst: CodexManagedMergeablePath}}}
	ApplyVerifiedMergeableOwnership(&cfg, verified)
	if len(cfg.ReconcilerOwnedMergeablePaths) != 1 || len(cfg.CopyFiles) != 1 {
		t.Fatalf("projected config = paths:%v copies:%+v", cfg.ReconcilerOwnedMergeablePaths, cfg.CopyFiles)
	}
	if got := cfg.CopyFiles[0]; got.Src != hookPath || !got.Probed || got.ContentHash == "" {
		t.Fatalf("canonical self-copy = %+v", got)
	}

	blocked := runtime.Config{WorkDir: workDir}
	ApplyVerifiedMergeableOwnership(&blocked, map[string]string{CodexManagedMergeablePath: ""})
	if len(blocked.ReconcilerOwnedMergeablePaths) != 1 || len(blocked.CopyFiles) != 0 {
		t.Fatalf("fail-closed projection = paths:%v copies:%+v", blocked.ReconcilerOwnedMergeablePaths, blocked.CopyFiles)
	}
}
