package session

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// This file is the load-bearing safety pin for the WI-6 store-domain-objects
// migration: it characterizes Store.ListAll as EXACTLY ListAllSessionBeads'
// row-set/order/error semantics, projected via InfoFromPersistedBead. Every
// later WI-6 wave that moves a ListAllSessionBeads caller onto Store.ListAll
// reduces its safety to this test. It is written to fail loudly if ListAll is
// ever reimplemented over a naive Store.List (which silently strands the
// label-lost type-only beads and the label-only repairable beads — the
// documented session_bead_snapshot reconciler-stranding bug).

// listAllCorpus is the full fixture corpus the characterization asserts over.
// The CreatedAt values interleave the two union legs (type leg: canonical,
// type-only, closed; label leg: label-only) so a per-leg concatenation is
// distinguishable from the global re-sort.
func listAllCorpus() []beads.Bead {
	at := func(sec int) time.Time { return time.Date(2026, 3, 1, 0, 0, sec, 0, time.UTC) }
	return []beads.Bead{
		// canonical: type + label — the healthy shape; appears in BOTH legs and
		// must be deduped to exactly one row.
		{
			ID: "s-canonical", Type: BeadType, Status: "open", Title: "canon", Labels: []string{LabelSession},
			CreatedAt: at(1), Metadata: map[string]string{"session_name": "canonical", "state": "active"},
		},
		// label-only repairable: empty Type carrying gc:session — the legacy
		// crash/migration shape. A Store.List(Type=session) scan MISSES it.
		{
			ID: "s-label-only", Type: "", Status: "open", Title: "labelonly", Labels: []string{LabelSession},
			CreatedAt: at(2), Metadata: map[string]string{"session_name": "label-only"},
		},
		// type-only: label lost after a crash/partial write — THE fixture that
		// catches a naive Store.List(Label=gc:session) substitution silently
		// dropping repairable beads (session_bead_snapshot.go stranding bug).
		{
			ID: "s-type-only", Type: BeadType, Status: "open", Title: "typeonly", Labels: nil,
			CreatedAt: at(3), Metadata: map[string]string{"session_name": "type-only", "state": "asleep"},
		},
		// label-carrying non-session bead: has gc:session but a non-session,
		// non-empty Type — surfaced by the label leg, dropped by
		// IsSessionBeadOrRepairable. Must never appear in the result.
		{
			ID: "s-nonsession", Type: "task", Status: "open", Title: "task", Labels: []string{LabelSession},
			CreatedAt: at(4), Metadata: map[string]string{"session_name": "nonsession"},
		},
		// closed canonical: excluded unless IncludeClosed. Carries a raw state so
		// the closed-blanking projection is exercised.
		{
			ID: "s-closed", Type: BeadType, Status: "closed", Title: "closed", Labels: []string{LabelSession},
			CreatedAt: at(5), Metadata: map[string]string{"session_name": "closed", "state": "active"},
		},
	}
}

func newCorpusStore(t *testing.T) (*Store, *beads.MemStore) {
	t.Helper()
	corpus := listAllCorpus()
	mem := beads.NewMemStoreFrom(len(corpus), corpus, nil)
	return NewStore(beads.SessionStore{Store: mem}), mem
}

func beadIDs(bs []beads.Bead) []string {
	ids := make([]string, len(bs))
	for i, b := range bs {
		ids[i] = b.ID
	}
	return ids
}

// assertListAllEquivalent asserts Store.ListAll(opts) is row-for-row identical
// to InfoFromPersistedBead-projecting ListAllSessionBeads(raw, query), and that
// the errors agree. This is the whole game: ListAll must BE ListAllSessionBeads,
// projected.
func assertListAllEquivalent(t *testing.T, front *Store, raw beads.Store, opts ListAllOptions, query beads.ListQuery) {
	t.Helper()
	got, gotErr := front.ListAll(opts)
	wantBeads, wantErr := ListAllSessionBeads(raw, query)

	switch {
	case (gotErr == nil) != (wantErr == nil):
		t.Fatalf("opts=%+v: error presence mismatch got=%v want=%v", opts, gotErr, wantErr)
	case gotErr != nil && gotErr.Error() != wantErr.Error():
		t.Fatalf("opts=%+v: error text got=%q want=%q", opts, gotErr, wantErr)
	}
	if len(got) != len(wantBeads) {
		t.Fatalf("opts=%+v: row count got=%d want=%d\n gotIDs=%v\nwantIDs=%v",
			opts, len(got), len(wantBeads), infoIDs(got), beadIDs(wantBeads))
	}
	for i := range wantBeads {
		wantInfo := infoFromPersistedBead(wantBeads[i])
		if !reflect.DeepEqual(got[i], wantInfo) {
			t.Fatalf("opts=%+v: row %d (%s) projection diverged\n got=%+v\nwant=%+v",
				opts, i, wantBeads[i].ID, got[i], wantInfo)
		}
	}
}

