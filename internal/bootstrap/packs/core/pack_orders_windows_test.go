//go:build windows

package core

import (
	"io/fs"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

// TestCoreOrdersRefusedOnWindowsAreTheScriptOrders pins which of the core
// pack's orders Windows refuses to run: the twelve whose program is a POSIX
// shell script, until they are ported, and none of the three that run gc.
func TestCoreOrdersRefusedOnWindowsAreTheScriptOrders(t *testing.T) {
	want := []string{
		"cascade-nudge-on-blocker-close", "cross-rig-deps", "gate-sweep", "jsonl-export",
		"notify-on-human-gate-creation", "nudge-on-route", "orphan-sweep", "prune-branches",
		"reaper", "renudge-stale-human-gates", "spawn-storm-detect", "wisp-compact",
	}
	entries, err := fs.ReadDir(PackFS, "orders")
	if err != nil {
		t.Fatalf("reading orders dir: %v", err)
	}
	var refused, runnable []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".toml") {
			continue
		}
		o := readOrder(t, e.Name())
		if runtime.ShellCommandUnavailable(o.Exec) != "" {
			refused = append(refused, o.Name)
		} else {
			runnable = append(runnable, o.Name)
		}
	}
	if !slices.Equal(refused, want) {
		t.Errorf("Windows refuses %q; want the script orders %q", refused, want)
	}
	if runnable := strings.Join(runnable, " "); runnable != "beads-health nudge-mail-sweep order-tracking-sweep" {
		t.Errorf("Windows runs %q; want the three gc orders", runnable)
	}
}
