package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/orders"
)

const (
	// orderHistoryTestOrder is the single order these bound tests exercise.
	orderHistoryTestOrder = "digest"
	// orderHistoryTestRunCount is how many runs the fixture seeds — comfortably
	// more than any bound under test, so a bound that silently fails to apply
	// shows up as a count mismatch.
	orderHistoryTestRunCount = 25
)

// orderHistoryRunsStore builds a store holding orderHistoryTestRunCount
// order-run beads for orderHistoryTestOrder, one per hour going back from now,
// newest first. It goes through the bd-backed store rather than the in-memory
// one because MemStore.Create stamps its own CreatedAt, and these tests are
// entirely about created_at ordering and windows.
func orderHistoryRunsStore(t *testing.T, now time.Time) beads.Store {
	t.Helper()
	rows := make([]string, 0, orderHistoryTestRunCount)
	for i := 0; i < orderHistoryTestRunCount; i++ {
		rows = append(rows, fmt.Sprintf(
			`{"id":"WP-%d","title":"run %d","status":"closed","issue_type":"task","created_at":%q,"labels":["order-run:%s"]}`,
			i, i, now.Add(-time.Duration(i)*time.Hour).Format(time.RFC3339Nano), orderHistoryTestOrder,
		))
	}
	payload := []byte("[" + strings.Join(rows, ",") + "]")
	return beads.NewBdStore(t.TempDir(), func(_, _ string, args ...string) ([]byte, error) {
		if strings.Contains(strings.Join(args, " "), "--label=order-run:"+orderHistoryTestOrder) {
			return payload, nil
		}
		return []byte(`[]`), nil
	})
}

func orderHistoryEntries(t *testing.T, stdout *bytes.Buffer) orderHistoryJSONResult {
	t.Helper()
	var payload orderHistoryJSONResult
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout.String())
	}
	return payload
}

// TestOrderHistoryLimitCapsEntries pins the `--limit` contract: an operator who
// bounds the read gets at most that many runs back. Without a bound the command
// pulls every retained run, which measured 22s on a city with 11k+ order-run
// rows and made the command unusable interactively (ga-klv).
func TestOrderHistoryLimitCapsEntries(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	store := orderHistoryRunsStore(t, now)
	aa := []orders.Order{{Name: "digest", Formula: "mol-digest"}}
	resolver := func(orders.Order) ([]beads.OrdersStore, error) {
		return []beads.OrdersStore{{Store: store}}, nil
	}

	var stdout, stderr bytes.Buffer
	code := doOrderHistoryBounded("digest", "", aa, resolver, orderHistoryBounds{Limit: 5}, true, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doOrderHistoryBounded = %d, want 0; stderr: %s", code, stderr.String())
	}

	payload := orderHistoryEntries(t, &stdout)
	if len(payload.Entries) != 5 {
		t.Fatalf("entries = %d, want 5 (the requested limit)", len(payload.Entries))
	}
	if payload.Summary.Total != 5 {
		t.Fatalf("summary.total = %d, want 5", payload.Summary.Total)
	}
	// Newest-first: the limit must keep the most recent runs, not an arbitrary
	// slice — a history listing that drops the newest rows is useless.
	if !payload.Entries[0].CreatedAt.Equal(now) {
		t.Fatalf("first entry created_at = %s, want the newest run %s", payload.Entries[0].CreatedAt, now)
	}
}

// TestOrderHistoryLimitZeroStaysUnlimited keeps the escape hatch honest: an
// operator who explicitly asks for everything still gets everything.
func TestOrderHistoryLimitZeroStaysUnlimited(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	store := orderHistoryRunsStore(t, now)
	aa := []orders.Order{{Name: "digest", Formula: "mol-digest"}}
	resolver := func(orders.Order) ([]beads.OrdersStore, error) {
		return []beads.OrdersStore{{Store: store}}, nil
	}

	var stdout, stderr bytes.Buffer
	code := doOrderHistoryBounded("digest", "", aa, resolver, orderHistoryBounds{Limit: 0}, true, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doOrderHistoryBounded = %d, want 0; stderr: %s", code, stderr.String())
	}
	if payload := orderHistoryEntries(t, &stdout); len(payload.Entries) != orderHistoryTestRunCount {
		t.Fatalf("entries = %d, want all %d", len(payload.Entries), orderHistoryTestRunCount)
	}
}

