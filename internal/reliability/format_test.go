package reliability

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func sampleReport() Report {
	return Report{
		Groups: []Group{
			{
				Key:      GroupKey{Model: "claude-opus-4-7", PromptVersion: "v3", Rig: "rigA"},
				Sessions: 100, Crashed: 5, Quarantined: 1, IdleKilled: 2, Drained: 3,
				UnhealthyTotal: 11,
			},
			{
				Key:      GroupKey{Model: "claude-sonnet-4-6", PromptVersion: "v2", Rig: "rigB"},
				Sessions: 50, Crashed: 1, UnhealthyTotal: 1,
			},
		},
		Total:   Group{Sessions: 150, Crashed: 6, Quarantined: 1, IdleKilled: 2, Drained: 3, UnhealthyTotal: 12},
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
		"Model", "Version", "Rig", "Sessions",
		"claude-opus-4-7", "v3", "rigA",
		"claude-sonnet-4-6", "v2", "rigB",
		"TOTAL",
		"5.0%",  // crash rate for opus row (5/100)
		"11.0%", // unhealthy rate for opus row
	} {
		if !strings.Contains(out, want) {
			t.Errorf("table output missing %q\n%s", want, out)
		}
	}
}

func TestFormatTable_EmptyKeysShowDash(t *testing.T) {
	r := Report{
		Groups: []Group{
			{Key: GroupKey{}, Sessions: 1, Crashed: 1, UnhealthyTotal: 1},
		},
		Total: Group{Sessions: 1, Crashed: 1, UnhealthyTotal: 1},
	}
	var buf bytes.Buffer
	if err := FormatTable(&buf, r); err != nil {
		t.Fatalf("FormatTable: %v", err)
	}
	if !strings.Contains(buf.String(), "—") {
		t.Errorf("empty key fields should render as dash:\n%s", buf.String())
	}
}

func TestFormatTable_SkippedNoteWhenNonZero(t *testing.T) {
	r := sampleReport()
	r.Skipped = 4
	var buf bytes.Buffer
	if err := FormatTable(&buf, r); err != nil {
		t.Fatalf("FormatTable: %v", err)
	}
	if !strings.Contains(buf.String(), "4 lifecycle event(s) skipped") {
		t.Errorf("expected skipped note, got:\n%s", buf.String())
	}
}

func TestFormatTable_NoSkippedNoteWhenZero(t *testing.T) {
	r := sampleReport()
	r.Skipped = 0
	var buf bytes.Buffer
	if err := FormatTable(&buf, r); err != nil {
		t.Fatalf("FormatTable: %v", err)
	}
	if strings.Contains(buf.String(), "skipped") {
		t.Errorf("zero-skip report should not mention skipped events:\n%s", buf.String())
	}
}

func TestFormatTable_AmbiguousAliasNoteWhenNonZero(t *testing.T) {
	r := sampleReport()
	r.AmbiguousAliases = 2
	var buf bytes.Buffer
	if err := FormatTable(&buf, r); err != nil {
		t.Fatalf("FormatTable: %v", err)
	}
	if !strings.Contains(buf.String(), "2 lifecycle event(s) counted as ambiguous_aliases") {
		t.Errorf("expected ambiguous alias note, got:\n%s", buf.String())
	}
}

func TestFormatTable_DroppedLifecycleSummary(t *testing.T) {
	r := sampleReport()
	r.Skipped = 3
	r.AmbiguousAliases = 2
	var buf bytes.Buffer
	if err := FormatTable(&buf, r); err != nil {
		t.Fatalf("FormatTable: %v", err)
	}
	if !strings.Contains(buf.String(), "5 lifecycle event(s) dropped before grouping (skipped + ambiguous_aliases)") {
		t.Errorf("expected dropped lifecycle summary, got:\n%s", buf.String())
	}
}

