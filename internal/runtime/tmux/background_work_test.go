package tmux

import (
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

// TestClaudeHasBackgroundWorkReadsTheFooter: a Claude Code pane at its ready
// prompt after a turn that left shells running reports work from the footer's
// shell count, and none once the footer no longer counts any.
func TestClaudeHasBackgroundWorkReadsTheFooter(t *testing.T) {
	for _, tc := range []struct {
		name   string
		shells int
		want   bool
	}{
		{"one shell still running", 1, true},
		{"two shells still running", 2, true},
		{"no shell running", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pane := newFakeClaudeTaskPane(tc.shells)
			tm := newClaudeStopTmux(pane)
			got, err := tm.HasBackgroundWork("claude-bg-work")
			if err != nil {
				t.Fatalf("HasBackgroundWork: %v", err)
			}
			if got != tc.want {
				t.Fatalf("HasBackgroundWork = %t, want %t", got, tc.want)
			}
			if keys := pane.keys(); len(keys) != 0 {
				t.Fatalf("keys reaching the pane during a read = %q, want none", keys)
			}
		})
	}
}

// TestCodexHasBackgroundWorkReadsTheStatusArea: a Codex pane reports work
// while its status area counts running background terminals.
func TestCodexHasBackgroundWorkReadsTheStatusArea(t *testing.T) {
	for _, tc := range []struct {
		name       string
		background int
		want       bool
	}{
		{"one background terminal running", 1, true},
		{"no background terminal running", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pane := newFakeCodexPane(t)
			pane.background = tc.background
			tm := newCodexStopTmux(pane)
			got, err := tm.HasBackgroundWork("codex-bg-work")
			if err != nil {
				t.Fatalf("HasBackgroundWork: %v", err)
			}
			if got != tc.want {
				t.Fatalf("HasBackgroundWork = %t, want %t", got, tc.want)
			}
			if keys := pane.typedOrPressed(); len(keys) != 0 {
				t.Fatalf("keys reaching the pane during a read = %q, want none", keys)
			}
		})
	}
}

// TestOtherProvidersHasBackgroundWorkIsUnsupported: a session that runs neither
// Codex nor Claude has no footer to read the work from.
func TestOtherProvidersHasBackgroundWorkIsUnsupported(t *testing.T) {
	pane := newFakeCodexPane(t)
	pane.provider = "gemini"
	tm := newCodexStopTmux(pane)
	_, err := tm.HasBackgroundWork("gemini-bg-work")
	if !errors.Is(err, runtime.ErrInteractionUnsupported) {
		t.Fatalf("HasBackgroundWork(gemini) = %v, want ErrInteractionUnsupported", err)
	}
}

// The tmux Provider forwards the observer, so the reconciler's leaf lookup
// finds it on the provider it routes to.
var (
	_ runtime.BackgroundWorkObserver = (*Provider)(nil)
	_ runtime.BackgroundWorkObserver = (*Tmux)(nil)
)
