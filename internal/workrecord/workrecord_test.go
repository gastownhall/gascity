package workrecord

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

func TestValidOutcome(t *testing.T) {
	for _, v := range []string{
		beadmeta.WorkOutcomeShipped, beadmeta.WorkOutcomeNoOp,
		beadmeta.WorkOutcomeBlocked, beadmeta.WorkOutcomeAbandoned,
	} {
		if !ValidOutcome(v) {
			t.Errorf("ValidOutcome(%q) = false, want true", v)
		}
	}
	for _, v := range []string{"", "pass", "fail", "skipped", "done", "SHIPPED"} {
		if ValidOutcome(v) {
			t.Errorf("ValidOutcome(%q) = true, want false", v)
		}
	}
}

func TestAnalyzeCoverage(t *testing.T) {
	input := []beads.Bead{
		{ID: "wr-covered-1", Type: "task", Status: "closed", Metadata: map[string]string{beadmeta.WorkOutcomeMetadataKey: beadmeta.WorkOutcomeShipped}},
		{ID: "wr-covered-2", Type: "task", Status: "closed", Metadata: map[string]string{beadmeta.WorkOutcomeMetadataKey: beadmeta.WorkOutcomeNoOp}},
		{ID: "wr-missing-1", Type: "task", Status: "closed", Metadata: map[string]string{}},
		{ID: "wr-missing-2", Type: "task", Status: "closed", Metadata: map[string]string{beadmeta.WorkOutcomeMetadataKey: "done"}},
		{ID: "wr-nongated-1", Type: "convoy", Status: "closed", Metadata: map[string]string{}},
		{ID: "wr-nongated-2", Type: "task", Status: "closed", Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWorkflow}},
	}
	report := AnalyzeCoverage(input)
	if report.TotalGated != 4 {
		t.Fatalf("TotalGated = %d, want 4", report.TotalGated)
	}
	if report.Covered != 2 {
		t.Fatalf("Covered = %d, want 2", report.Covered)
	}
	if report.Missing != 2 {
		t.Fatalf("Missing = %d, want 2", report.Missing)
	}
	if report.Coverage != 0.5 {
		t.Fatalf("Coverage = %v, want 0.5", report.Coverage)
	}
	wantMissing := []string{"wr-missing-1", "wr-missing-2"}
	if len(report.MissingIDs) != len(wantMissing) {
		t.Fatalf("MissingIDs = %v, want %v", report.MissingIDs, wantMissing)
	}
	for i, id := range wantMissing {
		if report.MissingIDs[i] != id {
			t.Fatalf("MissingIDs[%d] = %q, want %q", i, report.MissingIDs[i], id)
		}
	}
}

func TestAnalyzeCoverageEmpty(t *testing.T) {
	report := AnalyzeCoverage(nil)
	if report.TotalGated != 0 || report.Covered != 0 || report.Missing != 0 || len(report.MissingIDs) != 0 {
		t.Fatalf("expected zero-value report for empty input, got %+v", report)
	}
	if report.Coverage != 0 {
		t.Fatalf("Coverage = %v, want 0 when TotalGated is 0", report.Coverage)
	}
}

func TestAnalyzeCoverageAllCovered(t *testing.T) {
	input := []beads.Bead{
		{ID: "wr-1", Type: "task", Status: "closed", Metadata: map[string]string{beadmeta.WorkOutcomeMetadataKey: beadmeta.WorkOutcomeBlocked}},
		{ID: "wr-2", Type: "task", Status: "closed", Metadata: map[string]string{beadmeta.WorkOutcomeMetadataKey: beadmeta.WorkOutcomeAbandoned}},
	}
	report := AnalyzeCoverage(input)
	if report.Missing != 0 || len(report.MissingIDs) != 0 {
		t.Fatalf("expected no missing beads, got Missing=%d MissingIDs=%v", report.Missing, report.MissingIDs)
	}
	if report.Coverage != 1 {
		t.Fatalf("Coverage = %v, want 1", report.Coverage)
	}
}
