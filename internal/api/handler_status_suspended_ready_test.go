package api

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// coldCacheStore models a rig store whose cache runs no background refresh:
// persisted counts answer normally, but the cache-only Ready projection always
// declines with ErrCacheUnavailable, exactly as CachingStore.ReadyContext does
// for a store that never reached cacheLive.
type coldCacheStore struct {
	beads.Store
	readyCalls atomic.Int32
}

func (c *coldCacheStore) ReadyContext(context.Context, ...beads.ReadyQuery) ([]beads.Bead, error) {
	c.readyCalls.Add(1)
	return nil, fmt.Errorf("reading complete ready projection from cache: %w", beads.ErrCacheUnavailable)
}

func newColdCacheRigState(t *testing.T) (*fakeState, *coldCacheStore) {
	t.Helper()
	backing := beads.NewMemStore()
	if _, err := backing.Create(beads.Bead{Type: "task", Title: "rig work", Status: "open"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	cold := &coldCacheStore{Store: backing}
	state := newFakeState(t)
	state.stores = map[string]beads.Store{"myrig": cold}
	state.cityBeadStore = nil
	return state, cold
}

// TestStatusWorkCountsSkipsCacheColdRigs: a suspended rig gets no cache
// (rigStoreBackgroundRefresh), so every read of its store would go to bd and
// restart its retired proxy and Dolt. Status leaves it alone entirely: no
// ready read (which used to make /status permanently partial, grey-dotting
// every systems tile in the dashboard) and no persisted counts either.
func TestStatusWorkCountsSkipsCacheColdRigs(t *testing.T) {
	state, cold := newColdCacheRigState(t)
	s := &Server{state: state}

	wc, errs := s.statusWorkCounts(context.Background(), map[string]bool{"myrig": true})

	if len(errs) != 0 {
		t.Fatalf("partial errors = %v, want none for a cache-cold rig", errs)
	}
	if got := cold.readyCalls.Load(); got != 0 {
		t.Errorf("ready reads = %d, want 0", got)
	}
	if wc.Open != 0 || wc.Ready != 0 {
		t.Errorf("counts = open %d ready %d, want 0: a suspended rig's store is not read", wc.Open, wc.Ready)
	}
	if wc.SuspendedRigsExcluded != 1 {
		t.Errorf("suspended_rigs_excluded = %d, want 1: the counts must say they left the rig out", wc.SuspendedRigsExcluded)
	}
}

// TestStatusWorkCountsStillReportsReadyFailureForRefreshingRigs pins the other
// half: when a rig is NOT cache-cold, a declining Ready read is a genuine
// problem and must still surface as a partial error. The fix must not silence
// cache failures on rigs whose cache is supposed to be live.
func TestStatusWorkCountsStillReportsReadyFailureForRefreshingRigs(t *testing.T) {
	state, cold := newColdCacheRigState(t)
	s := &Server{state: state}

	_, errs := s.statusWorkCounts(context.Background(), nil)

	if len(errs) != 1 {
		t.Fatalf("partial errors = %v, want exactly 1", errs)
	}
	if !strings.Contains(errs[0], "rig myrig work ready:") {
		t.Errorf("partial error = %q, want it to name the rig's ready read", errs[0])
	}
	if got := cold.readyCalls.Load(); got != 1 {
		t.Errorf("ready reads = %d, want 1 — a refreshing rig must still be asked", got)
	}
}
