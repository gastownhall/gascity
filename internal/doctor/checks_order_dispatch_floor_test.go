package doctor

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/orders"
)

// Floor arithmetic used throughout: floor = ceil(ring/budget) * patrol_interval.
// With the default budget (4) and a 30s patrol, a ring of 9 gives ceil(9/4)=3
// passes = 1m30s.

func orderDispatchFloorCity(t *testing.T, patrol string, budget int) (string, *config.City) {
	t.Helper()
	cityPath, cfg := orderFiringTestCity(t)
	cfg.Daemon.PatrolInterval = patrol
	if budget > 0 {
		cfg.Orders.MaxDispatchesPerTick = &budget
	}
	return cityPath, cfg
}

// writeOrderDispatchFloorFillers writes n long-interval cooldown orders: they
// occupy the ring without ever being tight themselves.
func writeOrderDispatchFloorFillers(t *testing.T, orderDir, prefix string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		writeOrderFiringTestOrderInDir(t, orderDir, fmt.Sprintf("%s-%02d", prefix, i), "cooldown", "1h")
	}
}

func runOrderDispatchFloorTest(t *testing.T, cfg *config.City, cityPath string, opts ...OrderDispatchFloorOption) *CheckResult {
	t.Helper()
	return NewOrderDispatchFloorCheck(cfg, cityPath, opts...).Run(&CheckContext{CityPath: cityPath})
}

func TestOrderDispatchFloor_FlagsIntervalBelowFloor(t *testing.T) {
	cityPath, cfg := orderDispatchFloorCity(t, "30s", 0)
	ordersDir := filepath.Join(cityPath, "orders")
	writeOrderFiringTestOrder(t, cityPath, "tight", "cooldown", "30s")
	writeOrderDispatchFloorFillers(t, ordersDir, "ring", 8)

	result := runOrderDispatchFloorTest(t, cfg, cityPath)

	// Floor 1m30s is 3x the 30s interval. A config observation over an
	// upper-bound estimate is a warning however far under, and never gates.
	if result.Status != StatusWarning {
		t.Fatalf("status = %v, want warning; msg = %s; details = %v", result.Status, result.Message, result.Details)
	}
	if result.Severity != SeverityAdvisory {
		t.Fatalf("Severity = %v, want SeverityAdvisory", result.Severity)
	}
	if result.Name != "order-dispatch-floor" {
		t.Fatalf("Name = %q, want order-dispatch-floor", result.Name)
	}
	details := strings.Join(result.Details, "\n")
	if !strings.Contains(details, "tight") {
		t.Fatalf("details = %v, want the tight order named", result.Details)
	}
	if strings.Contains(details, "ring-") {
		t.Fatalf("details = %v, 1h orders are above the floor and must not be flagged", result.Details)
	}
}

func TestOrderDispatchFloor_IntervalAtOrAboveFloorIsNotFlagged(t *testing.T) {
	cityPath, cfg := orderDispatchFloorCity(t, "30s", 0)
	ordersDir := filepath.Join(cityPath, "orders")
	writeOrderFiringTestOrder(t, cityPath, "exactly-at-floor", "cooldown", "90s")
	writeOrderFiringTestOrder(t, cityPath, "above-floor", "cooldown", "2m")
	writeOrderDispatchFloorFillers(t, ordersDir, "ring", 7)

	result := runOrderDispatchFloorTest(t, cfg, cityPath)

	if result.Status != StatusOK {
		t.Fatalf("status = %v, want OK; msg = %s; details = %v", result.Status, result.Message, result.Details)
	}
}

func TestOrderDispatchFloor_FlagsCronBelowFloor(t *testing.T) {
	cityPath, cfg := orderDispatchFloorCity(t, "30s", 0)
	writeOrderFiringTestOrder(t, cityPath, "every-minute", "cron", "* * * * *")
	writeOrderDispatchFloorFillers(t, filepath.Join(cityPath, "orders"), "ring", 8)

	result := runOrderDispatchFloorTest(t, cfg, cityPath)

	if result.Status == StatusOK {
		t.Fatalf("status = OK, want a flag for a 1m cron under a 1m30s floor; msg = %s", result.Message)
	}
	if !strings.Contains(strings.Join(result.Details, "\n"), "every-minute") {
		t.Fatalf("details = %v, want the cron order named", result.Details)
	}
}

