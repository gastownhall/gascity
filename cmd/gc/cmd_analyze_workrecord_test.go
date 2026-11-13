package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

func TestAnalyzeWorkRecordFromStoreTable(t *testing.T) {
	store := beads.NewMemStoreFrom(1, []beads.Bead{
		{ID: "wr-covered", Type: "task", Status: "closed", Metadata: map[string]string{beadmeta.WorkOutcomeMetadataKey: beadmeta.WorkOutcomeNoOp}},
		{ID: "wr-missing", Type: "task", Status: "closed", Metadata: map[string]string{}},
		{ID: "wr-open", Type: "task", Status: "open", Metadata: map[string]string{}},
	}, nil)

	var buf strings.Builder
	if err := analyzeWorkRecordFromStore(store, 100, false, &buf); err != nil {
		t.Fatalf("analyzeWorkRecordFromStore: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"wr-missing"} {
		if !strings.Contains(out, want) {
			t.Fatalf("table output missing %q; got:\n%s", want, out)
		}
	}
	if strings.Contains(out, "wr-open") {
		t.Fatalf("open bead should not be scanned; got:\n%s", out)
	}
}

func TestAnalyzeWorkRecordFromStoreJSON(t *testing.T) {
	store := beads.NewMemStoreFrom(1, []beads.Bead{
		{ID: "wr-covered", Type: "task", Status: "closed", Metadata: map[string]string{beadmeta.WorkOutcomeMetadataKey: beadmeta.WorkOutcomeShipped}},
	}, nil)

	var buf strings.Builder
	if err := analyzeWorkRecordFromStore(store, 100, true, &buf); err != nil {
		t.Fatalf("analyzeWorkRecordFromStore: %v", err)
	}
	out := buf.String()
	for _, want := range []string{`"total_gated"`, `"covered"`, `"missing"`, `"coverage"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("JSON output missing %q; got:\n%s", want, out)
		}
	}
}

func TestAnalyzeWorkRecordFromStoreOnlyScansClosed(t *testing.T) {
	store := beads.NewMemStoreFrom(1, []beads.Bead{
		{ID: "wr-in-progress", Type: "task", Status: "in_progress", Metadata: map[string]string{}},
	}, nil)

	var buf strings.Builder
	if err := analyzeWorkRecordFromStore(store, 100, true, &buf); err != nil {
		t.Fatalf("analyzeWorkRecordFromStore: %v", err)
	}
	var parsed struct {
		TotalGated int `json:"total_gated"`
	}
	if err := json.Unmarshal([]byte(buf.String()), &parsed); err != nil {
		t.Fatalf("output not valid JSON: %v\n%s", err, buf.String())
	}
	if parsed.TotalGated != 0 {
		t.Fatalf("expected zero gated beads scanned (in_progress bead must be excluded); got total_gated=%d (raw: %s)", parsed.TotalGated, buf.String())
	}
}

func TestNewAnalyzeCmdRegistersWorkRecordSubcommand(t *testing.T) {
	var stdout, stderr strings.Builder
	cmd := newAnalyzeCmd(&stdout, &stderr)
	found := false
	for _, sub := range cmd.Commands() {
		if sub.Use == "work-record" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a %q subcommand registered under %q", "work-record", "analyze")
	}
}
