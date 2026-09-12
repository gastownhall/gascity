package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
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

// TestReassignClearsPoolStartFailureRecordAndPark pins the designed unpark:
// gc sling --reassign clears the pool's failed-start record and park
// (beadmeta.WorkStartFailureMetadataKeys) before routing, so the re-dispatched
// bead is demand again with a clean count — the park it carried was the
// reason the operator is re-dispatching it.
func TestReassignClearsPoolStartFailureRecordAndPark(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{Name: "polecat", MaxActiveSessions: intPtr(2)}

	deps, stdout, stderr := testDeps(cfg, sp, runner.run)
	deps.Store = beads.NewMemStoreFrom(1, []beads.Bead{
		{ID: "BL-77", Title: "parked work", Type: "task", Status: "in_progress", Assignee: "polecat", Metadata: map[string]string{
			"gc.routed_to":           "polecat",
			"gc.parked_at":           "2026-09-11T02:50:00Z",
			"gc.park_reason":         "pre_start[0]: exit status 1",
			"gc.park_failures":       "5",
			"gc.park_mailed_at":      "2026-09-11T02:50:01Z",
			"gc.start_failed_at":     "2026-09-11T02:50:00Z",
			"gc.start_failure":       "pre_start[0]: exit status 1",
			"gc.start_backoff_until": "",
		}},
	}, nil)

	opts := testOpts(a, "BL-77")
	opts.NoConvoy = true
	opts.NoFormula = true
	opts.Reassign = true

	code := doSling(opts, deps, deps.Store, stdout, stderr)
	if code != 0 {
		t.Fatalf("doSling returned %d, want 0; stderr: %s", code, stderr.String())
	}
	got, err := deps.Store.Get("BL-77")
	if err != nil {
		t.Fatalf("store.Get(BL-77): %v", err)
	}
	for _, key := range beadmeta.WorkStartFailureMetadataKeys {
		if v := got.Metadata[key]; v != "" {
			t.Errorf("%s = %q after --reassign, want cleared (stdout: %s; stderr: %s)", key, v, stdout.String(), stderr.String())
		}
	}
	if got.Status != "open" || got.Assignee != "" {
		t.Errorf("status=%q assignee=%q after --reassign, want open and unassigned", got.Status, got.Assignee)
	}
	if got.Metadata["gc.routed_to"] != "polecat" {
		t.Errorf("gc.routed_to = %q, want polecat", got.Metadata["gc.routed_to"])
	}
}
