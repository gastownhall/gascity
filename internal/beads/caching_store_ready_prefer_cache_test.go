package beads

import (
	"context"
	"encoding/json"
	"reflect"
	"sync"
	"testing"
)

// readyCountingStore counts the backing Ready scans a cache-preferred read
// makes: a warm, valid cache must answer with none.
type readyCountingStore struct {
	*MemStore
	mu      sync.Mutex
	queries []ReadyQuery
}

func (s *readyCountingStore) Ready(query ...ReadyQuery) ([]Bead, error) {
	s.mu.Lock()
	s.queries = append(s.queries, readyQueryFromArgs(query))
	s.mu.Unlock()
	return s.MemStore.Ready(query...)
}

func (s *readyCountingStore) readyCalls() []ReadyQuery {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]ReadyQuery(nil), s.queries...)
}

// readyPreferCacheFixture: an open issue, an open wisp, a bead blocked by an
// open blocker (the blocker itself is ready), an assigned bead and a closed one.
func readyPreferCacheFixture(t *testing.T) (*readyCountingStore, map[string]string) {
	t.Helper()
	backing := &readyCountingStore{MemStore: NewMemStore()}
	ids := map[string]string{}
	mk := func(name string, b Bead) {
		t.Helper()
		created, err := backing.Create(b)
		if err != nil {
			t.Fatalf("Create(%s): %v", name, err)
		}
		ids[name] = created.ID
	}
	mk("issue", Bead{Title: "issue", Priority: rpcIntPtr(2)})
	mk("wisp", Bead{Title: "wisp", Ephemeral: true, Priority: rpcIntPtr(1)})
	mk("blocker", Bead{Title: "blocker", Priority: rpcIntPtr(3)})
	mk("blocked", Bead{Title: "blocked", Priority: rpcIntPtr(0)})
	mk("assigned", Bead{Title: "assigned", Assignee: "olivia", Priority: rpcIntPtr(2)})
	mk("closed", Bead{Title: "closed"})
	if err := backing.DepAdd(ids["blocked"], ids["blocker"], "blocks"); err != nil {
		t.Fatalf("DepAdd: %v", err)
	}
	if err := backing.Close(ids["closed"]); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return backing, ids
}

func rpcIntPtr(v int) *int { return &v }

// canonicalReady is the backing's answer to q in the canonical ready order the
// SQL-backed readers and the cache emit (MemStore emits creation order), with
// q's limit cut after the sort, as the cache cuts it.
func canonicalReady(t *testing.T, backing *readyCountingStore, q ReadyQuery) []Bead {
	t.Helper()
	unlimited := q
	unlimited.Limit = 0
	rows, err := backing.MemStore.Ready(unlimited)
	if err != nil {
		t.Fatalf("backing Ready(%+v): %v", q, err)
	}
	sortBeadsReadyOrder(rows)
	if q.Limit > 0 && len(rows) > q.Limit {
		rows = rows[:q.Limit]
	}
	return rows
}

func rpcIDs(rows []Bead) []string {
	out := make([]string, 0, len(rows))
	for _, b := range rows {
		out = append(out, b.ID)
	}
	return out
}

func rpcSameIDs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestReadyPreferCacheServesWarmCacheWithoutBackingScan is the core: a warm,
// valid cache answers a tier-stated ready query (the federated query native
// Ready sends) from memory, with the backing's own answer, in the backing's
// canonical order, and with no backing Ready scan.
func TestReadyPreferCacheServesWarmCacheWithoutBackingScan(t *testing.T) {
	backing, ids := readyPreferCacheFixture(t)
	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	for _, q := range []ReadyQuery{
		{TierMode: TierBoth},
		{TierMode: TierIssues},
		{TierMode: TierWisps},
		{TierMode: TierBoth, Assignee: "olivia"},
		{TierMode: TierBoth, Limit: 2},
		{},
	} {
		want := canonicalReady(t, backing, q)
		before := len(backing.readyCalls())
		got, err := cache.ReadyPreferCache(q)
		if err != nil {
			t.Fatalf("ReadyPreferCache(%+v): %v", q, err)
		}
		if calls := backing.readyCalls()[before:]; len(calls) != 0 {
			t.Errorf("ReadyPreferCache(%+v) scanned the backing %d time(s), want a cache answer", q, len(calls))
		}
		if !rpcSameIDs(rpcIDs(got), rpcIDs(want)) {
			t.Errorf("ReadyPreferCache(%+v) = %v, want the backing's %v", q, rpcIDs(got), rpcIDs(want))
		}
	}
	both, _ := cache.ReadyPreferCache(ReadyQuery{TierMode: TierBoth})
	for _, name := range []string{"issue", "wisp", "blocker", "assigned"} {
		found := false
		for _, id := range rpcIDs(both) {
			found = found || id == ids[name]
		}
		if !found {
			t.Errorf("TierBoth ready %v lacks %s", rpcIDs(both), name)
		}
	}
}