// TestListAllMatchesListAllSessionBeads is THE characterization pin: across the
// full fixture corpus and the matrix of options real callers set (default,
// IncludeClosed, both sort orders, post-union Limit, Live), ListAll's row set,
// order, and errors equal ListAllSessionBeads projected via InfoFromPersistedBead.
func TestListAllMatchesListAllSessionBeads(t *testing.T) {
	front, mem := newCorpusStore(t)

	cases := []struct {
		name  string
		opts  ListAllOptions
		query beads.ListQuery
	}{
		{"default", ListAllOptions{}, beads.ListQuery{}},
		{"include-closed", ListAllOptions{IncludeClosed: true}, beads.ListQuery{IncludeClosed: true}},
		{"sort-asc", ListAllOptions{Sort: beads.SortCreatedAsc}, beads.ListQuery{Sort: beads.SortCreatedAsc}},
		{"sort-desc", ListAllOptions{Sort: beads.SortCreatedDesc}, beads.ListQuery{Sort: beads.SortCreatedDesc}},
		{"limit-post-union", ListAllOptions{Sort: beads.SortCreatedAsc, Limit: 2}, beads.ListQuery{Sort: beads.SortCreatedAsc, Limit: 2}},
		{"include-closed-sorted", ListAllOptions{IncludeClosed: true, Sort: beads.SortCreatedAsc}, beads.ListQuery{IncludeClosed: true, Sort: beads.SortCreatedAsc}},
		{"limit-with-closed", ListAllOptions{IncludeClosed: true, Sort: beads.SortCreatedAsc, Limit: 3}, beads.ListQuery{IncludeClosed: true, Sort: beads.SortCreatedAsc, Limit: 3}},
		{"live", ListAllOptions{Live: true, Sort: beads.SortCreatedAsc}, beads.ListQuery{Live: true, Sort: beads.SortCreatedAsc}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertListAllEquivalent(t, front, mem, tc.opts, tc.query)
		})
	}
}

// reconcileCorpus extends listAllCorpus with a bead carrying a fully-populated
// 9-key circuit-breaker cluster, so the ReconcileSession row's Circuit projection
// is exercised against a non-zero fixture (not just the empty-cluster rows).
func reconcileCorpus() []beads.Bead {
	corpus := listAllCorpus()
	at := time.Date(2026, 3, 1, 0, 0, 6, 0, time.UTC)
	circuit := beads.Bead{
		ID: "s-circuit", Type: BeadType, Status: "open", Title: "circuit", Labels: []string{LabelSession},
		CreatedAt: at, Metadata: map[string]string{
			"session_name":                             "circuit",
			"state":                                    "active",
			SessionCircuitStateMetadataKey:             SessionCircuitStateOpen,
			SessionCircuitRestartsMetadataKey:          `["2026-03-01T00:00:00Z"]`,
			SessionCircuitLastRestartMetadataKey:       "2026-03-01T00:00:01Z",
			SessionCircuitLastProgressMetadataKey:      "2026-03-01T00:00:02Z",
			SessionCircuitLastObservedMetadataKey:      "2026-03-01T00:00:03Z",
			SessionCircuitProgressSignatureMetadataKey: "sig-abc",
			SessionCircuitOpenedAtMetadataKey:          "2026-03-01T00:00:04Z",
			SessionCircuitOpenRestartCountMetadataKey:  "3",
			SessionCircuitResetGenerationMetadataKey:   "2",
		},
	}
	return append(corpus, circuit)
}

