package hooks

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

func claudeHookCommand(t *testing.T, data []byte, event string) string {
	t.Helper()
	var cfg struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Command string `json:"command"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("unmarshal claude hooks: %v", err)
	}
	entries := cfg.Hooks[event]
	if len(entries) == 0 || len(entries[0].Hooks) == 0 {
		t.Fatalf("missing claude hook for %s", event)
	}
	return entries[0].Hooks[0].Command
}

func TestSupportedProviders(t *testing.T) {
	got := SupportedProviders()
	want := map[string]bool{
		"claude": true, "codex": true, "gemini": true, "opencode": true,
		"copilot": true, "cursor": true, "pi": true, "omp": true,
	}
	if len(got) != len(want) {
		t.Fatalf("SupportedProviders() = %v, want %d entries", got, len(want))
	}
	for _, p := range got {
		if !want[p] {
			t.Errorf("unexpected provider %q", p)
		}
	}
}

func TestValidateAcceptsSupported(t *testing.T) {
	if err := Validate([]string{"claude", "codex", "gemini"}); err != nil {
		t.Errorf("Validate([claude codex gemini]) = %v, want nil", err)
	}
}

func TestValidateRejectsUnsupported(t *testing.T) {
	err := Validate([]string{"claude", "amp", "auggie", "bogus"})
	if err == nil {
		t.Fatal("Validate should reject amp, auggie, and bogus")
	}
	if !strings.Contains(err.Error(), "amp (no hook mechanism)") {
		t.Errorf("error should mention amp: %v", err)
	}
	if !strings.Contains(err.Error(), "auggie (no hook mechanism)") {
		t.Errorf("error should mention auggie: %v", err)
	}
	if !strings.Contains(err.Error(), "bogus (unknown)") {
		t.Errorf("error should mention bogus: %v", err)
	}
}

func TestValidateEmpty(t *testing.T) {
	if err := Validate(nil); err != nil {
		t.Errorf("Validate(nil) = %v, want nil", err)
	}
}

func TestInstallClaude(t *testing.T) {
	fs := fsys.NewFake()
	err := Install(fs, "/city", "/work", []string{"claude"})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	data, ok := fs.Files["/city/hooks/claude.json"]
	if !ok {
		t.Fatal("expected /city/hooks/claude.json to be written")
	}
	runtimeData, ok := fs.Files["/city/.gc/settings.json"]
	if !ok {
		t.Fatal("expected /city/.gc/settings.json to be written")
	}
	s := string(data)
	if !strings.Contains(s, "SessionStart") {
		t.Error("claude settings should contain SessionStart hook")
	}
	if string(runtimeData) != string(data) {
		t.Error("runtime Claude settings should mirror hooks/claude.json")
	}
	if !strings.Contains(claudeHookCommand(t, data, "SessionStart"), "gc prime --hook") {
		t.Error("claude SessionStart hook should contain gc prime --hook")
	}
	if !strings.Contains(claudeHookCommand(t, data, "PreCompact"), `gc handoff "context cycle"`) {
		t.Error("claude PreCompact hook should use gc handoff (not gc prime) to avoid context accumulation on compaction")
	}
	if !strings.Contains(s, "gc nudge drain --inject") {
		t.Error("claude settings should contain gc nudge drain --inject")
	}
	if !strings.Contains(s, `"skipDangerousModePermissionPrompt": true`) {
		t.Error("claude settings should contain skipDangerousModePermissionPrompt")
	}
	if !strings.Contains(s, `"editorMode": "normal"`) {
		t.Error("claude settings should contain editorMode")
	}
	if !strings.Contains(s, `$HOME/go/bin`) {
		t.Error("claude hook commands should include PATH export")
	}
}

func TestInstallClaudeUpgradesStaleGeneratedFile(t *testing.T) {
	fs := fsys.NewFake()
	current, err := readEmbedded("config/claude.json")
	if err != nil {
		t.Fatalf("readEmbedded: %v", err)
	}
	stale := strings.Replace(string(current), `gc handoff "context cycle"`, `gc prime --hook`, 1)
	fs.Files["/city/hooks/claude.json"] = []byte(stale)
	fs.Files["/city/.gc/settings.json"] = []byte(stale)

	if err := Install(fs, "/city", "/work", []string{"claude"}); err != nil {
		t.Fatalf("Install: %v", err)
	}

	hookData := fs.Files["/city/hooks/claude.json"]
	runtimeData := fs.Files["/city/.gc/settings.json"]
	if !strings.Contains(claudeHookCommand(t, hookData, "PreCompact"), `gc handoff "context cycle"`) {
		t.Fatalf("upgraded claude hook missing gc handoff:\n%s", string(hookData))
	}
	if string(runtimeData) != string(hookData) {
		t.Fatalf("runtime Claude settings should mirror upgraded hook settings:\n%s", string(runtimeData))
	}
}

// TestInstallOverlayManagedNoOp verifies that providers whose hooks ship via
// the core pack overlay are accepted by Install but produce no Go-side files.
func TestInstallOverlayManagedNoOp(t *testing.T) {
	fs := fsys.NewFake()
	providers := []string{"codex", "gemini", "opencode", "copilot", "cursor", "pi", "omp"}
	if err := Install(fs, "/city", "/work", providers); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if len(fs.Files) != 0 {
		t.Errorf("overlay-managed providers should not write any files; got %v", fs.Files)
	}
}

func TestInstallMultipleProviders(t *testing.T) {
	fs := fsys.NewFake()
	// Claude writes city-level files; the overlay-managed names are accepted
	// but produce nothing here.
	err := Install(fs, "/city", "/work", []string{"claude", "codex", "gemini", "copilot"})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if _, ok := fs.Files["/city/hooks/claude.json"]; !ok {
		t.Error("missing claude settings")
	}
	if _, ok := fs.Files["/city/.gc/settings.json"]; !ok {
		t.Error("missing claude runtime settings")
	}
	for _, rel := range []string{
		"/work/.codex/hooks.json",
		"/work/.gemini/settings.json",
		"/work/.github/hooks/gascity.json",
	} {
		if _, ok := fs.Files[rel]; ok {
			t.Errorf("overlay-managed provider should not write %s via Install", rel)
		}
	}
}

func TestInstallIdempotent(t *testing.T) {
	fs := fsys.NewFake()
	// Pre-populate with custom content.
	fs.Files["/city/hooks/claude.json"] = []byte(`{"custom": true}`)

	err := Install(fs, "/city", "/work", []string{"claude"})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}

	// Should not overwrite existing file.
	got := string(fs.Files["/city/hooks/claude.json"])
	if got != `{"custom": true}` {
		t.Errorf("Install overwrote existing file: got %q", got)
	}
	if runtime := string(fs.Files["/city/.gc/settings.json"]); runtime != `{"custom": true}` {
		t.Errorf("Install should mirror existing hook settings into runtime file: got %q", runtime)
	}
}

func TestInstallUnknownProvider(t *testing.T) {
	fs := fsys.NewFake()
	err := Install(fs, "/city", "/work", []string{"bogus"})
	if err == nil {
		t.Fatal("Install should reject unknown provider")
	}
	if !strings.Contains(err.Error(), "unsupported") {
		t.Errorf("error should mention unsupported: %v", err)
	}
}

// TestSupportsHooksSyncWithProviderSpec verifies that the hooks supported list
// stays in sync with ProviderSpec.SupportsHooks across all builtin providers.
func TestSupportsHooksSyncWithProviderSpec(t *testing.T) {
	sup := make(map[string]bool, len(SupportedProviders()))
	for _, p := range SupportedProviders() {
		sup[p] = true
	}

	providers := config.BuiltinProviders()
	for name, spec := range providers {
		if spec.SupportsHooks && !sup[name] {
			t.Errorf("provider %q has SupportsHooks=true but is not in hooks.SupportedProviders()", name)
		}
		if !spec.SupportsHooks && sup[name] {
			t.Errorf("provider %q is in hooks.SupportedProviders() but has SupportsHooks=false", name)
		}
	}
	// Reverse check: every supported provider must be a known builtin.
	for _, p := range SupportedProviders() {
		if _, ok := providers[p]; !ok {
			t.Errorf("hooks.SupportedProviders() contains %q which is not a builtin provider", p)
		}
	}
}

func TestInstallEmpty(t *testing.T) {
	fs := fsys.NewFake()
	err := Install(fs, "/city", "/work", nil)
	if err != nil {
		t.Fatalf("Install(nil): %v", err)
	}
	if len(fs.Files) != 0 {
		t.Errorf("Install(nil) should not write files; got %v", fs.Files)
	}
}
