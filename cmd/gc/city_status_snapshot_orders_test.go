package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
)

// cityStatusOrderTestCity creates a minimal on-disk city (orders/ and
// formulas/ dirs) suitable for doctor.NewOrderFiringCurrentCheck and
// doctor.NewOrderOutcomeHealthyCheck. internal/doctor's own
// orderFiringTestCity fixture is unexported and lives in a different
// package, so its shape is reproduced here rather than imported.
func cityStatusOrderTestCity(t *testing.T) (string, *config.City) {
	t.Helper()
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, "orders"), 0o755); err != nil {
		t.Fatalf("mkdir orders: %v", err)
	}
	formulasDir := filepath.Join(cityPath, "formulas")
	if err := os.MkdirAll(formulasDir, 0o755); err != nil {
		t.Fatalf("mkdir formulas: %v", err)
	}
	return cityPath, &config.City{
		Workspace:     config.Workspace{Name: "city"},
		FormulaLayers: config.FormulaLayers{City: []string{formulasDir}},
	}
}

// writeCityStatusOrderTOML writes a minimal order definition. name becomes
// the order's bare Name (orders.ScanAll derives it from the filename stem),
// matching the convention cmd_doctor_order_firing_test.go and
// internal/doctor's writeOrderFiringTestOrder already use.
func writeCityStatusOrderTOML(t *testing.T, cityPath, name, trigger, interval string) {
	t.Helper()
	body := fmt.Sprintf("[order]\nexec = \"true\"\ntrigger = %q\n", trigger)
	if interval != "" {
		body += fmt.Sprintf("interval = %q\n", interval)
	}
	mustWriteDoctorOrderFiringTestFile(t, filepath.Join(cityPath, "orders", name+".toml"), body)
}

// writeCityStatusOrderEvents appends events to <cityPath>/.gc/events.jsonl,
// mirroring internal/doctor's writeOrderFiringTestEvents helper (unexported,
// different package, so reproduced here).
func writeCityStatusOrderEvents(t *testing.T, cityPath string, evts ...events.Event) {
	t.Helper()
	rec, err := events.NewFileRecorder(filepath.Join(cityPath, ".gc", "events.jsonl"), io.Discard)
	if err != nil {
		t.Fatalf("NewFileRecorder: %v", err)
	}
	for _, e := range evts {
		rec.Record(e)
	}
	if err := rec.Close(); err != nil {
		t.Fatalf("Close recorder: %v", err)
	}
}

func findCityStatusOrder(t *testing.T, orders []cityStatusOrder, name string) cityStatusOrder {
	t.Helper()
	for _, o := range orders {
		if o.Name == name {
			return o
		}
	}
	t.Fatalf("no Orders entry named %q in %+v", name, orders)
	return cityStatusOrder{}
}

// TestCityStatusOrdersEmptyOnHealthyCity pins the zero-config happy path:
// gc status must stay byte-for-byte unchanged (no "Orders:" section, empty
// Orders slice) when nothing is overdue, failing, or gate-suppressed. Every
// other test in this file adds a positive signal; this one proves silence
// when there is nothing to report, per this bead's exit contract ("healthy
// city -> gc status output unchanged").
func TestCityStatusOrdersEmptyOnHealthyCity(t *testing.T) {
	cityPath, cfg := cityStatusOrderTestCity(t)
	// No order files and no events.jsonl at all: scanOrderFiringCurrentOrders
	// and consecutiveOrderFailures both treat "nothing monitored" as
	// StatusOK, mirroring internal/doctor's own
	// TestOrderOutcomeHealthyReportsCleanCity.

	sp := runtime.NewFake()
	store := beads.NewMemStore()
	var stderr bytes.Buffer
	snapshot := collectCityStatusSnapshot(sp, cfg, cityPath, store, &stderr)

	if len(snapshot.Orders) != 0 {
		t.Fatalf("Orders = %+v, want empty on a healthy city", snapshot.Orders)
	}

	var stdout bytes.Buffer
	renderCityStatusText(snapshot, newDrainOps(sp), &stdout)
	if strings.Contains(stdout.String(), "Orders:") {
		t.Fatalf("stdout = %q, want no Orders section on a healthy city", stdout.String())
	}
}

