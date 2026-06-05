package main

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/doctor"
)

func TestClassifyBacklog(t *testing.T) {
	now := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	future := now.Add(24 * time.Hour)

	open := []beads.Bead{
		// control-plane: by type and by label
		{ID: "S-1", Title: "voxist.planner-1", Type: "session", Status: "open"},
		{ID: "S-2", Title: "registry", Type: "task", Status: "open", Labels: []string{"gc:session"}},
		// notification: nudge/mail title prefixes, type=message, gc:nudge label
		{ID: "N-1", Title: "nudge:abc", Type: "chore", Status: "open"},
		{ID: "N-2", Title: "mail:welcome", Type: "task", Status: "open"},
		{ID: "N-3", Title: "some mail", Type: "message", Status: "open"},
		{ID: "N-4", Title: "labeled nudge", Type: "task", Status: "open", Labels: []string{"gc:nudge"}},
		// epic parents
		{ID: "E-1", Title: "EPIC: self-healing", Type: "epic", Status: "open"},
		// genuinely claimable real work
		{ID: "R-1", Title: "fix the thing", Type: "task", Status: "open"},
		{ID: "R-2", Title: "feature work", Type: "feature", Status: "open"},
		// other: deferred (future), and an infra/excluded type
		{ID: "O-1", Title: "deferred task", Type: "task", Status: "open", DeferUntil: &future},
		{ID: "O-2", Title: "workflow container", Type: "molecule", Status: "open"},
	}

	b := classifyBacklog(open, now)

	if b.total != len(open) {
		t.Errorf("total = %d, want %d", b.total, len(open))
	}
	if b.controlPlane != 2 {
		t.Errorf("controlPlane = %d, want 2", b.controlPlane)
	}
	if b.notification != 4 {
		t.Errorf("notification = %d, want 4", b.notification)
	}
	if b.epic != 1 {
		t.Errorf("epic = %d, want 1", b.epic)
	}
	if b.other != 2 {
		t.Errorf("other = %d, want 2", b.other)
	}
	gotReal := make([]string, 0, len(b.real))
	for _, r := range b.real {
		gotReal = append(gotReal, r.ID)
	}
	want := []string{"R-1", "R-2"}
	if strings.Join(gotReal, ",") != strings.Join(want, ",") {
		t.Errorf("real = %v, want %v", gotReal, want)
	}
}

func TestBacklogDepthCheckRunReportsTrueDepth(t *testing.T) {
	now := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	store := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "S-1", Title: "planner-1", Type: "session", Status: "open"},
		{ID: "N-1", Title: "nudge:abc", Type: "chore", Status: "open"},
		{ID: "N-2", Title: "nudge:def", Type: "chore", Status: "open"},
		{ID: "E-1", Title: "EPIC", Type: "epic", Status: "open"},
		{ID: "R-1", Title: "real work", Type: "task", Status: "open"},
	}, nil)

	check := newBacklogDepthCheck("/city", func(string) (beads.Store, error) { return store, nil })
	check.now = now

	res := check.Run(&doctor.CheckContext{})
	if res.Status != doctor.StatusOK {
		t.Fatalf("Status = %v, want OK (observability never gates): %#v", res.Status, res)
	}
	if res.Severity != doctor.SeverityAdvisory {
		t.Fatalf("Severity = %v, want Advisory", res.Severity)
	}
	// Message must surface the real count (1) distinct from the raw open count (5).
	for _, want := range []string{"1", "5"} {
		if !strings.Contains(res.Message, want) {
			t.Errorf("Message %q missing %q", res.Message, want)
		}
	}
	details := strings.Join(res.Details, "\n")
	if !strings.Contains(details, "R-1") {
		t.Errorf("Details should name the real claimable bead R-1:\n%s", details)
	}
	for _, noise := range []string{"S-1", "N-1", "E-1"} {
		if strings.Contains(details, noise) {
			t.Errorf("Details should not list noise bead %q:\n%s", noise, details)
		}
	}
}

func TestBacklogDepthCheckStoreErrorIsGraceful(t *testing.T) {
	check := newBacklogDepthCheck("/city", func(string) (beads.Store, error) {
		return nil, fmt.Errorf("store unreachable")
	})
	res := check.Run(&doctor.CheckContext{})
	// A store read failure must not panic and must stay advisory (observability
	// only — it never gates dispatch or `gc start`).
	if res.Severity != doctor.SeverityAdvisory {
		t.Fatalf("Severity = %v, want Advisory on store error", res.Severity)
	}
	if res.Status == doctor.StatusError {
		t.Fatalf("store error should not be a blocking StatusError: %#v", res)
	}
	if !check.CanFix() == false {
		// CanFix must be false: this is a read-only observability check.
		t.Errorf("CanFix = %v, want false", check.CanFix())
	}
}
