package stalldetect

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
)

func mustEncodeBead(t *testing.T, b beads.Bead) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("marshal bead: %v", err)
	}
	return data
}

func beadEvent(t *testing.T, seq uint64, eventType, subject string, ts time.Time, b beads.Bead) events.Event {
	t.Helper()
	return events.Event{
		Seq:     seq,
		Type:    eventType,
		Ts:      ts,
		Subject: subject,
		Payload: mustEncodeBead(t, b),
	}
}

func plainEvent(seq uint64, eventType, subject string, ts time.Time) events.Event {
	return events.Event{Seq: seq, Type: eventType, Ts: ts, Subject: subject}
}

func TestAnalyze_StalledInProgressBeadReported(t *testing.T) {
	now := time.Now().UTC()
	stale := now.Add(-30 * time.Minute)
	es := []events.Event{
		beadEvent(t, 1, events.BeadCreated, "gcg-1", stale, beads.Bead{ID: "gcg-1", Status: "in_progress", Assignee: "polecat-2"}),
	}
	report := Analyze(es, Window{}, now, 15*time.Minute, Filter{})

	if report.TotalInProgress != 1 {
		t.Fatalf("TotalInProgress = %d, want 1", report.TotalInProgress)
	}
	if report.TotalStalled != 1 {
		t.Fatalf("TotalStalled = %d, want 1", report.TotalStalled)
	}
	entry := report.Entries[0]
	if entry.BeadID != "gcg-1" || entry.Pool != "polecat" || !entry.Stalled {
		t.Errorf("unexpected entry: %+v", entry)
	}
	if entry.AgeSeconds < 29*60 || entry.AgeSeconds > 31*60 {
		t.Errorf("AgeSeconds = %v, want ~1800", entry.AgeSeconds)
	}
}

func TestAnalyze_FreshInProgressBeadNotStalled(t *testing.T) {
	now := time.Now().UTC()
	fresh := now.Add(-1 * time.Minute)
	es := []events.Event{
		beadEvent(t, 1, events.BeadCreated, "gcg-1", fresh, beads.Bead{ID: "gcg-1", Status: "in_progress", Assignee: "polecat-1"}),
	}
	report := Analyze(es, Window{}, now, 15*time.Minute, Filter{})
	if report.TotalInProgress != 1 || report.TotalStalled != 0 {
		t.Fatalf("expected 1 in-progress, 0 stalled, got %+v", report)
	}
	if report.Entries[0].Stalled {
		t.Errorf("expected not stalled: %+v", report.Entries[0])
	}
}

func TestAnalyze_ClosedBeadNotReported(t *testing.T) {
	now := time.Now().UTC()
	es := []events.Event{
		beadEvent(t, 1, events.BeadCreated, "gcg-1", now.Add(-1*time.Hour), beads.Bead{ID: "gcg-1", Status: "in_progress"}),
		beadEvent(t, 2, events.BeadClosed, "gcg-1", now.Add(-30*time.Minute), beads.Bead{ID: "gcg-1", Status: "closed"}),
	}
	report := Analyze(es, Window{}, now, 15*time.Minute, Filter{})
	if report.TotalInProgress != 0 {
		t.Errorf("closed bead should not be reported, got %+v", report.Entries)
	}
}

func TestAnalyze_OpenBeadNotReported(t *testing.T) {
	now := time.Now().UTC()
	es := []events.Event{
		beadEvent(t, 1, events.BeadCreated, "gcg-1", now.Add(-1*time.Hour), beads.Bead{ID: "gcg-1", Status: "open"}),
	}
	report := Analyze(es, Window{}, now, 15*time.Minute, Filter{})
	if report.TotalInProgress != 0 {
		t.Errorf("open bead should not be reported, got %+v", report.Entries)
	}
}

func TestAnalyze_LastEventUsesLatestNonBeadEvent(t *testing.T) {
	now := time.Now().UTC()
	created := now.Add(-1 * time.Hour)
	laterOp := now.Add(-5 * time.Minute)
	es := []events.Event{
		beadEvent(t, 1, events.BeadCreated, "gcg-1", created, beads.Bead{ID: "gcg-1", Status: "in_progress"}),
		plainEvent(2, events.ExecutionStepStarted, "gcg-1", laterOp),
	}
	report := Analyze(es, Window{}, now, 15*time.Minute, Filter{})
	if len(report.Entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(report.Entries))
	}
	entry := report.Entries[0]
	if !entry.LastEventAt.Equal(laterOp) {
		t.Errorf("LastEventAt = %v, want %v (latest event, not just bead snapshot)", entry.LastEventAt, laterOp)
	}
	if entry.LastEventType != events.ExecutionStepStarted {
		t.Errorf("LastEventType = %q, want %q", entry.LastEventType, events.ExecutionStepStarted)
	}
	if entry.Stalled {
		t.Errorf("bead has a recent non-bead event; should not be stalled: %+v", entry)
	}
}

