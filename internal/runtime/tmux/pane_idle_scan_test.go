package tmux

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// PaneShowsIdlePrompt is the scan both local tmux (snapshotPaneIdleWithPrefix)
// and remote tmux-in-box runtimes apply to a captured pane, so a busy
// indicator must win over a visible prompt, the prompt may sit above a status
// footer, and NBSP or a boxed prompt must still match. The bottom-most prompt
// line decides; a numbered menu's cursor row there is not idle (see
// TestMenuCursorRowIsNotAReadyPrompt).
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

// codexFolderTrustMenu is codex-cli 0.157's folder-trust dialog: the selected
// option carries codex's prompt glyph, the other options do not.
var codexFolderTrustMenu = []string{
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
	lines := codexFolderTrustMenu
	if PaneShowsIdlePrompt(lines, "› ") {
		t.Errorf("PaneShowsIdlePrompt(%q, %q) = true, want false: a numbered menu's cursor row is not a ready prompt", lines, "› ")
	}
	if idle, err := ClassifyIdlePane(lines, "› "); idle || !errors.Is(err, runtime.ErrIdleIndeterminate) {
		t.Errorf("ClassifyIdlePane(%q, %q) = (%v, %v), want (false, ErrIdleIndeterminate)", lines, "› ", idle, err)
	}
}

// Codex echoes past user turns with its prompt glyph, so a menu drawn under
// transcript is not the only prompt-prefixed line on screen. The bottom-most
// prompt line is the live one; an echoed turn above the menu is history.
// Kills: a scan that skips the menu row and keeps looking, which finds the
// echoed turn and reads the dialog as idle.
func TestPaneShowsIdlePromptMenuBelowEchoedTurnIsNotIdle(t *testing.T) {
	tests := []struct {
		name  string
		lines []string
	}{
		{"folder trust below echoed turn", append([]string{"› Commit the staged API change", ""}, codexFolderTrustMenu...)},
		{"model switch below echoed turn", []string{
			"› fix the flaky test",
			"",
			"  Approaching rate limits. Switch to gpt-5.4-mini for lower credit usage?",
			"",
			"› 1. Switch to gpt-5.4-mini",
			"  2. Keep current model",
			"  3. Keep current model (never show again)",
			"",
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

// A menu cursor row is the selected option of a numbered menu: its label is
// "N. " and its nearest non-blank neighbor is option N-1 or N+1. Both
// ready-prompt scans, the idle boundary (PaneShowsIdlePrompt) and startup
// readiness (WaitForRuntimeReady), read such a row at the bottom-most prompt
// line as not ready, and every other bottom-most prompt line as a composer.
// Kills: a menu check without the neighbor test (a composer draft starting
// with "1. " reads as a menu), one that skips the menu row and keeps scanning
// (an echoed turn above the menu reads as ready), a neighbor parse without the
// NBSP / box-border normalization, and one that accepts only option N+1 (the
// cursor on the last option reads as ready).
func TestMenuCursorRowIsNotAReadyPrompt(t *testing.T) {
	tests := []struct {
		name      string
		lines     []string
		prefix    string
		wantReady bool
	}{
		{"folder trust, cursor on first option", codexFolderTrustMenu, "› ", false},
		{"folder trust, cursor on last option", []string{"  Trust this folder?", "", "  1. Trust and continue", "› 2. Quit", "", "  enter continue · esc quit"}, "› ", false},
		{"hook review", []string{
			"  Hooks need review",
			"  4 hooks are new or changed.",
			"› 1. Review hooks",
			"  2. Trust all and continue",
			"  3. Continue without trusting (hooks won't run)",
			"  enter confirm · esc skip",
		}, "› ", false},
		{"model switch below echoed turn", []string{"› fix the flaky test", "", "  Switch to gpt-5.4-mini?", "", "› 1. Switch to gpt-5.4-mini", "  2. Keep current model", "  3. Keep current model (never show again)"}, "› ", false},
		{"folder trust below echoed turn", append([]string{"› Commit the staged API change", ""}, codexFolderTrustMenu...), "› ", false},
		{"claude chooser with nbsp", []string{"  You've hit your session limit", "", "❯\u00a01. Stop and wait for limit to reset", "  2. Upgrade"}, DefaultReadyPromptPrefix, false},
		{"boxed menu", []string{"╭──────────────────────────╮", "│ › 1. Trust and continue  │", "│   2. Quit                │", "╰──────────────────────────╯"}, "› ", false},
		{"two-digit options", []string{"   9. Ninth", "› 10. Tenth"}, "› ", false},
		// Known false negative: a two-line numbered draft is indistinguishable
		// from a menu.
		{"two-line numbered draft reads as a menu", []string{"› 1. foo", "  2. bar"}, "› ", false},
		{"bare composer", []string{"› "}, "› ", true},
		{"composer placeholder above status line", []string{"› Ask Codex to do anything", "", "  gpt-5.4 high · 100% left · ~/repo"}, "› ", true},
		{"draft starting with a number", []string{"› 1. fix the flake"}, "› ", true},
		{"draft whose neighbor is not the adjacent option", []string{"› 1. foo", "  3. baz"}, "› ", true},
		{"numbered transcript list above composer", []string{"  1. First", "  2. Second", "", "› "}, "› ", true},
		{"echoed numbered turn above composer", []string{"› 1. first", "  2. second", "", "› "}, "› ", true},
		{"stale menu frame above composer", append(append([]string{}, codexFolderTrustMenu...), "", "› "), "› ", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := PaneShowsIdlePrompt(tt.lines, tt.prefix); got != tt.wantReady {
				t.Errorf("PaneShowsIdlePrompt(%q, %q) = %v, want %v", tt.lines, tt.prefix, got, tt.wantReady)
			}
			tm := scriptedTmux(scriptedCapture{out: strings.Join(tt.lines, "\n")})
			err := tm.WaitForRuntimeReady(context.Background(), "w", &RuntimeConfig{
				Tmux: &RuntimeTmuxConfig{ReadyPromptPrefix: tt.prefix},
			}, 300*time.Millisecond)
			if gotReady := err == nil; gotReady != tt.wantReady {
				t.Errorf("WaitForRuntimeReady(%q, %q) = %v, want ready=%v", tt.lines, tt.prefix, err, tt.wantReady)
			}
		})
	}
}

// Startup readiness polls until its budget runs out while the pane keeps
// showing an unanswered menu, instead of reporting the menu's cursor row as the
// runtime's ready prompt.
func TestWaitForRuntimeReadyMenuTimesOut(t *testing.T) {
	tm := scriptedTmux(scriptedCapture{out: strings.Join(codexFolderTrustMenu, "\n")})
	err := tm.WaitForRuntimeReady(context.Background(), "w", &RuntimeConfig{
		Tmux: &RuntimeTmuxConfig{ReadyPromptPrefix: "› "},
	}, 500*time.Millisecond)
	if err == nil || err.Error() != "timeout waiting for runtime prompt" {
		t.Fatalf("WaitForRuntimeReady = %v, want the timeout waiting for runtime prompt error", err)
	}
}
