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

func TestHoldLabelConventionsCheckBareHumanNotFlagged(t *testing.T) {
	store := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "H-1", Title: "operator card", Type: "task", Status: "open", Labels: []string{"human"}},
		{ID: "H-2", Title: "mayor hold", Type: "task", Status: "open", Labels: []string{"hold:mayor"}},
		{ID: "H-3", Title: "external hold", Type: "task", Status: "open", Labels: []string{"hold:external"}},
	}, nil)

	check := newHoldLabelConventionsCheck("/city", "city", func(string) (beads.Store, error) { return store, nil })
	res := check.Run(&doctor.CheckContext{})

	if res.Status != doctor.StatusOK {
		t.Fatalf("Status = %v, want OK (bare human is not a retired hold label): %#v", res.Status, res)
	}
	if len(res.Details) != 0 {
		t.Errorf("Details = %v, want empty", res.Details)
	}
}

func TestHoldLabelConventionsCheckFlagsHumanHold(t *testing.T) {
	store := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "R-1", Title: "legacy human hold", Type: "task", Status: "open", Labels: []string{"human-hold"}},
		{ID: "H-1", Title: "operator card", Type: "task", Status: "open", Labels: []string{"human"}},
		{ID: "H-2", Title: "mayor hold", Type: "task", Status: "open", Labels: []string{"hold:mayor"}},
		{ID: "H-3", Title: "external hold", Type: "task", Status: "open", Labels: []string{"hold:external"}},
	}, nil)

	check := newHoldLabelConventionsCheck("/city", "city", func(string) (beads.Store, error) { return store, nil })
	res := check.Run(&doctor.CheckContext{})

	if res.Status != doctor.StatusError {
		t.Fatalf("Status = %v, want Error: %#v", res.Status, res)
	}
	if len(res.Details) != 1 {
		t.Fatalf("Details = %v, want exactly 1 entry (human-hold only)", res.Details)
	}
	if !strings.Contains(res.Details[0], "human-hold") || !strings.Contains(res.Details[0], "R-1") {
		t.Errorf("Details[0] = %q, want retired label human-hold on R-1", res.Details[0])
	}
	if strings.Contains(res.Details[0], `"human"`) {
		t.Errorf("Details[0] = %q, must not flag bare human", res.Details[0])
	}
	if strings.Contains(res.FixHint, "and human migrate") || strings.Contains(res.FixHint, "human-hold, and human") {
		t.Errorf("FixHint must not prescribe migrating bare human: %q", res.FixHint)
	}
	if !strings.Contains(res.FixHint, "human-hold") {
		t.Errorf("FixHint should still mention human-hold: %q", res.FixHint)
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
		{ID: "O-9", Title: "bare human executor", Type: "task", Status: "open", Labels: []string{"human"}},
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

// TestHoldLabelConventionsCheckListsTheStoreOnce: the check reads the store
// once and matches the retired labels in memory, and a failed listing is an
// advisory warning naming the error.
func TestHoldLabelConventionsCheckListsTheStoreOnce(t *testing.T) {
	store := &routeQuerySpyStore{Store: beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "GA-1", Title: "two", Type: "task", Status: "open", Labels: []string{"on-hold", "blocked"}},
		{ID: "GA-2", Title: "closed", Type: "task", Status: "closed", Labels: []string{"on-hold"}},
	}, nil)}
	res := newHoldLabelConventionsCheck("/city", "city", func(string) (beads.Store, error) {
		return store, nil
	}).Run(&doctor.CheckContext{})
	want := []string{`retired label "blocked" on GA-1 "two"`, `retired label "on-hold" on GA-1 "two"`}
	if res.Status != doctor.StatusError || strings.Join(res.Details, "\n") != strings.Join(want, "\n") {
		t.Fatalf("result = %+v, want error with %v", res, want)
	}
	if len(store.queries) != 1 {
		t.Fatalf("store listed %d times, want 1: %+v", len(store.queries), store.queries)
	}

	res = newHoldLabelConventionsCheck("/city", "city", func(string) (beads.Store, error) {
		return routeListErrorStore{err: fmt.Errorf("store unreachable")}, nil
	}).Run(&doctor.CheckContext{})
	if res.Status != doctor.StatusWarning || res.Severity != doctor.SeverityAdvisory ||
		res.Message != "hold-label conventions unknown for city: listing beads: store unreachable" {
		t.Fatalf("result = %+v, want an advisory warning naming the listing error", res)
	}
}
