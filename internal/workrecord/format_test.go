package workrecord

import (
	"strings"
	"testing"
)

func TestFormatTable(t *testing.T) {
	report := CoverageReport{
		TotalGated: 4,
		Covered:    2,
		Missing:    2,
		Coverage:   0.5,
		MissingIDs: []string{"wr-missing-1", "wr-missing-2"},
	}
	var buf strings.Builder
	if err := FormatTable(&buf, report); err != nil {
		t.Fatalf("FormatTable: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"4", "2", "50.0%", "wr-missing-1", "wr-missing-2"} {
		if !strings.Contains(out, want) {
			t.Fatalf("FormatTable output missing %q; got:\n%s", want, out)
		}
	}
}

func TestFormatTableNoMissing(t *testing.T) {
	report := CoverageReport{TotalGated: 3, Covered: 3, Missing: 0, Coverage: 1}
	var buf strings.Builder
	if err := FormatTable(&buf, report); err != nil {
		t.Fatalf("FormatTable: %v", err)
	}
	out := buf.String()
	if strings.Contains(out, "Missing bead IDs") {
		t.Fatalf("expected no missing-IDs section when Missing is 0, got:\n%s", out)
	}
}

func TestFormatTableZeroTotal(t *testing.T) {
	var buf strings.Builder
	if err := FormatTable(&buf, CoverageReport{}); err != nil {
		t.Fatalf("FormatTable: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "0.0%") {
		t.Fatalf("expected 0%% coverage on zero total, got:\n%s", out)
	}
}
