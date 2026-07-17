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
	gotStatuses   []string
}

type contextBlockingCounterStore struct {
	beads.Store
	t *testing.T
}

func (s *contextBlockingCounterStore) Count(ctx context.Context, _ beads.ListQuery, _ ...string) (int, error) {
	<-ctx.Done()
	return 0, ctx.Err()
}

func (s *contextBlockingCounterStore) List(beads.ListQuery) ([]beads.Bead, error) {
	s.t.Error("List called after operational Count timeout")
	return nil, nil
}

type contextIgnoringCounterStore struct {
	beads.Store
	entered chan struct{}
	release chan struct{}
	exited  chan struct{}
}

func (s *contextIgnoringCounterStore) Count(context.Context, beads.ListQuery, ...string) (int, error) {
	close(s.entered)
	<-s.release
	close(s.exited)
	return 0, errors.New("released context-ignoring Count")
}

type partialCounterStore struct {
	beads.Store
}

func testReadyContext(ctx context.Context, store beads.Store, query ...beads.ReadyQuery) ([]beads.Bead, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return store.Ready(query...)
}

func (s *counterBeadStore) ReadyContext(ctx context.Context, query ...beads.ReadyQuery) ([]beads.Bead, error) {
	return testReadyContext(ctx, s.Store, query...)
}

func (s *contextBlockingCounterStore) ReadyContext(ctx context.Context, query ...beads.ReadyQuery) ([]beads.Bead, error) {
	return testReadyContext(ctx, s.Store, query...)
}

func (s *contextIgnoringCounterStore) ReadyContext(ctx context.Context, query ...beads.ReadyQuery) ([]beads.Bead, error) {
	return testReadyContext(ctx, s.Store, query...)
}

func (s *partialCounterStore) ReadyContext(ctx context.Context, query ...beads.ReadyQuery) ([]beads.Bead, error) {
	return testReadyContext(ctx, s.Store, query...)
}

func (s *partialCounterStore) Count(_ context.Context, query beads.ListQuery, _ ...string) (int, error) {
	if query.Status == "open" {
		return 7, nil
	}
	return 0, errors.New("in-progress count unavailable")
}

func (s *counterBeadStore) Count(_ context.Context, query beads.ListQuery, excludeTypes ...string) (int, error) {
	s.gotExcludes = excludeTypes
	s.gotStatuses = append(s.gotStatuses, query.Status)
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
	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{Type: "task", Title: "ready work"}); err != nil {
		t.Fatalf("Create ready work: %v", err)
	}
	blocker, err := store.Create(beads.Bead{Type: "task", Title: "claimed blocker", Status: "in_progress"})
	if err != nil {
		t.Fatalf("Create blocker: %v", err)
	}
	inProgress := "in_progress"
	if err := store.Update(blocker.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatalf("Update blocker: %v", err)
	}
	blocked, err := store.Create(beads.Bead{Type: "task", Title: "blocked work"})
	if err != nil {
		t.Fatalf("Create blocked work: %v", err)
	}
	if err := store.DepAdd(blocked.ID, blocker.ID, "blocks"); err != nil {
		t.Fatalf("DepAdd: %v", err)
	}
	future := time.Now().UTC().Add(time.Hour)
	for _, bead := range []beads.Bead{
		{Type: "message", Title: "infrastructure message"},
		{Type: "task", Title: "ephemeral work", Ephemeral: true},
		{Type: "task", Title: "deferred work", DeferUntil: &future},
	} {
		if _, err := store.Create(bead); err != nil {
			t.Fatalf("Create excluded ready candidate %q: %v", bead.Title, err)
		}
	}
	counter := &counterBeadStore{
		Store:         store,
		t:             t,
		counts:        map[string]int{"open": 2, "in_progress": 1, "ready": 99},
		listForbidden: true,
	}
	state.stores["myrig"] = counter

	resp := getStatus(t, state)

	if resp.Work.Open != 2 || resp.Work.InProgress != 1 || resp.Work.Ready != 1 {
		t.Fatalf("Work = %+v, want open=2 in_progress=1 ready=1", resp.Work)
	}
	if resp.Partial {
		t.Fatalf("Partial = true, want false; errors: %v", resp.PartialErrors)
	}
	if !slices.Equal(counter.gotExcludes, statusWorkExcludedTypes) {
		t.Fatalf("excludeTypes = %v, want %v (infrastructure beads are not work)", counter.gotExcludes, statusWorkExcludedTypes)
	}
	if slices.Contains(counter.gotStatuses, "ready") {
		t.Fatalf("Count statuses = %v, must not query the nonexistent stored status ready", counter.gotStatuses)
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

	if resp.Work.Open != 1 || resp.Work.Ready != 1 {
		t.Fatalf("Work = %+v, want open=1 ready=1 from List and canonical Ready fallbacks", resp.Work)
	}
	if resp.Partial {
		t.Fatalf("Partial = true, want false; errors: %v", resp.PartialErrors)
	}
}

