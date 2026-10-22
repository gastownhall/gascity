package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/orderdispatch"
	"github.com/gastownhall/gascity/internal/orders"
)

// Metadata that spells the Dolt backend "bd" (the provider name older gc
// wrote) is a Dolt scope: a Dolt-only order applies there.
func TestOrderScopeBackendTreatsBdAsDolt(t *testing.T) {
	cityPath := orderBackendTestCity(t, `{"database":"dolt","backend":"bd","dolt_database":"hq"}`)
	if got := orderScopeBackend(cityPath, cityPath); got != "dolt" {
		t.Fatalf("orderScopeBackend(backend=bd) = %q, want dolt", got)
	}
	t.Setenv("GC_BEADS_BACKEND", "bd")
	bare := t.TempDir()
	if err := os.WriteFile(filepath.Join(bare, "city.toml"), []byte("[workspace]\nname = \"t\"\n"), 0o644); err != nil { //nolint:gosec // fixture
		t.Fatal(err)
	}
	if got := orderScopeBackend(bare, bare); got != "dolt" {
		t.Fatalf("orderScopeBackend(city backend=bd) = %q, want dolt", got)
	}
}

// The wiring, not just the copier: a reload that rebuilds the dispatcher
// through replaceOrderDispatcher must not re-announce an inapplicable order.
func TestReplaceOrderDispatcherCarriesInapplicableReports(t *testing.T) {
	cityPath := orderBackendTestCity(t, `{"database":"beads","backend":"http"}`)
	aa := []orders.Order{{
		Name: "dolt-only", Trigger: "cooldown", Interval: "1m",
		Exec: "true", RequiresBackend: []string{"dolt"},
	}}
	rec := &recordingRecorder{}
	noop := func(context.Context, string, string, []string) ([]byte, error) { return nil, nil }
	prev := buildOrderDispatcherFromListExec(aa, beads.NewMemStore(), nil, noop, rec).(*memoryOrderDispatcher)
	prev.dispatch(context.Background(), cityPath, time.Now())

	next := buildOrderDispatcherFromListExec(aa, beads.NewMemStore(), nil, noop, rec).(*memoryOrderDispatcher)
	cr := &CityRuntime{od: prev}
	cr.replaceOrderDispatcher(next)
	next.dispatch(context.Background(), cityPath, time.Now())
	if n := len(rec.ofType(events.OrderInapplicable)); n != 1 {
		t.Fatalf("order.inapplicable events across a replaceOrderDispatcher rebuild = %d, want 1", n)
	}
}

// The wiring, not just the check: gc doctor's order-firing-current check, as
// buildDoctorChecks registers it, reports a Dolt-only order on an http city as
// inapplicable rather than never-fired.
func TestBuildDoctorChecksReportsInapplicableOrders(t *testing.T) {
	cityPath := orderBackendTestCity(t, `{"database":"beads","backend":"http"}`)
	t.Setenv("GC_DOLT", "skip")
	if err := os.MkdirAll(filepath.Join(cityPath, "orders"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "orders", "dolt-only.toml"), []byte(`[order]
exec = "true"
trigger = "cooldown"
interval = "30s"
requires_backend = ["dolt"]
`), 0o644); err != nil { //nolint:gosec // fixture
		t.Fatal(err)
	}
	cfg := &config.City{
		Workspace:     config.Workspace{Name: "t"},
		FormulaLayers: config.FormulaLayers{City: []string{filepath.Join(cityPath, "formulas")}},
	}
	var firing doctor.Check
	for _, c := range buildDoctorChecks(cityPath, cfg, nil, buildDoctorChecksOpts{
		ControllerRunning:    true,
		SkipCityDoltCheck:    true,
		SkipManagedDoltCheck: true,
	}) {
		if c.Name() == "order-firing-current" {
			firing = c
		}
	}
	if firing == nil {
		t.Fatal("order-firing-current not registered")
	}
	result := firing.Run(&doctor.CheckContext{CityPath: cityPath})
	if result.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want OK; msg = %s; details = %v", result.Status, result.Message, result.Details)
	}
	if got := strings.Join(result.Details, "\n"); !strings.Contains(got, "dolt-only: inapplicable (backend http)") {
		t.Fatalf("details = %v, want dolt-only reported inapplicable", result.Details)
	}
}