// TestOrderHistorySinceDropsOlderRuns pins the `--since` contract.
func TestOrderHistorySinceDropsOlderRuns(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	store := orderHistoryRunsStore(t, now)
	aa := []orders.Order{{Name: "digest", Formula: "mol-digest"}}
	resolver := func(orders.Order) ([]beads.OrdersStore, error) {
		return []beads.OrdersStore{{Store: store}}, nil
	}

	var stdout, stderr bytes.Buffer
	// Runs are one hour apart. The window deliberately lands between two of
	// them (5h30m) rather than exactly on one: the cutoff is computed at call
	// time, a hair after the fixture's `now`, so a window landing exactly on a
	// run would flip that run in or out on timing alone.
	window := 5*time.Hour + 30*time.Minute
	code := doOrderHistoryBounded("digest", "", aa, resolver, orderHistoryBounds{Since: window}, true, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doOrderHistoryBounded = %d, want 0; stderr: %s", code, stderr.String())
	}

	payload := orderHistoryEntries(t, &stdout)
	if len(payload.Entries) != 6 {
		t.Fatalf("entries = %d, want the 6 runs (0h..5h old) inside the %s window", len(payload.Entries), window)
	}
	cutoff := now.Add(-window)
	for i, e := range payload.Entries {
		if e.CreatedAt.Before(cutoff) {
			t.Fatalf("entry %d created_at = %s, older than the --since cutoff %s", i, e.CreatedAt, cutoff)
		}
	}
}

// TestOrderHistoryLimitAndSinceCompose checks the combined semantic: at most
// Limit runs, none older than Since.
func TestOrderHistoryLimitAndSinceCompose(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	store := orderHistoryRunsStore(t, now)
	aa := []orders.Order{{Name: "digest", Formula: "mol-digest"}}
	resolver := func(orders.Order) ([]beads.OrdersStore, error) {
		return []beads.OrdersStore{{Store: store}}, nil
	}

	var stdout, stderr bytes.Buffer
	code := doOrderHistoryBounded("digest", "", aa, resolver, orderHistoryBounds{Limit: 3, Since: 10 * time.Hour}, true, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doOrderHistoryBounded = %d, want 0; stderr: %s", code, stderr.String())
	}
	if payload := orderHistoryEntries(t, &stdout); len(payload.Entries) != 3 {
		t.Fatalf("entries = %d, want 3 (limit wins inside a wider since window)", len(payload.Entries))
	}
}

// TestOrderHistoryDefaultLimitIsBounded is the regression guard for the
// interactive-cost half of ga-klv: the command must ship a positive default
// bound. An unbounded default is what made a plain `gc order history` take 22s.
func TestOrderHistoryDefaultLimitIsBounded(t *testing.T) {
	if defaultOrderHistoryLimit <= 0 {
		t.Fatalf("defaultOrderHistoryLimit = %d, want positive; an unbounded default restores the 22s interactive read", defaultOrderHistoryLimit)
	}

	var stdout, stderr bytes.Buffer
	cmd := newOrderHistoryCmd(&stdout, &stderr)
	for _, name := range []string{"limit", "since"} {
		if cmd.Flags().Lookup(name) == nil {
			t.Fatalf("gc order history is missing the --%s flag", name)
		}
	}
	limitFlag := cmd.Flags().Lookup("limit")
	if limitFlag.DefValue != fmt.Sprint(defaultOrderHistoryLimit) {
		t.Fatalf("--limit default = %q, want %d", limitFlag.DefValue, defaultOrderHistoryLimit)
	}
}

// TestOrderHistoryRejectsBadSince keeps the duration parse at the CLI edge, so a
// typo fails fast with a usable message instead of being silently ignored.
func TestOrderHistoryRejectsBadSince(t *testing.T) {
	var stdout, stderr bytes.Buffer
	cmd := newOrderHistoryCmd(&stdout, &stderr)
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"--since", "yesterday"})

	if err := cmd.Execute(); err == nil {
		t.Fatal("gc order history --since yesterday returned nil error, want a parse failure")
	}
	if combined := stdout.String() + stderr.String(); !strings.Contains(combined, "since") {
		t.Fatalf("output = %q, want it to name the bad --since value", combined)
	}
}

