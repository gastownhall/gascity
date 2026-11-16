package doctor

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/events"
)

// orderFiringRingPeers is how many hourly cooldown orders writeOrderFiringRing
// adds beside the order under test: a ring of nine takes three passes of the
// default budget of 4, one default 30s patrol each, a 1m30s dispatch floor.
const orderFiringRingPeers = 8

func writeOrderFiringRing(t *testing.T, cityPath string) {
	t.Helper()
	for i := 0; i < orderFiringRingPeers; i++ {
		writeOrderFiringTestOrder(t, cityPath, fmt.Sprintf("ring-%02d", i), "cooldown", "1h")
	}
}

func TestOrderFiringCurrent_NeverFired_WithinTheDispatchFloor(t *testing.T) {
	// A 30s cooldown beside eight other counted orders cannot fire before the
	// ring comes round: three passes of four at 30s, a 1m30s floor. Controller
	// up 60s is within that first cycle, not a never-fired execution gap.
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	cityPath, cfg := orderFiringTestCity(t)
	writeOrderFiringTestOrder(t, cityPath, "beads-health", "cooldown", "30s")
	writeOrderFiringRing(t, cityPath)
	writeOrderFiringTestEvents(t, cityPath, events.Event{Type: events.ControllerStarted, Ts: now.Add(-60 * time.Second)})

	result := runOrderFiringCurrentTest(t, cfg, cityPath, now)
	if result.Status != StatusOK {
		t.Fatalf("status = %v, want OK; msg = %s; details = %v", result.Status, result.Message, result.Details)
	}
	detail := orderFiringDetail(t, result, "beads-health")
	if !strings.Contains(detail, "within first cycle") || !strings.Contains(detail, "1m30s dispatch floor") {
		t.Fatalf("beads-health detail = %q, want within-first-cycle naming the 1m30s dispatch floor", detail)
	}
}

func TestOrderFiringCurrent_NeverFired_BeyondTheDispatchFloorStaysBlocking(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	cityPath, cfg := orderFiringTestCity(t)
	writeOrderFiringTestOrder(t, cityPath, "beads-health", "cooldown", "30s")
	writeOrderFiringRing(t, cityPath)
	writeOrderFiringTestEvents(t, cityPath, events.Event{Type: events.ControllerStarted, Ts: now.Add(-3 * time.Minute)})

	result := runOrderFiringCurrentTest(t, cfg, cityPath, now)
	if result.Status != StatusError || result.Severity != SeverityBlocking {
		t.Fatalf("status = %v severity = %v, want blocking error; details = %v", result.Status, result.Severity, result.Details)
	}
	if detail := orderFiringDetail(t, result, "beads-health"); !strings.Contains(detail, "never fired since controller start") {
		t.Fatalf("beads-health detail = %q, want never-fired", detail)
	}
}

func TestOrderFiringCurrent_StalenessIsJudgedAgainstTheDispatchFloor(t *testing.T) {
	// Last fired 100s ago: three 30s intervals have passed (CRITICAL against
	// the interval), but not one and a half of the 1m30s cadence the budget allows.
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	cityPath, cfg := orderFiringTestCity(t)
	writeOrderFiringTestOrder(t, cityPath, "beads-health", "cooldown", "30s")
	writeOrderFiringRing(t, cityPath)
	evts := []events.Event{
		{Type: events.ControllerStarted, Ts: now.Add(-time.Hour)},
		{Type: events.OrderFired, Subject: "beads-health", Ts: now.Add(-100 * time.Second)},
	}
	for i := 0; i < orderFiringRingPeers; i++ {
		evts = append(evts, events.Event{Type: events.OrderFired, Subject: fmt.Sprintf("ring-%02d", i), Ts: now.Add(-10 * time.Minute)})
	}
	writeOrderFiringTestEvents(t, cityPath, evts...)

	result := runOrderFiringCurrentTest(t, cfg, cityPath, now)
	if result.Status != StatusOK {
		t.Fatalf("status = %v, want OK; msg = %s; details = %v", result.Status, result.Message, result.Details)
	}
	detail := orderFiringDetail(t, result, "beads-health")
	if !strings.Contains(detail, "interval 30s") || !strings.Contains(detail, "1m30s dispatch floor") {
		t.Fatalf("beads-health detail = %q, want the interval and the dispatch floor named", detail)
	}

	// Well past the floor's cadence it is stale again.
	result = runOrderFiringCurrentTest(t, cfg, cityPath, now.Add(5*time.Minute))
	if result.Status != StatusError || result.Severity != SeverityBlocking {
		t.Fatalf("status = %v severity = %v, want blocking error; details = %v", result.Status, result.Severity, result.Details)
	}
}

func TestOrderFiringCurrent_AnIntervalAboveTheFloorKeepsItsOwnCadence(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	cityPath, cfg := orderFiringTestCity(t)
	writeOrderFiringTestOrder(t, cityPath, "cleanup-cooldown", "cooldown", "1h")
	writeOrderFiringRing(t, cityPath)
	writeOrderFiringTestEvents(t, cityPath,
		events.Event{Type: events.ControllerStarted, Ts: now.Add(-24 * time.Hour)},
		events.Event{Type: events.OrderFired, Subject: "cleanup-cooldown", Ts: now.Add(-6 * time.Hour)},
	)

	result := runOrderFiringCurrentTest(t, cfg, cityPath, now)
	detail := orderFiringDetail(t, result, "cleanup-cooldown")
	if !strings.Contains(detail, "expected every 1h (CRITICAL: stale)") || strings.Contains(detail, "dispatch floor") {
		t.Fatalf("cleanup-cooldown detail = %q, want stale against its own 1h interval", detail)
	}
}

func orderFiringDetail(t *testing.T, result *CheckResult, order string) string {
	t.Helper()
	for _, d := range result.Details {
		if strings.HasPrefix(d, order+":") {
			return d
		}
	}
	t.Fatalf("no detail for %s in %v", order, result.Details)
	return ""
}
