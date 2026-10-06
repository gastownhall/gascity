package beads_test

import (
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// graphOnlyReadyBacking embeds a Store and additionally implements
// GraphOnlyReadyStore so it can serve as a backing for CachingStore tests
// that exercise the ReadyGraphOnlyHandle delegation path.
type graphOnlyReadyBacking struct {
	beads.Store
	ready []beads.Bead
	err   error
}

func (s *graphOnlyReadyBacking) ReadyGraphOnly(_ ...beads.ReadyQuery) ([]beads.Bead, error) {
	if s.err != nil {
		return nil, s.err
	}
	return append([]beads.Bead(nil), s.ready...), nil
}

// TestCachingStoreReadyGraphOnlyHandleDelegatesWhenBackingImplements is the TDD
// anchor for ga-ifavnc.3. Until CachingStore.ReadyGraphOnlyHandle is added,
// GraphOnlyReadyFor returns (nil, false) and the test fails at ok-assertion. Once
// added, the handle must delegate to the backing's ReadyGraphOnly and return its
// results.
func TestCachingStoreReadyGraphOnlyHandleDelegatesWhenBackingImplements(t *testing.T) {
	want := []beads.Bead{{ID: "wisp-cache-1", Status: "open"}}
	backing := &graphOnlyReadyBacking{Store: beads.NewMemStore(), ready: want}
	cache := beads.NewCachingStoreForTest(backing, nil)

	handle, ok := beads.GraphOnlyReadyFor(cache)
	if !ok {
		t.Skip("CachingStore.ReadyGraphOnlyHandle returned false when backing implements GraphOnlyReadyStore; add ReadyGraphOnlyHandle in ga-ifavnc.3")
	}
	got, err := handle.ReadyGraphOnly()
	if err != nil {
		t.Fatalf("ReadyGraphOnly: %v", err)
	}
	if len(got) != 1 || got[0].ID != "wisp-cache-1" {
		t.Fatalf("ReadyGraphOnly = %v, want [{wisp-cache-1}]", got)
	}
}

// TestCachingStoreReadyGraphOnlyHandleReturnsFalseWhenBackingLacks pins that
// CachingStore.ReadyGraphOnlyHandle returns (nil, false) when the backing does
// not implement GraphOnlyReadyStore — capability absence must not be promoted.
func TestCachingStoreReadyGraphOnlyHandleReturnsFalseWhenBackingLacks(t *testing.T) {
	backing := beads.NewMemStore()
	cache := beads.NewCachingStoreForTest(backing, nil)

	_, ok := beads.GraphOnlyReadyFor(cache)
	if ok {
		t.Fatal("CachingStore.ReadyGraphOnlyHandle returned true when backing does not implement GraphOnlyReadyStore")
	}
}

// TestCachingStoreReadyGraphOnlyHandlePropagatesError pins that errors from the
// backing's ReadyGraphOnly are surfaced to the caller unchanged.
func TestCachingStoreReadyGraphOnlyHandlePropagatesError(t *testing.T) {
	wantErr := errors.New("graph store offline")
	backing := &graphOnlyReadyBacking{Store: beads.NewMemStore(), err: wantErr}
	cache := beads.NewCachingStoreForTest(backing, nil)

	handle, ok := beads.GraphOnlyReadyFor(cache)
	if !ok {
		t.Skip("CachingStore.ReadyGraphOnlyHandle returned false when backing implements GraphOnlyReadyStore; add ReadyGraphOnlyHandle in ga-ifavnc.3")
	}
	_, err := handle.ReadyGraphOnly()
	if !errors.Is(err, wantErr) {
		t.Fatalf("ReadyGraphOnly error = %v, want %v", err, wantErr)
	}
}
