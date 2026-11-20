package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

type beadPointFreshBacking struct {
	beads.Store
	getErr   error
	getCalls int
}

func (s *beadPointFreshBacking) Get(id string) (beads.Bead, error) {
	s.getCalls++
	if s.getErr != nil {
		return beads.Bead{}, s.getErr
	}
	return s.Store.Get(id)
}

func beadPointFreshFixture(t *testing.T) (*fakeMutatorState, *beads.MemStore, *beadPointFreshBacking, *beads.CachingStore, beads.Bead) {
	t.Helper()
	base := beads.NewMemStore()
	row, err := base.Create(beads.Bead{Title: "Exact owned completion input", Status: "open", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	backing := &beadPointFreshBacking{Store: base}
	cache := beads.NewCachingStore(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatal(err)
	}
	state := newFakeMutatorState(t)
	state.cityBeadStore = cache
	return state, base, backing, cache, row
}

func TestBeadFreshPointReadSeesOutOfBandNativeClose(t *testing.T) {
	state, base, backing, cache, row := beadPointFreshFixture(t)
	if err := base.Close(row.ID); err != nil {
		t.Fatal(err)
	}
	held, err := cache.Get(row.ID)
	if err != nil || held.Status != "open" {
		t.Fatalf("fixture did not retain the pre-close cache row: %+v %v", held, err)
	}
	h := newTestCityHandler(t, state)
	before := backing.getCalls
	cached := httptest.NewRecorder()
	h.ServeHTTP(cached, httptest.NewRequest("GET", cityURL(state, "/bead/")+row.ID, nil))
	var cachedRow beads.Bead
	if err := json.Unmarshal(cached.Body.Bytes(), &cachedRow); err != nil {
		t.Fatal(err)
	}
	if cached.Code != http.StatusOK || cachedRow.Status != "open" || backing.getCalls != before {
		t.Fatalf("default snapshot read changed: status=%d row=%+v reads=%d before=%d", cached.Code, cachedRow, backing.getCalls, before)
	}
	fresh := httptest.NewRecorder()
	h.ServeHTTP(fresh, httptest.NewRequest("GET", cityURL(state, "/bead/")+row.ID+"?fresh=true", nil))
	var freshRow beads.Bead
	if err := json.Unmarshal(fresh.Body.Bytes(), &freshRow); err != nil {
		t.Fatal(err)
	}
	if fresh.Code != http.StatusOK || freshRow.ID != row.ID || freshRow.Status != "closed" || backing.getCalls != before+1 {
		t.Fatalf("fresh point read returned an older cached completion: status=%d row=%+v reads=%d before=%d", fresh.Code, freshRow, backing.getCalls, before)
	}
	if fresh.Header().Get("X-GC-Cache-Age-S") != "0" {
		t.Fatalf("live point read claimed a cached snapshot age: %q", fresh.Header().Get("X-GC-Cache-Age-S"))
	}
}

func TestBeadFreshPointReadFailureDoesNotFallBackToOpenCache(t *testing.T) {
	state, _, backing, _, row := beadPointFreshFixture(t)
	backing.getErr = errors.New("authoritative point read unavailable")
	h := newTestCityHandler(t, state)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", cityURL(state, "/bead/")+row.ID+"?fresh=true", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("failed authoritative read became cached success: status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestBeadFreshPointReadMissingDoesNotResurrectCachedRow(t *testing.T) {
	state, base, _, _, row := beadPointFreshFixture(t)
	if err := base.Delete(row.ID); err != nil {
		t.Fatal(err)
	}
	h := newTestCityHandler(t, state)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", cityURL(state, "/bead/")+row.ID+"?fresh=true", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing authoritative row became cached success: status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestBeadFreshPointReadOptionHasTypedValidation(t *testing.T) {
	state, _, backing, _, row := beadPointFreshFixture(t)
	h := newTestCityHandler(t, state)
	before := backing.getCalls
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", cityURL(state, "/bead/")+row.ID+"?fresh=invalid", nil))
	if rec.Code != http.StatusUnprocessableEntity || backing.getCalls != before {
		t.Fatalf("invalid fresh option reached a read: status=%d reads=%d before=%d", rec.Code, backing.getCalls, before)
	}
}
