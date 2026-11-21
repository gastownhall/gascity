package readywork

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// narrowingStore is a leg whose backend APPLIES the pushed predicates, as a
// pushdown-capable store does: it honors ReadyQuery's advisory narrowing
// predicates exactly, and it cuts a bounded read in order after its readiness
// filter (beads.OrderedReadyReader), the way NativeDoltStore does. Its plain
// Ready keeps MemStore's own Limit semantics (insertion order), which is exactly
// the cut a bounded read must never be answered with.
type narrowingStore struct {
	*beads.MemStore
	mu      sync.Mutex
	ready   []beads.ReadyQuery
	ordered []orderedCall
	lists   []beads.ListQuery
}

type orderedCall struct {
	query beads.ReadyQuery
	order beads.ReadyOrder
}

func (s *narrowingStore) Ready(query ...beads.ReadyQuery) ([]beads.Bead, error) {
	q := beads.ReadyQuery{}
	if len(query) > 0 {
		q = query[0]
	}
	s.mu.Lock()
	s.ready = append(s.ready, q)
	s.mu.Unlock()
	return s.narrowed(q)
}

func (s *narrowingStore) narrowed(q beads.ReadyQuery) ([]beads.Bead, error) {
	rows, err := s.MemStore.Ready(beads.ReadyQuery{Assignee: q.Assignee, TierMode: q.TierMode})
	if err != nil {
		return nil, err
	}
	out := rows[:0]
	for _, b := range rows {
		if q.Unassigned && b.Assignee != "" {
			continue
		}
		keep := true
		for k, v := range q.MetadataEquals {
			if got, ok := b.Metadata[k]; !ok || got != v {
				keep = false
				break
			}
		}
		if keep {
			out = append(out, b)
		}
	}
	if q.Limit > 0 && len(out) > q.Limit {
		out = out[:q.Limit]
	}
	return out, nil
}

func (s *narrowingStore) ReadyOrdered(q beads.ReadyQuery, order beads.ReadyOrder) ([]beads.Bead, bool, error) {
	s.mu.Lock()
	s.ordered = append(s.ordered, orderedCall{query: q, order: order})
	s.mu.Unlock()
	limit := q.Limit
	q.Limit = 0
	rows, err := s.narrowed(q)
	if err != nil {
		return nil, false, err
	}
	switch order {
	case beads.ReadyOrderCanonical:
		beads.SortBeadsReadyOrder(rows)
	case beads.ReadyOrderOldest:
		beads.SortBeads(rows, beads.SortCreatedAsc)
	default:
		return rows, false, nil
	}
	if limit > 0 && len(rows) > limit {
		rows = rows[:limit]
	}
	return rows, true, nil
}

// List records what the leg is asked and answers it as MemStore does, which
// applies ListQuery.Metadata and Assignee exactly.
func (s *narrowingStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	s.mu.Lock()
	s.lists = append(s.lists, q)
	s.mu.Unlock()
	return s.MemStore.List(q)
}

func (s *narrowingStore) listQueries() []beads.ListQuery {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.lists)
}

func (s *narrowingStore) readyQueries() []beads.ReadyQuery {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.ready)
}

func (s *narrowingStore) orderedCalls() []orderedCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.ordered)
}

// ignoringStore is a leg whose backend applies NONE of what the reader pushes:
// no advisory predicate, no assignee, no bound. It is the unpushed reader's
// reference answer, which every pushed answer must equal.
type ignoringStore struct {
	*beads.MemStore
}

func (s ignoringStore) Ready(query ...beads.ReadyQuery) ([]beads.Bead, error) {
	q := beads.ReadyQuery{}
	if len(query) > 0 {
		q = query[0]
	}
	return s.MemStore.Ready(beads.ReadyQuery{TierMode: q.TierMode})
}

func (s ignoringStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	query.Assignee = ""
	query.Metadata = nil
	return s.MemStore.List(query)
}

func newNarrowingStore(rows ...beads.Bead) *narrowingStore {
	return &narrowingStore{MemStore: beads.NewMemStoreFrom(0, rows, nil)}
}

// TestReadPushesTheSelectivePredicatesIntoEveryLeg pins what each leg is ASKED:
// the assignee, --unassigned and every plain-string key=value pair travel to
// the store, so a claim tier reads the rows routed or assigned to it rather than
// the city's whole ready set.
func TestReadPushesTheSelectivePredicatesIntoEveryLeg(t *testing.T) {
	city, rig := newNarrowingStore(), newNarrowingStore()
	legs := []Leg{testLeg("city", city), testLeg("rig r", rig)}
	_, err := Read(legs, Query{
		Assignee:       "  worker-1 ",
		Unassigned:     true,
		MetadataFields: []string{"gc.routed_to=gascity/worker", "gc.kind=workflow"},
	})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	for _, store := range []*narrowingStore{city, rig} {
		got := store.readyQueries()
		if len(got) != 1 {
			t.Fatalf("leg asked %d ready queries, want 1: %+v", len(got), got)
		}
		want := beads.ReadyQuery{
			Assignee:       "worker-1",
			TierMode:       beads.FederatedReadTier,
			Unassigned:     true,
			MetadataEquals: map[string]string{"gc.routed_to": "gascity/worker", "gc.kind": "workflow"},
		}
		if !reflect.DeepEqual(got[0], want) {
			t.Fatalf("leg asked %+v, want %+v", got[0], want)
		}
	}
}

