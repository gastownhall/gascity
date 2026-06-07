package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
)

// counterBeadStore is a Store + Counter fake for the status work-count path.
// listForbidden makes List fail the test, proving the count path was taken.
type counterBeadStore struct {
	beads.Store
	t             *testing.T
	counts        map[string]int // status → count
	countErr      error
	listForbidden bool
	gotExcludes   []string
}

func (s *counterBeadStore) Count(_ context.Context, query beads.ListQuery, excludeTypes ...string) (int, error) {
	s.gotExcludes = excludeTypes
	if s.countErr != nil {
		return 0, s.countErr
	}
	return s.counts[query.Status], nil
}

func (s *counterBeadStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	if s.listForbidden {
		s.t.Error("List called on Counter-capable store, want Count path")
		return nil, nil
	}
	return s.Store.List(q)
}

func getStatus(t *testing.T, state *fakeState) statusResponse {
	t.Helper()
	return getStatusFrom(t, newTestCityHandler(t, state), state)
}

// getStatusFrom fetches /status through an existing handler so tests can
// issue multiple requests against one handler's response cache.
func getStatusFrom(t *testing.T, h http.Handler, state *fakeState) statusResponse {
	t.Helper()
	req := httptest.NewRequest("GET", cityURL(state, "/status"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var resp statusResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return resp
}

func TestHandleStatusWorkCountsUseCounterStores(t *testing.T) {
	state := newFakeState(t)
	counter := &counterBeadStore{
		Store:         beads.NewMemStore(),
		t:             t,
		counts:        map[string]int{"open": 2, "in_progress": 1, "ready": 0},
		listForbidden: true,
	}
	state.stores["myrig"] = counter

	resp := getStatus(t, state)

	if resp.Work.Open != 2 || resp.Work.InProgress != 1 || resp.Work.Ready != 0 {
		t.Fatalf("Work = %+v, want open=2 in_progress=1 ready=0", resp.Work)
	}
	if resp.Partial {
		t.Fatalf("Partial = true, want false; errors: %v", resp.PartialErrors)
	}
	if !slices.Equal(counter.gotExcludes, statusWorkExcludedTypes) {
		t.Fatalf("excludeTypes = %v, want %v (infrastructure beads are not work)", counter.gotExcludes, statusWorkExcludedTypes)
	}
}

func TestHandleStatusCounterUnsupportedFallsBackToList(t *testing.T) {
	state := newFakeState(t)
	mem := beads.NewMemStore()
	if _, err := mem.Create(beads.Bead{Type: "task", Title: "open work"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	state.stores["myrig"] = &counterBeadStore{
		Store:    mem,
		t:        t,
		countErr: beads.ErrCountUnsupported,
	}

	resp := getStatus(t, state)

	if resp.Work.Open != 1 {
		t.Fatalf("Work.Open = %d, want 1 from List fallback", resp.Work.Open)
	}
	if resp.Partial {
		t.Fatalf("Partial = true, want false; errors: %v", resp.PartialErrors)
	}
}

func TestHandleStatusCounterFailureReportsPartialWithoutListRetry(t *testing.T) {
	state := newFakeState(t)
	state.stores["myrig"] = &counterBeadStore{
		Store:         beads.NewMemStore(),
		t:             t,
		countErr:      errors.New("dolt connection refused"),
		listForbidden: true, // operational failures must not pay a second 1s timeout on List
	}

	resp := getStatus(t, state)

	if !resp.Partial {
		t.Fatal("Partial = false, want true for count failure")
	}
	found := false
	for _, e := range resp.PartialErrors {
		if strings.Contains(e, "myrig") && strings.Contains(e, "work") {
			found = true
		}
	}
	if !found {
		t.Fatalf("PartialErrors = %v, want rig work error", resp.PartialErrors)
	}
}

func TestHandleStatusServesRecentResponseDespiteIndexAdvance(t *testing.T) {
	// Pin the time-bucket cache off so the TTL floor alone carries the
	// assertion: with the default 2s bucket both requests would land in the
	// same bucket and the bucket cache would serve the body before the floor
	// is ever consulted, masking a floor regression.
	oldTTL := timeBucketResponseCacheTTL
	timeBucketResponseCacheTTL = time.Nanosecond // bucket rolls every request
	t.Cleanup(func() { timeBucketResponseCacheTTL = oldTTL })

	state := newFakeState(t)
	counter := &counterBeadStore{
		Store:         beads.NewMemStore(),
		t:             t,
		counts:        map[string]int{"open": 2},
		listForbidden: true,
	}
	state.stores["myrig"] = counter
	h := newTestCityHandler(t, state)

	first := getStatusFrom(t, h, state)
	if first.Work.Open != 2 {
		t.Fatalf("Work.Open = %d, want 2", first.Work.Open)
	}

	// Advance the event index and change the underlying counts. A
	// non-blocking request inside the TTL floor must serve the recent
	// cached body instead of paying a full rebuild (#1896).
	state.eventProv.(*events.Fake).Record(events.Event{Type: "test.event", Actor: "test"})
	counter.counts["open"] = 7

	second := getStatusFrom(t, h, state)
	if second.Work.Open != 2 {
		t.Fatalf("Work.Open = %d, want 2 (cached within TTL floor)", second.Work.Open)
	}
}
