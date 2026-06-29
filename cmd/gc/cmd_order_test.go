package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/formulatest"
	"github.com/gastownhall/gascity/internal/molecule"
	"github.com/gastownhall/gascity/internal/orders"
)

func readFileString(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

type partialListStore struct {
	beads.Store
	rows []beads.Bead
	err  error
}

func (s *partialListStore) List(_ beads.ListQuery) ([]beads.Bead, error) {
	return s.rows, s.err
}

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

func TestOrderListJSON(t *testing.T) {
	aa := []orders.Order{
		{Name: "digest", Trigger: "cooldown", Interval: "24h", Pool: "dog", Formula: "mol-digest", Source: "/city/orders/digest.toml"},
		{Name: "poll", Trigger: "condition", Check: "bd ready --json", Exec: "scripts/poll.sh", Rig: "frontend", Env: map[string]string{"GC_JSONL_MIN_PREV_FOR_SPIKE": "250"}},
	}

	var stdout bytes.Buffer
	code := doOrderListJSON("/city", &config.City{Workspace: config.Workspace{Name: "bright-lights"}}, aa, &stdout)
	if code != 0 {
		t.Fatalf("doOrderListJSON = %d, want 0", code)
	}

	var got struct {
		SchemaVersion string `json:"schema_version"`
		CityName      string `json:"city_name"`
		Orders        []struct {
			Name       string            `json:"name"`
			ScopedName string            `json:"scoped_name"`
			Type       string            `json:"type"`
			Target     string            `json:"target"`
			Enabled    bool              `json:"enabled"`
			Env        map[string]string `json:"env"`
		} `json:"orders"`
		Summary struct {
			Count int `json:"count"`
		} `json:"summary"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("order list JSON invalid: %v\n%s", err, stdout.String())
	}
	if got.SchemaVersion != "1" || got.CityName != "bright-lights" || got.Summary.Count != 2 {
		t.Fatalf("payload = %+v", got)
	}
	if got.Orders[0].Name != "digest" || got.Orders[0].Type != "formula" || got.Orders[0].Target != "dog" || !got.Orders[0].Enabled {
		t.Fatalf("first order = %+v", got.Orders[0])
	}
	if got.Orders[1].ScopedName != "poll:rig:frontend" || got.Orders[1].Type != "exec" {
		t.Fatalf("second order = %+v", got.Orders[1])
	}
	if got.Orders[1].Env["GC_JSONL_MIN_PREV_FOR_SPIKE"] != "250" {
		t.Fatalf("second order env = %+v, want GC_JSONL_MIN_PREV_FOR_SPIKE=250", got.Orders[1].Env)
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

func TestOrderShowJSON(t *testing.T) {
	aa := []orders.Order{{
		Name:         "digest",
		Description:  "nightly digest",
		Formula:      "mol-digest",
		Trigger:      "cron",
		Schedule:     "0 3 * * *",
		Pool:         "dog",
		Source:       "/city/orders/digest.toml",
		FormulaLayer: "/city/formulas",
	}}

	var stdout, stderr bytes.Buffer
	code := doOrderShowJSON("/city", &config.City{Workspace: config.Workspace{Name: "bright-lights"}}, aa, "digest", "", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doOrderShowJSON = %d, want 0; stderr=%s", code, stderr.String())
	}

	var got struct {
		SchemaVersion string `json:"schema_version"`
		Order         struct {
			Name         string `json:"name"`
			Description  string `json:"description"`
			Formula      string `json:"formula"`
			Trigger      string `json:"trigger"`
			Schedule     string `json:"schedule"`
			Target       string `json:"target"`
			FormulaLayer string `json:"formula_layer"`
		} `json:"order"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("order show JSON invalid: %v\n%s", err, stdout.String())
	}
	if got.SchemaVersion != "1" || got.Order.Name != "digest" || got.Order.Target != "dog" || got.Order.FormulaLayer != "/city/formulas" {
		t.Fatalf("payload = %+v", got)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func TestOrderShowJSONIncludesEnv(t *testing.T) {
	aa := []orders.Order{{
		Name:    "poll",
		Exec:    "scripts/poll.sh",
		Trigger: "manual",
		Env: map[string]string{
			"GC_JSONL_MIN_PREV_FOR_SPIKE": "250",
			"CUSTOM_ORDER_FLAG":           "enabled",
		},
	}}

	var stdout, stderr bytes.Buffer
	code := doOrderShowJSON("/city", nil, aa, "poll", "", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doOrderShowJSON = %d, want 0; stderr=%s", code, stderr.String())
	}

	var got struct {
		Order struct {
			Env map[string]string `json:"env"`
		} `json:"order"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("order show JSON invalid: %v\n%s", err, stdout.String())
	}
	if got.Order.Env["GC_JSONL_MIN_PREV_FOR_SPIKE"] != "250" || got.Order.Env["CUSTOM_ORDER_FLAG"] != "enabled" {
		t.Fatalf("env = %+v, want configured order env", got.Order.Env)
	}
}

func TestOrderShowJSONMissingOrderKeepsHumanError(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := doOrderShowJSON("/city", nil, nil, "missing", "", &stdout, &stderr)
	if code == 0 {
		t.Fatal("doOrderShowJSON missing order = 0, want nonzero")
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty before global JSON failure wrapper", stdout.String())
	}
	if !strings.Contains(stderr.String(), `order "missing" not found`) {
		t.Fatalf("stderr = %q, want missing order diagnostic", stderr.String())
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
	if err := os.WriteFile(filepath.Join(cityDir, "orders", "health-check.toml"), []byte(`
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
	if aa[0].Source != filepath.Join(cityDir, "orders", "health-check.toml") {
		t.Fatalf("Source = %q, want %q", aa[0].Source, filepath.Join(cityDir, "orders", "health-check.toml"))
	}
}

func TestScanAllOrdersRemoteImportedFlatPackOrders(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GC_HOME", filepath.Join(home, ".gc"))

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
	if err := os.WriteFile(filepath.Join(repoDir, "orders", "health-check.toml"), []byte(`
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
	var imported *orders.Order
	for i := range aa {
		if aa[i].Name == "health-check" {
			imported = &aa[i]
			break
		}
	}
	if imported == nil {
		t.Fatalf("scanAllOrders() missing imported health-check order: %#v", aa)
	}
	if imported.Name != "health-check" {
		t.Fatalf("Name = %q, want %q", imported.Name, "health-check")
	}
	if imported.Source != filepath.Join(cacheDir, "orders", "health-check.toml") {
		t.Fatalf("Source = %q, want %q", imported.Source, filepath.Join(cacheDir, "orders", "health-check.toml"))
	}
}

func TestScanAllOrdersCityLegacyFormulaOrdersRejects(t *testing.T) {
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
	_, err := scanAllOrders(cityDir, cfg, &stderr, "gc order list")
	if err == nil {
		t.Fatal("scanAllOrders succeeded, want PackV1 order layout rejection")
	}
	if !strings.Contains(err.Error(), "unsupported PackV1 order path") {
		t.Fatalf("scanAllOrders error = %q, want PackV1 path rejection", err)
	}
	if !strings.Contains(err.Error(), "move to orders/health-check.toml") {
		t.Fatalf("scanAllOrders error = %q, want migration hint", err)
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
			Source:      "/city/orders/digest.toml",
		},
	}

	var stdout, stderr bytes.Buffer
	code := doOrderShow(aa, "digest", "", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doOrderShow = %d, want 0; stderr: %s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"digest", "Generate daily digest", "mol-digest", "cooldown", "24h", "dog", "digest.toml"} {
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
			Source:      "/city/orders/poll.toml",
			Env: map[string]string{
				"GC_JSONL_MIN_PREV_FOR_SPIKE": "250",
				"CUSTOM_ORDER_FLAG":           "enabled",
			},
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
	for _, want := range []string{"Env:", "CUSTOM_ORDER_FLAG=enabled", "GC_JSONL_MIN_PREV_FOR_SPIKE=250"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout missing %q:\n%s", want, out)
		}
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

func TestOrderCheckWithStoresResolverRejectsReservedOrderEnvKey(t *testing.T) {
	aa := []orders.Order{
		{
			Name:     "bad-env",
			Trigger:  "cooldown",
			Interval: "24h",
			Exec:     "scripts/bad-env.sh",
			Env:      map[string]string{"GC_CITY": "shadowed"},
		},
	}

	var stdout, stderr bytes.Buffer
	code := doOrderCheckWithStoresResolverScoped(
		t.TempDir(),
		&config.City{},
		aa,
		time.Date(2026, 2, 27, 12, 0, 0, 0, time.UTC),
		nil,
		func(orders.Order) ([]beads.OrdersStore, error) { return nil, nil },
		&stdout,
		&stderr,
	)
	if code != 1 {
		t.Fatalf("doOrderCheckWithStoresResolverScoped = %d, want 1; stdout: %s; stderr: %s", code, stdout.String(), stderr.String())
	}
	if got := stderr.String(); !strings.Contains(got, `controller-owned env key "GC_CITY"`) {
		t.Fatalf("stderr = %q, want reserved-key validation diagnostic", got)
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

func TestOrderCheckJSONNoneDuePreservesSemanticExit(t *testing.T) {
	aa := []orders.Order{
		{Name: "deploy", Trigger: "manual", Formula: "mol-deploy"},
	}
	now := time.Date(2026, 2, 27, 12, 0, 0, 0, time.UTC)
	neverRan := func(_ string) (time.Time, error) { return time.Time{}, nil }

	var stdout bytes.Buffer
	code := doOrderCheckJSON(aa, now, neverRan, true, &stdout, io.Discard)
	if code != 1 {
		t.Fatalf("doOrderCheckJSON = %d, want 1 (none due)", code)
	}
	got := stdout.String()
	for _, want := range []string{`"schema_version":"1"`, `"ok":true`, `"any_due":false`, `"orders_total":1`, `"due_total":0`, `"name":"deploy"`} {
		if !strings.Contains(got, want) {
			t.Fatalf("stdout = %q, missing %s", got, want)
		}
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
	resolver := func(a orders.Order) ([]beads.OrdersStore, error) {
		if a.Rig == "frontend" {
			return []beads.OrdersStore{{Store: rigStore}}, nil
		}
		return []beads.OrdersStore{{Store: cityStore}}, nil
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
	resolver := func(a orders.Order) ([]beads.OrdersStore, error) {
		if a.Rig == "frontend" {
			return []beads.OrdersStore{{Store: rigStore}, {Store: cityStore}}, nil
		}
		return []beads.OrdersStore{{Store: cityStore}}, nil
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
	resolver := func(orders.Order) ([]beads.OrdersStore, error) {
		return []beads.OrdersStore{{Store: beads.NewMemStore()}}, nil
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
	resolver := func(a orders.Order) ([]beads.OrdersStore, error) {
		if a.Rig == "frontend" {
			return []beads.OrdersStore{{Store: rigStore}, {Store: legacyStore}}, nil
		}
		return []beads.OrdersStore{{Store: rigStore}}, nil
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
	resolver := func(a orders.Order) ([]beads.OrdersStore, error) {
		if a.Rig == "frontend" {
			return []beads.OrdersStore{{Store: rigStore}, {Store: legacyStore}}, nil
		}
		return []beads.OrdersStore{{Store: rigStore}}, nil
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
	code := doOrderRun(aa, "digest", "", "/city", beads.OrdersStore{Store: store}, nil, &stdout, &stderr)
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

// orderRunGraphApplySpy is a graph-apply-capable store that records whether
// ApplyGraphPlan was invoked. Unlike graphApplySpyStore it creates real beads
// in the embedded MemStore so the manual order-run path can label the wisp root
// and record its tracking bead after instantiation. It implements
// beads.GraphApplyStore.
type orderRunGraphApplySpy struct {
	*beads.MemStore
	applied bool
}

func (s *orderRunGraphApplySpy) ApplyGraphPlan(_ context.Context, plan *beads.GraphApplyPlan) (*beads.GraphApplyResult, error) {
	s.applied = true
	ids := make(map[string]string, len(plan.Nodes))
	for _, node := range plan.Nodes {
		created, err := s.Create(beads.Bead{
			Title:    node.Title,
			Type:     node.Type,
			Labels:   node.Labels,
			Metadata: node.Metadata,
		})
		if err != nil {
			return nil, err
		}
		ids[node.Key] = created.ID
	}
	return &beads.GraphApplyResult{IDs: ids}, nil
}

// TestOrderRunUsesGraphApplyThroughOrdersStore pins the fix for the manual
// `gc order run` graph-apply regression: openOrderStoreForOrder hands
// doOrderRunWithJSON a beads.OrdersStore wrapper, and typed wrappers do not
// promote optional capabilities, so passing the wrapper straight into
// molecule.Instantiate hides the underlying GraphApplyStore and silently falls
// back to sequential creation. The path must unwrap to store.Store at the
// generic molecule/graph-routing boundary so graph apply is used when enabled,
// matching the controller dispatch path.
func TestOrderRunUsesGraphApplyThroughOrdersStore(t *testing.T) {
	aa := []orders.Order{
		{Name: "digest", Formula: "mol-digest", Trigger: "cooldown", Interval: "24h", Pool: "dog", FormulaLayer: sharedTestFormulaDir},
	}

	prevGraphApply := molecule.IsGraphApplyEnabled()
	molecule.SetGraphApplyEnabled(true)
	t.Cleanup(func() { molecule.SetGraphApplyEnabled(prevGraphApply) })

	spy := &orderRunGraphApplySpy{MemStore: beads.NewMemStore()}

	var stdout, stderr bytes.Buffer
	code := doOrderRun(aa, "digest", "", "/city", beads.OrdersStore{Store: spy}, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doOrderRun = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !spy.applied {
		t.Fatal("ApplyGraphPlan was not invoked — graph apply capability lost through the beads.OrdersStore wrapper")
	}
}

func TestOrderRunFormulaRecordsTrackingBead(t *testing.T) {
	aa := []orders.Order{
		{Name: "digest", Formula: "mol-digest", Trigger: "cooldown", Interval: "24h", Pool: "dog", FormulaLayer: sharedTestFormulaDir},
	}
	store := beads.NewMemStore()

	var stdout, stderr bytes.Buffer
	if code := doOrderRun(aa, "digest", "", "/city", beads.OrdersStore{Store: store}, nil, &stdout, &stderr); code != 0 {
		t.Fatalf("doOrderRun = %d, want 0; stderr: %s", code, stderr.String())
	}

	tracking, err := store.ListByLabel(labelOrderTracking, 0, beads.IncludeClosed, beads.WithBothTiers)
	if err != nil {
		t.Fatalf("store.ListByLabel(%s): %v", labelOrderTracking, err)
	}
	if len(tracking) != 1 {
		t.Fatalf("order-tracking beads = %d, want 1 (%#v)", len(tracking), tracking)
	}
	if !beadLabelsContain(tracking[0].Labels, "order-run:digest") {
		t.Fatalf("tracking bead labels = %v, want order-run:digest", tracking[0].Labels)
	}
	if tracking[0].Status != "closed" {
		t.Fatalf("tracking bead status = %q, want closed", tracking[0].Status)
	}
}

func TestOrderRunJSONFormulaSummary(t *testing.T) {
	aa := []orders.Order{
		{Name: "digest", Formula: "mol-digest", Trigger: "cooldown", Interval: "24h", Pool: "dog", FormulaLayer: sharedTestFormulaDir},
	}

	store := beads.NewMemStore()

	var stdout, stderr bytes.Buffer
	code := doOrderRunWithJSON(aa, "digest", "", "/city", beads.OrdersStore{Store: store}, nil, true, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doOrderRunWithJSON = %d, want 0; stderr: %s", code, stderr.String())
	}
	got := stdout.String()
	for _, want := range []string{`"schema_version":"1"`, `"ok":true`, `"order":"digest"`, `"scoped_name":"digest"`, `"action":"formula"`, `"wisp_id":`, `"routed_to":"dog"`} {
		if !strings.Contains(got, want) {
			t.Fatalf("stdout = %q, missing %s", got, want)
		}
	}
}

func TestOrderRunHonorsFormulaV2DisabledCity(t *testing.T) {
	t.Cleanup(func() {
		applyFeatureFlags(&config.City{Daemon: config.DaemonConfig{FormulaV2: boolPtr(true)}})
	})

	cityDir := t.TempDir()
	formulaDir := filepath.Join(cityDir, "formulas")
	if err := os.MkdirAll(formulaDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := `[workspace]
name = "test-city"

[daemon]
formula_v2 = false
`
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}
	graphFormula := `
formula = "graph-work"

[requires]
formula_compiler = ">=2.0.0"

[[steps]]
id = "prepare"
title = "Prepare workflow"

[[steps]]
id = "step"
title = "Do work"
depends_on = ["prepare"]
`
	if err := os.WriteFile(filepath.Join(formulaDir, "graph-work.toml"), []byte(graphFormula), 0o644); err != nil {
		t.Fatal(err)
	}

	aa := []orders.Order{
		{Name: "blocked", Formula: "graph-work", Trigger: "cooldown", Interval: "15m", FormulaLayer: formulaDir},
	}
	store := beads.NewMemStore()

	var stdout, stderr bytes.Buffer
	code := doOrderRun(aa, "blocked", "", cityDir, beads.OrdersStore{Store: store}, nil, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("doOrderRun = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "formula_v2 is disabled") {
		t.Fatalf("stderr missing formula_v2 diagnostic:\n%s", stderr.String())
	}
	results, err := store.ListByLabel("order-run:blocked", 0)
	if err != nil {
		t.Fatalf("store.ListByLabel(): %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("created %d order-run bead(s), want none: %#v", len(results), results)
	}
}

func TestOrderRunJSONRejectsExecWithoutRunning(t *testing.T) {
	aa := []orders.Order{
		{Name: "release-exec", Trigger: "manual", Exec: "printf unsafe"},
	}

	var stdout, stderr bytes.Buffer
	code := doOrderRunWithJSON(aa, "release-exec", "", "/city", beads.OrdersStore{Store: beads.NewMemStore()}, nil, true, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("doOrderRunWithJSON exec = %d, want 1", code)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty stdout on unsupported exec JSON", stdout.String())
	}
	if !strings.Contains(stderr.String(), "--json is not supported for exec orders") {
		t.Fatalf("stderr = %q, want unsupported exec message", stderr.String())
	}
}

func TestOrderRunEventExecAdvancesCursor(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_BEADS_SCOPE_ROOT", "")

	cityDir := t.TempDir()
	writeFile(t, filepath.Join(cityDir, "city.toml"), `[workspace]
name = "test-city"
`)
	store := beads.NewMemStore()
	eventLog := events.NewFake()
	eventLog.Record(events.Event{Type: events.BeadClosed, Actor: "test"})
	headSeq, err := eventLog.LatestSeq()
	if err != nil {
		t.Fatalf("LatestSeq(): %v", err)
	}
	aa := []orders.Order{{
		Name:    "release-exec",
		Trigger: "event",
		On:      events.BeadClosed,
		Exec:    "printf ok",
	}}

	var stdout, stderr bytes.Buffer
	code := doOrderRun(aa, "release-exec", "", cityDir, beads.OrdersStore{Store: store}, eventLog, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doOrderRun = %d, want 0; stderr: %s", code, stderr.String())
	}

	results, err := store.ListByLabel("order-run:release-exec", 0, beads.IncludeClosed, beads.WithBothTiers)
	if err != nil {
		t.Fatalf("store.ListByLabel(): %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("store.ListByLabel() len = %d, want 1 (%#v)", len(results), results)
	}
	if results[0].Ephemeral || !results[0].NoHistory {
		t.Fatalf("tracking bead storage = Ephemeral:%v NoHistory:%v, want no-history only", results[0].Ephemeral, results[0].NoHistory)
	}
	for _, want := range []string{"order:release-exec", fmt.Sprintf("seq:%d", headSeq), "exec"} {
		if !slicesContain(results[0].Labels, want) {
			t.Fatalf("tracking bead labels = %v, want %s", results[0].Labels, want)
		}
	}
}

func TestCmdOrderRunEventExecAdvancesCursor(t *testing.T) {
	cityDir := t.TempDir()
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_BEADS_SCOPE_ROOT", "")
	t.Setenv("GC_EVENTS", "")
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_CITY_PATH", cityDir)
	t.Setenv("GC_CITY_ROOT", cityDir)
	t.Setenv("GC_RIG", "")
	t.Setenv("GC_RIG_ROOT", "")
	t.Chdir(cityDir)

	writeFile(t, filepath.Join(cityDir, "city.toml"), `[workspace]
name = "test-city"
`)
	if err := os.MkdirAll(filepath.Join(cityDir, "orders"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(cityDir, "orders", "release-exec.toml"), `[order]
exec = "printf ok"
trigger = "event"
on = "bead.closed"
`)
	var eventStderr bytes.Buffer
	eventLog, err := events.NewFileRecorder(filepath.Join(cityDir, ".gc", "events.jsonl"), &eventStderr)
	if err != nil {
		t.Fatalf("NewFileRecorder(): %v", err)
	}
	eventLog.Record(events.Event{Type: events.BeadClosed, Actor: "test"})
	headSeq, err := eventLog.LatestSeq()
	if err != nil {
		t.Fatalf("LatestSeq(): %v", err)
	}
	if err := eventLog.Close(); err != nil {
		t.Fatalf("Close(): %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := cmdOrderRun("release-exec", "", false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cmdOrderRun = %d, want 0; stderr: %s", code, stderr.String())
	}

	store, err := openStoreAtForCity(cityDir, cityDir)
	if err != nil {
		t.Fatalf("openStoreAtForCity(): %v", err)
	}
	results, err := store.ListByLabel("order-run:release-exec", 0, beads.IncludeClosed, beads.WithBothTiers)
	if err != nil {
		t.Fatalf("store.ListByLabel(): %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("store.ListByLabel() len = %d, want 1 (%#v)", len(results), results)
	}
	if results[0].Ephemeral || !results[0].NoHistory {
		t.Fatalf("tracking bead storage = Ephemeral:%v NoHistory:%v, want no-history only", results[0].Ephemeral, results[0].NoHistory)
	}
	for _, want := range []string{"order:release-exec", fmt.Sprintf("seq:%d", headSeq), "exec"} {
		if !slicesContain(results[0].Labels, want) {
			t.Fatalf("tracking bead labels = %v, want %s", results[0].Labels, want)
		}
	}
}

func TestCmdOrderSweepTrackingClosesRigScopedTrackingAfterReopen(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_BEADS_SCOPE_ROOT", "")

	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "frontend")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_CITY_PATH", cityDir)
	t.Setenv("GC_CITY_ROOT", cityDir)
	t.Setenv("GC_RIG", "")
	t.Setenv("GC_RIG_ROOT", "")
	t.Chdir(cityDir)

	writeFile(t, filepath.Join(cityDir, "city.toml"), `[workspace]
name = "test-city"
prefix = "ct"

[[rigs]]
name = "frontend"
path = "frontend"
prefix = "fe"
`)
	if err := ensureScopedFileStoreLayout(cityDir); err != nil {
		t.Fatal(err)
	}
	if err := ensurePersistedScopeLocalFileStore(cityDir); err != nil {
		t.Fatal(err)
	}
	if err := ensurePersistedScopeLocalFileStore(rigDir); err != nil {
		t.Fatal(err)
	}
	rigStore, err := openStoreAtForCity(rigDir, cityDir)
	if err != nil {
		t.Fatalf("openStoreAtForCity(rig): %v", err)
	}
	stale, err := rigStore.Create(beads.Bead{
		Title:     "order:rig-digest:rig:frontend",
		Labels:    []string{"order-run:rig-digest:rig:frontend", labelOrderTracking},
		Ephemeral: true,
	})
	if err != nil {
		t.Fatalf("Create(stale): %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := cmdOrderSweepTrackingWithOptions(time.Nanosecond, false, false, false, []string{"rig-digest:rig:frontend"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cmdOrderSweepTracking = %d, want 0; stderr: %s", code, stderr.String())
	}

	reopened, err := openStoreAtForCity(rigDir, cityDir)
	if err != nil {
		t.Fatalf("openStoreAtForCity(rig reopen): %v", err)
	}
	got, err := reopened.Get(stale.ID)
	if err != nil {
		t.Fatalf("Get(stale): %v", err)
	}
	if got.Status != "closed" {
		t.Fatalf("stale rig tracking status after reopen = %q, want closed", got.Status)
	}
	if !strings.Contains(stdout.String(), "closed 1 stale order-tracking bead") {
		t.Fatalf("stdout = %q, want one closed tracking bead", stdout.String())
	}
}

func TestCmdOrderSweepTrackingClosesCityTrackingWhenRigStoreOpenFails(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_BEADS_SCOPE_ROOT", "")

	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "frontend")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_CITY_PATH", cityDir)
	t.Setenv("GC_CITY_ROOT", cityDir)
	t.Setenv("GC_RIG", "")
	t.Setenv("GC_RIG_ROOT", "")
	t.Chdir(cityDir)

	writeFile(t, filepath.Join(cityDir, "city.toml"), `[workspace]
name = "test-city"
prefix = "ct"

[[rigs]]
name = "frontend"
path = "frontend"
prefix = "fe"
`)
	if err := ensureScopedFileStoreLayout(cityDir); err != nil {
		t.Fatal(err)
	}
	if err := ensurePersistedScopeLocalFileStore(cityDir); err != nil {
		t.Fatal(err)
	}
	store, err := openStoreAtForCity(cityDir, cityDir)
	if err != nil {
		t.Fatalf("openStoreAtForCity(city): %v", err)
	}
	stale, err := store.Create(beads.Bead{
		Title:     "order:cleanup",
		Labels:    []string{"order-run:cleanup", labelOrderTracking},
		Ephemeral: true,
	})
	if err != nil {
		t.Fatalf("Create(stale): %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := cmdOrderSweepTrackingWithOptions(time.Nanosecond, false, false, false, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cmdOrderSweepTracking = %d, want 0; stderr: %s", code, stderr.String())
	}

	reopened, err := openStoreAtForCity(cityDir, cityDir)
	if err != nil {
		t.Fatalf("openStoreAtForCity(city reopen): %v", err)
	}
	got, err := reopened.Get(stale.ID)
	if err != nil {
		t.Fatalf("Get(stale): %v", err)
	}
	if got.Status != "closed" {
		t.Fatalf("city stale tracking status after reopen = %q, want closed", got.Status)
	}
	if !strings.Contains(stderr.String(), "opening rig \"frontend\" order store") {
		t.Fatalf("stderr = %q, want rig-open warning", stderr.String())
	}
	if !strings.Contains(stdout.String(), "closed 1 stale order-tracking bead") {
		t.Fatalf("stdout = %q, want one closed tracking bead", stdout.String())
	}
}

func TestSweepOrderTrackingCommandClosesAllStaleTracking(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_BEADS_SCOPE_ROOT", "")

	cityDir := t.TempDir()
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_CITY_PATH", cityDir)
	t.Setenv("GC_CITY_ROOT", cityDir)
	t.Setenv("GC_RIG", "")
	t.Setenv("GC_RIG_ROOT", "")
	t.Chdir(cityDir)

	writeFile(t, filepath.Join(cityDir, "city.toml"), `[workspace]
name = "test-city"
prefix = "ct"
`)
	if err := ensureScopedFileStoreLayout(cityDir); err != nil {
		t.Fatal(err)
	}
	if err := ensurePersistedScopeLocalFileStore(cityDir); err != nil {
		t.Fatal(err)
	}
	store, err := openStoreAtForCity(cityDir, cityDir)
	if err != nil {
		t.Fatalf("openStoreAtForCity(city): %v", err)
	}
	ids := make([]string, 0, orderTrackingSweepCloseBudget+1)
	for i := range orderTrackingSweepCloseBudget + 1 {
		stale, err := store.Create(beads.Bead{
			Title:     fmt.Sprintf("order:cleanup-%d", i),
			Labels:    []string{fmt.Sprintf("order-run:cleanup-%d", i), labelOrderTracking},
			Ephemeral: true,
		})
		if err != nil {
			t.Fatalf("Create(stale-%d): %v", i, err)
		}
		ids = append(ids, stale.ID)
	}

	var stdout, stderr bytes.Buffer
	code := cmdOrderSweepTrackingWithOptions(time.Nanosecond, false, false, false, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cmdOrderSweepTracking = %d, want 0; stderr: %s", code, stderr.String())
	}

	reopened, err := openStoreAtForCity(cityDir, cityDir)
	if err != nil {
		t.Fatalf("openStoreAtForCity(city reopen): %v", err)
	}
	closed := 0
	for _, id := range ids {
		got, err := reopened.Get(id)
		if err != nil {
			t.Fatalf("Get(%s): %v", id, err)
		}
		if got.Status == "closed" {
			closed++
		}
	}
	if closed != len(ids) {
		t.Fatalf("closed = %d, want %d", closed, len(ids))
	}
	want := fmt.Sprintf("closed %d stale order-tracking bead", len(ids))
	if !strings.Contains(stdout.String(), want) {
		t.Fatalf("stdout = %q, want %q", stdout.String(), want)
	}
}

func TestSweepOrderTrackingCommandPrunesClosedTrackingWithConfiguredPolicy(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_BEADS_SCOPE_ROOT", "")

	cityDir := t.TempDir()
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_CITY_PATH", cityDir)
	t.Setenv("GC_CITY_ROOT", cityDir)
	t.Setenv("GC_RIG", "")
	t.Setenv("GC_RIG_ROOT", "")
	t.Chdir(cityDir)

	writeFile(t, filepath.Join(cityDir, "city.toml"), `[workspace]
name = "test-city"
prefix = "ct"

[beads.policies.order_tracking]
delete_after_close = "1ns"
`)
	if err := ensureScopedFileStoreLayout(cityDir); err != nil {
		t.Fatal(err)
	}
	if err := ensurePersistedScopeLocalFileStore(cityDir); err != nil {
		t.Fatal(err)
	}
	store, err := openStoreAtForCity(cityDir, cityDir)
	if err != nil {
		t.Fatalf("openStoreAtForCity(city): %v", err)
	}
	ids := make([]string, 0, minClosedOrderTrackingRetained+2)
	for i := range minClosedOrderTrackingRetained + 2 {
		tracking, err := store.Create(beads.Bead{
			Title:     "order:cleanup",
			Labels:    []string{"order-run:cleanup", labelOrderTracking},
			Ephemeral: i%2 == 0,
		})
		if err != nil {
			t.Fatalf("Create(tracking-%d): %v", i, err)
		}
		if err := store.Close(tracking.ID); err != nil {
			t.Fatalf("Close(tracking-%d): %v", i, err)
		}
		ids = append(ids, tracking.ID)
	}

	var stdout, stderr bytes.Buffer
	code := cmdOrderSweepTrackingWithOptions(time.Hour, false, false, false, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cmdOrderSweepTracking = %d, want 0; stderr: %s", code, stderr.String())
	}

	reopened, err := openStoreAtForCity(cityDir, cityDir)
	if err != nil {
		t.Fatalf("openStoreAtForCity(city reopen): %v", err)
	}
	remaining := 0
	for _, id := range ids {
		if _, err := reopened.Get(id); err == nil {
			remaining++
		}
	}
	if remaining != minClosedOrderTrackingRetained {
		t.Fatalf("remaining closed tracking beads = %d, want %d", remaining, minClosedOrderTrackingRetained)
	}
	if !strings.Contains(stdout.String(), "deleted 2 closed order-tracking bead") {
		t.Fatalf("stdout = %q, want closed tracking delete count", stdout.String())
	}
}

func TestOrderTrackingSweepErrorIsFatalForRetentionAllStoreFailure(t *testing.T) {
	retentionErr := fmt.Errorf("retention failed")

	if !orderTrackingSweepErrorIsFatal(orderTrackingSweepResult{storesSwept: 1}, orderTrackingRetentionSweepResult{}, retentionErr) {
		t.Fatal("retention failure with no successful retention stores should be fatal")
	}
	if orderTrackingSweepErrorIsFatal(orderTrackingSweepResult{storesSwept: 1}, orderTrackingRetentionSweepResult{storesSwept: 1}, retentionErr) {
		t.Fatal("retention failure with at least one successful retention store should remain partial")
	}
	if !orderTrackingSweepErrorIsFatal(orderTrackingSweepResult{}, orderTrackingRetentionSweepResult{storesSwept: 1}, nil) {
		t.Fatal("stale sweep failure with no successful stale stores should be fatal")
	}
}

func TestSweepOrderTrackingCommandIncludeWispsRequiresOrderBeforePruning(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_BEADS_SCOPE_ROOT", "")

	cityDir := t.TempDir()
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_CITY_PATH", cityDir)
	t.Setenv("GC_CITY_ROOT", cityDir)
	t.Setenv("GC_RIG", "")
	t.Setenv("GC_RIG_ROOT", "")
	t.Chdir(cityDir)

	writeFile(t, filepath.Join(cityDir, "city.toml"), `[workspace]
name = "test-city"
prefix = "ct"

[beads.policies.order_tracking]
delete_after_close = "1ns"
`)
	if err := ensureScopedFileStoreLayout(cityDir); err != nil {
		t.Fatal(err)
	}
	if err := ensurePersistedScopeLocalFileStore(cityDir); err != nil {
		t.Fatal(err)
	}
	store, err := openStoreAtForCity(cityDir, cityDir)
	if err != nil {
		t.Fatalf("openStoreAtForCity(city): %v", err)
	}
	ids := make([]string, 0, minClosedOrderTrackingRetained+2)
	for i := range minClosedOrderTrackingRetained + 2 {
		tracking, err := store.Create(beads.Bead{
			Title:     "order:cleanup",
			Labels:    []string{"order-run:cleanup", labelOrderTracking},
			Ephemeral: true,
		})
		if err != nil {
			t.Fatalf("Create(tracking-%d): %v", i, err)
		}
		if err := store.Close(tracking.ID); err != nil {
			t.Fatalf("Close(tracking-%d): %v", i, err)
		}
		ids = append(ids, tracking.ID)
	}

	var stdout, stderr bytes.Buffer
	code := cmdOrderSweepTrackingWithOptions(time.Hour, true, false, false, nil, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("cmdOrderSweepTracking = 0, want failure")
	}
	if !strings.Contains(stderr.String(), "include-wisps requires at least one order name") {
		t.Fatalf("stderr = %q, want include-wisps error", stderr.String())
	}

	reopened, err := openStoreAtForCity(cityDir, cityDir)
	if err != nil {
		t.Fatalf("openStoreAtForCity(city reopen): %v", err)
	}
	for _, id := range ids {
		if _, err := reopened.Get(id); err != nil {
			t.Fatalf("%s should be preserved after invalid command: %v", id, err)
		}
	}
}

func TestCmdOrderSweepTrackingTargetedCityOrderSkipsUnrelatedRigStore(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_BEADS_SCOPE_ROOT", "")

	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "frontend")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_CITY_PATH", cityDir)
	t.Setenv("GC_CITY_ROOT", cityDir)
	t.Setenv("GC_RIG", "")
	t.Setenv("GC_RIG_ROOT", "")
	t.Chdir(cityDir)

	writeFile(t, filepath.Join(cityDir, "city.toml"), `[workspace]
name = "test-city"
prefix = "ct"

[[rigs]]
name = "frontend"
path = "frontend"
prefix = "fe"
`)
	if err := ensureScopedFileStoreLayout(cityDir); err != nil {
		t.Fatal(err)
	}
	if err := ensurePersistedScopeLocalFileStore(cityDir); err != nil {
		t.Fatal(err)
	}
	store, err := openStoreAtForCity(cityDir, cityDir)
	if err != nil {
		t.Fatalf("openStoreAtForCity(city): %v", err)
	}
	stale, err := store.Create(beads.Bead{
		Title:     "order:cleanup",
		Labels:    []string{"order-run:cleanup", labelOrderTracking},
		Ephemeral: true,
	})
	if err != nil {
		t.Fatalf("Create(stale): %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := cmdOrderSweepTrackingWithOptions(time.Nanosecond, false, false, false, []string{"cleanup"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cmdOrderSweepTracking = %d, want 0; stderr: %s", code, stderr.String())
	}

	reopened, err := openStoreAtForCity(cityDir, cityDir)
	if err != nil {
		t.Fatalf("openStoreAtForCity(city reopen): %v", err)
	}
	got, err := reopened.Get(stale.ID)
	if err != nil {
		t.Fatalf("Get(stale): %v", err)
	}
	if got.Status != "closed" {
		t.Fatalf("city stale tracking status after targeted sweep = %q, want closed", got.Status)
	}
	if strings.Contains(stderr.String(), "opening rig \"frontend\" order store") {
		t.Fatalf("stderr = %q, want targeted city sweep to skip unrelated rig store", stderr.String())
	}
	if !strings.Contains(stdout.String(), "closed 1 stale order-tracking bead") {
		t.Fatalf("stdout = %q, want one closed tracking bead", stdout.String())
	}
}

func TestCmdOrderSweepTrackingDryRunReportsWithoutClosing(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_BEADS_SCOPE_ROOT", "")

	cityDir := t.TempDir()
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_CITY_PATH", cityDir)
	t.Setenv("GC_CITY_ROOT", cityDir)
	t.Setenv("GC_RIG", "")
	t.Setenv("GC_RIG_ROOT", "")
	t.Chdir(cityDir)

	writeFile(t, filepath.Join(cityDir, "city.toml"), `[workspace]
name = "test-city"
prefix = "ct"
`)
	if err := ensureScopedFileStoreLayout(cityDir); err != nil {
		t.Fatal(err)
	}
	if err := ensurePersistedScopeLocalFileStore(cityDir); err != nil {
		t.Fatal(err)
	}
	store, err := openStoreAtForCity(cityDir, cityDir)
	if err != nil {
		t.Fatalf("openStoreAtForCity(city): %v", err)
	}
	stale, err := store.Create(beads.Bead{
		Title:     "order:cleanup",
		Labels:    []string{"order-run:cleanup", labelOrderTracking},
		Ephemeral: true,
	})
	if err != nil {
		t.Fatalf("Create(stale): %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := cmdOrderSweepTrackingWithOptions(time.Nanosecond, false, true, false, []string{"cleanup"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cmdOrderSweepTrackingWithOptions = %d, want 0; stderr: %s", code, stderr.String())
	}

	reopened, err := openStoreAtForCity(cityDir, cityDir)
	if err != nil {
		t.Fatalf("openStoreAtForCity(city reopen): %v", err)
	}
	got, err := reopened.Get(stale.ID)
	if err != nil {
		t.Fatalf("Get(stale): %v", err)
	}
	if got.Status != "open" {
		t.Fatalf("stale tracking status after dry-run = %q, want open", got.Status)
	}
	if !strings.Contains(stdout.String(), "would close 1 stale order-tracking bead") {
		t.Fatalf("stdout = %q, want dry-run tracking candidate", stdout.String())
	}
}

func TestCmdOrderSweepTrackingFailsWhenTargetedRigStoreOpenFails(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_BEADS_SCOPE_ROOT", "")

	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "frontend")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_CITY_PATH", cityDir)
	t.Setenv("GC_CITY_ROOT", cityDir)
	t.Setenv("GC_RIG", "")
	t.Setenv("GC_RIG_ROOT", "")
	t.Chdir(cityDir)

	writeFile(t, filepath.Join(cityDir, "city.toml"), `[workspace]
name = "test-city"
prefix = "ct"

[[rigs]]
name = "frontend"
path = "frontend"
prefix = "fe"
`)
	if err := ensureScopedFileStoreLayout(cityDir); err != nil {
		t.Fatal(err)
	}
	if err := ensurePersistedScopeLocalFileStore(cityDir); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := cmdOrderSweepTrackingWithOptions(time.Nanosecond, false, false, false, []string{"rig-digest:rig:frontend"}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("cmdOrderSweepTracking = 0, want failure; stdout: %s stderr: %s", stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "opening rig \"frontend\" order store") {
		t.Fatalf("stderr = %q, want rig-open warning", stderr.String())
	}
	if !strings.Contains(stderr.String(), "target store was not swept for rig-digest:rig:frontend") {
		t.Fatalf("stderr = %q, want targeted-store failure", stderr.String())
	}
}

func TestOrderRunEventFormulaLatestSeqErrorDoesNotInstantiate(t *testing.T) {
	aa := []orders.Order{{
		Name:         "release-watch",
		Trigger:      "event",
		On:           events.BeadClosed,
		Formula:      "test-formula",
		FormulaLayer: sharedTestFormulaDir,
	}}
	store := beads.NewMemStore()

	var stdout, stderr bytes.Buffer
	code := doOrderRun(aa, "release-watch", "", "/city", beads.OrdersStore{Store: store}, events.NewFailFake(), &stdout, &stderr)
	if code != 1 {
		t.Fatalf("doOrderRun = %d, want 1 when event cursor cannot be read; stdout: %s", code, stdout.String())
	}
	results, err := store.ListByLabel("order-run:release-watch", 0, beads.IncludeClosed)
	if err != nil {
		t.Fatalf("store.ListByLabel(): %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("store.ListByLabel() len = %d, want 0 (%#v)", len(results), results)
	}
	if !strings.Contains(stderr.String(), "reading event cursor for release-watch") {
		t.Fatalf("stderr = %q, want event cursor read failure", stderr.String())
	}
}

func TestOrderRunResolvesPackBindingForPool(t *testing.T) {
	cityDir := t.TempDir()
	writeOrderRunImportFixture(t, cityDir, "maintenance")
	formulaLayer := filepath.Join(cityDir, "packs", "maintenance", "formulas")
	if err := os.MkdirAll(formulaLayer, 0o755); err != nil {
		t.Fatal(err)
	}
	formulaText, err := os.ReadFile(filepath.Join(sharedTestFormulaDir, "mol-digest.toml"))
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(formulaLayer, "mol-digest.toml"), string(formulaText))

	aa := []orders.Order{
		{Name: "digest", Formula: "mol-digest", Trigger: "cooldown", Interval: "24h", Pool: "dog", FormulaLayer: formulaLayer},
	}
	store := beads.NewMemStore()

	var stdout, stderr bytes.Buffer
	code := doOrderRun(aa, "digest", "", cityDir, beads.OrdersStore{Store: store}, nil, &stdout, &stderr)
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
	assertNoDeprecatedPoolDemandMetadata(t, results[0].Metadata)
	if !strings.Contains(stdout.String(), "gc.routed_to=maintenance.dog") {
		t.Fatalf("stdout = %q, want binding-qualified route", stdout.String())
	}
}

func TestOrderRunDogVaporFormulaCreatesSelfInstructingReadyWisp(t *testing.T) {
	formulaDir := t.TempDir()
	writeFile(t, filepath.Join(formulaDir, "mol-dog-cleanup.toml"), `
description = """
Dog cleanup recipe.

After claiming this vapor wisp, run:

gc bd formula show mol-dog-cleanup --json

Follow the step descriptions in order. When finished, close this bead, then run:

gc runtime drain-ack

The drain-ack tells the controller this session is done.
"""
formula = "mol-dog-cleanup"
version = 1
phase = "vapor"

[[steps]]
id = "work"
title = "Do dog cleanup"
description = "Do the cleanup."
`)

	aa := []orders.Order{
		{Name: "dog-cleanup", Formula: "mol-dog-cleanup", Trigger: "cron", Schedule: "0 3 * * *", Pool: "dog", FormulaLayer: formulaDir},
	}
	store := beads.NewMemStore()

	var stdout, stderr bytes.Buffer
	code := doOrderRun(aa, "dog-cleanup", "", "/city", beads.OrdersStore{Store: store}, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doOrderRun = %d, want 0; stderr: %s", code, stderr.String())
	}

	results, err := store.ListByLabel("order-run:dog-cleanup", 0)
	if err != nil {
		t.Fatalf("store.ListByLabel(): %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("store.ListByLabel() len = %d, want 1 (%#v)", len(results), results)
	}
	wisp := results[0]
	if wisp.Type != "task" {
		t.Fatalf("wisp Type = %q, want task", wisp.Type)
	}
	if wisp.Ref != "mol-dog-cleanup" {
		t.Fatalf("wisp Ref = %q, want mol-dog-cleanup", wisp.Ref)
	}
	if got := wisp.Metadata["gc.kind"]; got != "wisp" {
		t.Fatalf("gc.kind = %q, want wisp", got)
	}
	if got := wisp.Metadata["gc.routed_to"]; got != "dog" {
		t.Fatalf("gc.routed_to = %q, want dog", got)
	}
	assertNoDeprecatedPoolDemandMetadata(t, wisp.Metadata)
	if !strings.Contains(wisp.Description, "Dog cleanup recipe.") {
		t.Fatalf("wisp description missing formula description:\n%s", wisp.Description)
	}
	if !strings.Contains(wisp.Description, "gc bd formula show mol-dog-cleanup --json") {
		t.Fatalf("wisp description missing self-instruction:\n%s", wisp.Description)
	}
	if !strings.Contains(wisp.Description, "gc runtime drain-ack") {
		t.Fatalf("wisp description missing explicit drain-ack:\n%s", wisp.Description)
	}
	ready, err := store.Ready()
	if err != nil {
		t.Fatalf("store.Ready(): %v", err)
	}
	if len(ready) != 1 || ready[0].ID != wisp.ID {
		t.Fatalf("Ready() = %#v, want only %s", ready, wisp.ID)
	}
}

func TestOrderRunPoolLegacyFormulaWarnsWhenRootIsNotReadyVisible(t *testing.T) {
	formulaDir := t.TempDir()
	writeFile(t, filepath.Join(formulaDir, "mol-legacy-cleanup.toml"), `
formula = "mol-legacy-cleanup"
version = 1

[[steps]]
id = "work"
title = "Do legacy cleanup"
description = "Do the cleanup."
`)

	aa := []orders.Order{
		{Name: "legacy-cleanup", Formula: "mol-legacy-cleanup", Trigger: "cron", Schedule: "0 3 * * *", Pool: "dog", FormulaLayer: formulaDir},
	}
	store := beads.NewMemStore()

	var stdout, stderr bytes.Buffer
	code := doOrderRun(aa, "legacy-cleanup", "", "/city", beads.OrdersStore{Store: store}, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doOrderRun = %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "scale-from-zero pools will not wake") {
		t.Fatalf("stderr = %q, want pool visibility warning", stderr.String())
	}
	results, err := store.ListByLabel("order-run:legacy-cleanup", 0)
	if err != nil {
		t.Fatalf("store.ListByLabel(): %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("store.ListByLabel() len = %d, want 1 (%#v)", len(results), results)
	}
	if results[0].Type != "molecule" {
		t.Fatalf("legacy root Type = %q, want molecule", results[0].Type)
	}
	ready, err := store.Ready()
	if err != nil {
		t.Fatalf("store.Ready(): %v", err)
	}
	if len(ready) != 0 {
		t.Fatalf("Ready() = %#v, want no Ready-visible root for legacy molecule formula", ready)
	}
}

func TestOrderRunNonPoolDoesNotSetRouteMetadata(t *testing.T) {
	aa := []orders.Order{
		{Name: "cleanup", Formula: "mol-cleanup", Trigger: "cron", Schedule: "0 3 * * *", FormulaLayer: sharedTestFormulaDir},
	}
	store := beads.NewMemStore()

	var stdout, stderr bytes.Buffer
	code := doOrderRun(aa, "cleanup", "", "/city", beads.OrdersStore{Store: store}, nil, &stdout, &stderr)
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
		t.Fatalf("gc.routed_to = %q, want empty for unrouted order", got)
	}
	assertNoDeprecatedPoolDemandMetadata(t, results[0].Metadata)
}

func assertNoDeprecatedPoolDemandMetadata(t *testing.T, metadata map[string]string) {
	t.Helper()
	if got := metadata["gc.pool_demand"]; got != "" {
		t.Fatalf("gc.pool_demand = %q, want empty", got)
	}
}

func TestOrderRunResolvesImportedPackPoolAgainstCityShadow(t *testing.T) {
	cityDir := t.TempDir()
	writeImportedDogOrderFixture(t, cityDir, true)
	_, aa := loadImportedDogOrders(t, cityDir)
	store := beads.NewMemStore()

	var stdout, stderr bytes.Buffer
	code := doOrderRun(aa, "digest", "", cityDir, beads.OrdersStore{Store: store}, nil, &stdout, &stderr)
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
	code := doOrderRun(aa, "digest", "", cityDir, beads.OrdersStore{Store: store}, nil, &stdout, &stderr)
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
	code := doOrderRun(aa, "digest", "", cityDir, beads.OrdersStore{Store: store}, nil, &stdout, &stderr)
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
	code := doOrderRun(aa, "digest", "", cityDir, beads.OrdersStore{Store: store}, nil, &stdout, &stderr)
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
	code := doOrderRun(aa, "cleanup", "", "/city", beads.OrdersStore{Store: store}, nil, &stdout, &stderr)
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
	if err := os.WriteFile(filepath.Join(dir, "order-required-vars.toml"), []byte(formulaBody), 0o644); err != nil {
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
	code := doOrderRun(aa, "digest", "", "/city", beads.OrdersStore{Store: store}, nil, &stdout, &stderr)
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
	if err := os.WriteFile(filepath.Join(formulaDir, "graph-work.toml"), []byte(graphFormula), 0o644); err != nil {
		t.Fatal(err)
	}

	aa := []orders.Order{
		{Name: "acceptance-patrol", Formula: "graph-work", Trigger: "cooldown", Interval: "15m", Pool: "quinn", FormulaLayer: formulaDir},
	}
	store := beads.NewMemStore()

	var stdout, stderr bytes.Buffer
	code := doOrderRun(aa, "acceptance-patrol", "", cityDir, beads.OrdersStore{Store: store}, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doOrderRun = %d, want 0; stderr: %s", code, stderr.String())
	}
	all, err := store.ListOpen()
	if err != nil {
		t.Fatalf("store.ListOpen(): %v", err)
	}

	foundRoot := false
	foundWorker := false
	for _, bead := range all {
		switch bead.Title {
		case "graph-work":
			if bead.Assignee != "" {
				t.Fatalf("workflow root assignee = %q, want empty routed pool queue", bead.Assignee)
			}
			if bead.Metadata["gc.kind"] != "workflow" {
				t.Fatalf("workflow root gc.kind = %q, want workflow", bead.Metadata["gc.kind"])
			}
			if bead.Metadata["gc.routed_to"] != "quinn" {
				t.Fatalf("workflow root gc.routed_to = %q, want quinn", bead.Metadata["gc.routed_to"])
			}
			foundRoot = true
		case "Do work":
			if bead.Assignee != "" {
				t.Fatalf("worker assignee = %q, want empty child under routed workflow root", bead.Assignee)
			}
			foundWorker = true
		}
	}

	if !foundRoot {
		t.Fatal("missing routed workflow root")
	}
	if !foundWorker {
		t.Fatal("missing workflow child step")
	}
}

func TestOrderRunGraphV2ConvoyReferenceRequiresTarget(t *testing.T) {
	formulatest.EnableV2ForTest(t)
	dir := t.TempDir()
	graphFormula := `
formula = "graph-needs-convoy"
version = 1
contract = "graph.v2"
type = "workflow"

[[steps]]
id = "step"
title = "Do work"
description = "Inspect convoy {{convoy_id}}"
`
	if err := os.WriteFile(filepath.Join(dir, "graph-needs-convoy.formula.toml"), []byte(strings.TrimSpace(graphFormula)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	aa := []orders.Order{
		{Name: "convoy-patrol", Formula: "graph-needs-convoy", Trigger: "cooldown", Interval: "15m", FormulaLayer: dir},
	}
	store := beads.NewMemStore()
	var stdout, stderr bytes.Buffer
	code := doOrderRun(aa, "convoy-patrol", "", "/city", beads.OrdersStore{Store: store}, nil, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("doOrderRun = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "requires a targeted formulas v2 invocation") {
		t.Fatalf("stderr = %q, want targeted formulas v2 invocation error", stderr.String())
	}
	results, err := store.ListByLabel("order-run:convoy-patrol", 0, beads.IncludeClosed)
	if err != nil {
		t.Fatalf("ListByLabel: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("created order-run beads = %+v, want none", results)
	}
}

func TestOrderRunNotFound(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := doOrderRun(nil, "nonexistent", "", "/city", beads.OrdersStore{}, nil, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("doOrderRun = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "not found") {
		t.Errorf("stderr = %q, want 'not found'", stderr.String())
	}
}

func TestOrderRunExecRigUsesScopedWorkdirAndStoreEnv(t *testing.T) {
	disableManagedDoltRecoveryForTest(t)

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
		Name:     "dolt-test-cooldown",
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
	clearGCEnv(t)
	t.Setenv("GC_DOLT", "skip")
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
		Name:     "dolt-test-cooldown",
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

func TestOrderRunExecTrackedLabelsEnvBuildFailure(t *testing.T) {
	clearAmbientPostgresEnv(t)
	t.Setenv("GC_BEADS", "bd")

	cityDir := t.TempDir()
	writePGScopeFixture(t, cityDir, "")
	if err := os.WriteFile(filepath.Join(cityDir, ".beads", "config.yaml"), []byte(`issue_prefix: city
gc.endpoint_origin: managed_city
gc.endpoint_status: verified
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}

	store := beads.NewMemStore()
	eventLog := events.NewFake()
	eventLog.Record(events.Event{Type: events.BeadClosed, Actor: "test"})
	a := orders.Order{Name: "pg-env", Trigger: "event", On: events.BeadClosed, Exec: "true"}

	var stdout, stderr bytes.Buffer
	code := doOrderRunExecTracked(a, cityDir, nil, beads.OrdersStore{Store: store}, eventLog, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("doOrderRunExecTracked = 0, want env failure; stdout=%q stderr=%q", stdout.String(), stderr.String())
	}

	all := trackingBeads(t, store, "order-run:pg-env")
	if len(all) != 1 {
		t.Fatalf("tracking bead count = %d, want 1", len(all))
	}
	if !slicesContain(all[0].Labels, "exec-env-failed") {
		t.Fatalf("tracking bead labels = %v, want exec-env-failed", all[0].Labels)
	}
	if slicesContain(all[0].Labels, "exec-failed") {
		t.Fatalf("tracking bead labels = %v, want no exec-failed for env-build failure", all[0].Labels)
	}
}

func TestOrderRunExecTrackedRecordsCooldownForNonEventRigOrder(t *testing.T) {
	// Regression for #3570: a manual `gc order run <name> --rig <rig>` of a
	// cooldown-triggered exec order must record a rig-scoped cooldown tracking
	// bead. Without it, `gc order check --rig` always reports "never run" and
	// the order re-fires every tick (process exhaustion).
	disableManagedDoltRecoveryForTest(t)

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

	store := beads.NewMemStore()
	a := orders.Order{
		Name:     "poll",
		Rig:      "frontend",
		Trigger:  "cooldown",
		Interval: "1m",
		Exec:     "true",
	}

	var stdout, stderr bytes.Buffer
	code := doOrderRunExecTracked(a, cityDir, cfg, beads.OrdersStore{Store: store}, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doOrderRunExecTracked = %d, want 0; stderr: %s", code, stderr.String())
	}

	all := trackingBeads(t, store, "order-run:poll:rig:frontend")
	if len(all) != 1 {
		t.Fatalf("rig-scoped tracking bead count = %d, want 1 (labels must be scoped so gc order check finds the run)", len(all))
	}
	if all[0].Title != "order:poll:rig:frontend" {
		t.Fatalf("tracking bead title = %q, want order:poll:rig:frontend", all[0].Title)
	}
	if !slicesContain(all[0].Labels, "exec") {
		t.Fatalf("tracking bead labels = %v, want exec", all[0].Labels)
	}
}

func TestOrderRunExecEnvBuildFailureRedactsProcessSecrets(t *testing.T) {
	clearAmbientPostgresEnv(t)
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_ORDER_SECRET", "db.example.test")

	cityDir := t.TempDir()
	writePGScopeFixture(t, cityDir, "")
	if err := os.WriteFile(filepath.Join(cityDir, ".beads", "config.yaml"), []byte(`issue_prefix: city
gc.endpoint_origin: managed_city
gc.endpoint_status: verified
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}

	a := orders.Order{Name: "pg-env", Trigger: "cooldown", Interval: "1m", Exec: "true"}
	var stdout, stderr bytes.Buffer
	result := doOrderRunExecResult(a, cityDir, nil, &stdout, &stderr)
	if result.code == 0 {
		t.Fatalf("doOrderRunExecResult = 0, want env failure; stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	if result.failureLabel != "exec-env-failed" {
		t.Fatalf("failureLabel = %q, want exec-env-failed", result.failureLabel)
	}
	if strings.Contains(stderr.String(), "db.example.test") {
		t.Fatalf("stderr leaked process secret: %s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "[redacted]") {
		t.Fatalf("stderr = %q, want redaction marker", stderr.String())
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
	code := doOrderHistory("", "", aa, beads.OrdersStore{Store: store}, &stdout)
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

func TestOrderHistoryJSON(t *testing.T) {
	store := beads.NewBdStore(t.TempDir(), func(_, _ string, args ...string) ([]byte, error) {
		if strings.Contains(strings.Join(args, " "), "--label=order-run:digest") {
			return []byte(`[{"id":"WP-42","title":"digest wisp","status":"closed","issue_type":"task","created_at":"2026-02-27T10:00:00Z","labels":["order-run:digest"]}]`), nil
		}
		return []byte(`[]`), nil
	})
	aa := []orders.Order{{Name: "digest", Formula: "mol-digest"}}
	resolver := func(orders.Order) ([]beads.OrdersStore, error) {
		return []beads.OrdersStore{{Store: store}}, nil
	}

	var stdout, stderr bytes.Buffer
	code := doOrderHistoryWithStoresResolverJSON("digest", "", aa, resolver, true, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doOrderHistoryWithStoresResolverJSON = %d, want 0; stderr: %s", code, stderr.String())
	}
	lines := strings.Split(strings.TrimSuffix(stdout.String(), "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("stdout lines = %d, want 1: %q", len(lines), stdout.String())
	}
	var payload orderHistoryJSONResult
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout.String())
	}
	if payload.SchemaVersion != "1" || !payload.OK || payload.Summary.Total != 1 {
		t.Fatalf("payload = %+v, want ok schema v1 with one entry", payload)
	}
	if len(payload.Entries) != 1 || payload.Entries[0].Order != "digest" || payload.Entries[0].BeadID != "WP-42" {
		t.Fatalf("entries = %+v, want digest WP-42", payload.Entries)
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
	code := doOrderHistory("digest", "", aa, beads.OrdersStore{Store: store}, &stdout)
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
	code := doOrderHistory("", "", aa, beads.OrdersStore{Store: store}, &stdout)
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
	resolver := func(a orders.Order) (beads.OrdersStore, error) {
		if a.Rig == "frontend" {
			return beads.OrdersStore{Store: rigStore}, nil
		}
		return beads.OrdersStore{Store: cityStore}, nil
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
	resolver := func(a orders.Order) ([]beads.OrdersStore, error) {
		if a.Rig == "frontend" {
			return []beads.OrdersStore{{Store: rigStore}, {Store: legacyStore}}, nil
		}
		return []beads.OrdersStore{{Store: rigStore}}, nil
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
	resolver := func(a orders.Order) ([]beads.OrdersStore, error) {
		if a.Rig == "frontend" {
			return []beads.OrdersStore{{Store: rigStore}, {Store: legacyStore}}, nil
		}
		return []beads.OrdersStore{{Store: legacyStore}}, nil
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

func TestBdCursorUsesRowsFromPartialTierError(t *testing.T) {
	store := &partialListStore{
		Store: beads.NewMemStore(),
		rows: []beads.Bead{{
			ID:     "cursor-1",
			Labels: []string{"order:digest", "seq:42"},
		}},
		err: fmt.Errorf("wisps tier unavailable"),
	}

	got, err := bdCursor(store, "digest")
	if err != nil {
		t.Fatalf("bdCursor: %v", err)
	}
	if got != 42 {
		t.Fatalf("bdCursor() = %d, want 42 from surviving rows", got)
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
	resolver := func(orders.Order) ([]beads.OrdersStore, error) {
		return []beads.OrdersStore{{Store: store}, {Store: store}}, nil
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
	resolver := func(orders.Order) ([]beads.OrdersStore, error) {
		return []beads.OrdersStore{{Store: rigStore}, {Store: legacyStore}}, nil
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
		{Name: "db-health", Formula: "mol-db-health", Trigger: "cooldown", Interval: "5m", Rig: "demo-repo", Source: "/topo/orders/db-health.toml"},
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
	code := doOrderRun(aa, "db-health", "demo-repo", "/city", beads.OrdersStore{Store: store}, nil, &stdout, &stderr)
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

// --- gc order history: API routing (ga-h6w / ga-6q1) ---

// okOrderHistoryHandler serves a non-stale single-entry history matching
// the test order.
func okOrderHistoryHandler(_ *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/orders/history") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("X-GC-Cache-Age-S", "2")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"entries": []map[string]any{
				{
					"bead_id":        "ca-1",
					"name":           "digest",
					"scoped_name":    "digest",
					"created_at":     "2026-04-22T12:00:00Z",
					"labels":         []string{"order-run:digest"},
					"capture_output": false,
					"has_output":     false,
					"store_ref":      "city",
				},
			},
		})
	})
}

// writeOrderHistoryTestCity creates a minimal city directory with a
// city.toml and a single city-level order so both the API-render path
// and the fallback path have something to format.
func writeOrderHistoryTestCity(t *testing.T) string {
	t.Helper()
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := `[workspace]
name = "test-city"

[[agent]]
name = "mayor"
`
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}
	formulasDir := filepath.Join(cityPath, "formulas")
	if err := os.MkdirAll(formulasDir, 0o755); err != nil {
		t.Fatal(err)
	}
	ordersDir := filepath.Join(cityPath, "orders")
	if err := os.MkdirAll(ordersDir, 0o755); err != nil {
		t.Fatal(err)
	}
	orderToml := `[order]
trigger = "manual"
formula = "mol-digest"
`
	if err := os.WriteFile(filepath.Join(ordersDir, "digest.toml"), []byte(orderToml), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_BEADS_SCOPE_ROOT", "")
	return cityPath
}

func TestRouteOrderHistory_SixRowMatrix(t *testing.T) {
	tests := []struct {
		name         string
		handler      rigListMatrixHandler
		useNilClient bool
		nilReason    string
		wantExit     int
		wantRoute    string
		wantReason   string
		wantStderr   string
		wantStdout   string
	}{
		{
			name:       "api-happy-path",
			handler:    okOrderHistoryHandler,
			wantExit:   0,
			wantRoute:  "api",
			wantStdout: "digest",
		},
		{
			name:       "api-cache-not-live",
			handler:    problemHandler(http.StatusServiceUnavailable, "cache_not_live: supervisor cache is priming"),
			wantExit:   0,
			wantRoute:  "fallback",
			wantReason: "cache-not-live",
		},
		{
			name:       "api-500-fallback",
			handler:    problemHandler(http.StatusInternalServerError, "internal: something exploded"),
			wantExit:   0,
			wantRoute:  "fallback",
			wantReason: "conn-refused",
		},
		{
			name:       "api-404-error",
			handler:    problemHandler(http.StatusNotFound, "not_found: city not configured"),
			wantExit:   1,
			wantStderr: "not_found",
		},
		{
			name:         "controller-down",
			useNilClient: true,
			nilReason:    "controller-down",
			wantExit:     0,
			wantRoute:    "fallback",
			wantReason:   "controller-down",
		},
		{
			name:         "escape-hatch",
			useNilClient: true,
			nilReason:    "escape-hatch",
			wantExit:     0,
			wantRoute:    "fallback",
			wantReason:   "escape-hatch",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GC_DEBUG", "1") // force route=... lines into stderr buffer

			cityPath := writeOrderHistoryTestCity(t)
			cfg, err := loadCityConfig(cityPath, &bytes.Buffer{})
			if err != nil {
				t.Fatalf("loadCityConfig: %v", err)
			}
			aa, code := loadAllOrders(cityPath, cfg, &bytes.Buffer{}, "test")
			if code != 0 {
				t.Fatalf("loadAllOrders = %d", code)
			}

			var c *api.Client
			if !tc.useNilClient {
				srv := httptest.NewServer(tc.handler(t))
				defer srv.Close()
				c = api.NewCityScopedClient(srv.URL, "test-city")
			}

			var stdout, stderr bytes.Buffer
			got := routeOrderHistory(cityPath, cfg, "digest", "", aa, c, tc.nilReason, false, &stdout, &stderr)

			if got != tc.wantExit {
				t.Fatalf("exit = %d, want %d; stderr=%q stdout=%q", got, tc.wantExit, stderr.String(), stdout.String())
			}
			if tc.wantRoute != "" {
				want := "route=" + tc.wantRoute
				if tc.wantReason != "" {
					want += " reason=" + tc.wantReason
				}
				if !strings.Contains(stderr.String(), want) {
					t.Errorf("stderr missing %q:\n%s", want, stderr.String())
				}
				if n := strings.Count(stderr.String(), "route="); n != 1 {
					t.Errorf("route=... lines = %d, want 1:\n%s", n, stderr.String())
				}
			}
			if tc.wantStderr != "" && !strings.Contains(stderr.String(), tc.wantStderr) {
				t.Errorf("stderr missing %q:\n%s", tc.wantStderr, stderr.String())
			}
			if tc.wantStdout != "" && !strings.Contains(stdout.String(), tc.wantStdout) {
				t.Errorf("stdout missing %q:\n%s", tc.wantStdout, stdout.String())
			}
			// Fallback rows must produce the local iterator's rendered output
			// (same "No order history." header/empty-state as the direct path).
			if tc.wantRoute == "fallback" {
				if !strings.Contains(stdout.String(), "No order history") && !strings.Contains(stdout.String(), "ORDER") {
					t.Errorf("fallback stdout missing history header/empty-state:\n%s", stdout.String())
				}
			}
		})
	}
}

// TestRouteOrderHistory_MultiOrderFallback verifies that `gc order history`
// with no name falls back with reason=multi-order so the deliberate
// no-API-routing branch is audit-logged.
func TestRouteOrderHistory_MultiOrderFallback(t *testing.T) {
	t.Setenv("GC_DEBUG", "1")

	cityPath := writeOrderHistoryTestCity(t)
	cfg, err := loadCityConfig(cityPath, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("loadCityConfig: %v", err)
	}
	aa, code := loadAllOrders(cityPath, cfg, &bytes.Buffer{}, "test")
	if code != 0 {
		t.Fatalf("loadAllOrders = %d", code)
	}

	srv := httptest.NewServer(okOrderHistoryHandler(t))
	defer srv.Close()
	c := api.NewCityScopedClient(srv.URL, "test-city")

	var stdout, stderr bytes.Buffer
	// Name empty → should not hit the API.
	if got := routeOrderHistory(cityPath, cfg, "", "", aa, c, "", false, &stdout, &stderr); got != 0 {
		t.Fatalf("exit = %d, stderr=%q", got, stderr.String())
	}
	if !strings.Contains(stderr.String(), "route=fallback reason=multi-order") {
		t.Errorf("stderr missing multi-order fallback:\n%s", stderr.String())
	}
}

// TestRouteOrderHistory_StaleBannerOver30s verifies the > 30 s stale banner
// on the API render path; matches the rig-list staleness test contract.
func TestRouteOrderHistory_StaleBannerOver30s(t *testing.T) {
	t.Setenv("GC_DEBUG", "0")

	cityPath := writeOrderHistoryTestCity(t)
	cfg, err := loadCityConfig(cityPath, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("loadCityConfig: %v", err)
	}
	aa, _ := loadAllOrders(cityPath, cfg, &bytes.Buffer{}, "test")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-GC-Cache-Age-S", "45")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"entries": []map[string]any{
				{
					"bead_id":        "ca-1",
					"name":           "digest",
					"scoped_name":    "digest",
					"created_at":     "2026-04-22T12:00:00Z",
					"labels":         []string{"order-run:digest"},
					"capture_output": false,
					"has_output":     false,
					"store_ref":      "city",
				},
			},
		})
	}))
	defer srv.Close()
	c := api.NewCityScopedClient(srv.URL, "test-city")

	var stdout, stderr bytes.Buffer
	if code := routeOrderHistory(cityPath, cfg, "digest", "", aa, c, "", false, &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d, stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "cache age: 45s") {
		t.Errorf("stale banner missing from human output:\n%s", stdout.String())
	}
}

// TestOrderScopedName verifies city-level vs rig-level scoping matches the
// server's ScopedName() convention (name alone vs name:rig:<rig>).
func TestOrderScopedName(t *testing.T) {
	aa := []orders.Order{
		{Name: "digest"},
		{Name: "cleanup", Rig: "frontend"},
	}
	cases := []struct {
		name string
		rig  string
		want string
	}{
		{"digest", "", "digest"},
		{"cleanup", "frontend", "cleanup:rig:frontend"},
		{"unknown", "", "unknown"},
		{"unknown", "backend", "unknown:rig:backend"},
	}
	for _, tc := range cases {
		got := orderScopedName(tc.name, tc.rig, aa)
		if got != tc.want {
			t.Errorf("orderScopedName(%q, %q) = %q, want %q", tc.name, tc.rig, got, tc.want)
		}
	}
}
