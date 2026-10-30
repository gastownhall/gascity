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
// Claude's spinner row, or reads a numbered menu's cursor row as the live
// composer.
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

// A numbered menu drawn with the runtime's own prompt glyph on its selected
// row is a pending choice, not a live composer — keys typed into it are menu
// input, and Enter picks the highlighted option. The bottom-most prompt line
// decides, so a menu below an echoed turn is still the menu and a composer
// below stale numbered text is still the composer; a composer draft that
// merely starts with "1. " has no adjacent option and still reads as a
// prompt. The pane twin of the dialog layer's isNumberedMenuRow rule.
func TestMenuCursorRowIsNotAReadyPrompt(t *testing.T) {
	tests := []struct {
		name   string
		lines  []string
		prefix string
		want   bool
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
			"  Trust the new hooks in .codex/hooks?",
			"",
			"› 1. Review and continue",
			"  2. Skip hooks for this session",
			"  3. Quit",
			"",
			"  enter confirm · esc skip",
		}, "› ", false},
		{"claude session-limit chooser with NBSP glyph", []string{
			"You've hit your session limit · resets 8:40am",
			"❯\u00a01. Stop and wait for limit to reset",
			"  2. Upgrade",
		}, DefaultReadyPromptPrefix, false},
		{"box-bordered menu", []string{
			"╭──────────────────────────────╮",
			"│ › 1. Trust and continue      │",
			"│   2. Quit                    │",
			"╰──────────────────────────────╯",
		}, "› ", false},
		{"two-digit option numbers", []string{
			"   9. Reboot now",
			"› 10. Reboot never",
		}, "› ", false},
		// Known false negative, pinned: a two-line numbered list typed into
		// the composer, with no other prompt line on screen, reads as a menu.
		{"composer draft directly above the next list row (known false negative)", []string{
			"› 1. foo",
			"  2. bar",
		}, "› ", false},

		// A menu below an echoed turn: the bottom-most prompt line is the
		// menu's cursor row, so the pane still reads not-ready — skipping the
		// cursor row and keeping the scan would land on the echoed turn.
		{"folder trust below an echoed turn", []string{
			"› Commit the staged API change",
			"",
			"  Trust this folder? Codex can read, edit, and run files here.",
			"",
			"› 1. Trust and continue",
			"  2. Quit",
			"",
			"  enter continue · esc quit",
		}, "› ", false},
		{"model-switch modal below an echoed turn", []string{
			"› fix the flaky test",
			"",
			"  Approaching rate limits",
			"  Switch to gpt-5.4-mini for lower credit usage?",
			"› 1. Switch to gpt-5.4-mini",
			"  2. Keep current model",
			"  3. Wait a minute",
			"  Press enter to confirm or esc to go back",
		}, "› ", false},

		// Composers: ready, including drafts that start with a number.
		{"bare composer", []string{"› "}, "› ", true},
		{"placeholder above status footer", []string{
			"› Ask Codex to do anything",
			"",
			"  ⏵⏵ bypass permissions on",
		}, "› ", true},
		{"numbered draft alone", []string{"› 1. fix the flake"}, "› ", true},
		{"numbered draft with a non-adjacent option", []string{
			"› 1. foo",
			"  3. baz",
		}, "› ", true},
		{"numbered transcript list above a bare composer", []string{
			"  1. Check the logs",
			"  2. Fix the import",
			"",
			"› ",
		}, "› ", true},
		{"echoed numbered turn above a bare composer", []string{
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
			if got := PaneShowsIdlePrompt(tt.lines, tt.prefix); got != tt.want {
				t.Errorf("PaneShowsIdlePrompt(%q, %q) = %v, want %v", tt.lines, tt.prefix, got, tt.want)
			}
		})
	}
}

// The ready wait applies the same composer rule as idle detection: a pane
// parked on a numbered menu never shows the composer, so the wait runs to its
// deadline and returns the timeout error (the startup nudge would otherwise
// be typed into the menu), while a composer — even below a stale menu frame
// or an echoed numbered turn — is ready at once.
func TestWaitForRuntimeReadyMenuCursorRowIsNotReady(t *testing.T) {
	tests := []struct {
		name   string
		lines  []string
		prefix string
		ready  bool
	}{
		{"folder-trust menu", []string{
			"  Trust this folder? Codex can read, edit, and run files here.",
			"",
			"› 1. Trust and continue",
			"  2. Quit",
			"",
			"  enter continue · esc quit",
		}, "› ", false},
		{"model-switch modal below an echoed turn", []string{
			"› fix the flaky test",
			"",
			"  Approaching rate limits",
			"  Switch to gpt-5.4-mini for lower credit usage?",
			"› 1. Switch to gpt-5.4-mini",
			"  2. Keep current model",
			"  Press enter to confirm or esc to go back",
		}, "› ", false},
		{"bare composer", []string{"› "}, "› ", true},
		{"numbered draft alone", []string{"› 1. fix the flake"}, "› ", true},
		{"echoed numbered turn above a bare composer", []string{
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
			tm := scriptedTmux(scriptedCapture{out: strings.Join(tt.lines, "\n")})
			err := tm.WaitForRuntimeReady(context.Background(), "w",
				&RuntimeConfig{Tmux: &RuntimeTmuxConfig{ReadyPromptPrefix: tt.prefix}},
				300*time.Millisecond)
			if got := err == nil; got != tt.ready {
				t.Fatalf("WaitForRuntimeReady err = %v, want ready=%v", err, tt.ready)
			}
			if !tt.ready && !strings.Contains(err.Error(), "timeout waiting for runtime prompt") {
				t.Fatalf("WaitForRuntimeReady err = %v, want the runtime-prompt timeout", err)
			}
		})
	}
}