// TestListAllForReconcileMatchesListAllSessionBeads is the ReconcileSession row
// oracle: across the same option matrix as ListAll, ListAllForReconcile's row
// set/order/errors equal ListAllSessionBeads, and per row Info ==
// infoFromPersistedBead(b) AND Circuit == CircuitStateFromMetadata(b.Metadata).
// The corpus carries the label-lost type-only bead, the label-only repairable
// bead, closed beads, and a populated 9-key circuit cluster, so it fails loudly
// if the row projection drops a leg, skips the filter/dedupe/sort, or diverges on
// either the Info or the Circuit projection.
func TestListAllForReconcileMatchesListAllSessionBeads(t *testing.T) {
	corpus := reconcileCorpus()
	newFront := func() (*Store, beads.Store) {
		mem := beads.NewMemStoreFrom(len(corpus), corpus, nil)
		return NewStore(beads.SessionStore{Store: mem}), mem
	}

	cases := []struct {
		name  string
		opts  ListAllOptions
		query beads.ListQuery
	}{
		{"default", ListAllOptions{}, beads.ListQuery{}},
		{"include-closed", ListAllOptions{IncludeClosed: true}, beads.ListQuery{IncludeClosed: true}},
		{"sort-asc", ListAllOptions{Sort: beads.SortCreatedAsc}, beads.ListQuery{Sort: beads.SortCreatedAsc}},
		{"sort-desc", ListAllOptions{Sort: beads.SortCreatedDesc}, beads.ListQuery{Sort: beads.SortCreatedDesc}},
		{"limit-post-union", ListAllOptions{Sort: beads.SortCreatedAsc, Limit: 2}, beads.ListQuery{Sort: beads.SortCreatedAsc, Limit: 2}},
		{"include-closed-sorted", ListAllOptions{IncludeClosed: true, Sort: beads.SortCreatedAsc}, beads.ListQuery{IncludeClosed: true, Sort: beads.SortCreatedAsc}},
		{"live", ListAllOptions{Live: true, Sort: beads.SortCreatedAsc}, beads.ListQuery{Live: true, Sort: beads.SortCreatedAsc}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			front, mem := newFront()
			got, gotErr := front.ListAllForReconcile(tc.opts)
			wantBeads, wantErr := ListAllSessionBeads(mem, tc.query)
			switch {
			case (gotErr == nil) != (wantErr == nil):
				t.Fatalf("opts=%+v: error presence mismatch got=%v want=%v", tc.opts, gotErr, wantErr)
			case gotErr != nil && gotErr.Error() != wantErr.Error():
				t.Fatalf("opts=%+v: error text got=%q want=%q", tc.opts, gotErr, wantErr)
			}
			if len(got) != len(wantBeads) {
				t.Fatalf("opts=%+v: row count got=%d want=%d", tc.opts, len(got), len(wantBeads))
			}
			for i := range wantBeads {
				wantInfo := infoFromPersistedBead(wantBeads[i])
				wantCircuit := CircuitStateFromMetadata(wantBeads[i].Metadata)
				if !reflect.DeepEqual(got[i].Info, wantInfo) {
					t.Fatalf("opts=%+v row %d (%s): Info diverged\n got=%+v\nwant=%+v", tc.opts, i, wantBeads[i].ID, got[i].Info, wantInfo)
				}
				if !reflect.DeepEqual(got[i].Circuit, wantCircuit) {
					t.Fatalf("opts=%+v row %d (%s): Circuit diverged\n got=%+v\nwant=%+v", tc.opts, i, wantBeads[i].ID, got[i].Circuit, wantCircuit)
				}
			}
		})
	}
}

// TestListAllForReconcile_CircuitClusterProjected pins that a populated circuit
// cluster survives the row projection non-empty (guarding a mutation that zeroes
// the Circuit field or reads the wrong keys).
func TestListAllForReconcile_CircuitClusterProjected(t *testing.T) {
	corpus := reconcileCorpus()
	front := NewStore(beads.SessionStore{Store: beads.NewMemStoreFrom(len(corpus), corpus, nil)})
	rows, err := front.ListAllForReconcile(ListAllOptions{})
	if err != nil {
		t.Fatalf("ListAllForReconcile: %v", err)
	}
	var found bool
	for _, r := range rows {
		if r.Info.ID != "s-circuit" {
			continue
		}
		found = true
		if r.Circuit.State != SessionCircuitStateOpen || r.Circuit.OpenRestartCount != "3" || r.Circuit.ResetGeneration != "2" {
			t.Fatalf("circuit cluster not projected verbatim: %+v", r.Circuit)
		}
	}
	if !found {
		t.Fatal("s-circuit row missing from ListAllForReconcile output")
	}
}

