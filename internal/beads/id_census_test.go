package beads

import (
	"errors"
	"reflect"
	"testing"
)

type completeIDScannerStub struct {
	Store
	ids       []string
	err       error
	scanCalls int
	listCalls int
}

func (s *completeIDScannerStub) ScanAllIDs() ([]string, error) {
	s.scanCalls++
	return append([]string(nil), s.ids...), s.err
}

func (s *completeIDScannerStub) List(ListQuery) ([]Bead, error) {
	s.listCalls++
	return nil, errors.New("unexpected List")
}

type censusListStub struct {
	Store
	rows  []Bead
	err   error
	query ListQuery
	calls int
}

func (s *censusListStub) List(query ListQuery) ([]Bead, error) {
	s.calls++
	s.query = query
	return append([]Bead(nil), s.rows...), s.err
}

func TestScanAllIDsUsesStrictCapabilityAndNormalizesIDs(t *testing.T) {
	store := &completeIDScannerStub{ids: []string{"gc-b", "gc-a", "gc-b"}}
	got, err := ScanAllIDs(store)
	if err != nil {
		t.Fatalf("ScanAllIDs: %v", err)
	}
	if want := []string{"gc-b", "gc-a"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ScanAllIDs = %v, want %v", got, want)
	}
	if store.scanCalls != 1 || store.listCalls != 0 {
		t.Fatalf("scan calls = %d, list calls = %d, want 1, 0", store.scanCalls, store.listCalls)
	}
}

func TestScanAllIDsFallsBackToCompleteTierBothList(t *testing.T) {
	store := &censusListStub{rows: []Bead{{ID: "gc-a"}, {ID: "gc-b"}, {ID: "gc-a"}}}
	got, err := ScanAllIDs(store)
	if err != nil {
		t.Fatalf("ScanAllIDs: %v", err)
	}
	if want := []string{"gc-a", "gc-b"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ScanAllIDs = %v, want %v", got, want)
	}
	if store.calls != 1 {
		t.Fatalf("List calls = %d, want 1", store.calls)
	}
	if q := store.query; !q.AllowScan || !q.IncludeClosed || !q.SkipLabels || q.TierMode != TierBoth || q.HasFilter() {
		t.Fatalf("fallback List query = %#v, want unfiltered all-status TierBoth scan", q)
	}
}

func TestScanAllIDsFailsWholeCensus(t *testing.T) {
	partialCause := errors.New("partial native scan")
	for _, tc := range []struct {
		name  string
		store Store
		want  error
	}{
		{
			name:  "capability error",
			store: &completeIDScannerStub{ids: []string{"gc-a"}, err: &PartialResultError{Op: "scan", Err: partialCause}},
			want:  partialCause,
		},
		{
			name:  "malformed capability ID",
			store: &completeIDScannerStub{ids: []string{"gc-a", " "}},
		},
		{
			name:  "fallback list error",
			store: &censusListStub{rows: []Bead{{ID: "gc-a"}}, err: partialCause},
			want:  partialCause,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ScanAllIDs(tc.store)
			if err == nil {
				t.Fatal("ScanAllIDs error = nil, want whole-census failure")
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("ScanAllIDs error = %v, want errors.Is(%v)", err, tc.want)
			}
			if got != nil {
				t.Fatalf("ScanAllIDs rows = %v, want nil on failure", got)
			}
		})
	}
}

var _ CompleteIDScanner = (*completeIDScannerStub)(nil)
