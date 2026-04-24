package hooks

import (
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

// TestInstallWithResolver_WrappedCodex verifies that a wrapped custom
// provider name ("codex-mini") routes to the codex hook handler when the
// resolver maps it to "codex". Without the resolver the switch would
// fall through to the default branch and return "unsupported hook
// provider".
func TestInstallWithResolver_WrappedCodex(t *testing.T) {
	fs := fsys.NewFake()
	resolver := func(name string) string {
		if name == "codex-mini" {
			return "codex"
		}
		return ""
	}
	if err := InstallWithResolver(fs, "/city", "/work", []string{"codex-mini"}, resolver); err != nil {
		t.Fatalf("InstallWithResolver(codex-mini→codex) = %v, want nil", err)
	}
	// Codex installs .codex/hooks.json in the workDir.
	if _, err := fs.ReadFile(filepath.Join("/work", ".codex", "hooks.json")); err != nil {
		t.Errorf("expected /work/.codex/hooks.json to be written: %v", err)
	}
}

// TestInstallWithResolver_WrappedGemini covers the gemini-family sibling.
// Gemini's hook file lives under workDir/.gemini/settings.json, so a
// wrapped "gemini-fast" with resolver("gemini-fast")="gemini" must
// produce the same file.
func TestInstallWithResolver_WrappedGemini(t *testing.T) {
	fs := fsys.NewFake()
	resolver := func(name string) string {
		if name == "gemini-fast" {
			return "gemini"
		}
		return ""
	}
	if err := InstallWithResolver(fs, "/city", "/work", []string{"gemini-fast"}, resolver); err != nil {
		t.Fatalf("InstallWithResolver(gemini-fast→gemini) = %v, want nil", err)
	}
	if _, err := fs.ReadFile(filepath.Join("/work", ".gemini", "settings.json")); err != nil {
		t.Errorf("expected /work/.gemini/settings.json to be written: %v", err)
	}
}

// TestInstallWithResolver_NilResolverIsIdentity ensures backward compat:
// Install(nil-resolver) behaves exactly like Install with raw names.
func TestInstallWithResolver_NilResolverIsIdentity(t *testing.T) {
	fs := fsys.NewFake()
	if err := InstallWithResolver(fs, "/city", "/work", []string{"codex"}, nil); err != nil {
		t.Fatalf("InstallWithResolver(codex, nil) = %v, want nil", err)
	}
	if _, err := fs.ReadFile(filepath.Join("/work", ".codex", "hooks.json")); err != nil {
		t.Errorf("expected /work/.codex/hooks.json to be written: %v", err)
	}
}

// TestInstallWithResolver_EmptyFamilyFallsBackToName ensures a resolver
// returning "" (family undetermined) falls back to the raw name. A raw
// name that is a built-in family is still honored.
func TestInstallWithResolver_EmptyFamilyFallsBackToName(t *testing.T) {
	fs := fsys.NewFake()
	resolver := func(_ string) string { return "" } // always undetermined
	if err := InstallWithResolver(fs, "/city", "/work", []string{"codex"}, resolver); err != nil {
		t.Fatalf("InstallWithResolver fallback to identity = %v, want nil", err)
	}
	if _, err := fs.ReadFile(filepath.Join("/work", ".codex", "hooks.json")); err != nil {
		t.Errorf("expected /work/.codex/hooks.json to be written: %v", err)
	}
}

// TestInstallWithResolver_UnknownFamilyErrors confirms that a resolver
// mapping to an unknown family surfaces the usual "unsupported hook
// provider" error — wrapped aliases without a claude/codex/gemini/etc.
// ancestor do not silently no-op.
func TestInstallWithResolver_UnknownFamilyErrors(t *testing.T) {
	fs := fsys.NewFake()
	resolver := func(_ string) string { return "bogus" }
	err := InstallWithResolver(fs, "/city", "/work", []string{"my-alias"}, resolver)
	if err == nil {
		t.Fatal("InstallWithResolver(resolver→bogus) should error on unknown family")
	}
}

// TestValidateWithResolver_WrappedCodex verifies that a wrapped alias
// resolving to "codex" validates even though the raw name is not in the
// supported list.
func TestValidateWithResolver_WrappedCodex(t *testing.T) {
	resolver := func(name string) string {
		if name == "codex-mini" {
			return "codex"
		}
		return ""
	}
	if err := ValidateWithResolver([]string{"codex-mini"}, resolver); err != nil {
		t.Errorf("ValidateWithResolver(codex-mini→codex) = %v, want nil", err)
	}
}

// TestValidateWithResolver_UnknownNameErrors confirms the error path —
// an alias resolving to "" falls back to the raw name, which isn't in
// supported, so Validate reports the alias as unknown. The message
// surfaces the raw (user-visible) name so the operator can find it in
// their config.
func TestValidateWithResolver_UnknownNameErrors(t *testing.T) {
	err := ValidateWithResolver([]string{"my-unknown-alias"}, nil)
	if err == nil {
		t.Fatal("ValidateWithResolver should error for unknown alias")
	}
}
