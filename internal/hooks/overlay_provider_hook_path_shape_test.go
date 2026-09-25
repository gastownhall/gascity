package hooks

import (
	"encoding/json"
	iofs "io/fs"
	"testing"

	"github.com/gastownhall/gascity/internal/bootstrap/packs/core"
)

// TestOverlayProviderHookCommandsAppendPathAndUseGCBinVar expresses the
// acceptance criteria for ga-5korc0: the codex, copilot, and antigravity
// hook overlay files must APPEND $HOME/go/bin:$HOME/.local/bin to PATH
// (never prepend it ahead of the caller's own PATH) and must invoke
// "${GC_BIN:-gc}" instead of a bare `gc`, matching the shape the cursor
// overlay already uses. Prepending lets a stale, pre-af7ad8a0fe
// PATH-shadowing `gc` binary win over a freshly built test binary inside
// CI/integration test harnesses (see TestCleanInstallTutorialPath), since
// installOverlayManaged (hooks.go) writes these files verbatim on a fresh
// install.
func TestOverlayProviderHookCommandsAppendPathAndUseGCBinVar(t *testing.T) {
	codexGot := extractCodexOverlayCommands(t)
	codexWant := map[string]string{
		"codex SessionStart":                  `export PATH="$PATH:$HOME/go/bin:$HOME/.local/bin" && GC_MANAGED_SESSION_HOOK=1 GC_HOOK_EVENT_NAME=SessionStart "${GC_BIN:-gc}" prime --hook --hook-format codex`,
		"codex PreCompact":                    `export PATH="$PATH:$HOME/go/bin:$HOME/.local/bin" && "${GC_BIN:-gc}" handoff --auto --hook-format codex "context cycle"`,
		"codex UserPromptSubmit[nudge drain]": `export PATH="$PATH:$HOME/go/bin:$HOME/.local/bin" && "${GC_BIN:-gc}" hook run --timeout 15s --timeout-exit-code 0 -- nudge drain --inject --hook-format codex`,
		"codex UserPromptSubmit[mail check]":  `export PATH="$PATH:$HOME/go/bin:$HOME/.local/bin" && "${GC_BIN:-gc}" hook run --timeout 15s --timeout-exit-code 0 -- mail check --inject --hook-format codex`,
	}
	assertOverlayCommands(t, codexGot, codexWant)

	copilotGot := extractCopilotOverlayCommands(t)
	copilotWant := map[string]string{
		"copilot sessionStart":                     `export PATH="$PATH:$HOME/go/bin:$HOME/.local/bin" && "${GC_BIN:-gc}" prime --hook`,
		"copilot preCompact":                       `export PATH="$PATH:$HOME/go/bin:$HOME/.local/bin" && "${GC_BIN:-gc}" handoff --auto "context cycle"`,
		"copilot userPromptSubmitted[nudge drain]": `export PATH="$PATH:$HOME/go/bin:$HOME/.local/bin" && "${GC_BIN:-gc}" hook run --timeout 15s --timeout-exit-code 0 -- nudge drain --inject`,
		"copilot userPromptSubmitted[mail check]":  `export PATH="$PATH:$HOME/go/bin:$HOME/.local/bin" && "${GC_BIN:-gc}" hook run --timeout 15s --timeout-exit-code 0 -- mail check --inject`,
	}
	assertOverlayCommands(t, copilotGot, copilotWant)

	antigravityGot := extractAntigravityOverlayCommands(t)
	antigravityWant := map[string]string{
		"antigravity gascity-prime":       `export PATH="$PATH:$HOME/go/bin:$HOME/.local/bin:/opt/homebrew/bin:/usr/local/bin" && GC_PROVIDER_SESSION_ID_REQUIRED=antigravity GC_PROVIDER_SESSION_ID="${ANTIGRAVITY_CONVERSATION_ID:-}" GC_MANAGED_SESSION_HOOK=1 GC_HOOK_EVENT_NAME=SessionStart "${GC_BIN:-gc}" prime --hook --hook-format antigravity`,
		"antigravity gascity-nudge-drain": `export PATH="$PATH:$HOME/go/bin:$HOME/.local/bin:/opt/homebrew/bin:/usr/local/bin" && "${GC_BIN:-gc}" hook run --timeout 15s --timeout-exit-code 0 -- nudge drain --inject --hook-format antigravity`,
		"antigravity gascity-mail-check":  `export PATH="$PATH:$HOME/go/bin:$HOME/.local/bin:/opt/homebrew/bin:/usr/local/bin" && "${GC_BIN:-gc}" hook run --timeout 15s --timeout-exit-code 0 -- mail check --inject --hook-format antigravity`,
	}
	assertOverlayCommands(t, antigravityGot, antigravityWant)
}

