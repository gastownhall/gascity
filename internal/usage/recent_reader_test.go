package usage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func factLine(t *testing.T, fact Fact) string {
	t.Helper()
	b, err := json.Marshal(fact)
	if err != nil {
		t.Fatalf("marshal fact: %v", err)
	}
	return string(b)
}

func TestReadRecentFactsBoundsTheReadAndReportsTruncation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.jsonl")
	old := factLine(t, Fact{Kind: KindModel, InputTokens: 1, IdempotencyKey: "old"})
	recent := factLine(t, Fact{Kind: KindModel, InputTokens: 2, IdempotencyKey: "recent"})
	data := old + "\n" + strings.Repeat("x", 256) + "\n" + recent + "\n"
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}

	facts, report, err := ReadRecentFacts(path, int64(len(recent)+32))
	if err != nil {
		t.Fatal(err)
	}
	if !report.Truncated {
		t.Fatal("Truncated = false, want true")
	}
	if len(facts) != 1 || facts[0].IdempotencyKey != "recent" {
		t.Fatalf("facts = %+v, want only the newest complete fact", facts)
	}
}

func TestReadRecentFactsCountsMalformedLinesWithoutLeakingThePath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret-city", "usage.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	valid := factLine(t, Fact{Kind: KindModel, InputTokens: 3, IdempotencyKey: "valid"})
	if err := os.WriteFile(path, []byte("{not-json\n"+valid+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	facts, report, err := ReadRecentFacts(path, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if report.Malformed != 1 {
		t.Fatalf("Malformed = %d, want 1", report.Malformed)
	}
	if len(facts) != 1 || facts[0].IdempotencyKey != "valid" {
		t.Fatalf("facts = %+v", facts)
	}
}

func TestReadRecentFactsDeduplicatesWithinTheObservedWindow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.jsonl")
	line := factLine(t, Fact{Kind: KindModel, InputTokens: 5, IdempotencyKey: "same"})
	if err := os.WriteFile(path, []byte(line+"\n"+line+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	facts, report, err := ReadRecentFacts(path, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if report != (RecentReadReport{}) {
		t.Fatalf("report = %+v, want empty", report)
	}
	if len(facts) != 1 {
		t.Fatalf("facts = %+v, want one deduplicated fact", facts)
	}
}

func TestReadRecentFactsSkipsAnOversizedRecordAndContinues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.jsonl")
	valid := factLine(t, Fact{Kind: KindModel, InputTokens: 7, IdempotencyKey: "valid"})
	data := strings.Repeat("x", recentFactMaxBytes+1) + "\n" + valid + "\n"
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	facts, report, err := ReadRecentFacts(path, int64(len(data)+1))
	if err != nil {
		t.Fatal(err)
	}
	if report.Oversized != 1 {
		t.Fatalf("Oversized = %d, want 1", report.Oversized)
	}
	if len(facts) != 1 || facts[0].IdempotencyKey != "valid" {
		t.Fatalf("facts = %+v, want valid fact after oversized line", facts)
	}
}

func TestReadRecentFactsCapsDecodedRecordsAndKeepsTheNewestTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.jsonl")
	old := factLine(t, Fact{Kind: KindModel, InputTokens: 1, IdempotencyKey: "old"})
	recent := factLine(t, Fact{Kind: KindModel, InputTokens: 2, IdempotencyKey: "recent"})
	data := old + "\n" + strings.Repeat("{}\n", recentFactMaxRecords) + recent + "\n"
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}

	facts, report, err := ReadRecentFacts(path, int64(len(data)+1))
	if err != nil {
		t.Fatal(err)
	}
	if !report.RecordLimited {
		t.Fatal("RecordLimited = false, want true")
	}
	if len(facts) != recentFactMaxRecords {
		t.Fatalf("len(facts) = %d, want cap %d", len(facts), recentFactMaxRecords)
	}
	if facts[len(facts)-1].IdempotencyKey != "recent" {
		t.Fatalf("newest fact = %+v, want recent tail retained", facts[len(facts)-1])
	}
	for _, fact := range facts {
		if fact.IdempotencyKey == "old" {
			t.Fatal("oldest fact survived the decoded-record cap")
		}
	}
}
