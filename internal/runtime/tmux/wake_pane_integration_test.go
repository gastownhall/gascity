//go:build integration

package tmux

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

// wakeFixture is a live session whose agent pane records the SIGWINCH it
// gets (winch). tmux applies a pane's new size to its process on a resize
// timer, after resize-pane returns, so the signal is waited for; the pane
// heights, which resize-pane sets before it returns, are read directly.
type wakeFixture struct {
	p                    *Provider
	agentPane, splitPane string
	winch                string
}

// newWakeFixture starts live with the agent pane and a human's split of the
// agent's window, created without -d, so it is the window's active pane.
func newWakeFixture(t *testing.T, live string) *wakeFixture {
	t.Helper()
	cfg := DefaultConfig()
	cfg.SocketName = privateSocketName("wake")
	p := NewProviderWithConfig(cfg)
	socketPath := namedSocketPath(cfg.SocketName)
	t.Cleanup(func() {
		if err := p.TeardownServer(); err != nil && !errors.Is(err, ErrNoServer) {
			t.Errorf("tear down test tmux server: %v", err)
		}
		if err := os.Remove(socketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Errorf("remove test tmux socket: %v", err)
		}
	})
	dir := t.TempDir()
	f := &wakeFixture{p: p, winch: filepath.Join(dir, "winch")}
	ready := filepath.Join(dir, "ready")
	agentCmd := fmt.Sprintf(`sh -c 'trap "echo winch > %s" WINCH; echo up > %s; while :; do sleep 0.1; done'`, f.winch, ready)
	if err := p.Start(context.Background(), live, runtime.Config{Command: agentCmd, WorkDir: dir}); err != nil {
		t.Fatalf("Start(%s): %v", live, err)
	}
	waitForMarker(t, ready, "up")
	var err error
	if f.agentPane, err = agentPaneID(p.tm, live); err != nil {
		t.Fatalf("agent pane id: %v", err)
	}
	if f.splitPane, err = p.tm.run("split-window", "-P", "-F", "#{pane_id}", "-t", paneTarget(live), "sleep 300"); err != nil {
		t.Fatalf("split agent window: %v", err)
	}
	if active, err := p.tm.run("display-message", "-t", paneTarget(live), "-p", "#{pane_id}"); err != nil || strings.TrimSpace(active) != f.splitPane {
		t.Fatalf("active pane = (%q, %v), want the split %q (precondition)", strings.TrimSpace(active), err, f.splitPane)
	}
	// The split's own resize signalled the agent; start clean.
	waitForMarker(t, f.winch, "winch")
	f.clearWinch(t)
	return f
}

func (f *wakeFixture) clearWinch(t *testing.T) {
	t.Helper()
	if err := os.Remove(f.winch); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("clear the winch marker: %v", err)
	}
}

// waitWinched fails the test unless the agent gets a SIGWINCH.
func (f *wakeFixture) waitWinched(t *testing.T) {
	t.Helper()
	waitForMarker(t, f.winch, "winch")
}

// heights returns the pane heights of the agent's window.
func (f *wakeFixture) heights(t *testing.T) map[string]string {
	t.Helper()
	out, err := f.p.tm.run("list-panes", "-t", f.agentPane, "-F", "#{pane_id} #{pane_height}")
	if err != nil {
		t.Fatalf("list pane heights: %v", err)
	}
	h := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		id, height, _ := strings.Cut(line, " ")
		h[id] = height
	}
	return h
}

// WakePane resizes the agent pane, not a bare target's active pane: waking
// a session that is gone must not reach the prefix-matched live one
// (gc-test-wake-1 gone resolves gc-test-wake-10), and waking the live one
// must signal the agent and leave the focused split at its height. Before
// the fix, both wakes ran resize-pane -y +1 on the focused split, leaving it
// one row tall. (That the resize names the fenced agent pane is pinned in
// argv by TestWakePaneResizesTheFencedAgentPane.)
func TestWakePaneResizesAgentPaneNotFocusedSplitRealTmux(t *testing.T) {
	if !hasTmux() {
		t.Skip("tmux not installed")
	}
	const gone, live = "gc-test-wake-1", "gc-test-wake-10"
	f := newWakeFixture(t, live)
	if err := f.p.Start(context.Background(), gone, runtime.Config{Command: "sleep 300", WorkDir: t.TempDir()}); err != nil {
		t.Fatalf("Start(%s): %v", gone, err)
	}
	if err := f.p.tm.KillSession(gone); err != nil {
		t.Fatalf("KillSession(%s): %v", gone, err)
	}
	before := f.heights(t)
	if len(before) != 2 {
		t.Fatalf("agent window pane heights = %v, want the agent and the split", before)
	}

	f.p.tm.WakePaneIfDetached(gone, gone)
	if after := f.heights(t); after[f.agentPane] != before[f.agentPane] || after[f.splitPane] != before[f.splitPane] {
		t.Fatalf("waking the gone %s left %s's panes at %v, were %v", gone, live, after, before)
	}

	f.p.tm.WakePaneIfDetached(live, live)
	f.waitWinched(t)
	if after := f.heights(t); after[f.agentPane] != before[f.agentPane] || after[f.splitPane] != before[f.splitPane] {
		t.Errorf("pane heights after the wake = %v, want them restored to %v (the focused split keeps its height)", after, before)
	}
}

// The wake resizes the agent pane even when a human's window is the active
// one: a bare target names that window's pane, which fills its window and
// cannot resize, so the agent would get no SIGWINCH at all.
func TestWakePaneResizesAgentPaneNotActiveWindowRealTmux(t *testing.T) {
	if !hasTmux() {
		t.Skip("tmux not installed")
	}
	const live = "gc-test-wake"
	f := newWakeFixture(t, live)
	// The human's window: created without -d, so it becomes the active window.
	if _, err := f.p.tm.run("new-window", "-t", paneTarget(live), "sleep 300"); err != nil {
		t.Fatalf("create the human's window: %v", err)
	}
	before := f.heights(t)

	f.p.tm.WakePaneIfDetached(live, live)
	f.waitWinched(t) // a bare target resizes the active window's pane: no signal
	if after := f.heights(t); after[f.agentPane] != before[f.agentPane] || after[f.splitPane] != before[f.splitPane] {
		t.Errorf("agent window pane heights after the wake = %v, want them restored to %v", after, before)
	}
}