func TestHandleStatusReadyDeduplicatesAcrossStoresLikeCanonicalEndpoint(t *testing.T) {
	state := newFakeState(t)
	first := beads.NewMemStore()
	second := beads.NewMemStore()
	for name, store := range map[string]*beads.MemStore{"alpha": first, "beta": second} {
		created, err := store.Create(beads.Bead{Type: "task", Title: name + " ready work"})
		if err != nil {
			t.Fatalf("Create(%s): %v", name, err)
		}
		if created.ID != "gc-1" {
			t.Fatalf("Create(%s) ID = %q, want shared fixture ID gc-1", name, created.ID)
		}
	}
	state.stores = map[string]beads.Store{"alpha": first, "beta": second}

	status := getStatus(t, state)
	if status.Work.Open != 2 {
		t.Fatalf("Work.Open = %d, want existing per-store sum 2", status.Work.Open)
	}
	if status.Work.Ready != 1 {
		t.Fatalf("Work.Ready = %d, want shared ready ID deduplicated", status.Work.Ready)
	}

	h := newTestCityHandler(t, state)
	req := httptest.NewRequest(http.MethodGet, cityURL(state, "/beads/ready"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("ready status = %d, want %d", rec.Code, http.StatusOK)
	}
	var ready struct {
		Total int `json:"total"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&ready); err != nil {
		t.Fatalf("decode ready: %v", err)
	}
	if status.Work.Ready != ready.Total {
		t.Fatalf("status ready = %d, canonical ready total = %d", status.Work.Ready, ready.Total)
	}
}

func TestHandleStatusReadyFederatesCityStoreLikeCanonicalEndpoint(t *testing.T) {
	state := newFakeState(t)
	state.stores = map[string]beads.Store{}
	state.cityBeadStore = beads.NewMemStore()
	if _, err := state.cityBeadStore.Create(beads.Bead{Type: "task", Title: "city-only ready work"}); err != nil {
		t.Fatalf("Create city ready work: %v", err)
	}
	h := newTestCityHandler(t, state)

	status := getStatusFrom(t, h, state)
	req := httptest.NewRequest(http.MethodGet, cityURL(state, "/beads/ready"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("ready status = %d, want %d", rec.Code, http.StatusOK)
	}
	var ready struct {
		Total int `json:"total"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&ready); err != nil {
		t.Fatalf("decode ready: %v", err)
	}
	if status.Work.Ready != 1 || status.Work.Ready != ready.Total {
		t.Fatalf("status ready = %d, canonical city-ready total = %d, want both 1", status.Work.Ready, ready.Total)
	}
}

func TestHandleStatusCounterFailureReportsPartialWithoutListRetry(t *testing.T) {
	state := newFakeState(t)
	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{Type: "task", Title: "ready survivor"}); err != nil {
		t.Fatalf("Create ready survivor: %v", err)
	}
	state.stores["myrig"] = &counterBeadStore{
		Store:         store,
		t:             t,
		countErr:      errors.New("dolt connection refused"),
		listForbidden: true, // operational failures must not pay a second 1s timeout on List
	}

	resp := getStatus(t, state)

	if !resp.Partial {
		t.Fatal("Partial = false, want true for count failure")
	}
	if resp.Work.Ready != 1 {
		t.Fatalf("Work.Ready = %d, want canonical ready survivor despite count failure", resp.Work.Ready)
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

func TestStatusStoreWorkCountsPreservesReadyWhenCounterTimesOut(t *testing.T) {
	oldTimeout := statusStoreReadTimeout
	statusStoreReadTimeout = 200 * time.Millisecond
	t.Cleanup(func() { statusStoreReadTimeout = oldTimeout })

	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{Type: "task", Title: "ready survivor"}); err != nil {
		t.Fatalf("Create ready survivor: %v", err)
	}
	state := newFakeState(t)
	result := statusStoreWorkCounts(context.Background(), state, "myrig", &contextBlockingCounterStore{
		Store: store,
		t:     t,
	})

	if result.wc.Ready != 1 {
		t.Fatalf("Ready = %d, want 1 even when persisted Count consumes the deadline", result.wc.Ready)
	}
	if len(result.errs) == 0 {
		t.Fatal("errors empty, want Count timeout reported as partial")
	}
}

func TestStatusStoreWorkCountsBoundsContextIgnoringCounter(t *testing.T) {
	oldTimeout := statusStoreReadTimeout
	statusStoreReadTimeout = 100 * time.Millisecond
	t.Cleanup(func() { statusStoreReadTimeout = oldTimeout })

	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{Type: "task", Title: "ready survivor"}); err != nil {
		t.Fatalf("Create ready survivor: %v", err)
	}
	blocking := &contextIgnoringCounterStore{
		Store:   store,
		entered: make(chan struct{}),
		release: make(chan struct{}),
		exited:  make(chan struct{}),
	}
	released := false
	defer func() {
		if !released {
			close(blocking.release)
		}
	}()

	state := newFakeState(t)
	resultDone := make(chan statusWorkResult, 1)
	go func() {
		resultDone <- statusStoreWorkCounts(context.Background(), state, "myrig", blocking)
	}()
	select {
	case <-blocking.entered:
	case <-time.After(time.Second):
		t.Fatal("Count was not entered")
	}

	var result statusWorkResult
	select {
	case result = <-resultDone:
	case <-time.After(time.Second):
		t.Fatal("statusStoreWorkCounts did not honor its outer deadline")
	}
	if result.wc.Ready != 1 {
		t.Fatalf("Ready = %d, want completed Ready survivor", result.wc.Ready)
	}
	if len(result.errs) == 0 {
		t.Fatal("errors empty, want context-ignoring Count timeout reported")
	}

	close(blocking.release)
	released = true
	select {
	case <-blocking.exited:
	case <-time.After(time.Second):
		t.Fatal("context-ignoring Count goroutine did not exit after release")
	}
}

func TestStatusStoreWorkCountsPreservesPartialCounterBuckets(t *testing.T) {
	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{Type: "task", Title: "ready survivor"}); err != nil {
		t.Fatalf("Create ready survivor: %v", err)
	}
	result := statusStoreWorkCounts(context.Background(), newFakeState(t), "myrig", &partialCounterStore{Store: store})

	if result.wc.Open != 7 || result.wc.Ready != 1 {
		t.Fatalf("Work = %+v, want open=7 and ready=1 survivors", result.wc)
	}
	if len(result.errs) == 0 {
		t.Fatal("errors empty, want in-progress Count failure reported")
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
