package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// TestOnFormulaReassignReopensOrderClaimedBead is the end-to-end regression for
// gastownhall/gascity#3231, mirroring the exact failing command from the
// issue: `gc sling <pool> <bead> --on mol-polecat-work --no-convoy --reassign`.
//
// An order claims the bead first (status=in_progress, assignee=order:<name>);
// without the reopen, --reassign clears the assignee but leaves the status
// in_progress, so the routed bead never becomes a Ready candidate and no pool
// worker can claim it. After the fix the source bead is routed to the pool AND
// open + unassigned, i.e. claimable.
func TestOnFormulaReassignReopensOrderClaimedBead(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{Name: "polecat", MaxActiveSessions: intPtr(2)}

	deps, stdout, stderr := testDeps(cfg, sp, runner.run)
	deps.Store = beads.NewMemStoreFrom(1, []beads.Bead{
		{ID: "BL-42", Title: "hotspot work", Type: "task", Status: "in_progress", Assignee: "order:mol-dog-jsonl"},
	}, nil)

	opts := testOpts(a, "BL-42")
	opts.OnFormula = "mol-polecat-work"
	opts.NoConvoy = true
	opts.Reassign = true

	code := doSling(opts, deps, deps.Store, stdout, stderr)
	if code != 0 {
		t.Fatalf("doSling returned %d, want 0; stderr: %s", code, stderr.String())
	}

	source, err := deps.Store.Get("BL-42")
	if err != nil {
		t.Fatalf("store.Get(BL-42): %v", err)
	}
	if got := source.Metadata["gc.routed_to"]; got != "polecat" {
		t.Errorf("gc.routed_to = %q, want polecat", got)
	}
	if source.Assignee != "" {
		t.Errorf("Assignee = %q, want empty after --reassign (order actor must not retain pool work)", source.Assignee)
	}
	if source.Status != "open" {
		t.Errorf("Status = %q, want open after --reassign so the pool can claim it (#3231)", source.Status)
	}
}
