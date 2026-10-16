//go:build integration

package tmux

import (
	"context"
	"strings"
	"testing"
)

// Regression (#7871), on real tmux: an unmarked session listed LAST (tmux
// lists sessions by name) ends the executor-trimmed list-panes output, so its
// empty marker field loses its tab. Before and after #7893's marker clear
// (RemoveMeta unsets the option mirror), the batched inventory must still read
// every session, with only the marked one marked. The server is private to
// this test (its own -L socket under the package's private TMUX_TMPDIR) and
// only it is torn down.
// Kills: an inventory parse that needs the trailing empty marker's tab.
func TestTmuxRuntimeInventory_RealTmuxEmptyTrailingMarker(t *testing.T) {
	if !hasTmux() {
		t.Skip("tmux not installed")
	}
	cfg := DefaultConfig()
	cfg.SocketName = privateSocketName("inv")
	p := NewProviderWithConfig(cfg)
	t.Cleanup(func() { _ = p.tm.KillServer() })
	for _, name := range []string{"inv-a-asked", "inv-z-quiet"} {
		if err := p.tm.NewSessionWithCommand(name, t.TempDir(), "sleep 300"); err != nil {
			t.Fatalf("new session %s: %v", name, err)
		}
	}
	if err := p.SetMeta("inv-a-asked", "GC_RESTART_REQUESTED", "1700000050"); err != nil {
		t.Fatalf("SetMeta: %v", err)
	}

	// The hazard is real: the executor's output ends without the last
	// line's marker tab.
	raw, err := p.tm.runCtx(context.Background(), "list-panes", "-a", "-F", runtimeInventoryFormat)
	if err != nil {
		t.Fatalf("list-panes: %v", err)
	}
	lines := strings.Split(raw, "\n")
	if last := lines[len(lines)-1]; !strings.HasPrefix(last, "inv-z-quiet\t") || strings.Count(last, "\t") != 7 {
		t.Fatalf("last inventory line %q, want inv-z-quiet with its empty marker's tab trimmed", last)
	}

	check := func(when string, want map[string]bool) {
		t.Helper()
		got, err := p.RuntimeInventory(context.Background())
		if err != nil {
			t.Fatalf("%s: RuntimeInventory: %v", when, err)
		}
		if len(got) != len(want) {
			t.Fatalf("%s: RuntimeInventory = %#v, want sessions %v", when, got, want)
		}
		for name, asked := range want {
			e, ok := got[name]
			if !ok {
				t.Fatalf("%s: session %q missing from %#v", when, name, got)
			}
			if e.RestartRequested != asked || !e.DeadKnown || e.AllPanesDead || e.Incarnation == "" {
				t.Errorf("%s: %s = %#v, want a live session with RestartRequested=%t", when, name, e, asked)
			}
		}
	}
	check("one marked", map[string]bool{"inv-a-asked": true, "inv-z-quiet": false})

	if err := p.RemoveMeta("inv-a-asked", "GC_RESTART_REQUESTED"); err != nil {
		t.Fatalf("RemoveMeta: %v", err)
	}
	check("marker cleared", map[string]bool{"inv-a-asked": false, "inv-z-quiet": false})
}
