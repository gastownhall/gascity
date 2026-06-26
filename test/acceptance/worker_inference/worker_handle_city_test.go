//go:build acceptance_c

package workerinference_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/gastownhall/gascity/internal/beads"
	workerpkg "github.com/gastownhall/gascity/internal/worker"
	helpers "github.com/gastownhall/gascity/test/acceptance/helpers"
)

func TestProfileUsesHookSessionKeyPersistence(t *testing.T) {
	for _, profile := range []workerpkg.Profile{
		workerpkg.ProfileOpenCodeTmuxCLI,
		workerpkg.ProfileMimoCodeTmuxCLI,
		workerpkg.ProfileKimiTmuxCLI,
		workerpkg.ProfilePiTmuxCLI,
		workerpkg.ProfileAntigravityTmuxCLI,
	} {
		require.True(t, profileUsesHookSessionKeyPersistence(profile), "profile %s persists its resume key via the provider hook plugin and must be city-backed", profile)
	}

	for _, profile := range []workerpkg.Profile{
		workerpkg.ProfileClaudeTmuxCLI,
		workerpkg.ProfileCodexTmuxCLI,
		workerpkg.ProfileGeminiTmuxCLI,
	} {
		require.False(t, profileUsesHookSessionKeyPersistence(profile), "profile %s must stay MemStore-backed", profile)
	}
}

// requireLiveHandleCityDeps skips when a `gc init` hard dependency is missing
// so the city-backed harness construction tests stay runnable in -short mode
// on minimal machines.
func requireLiveHandleCityDeps(t *testing.T) {
	t.Helper()
	for _, dep := range []string{"tmux", "jq", "git", "pgrep", "lsof"} {
		if _, err := exec.LookPath(dep); err != nil {
			t.Skipf("worker-handle city harness: %s not found in PATH", dep)
		}
	}
}

// stageShortModeMimoCodeSetup points the harness at the mimocode profile with
// a construction-only provider binary; no provider session is ever launched.
func stageShortModeMimoCodeSetup(t *testing.T) {
	t.Helper()
	prevSetup := liveSetup
	t.Cleanup(func() { liveSetup = prevSetup })
	liveSetup = providerSetup{
		Profile:    workerpkg.ProfileMimoCodeTmuxCLI,
		Provider:   "mimocode",
		BinaryPath: "/bin/sh", // construction-only; never started
	}
	t.Setenv("XIAOMI_API_KEY", "short-mode-staging-key")
}

// stageShortModeKimiSetup points the harness at the kimi profile with a
// construction-only provider binary; no provider session is ever launched.
// Conflicting host auth env vars are cleared so stageKimiAuth deterministically
// takes the KIMI_API_KEY branch and writes the native share-dir config.
func stageShortModeKimiSetup(t *testing.T) {
	t.Helper()
	prevSetup := liveSetup
	t.Cleanup(func() { liveSetup = prevSetup })
	liveSetup = providerSetup{
		Profile:    workerpkg.ProfileKimiTmuxCLI,
		Provider:   "kimi",
		BinaryPath: "/bin/sh", // construction-only; never started
	}
	t.Setenv("GC_WORKER_INFERENCE_KIMI_CONFIG_TOML", "")
	t.Setenv("GC_WORKER_INFERENCE_KIMI_CONFIG_FILE", "")
	t.Setenv("OLLAMA_API_KEY", "")
	t.Setenv("KIMI_API_KEY", "short-mode-staging-key")
}

