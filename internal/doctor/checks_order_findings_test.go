package doctor

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/orders"
)

// orderFindingsFiringFixture writes two cooldown orders on a 5m cadence: one
// whose last firing is 20m old (CRITICAL: stale) and one fired a minute ago.
func orderFindingsFiringFixture(t *testing.T, now time.Time) (string, *config.City) {
	t.Helper()
	cityPath, cfg := orderFiringTestCity(t)
	writeOrderFiringTestOrder(t, cityPath, "stale-order", "cooldown", "5m")
	writeOrderFiringTestOrder(t, cityPath, "fresh-order", "cooldown", "5m")
	writeOrderFiringTestEvents(t, cityPath,
		events.Event{Type: events.OrderFired, Subject: "stale-order", Ts: now.Add(-20 * time.Minute)},
		events.Event{Type: events.OrderFired, Subject: "fresh-order", Ts: now.Add(-time.Minute)},
	)
	return cityPath, cfg
}

// orderFindingsOutcomeFixture writes two cooldown orders: one with three
// trailing failures, one whose last run succeeded.
func orderFindingsOutcomeFixture(t *testing.T) (string, *config.City) {
	t.Helper()
	now := time.Now().UTC()
	cityPath, cfg := orderFiringTestCity(t)
	writeOrderFiringTestOrder(t, cityPath, "flaky-order", "cooldown", "5m")
	writeOrderFiringTestOrder(t, cityPath, "steady-order", "cooldown", "5m")
	writeOrderFiringTestEvents(t, cityPath,
		events.Event{Type: events.OrderFailed, Subject: "flaky-order", Ts: now.Add(-18 * time.Hour), Message: "exit status 1"},
		events.Event{Type: events.OrderFailed, Subject: "flaky-order", Ts: now.Add(-12 * time.Hour), Message: "exit status 1"},
		events.Event{Type: events.OrderFailed, Subject: "flaky-order", Ts: now.Add(-6 * time.Hour), Message: "exit status 1"},
		events.Event{Type: events.OrderCompleted, Subject: "steady-order", Ts: now.Add(-time.Hour)},
	)
	return cityPath, cfg
}

func TestOrderFiringCurrentPayloadHoldsOnlyNonOKFindings(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	cityPath, cfg := orderFindingsFiringFixture(t, now)

	result := runOrderFiringCurrentTest(t, cfg, cityPath, now)

	if result.Status != StatusError {
		t.Fatalf("fixture sanity: status = %v (%s), want StatusError", result.Status, result.Message)
	}
	findings, ok := OrderFindings(result)
	if !ok {
		t.Fatalf("OrderFindings ok = false, want the payload set on an evaluated result")
	}
	if len(findings) != 1 {
		t.Fatalf("findings = %+v, want exactly the stale order", findings)
	}
	f := findings[0]
	if f.Order != "stale-order" || f.Status != "error" || f.Severity != "blocking" {
		t.Fatalf("finding = %+v, want order=stale-order status=error severity=blocking", f)
	}
	// Detail is the Details line, not a re-rendering of it.
	if !slices.Contains(result.Details, f.Detail) {
		t.Fatalf("finding Detail %q is not one of Details %q", f.Detail, result.Details)
	}
	if !strings.Contains(f.Detail, "CRITICAL: stale") {
		t.Fatalf("Detail = %q, want the stale classification", f.Detail)
	}
	// Details stays the full per-order list for gc doctor --verbose.
	joined := strings.Join(result.Details, "\n")
	if !strings.Contains(joined, "stale-order") || !strings.Contains(joined, "fresh-order") {
		t.Fatalf("Details = %q, want both orders still listed", joined)
	}
}

func TestOrderFiringCurrentFindingWarningSeverity(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	cityPath, cfg := orderFiringTestCity(t)
	writeOrderFiringTestOrder(t, cityPath, "late-order", "cooldown", "10m")
	// 16m is past 1.5x (overdue) and under 3x (stale).
	writeOrderFiringTestEvents(t, cityPath,
		events.Event{Type: events.OrderFired, Subject: "late-order", Ts: now.Add(-16 * time.Minute)},
	)

	result := runOrderFiringCurrentTest(t, cfg, cityPath, now)

	findings, ok := OrderFindings(result)
	if !ok || len(findings) != 1 {
		t.Fatalf("findings = %+v ok=%v, want one overdue finding", findings, ok)
	}
	if findings[0].Status != "warning" || findings[0].Severity != "blocking" {
		t.Fatalf("finding = %+v, want status=warning severity=blocking (spelled like gc doctor --json)", findings[0])
	}
}

