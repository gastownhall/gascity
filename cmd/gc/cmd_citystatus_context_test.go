package main

import (
	"context"
	"io"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// deadlineRecordingSessionStore is a beads.Store + beads.ContextLister fake
// that records whether ListContext calls carried a deadline.
type deadlineRecordingSessionStore struct {
	beads.Store
	listContextCalls int
	sawDeadline      bool
}

func (s *deadlineRecordingSessionStore) ListContext(ctx context.Context, query beads.ListQuery) ([]beads.Bead, error) {
	s.listContextCalls++
	if _, ok := ctx.Deadline(); ok {
		s.sawDeadline = true
	}
	return s.List(query)
}

// TestLoadStatusSessionSnapshotThreadsDeadlineIntoContextLister proves
// loadStatusSessionSnapshot's internal ctx (bound to
// statusSessionSnapshotTimeout) reaches the store's ListContext, not
// context.Background(), so a timeout actually cancels the backing bd child
// instead of merely abandoning the goroutine.
func TestLoadStatusSessionSnapshotThreadsDeadlineIntoContextLister(t *testing.T) {
	store := &deadlineRecordingSessionStore{Store: beads.NewMemStore()}

	snap := loadStatusSessionSnapshot(store, io.Discard)
	if snap.LoadError() != nil {
		t.Fatalf("LoadError() = %v, want nil", snap.LoadError())
	}
	if store.listContextCalls == 0 {
		t.Fatal("ListContext was never called; want the ContextLister path preferred over plain List")
	}
	if !store.sawDeadline {
		t.Fatal("ListContext ctx had no deadline; want it bound to statusSessionSnapshotTimeout")
	}
}

func TestLoadStatusSessionSnapshotNilStore(t *testing.T) {
	snap := loadStatusSessionSnapshot(nil, io.Discard)
	if len(snap.OpenInfos()) != 0 {
		t.Fatalf("OpenInfos() = %d, want 0 for nil store", len(snap.OpenInfos()))
	}
}