// TestListAll_GlobalSortInterleavesLegs pins that the merged union is sorted
// globally, not per leg: with SortCreatedAsc the label-leg row (label-only,
// created between the two type-leg rows) must interleave, not trail.
func TestListAll_GlobalSortInterleavesLegs(t *testing.T) {
	front, _ := newCorpusStore(t)
	got, err := front.ListAll(ListAllOptions{Sort: beads.SortCreatedAsc})
	if err != nil {
		t.Fatalf("ListAll: %v", err)
	}
	want := []string{"s-canonical", "s-label-only", "s-type-only"}
	if !reflect.DeepEqual(infoIDs(got), want) {
		t.Fatalf("global sort order = %v, want %v (label-leg row must interleave, not trail)", infoIDs(got), want)
	}
}

// TestListAll_UnionDefeatsStoreListSubstitution is the negative pin: it proves
// the type-only and label-only fixtures WOULD catch a naive Store.List
// substitution. ListAll's union surfaces both; a single-shape scan (label-only
// or type-only) strands one of them.
func TestListAll_UnionDefeatsStoreListSubstitution(t *testing.T) {
	front, mem := newCorpusStore(t)

	got, err := front.ListAll(ListAllOptions{})
	if err != nil {
		t.Fatalf("ListAll: %v", err)
	}
	inResult := map[string]bool{}
	for _, in := range got {
		inResult[in.ID] = true
	}
	if !inResult["s-type-only"] {
		t.Error("ListAll dropped the type-only (label-lost) bead — a Store.List(Label) substitution would have this bug")
	}
	if !inResult["s-label-only"] {
		t.Error("ListAll dropped the label-only repairable bead — a Store.List(Type) substitution would have this bug")
	}
	if inResult["s-nonsession"] {
		t.Error("ListAll surfaced the label-carrying non-session bead — IsSessionBeadOrRepairable filter regressed")
	}

	// Demonstrate the two single-shape scans each miss a repairable bead, so the
	// fixtures above are load-bearing rather than incidental.
	labelScan, err := mem.List(beads.ListQuery{Label: LabelSession})
	if err != nil {
		t.Fatalf("label scan: %v", err)
	}
	if idSet(beadIDs(labelScan))["s-type-only"] {
		t.Fatal("fixture invalid: a Label scan unexpectedly returned the type-only bead")
	}
	typeScan, err := mem.List(beads.ListQuery{Type: BeadType})
	if err != nil {
		t.Fatalf("type scan: %v", err)
	}
	if idSet(beadIDs(typeScan))["s-label-only"] {
		t.Fatal("fixture invalid: a Type scan unexpectedly returned the label-only bead")
	}
}

func idSet(ids []string) map[string]bool {
	out := make(map[string]bool, len(ids))
	for _, id := range ids {
		out[id] = true
	}
	return out
}

// legFaultStore injects a per-leg error onto the type or label union query so
// the partial-result fold-through and hard-error short-circuit can be
// characterized.
type legFaultStore struct {
	beads.Store
	typeErr  error
	labelErr error
}

func (s *legFaultStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	rows, err := s.Store.List(q)
	if err != nil {
		return rows, err
	}
	if q.Type == BeadType {
		return rows, s.typeErr
	}
	if q.Label == LabelSession {
		return rows, s.labelErr
	}
	return rows, nil
}

