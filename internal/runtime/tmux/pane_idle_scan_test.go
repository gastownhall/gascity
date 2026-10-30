package tmux

import (
	"context"
	"strings"
	"testing"
	"time"
)

// PaneShowsIdlePrompt is the scan both local tmux (snapshotPaneIdleWithPrefix)
// and remote tmux-in-box runtimes apply to a captured pane, so a busy
// indicator must win over a visible prompt, the prompt may sit above a status
// footer, and NBSP or a boxed prompt must still match.
// Kills: a scan that ignores the busy indicator, stops at the last non-blank
// line, applies a default for a blank prefix, drops the NBSP / box-border
// normalization, or reads a transcript line ending in a gerund and "…" as
// Claude's spinner row.
func TestPaneShowsIdlePrompt(t *testing.T) {
	tests := []struct {
		name   string
		lines  []string
		prefix string
		want   bool
	}{
		{"idle claude prompt", []string{"output", "❯ ", ""}, DefaultReadyPromptPrefix, true},
		{"prompt above status footer", []string{"❯ ", "", "  ⏵⏵ bypass permissions on", ""}, DefaultReadyPromptPrefix, true},
		{"busy esc to interrupt", []string{"❯ ", "  esc to interrupt  "}, DefaultReadyPromptPrefix, false},
		{"busy spinner footer", []string{"❯ ", "· Boogieing… (2m 28s · ↓ 10.9k tokens)"}, DefaultReadyPromptPrefix, false},
		{"busy bare spinner row", []string{"· Tinkering…", "", "❯\u00a0"}, DefaultReadyPromptPrefix, false},
		{"idle below transcript gerunds", []string{"✻ Worked for 1m 49s", "  1. Fixing… done, see the commit above", "> Investigating… turned up nothing", "❯ Testing…", "────", "❯\u00a0", "────", "  ⏵⏵ bypass permissions on"}, DefaultReadyPromptPrefix, true},
		{"nbsp after prompt glyph", []string{"❯\u00a0"}, DefaultReadyPromptPrefix, true},
		{"boxed prompt", []string{"╭──────╮", "│ ❯ ", "╰──────╯"}, DefaultReadyPromptPrefix, true},
		{"custom prefix matches", []string{"› "}, "› ", true},
		{"custom prefix ignores default glyph", []string{"❯ "}, "› ", false},
		{"no prompt", []string{"building...", "still going"}, DefaultReadyPromptPrefix, false},
		{"empty pane", nil, DefaultReadyPromptPrefix, false},
		{"blank prefix is not defaulted", []string{"❯ "}, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := PaneShowsIdlePrompt(tt.lines, tt.prefix); got != tt.want {
				t.Errorf("PaneShowsIdlePrompt(%q, %q) = %v, want %v", tt.lines, tt.prefix, got, tt.want)
			}
		})
	}
}

// codexTrustMenuLines is codex-cli 0.157's folder-trust dialog: the selected
// option carries codex's prompt glyph, so its row matches the "› " prefix.
var codexTrustMenuLines = []string{
	"  Trust this folder? Codex can read, edit, and run files here.",
	"",
	"› 1. Trust and continue",
	"  2. Quit",
	"",
	"  enter continue · esc quit",
}

// A numbered menu whose selected row carries the prompt glyph (codex's
// folder-trust dialog) is waiting for an answer, not an idle composer: a nudge
// typed now lands in the dialog, and codex's leading Escape quits it.
func TestPaneShowsIdlePromptMenuCursorRowIsNotIdle(t *testing.T) {
	if PaneShowsIdlePrompt(codexTrustMenuLines, "› ") {
		t.Errorf("PaneShowsIdlePrompt(%q, %q) = true, want false: a numbered menu's cursor row is not a ready prompt", codexTrustMenuLines, "› ")
	}
}

// Codex echoes past user turns with its prompt glyph, so a dialog drawn below
// transcript is not the only prefix line on screen. The bottom-most prompt line
// is the live one; an echoed turn above the menu is history, not a composer.
// Kills: a scan that skips the menu row and keeps looking for any other prompt
// line.
func TestPaneShowsIdlePromptMenuBelowEchoedTurnIsNotIdle(t *testing.T) {
	tests := []struct {
		name  string
		lines []string
	}{
		{"folder trust below echoed turn", append([]string{"› Commit the staged API change", ""}, codexTrustMenuLines...)},
		{"model switch below echoed turn", []string{
			"› fix the flaky test",
			"",
			"  Approaching rate limits",
			"  Switch to gpt-5.4-mini for lower credit usage?",
			"› 1. Switch to gpt-5.4-mini",
			"  2. Keep current model",
			"  3. Keep current model (never show again)",
			"  Press enter to confirm or esc to go back",
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if PaneShowsIdlePrompt(tt.lines, "› ") {
				t.Errorf("PaneShowsIdlePrompt(%q, %q) = true, want false: the bottom-most prompt line is a menu cursor row", tt.lines, "› ")
			}
		})
	}
}

// Both prompt scans, idle (PaneShowsIdlePrompt) and start readiness
// (WaitForRuntimeReady), read a numbered menu's cursor row as a pending choice
// and a composer as ready. A cursor row is a "N. " label whose nearest
// non-blank neighbor is option N-1 or N+1; only the bottom-most prompt line
// decides.
// Kills: a scan that treats every "N. " prompt line as a menu (a composer
// draft starting "1. " stops reading as ready), or one that lets a stale menu
// or a numbered echoed turn above the composer decide.
func TestMenuCursorRowIsNotAReadyPrompt(t *testing.T) {
	tests := []struct {
		name      string
		lines     []string
		prefix    string
		wantReady bool
	}{
		{"bare composer", []string{"› "}, "› ", true},
		{"composer placeholder above status", []string{"› Ask Codex to do anything", "  GPT-6.1-Sol default · ~/city", "  ? for shortcuts"}, "› ", true},
		{"draft starting with a number", []string{"› 1. fix the flake"}, "› ", true},
		{"draft whose neighbor is not an adjacent option", []string{"› 1. foo", "  3. baz"}, "› ", true},
		{"numbered transcript list above composer", []string{"  1. first", "  2. second", "", "› "}, "› ", true},
		{"numbered echoed turn above composer", []string{"› 1. first", "  2. second", "", "› "}, "› ", true},
		{"stale menu frame above composer", append(append([]string{}, codexTrustMenuLines...), "", "› "), "› ", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := PaneShowsIdlePrompt(tt.lines, tt.prefix); got != tt.wantReady {
				t.Errorf("PaneShowsIdlePrompt(%q, %q) = %v, want %v", tt.lines, tt.prefix, got, tt.wantReady)
			}
			tm := scriptedTmux(scriptedCapture{out: strings.Join(tt.lines, "\n")})
			rc := &RuntimeConfig{Tmux: &RuntimeTmuxConfig{ReadyPromptPrefix: tt.prefix}}
			err := tm.WaitForRuntimeReady(context.Background(), "w", rc, 300*time.Millisecond)
			if (err == nil) != tt.wantReady {
				t.Errorf("WaitForRuntimeReady(%q, %q) = %v, want ready %v", tt.lines, tt.prefix, err, tt.wantReady)
			}
		})
	}
}
