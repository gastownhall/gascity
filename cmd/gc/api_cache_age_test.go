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

const cacheAgeFooter = "reconciler may be lagging"

// cacheAgeCity is the harness city with a fake session provider, so the
// session list read below has a provider to observe.
const cacheAgeCity = charCityBasic + "\n[session]\nprovider = \"fake\"\n"

// The API reports the real age of the city's bead cache through the store
// stack the controller and supervisor serve (bead-policy layer over a
// CachingStore), and the CLI prints it as _cache_age_s.
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
			for _, path := range []string{"/rigs", "/sessions"} {
				inRange("GET "+path+" X-GC-Cache-Age-S", cacheAgeHeaderOf(t, getCityPath(t, srv, path), path))
			}

			lanes := apiLanes(t, h)
			for _, lane := range lanes {
				var stdout, stderr bytes.Buffer
				if code := routeRigList(h.cityPath, lane.client, "", true, &stdout, &stderr); code != 0 {
					t.Fatalf("%s: rig list --json exit %d: %s", lane.name, code, stderr.String())
				}
				inRange(lane.name+": rig list --json", jsonCacheAgeOf(t, lane.name+": rig list --json", stdout.Bytes()))

				stdout.Reset()
				stderr.Reset()
				if code := routeRigList(h.cityPath, lane.client, "", false, &stdout, &stderr); code != 0 {
					t.Fatalf("%s: rig list exit %d: %s", lane.name, code, stderr.String())
				}
				if got := strings.Contains(stdout.String(), cacheAgeFooter); got != tc.footer {
					t.Errorf("%s: rig list footer shown = %v, want %v:\n%s", lane.name, got, tc.footer, stdout.String())
				}
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
		if age := cacheAgeHeaderOf(t, getCityPath(t, srv, "/rigs"), "/rigs"); age != 0 {
			t.Errorf("GET /rigs X-GC-Cache-Age-S = %v, want 0 over an uncached store", age)
		}
		for _, lane := range apiLanes(t, h) {
			var stdout, stderr bytes.Buffer
			if code := routeRigList(h.cityPath, lane.client, "", true, &stdout, &stderr); code != 0 {
				t.Fatalf("%s: rig list --json exit %d: %s", lane.name, code, stderr.String())
			}
			if age := jsonCacheAgeOf(t, lane.name+": rig list --json", stdout.Bytes()); age != 0 {
				t.Errorf("%s: rig list --json _cache_age_s = %v, want 0 over an uncached store", lane.name, age)
			}
			stdout.Reset()
			stderr.Reset()
			if code := routeRigList(h.cityPath, lane.client, "", false, &stdout, &stderr); code != 0 {
				t.Fatalf("%s: rig list exit %d: %s", lane.name, code, stderr.String())
			}
			if strings.Contains(stdout.String(), cacheAgeFooter) {
				t.Errorf("%s: rig list shows the cache-age footer over an uncached store:\n%s", lane.name, stdout.String())
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
