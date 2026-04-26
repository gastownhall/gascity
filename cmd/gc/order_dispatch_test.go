package main

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/orders"
)

func trackingBeads(t *testing.T, store beads.Store, label string) []beads.Bead {
	t.Helper()
	all, err := store.ListByLabel(label, 0, beads.IncludeClosed)
	if err != nil {
		t.Fatalf("ListByLabel(%q): %v", label, err)
	}
	return all
}

func workBeadByOrderLabel(t *testing.T, store beads.Store, label string) beads.Bead {
	t.Helper()
	all := trackingBeads(t, store, label)
	for _, b := range all {
		if !strings.HasPrefix(b.Title, "order:") {
			return b
		}
	}
	t.Fatalf("no non-tracking bead found for %q", label)
	return beads.Bead{}
}

type selectiveUpdateFailStore struct {
	beads.Store
}

func (s selectiveUpdateFailStore) Update(id string, opts beads.UpdateOpts) error {
	for _, label := range opts.Labels {
		if strings.HasPrefix(label, "order-run:") {
			return fmt.Errorf("label failed")
		}
	}
	return s.Store.Update(id, opts)
}

func TestOrderDispatcherNil(t *testing.T) {
	ad := buildOrderDispatcher(t.TempDir(), &config.City{}, events.Discard, &bytes.Buffer{})
	if ad != nil {
		t.Error("expected nil dispatcher for empty orders")
	}
}

func TestBuildOrderDispatcherNoOrders(t *testing.T) {
	// City with formula layers that exist but contain no orders.
	dir := t.TempDir()
	cfg := &config.City{}
	ad := buildOrderDispatcher(dir, cfg, events.Discard, &bytes.Buffer{})
	if ad != nil {
		t.Error("expected nil dispatcher when no orders exist")
	}
}

func TestOrderDispatchManualFiltered(t *testing.T) {
	ad := buildOrderDispatcherFromList(
		[]orders.Order{{Name: "manual-only", Trigger: "manual", Formula: "noop"}},
		beads.NewMemStore(), nil,
	)
	if ad != nil {
		t.Error("expected nil dispatcher — manual orders should be filtered out")
	}
}

func TestOrderDispatchCooldownDue(t *testing.T) {
	store := beads.NewMemStore()

	aa := []orders.Order{{
		Name:         "test-order",
		Trigger:      "cooldown",
		Interval:     "1m",
		Formula:      "test-formula",
		Pool:         "worker",
		FormulaLayer: sharedTestFormulaDir,
	}}
	ad := buildOrderDispatcherFromList(aa, store, nil)
	if ad == nil {
		t.Fatal("expected non-nil dispatcher")
	}

	ad.dispatch(context.Background(), t.TempDir(), time.Now())

	// Wait briefly for goroutine to complete.
	time.Sleep(50 * time.Millisecond)

	// Verify tracking bead was created.
	all := trackingBeads(t, store, "order-run:test-order")
	if len(all) == 0 {
		t.Fatal("expected tracking bead to be created")
	}
	found := false
	for _, b := range all {
		for _, l := range b.Labels {
			if l == "order-run:test-order" {
				found = true
			}
		}
	}
	if !found {
		t.Error("tracking bead missing order-run:test-order label")
	}

	work := workBeadByOrderLabel(t, store, "order-run:test-order")
	if !slicesContain(work.Labels, "order-run:test-order") {
		t.Errorf("work bead missing order-run:test-order label, got %v", work.Labels)
	}
	if work.Metadata["gc.routed_to"] != "worker" {
		t.Errorf("gc.routed_to = %q, want %q", work.Metadata["gc.routed_to"], "worker")
	}
}

func TestOrderDispatchCooldownNotDue(t *testing.T) {
	store := beads.NewMemStore()

	// Seed a recent order-run bead.
	_, err := store.Create(beads.Bead{
		Title:  "order run",
		Labels: []string{"order-run:test-order"},
	})
	if err != nil {
		t.Fatal(err)
	}

	aa := []orders.Order{{
		Name:     "test-order",
		Trigger:  "cooldown",
		Interval: "1h", // 1 hour — far in the future
		Formula:  "test-formula",
	}}
	ad := buildOrderDispatcherFromList(aa, store, nil)
	if ad == nil {
		t.Fatal("expected non-nil dispatcher")
	}

	ad.dispatch(context.Background(), t.TempDir(), time.Now())

	// Wait briefly.
	time.Sleep(50 * time.Millisecond)

	// Should still have only the seed bead.
	all, _ := store.ListOpen()
	if len(all) != 1 {
		t.Errorf("expected 1 bead (seed only), got %d", len(all))
	}
}

func TestOrderDispatchMultiple(t *testing.T) {
	store := beads.NewMemStore()

	// Seed a recent run for order-b so only order-a is due.
	_, err := store.Create(beads.Bead{
		Title:  "recent run",
		Labels: []string{"order-run:order-b"},
	})
	if err != nil {
		t.Fatal(err)
	}

	aa := []orders.Order{
		{Name: "order-a", Trigger: "cooldown", Interval: "1m", Formula: "formula-a"},
		{Name: "order-b", Trigger: "cooldown", Interval: "1h", Formula: "formula-b"},
	}
	ad := buildOrderDispatcherFromList(aa, store, nil)
	if ad == nil {
		t.Fatal("expected non-nil dispatcher")
	}

	ad.dispatch(context.Background(), t.TempDir(), time.Now())

	// Wait briefly for goroutine.
	time.Sleep(50 * time.Millisecond)

	// Should have the seed bead + 1 tracking bead for order-a.
	all := trackingBeads(t, store, "order-run:order-a")
	trackingCount := 0
	for _, b := range all {
		for _, l := range b.Labels {
			if l == "order-run:order-a" {
				trackingCount++
			}
		}
	}
	if trackingCount != 1 {
		t.Errorf("expected 1 tracking bead for order-a, got %d", trackingCount)
	}
}

// --- exec order dispatch tests ---

func TestOrderDispatchExecDue(t *testing.T) {
	store := beads.NewMemStore()
	var rec memRecorder

	ran := false
	fakeExec := func(_ context.Context, _, _ string, _ []string) ([]byte, error) {
		ran = true
		return []byte("ok\n"), nil
	}

	aa := []orders.Order{{
		Name:     "wasteland-poll",
		Trigger:  "cooldown",
		Interval: "2m",
		Exec:     "$ORDER_DIR/scripts/poll.sh",
		Source:   "/city/formulas/orders/wasteland-poll/order.toml",
	}}
	ad := buildOrderDispatcherFromListExec(aa, store, nil, fakeExec, &rec)
	if ad == nil {
		t.Fatal("expected non-nil dispatcher")
	}

	ad.dispatch(context.Background(), t.TempDir(), time.Now())
	time.Sleep(100 * time.Millisecond)

	if !ran {
		t.Error("exec runner was not called")
	}

	// Check tracking bead exists with exec label.
	all := trackingBeads(t, store, "order-run:wasteland-poll")
	found := false
	hasExec := false
	for _, b := range all {
		for _, l := range b.Labels {
			if l == "order-run:wasteland-poll" {
				found = true
			}
			if l == "exec" {
				hasExec = true
			}
		}
	}
	if !found {
		t.Error("tracking bead missing order-run label")
	}
	if !hasExec {
		t.Error("tracking bead missing exec label")
	}

	// Check events.
	if !rec.hasType(events.OrderFired) {
		t.Error("missing order.fired event")
	}
	if !rec.hasType(events.OrderCompleted) {
		t.Error("missing order.completed event")
	}
}

func TestOrderDispatchExecFailure(t *testing.T) {
	store := beads.NewMemStore()
	var rec memRecorder
	var stderr bytes.Buffer
	tracking, err := store.Create(beads.Bead{
		Title:  "order:fail-exec",
		Labels: []string{"order-run:fail-exec", labelOrderTracking},
	})
	if err != nil {
		t.Fatal(err)
	}

	fakeExec := func(_ context.Context, _, _ string, _ []string) ([]byte, error) {
		return []byte("error output\n"), fmt.Errorf("exit status 1")
	}

	aa := []orders.Order{{
		Name:     "fail-exec",
		Trigger:  "cooldown",
		Interval: "2m",
		Exec:     "scripts/fail.sh",
	}}
	ad := buildOrderDispatcherFromListExec(aa, store, nil, fakeExec, &rec)
	mad := ad.(*memoryOrderDispatcher)
	mad.stderr = &stderr

	logs := captureCmdOrderLogs(t, func() {
		mad.dispatchExec(context.Background(), store, execStoreTarget{ScopeRoot: t.TempDir()}, aa[0], t.TempDir(), tracking.ID)
	})

	// Check tracking bead has exec-failed label.
	all := trackingBeads(t, store, "order-run:fail-exec")
	hasFailed := false
	for _, b := range all {
		for _, l := range b.Labels {
			if l == "exec-failed" {
				hasFailed = true
			}
		}
	}
	if !hasFailed {
		t.Error("tracking bead missing exec-failed label")
	}

	// Check order.failed event.
	if !rec.hasType(events.OrderFailed) {
		t.Error("missing order.failed event")
	}
	if !strings.Contains(logs, "order exec fail-exec failed") {
		t.Fatalf("logs = %q, want exec failure warning", logs)
	}
}

func TestOrderDispatchFormulaCookFailureLabelsTrackingBead(t *testing.T) {
	store := beads.NewMemStore()
	var rec memRecorder

	aa := []orders.Order{{
		Name:         "fail-formula",
		Trigger:      "cooldown",
		Interval:     "2m",
		Formula:      "missing-formula",
		FormulaLayer: sharedTestFormulaDir,
	}}
	ad := buildOrderDispatcherFromList(aa, store, nil)
	if ad == nil {
		t.Fatal("expected non-nil dispatcher")
	}

	mad := ad.(*memoryOrderDispatcher)
	mad.rec = &rec

	ad.dispatch(context.Background(), t.TempDir(), time.Now())
	time.Sleep(100 * time.Millisecond)

	all := trackingBeads(t, store, "order-run:fail-formula")
	hasFailed := false
	for _, b := range all {
		for _, l := range b.Labels {
			if l == "wisp-failed" {
				hasFailed = true
			}
		}
	}
	if !hasFailed {
		t.Error("tracking bead missing wisp-failed label after cook failure")
	}
	if !rec.hasType(events.OrderFailed) {
		t.Error("missing order.failed event")
	}
}

