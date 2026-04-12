package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
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

func TestOrderDispatcherNil(t *testing.T) {
	ad := buildOrderDispatcher(t.TempDir(), &config.City{}, noopRunner, events.Discard, &bytes.Buffer{})
	if ad != nil {
		t.Error("expected nil dispatcher for empty orders")
	}
}

func TestBuildOrderDispatcherNoOrders(t *testing.T) {
	// City with formula layers that exist but contain no orders.
	dir := t.TempDir()
	cfg := &config.City{}
	ad := buildOrderDispatcher(dir, cfg, noopRunner, events.Discard, &bytes.Buffer{})
	if ad != nil {
		t.Error("expected nil dispatcher when no orders exist")
	}
}

func TestOrderDispatchManualFiltered(t *testing.T) {
	ad := buildOrderDispatcherFromList(
		[]orders.Order{{Name: "manual-only", Gate: "manual", Formula: "noop"}},
		beads.NewMemStore(), nil, noopRunner,
	)
	if ad != nil {
		t.Error("expected nil dispatcher — manual orders should be filtered out")
	}
}

func TestOrderDispatchCooldownDue(t *testing.T) {
	store := beads.NewMemStore()
	var labelArgs []string
	runner := func(_, name string, args ...string) ([]byte, error) {
		if name == "bd" && len(args) > 0 && args[0] == "update" {
			labelArgs = args
		}
		return []byte("ok\n"), nil
	}

	aa := []orders.Order{{
		Name:         "test-order",
		Gate:         "cooldown",
		Interval:     "1m",
		Formula:      "test-formula",
		Pool:         "worker",
		FormulaLayer: sharedTestFormulaDir,
	}}
	ad := buildOrderDispatcherFromList(aa, store, nil, runner)
	if ad == nil {
		t.Fatal("expected non-nil dispatcher")
	}

	ad.dispatch(context.Background(), t.TempDir(), time.Now())

	deadline := time.Now().Add(2 * time.Second)
	for {
		// Verify tracking bead was created.
		all := trackingBeads(t, store, "order-run:test-order")
		foundTracking := false
		for _, b := range all {
			for _, l := range b.Labels {
				if l == "order-run:test-order" {
					foundTracking = true
					break
				}
			}
			if foundTracking {
				break
			}
		}

		// Verify wisp was stamped with routed_to metadata.
		foundRoute := false
		for _, a := range labelArgs {
			if a == "gc.routed_to=worker" {
				foundRoute = true
				break
			}
		}
		if foundTracking && foundRoute {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for dispatch labels; tracking=%v labelArgs=%v", foundTracking, labelArgs)
		}
		time.Sleep(10 * time.Millisecond)
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
		Gate:     "cooldown",
		Interval: "1h", // 1 hour — far in the future
		Formula:  "test-formula",
	}}
	ad := buildOrderDispatcherFromList(aa, store, nil, noopRunner)
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
		{Name: "order-a", Gate: "cooldown", Interval: "1m", Formula: "formula-a"},
		{Name: "order-b", Gate: "cooldown", Interval: "1h", Formula: "formula-b"},
	}
	ad := buildOrderDispatcherFromList(aa, store, nil, noopRunner)
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
		Gate:     "cooldown",
		Interval: "2m",
		Exec:     "$ORDER_DIR/scripts/poll.sh",
		Source:   "/city/formulas/orders/wasteland-poll/order.toml",
	}}
	ad := buildOrderDispatcherFromListExec(aa, store, nil, noopRunner, fakeExec, &rec)
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

	fakeExec := func(_ context.Context, _, _ string, _ []string) ([]byte, error) {
		return []byte("error output\n"), fmt.Errorf("exit status 1")
	}

	aa := []orders.Order{{
		Name:     "fail-exec",
		Gate:     "cooldown",
		Interval: "2m",
		Exec:     "scripts/fail.sh",
	}}
	ad := buildOrderDispatcherFromListExec(aa, store, nil, noopRunner, fakeExec, &rec)
	mad := ad.(*memoryOrderDispatcher)
	mad.stderr = &stderr

	ad.dispatch(context.Background(), t.TempDir(), time.Now())
	time.Sleep(100 * time.Millisecond)

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
}

