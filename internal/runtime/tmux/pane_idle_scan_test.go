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
// normalization, reads a transcript line ending in a gerund and "…" as
// Claude's spinner row, or reads a numbered menu's cursor row as the composer.
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
		{"menu below echoed turn", []string{"› Commit the staged API change", "", "› 1. Trust and continue", "  2. Quit"}, "› ", false},
		{"model-switch modal below echoed turn", []string{"› fix the flaky test", "", "Approaching rate limits", "› 1. Switch to gpt-5.4-mini", "  2. Keep current model"}, "› ", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := PaneShowsIdlePrompt(tt.lines, tt.prefix); got != tt.want {
				t.Errorf("PaneShowsIdlePrompt(%q, %q) = %v, want %v", tt.lines, tt.prefix, got, tt.want)
			}
		})
	}
}

// A numbered menu whose selected row carries the prompt glyph (codex's
// folder-trust dialog) is waiting for an answer, not an idle composer: a nudge
// typed now lands in the dialog, and codex's leading Escape quits it.
func TestPaneShowsIdlePromptMenuCursorRowIsNotIdle(t *testing.T) {
	lines := []string{
		"  Trust this folder? Codex can read, edit, and run files here.",
		"",
		"› 1. Trust and continue",
		"  2. Quit",
		"",
		"  enter continue · esc quit",
	}
	if PaneShowsIdlePrompt(lines, "› ") {
		t.Errorf("PaneShowsIdlePrompt(%q, %q) = true, want false: a numbered menu's cursor row is not a ready prompt", lines, "› ")
	}
}

// TestMenuCursorRowIsNotAReadyPrompt pins both pane scans — the idle scan and
// the ready wait — on numbered option menus whose selected row carries the
// prompt glyph: that row is a pending choice (keys typed into it are menu
// input; Enter picks the highlighted option), and the bottom-most prompt line
// deciding means the pane is not ready even below an echoed prompt-prefixed
// turn, whose echo is not the live composer. The composer rows guard the
// opposite error: a draft that merely starts with "1. " and a stale menu
// above a live composer still read ready.
func TestMenuCursorRowIsNotAReadyPrompt(t *testing.T) {
	tests := []struct {
		name      string
		lines     []string
		prefix    string
		wantReady bool
	}{
		// Menus: the cursor row is a pending choice, not a composer.
		{"codex folder trust, cursor on first option", []string{
			"  Trust this folder? Codex can read, edit, and run files here.",
			"",
			"› 1. Trust and continue",
			"  2. Quit",
			"",
			"  enter continue · esc quit",
		}, "› ", false},
		{"codex folder trust, cursor on last option", []string{
			"  Trust this folder? Codex can read, edit, and run files here.",
			"",
			"  1. Trust and continue",
			"› 2. Quit",
			"",
			"  enter continue · esc quit",
		}, "› ", false},
		{"codex hook review, three options", []string{
			"  Hooks need review",
			"",
			"› 1. Review hooks",
			"  2. Trust all and continue",
			"  3. Continue without trusting",
			"",
			"  enter confirm · esc skip",
		}, "› ", false},
		{"model-switch modal below an echoed turn", []string{
			"› fix the flaky test",
			"  Ran the failing test; it still fails",
			"",
			"Approaching rate limits",
			"› 1. Switch to gpt-5.4-mini",
			"  2. Keep current model",
			"  3. Pause the session",
			"  enter confirm · esc back",
		}, "› ", false},
		{"folder trust below an echoed turn and a blank line", []string{
			"› Commit the staged API change",
			"",
			"› 1. Trust and continue",
			"  2. Quit",
		}, "› ", false},
		{"session-limit chooser, NBSP after the glyph", []string{
			"You've hit your session limit · resets 8:40am",
			"❯\u00a01. Stop and wait for limit to reset",
			"  2. Upgrade",
		}, "❯ ", false},
		{"boxed menu", []string{
			"│ › 1. Trust and continue │",
			"│   2. Quit                │",
		}, "› ", false},
		{"two-digit options, cursor on 10", []string{
			"   9. Reuse the last plan",
			"› 10. Draft a new plan",
		}, "› ", false},
		// Known false negative, pinned (PR #7670 open question): a
		// two-line numbered draft typed into the composer, with no other
		// prompt line, reads as a menu.
		{"numbered composer draft above its own next line", []string{
			"› 1. foo",
			"  2. bar",
		}, "› ", false},

		// Composers: the bottom-most prompt line is live.
		{"bare prompt", []string{"› "}, "› ", true},
		{"placeholder prompt above status line", []string{
			"› Ask Codex to do anything",
			"",
			"  GPT-6.1-Sol default · ~/city",
		}, "› ", true},
		{"numbered draft alone", []string{"› 1. fix the flake"}, "› ", true},
		{"numbered draft with a non-adjacent option below", []string{
			"› 1. foo",
			"  3. baz",
		}, "› ", true},
		{"numbered transcript list above a bare prompt", []string{
			"  1. first",
			"  2. second",
			"› ",
		}, "› ", true},
		{"echoed numbered turn above a live composer", []string{
			"› 1. first",
			"  2. second",
			"",
			"› ",
		}, "› ", true},
		{"stale menu frame above a live composer", []string{
			"› 1. Trust and continue",
			"  2. Quit",
			"",
			"› ",
		}, "› ", true},
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