// TestListAll_PartialAndHardErrors characterizes the error paths against
// ListAllSessionBeads on the same fault-injecting store: partial results fold
// their partial rows AND surface the PartialResultError; a hard error on either
// leg short-circuits to nil rows with the leg-naming wrapped error.
func TestListAll_PartialAndHardErrors(t *testing.T) {
	corpus := listAllCorpus()

	cases := []struct {
		name        string
		typeErr     error
		labelErr    error
		wantPartial bool
		wantHardHas string
	}{
		{
			name:        "partial-on-type-leg",
			typeErr:     &beads.PartialResultError{Op: "bd list", Err: errors.New("one row corrupt")},
			wantPartial: true,
		},
		{
			name:        "partial-on-label-leg",
			labelErr:    &beads.PartialResultError{Op: "bd list", Err: errors.New("one row corrupt")},
			wantPartial: true,
		},
		{
			name:        "hard-on-type-leg",
			typeErr:     errors.New("boom"),
			wantHardHas: "listing session beads by type",
		},
		{
			name:        "hard-on-label-leg",
			labelErr:    errors.New("boom"),
			wantHardHas: "listing session beads by label",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fault := &legFaultStore{
				Store:    beads.NewMemStoreFrom(len(corpus), corpus, nil),
				typeErr:  tc.typeErr,
				labelErr: tc.labelErr,
			}
			front := NewStore(beads.SessionStore{Store: fault})

			// Equivalence vs the raw helper on the SAME fault store: this is the
			// pin that ListAll IS ListAllSessionBeads projected, even on the error
			// paths.
			assertListAllEquivalent(t, front, fault, ListAllOptions{Sort: beads.SortCreatedAsc}, beads.ListQuery{Sort: beads.SortCreatedAsc})

			got, err := front.ListAll(ListAllOptions{Sort: beads.SortCreatedAsc})
			switch {
			case tc.wantPartial:
				if !beads.IsPartialResult(err) {
					t.Fatalf("want PartialResultError, got %v", err)
				}
				if len(got) == 0 {
					t.Fatal("partial result must still fold the surviving rows, got none")
				}
			case tc.wantHardHas != "":
				if err == nil || !strings.Contains(err.Error(), tc.wantHardHas) {
					t.Fatalf("hard error = %v, want text containing %q", err, tc.wantHardHas)
				}
				if got != nil {
					t.Fatalf("hard error must return nil rows, got %d", len(got))
				}
			}
		})
	}
}

// recordingCacheStore records List and CachedList calls and serves CachedList
// from a configurable per-leg script, so the read-tier pins can assert exactly
// which tier ListAll reached. CachedList serves rows via the embedded store's
// List (NOT the recorded override), so a cache hit leaves the ListAll-driven
// listCalls count at zero.
type recordingCacheStore struct {
	beads.Store
	listCalls     []beads.ListQuery
	cachedCalls   []beads.ListQuery
	cachedTypeOK  bool
	cachedLabelOK bool
}

func (s *recordingCacheStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	s.listCalls = append(s.listCalls, q)
	return s.Store.List(q)
}

func (s *recordingCacheStore) CachedList(q beads.ListQuery) ([]beads.Bead, bool) {
	s.cachedCalls = append(s.cachedCalls, q)
	hit := (q.Type == BeadType && s.cachedTypeOK) || (q.Label == LabelSession && s.cachedLabelOK)
	if !hit {
		return nil, false
	}
	rows, err := s.Store.List(q)
	if err != nil {
		return nil, false
	}
	return rows, true
}

