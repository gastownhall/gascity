package api

import (
	"context"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

// contextOnlyListStore is a beads.Store + beads.ContextLister fake that does
// NOT implement cachedListStore, so sessionReadModelInfosContext must fall
// through to session.Store.ListAllContext's ContextLister path.
type contextOnlyListStore struct {
	beads.Store
	calls []context.Context
}

func (s *contextOnlyListStore) ListContext(ctx context.Context, query beads.ListQuery) ([]beads.Bead, error) {
	s.calls = append(s.calls, ctx)
	return s.List(query)
}

func TestSessionReadModelInfosContext_DelegatesToContextListerWhenCacheMisses(t *testing.T) {
	mem := beads.NewMemStore()
	if _, err := mem.Create(beads.Bead{
		Title:  "session",
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	store := &contextOnlyListStore{Store: mem}
	sessFront := session.NewStore(beads.SessionStore{Store: store})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	infos, partialErrors, err := sessionReadModelInfosContext(ctx, sessFront)
	if err != nil {
		t.Fatalf("sessionReadModelInfosContext: %v", err)
	}
	if len(partialErrors) != 0 {
		t.Fatalf("partialErrors = %v, want none", partialErrors)
	}
	if len(infos) != 1 {
		t.Fatalf("len(infos) = %d, want 1", len(infos))
	}
	if len(store.calls) == 0 {
		t.Fatal("ListContext was never called; want delegation via ContextLister")
	}
	for i, gotCtx := range store.calls {
		if gotCtx != ctx {
			t.Errorf("call %d: ctx = %v, want the caller's ctx", i, gotCtx)
		}
	}
}

func TestSessionReadModelInfosContext_ServedFromCacheFastPath(t *testing.T) {
	backing := beads.NewMemStore()
	if _, err := backing.Create(beads.Bead{
		Title:  "session",
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	cache := beads.NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	sessFront := session.NewStore(beads.SessionStore{Store: cache})

	// A canceled ctx must not matter: the cache fast path answers entirely
	// in-memory, mirroring sessionReadModelInfos's own behavior.
	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	infos, partialErrors, err := sessionReadModelInfosContext(canceledCtx, sessFront)
	if err != nil {
		t.Fatalf("sessionReadModelInfosContext: %v", err)
	}
	if len(partialErrors) != 0 {
		t.Fatalf("partialErrors = %v, want none", partialErrors)
	}
	if len(infos) != 1 {
		t.Fatalf("len(infos) = %d, want 1 (served from cache, ctx cancellation irrelevant)", len(infos))
	}
}

func TestSessionReadModelInfosContext_WrapsPartialResultAsWarning(t *testing.T) {
	backing := &partialPrimeSessionStore{MemStore: beads.NewMemStore()}
	survivor, err := backing.Create(beads.Bead{
		Title:  "session survivor",
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	backing.partialRows = []beads.Bead{survivor}

	cache := beads.NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	sessFront := session.NewStore(beads.SessionStore{Store: cache})

	infos, partialErrors, err := sessionReadModelInfosContext(context.Background(), sessFront)
	if err != nil {
		t.Fatalf("sessionReadModelInfosContext: %v", err)
	}
	if len(infos) != 1 {
		t.Fatalf("len(infos) = %d, want 1 survivor row", len(infos))
	}
	if len(partialErrors) == 0 {
		t.Fatal("want a partial-result warning surfaced, got none")
	}
}