// TestReadyPreferCacheReadsLiveWhenCold: an unprimed cache has nothing to vouch
// for, so the read goes to the backing exactly once, with the caller's query.
func TestReadyPreferCacheReadsLiveWhenCold(t *testing.T) {
	backing, _ := readyPreferCacheFixture(t)
	cache := NewCachingStoreForTest(backing, nil)
	q := ReadyQuery{TierMode: TierBoth}
	got, err := cache.ReadyPreferCache(q)
	if err != nil {
		t.Fatalf("ReadyPreferCache: %v", err)
	}
	calls := backing.readyCalls()
	if len(calls) != 1 || !reflect.DeepEqual(calls[0], q) {
		t.Fatalf("backing Ready calls = %+v, want exactly one live read of %+v", calls, q)
	}
	want, _ := backing.MemStore.Ready(q)
	if !rpcSameIDs(rpcIDs(got), rpcIDs(want)) {
		t.Fatalf("cold read = %v, want %v", rpcIDs(got), rpcIDs(want))
	}
}

// TestReadyPreferCacheReadsLiveWhenProjectionDegraded: a cache whose ready
// verdicts must go live (the same latch Ready honors) does not answer.
func TestReadyPreferCacheReadsLiveWhenProjectionDegraded(t *testing.T) {
	backing, _ := readyPreferCacheFixture(t)
	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	cache.readyProjectionDegraded.Store(true)
	if _, err := cache.ReadyPreferCache(ReadyQuery{TierMode: TierBoth}); err != nil {
		t.Fatalf("ReadyPreferCache: %v", err)
	}
	if calls := backing.readyCalls(); len(calls) != 1 {
		t.Fatalf("backing Ready calls = %d, want the live read a degraded projection forces", len(calls))
	}
}

// TestReadyPreferCacheReadsLiveForAnUnanswerableCandidate: one candidate whose
// verdict the cache cannot vouch for sends the read live. A query whose
// assignee filter leaves that row out is still answered from the cache.
func TestReadyPreferCacheReadsLiveForAnUnanswerableCandidate(t *testing.T) {
	backing, ids := readyPreferCacheFixture(t)
	// The dependency-derived predicate does not model a parent-child edge, so
	// it cannot re-derive this row's verdict once the projected one is lost.
	if err := backing.DepAdd(ids["issue"], ids["assigned"], "parent-child"); err != nil {
		t.Fatalf("DepAdd: %v", err)
	}
	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	cache.mu.Lock()
	cache.markReadyProjectionLostLocked(ids["issue"])
	cache.mu.Unlock()
	if !cache.readyProjectionUnknownForTest(ids["issue"]) {
		t.Fatalf("issue is servable from cache, so the fallback below proves nothing")
	}

	all := ReadyQuery{TierMode: TierBoth}
	got, err := cache.ReadyPreferCache(all)
	if err != nil {
		t.Fatalf("ReadyPreferCache(%+v): %v", all, err)
	}
	if calls := backing.readyCalls(); len(calls) != 1 || !reflect.DeepEqual(calls[0], all) {
		t.Fatalf("backing Ready calls = %+v, want one live read of %+v", calls, all)
	}
	if want, _ := backing.MemStore.Ready(all); !rpcSameIDs(rpcIDs(got), rpcIDs(want)) {
		t.Fatalf("ReadyPreferCache(%+v) = %v, want the live %v", all, rpcIDs(got), rpcIDs(want))
	}

	theirs := ReadyQuery{TierMode: TierBoth, Assignee: "olivia"}
	got, err = cache.ReadyPreferCache(theirs)
	if err != nil {
		t.Fatalf("ReadyPreferCache(%+v): %v", theirs, err)
	}
	if calls := backing.readyCalls(); len(calls) != 1 {
		t.Fatalf("backing Ready calls = %d after ReadyPreferCache(%+v), want still 1: the filter leaves the unanswerable row out", len(calls), theirs)
	}
	if want := []string{ids["assigned"]}; !rpcSameIDs(rpcIDs(got), want) {
		t.Fatalf("ReadyPreferCache(%+v) = %v, want %v", theirs, rpcIDs(got), want)
	}
}