func assertOverlayCommands(t *testing.T, got, want map[string]string) {
	t.Helper()
	for label, wantCmd := range want {
		gotCmd, ok := got[label]
		if !ok {
			t.Errorf("%s: missing from parsed overlay file", label)
			continue
		}
		if gotCmd != wantCmd {
			t.Errorf("%s: command shape mismatch\n got:  %s\n want: %s", label, gotCmd, wantCmd)
		}
	}
}

func readOverlayFile(t *testing.T, relPath string) []byte {
	t.Helper()
	data, err := iofs.ReadFile(core.PackFS, relPath)
	if err != nil {
		t.Fatalf("reading %s from core.PackFS: %v", relPath, err)
	}
	return data
}

// extractCodexOverlayCommands parses .codex/hooks.json's
// {"hooks": {"<Event>": [{"matcher": "...", "hooks": [{"type":"command","command":"..."}]}]}}
// shape and returns a label->command map for every managed hook entry.
func extractCodexOverlayCommands(t *testing.T) map[string]string {
	t.Helper()
	data := readOverlayFile(t, "overlay/per-provider/codex/.codex/hooks.json")

	var parsed struct {
		Hooks map[string][]struct {
			Matcher string `json:"matcher"`
			Hooks   []struct {
				Type    string `json:"type"`
				Command string `json:"command"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("parsing codex overlay hooks.json: %v", err)
	}

	get := func(event string, hookIdx int) string {
		blocks := parsed.Hooks[event]
		if len(blocks) == 0 || hookIdx >= len(blocks[0].Hooks) {
			t.Fatalf("codex overlay: %s[0].hooks[%d] not found", event, hookIdx)
		}
		return blocks[0].Hooks[hookIdx].Command
	}

	return map[string]string{
		"codex SessionStart":                  get("SessionStart", 0),
		"codex PreCompact":                    get("PreCompact", 0),
		"codex UserPromptSubmit[nudge drain]": get("UserPromptSubmit", 0),
		"codex UserPromptSubmit[mail check]":  get("UserPromptSubmit", 1),
	}
}

// extractCopilotOverlayCommands parses .github/hooks/gascity.json's
// {"hooks": {"<event>": [{"type":"command","bash":"...","timeoutSec":30}]}}
// shape and returns a label->command map for every managed hook entry.
func extractCopilotOverlayCommands(t *testing.T) map[string]string {
	t.Helper()
	data := readOverlayFile(t, "overlay/per-provider/copilot/.github/hooks/gascity.json")

	var parsed struct {
		Hooks map[string][]struct {
			Type       string `json:"type"`
			Bash       string `json:"bash"`
			TimeoutSec int    `json:"timeoutSec"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("parsing copilot overlay gascity.json: %v", err)
	}

	get := func(event string, idx int) string {
		entries := parsed.Hooks[event]
		if idx >= len(entries) {
			t.Fatalf("copilot overlay: %s[%d] not found", event, idx)
		}
		return entries[idx].Bash
	}

	return map[string]string{
		"copilot sessionStart":                     get("sessionStart", 0),
		"copilot preCompact":                       get("preCompact", 0),
		"copilot userPromptSubmitted[nudge drain]": get("userPromptSubmitted", 0),
		"copilot userPromptSubmitted[mail check]":  get("userPromptSubmitted", 1),
	}
}

// extractAntigravityOverlayCommands parses .agents/hooks.json's
// {"<tool-name>": {"PreInvocation": [{"type":"command","command":"...","timeout":30}]}}
// shape and returns a label->command map for every managed hook entry.
func extractAntigravityOverlayCommands(t *testing.T) map[string]string {
	t.Helper()
	data := readOverlayFile(t, "overlay/per-provider/antigravity/.agents/hooks.json")

	var parsed map[string]map[string][]struct {
		Type    string `json:"type"`
		Command string `json:"command"`
		Timeout int    `json:"timeout"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("parsing antigravity overlay hooks.json: %v", err)
	}

	get := func(tool string) string {
		entries := parsed[tool]["PreInvocation"]
		if len(entries) == 0 {
			t.Fatalf("antigravity overlay: %s.PreInvocation not found", tool)
		}
		return entries[0].Command
	}

	return map[string]string{
		"antigravity gascity-prime":       get("gascity-prime"),
		"antigravity gascity-nudge-drain": get("gascity-nudge-drain"),
		"antigravity gascity-mail-check":  get("gascity-mail-check"),
	}
}