func TestOrderDispatchReportsAllMissingRequiredVarsAtOnce(t *testing.T) {
	store := beads.NewMemStore()
	var rec memRecorder

	formulaDir := t.TempDir()
	writeFile(t, filepath.Join(formulaDir, "order-required-vars.formula.toml"), `
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
`)

	aa := []orders.Order{{
		Name:         "fail-formula-vars",
		Trigger:      "cooldown",
		Interval:     "2m",
		Formula:      "order-required-vars",
		FormulaLayer: formulaDir,
	}}
	ad := buildOrderDispatcherFromList(aa, store, nil)
	if ad == nil {
		t.Fatal("expected non-nil dispatcher")
	}

	mad := ad.(*memoryOrderDispatcher)
	mad.rec = &rec

	ad.dispatch(context.Background(), t.TempDir(), time.Now())
	time.Sleep(100 * time.Millisecond)

	all := trackingBeads(t, store, "order-run:fail-formula-vars")
	hasFailed := false
	for _, b := range all {
		for _, l := range b.Labels {
			if l == "wisp-failed" {
				hasFailed = true
			}
		}
	}
	if !hasFailed {
		t.Error("tracking bead missing wisp-failed label after validation failure")
	}
	if !rec.hasType(events.OrderFailed) {
		t.Fatal("missing order.failed event")
	}

	var failedMessage string
	for _, event := range rec.events {
		if event.Type == events.OrderFailed && event.Subject == "fail-formula-vars" {
			failedMessage = event.Message
			break
		}
	}
	if failedMessage == "" {
		t.Fatal("missing order.failed message for formula validation failure")
	}
	if !strings.Contains(failedMessage, `variable "target_id" is required`) {
		t.Fatalf("order.failed message = %q, want missing target_id reported", failedMessage)
	}
	if !strings.Contains(failedMessage, `variable "workspace" is required`) {
		t.Fatalf("order.failed message = %q, want missing workspace reported", failedMessage)
	}
	if strings.Contains(failedMessage, "bead title contains unresolved variable(s)") {
		t.Fatalf("order.failed message = %q, want consolidated required-var validation instead of title-only failure", failedMessage)
	}
}

func TestOrderDispatchFormulaLabelFailureLabelsTrackingBead(t *testing.T) {
	store := beads.NewMemStore()
	var rec memRecorder
	var stderr bytes.Buffer

	aa := []orders.Order{{
		Name:         "fail-label",
		Trigger:      "cooldown",
		Interval:     "2m",
		Formula:      "test-formula",
		FormulaLayer: sharedTestFormulaDir,
	}}
	ad := buildOrderDispatcherFromList(aa, selectiveUpdateFailStore{Store: store}, nil)
	if ad == nil {
		t.Fatal("expected non-nil dispatcher")
	}

	mad := ad.(*memoryOrderDispatcher)
	mad.rec = &rec
	mad.stderr = &stderr

	ad.dispatch(context.Background(), t.TempDir(), time.Now())
	time.Sleep(100 * time.Millisecond)

	all := trackingBeads(t, store, "order-run:fail-label")
	hasFailed := false
	for _, b := range all {
		for _, l := range b.Labels {
			if l == "wisp-failed" {
				hasFailed = true
			}
		}
	}
	if !hasFailed {
		t.Error("tracking bead missing wisp-failed label after label failure")
	}
	if !rec.hasType(events.OrderFailed) {
		t.Error("missing order.failed event")
	}
}

func TestOrderDispatchExecCooldown(t *testing.T) {
	store := beads.NewMemStore()

	// Seed a recent exec run.
	_, err := store.Create(beads.Bead{
		Title:  "order:wasteland-poll",
		Labels: []string{"order-run:wasteland-poll"},
	})
	if err != nil {
		t.Fatal(err)
	}

	ran := false
	fakeExec := func(_ context.Context, _, _ string, _ []string) ([]byte, error) {
		ran = true
		return nil, nil
	}

	aa := []orders.Order{{
		Name:     "wasteland-poll",
		Trigger:  "cooldown",
		Interval: "1h",
		Exec:     "scripts/poll.sh",
	}}
	ad := buildOrderDispatcherFromListExec(aa, store, nil, fakeExec, nil)

	ad.dispatch(context.Background(), t.TempDir(), time.Now())
	time.Sleep(50 * time.Millisecond)

	if ran {
		t.Error("exec should not have run — cooldown not elapsed")
	}
}

func TestOrderDispatchExecOrderDir(t *testing.T) {
	store := beads.NewMemStore()
	var gotEnv []string

	fakeExec := func(_ context.Context, _, _ string, env []string) ([]byte, error) {
		gotEnv = env
		return nil, nil
	}

	aa := []orders.Order{{
		Name:     "poll",
		Trigger:  "cooldown",
		Interval: "1m",
		Exec:     "$ORDER_DIR/scripts/poll.sh",
		Source:   "/city/formulas/orders/poll/order.toml",
	}}
	ad := buildOrderDispatcherFromListExec(aa, store, nil, fakeExec, nil)

	ad.dispatch(context.Background(), "/city-root", time.Now())
	time.Sleep(100 * time.Millisecond)

	foundDir := false
	foundCity := false
	foundCityPath := false
	foundRuntime := false
	for _, e := range gotEnv {
		if e == "ORDER_DIR=/city/formulas/orders/poll" {
			foundDir = true
		}
		if e == "GC_CITY=/city-root" {
			foundCity = true
		}
		if e == "GC_CITY_PATH=/city-root" {
			foundCityPath = true
		}
		if e == "GC_CITY_RUNTIME_DIR=/city-root/.gc/runtime" {
			foundRuntime = true
		}
	}
	if !foundDir {
		t.Errorf("ORDER_DIR not set correctly, got env: %v", gotEnv)
	}
	if !foundCity {
		t.Errorf("GC_CITY not set correctly, got env: %v", gotEnv)
	}
	if !foundCityPath {
		t.Errorf("GC_CITY_PATH not set correctly, got env: %v", gotEnv)
	}
	if !foundRuntime {
		t.Errorf("GC_CITY_RUNTIME_DIR not set correctly, got env: %v", gotEnv)
	}
}

func TestOrderDispatchExecPackDir(t *testing.T) {
	store := beads.NewMemStore()
	var gotEnv []string

	fakeExec := func(_ context.Context, _, _ string, env []string) ([]byte, error) {
		gotEnv = env
		return nil, nil
	}

	aa := []orders.Order{{
		Name:         "gate-sweep",
		Trigger:      "cooldown",
		Interval:     "1m",
		Exec:         "$PACK_DIR/scripts/gate-sweep.sh",
		Source:       "/city/packs/maintenance/formulas/orders/gate-sweep/order.toml",
		FormulaLayer: "/city/packs/maintenance/formulas",
	}}
	ad := buildOrderDispatcherFromListExec(aa, store, nil, fakeExec, nil)

	ad.dispatch(context.Background(), "/city-root", time.Now())
	time.Sleep(100 * time.Millisecond)

	foundPackDir := false
	foundAutoDir := false
	foundPackName := false
	foundPackState := false
	for _, e := range gotEnv {
		if e == "PACK_DIR=/city/packs/maintenance" {
			foundPackDir = true
		}
		if e == "ORDER_DIR=/city/packs/maintenance/formulas/orders/gate-sweep" {
			foundAutoDir = true
		}
		if e == "GC_PACK_NAME=maintenance" {
			foundPackName = true
		}
		if e == "GC_PACK_STATE_DIR=/city-root/.gc/runtime/packs/maintenance" {
			foundPackState = true
		}
	}
	if !foundPackDir {
		t.Errorf("PACK_DIR not set correctly, got env: %v", gotEnv)
	}
	if !foundAutoDir {
		t.Errorf("ORDER_DIR not set correctly, got env: %v", gotEnv)
	}
	if !foundPackName {
		t.Errorf("GC_PACK_NAME not set correctly, got env: %v", gotEnv)
	}
	if !foundPackState {
		t.Errorf("GC_PACK_STATE_DIR not set correctly, got env: %v", gotEnv)
	}
}

func TestOrderDispatchExecManagedDoltPreservesOrderPackStateDir(t *testing.T) {
	store := beads.NewMemStore()
	cityDir := t.TempDir()
	dataDir := filepath.Join(cityDir, ".beads", "dolt")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
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
	writeFile(t, filepath.Join(cityDir, ".beads", "metadata.json"), `{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":"ct"}`)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer func() {
		if err := listener.Close(); err != nil {
			t.Fatalf("Close listener: %v", err)
		}
	}()
	stateDir := filepath.Join(cityDir, ".gc", "runtime", "packs", "dolt")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(stateDir, "dolt-state.json"), fmt.Sprintf(
		`{"running":true,"pid":%d,"port":%d,"data_dir":%q}`,
		os.Getpid(),
		listener.Addr().(*net.TCPAddr).Port,
		dataDir,
	))

	envCh := make(chan []string, 1)
	fakeExec := func(_ context.Context, _, _ string, env []string) ([]byte, error) {
		envCh <- env
		return nil, nil
	}
	aa := []orders.Order{{
		Name:         "gate-sweep",
		Trigger:      "cooldown",
		Interval:     "1m",
		Exec:         "$PACK_DIR/scripts/gate-sweep.sh",
		Source:       filepath.Join(cityDir, "packs", "maintenance", "formulas", "orders", "gate-sweep", "order.toml"),
		FormulaLayer: filepath.Join(cityDir, "packs", "maintenance", "formulas"),
	}}
	ad := buildOrderDispatcherFromListExec(aa, store, nil, fakeExec, nil)
	ad.dispatch(context.Background(), cityDir, time.Now())

	got := orderDispatchTestEnv(t, envCh)
	wantPackState := filepath.Join(cityDir, ".gc", "runtime", "packs", "maintenance")
	if got["GC_PACK_STATE_DIR"] != wantPackState {
		t.Fatalf("GC_PACK_STATE_DIR = %q, want order pack state %q; env=%v", got["GC_PACK_STATE_DIR"], wantPackState, got)
	}
	wantDoltState := filepath.Join(cityDir, ".gc", "runtime", "packs", "dolt", "dolt-state.json")
	if got["GC_DOLT_STATE_FILE"] != wantDoltState {
		t.Fatalf("GC_DOLT_STATE_FILE = %q, want %q; env=%v", got["GC_DOLT_STATE_FILE"], wantDoltState, got)
	}
}

