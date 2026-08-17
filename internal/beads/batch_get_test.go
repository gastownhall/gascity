package beads

import (
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

type batchGetStub struct {
	Store
	rows  []Bead
	err   error
	calls [][]string
}

func (s *batchGetStub) GetBatch(ids []string) ([]Bead, error) {
	s.calls = append(s.calls, append([]string(nil), ids...))
	return append([]Bead(nil), s.rows...), s.err
}

type countingGetStore struct {
	Store
	calls []string
}

func (s *countingGetStore) Get(id string) (Bead, error) {
	s.calls = append(s.calls, id)
	return s.Store.Get(id)
}

type fixedPointGetStore struct {
	Store
	row Bead
}

func (s *fixedPointGetStore) Get(string) (Bead, error) {
	return s.row, nil
}

func TestGetBatchUsesCapabilityAndNormalizesOrder(t *testing.T) {
	store := &batchGetStub{
		Store: NewMemStore(),
		rows: []Bead{
			{ID: "gc-1", Title: "first"},
			{ID: "gc-2", Title: "second"},
		},
	}

	got, err := GetBatch(store, []string{"gc-2", "gc-1", "gc-2"})
	if err != nil {
		t.Fatalf("GetBatch: %v", err)
	}
	if gotIDs := batchRowIDs(got); !reflect.DeepEqual(gotIDs, []string{"gc-2", "gc-1"}) {
		t.Fatalf("GetBatch IDs = %v, want stable first-input order", gotIDs)
	}
	if !reflect.DeepEqual(store.calls, [][]string{{"gc-2", "gc-1"}}) {
		t.Fatalf("BatchGetter calls = %v, want one deduplicated call", store.calls)
	}
}

func TestGetBatchRejectsIncompleteOrMalformedCapabilityResults(t *testing.T) {
	partialCause := errors.New("truncated row")
	tests := []struct {
		name        string
		rows        []Bead
		err         error
		want        string
		wantIs      error
		wantPartial bool
	}{
		{
			name: "missing", rows: []Bead{{ID: "gc-1"}},
			want: "missing", wantIs: ErrNotFound,
		},
		{
			name: "duplicate", rows: []Bead{{ID: "gc-1"}, {ID: "gc-1"}, {ID: "gc-2"}},
			want: "duplicate",
		},
		{
			name: "unexpected", rows: []Bead{{ID: "gc-1"}, {ID: "gc-other"}},
			want: "unexpected",
		},
		{
			name: "malformed", rows: []Bead{{ID: "gc-1"}, {}},
			want: "malformed",
		},
		{
			name: "partial error",
			rows: []Bead{{ID: "gc-1"}},
			err:  &PartialResultError{Op: "test batch", Err: partialCause},
			want: "test batch", wantIs: partialCause, wantPartial: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := &batchGetStub{Store: NewMemStore(), rows: tc.rows, err: tc.err}
			got, err := GetBatch(store, []string{"gc-1", "gc-2"})
			if err == nil {
				t.Fatal("GetBatch error = nil, want whole-batch failure")
			}
			if got != nil {
				t.Fatalf("GetBatch rows = %#v, want nil on whole-batch failure", got)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("GetBatch error = %v, want substring %q", err, tc.want)
			}
			if tc.wantIs != nil && !errors.Is(err, tc.wantIs) {
				t.Fatalf("GetBatch error = %v, want errors.Is(%v)", err, tc.wantIs)
			}
			if tc.wantPartial != IsPartialResult(err) {
				t.Fatalf("IsPartialResult(%v) = %v, want %v", err, IsPartialResult(err), tc.wantPartial)
			}
		})
	}
}

func TestGetBatchFallsBackToUniquePointReads(t *testing.T) {
	backing := NewMemStore()
	first, err := backing.Create(Bead{Title: "first"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := backing.Create(Bead{Title: "second"})
	if err != nil {
		t.Fatal(err)
	}
	store := &countingGetStore{Store: backing}

	got, err := GetBatch(store, []string{second.ID, first.ID, second.ID})
	if err != nil {
		t.Fatalf("GetBatch fallback: %v", err)
	}
	if gotIDs := batchRowIDs(got); !reflect.DeepEqual(gotIDs, []string{second.ID, first.ID}) {
		t.Fatalf("GetBatch fallback IDs = %v, want [%s %s]", gotIDs, second.ID, first.ID)
	}
	if !reflect.DeepEqual(store.calls, []string{second.ID, first.ID}) {
		t.Fatalf("Get calls = %v, want one per unique ID", store.calls)
	}

	got, err = GetBatch(store, []string{first.ID, "gc-missing"})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetBatch fallback missing error = %v, want ErrNotFound", err)
	}
	if got != nil {
		t.Fatalf("GetBatch fallback missing rows = %#v, want nil", got)
	}
}

func TestGetBatchValidatesFallbackRows(t *testing.T) {
	tests := []struct {
		name string
		row  Bead
		want string
	}{
		{name: "unexpected", row: Bead{ID: "gc-other"}, want: "unexpected"},
		{name: "malformed", row: Bead{}, want: "malformed"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := &fixedPointGetStore{Store: NewMemStore(), row: tc.row}
			got, err := GetBatch(store, []string{"gc-requested"})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("GetBatch error = %v, want %q failure", err, tc.want)
			}
			if got != nil {
				t.Fatalf("GetBatch rows = %#v, want nil", got)
			}
		})
	}
}

