package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/doctor"
)

func TestHoldLabelConventionsCheckCleanState(t *testing.T) {
	store := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "H-1", Title: "mayor hold", Type: "task", Status: "open", Labels: []string{"hold:mayor"}},
		{ID: "H-2", Title: "external hold", Type: "task", Status: "open", Labels: []string{"hold:external"}},
		{ID: "H-3", Title: "no hold labels at all", Type: "task", Status: "open"},
	}, nil)

	check := newHoldLabelConventionsCheck("/city", "city", func(string) (beads.Store, error) { return store, nil })
	res := check.Run(&doctor.CheckContext{})

	if res.Status != doctor.StatusOK {
		t.Fatalf("Status = %v, want OK: %#v", res.Status, res)
	}
	if res.Severity != doctor.SeverityAdvisory {
		t.Fatalf("Severity = %v, want Advisory", res.Severity)
	}
	if len(res.Details) != 0 {
		t.Errorf("Details = %v, want empty", res.Details)
	}
}

func TestHoldLabelConventionsCheckFlagsRetiredLabels(t *testing.T) {
	store := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "R-1", Title: "old style block", Type: "task", Status: "open", Labels: []string{"blocked"}},
		{ID: "R-2", Title: "arch blocker", Type: "task", Status: "open", Labels: []string{"arch-hold"}},
		{ID: "H-1", Title: "fine", Type: "task", Status: "open", Labels: []string{"hold:mayor"}},
	}, nil)

	check := newHoldLabelConventionsCheck("/city", "city", func(string) (beads.Store, error) { return store, nil })
	res := check.Run(&doctor.CheckContext{})

	if res.Status != doctor.StatusError {
		t.Fatalf("Status = %v, want Error: %#v", res.Status, res)
	}
	if res.Severity != doctor.SeverityAdvisory {
		t.Fatalf("Severity = %v, want Advisory", res.Severity)
	}
	details := strings.Join(res.Details, "\n")
	for _, want := range []string{"R-1", "blocked", "R-2", "arch-hold"} {
		if !strings.Contains(details, want) {
			t.Errorf("Details missing %q:\n%s", want, details)
		}
	}
	if strings.Contains(details, "H-1") {
		t.Errorf("Details should not flag hold:mayor bead H-1:\n%s", details)
	}
	if res.FixHint == "" {
		t.Error("FixHint should be set when retired labels are found")
	}
	if strings.Contains(res.FixHint, "hold-label-conventions.md") {
		t.Errorf("FixHint should not reference the not-yet-merged doc file: %q", res.FixHint)
	}
}

func TestHoldLabelConventionsCheckOutOfScopeLabelsExactMatchOnly(t *testing.T) {
	store := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "O-1", Title: "build blocker", Type: "task", Status: "open", Labels: []string{"build-blocker"}},
		{ID: "O-2", Title: "pre push blocker", Type: "task", Status: "open", Labels: []string{"pre-push-blocker"}},
		{ID: "O-3", Title: "ci blocker", Type: "task", Status: "open", Labels: []string{"ci-blocker"}},
		{ID: "O-4", Title: "test blocker", Type: "task", Status: "open", Labels: []string{"test-blocker"}},
		{ID: "O-5", Title: "push blocking", Type: "task", Status: "open", Labels: []string{"push-blocking"}},
		{ID: "O-6", Title: "needs mayor", Type: "task", Status: "open", Labels: []string{"needs-mayor"}},
		{ID: "O-7", Title: "needs mayor decision", Type: "task", Status: "open", Labels: []string{"needs-mayor-decision"}},
		{ID: "O-8", Title: "mpr human hold", Type: "task", Status: "open", Labels: []string{"mpr-human-hold"}},
	}, nil)

	check := newHoldLabelConventionsCheck("/city", "city", func(string) (beads.Store, error) { return store, nil })
	res := check.Run(&doctor.CheckContext{})

	if res.Status != doctor.StatusOK {
		t.Fatalf("Status = %v, want OK (out-of-scope labels must never false-positive): %#v", res.Status, res)
	}
	if len(res.Details) != 0 {
		t.Errorf("Details = %v, want empty", res.Details)
	}
}

func TestHoldLabelConventionsCheckMixedLabelsFlagsOnlyRetired(t *testing.T) {
	store := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "M-1", Title: "mixed labels", Type: "task", Status: "open", Labels: []string{"blocked", "build-blocker"}},
	}, nil)

	check := newHoldLabelConventionsCheck("/city", "city", func(string) (beads.Store, error) { return store, nil })
	res := check.Run(&doctor.CheckContext{})

	if res.Status != doctor.StatusError {
		t.Fatalf("Status = %v, want Error: %#v", res.Status, res)
	}
	if len(res.Details) != 1 {
		t.Fatalf("Details = %v, want exactly 1 entry (only the retired label)", res.Details)
	}
	if !strings.Contains(res.Details[0], "blocked") || strings.Contains(res.Details[0], "build-blocker") {
		t.Errorf("Details[0] = %q, want to name retired label 'blocked' only, not out-of-scope 'build-blocker'", res.Details[0])
	}
}

func TestHoldLabelConventionsCheckIgnoresClosedBeads(t *testing.T) {
	store := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "C-1", Title: "closed with retired label", Type: "task", Status: "closed", Labels: []string{"human-hold"}},
	}, nil)

	check := newHoldLabelConventionsCheck("/city", "city", func(string) (beads.Store, error) { return store, nil })
	res := check.Run(&doctor.CheckContext{})

	if res.Status != doctor.StatusOK {
		t.Fatalf("Status = %v, want OK (closed beads must not be flagged): %#v", res.Status, res)
	}
	if len(res.Details) != 0 {
		t.Errorf("Details = %v, want empty", res.Details)
	}
}

func TestHoldLabelConventionsCheckStoreErrorIsGraceful(t *testing.T) {
	check := newHoldLabelConventionsCheck("/city", "city", func(string) (beads.Store, error) {
		return nil, fmt.Errorf("store unreachable")
	})
	res := check.Run(&doctor.CheckContext{})

	if res.Status != doctor.StatusWarning {
		t.Fatalf("Status = %v, want Warning on store error: %#v", res.Status, res)
	}
	if res.Severity != doctor.SeverityAdvisory {
		t.Fatalf("Severity = %v, want Advisory", res.Severity)
	}
	if check.CanFix() {
		t.Errorf("CanFix = true, want false (no single mechanical fix applies)")
	}
}