// TestReadyPreferCacheHonorsInvalidation: an event that closes the blocker
// invalidates the dependent's verdict, and the next cache-preferred read
// reflects it without a backing scan, exactly as the zero-query Ready does.
func TestReadyPreferCacheHonorsInvalidation(t *testing.T) {
	backing, ids := readyPreferCacheFixture(t)
	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	if err := backing.Close(ids["blocker"]); err != nil {
		t.Fatalf("Close(blocker): %v", err)
	}
	closed, err := backing.Get(ids["blocker"])
	if err != nil {
		t.Fatalf("Get(blocker): %v", err)
	}
	payload, err := json.Marshal(closed)
	if err != nil {
		t.Fatal(err)
	}
	cache.ApplyEvent("bead.closed", payload)

	got, err := cache.ReadyPreferCache(ReadyQuery{TierMode: TierBoth})
	if err != nil {
		t.Fatalf("ReadyPreferCache: %v", err)
	}
	want := canonicalReady(t, backing, ReadyQuery{TierMode: TierBoth})
	if !rpcSameIDs(rpcIDs(got), rpcIDs(want)) {
		t.Fatalf("after invalidation = %v, want %v", rpcIDs(got), rpcIDs(want))
	}
}

// TestReadyPreferCacheFallsBackForPlainStores: a store with no cache is read
// live through its handles, unchanged.
func TestReadyPreferCacheFallsBackForPlainStores(t *testing.T) {
	backing, _ := readyPreferCacheFixture(t)
	if _, err := ReadyPreferCache(backing, ReadyQuery{TierMode: TierBoth}); err != nil {
		t.Fatalf("ReadyPreferCache: %v", err)
	}
	if calls := backing.readyCalls(); len(calls) != 1 {
		t.Fatalf("backing Ready calls = %d, want one live read", len(calls))
	}
	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	if _, err := ReadyPreferCache(cache, ReadyQuery{TierMode: TierBoth}); err != nil {
		t.Fatalf("ReadyPreferCache(cache): %v", err)
	}
	if calls := backing.readyCalls(); len(calls) != 1 {
		t.Fatalf("backing Ready calls = %d after a warm cache read, want still 1", len(calls))
	}
}

// TestReadyQueryIsZeroConsultsEveryField pins IsZero to ReadyQuery's field set.
// CachingStore.Ready answers a query IsZero calls zero from its snapshot, which
// applies no selector at all, so a field IsZero does not consult would have a
// query that sets it served the unnarrowed cached answer. Each field is set on
// its own and must make the query non-zero; a field of a kind this test cannot
// set fails it until the test learns that kind.
func TestReadyQueryIsZeroConsultsEveryField(t *testing.T) {
	if !(ReadyQuery{}).IsZero() {
		t.Fatal("ReadyQuery{}.IsZero() = false, want true")
	}
	typ := reflect.TypeOf(ReadyQuery{})
	for i := range typ.NumField() {
		field := typ.Field(i)
		t.Run(field.Name, func(t *testing.T) {
			var q ReadyQuery
			v := reflect.ValueOf(&q).Elem().Field(i)
			switch v.Kind() {
			case reflect.String:
				v.SetString("x")
			case reflect.Bool:
				v.SetBool(true)
			case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
				v.SetInt(1)
			case reflect.Map:
				m := reflect.MakeMap(v.Type())
				m.SetMapIndex(reflect.New(v.Type().Key()).Elem(), reflect.New(v.Type().Elem()).Elem())
				v.Set(m)
			case reflect.Slice:
				v.Set(reflect.MakeSlice(v.Type(), 1, 1))
			case reflect.Pointer:
				v.Set(reflect.New(v.Type().Elem()))
			default:
				t.Fatalf("ReadyQuery.%s is a %s, which this test cannot set; teach it that kind", field.Name, v.Kind())
			}
			if q.IsZero() {
				t.Fatalf("ReadyQuery{%s: %#v}.IsZero() = true: IsZero does not consult %s, so CachingStore.Ready would answer a query that sets it from the unnarrowed cache", field.Name, v.Interface(), field.Name)
			}
		})
	}
}
