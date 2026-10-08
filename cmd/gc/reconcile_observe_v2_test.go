package main

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/session"
)

// observeFixture is a gather fixture whose planner runs real trace-only
// passes on a fake clock, records its events and stderr, and keeps every
// pass record it emits.
type observeFixture struct {
	*gatherFixture
	clk     *fakePlannerClock
	ev      *events.Fake
	stderr  *bytes.Buffer
	records []map[string]any
}

func newObserveFixture(t *testing.T, rows ...beads.Bead) *observeFixture {
	t.Helper()
	f := &observeFixture{gatherFixture: newGatherFixture(t, rows...), clk: newFakePlannerClock(gatherNow), ev: events.NewFake(), stderr: &bytes.Buffer{}}
	f.p.clock, f.p.rec, f.p.stderr = f.clk, f.ev, f.stderr
	f.p.pass = func(now time.Time) passResult { return f.p.tracePass(f.env, now) }
	f.p.emitRecord = func(fields map[string]any) { f.records = append(f.records, fields) }
	return f
}

// runPass advances the clock by d and runs one pass.
func (f *observeFixture) runPass(d time.Duration) {
	f.clk.Advance(d)
	f.p.runPass(f.clk.Now())
}

// alerts returns the reconciler.alert events' kinds, in order.
func (f *observeFixture) alerts(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, e := range f.ev.Events {
		if e.Type != events.ReconcilerAlert {
			continue
		}
		var p struct{ Alert string }
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatalf("alert payload %s: %v", e.Payload, err)
		}
		out = append(out, p.Alert)
	}
	return out
}

// v2PassRecordWireFields are the planner's reconcile.pass record fields. They
// are pinned: mdash and the Gate S queries read them by name (B6).
var v2PassRecordWireFields = []string{
	"admitted", "admitted_total", "alerts", "boot_gate", "deferred", "deferred_total", "duration_ms",
	"duration_p99_ms", "duty_1m", "effects", "gap_p99_ms", "idle_respawn_begun", "idle_respawn_completed",
	"idle_respawn_voided", "in_flight", "input_age_ms", "panicked", "panics", "passes", "rows", "settled",
	"stale_self_rekeys", "stop_escalations", "stops_outstanding_30m", "wakes",
}