func TestOrderDispatchFormulaCookFailureLabelsTrackingBead(t *testing.T) {
	store := beads.NewMemStore()
	var rec memRecorder

	aa := []orders.Order{{
		Name:         "fail-formula",
		Gate:         "cooldown",
		Interval:     "2m",
		Formula:      "missing-formula",
		FormulaLayer: sharedTestFormulaDir,
	}}
	ad := buildOrderDispatcherFromList(aa, store, nil, noopRunner)
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

func TestOrderDispatchFormulaLabelFailureLabelsTrackingBead(t *testing.T) {
	store := beads.NewMemStore()
	var rec memRecorder
	var stderr bytes.Buffer

	runner := func(_ string, name string, args ...string) ([]byte, error) {
		if name == "bd" && len(args) > 0 && args[0] == "update" {
			return nil, fmt.Errorf("label failed")
		}
		return []byte("ok\n"), nil
	}

	aa := []orders.Order{{
		Name:         "fail-label",
		Gate:         "cooldown",
		Interval:     "2m",
		Formula:      "test-formula",
		FormulaLayer: sharedTestFormulaDir,
	}}
	ad := buildOrderDispatcherFromList(aa, store, nil, runner)
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
		Gate:     "cooldown",
		Interval: "1h",
		Exec:     "scripts/poll.sh",
	}}
	ad := buildOrderDispatcherFromListExec(aa, store, nil, noopRunner, fakeExec, nil)

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
		Gate:     "cooldown",
		Interval: "1m",
		Exec:     "$ORDER_DIR/scripts/poll.sh",
		Source:   "/city/formulas/orders/poll/order.toml",
	}}
	ad := buildOrderDispatcherFromListExec(aa, store, nil, noopRunner, fakeExec, nil)

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
		Gate:         "cooldown",
		Interval:     "1m",
		Exec:         "$PACK_DIR/scripts/gate-sweep.sh",
		Source:       "/city/packs/maintenance/formulas/orders/gate-sweep/order.toml",
		FormulaLayer: "/city/packs/maintenance/formulas",
	}}
	ad := buildOrderDispatcherFromListExec(aa, store, nil, noopRunner, fakeExec, nil)

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
		Gate:     "cooldown",
		Interval: "1m",
		Exec:     "scripts/test.sh",
		Source:   "/city/formulas/orders/no-layer/order.toml",
		// FormulaLayer intentionally empty.
	}}
	ad := buildOrderDispatcherFromListExec(aa, store, nil, noopRunner, fakeExec, nil)

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
		Gate:     "cooldown",
		Interval: "1m",
		Exec:     "scripts/slow.sh",
		Timeout:  "100ms",
	}}
	ad := buildOrderDispatcherFromListExec(aa, store, nil, noopRunner, fakeExec, &rec)

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
		Gate:         "cooldown",
		Interval:     "1m",
		Formula:      "test-formula",
		Rig:          "demo",
		FormulaLayer: sharedTestFormulaDir,
	}}
	ad := buildOrderDispatcherFromList(aa, store, nil, noopRunner)
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
		Gate:         "cooldown",
		Interval:     "1m",
		Formula:      "test-formula",
		Pool:         "demo/polecat",
		FormulaLayer: sharedTestFormulaDir,
	}}
	ad := buildOrderDispatcherFromList(aa, store, nil, noopRunner)
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
		Gate:         "cooldown",
		Interval:     "1m",
		Formula:      "test-formula",
		Rig:          "demo",
		FormulaLayer: sharedTestFormulaDir,
	}}
	ad := buildOrderDispatcherFromList(aa, store, nil, noopRunner)
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
		Gate:         "cooldown",
		Interval:     "1m",
		Formula:      "test-formula",
		Pool:         "polecat",
		FormulaLayer: sharedTestFormulaDir,
	}}
	ad := buildOrderDispatcherFromList(aa, store, nil, noopRunner)
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
		Gate:     "cooldown",
		Interval: "1m",
		Exec:     "echo hello",
		Rig:      "demo",
	}}
	ad := buildOrderDispatcherFromList(aa, store, nil, noopRunner)
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
	// This mirrors newCityRuntime which calls sweepOrphanedOrderTracking
	// before buildOrderDispatcher. The sweep is intentionally NOT inside
	// buildOrderDispatcher so config reloads don't close in-flight beads.
	closed, err := sweepOrphanedOrderTracking(store)
	if err != nil {
		t.Fatalf("sweepOrphanedOrderTracking: %v", err)
	}
	if closed != 1 {
		t.Fatalf("closed = %d, want 1", closed)
	}

	aa := []orders.Order{{
		Name:     "test-order",
		Gate:     "cooldown",
		Interval: "1m",
		Formula:  "test-formula",
	}}
	ad := buildOrderDispatcherFromList(aa, store, nil, noopRunner)
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

// --- helpers ---

// noopRunner is a CommandRunner that always succeeds.
var noopRunner beads.CommandRunner = func(_, _ string, _ ...string) ([]byte, error) {
	return []byte("ok\n"), nil
}

// buildOrderDispatcherFromList builds a dispatcher from pre-scanned orders,
// bypassing the filesystem scan. Returns nil if no auto-dispatchable orders.
func buildOrderDispatcherFromList(aa []orders.Order, store beads.Store, ep events.Provider, runner beads.CommandRunner) orderDispatcher { //nolint:unparam // ep is nil in current tests but needed for event-gate tests
	return buildOrderDispatcherFromListExec(aa, store, ep, runner, nil, nil)
}

