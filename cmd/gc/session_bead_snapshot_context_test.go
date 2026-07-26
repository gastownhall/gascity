package main

import (
	"context"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// contextListingSessionStore is a beads.Store + beads.ContextLister fake
// that records the ctx each ListContext call received.
type contextListingSessionStore struct {
	beads.Store
	calls []context.Context
}

func (s *contextListingSessionStore) ListContext(ctx context.Context, query beads.ListQuery) ([]beads.Bead, error) {
	s.calls = append(s.calls, ctx)
	return s.List(query)
}

func TestLoadSessionBeadSnapshotContextNilStore(t *testing.T) {
	snap, err := loadSessionBeadSnapshotContext(context.Background(), nil)
	if err != nil {
		t.Fatalf("loadSessionBeadSnapshotContext(nil): %v", err)
	}
	if len(snap.OpenInfos()) != 0 {
		t.Fatalf("OpenInfos() = %d, want 0 for nil store", len(snap.OpenInfos()))
	}
}

func TestLoadSessionBeadSnapshotContextUsesContextLister(t *testing.T) {
	mem := beads.NewMemStore()
	seedSessionBeads(t, mem, 2, 0)
	store := &contextListingSessionStore{Store: mem}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	snap, err := loadSessionBeadSnapshotContext(ctx, store)
	if err != nil {
		t.Fatalf("loadSessionBeadSnapshotContext: %v", err)
	}
	if len(snap.OpenInfos()) != 2 {
		t.Fatalf("OpenInfos() = %d, want 2", len(snap.OpenInfos()))
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

func TestLoadSessionBeadSnapshotContextFallsBackWithoutContextLister(t *testing.T) {
	mem := beads.NewMemStore()
	seedSessionBeads(t, mem, 3, 0)

	snap, err := loadSessionBeadSnapshotContext(context.Background(), mem)
	if err != nil {
		t.Fatalf("loadSessionBeadSnapshotContext: %v", err)
	}
	if len(snap.OpenInfos()) != 3 {
		t.Fatalf("OpenInfos() = %d, want 3 (fell back to plain List)", len(snap.OpenInfos()))
	}
}
