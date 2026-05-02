package gastown_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPreCompactHookUsesAutoHandoff guards the PreCompact hook in the gastown
// pack overlay against the destructive-eviction regression documented in
// gc-flp1.
//
// `gc handoff` (bare) sends mail AND requests a controller restart, which kills
// the running session. For PreCompact — which fires automatically on every
// Claude context-cycle inside any session running this overlay — restart-mode
// turns every compaction into a session kill. When a human is mid-conversation
// in a crew session (Mayor, Navani, Syl, etc.), this destroys their state.
//
// `gc handoff --auto` is the documented mode for this scenario: send mail,
// skip restart, return immediately. The internal SDK hook config
// (internal/hooks/config/claude.json) was switched to --auto in commit
// 7b3b913a ("fix: add auto handoff for precompact"); the gastown pack overlay
// must match.
func TestPreCompactHookUsesAutoHandoff(t *testing.T) {
	dir := exampleDir()
	path := filepath.Join(dir, "packs", "gastown", "overlay", ".claude", "settings.json")

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading overlay settings: %v", err)
	}

	var cfg struct {
		Hooks struct {
			PreCompact []struct {
				Hooks []struct {
					Type    string `json:"type"`
					Command string `json:"command"`
				} `json:"hooks"`
			} `json:"PreCompact"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("parsing overlay settings as JSON: %v", err)
	}
	if len(cfg.Hooks.PreCompact) == 0 {
		t.Fatalf("expected PreCompact hooks in overlay settings; got none")
	}

	var sawHandoff bool
	for _, group := range cfg.Hooks.PreCompact {
		for _, h := range group.Hooks {
			if !strings.Contains(h.Command, "gc handoff") {
				continue
			}
			sawHandoff = true
			if !strings.Contains(h.Command, "--auto") {
				t.Errorf("PreCompact hook invokes 'gc handoff' without --auto; bare gc handoff requests a restart and kills the session on every compaction (gc-flp1).\n  command: %q\n  fix: insert --auto, e.g. 'gc handoff --auto \"context cycle\"'", h.Command)
			}
		}
	}
	if !sawHandoff {
		t.Errorf("PreCompact hook does not call 'gc handoff' at all; expected 'gc handoff --auto \"context cycle\"'")
	}
}
