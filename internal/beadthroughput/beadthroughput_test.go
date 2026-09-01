package beadthroughput

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

func beadEvent(t *testing.T, seq uint64, eventType string, ts time.Time, b beads.Bead) events.Event {
	t.Helper()
	return events.Event{
		Seq:     seq,
		Type:    eventType,
		Ts:      ts,
		Subject: b.ID,
		Payload: mustEncodeBead(t, b),
	}
}

func TestAnalyze_GroupsByStoreTypeLabel(t *testing.T) {
	now := time.Now().UTC()
	es := []events.Event{
		beadEvent(t, 1, events.BeadCreated, now, beads.Bead{ID: "gcg-1", Type: "task", Labels: []string{"urgent"}}),
		beadEvent(t, 2, events.BeadClosed, now, beads.Bead{ID: "gcg-1", Type: "task", Labels: []string{"urgent"}}),
		beadEvent(t, 3, events.BeadCreated, now, beads.Bead{ID: "rig1-42", Type: "bug"}),
	}
	report := Analyze(es, Window{}, Filter{})

	if report.Total.Opened != 2 {
		t.Errorf("Total.Opened = %d, want 2", report.Total.Opened)
	}
	if report.Total.Closed != 1 {
		t.Errorf("Total.Closed = %d, want 1", report.Total.Closed)
	}
	if len(report.Groups) != 2 {
		t.Fatalf("expected 2 groups, got %d: %+v", len(report.Groups), report.Groups)
	}

	var gcg, rig1 *Group
	for i := range report.Groups {
		g := &report.Groups[i]
		switch g.Key.Store {
		case "gcg":
			gcg = g
		case "rig1":
			rig1 = g
		}
	}
	if gcg == nil || gcg.Key.Type != "task" || gcg.Key.Label != "urgent" || gcg.Opened != 1 || gcg.Closed != 1 {
		t.Errorf("gcg group wrong: %+v", gcg)
	}
	if rig1 == nil || rig1.Key.Type != "bug" || rig1.Key.Label != "" || rig1.Opened != 1 {
		t.Errorf("rig1 group wrong: %+v", rig1)
	}
}

func TestAnalyze_UnprefixedIDUsesDefaultStore(t *testing.T) {
	now := time.Now().UTC()
	es := []events.Event{
		beadEvent(t, 1, events.BeadCreated, now, beads.Bead{ID: "42", Type: "task"}),
	}
	report := Analyze(es, Window{}, Filter{})
	if len(report.Groups) != 1 || report.Groups[0].Key.Store != defaultStore {
		t.Errorf("expected default store bucket, got %+v", report.Groups)
	}
}

func TestAnalyze_MultiLabelBeadFansOutButTotalCountsDistinctBeads(t *testing.T) {
	now := time.Now().UTC()
	es := []events.Event{
		beadEvent(t, 1, events.BeadCreated, now, beads.Bead{ID: "gcg-1", Type: "task", Labels: []string{"a", "b"}}),
	}
	report := Analyze(es, Window{}, Filter{})
	if report.Total.Opened != 1 {
		t.Errorf("Total.Opened = %d, want 1 (distinct bead, not per-label)", report.Total.Opened)
	}
	if len(report.Groups) != 2 {
		t.Fatalf("expected 2 label groups, got %d", len(report.Groups))
	}
	for _, g := range report.Groups {
		if g.Opened != 1 {
			t.Errorf("group %+v: Opened = %d, want 1", g.Key, g.Opened)
		}
	}
}

func TestAnalyze_WindowExcludesOutOfRangeEvents(t *testing.T) {
	now := time.Now().UTC()
	es := []events.Event{
		beadEvent(t, 1, events.BeadCreated, now.Add(-48*time.Hour), beads.Bead{ID: "gcg-1", Type: "task"}),
		beadEvent(t, 2, events.BeadCreated, now, beads.Bead{ID: "gcg-2", Type: "task"}),
	}
	report := Analyze(es, Window{Since: now.Add(-1 * time.Hour)}, Filter{})
	if report.Total.Opened != 1 {
		t.Errorf("Total.Opened = %d, want 1 (window should exclude the older event)", report.Total.Opened)
	}
}

func TestAnalyze_FilterByStoreTypeLabel(t *testing.T) {
	now := time.Now().UTC()
	es := []events.Event{
		beadEvent(t, 1, events.BeadCreated, now, beads.Bead{ID: "gcg-1", Type: "task", Labels: []string{"urgent"}}),
		beadEvent(t, 2, events.BeadCreated, now, beads.Bead{ID: "gcm-1", Type: "bug"}),
	}
	report := Analyze(es, Window{}, Filter{Store: "gcg"})
	if len(report.Groups) != 1 || report.Groups[0].Key.Store != "gcg" {
		t.Errorf("store filter failed: %+v", report.Groups)
	}

	report = Analyze(es, Window{}, Filter{Type: "bug"})
	if len(report.Groups) != 1 || report.Groups[0].Key.Type != "bug" {
		t.Errorf("type filter failed: %+v", report.Groups)
	}

	report = Analyze(es, Window{}, Filter{Label: "urgent"})
	if len(report.Groups) != 1 || report.Groups[0].Key.Label != "urgent" {
		t.Errorf("label filter failed: %+v", report.Groups)
	}
}

func TestAnalyze_UndecodablePayloadCountsAsSkipped(t *testing.T) {
	now := time.Now().UTC()
	es := []events.Event{
		{Seq: 1, Type: events.BeadCreated, Ts: now, Payload: json.RawMessage(`not json`)},
		{Seq: 2, Type: events.BeadCreated, Ts: now}, // no payload at all
	}
	report := Analyze(es, Window{}, Filter{})
	if report.Skipped != 2 {
		t.Errorf("Skipped = %d, want 2", report.Skipped)
	}
	if len(report.Groups) != 0 {
		t.Errorf("expected no groups from undecodable events, got %+v", report.Groups)
	}
}

func TestAnalyze_IgnoresOtherEventTypes(t *testing.T) {
	now := time.Now().UTC()
	es := []events.Event{
		{Seq: 1, Type: events.WorkerOperation, Ts: now, Subject: "s1"},
		{Seq: 2, Type: events.SessionCrashed, Ts: now, Subject: "s1"},
	}
	report := Analyze(es, Window{}, Filter{})
	if len(report.Groups) != 0 || report.Skipped != 0 {
		t.Errorf("expected no groups and no skips for non-bead events, got groups=%+v skipped=%d", report.Groups, report.Skipped)
	}
}

func TestStoreForBeadID(t *testing.T) {
	cases := map[string]string{
		"gcg-1":           "gcg",
		"rig1-42":         "rig1",
		"42":              defaultStore,
		"":                defaultStore,
		"-leading":        defaultStore,
		"no-hyphen-multi": "no",
	}
	for id, want := range cases {
		if got := storeForBeadID(id); got != want {
			t.Errorf("storeForBeadID(%q) = %q, want %q", id, got, want)
		}
	}
}

func TestGroupNet(t *testing.T) {
	g := Group{Opened: 5, Closed: 2}
	if g.Net() != 3 {
		t.Errorf("Net() = %d, want 3", g.Net())
	}
}