// TestListAll_ReadTierRouting pins the three read tiers with a counting store:
// CacheFirst peeks the cache and skips the store when both legs hit; the default
// tier never peeks the cache; Live reaches the store with query.Live==true on
// both legs and never peeks the cache.
func TestListAll_ReadTierRouting(t *testing.T) {
	corpus := listAllCorpus()
	newFront := func(typeOK, labelOK bool) (*Store, *recordingCacheStore) {
		rec := &recordingCacheStore{
			Store:         beads.NewMemStoreFrom(len(corpus), corpus, nil),
			cachedTypeOK:  typeOK,
			cachedLabelOK: labelOK,
		}
		return NewStore(beads.SessionStore{Store: rec}), rec
	}

	t.Run("cache-first-both-hit-skips-store", func(t *testing.T) {
		front, rec := newFront(true, true)
		if _, err := front.ListAll(ListAllOptions{CacheFirst: true, Sort: beads.SortCreatedDesc}); err != nil {
			t.Fatalf("ListAll: %v", err)
		}
		if len(rec.listCalls) != 0 {
			t.Errorf("CacheFirst with both legs cached must not call store.List, got %d calls", len(rec.listCalls))
		}
		if len(rec.cachedCalls) != 2 {
			t.Errorf("CacheFirst must peek CachedList for both legs, got %d calls", len(rec.cachedCalls))
		}
	})

	t.Run("cache-first-miss-falls-through", func(t *testing.T) {
		front, rec := newFront(true, false) // label leg misses
		if _, err := front.ListAll(ListAllOptions{CacheFirst: true, Sort: beads.SortCreatedDesc}); err != nil {
			t.Fatalf("ListAll: %v", err)
		}
		if len(rec.cachedCalls) != 2 {
			t.Errorf("CacheFirst must attempt both cache legs before falling through, got %d", len(rec.cachedCalls))
		}
		if len(rec.listCalls) != 2 {
			t.Errorf("cache miss must fall through to the direct 2-leg union, got %d store.List calls", len(rec.listCalls))
		}
	})

	t.Run("default-never-peeks-cache", func(t *testing.T) {
		front, rec := newFront(true, true) // would hit if asked
		if _, err := front.ListAll(ListAllOptions{}); err != nil {
			t.Fatalf("ListAll: %v", err)
		}
		if len(rec.cachedCalls) != 0 {
			t.Errorf("default tier must never peek CachedList, got %d", len(rec.cachedCalls))
		}
		if len(rec.listCalls) != 2 {
			t.Errorf("default tier must issue the 2-leg direct union, got %d", len(rec.listCalls))
		}
	})

	t.Run("live-reaches-store-and-skips-cache", func(t *testing.T) {
		front, rec := newFront(true, true)
		if _, err := front.ListAll(ListAllOptions{Live: true}); err != nil {
			t.Fatalf("ListAll: %v", err)
		}
		if len(rec.cachedCalls) != 0 {
			t.Errorf("Live tier must never peek CachedList, got %d", len(rec.cachedCalls))
		}
		if len(rec.listCalls) != 2 {
			t.Fatalf("Live tier must issue the 2-leg direct union, got %d", len(rec.listCalls))
		}
		for i, q := range rec.listCalls {
			if !q.Live {
				t.Errorf("Live leg %d reached the store with Live=false", i)
			}
		}
	})

	t.Run("live-wins-over-cache-first", func(t *testing.T) {
		front, rec := newFront(true, true)
		if _, err := front.ListAll(ListAllOptions{CacheFirst: true, Live: true}); err != nil {
			t.Fatalf("ListAll: %v", err)
		}
		if len(rec.cachedCalls) != 0 {
			t.Errorf("Live must win over CacheFirst (no cache peek), got %d cache calls", len(rec.cachedCalls))
		}
		if len(rec.listCalls) != 2 {
			t.Errorf("Live must reach the store, got %d store.List calls", len(rec.listCalls))
		}
	})

	// The cache-first union body is a SECOND copy of the union (dedupe/filter/
	// sort/limit). Call-count pins alone let it drift (drop the dedupe, filter, or
	// apply Limit per leg and every count check still passes). Pin the cache tier's
	// OUTPUT ROWS against the default tier — which TestListAllMatchesListAllSessionBeads
	// already pins to ListAllSessionBeads — so the cache union is row-equivalent,
	// not just count-equivalent. The corpus carries the load-bearing shapes:
	// s-canonical in BOTH legs (dedupe), s-nonsession (filter), interleaved
	// CreatedAt (global sort).
	t.Run("cache-first-row-equivalent-to-default", func(t *testing.T) {
		combos := []ListAllOptions{
			{Sort: beads.SortCreatedDesc},
			{Sort: beads.SortCreatedAsc},
			{Sort: beads.SortCreatedAsc, Limit: 2}, // Limit must be post-union, not per-leg
			{Sort: beads.SortCreatedDesc, Limit: 1},
			{Sort: beads.SortCreatedAsc, IncludeClosed: true},
		}
		for _, base := range combos {
			front, _ := newFront(true, true)
			cacheOpts := base
			cacheOpts.CacheFirst = true
			cacheRows, err := front.ListAll(cacheOpts)
			if err != nil {
				t.Fatalf("cache-first %+v: %v", cacheOpts, err)
			}
			defaultRows, err := front.ListAll(base)
			if err != nil {
				t.Fatalf("default %+v: %v", base, err)
			}
			if !reflect.DeepEqual(cacheRows, defaultRows) {
				t.Errorf("%+v: cache-first rows diverged from the default tier\n cache=%v\n  def=%v",
					base, infoIDs(cacheRows), infoIDs(defaultRows))
			}
		}
	})

	// SortDefault CacheFirst falls through (warm-cache order is nondeterministic
	// with no sort); the cache is never peeked and the store serves the 2-leg union.
	t.Run("cache-first-sort-default-falls-through", func(t *testing.T) {
		front, rec := newFront(true, true)
		if _, err := front.ListAll(ListAllOptions{CacheFirst: true}); err != nil {
			t.Fatalf("ListAll: %v", err)
		}
		if len(rec.cachedCalls) != 0 {
			t.Errorf("SortDefault CacheFirst must not peek the cache, got %d cache calls", len(rec.cachedCalls))
		}
		if len(rec.listCalls) != 2 {
			t.Errorf("SortDefault CacheFirst must fall through to the direct union, got %d store.List calls", len(rec.listCalls))
		}
	})
}

