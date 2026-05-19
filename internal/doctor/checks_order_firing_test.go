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
		schedule string
		want     time.Duration
	}{
		{"0 */4 * * *", 4 * time.Hour},
		{"*/15 * * * *", 15 * time.Minute},
		{"0 3 * * *", 24 * time.Hour},
		{"0 9-17 * * *", time.Hour},
	}
	for _, tt := range tests {
		got, err := computeExpectedIntervalForCronSchedule(tt.schedule)
		if err != nil {
			t.Fatalf("computeExpectedIntervalForCronSchedule(%q): %v", tt.schedule, err)
		}
		if got != tt.want {
			t.Fatalf("computeExpectedIntervalForCronSchedule(%q) = %s, want %s", tt.schedule, got, tt.want)
		}
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
