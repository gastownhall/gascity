package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// The reconciler report is the third consumer of the roster, alongside the
// `gc bd` write gate and the `assignee-resolves` doctor check. It is the only
// one that sees every bead on every reconcile, so it is the site that must not
// go quiet.
func TestReportUnroutableAssigneesNamesOnlyUnfinishedUnroutableWork(t *testing.T) {
	work := []beads.Bead{
		{ID: "dr-phantom-open", Status: "open", Assignee: "goal-5-temporal"},
		{ID: "dr-phantom-blocked", Status: "blocked", Assignee: "controller"},
		{ID: "dr-phantom-closed", Status: "closed", Assignee: "goal-5-temporal"},
		{ID: "dr-routable", Status: "open", Assignee: "polecat-4"},
		{ID: "dr-unowned", Status: "open", Assignee: ""},
	}

	var stderr bytes.Buffer
	reportUnroutableAssignees(&stderr, testRosterCity(), work)
	got := stderr.String()

	for _, want := range []string{"dr-phantom-open", "dr-phantom-blocked"} {
		if !strings.Contains(got, want) {
			t.Errorf("report omitted %q; got %q", want, got)
		}
	}
	// A blocked bead assigned to a phantom is the same finding as an open one.
	// The predicate here must not drift from the one the doctor check uses.
	for _, unwanted := range []string{"dr-phantom-closed", "dr-routable", "dr-unowned"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("report named %q, which is not a finding; got %q", unwanted, got)
		}
	}
	if !strings.Contains(got, "2 unfinished bead(s)") {
		t.Errorf("report miscounted; got %q", got)
	}
}

// Negative rail: a roster that knows nothing must stay silent rather than
// declare the whole fleet unroutable.
func TestReportUnroutableAssigneesSilentOnEmptyRoster(t *testing.T) {
	var stderr bytes.Buffer
	reportUnroutableAssignees(&stderr, nil, []beads.Bead{
		{ID: "dr-phantom-open", Status: "open", Assignee: "goal-5-temporal"},
	})
	if stderr.Len() != 0 {
		t.Errorf("empty roster reported findings: %q", stderr.String())
	}
}
