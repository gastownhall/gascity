package session

import (
	"context"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// contextListingStore is a beads.Store + beads.ContextLister fake that
// records ListContext calls and the ctx each received.
type contextListingStore struct {
	beads.Store
	calls []context.Context
}

func (s *contextListingStore) ListContext(ctx context.Context, query beads.ListQuery) ([]beads.Bead, error) {
	s.calls = append(s.calls, ctx)
	return s.List(query)
}

func TestListAllSessionBeadsContext_NilStore(t *testing.T) {
	got, err := ListAllSessionBeadsContext(context.Background(), nil, beads.ListQuery{})
	if err != nil {
		t.Errorf("nil store should not error, got %v", err)
	}
	if len(got) != 0 {
		t.Errorf("nil store should return empty, got %d beads", len(got))
	}
}

func TestListAllSessionBeadsContext_UsesContextListerForBothLegs(t *testing.T) {
	mem := beads.NewMemStore()
	if _, err := mem.Create(beads.Bead{
		Title:    "healthy",
		Type:     BeadType,
		Labels:   []string{LabelSession},
		Metadata: map[string]string{"session_name": "healthy"},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	store := &contextListingStore{Store: mem}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	got, err := ListAllSessionBeadsContext(ctx, store, beads.ListQuery{})
	if err != nil {
		t.Fatalf("ListAllSessionBeadsContext: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(got) = %d, want 1", len(got))
	}
	// One call per leg (type query, label query) — both are ContextLister
	// calls carrying the caller's own ctx, not context.Background().
	if len(store.calls) != 2 {
		t.Fatalf("ListContext calls = %d, want 2 (type leg + label leg)", len(store.calls))
	}
	for i, gotCtx := range store.calls {
		if gotCtx != ctx {
			t.Errorf("call %d: ctx = %v, want the caller's ctx", i, gotCtx)
		}
	}
}

func TestListAllSessionBeadsContext_FallsBackToListWithoutContextLister(t *testing.T) {
	mem := beads.NewMemStore()
	if _, err := mem.Create(beads.Bead{
		Title:    "healthy",
		Type:     BeadType,
		Labels:   []string{LabelSession},
		Metadata: map[string]string{"session_name": "healthy"},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := ListAllSessionBeadsContext(context.Background(), mem, beads.ListQuery{})
	if err != nil {
		t.Fatalf("ListAllSessionBeadsContext: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(got) = %d, want 1 (fell back to plain List)", len(got))
	}
}
