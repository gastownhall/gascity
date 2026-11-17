package main

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
)

// statusOrdersFixtureTwoOrders writes a stale order and a fresh order, both on
// a 5m cooldown. Only the stale one is unhealthy.
func statusOrdersFixtureTwoOrders(t *testing.T) (string, *config.City) {
	t.Helper()
	cityPath, cfg := cityStatusOrderTestCity(t)
	writeCityStatusOrderTOML(t, cityPath, "stale-order", "cooldown", "5m")
	writeCityStatusOrderTOML(t, cityPath, "fresh-order", "cooldown", "5m")
	now := time.Now().UTC()
	writeCityStatusOrderEvents(t, cityPath,
		events.Event{Type: events.OrderFired, Subject: "stale-order", Ts: now.Add(-20 * time.Minute)},
		events.Event{Type: events.OrderFired, Subject: "fresh-order", Ts: now.Add(-time.Minute)},
		events.Event{Type: events.OrderCompleted, Subject: "fresh-order", Ts: now.Add(-time.Minute)},
	)
	return cityPath, cfg
}

func statusOrdersTextRun(t *testing.T, cfg *config.City, cityPath string) string {
	t.Helper()
	sp := runtime.NewFake()
	store := beads.NewMemStore()
	var stdout, stderr bytes.Buffer
	statusSnapshot := loadStatusSessionSnapshot(cityPath, cfg, cliSessionStore(store, cfg, cityPath), &stderr)
	doCityStatusWithStoreAndSnapshot(sp, newDrainOps(sp), cfg, cityPath, store, statusSnapshot, &stdout, &stderr)
	return stdout.String()
}

func statusOrdersJSONRun(t *testing.T, cfg *config.City, cityPath string) string {
	t.Helper()
	sp := runtime.NewFake()
	store := beads.NewMemStore()
	var stdout, stderr bytes.Buffer
	statusSnapshot := loadStatusSessionSnapshot(cityPath, cfg, cliSessionStore(store, cfg, cityPath), &stderr)
	doCityStatusJSONWithDiagnosticAndSnapshot(sp, cfg, cityPath, store, nil, statusSnapshot, &stdout, &stderr)
	return stdout.String()
}

// stubStatusOrderChecks swaps the check set the Orders leg runs and returns a
// counter of how many times the constructor was asked for checks.
func stubStatusOrderChecks(t *testing.T, build func() []doctor.Check) *atomic.Int32 {
	t.Helper()
	var calls atomic.Int32
	old := newStatusOrderChecks
	newStatusOrderChecks = func(*config.City, string, io.Writer) []doctor.Check {
		calls.Add(1)
		return build()
	}
	t.Cleanup(func() { newStatusOrderChecks = old })
	return &calls
}

type fakeStatusOrderCheck struct {
	name string
	run  func(*doctor.CheckContext) *doctor.CheckResult
}

func (c fakeStatusOrderCheck) Name() string                                     { return c.name }
func (c fakeStatusOrderCheck) CanFix() bool                                     { return false }
func (c fakeStatusOrderCheck) WarmupEligible() bool                             { return false }
func (c fakeStatusOrderCheck) Fix(*doctor.CheckContext) error                   { return nil }
func (c fakeStatusOrderCheck) Run(ctx *doctor.CheckContext) *doctor.CheckResult { return c.run(ctx) }

func TestGCStatusOrdersTextShowsOnlyTheUnhealthyOrder(t *testing.T) {
	cityPath, cfg := statusOrdersFixtureTwoOrders(t)

	out := statusOrdersTextRun(t, cfg, cityPath)

	if !strings.Contains(out, "Orders:") {
		t.Fatalf("stdout has no Orders block:\n%s", out)
	}
	if !strings.Contains(out, "stale-order: last fired") {
		t.Fatalf("stdout lacks the unhealthy order's line:\n%s", out)
	}
	if strings.Contains(out, "fresh-order") {
		t.Fatalf("stdout names the healthy order, want only non-OK findings:\n%s", out)
	}
}

func TestGCStatusOrdersTextOmitsBlockWhenAllHealthy(t *testing.T) {
	cityPath, cfg := cityStatusOrderTestCity(t)
	writeCityStatusOrderTOML(t, cityPath, "fresh-order", "cooldown", "5m")
	now := time.Now().UTC()
	writeCityStatusOrderEvents(t, cityPath,
		events.Event{Type: events.OrderFired, Subject: "fresh-order", Ts: now.Add(-time.Minute)},
		events.Event{Type: events.OrderCompleted, Subject: "fresh-order", Ts: now.Add(-time.Minute)},
	)

	out := statusOrdersTextRun(t, cfg, cityPath)

	if strings.Contains(out, "Orders:") || strings.Contains(out, "fresh-order") {
		t.Fatalf("stdout carries an Orders block for an all-healthy city:\n%s", out)
	}
}