func TestGetBatchRejectsMalformedInputWithoutCallingStore(t *testing.T) {
	store := &batchGetStub{Store: NewMemStore()}
	for _, ids := range [][]string{{""}, {"gc-1", " \t"}} {
		got, err := GetBatch(store, ids)
		if err == nil || !strings.Contains(err.Error(), "malformed") {
			t.Fatalf("GetBatch(%q) error = %v, want malformed ID error", ids, err)
		}
		if got != nil {
			t.Fatalf("GetBatch(%q) rows = %#v, want nil", ids, got)
		}
	}
	if len(store.calls) != 0 {
		t.Fatalf("BatchGetter calls = %v, want none for malformed input", store.calls)
	}
}

func TestGetBatchEmptyIsNoop(t *testing.T) {
	store := &batchGetStub{Store: NewMemStore()}
	got, err := GetBatch(store, nil)
	if err != nil {
		t.Fatalf("GetBatch(nil): %v", err)
	}
	if got != nil {
		t.Fatalf("GetBatch(nil) = %#v, want nil", got)
	}
	if len(store.calls) != 0 {
		t.Fatalf("BatchGetter calls = %v, want none", store.calls)
	}
}

func TestMemStoreGetBatchClonesUnderStableUniqueOrder(t *testing.T) {
	store := NewMemStore()
	priority := 3
	first, err := store.Create(Bead{
		Title: "first", Priority: &priority,
		Metadata: map[string]string{"owner": "one"}, Labels: []string{"alpha"},
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Create(Bead{
		Title: "second", Priority: &priority,
		Metadata: map[string]string{"owner": "two"}, Labels: []string{"beta"},
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err := store.GetBatch([]string{second.ID, first.ID, second.ID})
	if err != nil {
		t.Fatalf("MemStore.GetBatch: %v", err)
	}
	if gotIDs := batchRowIDs(got); !reflect.DeepEqual(gotIDs, []string{second.ID, first.ID}) {
		t.Fatalf("MemStore.GetBatch IDs = %v, want [%s %s]", gotIDs, second.ID, first.ID)
	}

	got[0].Metadata["owner"] = "mutated"
	got[0].Labels[0] = "mutated"
	*got[0].Priority = 99
	stored, err := store.Get(second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Metadata["owner"] != "two" || stored.Labels[0] != "beta" || *stored.Priority != priority {
		t.Fatalf("mutating batch result changed stored bead: %+v", stored)
	}

	got, err = store.GetBatch([]string{first.ID, "gc-missing"})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("MemStore.GetBatch missing error = %v, want ErrNotFound", err)
	}
	if got != nil {
		t.Fatalf("MemStore.GetBatch missing rows = %#v, want nil", got)
	}
}

func TestFileStoreGetBatchRefreshesOnce(t *testing.T) {
	filesystem := fsys.NewFake()
	path := filepath.Join("/city", ".gc", "beads.json")
	writer, err := OpenFileStore(filesystem, path)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := OpenFileStore(filesystem, path)
	if err != nil {
		t.Fatal(err)
	}

	var created []Bead
	for _, title := range []string{"first", "second", "third"} {
		b, err := writer.Create(Bead{Title: title})
		if err != nil {
			t.Fatal(err)
		}
		created = append(created, b)
	}

	filesystem.Calls = nil
	got, err := reader.GetBatch([]string{created[2].ID, created[0].ID, created[1].ID})
	if err != nil {
		t.Fatalf("FileStore.GetBatch: %v", err)
	}
	if gotIDs := batchRowIDs(got); !reflect.DeepEqual(gotIDs, []string{created[2].ID, created[0].ID, created[1].ID}) {
		t.Fatalf("FileStore.GetBatch IDs = %v, want stable request order", gotIDs)
	}

	var statCalls, readCalls int
	for _, call := range filesystem.Calls {
		if call.Path != path {
			continue
		}
		switch call.Method {
		case "Stat":
			statCalls++
		case "ReadFile":
			readCalls++
		}
	}
	if statCalls != 1 || readCalls != 1 {
		t.Fatalf("FileStore.GetBatch refresh calls: Stat=%d ReadFile=%d, want one each", statCalls, readCalls)
	}
}

func batchRowIDs(rows []Bead) []string {
	ids := make([]string, len(rows))
	for i := range rows {
		ids[i] = rows[i].ID
	}
	return ids
}

var (
	_ BatchGetter = (*batchGetStub)(nil)
	_ BatchGetter = (*MemStore)(nil)
	_ BatchGetter = (*FileStore)(nil)
)
