package coordstore

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTriageReportSynthesizesTimeseriesAndKillEvents(t *testing.T) {
	resultsDir := t.TempDir()
	runID := "20260524T210000Z"
	phaseADir := filepath.Join(resultsDir, runID, "badger", string(SoakPhaseA))
	phaseBDir := filepath.Join(resultsDir, runID, "badger", string(SoakPhaseB))
	start := time.Date(2026, 5, 24, 21, 0, 0, 0, time.UTC)

	writeJSONL(t, filepath.Join(phaseADir, "timeseries.jsonl"),
		TelemetrySample{
			Timestamp:      start,
			RSSBytes:       100 * 1024 * 1024,
			Goroutines:     10,
			StoreSizeBytes: 1000,
			Operations: map[string]OpSnapshot{
				"Get":                 {P50Nanos: int64(500 * time.Microsecond), P99Nanos: int64(900 * time.Microsecond), P999Nanos: int64(950 * time.Microsecond)},
				"Create":              {P50Nanos: int64(3 * time.Millisecond), P99Nanos: int64(6 * time.Millisecond), P999Nanos: int64(7 * time.Millisecond)},
				"EphemeralFilterScan": {P50Nanos: int64(2 * time.Millisecond), P99Nanos: int64(5 * time.Millisecond), P999Nanos: int64(6 * time.Millisecond)},
			},
		},
		TelemetrySample{
			Timestamp:      start.Add(time.Hour),
			RSSBytes:       130 * 1024 * 1024,
			Goroutines:     30,
			StoreSizeBytes: 2000,
			Operations: map[string]OpSnapshot{
				"Get":                 {P50Nanos: int64(700 * time.Microsecond), P99Nanos: int64(1500 * time.Microsecond), P999Nanos: int64(2 * time.Millisecond)},
				"Create":              {P50Nanos: int64(4 * time.Millisecond), P99Nanos: int64(8 * time.Millisecond), P999Nanos: int64(10 * time.Millisecond)},
				"EphemeralFilterScan": {P50Nanos: int64(3 * time.Millisecond), P99Nanos: int64(7 * time.Millisecond), P999Nanos: int64(9 * time.Millisecond)},
			},
		},
	)
	writeJSONL(t, filepath.Join(phaseBDir, "kill-events.jsonl"),
		KillEvent{RecoveryNanos: int64(100 * time.Millisecond), MissingIDs: []string{"lost-1"}},
		KillEvent{RecoveryNanos: int64(250 * time.Millisecond), MissingIDs: []string{"lost-2", "lost-3"}},
	)

	var report TriageReport
	if err := report.Synthesize(context.Background(), resultsDir); err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	if report.RunID != runID {
		t.Fatalf("RunID = %q, want %q", report.RunID, runID)
	}
	if report.GeneratedAt.IsZero() {
		t.Fatalf("GeneratedAt was not set")
	}
	if len(report.Backends) != 1 {
		t.Fatalf("backends = %d, want 1: %#v", len(report.Backends), report.Backends)
	}
	got := report.Backends[0]
	if got.Name != "badger" {
		t.Fatalf("backend name = %q, want badger", got.Name)
	}
	if got.GetP99Ms.P99 != 1.5 || got.GetP99Ms.P999 != 2 {
		t.Fatalf("Get percentiles = %#v, want p99=1.5 p999=2", got.GetP99Ms)
	}
	if got.CreateP99Ms.P99 != 8 || got.MailPollP99Ms.P99 != 7 {
		t.Fatalf("write/mail percentiles = create %#v mail %#v", got.CreateP99Ms, got.MailPollP99Ms)
	}
	if got.RSSCeilingMB != 130 {
		t.Fatalf("RSSCeilingMB = %.1f, want 130", got.RSSCeilingMB)
	}
	if got.RSSGrowthMBPerHour != 30 {
		t.Fatalf("RSSGrowthMBPerHour = %.1f, want 30", got.RSSGrowthMBPerHour)
	}
	if got.GoroutineP99 != 30 {
		t.Fatalf("GoroutineP99 = %d, want 30", got.GoroutineP99)
	}
	if got.KillEvents != 2 || got.LostRecords != 3 || got.RecoveryP99Ms != 250 {
		t.Fatalf("chaos metrics = kills %d lost %d recovery %.1f", got.KillEvents, got.LostRecords, got.RecoveryP99Ms)
	}
	if got.ForeverTax != "~3d (fully reversible)" {
		t.Fatalf("ForeverTax = %q", got.ForeverTax)
	}
}