// TestCityStatusOrdersSurfacesStaleFiring cross-checks gc status against
// gc doctor's own order-firing-current check on the same fixture: an order
// whose last fire crosses classifyOrderFiring's 3x-interval "CRITICAL:
// stale" threshold. gc status must carry the exact Status/Severity/Message
// doctor.NewOrderFiringCurrentCheck.Run produces -- not a re-derived
// approximation -- so the two commands can never disagree (this bead's
// core zero-config requirement).
func TestCityStatusOrdersSurfacesStaleFiring(t *testing.T) {
	cityPath, cfg := cityStatusOrderTestCity(t)
	writeCityStatusOrderTOML(t, cityPath, "stale-order", "cooldown", "5m")

	now := time.Now().UTC()
	writeCityStatusOrderEvents(t, cityPath,
		events.Event{Type: events.OrderFired, Subject: "stale-order", Ts: now.Add(-20 * time.Minute)},
	)

	wantResult := doctor.NewOrderFiringCurrentCheck(cfg, cityPath).Run(&doctor.CheckContext{CityPath: cityPath})
	if wantResult.Status != doctor.StatusError {
		t.Fatalf("fixture sanity: doctor order-firing-current status = %v, want StatusError (fixture drifted off the 3x-interval threshold)", wantResult.Status)
	}

	sp := runtime.NewFake()
	store := beads.NewMemStore()
	var stderr bytes.Buffer
	snapshot := collectCityStatusSnapshot(sp, cfg, cityPath, store, &stderr)

	order := findCityStatusOrder(t, snapshot.Orders, wantResult.Name)
	if order.Status != wantResult.Status {
		t.Fatalf("Orders[%s].Status = %v, want %v (from gc doctor's own Run)", wantResult.Name, order.Status, wantResult.Status)
	}
	if order.Severity != wantResult.Severity {
		t.Fatalf("Orders[%s].Severity = %v, want %v", wantResult.Name, order.Severity, wantResult.Severity)
	}
	if order.Message != wantResult.Message {
		t.Fatalf("Orders[%s].Message = %q, want %q (gc status and gc doctor must never disagree)", wantResult.Name, order.Message, wantResult.Message)
	}

	// The check's Message names no order ("scheduled orders are stale"), so
	// the row is only actionable if it carries the check's own per-order
	// Details through. Without them an operator has to run gc doctor to learn
	// which order is stale, which is the thing this bead exists to avoid.
	if len(order.Details) == 0 {
		t.Fatalf("Orders[%s].Details is empty, want doctor's per-order breakdown %v", wantResult.Name, wantResult.Details)
	}
	if !slices.Equal(order.Details, wantResult.Details) {
		t.Fatalf("Orders[%s].Details = %v, want %v (carried through verbatim from gc doctor)", wantResult.Name, order.Details, wantResult.Details)
	}

	var stdout bytes.Buffer
	renderCityStatusText(snapshot, newDrainOps(sp), &stdout)
	if !strings.Contains(stdout.String(), "Orders:") {
		t.Fatalf("stdout missing Orders section:\n%s", stdout.String())
	}
	if !strings.Contains(stdout.String(), wantResult.Message) {
		t.Fatalf("stdout = %q, want it to contain doctor's message %q", stdout.String(), wantResult.Message)
	}
	if !strings.Contains(stdout.String(), "stale-order") {
		t.Fatalf("stdout = %q, want the rendered row to name the offending order %q", stdout.String(), "stale-order")
	}
}

