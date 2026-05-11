package hooks

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

func claudeHookCommand(t *testing.T, data []byte, event string) string {
	t.Helper()
	entries := claudeHookEntries(t, data, event)
	if len(entries) == 0 || len(entries[0].Hooks) == 0 {
		t.Fatalf("missing claude hook for %s", event)
	}
	return entries[0].Hooks[0].Command
}

type claudeHookEntry struct {
	Matcher string `json:"matcher"`
	Hooks   []struct {
		Command string `json:"command"`
	} `json:"hooks"`
}

func claudeHookEntries(t *testing.T, data []byte, event string) []claudeHookEntry {
	t.Helper()
	var cfg struct {
		Hooks map[string][]claudeHookEntry `json:"hooks"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("unmarshal claude hooks: %v", err)
	}
	return cfg.Hooks[event]
}

func TestSupportedProviders(t *testing.T) {
	got := SupportedProviders()
	want := map[string]bool{
		"claude": true, "codex": true, "gemini": true, "kiro": true, "opencode": true,
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
	// Post stale-mirror fix: hooks/claude.json is no longer seeded on
	// fresh installs. The gc-managed .gc/settings.json is the sole
	// Install output for a claude-only fresh install.
	if _, ok := fs.Files["/city/hooks/claude.json"]; ok {
		t.Fatal("hooks/claude.json should NOT be written on fresh install (stale-mirror risk)")
	}
	runtimeData, ok := fs.Files["/city/.gc/settings.json"]
	if !ok {
		t.Fatal("expected /city/.gc/settings.json to be written")
	}
	s := string(runtimeData)
	if !strings.Contains(s, "SessionStart") {
		t.Error("claude settings should contain SessionStart hook")
	}
	sessionStartCommand := claudeHookCommand(t, runtimeData, "SessionStart")
	if !strings.Contains(sessionStartCommand, "gc prime --hook") {
		t.Error("claude SessionStart hook should contain gc prime --hook")
	}
	if !strings.Contains(sessionStartCommand, "GC_HOOK_EVENT_NAME=SessionStart") {
		t.Error("claude SessionStart hook should mark managed hook event")
	}
	if !strings.Contains(sessionStartCommand, "GC_MANAGED_SESSION_HOOK=1") {
		t.Error("claude SessionStart hook should mark managed hook invocation")
	}
	if entries := claudeHookEntries(t, runtimeData, "SessionStart"); len(entries) == 0 || entries[0].Matcher != "startup" {
		t.Errorf("claude SessionStart matcher should be \"startup\" to avoid re-injecting prompt on resume/clear/compact, got %q", func() string {
			if len(entries) == 0 {
				return ""
			}
			return entries[0].Matcher
		}())
	}
	if !strings.Contains(claudeHookCommand(t, runtimeData, "PreCompact"), `gc handoff --auto "context cycle"`) {
		t.Error("claude PreCompact hook should use gc handoff --auto (not gc prime or restart handoff) on compaction")
	}
	if !strings.Contains(s, "gc nudge drain --inject") {
		t.Error("claude settings should contain gc nudge drain --inject")
	}
	if strings.Contains(s, "gc hook --inject") {
		t.Error("fresh claude settings should not install no-op gc hook --inject")
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
	// Build a realistic stale fixture: the embedded file stores the command
	// as JSON, so the literal bytes contain escaped quotes. Matching that
	// shape is what claudeFileNeedsUpgrade expects.
	stale := strings.Replace(string(current), `gc handoff --auto \"context cycle\"`, `gc prime --hook`, 1)
	if stale == string(current) {
		t.Fatal("stale fixture did not diverge from current embedded config — check stale pattern")
	}
	fs.Files["/city/hooks/claude.json"] = []byte(stale)
	fs.Files["/city/.gc/settings.json"] = []byte(stale)

	if err := Install(fs, "/city", "/work", []string{"claude"}); err != nil {
		t.Fatalf("Install: %v", err)
	}

	hookData := fs.Files["/city/hooks/claude.json"]
	runtimeData := fs.Files["/city/.gc/settings.json"]
	if !strings.Contains(claudeHookCommand(t, hookData, "PreCompact"), `gc handoff --auto "context cycle"`) {
		t.Fatalf("upgraded claude hook missing gc handoff:\n%s", string(hookData))
	}
	if string(runtimeData) != string(hookData) {
		t.Fatalf("runtime Claude settings should mirror upgraded hook settings:\n%s", string(runtimeData))
	}
}

func TestInstallClaudeUpgradesRestartingPreCompactHandoff(t *testing.T) {
	fs := fsys.NewFake()
	current, err := readEmbedded("config/claude.json")
	if err != nil {
		t.Fatalf("readEmbedded: %v", err)
	}
	stale := strings.Replace(string(current), `gc handoff --auto \"context cycle\"`, `gc handoff \"context cycle\"`, 1)
	if stale == string(current) {
		t.Fatal("stale fixture did not diverge from current embedded config — check stale pattern")
	}
	fs.Files["/city/hooks/claude.json"] = []byte(stale)
	fs.Files["/city/.gc/settings.json"] = []byte(stale)

	if err := Install(fs, "/city", "/work", []string{"claude"}); err != nil {
		t.Fatalf("Install: %v", err)
	}

	hookData := fs.Files["/city/hooks/claude.json"]
	if !strings.Contains(claudeHookCommand(t, hookData, "PreCompact"), `gc handoff --auto "context cycle"`) {
		t.Fatalf("upgraded claude hook missing gc handoff --auto:\n%s", string(hookData))
	}
}

func TestInstallClaudeUpgradesGeneratedFileMissingManagedSessionMarkers(t *testing.T) {
	fs := fsys.NewFake()
	current, err := readEmbedded("config/claude.json")
	if err != nil {
		t.Fatalf("readEmbedded: %v", err)
	}
	stale := strings.Replace(string(current), `GC_MANAGED_SESSION_HOOK=1 GC_HOOK_EVENT_NAME=SessionStart gc prime --hook`, `gc prime --hook`, 1)
	if stale == string(current) {
		t.Fatal("stale fixture did not diverge from current embedded config — check SessionStart marker pattern")
	}
	fs.Files["/city/hooks/claude.json"] = []byte(stale)
	fs.Files["/city/.gc/settings.json"] = []byte(stale)

	if err := Install(fs, "/city", "/work", []string{"claude"}); err != nil {
		t.Fatalf("Install: %v", err)
	}

	hookData := fs.Files["/city/hooks/claude.json"]
	runtimeData := fs.Files["/city/.gc/settings.json"]
	sessionStartCommand := claudeHookCommand(t, hookData, "SessionStart")
	if !strings.Contains(sessionStartCommand, "GC_HOOK_EVENT_NAME=SessionStart") {
		t.Fatalf("upgraded SessionStart missing event marker: %s", sessionStartCommand)
	}
	if !strings.Contains(sessionStartCommand, "GC_MANAGED_SESSION_HOOK=1") {
		t.Fatalf("upgraded SessionStart missing managed marker: %s", sessionStartCommand)
	}
	if string(runtimeData) != string(hookData) {
		t.Fatalf("runtime Claude settings should mirror upgraded hook settings:\n%s", string(runtimeData))
	}
}

func TestInstallClaudeUpgradesGeneratedFileSessionStartMatcher(t *testing.T) {
	fs := fsys.NewFake()
	current, err := readEmbedded("config/claude.json")
	if err != nil {
		t.Fatalf("readEmbedded: %v", err)
	}
	stale := strings.Replace(string(current), `"matcher": "startup"`, `"matcher": ""`, 1)
	if stale == string(current) {
		t.Fatal("stale fixture did not diverge from current embedded config — check SessionStart matcher pattern")
	}
	fs.Files["/city/hooks/claude.json"] = []byte(stale)
	fs.Files["/city/.gc/settings.json"] = []byte(stale)

	if err := Install(fs, "/city", "/work", []string{"claude"}); err != nil {
		t.Fatalf("Install: %v", err)
	}

	hookData := fs.Files["/city/hooks/claude.json"]
	runtimeData := fs.Files["/city/.gc/settings.json"]
	if entries := claudeHookEntries(t, hookData, "SessionStart"); len(entries) == 0 || entries[0].Matcher != "startup" {
		t.Fatalf("upgraded hook SessionStart matcher = %q, want startup", func() string {
			if len(entries) == 0 {
				return ""
			}
			return entries[0].Matcher
		}())
	}
	if string(runtimeData) != string(hookData) {
		t.Fatalf("runtime Claude settings should mirror upgraded hook settings:\n%s", string(runtimeData))
	}
}

func TestInstallCodexUpgradesGeneratedFileMissingHookFormat(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/work/.codex/hooks.json"] = []byte(`{
  "hooks": {
    "SessionStart": [{
      "hooks": [{
        "type": "command",
        "command": "export PATH=\"$HOME/go/bin:$HOME/.local/bin:$PATH\" && gc prime --hook"
      }]
    }]
  }
}`)

	if err := Install(fs, "/city", "/work", []string{"codex"}); err != nil {
		t.Fatalf("Install: %v", err)
	}

	got := string(fs.Files["/work/.codex/hooks.json"])
	if !strings.Contains(got, "--hook-format codex") {
		t.Errorf("upgraded codex hooks missing Codex hook output format:\n%s", got)
	}
	if !strings.Contains(got, `"PreCompact"`) {
		t.Errorf("upgraded codex hooks missing PreCompact:\n%s", got)
	}
	if !strings.Contains(got, `gc handoff --auto --hook-format codex \"context cycle\"`) {
		t.Errorf("upgraded codex PreCompact missing auto handoff command:\n%s", got)
	}
}

