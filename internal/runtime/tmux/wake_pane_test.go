package tmux

import (
	"errors"
	"slices"
	"testing"
)

// WakePane resizes the session's marked agent pane, fenced to its session and
// window, by one row and back to the height it read: never a bare session
// target (prefix-matched, and the window's active pane, maybe a human's
// split), and never an absolute -y of -1 or +1.
func TestWakePaneResizesTheFencedAgentPane(t *testing.T) {
	e := &markedAgentPaneExecutor{}
	tm := NewTmux()
	tm.exec = e
	tm.WakePane("worker-1")

	var got [][]string
	for _, call := range e.calls {
		if tmuxSubcommand(call) == "resize-pane" {
			i := slices.Index(call, "resize-pane")
			got = append(got, call[i+1:])
		}
	}
	want := [][]string{
		{"-t", "=worker-1:@2.%5", "-y", "19"},
		{"-t", "=worker-1:@2.%5", "-y", "20"},
	}
	if !slices.EqualFunc(got, want, slices.Equal[[]string]) {
		t.Errorf("resize-pane calls = %q, want %q (the agent pane, shrunk one row and restored)", got, want)
	}
}

// A pane WakePane cannot name is not resized: a failed or ambiguous agent
// pane lookup, and a pane too short to shrink, resize nothing.
func TestWakePaneFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name string
		exec executor
	}{
		{"lookup fails", &markedAgentPaneExecutor{lookErr: errors.New("can't find session: worker-1")}},
		{"two agent panes", &scriptedTargetExecutor{listPanes: func(string) string { return "agent\t@1.%1 20\nagent\t@2.%2 20" }}},
		{"no agent pane", &scriptedTargetExecutor{listPanes: func(string) string { return "orphan\t@1.%1 20" }}},
		{"one row tall", &scriptedTargetExecutor{listPanes: func(string) string { return "agent\t@1.%1 1" }}},
		{"unreadable height", &scriptedTargetExecutor{listPanes: func(string) string { return "agent\t@1.%1 " }}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tm := NewTmux()
			tm.exec = tc.exec
			tm.WakePane("worker-1")
			var calls [][]string
			switch e := tc.exec.(type) {
			case *markedAgentPaneExecutor:
				calls = e.calls
			case *scriptedTargetExecutor:
				calls = e.calls
			}
			if targets := targetsOf(calls, "resize-pane"); len(targets) != 0 {
				t.Errorf("resize-pane ran on %q, want no resize of a pane it could not name", targets)
			}
		})
	}
}

// A nudge wakes the agent pane it already resolved: the wake reads that
// fenced pane's height and resizes it, with no second agent-pane lookup.
func TestWakePaneIfDetachedWakesTheResolvedPane(t *testing.T) {
	e := &scriptedTargetExecutor{display: "@2.%5 20"}
	tm := NewTmux()
	tm.exec = e
	tm.WakePaneIfDetached("worker-1", "=worker-1:@2.%5")
	if got := targetsOf(e.calls, "list-panes"); len(got) != 0 {
		t.Errorf("list-panes ran on %q, want no second agent-pane lookup", got)
	}
	want := []string{"=worker-1:@2.%5", "=worker-1:@2.%5"}
	if got := targetsOf(e.calls, "resize-pane"); !slices.Equal(got, want) {
		t.Errorf("resize-pane targets = %q, want %q", got, want)
	}
}
