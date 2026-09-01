package stalldetect

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func sampleReport() Report {
	now := time.Now().UTC()
	return Report{
		EvaluatedAt:      now,
		ThresholdSeconds: (15 * time.Minute).Seconds(),
		Entries: []Entry{
			{BeadID: "gcg-1", Pool: "polecat", Assignee: "polecat-2", LastEventType: "execution.step_started", LastEventAt: now.Add(-30 * time.Minute), AgeSeconds: (30 * time.Minute).Seconds(), Stalled: true},
			{BeadID: "gcg-2", Pool: "unassigned", Assignee: "", LastEventType: "bead.created", LastEventAt: now.Add(-1 * time.Minute), AgeSeconds: 60, Stalled: false},
		},
		Pools: []PoolSummary{
			{Pool: "polecat", InProgress: 1, Stalled: 1, OldestAgeSeconds: (30 * time.Minute).Seconds()},
			{Pool: "unassigned", InProgress: 1, Stalled: 0, OldestAgeSeconds: 60},
		},
		TotalInProgress: 2,
		TotalStalled:    1,
		Skipped:         0,
	}
}

func TestFormatTable_HappyPath(t *testing.T) {
	var buf bytes.Buffer
	if err := FormatTable(&buf, sampleReport()); err != nil {
		t.Fatalf("FormatTable: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"Bead", "Pool", "Assignee", "Last Event", "Age", "Stalled",
		"gcg-1", "polecat", "polecat-2",
		"gcg-2", "unassigned",
		"yes", "no",
		"2 in-progress, 1 stalled",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected output to contain %q\n%s", want, out)
		}
	}
}

func TestFormatTable_EmptyAssigneeRendersAsDash(t *testing.T) {
	var buf bytes.Buffer
	if err := FormatTable(&buf, sampleReport()); err != nil {
		t.Fatalf("FormatTable: %v", err)
	}
	if !strings.Contains(buf.String(), "—") {
		t.Errorf("expected em-dash placeholder for empty assignee, got:\n%s", buf.String())
	}
}

func TestFormatTable_SkippedNoteAppears(t *testing.T) {
	var buf bytes.Buffer
	r := sampleReport()
	r.Skipped = 2
	if err := FormatTable(&buf, r); err != nil {
		t.Fatalf("FormatTable: %v", err)
	}
	if !strings.Contains(buf.String(), "2 bead.created/bead.updated/bead.closed event(s) skipped") {
		t.Errorf("expected skipped note, got:\n%s", buf.String())
	}
}

func TestFormatTable_NoEntriesStillEmitsHeader(t *testing.T) {
	var buf bytes.Buffer
	if err := FormatTable(&buf, Report{}); err != nil {
		t.Fatalf("FormatTable: %v", err)
	}
	if !strings.Contains(buf.String(), "Bead") {
		t.Errorf("expected header row even with no entries, got:\n%s", buf.String())
	}
}

func TestFormatJSON_RoundTrips(t *testing.T) {
	var buf bytes.Buffer
	r := sampleReport()
	if err := FormatJSON(&buf, r); err != nil {
		t.Fatalf("FormatJSON: %v", err)
	}
	var decoded Report
	if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, buf.String())
	}
	if len(decoded.Entries) != len(r.Entries) {
		t.Errorf("Entries length mismatch: got %d, want %d", len(decoded.Entries), len(r.Entries))
	}
	if decoded.TotalInProgress != r.TotalInProgress || decoded.TotalStalled != r.TotalStalled {
		t.Errorf("totals mismatch: got in_progress=%d stalled=%d, want in_progress=%d stalled=%d",
			decoded.TotalInProgress, decoded.TotalStalled, r.TotalInProgress, r.TotalStalled)
	}
}

func TestFormatAge(t *testing.T) {
	cases := []struct {
		seconds float64
		want    string
	}{
		{-5, "0s"},
		{0, "0s"},
		{30, "30s"},
		{90, "1m30s"},
		{3661, "1h1m"},
	}
	for _, tc := range cases {
		if got := formatAge(tc.seconds); got != tc.want {
			t.Errorf("formatAge(%v) = %q, want %q", tc.seconds, got, tc.want)
		}
	}
}