// TestPassRecordCarriesSoakFields pins the record's wire fields and the
// values Gate S compares with legacy: signaled stops older than 30 minutes,
// the stop-outstanding alerts, StaleSelf rekeys, the idle-respawn drains by
// phase, settlements by outcome and cause, and the alerts.
//
// Kills: a missing or renamed metric the soak criteria need; a stop counted
// before 30 minutes; an idle-respawn series fed by another reason's drain;
// an effect settled at its deadline that does not alert.
func TestPassRecordCarriesSoakFields(t *testing.T) {
	stopPending := func(id string, slot int, age time.Duration) beads.Bead {
		return poolRow(id, "worker", slot, "draining", "state_reason", session.DrainAckStopPendingReason, "drain_at", rowAt(-age))
	}
	f := newObserveFixture(t, stopPending("gc-1", 1, 31*time.Minute), stopPending("gc-2", 2, 29*time.Minute))
	for _, s := range []settlement{
		{Kind: intentRekey, Outcome: settledLanded},
		{Kind: intentRekey, Outcome: settledRefused, Cause: "name-busy"},
		{Kind: intentDrainBeginFresh, Reason: idleRespawnDrainReason, Outcome: settledLanded},
		{Kind: intentDrainBeginFresh, Reason: "idle", Outcome: settledLanded},
		{Kind: intentDrainVoid, Reason: idleRespawnDrainReason, Outcome: settledLanded},
		{Kind: intentStop, Reason: idleRespawnDrainReason, Outcome: settledLanded},
		{Kind: intentStop, Reason: idleRespawnDrainReason, Outcome: settledRefused, Cause: "attached"},
		{Kind: intentStart, Key: rowKey{ID: "gc-3"}, Outcome: settledFailed, Cause: causeDeadline},
	} {
		f.p.settlements.post(s)
	}
	f.p.record(events.Event{Type: events.SessionDrainStopEscalated})
	f.runPass(0)

	if len(f.records) != 1 {
		t.Fatalf("records = %d, want 1", len(f.records))
	}
	rec := f.records[0]
	var names []string
	for k := range rec {
		names = append(names, k)
	}
	slices.Sort(names)
	assertLinesEqual(t, "reconcile.pass fields", names, v2PassRecordWireFields)
	for field, want := range map[string]any{
		"rows": 2, "stops_outstanding_30m": 1, "stop_escalations": uint64(1), "stale_self_rekeys": uint64(1),
		"idle_respawn_begun": uint64(1), "idle_respawn_voided": uint64(1), "idle_respawn_completed": uint64(1),
		"effects": "trace-only", "passes": uint64(1),
	} {
		if rec[field] != want {
			t.Errorf("%s = %v (%T), want %v", field, rec[field], rec[field], want)
		}
	}
	settled := rec["settled"].(map[string]uint64)
	if settled["start/failed/deadline"] != 1 || settled["stop/refused/attached"] != 1 || settled["drain-begin-fresh/landed"] != 2 {
		t.Errorf("settled = %v", settled)
	}
	if got := rec["alerts"].(map[string]uint64); got[alertEffectDeadline] != 1 {
		t.Errorf("alerts = %v, want one %s", got, alertEffectDeadline)
	}
	if got := f.alerts(t); !slices.Equal(got, []string{alertEffectDeadline}) {
		t.Errorf("alert events = %v, want [%s]", got, alertEffectDeadline)
	}
	if ages := rec["input_age_ms"].(map[string]int64); len(ages) == 0 {
		t.Error("input_age_ms is empty")
	} else if _, ok := ages["inventory"]; !ok {
		t.Errorf("input_age_ms = %v, want the inventory's age", ages)
	}
}

// TestPassRecordOnlyOnChangeOrPatrol pins the emission rule: a record when
// the content changes, and one a patrol otherwise.
//
// Kills: a record every pass (30-40k trace events an hour at the pacing
// floor), a change key that holds a timing or the pass count, and a quiet
// city that never records.
func TestPassRecordOnlyOnChangeOrPatrol(t *testing.T) {
	f := newObserveFixture(t, poolRow("gc-1", "worker", 1, "active"))
	for range 5 {
		f.runPass(time.Second)
	}
	if len(f.records) != 1 {
		t.Fatalf("records after 5 unchanged passes = %d, want 1", len(f.records))
	}
	f.p.settlements.post(settlement{Kind: intentRekey, Outcome: settledLanded})
	f.runPass(time.Second)
	if len(f.records) != 2 {
		t.Fatalf("records after a settlement = %d, want 2", len(f.records))
	}
	for range 3 {
		f.runPass(time.Second)
	}
	if len(f.records) != 2 {
		t.Fatalf("records after 3 more unchanged passes = %d, want 2", len(f.records))
	}
	f.runPass(time.Minute) // the fixture's patrol
	if len(f.records) != 3 {
		t.Fatalf("records after a patrol = %d, want 3", len(f.records))
	}
	if got := f.records[2]["passes"]; got != uint64(10) {
		t.Errorf("passes = %v, want 10: every pass is counted, recorded or not", got)
	}
}

