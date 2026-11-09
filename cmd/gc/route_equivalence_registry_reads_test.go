package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/chartest"
	"github.com/gastownhall/gascity/internal/orders"
)

// Route-equivalence tests for the routed reads that predate #8453 and had no
// same-state comparison: gc rig list, gc wait list, gc wait inspect and gc
// order history. Each runs the command on the local route (nil
// client) and through an in-process SupervisorMux over the same
// controllerState and store, validates both payloads against the command's
// result schema, and requires them to match exactly, modulo the additive keys
// that schema declares for the API route.

// assertRouteEquivalentJSONModulo requires both payloads to validate against
// the command's result schema, and the API payload to carry every local key
// with an equal value plus, at most, the additive keys the schema declares
// optional for the API route.
func assertRouteEquivalentJSONModulo(t *testing.T, command []string, local, viaAPI []byte, additive []string) {
	t.Helper()
	validateJSONAgainstResultSchema(t, command, local)
	validateJSONAgainstResultSchema(t, command, viaAPI)
	if diff := chartest.JSONShapeDiff(local, viaAPI, additive); diff != "" {
		t.Fatalf("API route differs from local route for %v: %s\nlocal: %s\napi:   %s", command, diff, local, viaAPI)
	}
}

// registryReadsAPIClient returns the plain-HTTP client lane of the harness's
// in-process SupervisorMux, which serves h.cs.
func registryReadsAPIClient(t *testing.T, h *charHarness) *api.Client {
	t.Helper()
	for _, lane := range h.lanes(t) {
		if lane.name == "alive" {
			return lane.client
		}
	}
	t.Fatal("char harness has no alive lane")
	return nil
}

// runBothRoutes runs route once through client and once locally, requires the
// expected route log on each, and returns (local, api) stdout.
func runBothRoutes(t *testing.T, cmdName string, client *api.Client, route func(c *api.Client, nilReason string, stdout, stderr io.Writer) int) ([]byte, []byte) {
	t.Helper()
	run := func(c *api.Client, nilReason string) (int, []byte, []byte) {
		clearBuiltinImportWarningCache()
		var stdout, stderr bytes.Buffer
		exit := route(c, nilReason, &stdout, &stderr)
		return exit, stdout.Bytes(), stderr.Bytes()
	}
	requireRouteLog := func(stderr []byte, route string) {
		t.Helper()
		want := "cmd=" + cmdName + " route=" + route
		if !strings.Contains(string(stderr), want) {
			t.Fatalf("stderr missing %q:\n%s", want, stderr)
		}
	}
	apiExit, apiOut, apiErr := run(client, "")
	requireRouteLog(apiErr, "api")
	localExit, localOut, localErr := run(nil, "controller-down")
	requireRouteLog(localErr, "fallback")
	if apiExit != 0 || localExit != 0 {
		t.Fatalf("%s exit api=%d local=%d, want 0/0\napi stderr: %s\nlocal stderr: %s", cmdName, apiExit, localExit, apiErr, localErr)
	}
	return localOut, apiOut
}

func TestRigListRouteEquivalence(t *testing.T) {
	clearInheritedBeadsEnv(t)
	prevHQRunning := rigListHQRunning
	rigListHQRunning = func(string) bool { return true }
	t.Cleanup(func() { rigListHQRunning = prevHQRunning })

	root := t.TempDir()
	frontend := filepath.Join(root, "frontend")
	backend := filepath.Join(root, "backend")
	for _, dir := range []string{frontend, backend} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cityToml := charCityBasic + fmt.Sprintf(`
[[agent]]
name = "worker"
dir = "frontend"

[[rigs]]
name = "frontend"
path = %q
prefix = "fe"
default_branch = "trunk"
default_sling_target = "frontend/worker"
default_sling_targets = ["frontend/worker", "frontend/reviewer"]

[[rigs]]
name = "backend"
path = %q
prefix = "be"
suspended = true
`, frontend, backend)
	h := newCharCity(t, cityToml, nil)
	client := registryReadsAPIClient(t, h)

	local, viaAPI := runBothRoutes(t, "rig list", client, func(c *api.Client, nilReason string, stdout, stderr io.Writer) int {
		return routeRigList(h.cityPath, c, nilReason, true, stdout, stderr)
	})
	assertRouteEquivalentJSONModulo(t, []string{"rig", "list"}, local, viaAPI, []string{"_cache_age_s"})
}