// statusLegRows seeds one leg with a row per status and metadata shape: one
// carrying gc.root_bead_id=R, one carrying another root, one carrying none. The
// in_progress rows are assigned to w1 and w2 in turn.
func statusLegRows(prefix string) []beads.Bead {
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	var rows []beads.Bead
	for si, status := range []string{StatusOpen, StatusInProgress, StatusBlocked, StatusClosed} {
		for mi, root := range []string{"R", "other", ""} {
			for ai, assignee := range []string{"w1", "w2"} {
				id := fmt.Sprintf("%s-%s-%d-%s", prefix, status, mi, assignee)
				rows = append(rows, readyRow(id, (si+mi+ai)%4, base.Add(time.Duration(len(rows))*time.Minute), func(b *beads.Bead) {
					b.Status = status
					b.Assignee = assignee
					if root != "" {
						b.Metadata = beads.StringMap{"gc.root_bead_id": root}
					}
				}))
			}
		}
	}
	return rows
}

// TestReadyStatusLegsFilterMetadataInTheStore pins what a --status arm asks each
// leg for (ga-8g39ur): a plain-string key=value --metadata-field travels to the
// store as ListQuery.Metadata beside the arm's own query, so a store with a
// large tier in that status is not read whole and filtered here afterwards. The
// answer is the rows of that status carrying the pair, from every leg.
func TestReadyStatusLegsFilterMetadataInTheStore(t *testing.T) {
	root := map[string]string{"gc.root_bead_id": "R"}
	for _, tc := range []struct {
		name  string
		query Query
		want  beads.ListQuery
		ids   []string
	}{
		{
			name:  "closed",
			query: Query{Status: StatusClosed},
			want:  beads.ListQuery{Status: StatusClosed, TierMode: beads.FederatedReadTier, Metadata: root},
			ids:   []string{"c-closed-0-w1", "c-closed-0-w2", "r-closed-0-w1", "r-closed-0-w2"},
		},
		{
			name:  "blocked",
			query: Query{Status: StatusBlocked},
			want:  beads.ListQuery{Status: StatusBlocked, TierMode: beads.FederatedReadTier, Metadata: root},
			ids:   []string{"c-blocked-0-w1", "c-blocked-0-w2", "r-blocked-0-w1", "r-blocked-0-w2"},
		},
		{
			name:  "in_progress",
			query: Query{Status: StatusInProgress},
			want:  beads.ListQuery{Status: StatusInProgress, TierMode: beads.FederatedReadTier, Metadata: root},
			ids:   []string{"c-in_progress-0-w1", "c-in_progress-0-w2", "r-in_progress-0-w1", "r-in_progress-0-w2"},
		},
		{
			name:  "in_progress with an assignee",
			query: Query{Status: StatusInProgress, Assignee: "w1"},
			want:  beads.ListQuery{Status: StatusInProgress, Assignee: "w1", TierMode: beads.FederatedReadTier, Metadata: root},
			ids:   []string{"c-in_progress-0-w1", "r-in_progress-0-w1"},
		},
		{
			// Gas City's open is every non-closed row that maps to open, so the
			// open leg scans with no status and the pair beside it.
			name:  "open",
			query: Query{Status: StatusOpen},
			want:  beads.ListQuery{AllowScan: true, TierMode: beads.FederatedReadTier, Metadata: root},
			ids:   []string{"c-open-0-w1", "c-open-0-w2", "r-open-0-w1", "r-open-0-w2"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			city, rig := newNarrowingStore(statusLegRows("c")...), newNarrowingStore(statusLegRows("r")...)
			legs := []Leg{testLeg("city", city), testLeg("rig r", rig)}
			q := tc.query
			q.MetadataFields = []string{"gc.root_bead_id=R"}
			got := readIDs(t, legs, q)
			plain := []Leg{
				testLeg("city", ignoringStore{beads.NewMemStoreFrom(0, statusLegRows("c"), nil)}),
				testLeg("rig r", ignoringStore{beads.NewMemStoreFrom(0, statusLegRows("r"), nil)}),
			}
			if want := readIDs(t, plain, q); !slices.Equal(got, want) {
				t.Fatalf("pushed answer = %v, want the unpushed %v", got, want)
			}
			slices.Sort(got)
			if !slices.Equal(got, tc.ids) {
				t.Fatalf("answer = %v, want %v", got, tc.ids)
			}
			for _, store := range []*narrowingStore{city, rig} {
				lists := store.listQueries()
				if len(lists) == 0 {
					t.Fatal("leg was asked no List query")
				}
				// The live handle every leg is read through stamps Live on the
				// query; the arm's own question is the rest of it.
				got := lists[0]
				got.Live = false
				if !reflect.DeepEqual(got, tc.want) {
					t.Fatalf("leg's first List query = %+v, want %+v", got, tc.want)
				}
			}
		})
	}
}