func TestOrderDispatchExecManagedDoltUsesTrustedCityRuntimeDir(t *testing.T) {
	store := beads.NewMemStore()
	cityDir := t.TempDir()
	dataDir := filepath.Join(cityDir, ".beads", "dolt")
	customRuntimeDir := filepath.Join(t.TempDir(), "runtime-root")
	packStateDir := filepath.Join(customRuntimeDir, "packs", "dolt")
	t.Setenv("GC_CITY_PATH", cityDir)
	t.Setenv("GC_CITY_RUNTIME_DIR", customRuntimeDir)
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(packStateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(cityDir, "city.toml"), `[workspace]
name = "test-city"
prefix = "ct"

[beads]
provider = "bd"
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
	writeFile(t, filepath.Join(packStateDir, "dolt-state.json"), fmt.Sprintf(
		`{"running":true,"pid":%d,"port":%d,"data_dir":%q}`,
		os.Getpid(),
		listener.Addr().(*net.TCPAddr).Port,
		dataDir,
	))

	envCh := make(chan []string, 1)
	fakeExec := func(_ context.Context, _, _ string, env []string) ([]byte, error) {
		envCh <- env
		return nil, nil
	}
	aa := []orders.Order{{
		Name:     "dolt-gc-nudge",
		Trigger:  "cooldown",
		Interval: "1m",
		Exec:     "gc dolt gc-nudge",
	}}
	ad := buildOrderDispatcherFromListExec(aa, store, nil, fakeExec, nil)
	ad.dispatch(context.Background(), cityDir, time.Now())

	got := orderDispatchTestEnv(t, envCh)
	if got["GC_CITY_RUNTIME_DIR"] != customRuntimeDir {
		t.Fatalf("GC_CITY_RUNTIME_DIR = %q, want %q; env=%v", got["GC_CITY_RUNTIME_DIR"], customRuntimeDir, got)
	}
	wantStateFile := filepath.Join(packStateDir, "dolt-state.json")
	if got["GC_DOLT_STATE_FILE"] != wantStateFile {
		t.Fatalf("GC_DOLT_STATE_FILE = %q, want %q; env=%v", got["GC_DOLT_STATE_FILE"], wantStateFile, got)
	}
}

func TestOrderDispatchExecPackDirEmpty(t *testing.T) {
	// When FormulaLayer is empty, PACK_DIR should not be in env.
	store := beads.NewMemStore()
	var gotEnv []string

	fakeExec := func(_ context.Context, _, _ string, env []string) ([]byte, error) {
		gotEnv = env
		return nil, nil
	}

	aa := []orders.Order{{
		Name:     "no-layer",
		Trigger:  "cooldown",
		Interval: "1m",
		Exec:     "scripts/test.sh",
		Source:   "/city/formulas/orders/no-layer/order.toml",
		// FormulaLayer intentionally empty.
	}}
	ad := buildOrderDispatcherFromListExec(aa, store, nil, fakeExec, nil)

	ad.dispatch(context.Background(), "/city-root", time.Now())
	time.Sleep(100 * time.Millisecond)

	for _, e := range gotEnv {
		if strings.HasPrefix(e, "PACK_DIR=") {
			t.Errorf("PACK_DIR should not be set when FormulaLayer is empty, got: %s", e)
		}
		if strings.HasPrefix(e, "GC_PACK_STATE_DIR=") {
			t.Errorf("GC_PACK_STATE_DIR should not be set when FormulaLayer is empty, got: %s", e)
		}
	}
}

func TestOrderDispatchExecRigUsesScopedWorkdirAndStoreEnv(t *testing.T) {
	store := beads.NewMemStore()
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "frontend")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	var gotDir string
	var gotEnv []string

	fakeExec := func(_ context.Context, _, dir string, env []string) ([]byte, error) {
		gotDir = dir
		gotEnv = env
		return nil, nil
	}

	aa := []orders.Order{{
		Name:     "poll",
		Rig:      "frontend",
		Trigger:  "cooldown",
		Interval: "1m",
		Exec:     "$ORDER_DIR/scripts/poll.sh",
		Source:   "/city/formulas/orders/poll/order.toml",
	}}
	ad := buildOrderDispatcherFromListExec(aa, store, nil, fakeExec, nil)
	mad := ad.(*memoryOrderDispatcher)
	mad.cfg = &config.City{
		Workspace: config.Workspace{Name: "test-city", Prefix: "ct"},
		Rigs: []config.Rig{{
			Name:   "frontend",
			Path:   "frontend",
			Prefix: "fe",
		}},
	}

	ad.dispatch(context.Background(), cityDir, time.Now())
	time.Sleep(100 * time.Millisecond)

	if gotDir != rigDir {
		t.Fatalf("exec dir = %q, want %q", gotDir, rigDir)
	}
	checks := map[string]string{
		"GC_CITY":         cityDir,
		"GC_CITY_PATH":    cityDir,
		"BEADS_DIR":       filepath.Join(rigDir, ".beads"),
		"GC_STORE_ROOT":   rigDir,
		"GC_STORE_SCOPE":  "rig",
		"GC_BEADS_PREFIX": "fe",
		"GC_RIG":          "frontend",
		"GC_RIG_ROOT":     rigDir,
		"ORDER_DIR":       "/city/formulas/orders/poll",
	}
	for key, want := range checks {
		entry := key + "=" + want
		if !slicesContain(gotEnv, entry) {
			t.Fatalf("missing %s in env: %v", entry, gotEnv)
		}
	}
}

func TestOrderDispatchExecMarksExternalDoltTargetForManagedLocalOnlyOrders(t *testing.T) {
	store := beads.NewMemStore()
	cityDir := t.TempDir()
	t.Setenv("GC_PACK_STATE_DIR", filepath.Join(t.TempDir(), "poison-pack-state"))
	t.Setenv("GC_DOLT_DATA_DIR", filepath.Join(t.TempDir(), "poison-dolt-data"))
	t.Setenv("GC_DOLT_CONFIG_FILE", filepath.Join(t.TempDir(), "poison-dolt-config.yaml"))
	t.Setenv("GC_DOLT_STATE_FILE", filepath.Join(t.TempDir(), "poison-state.json"))
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
		"",
	}, "\n"))

	envCh := make(chan []string, 1)
	fakeExec := func(_ context.Context, _, _ string, env []string) ([]byte, error) {
		envCh <- env
		return nil, nil
	}
	aa := []orders.Order{{
		Name:     "dolt-gc-nudge",
		Trigger:  "cooldown",
		Interval: "1m",
		Exec:     "gc dolt gc-nudge",
	}}
	ad := buildOrderDispatcherFromListExec(aa, store, nil, fakeExec, nil)
	ad.dispatch(context.Background(), cityDir, time.Now())

	got := orderDispatchTestEnv(t, envCh)
	externalRoot := filepath.Join(cityDir, ".gc", "runtime", "packs", "dolt", "external-target")
	checks := map[string]string{
		"GC_DOLT_MANAGED_LOCAL": "0",
		"GC_DOLT_HOST":          "external.example.internal",
		"GC_DOLT_PORT":          "4406",
		"GC_DOLT_DATA_DIR":      externalRoot,
		"GC_DOLT_CONFIG_FILE":   filepath.Join(externalRoot, "dolt-config.yaml"),
		"GC_DOLT_STATE_FILE":    filepath.Join(externalRoot, "dolt-state.json"),
	}
	for key, want := range checks {
		if got[key] != want {
			t.Fatalf("%s = %q, want %q; env=%v", key, got[key], want, got)
		}
	}
}

func TestOrderDispatchExecPropagatesManagedDoltLayout(t *testing.T) {
	store := beads.NewMemStore()
	cityDir := t.TempDir()
	dataDir := filepath.Join(t.TempDir(), "managed-dolt")
	configFile := filepath.Join(cityDir, ".gc", "runtime", "packs", "dolt", "dolt-config.yaml")
	if err := os.MkdirAll(filepath.Join(cityDir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
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
		dataDir,
	))
	t.Setenv("GC_DOLT_DATA_DIR", filepath.Join(t.TempDir(), "poison-dolt-data"))
	t.Setenv("GC_DOLT_CONFIG_FILE", filepath.Join(t.TempDir(), "poison-dolt-config.yaml"))
	t.Setenv("GC_PACK_STATE_DIR", filepath.Join(t.TempDir(), "poison-pack-state"))
	t.Setenv("GC_DOLT_STATE_FILE", filepath.Join(t.TempDir(), "poison-state.json"))

	envCh := make(chan []string, 1)
	fakeExec := func(_ context.Context, _, _ string, env []string) ([]byte, error) {
		envCh <- env
		return nil, nil
	}
	aa := []orders.Order{{
		Name:     "dolt-gc-nudge",
		Trigger:  "cooldown",
		Interval: "1m",
		Exec:     "gc dolt gc-nudge",
	}}
	ad := buildOrderDispatcherFromListExec(aa, store, nil, fakeExec, nil)
	ad.dispatch(context.Background(), cityDir, time.Now())

	got := orderDispatchTestEnv(t, envCh)
	checks := map[string]string{
		"GC_DOLT_MANAGED_LOCAL": "1",
		"GC_DOLT_PORT":          port,
		"GC_DOLT_DATA_DIR":      dataDir,
		"GC_DOLT_CONFIG_FILE":   configFile,
	}
	for key, want := range checks {
		if got[key] != want {
			t.Fatalf("%s = %q, want %q; env=%v", key, got[key], want, got)
		}
	}
}

func TestOrderDispatchExecPropagatesLegacyManagedDoltDataDir(t *testing.T) {
	store := beads.NewMemStore()
	cityDir := t.TempDir()
	dataDir := filepath.Join(cityDir, ".gc", "dolt-data")
	if err := os.MkdirAll(filepath.Join(cityDir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
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
		dataDir,
	))

	envCh := make(chan []string, 1)
	fakeExec := func(_ context.Context, _, _ string, env []string) ([]byte, error) {
		envCh <- env
		return nil, nil
	}
	aa := []orders.Order{{
		Name:     "dolt-gc-nudge",
		Trigger:  "cooldown",
		Interval: "1m",
		Exec:     "gc dolt gc-nudge",
	}}
	ad := buildOrderDispatcherFromListExec(aa, store, nil, fakeExec, nil)
	ad.dispatch(context.Background(), cityDir, time.Now())

	got := orderDispatchTestEnv(t, envCh)
	checks := map[string]string{
		"GC_DOLT_MANAGED_LOCAL": "1",
		"GC_DOLT_PORT":          port,
		"GC_DOLT_DATA_DIR":      dataDir,
	}
	for key, want := range checks {
		if got[key] != want {
			t.Fatalf("%s = %q, want %q; env=%v", key, got[key], want, got)
		}
	}
}

func TestOrderDispatchExecIgnoresPublishedRunningDataDirWithUnreachablePort(t *testing.T) {
	store := beads.NewMemStore()
	cityDir := t.TempDir()
	staleDataDir := filepath.Join(t.TempDir(), "stale-published-dolt")
	defaultDataDir := filepath.Join(cityDir, ".beads", "dolt")
	if err := os.MkdirAll(defaultDataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(staleDataDir, 0o755); err != nil {
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
	port := fmt.Sprint(listener.Addr().(*net.TCPAddr).Port)
	if err := listener.Close(); err != nil {
		t.Fatalf("Close listener: %v", err)
	}
	stateDir := filepath.Join(cityDir, ".gc", "runtime", "packs", "dolt")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(stateDir, "dolt-state.json"), fmt.Sprintf(
		`{"running":true,"pid":%d,"port":%s,"data_dir":%q}`,
		os.Getpid(),
		port,
		staleDataDir,
	))

	envCh := make(chan []string, 1)
	fakeExec := func(_ context.Context, _, _ string, env []string) ([]byte, error) {
		envCh <- env
		return nil, nil
	}
	aa := []orders.Order{{
		Name:     "dolt-gc-nudge",
		Trigger:  "cooldown",
		Interval: "1m",
		Exec:     "gc dolt gc-nudge",
	}}
	ad := buildOrderDispatcherFromListExec(aa, store, nil, fakeExec, nil)
	ad.dispatch(context.Background(), cityDir, time.Now())

	got := orderDispatchTestEnv(t, envCh)
	if got["GC_DOLT_DATA_DIR"] != defaultDataDir {
		t.Fatalf("GC_DOLT_DATA_DIR = %q, want default %q; env=%v", got["GC_DOLT_DATA_DIR"], defaultDataDir, got)
	}
}

func TestOrderExecManagedDoltFallbackSkipsInheritedExternalCity(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "frontend")
	if err := os.MkdirAll(filepath.Join(cityDir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(cityDir, ".beads", "config.yaml"), strings.Join([]string{
		"issue_prefix: ct",
		"gc.endpoint_origin: city_canonical",
		"gc.endpoint_status: verified",
		"dolt.host: external.example.internal",
		"dolt.port: 4406",
		"",
	}, "\n"))
	writeFile(t, filepath.Join(rigDir, ".beads", "config.yaml"), strings.Join([]string{
		"issue_prefix: fe",
		"gc.endpoint_origin: inherited_city",
		"gc.endpoint_status: verified",
		"dolt.host: external.example.internal",
		"dolt.port: 4406",
		"",
	}, "\n"))

	env := map[string]string{
		"GC_DOLT_HOST": "external.example.internal",
		"GC_DOLT_PORT": "4406",
	}
	if applyOrderExecManagedDoltFallback(cityDir, rigDir, env, fmt.Errorf("simulated target error")) {
		t.Fatal("managed fallback applied to inherited external city endpoint")
	}
	if env["GC_DOLT_MANAGED_LOCAL"] == "1" {
		t.Fatalf("GC_DOLT_MANAGED_LOCAL = %q, want not managed-local; env=%v", env["GC_DOLT_MANAGED_LOCAL"], env)
	}
}

func TestOrderDispatchExecTimeout(t *testing.T) {
	store := beads.NewMemStore()
	var rec memRecorder

	fakeExec := func(ctx context.Context, _, _ string, _ []string) ([]byte, error) {
		// Simulate a command that blocks until context is canceled.
		<-ctx.Done()
		return nil, ctx.Err()
	}

	aa := []orders.Order{{
		Name:     "slow-exec",
		Trigger:  "cooldown",
		Interval: "1m",
		Exec:     "scripts/slow.sh",
		Timeout:  "100ms",
	}}
	ad := buildOrderDispatcherFromListExec(aa, store, nil, fakeExec, &rec)

	ad.dispatch(context.Background(), t.TempDir(), time.Now())
	time.Sleep(300 * time.Millisecond)

	// Should have failed due to timeout.
	if !rec.hasType(events.OrderFailed) {
		t.Error("missing order.failed event after timeout")
	}
}

func TestEffectiveTimeout(t *testing.T) {
	tests := []struct {
		name       string
		a          orders.Order
		maxTimeout time.Duration
		want       time.Duration
	}{
		{"exec default", orders.Order{Exec: "x.sh"}, 0, 300 * time.Second},
		{"formula default", orders.Order{Formula: "mol-x"}, 0, 30 * time.Second},
		{"custom timeout", orders.Order{Exec: "x.sh", Timeout: "90s"}, 0, 90 * time.Second},
		{"capped by max", orders.Order{Exec: "x.sh", Timeout: "120s"}, 60 * time.Second, 60 * time.Second},
		{"not capped under max", orders.Order{Exec: "x.sh", Timeout: "30s"}, 60 * time.Second, 30 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := effectiveTimeout(tt.a, tt.maxTimeout)
			if got != tt.want {
				t.Errorf("effectiveTimeout() = %v, want %v", got, tt.want)
			}
		})
	}
}

// --- suspended rig tests ---

func TestOrderDispatchSkipsSuspendedRig(t *testing.T) {
	store := beads.NewMemStore()

	aa := []orders.Order{{
		Name:         "rig-order",
		Trigger:      "cooldown",
		Interval:     "1m",
		Formula:      "test-formula",
		Rig:          "demo",
		FormulaLayer: sharedTestFormulaDir,
	}}
	ad := buildOrderDispatcherFromList(aa, store, nil)
	if ad == nil {
		t.Fatal("expected non-nil dispatcher")
	}

	// Mark the rig as suspended.
	mad := ad.(*memoryOrderDispatcher)
	mad.cfg = &config.City{
		Rigs: []config.Rig{{Name: "demo", Path: "/tmp/demo", Suspended: true}},
	}

	ad.dispatch(context.Background(), t.TempDir(), time.Now())
	time.Sleep(50 * time.Millisecond)

	// No tracking bead should be created for a suspended rig.
	all := trackingBeads(t, store, "order-run:rig-order:rig:demo")
	if len(all) != 0 {
		t.Errorf("expected 0 tracking beads for suspended rig, got %d", len(all))
	}
}

func TestOrderDispatchSkipsSuspendedRigQualifiedPool(t *testing.T) {
	store := beads.NewMemStore()

	// City-level order with a qualified pool targeting a suspended rig.
	aa := []orders.Order{{
		Name:         "city-order",
		Trigger:      "cooldown",
		Interval:     "1m",
		Formula:      "test-formula",
		Pool:         "demo/polecat",
		FormulaLayer: sharedTestFormulaDir,
	}}
	ad := buildOrderDispatcherFromList(aa, store, nil)
	if ad == nil {
		t.Fatal("expected non-nil dispatcher")
	}

	mad := ad.(*memoryOrderDispatcher)
	mad.cfg = &config.City{
		Rigs: []config.Rig{{Name: "demo", Path: "/tmp/demo", Suspended: true}},
	}

	ad.dispatch(context.Background(), t.TempDir(), time.Now())
	time.Sleep(50 * time.Millisecond)

	all := trackingBeads(t, store, "order-run:city-order")
	if len(all) != 0 {
		t.Errorf("expected 0 tracking beads for suspended rig pool, got %d", len(all))
	}
}

func TestOrderDispatchAllowsNonSuspendedRig(t *testing.T) {
	store := beads.NewMemStore()

	aa := []orders.Order{{
		Name:         "rig-order",
		Trigger:      "cooldown",
		Interval:     "1m",
		Formula:      "test-formula",
		Rig:          "demo",
		FormulaLayer: sharedTestFormulaDir,
	}}
	ad := buildOrderDispatcherFromList(aa, store, nil)
	if ad == nil {
		t.Fatal("expected non-nil dispatcher")
	}

	// Rig exists but is NOT suspended.
	mad := ad.(*memoryOrderDispatcher)
	mad.cfg = &config.City{
		Rigs: []config.Rig{{Name: "demo", Path: "/tmp/demo", Suspended: false}},
	}

	ad.dispatch(context.Background(), t.TempDir(), time.Now())
	time.Sleep(50 * time.Millisecond)

	all := trackingBeads(t, store, "order-run:rig-order:rig:demo")
	if len(all) == 0 {
		t.Error("expected tracking bead for non-suspended rig")
	}
}

func TestOrderDispatchSkipsCitySuspended(t *testing.T) {
	store := beads.NewMemStore()

	aa := []orders.Order{{
		Name:         "city-order",
		Trigger:      "cooldown",
		Interval:     "1m",
		Formula:      "test-formula",
		Pool:         "polecat",
		FormulaLayer: sharedTestFormulaDir,
	}}
	ad := buildOrderDispatcherFromList(aa, store, nil)
	if ad == nil {
		t.Fatal("expected non-nil dispatcher")
	}

	// Suspend the entire workspace.
	mad := ad.(*memoryOrderDispatcher)
	mad.cfg = &config.City{
		Workspace: config.Workspace{Suspended: true},
	}

	ad.dispatch(context.Background(), t.TempDir(), time.Now())
	time.Sleep(50 * time.Millisecond)

	all := trackingBeads(t, store, "order-run:city-order")
	if len(all) != 0 {
		t.Errorf("expected 0 tracking beads for suspended city, got %d", len(all))
	}
}

func TestOrderDispatchSkipsSuspendedRigExec(t *testing.T) {
	store := beads.NewMemStore()

	aa := []orders.Order{{
		Name:     "exec-order",
		Trigger:  "cooldown",
		Interval: "1m",
		Exec:     "echo hello",
		Rig:      "demo",
	}}
	ad := buildOrderDispatcherFromList(aa, store, nil)
	if ad == nil {
		t.Fatal("expected non-nil dispatcher")
	}

	mad := ad.(*memoryOrderDispatcher)
	mad.cfg = &config.City{
		Rigs: []config.Rig{{Name: "demo", Path: "/tmp/demo", Suspended: true}},
	}

	ad.dispatch(context.Background(), t.TempDir(), time.Now())
	time.Sleep(50 * time.Millisecond)

	all := trackingBeads(t, store, "order-run:exec-order:rig:demo")
	if len(all) != 0 {
		t.Errorf("expected 0 tracking beads for exec order on suspended rig, got %d", len(all))
	}
}

func TestOrderRigSuspended(t *testing.T) {
	cfg := &config.City{
		Rigs: []config.Rig{
			{Name: "active", Path: "/tmp/active", Suspended: false},
			{Name: "frozen", Path: "/tmp/frozen", Suspended: true},
		},
	}
	m := &memoryOrderDispatcher{cfg: cfg}

	tests := []struct {
		name string
		a    orders.Order
		want bool
	}{
		{"rig-scoped suspended", orders.Order{Rig: "frozen"}, true},
		{"rig-scoped active", orders.Order{Rig: "active"}, false},
		{"rig-scoped unknown", orders.Order{Rig: "unknown"}, false},
		{"qualified pool suspended", orders.Order{Pool: "frozen/polecat"}, true},
		{"qualified pool active", orders.Order{Pool: "active/polecat"}, false},
		{"unqualified pool", orders.Order{Pool: "polecat"}, false},
		{"cross-rig qualified pool", orders.Order{Rig: "active", Pool: "frozen/polecat"}, true},
		{"no rig no pool", orders.Order{}, false},
		{"nil cfg", orders.Order{Rig: "frozen"}, false}, // handled separately
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			target := m
			if tt.name == "nil cfg" {
				target = &memoryOrderDispatcher{}
			}
			if got := target.orderRigSuspended(tt.a); got != tt.want {
				t.Errorf("orderRigSuspended() = %v, want %v", got, tt.want)
			}
		})
	}
}

// --- orphaned tracking bead sweep tests (#520) ---

func TestSweepOrphanedOrderTracking_ClosesOpenTrackingBeads(t *testing.T) {
	store := beads.NewMemStore()

	// Create some open tracking beads (simulating goroutines killed on restart).
	for _, name := range []string{"dolt-health", "gate-sweep", "beads-health"} {
		_, err := store.Create(beads.Bead{
			Title:  "order:" + name,
			Labels: []string{"order-run:" + name, labelOrderTracking},
		})
		if err != nil {
			t.Fatalf("Create(%s): %v", name, err)
		}
	}

	// Create one that's already closed (should be left alone).
	b, err := store.Create(beads.Bead{
		Title:  "order:old-sweep",
		Labels: []string{"order-run:old-sweep", labelOrderTracking},
	})
	if err != nil {
		t.Fatalf("Create(old-sweep): %v", err)
	}
	if err := store.Close(b.ID); err != nil {
		t.Fatalf("Close(old-sweep): %v", err)
	}

	// Create a non-tracking bead that happens to be open (should not be touched).
	_, err = store.Create(beads.Bead{
		Title:  "real work",
		Labels: []string{"order-run:dolt-health"},
	})
	if err != nil {
		t.Fatalf("Create(real work): %v", err)
	}

	closed, err := sweepOrphanedOrderTracking(store)
	if err != nil {
		t.Fatalf("sweepOrphanedOrderTracking: %v", err)
	}
	if closed != 3 {
		t.Fatalf("closed = %d, want 3", closed)
	}

	// Verify the 3 open tracking beads are now closed.
	all := trackingBeads(t, store, labelOrderTracking)
	for _, b := range all {
		if b.Status != "closed" {
			t.Errorf("tracking bead %s (%s) still open", b.ID, b.Title)
		}
	}

	// Verify the non-tracking work bead is still open.
	work, err := store.ListByLabel("order-run:dolt-health", 0)
	if err != nil {
		t.Fatalf("ListByLabel: %v", err)
	}
	for _, b := range work {
		if b.Title == "real work" && b.Status != "open" {
			t.Errorf("non-tracking bead %s should still be open, got %s", b.ID, b.Status)
		}
	}
}

func TestSweepOrphanedOrderTracking_NoOrphans(t *testing.T) {
	store := beads.NewMemStore()

	closed, err := sweepOrphanedOrderTracking(store)
	if err != nil {
		t.Fatalf("sweepOrphanedOrderTracking: %v", err)
	}
	if closed != 0 {
		t.Fatalf("closed = %d, want 0", closed)
	}
}

func TestSweepOrphanedOrderTracking_OnlyClosedBeads(t *testing.T) {
	store := beads.NewMemStore()

	b, err := store.Create(beads.Bead{
		Title:  "order:dolt-health",
		Labels: []string{"order-run:dolt-health", labelOrderTracking},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.Close(b.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}

	closed, err := sweepOrphanedOrderTracking(store)
	if err != nil {
		t.Fatalf("sweepOrphanedOrderTracking: %v", err)
	}
	if closed != 0 {
		t.Fatalf("closed = %d, want 0", closed)
	}
}

func TestStartupSweepThenBuildDispatcher(t *testing.T) {
	store := beads.NewMemStore()

	// Pre-create an orphaned tracking bead (simulating a crashed controller).
	_, err := store.Create(beads.Bead{
		Title:  "order:test-order",
		Labels: []string{"order-run:test-order", labelOrderTracking},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Production startup sequence: sweep first, then build dispatcher.
	// This mirrors newCityRuntime which calls sweepOrphanedOrderTrackingRetry
	// before buildOrderDispatcher. The sweep is intentionally NOT inside
	// buildOrderDispatcher so config reloads don't close in-flight beads.
	closed, err := sweepOrphanedOrderTrackingRetry(store, 3, time.Millisecond)
	if err != nil {
		t.Fatalf("sweepOrphanedOrderTrackingRetry: %v", err)
	}
	if closed != 1 {
		t.Fatalf("closed = %d, want 1", closed)
	}

	aa := []orders.Order{{
		Name:     "test-order",
		Trigger:  "cooldown",
		Interval: "1m",
		Formula:  "test-formula",
	}}
	ad := buildOrderDispatcherFromList(aa, store, nil)
	if ad == nil {
		t.Fatal("expected non-nil dispatcher")
	}

	// The orphaned bead should have been closed before dispatcher construction.
	all := trackingBeads(t, store, labelOrderTracking)
	for _, b := range all {
		if b.Status != "closed" {
			t.Errorf("orphaned tracking bead %s still open after startup sweep", b.ID)
		}
	}
}

func TestSweepOrphanedOrderTracking_RetryOnTransientError(t *testing.T) {
	inner := beads.NewMemStore()
	_, err := inner.Create(beads.Bead{
		Title:  "order:test",
		Labels: []string{"order-run:test", labelOrderTracking},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Fail the first 2 ListByLabel calls, succeed on the 3rd.
	fs := &countFailStore{Store: inner, failCount: 2}
	closed, err := sweepOrphanedOrderTrackingRetry(fs, 3, time.Millisecond)
	if err != nil {
		t.Fatalf("unexpected error after retry: %v", err)
	}
	if closed != 1 {
		t.Fatalf("closed = %d, want 1", closed)
	}
	if fs.calls != 3 {
		t.Fatalf("ListByLabel calls = %d, want 3", fs.calls)
	}
}

func TestSweepOrphanedOrderTracking_RetryExhausted(t *testing.T) {
	inner := beads.NewMemStore()
	_, err := inner.Create(beads.Bead{
		Title:  "order:test",
		Labels: []string{"order-run:test", labelOrderTracking},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Fail all 3 attempts.
	fs := &countFailStore{Store: inner, failCount: 3}
	_, err = sweepOrphanedOrderTrackingRetry(fs, 3, time.Millisecond)
	if err == nil {
		t.Fatal("expected error when retries exhausted")
	}
	if fs.calls != 3 {
		t.Fatalf("ListByLabel calls = %d, want 3", fs.calls)
	}
}

func TestSweepOrphanedOrderTracking_RetryOnPartialClose(t *testing.T) {
	inner := beads.NewMemStore()
	_, err := inner.Create(beads.Bead{
		Title:  "order:test",
		Labels: []string{"order-run:test", labelOrderTracking},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// closeFailStore returns (1, err) from every CloseAll call — simulating
	// a partial close that keeps erroring. The retry loop MUST retry because
	// beads.Store.CloseAll skips already-closed beads, so retrying after a
	// partial close is safe. We verify the total count accumulates across
	// attempts and the final error is wrapped with the attempt count.
	fs := &closeFailStore{Store: inner, closeN: 1}
	n, err := sweepOrphanedOrderTrackingRetry(fs, 3, time.Millisecond)
	if err == nil {
		t.Fatal("expected error from CloseAll failure")
	}
	if !strings.Contains(err.Error(), "after 3 attempts") {
		t.Fatalf("error = %q, want attempt count in message", err.Error())
	}
	// Each of 3 attempts closes 1 bead → total = 3.
	if n != 3 {
		t.Fatalf("n = %d, want 3 (accumulated across retries)", n)
	}
	if fs.listCalls != 3 {
		t.Fatalf("ListByLabel calls = %d, want 3 (retry on partial close)", fs.listCalls)
	}
}

// countFailStore wraps a Store and fails the first N ListByLabel calls.
type countFailStore struct {
	beads.Store
	failCount int
	calls     int
}

func (f *countFailStore) ListByLabel(label string, limit int, opts ...beads.QueryOpt) ([]beads.Bead, error) {
	f.calls++
	if f.calls <= f.failCount {
		return nil, fmt.Errorf("connection refused")
	}
	return f.Store.ListByLabel(label, limit, opts...)
}

// closeFailStore wraps a Store and always fails CloseAll with a
// configurable partial-close count.
type closeFailStore struct {
	beads.Store
	listCalls int
	closeN    int // number of beads "closed" before error
}

func (f *closeFailStore) ListByLabel(label string, limit int, opts ...beads.QueryOpt) ([]beads.Bead, error) {
	f.listCalls++
	return f.Store.ListByLabel(label, limit, opts...)
}

func (f *closeFailStore) CloseAll(_ []string, _ map[string]string) (int, error) {
	return f.closeN, fmt.Errorf("close failed")
}

type labelFailListStore struct {
	beads.Store
	failLabel string
}

func (s labelFailListStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if query.Label == s.failLabel {
		return nil, fmt.Errorf("list failed for %s", query.Label)
	}
	return s.Store.List(query)
}

// --- helpers ---

func successfulExec(context.Context, string, string, []string) ([]byte, error) {
	return nil, nil
}

// buildOrderDispatcherFromList builds a dispatcher from pre-scanned orders,
// bypassing the filesystem scan. Returns nil if no auto-dispatchable orders.
func buildOrderDispatcherFromList(aa []orders.Order, store beads.Store, ep events.Provider) orderDispatcher { //nolint:unparam // ep is nil in current tests but needed for event-gate tests
	return buildOrderDispatcherFromListExec(aa, store, ep, nil, nil)
}

// buildOrderDispatcherFromListExec builds a dispatcher with exec runner support.
func buildOrderDispatcherFromListExec(aa []orders.Order, store beads.Store, ep events.Provider, execRun ExecRunner, rec events.Recorder) orderDispatcher {
	var auto []orders.Order
	cfg := &config.City{}
	seenRigs := make(map[string]bool)
	for _, a := range aa {
		if a.Trigger != "manual" {
			auto = append(auto, a)
		}
		if a.Rig != "" && !seenRigs[a.Rig] {
			cfg.Rigs = append(cfg.Rigs, config.Rig{Name: a.Rig, Path: a.Rig})
			seenRigs[a.Rig] = true
		}
	}
	if len(auto) == 0 {
		return nil
	}
	if rec == nil {
		rec = events.Discard
	}
	if execRun == nil {
		execRun = shellExecRunner
	}
	return &memoryOrderDispatcher{
		aa: auto,
		storeFn: func(_ execStoreTarget) (beads.Store, error) {
			return store, nil
		},
		ep:      ep,
		execRun: execRun,
		rec:     rec,
		stderr:  &bytes.Buffer{},
		cfg:     cfg,
	}
}

func slicesContain(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

func orderDispatchTestEnv(t *testing.T, envCh <-chan []string) map[string]string {
	t.Helper()
	select {
	case entries := <-envCh:
		env := map[string]string{}
		for _, entry := range entries {
			key, value, ok := strings.Cut(entry, "=")
			if ok {
				env[key] = value
			}
		}
		return env
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for order exec env")
	}
	return nil
}

// --- rig-scoped dispatch tests ---

func TestBuildOrderDispatcherWithRigs(t *testing.T) {
	// Build a config with rig formula layers that include orders.
	rigDir := t.TempDir()
	// Create an order in the rig-exclusive layer.
	orderDir := rigDir + "/orders/rig-health"
	if err := mkdirAll(orderDir); err != nil {
		t.Fatal(err)
	}
	writeFile(t, orderDir+"/order.toml", `[order]
formula = "mol-rig-health"
trigger = "cooldown"
interval = "5m"
pool = "polecat"
`)

	cfg := &config.City{
		FormulaLayers: config.FormulaLayers{
			City: []string{"/nonexistent/city-layer"}, // no city orders
			Rigs: map[string][]string{
				"demo": {"/nonexistent/city-layer", rigDir},
			},
		},
	}

	var stderr bytes.Buffer
	ad := buildOrderDispatcher(t.TempDir(), cfg, events.Discard, &stderr)
	if ad == nil {
		t.Fatalf("expected non-nil dispatcher; stderr: %s", stderr.String())
	}

	mad := ad.(*memoryOrderDispatcher)
	if len(mad.aa) != 1 {
		t.Fatalf("got %d orders, want 1", len(mad.aa))
	}
	if mad.aa[0].Rig != "demo" {
		t.Errorf("order Rig = %q, want %q", mad.aa[0].Rig, "demo")
	}
	if mad.aa[0].Name != "rig-health" {
		t.Errorf("order Name = %q, want %q", mad.aa[0].Name, "rig-health")
	}
}

func TestOrderDispatchRigScoped(t *testing.T) {
	store := beads.NewMemStore()

	aa := []orders.Order{{
		Name:         "db-health",
		Trigger:      "cooldown",
		Interval:     "1m",
		Formula:      "mol-db-health",
		Pool:         "polecat",
		Rig:          "demo-repo",
		FormulaLayer: sharedTestFormulaDir,
	}}
	ad := buildOrderDispatcherFromList(aa, store, nil)
	if ad == nil {
		t.Fatal("expected non-nil dispatcher")
	}

	ad.dispatch(context.Background(), t.TempDir(), time.Now())
	time.Sleep(50 * time.Millisecond)

	work := workBeadByOrderLabel(t, store, "order-run:db-health:rig:demo-repo")
	if !slicesContain(work.Labels, "order-run:db-health:rig:demo-repo") {
		t.Errorf("missing scoped order-run label, got %v", work.Labels)
	}
	if work.Metadata["gc.routed_to"] != "demo-repo/polecat" {
		t.Errorf("gc.routed_to = %q, want %q", work.Metadata["gc.routed_to"], "demo-repo/polecat")
	}
}

func TestOrderDispatchRigCooldownIndependent(t *testing.T) {
	store := beads.NewMemStore()

	// Seed a recent run for rig-A's order (scoped name).
	_, err := store.Create(beads.Bead{
		Title:  "order run",
		Labels: []string{"order-run:db-health:rig:rig-a"},
	})
	if err != nil {
		t.Fatal(err)
	}

	aa := []orders.Order{
		{Name: "db-health", Trigger: "cooldown", Interval: "1h", Formula: "mol-db-health", Rig: "rig-a"},
		{Name: "db-health", Trigger: "cooldown", Interval: "1h", Formula: "mol-db-health", Rig: "rig-b"},
	}
	ad := buildOrderDispatcherFromList(aa, store, nil)
	if ad == nil {
		t.Fatal("expected non-nil dispatcher")
	}

	ad.dispatch(context.Background(), t.TempDir(), time.Now())
	time.Sleep(50 * time.Millisecond)

	// rig-b should have a tracking bead, rig-a should not.
	all := trackingBeads(t, store, "order-run:db-health:rig:rig-b")
	rigBTracked := false
	rigATracked := false
	for _, b := range all {
		for _, l := range b.Labels {
			if l == "order-run:db-health:rig:rig-b" {
				rigBTracked = true
			}
			// Check that no NEW bead was created for rig-a (only the seed).
			// The seed bead is the only one with rig-a label.
		}
	}
	if !rigBTracked {
		t.Error("missing tracking bead for rig-b")
	}

	// Count rig-a beads — should be exactly 1 (the seed).
	rigAAll := trackingBeads(t, store, "order-run:db-health:rig:rig-a")
	rigACount := 0
	for _, b := range rigAAll {
		for _, l := range b.Labels {
			if l == "order-run:db-health:rig:rig-a" {
				rigACount++
			}
		}
	}
	if rigACount != 1 {
		t.Errorf("rig-a bead count = %d, want 1 (seed only)", rigACount)
	}
	_ = rigATracked
}

func TestRigExclusiveLayers(t *testing.T) {
	city := []string{"/city/topo", "/city/local"}
	rig := []string{"/city/topo", "/city/local", "/rig/topo", "/rig/local"}

	got := rigExclusiveLayers(rig, city)
	if len(got) != 2 {
		t.Fatalf("got %d layers, want 2", len(got))
	}
	if got[0] != "/rig/topo" || got[1] != "/rig/local" {
		t.Errorf("got %v, want [/rig/topo /rig/local]", got)
	}
}

func TestRigExclusiveLayersNoCityPrefix(t *testing.T) {
	// Rig shorter than city → no exclusive layers.
	got := rigExclusiveLayers([]string{"/x"}, []string{"/a", "/b"})
	if got != nil {
		t.Errorf("got %v, want nil", got)
	}
}

func TestQualifyPool(t *testing.T) {
	tests := []struct {
		pool, rig, want string
	}{
		{"polecat", "demo-repo", "demo-repo/polecat"},
		{"demo-repo/polecat", "demo-repo", "demo-repo/polecat"}, // already qualified
		{"dog", "", "dog"}, // city order
	}
	for _, tt := range tests {
		got := qualifyPool(tt.pool, tt.rig)
		if got != tt.want {
			t.Errorf("qualifyPool(%q, %q) = %q, want %q", tt.pool, tt.rig, got, tt.want)
		}
	}
}

// --- city pack layer tests ---

func TestBuildOrderDispatcherUsesProviderAwareFileStore(t *testing.T) {
	t.Setenv("GC_BEADS", "file")

	cityDir := t.TempDir()
	layerDir := filepath.Join(cityDir, "formulas")
	orderDir := filepath.Join(layerDir, "orders", "file-order")
	if err := mkdirAll(orderDir); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(orderDir, "order.toml"), `[order]
formula = "test-formula"
trigger = "cooldown"
interval = "1m"
pool = "worker"
`)
	formulaText, err := os.ReadFile(filepath.Join(sharedTestFormulaDir, "test-formula.formula.toml"))
	if err != nil {
		t.Fatalf("ReadFile(test-formula): %v", err)
	}
	writeFile(t, filepath.Join(layerDir, "test-formula.formula.toml"), string(formulaText))

	cfg := &config.City{
		FormulaLayers: config.FormulaLayers{
			City: []string{layerDir},
		},
	}

	var stderr bytes.Buffer
	ad := buildOrderDispatcher(cityDir, cfg, events.Discard, &stderr)
	if ad == nil {
		t.Fatalf("expected non-nil dispatcher; stderr: %s", stderr.String())
	}

	ad.dispatch(context.Background(), cityDir, time.Now())
	time.Sleep(100 * time.Millisecond)

	store, err := openStoreAtForCity(cityDir, cityDir)
	if err != nil {
		t.Fatalf("openStoreAtForCity: %v", err)
	}
	work := workBeadByOrderLabel(t, store, "order-run:file-order")
	if work.Metadata["gc.routed_to"] != "worker" {
		t.Errorf("gc.routed_to = %q, want %q", work.Metadata["gc.routed_to"], "worker")
	}
}

func TestBuildOrderDispatcherRigOrderUsesRigFileStore(t *testing.T) {
	t.Setenv("GC_BEADS", "file")

	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "frontend")
	cityLayer := filepath.Join(cityDir, "formulas")
	rigLayer := filepath.Join(rigDir, "formulas")
	orderDir := filepath.Join(rigDir, "orders", "rig-digest")
	for _, dir := range []string{cityLayer, rigLayer, orderDir} {
		if err := mkdirAll(dir); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(orderDir, "order.toml"), `[order]
formula = "test-formula"
trigger = "cooldown"
interval = "1m"
pool = "worker"
`)
	formulaText, err := os.ReadFile(filepath.Join(sharedTestFormulaDir, "test-formula.formula.toml"))
	if err != nil {
		t.Fatalf("ReadFile(test-formula): %v", err)
	}
	writeFile(t, filepath.Join(rigLayer, "test-formula.formula.toml"), string(formulaText))
	if err := ensureScopedFileStoreLayout(cityDir); err != nil {
		t.Fatal(err)
	}
	if err := ensurePersistedScopeLocalFileStore(cityDir); err != nil {
		t.Fatal(err)
	}
	if err := ensurePersistedScopeLocalFileStore(rigDir); err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{
		Workspace: config.Workspace{Name: "demo", Prefix: "ct"},
		FormulaLayers: config.FormulaLayers{
			City: []string{cityLayer},
			Rigs: map[string][]string{
				"frontend": {cityLayer, rigLayer},
			},
		},
		Rigs: []config.Rig{{
			Name:   "frontend",
			Path:   "frontend",
			Prefix: "fe",
		}},
	}

	var stderr bytes.Buffer
	ad := buildOrderDispatcher(cityDir, cfg, events.Discard, &stderr)
	if ad == nil {
		t.Fatalf("expected non-nil dispatcher; stderr: %s", stderr.String())
	}

	ad.dispatch(context.Background(), cityDir, time.Now())
	time.Sleep(100 * time.Millisecond)

	cityStore, err := openStoreAtForCity(cityDir, cityDir)
	if err != nil {
		t.Fatalf("openStoreAtForCity(city): %v", err)
	}
	cityRuns := trackingBeads(t, cityStore, "order-run:rig-digest:rig:frontend")
	if len(cityRuns) != 0 {
		t.Fatalf("city store has %d rig order bead(s), want 0", len(cityRuns))
	}

	rigStore, err := openStoreAtForCity(rigDir, cityDir)
	if err != nil {
		t.Fatalf("openStoreAtForCity(rig): %v", err)
	}
	work := workBeadByOrderLabel(t, rigStore, "order-run:rig-digest:rig:frontend")
	if work.Metadata["gc.routed_to"] != "frontend/worker" {
		t.Errorf("gc.routed_to = %q, want %q", work.Metadata["gc.routed_to"], "frontend/worker")
	}
}

func TestBuildOrderDispatcherRigOrderHonorsLegacyCityRunHistory(t *testing.T) {
	t.Setenv("GC_BEADS", "file")

	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "frontend")
	cityLayer := filepath.Join(cityDir, "formulas")
	rigLayer := filepath.Join(rigDir, "formulas")
	orderDir := filepath.Join(rigDir, "orders", "rig-digest")
	for _, dir := range []string{cityLayer, rigLayer, orderDir} {
		if err := mkdirAll(dir); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(orderDir, "order.toml"), `[order]
formula = "test-formula"
trigger = "cooldown"
interval = "24h"
pool = "worker"
`)
	formulaText, err := os.ReadFile(filepath.Join(sharedTestFormulaDir, "test-formula.formula.toml"))
	if err != nil {
		t.Fatalf("ReadFile(test-formula): %v", err)
	}
	writeFile(t, filepath.Join(rigLayer, "test-formula.formula.toml"), string(formulaText))
	if err := ensureScopedFileStoreLayout(cityDir); err != nil {
		t.Fatal(err)
	}
	if err := ensurePersistedScopeLocalFileStore(cityDir); err != nil {
		t.Fatal(err)
	}
	if err := ensurePersistedScopeLocalFileStore(rigDir); err != nil {
		t.Fatal(err)
	}

	cityStore, err := openStoreAtForCity(cityDir, cityDir)
	if err != nil {
		t.Fatalf("openStoreAtForCity(city): %v", err)
	}
	if _, err := cityStore.Create(beads.Bead{
		Title:  "legacy rig digest run",
		Labels: []string{"order-run:rig-digest:rig:frontend"},
	}); err != nil {
		t.Fatalf("Create(legacy city run): %v", err)
	}

	cfg := &config.City{
		Workspace: config.Workspace{Name: "demo", Prefix: "ct"},
		FormulaLayers: config.FormulaLayers{
			City: []string{cityLayer},
			Rigs: map[string][]string{
				"frontend": {cityLayer, rigLayer},
			},
		},
		Rigs: []config.Rig{{
			Name:   "frontend",
			Path:   "frontend",
			Prefix: "fe",
		}},
	}

	var stderr bytes.Buffer
	ad := buildOrderDispatcher(cityDir, cfg, events.Discard, &stderr)
	if ad == nil {
		t.Fatalf("expected non-nil dispatcher; stderr: %s", stderr.String())
	}

	ad.dispatch(context.Background(), cityDir, time.Now())
	time.Sleep(100 * time.Millisecond)

	rigStore, err := openStoreAtForCity(rigDir, cityDir)
	if err != nil {
		t.Fatalf("openStoreAtForCity(rig): %v", err)
	}
	rigRuns := trackingBeads(t, rigStore, "order-run:rig-digest:rig:frontend")
	if len(rigRuns) != 0 {
		t.Fatalf("rig store has %d new run bead(s), want 0 because legacy city run is still inside cooldown", len(rigRuns))
	}
}

func TestOrderDispatchSkipsRigOrderWhenLegacyCityFallbackUnavailable(t *testing.T) {
	rigStore := beads.NewMemStore()
	stderr := &bytes.Buffer{}
	m := &memoryOrderDispatcher{
		aa: []orders.Order{{
			Name:         "rig-digest",
			Rig:          "frontend",
			Trigger:      "cooldown",
			Interval:     "1m",
			Formula:      "test-formula",
			Pool:         "worker",
			FormulaLayer: sharedTestFormulaDir,
		}},
		storeFn: func(target execStoreTarget) (beads.Store, error) {
			if target.ScopeKind == "city" {
				return nil, fmt.Errorf("legacy city store unavailable")
			}
			return rigStore, nil
		},
		execRun:    shellExecRunner,
		rec:        events.Discard,
		stderr:     stderr,
		maxTimeout: time.Minute,
		cfg: &config.City{
			Rigs: []config.Rig{{
				Name: "frontend",
				Path: "frontend",
			}},
		},
	}

	m.dispatch(context.Background(), t.TempDir(), time.Now())
	time.Sleep(50 * time.Millisecond)

	rigRuns := trackingBeads(t, rigStore, "order-run:rig-digest:rig:frontend")
	if len(rigRuns) != 0 {
		t.Fatalf("rig store has %d new run bead(s), want 0 when legacy city fallback cannot be checked", len(rigRuns))
	}
	if !strings.Contains(stderr.String(), "legacy city store") {
		t.Fatalf("stderr missing legacy fallback error:\n%s", stderr.String())
	}
}

func TestOrderDispatchSkipsRigEventWhenLegacyCursorReadFails(t *testing.T) {
	rigStore := beads.NewMemStore()
	legacyStore := labelFailListStore{
		Store:     beads.NewMemStore(),
		failLabel: "order:release-watch:rig:frontend",
	}
	eventLog := events.NewFake()
	eventLog.Record(events.Event{Type: events.BeadClosed, Actor: "test"})

	stderr := &bytes.Buffer{}
	m := &memoryOrderDispatcher{
		aa: []orders.Order{{
			Name:    "release-watch",
			Rig:     "frontend",
			Trigger: "event",
			On:      events.BeadClosed,
			Exec:    "true",
			Pool:    "worker",
			Timeout: "1m",
		}},
		storeFn: func(target execStoreTarget) (beads.Store, error) {
			if target.ScopeKind == "city" {
				return legacyStore, nil
			}
			return rigStore, nil
		},
		ep:      eventLog,
		execRun: successfulExec,
		rec:     events.Discard,
		stderr:  stderr,
		cfg: &config.City{
			Rigs: []config.Rig{{
				Name: "frontend",
				Path: "frontend",
			}},
		},
	}

	m.dispatch(context.Background(), t.TempDir(), time.Now())
	time.Sleep(50 * time.Millisecond)

	rigRuns := trackingBeads(t, rigStore, "order-run:release-watch:rig:frontend")
	if len(rigRuns) != 0 {
		t.Fatalf("rig store has %d new run bead(s), want 0 when legacy event cursor cannot be read", len(rigRuns))
	}
	if !strings.Contains(stderr.String(), "event cursor") {
		t.Fatalf("stderr missing event cursor error:\n%s", stderr.String())
	}
}

func TestOrderDispatchSkipsRigConditionWhenLegacyOpenWorkReadFails(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "frontend")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	rigStore := beads.NewMemStore()
	legacyStore := labelFailListStore{
		Store:     beads.NewMemStore(),
		failLabel: "order-run:rig-digest:rig:frontend",
	}

	stderr := &bytes.Buffer{}
	m := &memoryOrderDispatcher{
		aa: []orders.Order{{
			Name:    "rig-digest",
			Rig:     "frontend",
			Trigger: "condition",
			Check:   "true",
			Exec:    "true",
			Pool:    "worker",
			Timeout: "1m",
		}},
		storeFn: func(target execStoreTarget) (beads.Store, error) {
			if target.ScopeKind == "city" {
				return legacyStore, nil
			}
			return rigStore, nil
		},
		execRun: successfulExec,
		rec:     events.Discard,
		stderr:  stderr,
		cfg: &config.City{
			Rigs: []config.Rig{{
				Name: "frontend",
				Path: rigDir,
			}},
		},
	}

	m.dispatch(context.Background(), cityDir, time.Now())
	time.Sleep(50 * time.Millisecond)

	rigRuns := trackingBeads(t, rigStore, "order-run:rig-digest:rig:frontend")
	if len(rigRuns) != 0 {
		t.Fatalf("rig store has %d new run bead(s), want 0 when legacy open-work state cannot be read", len(rigRuns))
	}
	if !strings.Contains(stderr.String(), "open work") {
		t.Fatalf("stderr missing open-work error:\n%s", stderr.String())
	}
}

func TestOrderDispatchConditionUsesScopedEnv(t *testing.T) {
	cityDir := t.TempDir()
	store := beads.NewMemStore()
	check := fmt.Sprintf(
		`test "$GC_CITY_PATH" = '%s' && test "$GC_STORE_ROOT" = '%s' && test "$GC_STORE_SCOPE" = city && test "$(pwd)" = '%s'`,
		cityDir,
		cityDir,
		cityDir,
	)
	ran := make(chan struct{}, 1)
	fakeExec := func(_ context.Context, _, _ string, _ []string) ([]byte, error) {
		ran <- struct{}{}
		return nil, nil
	}
	aa := []orders.Order{{
		Name:    "scoped-check",
		Trigger: "condition",
		Check:   check,
		Exec:    "true",
	}}
	ad := buildOrderDispatcherFromListExec(aa, store, nil, fakeExec, nil)

	ad.dispatch(context.Background(), cityDir, time.Now())

	select {
	case <-ran:
	case <-time.After(2 * time.Second):
		t.Fatal("condition order did not dispatch with scoped cwd/env")
	}
}

func TestOrderDispatchSkipsRigCooldownWhenLegacyLastRunReadFails(t *testing.T) {
	rigStore := beads.NewMemStore()
	legacyStore := labelFailListStore{
		Store:     beads.NewMemStore(),
		failLabel: "order-run:rig-digest:rig:frontend",
	}

	stderr := &bytes.Buffer{}
	m := &memoryOrderDispatcher{
		aa: []orders.Order{{
			Name:         "rig-digest",
			Rig:          "frontend",
			Trigger:      "cooldown",
			Interval:     "1m",
			Formula:      "test-formula",
			Pool:         "worker",
			FormulaLayer: sharedTestFormulaDir,
		}},
		storeFn: func(target execStoreTarget) (beads.Store, error) {
			if target.ScopeKind == "city" {
				return legacyStore, nil
			}
			return rigStore, nil
		},
		execRun:    shellExecRunner,
		rec:        events.Discard,
		stderr:     stderr,
		maxTimeout: time.Minute,
		cfg: &config.City{
			Rigs: []config.Rig{{
				Name: "frontend",
				Path: "frontend",
			}},
		},
	}

	m.dispatch(context.Background(), t.TempDir(), time.Now())
	time.Sleep(50 * time.Millisecond)

	rigRuns := trackingBeads(t, rigStore, "order-run:rig-digest:rig:frontend")
	if len(rigRuns) != 0 {
		t.Fatalf("rig store has %d new run bead(s), want 0 when legacy last-run state cannot be read", len(rigRuns))
	}
	if !strings.Contains(stderr.String(), "last run") {
		t.Fatalf("stderr missing last-run error:\n%s", stderr.String())
	}
}

func TestBuildOrderDispatcherReopensStoreForScopedFileReads(t *testing.T) {
	t.Setenv("GC_BEADS", "file")

	cityDir := t.TempDir()
	layerDir := filepath.Join(cityDir, "formulas")
	orderDir := filepath.Join(layerDir, "orders", "file-order")
	if err := mkdirAll(orderDir); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(orderDir, "order.toml"), `[order]
formula = "test-formula"
trigger = "cooldown"
interval = "1m"
pool = "worker"
`)
	formulaText, err := os.ReadFile(filepath.Join(sharedTestFormulaDir, "test-formula.formula.toml"))
	if err != nil {
		t.Fatalf("ReadFile(test-formula): %v", err)
	}
	writeFile(t, filepath.Join(layerDir, "test-formula.formula.toml"), string(formulaText))

	cfg := &config.City{
		FormulaLayers: config.FormulaLayers{
			City: []string{layerDir},
		},
	}

	ad := buildOrderDispatcher(cityDir, cfg, events.Discard, &bytes.Buffer{})
	if ad == nil {
		t.Fatal("expected non-nil dispatcher")
	}

	store, err := openStoreAtForCity(cityDir, cityDir)
	if err != nil {
		t.Fatalf("openStoreAtForCity: %v", err)
	}
	if _, err := store.Create(beads.Bead{
		Title:  "existing work",
		Labels: []string{"order-run:file-order"},
	}); err != nil {
		t.Fatalf("Create(existing work): %v", err)
	}

	ad.dispatch(context.Background(), cityDir, time.Now())
	time.Sleep(100 * time.Millisecond)

	results := trackingBeads(t, store, "order-run:file-order")
	tracking := 0
	for _, b := range results {
		if b.Title == "order:file-order" {
			tracking++
		}
	}
	if tracking != 0 {
		t.Fatalf("dispatcher created %d tracking bead(s) despite existing open work", tracking)
	}
}

func TestBuildOrderDispatcherCityPackLayers(t *testing.T) {
	// Simulate system formulas + pack formulas as two city layers.
	sysDir := t.TempDir()
	topoDir := t.TempDir()

	// System dir: beads-health order.
	sysAutoDir := sysDir + "/orders/beads-health"
	if err := mkdirAll(sysAutoDir); err != nil {
		t.Fatal(err)
	}
	writeFile(t, sysAutoDir+"/order.toml", `[order]
exec = "scripts/beads-health.sh"
trigger = "cooldown"
interval = "30s"
`)

	// Pack dir: wasteland-poll order.
	topoAutoDir := topoDir + "/orders/wasteland-poll"
	if err := mkdirAll(topoAutoDir); err != nil {
		t.Fatal(err)
	}
	writeFile(t, topoAutoDir+"/order.toml", `[order]
exec = "scripts/wasteland-poll.sh"
trigger = "cooldown"
interval = "2m"
`)

	cfg := &config.City{
		FormulaLayers: config.FormulaLayers{
			City: []string{sysDir, topoDir},
		},
	}

	var stderr bytes.Buffer
	ad := buildOrderDispatcher(t.TempDir(), cfg, events.Discard, &stderr)
	if ad == nil {
		t.Fatalf("expected non-nil dispatcher; stderr: %s", stderr.String())
	}

	mad := ad.(*memoryOrderDispatcher)
	if len(mad.aa) != 2 {
		t.Fatalf("got %d orders, want 2; stderr: %s", len(mad.aa), stderr.String())
	}

	names := map[string]bool{}
	for _, a := range mad.aa {
		names[a.Name] = true
	}
	if !names["beads-health"] {
		t.Error("missing beads-health order")
	}
	if !names["wasteland-poll"] {
		t.Error("missing wasteland-poll order")
	}
}

func TestBuildOrderDispatcherCityPackWithOverride(t *testing.T) {
	// Same two-layer setup, plus a config override on wasteland-poll interval.
	sysDir := t.TempDir()
	topoDir := t.TempDir()

	sysAutoDir := sysDir + "/orders/beads-health"
	if err := mkdirAll(sysAutoDir); err != nil {
		t.Fatal(err)
	}
	writeFile(t, sysAutoDir+"/order.toml", `[order]
exec = "scripts/beads-health.sh"
trigger = "cooldown"
interval = "30s"
`)

	topoAutoDir := topoDir + "/orders/wasteland-poll"
	if err := mkdirAll(topoAutoDir); err != nil {
		t.Fatal(err)
	}
	writeFile(t, topoAutoDir+"/order.toml", `[order]
exec = "scripts/wasteland-poll.sh"
trigger = "cooldown"
interval = "2m"
`)

	tenSec := "10s"
	cfg := &config.City{
		FormulaLayers: config.FormulaLayers{
			City: []string{sysDir, topoDir},
		},
		Orders: config.OrdersConfig{
			Overrides: []config.OrderOverride{
				{Name: "wasteland-poll", Interval: &tenSec},
			},
		},
	}

	var stderr bytes.Buffer
	ad := buildOrderDispatcher(t.TempDir(), cfg, events.Discard, &stderr)
	if ad == nil {
		t.Fatalf("expected non-nil dispatcher; stderr: %s", stderr.String())
	}

	mad := ad.(*memoryOrderDispatcher)
	if len(mad.aa) != 2 {
		t.Fatalf("got %d orders, want 2", len(mad.aa))
	}

	// Verify wasteland-poll interval was overridden to 10s.
	for _, a := range mad.aa {
		if a.Name == "wasteland-poll" {
			if a.Interval != "10s" {
				t.Errorf("wasteland-poll interval = %q, want %q", a.Interval, "10s")
			}
			return
		}
	}
	t.Error("wasteland-poll not found in dispatcher orders")
}

func TestBuildOrderDispatcherOverrideNotFoundNonFatal(t *testing.T) {
	// Single formula layer with beads-health only.
	// Override targets wasteland-poll (nonexistent).
	// Verify beads-health is still dispatched and stderr contains warning.
	sysDir := t.TempDir()

	sysAutoDir := sysDir + "/orders/beads-health"
	if err := mkdirAll(sysAutoDir); err != nil {
		t.Fatal(err)
	}
	writeFile(t, sysAutoDir+"/order.toml", `[order]
exec = "scripts/beads-health.sh"
trigger = "cooldown"
interval = "30s"
`)

	tenSec := "10s"
	cfg := &config.City{
		FormulaLayers: config.FormulaLayers{
			City: []string{sysDir},
		},
		Orders: config.OrdersConfig{
			Overrides: []config.OrderOverride{
				{Name: "wasteland-poll", Interval: &tenSec},
			},
		},
	}

	var stderr bytes.Buffer
	ad := buildOrderDispatcher(t.TempDir(), cfg, events.Discard, &stderr)
	if ad == nil {
		t.Fatalf("expected non-nil dispatcher (beads-health should still be found); stderr: %s", stderr.String())
	}

	mad := ad.(*memoryOrderDispatcher)
	if len(mad.aa) != 1 {
		t.Fatalf("got %d orders, want 1", len(mad.aa))
	}
	if mad.aa[0].Name != "beads-health" {
		t.Errorf("order name = %q, want %q", mad.aa[0].Name, "beads-health")
	}

	// Verify stderr contains the "not found" warning from ApplyOverrides.
	if !strings.Contains(stderr.String(), "not found") {
		t.Errorf("expected stderr to contain 'not found' warning, got: %s", stderr.String())
	}
}

// --- helpers ---

func mkdirAll(path string) error {
	return os.MkdirAll(path, 0o755)
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// memRecorder records events in memory for test assertions.
type memRecorder struct {
	events []events.Event
}

func (r *memRecorder) Record(e events.Event) {
	r.events = append(r.events, e)
}

func (r *memRecorder) hasType(typ string) bool {
	for _, e := range r.events {
		if e.Type == typ {
			return true
		}
	}
	return false
}

func (r *memRecorder) hasSubject(subject string) bool {
	for _, e := range r.events {
		if e.Subject == subject {
			return true
		}
	}
	return false
}

// --- dedup / tracking bead lifecycle tests ---

func TestOrderDispatchClosesTrackingBead(t *testing.T) {
	store := beads.NewMemStore()
	var rec memRecorder

	fakeExec := func(_ context.Context, _, _ string, _ []string) ([]byte, error) {
		return []byte("ok\n"), nil
	}

	aa := []orders.Order{{
		Name:     "health-check",
		Trigger:  "cooldown",
		Interval: "1m",
		Exec:     "scripts/health.sh",
	}}
	ad := buildOrderDispatcherFromListExec(aa, store, nil, fakeExec, &rec)

	ad.dispatch(context.Background(), t.TempDir(), time.Now())
	time.Sleep(100 * time.Millisecond)

	// Tracking bead should be closed after dispatch completes.
	all := trackingBeads(t, store, "order-run:health-check")
	for _, b := range all {
		for _, l := range b.Labels {
			if l == "order-run:health-check" {
				if b.Status != "closed" {
					t.Errorf("tracking bead status = %q, want %q", b.Status, "closed")
				}
				return
			}
		}
	}
	t.Error("tracking bead not found")
}

func TestOrderDispatchSkipsOpenWork(t *testing.T) {
	store := beads.NewMemStore()

	// Seed an open wisp (non-tracking bead) for this order.
	_, err := store.Create(beads.Bead{
		Title:  "mol-do-work", // not "order:my-auto" → counts as real work
		Labels: []string{"order-run:my-auto"},
	})
	if err != nil {
		t.Fatal(err)
	}

	ran := false
	fakeExec := func(_ context.Context, _, _ string, _ []string) ([]byte, error) {
		ran = true
		return nil, nil
	}

	aa := []orders.Order{{
		Name:     "my-auto",
		Trigger:  "cooldown",
		Interval: "1s", // short cooldown — would fire if not deduped
		Exec:     "scripts/run.sh",
	}}
	ad := buildOrderDispatcherFromListExec(aa, store, nil, fakeExec, nil)

	ad.dispatch(context.Background(), t.TempDir(), time.Now())
	time.Sleep(50 * time.Millisecond)

	if ran {
		t.Error("exec should not have run — open work exists")
	}

	// No new beads should have been created (only the seed).
	all, _ := store.ListOpen()
	if len(all) != 1 {
		t.Errorf("expected 1 bead (seed only), got %d", len(all))
	}
}

func TestOrderDispatchSkipsOpenTrackingBeadForConditionOrder(t *testing.T) {
	store := beads.NewMemStore()

	_, err := store.Create(beads.Bead{
		Title:  "order:my-auto",
		Labels: []string{"order-run:my-auto", labelOrderTracking},
	})
	if err != nil {
		t.Fatal(err)
	}

	ran := false
	fakeExec := func(_ context.Context, _, _ string, _ []string) ([]byte, error) {
		ran = true
		return nil, nil
	}

	aa := []orders.Order{{
		Name:    "my-auto",
		Trigger: "condition",
		Check:   "true",
		Exec:    "scripts/run.sh",
	}}
	ad := buildOrderDispatcherFromListExec(aa, store, nil, fakeExec, nil)

	ad.dispatch(context.Background(), t.TempDir(), time.Now())
	time.Sleep(50 * time.Millisecond)

	if ran {
		t.Error("exec should not have run while an order-tracking bead is open")
	}
}

func TestOrderDispatchFiresAfterWorkClosed(t *testing.T) {
	store := beads.NewMemStore()

	// Seed a CLOSED wisp — should not block new dispatch.
	b, err := store.Create(beads.Bead{
		Title:  "mol-do-work",
		Labels: []string{"order-run:my-auto"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(b.ID); err != nil {
		t.Fatal(err)
	}

	ran := false
	fakeExec := func(_ context.Context, _, _ string, _ []string) ([]byte, error) {
		ran = true
		return nil, nil
	}

	aa := []orders.Order{{
		Name:     "my-auto",
		Trigger:  "cooldown",
		Interval: "1s",
		Exec:     "scripts/run.sh",
	}}
	ad := buildOrderDispatcherFromListExec(aa, store, nil, fakeExec, nil)

	// Use a future "now" so cooldown trigger sees the seed bead as old enough.
	ad.dispatch(context.Background(), t.TempDir(), time.Now().Add(5*time.Second))
	time.Sleep(100 * time.Millisecond)

	if !ran {
		t.Error("exec should have run — all previous work is closed")
	}
}

// Unused but keep for future event assertion tests.
var (
	_ = (*memRecorder).hasSubject
	_ = strings.Contains
)

func TestResolveOrderExecTarget_UnboundRigErrors(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test"},
		Rigs: []config.Rig{{
			Name: "frontend",
			Path: "", // unbound — no site binding
		}},
	}
	_, err := resolveOrderExecTarget("/city", cfg, orders.Order{Name: "deploy", Rig: "frontend"})
	if err == nil {
		t.Fatal("resolveOrderExecTarget: expected error for unbound rig, got nil")
	}
	if !strings.Contains(err.Error(), "frontend") {
		t.Errorf("error = %q, want mention of rig name 'frontend'", err)
	}
	if !strings.Contains(err.Error(), "no path binding") {
		t.Errorf("error = %q, want mention of 'no path binding'", err)
	}
}

func TestResolveOrderExecTarget_BoundRigDispatchesNormally(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test"},
		Rigs: []config.Rig{{
			Name: "frontend",
			Path: "/home/user/frontend",
		}},
	}
	target, err := resolveOrderExecTarget("/city", cfg, orders.Order{Name: "deploy", Rig: "frontend"})
	if err != nil {
		t.Fatalf("resolveOrderExecTarget: unexpected error: %v", err)
	}
	if target.ScopeKind != "rig" {
		t.Errorf("ScopeKind = %q, want %q", target.ScopeKind, "rig")
	}
	if target.RigName != "frontend" {
		t.Errorf("RigName = %q, want %q", target.RigName, "frontend")
	}
	if target.ScopeRoot != "/home/user/frontend" {
		t.Errorf("ScopeRoot = %q, want %q", target.ScopeRoot, "/home/user/frontend")
	}
}