// TestAmbiguousCreateBoundClearAlerts pins P5's alert: an ambiguous create
// that the hard bound clears alerts once, naming its identity and leg; one
// whose token reached the census clears silently.
//
// Kills: a silent bound clear, which Gate S requires zero of; an alert on a
// create that landed.
func TestAmbiguousCreateBoundClearAlerts(t *testing.T) {
	f := newObserveFixture(t)
	m := newInflightMap()
	for _, tok := range []string{"tok-lost", "tok-seen"} {
		seq := m.add(inflightEntry{Kind: inflightCreate, Token: tok, Identity: "named:" + tok, Leg: "city:test-city"})
		m.settle(settlement{Kind: inflightCreate, Token: tok, Seq: seq, Outcome: settledAmbiguous, At: gatherNow})
	}
	recs := m.clearVisible(inflightCensus{Tokens: map[string]bool{"tok-seen": true}}, gatherNow.Add(inflightHardBound))
	if len(recs) != 2 {
		t.Fatalf("cleared = %+v, want both", recs)
	}
	f.p.alertCleared(recs)
	if got := f.alerts(t); !slices.Equal(got, []string{alertAmbiguousBound}) {
		t.Fatalf("alerts = %v, want one %s", got, alertAmbiguousBound)
	}
	if e := f.ev.Events[0]; e.Subject != "named:tok-lost" || !strings.Contains(e.Message, `"city:test-city"`) {
		t.Errorf("alert subject %q message %q, want the lost create's identity and leg", e.Subject, e.Message)
	}
	if !strings.Contains(f.stderr.String(), "alert "+alertAmbiguousBound) {
		t.Errorf("stderr = %q, want the alert", f.stderr.String())
	}
}

// TestV2AlertsOncePerEpisode pins each recurring alert to once per episode:
// a run of panicking passes, a row's unknown state, a named duplicate, and a
// boot gate closed for longer than 2 × patrol.
//
// Kills: an alert storm at the pass rate, and an episode that ends without
// re-arming its alert.
func TestV2AlertsOncePerEpisode(t *testing.T) {
	t.Run("pass panic", func(t *testing.T) {
		f := newObserveFixture(t)
		pass := f.p.pass
		f.p.pass = func(time.Time) passResult { panic("boom") }
		f.runPass(time.Second)
		f.runPass(time.Second)
		f.p.pass = pass
		f.runPass(time.Second)
		f.p.pass = func(time.Time) passResult { panic("boom") }
		f.runPass(time.Second)
		if got := f.alerts(t); !slices.Equal(got, []string{alertPassPanic, alertPassPanic}) {
			t.Errorf("alerts = %v, want one per run of panics", got)
		}
	})
	t.Run("unknown state", func(t *testing.T) {
		f := newObserveFixture(t, poolRow("gc-1", "worker", 1, "bogus"))
		f.runPass(time.Second)
		f.runPass(time.Second)
		if got := f.alerts(t); !slices.Equal(got, []string{alertUnknownState}) {
			t.Errorf("alerts = %v, want one %s", got, alertUnknownState)
		}
	})
	t.Run("named duplicate", func(t *testing.T) {
		f := newObserveFixture(t)
		w := f.gather(t)
		for _, dups := range [][]string{{"chat: 2 open rows"}, {"chat: 2 open rows"}, nil, {"chat: 2 open rows"}} {
			f.p.observeWorld(&w, dups, &passCounts{})
		}
		if got := f.alerts(t); !slices.Equal(got, []string{alertNamedDuplicate, alertNamedDuplicate}) {
			t.Errorf("alerts = %v, want one per episode", got)
		}
	})
	t.Run("boot gate closed", func(t *testing.T) {
		f := newObserveFixture(t)
		open := false
		f.p.pass = func(time.Time) passResult {
			f.p.boot = bootState{CachePrimed: true, InventoryComplete: open, RecordingSeen: true}
			return passResult{}
		}
		f.runPass(0)
		f.runPass(2 * time.Minute)
		if got := f.alerts(t); len(got) != 0 {
			t.Fatalf("alerts = %v at 2 × patrol closed, want none until past it", got)
		}
		f.runPass(time.Second)
		f.runPass(5 * time.Minute)
		if got := f.alerts(t); !slices.Equal(got, []string{alertBootGateClosed}) {
			t.Fatalf("alerts = %v, want one once the gate stayed closed past 2 × patrol", got)
		}
		open = true
		f.runPass(time.Second)
		open = false
		f.runPass(time.Second)
		f.runPass(2*time.Minute + time.Second)
		if got := f.alerts(t); len(got) != 2 {
			t.Errorf("alerts = %v, want a second episode's alert", got)
		}
	})
}

