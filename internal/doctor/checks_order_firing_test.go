package doctor

import (
	"fmt"
	"io"
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

func TestOrderFiringCurrent_NeverFired_BeyondUptime(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	cityPath, cfg := orderFiringTestCity(t)
	writeOrderFiringTestOrder(t, cityPath, "mol-dog-stale-db", "cron", "0 */4 * * *")
	writeOrderFiringTestEvents(t, cityPath, events.Event{
		Type: events.ControllerStarted,
		Ts:   now.Add(-8 * time.Hour),
	})

	result := runOrderFiringCurrentTest(t, cfg, cityPath, now)
	if result.Status != StatusError {
		t.Fatalf("status = %v, want error; msg = %s; details = %v", result.Status, result.Message, result.Details)
	}
	// The "never fired beyond uptime" path is advisory — it most often
	// reflects the cron-scheduler bug (ga-97qngx) rather than a real
	// outage, so it must not wedge dispatch gates that read BlockingFailed.
	if result.Severity != SeverityAdvisory {
		t.Fatalf("Severity = %v, want SeverityAdvisory for never-fired-beyond-uptime path", result.Severity)
	}
	if !strings.Contains(strings.Join(result.Details, "\n"), "never fired since controller start") {
		t.Fatalf("details = %v, want never-fired controller-start message", result.Details)
	}
	if result.FixHint != "Inspect with: gc order check && gc order history mol-dog-stale-db" {
		t.Fatalf("FixHint = %q, want inspect hint for order", result.FixHint)
	}
}

func TestOrderFiringCurrent_Stale_StaysBlocking(t *testing.T) {
	// Cooldown stale (CRITICAL) must remain blocking even though the
	// sibling "never fired" path was demoted to advisory; the stale
	// signal reflects a real execution gap consumers should gate on.
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	cityPath, cfg := orderFiringTestCity(t)
	writeOrderFiringTestOrder(t, cityPath, "cleanup-cooldown", "cooldown", "1h")
	writeOrderFiringTestEvents(t, cityPath,
		events.Event{Type: events.ControllerStarted, Ts: now.Add(-24 * time.Hour)},
		events.Event{Type: events.OrderFired, Subject: "cleanup-cooldown", Ts: now.Add(-6 * time.Hour)},
	)

	result := runOrderFiringCurrentTest(t, cfg, cityPath, now)
	if result.Status != StatusError {
		t.Fatalf("status = %v, want error; msg = %s; details = %v", result.Status, result.Message, result.Details)
	}
	if result.Severity != SeverityBlocking {
		t.Fatalf("Severity = %v, want SeverityBlocking for cooldown stale", result.Severity)
	}
}

func TestOrderFiringCurrent_MixedAdvisoryAndBlocking_AggregatesBlocking(t *testing.T) {
	// One advisory (never-fired cron) + one blocking (cooldown stale) →
	// aggregate severity must be Blocking so the presence of any real
	// outage keeps gates closed even if other entries are merely advisory.
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	cityPath, cfg := orderFiringTestCity(t)
	writeOrderFiringTestOrder(t, cityPath, "mol-dog-stale-db", "cron", "0 */4 * * *")
	writeOrderFiringTestOrder(t, cityPath, "cleanup-cooldown", "cooldown", "1h")
	writeOrderFiringTestEvents(t, cityPath,
		events.Event{Type: events.ControllerStarted, Ts: now.Add(-24 * time.Hour)},
		events.Event{Type: events.OrderFired, Subject: "cleanup-cooldown", Ts: now.Add(-6 * time.Hour)},
	)

	result := runOrderFiringCurrentTest(t, cfg, cityPath, now)
	if result.Status != StatusError {
		t.Fatalf("status = %v, want error; msg = %s; details = %v", result.Status, result.Message, result.Details)
	}
	if result.Severity != SeverityBlocking {
		t.Fatalf("Severity = %v, want SeverityBlocking when any non-OK entry is blocking", result.Severity)
	}
}

func TestOrderFiringCurrent_MixedAdvisoryAndWarning_AggregatesAdvisory(t *testing.T) {
	// A warning-level overdue order should stay visible in details without
	// converting an advisory error into a blocking gate failure.
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	cityPath, cfg := orderFiringTestCity(t)
	writeOrderFiringTestOrder(t, cityPath, "mol-dog-stale-db", "cron", "0 */4 * * *")
	writeOrderFiringTestOrder(t, cityPath, "cleanup-cooldown", "cooldown", "1h")
	writeOrderFiringTestEvents(t, cityPath,
		events.Event{Type: events.ControllerStarted, Ts: now.Add(-24 * time.Hour)},
		events.Event{Type: events.OrderFired, Subject: "cleanup-cooldown", Ts: now.Add(-2 * time.Hour)},
	)

	result := runOrderFiringCurrentTest(t, cfg, cityPath, now)
	if result.Status != StatusError {
		t.Fatalf("status = %v, want error; msg = %s; details = %v", result.Status, result.Message, result.Details)
	}
	if result.Severity != SeverityAdvisory {
		t.Fatalf("Severity = %v, want SeverityAdvisory when only error entries are advisory", result.Severity)
	}
}

func TestOrderFiringCurrent_NeverFiredCooldown_StaysBlocking(t *testing.T) {
	// Never-fired cooldown orders represent the same execution gap as stale
	// cooldown orders and should continue to gate dispatch consumers.
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	cityPath, cfg := orderFiringTestCity(t)
	writeOrderFiringTestOrder(t, cityPath, "cleanup-cooldown", "cooldown", "1h")
	writeOrderFiringTestEvents(t, cityPath, events.Event{
		Type: events.ControllerStarted,
		Ts:   now.Add(-2 * time.Hour),
	})

	result := runOrderFiringCurrentTest(t, cfg, cityPath, now)
	if result.Status != StatusError {
		t.Fatalf("status = %v, want error; msg = %s; details = %v", result.Status, result.Message, result.Details)
	}
	if result.Severity != SeverityBlocking {
		t.Fatalf("Severity = %v, want SeverityBlocking for never-fired cooldown", result.Severity)
	}
}

func TestOrderFiringCurrent_NeverFired_WithinFirstCycle(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	cityPath, cfg := orderFiringTestCity(t)
	writeOrderFiringTestOrder(t, cityPath, "mol-dog-stale-db", "cron", "0 */4 * * *")
	writeOrderFiringTestEvents(t, cityPath, events.Event{
		Type: events.ControllerStarted,
		Ts:   now.Add(-30 * time.Minute),
	})

	result := runOrderFiringCurrentTest(t, cfg, cityPath, now)
	if result.Status != StatusOK {
		t.Fatalf("status = %v, want OK; msg = %s; details = %v", result.Status, result.Message, result.Details)
	}
	if !strings.Contains(strings.Join(result.Details, "\n"), "within first cycle") {
		t.Fatalf("details = %v, want within-first-cycle message", result.Details)
	}
}

func TestOrderFiringCurrent_FiredRecently(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	cityPath, cfg := orderFiringTestCity(t)
	writeOrderFiringTestOrder(t, cityPath, "mol-dog-stale-db", "cron", "0 */4 * * *")
	writeOrderFiringTestOrder(t, cityPath, "cleanup-cooldown", "cooldown", "4h")
	writeOrderFiringTestEvents(t, cityPath,
		events.Event{Type: events.ControllerStarted, Ts: now.Add(-8 * time.Hour)},
		events.Event{Type: events.OrderFired, Subject: "mol-dog-stale-db", Ts: now.Add(-1 * time.Hour)},
		events.Event{Type: events.OrderFired, Subject: "cleanup-cooldown", Ts: now.Add(-1 * time.Hour)},
	)

	result := runOrderFiringCurrentTest(t, cfg, cityPath, now)
	if result.Status != StatusOK {
		t.Fatalf("status = %v, want OK; msg = %s; details = %v", result.Status, result.Message, result.Details)
	}
	if !strings.Contains(strings.Join(result.Details, "\n"), "last fired 1h ago, expected every 4h") {
		t.Fatalf("details = %v, want recent-fire detail", result.Details)
	}
}

func TestOrderFiringCurrent_UsesNewestOrderRunHistory(t *testing.T) {
	// Manual `gc order run` creates order-run beads even when no controller
	// order.fired event is emitted. Doctor must therefore merge bead history
	// with event history and select the newest execution, not a stale event.
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	cityPath, cfg := orderFiringTestCity(t)
	writeOrderFiringTestOrder(t, cityPath, "mol-dog-stale-db", "cron", "0 */4 * * *")
	writeOrderFiringTestEvents(t, cityPath,
		events.Event{Type: events.ControllerStarted, Ts: now.Add(-24 * time.Hour)},
		events.Event{Type: events.OrderFired, Subject: "mol-dog-stale-db", Ts: now.Add(-13 * time.Hour)},
	)
	store := beads.NewMemStoreFrom(2, []beads.Bead{
		{
			ID:        "old-run",
			Title:     "old mol-dog-stale-db",
			Status:    "closed",
			Type:      "molecule",
			Labels:    []string{"order-run:mol-dog-stale-db"},
			CreatedAt: now.Add(-13 * time.Hour),
		},
		{
			ID:        "new-run",
			Title:     "new mol-dog-stale-db",
			Status:    "closed",
			Type:      "molecule",
			Labels:    []string{"order-run:mol-dog-stale-db"},
			CreatedAt: now.Add(-1 * time.Hour),
		},
	}, nil)

	check := NewOrderFiringCurrentCheck(cfg, cityPath, WithOrderFiringCurrentLastRunFunc(func(order orders.Order) (time.Time, error) {
		return orders.NewStoreWithGraph(beads.OrdersStore{Store: store}, beads.GraphStore{Store: store}).LastRun(order.ScopedName())
	}))
	check.clock = func() time.Time { return now }
	result := check.Run(&CheckContext{CityPath: cityPath})

	if result.Status != StatusOK {
		t.Fatalf("status = %v, want OK; msg = %s; details = %v", result.Status, result.Message, result.Details)
	}
	if !strings.Contains(strings.Join(result.Details, "\n"), "last fired 1h ago, expected every 4h") {
		t.Fatalf("details = %v, want newest order-run bead to win over stale event", result.Details)
	}
}

func TestOrderFiringCurrent_SkipsSuspendedRigOrders(t *testing.T) {
	// The dispatcher intentionally skips suspended rigs. Doctor should not turn
	// their paused recurring orders into blocking stale-order failures.
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	cityPath, cfg := orderFiringTestCity(t)
	rigPath := filepath.Join(cityPath, "rigs", "parked")
	rigFormulas := filepath.Join(rigPath, "formulas")
	rigOrders := filepath.Join(rigPath, "orders")
	if err := os.MkdirAll(rigOrders, 0o755); err != nil {
		t.Fatalf("creating rig orders dir: %v", err)
	}
	cfg.Rigs = []config.Rig{{Name: "parked", Path: rigPath, Suspended: true}}
	cfg.FormulaLayers.Rigs = map[string][]string{"parked": {cfg.FormulaLayers.City[0], rigFormulas}}
	writeOrderFiringTestOrderInDir(t, rigOrders, "gate-sweep", "cooldown", "1m")
	writeOrderFiringTestEvents(t, cityPath,
		events.Event{Type: events.ControllerStarted, Ts: now.Add(-24 * time.Hour)},
		events.Event{Type: events.OrderFired, Subject: "gate-sweep:rig:parked", Ts: now.Add(-24 * time.Hour)},
	)

	result := runOrderFiringCurrentTest(t, cfg, cityPath, now)
	if result.Status != StatusOK {
		t.Fatalf("status = %v, want OK for paused rig; msg = %s; details = %v", result.Status, result.Message, result.Details)
	}
	if strings.Contains(strings.Join(result.Details, "\n"), "parked") {
		t.Fatalf("details = %v, suspended rig order should be skipped", result.Details)
	}
}

func TestOrderFiringCurrent_SkipsSuspendedRigOverrides(t *testing.T) {
	// Suspended rig orders are pruned from the doctor scan; matching overrides
	// must be pruned with them so a harmless paused rig does not become a scan
	// error before the stale-order filter can skip it.
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	cityPath, cfg := orderFiringTestCity(t)
	rigPath := filepath.Join(cityPath, "rigs", "parked")
	rigFormulas := filepath.Join(rigPath, "formulas")
	rigOrders := filepath.Join(rigPath, "orders")
	if err := os.MkdirAll(rigOrders, 0o755); err != nil {
		t.Fatalf("creating rig orders dir: %v", err)
	}
	cfg.Rigs = []config.Rig{{Name: "parked", Path: rigPath, Suspended: true}}
	cfg.FormulaLayers.Rigs = map[string][]string{"parked": {cfg.FormulaLayers.City[0], rigFormulas}}
	interval := "2m"
	cfg.Orders.Overrides = []config.OrderOverride{{Name: "gate-sweep", Rig: "parked", Interval: &interval}}
	writeOrderFiringTestOrderInDir(t, rigOrders, "gate-sweep", "cooldown", "1m")
	writeOrderFiringTestEvents(t, cityPath, events.Event{Type: events.ControllerStarted, Ts: now.Add(-24 * time.Hour)})

	result := runOrderFiringCurrentTest(t, cfg, cityPath, now)
	if result.Status != StatusOK {
		t.Fatalf("status = %v, want OK for paused rig override; msg = %s; details = %v", result.Status, result.Message, result.Details)
	}
}

func TestOrderFiringCurrent_SkipsWildcardOverridesForSuspendedOnlyOrders(t *testing.T) {
	// Wildcard overrides should not turn a suspended-only order into a scan
	// error after the doctor prunes that suspended rig from the active view.
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	cityPath, cfg := orderFiringTestCity(t)
	rigPath := filepath.Join(cityPath, "rigs", "parked")
	rigFormulas := filepath.Join(rigPath, "formulas")
	rigOrders := filepath.Join(rigPath, "orders")
	if err := os.MkdirAll(rigOrders, 0o755); err != nil {
		t.Fatalf("creating rig orders dir: %v", err)
	}
	cfg.Rigs = []config.Rig{{Name: "parked", Path: rigPath, Suspended: true}}
	cfg.FormulaLayers.Rigs = map[string][]string{"parked": {cfg.FormulaLayers.City[0], rigFormulas}}
	interval := "2m"
	cfg.Orders.Overrides = []config.OrderOverride{{Name: "gate-sweep", Rig: orders.RigWildcard, Interval: &interval}}
	writeOrderFiringTestOrderInDir(t, rigOrders, "gate-sweep", "cooldown", "1m")
	writeOrderFiringTestEvents(t, cityPath, events.Event{Type: events.ControllerStarted, Ts: now.Add(-24 * time.Hour)})

	result := runOrderFiringCurrentTest(t, cfg, cityPath, now)
	if result.Status != StatusOK {
		t.Fatalf("status = %v, want OK for wildcard override targeting only a paused rig; msg = %s; details = %v", result.Status, result.Message, result.Details)
	}
}

func TestOrderFiringCurrent_SkipsInvalidOrderDuringScan(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	cityPath, cfg := orderFiringTestCity(t)
	writeOrderFiringTestOrder(t, cityPath, "cleanup-cooldown", "cooldown", "4h")
	writeOrderFiringRawOrder(t, cityPath, "invalid-env-on-formula", `[order]
formula = "mol-maintenance"
trigger = "manual"

[order.env]
CUSTOM_ORDER_FLAG = "enabled"
`)
	writeOrderFiringTestEvents(t, cityPath,
		events.Event{Type: events.ControllerStarted, Ts: now.Add(-8 * time.Hour)},
		events.Event{Type: events.OrderFired, Subject: "cleanup-cooldown", Ts: now.Add(-1 * time.Hour)},
	)

	result := runOrderFiringCurrentTest(t, cfg, cityPath, now)
	if result.Status != StatusOK {
		t.Fatalf("status = %v, want OK; msg = %s; details = %v", result.Status, result.Message, result.Details)
	}
	details := strings.Join(result.Details, "\n")
	if !strings.Contains(details, "cleanup-cooldown: last fired 1h ago, expected every 4h") {
		t.Fatalf("details = %v, want valid order firing detail", result.Details)
	}
	if strings.Contains(details, "invalid-env-on-formula") {
		t.Fatalf("details = %v, want invalid order skipped", result.Details)
	}
}

func TestOrderFiringCurrent_SkipsReservedExecEnvDuringScan(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	cityPath, cfg := orderFiringTestCity(t)
	writeOrderFiringTestOrder(t, cityPath, "cleanup-cooldown", "cooldown", "4h")
	writeOrderFiringRawOrder(t, cityPath, "invalid-reserved-env", `[order]
exec = "true"
trigger = "cooldown"
interval = "4h"

[order.env]
GC_CITY = "shadow-city"
`)
	writeOrderFiringTestEvents(t, cityPath,
		events.Event{Type: events.ControllerStarted, Ts: now.Add(-8 * time.Hour)},
		events.Event{Type: events.OrderFired, Subject: "cleanup-cooldown", Ts: now.Add(-1 * time.Hour)},
	)

	result := runOrderFiringCurrentTest(t, cfg, cityPath, now)
	if result.Status != StatusOK {
		t.Fatalf("status = %v, want OK; msg = %s; details = %v", result.Status, result.Message, result.Details)
	}
	details := strings.Join(result.Details, "\n")
	if !strings.Contains(details, "cleanup-cooldown: last fired 1h ago, expected every 4h") {
		t.Fatalf("details = %v, want valid order firing detail", result.Details)
	}
	if strings.Contains(details, "invalid-reserved-env") {
		t.Fatalf("details = %v, want reserved-env order skipped", result.Details)
	}
}

func TestOrderFiringCurrent_Overdue(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	cityPath, cfg := orderFiringTestCity(t)
	writeOrderFiringTestOrder(t, cityPath, "mol-dog-stale-db", "cron", "0 */4 * * *")
	writeOrderFiringTestEvents(t, cityPath,
		events.Event{Type: events.ControllerStarted, Ts: now.Add(-8 * time.Hour)},
		events.Event{Type: events.OrderFired, Subject: "mol-dog-stale-db", Ts: now.Add(-7 * time.Hour)},
	)

	result := runOrderFiringCurrentTest(t, cfg, cityPath, now)
	if result.Status != StatusWarning {
		t.Fatalf("status = %v, want warning; msg = %s; details = %v", result.Status, result.Message, result.Details)
	}
	if !strings.Contains(strings.Join(result.Details, "\n"), "(overdue)") {
		t.Fatalf("details = %v, want overdue detail", result.Details)
	}
}

func TestOrderFiringCurrent_Stale(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	cityPath, cfg := orderFiringTestCity(t)
	writeOrderFiringTestOrder(t, cityPath, "mol-dog-stale-db", "cron", "0 */4 * * *")
	writeOrderFiringTestEvents(t, cityPath,
		events.Event{Type: events.ControllerStarted, Ts: now.Add(-24 * time.Hour)},
		events.Event{Type: events.OrderFired, Subject: "mol-dog-stale-db", Ts: now.Add(-13 * time.Hour)},
	)

	result := runOrderFiringCurrentTest(t, cfg, cityPath, now)
	if result.Status != StatusError {
		t.Fatalf("status = %v, want error; msg = %s; details = %v", result.Status, result.Message, result.Details)
	}
	if !strings.Contains(strings.Join(result.Details, "\n"), "(CRITICAL: stale)") {
		t.Fatalf("details = %v, want stale detail", result.Details)
	}
}

func TestOrderFiringCurrent_IgnoresManualAndEventTriggers(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	cityPath, cfg := orderFiringTestCity(t)
	writeOrderFiringTestOrder(t, cityPath, "manual-maintenance", "manual", "")
	writeOrderFiringTestOrder(t, cityPath, "convoy-check", "event", "bead.closed")
	writeOrderFiringTestOrder(t, cityPath, "condition-check", "condition", "")
	writeOrderFiringTestEvents(t, cityPath, events.Event{
		Type: events.ControllerStarted,
		Ts:   now.Add(-8 * time.Hour),
	})

	result := runOrderFiringCurrentTest(t, cfg, cityPath, now)
	if result.Status != StatusOK {
		t.Fatalf("status = %v, want OK; msg = %s; details = %v", result.Status, result.Message, result.Details)
	}
	if len(result.Details) != 0 {
		t.Fatalf("details = %v, want no rows for manual/event triggers", result.Details)
	}
}

func TestComputeExpectedIntervalForCronSchedules(t *testing.T) {
	tests := []struct {
		name     string
		schedule string
		want     time.Duration
	}{
		{"every-4h", "0 */4 * * *", 4 * time.Hour},
		{"every-15min", "*/15 * * * *", 15 * time.Minute},
		{"daily-0300", "0 3 * * *", 24 * time.Hour},
		{"hourly-business", "0 9-17 * * *", time.Hour},
		// #2499: schedules coarser than daily must compute an honest interval
		// instead of erroring on an empty 24h scan window. Weekly, biweekly,
		// monthly, and yearly are the common shapes; the progressive-widen
		// algorithm walks 24h → 7d → 31d → 366d. Per the Copilot review on
		// #2525, a single match in a smaller window no longer fixes the
		// interval — the algorithm keeps widening until either a second match
		// lands or the largest window is exhausted, at which point the window
		// length is used as a conservative interval.
		{"weekly-monday-0830", "30 8 * * 1", 7 * 24 * time.Hour}, // 31d window: 5 Mondays → minGap 7d
		{"weekly-sunday", "0 0 * * 0", 7 * 24 * time.Hour},
		{"biweekly-1st-and-15th", "0 0 1,15 * *", 14 * 24 * time.Hour}, // 31d window: Jan 1, 15, Feb 1, 15, ... → minGap 14d
		{"mon-wed-fri-0830", "30 8 * * 1,3,5", 2 * 24 * time.Hour},     // 7d window has 3 matches → minGap min(Mon→Wed, Wed→Fri) = 2d
		{"monthly-first-midnight", "0 0 1 * *", 29 * 24 * time.Hour},   // 31d window: only Jan 1 → continue. 366d window: 12 matches → minGap = Feb→Mar in leap-year base 2024 = 29d
		{"yearly-new-year", "0 0 1 1 *", 366 * 24 * time.Hour},         // 366d window: Jan 1 base + (next Jan 1 at boundary excluded) → single match → window length
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := computeExpectedIntervalForCronSchedule(tt.schedule)
			if err != nil {
				t.Fatalf("computeExpectedIntervalForCronSchedule(%q): %v", tt.schedule, err)
			}
			if got != tt.want {
				t.Fatalf("computeExpectedIntervalForCronSchedule(%q) = %s, want %s", tt.schedule, got, tt.want)
			}
		})
	}
}

