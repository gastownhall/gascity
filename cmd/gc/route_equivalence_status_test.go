package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/chartest"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/supervisor"
)

// cityStatusRouteAdditiveKeys are the keys `gc status --json` may print only
// on the API route. The schema declares both as optional; every other key
// must read the same on both routes.
var cityStatusRouteAdditiveKeys = []string{"_cache_age_s", "conditional_writes"}

// cityStatusRouteProxied is the proxied-native account the tests hand both
// routes, so beads.proxied is compared like any other field.
var cityStatusRouteProxied = beads.ProxiedDiagnostic{
	Endpoint:   beads.ProxiedEndpointStamp{Port: 3307, PID: 4242, Generation: "4242:1"},
	Evidence:   "argv",
	IdlePolicy: "never",
	Verdict:    beads.ProxiedVerdictDraining,
}

// newCityStatusRouteHarness builds the shared state both routes read: a
// file-store city with two singleton agents and two rigs whose prefix and
// default sling targets come only from city.toml, served by a
// controllerState whose bead caches are primed. newCharCity leaves the caches
// pre-primed only, and a status body built over them is partial.
func newCityStatusRouteHarness(t *testing.T) *charHarness {
	t.Helper()
	t.Setenv("GC_SESSION", "fake")
	rigs := map[string]string{"alpha": t.TempDir(), "omega": t.TempDir()}
	for _, dir := range rigs {
		writeContractFile(t, filepath.Join(dir, ".gc", "beads.json"), "{\"seq\":0,\"beads\":[]}\n")
	}
	h := newCharCity(t, fmt.Sprintf(`[workspace]
name = %q
prefix = "gc"

[session]
provider = "fake"

[[agent]]
name = "worker"
start_command = "true"
max_active_sessions = 1

[[agent]]
name = "rigbot"
dir = "alpha"
start_command = "true"
max_active_sessions = 1

[[rigs]]
name = "alpha"
path = %q
prefix = "al"
default_sling_target = "alpha/rigbot"

[[rigs]]
name = "omega"
path = %q
default_sling_targets = ["omega/rigbot", "alpha/rigbot"]
`, charCityName, rigs["alpha"], rigs["omega"]), func(t *testing.T, store beads.Store) {
		if _, err := store.Create(beads.Bead{Title: "status route seed"}); err != nil {
			t.Fatalf("seed bead: %v", err)
		}
	})
	primeCityStatusRouteCaches(t, h.cs)
	return h
}

// primeCityStatusRouteCaches fully primes every bead cache cs serves from.
// Prime returns once the cache is live (it joins a prime already in flight).
// It reads cs's own stores, not CityBeadStore and BeadStores: under
// GC_TEST_SPLIT_STRICT=1 those return the gate's views, which read through
// the store they wrap but are not caches themselves.
func primeCityStatusRouteCaches(t *testing.T, cs *controllerState) {
	t.Helper()
	cs.mu.RLock()
	stores := []beads.Store{cs.cityBeadStore}
	for _, store := range cs.beadStores {
		stores = append(stores, store)
	}
	cs.mu.RUnlock()
	for _, store := range stores {
		cache, ok := demandLabelKey(store).(*beads.CachingStore)
		if !ok {
			t.Fatalf("store %T is not a bead cache", demandLabelKey(store))
		}
		if err := cache.Prime(context.Background()); err != nil {
			t.Fatalf("prime bead cache: %v", err)
		}
		if !cache.IsLive() {
			t.Fatal("bead cache is not live after a full prime")
		}
	}
}

// markCityStatusRouteControllerRunning makes controllerStatusForCity report
// the supervisor-managed city running, the state in which the API route
// answers, so both routes render the controller, health and store_health
// from the same fact.
func markCityStatusRouteControllerRunning(t *testing.T, h *charHarness) {
	t.Helper()
	if err := supervisor.NewRegistry(supervisor.RegistryPath()).Register(h.cityPath, charCityName); err != nil {
		t.Fatalf("register city: %v", err)
	}
	stubSupervisorAlive(t)
	stubStoreHealthEvents(t, events.NewFake())
}

