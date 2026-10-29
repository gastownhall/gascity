package api

import (
	"context"
	"fmt"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

// readModelCountingStore counts store.List calls while delegating everything else to
// the embedded store, so a test can prove a read reached the cache tier (zero
// List) rather than the backing store.
type readModelCountingStore struct {
	beads.Store
	listCalls int
}

func (s *readModelCountingStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	s.listCalls++
	return s.Store.List(q)
}

// TestSessionReadModelListingsWarmCacheZeroStoreList is the read-tier contract of
// WI-6 W2: the typed read-model feed must serve a warm cachedListStore without a
// single store.List call. This is the #3939/#3941 dashboard-perf guarantee (no
// per-request bd hit) that the raw sessionReadModelRows path held and the typed
// twin must not regress. It holds for the default list and for every state
// filter that keeps open sessions only.
func TestSessionReadModelListingsWarmCacheZeroStoreList(t *testing.T) {
	t.Parallel()

	backing := &readModelCountingStore{Store: beads.NewMemStore()}
	// Production session beads carry Type=BeadType + LabelSession so the
	// type+label union surfaces them; the fixtures must match that shape.
	for i := 0; i < 3; i++ {
		if _, err := backing.Create(beads.Bead{
			Title:  fmt.Sprintf("session %d", i),
			Type:   session.BeadType,
			Labels: []string{session.LabelSession},
		}); err != nil {
			t.Fatalf("Create(session %d): %v", i, err)
		}
	}

	cache := beads.NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	// Priming necessarily reads the backing store; reset so the assertion below
	// measures only the read-model feed's store access.
	backing.listCalls = 0

	sessFront := session.NewStore(beads.SessionStore{Store: cache})

	for _, stateFilter := range []string{"", "active", "open", "asleep,active"} {
		listings, partial, err := sessionReadModelListings(sessFront, stateFilter)
		if err != nil {
			t.Fatalf("sessionReadModelListings(state=%q): %v", stateFilter, err)
		}
		if len(partial) != 0 {
			t.Fatalf("state=%q: partial errors = %v, want none on a clean warm cache", stateFilter, partial)
		}
		if len(listings) != 3 {
			t.Fatalf("state=%q: listings = %d, want 3", stateFilter, len(listings))
		}
		if backing.listCalls != 0 {
			t.Fatalf("state=%q: warm cache served the read model with %d store.List call(s), want 0 (the #3939/#3941 dashboard-perf tier)", stateFilter, backing.listCalls)
		}
	}

	// The Info-only variant shares the same tier; pin it too.
	backing.listCalls = 0
	infos, partial, err := sessionReadModelInfos(sessFront)
	if err != nil {
		t.Fatalf("sessionReadModelInfos: %v", err)
	}
	if len(partial) != 0 {
		t.Fatalf("infos partial errors = %v, want none", partial)
	}
	if len(infos) != 3 {
		t.Fatalf("infos = %d, want 3", len(infos))
	}
	if backing.listCalls != 0 {
		t.Fatalf("warm cache served the Info feed with %d store.List call(s), want 0", backing.listCalls)
	}
}

// TestSessionReadModelListingsReadsClosedRowsForClosedFilters pins the other
// half of the tier: a state filter that can keep a closed session ("closed",
// "all", or a list naming closed) gets the closed rows, which the warm cache
// does not hold, while the open-only filters keep getting open rows alone.
func TestSessionReadModelListingsReadsClosedRowsForClosedFilters(t *testing.T) {
	t.Parallel()

	backing := beads.NewMemStore()
	var closedID string
	for i := 0; i < 3; i++ {
		b, err := backing.Create(beads.Bead{
			Title:  fmt.Sprintf("session %d", i),
			Type:   session.BeadType,
			Labels: []string{session.LabelSession},
		})
		if err != nil {
			t.Fatalf("Create(session %d): %v", i, err)
		}
		closedID = b.ID
	}
	if err := backing.Close(closedID); err != nil {
		t.Fatalf("Close(%s): %v", closedID, err)
	}

	cache := beads.NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	sessFront := session.NewStore(beads.SessionStore{Store: cache})

	listedClosed := func(t *testing.T, stateFilter string) (int, bool) {
		t.Helper()
		listings, partial, err := sessionReadModelListings(sessFront, stateFilter)
		if err != nil {
			t.Fatalf("sessionReadModelListings(state=%q): %v", stateFilter, err)
		}
		if len(partial) != 0 {
			t.Fatalf("state=%q: partial errors = %v, want none", stateFilter, partial)
		}
		for _, listing := range listings {
			if listing.Info.ID == closedID {
				if !listing.Info.Closed {
					t.Fatalf("state=%q: session %s listed as open, want closed", stateFilter, closedID)
				}
				return len(listings), true
			}
		}
		return len(listings), false
	}
	for _, stateFilter := range []string{"closed", "all", "active,closed"} {
		if n, ok := listedClosed(t, stateFilter); !ok || n != 3 {
			t.Fatalf("state=%q: listed %d rows (closed %s present: %v), want the 2 open and the closed one", stateFilter, n, closedID, ok)
		}
	}
	for _, stateFilter := range []string{"", "active", "open"} {
		if n, ok := listedClosed(t, stateFilter); ok || n != 2 {
			t.Fatalf("state=%q: listed %d rows (closed %s present: %v), want the 2 open ones only", stateFilter, n, closedID, ok)
		}
	}
}
