package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/orders"
)

// --- gc order list ---

func TestOrderListEmpty(t *testing.T) {
	var stdout bytes.Buffer
	code := doOrderList(nil, &stdout)
	if code != 0 {
		t.Fatalf("doOrderList = %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), "No orders found") {
		t.Errorf("stdout = %q, want 'No orders found'", stdout.String())
	}
}

func TestOrderList(t *testing.T) {
	aa := []orders.Order{
		{Name: "digest", Gate: "cooldown", Interval: "24h", Pool: "dog", Formula: "mol-digest"},
		{Name: "cleanup", Gate: "cron", Schedule: "0 3 * * *", Formula: "mol-cleanup"},
		{Name: "deploy", Gate: "manual", Formula: "mol-deploy"},
	}

	var stdout bytes.Buffer
	code := doOrderList(aa, &stdout)
	if code != 0 {
		t.Fatalf("doOrderList = %d, want 0", code)
	}
	out := stdout.String()
	for _, want := range []string{"digest", "cooldown", "24h", "dog", "cleanup", "cron", "deploy", "manual", "TYPE", "formula"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout missing %q:\n%s", want, out)
		}
	}
}

func TestOrderListExecType(t *testing.T) {
	aa := []orders.Order{
		{Name: "poll", Gate: "cooldown", Interval: "2m", Exec: "scripts/poll.sh"},
		{Name: "digest", Gate: "cooldown", Interval: "24h", Formula: "mol-digest"},
	}

	var stdout bytes.Buffer
	code := doOrderList(aa, &stdout)
	if code != 0 {
		t.Fatalf("doOrderList = %d, want 0", code)
	}
	out := stdout.String()
	if !strings.Contains(out, "exec") {
		t.Errorf("stdout missing 'exec' type:\n%s", out)
	}
	if !strings.Contains(out, "formula") {
		t.Errorf("stdout missing 'formula' type:\n%s", out)
	}
}

func TestCityOrderRootsUseLocalFormulaLayerForVisibleRoot(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, "orders"), 0o755); err != nil {
		t.Fatal(err)
	}

	roots := cityOrderRoots(cityDir, &config.City{})
	visibleRoot := filepath.Join(cityDir, "orders")
	wantLayer := filepath.Join(cityDir, "formulas")
	for _, root := range roots {
		if root.Dir != visibleRoot {
			continue
		}
		if root.FormulaLayer != wantLayer {
			t.Fatalf("FormulaLayer = %q, want %q", root.FormulaLayer, wantLayer)
		}
		return
	}
	t.Fatalf("cityOrderRoots() missing %q", visibleRoot)
}

