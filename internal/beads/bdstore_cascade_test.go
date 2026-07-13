package beads

import (
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

func TestBdStoreDeleteCascadeBatchesInOneCall(t *testing.T) {
	var calls [][]string
	s := NewBdStore("/city", recordingBdRunner(&calls))
	if err := s.DeleteCascade([]string{"a", "b", "c"}); err != nil {
		t.Fatalf("DeleteCascade: %v", err)
	}
	if len(calls) != 1 {
		t.Fatalf("want 1 batched bd call, got %d: %v", len(calls), calls)
	}
	got := strings.Join(calls[0], " ")
	for _, want := range []string{"bd", "delete", "a", "b", "c", "--cascade", "--force"} {
		if !strings.Contains(got, want) {
			t.Errorf("batched call %q missing %q", got, want)
		}
	}
}

func TestBdStoreDeleteCascadeChunksLargeSets(t *testing.T) {
	var calls [][]string
	s := NewBdStore("/city", recordingBdRunner(&calls))
	n := bdDeleteCascadeChunk + 5
	ids := make([]string, n)
	for i := range ids {
		ids[i] = "id" + strconv.Itoa(i)
	}
	if err := s.DeleteCascade(ids); err != nil {
		t.Fatalf("DeleteCascade: %v", err)
	}
	if len(calls) != 2 {
		t.Fatalf("want 2 chunked calls for %d ids (chunk=%d), got %d", n, bdDeleteCascadeChunk, len(calls))
	}
}

func TestBdStoreDeleteCascadeEmptyIsNoop(t *testing.T) {
	called := false
	s := NewBdStore("/city", func(_, _ string, _ ...string) ([]byte, error) {
		called = true
		return nil, nil
	})
	if err := s.DeleteCascade(nil); err != nil {
		t.Fatalf("DeleteCascade(nil): %v", err)
	}
	if called {
		t.Fatalf("DeleteCascade(nil) should not invoke bd")
	}
}

// BdStore must advertise the batched cascade-delete capability so the wisp GC
// discovers it by interface assertion.
var _ CascadeDeleter = (*BdStore)(nil)