// giveCityStatusRoutesProxiedDiagnostic hands the proxied-native account to
// the local route's store open and to the controller the API route reads.
func giveCityStatusRoutesProxiedDiagnostic(t *testing.T, h *charHarness) {
	t.Helper()
	orig := openCityStoreAtForStatus
	t.Cleanup(func() { openCityStoreAtForStatus = orig })
	openCityStoreAtForStatus = func(path string) (beads.StoreOpenResult, error) {
		opened, err := orig(path)
		proxied := cityStatusRouteProxied
		opened.Diagnostic.Proxied = &proxied
		return opened, err
	}
	h.cs.mu.Lock()
	defer h.cs.mu.Unlock()
	if h.cs.cityBeadsDiagnostic == nil {
		t.Fatal("controller reports no bead store diagnostic")
	}
	proxied := cityStatusRouteProxied
	h.cs.cityBeadsDiagnostic.Proxied = &proxied
}

// cityStatusCharRoute adapts gc status to the charharness route signature.
// It runs the real CLI (`gc --city <path> status --json`) with the API-client
// seam pinned to the lane's client, so the output is exactly what a user of
// that route reads.
func cityStatusCharRoute(cityPath string, c *api.Client, nilReason string, jsonOut bool, stdout, stderr io.Writer) int {
	orig := cityStatusAPIClient
	cityStatusAPIClient = func(string) (*api.Client, string) { return c, nilReason }
	defer func() { cityStatusAPIClient = orig }()
	args := []string{"--city", cityPath, "status"}
	if jsonOut {
		args = append(args, "--json")
	}
	return run(args, stdout, stderr)
}

// cityStatusRouteJSON runs `gc status --json` on one lane and returns stdout,
// failing unless it exits 0 on the route the lane names. A silent fallback
// would print the local rendering on the API lane and make every comparison
// vacuous.
func cityStatusRouteJSON(t *testing.T, h *charHarness, lanes []charLane, laneName string) []byte {
	t.Helper()
	wantRoute := map[string]string{"alive": "route=api", "serverless": "route=fallback reason=controller-down"}[laneName]
	for _, lane := range lanes {
		if lane.name != laneName {
			continue
		}
		var stdout, stderr bytes.Buffer
		if code, _ := h.run(lane, charCommand{name: "status", route: cityStatusCharRoute}, true, &stdout, &stderr); code != 0 {
			t.Fatalf("%s lane: gc status --json exit %d; stderr:\n%s", laneName, code, stderr.String())
		}
		if !strings.Contains(stderr.String(), wantRoute) {
			t.Fatalf("%s lane: stderr %q lacks %q", laneName, stderr.String(), wantRoute)
		}
		return stdout.Bytes()
	}
	t.Fatalf("no %s lane", laneName)
	return nil
}

// cityStatusRouteKeys decodes one `gc status --json` payload.
func cityStatusRouteKeys(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("gc status --json is not one JSON object: %v\n%s", err, raw)
	}
	return payload
}

// TestCityStatusJSONValidatesOnEveryRoute validates real `gc status --json`
// output from the local route and the API route against the published
// schema, with seed data that makes each route print its optional blocks:
// the API route's _cache_age_s and conditional_writes, store_health from a
// running controller, a rig's default_sling_targets, and beads.proxied.
func TestCityStatusJSONValidatesOnEveryRoute(t *testing.T) {
	for _, controller := range []string{"stopped", "running"} {
		for _, lane := range []string{"serverless", "alive"} {
			t.Run(controller+"/"+lane, func(t *testing.T) {
				h := newCityStatusRouteHarness(t)
				if controller == "running" {
					markCityStatusRouteControllerRunning(t, h)
				}
				giveCityStatusRoutesProxiedDiagnostic(t, h)
				raw := cityStatusRouteJSON(t, h, h.lanes(t), lane)
				validateJSONAgainstBuiltinSchema(t, []string{"status"}, raw)

				payload := cityStatusRouteKeys(t, raw)
				beadsDiag, _ := payload["beads"].(map[string]any)
				if _, ok := beadsDiag["proxied"].(map[string]any); !ok {
					t.Errorf("beads.proxied = %#v, want the proxied-native account", beadsDiag["proxied"])
				}
				summary, _ := payload["summary"].(map[string]any)
				_, hasStoreHealth := summary["store_health"].(map[string]any)
				if wantStoreHealth := controller == "running" || lane == "alive"; hasStoreHealth != wantStoreHealth {
					t.Errorf("summary.store_health present = %v, want %v", hasStoreHealth, wantStoreHealth)
				}
				if lane == "alive" {
					if _, ok := payload["_cache_age_s"].(float64); !ok {
						t.Errorf("_cache_age_s = %#v, want a number on the API route", payload["_cache_age_s"])
					}
					if _, ok := payload["conditional_writes"].(map[string]any); !ok {
						t.Errorf("conditional_writes = %#v, want the controller's block on the API route", payload["conditional_writes"])
					}
				} else {
					for _, key := range cityStatusRouteAdditiveKeys {
						if _, ok := payload[key]; ok {
							t.Errorf("%s present on the local route, want it absent", key)
						}
					}
				}
			})
		}
	}
}

