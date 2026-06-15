package usage

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestLocalSinkAppendAndReadDedup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "usage.jsonl")
	s := NewLocalSink(path)
	ctx := context.Background()

	f1 := UsageFact{Kind: KindModel, RunID: "r1", IdempotencyKey: "k1", InputTokens: 1}
	f2 := UsageFact{Kind: KindModel, RunID: "r1", IdempotencyKey: "k2", InputTokens: 2}
	for _, f := range []UsageFact{f1, f2, f1 /* replay of k1 */} {
		if err := s.Record(ctx, f); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}

	got, err := ReadFacts(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 facts after dedup, got %d", len(got))
	}
	// First-occurrence order preserved.
	if got[0].IdempotencyKey != "k1" || got[1].IdempotencyKey != "k2" {
		t.Fatalf("order/dedup wrong: %q, %q", got[0].IdempotencyKey, got[1].IdempotencyKey)
	}
}

func TestReadFactsMissingFile(t *testing.T) {
	got, err := ReadFacts(filepath.Join(t.TempDir(), "does-not-exist.jsonl"))
	if err != nil {
		t.Fatalf("missing file must not error: %v", err)
	}
	if got != nil {
		t.Fatalf("missing file must yield nil, got %v", got)
	}
}

func TestReadFactsKeepsEmptyKeyFacts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.jsonl")
	s := NewLocalSink(path)
	ctx := context.Background()
	// Two distinct facts with no idempotency key must both survive.
	if err := s.Record(ctx, UsageFact{Kind: KindCompute, RunID: "r1", WallSeconds: 1}); err != nil {
		t.Fatal(err)
	}
	if err := s.Record(ctx, UsageFact{Kind: KindCompute, RunID: "r2", WallSeconds: 2}); err != nil {
		t.Fatal(err)
	}
	got, err := ReadFacts(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("empty-key facts must not be deduped: got %d", len(got))
	}
}

func TestReadFactsSkipsTornFinalLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.jsonl")
	if err := NewLocalSink(path).Record(context.Background(), UsageFact{Kind: KindModel, IdempotencyKey: "k1"}); err != nil {
		t.Fatal(err)
	}
	// Simulate a crash mid-append: a partial, unparseable trailing line.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"kind":"model","input_tok`); err != nil {
		t.Fatal(err)
	}
	f.Close()

	got, err := ReadFacts(path)
	if err != nil {
		t.Fatalf("torn line must not error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected the one intact fact, got %d", len(got))
	}
}

func TestLocalSinkConcurrentRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.jsonl")
	s := NewLocalSink(path)
	const n = 50
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Distinct keys so none are deduped.
			_ = s.Record(context.Background(), UsageFact{Kind: KindModel, IdempotencyKey: ModelIdempotencyKey("r", string(rune('A'+i%26))+pad(i))})
		}(i)
	}
	wg.Wait()

	got, err := ReadFacts(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != n {
		t.Fatalf("concurrent appends lost/corrupted lines: got %d want %d", len(got), n)
	}
}

func pad(i int) string {
	return string(rune('0'+i/10)) + string(rune('0'+i%10))
}