// buildOrderDispatcherFromListExec builds a dispatcher with exec runner support.
func buildOrderDispatcherFromListExec(aa []orders.Order, store beads.Store, ep events.Provider, runner beads.CommandRunner, execRun ExecRunner, rec events.Recorder) orderDispatcher {
	var auto []orders.Order
	for _, a := range aa {
		if a.Gate != "manual" {
			auto = append(auto, a)
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
		aa:      auto,
		store:   store,
		ep:      ep,
		runner:  runner,
		execRun: execRun,
		rec:     rec,
		stderr:  &bytes.Buffer{},
	}
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
gate = "cooldown"
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
	ad := buildOrderDispatcher(t.TempDir(), cfg, noopRunner, events.Discard, &stderr)
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
	var labelArgs []string
	runner := func(_, name string, args ...string) ([]byte, error) {
		if name == "bd" && len(args) > 0 && args[0] == "update" {
			labelArgs = args
		}
		return []byte("ok\n"), nil
	}

	aa := []orders.Order{{
		Name:         "db-health",
		Gate:         "cooldown",
		Interval:     "1m",
		Formula:      "mol-db-health",
		Pool:         "polecat",
		Rig:          "demo-repo",
		FormulaLayer: sharedTestFormulaDir,
	}}
	ad := buildOrderDispatcherFromList(aa, store, nil, runner)
	if ad == nil {
		t.Fatal("expected non-nil dispatcher")
	}

	ad.dispatch(context.Background(), t.TempDir(), time.Now())
	time.Sleep(50 * time.Millisecond)

	found := map[string]bool{}
	for _, a := range labelArgs {
		found[a] = true
	}
	// Scoped label.
	if !found["--add-label=order-run:db-health:rig:demo-repo"] {
		t.Errorf("missing scoped order-run label, got %v", labelArgs)
	}
	// Auto-qualified routed_to target.
	if !found["gc.routed_to=demo-repo/polecat"] {
		t.Errorf("missing qualified routed_to metadata, got %v", labelArgs)
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
		{Name: "db-health", Gate: "cooldown", Interval: "1h", Formula: "mol-db-health", Rig: "rig-a"},
		{Name: "db-health", Gate: "cooldown", Interval: "1h", Formula: "mol-db-health", Rig: "rig-b"},
	}
	ad := buildOrderDispatcherFromList(aa, store, nil, noopRunner)
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
gate = "cooldown"
interval = "30s"
`)

	// Pack dir: wasteland-poll order.
	topoAutoDir := topoDir + "/orders/wasteland-poll"
	if err := mkdirAll(topoAutoDir); err != nil {
		t.Fatal(err)
	}
	writeFile(t, topoAutoDir+"/order.toml", `[order]
exec = "scripts/wasteland-poll.sh"
gate = "cooldown"
interval = "2m"
`)

	cfg := &config.City{
		FormulaLayers: config.FormulaLayers{
			City: []string{sysDir, topoDir},
		},
	}

	var stderr bytes.Buffer
	ad := buildOrderDispatcher(t.TempDir(), cfg, noopRunner, events.Discard, &stderr)
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
gate = "cooldown"
interval = "30s"
`)

	topoAutoDir := topoDir + "/orders/wasteland-poll"
	if err := mkdirAll(topoAutoDir); err != nil {
		t.Fatal(err)
	}
	writeFile(t, topoAutoDir+"/order.toml", `[order]
exec = "scripts/wasteland-poll.sh"
gate = "cooldown"
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
	ad := buildOrderDispatcher(t.TempDir(), cfg, noopRunner, events.Discard, &stderr)
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
gate = "cooldown"
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
	ad := buildOrderDispatcher(t.TempDir(), cfg, noopRunner, events.Discard, &stderr)
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
		Gate:     "cooldown",
		Interval: "1m",
		Exec:     "scripts/health.sh",
	}}
	ad := buildOrderDispatcherFromListExec(aa, store, nil, noopRunner, fakeExec, &rec)

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
		Gate:     "cooldown",
		Interval: "1s", // short cooldown — would fire if not deduped
		Exec:     "scripts/run.sh",
	}}
	ad := buildOrderDispatcherFromListExec(aa, store, nil, noopRunner, fakeExec, nil)

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
		Gate:     "cooldown",
		Interval: "1s",
		Exec:     "scripts/run.sh",
	}}
	ad := buildOrderDispatcherFromListExec(aa, store, nil, noopRunner, fakeExec, nil)

	// Use a future "now" so cooldown gate sees the seed bead as old enough.
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
