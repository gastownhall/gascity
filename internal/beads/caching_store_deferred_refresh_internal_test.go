package beads

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestLiveListRefreshConvergesOnIndefinitelyDeferredBead pins the convergence
// contract of the live-missing refresh: once refreshCachedBeads has absorbed a
// fresh copy of a cached bead that the live list no longer returns, a
// subsequent identical live list must not re-Get that bead.
//
// bd's "deferred" status is the case that breaks it. mapBdStatus collapses bd's
// richer statuses onto Gas City's three, so bd's deferred normalizes to
// Status "open" with the deferral preserved out-of-band in
// Bead.IndefinitelyDeferred (bdstore.go normalizedBdReadState). ListQuery.Matches
// filters on Status alone, so the absorbed copy keeps matching a Status "open"
// query, while `bd list --status=open` keeps omitting it — the bead is stale
// again on the very next live list, forever.
func TestLiveListRefreshConvergesOnIndefinitelyDeferredBead(t *testing.T) {
	const beadID = "ga-deferred1"

	var mu sync.Mutex
	deferredNow := false
	showCalls := 0

	issue := func(status string) []byte {
		// Hand-written so the fixture is exactly the JSON bd emits. Marshaling
		// bdIssue instead serializes optionalBool is_blocked as {}, which the
		// decoder rejects as corrupt.
		return []byte(`[{"id":"` + beadID + `","title":"deferrable work","status":"` + status +
			`","issue_type":"task","assignee":"gascity/builder"}]`)
	}

	runner := func(_, name string, args ...string) ([]byte, error) {
		if name != "bd" {
			t.Fatalf("command name = %q, want bd", name)
		}
		if len(args) == 0 {
			return []byte(`[]`), nil
		}
		mu.Lock()
		defer mu.Unlock()
		switch args[0] {
		case "version":
			return []byte("bd version 1.3.0\n"), nil
		case "list":
			if !deferredNow {
				return issue("open"), nil
			}
			// bd filters on its OWN status vocabulary, before Gas City's
			// normalization: a deferred row is not an open row, so
			// --status=open stops returning it. An UNFILTERED bd list still
			// returns it, carrying bd's real status — which is why the
			// unfiltered reconcile scan can still see it.
			if hasArgPrefix(args, "--status=open") {
				return []byte(`[]`), nil
			}
			return issue("deferred"), nil
		case "show":
			showCalls++
			if deferredNow {
				return issue("deferred"), nil
			}
			return issue("open"), nil
		}
		return []byte(`[]`), nil
	}

	cache := NewCachingStore(NewBdStore("/city", runner), nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	// The bead is primed as open.
	if _, ok := cache.beads[beadID]; !ok {
		t.Fatalf("prime did not cache %s; cache holds %d beads", beadID, len(cache.beads))
	}

	// Someone defers it. bd list --status=open no longer returns it.
	mu.Lock()
	deferredNow = true
	showCalls = 0
	mu.Unlock()

	liveQuery := ListQuery{Status: "open", Live: true, TierMode: TierBoth}

	// First live list: discovering the deferral costs one Get. That is the
	// refresh doing its job.
	if _, err := cache.List(liveQuery); err != nil {
		t.Fatalf("first live List: %v", err)
	}
	mu.Lock()
	afterFirst := showCalls
	mu.Unlock()
	if afterFirst == 0 {
		t.Fatalf("first live List issued no bd show; the refresh never noticed the deferral")
	}

	// Subsequent live lists must converge: the absorbed copy is now current,
	// so there is nothing left to refresh.
	for i := 2; i <= 4; i++ {
		if _, err := cache.List(liveQuery); err != nil {
			t.Fatalf("live List #%d: %v", i, err)
		}
	}
	mu.Lock()
	total := showCalls
	cached := cache.beads[beadID]
	mu.Unlock()

	if extra := total - afterFirst; extra != 0 {
		t.Fatalf("live-list refresh never converges: 3 further identical live lists issued %d more bd show calls for %s (cached Status=%q IndefinitelyDeferred=%v). "+
			"Every indefinitely-deferred bead in the cache is re-Got on every live list.",
			extra, beadID, cached.Status, cached.IndefinitelyDeferred)
	}
}

// getCountingStore counts backing Gets per id. It does not implement
// StatusListOmitsDeferred, so it models a store whose status-filtered List
// returns deferred rows (native Dolt).
type getCountingStore struct {
	Store
	mu   sync.Mutex
	gets map[string]int
}

func (g *getCountingStore) Get(id string) (Bead, error) {
	g.mu.Lock()
	g.gets[id]++
	g.mu.Unlock()
	return g.Store.Get(id)
}

// TestLiveListRefreshStillRefreshesDeferredRowWhenBackingListsDeferred pins
// that the deferred skip is scoped to backing stores whose status filter omits
// deferred rows. A store that lists them only drops one after a real status
// change, so its absence must still be refreshed.
func TestLiveListRefreshStillRefreshesDeferredRowWhenBackingListsDeferred(t *testing.T) {
	mem := NewMemStore()
	created, err := mem.Create(Bead{Title: "deferred work", IndefinitelyDeferred: true})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	backing := &getCountingStore{Store: mem, gets: map[string]int{}}
	cache := NewCachingStore(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	if cached, ok := cache.beads[created.ID]; !ok || !cached.IndefinitelyDeferred {
		t.Fatalf("prime did not cache %s as indefinitely deferred (cached=%+v ok=%v)", created.ID, cached, ok)
	}

	// Another process closes it, bypassing the cache.
	if err := mem.Close(created.ID); err != nil {
		t.Fatalf("backing Close: %v", err)
	}
	backing.mu.Lock()
	backing.gets = map[string]int{}
	backing.mu.Unlock()

	if _, err := cache.List(ListQuery{Status: "open", Live: true, TierMode: TierBoth}); err != nil {
		t.Fatalf("live List: %v", err)
	}

	backing.mu.Lock()
	gets := backing.gets[created.ID]
	backing.mu.Unlock()
	if gets == 0 {
		t.Fatalf("live List did not refresh %s after it left the backing open list; a store that lists deferred rows dropped it for a real status change", created.ID)
	}
	cache.mu.RLock()
	cached, stillCached := cache.beads[created.ID]
	cache.mu.RUnlock()
	if stillCached && cached.Status != "closed" {
		t.Fatalf("cached %s Status = %q after refresh, want closed or evicted", created.ID, cached.Status)
	}
}

// TestLiveListRefreshStillRefreshesTimeDeferredBdRow pins that the skip covers
// only bd's indefinite "deferred" status. A bd row that is "open" with a future
// defer_until still appears in `bd list --status=open`, so its absence means a
// real change and must be refreshed.
func TestLiveListRefreshStillRefreshesTimeDeferredBdRow(t *testing.T) {
	const beadID = "ga-timedefer1"
	deferUntil := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)

	var mu sync.Mutex
	closedNow := false
	showCalls := 0

	issue := func(status string) []byte {
		return []byte(`[{"id":"` + beadID + `","title":"time-deferred work","status":"` + status +
			`","issue_type":"task","assignee":"gascity/builder","defer_until":"` + deferUntil + `"}]`)
	}

	runner := func(_, name string, args ...string) ([]byte, error) {
		if name != "bd" {
			t.Fatalf("command name = %q, want bd", name)
		}
		if len(args) == 0 {
			return []byte(`[]`), nil
		}
		mu.Lock()
		defer mu.Unlock()
		switch args[0] {
		case "version":
			return []byte("bd version 1.3.0\n"), nil
		case "list":
			if !closedNow {
				return issue("open"), nil
			}
			if hasArgPrefix(args, "--status=open") {
				return []byte(`[]`), nil
			}
			return issue("closed"), nil
		case "show":
			showCalls++
			if closedNow {
				return issue("closed"), nil
			}
			return issue("open"), nil
		}
		return []byte(`[]`), nil
	}

	cache := NewCachingStore(NewBdStore("/city", runner), nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	cached, ok := cache.beads[beadID]
	if !ok {
		t.Fatalf("prime did not cache %s; cache holds %d beads", beadID, len(cache.beads))
	}
	if cached.IndefinitelyDeferred || cached.DeferUntil == nil {
		t.Fatalf("fixture cached %s with IndefinitelyDeferred=%v DeferUntil=%v, want a time-bound deferral only",
			beadID, cached.IndefinitelyDeferred, cached.DeferUntil)
	}

	// Someone closes it. bd list --status=open no longer returns it.
	mu.Lock()
	closedNow = true
	showCalls = 0
	mu.Unlock()

	if _, err := cache.List(ListQuery{Status: "open", Live: true, TierMode: TierBoth}); err != nil {
		t.Fatalf("live List: %v", err)
	}
	mu.Lock()
	shows := showCalls
	mu.Unlock()
	if shows == 0 {
		t.Fatalf("live List issued no bd show for time-deferred %s after it left the open list; only bd's indefinite deferred status is omitted by construction", beadID)
	}
}

func hasArgPrefix(args []string, prefix string) bool {
	for _, a := range args {
		if strings.HasPrefix(a, prefix) {
			return true
		}
	}
	return false
}
