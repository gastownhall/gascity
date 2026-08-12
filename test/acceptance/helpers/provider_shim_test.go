package acceptancehelpers

import (
	"os"
	"path/filepath"
	"testing"
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
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read staged provider: %v", err)
	}
	if got, want := string(body), "#!/bin/sh\nexec sleep 3600\n"; got != want {
		t.Fatalf("staged provider body = %q, want %q", got, want)
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
