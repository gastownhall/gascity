package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

type beadReadyResponse struct {
	Items         []beads.Bead `json:"items"`
	Total         int          `json:"total"`
	Partial       bool         `json:"partial"`
	PartialErrors []string     `json:"partial_errors"`
}

func getBeadReady(t *testing.T, state *fakeState) (int, beadReadyResponse) {
	t.Helper()
	h := newTestCityHandler(t, state)
	req := httptest.NewRequest("GET", cityURL(state, "/beads/ready"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var resp beadReadyResponse
	if rec.Code == http.StatusOK {
		if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
	}
	return rec.Code, resp
}

func beadIDsOf(rows []beads.Bead) []string {
	ids := make([]string, 0, len(rows))
	for _, b := range rows {
		ids = append(ids, b.ID)
	}
	return ids
}

// TestBeadReadyServesWarmCache: a warm, valid cache answers native
// Ready with no backing scan, and the answer is the backing's own, wisps
// included (the federated tier).
func TestBeadReadyServesWarmCache(t *testing.T) {
	state := newFakeState(t)
	backing := &countingReadyStore{MemStore: beads.NewMemStore()}
	ready, err := backing.Create(beads.Bead{Title: "ready"})
	if err != nil {
		t.Fatalf("Create(ready): %v", err)
	}
	wisp, err := backing.Create(beads.Bead{Title: "wisp", Ephemeral: true})
	if err != nil {
		t.Fatalf("Create(wisp): %v", err)
	}
	blocker, err := backing.Create(beads.Bead{Title: "blocker"})
	if err != nil {
		t.Fatalf("Create(blocker): %v", err)
	}
	blocked, err := backing.Create(beads.Bead{Title: "blocked"})
	if err != nil {
		t.Fatalf("Create(blocked): %v", err)
	}
	if err := backing.DepAdd(blocked.ID, blocker.ID, "blocks"); err != nil {
		t.Fatalf("DepAdd: %v", err)
	}
	cache := beads.NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	state.stores["myrig"] = cache
	backing.readyCalls = 0

	code, resp := getBeadReady(t, state)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if backing.readyCalls != 0 {
		t.Fatalf("backing Ready scans = %d, want a warm-cache answer", backing.readyCalls)
	}
	got := strings.Join(beadIDsOf(resp.Items), ",")
	for _, want := range []string{ready.ID, wisp.ID, blocker.ID} {
		if !strings.Contains(got, want) {
			t.Errorf("ready = %s, want it to include %s", got, want)
		}
	}
	if strings.Contains(got, blocked.ID) {
		t.Errorf("ready = %s, want blocked %s excluded", got, blocked.ID)
	}
}

// TestBeadReadyReadsLiveWhenCacheCold: an unprimed cache cannot vouch for an
// answer, so the leg is read live, as before.
func TestBeadReadyReadsLiveWhenCacheCold(t *testing.T) {
	state := newFakeState(t)
	backing := &countingReadyStore{MemStore: beads.NewMemStore()}
	ready, err := backing.Create(beads.Bead{Title: "ready"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	state.stores["myrig"] = beads.NewCachingStoreForTest(backing, nil)

	code, resp := getBeadReady(t, state)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if backing.readyCalls != 1 {
		t.Fatalf("backing Ready scans = %d, want one live read of the cold leg", backing.readyCalls)
	}
	if resp.Total != 1 || resp.Items[0].ID != ready.ID {
		t.Fatalf("ready = %v, want [%s]", beadIDsOf(resp.Items), ready.ID)
	}
}

// TestBeadReadyReflectsAnInvalidatedCacheRow replaces the old "always live"
// pin: a write the cache has been told about (the event that closes the
// blocker) is reflected on the next Ready, under the cache's own invalidation
// rules, with no backing scan.
func TestBeadReadyReflectsAnInvalidatedCacheRow(t *testing.T) {
	state := newFakeState(t)
	backing := &countingReadyStore{MemStore: beads.NewMemStore()}
	blocker, err := backing.Create(beads.Bead{Title: "blocker"})
	if err != nil {
		t.Fatalf("Create(blocker): %v", err)
	}
	ready, err := backing.Create(beads.Bead{Title: "ready"})
	if err != nil {
		t.Fatalf("Create(ready): %v", err)
	}
	if err := backing.DepAdd(ready.ID, blocker.ID, "blocks"); err != nil {
		t.Fatalf("DepAdd: %v", err)
	}
	cache := beads.NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	state.stores["myrig"] = cache
	if err := backing.Close(blocker.ID); err != nil {
		t.Fatalf("Close(blocker): %v", err)
	}
	closed, err := backing.Get(blocker.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	payload, err := json.Marshal(closed)
	if err != nil {
		t.Fatal(err)
	}
	cache.ApplyEvent("bead.closed", payload)
	backing.readyCalls = 0

	code, resp := getBeadReady(t, state)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if resp.Total != 1 || len(resp.Items) != 1 || resp.Items[0].ID != ready.ID {
		t.Fatalf("ready = %v, want only %s", beadIDsOf(resp.Items), ready.ID)
	}
	if backing.readyCalls != 0 {
		t.Fatalf("backing Ready scans = %d, want the invalidated row refreshed from the cache path", backing.readyCalls)
	}
}