func TestInstallCodexUpgradesManagedFileMissingPreCompact(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/work/.codex/hooks.json"] = []byte(`{
  "hooks": {
    "SessionStart": [{
      "hooks": [{
        "type": "command",
        "command": "export PATH=\"$HOME/go/bin:$HOME/.local/bin:$PATH\" && gc prime --hook --hook-format codex"
      }]
    }],
    "UserPromptSubmit": [{
      "hooks": [{
        "type": "command",
        "command": "export PATH=\"$HOME/go/bin:$HOME/.local/bin:$PATH\" && gc mail check --inject --hook-format codex"
      }]
    }]
  }
}`)

	if err := Install(fs, "/city", "/work", []string{"codex"}); err != nil {
		t.Fatalf("Install: %v", err)
	}

	got := string(fs.Files["/work/.codex/hooks.json"])
	if !strings.Contains(got, `"PreCompact"`) {
		t.Errorf("upgraded codex hooks missing PreCompact:\n%s", got)
	}
	if !strings.Contains(got, `gc handoff --auto --hook-format codex \"context cycle\"`) {
		t.Errorf("upgraded codex PreCompact missing auto handoff command:\n%s", got)
	}
}

func TestInstallCodexWritesCanonicalHookBytes(t *testing.T) {
	fs := fsys.NewFake()
	if err := Install(fs, "/city", "/work", []string{"codex"}); err != nil {
		t.Fatalf("Install: %v", err)
	}

	got := fs.Files["/work/.codex/hooks.json"]
	normalized, changed, err := normalizeCodexHookCommands(got)
	if err != nil {
		t.Fatalf("normalizeCodexHookCommands: %v", err)
	}
	if changed || !bytes.Equal(normalized, got) {
		t.Fatalf("codex hook install should write canonical bytes")
	}
}