func TestOrderDispatchFloor_HonorsConfiguredBudget(t *testing.T) {
	// Budget 9 over a ring of 9 is one pass: the floor drops to the 30s patrol.
	cityPath, cfg := orderDispatchFloorCity(t, "30s", 9)
	writeOrderFiringTestOrder(t, cityPath, "tight", "cooldown", "30s")
	writeOrderDispatchFloorFillers(t, filepath.Join(cityPath, "orders"), "ring", 8)

	result := runOrderDispatchFloorTest(t, cfg, cityPath)

	if result.Status != StatusOK {
		t.Fatalf("status = %v, want OK once orders.max_dispatches_per_tick covers the ring; msg = %s; details = %v", result.Status, result.Message, result.Details)
	}
}

func TestOrderDispatchFloor_UsesConfiguredPatrolInterval(t *testing.T) {
	// 10s patrol: ring 9 / budget 4 = 3 passes = 30s floor.
	cityPath, cfg := orderDispatchFloorCity(t, "10s", 0)
	writeOrderFiringTestOrder(t, cityPath, "below", "cooldown", "20s")
	writeOrderFiringTestOrder(t, cityPath, "at-floor", "cooldown", "30s")
	writeOrderDispatchFloorFillers(t, filepath.Join(cityPath, "orders"), "ring", 7)

	result := runOrderDispatchFloorTest(t, cfg, cityPath)

	details := strings.Join(result.Details, "\n")
	if !strings.Contains(details, "below") || strings.Contains(details, "at-floor") {
		t.Fatalf("details = %v, want only the 20s order flagged under a 30s floor", result.Details)
	}
}

func TestOrderDispatchFloor_RingCountsOnlyBudgetedTriggers(t *testing.T) {
	// Manual orders are never auto-dispatched and due condition orders are
	// outside the per-pass budget, so neither occupies the ring. Counted, they
	// would lift the floor from 30s to 2m and flag the 40s order.
	cityPath, cfg := orderDispatchFloorCity(t, "30s", 0)
	ordersDir := filepath.Join(cityPath, "orders")
	writeOrderFiringTestOrder(t, cityPath, "tight", "cooldown", "40s")
	writeOrderDispatchFloorFillers(t, ordersDir, "ring", 3)
	for i := 0; i < 5; i++ {
		writeOrderFiringTestOrderInDir(t, ordersDir, fmt.Sprintf("manual-%d", i), "manual", "")
		writeOrderFiringTestOrderInDir(t, ordersDir, fmt.Sprintf("cond-%d", i), "condition", "")
	}

	result := runOrderDispatchFloorTest(t, cfg, cityPath)

	if result.Status != StatusOK {
		t.Fatalf("status = %v, want OK; msg = %s; details = %v", result.Status, result.Message, result.Details)
	}
}

func TestOrderDispatchFloor_EventOrdersOccupyTheRing(t *testing.T) {
	for _, tc := range []struct {
		name    string
		events  int
		flagged bool
	}{
		{"ring of 4 leaves a 30s floor", 3, false},
		{"ring of 5 lifts the floor to 1m", 4, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cityPath, cfg := orderDispatchFloorCity(t, "30s", 0)
			ordersDir := filepath.Join(cityPath, "orders")
			writeOrderFiringTestOrder(t, cityPath, "tight", "cooldown", "40s")
			for i := 0; i < tc.events; i++ {
				writeOrderFiringTestOrderInDir(t, ordersDir, fmt.Sprintf("on-close-%d", i), "event", "bead.closed")
			}

			result := runOrderDispatchFloorTest(t, cfg, cityPath)

			if got := result.Status != StatusOK; got != tc.flagged {
				t.Fatalf("flagged = %v, want %v; msg = %s; details = %v", got, tc.flagged, result.Message, result.Details)
			}
		})
	}
}

func TestOrderDispatchFloor_SkipsSuspendedRigOrders(t *testing.T) {
	// The dispatcher skips suspended rigs before they reach the budget, so
	// their orders neither occupy the ring nor get flagged.
	cityPath, cfg := orderDispatchFloorCity(t, "30s", 0)
	writeOrderFiringTestOrder(t, cityPath, "tight", "cooldown", "40s")
	writeOrderDispatchFloorFillers(t, filepath.Join(cityPath, "orders"), "ring", 3)

	rigPath := filepath.Join(cityPath, "rigs", "parked")
	rigOrders := filepath.Join(rigPath, "orders")
	if err := os.MkdirAll(rigOrders, 0o755); err != nil {
		t.Fatalf("creating rig orders dir: %v", err)
	}
	cfg.Rigs = []config.Rig{{Name: "parked", Path: rigPath, Suspended: true}}
	cfg.FormulaLayers.Rigs = map[string][]string{"parked": {cfg.FormulaLayers.City[0], filepath.Join(rigPath, "formulas")}}
	writeOrderDispatchFloorFillers(t, rigOrders, "parked", 5)
	writeOrderFiringTestOrderInDir(t, rigOrders, "parked-tight", "cooldown", "10s")

	result := runOrderDispatchFloorTest(t, cfg, cityPath)

	if result.Status != StatusOK {
		t.Fatalf("status = %v, want OK for a suspended rig; msg = %s; details = %v", result.Status, result.Message, result.Details)
	}
	if strings.Contains(strings.Join(result.Details, "\n"), "parked") {
		t.Fatalf("details = %v, suspended rig orders must be skipped", result.Details)
	}
}

