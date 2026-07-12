package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// TestBeadReadyServedFromCacheWhenBackingReadyFails proves GET /v0/beads/ready
// with cached=true serves ready work from the supervisor cache projection, not
// the live backing store. The control dispatcher's api.Client.ListReadyBeads
// sets cached=true and depends on this so a slow or failing backing store
// cannot block or fail every control serve cycle.
//
// The backing store below fails Ready() while its cached projection holds a
// ready control bead; with cached=true the handler must still return that bead
// without a partial error. The default (live) path is covered by
// TestBeadReadyUsesLiveLookup, which proves an omitted cached param reads the
// authoritative backing store so closed/blocked beads are never reported ready.
func TestBeadReadyServedFromCacheWhenBackingReadyFails(t *testing.T) {
	// Backing store whose Ready() always fails but whose List() (used by the
	// cache prime) still works, so the cache primes while the live read stays
	// broken — the exact split the handler must honor.
	backing := beads.NewMemStore()
	control, err := backing.Create(beads.Bead{Type: "task", Title: "control-ready-bead"})
	if err != nil {
		t.Fatalf("seed control bead: %v", err)
	}
	failing := &failingBeadStore{Store: backing, readyErr: errors.New("backing Ready() unavailable")}

	cache := beads.NewCachingStoreForTest(failing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("prime cache: %v", err)
	}

	// Test premise: the live backing read is broken, but the cached projection
	// holds the ready control bead. If this split ever stops holding, the
	// handler assertion below would pass for the wrong reason.
	if _, err := beads.HandlesFor(cache).Live.Ready(); err == nil {
		t.Fatalf("Live.Ready() = nil error, want backing failure (test premise broken)")
	}
	cachedReady, err := beads.HandlesFor(cache).Cached.Ready()
	if err != nil {
		t.Fatalf("Cached.Ready() unexpected error: %v", err)
	}
	if !containsBeadTitle(cachedReady, "control-ready-bead") {
		t.Fatalf("cached ready = %+v, want control bead %s", cachedReady, control.ID)
	}

	// Control beads live in the city store; federate the cache there.
	fs := newFakeState(t)
	fs.cityBeadStore = cache
	h := newTestCityHandler(t, fs)

	req := httptest.NewRequest("GET", cityURL(fs, "/beads/ready?cached=true"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 (body=%q)", rec.Code, rec.Body.String())
	}

	var body struct {
		Items         []beads.Bead `json:"items"`
		Partial       bool         `json:"partial"`
		PartialErrors []string     `json:"partial_errors"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v (body=%q)", err, rec.Body.String())
	}
	if body.Partial {
		t.Errorf("Partial = true (errors=%v), want false: the cached read should succeed without touching the failing backing store", body.PartialErrors)
	}
	if !containsBeadTitle(body.Items, "control-ready-bead") {
		t.Fatalf("Items = %+v, want control bead %s served from cache", body.Items, control.ID)
	}
}

// trackingBackingStore counts and fails backing-store reads. A test uses it to
// prove the cache-only ready path (ReadyCacheOnly / GET ?cached=true) never
// primes from or reads the backing store, and to confirm — by contrast — that
// the priming Cached.Ready handle does reach it.
type trackingBackingStore struct {
	beads.Store
	listCalls  int
	readyCalls int
}

func (s *trackingBackingStore) List(beads.ListQuery) ([]beads.Bead, error) {
	s.listCalls++
	return nil, errors.New("backing List must not be reached on the cache-only ready path")
}

func (s *trackingBackingStore) Ready(...beads.ReadyQuery) ([]beads.Bead, error) {
	s.readyCalls++
	return nil, errors.New("backing Ready must not be reached on the cache-only ready path")
}

// TestBeadReadyCachedColdCacheNeverPrimesBackingStore is the blocker regression
// for PR #3821: on a cold (unprimed) supervisor cache, GET /v0/beads/ready?cached=true
// must answer strictly from the in-memory projection and never prime or read the
// failing/blocking backing store. Otherwise the control dispatcher's tight serve
// loop could block on the exact backing-store scan the control-ready cache exists
// to avoid. The priming Cached.Ready handle, by contrast, does reach the backing
// store — that is the gap ReadyCacheOnly closes.
func TestBeadReadyCachedColdCacheNeverPrimesBackingStore(t *testing.T) {
	backing := &trackingBackingStore{Store: beads.NewMemStore()}
	cache := beads.NewCachingStoreForTest(backing, nil)
	// No real backoff sleeps: the failing prime would otherwise retry over ~15s.
	cache.SetPrimeRetryDelayForTest(func(int) time.Duration { return 0 })
	// Cold: the cache is never primed.

	handles := beads.HandlesFor(cache)

	// Contrast: the priming Ready handle reaches the backing store — this is the
	// blocking/failing scan the cache-only path must avoid.
	if _, err := handles.Cached.Ready(); !errors.Is(err, beads.ErrCacheUnavailable) {
		t.Fatalf("Cached.Ready() err = %v, want ErrCacheUnavailable on a cold cache", err)
	}
	if backing.listCalls == 0 {
		t.Fatal("Cached.Ready() never reached the backing store; test premise broken (it should prime)")
	}

	// ReadyCacheOnly must not prime: no new backing reads, ever.
	primeListCalls := backing.listCalls
	rows, err := handles.Cached.ReadyCacheOnly()
	if !errors.Is(err, beads.ErrCacheUnavailable) {
		t.Fatalf("ReadyCacheOnly() err = %v, want ErrCacheUnavailable on a cold cache", err)
	}
	if len(rows) != 0 {
		t.Fatalf("ReadyCacheOnly() rows = %+v, want none on a cold cache", rows)
	}
	if backing.listCalls != primeListCalls || backing.readyCalls != 0 {
		t.Fatalf("ReadyCacheOnly() touched the backing store: listCalls %d->%d, readyCalls=%d",
			primeListCalls, backing.listCalls, backing.readyCalls)
	}

	// Through the handler: GET /beads/ready?cached=true on the cold cache must not
	// touch the backing store, and must surface the unavailability as a partial
	// read that omits the city store, so the control dispatcher backs off and
	// retries instead of acting on a falsely-idle queue.
	backing.listCalls = 0
	backing.readyCalls = 0
	fs := newFakeState(t)
	fs.cityBeadStore = cache
	h := newTestCityHandler(t, fs)

	req := httptest.NewRequest("GET", cityURL(fs, "/beads/ready?cached=true"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if backing.listCalls != 0 || backing.readyCalls != 0 {
		t.Fatalf("handler touched the backing store on a cold cached read: listCalls=%d readyCalls=%d",
			backing.listCalls, backing.readyCalls)
	}
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 (body=%q)", rec.Code, rec.Body.String())
	}
	var body struct {
		Items         []beads.Bead `json:"items"`
		Partial       bool         `json:"partial"`
		PartialErrors []string     `json:"partial_errors"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v (body=%q)", err, rec.Body.String())
	}
	if !body.Partial {
		t.Errorf("Partial = false, want true: a cold city cache read is non-authoritative")
	}
	if len(body.Items) != 0 {
		t.Errorf("Items = %+v, want none: the cold cache must not fabricate ready work", body.Items)
	}
	foundCity := false
	for _, e := range body.PartialErrors {
		if strings.HasPrefix(e, CityReadyPartialLabel+": ") {
			foundCity = true
		}
	}
	if !foundCity {
		t.Errorf("PartialErrors = %v, want a city-store entry so CityReadAuthoritative is false", body.PartialErrors)
	}
}

// TestBeadReadyControlFilterAppliesServerSide proves the control dispatcher's
// assignee/route predicate is applied server-side, before serialization, when
// the request carries control_assignees/control_routes. Only the dispatcher's own
// control work crosses the wire; every other rig's ready bead is filtered out by
// the supervisor instead of being shipped for the dispatcher to discard.
func TestBeadReadyControlFilterAppliesServerSide(t *testing.T) {
	city := beads.NewMemStore()
	for _, b := range []beads.Bead{
		{Type: "task", Title: "mine-assigned", Assignee: "ctl-session"},
		{Type: "task", Title: "other-assigned", Assignee: "someone-else"},
		{Type: "task", Title: "mine-routed", Metadata: map[string]string{beadmeta.RunTargetMetadataKey: "ctl-route"}},
		{Type: "task", Title: "unrelated-routed", Metadata: map[string]string{beadmeta.RunTargetMetadataKey: "other-route"}},
	} {
		if _, err := city.Create(b); err != nil {
			t.Fatalf("seed %q: %v", b.Title, err)
		}
	}

	fs := newFakeState(t)
	fs.cityBeadStore = city
	h := newTestCityHandler(t, fs)

	req := httptest.NewRequest("GET", cityURL(fs, "/beads/ready?cached=true&control_assignees=ctl-session&control_routes=ctl-route&control_limit=10"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 (body=%q)", rec.Code, rec.Body.String())
	}
	var body struct {
		Items []beads.Bead `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v (body=%q)", err, rec.Body.String())
	}
	got := make(map[string]bool, len(body.Items))
	var titles []string
	for _, b := range body.Items {
		got[b.Title] = true
		titles = append(titles, b.Title)
	}
	if !got["mine-assigned"] || !got["mine-routed"] {
		t.Errorf("Items = %v, want both mine-assigned (assignee match) and mine-routed (route match)", titles)
	}
	if got["other-assigned"] || got["unrelated-routed"] {
		t.Errorf("Items = %v, want the server-side filter to drop other-assigned and unrelated-routed", titles)
	}
}
