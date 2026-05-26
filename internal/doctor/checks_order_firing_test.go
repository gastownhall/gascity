package doctor

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
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
	if !strings.Contains(strings.Join(result.Details, "\n"), "never fired since controller start") {
		t.Fatalf("details = %v, want never-fired controller-start message", result.Details)
	}
	if result.FixHint != "Inspect with: gc order check && gc order history mol-dog-stale-db" {
		t.Fatalf("FixHint = %q, want inspect hint for order", result.FixHint)
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
	default:
		body = `[order]
exec = "true"
trigger = "` + trigger + `"
`
	}
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
