package beads

import (
	"errors"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

type authoritativeSnapshotCapabilityStub struct {
	Store
	rows  []Bead
	err   error
	calls int
}

func (s *authoritativeSnapshotCapabilityStub) AuthoritativeSnapshot() ([]Bead, error) {
	s.calls++
	return append([]Bead(nil), s.rows...), s.err
}

type authoritativeSnapshotFallbackStub struct {
	Store
	ids        []string
	rows       []Bead
	scanCalls  int
	batchCalls int
	batchIDs   []string
}

func (s *authoritativeSnapshotFallbackStub) ScanAllIDs() ([]string, error) {
	s.scanCalls++
	return append([]string(nil), s.ids...), nil
}

func (s *authoritativeSnapshotFallbackStub) GetBatch(ids []string) ([]Bead, error) {
	s.batchCalls++
	s.batchIDs = append([]string(nil), ids...)
	return append([]Bead(nil), s.rows...), nil
}

func TestAuthoritativeSnapshotUsesCapabilityAndValidatesWholeResult(t *testing.T) {
	store := &authoritativeSnapshotCapabilityStub{
		Store: NewMemStore(),
		rows:  []Bead{{ID: "gc-z"}, {ID: "gc-a"}},
	}
	got, err := AuthoritativeSnapshot(store)
	if err != nil {
		t.Fatalf("AuthoritativeSnapshot: %v", err)
	}
	if store.calls != 1 {
		t.Fatalf("capability calls = %d, want 1", store.calls)
	}
	if gotIDs := []string{got[0].ID, got[1].ID}; !reflect.DeepEqual(gotIDs, []string{"gc-a", "gc-z"}) {
		t.Fatalf("snapshot IDs = %v, want deterministic ID order", gotIDs)
	}

	store.rows = []Bead{{ID: "gc-a"}, {ID: "gc-a"}}
	if _, err := AuthoritativeSnapshot(store); err == nil {
		t.Fatal("duplicate capability rows returned nil error")
	}
	store.rows = []Bead{{ID: " "}}
	if _, err := AuthoritativeSnapshot(store); err == nil {
		t.Fatal("malformed capability row returned nil error")
	}
	sentinel := errors.New("snapshot unavailable")
	store.rows, store.err = nil, sentinel
	if _, err := AuthoritativeSnapshot(store); !errors.Is(err, sentinel) {
		t.Fatalf("capability error = %v, want %v", err, sentinel)
	}
}

func TestAuthoritativeSnapshotFallsBackToSortedCensusAndBatch(t *testing.T) {
	store := &authoritativeSnapshotFallbackStub{
		Store: NewMemStore(),
		ids:   []string{"gc-z", "gc-a"},
		rows:  []Bead{{ID: "gc-a"}, {ID: "gc-z"}},
	}
	got, err := AuthoritativeSnapshot(store)
	if err != nil {
		t.Fatalf("AuthoritativeSnapshot: %v", err)
	}
	if store.scanCalls != 1 || store.batchCalls != 1 {
		t.Fatalf("scan/batch calls = %d/%d, want 1/1", store.scanCalls, store.batchCalls)
	}
	if !reflect.DeepEqual(store.batchIDs, []string{"gc-a", "gc-z"}) {
		t.Fatalf("batch IDs = %v, want sorted census", store.batchIDs)
	}
	if gotIDs := []string{got[0].ID, got[1].ID}; !reflect.DeepEqual(gotIDs, store.batchIDs) {
		t.Fatalf("snapshot IDs = %v, want %v", gotIDs, store.batchIDs)
	}
}

func TestMemStoreAuthoritativeSnapshotIsDetachedAndIncludesClosed(t *testing.T) {
	store := NewMemStore()
	first, err := store.Create(Bead{Title: "first", Metadata: map[string]string{"kind": "original"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(first.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(Bead{Title: "second"}); err != nil {
		t.Fatal(err)
	}

	rows, err := store.AuthoritativeSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Status != "closed" {
		t.Fatalf("snapshot = %#v, want closed and open rows", rows)
	}
	rows[0].Metadata["kind"] = "mutated"
	point, err := store.Get(first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if point.Metadata["kind"] != "original" {
		t.Fatalf("snapshot mutation leaked into store: %#v", point.Metadata)
	}
}

func TestFileStoreAuthoritativeSnapshotRefreshesCrossProcessState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "beads.json")
	writer, err := OpenFileStore(fsys.OSFS{}, path)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := OpenFileStore(fsys.OSFS{}, path)
	if err != nil {
		t.Fatal(err)
	}
	created, err := writer.Create(Bead{Title: "written elsewhere"})
	if err != nil {
		t.Fatal(err)
	}

	rows, err := reader.AuthoritativeSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID != created.ID {
		t.Fatalf("snapshot rows = %#v, want freshly reloaded bead %q", rows, created.ID)
	}
}
