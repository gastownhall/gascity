package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/orders"
)

// orderBackendTestCity is a city whose city-scope metadata names backend, or
// that has no metadata at all when metadata is empty.
func orderBackendTestCity(t *testing.T, metadata string) string {
	t.Helper()
	t.Setenv("GC_BEADS_BACKEND", "")
	cityPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"t\"\n"), 0o644); err != nil { //nolint:gosec // fixture config
		t.Fatal(err)
	}
	if metadata != "" {
		writeScopeBeadsMetadata(t, cityPath, metadata)
	}
	return cityPath
}

func TestOrderScopeBackendClassifiesScopes(t *testing.T) {
	for _, tc := range []struct {
		name     string
		metadata string
		want     string
	}{
		{"http", `{"database":"beads","backend":"http"}`, "http"},
		{"dolt", `{"database":"dolt","backend":"dolt","dolt_database":"hq"}`, "dolt"},
		{"dolt by default", `{"database":"dolt","dolt_database":"hq"}`, "dolt"},
		{"doltlite backend", `{"database":"doltlite","backend":"doltlite"}`, "doltlite"},
		{"doltlite database", `{"database":"doltlite","dolt_database":"hq"}`, "doltlite"},
		{"postgres binding", `{"backend":"postgres","storage_endpoint":"postgres://db","storage_database":"hq"}`, "postgres"},
		{"no metadata", ``, "dolt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cityPath := orderBackendTestCity(t, tc.metadata)
			if got := orderScopeBackend(cityPath, cityPath); got != tc.want {
				t.Fatalf("orderScopeBackend = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestOrderScopeBackendRigInheritsOnlyWithoutOwnMetadata(t *testing.T) {
	cityPath := orderBackendTestCity(t, `{"database":"beads","backend":"http"}`)
	bare := filepath.Join(cityPath, "rigs", "bare")
	if err := os.MkdirAll(bare, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := orderScopeBackend(cityPath, bare); got != "http" {
		t.Fatalf("rig without metadata = %q, want the city's http", got)
	}
	own := filepath.Join(cityPath, "rigs", "own")
	writeScopeBeadsMetadata(t, own, `{"database":"dolt","backend":"dolt","dolt_database":"own"}`)
	if got := orderScopeBackend(cityPath, own); got != "dolt" {
		t.Fatalf("rig with its own dolt metadata = %q, want dolt", got)
	}
}

// recordingRecorder keeps every recorded event, for asserting what dispatch
// reported.
type recordingRecorder struct {
	mu     sync.Mutex
	events []events.Event
}

func (r *recordingRecorder) Record(e events.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

func (r *recordingRecorder) ofType(typ string) []events.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []events.Event
	for _, e := range r.events {
		if e.Type == typ {
			out = append(out, e)
		}
	}
	return out
}

// An order that requires a Dolt backend is never dispatched for a scope on
// another backend. It is reported once as inapplicable, never as failed, and
// an order that declares nothing keeps running beside it. On a Dolt city the
// same order dispatches.
func TestOrderRequiresBackendSkipsMismatchedScopes(t *testing.T) {
	doltOnly := orders.Order{
		Name: "dolt-only", Trigger: "cooldown", Interval: "1m",
		Exec: "scripts/dolt-only.sh", RequiresBackend: []string{"dolt"},
	}
	anywhere := orders.Order{
		Name: "anywhere", Trigger: "cooldown", Interval: "1m",
		Exec: "scripts/anywhere.sh",
	}
	run := func(t *testing.T, cityPath string) (map[string]int, *recordingRecorder) {
		t.Helper()
		var mu sync.Mutex
		calls := map[string]int{}
		execRun := func(_ context.Context, command, _ string, _ []string) ([]byte, error) {
			mu.Lock()
			defer mu.Unlock()
			calls[command]++
			return []byte("ok"), nil
		}
		rec := &recordingRecorder{}
		ad := buildOrderDispatcherFromListExec([]orders.Order{doltOnly, anywhere}, beads.NewMemStore(), nil, execRun, rec)
		now := time.Now()
		for i := range 3 {
			ad.dispatch(context.Background(), cityPath, now.Add(time.Duration(i)*2*time.Minute))
			ad.drain(context.Background())
		}
		mu.Lock()
		defer mu.Unlock()
		out := map[string]int{}
		for k, v := range calls {
			out[k] = v
		}
		return out, rec
	}

	t.Run("http city", func(t *testing.T) {
		cityPath := orderBackendTestCity(t, `{"database":"beads","backend":"http"}`)
		calls, rec := run(t, cityPath)
		if calls["scripts/dolt-only.sh"] != 0 {
			t.Fatalf("dolt-only order ran %d times on an http city, want 0", calls["scripts/dolt-only.sh"])
		}
		if calls["scripts/anywhere.sh"] == 0 {
			t.Fatal("an order with no requires_backend did not run on the http city")
		}
		inapplicable := rec.ofType(events.OrderInapplicable)
		if len(inapplicable) != 1 {
			t.Fatalf("order.inapplicable events = %d, want exactly 1 across ticks", len(inapplicable))
		}
		var payload events.OrderInapplicablePayload
		if err := json.Unmarshal(inapplicable[0].Payload, &payload); err != nil {
			t.Fatalf("decode payload: %v", err)
		}
		if payload.OrderName != "dolt-only" || payload.Backend != "http" || payload.Scope != "city" ||
			strings.Join(payload.RequiresBackend, ",") != "dolt" {
			t.Fatalf("payload = %+v, want dolt-only / city / http / [dolt]", payload)
		}
		if inapplicable[0].Subject != "dolt-only" {
			t.Fatalf("subject = %q, want dolt-only", inapplicable[0].Subject)
		}
		for _, e := range append(rec.ofType(events.OrderFailed), rec.ofType(events.OrderFired)...) {
			if e.Subject == "dolt-only" {
				t.Fatalf("inapplicable order produced %s", e.Type)
			}
		}
	})

	t.Run("dolt city", func(t *testing.T) {
		cityPath := orderBackendTestCity(t, `{"database":"dolt","backend":"dolt","dolt_database":"hq"}`)
		calls, rec := run(t, cityPath)
		if calls["scripts/dolt-only.sh"] == 0 {
			t.Fatal("dolt-only order did not run on a dolt city")
		}
		if n := len(rec.ofType(events.OrderInapplicable)); n != 0 {
			t.Fatalf("order.inapplicable events on a dolt city = %d, want 0", n)
		}
	})
}

// A rebuilt dispatcher (config reload) does not re-announce an order it
// already reported inapplicable: the report is once per controller boot.
func TestOrderInapplicableReportCarriesAcrossDispatcherRebuild(t *testing.T) {
	cityPath := orderBackendTestCity(t, `{"database":"beads","backend":"http"}`)
	aa := []orders.Order{{
		Name: "dolt-only", Trigger: "cooldown", Interval: "1m",
		Exec: "true", RequiresBackend: []string{"dolt"},
	}}
	rec := &recordingRecorder{}
	noop := func(context.Context, string, string, []string) ([]byte, error) { return nil, nil }
	first := buildOrderDispatcherFromListExec(aa, beads.NewMemStore(), nil, noop, rec).(*memoryOrderDispatcher)
	first.dispatch(context.Background(), cityPath, time.Now())
	second := buildOrderDispatcherFromListExec(aa, beads.NewMemStore(), nil, noop, rec).(*memoryOrderDispatcher)
	second.carryInapplicableReportsFrom(first)
	second.dispatch(context.Background(), cityPath, time.Now())
	if n := len(rec.ofType(events.OrderInapplicable)); n != 1 {
		t.Fatalf("order.inapplicable events = %d, want 1", n)
	}
}

// gc order list marks an order inapplicable for its scope's backend, in the
// table and in JSON, and leaves the table unchanged when nothing is.
func TestOrderListReportsInapplicableOrders(t *testing.T) {
	cityPath := orderBackendTestCity(t, `{"database":"beads","backend":"http"}`)
	cfg := &config.City{}
	aa := []orders.Order{
		{Name: "dolt-only", Trigger: "cooldown", Interval: "1m", Exec: "true", RequiresBackend: []string{"dolt"}},
		{Name: "anywhere", Trigger: "cooldown", Interval: "1m", Exec: "true"},
	}
	var table bytes.Buffer
	if code := doOrderListForCity(cityPath, cfg, aa, &table); code != 0 {
		t.Fatalf("doOrderListForCity = %d", code)
	}
	lines := strings.Split(strings.TrimSpace(table.String()), "\n")
	if !strings.Contains(lines[0], "STATUS") {
		t.Fatalf("header missing STATUS column:\n%s", table.String())
	}
	for _, line := range lines[1:] {
		switch {
		case strings.HasPrefix(line, "dolt-only"):
			if !strings.HasSuffix(line, "inapplicable (backend http)") {
				t.Fatalf("dolt-only row = %q, want inapplicable (backend http)", line)
			}
		case strings.HasPrefix(line, "anywhere"):
			if strings.Contains(line, "inapplicable") {
				t.Fatalf("anywhere row = %q, want applicable", line)
			}
		}
	}

	var plain bytes.Buffer
	doOrderListForCity(cityPath, cfg, aa[1:], &plain)
	if strings.Contains(plain.String(), "STATUS") {
		t.Fatalf("STATUS column shown with nothing inapplicable:\n%s", plain.String())
	}

	var js bytes.Buffer
	if code := doOrderListJSON(cityPath, cfg, aa, &js); code != 0 {
		t.Fatalf("doOrderListJSON = %d", code)
	}
	var payload struct {
		Orders []struct {
			Name            string   `json:"name"`
			RequiresBackend []string `json:"requires_backend"`
			Inapplicable    string   `json:"inapplicable"`
		} `json:"orders"`
	}
	if err := json.Unmarshal(js.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v\n%s", err, js.String())
	}
	got := map[string]string{}
	for _, o := range payload.Orders {
		got[o.Name] = o.Inapplicable
		if o.Name == "dolt-only" && strings.Join(o.RequiresBackend, ",") != "dolt" {
			t.Fatalf("requires_backend = %v, want [dolt]", o.RequiresBackend)
		}
	}
	if got["dolt-only"] != "backend http" || got["anywhere"] != "" {
		t.Fatalf("inapplicable = %v, want dolt-only=backend http, anywhere empty", got)
	}
}
