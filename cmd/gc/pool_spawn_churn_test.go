package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// spawnChurnHarness drives a pool spawn-churn guard with synthesized
// session-bead notifications against a fake clock.
type spawnChurnHarness struct {
	t        *testing.T
	guard    *poolSpawnChurnGuard
	now      time.Time
	recorded []events.Event
	stderr   strings.Builder
	next     int
}

func newSpawnChurnHarness(t *testing.T) *spawnChurnHarness {
	t.Helper()
	h := &spawnChurnHarness{t: t, now: time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)}
	h.guard = newPoolSpawnChurnGuard(func(e events.Event) { h.recorded = append(h.recorded, e) }, &h.stderr)
	h.guard.now = func() time.Time { return h.now }
	return h
}

func (h *spawnChurnHarness) advance(d time.Duration) { h.now = h.now.Add(d) }

// seatEvent feeds one session-bead notification about id, stamped now.
func (h *spawnChurnHarness) seatEvent(typ, id, template string, created time.Time, md map[string]string) {
	h.t.Helper()
	meta := map[string]string{"template": template}
	for k, v := range md {
		meta[k] = v
	}
	b := beads.Bead{ID: id, Title: id, Status: "open", Type: session.BeadType, CreatedAt: created, Metadata: meta}
	if typ == events.BeadClosed {
		b.Status = "closed"
	}
	payload, err := beads.EncodeBeadEventPayload(b)
	if err != nil {
		h.t.Fatal(err)
	}
	h.guard.observe(events.Event{Type: typ, Ts: h.now, Subject: id, Payload: payload})
}

// blindSeat is the metadata of a pool seat spawned for no work bead.
func blindSeat() map[string]string {
	return map[string]string{"pool_managed": "true"}
}

// awake is md plus a creation_complete_at stamp: the seat woke.
func awake(md map[string]string, at time.Time) map[string]string {
	out := map[string]string{"creation_complete_at": at.UTC().Format(time.RFC3339)}
	for k, v := range md {
		out[k] = v
	}
	return out
}

// spawn creates a seat of template now, with md as its creation metadata.
func (h *spawnChurnHarness) spawn(template string, md map[string]string) (id string, created time.Time) {
	h.t.Helper()
	h.next++
	id, created = fmt.Sprintf("%s-seat-%d", template, h.next), h.now
	h.seatEvent(events.BeadCreated, id, template, created, md)
	return id, created
}

// churn spawns a blind seat of template now, wakes it and closes it with no
// claim: one churned close.
func (h *spawnChurnHarness) churn(template string) string {
	h.t.Helper()
	id, created := h.spawn(template, blindSeat())
	h.seatEvent(events.BeadUpdated, id, template, created, awake(blindSeat(), h.now))
	h.seatEvent(events.BeadClosed, id, template, created, awake(blindSeat(), h.now))
	return id
}

// coolingDowns are the session.pool_spawn_churn_cooling_down records so far.
func (h *spawnChurnHarness) coolingDowns() []events.PoolSpawnChurnPayload {
	h.t.Helper()
	var out []events.PoolSpawnChurnPayload
	for _, e := range h.recorded {
		if e.Type != events.SessionPoolSpawnChurnCoolingDown {
			continue
		}
		var p events.PoolSpawnChurnPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			h.t.Fatal(err)
		}
		if e.Subject != p.Template {
			h.t.Fatalf("event subject %q, payload template %q", e.Subject, p.Template)
		}
		out = append(out, p)
	}
	return out
}

func (h *spawnChurnHarness) held(template string) bool {
	allowed, ok := h.guard.allowance()[template]
	return ok && allowed == 0
}

// Three churned closes, the first and the last 2 min apart, trip the
// template: its blind demand is held for 5 min, and exactly one
// cooling-down event names the template, the seats, the streak's start and
// the hold's end, however many passes read the hold.
func TestPoolSpawnChurnTripsOnThreeChurnedClosesSpanningTwoMinutes(t *testing.T) {
	h := newSpawnChurnHarness(t)
	since := h.now
	h.churn("churny")
	h.advance(time.Minute)
	h.churn("churny")
	if h.held("churny") {
		t.Fatal("held after two churned closes")
	}
	h.advance(time.Minute)
	h.churn("churny")
	if !h.held("churny") {
		t.Fatalf("allowance = %v, want churny held at 0", h.guard.allowance())
	}
	tripped := h.now
	for i := 0; i < 4; i++ {
		h.advance(time.Minute)
		if !h.held("churny") {
			t.Fatalf("hold ended %s after the trip, want 5m", h.now.Sub(tripped))
		}
	}
	got := h.coolingDowns()
	if len(got) != 1 {
		t.Fatalf("cooling-down events = %d, want exactly 1: %+v", len(got), got)
	}
	p := got[0]
	if p.Template != "churny" || p.Cause != events.PoolSpawnChurnCauseTripped || p.Seats != 3 || !p.Since.Equal(since) || p.Episode == "" {
		t.Fatalf("payload = %+v, want churny/tripped/3 seats since %s", p, since)
	}
	if p.Until == nil || !p.Until.Equal(tripped.Add(5*time.Minute)) {
		t.Fatalf("until = %v, want %s", p.Until, tripped.Add(5*time.Minute))
	}
	if !strings.Contains(h.stderr.String(), "churny") {
		t.Fatalf("stderr = %q, want a line naming churny", h.stderr.String())
	}
}

