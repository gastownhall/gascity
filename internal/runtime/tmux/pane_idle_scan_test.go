package tmux

import "testing"

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

// An open Claude Code AskUserQuestion dialog renders its focused option as
// "❯ 1. …", which matches the ready-prompt prefix. It is not an idle boundary:
// a nudge pasted now and followed by Enter selects that option instead of
// reaching the composer (#8150).
func TestPaneShowsIdlePromptAskUserQuestionDialogIsNotIdle(t *testing.T) {
	lines := []string{
		"⏺ I have a few questions before I plan this.",
		"",
		"────────────────────────────────────────────────────────────",
		" ←  ☐ Scope  ☐ Approach  ☐ Timing  ✔ Submit  →",
		"",
		"Which scope should the change cover?",
		"",
		"❯ 1. Small",
		"     Only the bug fix",
		"  2. Medium",
		"     Fix plus regression tests",
		"  3. Large",
		"     Fix, tests and a refactor",
		"  4. Type something.",
		"────────────────────────────────────────────────────────────",
		"  5. Chat about this",
		"",
		"Enter to select · Tab/Arrow keys to navigate · Esc to cancel",
	}
	if PaneShowsIdlePrompt(lines, DefaultReadyPromptPrefix) {
		t.Errorf("PaneShowsIdlePrompt reported an open AskUserQuestion dialog as idle; a nudge would answer it")
	}
	if idle, err := ClassifyIdlePane(lines, DefaultReadyPromptPrefix); err == nil && idle {
		t.Errorf("ClassifyIdlePane(AskUserQuestion dialog) = (true, nil), want not idle")
	}
}

// A question page still owns the pane when its marks the older detector keyed
// on are gone: the answered-question page's chips read ☒ instead of ☐, focus
// in "Type something." draws no ❯ cursor, and a narrow pane wraps the select
// footer. None of those panes is an idle boundary — Enter on each answers the
// dialog (#8150).
func TestPaneShowsIdlePromptQuestionPagesWithoutCursorOrHeaderChips(t *testing.T) {
	// questionPaneFocusInTypeSomething is a reconstruction: the 2.1.304
	// multi-question page with focus in the free-text row, which draws no ❯.
	questionPaneFocusInTypeSomething := []string{
		" ←  ☐ Scope  ☐ Approach  ☐ Timing  ✔ Submit  →",
		"",
		"Which scope should the change cover?",
		"",
		"  1. Small",
		"     Only the bug fix",
		"  2. Medium",
		"     Fix plus regression tests",
		"  3. Large",
		"     Fix, tests and a refactor",
		"  4. Type something.",
		"     fix it fast",
		"────────────────────────────────────────────────────────────",
		"  5. Chat about this",
		"",
		"Enter to select · Tab/Arrow keys to navigate · Esc to cancel",
	}
	panes := map[string][]string{
		"focus in Type something, no cursor": questionPaneFocusInTypeSomething,
		"page after one Enter, ☒ chips":      readQuestionFixture(t, "question-after-enter-ga-ngrme8.txt"),
		"wrapped select footer":              readQuestionFixture(t, "question-multi-wrapped-footer-40x18.txt"),
	}
	for name, lines := range panes {
		t.Run(name, func(t *testing.T) {
			if PaneShowsIdlePrompt(lines, DefaultReadyPromptPrefix) {
				t.Errorf("PaneShowsIdlePrompt = true; a nudge's Enter would answer this dialog")
			}
			if idle, err := ClassifyIdlePane(lines, DefaultReadyPromptPrefix); err == nil && idle {
				t.Errorf("ClassifyIdlePane = (true, nil), want not idle")
			}
		})
	}
}
