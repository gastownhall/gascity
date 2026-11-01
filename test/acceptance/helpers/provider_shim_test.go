package acceptancehelpers

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStageIdleProviderBinary(t *testing.T) {
	binDir := t.TempDir()
	if err := StageIdleProviderBinary(binDir, "claude"); err != nil {
		t.Fatalf("StageIdleProviderBinary: %v", err)
	}

	path := filepath.Join(binDir, "claude")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat staged provider: %v", err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("staged provider mode = %v, want executable", info.Mode())
	}
}

func TestProviderShimCommand_UsesDefaultWhenEnvUnset(t *testing.T) {
	shim, ok := providerShimCommand("claude_test_default", "aimux run claude --")
	if !ok {
		t.Fatal("providerShimCommand should use the default shim when env is unset")
	}
	if got, want := shim, "aimux run claude --"; got != want {
		t.Fatalf("providerShimCommand default = %q, want %q", got, want)
	}
}

func TestProviderShimCommand_EnvOverrideWins(t *testing.T) {
	t.Setenv("GC_ACCEPTANCE_PROVIDER_SHIM_CLAUDE", "custom-wrapper --")

	shim, ok := providerShimCommand("claude", "aimux run claude --")
	if !ok {
		t.Fatal("providerShimCommand should use the env override")
	}
	if got, want := shim, "custom-wrapper --"; got != want {
		t.Fatalf("providerShimCommand override = %q, want %q", got, want)
	}
}

func TestProviderShimCommand_EmptyOverrideDisablesDefault(t *testing.T) {
	t.Setenv("GC_ACCEPTANCE_PROVIDER_SHIM_CLAUDE", "")

	if shim, ok := providerShimCommand("claude", "aimux run claude --"); ok || shim != "" {
		t.Fatalf("providerShimCommand should disable the default shim, got ok=%v shim=%q", ok, shim)
	}
}

// The lifecycle fixture implements local readiness only; it is not inference evidence.
func TestStageIdleProviderAnswersNativeStatus(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"claude", []string{"auth", "status", "--json"}, `"loggedIn":true`},
		{"codex", []string{"login", "status"}, "Logged in"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := StageIdleProviderBinary(dir, tc.name); err != nil {
				t.Fatal(err)
			}
			cmd, _ := boundedToolCommand(t, time.Second, filepath.Join(dir, tc.name), tc.args...)
			output, err := cmd.CombinedOutput()
			if err != nil || !strings.Contains(string(output), tc.want) {
				t.Fatalf("local readiness protocol: err=%v output=%q", err, output)
			}
		})
	}
}
