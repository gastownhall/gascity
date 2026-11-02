package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
)

// cityCacheOf returns the CachingStore the controller serves the city's bead
// reads from, through the bead-policy layer wrapWithCachingStoreFeed puts back
// over it. It fails the test when the real wiring no longer has that shape, so
// the age assertions below can never pass vacuously.
func cityCacheOf(t *testing.T, cs *controllerState) *beads.CachingStore {
	t.Helper()
	cs.mu.RLock()
	store := cs.cityBeadStore
	cs.mu.RUnlock()
	inner, policy, wrapped := unwrapBeadPolicyStore(store)
	if !wrapped || policy == nil {
		t.Fatalf("precondition: city store %T is not the bead-policy layer", store)
	}
	cache, ok := inner.(*beads.CachingStore)
	if !ok {
		t.Fatalf("precondition: the bead-policy layer wraps %T, want *beads.CachingStore", inner)
	}
	if cache.Stats().LastFreshAt.IsZero() {
		t.Fatal("precondition: the city cache has never taken in fresh data")
	}
	return cache
}

// cityAPI serves the controller's API in-process, the way the controller and
// supervisor mount it.
func cityAPI(cs *controllerState) http.Handler {
	return api.NewSupervisorMux(&singleCityStateResolver{state: cs}, nil, false, "controller", "test", time.Now()).
		WithAnyHostAllowed().
		Handler()
}

func getCityPath(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v0/city/"+charCityName+path, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func cacheAgeHeaderOf(t *testing.T, rec *httptest.ResponseRecorder, path string) float64 {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200: %s", path, rec.Code, rec.Body.String())
	}
	raw := rec.Header().Get("X-GC-Cache-Age-S")
	if raw == "" {
		return 0
	}
	age, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		t.Fatalf("GET %s: X-GC-Cache-Age-S = %q: %v", path, raw, err)
	}
	return age
}