// Two churned closes never trip, however far apart.
func TestPoolSpawnChurnTwoChurnedClosesDoNotTrip(t *testing.T) {
	h := newSpawnChurnHarness(t)
	h.churn("pair")
	h.advance(10 * time.Minute)
	h.churn("pair")
	if got := h.guard.allowance(); got != nil {
		t.Fatalf("allowance = %v, want none", got)
	}
	if len(h.recorded) != 0 {
		t.Fatalf("recorded %d events, want none", len(h.recorded))
	}
}

// Three churned closes inside 2 min are one burst and do not trip; the streak
// goes on, and the close that spans 2 min trips it.
func TestPoolSpawnChurnThreeClosesInsideTwoMinutesDoNotTrip(t *testing.T) {
	h := newSpawnChurnHarness(t)
	for i := 0; i < 3; i++ {
		h.churn("bursty")
		h.advance(30 * time.Second)
	}
	if h.held("bursty") {
		t.Fatal("held after three closes inside 1m")
	}
	h.advance(30 * time.Second)
	h.churn("bursty")
	if !h.held("bursty") {
		t.Fatal("not held after a fourth close 2m after the first")
	}
	if got := h.coolingDowns(); len(got) != 1 || got[0].Seats != 4 {
		t.Fatalf("cooling-down events = %+v, want one with 4 seats", got)
	}
}

// A churned close older than 30 min leaves the streak.
func TestPoolSpawnChurnWindowExpires(t *testing.T) {
	h := newSpawnChurnHarness(t)
	h.churn("churny")
	h.advance(20 * time.Minute)
	h.churn("churny")
	h.advance(11 * time.Minute)
	h.churn("churny")
	if h.held("churny") {
		t.Fatal("held with the first close 31m old")
	}
	h.advance(time.Minute)
	h.churn("churny")
	if !h.held("churny") {
		t.Fatal("not held after three closes within 30m")
	}
}

// spawnChurnDemandCity is two pools whose custom scale_check reports 3 seats
// while one routed work bead waits for each: "churny" and its sibling
// "steady".
func spawnChurnDemandCity(t *testing.T) (*config.City, beads.Store) {
	t.Helper()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "gc"},
		Agents: []config.Agent{
			{Name: "churny", StartCommand: "true", ScaleCheck: "echo 3", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(5)},
			{Name: "steady", StartCommand: "true", ScaleCheck: "echo 3", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(5)},
		},
	}
	store := beads.NewMemStore()
	for _, template := range []string{"churny", "steady"} {
		if _, err := store.Create(beads.Bead{Title: "routed", Type: "task", Status: "open", Metadata: map[string]string{"gc.routed_to": template}}); err != nil {
			t.Fatal(err)
		}
	}
	return cfg, store
}

// tripChurny registers, for cityPath, a guard that has just held "churny"
// on the real clock.
func tripChurny(t *testing.T, cityPath string) {
	t.Helper()
	h := newSpawnChurnHarness(t)
	h.now = time.Now().Add(-3 * time.Minute)
	for i := 0; i < 3; i++ {
		h.churn("churny")
		h.advance(time.Minute)
	}
	h.guard.now = time.Now
	if !h.held("churny") {
		t.Fatalf("guard did not hold churny: %v", h.guard.allowance())
	}
	t.Cleanup(registerPoolSpawnChurnGuard(context.Background(), cityPath, h.guard))
}

// End to end: after three churned blind closes of "churny", both demand
// passes clamp its custom count 3 to its one named work bead, and the planner
// keeps its bound request and emits no blind one; sibling "steady", with the
// same demand, keeps its count and its blind tail.
func TestPoolSpawnChurnHoldWithholdsBlindSpawnsInBothDemandPasses(t *testing.T) {
	cityPath := t.TempDir()
	cfg, store := spawnChurnDemandCity(t)
	tripChurny(t, cityPath)

	legacy := buildDesiredStateWithSessionBeads("gc", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, nil, nil, nil, io.Discard)
	if got := legacy.ScaleCheckCounts["churny"]; got != 1 {
		t.Fatalf("legacy ScaleCheckCounts[churny] = %d, want 1 (its named bead)", got)
	}
	if got := legacy.ScaleCheckCounts["steady"]; got != 3 {
		t.Fatalf("legacy ScaleCheckCounts[steady] = %d, want the custom count 3", got)
	}

	view, err := gatherDemand(demandGatherEnv{CityName: "gc", CityPath: cityPath, Cfg: cfg, CityStore: store}, legacyDemandReads{})
	if err != nil {
		t.Fatalf("gatherDemand: %v", err)
	}
	view.Collected.CustomCounts = map[string]int{"churny": 3, "steady": 3}
	merged := mergeCollectedDemand(cfg, view.Collected)
	states := computePoolDesiredStatesParkedAt(cfg, nil, nil, nil, merged.ScaleCheckCounts, merged.ScaleCheckDemand, time.Now(), nil, nil)
	if blind, bound := blindAndBoundNewRequests(states, "churny"); blind != 0 || bound != 1 {
		t.Fatalf("v2 churny new requests: blind=%d bound=%d, want blind=0 bound=1", blind, bound)
	}
	if blind, bound := blindAndBoundNewRequests(states, "steady"); blind != 2 || bound != 1 {
		t.Fatalf("v2 steady new requests: blind=%d bound=%d, want blind=2 bound=1", blind, bound)
	}
}