// TestOrderHistoryLimitReachesStoreQuery proves the bound is pushed into the
// read rather than applied only after fetching everything. Trimming client-side
// still pays the full fetch and serialization cost this bead is about.
func TestOrderHistoryLimitReachesStoreQuery(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	spy := &orderHistoryListSpy{Store: orderHistoryRunsStore(t, now)}
	aa := []orders.Order{{Name: "digest", Formula: "mol-digest"}}
	resolver := func(orders.Order) ([]beads.OrdersStore, error) {
		return []beads.OrdersStore{{Store: spy}}, nil
	}

	var stdout, stderr bytes.Buffer
	if code := doOrderHistoryBounded("digest", "", aa, resolver, orderHistoryBounds{Limit: 5}, true, &stdout, &stderr); code != 0 {
		t.Fatalf("doOrderHistoryBounded = %d, want 0; stderr: %s", code, stderr.String())
	}
	if len(spy.queries) == 0 {
		t.Fatal("no list query reached the store")
	}
	for i, q := range spy.queries {
		if q.Limit != 5 {
			t.Fatalf("query %d: Limit = %d, want 5 pushed into the read", i, q.Limit)
		}
	}
}

// TestOrderHistoryUnlimitedAvoidsAPIRoute pins the routing half of the
// `--limit 0` contract. The API has no wire representation for an unlimited
// read: GetOrderHistory omits `limit` when it is non-positive, and the server
// then applies its own default of 20 rows. An API-routed `--limit 0` would
// therefore silently return 20 runs while the flag help promises every
// retained one, so an unbounded read must stay on the local iterator.
func TestOrderHistoryUnlimitedAvoidsAPIRoute(t *testing.T) {
	t.Setenv("GC_DEBUG", "1") // force route=... lines into the stderr buffer

	cityPath := writeOrderHistoryTestCity(t)
	cfg, err := loadCityConfig(cityPath, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("loadCityConfig: %v", err)
	}
	aa, code := loadAllOrders(cityPath, cfg, &bytes.Buffer{}, "test")
	if code != 0 {
		t.Fatalf("loadAllOrders = %d", code)
	}

	// A non-nil client pointed at a port that always refuses. The only reason
	// to skip the API here is the unlimited bound itself: if the routing
	// regresses and consults the API, routeRead logs route=api or a fallback
	// whose reason comes from api.FallbackReason (route_read.go:44-77) — never
	// "unlimited" — so the assertion below still catches it without a server.
	c := api.NewCityScopedClient("http://127.0.0.1:1", "test-city")

	var stdout, stderr bytes.Buffer
	if got := routeOrderHistory(cityPath, cfg, "digest", "", aa, c, "", orderHistoryBounds{}, false, &stdout, &stderr); got != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", got, stderr.String())
	}
	if !strings.Contains(stderr.String(), "route=fallback reason=unlimited") {
		t.Fatalf("stderr missing %q:\n%s", "route=fallback reason=unlimited", stderr.String())
	}
}

// TestOrderHistoryRejectsNegativeLimit keeps 0 as the only spelling of
// "unlimited". A negative limit was previously accepted and read two different
// ways — unlimited locally, the server's 20-row default over the API — so it
// is rejected at the CLI edge instead.
func TestOrderHistoryRejectsNegativeLimit(t *testing.T) {
	var stdout, stderr bytes.Buffer
	cmd := newOrderHistoryCmd(&stdout, &stderr)
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"--limit", "-1"})

	if err := cmd.Execute(); err == nil {
		t.Fatal("gc order history --limit -1 returned nil error, want a validation failure")
	}
	if combined := stdout.String() + stderr.String(); !strings.Contains(combined, "--limit") {
		t.Fatalf("output = %q, want it to name --limit", combined)
	}
}

type orderHistoryListSpy struct {
	beads.Store
	queries []beads.ListQuery
}

func (s *orderHistoryListSpy) List(q beads.ListQuery) ([]beads.Bead, error) {
	s.queries = append(s.queries, q)
	return s.Store.List(q)
}
