package beads

import (
	"errors"
	"strconv"
	"strings"
	"testing"
)

func recordingBdRunner(calls *[][]string) CommandRunner {
	return func(_, name string, args ...string) ([]byte, error) {
		*calls = append(*calls, append([]string{name}, args...))
		return []byte("{}"), nil
	}
}

func TestBdStoreDeleteBatchBatchesInOneCall(t *testing.T) {
	var calls [][]string
	s := NewBdStore("/city", recordingBdRunner(&calls))
	if err := s.DeleteBatch([]string{"a", "b", "c"}); err != nil {
		t.Fatalf("DeleteBatch: %v", err)
	}
	if len(calls) != 1 {
		t.Fatalf("want 1 batched bd call, got %d: %v", len(calls), calls)
	}
	got := strings.Join(calls[0], " ")
	for _, want := range []string{"bd", "delete", "a", "b", "c", "--force"} {
		if !strings.Contains(got, want) {
			t.Errorf("batched call %q missing %q", got, want)
		}
	}
	// The batch delete must use --force (orphan external dependents), never
	// --cascade (recursively delete dependents outside the collected closure).
	// Passing --cascade here is the data-loss regression this test guards.
	if strings.Contains(got, "--cascade") {
		t.Errorf("batched call %q must not pass --cascade (would recursively delete external dependents)", got)
	}
}

func TestBdStoreDeleteBatchChunksLargeSets(t *testing.T) {
	var calls [][]string
	s := NewBdStore("/city", recordingBdRunner(&calls))
	n := bdDeleteBatchChunk + 5
	ids := make([]string, n)
	for i := range ids {
		ids[i] = "id" + strconv.Itoa(i)
	}
	if err := s.DeleteBatch(ids); err != nil {
		t.Fatalf("DeleteBatch: %v", err)
	}
	if len(calls) != 2 {
		t.Fatalf("want 2 chunked calls for %d ids (chunk=%d), got %d", n, bdDeleteBatchChunk, len(calls))
	}
}

func TestBdStoreDeleteBatchEmptyIsNoop(t *testing.T) {
	called := false
	s := NewBdStore("/city", func(_, _ string, _ ...string) ([]byte, error) {
		called = true
		return nil, nil
	})
	if err := s.DeleteBatch(nil); err != nil {
		t.Fatalf("DeleteBatch(nil): %v", err)
	}
	if called {
		t.Fatalf("DeleteBatch(nil) should not invoke bd")
	}
}

// A later chunk failing after earlier chunks committed must report the
// committed ids so a caching layer can reconcile the partial success instead of
// treating the whole batch as untouched.
func TestBdStoreDeleteBatchReportsCommittedOnLaterChunkFailure(t *testing.T) {
	var call int
	runner := func(_, _ string, _ ...string) ([]byte, error) {
		call++
		if call == 2 { // second chunk fails after the first committed
			return nil, errors.New("bd delete: backend unavailable")
		}
		return []byte("{}"), nil
	}
	s := NewBdStore("/city", runner)

	n := bdDeleteBatchChunk + 5 // two chunks: [0:chunk] then [chunk:chunk+5]
	ids := make([]string, n)
	for i := range ids {
		ids[i] = "id" + strconv.Itoa(i)
	}

	err := s.DeleteBatch(ids)
	var batchErr *BatchDeleteError
	if !errors.As(err, &batchErr) {
		t.Fatalf("DeleteBatch err = %v, want *BatchDeleteError", err)
	}
	if len(batchErr.Committed) != bdDeleteBatchChunk {
		t.Fatalf("Committed len = %d, want %d (the fully-applied first chunk)", len(batchErr.Committed), bdDeleteBatchChunk)
	}
	for i := 0; i < bdDeleteBatchChunk; i++ {
		if batchErr.Committed[i] != ids[i] {
			t.Fatalf("Committed[%d] = %q, want %q", i, batchErr.Committed[i], ids[i])
		}
	}
}

// A first-chunk failure has committed nothing, so the reported committed set is
// empty and a caching layer leaves the cache untouched.
func TestBdStoreDeleteBatchReportsNoCommittedOnFirstChunkFailure(t *testing.T) {
	runner := func(_, _ string, _ ...string) ([]byte, error) {
		return nil, errors.New("bd delete: backend unavailable")
	}
	s := NewBdStore("/city", runner)

	err := s.DeleteBatch([]string{"a", "b", "c"})
	var batchErr *BatchDeleteError
	if !errors.As(err, &batchErr) {
		t.Fatalf("DeleteBatch err = %v, want *BatchDeleteError", err)
	}
	if len(batchErr.Committed) != 0 {
		t.Fatalf("Committed = %v, want empty on first-chunk failure", batchErr.Committed)
	}
}

// BdStore must advertise the batched delete capability so the wisp GC discovers
// it by interface assertion.
var _ BatchDeleter = (*BdStore)(nil)