// The webhook seam is the second dispatch path: it must refuse an order its
// scope's backend rules out, without writing a tracking bead or running it.
func TestOrderDispatchSeamRejectsInapplicableOrder(t *testing.T) {
	cityPath := orderBackendTestCity(t, `{"database":"beads","backend":"http"}`)
	store := beads.NewMemStore()
	ran := false
	execRun := func(context.Context, string, string, []string) ([]byte, error) {
		ran = true
		return nil, nil
	}
	a := orders.Order{Name: "dolt-hook", Trigger: "webhook", Exec: "true", RequiresBackend: []string{"dolt"}}
	m := buildOrderDispatcherFromListExec([]orders.Order{a}, store, nil, execRun, events.Discard).(*memoryOrderDispatcher)
	m.cityPath = cityPath
	res, err := m.Dispatch(context.Background(), orderdispatch.DispatchRequest{Order: a})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if !res.Rejected || res.Fired {
		t.Fatalf("result = %+v, want rejected and not fired", res)
	}
	if !strings.Contains(res.Reason, "inapplicable (backend http)") {
		t.Fatalf("reason = %q, want it to name the inapplicable backend", res.Reason)
	}
	m.drain(context.Background())
	if ran {
		t.Fatal("an inapplicable webhook order ran")
	}
	if got := trackingBeads(t, store, "order-run:dolt-hook"); len(got) != 0 {
		t.Fatalf("tracking beads = %d, want 0", len(got))
	}

	// The same order on a Dolt city fires.
	doltCity := orderBackendTestCity(t, `{"database":"dolt","backend":"dolt","dolt_database":"hq"}`)
	m2 := buildOrderDispatcherFromListExec([]orders.Order{a}, beads.NewMemStore(), nil, execRun, events.Discard).(*memoryOrderDispatcher)
	m2.cityPath = doltCity
	res, err = m2.Dispatch(context.Background(), orderdispatch.DispatchRequest{Order: a})
	if err != nil || !res.Fired {
		t.Fatalf("Dispatch on a dolt city = %+v, %v; want fired", res, err)
	}
	m2.drain(context.Background())
}

// The controller's webhook adapter builds a dispatcher per delivery, and a
// refused delivery is re-sent on every sender retry: an inapplicable order is
// still announced once per controller boot, and each order once. Deliveries
// arrive concurrently, one HTTP handler goroutine each.
func TestWebhookDispatcherReportsInapplicableOrderOncePerBoot(t *testing.T) {
	ep := events.NewFake()
	cs := &controllerState{
		cityPath:  orderBackendTestCity(t, `{"database":"beads","backend":"http"}`),
		cfg:       &config.City{Workspace: config.Workspace{Name: "t"}},
		eventProv: ep,
	}
	hook := orders.Order{Name: "dolt-hook", Trigger: "webhook", Exec: "true", RequiresBackend: []string{"dolt"}}
	doltSync := orders.Order{Name: "dolt-sync", Trigger: "webhook", Exec: "true", RequiresBackend: []string{"dolt"}}
	var wg sync.WaitGroup
	for i, a := range []orders.Order{hook, hook, hook, doltSync, doltSync} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := cs.WebhookDispatcher().Dispatch(context.Background(), orderdispatch.DispatchRequest{Order: a})
			if err != nil || !res.Rejected || res.Fired {
				t.Errorf("delivery %d = %+v, %v; want rejected and not fired", i, res, err)
			}
		}()
	}
	wg.Wait()
	reported, err := ep.List(events.Filter{Type: events.OrderInapplicable})
	if err != nil {
		t.Fatal(err)
	}
	subjects := make([]string, 0, len(reported))
	for _, e := range reported {
		subjects = append(subjects, e.Subject)
	}
	slices.Sort(subjects)
	if got, want := strings.Join(subjects, ","), "dolt-hook,dolt-sync"; got != want {
		t.Fatalf("order.inapplicable subjects across 5 deliveries = %q, want %q", got, want)
	}
}

// orderRunHTTPCity is an http city with one manual Dolt-only exec order that
// leaves a marker when it runs.
func orderRunHTTPCity(t *testing.T) (marker string) {
	t.Helper()
	cityPath := orderBackendTestCity(t, `{"database":"beads","backend":"http"}`)
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_BEADS_SCOPE_ROOT", "")
	t.Setenv("GC_EVENTS", "")
	t.Setenv("GC_CITY", cityPath)
	t.Setenv("GC_CITY_PATH", cityPath)
	t.Setenv("GC_CITY_ROOT", cityPath)
	t.Setenv("GC_RIG", "")
	t.Setenv("GC_RIG_ROOT", "")
	t.Chdir(cityPath)
	marker = filepath.Join(t.TempDir(), "ran")
	if err := os.MkdirAll(filepath.Join(cityPath, "orders"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "orders", "stale-db.toml"), []byte(`[order]
exec = "touch `+marker+`"
trigger = "manual"
requires_backend = ["dolt"]
`), 0o644); err != nil { //nolint:gosec // fixture
		t.Fatal(err)
	}
	return marker
}

// gc order run refuses an order its scope's backend rules out, so a manual run
// of a Dolt cleanup on an http city never reaches a Dolt server.
func TestCmdOrderRunRefusesInapplicableOrder(t *testing.T) {
	marker := orderRunHTTPCity(t)
	var stdout, stderr bytes.Buffer
	if code := cmdOrderRun("stale-db", "", false, false, nil, &stdout, &stderr); code == 0 {
		t.Fatalf("cmdOrderRun = 0, want refusal; stdout=%s", stdout.String())
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the inapplicable order ran")
	}
	msg := stderr.String()
	for _, want := range []string{"stale-db", "inapplicable (backend http)", "requires backend dolt", "--force"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("stderr = %q, want it to mention %q", msg, want)
		}
	}
}

