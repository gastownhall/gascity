package acceptancehelpers

import "testing"

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