// TestComputeExpectedIntervalForCronSchedule_YearlyWithFirstWindowMatch
// pins the Copilot review finding on PR #2525 thread @ line 211: a yearly
// schedule whose firing minute coincidentally falls inside the 24h or 7d
// window must not be classified as a 24h/7d interval. The progressive-widen
// loop continues past single-match windows until either a second match
// lands or the largest (366d) window is exhausted; the chosen base of
// 2024-01-01 plus the leap-year window covers `0 H 1 1 *` schedules
// honestly (1 match in 366d → 366d, not 24h).
func TestComputeExpectedIntervalForCronSchedule_YearlyWithFirstWindowMatch(t *testing.T) {
	// `0 0 1 1 *` matches at base (Jan 1 2024 00:00 — i=0) and the next
	// occurrence is Jan 1 2025, which lies at exactly base+366d and is
	// excluded by the `i < windowMinutes` loop boundary. Before the
	// Copilot fix, the 24h-window early-return would have returned 24h.
	// After the fix, the loop continues past every window-with-one-match
	// and returns the 366d window length only at the last step.
	got, err := computeExpectedIntervalForCronSchedule("0 0 1 1 *")
	if err != nil {
		t.Fatalf("computeExpectedIntervalForCronSchedule(yearly): %v", err)
	}
	if got < 30*24*time.Hour {
		t.Fatalf("yearly schedule classified as %s (< 30d) — early-return-on-first-window bug regressed", got)
	}
}