func TestOrderOutcomeHealthyPayloadHoldsOnlyNonOKFindings(t *testing.T) {
	cityPath, cfg := orderFindingsOutcomeFixture(t)

	result := NewOrderOutcomeHealthyCheck(cfg, cityPath).Run(&CheckContext{CityPath: cityPath})

	if result.Status != StatusWarning {
		t.Fatalf("fixture sanity: status = %v (%s), want StatusWarning", result.Status, result.Message)
	}
	findings, ok := OrderFindings(result)
	if !ok {
		t.Fatalf("OrderFindings ok = false, want the payload set on an evaluated result")
	}
	if len(findings) != 1 {
		t.Fatalf("findings = %+v, want exactly the failing order", findings)
	}
	f := findings[0]
	if f.Order != "flaky-order" || f.Status != "warning" || f.Severity != "advisory" {
		t.Fatalf("finding = %+v, want order=flaky-order status=warning severity=advisory", f)
	}
	if !slices.Contains(result.Details, f.Detail) {
		t.Fatalf("finding Detail %q is not one of Details %q", f.Detail, result.Details)
	}
	if got, want := result.Message, "1 order(s) failing repeatedly"; got != want {
		t.Fatalf("Message = %q, want %q (unchanged)", got, want)
	}
	// len(Findings) is the count the Message reports.
	if want := fmt.Sprintf("%d order(s) failing repeatedly", len(findings)); result.Message != want {
		t.Fatalf("Message = %q, want %q (len(Findings) equals the Message count)", result.Message, want)
	}
	joined := strings.Join(result.Details, "\n")
	if !strings.Contains(joined, "flaky-order") || !strings.Contains(joined, "steady-order") {
		t.Fatalf("Details = %q, want both orders still listed", joined)
	}
}

// TestOrderChecksOKLevelLinesAreNeverFindings pins the lines the checks print
// at OK level: they stay in Details and never enter Findings.
func TestOrderChecksOKLevelLinesAreNeverFindings(t *testing.T) {
	t.Run("order-outcome-healthy", func(t *testing.T) {
		now := time.Now().UTC()
		cityPath, cfg := orderFiringTestCity(t)
		writeOrderFiringTestOrder(t, cityPath, "new-order", "cooldown", "5m")
		writeOrderFiringTestOrder(t, cityPath, "twice-order", "cooldown", "5m")
		writeOrderFiringTestOrder(t, cityPath, "ok-order", "cooldown", "5m")
		writeOrderFiringTestEvents(t, cityPath,
			events.Event{Type: events.OrderFailed, Subject: "twice-order", Ts: now.Add(-2 * time.Hour), Message: "x"},
			events.Event{Type: events.OrderFailed, Subject: "twice-order", Ts: now.Add(-time.Hour), Message: "x"},
			events.Event{Type: events.OrderCompleted, Subject: "ok-order", Ts: now.Add(-time.Hour)},
		)

		result := NewOrderOutcomeHealthyCheck(cfg, cityPath).Run(&CheckContext{CityPath: cityPath})

		if result.Status != StatusOK {
			t.Fatalf("status = %v (%s), want StatusOK", result.Status, result.Message)
		}
		joined := strings.Join(result.Details, "\n")
		for _, want := range []string{"no completed runs yet", "under threshold", "last run succeeded"} {
			if !strings.Contains(joined, want) {
				t.Fatalf("Details = %q, want a %q line", joined, want)
			}
		}
		findings, ok := OrderFindings(result)
		if !ok || findings == nil || len(findings) != 0 {
			t.Fatalf("findings = %#v ok=%v, want a non-nil empty slice", findings, ok)
		}
	})

	t.Run("order-firing-current", func(t *testing.T) {
		now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
		cityPath, cfg := orderFiringTestCity(t)
		writeOrderFiringTestOrder(t, cityPath, "early-order", "cooldown", "1h")
		writeOrderFiringTestOrder(t, cityPath, "elsewhere-order", "cooldown", "5m")
		// Controller started five minutes ago: inside the first cycle of a 1h
		// order, so "never fired" is OK-level.
		writeOrderFiringTestEvents(t, cityPath,
			events.Event{Type: events.ControllerStarted, Ts: now.Add(-5 * time.Minute)},
		)
		check := NewOrderFiringCurrentCheck(cfg, cityPath,
			WithOrderFiringCurrentInapplicableFunc(func(o orders.Order) (string, bool) {
				return "bd", o.Name == "elsewhere-order"
			}),
		)
		check.clock = func() time.Time { return now }

		result := check.Run(&CheckContext{CityPath: cityPath})

		if result.Status != StatusOK {
			t.Fatalf("status = %v (%s), want StatusOK", result.Status, result.Message)
		}
		joined := strings.Join(result.Details, "\n")
		for _, want := range []string{"within first cycle", "inapplicable (backend bd)"} {
			if !strings.Contains(joined, want) {
				t.Fatalf("Details = %q, want a %q line", joined, want)
			}
		}
		findings, ok := OrderFindings(result)
		if !ok || findings == nil || len(findings) != 0 {
			t.Fatalf("findings = %#v ok=%v, want a non-nil empty slice", findings, ok)
		}
	})
}