func TestOrderDispatchFloor_InapplicableOrdersDoNotOccupyTheRing(t *testing.T) {
	cityPath, cfg := orderDispatchFloorCity(t, "30s", 0)
	ordersDir := filepath.Join(cityPath, "orders")
	writeOrderFiringTestOrder(t, cityPath, "tight", "cooldown", "40s")
	writeOrderDispatchFloorFillers(t, ordersDir, "ring", 3)
	writeOrderDispatchFloorFillers(t, ordersDir, "http-only", 5)

	result := runOrderDispatchFloorTest(t, cfg, cityPath,
		WithOrderDispatchFloorInapplicableFunc(func(o orders.Order) (string, bool) {
			return "http", strings.HasPrefix(o.Name, "http-only")
		}))

	if result.Status != StatusOK {
		t.Fatalf("status = %v, want OK; msg = %s; details = %v", result.Status, result.Message, result.Details)
	}
}

func TestOrderDispatchFloor_NothingToCheckIsOK(t *testing.T) {
	cityPath, cfg := orderDispatchFloorCity(t, "30s", 0)
	writeOrderFiringTestOrder(t, cityPath, "manual-only", "manual", "")

	if result := runOrderDispatchFloorTest(t, cfg, cityPath); result.Status != StatusOK {
		t.Fatalf("status = %v, want OK with no budgeted orders; msg = %s", result.Status, result.Message)
	}
	if result := runOrderDispatchFloorTest(t, nil, cityPath); result.Status != StatusOK {
		t.Fatalf("status = %v, want OK with no city config; msg = %s", result.Status, result.Message)
	}
}

func TestOrderDispatchFloor_NeverReadsAsAFixableCheck(t *testing.T) {
	check := NewOrderDispatchFloorCheck(nil, "")
	if check.CanFix() {
		t.Fatal("CanFix = true; the floor is an operator decision, not a repair")
	}
	if err := check.Fix(&CheckContext{}); err != nil {
		t.Fatalf("Fix = %v, want nil no-op", err)
	}
}

// gc doctor prints Details only under --verbose, so the always-printed line
// and hint must carry what the operator needs: how many orders, the floor,
// and the lever.
func TestOrderDispatchFloor_SurfacesInNonVerboseDoctorOutput(t *testing.T) {
	cityPath, cfg := orderDispatchFloorCity(t, "30s", 0)
	writeOrderFiringTestOrder(t, cityPath, "tight", "cooldown", "30s")
	writeOrderDispatchFloorFillers(t, filepath.Join(cityPath, "orders"), "ring", 8)

	result := runOrderDispatchFloorTest(t, cfg, cityPath)
	var out bytes.Buffer
	printResult(&out, result, false)

	got := out.String()
	for _, want := range []string{"order-dispatch-floor", "1m30s", "orders.max_dispatches_per_tick", "minimum", "Worst case"} {
		if !strings.Contains(got, want) {
			t.Errorf("non-verbose output missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "ring-00") {
		t.Errorf("non-verbose output leaked per-order details:\n%s", got)
	}
}

// An interval shorter than one patrol pass cannot be fit by any budget, so the
// hint points at the patrol interval instead.
func TestOrderDispatchFloor_HintNamesPatrolWhenIntervalBelowOnePass(t *testing.T) {
	cityPath, cfg := orderDispatchFloorCity(t, "30s", 0)
	writeOrderFiringTestOrder(t, cityPath, "sub-pass", "cooldown", "10s")
	writeOrderDispatchFloorFillers(t, filepath.Join(cityPath, "orders"), "ring", 8)

	result := runOrderDispatchFloorTest(t, cfg, cityPath)

	if result.Status != StatusWarning {
		t.Fatalf("status = %v, want warning; msg = %s; details = %v", result.Status, result.Message, result.Details)
	}
	if !strings.Contains(result.FixHint, "daemon.patrol_interval") {
		t.Fatalf("FixHint = %q, want it to name daemon.patrol_interval", result.FixHint)
	}
}
