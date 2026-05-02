package main

import (
	"bytes"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
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
		{Name: "digest", Trigger: "cooldown", Interval: "24h", Pool: "dog", Formula: "mol-digest"},
		{Name: "cleanup", Trigger: "cron", Schedule: "0 3 * * *", Formula: "mol-cleanup"},
		{Name: "deploy", Trigger: "manual", Formula: "mol-deploy"},
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
		{Name: "poll", Trigger: "cooldown", Interval: "2m", Exec: "scripts/poll.sh"},
		{Name: "digest", Trigger: "cooldown", Interval: "24h", Formula: "mol-digest"},
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

func TestCityOrderRootsUseTopLevelPackOrders(t *testing.T) {
	cityDir := t.TempDir()
	packDir := filepath.Join(cityDir, "packs", "alpha")
	formulasDir := filepath.Join(packDir, "formulas")
	ordersDir := filepath.Join(packDir, "orders")
	if err := os.MkdirAll(formulasDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(ordersDir, 0o755); err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{}
	cfg.FormulaLayers.City = []string{formulasDir}

	roots := cityOrderRoots(cityDir, cfg)
	for _, root := range roots {
		if root.Dir != ordersDir {
			continue
		}
		if root.FormulaLayer != formulasDir {
			t.Fatalf("FormulaLayer = %q, want %q", root.FormulaLayer, formulasDir)
		}
		return
	}
	t.Fatalf("cityOrderRoots() missing %q", ordersDir)
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
		if root.Dir == citylayout.OrdersPath(cityDir) {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("visible order root appeared %d times, want 1", count)
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

	ordersDir := filepath.Join(packDir, "orders")
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

func TestScanAllOrdersCityFlatFile(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, "orders"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityDir, "formulas"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "orders", "health-check.order.toml"), []byte(`
[order]
formula = "health-check"
trigger = "cron"
schedule = "*/5 * * * *"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{}
	cfg.FormulaLayers.City = []string{filepath.Join(cityDir, "formulas")}

	var stderr bytes.Buffer
	aa, err := scanAllOrders(cityDir, cfg, &stderr, "gc order list")
	if err != nil {
		t.Fatalf("scanAllOrders: %v", err)
	}
	if len(aa) != 1 {
		t.Fatalf("got %d orders, want 1", len(aa))
	}
	if aa[0].Name != "health-check" {
		t.Fatalf("Name = %q, want %q", aa[0].Name, "health-check")
	}
	if aa[0].Source != filepath.Join(cityDir, "orders", "health-check.order.toml") {
		t.Fatalf("Source = %q, want %q", aa[0].Source, filepath.Join(cityDir, "orders", "health-check.order.toml"))
	}
}

func TestScanAllOrdersRemoteImportedFlatPackOrders(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	cityDir := t.TempDir()
	source := "https://github.com/example/orders-pack.git"
	repoDir := filepath.Join(home, "orders-pack-work")
	if err := os.MkdirAll(filepath.Join(repoDir, "formulas"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repoDir, "orders"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(repoDir, "pack.toml"), []byte(`
[pack]
name = "ops"
schema = 1
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "orders", "health-check.order.toml"), []byte(`
[order]
formula = "health-check"
trigger = "cron"
schedule = "*/5 * * * *"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGitImport(t, repoDir, "init")
	mustGitImport(t, repoDir, "add", ".")
	mustGitImport(t, repoDir, "commit", "-m", "initial")
	commit := gitOutputImport(t, repoDir, "rev-parse", "HEAD")

	cacheDir := filepath.Join(home, ".gc", "cache", "repos", config.RepoCacheKey(source, commit))
	if err := os.MkdirAll(filepath.Dir(cacheDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(repoDir, cacheDir); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`
[workspace]
name = "test"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "pack.toml"), []byte(`
[pack]
name = "test"
schema = 1

[imports.ops]
source = "https://github.com/example/orders-pack.git"
version = "^1.2"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "packs.lock"), []byte(`
schema = 1

[packs."https://github.com/example/orders-pack.git"]
version = "1.2.3"
commit = "`+commit+`"
fetched = "2026-04-10T00:00:00Z"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadCityConfig(cityDir)
	if err != nil {
		t.Fatalf("loadCityConfig: %v", err)
	}

	var stderr bytes.Buffer
	aa, err := scanAllOrders(cityDir, cfg, &stderr, "gc order list")
	if err != nil {
		t.Fatalf("scanAllOrders: %v", err)
	}
	if len(aa) != 1 {
		t.Fatalf("got %d orders, want 1", len(aa))
	}
	if aa[0].Name != "health-check" {
		t.Fatalf("Name = %q, want %q", aa[0].Name, "health-check")
	}
	if aa[0].Source != filepath.Join(cacheDir, "orders", "health-check.order.toml") {
		t.Fatalf("Source = %q, want %q", aa[0].Source, filepath.Join(cacheDir, "orders", "health-check.order.toml"))
	}
}

func TestScanAllOrdersCityLegacyFormulaOrdersWarns(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, "formulas", "orders", "health-check"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "formulas", "orders", "health-check", "order.toml"), []byte(`
[order]
formula = "health-check"
trigger = "cron"
schedule = "*/5 * * * *"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{}
	cfg.FormulaLayers.City = []string{filepath.Join(cityDir, "formulas")}

	var stderr bytes.Buffer
	logs := captureCmdOrderLogs(t, func() {
		aa, err := scanAllOrders(cityDir, cfg, &stderr, "gc order list")
		if err != nil {
			t.Fatalf("scanAllOrders: %v", err)
		}
		if len(aa) != 1 {
			t.Fatalf("got %d orders, want 1", len(aa))
		}
		if aa[0].Source != filepath.Join(cityDir, "formulas", "orders", "health-check", "order.toml") {
			t.Fatalf("Source = %q, want %q", aa[0].Source, filepath.Join(cityDir, "formulas", "orders", "health-check", "order.toml"))
		}
	})

	if !strings.Contains(logs, "move to orders/health-check.toml") {
		t.Fatalf("logs = %q, want move warning", logs)
	}
}

// --- gc order show ---

func TestOrderShow(t *testing.T) {
	aa := []orders.Order{
		{
			Name:        "digest",
			Description: "Generate daily digest",
			Formula:     "mol-digest",
			Trigger:     "cooldown",
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

func captureCmdOrderLogs(t *testing.T, fn func()) string {
	t.Helper()

	var buf bytes.Buffer
	origWriter := log.Writer()
	origFlags := log.Flags()
	origPrefix := log.Prefix()
	log.SetOutput(&buf)
	log.SetFlags(0)
	log.SetPrefix("")
	defer func() {
		log.SetOutput(origWriter)
		log.SetFlags(origFlags)
		log.SetPrefix(origPrefix)
	}()

	fn()
	return buf.String()
}

func TestOrderShowExec(t *testing.T) {
	aa := []orders.Order{
		{
			Name:        "poll",
			Description: "Poll wasteland",
			Exec:        "$ORDER_DIR/scripts/poll.sh",
			Trigger:     "cooldown",
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
		{Name: "digest", Trigger: "cooldown", Interval: "24h", Formula: "mol-digest"},
	}
	now := time.Date(2026, 2, 27, 12, 0, 0, 0, time.UTC)
	neverRan := func(_ string) (time.Time, error) { return time.Time{}, nil }

	var stdout bytes.Buffer
	code := doOrderCheck(aa, now, neverRan, &stdout)
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
		{Name: "deploy", Trigger: "manual", Formula: "mol-deploy"},
	}
	now := time.Date(2026, 2, 27, 12, 0, 0, 0, time.UTC)
	neverRan := func(_ string) (time.Time, error) { return time.Time{}, nil }

	var stdout bytes.Buffer
	code := doOrderCheck(aa, now, neverRan, &stdout)
	if code != 1 {
		t.Fatalf("doOrderCheck = %d, want 1 (none due)", code)
	}
}

func TestOrderCheckEmpty(t *testing.T) {
	now := time.Date(2026, 2, 27, 12, 0, 0, 0, time.UTC)
	neverRan := func(_ string) (time.Time, error) { return time.Time{}, nil }

	var stdout bytes.Buffer
	code := doOrderCheck(nil, now, neverRan, &stdout)
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
		{Name: "digest", Trigger: "cooldown", Interval: "24h", Formula: "mol-digest"},
	}
	now := time.Date(2026, 2, 27, 12, 0, 0, 0, time.UTC)
	// Last ran 1 hour ago — cooldown of 24h means NOT due.
	recentRun := func(_ string) (time.Time, error) {
		return now.Add(-1 * time.Hour), nil
	}

	var stdout bytes.Buffer
	code := doOrderCheck(aa, now, recentRun, &stdout)
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

func TestOrderCheckWithStoresResolverUsesRigStore(t *testing.T) {
	cityStore := beads.NewMemStore()
	rigStore := beads.NewMemStore()
	if _, err := rigStore.Create(beads.Bead{
		Title:  "recent rig run",
		Labels: []string{"order-run:digest:rig:frontend"},
	}); err != nil {
		t.Fatal(err)
	}

	aa := []orders.Order{{
		Name:     "digest",
		Rig:      "frontend",
		Trigger:  "cooldown",
		Interval: "24h",
		Formula:  "mol-digest",
	}}
	resolver := func(a orders.Order) ([]beads.Store, error) {
		if a.Rig == "frontend" {
			return []beads.Store{rigStore}, nil
		}
		return []beads.Store{cityStore}, nil
	}

	var stdout, stderr bytes.Buffer
	code := doOrderCheckWithStoresResolver(aa, time.Now().Add(time.Second), nil, resolver, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("doOrderCheckWithStoresResolver = %d, want 1 (rig cooldown active); stderr: %s; stdout: %s", code, stderr.String(), stdout.String())
	}
	if !strings.Contains(stdout.String(), "no") {
		t.Fatalf("stdout missing not-due row:\n%s", stdout.String())
	}
}

func TestOrderCheckWithStoresResolverUsesLegacyCityStore(t *testing.T) {
	cityStore := beads.NewMemStore()
	rigStore := beads.NewMemStore()
	if _, err := cityStore.Create(beads.Bead{
		Title:  "legacy rig run",
		Labels: []string{"order-run:digest:rig:frontend"},
	}); err != nil {
		t.Fatal(err)
	}

	aa := []orders.Order{{
		Name:     "digest",
		Rig:      "frontend",
		Trigger:  "cooldown",
		Interval: "24h",
		Formula:  "mol-digest",
	}}
	resolver := func(a orders.Order) ([]beads.Store, error) {
		if a.Rig == "frontend" {
			return []beads.Store{rigStore, cityStore}, nil
		}
		return []beads.Store{cityStore}, nil
	}

	var stdout, stderr bytes.Buffer
	code := doOrderCheckWithStoresResolver(aa, time.Now().Add(time.Second), nil, resolver, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("doOrderCheckWithStoresResolver = %d, want 1 (legacy city cooldown active); stderr: %s; stdout: %s", code, stderr.String(), stdout.String())
	}
	if !strings.Contains(stdout.String(), "no") {
		t.Fatalf("stdout missing not-due row:\n%s", stdout.String())
	}
}

func TestOrderCheckConditionUsesCityScope(t *testing.T) {
	cityDir := t.TempDir()
	orderDir := filepath.Join(cityDir, "packs", "workflows", "orders")
	check := fmt.Sprintf(
		`test "$GC_CITY_PATH" = '%s' && test "$GC_STORE_ROOT" = '%s' && test "$GC_STORE_SCOPE" = city && test "$ORDER_DIR" = '%s'`,
		cityDir,
		cityDir,
		orderDir,
	)
	aa := []orders.Order{{
		Name:    "pr-review-router",
		Trigger: "condition",
		Check:   check,
		Formula: "mol-pr-review-router",
		Pool:    "workflows.pr-review-router",
		Source:  filepath.Join(orderDir, "pr-review-router.toml"),
	}}
	resolver := func(orders.Order) ([]beads.Store, error) {
		return []beads.Store{beads.NewMemStore()}, nil
	}

	var stdout, stderr bytes.Buffer
	code := doOrderCheckWithStoresResolverScoped(cityDir, &config.City{}, aa, time.Now(), nil, resolver, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doOrderCheckWithStoresResolverScoped = %d, want 0; stderr: %s; stdout: %s", code, stderr.String(), stdout.String())
	}
	if !strings.Contains(stdout.String(), "yes") {
		t.Fatalf("stdout missing due row:\n%s", stdout.String())
	}
}

func TestOrderCheckWithStoresResolverFailsWhenLegacyEventCursorReadFails(t *testing.T) {
	rigStore := beads.NewMemStore()
	legacyStore := labelFailListStore{
		Store:     beads.NewMemStore(),
		failLabel: "order:watch:rig:frontend",
	}
	eventLog := events.NewFake()
	eventLog.Record(events.Event{Type: events.BeadClosed, Actor: "test"})

	aa := []orders.Order{{
		Name:    "watch",
		Rig:     "frontend",
		Trigger: "event",
		On:      events.BeadClosed,
		Formula: "mol-watch",
	}}
	resolver := func(a orders.Order) ([]beads.Store, error) {
		if a.Rig == "frontend" {
			return []beads.Store{rigStore, legacyStore}, nil
		}
		return []beads.Store{rigStore}, nil
	}

	var stdout, stderr bytes.Buffer
	code := doOrderCheckWithStoresResolver(aa, time.Now(), eventLog, resolver, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("doOrderCheckWithStoresResolver = %d, want 1 when legacy event cursor cannot be read; stdout: %s", code, stdout.String())
	}
	if !strings.Contains(stderr.String(), "event cursor") {
		t.Fatalf("stderr missing event cursor error:\n%s", stderr.String())
	}
}

func TestOrderCheckWithStoresResolverFailsWhenLegacyLastRunReadFails(t *testing.T) {
	rigStore := beads.NewMemStore()
	legacyStore := labelFailListStore{
		Store:     beads.NewMemStore(),
		failLabel: "order-run:digest:rig:frontend",
	}

	aa := []orders.Order{{
		Name:     "digest",
		Rig:      "frontend",
		Trigger:  "cooldown",
		Interval: "24h",
		Formula:  "mol-digest",
	}}
	resolver := func(a orders.Order) ([]beads.Store, error) {
		if a.Rig == "frontend" {
			return []beads.Store{rigStore, legacyStore}, nil
		}
		return []beads.Store{rigStore}, nil
	}

	var stdout, stderr bytes.Buffer
	code := doOrderCheckWithStoresResolver(aa, time.Now(), nil, resolver, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("doOrderCheckWithStoresResolver = %d, want 1 when legacy last-run state cannot be read; stdout: %s", code, stdout.String())
	}
	if !strings.Contains(stderr.String(), "last run") {
		t.Fatalf("stderr missing last-run error:\n%s", stderr.String())
	}
}

// --- gc order run ---

func TestOrderRun(t *testing.T) {
	aa := []orders.Order{
		{Name: "digest", Formula: "mol-digest", Trigger: "cooldown", Interval: "24h", Pool: "dog", FormulaLayer: sharedTestFormulaDir},
	}

	store := beads.NewMemStore()

	var stdout, stderr bytes.Buffer
	code := doOrderRun(aa, "digest", "", "/city", store, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doOrderRun = %d, want 0; stderr: %s", code, stderr.String())
	}

	results, err := store.ListByLabel("order-run:digest", 0)
	if err != nil {
		t.Fatalf("store.ListByLabel(): %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("store.ListByLabel() len = %d, want 1 (%#v)", len(results), results)
	}
	if got := results[0].Metadata["gc.routed_to"]; got != "dog" {
		t.Fatalf("gc.routed_to = %q, want dog", got)
	}
}

func TestOrderRunResolvesPackBindingForPool(t *testing.T) {
	aa := []orders.Order{
		{Name: "digest", Formula: "mol-digest", Trigger: "cooldown", Interval: "24h", Pool: "dog", FormulaLayer: sharedTestFormulaDir},
	}
	cityDir := t.TempDir()
	writeOrderRunImportFixture(t, cityDir, "maintenance")
	store := beads.NewMemStore()

	var stdout, stderr bytes.Buffer
	code := doOrderRun(aa, "digest", "", cityDir, store, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doOrderRun = %d, want 0; stderr: %s", code, stderr.String())
	}

	results, err := store.ListByLabel("order-run:digest", 0)
	if err != nil {
		t.Fatalf("store.ListByLabel(): %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("store.ListByLabel() len = %d, want 1 (%#v)", len(results), results)
	}
	if got := results[0].Metadata["gc.routed_to"]; got != "maintenance.dog" {
		t.Fatalf("gc.routed_to = %q, want maintenance.dog", got)
	}
	if !strings.Contains(stdout.String(), "gc.routed_to=maintenance.dog") {
		t.Fatalf("stdout = %q, want binding-qualified route", stdout.String())
	}
}

func TestOrderRunResolvesImportedPackPoolAgainstCityShadow(t *testing.T) {
	cityDir := t.TempDir()
	writeImportedDogOrderFixture(t, cityDir, true)
	_, aa := loadImportedDogOrders(t, cityDir)
	store := beads.NewMemStore()

	var stdout, stderr bytes.Buffer
	code := doOrderRun(aa, "digest", "", cityDir, store, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doOrderRun = %d, want 0; stderr: %s", code, stderr.String())
	}

	results, err := store.ListByLabel("order-run:digest", 0)
	if err != nil {
		t.Fatalf("store.ListByLabel(): %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("store.ListByLabel() len = %d, want 1 (%#v)", len(results), results)
	}
	if got := results[0].Metadata["gc.routed_to"]; got != "maintenance.dog" {
		t.Fatalf("gc.routed_to = %q, want maintenance.dog", got)
	}
}

func TestOrderRunResolvesImportedPackPoolAgainstSiblingImportCollision(t *testing.T) {
	cityDir := t.TempDir()
	writeImportedDogOrderFixture(t, cityDir, false, "gastown")
	_, aa := loadImportedDogOrders(t, cityDir)
	store := beads.NewMemStore()

	var stdout, stderr bytes.Buffer
	code := doOrderRun(aa, "digest", "", cityDir, store, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doOrderRun = %d, want 0; stderr: %s", code, stderr.String())
	}

	results, err := store.ListByLabel("order-run:digest", 0)
	if err != nil {
		t.Fatalf("store.ListByLabel(): %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("store.ListByLabel() len = %d, want 1 (%#v)", len(results), results)
	}
	if got := results[0].Metadata["gc.routed_to"]; got != "maintenance.dog" {
		t.Fatalf("gc.routed_to = %q, want maintenance.dog", got)
	}
}

func TestOrderRunPrefersCityShadowForPool(t *testing.T) {
	aa := []orders.Order{
		{Name: "digest", Formula: "mol-digest", Trigger: "cooldown", Interval: "24h", Pool: "dog", FormulaLayer: sharedTestFormulaDir},
	}
	cityDir := t.TempDir()
	writeOrderRunImportFixture(t, cityDir, "maintenance")
	writeFile(t, filepath.Join(cityDir, "city.toml"), `[workspace]
name = "shadow-city"
prefix = "shd"

[[agent]]
name = "dog"
`)
	store := beads.NewMemStore()

	var stdout, stderr bytes.Buffer
	code := doOrderRun(aa, "digest", "", cityDir, store, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doOrderRun = %d, want 0; stderr: %s", code, stderr.String())
	}

	results, err := store.ListByLabel("order-run:digest", 0)
	if err != nil {
		t.Fatalf("store.ListByLabel(): %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("store.ListByLabel() len = %d, want 1 (%#v)", len(results), results)
	}
	if got := results[0].Metadata["gc.routed_to"]; got != "dog" {
		t.Fatalf("gc.routed_to = %q, want dog", got)
	}
	if !strings.Contains(stdout.String(), "gc.routed_to=dog") {
		t.Fatalf("stdout = %q, want city-local route", stdout.String())
	}
}

func TestOrderRunRejectsAmbiguousPackPool(t *testing.T) {
	aa := []orders.Order{
		{Name: "digest", Formula: "mol-digest", Trigger: "cooldown", Interval: "24h", Pool: "dog", FormulaLayer: sharedTestFormulaDir},
	}
	cityDir := t.TempDir()
	writeOrderRunImportFixture(t, cityDir, "gastown", "maintenance")
	store := beads.NewMemStore()

	var stdout, stderr bytes.Buffer
	code := doOrderRun(aa, "digest", "", cityDir, store, nil, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("doOrderRun = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), `ambiguous pool "dog"`) {
		t.Fatalf("stderr = %q, want ambiguity error", stderr.String())
	}
	results, err := store.ListByLabel("order-run:digest", 0)
	if err != nil {
		t.Fatalf("store.ListByLabel(): %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("store.ListByLabel() len = %d, want 0 (%#v)", len(results), results)
	}
}

func writeOrderRunImportFixture(t *testing.T, cityDir string, bindings ...string) {
	t.Helper()

	packRoot := filepath.Join(cityDir, "packs")
	if err := os.MkdirAll(packRoot, 0o755); err != nil {
		t.Fatal(err)
	}

	writeFile(t, filepath.Join(cityDir, "city.toml"), `
[workspace]
name = "test-city"
`)

	var packToml strings.Builder
	packToml.WriteString(`
[pack]
name = "test-city"
schema = 1
`)
	for _, binding := range bindings {
		packDir := filepath.Join(packRoot, binding)
		if err := os.MkdirAll(packDir, 0o755); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(packDir, "pack.toml"), `
[pack]
name = "`+binding+`"
schema = 1

[[agent]]
name = "dog"
scope = "city"
`)
		packToml.WriteString(`
[imports.` + binding + `]
source = "./packs/` + binding + `"
`)
	}
	writeFile(t, filepath.Join(cityDir, "pack.toml"), packToml.String())
}

func TestOrderRunNoPool(t *testing.T) {
	aa := []orders.Order{
		{Name: "cleanup", Formula: "mol-cleanup", Trigger: "cron", Schedule: "0 3 * * *", FormulaLayer: sharedTestFormulaDir},
	}

	store := beads.NewMemStore()

	var stdout, stderr bytes.Buffer
	code := doOrderRun(aa, "cleanup", "", "/city", store, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doOrderRun = %d, want 0; stderr: %s", code, stderr.String())
	}

	results, err := store.ListByLabel("order-run:cleanup", 0)
	if err != nil {
		t.Fatalf("store.ListByLabel(): %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("store.ListByLabel() len = %d, want 1 (%#v)", len(results), results)
	}
	if got := results[0].Metadata["gc.routed_to"]; got != "" {
		t.Fatalf("gc.routed_to = %q, want empty", got)
	}
	// Verify wisp ID appears in stdout (MemStore generates gc-N IDs).
	if !strings.Contains(stdout.String(), "gc-1") {
		t.Errorf("stdout missing wisp ID: %s", stdout.String())
	}
}

func TestOrderRunReportsAllMissingRequiredVarsAtOnce(t *testing.T) {
	dir := t.TempDir()
	formulaBody := `
formula = "order-required-vars"
version = 1

[vars.target_id]
description = "Bead being worked on"
required = true

[vars.workspace]
description = "Workspace path"
required = true

[[steps]]
id = "do-work"
title = "Do work for {{target_id}}"
description = "Target: {{target_id}}, workspace: {{workspace}}"
`
	if err := os.WriteFile(filepath.Join(dir, "order-required-vars.formula.toml"), []byte(formulaBody), 0o644); err != nil {
		t.Fatal(err)
	}

	aa := []orders.Order{{
		Name:         "digest",
		Formula:      "order-required-vars",
		Trigger:      "cooldown",
		Interval:     "24h",
		FormulaLayer: dir,
	}}

	store := beads.NewMemStore()
	var stdout, stderr bytes.Buffer
	code := doOrderRun(aa, "digest", "", "/city", store, nil, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("doOrderRun = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}

	errText := stderr.String()
	if !strings.Contains(errText, `variable "target_id" is required`) {
		t.Fatalf("stderr = %q, want missing target_id reported", errText)
	}
	if !strings.Contains(errText, `variable "workspace" is required`) {
		t.Fatalf("stderr = %q, want missing workspace reported", errText)
	}
	if strings.Contains(errText, "bead title contains unresolved variable(s)") {
		t.Fatalf("stderr = %q, want consolidated required-var validation instead of title-only failure", errText)
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
contract = "graph.v2"

[[steps]]
id = "step"
title = "Do work"
`
	if err := os.WriteFile(filepath.Join(formulaDir, "graph-work.formula.toml"), []byte(graphFormula), 0o644); err != nil {
		t.Fatal(err)
	}

	aa := []orders.Order{
		{Name: "acceptance-patrol", Formula: "graph-work", Trigger: "cooldown", Interval: "15m", Pool: "quinn", FormulaLayer: formulaDir},
	}
	store := beads.NewMemStore()

	var stdout, stderr bytes.Buffer
	code := doOrderRun(aa, "acceptance-patrol", "", cityDir, store, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doOrderRun = %d, want 0; stderr: %s", code, stderr.String())
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
			if bead.Metadata["gc.routed_to"] != "" {
				t.Fatalf("finalizer gc.routed_to = %q, want empty for concrete control dispatcher assignee", bead.Metadata["gc.routed_to"])
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
	code := doOrderRun(nil, "nonexistent", "", "/city", nil, nil, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("doOrderRun = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "not found") {
		t.Errorf("stderr = %q, want 'not found'", stderr.String())
	}
}

func TestOrderRunExecRigUsesScopedWorkdirAndStoreEnv(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "frontend")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(cityDir, "city.toml"), `[workspace]
name = "test-city"
prefix = "ct"

[[rigs]]
name = "frontend"
path = "frontend"
prefix = "fe"
`)
	cfg, err := loadCityConfig(cityDir)
	if err != nil {
		t.Fatalf("loadCityConfig: %v", err)
	}
	outPath := filepath.Join(cityDir, "exec-env.txt")
	a := orders.Order{
		Name:     "poll",
		Rig:      "frontend",
		Trigger:  "cooldown",
		Interval: "1m",
		Exec:     fmt.Sprintf(`pwd > %q && printf '%%s\n%%s\n%%s\n%%s\n%%s\n%%s\n' "$BEADS_DIR" "$GC_STORE_ROOT" "$GC_STORE_SCOPE" "$GC_BEADS_PREFIX" "$GC_RIG" "$GC_RIG_ROOT" >> %q`, outPath, outPath),
	}

	var stdout, stderr bytes.Buffer
	code := doOrderRunExec(a, cityDir, cfg, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doOrderRunExec = %d, want 0; stderr: %s", code, stderr.String())
	}
	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("ReadFile(exec-env): %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 7 {
		t.Fatalf("exec env lines = %d, want 7 (%q)", len(lines), string(data))
	}
	if lines[0] != rigDir {
		t.Fatalf("pwd = %q, want %q", lines[0], rigDir)
	}
	if lines[1] != filepath.Join(rigDir, ".beads") {
		t.Fatalf("BEADS_DIR = %q, want %q", lines[1], filepath.Join(rigDir, ".beads"))
	}
	if lines[2] != rigDir {
		t.Fatalf("GC_STORE_ROOT = %q, want %q", lines[2], rigDir)
	}
	if lines[3] != "rig" {
		t.Fatalf("GC_STORE_SCOPE = %q, want rig", lines[3])
	}
	if lines[4] != "fe" {
		t.Fatalf("GC_BEADS_PREFIX = %q, want fe", lines[4])
	}
	if lines[5] != "frontend" {
		t.Fatalf("GC_RIG = %q, want frontend", lines[5])
	}
	if lines[6] != rigDir {
		t.Fatalf("GC_RIG_ROOT = %q, want %q", lines[6], rigDir)
	}
}

func TestOrderRunExecProjectsExternalDoltTarget(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(cityDir, "city.toml"), `[workspace]
name = "test-city"
prefix = "ct"
`)
	if err := os.MkdirAll(filepath.Join(cityDir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(cityDir, ".beads", "config.yaml"), strings.Join([]string{
		"issue_prefix: ct",
		"gc.endpoint_origin: city_canonical",
		"gc.endpoint_status: verified",
		"dolt.auto-start: false",
		"dolt.host: external.example.internal",
		"dolt.port: 4406",
		"dolt.user: maintenance-user",
		"",
	}, "\n"))
	cfg, err := loadCityConfig(cityDir)
	if err != nil {
		t.Fatalf("loadCityConfig: %v", err)
	}

	t.Setenv("GC_DOLT_HOST", "ambient.invalid")
	t.Setenv("GC_DOLT_PORT", "9999")
	outPath := filepath.Join(cityDir, "exec-dolt-env.txt")
	a := orders.Order{
		Name:     "poll",
		Trigger:  "cooldown",
		Interval: "1m",
		Exec:     fmt.Sprintf(`printf '%%s\n%%s\n%%s\n' "$GC_DOLT_HOST" "$GC_DOLT_PORT" "$GC_DOLT_USER" > %q`, outPath),
	}

	var stdout, stderr bytes.Buffer
	code := doOrderRunExec(a, cityDir, cfg, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doOrderRunExec = %d, want 0; stderr: %s", code, stderr.String())
	}
	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("ReadFile(exec-dolt-env): %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 3 {
		t.Fatalf("exec dolt env lines = %d, want 3 (%q)", len(lines), string(data))
	}
	if lines[0] != "external.example.internal" {
		t.Fatalf("GC_DOLT_HOST = %q, want external.example.internal", lines[0])
	}
	if lines[1] != "4406" {
		t.Fatalf("GC_DOLT_PORT = %q, want 4406", lines[1])
	}
	if lines[2] != "maintenance-user" {
		t.Fatalf("GC_DOLT_USER = %q, want maintenance-user", lines[2])
	}
}

func TestOrderRunExecPreservesAuthOnlyOverridesForManagedLocal(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".beads", "dolt"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(cityDir, "city.toml"), `[workspace]
name = "test-city"
prefix = "ct"
`)
	if err := os.MkdirAll(filepath.Join(cityDir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(cityDir, ".beads", "config.yaml"), strings.Join([]string{
		"issue_prefix: ct",
		"gc.endpoint_origin: managed_city",
		"gc.endpoint_status: verified",
		"dolt.auto-start: false",
		"",
	}, "\n"))
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer func() {
		if err := listener.Close(); err != nil {
			t.Fatalf("Close listener: %v", err)
		}
	}()
	port := fmt.Sprint(listener.Addr().(*net.TCPAddr).Port)
	stateDir := filepath.Join(cityDir, ".gc", "runtime", "packs", "dolt")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(stateDir, "dolt-state.json"), fmt.Sprintf(
		`{"running":true,"pid":%d,"port":%s,"data_dir":%q}`,
		os.Getpid(),
		port,
		filepath.Join(cityDir, ".beads", "dolt"),
	))
	cfg, err := loadCityConfig(cityDir)
	if err != nil {
		t.Fatalf("loadCityConfig: %v", err)
	}

	t.Setenv("GC_DOLT_HOST", "ambient.invalid")
	t.Setenv("GC_DOLT_PORT", "9999")
	t.Setenv("GC_DOLT_USER", "ambient-user")
	t.Setenv("GC_DOLT_PASSWORD", "ambient-secret")
	t.Setenv("BEADS_DOLT_SERVER_HOST", "ambient-beads.invalid")
	t.Setenv("BEADS_DOLT_SERVER_PORT", "9998")
	t.Setenv("BEADS_DOLT_SERVER_USER", "ambient-beads-user")
	t.Setenv("BEADS_DOLT_PASSWORD", "ambient-beads-secret")
	outPath := filepath.Join(cityDir, "exec-dolt-env.txt")
	a := orders.Order{
		Name:     "poll",
		Trigger:  "cooldown",
		Interval: "1m",
		Exec: fmt.Sprintf(
			`printf 'host=<%%s>\nport=<%%s>\nuser=<%%s>\npass=<%%s>\nbeads_host=<%%s>\nbeads_port=<%%s>\nbeads_user=<%%s>\nbeads_pass=<%%s>\n' "$GC_DOLT_HOST" "$GC_DOLT_PORT" "$GC_DOLT_USER" "$GC_DOLT_PASSWORD" "$BEADS_DOLT_SERVER_HOST" "$BEADS_DOLT_SERVER_PORT" "$BEADS_DOLT_SERVER_USER" "$BEADS_DOLT_PASSWORD" > %q`,
			outPath,
		),
	}

	var stdout, stderr bytes.Buffer
	code := doOrderRunExec(a, cityDir, cfg, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doOrderRunExec = %d, want 0; stderr: %s", code, stderr.String())
	}
	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("ReadFile(exec-dolt-env): %v", err)
	}
	got := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	want := []string{
		"host=<>",
		"port=<" + port + ">",
		"user=<ambient-user>",
		"pass=<ambient-secret>",
		"beads_host=<>",
		"beads_port=<" + port + ">",
		"beads_user=<ambient-user>",
		"beads_pass=<ambient-secret>",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("exec dolt env:\ngot:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestOrderRunExecMarksExternalDoltTargetForManagedLocalOnlyOrders(t *testing.T) {
	cityDir := t.TempDir()
	t.Setenv("GC_PACK_STATE_DIR", filepath.Join(t.TempDir(), "poison-pack-state"))
	t.Setenv("GC_DOLT_DATA_DIR", filepath.Join(t.TempDir(), "poison-dolt-data"))
	t.Setenv("GC_DOLT_CONFIG_FILE", filepath.Join(t.TempDir(), "poison-dolt-config.yaml"))
	t.Setenv("GC_DOLT_STATE_FILE", filepath.Join(t.TempDir(), "poison-state.json"))
	if err := os.MkdirAll(filepath.Join(cityDir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(cityDir, "city.toml"), `[workspace]
name = "test-city"
prefix = "ct"
`)
	writeFile(t, filepath.Join(cityDir, ".beads", "config.yaml"), strings.Join([]string{
		"issue_prefix: ct",
		"gc.endpoint_origin: city_canonical",
		"gc.endpoint_status: verified",
		"dolt.auto-start: false",
		"dolt.host: external.example.internal",
		"dolt.port: 4406",
		"",
	}, "\n"))
	cfg, err := loadCityConfig(cityDir)
	if err != nil {
		t.Fatalf("loadCityConfig: %v", err)
	}

	outPath := filepath.Join(cityDir, "exec-managed-marker.txt")
	a := orders.Order{
		Name:     "dolt-gc-nudge",
		Trigger:  "cooldown",
		Interval: "1m",
		Exec:     fmt.Sprintf(`printf 'managed=<%%s>\nhost=<%%s>\nport=<%%s>\npack_state=<%%s>\ndata=<%%s>\nconfig=<%%s>\nstate=<%%s>\n' "$GC_DOLT_MANAGED_LOCAL" "$GC_DOLT_HOST" "$GC_DOLT_PORT" "$GC_PACK_STATE_DIR" "$GC_DOLT_DATA_DIR" "$GC_DOLT_CONFIG_FILE" "$GC_DOLT_STATE_FILE" > %q`, outPath),
	}

	var stdout, stderr bytes.Buffer
	code := doOrderRunExec(a, cityDir, cfg, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doOrderRunExec = %d, want 0; stderr: %s", code, stderr.String())
	}
	got := strings.TrimSpace(readFileString(t, outPath))
	externalRoot := filepath.Join(cityDir, ".gc", "runtime", "packs", "dolt", "external-target")
	want := strings.Join([]string{
		"managed=<0>",
		"host=<external.example.internal>",
		"port=<4406>",
		"pack_state=<>",
		"data=<" + externalRoot + ">",
		"config=<" + filepath.Join(externalRoot, "dolt-config.yaml") + ">",
		"state=<" + filepath.Join(externalRoot, "dolt-state.json") + ">",
	}, "\n")
	if got != want {
		t.Fatalf("order exec env:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func TestOrderRunExecPropagatesManagedDoltLayout(t *testing.T) {
	cityDir := t.TempDir()
	dataDir := filepath.Join(t.TempDir(), "managed-dolt")
	configFile := filepath.Join(cityDir, ".gc", "runtime", "packs", "dolt", "dolt-config.yaml")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityDir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(cityDir, "city.toml"), `[workspace]
name = "test-city"
prefix = "ct"
`)
	writeFile(t, filepath.Join(cityDir, ".beads", "config.yaml"), strings.Join([]string{
		"issue_prefix: ct",
		"gc.endpoint_origin: managed_city",
		"gc.endpoint_status: verified",
		"dolt.auto-start: false",
		"",
	}, "\n"))
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer func() {
		if err := listener.Close(); err != nil {
			t.Fatalf("Close listener: %v", err)
		}
	}()
	port := fmt.Sprint(listener.Addr().(*net.TCPAddr).Port)
	stateDir := filepath.Join(cityDir, ".gc", "runtime", "packs", "dolt")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(stateDir, "dolt-state.json"), fmt.Sprintf(
		`{"running":true,"pid":%d,"port":%s,"data_dir":%q}`,
		os.Getpid(),
		port,
		dataDir,
	))
	cfg, err := loadCityConfig(cityDir)
	if err != nil {
		t.Fatalf("loadCityConfig: %v", err)
	}

	t.Setenv("GC_DOLT_DATA_DIR", filepath.Join(t.TempDir(), "poison-dolt-data"))
	t.Setenv("GC_DOLT_CONFIG_FILE", filepath.Join(t.TempDir(), "poison-dolt-config.yaml"))
	t.Setenv("GC_PACK_STATE_DIR", filepath.Join(t.TempDir(), "poison-pack-state"))
	t.Setenv("GC_DOLT_STATE_FILE", filepath.Join(t.TempDir(), "poison-state.json"))
	outPath := filepath.Join(cityDir, "exec-managed-layout.txt")
	a := orders.Order{
		Name:     "dolt-gc-nudge",
		Trigger:  "cooldown",
		Interval: "1m",
		Exec:     fmt.Sprintf(`printf 'managed=<%%s>\nport=<%%s>\ndata=<%%s>\nconfig=<%%s>\n' "$GC_DOLT_MANAGED_LOCAL" "$GC_DOLT_PORT" "$GC_DOLT_DATA_DIR" "$GC_DOLT_CONFIG_FILE" > %q`, outPath),
	}

	var stdout, stderr bytes.Buffer
	code := doOrderRunExec(a, cityDir, cfg, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doOrderRunExec = %d, want 0; stderr: %s", code, stderr.String())
	}
	got := strings.TrimSpace(readFileString(t, outPath))
	want := strings.Join([]string{
		"managed=<1>",
		"port=<" + port + ">",
		"data=<" + dataDir + ">",
		"config=<" + configFile + ">",
	}, "\n")
	if got != want {
		t.Fatalf("order exec env:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func TestOrderRunExecHonorsOrdersMaxTimeout(t *testing.T) {
	cityDir := t.TempDir()
	cfg := &config.City{
		Orders: config.OrdersConfig{MaxTimeout: "50ms"},
	}
	a := orders.Order{
		Name:    "slow-exec",
		Trigger: "manual",
		Exec:    "while :; do :; done",
		Timeout: "5s",
	}

	var stdout, stderr bytes.Buffer
	start := time.Now()
	code := doOrderRunExec(a, cityDir, cfg, &stdout, &stderr)
	elapsed := time.Since(start)
	if code == 0 {
		t.Fatalf("doOrderRunExec = 0, want timeout failure; stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	if elapsed > time.Second {
		t.Fatalf("doOrderRunExec elapsed = %s, want capped below 1s", elapsed)
	}
	if !strings.Contains(stderr.String(), "exec failed") {
		t.Fatalf("stderr = %q, want exec failure", stderr.String())
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

func TestOrderHistoryWithStoreResolverUsesRigStore(t *testing.T) {
	cityStore := beads.NewMemStore()
	rigStore := beads.NewMemStore()
	run, err := rigStore.Create(beads.Bead{
		Title:  "rig digest",
		Labels: []string{"order-run:digest:rig:frontend"},
	})
	if err != nil {
		t.Fatal(err)
	}

	aa := []orders.Order{{
		Name:    "digest",
		Rig:     "frontend",
		Formula: "mol-digest",
	}}
	resolver := func(a orders.Order) (beads.Store, error) {
		if a.Rig == "frontend" {
			return rigStore, nil
		}
		return cityStore, nil
	}

	var stdout, stderr bytes.Buffer
	code := doOrderHistoryWithStoreResolver("", "", aa, resolver, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doOrderHistoryWithStoreResolver = %d, want 0; stderr: %s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, run.ID) {
		t.Fatalf("stdout missing rig run %q:\n%s", run.ID, out)
	}
	if !strings.Contains(out, "frontend") {
		t.Fatalf("stdout missing rig name:\n%s", out)
	}
}

func TestOrderHistoryWithStoresResolverSkipsUnreadableLegacyStore(t *testing.T) {
	rigStore := beads.NewMemStore()
	legacyStore := labelFailListStore{
		Store:     beads.NewMemStore(),
		failLabel: "order-run:digest:rig:frontend",
	}
	run, err := rigStore.Create(beads.Bead{
		Title:  "rig digest",
		Labels: []string{"order-run:digest:rig:frontend"},
	})
	if err != nil {
		t.Fatal(err)
	}

	aa := []orders.Order{{
		Name:    "digest",
		Rig:     "frontend",
		Formula: "mol-digest",
	}}
	resolver := func(a orders.Order) ([]beads.Store, error) {
		if a.Rig == "frontend" {
			return []beads.Store{rigStore, legacyStore}, nil
		}
		return []beads.Store{rigStore}, nil
	}

	var stdout, stderr bytes.Buffer
	code := doOrderHistoryWithStoresResolver("", "", aa, resolver, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doOrderHistoryWithStoresResolver = %d, want 0 for partial history; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), run.ID) {
		t.Fatalf("stdout missing primary rig history %q:\n%s", run.ID, stdout.String())
	}
	if !strings.Contains(stderr.String(), "list failed") {
		t.Fatalf("stderr missing legacy list warning:\n%s", stderr.String())
	}
}

func TestOrderHistoryWithStoresResolverFailsUnreadablePrimaryStore(t *testing.T) {
	rigStore := labelFailListStore{
		Store:     beads.NewMemStore(),
		failLabel: "order-run:digest:rig:frontend",
	}
	legacyStore := beads.NewMemStore()
	if _, err := legacyStore.Create(beads.Bead{
		Title:  "legacy digest",
		Labels: []string{"order-run:digest:rig:frontend"},
	}); err != nil {
		t.Fatal(err)
	}

	aa := []orders.Order{{
		Name:    "digest",
		Rig:     "frontend",
		Formula: "mol-digest",
	}}
	resolver := func(a orders.Order) ([]beads.Store, error) {
		if a.Rig == "frontend" {
			return []beads.Store{rigStore, legacyStore}, nil
		}
		return []beads.Store{legacyStore}, nil
	}

	var stdout, stderr bytes.Buffer
	code := doOrderHistoryWithStoresResolver("", "", aa, resolver, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("doOrderHistoryWithStoresResolver = %d, want 1 for unreadable primary history; stdout: %s", code, stdout.String())
	}
	if !strings.Contains(stderr.String(), "list failed") {
		t.Fatalf("stderr missing primary list error:\n%s", stderr.String())
	}
}

func TestOrderHistoryWithStoresResolverDeduplicatesSameBackingStore(t *testing.T) {
	store := beads.NewMemStore()
	run, err := store.Create(beads.Bead{
		Title:  "rig digest",
		Labels: []string{"order-run:digest:rig:frontend"},
	})
	if err != nil {
		t.Fatal(err)
	}

	aa := []orders.Order{{
		Name:    "digest",
		Rig:     "frontend",
		Formula: "mol-digest",
	}}
	resolver := func(orders.Order) ([]beads.Store, error) {
		return []beads.Store{store, store}, nil
	}

	var stdout, stderr bytes.Buffer
	code := doOrderHistoryWithStoresResolver("", "", aa, resolver, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doOrderHistoryWithStoresResolver = %d, want 0; stderr: %s", code, stderr.String())
	}
	if count := strings.Count(stdout.String(), run.ID); count != 1 {
		t.Fatalf("stdout contains history row %q %d times, want 1:\n%s", run.ID, count, stdout.String())
	}
}

func TestOrderHistoryWithStoresResolverSortsMergedStoresByRecency(t *testing.T) {
	rigStore := beads.NewMemStore()
	legacyStore := beads.NewMemStore()
	if _, err := legacyStore.Create(beads.Bead{
		Title:  "unrelated",
		Labels: []string{"order-run:other:rig:frontend"},
	}); err != nil {
		t.Fatal(err)
	}
	oldRun, err := rigStore.Create(beads.Bead{
		Title:  "old rig digest",
		Labels: []string{"order-run:digest:rig:frontend"},
	})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Millisecond)
	newRun, err := legacyStore.Create(beads.Bead{
		Title:  "new legacy digest",
		Labels: []string{"order-run:digest:rig:frontend"},
	})
	if err != nil {
		t.Fatal(err)
	}

	aa := []orders.Order{{
		Name:    "digest",
		Rig:     "frontend",
		Formula: "mol-digest",
	}}
	resolver := func(orders.Order) ([]beads.Store, error) {
		return []beads.Store{rigStore, legacyStore}, nil
	}

	var stdout, stderr bytes.Buffer
	code := doOrderHistoryWithStoresResolver("", "", aa, resolver, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doOrderHistoryWithStoresResolver = %d, want 0; stderr: %s", code, stderr.String())
	}
	out := stdout.String()
	newIndex := strings.Index(out, newRun.ID)
	oldIndex := strings.LastIndex(out, oldRun.ID)
	if newIndex == -1 || oldIndex == -1 {
		t.Fatalf("stdout missing history rows old=%q new=%q:\n%s", oldRun.ID, newRun.ID, out)
	}
	if newIndex > oldIndex {
		t.Fatalf("newer legacy row appears after older primary row:\n%s", out)
	}
}

// --- rig-scoped tests ---

func TestOrderListWithRig(t *testing.T) {
	aa := []orders.Order{
		{Name: "digest", Trigger: "cooldown", Interval: "24h", Pool: "dog", Formula: "mol-digest"},
		{Name: "db-health", Trigger: "cooldown", Interval: "5m", Pool: "polecat", Formula: "mol-db-health", Rig: "demo-repo"},
	}

	var stdout bytes.Buffer
	code := doOrderList(aa, &stdout)
	if code != 0 {
		t.Fatalf("doOrderList = %d, want 0", code)
	}
	out := stdout.String()
	header := strings.SplitN(strings.TrimSpace(out), "\n", 2)[0]
	// RIG column should appear because at least one order has a rig.
	if !strings.Contains(header, " RIG") {
		t.Errorf("stdout missing 'RIG' column:\n%s", out)
	}
	if !strings.Contains(out, "demo-repo") {
		t.Errorf("stdout missing 'demo-repo':\n%s", out)
	}
}

func TestOrderListCityOnly(t *testing.T) {
	aa := []orders.Order{
		{Name: "digest", Trigger: "cooldown", Interval: "24h", Pool: "dog", Formula: "mol-digest"},
	}

	var stdout bytes.Buffer
	code := doOrderList(aa, &stdout)
	if code != 0 {
		t.Fatalf("doOrderList = %d, want 0", code)
	}
	out := stdout.String()
	header := strings.SplitN(strings.TrimSpace(out), "\n", 2)[0]
	// No RIG column when all orders are city-level.
	if strings.Contains(header, " RIG") {
		t.Errorf("stdout should not have 'RIG' column for city-only:\n%s", out)
	}
}

func TestFindOrderRigScoped(t *testing.T) {
	aa := []orders.Order{
		{Name: "dolt-health", Trigger: "cooldown", Interval: "1h", Formula: "mol-dh"},
		{Name: "dolt-health", Trigger: "cooldown", Interval: "5m", Formula: "mol-dh", Rig: "repo-a"},
		{Name: "dolt-health", Trigger: "cooldown", Interval: "10m", Formula: "mol-dh", Rig: "repo-b"},
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
		{Name: "digest", Trigger: "cooldown", Interval: "24h", Formula: "mol-digest"},
		{Name: "db-health", Trigger: "cooldown", Interval: "5m", Formula: "mol-db-health", Rig: "demo-repo"},
	}
	now := time.Date(2026, 2, 27, 12, 0, 0, 0, time.UTC)
	neverRan := func(_ string) (time.Time, error) { return time.Time{}, nil }

	var stdout bytes.Buffer
	code := doOrderCheck(aa, now, neverRan, &stdout)
	if code != 0 {
		t.Fatalf("doOrderCheck = %d, want 0", code)
	}
	out := stdout.String()
	if !strings.Contains(out, " RIG") {
		t.Errorf("stdout missing 'RIG' column:\n%s", out)
	}
	if !strings.Contains(out, "demo-repo") {
		t.Errorf("stdout missing 'demo-repo':\n%s", out)
	}
}

func TestOrderShowWithRig(t *testing.T) {
	aa := []orders.Order{
		{Name: "db-health", Formula: "mol-db-health", Trigger: "cooldown", Interval: "5m", Rig: "demo-repo", Source: "/topo/orders/db-health/order.toml"},
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
		{Name: "db-health", Formula: "mol-db-health", Trigger: "cooldown", Interval: "5m", Pool: "polecat", Rig: "demo-repo", FormulaLayer: sharedTestFormulaDir},
	}

	store := beads.NewMemStore()

	var stdout, stderr bytes.Buffer
	code := doOrderRun(aa, "db-health", "demo-repo", "/city", store, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doOrderRun = %d, want 0; stderr: %s", code, stderr.String())
	}

	results, err := store.ListByLabel("order-run:db-health:rig:demo-repo", 0)
	if err != nil {
		t.Fatalf("store.ListByLabel(): %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("store.ListByLabel() len = %d, want 1 (%#v)", len(results), results)
	}
	if got := results[0].Metadata["gc.routed_to"]; got != "demo-repo/polecat" {
		t.Fatalf("gc.routed_to = %q, want demo-repo/polecat", got)
	}
}

func TestOpenCityOrderStoreUsesProviderAwareStore(t *testing.T) {
	resetFlags(t)
	t.Setenv("GC_BEADS", "file")

	cityDir := setupCity(t, "orders-city")
	store, err := openScopeLocalFileStore(cityDir)
	if err != nil {
		t.Fatalf("openScopeLocalFileStore(city): %v", err)
	}
	created, err := store.Create(beads.Bead{Title: "digest run", Labels: []string{"order-run:digest"}})
	if err != nil {
		t.Fatalf("store.Create(): %v", err)
	}

	setCwd(t, cityDir)
	var stderr bytes.Buffer
	resolved, code := openCityOrderStore(&stderr, "gc order history")
	if code != 0 {
		t.Fatalf("openCityOrderStore() = %d, stderr = %s", code, stderr.String())
	}
	results, err := resolved.ListByLabel("order-run:digest", 0)
	if err != nil {
		t.Fatalf("resolved.ListByLabel(): %v", err)
	}
	if len(results) != 1 || results[0].ID != created.ID {
		t.Fatalf("order history results = %#v, want bead %q", results, created.ID)
	}
}