func TestOrderChecksPayloadNilOnEarlyReturn(t *testing.T) {
	cityPath, cfg := orderFiringTestCity(t)
	for name, result := range map[string]*CheckResult{
		"firing/no config":  NewOrderFiringCurrentCheck(nil, cityPath).Run(&CheckContext{}),
		"firing/no city":    NewOrderFiringCurrentCheck(cfg, "").Run(&CheckContext{}),
		"outcome/no config": NewOrderOutcomeHealthyCheck(nil, cityPath).Run(&CheckContext{}),
		"outcome/no city":   NewOrderOutcomeHealthyCheck(cfg, "").Run(&CheckContext{}),
	} {
		if result.Payload != nil {
			t.Errorf("%s: Payload = %#v, want nil on an early return", name, result.Payload)
		}
		if findings, ok := OrderFindings(result); ok || findings != nil {
			t.Errorf("%s: OrderFindings = %#v, %v, want nil, false", name, findings, ok)
		}
	}
}

func TestOrderFindingsAccessorIsNilSafe(t *testing.T) {
	if f, ok := OrderFindings(nil); ok || f != nil {
		t.Fatalf("OrderFindings(nil) = %#v, %v, want nil, false", f, ok)
	}
	if f, ok := OrderFindings(&CheckResult{Payload: "not ours"}); ok || f != nil {
		t.Fatalf("OrderFindings(foreign payload) = %#v, %v, want nil, false", f, ok)
	}
	var typedNil *OrderFindingsPayload
	if f, ok := OrderFindings(&CheckResult{Payload: typedNil}); ok || f != nil {
		t.Fatalf("OrderFindings(typed nil) = %#v, %v, want nil, false", f, ok)
	}
}

// TestOrderChecksAllOKPayloadMarshalsEmptyArray pins that gc doctor --json
// prints "findings":[] (not null) for an evaluated all-OK result.
func TestOrderChecksAllOKPayloadMarshalsEmptyArray(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	cityPath, cfg := orderFiringTestCity(t)
	writeOrderFiringTestOrder(t, cityPath, "fresh-order", "cooldown", "5m")
	writeOrderFiringTestEvents(t, cityPath,
		events.Event{Type: events.OrderFired, Subject: "fresh-order", Ts: now.Add(-time.Minute)},
		events.Event{Type: events.OrderCompleted, Subject: "fresh-order", Ts: now.Add(-time.Minute)},
	)

	for name, result := range map[string]*CheckResult{
		"firing":  runOrderFiringCurrentTest(t, cfg, cityPath, now),
		"outcome": NewOrderOutcomeHealthyCheck(cfg, cityPath).Run(&CheckContext{CityPath: cityPath}),
	} {
		if result.Status != StatusOK {
			t.Fatalf("%s: status = %v (%s), want StatusOK", name, result.Status, result.Message)
		}
		data, err := json.Marshal(result.Payload)
		if err != nil {
			t.Fatalf("%s: marshal payload: %v", name, err)
		}
		if got, want := string(data), `{"findings":[]}`; got != want {
			t.Fatalf("%s: payload JSON = %s, want %s", name, got, want)
		}
	}
}
