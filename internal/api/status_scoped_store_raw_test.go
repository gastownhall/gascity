package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// TestStatusHandsScopedStoreLikeRawStores pins R12 (typed-stores T6 plan):
// ScopedStoreLike recognizes a bd-backed store by its concrete layers
// (cmd/gc bdStoreBacking), so a class handle handed to it answers "not bd"
// and the ga-cdmx6x ctx-bound clone silently turns off for /status. Until T7
// retypes it, every store /status hands ScopedStoreLike is a raw store a
// State accessor returns (or a class accessor's unwrap), never a handle.
func TestStatusHandsScopedStoreLikeRawStores(t *testing.T) {
	state := newFakeState(t)
	city := &legacyReadyStore{Store: beads.NewMemStore()}
	rig := &legacyReadyStore{Store: beads.NewMemStore()}
	state.cityBeadStore = city
	state.stores = map[string]beads.Store{"myrig": rig}
	var mu sync.Mutex
	var handed []beads.Store
	state.scopedStoreFn = func(_ context.Context, existing beads.Store) (beads.Store, error) {
		mu.Lock()
		handed = append(handed, existing)
		mu.Unlock()
		return nil, nil
	}
	h := newTestCityHandler(t, state)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, cityURL(state, "/status"), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /status = %d: %s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(handed) == 0 {
		t.Fatal("/status never asked ScopedStoreLike for a scoped clone; the pin proves nothing")
	}
	raw := map[beads.Store]bool{
		state.CityBeadStore(): true, state.BeadStore("myrig"): true,
		state.Stores().Sessions().Store: true, state.Stores().Graph().Store: true,
	}
	for _, s := range handed {
		switch s.(type) {
		case beads.WorkStore, beads.GraphStore, beads.SessionStore, beads.MailStore, beads.OrdersStore, beads.NudgesStore, beads.MoleculeTarget:
			t.Errorf("/status handed ScopedStoreLike a class handle %T; it must be the raw store", s)
			continue
		}
		if !raw[s] {
			t.Errorf("/status handed ScopedStoreLike %T, not a store a raw accessor returns", s)
		}
	}
}