// TestCityStatusOrdersSurfacesRepeatedFailures cross-checks gc status
// against gc doctor's order-outcome-healthy check: an order with three
// consecutive order.failed events crosses classifyOrderOutcome's
// failure-streak threshold. Mirrors internal/doctor's
// TestOrderOutcomeHealthy_FlagsRigScopedOrderFailureStreak fixture shape
// (three failures at -18h/-12h/-6h, well clear of any controller-start
// grace window) without rig scoping, since gc status's Orders surfacing
// does not depend on rig scope.
func TestCityStatusOrdersSurfacesRepeatedFailures(t *testing.T) {
	cityPath, cfg := cityStatusOrderTestCity(t)
	writeCityStatusOrderTOML(t, cityPath, "flaky-order", "cooldown", "5m")

	now := time.Now().UTC()
	writeCityStatusOrderEvents(t, cityPath,
		events.Event{Type: events.OrderFailed, Subject: "flaky-order", Ts: now.Add(-18 * time.Hour), Message: "exit status 1"},
		events.Event{Type: events.OrderFailed, Subject: "flaky-order", Ts: now.Add(-12 * time.Hour), Message: "exit status 1"},
		events.Event{Type: events.OrderFailed, Subject: "flaky-order", Ts: now.Add(-6 * time.Hour), Message: "exit status 1"},
	)

	wantResult := doctor.NewOrderOutcomeHealthyCheck(cfg, cityPath).Run(&doctor.CheckContext{CityPath: cityPath})
	if wantResult.Status != doctor.StatusWarning {
		t.Fatalf("fixture sanity: doctor order-outcome-healthy status = %v, want StatusWarning (fixture drifted off the 3-consecutive-failure threshold)", wantResult.Status)
	}

	sp := runtime.NewFake()
	store := beads.NewMemStore()
	var stderr bytes.Buffer
	snapshot := collectCityStatusSnapshot(sp, cfg, cityPath, store, &stderr)

	order := findCityStatusOrder(t, snapshot.Orders, wantResult.Name)
	if order.Message != wantResult.Message {
		t.Fatalf("Orders[%s].Message = %q, want %q (gc status and gc doctor must never disagree)", wantResult.Name, order.Message, wantResult.Message)
	}
	if order.Severity != doctor.SeverityAdvisory {
		t.Fatalf("Orders[%s].Severity = %v, want SeverityAdvisory (a failing order must not gate gc status)", wantResult.Name, order.Severity)
	}

	var stdout bytes.Buffer
	renderCityStatusText(snapshot, newDrainOps(sp), &stdout)
	if !strings.Contains(stdout.String(), wantResult.Message) {
		t.Fatalf("stdout = %q, want it to contain doctor's message %q", stdout.String(), wantResult.Message)
	}
}