// TestComputeExpectedIntervalForCronSchedule_LeapDay pins the Copilot
// finding on PR #2525 thread @ line 192: `0 0 29 2 *` (Feb 29) must not
// produce a permanent doctor-red on cities whose check window starts
// outside a leap-year window. The leap-year base (2024-01-01) means the
// 366d window includes 2024-02-29, so Feb 29 schedules match once and
// are classified as 366d (single-match-in-largest-window).
func TestComputeExpectedIntervalForCronSchedule_LeapDay(t *testing.T) {
	got, err := computeExpectedIntervalForCronSchedule("0 0 29 2 *")
	if err != nil {
		t.Fatalf("computeExpectedIntervalForCronSchedule(Feb 29): %v — leap-day schedule should not error", err)
	}
	if got != 366*24*time.Hour {
		t.Fatalf("Feb 29 schedule = %s, want 366d (single match in leap-year 366d window)", got)
	}
}

// TestComputeExpectedIntervalForCronSchedule_NoMatchInAYear pins the only
// remaining error path now that coarse schedules widen the scan up to 366
// days: a schedule that cannot match any minute in a year (here, an
// impossible day-of-month) still returns an explicit error so doctor
// surfaces it rather than silently mis-classifying the order.
func TestComputeExpectedIntervalForCronSchedule_NoMatchInAYear(t *testing.T) {
	const impossible = "0 0 31 2 *" // Feb 31 — never matches
	_, err := computeExpectedIntervalForCronSchedule(impossible)
	if err == nil {
		t.Fatalf("computeExpectedIntervalForCronSchedule(%q) returned no error; want diagnostic for unmatched schedule", impossible)
	}
}