func TestInstallCodexIsByteStableAcrossRepeatedInstalls(t *testing.T) {
	fs := fsys.NewFake()
	if err := Install(fs, "/city", "/work", []string{"codex"}); err != nil {
		t.Fatalf("first Install: %v", err)
	}
	before := append([]byte(nil), fs.Files["/work/.codex/hooks.json"]...)

	if err := Install(fs, "/city", "/work", []string{"codex"}); err != nil {
		t.Fatalf("second Install: %v", err)
	}
	after := fs.Files["/work/.codex/hooks.json"]
	if !bytes.Equal(before, after) {
		t.Fatalf("second Install rewrote codex hooks:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

func TestInstallCodexPreservesCustomOnlyHooksByteForByte(t *testing.T) {
	fs := fsys.NewFake()
	custom := []byte(`{"hooks":{"UserPromptSubmit":[{"hooks":[{"command":"printf custom-codex-hook","type":"command"}]}]}}`)
	fs.Files["/work/.codex/hooks.json"] = append([]byte(nil), custom...)

	if err := Install(fs, "/city", "/work", []string{"codex"}); err != nil {
		t.Fatalf("Install: %v", err)
	}
	got := fs.Files["/work/.codex/hooks.json"]
	if !bytes.Equal(custom, got) {
		t.Fatalf("custom-only codex hooks were rewritten:\nbefore:\n%s\nafter:\n%s", custom, got)
	}
}

func TestInstallCodexUpgradePreservesCustomHooks(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/work/.codex/hooks.json"] = []byte(`{
  "hooks": {
    "SessionStart": [{
      "hooks": [{
        "type": "command",
        "command": "export PATH=\"$HOME/go/bin:$HOME/.local/bin:$PATH\" && gc prime --hook"
      }]
    }],
    "UserPromptSubmit": [{
      "hooks": [{
        "type": "command",
        "command": "printf custom-codex-hook"
      }]
    }]
  }
}`)

	if err := Install(fs, "/city", "/work", []string{"codex"}); err != nil {
		t.Fatalf("Install: %v", err)
	}

	got := string(fs.Files["/work/.codex/hooks.json"])
	if !strings.Contains(got, "--hook-format codex") {
		t.Errorf("upgraded codex hooks missing Codex hook output format:\n%s", got)
	}
	if !strings.Contains(got, "printf custom-codex-hook") {
		t.Errorf("custom codex hook was not preserved:\n%s", got)
	}
	if !strings.Contains(got, `"PreCompact"`) {
		t.Errorf("managed codex upgrade should add PreCompact while preserving custom hooks:\n%s", got)
	}
}

func TestInstallCodexPreservesFullyCustomHooks(t *testing.T) {
	fs := fsys.NewFake()
	custom := []byte(`{
  "hooks": {
    "UserPromptSubmit": [{
      "hooks": [{
        "type": "command",
        "command": "printf custom-codex-hook"
      }]
    }]
  }
}`)
	fs.Files["/work/.codex/hooks.json"] = custom

	if err := Install(fs, "/city", "/work", []string{"codex"}); err != nil {
		t.Fatalf("Install: %v", err)
	}

	if got := string(fs.Files["/work/.codex/hooks.json"]); got != string(custom) {
		t.Fatalf("fully custom codex hooks were overwritten:\n%s", got)
	}
}

func TestUpgradeCodexHooksSkipsWhenDesiredPreCompactUnavailable(t *testing.T) {
	existing := []byte(`{
  "hooks": {
    "SessionStart": [{
      "hooks": [{
        "type": "command",
        "command": "gc prime --hook --hook-format codex"
      }]
    }]
  }
}`)
	for name, desired := range map[string][]byte{
		"malformed": []byte(`{not-json`),
		"missing":   []byte(`{"hooks":{}}`),
	} {
		t.Run(name, func(t *testing.T) {
			if _, changed, err := upgradeCodexHooks(existing, desired); err != nil || changed {
				t.Fatalf("changed = %v, err = %v, want unchanged without error", changed, err)
			}
		})
	}
}

func TestAddCodexPreCompactHookRejectsInvalidRoots(t *testing.T) {
	desired := []byte(`{"hooks":{"PreCompact":[{"hooks":[{"type":"command","command":"gc handoff --auto"}]}]}}`)
	for name, root := range map[string]any{
		"non-map-root": []any{},
		"custom-only": map[string]any{
			"hooks": map[string]any{
				"UserPromptSubmit": []any{map[string]any{
					"hooks": []any{map[string]any{"command": "printf custom"}},
				}},
			},
		},
		"missing-hooks-map": map[string]any{
			"other": []any{map[string]any{"command": "gc prime --hook"}},
		},
		"already-has-precompact": map[string]any{
			"hooks": map[string]any{
				"SessionStart": []any{map[string]any{
					"hooks": []any{map[string]any{"command": "gc prime --hook"}},
				}},
				"PreCompact": []any{},
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			if addCodexPreCompactHook(root, desired) {
				t.Fatalf("addCodexPreCompactHook(%s) = true, want false", name)
			}
		})
	}
}

func TestDesiredCodexPreCompactHookFallsBackToEmbeddedOverlay(t *testing.T) {
	if got := desiredCodexPreCompactHook(nil); got == nil {
		t.Fatal("desiredCodexPreCompactHook(nil) = nil, want embedded PreCompact hook")
	}
}

func TestInstallCodexPreservesUnreadableExistingHooks(t *testing.T) {
	workDir := t.TempDir()
	hookDir := filepath.Join(workDir, ".codex")
	if err := os.MkdirAll(hookDir, 0o755); err != nil {
		t.Fatalf("mkdir hooks dir: %v", err)
	}
	hookPath := filepath.Join(hookDir, "hooks.json")
	custom := []byte(`{"hooks":{"UserPromptSubmit":[{"hooks":[{"type":"command","command":"printf custom"}]}]}}`)
	if err := os.WriteFile(hookPath, custom, 0o644); err != nil {
		t.Fatalf("write hooks: %v", err)
	}
	if err := os.Chmod(hookPath, 0); err != nil {
		t.Fatalf("chmod hooks unreadable: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(hookPath, 0o644)
	})

	if err := Install(fsys.OSFS{}, "/city", workDir, []string{"codex"}); err != nil {
		t.Fatalf("Install: %v", err)
	}

	if err := os.Chmod(hookPath, 0o644); err != nil {
		t.Fatalf("restore hooks mode: %v", err)
	}
	got, err := os.ReadFile(hookPath)
	if err != nil {
		t.Fatalf("read hooks: %v", err)
	}
	if string(got) != string(custom) {
		t.Fatalf("unreadable codex hooks were overwritten:\n%s", string(got))
	}
}

func TestInstallClaudeUpgradesGeneratedFileWithCombinedKnownDrift(t *testing.T) {
	fs := fsys.NewFake()
	current, err := readEmbedded("config/claude.json")
	if err != nil {
		t.Fatalf("readEmbedded: %v", err)
	}
	stale := strings.Replace(string(current), `GC_MANAGED_SESSION_HOOK=1 GC_HOOK_EVENT_NAME=SessionStart gc prime --hook`, `gc prime --hook`, 1)
	stale = strings.Replace(stale, `"matcher": "startup"`, `"matcher": ""`, 1)
	if stale == string(current) {
		t.Fatal("stale fixture did not diverge from current embedded config — check combined SessionStart drift pattern")
	}
	fs.Files["/city/hooks/claude.json"] = []byte(stale)
	fs.Files["/city/.gc/settings.json"] = []byte(stale)

	if err := Install(fs, "/city", "/work", []string{"claude"}); err != nil {
		t.Fatalf("Install: %v", err)
	}

	hookData := fs.Files["/city/hooks/claude.json"]
	runtimeData := fs.Files["/city/.gc/settings.json"]
	sessionStartCommand := claudeHookCommand(t, hookData, "SessionStart")
	if !strings.Contains(sessionStartCommand, "GC_HOOK_EVENT_NAME=SessionStart") {
		t.Fatalf("upgraded combined-drift SessionStart missing event marker: %s", sessionStartCommand)
	}
	if !strings.Contains(sessionStartCommand, "GC_MANAGED_SESSION_HOOK=1") {
		t.Fatalf("upgraded combined-drift SessionStart missing managed marker: %s", sessionStartCommand)
	}
	if entries := claudeHookEntries(t, hookData, "SessionStart"); len(entries) == 0 || entries[0].Matcher != "startup" {
		t.Fatalf("upgraded combined-drift hook SessionStart matcher = %q, want startup", func() string {
			if len(entries) == 0 {
				return ""
			}
			return entries[0].Matcher
		}())
	}
	if string(runtimeData) != string(hookData) {
		t.Fatalf("runtime Claude settings should mirror upgraded combined-drift hook settings:\n%s", string(runtimeData))
	}
}

func TestInstallClaudeUpgradesGeneratedFileWithAllKnownDrift(t *testing.T) {
	fs := fsys.NewFake()
	current, err := readEmbedded("config/claude.json")
	if err != nil {
		t.Fatalf("readEmbedded: %v", err)
	}
	stale := strings.Replace(string(current), `gc handoff --auto \"context cycle\"`, `gc prime --hook`, 1)
	stale = strings.Replace(stale, `GC_MANAGED_SESSION_HOOK=1 GC_HOOK_EVENT_NAME=SessionStart gc prime --hook`, `gc prime --hook`, 1)
	stale = strings.Replace(stale, `"matcher": "startup"`, `"matcher": ""`, 1)
	if stale == string(current) {
		t.Fatal("stale fixture did not diverge from current embedded config — check all known Claude drift patterns")
	}
	fs.Files["/city/hooks/claude.json"] = []byte(stale)
	fs.Files["/city/.gc/settings.json"] = []byte(stale)

	if err := Install(fs, "/city", "/work", []string{"claude"}); err != nil {
		t.Fatalf("Install: %v", err)
	}

	hookData := fs.Files["/city/hooks/claude.json"]
	runtimeData := fs.Files["/city/.gc/settings.json"]
	sessionStartCommand := claudeHookCommand(t, hookData, "SessionStart")
	if !strings.Contains(sessionStartCommand, "GC_HOOK_EVENT_NAME=SessionStart") {
		t.Fatalf("upgraded all-drift SessionStart missing event marker: %s", sessionStartCommand)
	}
	if !strings.Contains(sessionStartCommand, "GC_MANAGED_SESSION_HOOK=1") {
		t.Fatalf("upgraded all-drift SessionStart missing managed marker: %s", sessionStartCommand)
	}
	if entries := claudeHookEntries(t, hookData, "SessionStart"); len(entries) == 0 || entries[0].Matcher != "startup" {
		t.Fatalf("upgraded all-drift hook SessionStart matcher = %q, want startup", func() string {
			if len(entries) == 0 {
				return ""
			}
			return entries[0].Matcher
		}())
	}
	if !strings.Contains(claudeHookCommand(t, hookData, "PreCompact"), `gc handoff --auto "context cycle"`) {
		t.Fatalf("upgraded all-drift PreCompact hook missing gc handoff:\n%s", string(hookData))
	}
	if string(runtimeData) != string(hookData) {
		t.Fatalf("runtime Claude settings should mirror upgraded all-drift hook settings:\n%s", string(runtimeData))
	}
}

func TestInstallClaudeMergesCityDotClaudeSettings(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/.claude/settings.json"] = []byte(`{
  "custom": true,
  "mcpServers": {
    "notes": {
      "command": "notes-mcp"
    }
  }
}`)

	if err := Install(fs, "/city", "/work", []string{"claude"}); err != nil {
		t.Fatalf("Install: %v", err)
	}

	data := string(fs.Files["/city/.gc/settings.json"])
	if !strings.Contains(data, `"custom": true`) {
		t.Fatalf("runtime settings missing custom top-level key:\n%s", data)
	}
	if !strings.Contains(data, `"mcpServers"`) {
		t.Fatalf("runtime settings missing merged mcpServers:\n%s", data)
	}
	if !strings.Contains(data, "SessionStart") {
		t.Fatalf("runtime settings lost default hooks during merge:\n%s", data)
	}
	// With the stale-mirror fix, installClaude no longer writes to
	// hooks/claude.json when the source is .claude/settings.json.
	// Writing a mirror would produce a stale file: if the user later
	// removes .claude/settings.json, desiredClaudeSettings would fall
	// back to the mirror as "legacy hook" and ship previous-generation
	// settings instead of current defaults.
	if _, ok := fs.Files["/city/hooks/claude.json"]; ok {
		t.Fatalf("hooks/claude.json should NOT be written when source is .claude/settings.json (stale-mirror risk)")
	}
}

func TestInstallClaudePrefersCityDotClaudeSettingsOverLegacyHookSource(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/.claude/settings.json"] = []byte(`{"preferred": true}`)
	fs.Files["/city/hooks/claude.json"] = []byte(`{"legacy": true}`)

	if err := Install(fs, "/city", "/work", []string{"claude"}); err != nil {
		t.Fatalf("Install: %v", err)
	}

	data := string(fs.Files["/city/.gc/settings.json"])
	if !strings.Contains(data, `"preferred": true`) {
		t.Fatalf("runtime settings missing preferred city .claude override:\n%s", data)
	}
	if strings.Contains(data, `"legacy": true`) {
		t.Fatalf("legacy hooks source should not win over city .claude/settings.json:\n%s", data)
	}
}

// TestInstallClaudePreservesUserOwnedHookFile verifies that when both
// .claude/settings.json and a hand-written hooks/claude.json are present,
// Install writes only the runtime settings file and leaves the user-owned
// hook file untouched. The old behavior silently rewrote hooks/claude.json
// with merged bytes, violating the "hook file is user-authored" contract.
func TestInstallClaudePreservesUserOwnedHookFile(t *testing.T) {
	fs := fsys.NewFake()
	userHook := []byte(`{"user_authored": true}`)
	fs.Files["/city/hooks/claude.json"] = userHook
	fs.Files["/city/.claude/settings.json"] = []byte(`{"custom": true}`)

	if err := Install(fs, "/city", "/work", []string{"claude"}); err != nil {
		t.Fatalf("Install: %v", err)
	}

	if got := string(fs.Files["/city/hooks/claude.json"]); got != string(userHook) {
		t.Errorf("user-owned hooks/claude.json was clobbered:\n  want: %q\n  got:  %q", userHook, got)
	}
	runtime := string(fs.Files["/city/.gc/settings.json"])
	if !strings.Contains(runtime, `"custom": true`) {
		t.Errorf("runtime settings missing .claude override merge:\n%s", runtime)
	}
	if !strings.Contains(runtime, "SessionStart") {
		t.Errorf("runtime settings missing embedded base hooks:\n%s", runtime)
	}
}

// TestInstallClaudeTolerantToUnreadableLegacyCandidate verifies that a
// non-chosen legacy candidate whose ReadFile fails (simulated by injecting
// a read error) does not block installation when .claude/settings.json is
// a valid higher-priority source. Previously readClaudeSettingsCandidate
// returned a hard error for any existing-but-unreadable candidate,
// aborting resolution even when the preferred source was perfectly fine.
func TestInstallClaudeTolerantToUnreadableLegacyCandidate(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/.claude/settings.json"] = []byte(`{"custom": true}`)
	// Inject a read error on the legacy hook path so any attempt to read
	// it fails. This models a permission-denied or i/o-error file that
	// would otherwise have made readClaudeSettingsCandidate abort source
	// selection.
	fs.Errors["/city/hooks/claude.json"] = errors.New("permission denied")

	if err := Install(fs, "/city", "/work", []string{"claude"}); err != nil {
		t.Fatalf("Install must tolerate unreadable non-chosen legacy candidate: %v", err)
	}

	runtime := string(fs.Files["/city/.gc/settings.json"])
	if !strings.Contains(runtime, `"custom": true`) {
		t.Errorf("runtime settings missing .claude override:\n%s", runtime)
	}
}

// TestInstallClaudePinnedHookFileOutranksRuntime verifies that when a user
// pins hooks/claude.json to content that happens to match the embedded
// defaults byte-for-byte, it still wins over .gc/settings.json per the
// documented precedence. Earlier versions disqualified any
// bytes-equal-base hook file, silently letting a stale .gc/settings.json
// override the user's chosen source.
func TestInstallClaudePinnedHookFileOutranksRuntime(t *testing.T) {
	fs := fsys.NewFake()
	base, err := readEmbedded("config/claude.json")
	if err != nil {
		t.Fatalf("readEmbedded: %v", err)
	}
	// User has pinned their hook file to exactly the embedded defaults
	// and separately has a stale .gc/settings.json with a custom key that
	// they intended to remove when they pinned the hook file.
	fs.Files["/city/hooks/claude.json"] = base
	fs.Files["/city/.gc/settings.json"] = []byte(`{"stale_override": true}`)

	if err := Install(fs, "/city", "/work", []string{"claude"}); err != nil {
		t.Fatalf("Install: %v", err)
	}

	runtime := string(fs.Files["/city/.gc/settings.json"])
	if strings.Contains(runtime, `"stale_override": true`) {
		t.Errorf("runtime must reflect pinned hook source, not stale runtime override:\n%s", runtime)
	}
	if !strings.Contains(runtime, "SessionStart") {
		t.Errorf("runtime must contain embedded default hooks:\n%s", runtime)
	}
}

// TestInstallClaudeUnreadableHookBlocksRuntimeFallback verifies that when
// hooks/claude.json exists-but-is-unreadable and .gc/settings.json exists
// with content, the tolerant-legacy path does NOT silently demote hook
// precedence and let the runtime file become the source. Earlier versions
// of the tolerant-read change skipped the unreadable hook file entirely,
// which allowed a stale .gc/settings.json to override the user-owned but
// currently-unreadable hook file — a precedence violation. The override
// now resolves to "no source" (embedded base defaults) so Claude launches
// with known-good settings instead.
func TestInstallClaudeUnreadableHookBlocksRuntimeFallback(t *testing.T) {
	fs := fsys.NewFake()
	fs.Errors["/city/hooks/claude.json"] = errors.New("permission denied")
	fs.Files["/city/.gc/settings.json"] = []byte(`{"stale_runtime_override": true}`)

	if err := Install(fs, "/city", "/work", []string{"claude"}); err != nil {
		t.Fatalf("Install: %v", err)
	}

	runtime := string(fs.Files["/city/.gc/settings.json"])
	if strings.Contains(runtime, `"stale_runtime_override": true`) {
		t.Errorf("unreadable hook must not let stale runtime override win:\n%s", runtime)
	}
	if !strings.Contains(runtime, "SessionStart") {
		t.Errorf("runtime must contain embedded base defaults:\n%s", runtime)
	}
}

// TestInstallClaudeUnreadableRuntimeDoesNotDemoteValidHook verifies that
// when hooks/claude.json is readable and .gc/settings.json is unreadable,
// the hook file still wins source selection — the runtime file is gc-owned,
// not user-owned, so its unreadability must not demote a legitimate user
// hook to "no source." A prior fixup blocked on either candidate being
// unreadable, which inverted precedence for this case.
func TestInstallClaudeUnreadableRuntimeDoesNotDemoteValidHook(t *testing.T) {
	fs := fsys.NewFake()
	// User pins hooks/claude.json with a custom key (not stale, not base).
	fs.Files["/city/hooks/claude.json"] = []byte(`{"user_hook": true}`)
	// The gc-managed runtime file is present but unreadable.
	fs.Errors["/city/.gc/settings.json"] = errors.New("permission denied")

	if err := Install(fs, "/city", "/work", []string{"claude"}); err != nil {
		// Install may surface an error from the force-overwrite write if
		// the injected error also blocks WriteFile (it does, in the Fake).
		// That's acceptable: a failed write surfaces loudly. What must NOT
		// happen is silent success with the stale unreadable runtime kept.
		if !strings.Contains(err.Error(), ".gc/settings.json") {
			t.Fatalf("unexpected error (expected a write failure surfacing the runtime path): %v", err)
		}
		return
	}
	// If Install succeeded, the runtime file must now contain the merged
	// hook-source content (which includes the user_hook key).
	runtime := string(fs.Files["/city/.gc/settings.json"])
	if !strings.Contains(runtime, `"user_hook": true`) {
		t.Errorf("runtime must reflect hook source even when prior runtime was unreadable:\n%s", runtime)
	}
}

// TestInstallClaudeForceOverwritesUnreadableRuntimeOSFS verifies the
// force-overwrite policy against a real filesystem. The gc-managed
// .gc/settings.json is seeded write-only (mode 0o200): stat succeeds,
// read fails, but WriteFile still succeeds. Under the old preserve
// policy Install would silently return without writing; under the new
// force-overwrite policy it attempts the write and succeeds. The Fake
// cannot express stat-ok/read-fail (its Errors map is symmetric across
// ReadFile, Stat, and WriteFile), so real OSFS is the only way to lock
// this branch.
//
// Skipped as root (root bypasses unix permission checks).
func TestInstallClaudeForceOverwritesUnreadableRuntimeOSFS(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses unix permission checks; cannot simulate stat-ok/read-fail")
	}
	cityDir := t.TempDir()
	claudeDir := filepath.Join(cityDir, ".claude")
	if err := os.MkdirAll(claudeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(claudeDir, "settings.json"), []byte(`{"custom": true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	gcDir := filepath.Join(cityDir, ".gc")
	if err := os.MkdirAll(gcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	runtimePath := filepath.Join(gcDir, "settings.json")
	if err := os.WriteFile(runtimePath, []byte(`{"stale": true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	// Write-only mode: Stat succeeds, ReadFile fails, WriteFile succeeds.
	// This is the only permission bitmask that can distinguish preserve-on-
	// unreadable from force-overwrite through observable behavior.
	if err := os.Chmod(runtimePath, 0o200); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(runtimePath, 0o644) })

	if err := Install(fsys.OSFS{}, cityDir, cityDir, []string{"claude"}); err != nil {
		t.Fatalf("Install with unreadable-but-writable runtime: %v", err)
	}

	// The file must be readable immediately after Install — no test-side
	// chmod. force-overwrite is responsible for normalizing the mode so
	// Claude can actually open --settings at launch time.
	//
	// Asserting the EXACT mode (0o600 from 0o200) pins the "minimal repair"
	// contract: we add ONLY the owner-read bit. A regression to a broader
	// chmod (e.g. unconditional 0o644) would widen other bits and still
	// pass a looser readability check — this assertion catches that.
	info, err := os.Stat(runtimePath)
	if err != nil {
		t.Fatalf("stat runtime after Install: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("runtime mode must be exactly 0o600 (0o200 + 0o400 owner-read); got %o", got)
	}
	data, err := os.ReadFile(runtimePath)
	if err != nil {
		t.Fatalf("reading runtime immediately after Install: %v", err)
	}
	runtime := string(data)
	if strings.Contains(runtime, `"stale": true`) {
		t.Errorf("runtime must be overwritten, not preserved:\n%s", runtime)
	}
	if !strings.Contains(runtime, `"custom": true`) {
		t.Errorf("runtime must reflect .claude/settings.json override:\n%s", runtime)
	}
}

// TestInstallClaudePreservesTightenedRuntimeMode verifies that a user who
// intentionally tightened .gc/settings.json permissions (e.g. 0o600 for
// privacy) keeps that mode after Install rewrites the file. The
// force-overwrite policy must only ADD owner-read when absent, never
// widen existing permissions.
//
// Skipped as root (root bypasses unix permission checks).
func TestInstallClaudePreservesTightenedRuntimeMode(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses unix permission checks")
	}
	cityDir := t.TempDir()
	claudeDir := filepath.Join(cityDir, ".claude")
	if err := os.MkdirAll(claudeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(claudeDir, "settings.json"), []byte(`{"custom": true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	gcDir := filepath.Join(cityDir, ".gc")
	if err := os.MkdirAll(gcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	runtimePath := filepath.Join(gcDir, "settings.json")
	// User-tightened: readable, but private (no group/other access).
	if err := os.WriteFile(runtimePath, []byte(`{"stale": true}`), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := Install(fsys.OSFS{}, cityDir, cityDir, []string{"claude"}); err != nil {
		t.Fatalf("Install: %v", err)
	}

	info, err := os.Stat(runtimePath)
	if err != nil {
		t.Fatal(err)
	}
	// Must preserve the user's 0o600, not widen to 0o644.
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("runtime mode widened from 0o600 to %o; force-overwrite must not override user tightening", got)
	}
}

// TestInstallClaudeSurfacesEmptyPreferredOverride verifies that a
// zero-byte .claude/settings.json is treated as malformed and surfaces a
// descriptive error rather than silently degrading to embedded defaults.
// A truncated or mid-edit file that happens to be zero bytes is
// indistinguishable from a valid "empty config" intent — strict behavior
// is to fail loudly so the user notices the truncation.
func TestInstallClaudeSurfacesEmptyPreferredOverride(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/.claude/settings.json"] = []byte{}

	err := Install(fs, "/city", "/work", []string{"claude"})
	if err == nil {
		t.Fatal("Install must surface empty .claude/settings.json as an error")
	}
	if !strings.Contains(err.Error(), ".claude/settings.json") {
		t.Errorf("error must name the offending path: %v", err)
	}
	if !strings.Contains(err.Error(), "empty") {
		t.Errorf("error must indicate emptiness: %v", err)
	}
}

// TestInstallClaudeSurfacesMalformedOverride verifies that a syntactically
// invalid .claude/settings.json surfaces a descriptive error rather than
// silently falling back to a legacy source or the embedded base.
func TestInstallClaudeSurfacesMalformedOverride(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/.claude/settings.json"] = []byte(`{not valid json`)

	err := Install(fs, "/city", "/work", []string{"claude"})
	if err == nil {
		t.Fatal("Install must surface malformed .claude/settings.json as an error")
	}
	if !strings.Contains(err.Error(), ".claude/settings.json") {
		t.Errorf("error must name the offending path: %v", err)
	}
}

// TestInstallOverlayManagedProviders verifies that overlay-managed providers
// are materialized from the embedded core pack overlay into the workdir.
func TestInstallOverlayManagedProviders(t *testing.T) {
	fs := fsys.NewFake()
	providers := []string{"codex", "gemini", "opencode", "copilot", "cursor", "kiro", "pi", "omp"}
	if err := Install(fs, "/city", "/work", providers); err != nil {
		t.Fatalf("Install: %v", err)
	}
	for _, rel := range []string{
		"/work/.codex/hooks.json",
		"/work/.gemini/settings.json",
		"/work/.opencode/plugins/gascity.js",
		"/work/.github/hooks/gascity.json",
		"/work/.github/copilot-instructions.md",
		"/work/.cursor/hooks.json",
		"/work/.kiro/agents/gascity.json",
		"/work/AGENTS.md",
		"/work/.pi/extensions/gc-hooks.js",
		"/work/.omp/hooks/gc-hook.ts",
	} {
		if _, ok := fs.Files[rel]; !ok {
			t.Errorf("expected overlay-managed provider file %s to be written", rel)
		}
	}
	codexHooks := string(fs.Files["/work/.codex/hooks.json"])
	if !strings.Contains(codexHooks, "--hook-format codex") {
		t.Error("codex hooks should request Codex hook output format")
	}
	if !strings.Contains(codexHooks, `"PreCompact"`) {
		t.Error("codex hooks should include PreCompact")
	}
	if !strings.Contains(codexHooks, `gc handoff --auto --hook-format codex \"context cycle\"`) {
		t.Error("codex PreCompact should use auto handoff with Codex hook output format")
	}
	for _, rel := range []string{
		"/work/.codex/hooks.json",
		"/work/.gemini/settings.json",
		"/work/.opencode/plugins/gascity.js",
		"/work/.github/hooks/gascity.json",
		"/work/.cursor/hooks.json",
		"/work/.kiro/agents/gascity.json",
		"/work/AGENTS.md",
		"/work/.pi/extensions/gc-hooks.js",
		"/work/.omp/hooks/gc-hook.ts",
	} {
		if strings.Contains(string(fs.Files[rel]), "gc hook --inject") {
			t.Errorf("fresh overlay-managed provider file %s should not install no-op gc hook --inject", rel)
		}
	}
	var kiroAgent struct {
		Name   string `json:"name"`
		Prompt string `json:"prompt"`
		Hooks  map[string][]struct {
			Command string `json:"command"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(fs.Files["/work/.kiro/agents/gascity.json"], &kiroAgent); err != nil {
		t.Fatalf("unmarshal Kiro agent config: %v", err)
	}
	if kiroAgent.Name != "gascity" {
		t.Errorf("Kiro agent name = %q, want gascity", kiroAgent.Name)
	}
	switch {
	case kiroAgent.Prompt == "":
		t.Error("Kiro agent config missing prompt")
	case !strings.HasPrefix(kiroAgent.Prompt, "file://"):
		t.Errorf("Kiro prompt = %q, want file:// URI", kiroAgent.Prompt)
	default:
		promptRel := strings.TrimPrefix(kiroAgent.Prompt, "file://")
		promptPath := filepath.Clean(filepath.Join(filepath.Dir("/work/.kiro/agents/gascity.json"), promptRel))
		if promptPath != "/work/AGENTS.md" {
			t.Errorf("Kiro prompt resolves to %q, want /work/AGENTS.md", promptPath)
		}
		if _, ok := fs.Files[promptPath]; !ok {
			t.Errorf("Kiro prompt target %s was not installed", promptPath)
		}
	}
	for _, trigger := range []string{"agentSpawn", "userPromptSubmit"} {
		if len(kiroAgent.Hooks[trigger]) == 0 {
			t.Errorf("Kiro agent config missing documented %s hook", trigger)
		}
	}
	for trigger := range kiroAgent.Hooks {
		switch trigger {
		case "agentSpawn", "userPromptSubmit", "preToolUse", "postToolUse", "stop":
		default:
			t.Errorf("Kiro agent config uses undocumented hook trigger %q", trigger)
		}
	}
	if strings.Contains(string(fs.Files["/work/.kiro/agents/gascity.json"]), "gc handoff") {
		t.Error("Kiro agent config should not install unsupported compaction handoff hooks")
	}
}

func TestInstallPiHookUsesCurrentExtensionAPI(t *testing.T) {
	fs := fsys.NewFake()
	if err := Install(fs, "/city", "/work", []string{"pi"}); err != nil {
		t.Fatalf("Install: %v", err)
	}

	data := string(fs.Files["/work/.pi/extensions/gc-hooks.js"])
	for _, want := range []string{
		"module.exports = function gascityPiExtension(pi)",
		`pi.on("session_start"`,
		`pi.on("session_compact"`,
		`pi.on("before_agent_start"`,
	} {
		if !strings.Contains(data, want) {
			t.Errorf("Pi hook missing current extension API marker %q:\n%s", want, data)
		}
	}
	for _, legacy := range []string{
		"module.exports = {",
		`"session.created"`,
		`"session.compacted"`,
		`"session.deleted"`,
		`"experimental.chat.system.transform"`,
	} {
		if strings.Contains(data, legacy) {
			t.Errorf("Pi hook still contains legacy API marker %q:\n%s", legacy, data)
		}
	}
}

func TestInstallPiHookUpgradesLegacyObjectExport(t *testing.T) {
	fs := fsys.NewFake()
	legacy := []byte(`// Gas City hooks for Pi Coding Agent.
module.exports = {
  name: "gascity",
  events: { "session.created": () => "" },
  hooks: { "experimental.chat.system.transform": (system) => system },
};
`)
	fs.Files["/work/.pi/extensions/gc-hooks.js"] = legacy

	if err := Install(fs, "/city", "/work", []string{"pi"}); err != nil {
		t.Fatalf("Install: %v", err)
	}

	data := string(fs.Files["/work/.pi/extensions/gc-hooks.js"])
	if data == string(legacy) {
		t.Fatal("legacy Pi object-export hook was preserved; expected managed upgrade")
	}
	if !strings.Contains(data, `pi.on("session_start"`) {
		t.Fatalf("upgraded Pi hook does not use current extension API:\n%s", data)
	}
}

func TestInstallPiHookPreservesUserAuthoredFile(t *testing.T) {
	fs := fsys.NewFake()
	custom := []byte(`module.exports = function customPiExtension(pi) {
  pi.on("session_start", () => {});
};
`)
	fs.Files["/work/.pi/extensions/gc-hooks.js"] = custom

	if err := Install(fs, "/city", "/work", []string{"pi"}); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if got := string(fs.Files["/work/.pi/extensions/gc-hooks.js"]); got != string(custom) {
		t.Fatalf("user-authored Pi hook was overwritten:\n%s", got)
	}
}

func TestInstallMultipleProviders(t *testing.T) {
	fs := fsys.NewFake()
	// Claude writes city-level files; overlay-managed names write their
	// provider hook files into workDir.
	err := Install(fs, "/city", "/work", []string{"claude", "codex", "gemini", "copilot"})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	// Post stale-mirror fix: hooks/claude.json is no longer written on
	// fresh installs (only when the user explicitly uses it as the
	// source). The gc-managed .gc/settings.json is what Install produces.
	if _, ok := fs.Files["/city/.gc/settings.json"]; !ok {
		t.Error("missing claude runtime settings")
	}
	for _, rel := range []string{
		"/work/.codex/hooks.json",
		"/work/.gemini/settings.json",
		"/work/.github/hooks/gascity.json",
	} {
		if _, ok := fs.Files[rel]; !ok {
			t.Errorf("expected overlay-managed provider file %s via Install", rel)
		}
	}
}

func TestInstallCodexWritesCanonicalJSON(t *testing.T) {
	fs := fsys.NewFake()

	if err := Install(fs, "/city", "/work", []string{"codex"}); err != nil {
		t.Fatalf("Install: %v", err)
	}

	data := fs.Files["/work/.codex/hooks.json"]
	if bytes.Contains(data, []byte(`\u0026`)) {
		t.Fatalf("codex hook escaped command operator:\n%s", data)
	}
	if !bytes.Contains(data, []byte(` && gc prime`)) {
		t.Fatalf("codex hook missing literal command operator:\n%s", data)
	}
	if !bytes.HasSuffix(data, []byte("\n")) {
		t.Fatalf("codex hook missing trailing newline:\n%s", data)
	}
}

func TestInstallIdempotent(t *testing.T) {
	fs := fsys.NewFake()
	// Pre-populate with a legacy hook file that carries a custom key. Under
	// the current contract this is treated as the chosen source and merged
	// against the embedded base so future default hooks land for users who
	// stayed on hooks/claude.json.
	fs.Files["/city/hooks/claude.json"] = []byte(`{"custom": true}`)

	if err := Install(fs, "/city", "/work", []string{"claude"}); err != nil {
		t.Fatalf("Install: %v", err)
	}

	hookData := string(fs.Files["/city/hooks/claude.json"])
	runtimeData := string(fs.Files["/city/.gc/settings.json"])
	if !strings.Contains(hookData, `"custom": true`) {
		t.Errorf("merge must preserve user-authored custom key in hook file:\n%s", hookData)
	}
	if !strings.Contains(hookData, "SessionStart") {
		t.Errorf("merge must pull embedded default hooks into hook file:\n%s", hookData)
	}
	if hookData != runtimeData {
		t.Error("runtime settings must mirror merged hook settings")
	}

	// A second Install must be a true no-op: bytes already match the merged
	// result, so writeManagedFile short-circuits.
	if err := Install(fs, "/city", "/work", []string{"claude"}); err != nil {
		t.Fatalf("second Install: %v", err)
	}
	if got := string(fs.Files["/city/hooks/claude.json"]); got != hookData {
		t.Errorf("second Install changed hook file bytes:\n  before: %q\n  after:  %q", hookData, got)
	}
	if got := string(fs.Files["/city/.gc/settings.json"]); got != runtimeData {
		t.Errorf("second Install changed runtime file bytes:\n  before: %q\n  after:  %q", runtimeData, got)
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
		supports := spec.SupportsHooks != nil && *spec.SupportsHooks
		if supports && !sup[name] {
			t.Errorf("provider %q has SupportsHooks=true but is not in hooks.SupportedProviders()", name)
		}
		if !supports && sup[name] {
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