// TestReadClosedStatusAsksBdListForTheMetadataField pins the invocation that
// timed out in the field (ga-8g39ur): on a bd-backed leg, `gc ready --status
// closed --metadata-field gc.root_bead_id=R` asks bd list for the pair, rather
// than listing the whole closed tier.
func TestReadClosedStatusAsksBdListForTheMetadataField(t *testing.T) {
	var mu sync.Mutex
	var calls [][]string
	runner := func(_, _ string, args ...string) ([]byte, error) {
		mu.Lock()
		calls = append(calls, slices.Clone(args))
		mu.Unlock()
		return []byte("[]"), nil
	}
	leg := testLeg("city", beads.NewBdStore("/city", runner))
	if _, err := Read([]Leg{leg}, Query{Status: StatusClosed, MetadataFields: []string{"gc.root_bead_id=R"}}); err != nil {
		t.Fatalf("Read: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	var lists [][]string
	for _, args := range calls {
		if len(args) > 0 && args[0] == "list" {
			lists = append(lists, args)
		}
	}
	if len(lists) == 0 {
		t.Fatalf("no bd list call among %q", calls)
	}
	for _, args := range lists {
		if !slices.Contains(args, "--status=closed") || !slices.Contains(args, "--all") {
			t.Fatalf("bd list %q does not read the closed tier", args)
		}
		i := slices.Index(args, "--metadata-field")
		if i < 0 || i+1 >= len(args) || args[i+1] != "gc.root_bead_id=R" {
			t.Fatalf("bd list %q does not carry --metadata-field gc.root_bead_id=R", args)
		}
	}
}

// TestReadNeverPushesAPredicateABackendCouldCompareDifferently pins the other
// half: a predicate whose backend comparison is not provably the Go comparison
// stays Go-side, so no store can drop a row the Go filter keeps.
//
// The --status arms' list legs push less than the ready arm does: an empty
// value ("key=" is present-and-empty in Go, which a backend's = ” or LIKE
// misses for a stored JSON null) and a value whose JSON string form is not the
// value between quotes (DoltLite's LIKE prefilter matches one encoding of it)
// stay Go-side there. The ready arm's query for those forms is unchanged.
func TestReadNeverPushesAPredicateABackendCouldCompareDifferently(t *testing.T) {
	for _, tc := range []struct {
		name  string
		query Query
		// readyPushes is what the ready arm pushes for this form.
		readyPushes map[string]string
	}{
		{name: "bare key", query: Query{MetadataFields: []string{"gc.routed_to"}}},
		{name: "JSON boolean value", query: Query{MetadataFields: []string{"gc.flag=true"}}},
		{name: "JSON number value", query: Query{MetadataFields: []string{"gc.count=42"}}},
		{name: "JSON null value", query: Query{MetadataFields: []string{"gc.x=null"}}},
		{name: "JSON object value", query: Query{MetadataFields: []string{`gc.x={"a":1}`}}},
		{name: "key outside bd's grammar", query: Query{MetadataFields: []string{"gc route=x"}}},
		{name: "excluded type", query: Query{ExcludeTypes: []string{"epic"}}},
		{name: "excluded label", query: Query{ExcludeLabels: []string{"hold:mayor"}}},
		{name: "empty value", query: Query{MetadataFields: []string{"gc.x="}}, readyPushes: map[string]string{"gc.x": ""}},
		{name: "value needs JSON escaping", query: Query{MetadataFields: []string{"gc.x=a<b"}}, readyPushes: map[string]string{"gc.x": "a<b"}},
		{name: "value with a quote", query: Query{MetadataFields: []string{`gc.x=say "hi"`}}, readyPushes: map[string]string{"gc.x": `say "hi"`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newNarrowingStore()
			if _, err := Read([]Leg{testLeg("city", store)}, tc.query); err != nil {
				t.Fatalf("Read: %v", err)
			}
			got := store.readyQueries()
			if len(got) != 1 || got[0].Unassigned || got[0].Assignee != "" {
				t.Fatalf("leg asked %+v; this predicate must stay Go-side", got)
			}
			if len(got[0].MetadataEquals) != 0 || len(tc.readyPushes) != 0 {
				if !reflect.DeepEqual(got[0].MetadataEquals, tc.readyPushes) {
					t.Fatalf("ready leg asked MetadataEquals %v, want %v", got[0].MetadataEquals, tc.readyPushes)
				}
			}
			for _, status := range []string{StatusOpen, StatusInProgress, StatusBlocked, StatusClosed} {
				q := tc.query
				q.Status = status
				rows := metadataFormRows()
				store := newNarrowingStore(rows...)
				gotIDs := readIDs(t, []Leg{testLeg("city", store)}, q)
				for _, lq := range store.listQueries() {
					if lq.Metadata != nil {
						t.Fatalf("--status %s leg asked %+v; this pair must stay Go-side", status, lq)
					}
				}
				wantIDs := readIDs(t, []Leg{testLeg("city", ignoringStore{beads.NewMemStoreFrom(0, rows, nil)})}, q)
				if !slices.Equal(gotIDs, wantIDs) {
					t.Fatalf("--status %s answer = %v, want the unpushed %v", status, gotIDs, wantIDs)
				}
			}
		})
	}
	t.Run("empty value keeps only the present-empty bead", func(t *testing.T) {
		for _, status := range []string{StatusOpen, StatusInProgress, StatusBlocked, StatusClosed} {
			got := readIDs(t, []Leg{testLeg("city", newNarrowingStore(metadataFormRows()...))}, Query{Status: status, MetadataFields: []string{"gc.x="}})
			if want := []string{status + "-empty"}; !slices.Equal(got, want) {
				t.Fatalf("--status %s gc.x= answer = %v, want %v", status, got, want)
			}
		}
	})
}

// metadataFormRows seeds, in every status, one row per form gc.x can be stored
// in: present and empty, missing, each JSON literal, and values whose JSON
// string form needs escaping.
func metadataFormRows() []beads.Bead {
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	forms := []struct {
		name  string
		value *string
	}{
		{name: "empty", value: new("")},
		{name: "missing"},
		{name: "true", value: new("true")},
		{name: "42", value: new("42")},
		{name: "null", value: new("null")},
		{name: "object", value: new(`{"a":1}`)},
		{name: "angle", value: new("a<b")},
		{name: "quote", value: new(`say "hi"`)},
	}
	var rows []beads.Bead
	for _, status := range []string{StatusOpen, StatusInProgress, StatusBlocked, StatusClosed} {
		for _, form := range forms {
			rows = append(rows, readyRow(status+"-"+form.name, 1, base.Add(time.Duration(len(rows))*time.Minute), func(b *beads.Bead) {
				b.Status = status
				b.Metadata = beads.StringMap{"gc.routed_to": "pool/a", "gc.flag": "true", "gc.count": "42", "gc route": "x"}
				if form.value != nil {
					b.Metadata["gc.x"] = *form.value
				}
			}))
		}
	}
	return rows
}

// TestReadPushesABoundedOrderedReadAndKeepsTheTopN pins the limit pushdown: a
// store that can cut in order is asked for Limit+boundedSlack rows in the
// query's order, and the merged answer is still the city's top N.
func TestReadPushesABoundedOrderedReadAndKeepsTheTopN(t *testing.T) {
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	var cityRows, rigRows []beads.Bead
	for i := range 60 {
		cityRows = append(cityRows, readyRow(fmt.Sprintf("c-%02d", i), i%4, base.Add(time.Duration(i)*time.Minute)))
		rigRows = append(rigRows, readyRow(fmt.Sprintf("r-%02d", i), (i+1)%4, base.Add(time.Duration(i)*time.Minute+time.Second)))
	}
	for _, sortOrder := range []string{"", SortOldest} {
		t.Run("sort="+sortOrder, func(t *testing.T) {
			city, rig := newNarrowingStore(cityRows...), newNarrowingStore(rigRows...)
			legs := []Leg{testLeg("city", city), testLeg("rig r", rig)}
			plain := []Leg{testLeg("city", beads.NewMemStoreFrom(0, cityRows, nil)), testLeg("rig r", beads.NewMemStoreFrom(0, rigRows, nil))}
			q := Query{Limit: 5, Sort: sortOrder}
			got, want := readIDs(t, legs, q), readIDs(t, plain, q)
			if !slices.Equal(got, want) {
				t.Fatalf("bounded answer = %v, want the unbounded top 5 %v", got, want)
			}
			wantOrder := beads.ReadyOrderCanonical
			if sortOrder == SortOldest {
				wantOrder = beads.ReadyOrderOldest
			}
			for _, store := range []*narrowingStore{city, rig} {
				calls := store.orderedCalls()
				if len(calls) != 1 || calls[0].query.Limit != 5+boundedSlack || calls[0].order != wantOrder {
					t.Fatalf("leg asked %+v, want one ordered read of %d rows in order %v", calls, 5+boundedSlack, wantOrder)
				}
				if len(store.readyQueries()) != 0 {
					t.Fatalf("leg also read its whole ready set: %+v", store.readyQueries())
				}
			}
		})
	}
}

// TestReadNeverPushesTheLimitUnderNewest: no store cuts in created_at
// descending order, so --sort newest reads every leg whole.
func TestReadNeverPushesTheLimitUnderNewest(t *testing.T) {
	store := newNarrowingStore(readyRow("c-1", 2, time.Now().UTC()))
	if _, err := Read([]Leg{testLeg("city", store)}, Query{Limit: 1, Sort: SortNewest}); err != nil {
		t.Fatalf("Read: %v", err)
	}
	if calls := store.orderedCalls(); len(calls) != 0 {
		t.Fatalf("--sort newest pushed a bounded read: %+v", calls)
	}
	if got := store.readyQueries(); len(got) != 1 || got[0].Limit != 0 {
		t.Fatalf("--sort newest asked %+v, want one unbounded read", got)
	}
}

// TestReadBoundedLegIsAskedAgainWhenGoOnlyPredicatesThinIt pins the refill: a
// predicate that stays Go-side can drop most of a leg's bounded rows, and the
// leg is then asked for more rather than leaving the answer short.
func TestReadBoundedLegIsAskedAgainWhenGoOnlyPredicatesThinIt(t *testing.T) {
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	var rows []beads.Bead
	for i := range 200 {
		held := i < 150
		rows = append(rows, readyRow(fmt.Sprintf("c-%03d", i), 1, base.Add(time.Duration(i)*time.Second), func(b *beads.Bead) {
			if held {
				b.Labels = []string{"hold:mayor"}
			}
		}))
	}
	store := newNarrowingStore(rows...)
	q := Query{Limit: 5, ExcludeLabels: []string{"hold:mayor"}}
	got := readIDs(t, []Leg{testLeg("city", store)}, q)
	want := readIDs(t, []Leg{testLeg("city", beads.NewMemStoreFrom(0, rows, nil))}, q)
	if !slices.Equal(got, want) || len(got) != 5 {
		t.Fatalf("bounded answer = %v, want the unbounded %v", got, want)
	}
	calls := store.orderedCalls()
	if len(calls) < 2 {
		t.Fatalf("leg asked %d bounded reads, want a refill after the Go-only predicate thinned the first", len(calls))
	}
	for i := 1; i < len(calls); i++ {
		if calls[i].query.Limit <= calls[i-1].query.Limit {
			t.Fatalf("refill limits %v do not grow", calls)
		}
	}
}

// cancellingOrderedStore ends its read's context as each bounded read returns.
type cancellingOrderedStore struct {
	*narrowingStore
	cancel context.CancelFunc
}

func (s cancellingOrderedStore) ReadyOrdered(q beads.ReadyQuery, order beads.ReadyOrder) ([]beads.Bead, bool, error) {
	defer s.cancel()
	return s.narrowingStore.ReadyOrdered(q, order)
}

// TestReadBoundedLegIsNotAskedAgainOnceTheContextEnds pins cancellation in the
// refill: a leg whose bounded rows the Go-only predicates thinned is not asked
// for more once the context has ended.
func TestReadBoundedLegIsNotAskedAgainOnceTheContextEnds(t *testing.T) {
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	var rows []beads.Bead
	for i := range 200 {
		rows = append(rows, readyRow(fmt.Sprintf("c-%03d", i), 1, base.Add(time.Duration(i)*time.Second), func(b *beads.Bead) {
			b.Labels = []string{"hold:later"}
		}))
	}
	store := newNarrowingStore(rows...)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	leg := testLeg("city", cancellingOrderedStore{narrowingStore: store, cancel: cancel})
	got, err := ReadContext(ctx, []Leg{leg}, Query{Limit: 5, ExcludeLabels: []string{"hold:later"}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ReadContext = %v, %v; want the context's error once it ended", got, err)
	}
	if calls := store.orderedCalls(); len(calls) != 1 {
		t.Fatalf("leg asked %d bounded reads, want only the one in flight when the context ended", len(calls))
	}
}

// TestReadBoundedAnswerFallsBackWhenTheBindingServesAnIDAnEarlierCutLegHolds
// pins the exactness guard. A migrated bead lives in the work store it was
// minted in AND in the class binding; an unbounded merge keeps the work store's
// copy. When the work leg was cut before that copy and the binding's copy lands
// in the answer, the bounded merge would serve the wrong copy, so the answer is
// read again unbounded.
func TestReadBoundedAnswerFallsBackWhenTheBindingServesAnIDAnEarlierCutLegHolds(t *testing.T) {
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	var workRows []beads.Bead
	for i := range 40 {
		workRows = append(workRows, readyRow(fmt.Sprintf("w-%02d", i), 1, base.Add(time.Duration(i)*time.Minute)))
	}
	// The work store's stale copy of the migrated bead ranks LAST there.
	workRows = append(workRows, readyRow("w-migrated", 3, base.Add(time.Hour), func(b *beads.Bead) { b.Title = "work copy" }))
	// The binding's live copy ranks FIRST in the city.
	graphRows := []beads.Bead{readyRow("w-migrated", 0, base, func(b *beads.Bead) { b.Title = "binding copy" })}

	work, graph := newNarrowingStore(workRows...), newNarrowingStore(graphRows...)
	legs := []Leg{testLeg("city", work), {Label: "graph", Store: graph, ClassBinding: true}}
	plain := []Leg{testLeg("city", beads.NewMemStoreFrom(0, workRows, nil)), {Label: "graph", Store: beads.NewMemStoreFrom(0, graphRows, nil), ClassBinding: true}}

	q := Query{Limit: 3}
	got, err := Read(legs, q)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	want, err := Read(plain, q)
	if err != nil {
		t.Fatalf("Read (unbounded): %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("bounded answer = %+v\nwant the unbounded answer %+v", got, want)
	}
	if len(work.readyQueries()) == 0 {
		t.Fatal("the guard never fell back to an unbounded read of the cut work leg")
	}
}

// TestReadBoundedAnswerTrustsTheBindingWhenNoCutLegHoldsItsRow is the control:
// a binding row no earlier leg holds keeps the bounded answer.
func TestReadBoundedAnswerTrustsTheBindingWhenNoCutLegHoldsItsRow(t *testing.T) {
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	var workRows []beads.Bead
	for i := range 40 {
		workRows = append(workRows, readyRow(fmt.Sprintf("w-%02d", i), 1, base.Add(time.Duration(i)*time.Minute)))
	}
	graphRows := []beads.Bead{readyRow("g-1", 0, base)}
	work, graph := newNarrowingStore(workRows...), newNarrowingStore(graphRows...)
	legs := []Leg{testLeg("city", work), {Label: "graph", Store: graph, ClassBinding: true}}
	if got := readIDs(t, legs, Query{Limit: 3}); !slices.Equal(got, []string{"g-1", "w-00", "w-01"}) {
		t.Fatalf("answer = %v", got)
	}
	if got := work.readyQueries(); len(got) != 0 {
		t.Fatalf("the work leg was read unbounded %d times; nothing it holds conflicts", len(got))
	}
}

// TestReadBoundedBindingReadIsNeverEndedByRelics pins the other direction of
// co-residency. The merge drops a binding's relic for the work store's copy of
// the same id, so a relic must not count toward the rows a bounded binding read
// has in hand. Counted, relics at the head of the binding end its read before
// the binding's own answer rows. The answer then comes back short or wrong,
// and the guard, which probes only the rows the answer serves, has nothing to
// probe.
func TestReadBoundedBindingReadIsNeverEndedByRelics(t *testing.T) {
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	routed := func(b *beads.Bead) { b.Metadata = beads.StringMap{"gc.routed_to": "gascity/worker"} }
	held := func(b *beads.Bead) { b.Labels = []string{"hold:mayor"} }
	// The routed claim tier, flag for flag (config.bdReadyPoolDemandShell).
	claimTier := func(limit int) Query {
		return Query{
			MetadataFields: []string{"gc.routed_to=gascity/worker"},
			Unassigned:     true,
			ExcludeTypes:   []string{"epic"},
			ExcludeLabels:  []string{"hold:mayor", "hold:external", "human"},
			Limit:          limit,
		}
	}
	// ownRows is the binding's own rows, minted after every relic: held rows
	// g-00.. first, then ready ones.
	ownRows := func(heldRows, readyRows int) []beads.Bead {
		var rows []beads.Bead
		for i := range heldRows + readyRows {
			mutate := []func(*beads.Bead){routed}
			if i < heldRows {
				mutate = append(mutate, held)
			}
			rows = append(rows, readyRow(fmt.Sprintf("g-%02d", i), 1, base.Add(time.Hour+time.Duration(i)*time.Second), mutate...))
		}
		return rows
	}
	var heldWork, releasedRelics []beads.Bead
	for i := range 20 {
		id, created := fmt.Sprintf("w-r%02d", i), base.Add(time.Duration(i)*time.Second)
		heldWork = append(heldWork, readyRow(id, 1, created, routed, held))
		releasedRelics = append(releasedRelics, readyRow(id, 1, created, routed))
	}
	for _, tc := range []struct {
		name        string
		work, graph []beads.Bead
		query       Query
		want        []string
	}{
		{
			name:  "the hold the binding released is still on every work copy",
			work:  heldWork,
			graph: append(slices.Clone(releasedRelics), ownRows(20, 5)...),
			query: claimTier(20),
			want:  []string{"g-20", "g-21", "g-22", "g-23", "g-24"},
		},
		{
			name:  "one ready row past the relics",
			work:  heldWork,
			graph: append(slices.Clone(releasedRelics), ownRows(20, 1)...),
			query: claimTier(1),
			want:  []string{"g-20"},
		},
		{
			name:  "the work copy kept a stale priority",
			work:  []beads.Bead{readyRow("w-r00", 3, base, routed)},
			graph: append([]beads.Bead{readyRow("w-r00", 0, base, routed)}, ownRows(20, 1)...),
			query: claimTier(1),
			want:  []string{"g-20"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			binding := func(store beads.Store) Leg {
				return Leg{Label: "graph", Store: store, ClassBinding: true, ReservedPrefixes: []string{"g"}}
			}
			legs := []Leg{testLeg("city", newNarrowingStore(tc.work...)), binding(newNarrowingStore(tc.graph...))}
			plain := []Leg{testLeg("city", ignoringStore{beads.NewMemStoreFrom(0, tc.work, nil)}), binding(ignoringStore{beads.NewMemStoreFrom(0, tc.graph, nil)})}
			if got := readIDs(t, plain, tc.query); !slices.Equal(got, tc.want) {
				t.Fatalf("unpushed answer = %v, want %v", got, tc.want)
			}
			if got := readIDs(t, legs, tc.query); !slices.Equal(got, tc.want) {
				t.Fatalf("pushed answer = %v, want the unpushed %v", got, tc.want)
			}
		})
	}
}

// TestReadStatusLegsNeverServeARelicThePlainReadDrops pins answer preservation
// on the --status arms (ga-8g39ur) over a migration relic: an id the work leg
// holds whose class-binding copy carries the pushed pair and the work copy does
// not. The plain read keeps the work copy (first leg wins) and the filter drops
// it; a narrowed read must not serve the binding's copy instead.
func TestReadStatusLegsNeverServeARelicThePlainReadDrops(t *testing.T) {
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	rooted := func(b *beads.Bead) { b.Metadata = beads.StringMap{"gc.root_bead_id": "R"} }
	status := func(s string) func(*beads.Bead) { return func(b *beads.Bead) { b.Status = s } }
	for _, tc := range []struct {
		name        string
		work, graph []beads.Bead
		query       Query
		want        []string
	}{
		{
			name: "closed",
			work: []beads.Bead{readyRow("w-1", 1, base, status(StatusClosed))},
			graph: []beads.Bead{
				readyRow("w-1", 1, base, status(StatusClosed), rooted),
				readyRow("g-1", 1, base.Add(time.Minute), status(StatusClosed), rooted),
			},
			query: Query{Status: StatusClosed, MetadataFields: []string{"gc.root_bead_id=R"}},
			want:  []string{"g-1"},
		},
		{
			// The work leg's in_progress row ranks first. Left in the narrowed
			// open read's window, it would fill --limit 1 and leave the
			// binding's relic unprobed, and the open cut would then serve it.
			name: "open with a limit",
			work: []beads.Bead{
				readyRow("w-0", 0, base, status(StatusInProgress), rooted),
				readyRow("w-1", 1, base.Add(time.Minute)),
			},
			graph: []beads.Bead{readyRow("w-1", 1, base.Add(time.Minute), rooted)},
			query: Query{Status: StatusOpen, MetadataFields: []string{"gc.root_bead_id=R"}, Limit: 1},
			want:  []string{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			binding := func(store beads.Store) Leg {
				return Leg{Label: "graph", Store: store, ClassBinding: true, ReservedPrefixes: []string{"g"}}
			}
			legs := []Leg{testLeg("city", newNarrowingStore(tc.work...)), binding(newNarrowingStore(tc.graph...))}
			plain := []Leg{testLeg("city", ignoringStore{beads.NewMemStoreFrom(0, tc.work, nil)}), binding(ignoringStore{beads.NewMemStoreFrom(0, tc.graph, nil)})}
			if got := readIDs(t, plain, tc.query); !slices.Equal(got, tc.want) {
				t.Fatalf("unpushed answer = %v, want %v", got, tc.want)
			}
			if got := readIDs(t, legs, tc.query); !slices.Equal(got, tc.want) {
				t.Fatalf("pushed answer = %v, want the unpushed %v", got, tc.want)
			}
		})
	}
}

// TestReadPushdownNeverChangesTheAnswer is the property the pushdown rests on:
// over random cities and random queries, every arm, legs that apply every pushed
// predicate and cut bounded reads in order give exactly the answer of legs that
// apply none of them, row for row and field for field, including on a city
// whose class binding carries relics of earlier legs' beads.
func TestReadPushdownNeverChangesTheAnswer(t *testing.T) {
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	t.Run("random cities", func(t *testing.T) {
		rng := rand.New(rand.NewSource(20261017))
		assignees := []string{"", "", "", "w1", "w2", " w1", "  "}
		routes := []string{"", "pool/a", "pool/b", "true", "42"}
		labels := []string{"", "hold:mayor", "human", "x"}
		types := []string{"task", "task", "bug", "epic"}
		for trial := range 300 {
			legCount := 1 + rng.Intn(4)
			bindingHasRelics := rng.Intn(2) == 0
			var narrowLegs, plainLegs []Leg
			var earlierIDs []string
			for l := range legCount {
				isBinding := l == legCount-1 && legCount > 1
				var rows []beads.Bead
				n := rng.Intn(60)
				for i := range n {
					id := fmt.Sprintf("l%d-%03d", l, i)
					if isBinding && bindingHasRelics && len(earlierIDs) > 0 && rng.Intn(3) == 0 {
						id = earlierIDs[rng.Intn(len(earlierIDs))]
					}
					b := readyRow(id, rng.Intn(4), base.Add(time.Duration(rng.Intn(500))*time.Second), func(b *beads.Bead) {
						b.Status = []string{"open", "open", "open", "in_progress", "blocked", "closed"}[rng.Intn(6)]
						b.Assignee = assignees[rng.Intn(len(assignees))]
						b.Type = types[rng.Intn(len(types))]
						if r := routes[rng.Intn(len(routes))]; r != "" {
							b.Metadata = beads.StringMap{"gc.routed_to": r}
						}
						if lb := labels[rng.Intn(len(labels))]; lb != "" {
							b.Labels = []string{lb}
						}
					})
					if slices.ContainsFunc(rows, func(r beads.Bead) bool { return r.ID == b.ID }) {
						continue
					}
					rows = append(rows, b)
					earlierIDs = append(earlierIDs, b.ID)
				}
				label := fmt.Sprintf("leg %d", l)
				var reserved []string
				if isBinding {
					// The binding's own ids are in its reserved namespace; only the
					// relics it carries are work-shaped.
					reserved = []string{fmt.Sprintf("l%d", l)}
				}
				narrowLegs = append(narrowLegs, Leg{Label: label, Store: newNarrowingStore(rows...), ClassBinding: isBinding, ReservedPrefixes: reserved})
				plainLegs = append(plainLegs, Leg{Label: label, Store: ignoringStore{beads.NewMemStoreFrom(0, rows, nil)}, ClassBinding: isBinding, ReservedPrefixes: reserved})
			}
			q := Query{
				Assignee:   []string{"", "", "w1", "w2"}[rng.Intn(4)],
				Unassigned: rng.Intn(3) == 0,
				Limit:      []int{0, 1, 3, 20}[rng.Intn(4)],
				Sort:       []string{"", "", SortOldest, SortNewest}[rng.Intn(4)],
				Status:     []string{"", "", "", StatusInProgress, StatusOpen, StatusBlocked, StatusClosed}[rng.Intn(7)],
			}
			if rng.Intn(2) == 0 {
				q.MetadataFields = []string{"gc.routed_to=" + routes[1+rng.Intn(len(routes)-1)]}
			}
			if rng.Intn(3) == 0 {
				q.ExcludeTypes = []string{"epic"}
			}
			if rng.Intn(3) == 0 {
				q.ExcludeLabels = []string{"hold:mayor"}
			}
			assertPushdownKeepsTheAnswer(t, trial, narrowLegs, plainLegs, q)
		}
	})
	// A migrated city: `gc storage migrate` copied the work legs' beads into the
	// class binding under their ids, and each binding copy then drifted from the
	// frozen copy left behind (a hold released or taken, a priority re-rolled).
	// The relics are older than every row the binding mints itself, and the
	// binding holds enough routed rows that a bounded read's cut fills, which is
	// where a relic the merge drops could end the binding's read early. The
	// queries are routed claim tiers.
	t.Run("migrated cities", func(t *testing.T) {
		rng := rand.New(rand.NewSource(20261018))
		for trial := range 300 {
			// Each city draws how many of its rows are held and how many of its
			// work beads were migrated.
			holdRate, relicRate := rng.Intn(10), 1+rng.Intn(4)
			routedRow := func(id string, created time.Time) beads.Bead {
				return readyRow(id, rng.Intn(4), created, func(b *beads.Bead) {
					b.Metadata = beads.StringMap{"gc.routed_to": []string{"pool/a", "pool/a", "pool/a", "pool/b"}[rng.Intn(4)]}
					if rng.Intn(5) == 0 {
						b.Assignee = "w1"
					}
					if rng.Intn(6) == 0 {
						b.Type = "epic"
					}
					if rng.Intn(10) < holdRate {
						b.Labels = []string{"hold:mayor"}
					}
				})
			}
			q := Query{
				MetadataFields: []string{"gc.routed_to=pool/a"},
				Unassigned:     rng.Intn(4) != 0,
				ExcludeTypes:   []string{"epic"},
				ExcludeLabels:  []string{"hold:mayor"},
				Limit:          []int{1, 3, 20}[rng.Intn(3)],
				Sort:           []string{"", SortOldest}[rng.Intn(2)],
			}
			workLegs := 1 + rng.Intn(3)
			var narrowLegs, plainLegs []Leg
			var graphRows []beads.Bead
			for l := range workLegs {
				var rows []beads.Bead
				for i := range rng.Intn(40) {
					rows = append(rows, routedRow(fmt.Sprintf("l%d-%03d", l, i), base.Add(time.Duration(rng.Intn(500))*time.Second)))
				}
				for _, w := range rows {
					if rng.Intn(4) >= relicRate {
						continue
					}
					relic := w
					if rng.Intn(2) == 0 {
						relic.Labels = nil
						if len(w.Labels) == 0 {
							relic.Labels = []string{"hold:mayor"}
						}
					} else {
						relic.Priority = intRef(rng.Intn(4))
					}
					graphRows = append(graphRows, relic)
				}
				label := fmt.Sprintf("leg %d", l)
				narrowLegs = append(narrowLegs, testLeg(label, newNarrowingStore(rows...)))
				plainLegs = append(plainLegs, testLeg(label, ignoringStore{beads.NewMemStoreFrom(0, rows, nil)}))
			}
			own := fmt.Sprintf("l%d", workLegs)
			for i := range 2*(q.Limit+boundedSlack) + rng.Intn(40) {
				graphRows = append(graphRows, routedRow(fmt.Sprintf("%s-%03d", own, i), base.Add(time.Duration(500+rng.Intn(500))*time.Second)))
			}
			label := fmt.Sprintf("leg %d", workLegs)
			narrowLegs = append(narrowLegs, Leg{Label: label, Store: newNarrowingStore(graphRows...), ClassBinding: true, ReservedPrefixes: []string{own}})
			plainLegs = append(plainLegs, Leg{Label: label, Store: ignoringStore{beads.NewMemStoreFrom(0, graphRows, nil)}, ClassBinding: true, ReservedPrefixes: []string{own}})
			assertPushdownKeepsTheAnswer(t, trial, narrowLegs, plainLegs, q)
		}
	})
}

// assertPushdownKeepsTheAnswer fails the test unless legs that apply every
// pushed predicate and bound answer q exactly as legs that apply none of them.
func assertPushdownKeepsTheAnswer(t *testing.T, trial int, narrowLegs, plainLegs []Leg, q Query) {
	t.Helper()
	got, err := Read(narrowLegs, q)
	if err != nil {
		t.Fatalf("trial %d: Read: %v", trial, err)
	}
	want, err := Read(plainLegs, q)
	if err != nil {
		t.Fatalf("trial %d: Read (no pushdown): %v", trial, err)
	}
	if !reflect.DeepEqual(got, want) {
		gotJSON, _ := json.Marshal(got)
		wantJSON, _ := json.Marshal(want)
		t.Fatalf("trial %d, query %+v:\npushed   %s\nunpushed %s", trial, q, gotJSON, wantJSON)
	}
}