// TestDoctorWarnsFloorOutsideControlDispatcher pins ruling 2's doctor
// warning: a floor on any template but the control dispatcher is a v2
// warning (a note under legacy) and never changes the status; and under v2
// the check prints the last pass's age when the controller answers.
//
// Kills: a silent floor-ack difference on a new template; a warning on
// core.control-dispatcher; a warning that changes the exit status; a pass
// age read from a legacy city's socket.
func TestDoctorWarnsFloorOutsideControlDispatcher(t *testing.T) {
	one, two := 1, 2
	agents := []config.Agent{
		{Name: "core.control-dispatcher", Dir: "gascity", MinActiveSessions: &one},
		{Name: "worker", MinActiveSessions: &two},
		{Name: "idle"},
	}
	override := func(k string) (string, bool) { return "1", k == v2SkeletonEnv }
	for _, tc := range []struct{ raw, prefix string }{{"", "v2 note: "}, {"v2", "v2 warning: "}} {
		cfg := &config.City{Daemon: config.DaemonConfig{SessionReconciler: tc.raw}, Agents: agents}
		c := newSessionReconcilerDoctorCheck(cfg, override)
		queried := false
		c.queryPass = func(string) (v2PassStatus, error) {
			queried = true
			return v2PassStatus{Passes: 7, LastPassAgeMS: 1500}, nil
		}
		r := c.Run(&doctor.CheckContext{CityPath: "/city"})
		var floors []string
		for _, d := range r.Details {
			if strings.Contains(d, "min_active_sessions") {
				floors = append(floors, d)
			}
		}
		if len(floors) != 1 || !strings.HasPrefix(floors[0], tc.prefix+"agent worker sets min_active_sessions = 2") || !strings.Contains(floors[0], "stopped and restarted, not kept warm") {
			t.Errorf("%q: floor details = %q, want one %q line for worker", tc.raw, floors, tc.prefix)
		}
		bare := newSessionReconcilerDoctorCheck(&config.City{Daemon: cfg.Daemon}, override)
		bare.queryPass = c.queryPass
		if want := bare.Run(&doctor.CheckContext{}).Status; r.Status != want {
			t.Errorf("%q: status = %v with the floor, want %v as without it", tc.raw, r.Status, want)
		}
		if wantAge := tc.raw == "v2"; queried != wantAge || slices.Contains(r.Details, "v2 last pass record: 1.5s ago (7 passes)") != wantAge {
			t.Errorf("%q: queried %t, details %q; want the pass age only under v2", tc.raw, queried, r.Details)
		}
	}
}

// TestControllerReportsV2PassStatus pins the socket's answer doctor reads:
// the passes and the last pass's age, and zero with no planner.
//
// Kills: an age measured from the first pass, or a legacy controller that
// answers as a v2 one.
func TestControllerReportsV2PassStatus(t *testing.T) {
	p := newPlanner(newFakePlannerClock(plannerT0), func() time.Duration { return time.Minute }, nil, &fakeInflight{}, nil, nil)
	p.metrics.recordPass(plannerT0, time.Second, false, passCounts{})
	p.metrics.recordPass(plannerT0.Add(10*time.Second), time.Second, false, passCounts{})
	got := (&controllerWake{planner: p}).v2PassStatus(plannerT0.Add(20 * time.Second))
	if got != (v2PassStatus{Passes: 2, LastPassAgeMS: 9000}) {
		t.Errorf("status = %+v, want 2 passes, the last ended 9s ago", got)
	}
	if got := (&controllerWake{}).v2PassStatus(plannerT0); got != (v2PassStatus{}) {
		t.Errorf("legacy status = %+v, want zero", got)
	}
}