// TestGCStatusOrdersNoRowIsOK pins the structural rule: every row that reaches
// snapshot.Orders is non-OK, including the suppression rows that used to leave
// Status at its zero value.
func TestGCStatusOrdersNoRowIsOK(t *testing.T) {
	cityPath, cfg := statusOrdersFixtureTwoOrders(t)
	now := time.Now().UTC()
	writeCityStatusOrderEvents(t, cityPath, events.Event{
		Type:    events.OrderSuppressed,
		Subject: "watched-order",
		Ts:      now,
		Message: "open-work gate has suppressed this order for 12 consecutive dispatch checks",
		Payload: events.OrderSuppressedPayloadJSON(events.OrderSuppressedPayload{
			OrderName:       "watched-order",
			Consecutive:     12,
			FirstSuppressed: now.Add(-time.Hour).Format(time.RFC3339),
		}),
	})
	sp := runtime.NewFake()
	var stderr bytes.Buffer
	statusSnapshot := loadStatusSessionSnapshot(cityPath, cfg, cliSessionStore(beads.NewMemStore(), cfg, cityPath), &stderr)

	snapshot := collectCityStatusTextSnapshot(sp, cfg, cityPath, beads.NewMemStore(), statusSnapshot, &stderr)

	if len(snapshot.Orders) < 2 {
		t.Fatalf("Orders = %+v, want the firing row and the suppression row", snapshot.Orders)
	}
	for _, row := range snapshot.Orders {
		if row.Status == doctor.StatusOK {
			t.Fatalf("row %+v has Status OK; every Orders row must be non-OK", row)
		}
	}
	supp := findCityStatusOrder(t, snapshot.Orders, "watched-order")
	if supp.Status != doctor.StatusWarning || supp.Severity != doctor.SeverityAdvisory {
		t.Fatalf("suppression row = %+v, want an explicit warning/advisory", supp)
	}
}

func TestGCStatusOrdersCapsFindingsWorstFirstWithOverflow(t *testing.T) {
	// 22 advisory warnings then 3 blocking errors, handed over in the order
	// that would hide the errors behind the cap if the renderer kept it.
	var findings []doctor.OrderFinding
	for i := 0; i < 22; i++ {
		findings = append(findings, doctor.OrderFinding{
			Order: fmt.Sprintf("warn-%02d", i), Status: "warning", Severity: "advisory",
			Detail: fmt.Sprintf("warn-%02d: overdue", i),
		})
	}
	for _, name := range []string{"err-c", "err-a", "err-b"} {
		findings = append(findings, doctor.OrderFinding{
			Order: name, Status: "error", Severity: "blocking", Detail: name + ": stale",
		})
	}
	stubStatusOrderChecks(t, func() []doctor.Check {
		return []doctor.Check{fakeStatusOrderCheck{name: "order-firing-current", run: func(*doctor.CheckContext) *doctor.CheckResult {
			return &doctor.CheckResult{
				Name: "order-firing-current", Status: doctor.StatusError, Message: "scheduled orders are stale",
				Payload: &doctor.OrderFindingsPayload{Findings: findings},
			}
		}}}
	})
	cityPath, cfg := cityStatusOrderTestCity(t)
	var stdout, stderr bytes.Buffer
	snapshot := collectCityStatusTextSnapshot(runtime.NewFake(), cfg, cityPath, beads.NewMemStore(), nil, &stderr)

	renderCityStatusText(snapshot, newDrainOps(runtime.NewFake()), &stdout)

	var lines []string
	for _, l := range strings.Split(stdout.String(), "\n") {
		if strings.HasPrefix(l, "    ") {
			lines = append(lines, strings.TrimSpace(l))
		}
	}
	if len(lines) != cityStatusOrderDetailLimit+1 {
		t.Fatalf("got %d finding lines, want %d findings plus the overflow line:\n%s", len(lines), cityStatusOrderDetailLimit, stdout.String())
	}
	for i, want := range []string{"err-a: stale", "err-b: stale", "err-c: stale", "warn-00: overdue"} {
		if lines[i] != want {
			t.Fatalf("line %d = %q, want %q (worst first, then order name):\n%s", i, lines[i], want, stdout.String())
		}
	}
	if got, want := lines[len(lines)-1], "... and 5 more (gc doctor --verbose)"; got != want {
		t.Fatalf("overflow line = %q, want %q (counted over findings)", got, want)
	}
}