// TestStoreGetPersistedResponse pins GetPersistedResponse as the single-fetch
// (Info, PersistedResponse) twin: both projections equal the from-bead codecs,
// and a non-session or absent id is ErrSessionNotFound.
func TestStoreGetPersistedResponse(t *testing.T) {
	b := sessionBeadFixture("s-gpr-1", "open", map[string]string{
		"__title":      "Persisted",
		"template":     "polecat",
		"state":        "asleep",
		"alias":        "pc-1",
		"agent_name":   "polecat-7",
		"session_name": "s-gpr-1",
	})
	front := NewStore(seedSessionStore(t, b))

	info, pr, err := front.GetPersistedResponse("s-gpr-1")
	if err != nil {
		t.Fatalf("GetPersistedResponse: %v", err)
	}
	if wantInfo := infoFromPersistedBead(b); !reflect.DeepEqual(info, wantInfo) {
		t.Fatalf("Info mismatch\n got=%+v\nwant=%+v", info, wantInfo)
	}
	wantPR := PersistedResponseFromBead(b)
	if pr.Status != wantPR.Status || !reflect.DeepEqual(pr.Metadata, wantPR.Metadata) {
		t.Fatalf("PersistedResponse mismatch\n got=%+v\nwant=%+v", pr, wantPR)
	}

	// Absent id: error equivalence with Get (both route through validatedBead and
	// wrap the store's not-found error — ErrSessionNotFound is reserved for a
	// present-but-non-session bead, exactly as Get behaves).
	_, _, gprErr := front.GetPersistedResponse("missing")
	_, getErr := front.Get("missing")
	if gprErr == nil || getErr == nil || gprErr.Error() != getErr.Error() {
		t.Fatalf("GetPersistedResponse(missing)=%v must match Get(missing)=%v", gprErr, getErr)
	}

	// Present-but-non-session bead: ErrSessionNotFound (parity with Get).
	task := beads.Bead{ID: "t-1", Type: "task", Status: "open", Labels: []string{"other"}}
	taskFront := NewStore(beads.SessionStore{Store: beads.NewMemStoreFrom(1, []beads.Bead{task}, nil)})
	if _, _, err := taskFront.GetPersistedResponse("t-1"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("GetPersistedResponse(task) = %v, want ErrSessionNotFound", err)
	}
}

// TestHasOpenSessionNamed pins the Live-tier existence probe: an open session
// bead with the runtime name reports true, a closed-only match reports false, an
// unmatched name reports false (which also proves the metadata filter is applied
// — otherwise the open beads would leak a false positive), and the underlying
// scan runs with Live=true and the session_name metadata filter.
func TestHasOpenSessionNamed(t *testing.T) {
	openBead := sessionBeadFixture("s-open", "open", map[string]string{"session_name": "worker-1"})
	otherOpen := sessionBeadFixture("s-open-2", "open", map[string]string{"session_name": "worker-3"})
	closedBead := sessionBeadFixture("s-closed", "closed", map[string]string{"session_name": "worker-2"})

	rec := &recordingCacheStore{Store: beads.NewMemStoreFrom(3, []beads.Bead{openBead, otherOpen, closedBead}, nil)}
	front := NewStore(beads.SessionStore{Store: rec})

	if ok, err := front.HasOpenSessionNamed("worker-1"); err != nil || !ok {
		t.Fatalf("HasOpenSessionNamed(worker-1) = (%v, %v), want (true, nil)", ok, err)
	}
	if ok, err := front.HasOpenSessionNamed("worker-2"); err != nil || ok {
		t.Fatalf("HasOpenSessionNamed(worker-2, closed-only) = (%v, %v), want (false, nil)", ok, err)
	}
	if ok, err := front.HasOpenSessionNamed("nonexistent"); err != nil || ok {
		t.Fatalf("HasOpenSessionNamed(nonexistent) = (%v, %v), want (false, nil)", ok, err)
	}

	// The probe must be Live and session_name-filtered on every leg.
	if len(rec.listCalls) == 0 {
		t.Fatal("HasOpenSessionNamed issued no store scan")
	}
	for i, q := range rec.listCalls {
		if !q.Live {
			t.Errorf("probe leg %d ran with Live=false", i)
		}
		if q.Metadata["session_name"] == "" {
			t.Errorf("probe leg %d dropped the session_name metadata filter: %+v", i, q.Metadata)
		}
	}
}