func orderFiringTestCity(t *testing.T) (string, *config.City) {
	t.Helper()
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, "orders"), 0o755); err != nil {
		t.Fatalf("creating orders dir: %v", err)
	}
	formulasDir := filepath.Join(cityPath, "formulas")
	return cityPath, &config.City{
		FormulaLayers: config.FormulaLayers{
			City: []string{formulasDir},
		},
	}
}

func writeOrderFiringTestOrder(t *testing.T, cityPath, name, trigger, timing string) {
	t.Helper()
	writeOrderFiringTestOrderInDir(t, filepath.Join(cityPath, "orders"), name, trigger, timing)
}

func writeOrderFiringTestOrderInDir(t *testing.T, orderDir, name, trigger, timing string) {
	t.Helper()
	var body string
	switch trigger {
	case "cron":
		body = `[order]
exec = "true"
trigger = "cron"
schedule = "` + timing + `"
`
	case "cooldown":
		body = `[order]
exec = "true"
trigger = "cooldown"
interval = "` + timing + `"
`
	case "event":
		body = `[order]
exec = "true"
trigger = "event"
on = "` + timing + `"
`
	case "condition":
		body = `[order]
exec = "true"
trigger = "condition"
check = "true"
`
	default:
		body = `[order]
exec = "true"
trigger = "` + trigger + `"
`
	}
	if err := os.WriteFile(filepath.Join(orderDir, name+".toml"), []byte(body), 0o644); err != nil {
		t.Fatalf("writing order %s: %v", name, err)
	}
}