func TestGCStatusOrdersTimeoutBecomesUnmeasuredRow(t *testing.T) {
	old := statusOrderHealthTimeout
	statusOrderHealthTimeout = 100 * time.Millisecond
	t.Cleanup(func() { statusOrderHealthTimeout = old })

	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	stubStatusOrderChecks(t, func() []doctor.Check {
		return []doctor.Check{
			fakeStatusOrderCheck{name: "order-firing-current", run: func(*doctor.CheckContext) *doctor.CheckResult {
				<-release
				return &doctor.CheckResult{Name: "order-firing-current", Status: doctor.StatusOK}
			}},
			fakeStatusOrderCheck{name: "order-outcome-healthy", run: func(*doctor.CheckContext) *doctor.CheckResult {
				<-release
				return &doctor.CheckResult{Name: "order-outcome-healthy", Status: doctor.StatusOK}
			}},
		}
	})
	cityPath, cfg := cityStatusOrderTestCity(t)
	cfg.Agents = []config.Agent{{Name: "worker", MaxActiveSessions: intPtr(1)}}
	sp := runtime.NewFake()
	var stderr bytes.Buffer

	start := time.Now()
	snapshot := collectCityStatusTextSnapshot(sp, cfg, cityPath, beads.NewMemStore(), nil, &stderr)
	elapsed := time.Since(start)

	// One budget, not two: the checks run concurrently.
	if elapsed > statusOrderHealthTimeout+1500*time.Millisecond {
		t.Fatalf("collect took %s with a %s order budget", elapsed, statusOrderHealthTimeout)
	}
	for _, name := range []string{"order-firing-current", "order-outcome-healthy"} {
		row := findCityStatusOrder(t, snapshot.Orders, name)
		if row.Status != doctor.StatusWarning || row.Severity != doctor.SeverityAdvisory {
			t.Fatalf("row %+v, want warning/advisory", row)
		}
		for _, want := range []string{"unmeasured", "no verdict within 100ms", "gc doctor --check " + name} {
			if !strings.Contains(row.Message, want) {
				t.Fatalf("row message %q lacks %q", row.Message, want)
			}
		}
	}
	// The timeout is not an agent-status problem.
	if snapshot.Partial || len(snapshot.PartialErrors) != 0 {
		t.Fatalf("Partial = %v %v, want the order timeout kept out of Partial", snapshot.Partial, snapshot.PartialErrors)
	}
	if len(snapshot.Agents) != 1 || snapshot.Summary.TotalAgents != 1 {
		t.Fatalf("Agents = %+v, want the agent block unchanged", snapshot.Agents)
	}
}

func TestGCStatusJSONDoesNotRunTheOrdersLeg(t *testing.T) {
	cityPath, cfg := cityStatusOrderTestCity(t)
	calls := stubStatusOrderChecks(t, func() []doctor.Check { return nil })

	healthy := statusOrdersJSONRun(t, cfg, cityPath)

	writeCityStatusOrderTOML(t, cityPath, "stale-order", "cooldown", "5m")
	writeCityStatusOrderEvents(t, cityPath,
		events.Event{Type: events.OrderFired, Subject: "stale-order", Ts: time.Now().UTC().Add(-20 * time.Minute)},
	)
	unhealthy := statusOrdersJSONRun(t, cfg, cityPath)

	if healthy != unhealthy {
		t.Fatalf("gc status --json differs with an unhealthy order:\n--- without ---\n%s\n--- with ---\n%s", healthy, unhealthy)
	}
	if strings.Contains(unhealthy, "stale-order") || strings.Contains(strings.ToLower(unhealthy), "\"orders\"") {
		t.Fatalf("gc status --json mentions orders:\n%s", unhealthy)
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("order checks were built %d times for --json, want 0", n)
	}

	// Control: the text route does run the leg.
	statusOrdersTextRun(t, cfg, cityPath)
	if n := calls.Load(); n != 1 {
		t.Fatalf("order checks were built %d times for text, want 1", n)
	}
}

// TestGCDoctorJSONCarriesEmptyFindingsForAllOK pins the gc doctor --json
// shape: an evaluated all-OK order check prints "findings":[] not null.
func TestGCDoctorJSONCarriesEmptyFindingsForAllOK(t *testing.T) {
	cityPath, cfg := cityStatusOrderTestCity(t)
	writeCityStatusOrderTOML(t, cityPath, "fresh-order", "cooldown", "5m")
	writeCityStatusOrderEvents(t, cityPath,
		events.Event{Type: events.OrderFired, Subject: "fresh-order", Ts: time.Now().UTC().Add(-time.Minute)},
		events.Event{Type: events.OrderCompleted, Subject: "fresh-order", Ts: time.Now().UTC().Add(-time.Minute)},
	)
	d := &doctor.Doctor{}
	d.Register(doctor.NewOrderFiringCurrentCheck(cfg, cityPath))
	d.Register(doctor.NewOrderOutcomeHealthyCheck(cfg, cityPath))
	report := d.RunCollect(&doctor.CheckContext{CityPath: cityPath}, false)

	var buf bytes.Buffer
	if err := writeDoctorJSON(&buf, report); err != nil {
		t.Fatalf("writeDoctorJSON: %v", err)
	}
	if got := strings.Count(buf.String(), `"findings":[]`); got != 2 {
		t.Fatalf(`doctor --json carries "findings":[] %d times, want 2 (one per check):`+"\n%s", got, buf.String())
	}
}