// TestNewLiveWorkerHandleHarnessKimiIsCityBacked proves the kimi-specific
// staging chain minus the provider binary: a real city behind the harness,
// the overlay hook script materialized into the work dir, and the overlay
// [[hooks]] block merged into the staged share-dir config that kimi loads by
// default (kimi reads hooks only from its loaded config file, not from
// workdir dotfiles), without clobbering the staged auth.
func TestNewLiveWorkerHandleHarnessKimiIsCityBacked(t *testing.T) {
	requireLiveHandleCityDeps(t)
	stageShortModeKimiSetup(t)

	harness, err := newLiveWorkerHandleHarness(t)
	require.NoError(t, err)

	require.Equal(t, harness.workDir, harness.cityDir, "harness work dir must sit at the city root so cwd walk-up resolves the city")
	require.FileExists(t, filepath.Join(harness.cityDir, "city.toml"))
	require.IsType(t, &beads.FileStore{}, harness.store, "hook-managed profiles must use the shared city store, not MemStore")

	require.FileExists(t, filepath.Join(harness.workDir, ".kimi", "hooks", "gascity-session-start.py"))

	shareConfig, err := os.ReadFile(filepath.Join(harness.gcHome, ".kimi", "config.toml"))
	require.NoError(t, err)
	require.Contains(t, string(shareConfig), "gascity-session-start.py", "overlay SessionStart hook must be merged into the share-dir config kimi loads")
	require.Contains(t, string(shareConfig), `api_key = "short-mode-staging-key"`, "staged auth must survive the hook merge")
	require.Equal(t, filepath.Join(harness.gcHome, ".kimi"), harness.sessionEnv["KIMI_SHARE_DIR"])
}

func TestNewLiveWorkerHandleHarnessMimoCodeIsCityBacked(t *testing.T) {
	requireLiveHandleCityDeps(t)
	stageShortModeMimoCodeSetup(t)

	harness, err := newLiveWorkerHandleHarness(t)
	require.NoError(t, err)

	require.Equal(t, harness.workDir, harness.cityDir, "harness work dir must sit at the city root so cwd walk-up resolves the city")
	require.FileExists(t, filepath.Join(harness.cityDir, "city.toml"))

	gcPath, err := helpers.ResolveGCPath(liveEnv)
	require.NoError(t, err)
	require.Equal(t, harness.cityDir, harness.sessionEnv["GC_CITY"])
	require.Equal(t, gcPath, harness.sessionEnv["GC_BIN"])

	require.IsType(t, &beads.FileStore{}, harness.store, "hook-managed profiles must use the shared city store, not MemStore")
}

// TestLiveHandleCityStoreSharedWithGCPrimeHook proves the production
// persistence chain minus the provider binary: a session bead created by the
// in-process session manager is visible to a real `gc prime --hook` child
// (the same invocation the provider hook plugin makes), and the session key
// that child persists is visible back through the harness manager's store.
func TestLiveHandleCityStoreSharedWithGCPrimeHook(t *testing.T) {
	requireLiveHandleCityDeps(t)
	stageShortModeMimoCodeSetup(t)

	harness, err := newLiveWorkerHandleHarness(t)
	require.NoError(t, err)

	info, err := harness.handle.Create(context.Background(), workerpkg.CreateModeDeferred)
	require.NoError(t, err)
	require.NotEmpty(t, info.ID)

	gcPath, err := helpers.ResolveGCPath(liveEnv)
	require.NoError(t, err)

	// Run from a neutral directory: GC_CITY in the session env must be
	// enough for the gc child to resolve the city (belt and braces with the
	// cwd walk-up used when the plugin runs inside the session work dir).
	cmd := exec.Command(gcPath, "prime", "--hook")
	cmd.Dir = t.TempDir()
	childEnv := make([]string, 0, len(harness.sessionEnv)+3)
	for key, value := range harness.sessionEnv {
		childEnv = append(childEnv, key+"="+value)
	}
	childEnv = append(childEnv,
		"GC_SESSION_ID="+info.ID,
		"GC_PROVIDER_SESSION_ID=short-provider-session-key",
		"GC_PROVIDER_SESSION_ID_REQUIRED="+liveSetup.Provider,
	)
	cmd.Env = childEnv
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))

	bead, err := harness.store.Get(info.ID)
	require.NoError(t, err)
	require.Equal(t, "short-provider-session-key", bead.Metadata["session_key"])
}