// waitRouteCity seeds a city with a pending wait that sets every wait field,
// a ready wait, and a closed wait, and returns the harness and their IDs.
func waitRouteCity(t *testing.T) (*charHarness, []string) {
	t.Helper()
	clearInheritedBeadsEnv(t)
	var ids []string
	h := newCharCity(t, charCityBasic, func(t *testing.T, store beads.Store) {
		for _, w := range []struct {
			meta   map[string]string
			note   string
			closed bool
		}{
			{meta: map[string]string{
				"session_id": "gc-s1", "session_name": "demo", "kind": "deps", "state": waitStatePending,
				"dep_ids": "gc-a, gc-b", "dep_mode": "any", "registered_epoch": "3", "delivery_attempt": "1",
				"nudge_id": "n-1", "expires_at": "2030-01-01T00:00:00Z",
			}, note: "resume the build\n"},
			{meta: map[string]string{"session_id": "gc-s2", "session_name": "other", "kind": "probe", "state": waitStateReady}, note: "probe ready"},
			{meta: map[string]string{"session_id": "gc-s1", "kind": "deps", "state": "closed"}, note: "done", closed: true},
		} {
			b, err := store.Create(beads.Bead{
				Title:       "wait:demo",
				Type:        waitBeadType,
				Status:      "open",
				Description: w.note,
				Labels:      []string{waitBeadLabel, "session:" + w.meta["session_id"]},
				Metadata:    w.meta,
			})
			if err != nil {
				t.Fatalf("seed wait: %v", err)
			}
			if w.closed {
				if err := store.Close(b.ID); err != nil {
					t.Fatalf("close wait: %v", err)
				}
			}
			ids = append(ids, b.ID)
		}
	})
	return h, ids
}

func TestWaitListRouteEquivalence(t *testing.T) {
	for _, tc := range []struct {
		name, state, session string
	}{
		{name: "all"},
		{name: "state filter", state: waitStateReady},
		{name: "session filter", session: "gc-s1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := waitRouteCity(t)
			client := registryReadsAPIClient(t, h)
			local, viaAPI := runBothRoutes(t, "wait list", client, func(c *api.Client, nilReason string, stdout, stderr io.Writer) int {
				return routeWaitList(h.cityPath, c, nilReason, tc.state, tc.session, true, stdout, stderr)
			})
			assertRouteEquivalentJSONModulo(t, []string{"wait", "list"}, local, viaAPI, nil)
			if !bytes.Equal(local, viaAPI) {
				t.Fatalf("wait list bytes differ (row order or encoding):\nlocal: %s\napi:   %s", local, viaAPI)
			}
		})
	}
}

func TestWaitInspectRouteEquivalence(t *testing.T) {
	h, ids := waitRouteCity(t)
	client := registryReadsAPIClient(t, h)
	for i, id := range ids {
		t.Run(fmt.Sprintf("wait %d", i), func(t *testing.T) {
			local, viaAPI := runBothRoutes(t, "wait inspect", client, func(c *api.Client, nilReason string, stdout, stderr io.Writer) int {
				return routeWaitInspect(h.cityPath, c, nilReason, id, true, stdout, stderr)
			})
			assertRouteEquivalentJSONModulo(t, []string{"wait", "inspect"}, local, viaAPI, nil)
		})
	}
}

