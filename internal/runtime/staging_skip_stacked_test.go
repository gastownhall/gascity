package runtime

import (
	"os"
	"path/filepath"
	"testing"
)

// TestStageSessionWorkDirSkipsStackedHookPaths is the #5872 session-start half
// of AC-2: a runtime.Config whose SkipStackedHookPaths names the project-level
// codex hooks file must withhold it from overlay staging (the user-level file
// already provides those hooks, and codex stacks the two), while staging every
// non-listed sibling. An empty SkipStackedHookPaths must reproduce legacy
// staging exactly — the file is staged.
func TestStageSessionWorkDirSkipsStackedHookPaths(t *testing.T) {
	t.Parallel()

	stagedHook := func(t *testing.T, cfg Config) bool {
		t.Helper()
		workDir := t.TempDir()
		cfg.WorkDir = workDir
		if err := StageSessionWorkDirWithWarnings(cfg, nil); err != nil {
			t.Fatalf("StageSessionWorkDirWithWarnings: %v", err)
		}
		if _, err := os.Stat(filepath.Join(workDir, ".codex", "hooks.json")); err != nil {
			return false
		}
		return true
	}
	cfg := func() Config {
		return Config{
			ProviderName:        "codex",
			ProviderOverlayName: "codex",
			PackOverlayDirs:     []string{codexHooksOverlaySrc(t)},
		}
	}

	if !stagedHook(t, cfg()) {
		t.Fatal("empty SkipStackedHookPaths must stage .codex/hooks.json (legacy behavior)")
	}

	skipCfg := cfg()
	skipCfg.SkipStackedHookPaths = []string{".codex/hooks.json"}
	if stagedHook(t, skipCfg) {
		t.Fatal("SkipStackedHookPaths=[\".codex/hooks.json\"] did not withhold the stacked project hooks file")
	}

	// Non-listed siblings still stage with the skip set.
	workDir := t.TempDir()
	skipCfg.WorkDir = workDir
	if err := StageSessionWorkDirWithWarnings(skipCfg, nil); err != nil {
		t.Fatalf("StageSessionWorkDirWithWarnings: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workDir, "AGENTS.codex.md")); err != nil {
		t.Fatalf("non-listed sibling overlay not staged: %v", err)
	}
}
