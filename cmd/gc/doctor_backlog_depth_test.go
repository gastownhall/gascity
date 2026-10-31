package main

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/storebinding"
)

func TestClassifyBacklog(t *testing.T) {
	future := time.Now().Add(24 * time.Hour)

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
		// genuinely claimable real work (unblocked, in readyIDs)
		{ID: "R-1", Title: "fix the thing", Type: "task", Status: "open"},
		{ID: "R-2", Title: "feature work", Type: "feature", Status: "open"},
		// other: deferred (future), infra/excluded type, and a blocked real-type bead
		{ID: "O-1", Title: "deferred task", Type: "task", Status: "open", DeferUntil: &future},
		{ID: "O-2", Title: "workflow container", Type: "molecule", Status: "open"},
		// B-1 would pass IsReadyCandidateForTier but is dep-blocked: must go to other, not real.
		{ID: "B-1", Title: "blocked work", Type: "task", Status: "open"},
	}

	// readyIDs represents store.Ready() output: only R-1 and R-2 are dep-unblocked
	// actionable work. B-1 is blocked (open dep), O-1 deferred, O-2 excluded type.
	readyIDs := map[string]bool{"R-1": true, "R-2": true}

	b := classifyBacklog(open, readyIDs)

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
	// other = O-1 (deferred) + O-2 (molecule) + B-1 (dep-blocked) = 3
	if b.other != 3 {
		t.Errorf("other = %d, want 3 (deferred + molecule + dep-blocked)", b.other)
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
	// Store with one blocked bead (B-1 depends on R-1) to verify B-1 is not
	// counted in the claimable total.
	store := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "S-1", Title: "planner-1", Type: "session", Status: "open"},
		{ID: "N-1", Title: "nudge:abc", Type: "chore", Status: "open"},
		{ID: "N-2", Title: "nudge:def", Type: "chore", Status: "open"},
		{ID: "E-1", Title: "EPIC", Type: "epic", Status: "open"},
		{ID: "R-1", Title: "real work", Type: "task", Status: "open"},
		{ID: "B-1", Title: "blocked work", Type: "task", Status: "open"},
	}, []beads.Dep{
		{IssueID: "B-1", DependsOnID: "R-1", Type: "blocks"},
	})

	check := newBacklogDepthCheck("/city", func(string) (beads.Store, error) { return store, nil })

	res := check.Run(&doctor.CheckContext{})
	if res.Status != doctor.StatusOK {
		t.Fatalf("Status = %v, want OK (observability never gates): %#v", res.Status, res)
	}
	if res.Severity != doctor.SeverityAdvisory {
		t.Fatalf("Severity = %v, want Advisory", res.Severity)
	}
	// Message must surface real=1 (only R-1; B-1 is dep-blocked) and total=6.
	for _, want := range []string{"1", "6"} {
		if !strings.Contains(res.Message, want) {
			t.Errorf("Message %q missing %q", res.Message, want)
		}
	}
	// Message must scope to city store, not claim city-wide truth.
	if !strings.Contains(res.Message, "city store") {
		t.Errorf("Message %q missing scope qualifier 'city store'", res.Message)
	}
	details := strings.Join(res.Details, "\n")
	if !strings.Contains(details, "R-1") {
		t.Errorf("Details should name the real claimable bead R-1:\n%s", details)
	}
	for _, noise := range []string{"S-1", "N-1", "E-1", "B-1"} {
		if strings.Contains(details, noise) {
			t.Errorf("Details should not list noise/blocked bead %q:\n%s", noise, details)
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
	if check.CanFix() {
		t.Errorf("CanFix = true, want false (read-only observability check)")
	}
}

// A beads nudge queue record is a delivery chore, as a gc:nudge shadow is
// (DES-NQ D14): without it a city in beads mode reads its queue as backlog.
func TestBacklogDepthCountsNudgeQueueRecordsAsNotifications(t *testing.T) {
	store := openShimTestBeadsStore(t)
	queue, err := storebinding.NewBeadsNudgeQueue(beads.NudgesStore{Store: store})
	if err != nil {
		t.Fatalf("NewBeadsNudgeQueue: %v", err)
	}
	now := time.Now().UTC()
	if err := queue.Enqueue(nudgequeue.Item{ID: "n-backlog", Agent: "alpha/worker", Source: "test", Message: "hi", CreatedAt: now, DeliverAfter: now, ExpiresAt: now.Add(time.Hour)}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	open, err := store.ListOpen("open")
	if err != nil {
		t.Fatalf("ListOpen: %v", err)
	}
	var records []beads.Bead
	for _, b := range open {
		if storebinding.IsNudgeQueueRecord(b) {
			records = append(records, b)
		}
	}
	if len(records) != 1 {
		t.Fatalf("open queue records = %d, want 1", len(records))
	}
	if !isNotificationBacklogBead(records[0]) {
		t.Fatalf("isNotificationBacklogBead(%+v) = false, want a queue record counted as a notification chore", records[0])
	}
	b := classifyBacklog(records, map[string]bool{records[0].ID: true})
	if b.notification != 1 || len(b.real) != 0 {
		t.Fatalf("classifyBacklog = notification %d, real %d; want the record as a notification, not real work", b.notification, len(b.real))
	}
}
