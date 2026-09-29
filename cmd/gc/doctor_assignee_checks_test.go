package main

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
)

func TestAssigneeResolvesCheckWarnsWhenRosterIsEmpty(t *testing.T) {
	// A config that failed to expand loads with zero agents. Reporting every
	// assignee as unroutable off that roster would be worse than the bug.
	result := newAssigneeResolvesCheck(nil, "/nonexistent", nil).Run(nil)
	if result.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want warning", result.Status)
	}
	if !strings.Contains(result.Message, "cannot be checked") {
		t.Errorf("message does not say the check could not answer: %q", result.Message)
	}
}

// TestAssigneeResolvesCheckReportsUnroutableAssignee is the findings-path
// test M2 flagged as missing: before this, scanScope's finding-append logic
// could be deleted entirely and every test in the tree would still pass,
// because nothing exercised the case where a store actually holds an open
// bead with an unroutable owner. This makes that path go red without the fix.
func TestAssigneeResolvesCheckReportsUnroutableAssignee(t *testing.T) {
	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{Title: "orphaned", Status: "open", Assignee: "phantom-owner"}); err != nil {
		t.Fatalf("seeding store: %v", err)
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "gascity"},
		Agents:    []config.Agent{{Name: "polecat"}},
	}
	check := newAssigneeResolvesCheck(cfg, "/city", func(string) (beads.Store, error) { return store, nil })

	result := check.Run(nil)

	if result.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want warning", result.Status)
	}
	if !strings.Contains(result.Message, "1 open bead") {
		t.Errorf("message = %q, want it to report 1 unroutable bead", result.Message)
	}
	found := false
	for _, d := range result.Details {
		if strings.Contains(d, "phantom-owner") {
			found = true
		}
	}
	if !found {
		t.Errorf("details = %v, want an entry naming phantom-owner", result.Details)
	}
}

// TestAssigneeResolvesCheckScanScopeRecordsNilStoreConstructorAsSkipped is
// M2's second half: an unmeasured scope (no store constructor wired) must
// show up as skipped, not silently read as clean.
func TestAssigneeResolvesCheckScanScopeRecordsNilStoreConstructorAsSkipped(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{{Name: "polecat"}}}
	check := newAssigneeResolvesCheck(cfg, "/city", nil)

	result := check.Run(nil)

	if result.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want warning", result.Status)
	}
	if !strings.Contains(result.Message, "skipped") {
		t.Errorf("message = %q, want it to say scopes were skipped", result.Message)
	}
}

func TestIsOpenWorkStatus(t *testing.T) {
	for _, status := range []string{"open", "in_progress", "blocked", " open "} {
		if !isOpenWorkStatus(status) {
			t.Errorf("isOpenWorkStatus(%q) = false, want true", status)
		}
	}
	for _, status := range []string{"closed", "", "done"} {
		if isOpenWorkStatus(status) {
			t.Errorf("isOpenWorkStatus(%q) = true, want false", status)
		}
	}
}