// TestCityStatusJSONRouteEquivalence pins the #8453 contract for gc status:
// the API route prints the local rendering, plus only the declared API-only
// keys. The rigs' prefix and default sling targets live only in city.toml,
// and the proxied-native account rides the status wire, so both are seeded.
func TestCityStatusJSONRouteEquivalence(t *testing.T) {
	h := newCityStatusRouteHarness(t)
	markCityStatusRouteControllerRunning(t, h)
	giveCityStatusRoutesProxiedDiagnostic(t, h)
	lanes := h.lanes(t)

	local := cityStatusRouteJSON(t, h, lanes, "serverless")
	viaAPI := cityStatusRouteJSON(t, h, lanes, "alive")
	if diff := chartest.JSONShapeDiff(local, viaAPI, cityStatusRouteAdditiveKeys); diff != "" {
		t.Fatalf("gc status --json differs by route: %s\nlocal:\n%s\napi:\n%s", diff, local, viaAPI)
	}
}

// TestFillStatusRigConfigFieldsKeepsRigsConfigDoesNotName pins the edges of
// the API route's rig-row fill: config supplies prefix and default sling
// targets by rig name, a rig config does not name keeps its wire row, and
// the filled slice does not alias the config's.
func TestFillStatusRigConfigFieldsKeepsRigsConfigDoesNotName(t *testing.T) {
	wireRows := func() []StatusRigJSON {
		return []StatusRigJSON{
			{Name: "alpha", Path: "/srv/alpha"},
			{Name: "omega", Path: "/srv/omega", Suspended: true},
			{Name: "stray", Path: "/srv/stray"},
		}
	}
	cfg := &config.City{Rigs: []config.Rig{
		{Name: "alpha", Path: "/cfg/alpha", Prefix: "al", DefaultSlingTarget: "alpha/rigbot"},
		{Name: "omega", Path: "/cfg/omega", DefaultSlingTargets: []string{"omega/rigbot", "alpha/rigbot"}},
	}}
	omegaPrefix := cfg.Rigs[1].EffectivePrefix()
	if omegaPrefix == "" {
		t.Fatal("derived prefix for omega is empty; the case would not pin derivation")
	}

	tests := []struct {
		name string
		cfg  *config.City
		want []StatusRigJSON
	}{
		{name: "nil config leaves rows untouched", cfg: nil, want: wireRows()},
		{
			name: "config rigs filled, unnamed rig kept",
			cfg:  cfg,
			want: []StatusRigJSON{
				{Name: "alpha", Path: "/srv/alpha", Prefix: "al", DefaultSlingTarget: "alpha/rigbot"},
				{Name: "omega", Path: "/srv/omega", Suspended: true, Prefix: omegaPrefix, DefaultSlingTargets: []string{"omega/rigbot", "alpha/rigbot"}},
				{Name: "stray", Path: "/srv/stray"},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rows := wireRows()
			fillStatusRigConfigFields(rows, tc.cfg)
			if !reflect.DeepEqual(rows, tc.want) {
				t.Fatalf("rows = %+v, want %+v", rows, tc.want)
			}
		})
	}

	t.Run("targets slice is copied", func(t *testing.T) {
		rows := wireRows()
		fillStatusRigConfigFields(rows, cfg)
		cfg.Rigs[1].DefaultSlingTargets[0] = "mutated"
		if got := rows[1].DefaultSlingTargets[0]; got != "omega/rigbot" {
			t.Fatalf("row targets alias the config slice: got %q after mutating config", got)
		}
	})
}
