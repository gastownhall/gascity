package api

import (
	"errors"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

func TestParseOrdersFeedLimitCapsLargeValues(t *testing.T) {
	if got := parseOrdersFeedLimit(""); got != 50 {
		t.Fatalf("default limit = %d, want 50", got)
	}
	if got := parseOrdersFeedLimit("25"); got != 25 {
		t.Fatalf("parsed limit = %d, want 25", got)
	}
	if got := parseOrdersFeedLimit("999999"); got != maxOrdersFeedLimit {
		t.Fatalf("capped limit = %d, want %d", got, maxOrdersFeedLimit)
	}
}

func TestOrderTrackingStatusTreatsWispFailedAsFailed(t *testing.T) {
	bead := beads.Bead{
		Status: "closed",
		Labels: []string{"order-tracking", "wisp", "wisp-failed"},
	}
	if got := orderTrackingStatus(bead); got != "failed" {
		t.Fatalf("orderTrackingStatus = %q, want failed", got)
	}
}

func TestParseMonitorTimestampAcceptsRFC3339AndNano(t *testing.T) {
	base := "2026-03-26T14:06:31+01:00"
	if got := parseMonitorTimestamp(base); got.IsZero() {
		t.Fatalf("parseMonitorTimestamp(%q) = zero, want parsed timestamp", base)
	}

	nano := "2026-03-26T14:06:31.123456789+01:00"
	got := parseMonitorTimestamp(nano)
	if got.IsZero() {
		t.Fatalf("parseMonitorTimestamp(%q) = zero, want parsed timestamp", nano)
	}
	if got.Nanosecond() != 123456789 {
		t.Fatalf("nanoseconds = %d, want 123456789", got.Nanosecond())
	}
	if got.Format("2006-01-02T15:04:05.999999999Z07:00") != nano {
		t.Fatalf("formatted timestamp = %q, want %q", got.Format("2006-01-02T15:04:05.999999999Z07:00"), nano)
	}
}

func TestBuildWorkflowRunProjectionsKeepsInProgressChildrenOnHistoryFailure(t *testing.T) {
	state := newFakeState(t)
	mem := beads.NewMemStore()
	state.stores = map[string]beads.Store{
		"myrig": &workflowProjectionStore{MemStore: mem},
	}

	root, err := mem.Create(beads.Bead{
		Title: "Deploy",
		Type:  "workflow",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	child, err := mem.Create(beads.Bead{
		Title:    "Run step",
		Type:     "task",
		Assignee: "agent/alice",
		Metadata: map[string]string{
			"gc.root_bead_id": root.ID,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	status := "in_progress"
	if err := mem.Update(child.ID, beads.UpdateOpts{Status: &status}); err != nil {
		t.Fatal(err)
	}

	got, err := buildWorkflowRunProjections(state, "rig", "myrig")
	if err != nil {
		t.Fatalf("buildWorkflowRunProjections: %v", err)
	}
	if len(got.Items) != 1 {
		t.Fatalf("items = %d, want 1", len(got.Items))
	}
	if got.Items[0].Status != "active" {
		t.Fatalf("status = %q, want active", got.Items[0].Status)
	}
	if !got.Items[0].UpdatedAt.Equal(child.CreatedAt) {
		t.Fatalf("updatedAt = %s, want %s", got.Items[0].UpdatedAt, child.CreatedAt)
	}
}

type workflowProjectionStore struct {
	*beads.MemStore
}

func (s *workflowProjectionStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if query.IncludeClosed && query.Metadata["gc.root_bead_id"] != "" {
		return nil, errors.New("history unavailable")
	}
	return s.MemStore.List(query)
}