func jsonCacheAgeOf(t *testing.T, what string, out []byte) float64 {
	t.Helper()
	var doc struct {
		CacheAgeS *float64 `json:"_cache_age_s"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("%s: decoding JSON: %v\n%s", what, err, out)
	}
	if doc.CacheAgeS == nil {
		t.Fatalf("%s: JSON has no _cache_age_s:\n%s", what, out)
	}
	return *doc.CacheAgeS
}

// beadsListJSONVia runs gc beads list --format=json through lane's API client
// and returns its stdout.
func beadsListJSONVia(t *testing.T, h *charHarness, lane charLane) []byte {
	t.Helper()
	var stdout, stderr bytes.Buffer
	if code := routeBeadsList(h.cityPath, lane.client, "", "json", beadFilters{}, &stdout, &stderr); code != 0 {
		t.Fatalf("%s: beads list --format=json exit %d: %s", lane.name, code, stderr.String())
	}
	return stdout.Bytes()
}

// beadsListCacheAgeVia returns the _cache_age_s gc beads list --format=json
// prints through lane. Only a remote target prints the {beads, _cache_age_s}
// envelope (#8699); a local target prints the bare array on every route, so
// for a local lane it fails the test unless the output is that bare array
// with no _cache_age_s, and reports ok=false.
func beadsListCacheAgeVia(t *testing.T, h *charHarness, lane charLane) (age float64, ok bool) {
	t.Helper()
	what := lane.name + ": beads list --format=json"
	out := beadsListJSONVia(t, h, lane)
	if lane.client.IsRemote() {
		return jsonCacheAgeOf(t, what, out), true
	}
	var rows []json.RawMessage
	if err := json.Unmarshal(out, &rows); err != nil {
		t.Fatalf("%s: a local target must print the bare array: %v\n%s", what, err, out)
	}
	if bytes.Contains(out, []byte("_cache_age_s")) {
		t.Fatalf("%s: a local target printed _cache_age_s:\n%s", what, out)
	}
	return 0, false
}

func apiLanes(t *testing.T, h *charHarness) []charLane {
	t.Helper()
	var out []charLane
	for _, lane := range h.lanes(t) {
		if lane.client != nil {
			out = append(out, lane)
		}
	}
	return out
}

// cityStatusVia runs gc status through lane's API client and returns its
// stdout, failing the test if it falls back to the local route.
func cityStatusVia(t *testing.T, h *charHarness, lane charLane, jsonOut bool) []byte {
	t.Helper()
	var stdout, stderr bytes.Buffer
	fallback := func() int {
		t.Errorf("%s: gc status fell back to the local route: %s", lane.name, stderr.String())
		return 1
	}
	if code := routeCityStatus(h.cityPath, h.cs.Config(), newFakeDrainOps(), lane.client, "", jsonOut, &stdout, &stderr, fallback); code != 0 {
		t.Fatalf("%s: gc status (json=%v) exit %d: %s", lane.name, jsonOut, code, stderr.String())
	}
	return stdout.Bytes()
}

const cacheAgeFooter = "reconciler may be lagging"

// cacheAgeCity is the harness city with a fake session provider, so the
// session list read below has a provider to observe.
const cacheAgeCity = charCityBasic + "\n[session]\nprovider = \"fake\"\n"

// The API reports the real age of the city's bead cache through the store
// stack the controller and supervisor serve (bead-policy layer over a
// CachingStore), and the CLI prints it as _cache_age_s and, past 30 s, as the
// cache-age footer. gc beads list --format=json prints _cache_age_s only for a
// remote target: a local city prints the bare array on every route, so there
// the age shows only in the footer. The rig list is built from config, the
// suspension state and runtime probes, not from the cache, so it reports no
// age and prints no footer however old the cache is. Neither does a suspended
// city's status, which is built without reading any bead store.
func TestCityAPIReportsTheCityCacheAge(t *testing.T) {
	for _, tc := range []struct {
		name     string
		ageBy    time.Duration
		min, max float64
		footer   bool
	}{
		{name: "aged", ageBy: 45 * time.Second, min: 30, max: 45, footer: true},
		{name: "fresh", ageBy: 5 * time.Second, min: 0, max: 5, footer: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newCharCity(t, cacheAgeCity, nil)
			cache := cityCacheOf(t, h.cs)
			t.Cleanup(api.SetLivenessClockForTest(&clock.Fake{Time: cache.Stats().LastFreshAt.Add(tc.ageBy)}))
			inRange := func(what string, age float64) {
				t.Helper()
				if age <= tc.min || age > tc.max {
					t.Errorf("%s: cache age = %v, want in (%v, %v]", what, age, tc.min, tc.max)
				}
			}

			srv := cityAPI(h.cs)
			for _, path := range []string{"/beads", "/sessions", "/status"} {
				inRange("GET "+path+" X-GC-Cache-Age-S", cacheAgeHeaderOf(t, getCityPath(t, srv, path), path))
			}
			if age := cacheAgeHeaderOf(t, getCityPath(t, srv, "/rigs"), "/rigs"); age != 0 {
				t.Errorf("GET /rigs X-GC-Cache-Age-S = %v, want 0: the rig list is not read from the bead cache", age)
			}

			lanes := apiLanes(t, h)
			var remoteLanes, localLanes int
			for _, lane := range lanes {
				if age, ok := beadsListCacheAgeVia(t, h, lane); ok {
					remoteLanes++
					inRange(lane.name+": beads list --format=json", age)
				} else {
					localLanes++
				}

				var stdout, stderr bytes.Buffer
				if code := routeBeadsList(h.cityPath, lane.client, "", "text", beadFilters{}, &stdout, &stderr); code != 0 {
					t.Fatalf("%s: beads list exit %d: %s", lane.name, code, stderr.String())
				}
				if got := strings.Contains(stdout.String(), cacheAgeFooter); got != tc.footer {
					t.Errorf("%s: beads list footer shown = %v, want %v:\n%s", lane.name, got, tc.footer, stdout.String())
				}

				stdout.Reset()
				stderr.Reset()
				if code := routeRigList(h.cityPath, lane.client, "", true, &stdout, &stderr); code != 0 {
					t.Fatalf("%s: rig list --json exit %d: %s", lane.name, code, stderr.String())
				}
				if age := jsonCacheAgeOf(t, lane.name+": rig list --json", stdout.Bytes()); age != 0 {
					t.Errorf("%s: rig list --json _cache_age_s = %v, want 0: the rig list is not read from the bead cache", lane.name, age)
				}

				stdout.Reset()
				stderr.Reset()
				if code := routeRigList(h.cityPath, lane.client, "", false, &stdout, &stderr); code != 0 {
					t.Fatalf("%s: rig list exit %d: %s", lane.name, code, stderr.String())
				}
				if strings.Contains(stdout.String(), cacheAgeFooter) {
					t.Errorf("%s: rig list shows the cache-age footer over output not read from the bead cache:\n%s", lane.name, stdout.String())
				}

				inRange(lane.name+": status --json", jsonCacheAgeOf(t, lane.name+": status --json", cityStatusVia(t, h, lane, true)))
				out := cityStatusVia(t, h, lane, false)
				if got := bytes.Contains(out, []byte(cacheAgeFooter)); got != tc.footer {
					t.Errorf("%s: status footer shown = %v, want %v:\n%s", lane.name, got, tc.footer, out)
				}
			}

			if remoteLanes == 0 || localLanes == 0 {
				t.Fatalf("precondition: beads list ran over %d remote and %d local API lanes, want at least one of each", remoteLanes, localLanes)
			}

			var stdout, stderr bytes.Buffer
			remote := lanes[0]
			if code := routeSessionList(h.cityPath, "", "", remote.client, "", true, &stdout, &stderr); code != 0 {
				t.Fatalf("session list --json exit %d: %s", code, stderr.String())
			}
			inRange("session list --json", jsonCacheAgeOf(t, "session list --json", stdout.Bytes()))
		})
	}

	// A store with no cache under the policy layer reports 0: the forward
	// must not make up an age.
	t.Run("uncached", func(t *testing.T) {
		h := newCharCity(t, charCityBasic, nil)
		cache := cityCacheOf(t, h.cs)
		h.cs.mu.Lock()
		h.cs.cityBeadStore = wrapStoreWithBeadPolicies(beads.NewMemStore(), h.cs.cfg)
		h.cs.mu.Unlock()
		t.Cleanup(api.SetLivenessClockForTest(&clock.Fake{Time: cache.Stats().LastFreshAt.Add(45 * time.Second)}))

		srv := cityAPI(h.cs)
		if age := cacheAgeHeaderOf(t, getCityPath(t, srv, "/beads"), "/beads"); age != 0 {
			t.Errorf("GET /beads X-GC-Cache-Age-S = %v, want 0 over an uncached store", age)
		}
		for _, lane := range apiLanes(t, h) {
			if age, ok := beadsListCacheAgeVia(t, h, lane); ok && age != 0 {
				t.Errorf("%s: beads list --format=json _cache_age_s = %v, want 0 over an uncached store", lane.name, age)
			}
			var stdout, stderr bytes.Buffer
			if code := routeBeadsList(h.cityPath, lane.client, "", "text", beadFilters{}, &stdout, &stderr); code != 0 {
				t.Fatalf("%s: beads list exit %d: %s", lane.name, code, stderr.String())
			}
			if strings.Contains(stdout.String(), cacheAgeFooter) {
				t.Errorf("%s: beads list shows the cache-age footer over an uncached store:\n%s", lane.name, stdout.String())
			}
		}
	})

	// A suspended city's status reads no bead store, so it reports no age and
	// gc status prints no footer however old the paused cache is. GET /beads,
	// read from the cache, still reports its age.
	t.Run("suspended", func(t *testing.T) {
		h := newCharCity(t, cacheAgeCity, nil)
		cache := cityCacheOf(t, h.cs)
		t.Cleanup(api.SetLivenessClockForTest(&clock.Fake{Time: cache.Stats().LastFreshAt.Add(45 * time.Second)}))
		suspendCityAt(t, h.cityPath)

		srv := cityAPI(h.cs)
		if age := cacheAgeHeaderOf(t, getCityPath(t, srv, "/beads"), "/beads"); age <= 30 || age > 45 {
			t.Fatalf("precondition: GET /beads X-GC-Cache-Age-S = %v, want in (30, 45]", age)
		}
		if age := cacheAgeHeaderOf(t, getCityPath(t, srv, "/status"), "/status"); age != 0 {
			t.Errorf("GET /status X-GC-Cache-Age-S = %v on a suspended city, want 0: its body is built without reading any bead store", age)
		}
		for _, lane := range apiLanes(t, h) {
			if age := jsonCacheAgeOf(t, lane.name+": status --json", cityStatusVia(t, h, lane, true)); age != 0 {
				t.Errorf("%s: status --json _cache_age_s = %v on a suspended city, want 0", lane.name, age)
			}
			out := cityStatusVia(t, h, lane, false)
			if !bytes.Contains(out, []byte("Suspended:  yes")) {
				t.Fatalf("precondition: %s: status does not show the city suspended:\n%s", lane.name, out)
			}
			if bytes.Contains(out, []byte(cacheAgeFooter)) {
				t.Errorf("%s: status shows the cache-age footer on a suspended city:\n%s", lane.name, out)
			}
		}
	})
}

// The API keeps serving city reads while the city cache is not live: the
// bead-policy layer forwards the cache's Stats but deliberately not IsLive, so
// the cache_not_live 503 never fires for city and rig stores. Firing it would
// fail remote reads (they never fall back) and dashboard refreshes during
// every controller start and config-write rebuild, and a not-live cache
// already answers from its store. See cacheLiveOr503.
func TestCityAPIServesReadsWhileTheCityCacheIsNotLive(t *testing.T) {
	h := newCharCity(t, charCityBasic, nil)
	// newControllerState with a non-cancellable ctx skips the async full
	// prime, so the cache stays partial (not live) for the whole test.
	cache := cityCacheOf(t, h.cs)

	srv := cityAPI(h.cs)
	for _, path := range []string{"/status", "/beads", "/convoys", "/rigs", "/mail", "/mail/count", "/waits"} {
		if cache.IsLive() {
			t.Fatalf("precondition: the city cache is live before GET %s", path)
		}
		rec := getCityPath(t, srv, path)
		if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "cache_not_live") {
			t.Errorf("GET %s while the city cache is not live = %d, want 200: %s", path, rec.Code, rec.Body.String())
		}
	}

	for _, lane := range apiLanes(t, h) {
		if cache.IsLive() {
			t.Fatalf("precondition: the city cache is live before %s rig list", lane.name)
		}
		var stdout, stderr bytes.Buffer
		if code := routeRigList(h.cityPath, lane.client, "", true, &stdout, &stderr); code != 0 {
			t.Errorf("%s: rig list --json exit %d while the city cache is not live: %s", lane.name, code, stderr.String())
		}
		if !strings.Contains(stderr.String(), "route=api") {
			t.Errorf("%s: rig list did not read through the API while the city cache is not live: %s", lane.name, stderr.String())
		}
	}
}
