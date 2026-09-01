package sessionoutcomes

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestFormatTable_RendersGroupsAndTotal(t *testing.T) {
	bucket := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	r := Report{
		Groups: []Group{
			{Key: GroupKey{Template: "polecat", Provider: "anthropic", BucketStart: bucket}, Started: 2, Succeeded: 1, Failed: 1, MinDurationMs: 100, MaxDurationMs: 200},
		},
		Total: Group{Started: 2, Succeeded: 1, Failed: 1, MinDurationMs: 100, MaxDurationMs: 200},
	}
	var buf bytes.Buffer
	if err := FormatTable(&buf, r); err != nil {
		t.Fatalf("FormatTable: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "polecat") || !strings.Contains(out, "anthropic") {
		t.Errorf("expected group row in output, got:\n%s", out)
	}
	if !strings.Contains(out, "TOTAL") {
		t.Errorf("expected TOTAL row in output, got:\n%s", out)
	}
}

func TestFormatTable_NotesSkippedAndInstrumentation(t *testing.T) {
	r := Report{
		Skipped:         2,
		Instrumentation: Instrumentation{StartAttempts: 5, MissingTemplate: 1, MissingProvider: 3},
	}
	var buf bytes.Buffer
	if err := FormatTable(&buf, r); err != nil {
		t.Fatalf("FormatTable: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "2 worker.operation event(s) skipped") {
		t.Errorf("expected skipped note, got:\n%s", out)
	}
	if !strings.Contains(out, "template missing on 1/5") {
		t.Errorf("expected instrumentation note, got:\n%s", out)
	}
}

func TestFormatTable_PossibleOutageNote(t *testing.T) {
	bucket := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	r := Report{
		Groups: []Group{
			{Key: GroupKey{Template: "polecat", Provider: "anthropic", BucketStart: bucket}, Started: 3, Failed: 3, MinDurationMs: 100, MaxDurationMs: 150},
		},
		Total: Group{Started: 3, Failed: 3, MinDurationMs: 100, MaxDurationMs: 150},
	}
	var buf bytes.Buffer
	if err := FormatTable(&buf, r); err != nil {
		t.Fatalf("FormatTable: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "possible outage") {
		t.Errorf("expected outage note, got:\n%s", out)
	}
	if !strings.Contains(out, "yes") {
		t.Errorf("expected Outage? column to render yes, got:\n%s", out)
	}
}

func TestFormatJSON_EncodesReport(t *testing.T) {
	r := Report{Total: Group{Started: 1, Succeeded: 1}}
	var buf bytes.Buffer
	if err := FormatJSON(&buf, r); err != nil {
		t.Fatalf("FormatJSON: %v", err)
	}
	if !strings.Contains(buf.String(), `"started": 1`) {
		t.Errorf("expected started field in JSON, got:\n%s", buf.String())
	}
}
