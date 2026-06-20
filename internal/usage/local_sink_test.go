package usage

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestLocalSinkAppendAndReadDedup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "usage.jsonl")
	s := NewLocalSink(path)
	ctx := context.Background()

	f1 := Fact{Kind: KindModel, RunID: "r1", IdempotencyKey: "k1", InputTokens: 1}
	f2 := Fact{Kind: KindModel, RunID: "r1", IdempotencyKey: "k2", InputTokens: 2}
	for _, f := range []Fact{f1, f2, f1 /* replay of k1 */} {
		if err := s.Record(ctx, f); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}

	got, warnings, err := ReadFacts(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 0 {
		t.Fatalf("clean log must yield no warnings, got %v", warnings)
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
	got, warnings, err := ReadFacts(filepath.Join(t.TempDir(), "does-not-exist.jsonl"))
	if err != nil {
		t.Fatalf("missing file must not error: %v", err)
	}
	if got != nil || warnings != nil {
		t.Fatalf("missing file must yield nil/nil, got facts=%v warnings=%v", got, warnings)
	}
}

func TestReadFactsKeepsEmptyKeyFacts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.jsonl")
	s := NewLocalSink(path)
	ctx := context.Background()
	// Two distinct facts with no idempotency key must both survive.
	if err := s.Record(ctx, Fact{Kind: KindCompute, RunID: "r1", WallSeconds: 1}); err != nil {
		t.Fatal(err)
	}
	if err := s.Record(ctx, Fact{Kind: KindCompute, RunID: "r2", WallSeconds: 2}); err != nil {
		t.Fatal(err)
	}
	got, _, err := ReadFacts(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("empty-key facts must not be deduped: got %d", len(got))
	}
}

func TestReadFactsSkipsTornFinalLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.jsonl")
	if err := NewLocalSink(path).Record(context.Background(), Fact{Kind: KindModel, IdempotencyKey: "k1"}); err != nil {
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
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	got, warnings, err := ReadFacts(path)
	if err != nil {
		t.Fatalf("torn line must not error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected the one intact fact, got %d", len(got))
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], ":2") {
		t.Fatalf("torn line must be reported as a line-2 warning, got %v", warnings)
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
			_ = s.Record(context.Background(), Fact{Kind: KindModel, IdempotencyKey: ModelIdempotencyKey("r", string(rune('A'+i%26))+pad(i))})
		}(i)
	}
	wg.Wait()

	got, _, err := ReadFacts(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != n {
		t.Fatalf("concurrent appends lost/corrupted lines: got %d want %d", len(got), n)
	}
}

func TestReadFactsSkipsMalformedMiddleLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.jsonl")
	s := NewLocalSink(path)
	ctx := context.Background()
	if err := s.Record(ctx, Fact{Kind: KindModel, IdempotencyKey: "k1"}); err != nil {
		t.Fatal(err)
	}
	// Corrupt a complete record in the middle of the file (with a trailing
	// newline), then append another valid fact after it.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("{not valid json}\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Record(ctx, Fact{Kind: KindModel, IdempotencyKey: "k2"}); err != nil {
		t.Fatal(err)
	}

	// A malformed interior line must not fail the whole read; the surrounding
	// intact facts must still be returned and the bad line reported.
	got, warnings, err := ReadFacts(path)
	if err != nil {
		t.Fatalf("malformed interior line must not error: %v", err)
	}
	if len(got) != 2 || got[0].IdempotencyKey != "k1" || got[1].IdempotencyKey != "k2" {
		t.Fatalf("both intact facts must survive, got %+v", got)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], ":2") {
		t.Fatalf("malformed line 2 must be reported once, got %v", warnings)
	}
}

func TestReadFactsSkipsNewlineTerminatedCorruptFinalLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.jsonl")
	if err := NewLocalSink(path).Record(context.Background(), Fact{Kind: KindModel, IdempotencyKey: "k1"}); err != nil {
		t.Fatal(err)
	}
	// A corrupt, newline-terminated final record is skipped and reported, not
	// fatal: gc costs stays usable and still surfaces the loss.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("{not valid json}\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	got, warnings, err := ReadFacts(path)
	if err != nil {
		t.Fatalf("corrupt final record must not error: %v", err)
	}
	if len(got) != 1 || got[0].IdempotencyKey != "k1" {
		t.Fatalf("the intact fact must survive, got %+v", got)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], ":2") {
		t.Fatalf("corrupt line 2 must be reported, got %v", warnings)
	}
}

// TestReadFactsRecoversFromTornThenAppended is the regression for the adopt-pr
// review finding that a torn record followed by a later append turned into an
// interior malformed line and hard-failed every subsequent gc costs read. The
// later fact must survive (the writer separates the torn tail) and the read must
// succeed, skipping only the torn fragment with a warning.
func TestReadFactsRecoversFromTornThenAppended(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.jsonl")
	s := NewLocalSink(path)
	ctx := context.Background()
	if err := s.Record(ctx, Fact{Kind: KindModel, IdempotencyKey: "k1"}); err != nil {
		t.Fatal(err)
	}
	// Simulate a crash mid-append: a torn partial line with no trailing newline.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"kind":"model","run_id":"torn`); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	// A later append must not be swallowed by the torn tail.
	if err := s.Record(ctx, Fact{Kind: KindModel, IdempotencyKey: "k2"}); err != nil {
		t.Fatal(err)
	}

	got, warnings, err := ReadFacts(path)
	if err != nil {
		t.Fatalf("torn-then-appended log must not hard-fail the read: %v", err)
	}
	if len(got) != 2 || got[0].IdempotencyKey != "k1" || got[1].IdempotencyKey != "k2" {
		t.Fatalf("the pre- and post-torn facts must both survive, got %+v", got)
	}
	if len(warnings) != 1 {
		t.Fatalf("exactly the torn fragment must be reported, got %v", warnings)
	}
}

func TestReadFactsKeepsValidUnterminatedFinalLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.jsonl")
	if err := NewLocalSink(path).Record(context.Background(), Fact{Kind: KindModel, IdempotencyKey: "k1"}); err != nil {
		t.Fatal(err)
	}
	// A well-formed JSON record with no trailing newline is still a complete,
	// readable fact (only an UNPARSEABLE unterminated tail is treated as torn).
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"kind":"model","idempotency_key":"k2"}`); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	got, warnings, err := ReadFacts(path)
	if err != nil {
		t.Fatalf("valid unterminated final line must not error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected both facts (intact + unterminated), got %d", len(got))
	}
	if len(warnings) != 0 {
		t.Fatalf("a valid unterminated line is not malformed, got warnings %v", warnings)
	}
}

func pad(i int) string {
	return string(rune('0'+i/10)) + string(rune('0'+i%10))
}