// TestOrderHistoryRouteEquivalence compares both output modes. The store
// stamps each run to the nanosecond, so the human table also proves the API
// route prints EXECUTED in whole seconds, as the local route does, while
// --json keeps the full created_at on both.
func TestOrderHistoryRouteEquivalence(t *testing.T) {
	clearInheritedBeadsEnv(t)
	subSecond := false
	h := newCharCity(t, charCityBasic, func(t *testing.T, store beads.Store) {
		for i, closed := range []bool{true, false, true} {
			b, err := store.Create(beads.Bead{
				Title:  fmt.Sprintf("digest run %d", i),
				Type:   "task",
				Labels: []string{"order-tracking", "order-run:digest"},
			})
			if err != nil {
				t.Fatalf("seed order run: %v", err)
			}
			subSecond = subSecond || b.CreatedAt.Nanosecond() != 0
			if closed {
				if err := store.Close(b.ID); err != nil {
					t.Fatalf("close order run: %v", err)
				}
			}
		}
	})
	if !subSecond {
		t.Fatal("no seeded run has a sub-second creation time; the timestamp comparison needs one")
	}
	writeRouteEquivalenceOrder(t, h.cityPath)
	cfg, err := loadCityConfig(h.cityPath, io.Discard)
	if err != nil {
		t.Fatalf("load city config: %v", err)
	}
	aa, code := loadAllOrders(h.cityPath, cfg, io.Discard, "test")
	if code != 0 {
		t.Fatalf("loadAllOrders = %d", code)
	}
	client := registryReadsAPIClient(t, h)
	for _, limit := range []int{1, 10} {
		for _, jsonOut := range []bool{false, true} {
			t.Run(fmt.Sprintf("limit %d/json=%t", limit, jsonOut), func(t *testing.T) {
				local, viaAPI := runBothRoutes(t, "order history", client, func(c *api.Client, nilReason string, stdout, stderr io.Writer) int {
					return routeOrderHistory(h.cityPath, cfg, "digest", "", aa, c, nilReason, orderHistoryBounds{Limit: limit}, jsonOut, stdout, stderr)
				})
				if jsonOut {
					assertRouteEquivalentJSONModulo(t, []string{"order", "history"}, local, viaAPI, nil)
				}
				if !bytes.Equal(local, viaAPI) {
					t.Fatalf("order history bytes differ (row order or encoding):\nlocal: %s\napi:   %s", local, viaAPI)
				}
			})
		}
	}
}

// TestRenderOrderHistoryFromAPIPrintsTheLocalTable gives the API renderer the
// wire form of a store's runs and requires the local route's table for each
// order, with and without the RIG column: the wire carries created_at to the
// nanosecond, and both tables print EXECUTED in whole seconds.
func TestRenderOrderHistoryFromAPIPrintsTheLocalTable(t *testing.T) {
	created := time.Date(2026, 11, 1, 12, 30, 45, 123456789, time.UTC)
	cityRun := beads.Bead{ID: "gc-1", Title: "digest run", Status: "closed", Labels: []string{"order-run:digest"}, CreatedAt: created}
	rigRun := beads.Bead{ID: "gc-2", Title: "lint run", Status: "closed", Labels: []string{"order-run:lint:rig:frontend"}, CreatedAt: created.Add(time.Minute)}
	store := beads.OrdersStore{Store: beads.NewMemStoreFrom(2, []beads.Bead{cityRun, rigRun}, nil)}
	aa := []orders.Order{{Name: "digest"}, {Name: "lint", Rig: "frontend"}}
	for i, run := range []beads.Bead{cityRun, rigRun} {
		order := aa[i]
		t.Run(order.ScopedName(), func(t *testing.T) {
			var local, viaAPI, stderr bytes.Buffer
			if code := doOrderHistory(order.Name, order.Rig, aa, store, &local); code != 0 {
				t.Fatalf("local route exit = %d", code)
			}
			wire := api.CachedRead[[]api.OrderHistoryView]{Body: []api.OrderHistoryView{{
				BeadID:     run.ID,
				Name:       order.Name,
				ScopedName: order.ScopedName(),
				Rig:        order.Rig,
				CreatedAt:  run.CreatedAt.Format(time.RFC3339Nano),
			}}}
			if code := renderOrderHistoryFromAPI(wire, order.Name, order.Rig, orderHistoryBounds{}, false, &viaAPI, &stderr); code != 0 {
				t.Fatalf("API render exit = %d, stderr=%q", code, stderr.String())
			}
			if !strings.Contains(local.String(), run.ID) {
				t.Fatalf("local table has no row for run %s:\n%s", run.ID, local.String())
			}
			if viaAPI.String() != local.String() {
				t.Fatalf("API table differs from the local table:\nlocal: %q\napi:   %q", local.String(), viaAPI.String())
			}
		})
	}
}

// writeRouteEquivalenceOrder writes a manual "digest" order into the city.
func writeRouteEquivalenceOrder(t *testing.T, cityPath string) {
	t.Helper()
	ordersDir := filepath.Join(cityPath, "orders")
	if err := os.MkdirAll(ordersDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityPath, "formulas"), 0o755); err != nil {
		t.Fatal(err)
	}
	order := "[order]\ntrigger = \"manual\"\nformula = \"mol-digest\"\n"
	if err := os.WriteFile(filepath.Join(ordersDir, "digest.toml"), []byte(order), 0o644); err != nil {
		t.Fatal(err)
	}
}