func writeOrderFiringRawOrder(t *testing.T, cityPath, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(cityPath, "orders", name+".toml"), []byte(body), 0o644); err != nil {
		t.Fatalf("writing order %s: %v", name, err)
	}
}

func writeOrderFiringTestEvents(t *testing.T, cityPath string, evts ...events.Event) {
	t.Helper()
	rec, err := events.NewFileRecorder(filepath.Join(cityPath, ".gc", "events.jsonl"), io.Discard)
	if err != nil {
		t.Fatalf("NewFileRecorder: %v", err)
	}
	t.Cleanup(func() {
		if err := rec.Close(); err != nil {
			t.Fatalf("closing FileRecorder: %v", err)
		}
	})
	for _, e := range evts {
		rec.Record(e)
	}
}

func runOrderFiringCurrentTest(t *testing.T, cfg *config.City, cityPath string, now time.Time) *CheckResult {
	t.Helper()
	check := NewOrderFiringCurrentCheck(cfg, cityPath)
	check.clock = func() time.Time { return now }
	return check.Run(&CheckContext{CityPath: cityPath})
}

func TestLatestOrderFiredAt_RecentEventSkipsLastRun(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	expected := 4 * time.Hour
	order := orders.Order{Name: "cleanup-cooldown", Trigger: "cooldown"}
	evts := []events.Event{
		{Type: events.OrderFired, Subject: order.ScopedName(), Ts: now.Add(-1 * time.Hour)},
	}

	lastRunCalled := false
	check := &OrderFiringCurrentCheck{
		lastRun: func(orders.Order) (time.Time, error) {
			lastRunCalled = true
			return time.Time{}, fmt.Errorf("lastRun must not be consulted for a recent event")
		},
	}

	got, err := check.latestOrderFiredAt(evts, order, expected, now)
	if err != nil {
		t.Fatalf("latestOrderFiredAt returned error: %v", err)
	}
	if lastRunCalled {
		t.Fatalf("lastRun was consulted for an in-band event; want fast path to skip it")
	}
	if want := now.Add(-1 * time.Hour); !got.Equal(want) {
		t.Fatalf("latest = %v, want %v (event timestamp)", got, want)
	}
}

func TestLatestOrderFiredAt_StaleEventConsultsLastRun(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	expected := 4 * time.Hour
	order := orders.Order{Name: "mol-dog-stale-db", Trigger: "cron"}
	// Event age (13h) exceeds expected*1.5 (6h), so the fast path must not apply.
	staleEvent := now.Add(-13 * time.Hour)
	freshRun := now.Add(-1 * time.Hour)
	evts := []events.Event{
		{Type: events.OrderFired, Subject: order.ScopedName(), Ts: staleEvent},
	}

	lastRunCalled := false
	check := &OrderFiringCurrentCheck{
		lastRun: func(orders.Order) (time.Time, error) {
			lastRunCalled = true
			return freshRun, nil
		},
	}

	got, err := check.latestOrderFiredAt(evts, order, expected, now)
	if err != nil {
		t.Fatalf("latestOrderFiredAt returned error: %v", err)
	}
	if !lastRunCalled {
		t.Fatalf("lastRun was not consulted for a stale event; want order-run history queried")
	}
	if !got.Equal(freshRun) {
		t.Fatalf("latest = %v, want %v (newer order-run history)", got, freshRun)
	}
}
