package beadthroughput

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func sampleReport() Report {
	return Report{
		Groups: []Group{
			{Key: GroupKey{Store: "gcg", Type: "task", Label: "urgent"}, Opened: 5, Closed: 2},
			{Key: GroupKey{Store: "rig1", Type: "bug", Label: ""}, Opened: 1, Closed: 3},
		},
		Total:   Group{Opened: 6, Closed: 5},
		Skipped: 0,
	}
}

func TestFormatTable_HappyPath(t *testing.T) {
	var buf bytes.Buffer
	if err := FormatTable(&buf, sampleReport()); err != nil {
		t.Fatalf("FormatTable: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"Store", "Type", "Label", "Opened", "Closed", "Net",
		"gcg", "task", "urgent",
		"rig1", "bug",
		"TOTAL",
		"+3", // net for gcg row (5-2)
		"-2", // net for rig1 row (1-3)
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected output to contain %q\n%s", want, out)
		}
	}
}

func TestFormatTable_EmptyLabelRendersAsDash(t *testing.T) {
	var buf bytes.Buffer
	if err := FormatTable(&buf, sampleReport()); err != nil {
		t.Fatalf("FormatTable: %v", err)
	}
	if !strings.Contains(buf.String(), "—") {
		t.Errorf("expected em-dash placeholder for empty label, got:\n%s", buf.String())
	}
}

func TestFormatTable_SkippedNoteAppears(t *testing.T) {
	var buf bytes.Buffer
	r := sampleReport()
	r.Skipped = 3
	if err := FormatTable(&buf, r); err != nil {
		t.Fatalf("FormatTable: %v", err)
	}
	if !strings.Contains(buf.String(), "3 bead.created/bead.closed event(s) skipped") {
		t.Errorf("expected skipped note, got:\n%s", buf.String())
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
	if len(decoded.Groups) != len(r.Groups) {
		t.Errorf("Groups length mismatch: got %d, want %d", len(decoded.Groups), len(r.Groups))
	}
	if decoded.Total != r.Total {
		t.Errorf("Total mismatch: got %+v, want %+v", decoded.Total, r.Total)
	}
}