// --force is the operator's explicit override for the manual run.
func TestCmdOrderRunForceRunsInapplicableOrder(t *testing.T) {
	marker := orderRunHTTPCity(t)
	var stdout, stderr bytes.Buffer
	if code := cmdOrderRun("stale-db", "", false, true, nil, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdOrderRun --force = %d, want 0; stderr=%s", code, stderr.String())
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("the forced order did not run: %v", err)
	}
	if !strings.Contains(stderr.String(), "inapplicable (backend http)") {
		t.Fatalf("stderr = %q, want a warning naming the override", stderr.String())
	}
}

// gc order check reports an inapplicable order as inapplicable and never due,
// in the table and in JSON, without evaluating its trigger.
func TestOrderCheckReportsInapplicableOrders(t *testing.T) {
	cityPath := orderBackendTestCity(t, `{"database":"beads","backend":"http"}`)
	aa := []orders.Order{
		// A condition check that would fail the test if it ran.
		{Name: "dolt-only", Trigger: "condition", Check: "exit 0", Exec: "true", RequiresBackend: []string{"dolt"}},
	}
	resolve := func(orders.Order) ([]beads.OrdersStore, error) {
		t.Fatal("an inapplicable order's stores were resolved")
		return nil, nil
	}
	var table, stderr bytes.Buffer
	code := doOrderCheckWithStoresResolverScopedJSON(cityPath, &config.City{}, aa, time.Now(), nil, resolve, false, &table, &stderr)
	if code != 1 {
		t.Fatalf("exit = %d, want 1 (nothing due); stderr=%s", code, stderr.String())
	}
	var row string
	for _, line := range strings.Split(table.String(), "\n") {
		if strings.HasPrefix(line, "dolt-only") {
			row = line
		}
	}
	if !strings.Contains(row, "inapplicable (backend http)") || strings.Contains(row, " yes ") {
		t.Fatalf("row = %q, want inapplicable and not due", row)
	}

	var js bytes.Buffer
	doOrderCheckWithStoresResolverScopedJSON(cityPath, &config.City{}, aa, time.Now(), nil, resolve, true, &js, &stderr)
	var payload orderCheckJSON
	if err := json.Unmarshal(js.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v\n%s", err, js.String())
	}
	if payload.AnyDue || payload.DueTotal != 0 || len(payload.Orders) != 1 {
		t.Fatalf("payload = %+v, want one not-due order", payload)
	}
	if got := payload.Orders[0]; got.Due || got.Inapplicable != "backend http" || got.Reason != "inapplicable (backend http)" {
		t.Fatalf("row = %+v, want inapplicable backend http", got)
	}

	// Preflight still runs for an inapplicable order: an [order.env] override
	// of a controller-owned key is a config error whatever the backend.
	invalid := []orders.Order{{
		Name: "dolt-only", Trigger: "condition", Check: "exit 0", Exec: "true",
		RequiresBackend: []string{"dolt"}, Env: map[string]string{"BEADS_DIR": "/elsewhere"},
	}}
	for _, jsonOutput := range []bool{false, true} {
		var out, errOut bytes.Buffer
		if code := doOrderCheckWithStoresResolverScopedJSON(cityPath, &config.City{}, invalid, time.Now(), nil, resolve, jsonOutput, &out, &errOut); code != 1 {
			t.Fatalf("json=%v: exit = %d, want 1", jsonOutput, code)
		}
		if !strings.Contains(errOut.String(), `controller-owned env key "BEADS_DIR"`) {
			t.Fatalf("json=%v: stderr = %q, want the env override error; stdout = %q", jsonOutput, errOut.String(), out.String())
		}
	}
}

// The dispatcher resolves a scope's backend once per tick, not once per order:
// orderScopeBackend re-reads city.toml when the metadata names no backend.
func TestOrderDispatchResolvesScopeBackendOncePerTick(t *testing.T) {
	cityPath := orderBackendTestCity(t, `{"database":"beads","backend":"http"}`)
	var aa []orders.Order
	for _, name := range []string{"a", "b", "c"} {
		aa = append(aa, orders.Order{Name: name, Trigger: "cooldown", Interval: "1m", Exec: "true", RequiresBackend: []string{"dolt"}})
	}
	orig := orderScopeBackendFn
	t.Cleanup(func() { orderScopeBackendFn = orig })
	calls := 0
	orderScopeBackendFn = func(cityPath, scopeRoot string) string {
		calls++
		return orig(cityPath, scopeRoot)
	}
	noop := func(context.Context, string, string, []string) ([]byte, error) { return nil, nil }
	ad := buildOrderDispatcherFromListExec(aa, beads.NewMemStore(), nil, noop, events.Discard)
	ad.dispatch(context.Background(), cityPath, time.Now())
	if calls != 1 {
		t.Fatalf("scope backend resolved %d times for 3 orders on one scope, want 1", calls)
	}
}