// TestCityStatusOrdersSurfacesGateSuppression is the first consumer of
// events.OrderSuppressed (emitted by order_dispatch.go's
// noteOpenWorkSuppressed) anywhere in the repo. Neither doctor check reads
// this event type, so this signal only reaches gc status through the new
// bounded tail-read of events.jsonl this bead adds.
func TestCityStatusOrdersSurfacesGateSuppression(t *testing.T) {
	cityPath, cfg := cityStatusOrderTestCity(t)

	now := time.Now().UTC()
	firstSuppressed := now.Add(-90 * time.Minute).Format(time.RFC3339)
	payload := events.OrderSuppressedPayload{
		OrderName:       "watched-order",
		Consecutive:     12,
		FirstSuppressed: firstSuppressed,
		SuppressedForMS: 90 * 60 * 1000,
	}
	writeCityStatusOrderEvents(t, cityPath,
		events.Event{
			Type:    events.OrderSuppressed,
			Actor:   "controller",
			Subject: "watched-order",
			Ts:      now,
			Message: "open-work gate has suppressed this order for 12 consecutive dispatch checks",
			Payload: events.OrderSuppressedPayloadJSON(payload),
		},
	)

	sp := runtime.NewFake()
	store := beads.NewMemStore()
	var stderr bytes.Buffer
	snapshot := collectCityStatusSnapshot(sp, cfg, cityPath, store, &stderr)

	order := findCityStatusOrder(t, snapshot.Orders, "watched-order")
	if order.Consecutive != 12 {
		t.Fatalf("Orders[watched-order].Consecutive = %d, want 12", order.Consecutive)
	}
	if order.FirstSuppressed != firstSuppressed {
		t.Fatalf("Orders[watched-order].FirstSuppressed = %q, want %q", order.FirstSuppressed, firstSuppressed)
	}

	var stdout bytes.Buffer
	renderCityStatusText(snapshot, newDrainOps(sp), &stdout)
	if !strings.Contains(stdout.String(), "watched-order") {
		t.Fatalf("stdout missing suppressed order name:\n%s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "12") {
		t.Fatalf("stdout missing consecutive-suppression count:\n%s", stdout.String())
	}
}

// TestCityStatusOrdersFallsBackToOrderRunHistoryOutsideEventTail is the
// round-1 request-changes fix for ga-eua7hl: collectCityStatusOrders must
// wire doctor.WithOrderFiringCurrentLastRunFunc the same way cmd_doctor.go's
// buildDoctorChecks does (see TestBuildDoctorChecksOrderFiringCurrentUsesOrderRunHistory
// in cmd_doctor_order_firing_test.go, whose fixture this mirrors). Without
// that option, an order whose only event.jsonl evidence is stale (or,
// on a busy city, has scrolled entirely outside the bounded
// orderFiringEventTailLimit-line tail read) has no fallback, and
// collectCityStatusOrders silently disagrees with gc doctor -- which does
// have the authoritative order-run-history lookup -- by reporting a false
// stale/error for an order that is actually fine.
//
// Unlike the other tests in this file, this one needs a real on-disk bead
// store (not beads.NewMemStore()): the LastRunFunc under test
// (doctorOrderFiringCurrentLastRunFunc) resolves order-run history through
// cachedOrderHistoryStoresResolver, which opens the city's actual scoped
// file store.
func TestCityStatusOrdersFallsBackToOrderRunHistoryOutsideEventTail(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_BEADS_SCOPE_ROOT", "")

	cityPath := t.TempDir()
	t.Chdir(cityPath)
	mustWriteDoctorOrderFiringTestFile(t, filepath.Join(cityPath, "city.toml"), `[workspace]
name = "test-city"
`)
	if err := os.MkdirAll(filepath.Join(cityPath, "orders"), 0o755); err != nil {
		t.Fatal(err)
	}
	formulasDir := filepath.Join(cityPath, "formulas")
	if err := os.MkdirAll(formulasDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeCityStatusOrderTOML(t, cityPath, "mol-dog-stale-db", "cron", "")
	// writeCityStatusOrderTOML only handles the [order].interval field; this
	// check's cron order needs [order].schedule instead, so append it directly.
	appendToFile(t, filepath.Join(cityPath, "orders", "mol-dog-stale-db.toml"), "schedule = \"0 */4 * * *\"\n")

	if err := ensureScopedFileStoreLayout(cityPath); err != nil {
		t.Fatalf("ensureScopedFileStoreLayout: %v", err)
	}
	if err := ensurePersistedScopeLocalFileStore(cityPath); err != nil {
		t.Fatalf("ensurePersistedScopeLocalFileStore: %v", err)
	}
	store, err := openStoreAtForCity(cityPath, cityPath)
	if err != nil {
		t.Fatalf("openStoreAtForCity: %v", err)
	}
	// The authoritative signal: a fresh order-run bead, created "now". This is
	// what the LastRunFunc fallback must find once the event-tail evidence is
	// deemed insufficient.
	if _, err := store.Create(beads.Bead{
		Title:  "manual run mol-dog-stale-db",
		Type:   "molecule",
		Labels: []string{"order-run:mol-dog-stale-db"},
	}); err != nil {
		t.Fatalf("create recent order-run bead: %v", err)
	}

	now := time.Now().UTC()
	// The only event-tail evidence is 13h old against a 4h cron interval --
	// stale enough (> 1.5x interval) that eventEvidenceSuffices rejects it,
	// which is exactly the condition (also reached when a firing has scrolled
	// outside the bounded tail entirely) that must trigger the LastRunFunc
	// fallback rather than reporting the raw stale event.
	writeCityStatusOrderEvents(t, cityPath,
		events.Event{Type: events.ControllerStarted, Ts: now.Add(-24 * time.Hour)},
		events.Event{Type: events.OrderFired, Subject: "mol-dog-stale-db", Ts: now.Add(-13 * time.Hour)},
	)

	cfg := &config.City{
		Workspace:     config.Workspace{Name: "test-city"},
		FormulaLayers: config.FormulaLayers{City: []string{formulasDir}},
	}

	// Ground truth: doctor's own check, wired with the exact same LastRunFunc
	// option cmd_doctor.go's buildDoctorChecks uses (and
	// TestBuildDoctorChecksOrderFiringCurrentUsesOrderRunHistory already
	// proves resolves to StatusOK on this fixture shape).
	var doctorStderr bytes.Buffer
	wantResult := doctor.NewOrderFiringCurrentCheck(cfg, cityPath,
		doctor.WithOrderFiringCurrentLastRunFunc(doctorOrderFiringCurrentLastRunFunc(cityPath, cfg, &doctorStderr)),
	).Run(&doctor.CheckContext{CityPath: cityPath})
	if wantResult.Status != doctor.StatusOK {
		t.Fatalf("fixture sanity: doctor order-firing-current status = %v, want StatusOK (order-run history is fresh); msg = %s; stderr = %s", wantResult.Status, wantResult.Message, doctorStderr.String())
	}

	sp := runtime.NewFake()
	var stderr bytes.Buffer
	snapshot := collectCityStatusSnapshot(sp, cfg, cityPath, store, &stderr)

	// collectCityStatusOrders drops StatusOK results entirely (see the
	// "continue" above), so the healthy-per-doctor check must produce no
	// Orders entry at all. cityStatusOrder.Name is the *check's* name
	// (doctor.CheckResult.Name, e.g. "order-firing-current") for rows from
	// this loop -- not the order's own name -- matching how
	// TestCityStatusOrdersSurfacesStaleFiring/RepeatedFailures look rows up
	// via findCityStatusOrder(t, snapshot.Orders, wantResult.Name).
	//
	// Without the LastRunFunc wired, lastRun is nil, eventEvidenceSuffices
	// still rejects the 13h-old event against the 4h interval, and
	// latestOrderFiredAtUsing returns that stale time unmodified --
	// classifying the check as genuinely CRITICAL-stale and producing a
	// spurious "order-firing-current" entry here, silently disagreeing with
	// gc doctor's StatusOK (asserted above via wantResult).
	for _, o := range snapshot.Orders {
		if o.Name == wantResult.Name {
			t.Fatalf("Orders contains %+v for check %q, want no entry; gc doctor reports %v via order-run history, so gc status must too, not surface it as unhealthy; stderr = %s", o, wantResult.Name, wantResult.Status, stderr.String())
		}
	}
}

// TestCityStatusOrdersIgnoresStaleGateSuppression pins the expiry side of the
// suppression read. Recovery is silent -- clearOpenWorkSuppression drops the
// in-memory streak and emits nothing -- so the last order.suppressed line
// stays in events.jsonl forever. Without a recency window, one order that was
// wedged for twenty ticks last month produces an Orders row on every gc status
// from then on. A live streak re-alerts every orderOpenWorkSuppressionRepeat,
// so an event older than orderSuppressionRecencyWindow can only describe a
// streak that has already ended.
func TestCityStatusOrdersIgnoresStaleGateSuppression(t *testing.T) {
	cityPath, cfg := cityStatusOrderTestCity(t)

	now := time.Now().UTC()
	stale := now.Add(-orderSuppressionRecencyWindow - time.Hour)
	payload := events.OrderSuppressedPayload{
		OrderName:       "long-recovered-order",
		Consecutive:     31,
		FirstSuppressed: stale.Add(-time.Hour).Format(time.RFC3339),
		SuppressedForMS: 60 * 60 * 1000,
	}
	writeCityStatusOrderEvents(t, cityPath,
		events.Event{
			Type:    events.OrderSuppressed,
			Actor:   "controller",
			Subject: "long-recovered-order",
			Ts:      stale,
			Message: "open-work gate has suppressed this order for 31 consecutive dispatch checks",
			Payload: events.OrderSuppressedPayloadJSON(payload),
		},
	)

	sp := runtime.NewFake()
	store := beads.NewMemStore()
	var stderr bytes.Buffer
	snapshot := collectCityStatusSnapshot(sp, cfg, cityPath, store, &stderr)

	for _, o := range snapshot.Orders {
		if o.Name == "long-recovered-order" {
			t.Fatalf("Orders contains %+v for a suppression event %s old, want no row (the streak ended; recovery emits no event to clear it)", o, orderSuppressionRecencyWindow+time.Hour)
		}
	}

	var stdout bytes.Buffer
	renderCityStatusText(snapshot, newDrainOps(sp), &stdout)
	if strings.Contains(stdout.String(), "long-recovered-order") {
		t.Fatalf("stdout = %q, want no mention of an expired suppression streak", stdout.String())
	}
}

// TestCityStatusOrdersDedupesRepeatedGateSuppression exercises the
// newest-wins branch of the per-order dedupe, which no other test reaches: a
// wedged order re-alerts every orderOpenWorkSuppressionRepeat, so several
// events for the same order sit in the tail window and only the newest
// Consecutive/FirstSuppressed describe the current streak.
// ReadFilteredTail returns events oldest-first, so the loop's overwrite
// yields the newest -- this test is what pins that direction.
func TestCityStatusOrdersDedupesRepeatedGateSuppression(t *testing.T) {
	cityPath, cfg := cityStatusOrderTestCity(t)

	now := time.Now().UTC()
	firstSuppressed := now.Add(-150 * time.Minute).Format(time.RFC3339)
	suppressionEvent := func(ts time.Time, consecutive int) events.Event {
		payload := events.OrderSuppressedPayload{
			OrderName:       "wedged-order",
			Consecutive:     consecutive,
			FirstSuppressed: firstSuppressed,
			SuppressedForMS: now.Sub(ts).Milliseconds(),
		}
		return events.Event{
			Type:    events.OrderSuppressed,
			Actor:   "controller",
			Subject: "wedged-order",
			Ts:      ts,
			Message: fmt.Sprintf("open-work gate has suppressed this order for %d consecutive dispatch checks", consecutive),
			Payload: events.OrderSuppressedPayloadJSON(payload),
		}
	}
	writeCityStatusOrderEvents(t, cityPath,
		suppressionEvent(now.Add(-120*time.Minute), 20),
		suppressionEvent(now.Add(-60*time.Minute), 140),
		suppressionEvent(now.Add(-2*time.Minute), 260),
	)

	sp := runtime.NewFake()
	store := beads.NewMemStore()
	var stderr bytes.Buffer
	snapshot := collectCityStatusSnapshot(sp, cfg, cityPath, store, &stderr)

	var rows int
	for _, o := range snapshot.Orders {
		if o.Name == "wedged-order" {
			rows++
		}
	}
	if rows != 1 {
		t.Fatalf("got %d Orders rows for wedged-order, want exactly 1 (three events, one live streak): %+v", rows, snapshot.Orders)
	}

	order := findCityStatusOrder(t, snapshot.Orders, "wedged-order")
	if order.Consecutive != 260 {
		t.Fatalf("Orders[wedged-order].Consecutive = %d, want 260 (the newest event's count)", order.Consecutive)
	}
	if order.FirstSuppressed != firstSuppressed {
		t.Fatalf("Orders[wedged-order].FirstSuppressed = %q, want %q", order.FirstSuppressed, firstSuppressed)
	}

	var stdout bytes.Buffer
	renderCityStatusText(snapshot, newDrainOps(sp), &stdout)
	if strings.Count(stdout.String(), "wedged-order") != 1 {
		t.Fatalf("stdout = %q, want wedged-order rendered exactly once", stdout.String())
	}
	if !strings.Contains(stdout.String(), "260") {
		t.Fatalf("stdout = %q, want the newest consecutive-suppression count", stdout.String())
	}
}

// appendToFile appends content to an existing file, used where
// writeCityStatusOrderTOML's fixed [order] shape doesn't cover a field
// (here, cron's "schedule" vs. cooldown's "interval").
func appendToFile(t *testing.T, path, content string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open %s for append: %v", path, err)
	}
	defer func() {
		if err := f.Close(); err != nil {
			t.Fatalf("close %s: %v", path, err)
		}
	}()
	if _, err := f.WriteString(content); err != nil {
		t.Fatalf("append to %s: %v", path, err)
	}
}