func TestTriageReportSynthesizesLongKillEventLine(t *testing.T) {
	resultsDir := t.TempDir()
	runID := "20260526T210325Z"
	phaseBDir := filepath.Join(resultsDir, runID, "dolt", string(SoakPhaseB))
	missingIDs := make([]string, 7000)
	for i := range missingIDs {
		missingIDs[i] = "acked-id-" + strings.Repeat("x", 8) + "-" + string(rune('a'+i%26))
	}
	killEventsPath := filepath.Join(phaseBDir, "kill-events.jsonl")
	writeJSONL(t, killEventsPath,
		KillEvent{RecoveryNanos: int64(123 * time.Millisecond), MissingIDs: missingIDs},
	)
	data, err := os.ReadFile(killEventsPath)
	if err != nil {
		t.Fatalf("read kill events: %v", err)
	}
	if len(data) <= 64*1024 {
		t.Fatalf("test fixture line is %d bytes, want larger than bufio.Scanner default token limit", len(data))
	}

	var report TriageReport
	if err := report.Synthesize(context.Background(), resultsDir); err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	if len(report.Backends) != 1 {
		t.Fatalf("backends = %d, want 1: %#v", len(report.Backends), report.Backends)
	}
	got := report.Backends[0]
	if got.Name != "dolt" {
		t.Fatalf("backend name = %q, want dolt", got.Name)
	}
	if got.KillEvents != 1 || got.LostRecords != len(missingIDs) || got.RecoveryP99Ms != 123 {
		t.Fatalf("chaos metrics = kills %d lost %d recovery %.1f", got.KillEvents, got.LostRecords, got.RecoveryP99Ms)
	}
}

func TestTriageReportWritesJSONAndMarkdownInterpretation(t *testing.T) {
	report := TriageReport{
		RunID:       "run-1",
		GeneratedAt: time.Date(2026, 5, 24, 21, 0, 0, 0, time.UTC),
		Backends: []TriageBackend{
			{Name: "badger", GetP99Ms: Percentile{P99: 1}, CreateP99Ms: Percentile{P99: 9}, MailPollP99Ms: Percentile{P99: 2}, RSSCeilingMB: 128, RSSGrowthMBPerHour: 2, GoroutineP99: 30, KillEvents: 2, LostRecords: 0, RecoveryP99Ms: 40, ForeverTax: "~3d (fully reversible)"},
			{Name: "hqstore", GetP99Ms: Percentile{P99: 3}, CreateP99Ms: Percentile{P99: 4}, MailPollP99Ms: Percentile{P99: 1}, RSSCeilingMB: 256, RSSGrowthMBPerHour: 1, GoroutineP99: 20, KillEvents: 2, LostRecords: 1, RecoveryP99Ms: 30, ForeverTax: "~9d build + ~15-20d/yr maintain + own crash-recovery bugs"},
		},
	}

	var markdown bytes.Buffer
	if err := report.PrintMarkdown(&markdown); err != nil {
		t.Fatalf("PrintMarkdown: %v", err)
	}
	text := markdown.String()
	for _, want := range []string{
		"| Backend | Get p99 ms | Create p99 ms | Mail poll p99 ms | RSS ceiling MB | RSS growth MB/hour | Goroutine p99 | Kill events | Lost records | Recovery p99 ms | Forever tax |",
		"| badger |",
		"## Interpretation",
		"Get p99 ms winner: badger",
		"No clear winner:",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("markdown missing %q:\n%s", want, text)
		}
	}

	path := filepath.Join(t.TempDir(), "triage.json")
	if err := report.WriteJSON(path); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read json: %v", err)
	}
	var roundTrip TriageReport
	if err := json.Unmarshal(data, &roundTrip); err != nil {
		t.Fatalf("unmarshal json: %v", err)
	}
	if roundTrip.RunID != report.RunID || len(roundTrip.Backends) != 2 {
		t.Fatalf("round trip mismatch: %#v", roundTrip)
	}
}

func writeJSONL(t *testing.T, path string, values ...any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	file, err := os.Create(path)
	if err != nil {
		t.Fatalf("create jsonl: %v", err)
	}
	defer file.Close() //nolint:errcheck
	enc := json.NewEncoder(file)
	for _, value := range values {
		if err := enc.Encode(value); err != nil {
			t.Fatalf("encode jsonl: %v", err)
		}
	}
}
