//go:build windows

package sling

import (
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// unfitSlingBeadIDs are bead IDs a sling_query must never receive on Windows:
// each holds a character that cmd.exe, or PowerShell, would read as more than
// a literal word.
var unfitSlingBeadIDs = []string{
	"BL-1&calc", "BL-%PATH%", "BL-1|x", "BL-1>x", "BL-1<x", "BL^1", "BL-!x!",
	"BL 1", "BL-1\"x", "BL-1\nx", "BL-1(x)", "BL-1,x", "BL-1;x", "BL-1'x",
	"BL-1\u2019x", "BL-1`x", "BL-$x", "BL-1/x", "BL-1:x", "BL-\u00e9",
}

// TestSlingQueryRefusesABeadIDCmdExeWouldRead pins that on Windows a bead ID is
// substituted into a sling_query only when it holds nothing but ASCII letters,
// digits, '.', '_' and '-'. A forced sling skips the store lookup, so the ID is
// caller text, and when the operator's sling program is a batch file cmd.exe
// runs its & and expands its %NAME% whatever the quoting.
func TestSlingQueryRefusesABeadIDCmdExeWouldRead(t *testing.T) {
	a := config.Agent{Name: "worker", MaxActiveSessions: intPtr(1), SlingQuery: "custom-dispatch {}"}
	cfg := &config.City{Workspace: config.Workspace{Name: "test"}, Agents: []config.Agent{a}}
	for _, id := range unfitSlingBeadIDs {
		runner := newFakeRunner()
		deps := testDeps(cfg, runtime.NewFake(), runner.run)
		_, err := DoSling(SlingOpts{Target: a, BeadOrFormula: id, Force: true}, deps, nil)
		if err == nil || !strings.Contains(err.Error(), "bead ID") {
			t.Errorf("DoSling(%q) = %v, want the bead ID refused", id, err)
		}
		if len(runner.calls) != 0 {
			t.Errorf("DoSling(%q) ran %q", id, runner.calls)
		}
	}
	for _, id := range []string{"BL-42", "ab-12cd.14", "gc-wisp-y7jhci", "ga_x9k9b9.1.1", "HQ-2"} {
		runner := newFakeRunner()
		deps := testDeps(cfg, runtime.NewFake(), runner.run)
		if _, err := DoSling(SlingOpts{Target: a, BeadOrFormula: id, Force: true}, deps, nil); err != nil {
			t.Errorf("DoSling(%q) = %v, want it routed", id, err)
		}
		if want := "custom-dispatch " + runtime.ShellQuote(id); len(runner.calls) != 1 || runner.calls[0] != want {
			t.Errorf("DoSling(%q) ran %q, want %q", id, runner.calls, want)
		}
	}
}

// TestSlingBatchRefusesAChildIDCmdExeWouldRead pins the same rule for the
// children of a convoy, whose IDs come from the store rather than the caller.
func TestSlingBatchRefusesAChildIDCmdExeWouldRead(t *testing.T) {
	a := config.Agent{Name: "worker", MaxActiveSessions: intPtr(1), SlingQuery: "custom-dispatch {}"}
	cfg := &config.City{Workspace: config.Workspace{Name: "test"}, Agents: []config.Agent{a}}
	runner := newFakeRunner()
	deps := testDeps(cfg, runtime.NewFake(), runner.run)
	deps.Store = beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "BL-0", Title: "convoy", Type: "convoy", Status: "open", Metadata: map[string]string{}},
		{ID: "BL-1", Title: "clean", Type: "task", Status: "open", ParentID: "BL-0", Metadata: map[string]string{}},
		{ID: "BL-2&calc", Title: "child", Type: "task", Status: "open", ParentID: "BL-0", Metadata: map[string]string{}},
	}, nil)

	result, _ := DoSlingBatch(SlingOpts{Target: a, BeadOrFormula: "BL-0"}, deps, deps.Store)
	if want := "custom-dispatch " + runtime.ShellQuote("BL-1"); !slices.Equal(runner.calls, []string{want}) {
		t.Errorf("the batch ran %q, want only %q", runner.calls, want)
	}
	for _, child := range result.Children {
		if child.BeadID == "BL-2&calc" && (!child.Failed || !strings.Contains(child.FailReason, "bead ID")) {
			t.Errorf("child %s = %+v, want it failed for its bead ID", child.BeadID, child)
		}
	}
}