func TestAnalyze_StatusUsesLatestSnapshot(t *testing.T) {
	now := time.Now().UTC()
	es := []events.Event{
		beadEvent(t, 1, events.BeadCreated, "gcg-1", now.Add(-2*time.Hour), beads.Bead{ID: "gcg-1", Status: "open"}),
		beadEvent(t, 2, events.BeadUpdated, "gcg-1", now.Add(-1*time.Hour), beads.Bead{ID: "gcg-1", Status: "in_progress"}),
	}
	report := Analyze(es, Window{}, now, 15*time.Minute, Filter{})
	if len(report.Entries) != 1 {
		t.Fatalf("expected 1 in-progress entry reflecting latest snapshot, got %d", len(report.Entries))
	}
}

func TestAnalyze_UnassignedBeadGroupsUnderUnassignedPool(t *testing.T) {
	now := time.Now().UTC()
	es := []events.Event{
		beadEvent(t, 1, events.BeadCreated, "gcg-1", now.Add(-1*time.Hour), beads.Bead{ID: "gcg-1", Status: "in_progress"}),
	}
	report := Analyze(es, Window{}, now, 15*time.Minute, Filter{})
	if len(report.Entries) != 1 || report.Entries[0].Pool != unassignedPool {
		t.Errorf("expected unassigned pool, got %+v", report.Entries)
	}
}

func TestAnalyze_PoolFilter(t *testing.T) {
	now := time.Now().UTC()
	stale := now.Add(-1 * time.Hour)
	es := []events.Event{
		beadEvent(t, 1, events.BeadCreated, "gcg-1", stale, beads.Bead{ID: "gcg-1", Status: "in_progress", Assignee: "polecat-1"}),
		beadEvent(t, 2, events.BeadCreated, "gcg-2", stale, beads.Bead{ID: "gcg-2", Status: "in_progress", Assignee: "mechanic-1"}),
	}
	report := Analyze(es, Window{}, now, 15*time.Minute, Filter{Pool: "polecat"})
	if len(report.Entries) != 1 || report.Entries[0].Pool != "polecat" {
		t.Errorf("pool filter failed: %+v", report.Entries)
	}
}

func TestAnalyze_WindowExcludesOutOfRangeEvents(t *testing.T) {
	now := time.Now().UTC()
	es := []events.Event{
		beadEvent(t, 1, events.BeadCreated, "gcg-1", now.Add(-48*time.Hour), beads.Bead{ID: "gcg-1", Status: "in_progress"}),
	}
	report := Analyze(es, Window{Since: now.Add(-1 * time.Hour)}, now, 15*time.Minute, Filter{})
	if report.TotalInProgress != 0 {
		t.Errorf("expected window to exclude the older event, got %+v", report.Entries)
	}
}

func TestAnalyze_UndecodablePayloadCountsAsSkipped(t *testing.T) {
	now := time.Now().UTC()
	es := []events.Event{
		{Seq: 1, Type: events.BeadCreated, Ts: now, Subject: "gcg-1", Payload: json.RawMessage(`not json`)},
	}
	report := Analyze(es, Window{}, now, 15*time.Minute, Filter{})
	if report.Skipped != 1 {
		t.Errorf("Skipped = %d, want 1", report.Skipped)
	}
	if len(report.Entries) != 0 {
		t.Errorf("expected no entries from an undecodable snapshot, got %+v", report.Entries)
	}
}

func TestAnalyze_EventsWithoutSubjectIgnored(t *testing.T) {
	now := time.Now().UTC()
	es := []events.Event{
		plainEvent(1, events.SessionWoke, "", now),
	}
	report := Analyze(es, Window{}, now, 15*time.Minute, Filter{})
	if len(report.Entries) != 0 || report.Skipped != 0 {
		t.Errorf("expected no entries and no skips for subject-less events, got %+v", report)
	}
}

func TestAnalyze_PoolSummaryAggregates(t *testing.T) {
	now := time.Now().UTC()
	stale := now.Add(-1 * time.Hour)
	fresh := now.Add(-1 * time.Minute)
	es := []events.Event{
		beadEvent(t, 1, events.BeadCreated, "gcg-1", stale, beads.Bead{ID: "gcg-1", Status: "in_progress", Assignee: "polecat-1"}),
		beadEvent(t, 2, events.BeadCreated, "gcg-2", fresh, beads.Bead{ID: "gcg-2", Status: "in_progress", Assignee: "polecat-2"}),
	}
	report := Analyze(es, Window{}, now, 15*time.Minute, Filter{})
	if len(report.Pools) != 1 {
		t.Fatalf("expected 1 pool summary, got %d: %+v", len(report.Pools), report.Pools)
	}
	p := report.Pools[0]
	if p.Pool != "polecat" || p.InProgress != 2 || p.Stalled != 1 {
		t.Errorf("unexpected pool summary: %+v", p)
	}
}

func TestPoolForAssignee(t *testing.T) {
	cases := map[string]string{
		"":              unassignedPool,
		"polecat-2":     "polecat",
		"polecat-0":     "polecat-0", // "-0" is not a valid instance suffix (n >= 1)
		"mechanic-4090": "mechanic",
		"solo":          "solo",
		"weird-abc":     "weird-abc",
	}
	for assignee, want := range cases {
		if got := poolForAssignee(assignee); got != want {
			t.Errorf("poolForAssignee(%q) = %q, want %q", assignee, got, want)
		}
	}
}
