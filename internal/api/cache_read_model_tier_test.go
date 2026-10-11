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
// twin must not regress.
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

	listings, partial, err := sessionReadModelListings(sessFront)
	if err != nil {
		t.Fatalf("sessionReadModelListings: %v", err)
	}
	if len(partial) != 0 {
		t.Fatalf("partial errors = %v, want none on a clean warm cache", partial)
	}
	if len(listings) != 3 {
		t.Fatalf("listings = %d, want 3", len(listings))
	}
	if backing.listCalls != 0 {
		t.Fatalf("warm cache served the read model with %d store.List call(s), want 0 (the #3939/#3941 dashboard-perf tier)", backing.listCalls)
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