func TestCityOrderRootsDedupesLegacyLocalRoot(t *testing.T) {
	cityDir := t.TempDir()
	legacyRoot := filepath.Join(cityDir, ".gc", "formulas", "orders")
	if err := os.MkdirAll(legacyRoot, 0o755); err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{}
	cfg.Formulas.Dir = ".gc/formulas"
	roots := cityOrderRoots(cityDir, cfg)

	var count int
	for _, root := range roots {
		if root.Dir == legacyRoot {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("legacy order root appeared %d times, want 1", count)
	}
}

// TestCityOrderRootsIncludesPackDirs, TestCityOrderRootsScansOnDiskPacks,
// and TestCityOrderRootsLocalOverridesOnDiskPack were removed — system packs
// now go through LoadWithIncludes extraIncludes → ExpandCityPacks → FormulaLayers
// instead of the old PackDirs and packs/*/ on-disk scan paths.

func TestCityOrderRootsPackDirsDedupe(t *testing.T) {
	cityDir := t.TempDir()

	// Pack whose formulas dir is also a formula layer already.
	packDir := filepath.Join(cityDir, "packs", "alpha")
	formulasDir := filepath.Join(packDir, "formulas")
	if err := os.MkdirAll(filepath.Join(formulasDir, "orders"), 0o755); err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{}
	cfg.FormulaLayers.City = []string{formulasDir}
	cfg.PackDirs = []string{packDir}

	roots := cityOrderRoots(cityDir, cfg)

	ordersDir := filepath.Join(formulasDir, "orders")
	var count int
	for _, root := range roots {
		if root.Dir == ordersDir {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("pack order root appeared %d times, want 1 (dedup)", count)
	}
}

// --- gc order show ---

func TestOrderShow(t *testing.T) {
	aa := []orders.Order{
		{
			Name:        "digest",
			Description: "Generate daily digest",
			Formula:     "mol-digest",
			Gate:        "cooldown",
			Interval:    "24h",
			Pool:        "dog",
			Source:      "/city/formulas/orders/digest/order.toml",
		},
	}

	var stdout, stderr bytes.Buffer
	code := doOrderShow(aa, "digest", "", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doOrderShow = %d, want 0; stderr: %s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"digest", "Generate daily digest", "mol-digest", "cooldown", "24h", "dog", "order.toml"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout missing %q:\n%s", want, out)
		}
	}
}

func TestOrderShowExec(t *testing.T) {
	aa := []orders.Order{
		{
			Name:        "poll",
			Description: "Poll wasteland",
			Exec:        "$ORDER_DIR/scripts/poll.sh",
			Gate:        "cooldown",
			Interval:    "2m",
			Source:      "/city/formulas/orders/poll/order.toml",
		},
	}

	var stdout, stderr bytes.Buffer
	code := doOrderShow(aa, "poll", "", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doOrderShow = %d, want 0; stderr: %s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "Exec:") {
		t.Errorf("stdout missing 'Exec:' line:\n%s", out)
	}
	if !strings.Contains(out, "scripts/poll.sh") {
		t.Errorf("stdout missing script path:\n%s", out)
	}
	// Should NOT show Formula: line.
	if strings.Contains(out, "Formula:") {
		t.Errorf("stdout should not contain 'Formula:' for exec order:\n%s", out)
	}
}

func TestOrderShowNotFound(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := doOrderShow(nil, "nonexistent", "", &stdout, &stderr)
	if code != 1 {
		t.Fatalf("doOrderShow = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "not found") {
		t.Errorf("stderr = %q, want 'not found'", stderr.String())
	}
}

// --- gc order check ---

func TestOrderCheck(t *testing.T) {
	aa := []orders.Order{
		{Name: "digest", Gate: "cooldown", Interval: "24h", Formula: "mol-digest"},
	}
	now := time.Date(2026, 2, 27, 12, 0, 0, 0, time.UTC)
	neverRan := func(_ string) (time.Time, error) { return time.Time{}, nil }

	var stdout bytes.Buffer
	code := doOrderCheck(aa, now, neverRan, nil, nil, &stdout)
	if code != 0 {
		t.Fatalf("doOrderCheck = %d, want 0 (due)", code)
	}
	out := stdout.String()
	if !strings.Contains(out, "digest") {
		t.Errorf("stdout missing 'digest':\n%s", out)
	}
	if !strings.Contains(out, "yes") {
		t.Errorf("stdout missing 'yes':\n%s", out)
	}
}

func TestOrderCheckNoneDue(t *testing.T) {
	aa := []orders.Order{
		{Name: "deploy", Gate: "manual", Formula: "mol-deploy"},
	}
	now := time.Date(2026, 2, 27, 12, 0, 0, 0, time.UTC)
	neverRan := func(_ string) (time.Time, error) { return time.Time{}, nil }

	var stdout bytes.Buffer
	code := doOrderCheck(aa, now, neverRan, nil, nil, &stdout)
	if code != 1 {
		t.Fatalf("doOrderCheck = %d, want 1 (none due)", code)
	}
}

func TestOrderCheckEmpty(t *testing.T) {
	now := time.Date(2026, 2, 27, 12, 0, 0, 0, time.UTC)
	neverRan := func(_ string) (time.Time, error) { return time.Time{}, nil }

	var stdout bytes.Buffer
	code := doOrderCheck(nil, now, neverRan, nil, nil, &stdout)
	if code != 1 {
		t.Fatalf("doOrderCheck = %d, want 1 (empty)", code)
	}
}

func TestOrderLastRunFn(t *testing.T) {
	// Simulate a bead store that returns one result for "order-run:digest".
	store := beads.NewBdStore(t.TempDir(), func(_, _ string, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		if strings.Contains(joined, "--label=order-run:digest") {
			return []byte(`[{"id":"bd-aaa","title":"digest wisp","status":"open","issue_type":"task","created_at":"2026-02-27T10:00:00Z","labels":["order-run:digest"]}]`), nil
		}
		return []byte(`[]`), nil
	})

	fn := orderLastRunFn(store)

	// Known order — returns CreatedAt.
	got, err := fn("digest")
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 2, 27, 10, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("lastRun = %v, want %v", got, want)
	}

	// Unknown order — returns zero time.
	got, err = fn("unknown")
	if err != nil {
		t.Fatal(err)
	}
	if !got.IsZero() {
		t.Errorf("lastRun = %v, want zero time", got)
	}
}

func TestOrderCheckWithLastRun(t *testing.T) {
	aa := []orders.Order{
		{Name: "digest", Gate: "cooldown", Interval: "24h", Formula: "mol-digest"},
	}
	now := time.Date(2026, 2, 27, 12, 0, 0, 0, time.UTC)
	// Last ran 1 hour ago — cooldown of 24h means NOT due.
	recentRun := func(_ string) (time.Time, error) {
		return now.Add(-1 * time.Hour), nil
	}

	var stdout bytes.Buffer
	code := doOrderCheck(aa, now, recentRun, nil, nil, &stdout)
	if code != 1 {
		t.Fatalf("doOrderCheck = %d, want 1 (not due)", code)
	}
	out := stdout.String()
	if !strings.Contains(out, "no") {
		t.Errorf("stdout missing 'no':\n%s", out)
	}
	if !strings.Contains(out, "cooldown") {
		t.Errorf("stdout missing 'cooldown':\n%s", out)
	}
}

// --- gc order run ---

func TestOrderRun(t *testing.T) {
	aa := []orders.Order{
		{Name: "digest", Formula: "mol-digest", Gate: "cooldown", Interval: "24h", Pool: "dog", FormulaLayer: sharedTestFormulaDir},
	}

	store := beads.NewMemStore()

	// SlingRunner still handles the route command.
	calls := []string{}
	fakeRunner := func(_, cmd string, _ map[string]string) (string, error) {
		calls = append(calls, cmd)
		return "", nil
	}

	var stdout, stderr bytes.Buffer
	code := doOrderRun(aa, "digest", "", "/city", fakeRunner, store, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doOrderRun = %d, want 0; stderr: %s", code, stderr.String())
	}

	if len(calls) != 1 {
		t.Fatalf("got %d runner calls, want 1: %v", len(calls), calls)
	}
	// Should include both order-run label and pool label in a single bd update.
	if !strings.Contains(calls[0], "--add-label=order-run:digest") {
		t.Errorf("call[0] = %q, want --add-label=order-run:digest", calls[0])
	}
	if !strings.Contains(calls[0], "--add-label=pool:dog") {
		t.Errorf("call[0] = %q, want --add-label=pool:dog", calls[0])
	}
}

func TestOrderRunNoPool(t *testing.T) {
	aa := []orders.Order{
		{Name: "cleanup", Formula: "mol-cleanup", Gate: "cron", Schedule: "0 3 * * *", FormulaLayer: sharedTestFormulaDir},
	}

	store := beads.NewMemStore()

	calls := []string{}
	fakeRunner := func(_, cmd string, _ map[string]string) (string, error) {
		calls = append(calls, cmd)
		return "", nil
	}

	var stdout, stderr bytes.Buffer
	code := doOrderRun(aa, "cleanup", "", "/city", fakeRunner, store, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doOrderRun = %d, want 0; stderr: %s", code, stderr.String())
	}

	// Order with no pool still gets an order-run label via bd update.
	if len(calls) != 1 {
		t.Fatalf("got %d runner calls, want 1: %v", len(calls), calls)
	}
	if !strings.Contains(calls[0], "--add-label=order-run:cleanup") {
		t.Errorf("call[0] = %q, want --add-label=order-run:cleanup", calls[0])
	}
	// Should NOT contain pool label.
	if strings.Contains(calls[0], "--add-label=pool:") {
		t.Errorf("call[0] = %q, should not contain pool label", calls[0])
	}
	// Verify wisp ID appears in stdout (MemStore generates gc-N IDs).
	if !strings.Contains(stdout.String(), "gc-1") {
		t.Errorf("stdout missing wisp ID: %s", stdout.String())
	}
}

func TestOrderRunGraphWorkflowDecoratesStepRouting(t *testing.T) {
	cityDir := t.TempDir()
	formulaDir := t.TempDir()

	cityToml := `[workspace]
name = "test-city"

[daemon]
formula_v2 = true

[[agent]]
name = "quinn"
max_active_sessions = 1
`
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	graphFormula := `
formula = "graph-work"
version = 2

[[steps]]
id = "step"
title = "Do work"
`
	if err := os.WriteFile(filepath.Join(formulaDir, "graph-work.formula.toml"), []byte(graphFormula), 0o644); err != nil {
		t.Fatal(err)
	}

	aa := []orders.Order{
		{Name: "acceptance-patrol", Formula: "graph-work", Gate: "cooldown", Interval: "15m", Pool: "quinn", FormulaLayer: formulaDir},
	}
	store := beads.NewMemStore()

	calls := []string{}
	fakeRunner := func(_, cmd string, _ map[string]string) (string, error) {
		calls = append(calls, cmd)
		return "", nil
	}

	var stdout, stderr bytes.Buffer
	code := doOrderRun(aa, "acceptance-patrol", "", cityDir, fakeRunner, store, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doOrderRun = %d, want 0; stderr: %s", code, stderr.String())
	}
	if len(calls) != 1 {
		t.Fatalf("got %d runner calls, want 1: %v", len(calls), calls)
	}

	all, err := store.ListOpen()
	if err != nil {
		t.Fatalf("store.ListOpen(): %v", err)
	}

	foundWorker := false
	foundControl := false
	for _, bead := range all {
		switch bead.Title {
		case "Do work":
			if bead.Assignee != "quinn" {
				t.Fatalf("worker assignee = %q, want quinn", bead.Assignee)
			}
			if bead.Metadata["gc.routed_to"] != "quinn" {
				t.Fatalf("worker gc.routed_to = %q, want quinn", bead.Metadata["gc.routed_to"])
			}
			foundWorker = true
		case "Finalize workflow":
			if bead.Assignee != config.ControlDispatcherAgentName {
				t.Fatalf("finalizer assignee = %q, want %q", bead.Assignee, config.ControlDispatcherAgentName)
			}
			if bead.Metadata["gc.routed_to"] != config.ControlDispatcherAgentName {
				t.Fatalf("finalizer gc.routed_to = %q, want %q", bead.Metadata["gc.routed_to"], config.ControlDispatcherAgentName)
			}
			if bead.Metadata[graphExecutionRouteMetaKey] != "quinn" {
				t.Fatalf("finalizer execution route = %q, want quinn", bead.Metadata[graphExecutionRouteMetaKey])
			}
			foundControl = true
		}
	}

	if !foundWorker {
		t.Fatal("missing routed worker step")
	}
	if !foundControl {
		t.Fatal("missing routed workflow finalizer")
	}
}

func TestOrderRunNotFound(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := doOrderRun(nil, "nonexistent", "", "/city", nil, nil, nil, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("doOrderRun = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "not found") {
		t.Errorf("stderr = %q, want 'not found'", stderr.String())
	}
}

// --- gc order history ---

func TestOrderHistory(t *testing.T) {
	store := beads.NewBdStore(t.TempDir(), func(_, _ string, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		if strings.Contains(joined, "--label=order-run:digest") {
			return []byte(`[{"id":"WP-42","title":"digest wisp","status":"closed","issue_type":"task","created_at":"2026-02-27T10:00:00Z","labels":["order-run:digest"]}]`), nil
		}
		if strings.Contains(joined, "--label=order-run:cleanup") {
			return []byte(`[{"id":"WP-99","title":"cleanup wisp","status":"open","issue_type":"task","created_at":"2026-02-27T11:00:00Z","labels":["order-run:cleanup"]}]`), nil
		}
		return []byte(`[]`), nil
	})

	aa := []orders.Order{
		{Name: "digest", Formula: "mol-digest"},
		{Name: "cleanup", Formula: "mol-cleanup"},
	}

	var stdout bytes.Buffer
	code := doOrderHistory("", "", aa, store, &stdout)
	if code != 0 {
		t.Fatalf("doOrderHistory = %d, want 0", code)
	}
	out := stdout.String()
	// Table header.
	if !strings.Contains(out, "ORDER") {
		t.Errorf("stdout missing 'ORDER':\n%s", out)
	}
	if !strings.Contains(out, "BEAD") {
		t.Errorf("stdout missing 'BEAD':\n%s", out)
	}
	// Both orders should appear.
	if !strings.Contains(out, "digest") {
		t.Errorf("stdout missing 'digest':\n%s", out)
	}
	if !strings.Contains(out, "WP-42") {
		t.Errorf("stdout missing 'WP-42':\n%s", out)
	}
	if !strings.Contains(out, "cleanup") {
		t.Errorf("stdout missing 'cleanup':\n%s", out)
	}
	if !strings.Contains(out, "WP-99") {
		t.Errorf("stdout missing 'WP-99':\n%s", out)
	}
}

func TestOrderHistoryNamed(t *testing.T) {
	store := beads.NewBdStore(t.TempDir(), func(_, _ string, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		if strings.Contains(joined, "--label=order-run:digest") {
			return []byte(`[{"id":"WP-42","title":"digest wisp","status":"closed","issue_type":"task","created_at":"2026-02-27T10:00:00Z","labels":["order-run:digest"]}]`), nil
		}
		return []byte(`[]`), nil
	})

	aa := []orders.Order{
		{Name: "digest", Formula: "mol-digest"},
		{Name: "cleanup", Formula: "mol-cleanup"},
	}

	var stdout bytes.Buffer
	code := doOrderHistory("digest", "", aa, store, &stdout)
	if code != 0 {
		t.Fatalf("doOrderHistory = %d, want 0", code)
	}
	out := stdout.String()
	if !strings.Contains(out, "digest") {
		t.Errorf("stdout missing 'digest':\n%s", out)
	}
	if !strings.Contains(out, "WP-42") {
		t.Errorf("stdout missing 'WP-42':\n%s", out)
	}
	// Should NOT contain cleanup (filtered by name).
	if strings.Contains(out, "cleanup") {
		t.Errorf("stdout should not contain 'cleanup':\n%s", out)
	}
}

func TestOrderHistoryEmpty(t *testing.T) {
	store := beads.NewBdStore(t.TempDir(), func(_, _ string, _ ...string) ([]byte, error) {
		return []byte(`[]`), nil
	})

	aa := []orders.Order{
		{Name: "digest", Formula: "mol-digest"},
	}

	var stdout bytes.Buffer
	code := doOrderHistory("", "", aa, store, &stdout)
	if code != 0 {
		t.Fatalf("doOrderHistory = %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), "No order history") {
		t.Errorf("stdout = %q, want 'No order history'", stdout.String())
	}
}

// --- rig-scoped tests ---

func TestOrderListWithRig(t *testing.T) {
	aa := []orders.Order{
		{Name: "digest", Gate: "cooldown", Interval: "24h", Pool: "dog", Formula: "mol-digest"},
		{Name: "db-health", Gate: "cooldown", Interval: "5m", Pool: "polecat", Formula: "mol-db-health", Rig: "demo-repo"},
	}

	var stdout bytes.Buffer
	code := doOrderList(aa, &stdout)
	if code != 0 {
		t.Fatalf("doOrderList = %d, want 0", code)
	}
	out := stdout.String()
	// RIG column should appear because at least one order has a rig.
	if !strings.Contains(out, "RIG") {
		t.Errorf("stdout missing 'RIG' column:\n%s", out)
	}
	if !strings.Contains(out, "demo-repo") {
		t.Errorf("stdout missing 'demo-repo':\n%s", out)
	}
}

func TestOrderListCityOnly(t *testing.T) {
	aa := []orders.Order{
		{Name: "digest", Gate: "cooldown", Interval: "24h", Pool: "dog", Formula: "mol-digest"},
	}

	var stdout bytes.Buffer
	code := doOrderList(aa, &stdout)
	if code != 0 {
		t.Fatalf("doOrderList = %d, want 0", code)
	}
	out := stdout.String()
	// No RIG column when all orders are city-level.
	if strings.Contains(out, "RIG") {
		t.Errorf("stdout should not have 'RIG' column for city-only:\n%s", out)
	}
}

func TestFindOrderRigScoped(t *testing.T) {
	aa := []orders.Order{
		{Name: "dolt-health", Gate: "cooldown", Interval: "1h", Formula: "mol-dh"},
		{Name: "dolt-health", Gate: "cooldown", Interval: "5m", Formula: "mol-dh", Rig: "repo-a"},
		{Name: "dolt-health", Gate: "cooldown", Interval: "10m", Formula: "mol-dh", Rig: "repo-b"},
	}

	// No rig → first match (city-level).
	a, ok := findOrder(aa, "dolt-health", "")
	if !ok {
		t.Fatal("findOrder with empty rig should find city order")
	}
	if a.Rig != "" {
		t.Errorf("expected city order, got rig=%q", a.Rig)
	}

	// Exact rig match.
	a, ok = findOrder(aa, "dolt-health", "repo-b")
	if !ok {
		t.Fatal("findOrder with rig=repo-b should find rig order")
	}
	if a.Rig != "repo-b" {
		t.Errorf("expected rig=repo-b, got rig=%q", a.Rig)
	}

	// Non-existent rig.
	_, ok = findOrder(aa, "dolt-health", "repo-z")
	if ok {
		t.Error("findOrder with non-existent rig should not find anything")
	}
}

func TestOrderCheckWithRig(t *testing.T) {
	aa := []orders.Order{
		{Name: "digest", Gate: "cooldown", Interval: "24h", Formula: "mol-digest"},
		{Name: "db-health", Gate: "cooldown", Interval: "5m", Formula: "mol-db-health", Rig: "demo-repo"},
	}
	now := time.Date(2026, 2, 27, 12, 0, 0, 0, time.UTC)
	neverRan := func(_ string) (time.Time, error) { return time.Time{}, nil }

	var stdout bytes.Buffer
	code := doOrderCheck(aa, now, neverRan, nil, nil, &stdout)
	if code != 0 {
		t.Fatalf("doOrderCheck = %d, want 0", code)
	}
	out := stdout.String()
	if !strings.Contains(out, "RIG") {
		t.Errorf("stdout missing 'RIG' column:\n%s", out)
	}
	if !strings.Contains(out, "demo-repo") {
		t.Errorf("stdout missing 'demo-repo':\n%s", out)
	}
}

func TestOrderShowWithRig(t *testing.T) {
	aa := []orders.Order{
		{Name: "db-health", Formula: "mol-db-health", Gate: "cooldown", Interval: "5m", Rig: "demo-repo", Source: "/topo/orders/db-health/order.toml"},
	}

	var stdout, stderr bytes.Buffer
	code := doOrderShow(aa, "db-health", "demo-repo", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doOrderShow = %d, want 0; stderr: %s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "Rig:") {
		t.Errorf("stdout missing 'Rig:' line:\n%s", out)
	}
	if !strings.Contains(out, "demo-repo") {
		t.Errorf("stdout missing 'demo-repo':\n%s", out)
	}
}

func TestOrderRunRigQualifiesPool(t *testing.T) {
	aa := []orders.Order{
		{Name: "db-health", Formula: "mol-db-health", Gate: "cooldown", Interval: "5m", Pool: "polecat", Rig: "demo-repo", FormulaLayer: sharedTestFormulaDir},
	}

	store := beads.NewMemStore()

	calls := []string{}
	fakeRunner := func(_, cmd string, _ map[string]string) (string, error) {
		calls = append(calls, cmd)
		return "", nil
	}

	var stdout, stderr bytes.Buffer
	code := doOrderRun(aa, "db-health", "demo-repo", "/city", fakeRunner, store, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doOrderRun = %d, want 0; stderr: %s", code, stderr.String())
	}

	if len(calls) != 1 {
		t.Fatalf("got %d runner calls, want 1: %v", len(calls), calls)
	}
	// Scoped order-run label.
	if !strings.Contains(calls[0], "--add-label=order-run:db-health:rig:demo-repo") {
		t.Errorf("call[0] = %q, want --add-label=order-run:db-health:rig:demo-repo", calls[0])
	}
	// Auto-qualified pool.
	if !strings.Contains(calls[0], "--add-label=pool:demo-repo/polecat") {
		t.Errorf("call[0] = %q, want --add-label=pool:demo-repo/polecat", calls[0])
	}
}