func TestFormatTable_InstrumentationNotes(t *testing.T) {
	r := sampleReport()
	r.Instrumentation = Instrumentation{
		WorkerOperations:       3,
		MissingModel:           2,
		MissingPromptVersion:   1,
		QuarantineSignalStatus: quarantineSignalStatusNotEmitted,
	}
	var buf bytes.Buffer
	if err := FormatTable(&buf, r); err != nil {
		t.Fatalf("FormatTable: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"model/prompt_version instrumentation incomplete",
		"model missing on 2/3 worker.operation event(s)",
		"event counts, not session counts",
		"session.quarantined is not emitted by current production paths",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("instrumentation note missing %q\n%s", want, out)
		}
	}
}

func TestFormatTable_NoQuarantineNoteWhenObserved(t *testing.T) {
	r := sampleReport()
	r.Instrumentation.QuarantineSignalStatus = quarantineSignalStatusObserved
	var buf bytes.Buffer
	if err := FormatTable(&buf, r); err != nil {
		t.Fatalf("FormatTable: %v", err)
	}
	if strings.Contains(buf.String(), "session.quarantined is not emitted") {
		t.Errorf("observed quarantine signal should suppress not-emitted note:\n%s", buf.String())
	}
}

func TestFormatJSON(t *testing.T) {
	var buf bytes.Buffer
	if err := FormatJSON(&buf, sampleReport()); err != nil {
		t.Fatalf("FormatJSON: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(buf.Bytes(), &parsed); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, buf.String())
	}
	groups, _ := parsed["groups"].([]any)
	if len(groups) != 2 {
		t.Errorf("expected 2 groups in JSON, got %d", len(groups))
	}
	if _, ok := parsed["total"]; !ok {
		t.Error("JSON missing 'total' field")
	}
}

func TestFormatJSON_GroupKeyUsesSnakeCaseFields(t *testing.T) {
	var buf bytes.Buffer
	if err := FormatJSON(&buf, sampleReport()); err != nil {
		t.Fatalf("FormatJSON: %v", err)
	}
	var parsed struct {
		Groups []struct {
			Key map[string]any `json:"key"`
		} `json:"groups"`
	}
	if err := json.Unmarshal(buf.Bytes(), &parsed); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, buf.String())
	}
	if len(parsed.Groups) == 0 {
		t.Fatal("expected at least one group")
	}
	key := parsed.Groups[0].Key
	for _, want := range []string{"model", "prompt_version", "rig"} {
		if _, ok := key[want]; !ok {
			t.Fatalf("group key missing %q: %#v", want, key)
		}
	}
	for _, unwanted := range []string{"Model", "PromptVersion", "Rig"} {
		if _, ok := key[unwanted]; ok {
			t.Fatalf("group key should not include %q: %#v", unwanted, key)
		}
	}
}

func TestFormatJSON_EmptyReport(t *testing.T) {
	var buf bytes.Buffer
	if err := FormatJSON(&buf, Report{}); err != nil {
		t.Fatalf("FormatJSON empty: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(buf.Bytes(), &parsed); err != nil {
		t.Fatalf("empty report is not valid JSON: %v", err)
	}
}

func TestPctStr(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{0, "0.0%"},
		{0.5, "50.0%"},
		{0.123, "12.3%"},
		{1, "100.0%"},
	}
	for _, tc := range cases {
		if got := pctStr(tc.in); got != tc.want {
			t.Errorf("pctStr(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestPadRight(t *testing.T) {
	cases := []struct {
		in   string
		n    int
		want string
	}{
		{"abc", 5, "abc  "},
		{"abc", 3, "abc"},
		{"abcdef", 3, "abcdef"}, // longer-than-target unchanged
		{"", 4, "    "},
	}
	for _, tc := range cases {
		if got := padRight(tc.in, tc.n); got != tc.want {
			t.Errorf("padRight(%q, %d) = %q, want %q", tc.in, tc.n, got, tc.want)
		}
	}
}

func TestColumnWidthsRespectsHeaders(t *testing.T) {
	widths := columnWidths(
		[]string{"LongHeader", "X"},
		[][]string{{"a", "longvalue"}},
	)
	if widths[0] != 10 {
		t.Errorf("col 0 width = %d, want 10 (header)", widths[0])
	}
	if widths[1] != 9 {
		t.Errorf("col 1 width = %d, want 9 (longvalue)", widths[1])
	}
}
