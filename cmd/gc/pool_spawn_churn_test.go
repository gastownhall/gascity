package main

import (
	"context"
	"encoding/json"
	"errors"
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

// claimFailed feeds the hook's bead.claim_failed report for seat id of
// template: its claim write failed, and, if nothing else claims, it drains
// claims_errored.
func (h *spawnChurnHarness) claimFailed(id, template string) {
	h.t.Helper()
	payload, err := json.Marshal(events.BeadClaimFailedPayload{Template: template, SessionID: id, Error: "503 busy"})
	if err != nil {
		h.t.Fatal(err)
	}
	h.guard.observe(events.Event{Type: events.BeadClaimFailed, Ts: h.now, Subject: "gc-work", SessionID: id, Payload: payload})
}

// unrecordedClaim feeds the work.started_unrecorded report the hook writes
// when it delivered a claim to seat id's turn but could not record it in the
// seat's work_started cell (recordHookWorkStarted fails open).
func (h *spawnChurnHarness) unrecordedClaim(id, template string) {
	h.t.Helper()
	rec := events.NewFake()
	emitWorkStartedUnrecordedTo(rec, id, template, "gc-work", errors.New("delivered without a work_started record: sessions store unavailable"))
	if len(rec.Events) != 1 || rec.Events[0].Type != events.WorkStartedUnrecorded {
		h.t.Fatalf("hook recorded %+v, want one work.started_unrecorded", rec.Events)
	}
	evt := rec.Events[0]
	evt.Ts = h.now
	h.guard.observe(evt)
}

// claimedCell is a work_started cell naming one claim at at.
func claimedCell(at time.Time) string {
	return session.EncodeWorkStarted(session.WorkStarted{Claims: []session.WorkStartedEntry{{Store: "city", Bead: "gc-work", Gen: 1, At: at, Via: "claim"}}})
}

// with is md plus kv.
func with(md map[string]string, kv ...string) map[string]string {
	out := map[string]string{}
	for k, v := range md {
		out[k] = v
	}
	for i := 0; i+1 < len(kv); i += 2 {
		out[kv[i]] = kv[i+1]
	}
	return out
}

// Closes that are not blind churn never add to a template's streak: three of
// a kind, 2 min apart, trip nothing.
func TestPoolSpawnChurnIgnoresClosesThatAreNotBlindChurn(t *testing.T) {
	for _, tc := range []struct {
		name string
		// close plays one seat's life, from its creation to its close.
		close func(h *spawnChurnHarness, template string)
	}{
		{"bound seat", func(h *spawnChurnHarness, template string) {
			md := with(blindSeat(), "gc.trigger_bead_id", "gc-work")
			id, created := h.spawn(template, md)
			h.seatEvent(events.BeadClosed, id, template, created, awake(md, h.now))
		}},
		{"stamp cleared mid-life", func(h *spawnChurnHarness, template string) {
			id, created := h.spawn(template, with(blindSeat(), "gc.trigger_bead_id", "gc-work"))
			h.seatEvent(events.BeadUpdated, id, template, created, awake(blindSeat(), h.now))
			h.seatEvent(events.BeadClosed, id, template, created, awake(blindSeat(), h.now))
		}},
		{"claim in work_started", func(h *spawnChurnHarness, template string) {
			id, created := h.spawn(template, blindSeat())
			md := with(awake(blindSeat(), h.now), session.WorkStartedKey, claimedCell(h.now))
			h.seatEvent(events.BeadUpdated, id, template, created, md)
			h.seatEvent(events.BeadClosed, id, template, created, md)
		}},
		{"claim delivered unrecorded", func(h *spawnChurnHarness, template string) {
			id, created := h.spawn(template, blindSeat())
			h.seatEvent(events.BeadUpdated, id, template, created, awake(blindSeat(), h.now))
			h.unrecordedClaim(id, template)
			h.seatEvent(events.BeadClosed, id, template, created, awake(blindSeat(), h.now))
		}},
		{"claims_errored", func(h *spawnChurnHarness, template string) {
			id, created := h.spawn(template, blindSeat())
			h.seatEvent(events.BeadUpdated, id, template, created, awake(blindSeat(), h.now))
			h.claimFailed(id, template)
			h.seatEvent(events.BeadClosed, id, template, created, awake(blindSeat(), h.now))
		}},
		{"failed-create", func(h *spawnChurnHarness, template string) {
			id, created := h.spawn(template, blindSeat())
			h.seatEvent(events.BeadClosed, id, template, created, with(awake(blindSeat(), h.now), "state", "failed-create"))
		}},
		{"named session", func(h *spawnChurnHarness, template string) {
			md := with(blindSeat(), session.NamedSessionMetadataKey, "true")
			id, created := h.spawn(template, md)
			h.seatEvent(events.BeadClosed, id, template, created, awake(md, h.now))
		}},
		{"manual session", func(h *spawnChurnHarness, template string) {
			md := with(blindSeat(), "session_origin", "manual")
			id, created := h.spawn(template, md)
			h.seatEvent(events.BeadClosed, id, template, created, awake(md, h.now))
		}},
		{"legacy manual session", func(h *spawnChurnHarness, template string) {
			md := with(blindSeat(), "manual_session", "true")
			id, created := h.spawn(template, md)
			h.seatEvent(events.BeadClosed, id, template, created, awake(md, h.now))
		}},
		{"never woke", func(h *spawnChurnHarness, template string) {
			id, created := h.spawn(template, blindSeat())
			h.seatEvent(events.BeadClosed, id, template, created, blindSeat())
		}},
		{"not pool managed", func(h *spawnChurnHarness, template string) {
			id, created := h.spawn(template, nil)
			h.seatEvent(events.BeadClosed, id, template, created, awake(nil, h.now))
		}},
		{"creation not observed", func(h *spawnChurnHarness, template string) {
			h.next++
			id, created := fmt.Sprintf("%s-unseen-%d", template, h.next), h.now.Add(-time.Hour)
			h.seatEvent(events.BeadUpdated, id, template, created, awake(blindSeat(), h.now))
			h.seatEvent(events.BeadClosed, id, template, created, awake(blindSeat(), h.now))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newSpawnChurnHarness(t)
			for i := 0; i < 3; i++ {
				tc.close(h, "churny")
				h.advance(time.Minute)
			}
			if got := h.guard.allowance(); got != nil {
				t.Fatalf("allowance = %v, want none", got)
			}
			if len(h.recorded) != 0 {
				t.Fatalf("recorded %d events, want none", len(h.recorded))
			}
			// Nor do they pad a streak of real churn.
			h.churn("churny")
			h.advance(time.Minute)
			h.churn("churny")
			if h.held("churny") {
				t.Fatal("held after two churned closes and three excluded ones")
			}
		})
	}
}

// resumes are the session.pool_spawn_churn_resumed records so far.
func (h *spawnChurnHarness) resumes() []events.PoolSpawnChurnPayload {
	h.t.Helper()
	var out []events.PoolSpawnChurnPayload
	for _, e := range h.recorded {
		if e.Type != events.SessionPoolSpawnChurnResumed {
			continue
		}
		var p events.PoolSpawnChurnPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			h.t.Fatal(err)
		}
		out = append(out, p)
	}
	return out
}

// trip churns three blind seats of template a minute apart and returns when
// the hold ends.
func (h *spawnChurnHarness) trip(template string) time.Time {
	h.t.Helper()
	for i := 0; i < 3; i++ {
		if i > 0 {
			h.advance(time.Minute)
		}
		h.churn(template)
	}
	got := h.coolingDowns()
	if len(got) == 0 || got[len(got)-1].Until == nil {
		h.t.Fatalf("%s did not trip: %+v", template, got)
	}
	return *got[len(got)-1].Until
}

// claim feeds a work_started cell naming a claim at now for seat id.
func (h *spawnChurnHarness) claim(id, template string, created time.Time) {
	h.t.Helper()
	h.seatEvent(events.BeadUpdated, id, template, created, with(awake(blindSeat(), created), session.WorkStartedKey, claimedCell(h.now)))
}

// When the hold ends, one blind seat is admitted: the merge counts the named
// beads plus one, and the planner emits exactly one blind request.
func TestPoolSpawnChurnAdmitsOneProbeSeatWhenTheHoldEnds(t *testing.T) {
	h := newSpawnChurnHarness(t)
	h.now = h.trip("churny")
	if got := h.guard.allowance(); got["churny"] != 1 {
		t.Fatalf("allowance = %v, want churny 1 after the hold", got)
	}
	cfg := blindSpawnDemandCity()
	in := blindSpawnCollected()
	in.BlindSpawnAllowance = h.guard.allowance()
	merged := mergeCollectedDemand(cfg, in)
	states := computePoolDesiredStatesParkedAt(cfg, nil, nil, nil, merged.ScaleCheckCounts, merged.ScaleCheckDemand, h.now, nil, nil)
	if blind, bound := blindAndBoundNewRequests(states, "churny"); blind != 1 || bound != 1 {
		t.Fatalf("churny new requests: blind=%d bound=%d, want blind=1 bound=1", blind, bound)
	}
}

// A probe seat (created at or after the hold's end) that churns doubles the
// hold, up to 30 min, and records the cooling-down event again in the same
// episode with cause probe_failed.
func TestPoolSpawnChurnFailedProbeDoublesTheHoldToTheCap(t *testing.T) {
	h := newSpawnChurnHarness(t)
	h.now = h.trip("churny")
	episode := h.coolingDowns()[0].Episode
	for i, want := range []time.Duration{10 * time.Minute, 20 * time.Minute, 30 * time.Minute, 30 * time.Minute} {
		h.advance(time.Minute)
		h.churn("churny")
		got := h.coolingDowns()
		if len(got) != i+2 {
			t.Fatalf("probe %d: cooling-down events = %d, want %d", i+1, len(got), i+2)
		}
		p := got[len(got)-1]
		if p.Cause != events.PoolSpawnChurnCauseProbeFailed || p.Episode != episode || p.Seats != 4+i {
			t.Fatalf("probe %d: payload = %+v, want probe_failed in episode %s with %d seats", i+1, p, episode, 4+i)
		}
		if p.Until == nil || !p.Until.Equal(h.now.Add(want)) {
			t.Fatalf("probe %d: until = %v, want %s", i+1, p.Until, h.now.Add(want))
		}
		if !h.held("churny") {
			t.Fatalf("probe %d: not held after it failed", i+1)
		}
		h.now = *p.Until
	}
}

// A seat created before the hold ended is not the probe: its churned close,
// however late, changes nothing.
func TestPoolSpawnChurnIgnoresStragglersCreatedBeforeTheHoldEnded(t *testing.T) {
	h := newSpawnChurnHarness(t)
	until := h.trip("straggly")
	h.advance(time.Minute)
	id, created := h.spawn("straggly", blindSeat())
	h.seatEvent(events.BeadUpdated, id, "straggly", created, awake(blindSeat(), h.now))
	h.now = until.Add(time.Minute)
	h.seatEvent(events.BeadClosed, id, "straggly", created, awake(blindSeat(), created))
	if got := h.coolingDowns(); len(got) != 1 {
		t.Fatalf("cooling-down events = %d, want only the trip's", len(got))
	}
	if got := h.guard.allowance(); got["straggly"] != 1 {
		t.Fatalf("allowance = %v, want the probe still admitted", got)
	}
}

// The first claim by a counted seat ends the episode: the resumed event is
// recorded, cause claimed, and the template's full blind count returns.
func TestPoolSpawnChurnResumesOnTheFirstClaim(t *testing.T) {
	h := newSpawnChurnHarness(t)
	h.now = h.trip("churny")
	id, created := h.spawn("churny", blindSeat())
	h.advance(time.Minute)
	h.claim(id, "churny", created)
	if got := h.guard.allowance(); got != nil {
		t.Fatalf("allowance = %v, want none after a claim", got)
	}
	got := h.resumes()
	if len(got) != 1 || got[0].Template != "churny" || got[0].Cause != events.PoolSpawnChurnCauseClaimed || got[0].Episode != h.coolingDowns()[0].Episode {
		t.Fatalf("resumed events = %+v, want one, cause claimed, in the trip's episode", got)
	}
	h.claim(id, "churny", created)
	if len(h.resumes()) != 1 {
		t.Fatal("a repeat claim recorded another resumed event")
	}
}

// A probe seat whose claim the hook delivered without recording it
// (work.started_unrecorded) ends the episode as a recorded claim does, once:
// the template's full blind count returns, and the probe's close is no failed
// probe, so the hold does not double.
func TestPoolSpawnChurnResumesOnAnUnrecordedClaim(t *testing.T) {
	h := newSpawnChurnHarness(t)
	h.now = h.trip("churny")
	id, created := h.spawn("churny", blindSeat())
	h.seatEvent(events.BeadUpdated, id, "churny", created, awake(blindSeat(), h.now))
	h.advance(time.Minute)
	h.unrecordedClaim(id, "churny")
	if got := h.guard.allowance(); got != nil {
		t.Fatalf("allowance = %v, want none after an unrecorded claim", got)
	}
	got := h.resumes()
	if len(got) != 1 || got[0].Template != "churny" || got[0].Cause != events.PoolSpawnChurnCauseClaimed || got[0].Episode != h.coolingDowns()[0].Episode {
		t.Fatalf("resumed events = %+v, want one, cause claimed, in the trip's episode", got)
	}
	h.unrecordedClaim(id, "churny")
	h.advance(time.Minute)
	h.seatEvent(events.BeadClosed, id, "churny", created, awake(blindSeat(), created))
	if len(h.resumes()) != 1 {
		t.Fatal("a repeat unrecorded claim recorded another resumed event")
	}
	if got := h.coolingDowns(); len(got) != 1 {
		t.Fatalf("cooling-down events = %+v, want only the trip's: the probe's close counted as churn", got)
	}
	if got := h.guard.allowance(); got != nil {
		t.Fatalf("allowance = %v after the probe closed, want none", got)
	}
}

// A seat's first claim clears the streak; the same long-lived seat claiming
// again does not.
func TestPoolSpawnChurnRepeatClaimByALongLivedSeatDoesNotReset(t *testing.T) {
	h := newSpawnChurnHarness(t)
	h.churn("longlived")
	h.advance(time.Minute)
	h.churn("longlived")
	id, created := h.spawn("longlived", blindSeat())
	h.claim(id, "longlived", created)
	h.advance(time.Minute)
	h.churn("longlived")
	h.advance(time.Minute)
	h.churn("longlived")
	if h.held("longlived") {
		t.Fatal("held: the first claim did not clear the streak")
	}
	h.advance(time.Minute)
	h.claim(id, "longlived", created)
	h.advance(time.Minute)
	h.churn("longlived")
	if !h.held("longlived") {
		t.Fatal("not held: a repeat claim by the same seat cleared the streak")
	}
}

// spawnChurnConfig is "churny" and a sibling, each with a custom scale_check.
func spawnChurnConfig() *config.City {
	return &config.City{Agents: []config.Agent{
		{Name: "churny", Provider: "claude", ScaleCheck: "echo 3"},
		{Name: "steady", Provider: "claude", ScaleCheck: "echo 3"},
	}}
}

// A config change to how a template's demand is counted or how its seats run
// ends its episode (resumed, cause config_changed) and starts it afresh:
// seats created before the change count for nothing. An unchanged config
// leaves the hold standing; a template removed from the config is forgotten.
func TestPoolSpawnChurnConfigChangeResetsTheTemplate(t *testing.T) {
	h := newSpawnChurnHarness(t)
	h.guard.configure(spawnChurnConfig())
	h.trip("churny")
	h.guard.configure(spawnChurnConfig())
	if !h.held("churny") {
		t.Fatal("an unchanged config ended the hold")
	}
	// Two seats from before the change, still alive.
	var old []string
	var oldCreated []time.Time
	for i := 0; i < 2; i++ {
		id, created := h.spawn("churny", blindSeat())
		h.seatEvent(events.BeadUpdated, id, "churny", created, awake(blindSeat(), h.now))
		old, oldCreated = append(old, id), append(oldCreated, created)
	}
	h.advance(time.Second)
	for _, mutate := range []func(*config.City){
		func(c *config.City) { c.Agents[0].ScaleCheck = "echo 1" },
		func(c *config.City) { c.Agents[0].WorkQuery = "bd ready" },
		func(c *config.City) { c.Agents[0].ScaleCheckAuthoritative = true },
		func(c *config.City) { c.Agents[0].Provider = "codex" },
	} {
		cfg := spawnChurnConfig()
		mutate(cfg)
		h.recorded = nil
		h.guard.configure(spawnChurnConfig())
		if !h.held("churny") {
			h.trip("churny")
		}
		h.recorded = nil
		h.advance(time.Second)
		h.guard.configure(cfg)
		if got := h.guard.allowance(); got != nil {
			t.Fatalf("allowance = %v after a config change, want none", got)
		}
		if got := h.resumes(); len(got) != 1 || got[0].Cause != events.PoolSpawnChurnCauseConfigChanged || got[0].Template != "churny" {
			t.Fatalf("resumed events = %+v, want one, cause config_changed", got)
		}
		h.advance(time.Minute)
	}
	// The pre-change seats churn now, with one new one: no trip.
	for i, id := range old {
		h.seatEvent(events.BeadClosed, id, "churny", oldCreated[i], awake(blindSeat(), oldCreated[i]))
		h.advance(time.Minute)
	}
	h.churn("churny")
	if h.held("churny") {
		t.Fatal("seats created before the config change counted")
	}
	h.recorded = nil
	h.guard.configure(&config.City{Agents: []config.Agent{{Name: "steady", Provider: "claude", ScaleCheck: "echo 3"}}})
	h.advance(time.Minute)
	h.churn("churny")
	h.advance(time.Minute)
	h.churn("churny")
	if h.held("churny") {
		t.Fatal("a removed template kept its streak")
	}
}

// A deterministic control dispatcher's seats never count: they are pool
// managed and never trigger-bound, and they serve control work without
// claiming any and retire when it is drained, so each would read as blind
// churn, though clampBlindSpawnDemand never limits a dispatcher. Two hours of
// city and rig dispatcher retirements build no streak, record no event or
// stderr line, and leave no allowance entry; an ordinary template churning
// beside them still trips.
func TestPoolSpawnChurnNeverCountsAControlDispatcher(t *testing.T) {
	cityDispatcher := config.Agent{Name: config.ControlDispatcherAgentName, BindingName: "core"}
	cityDispatcher.StartCommand = config.ControlDispatcherStartCommandFor(cityDispatcher.QualifiedName())
	rigDispatcher := cityDispatcher
	rigDispatcher.Dir = "gascity"
	rigDispatcher.StartCommand = config.ControlDispatcherStartCommandFor(rigDispatcher.QualifiedName())
	cfg := spawnChurnConfig()
	cfg.Agents = append(cfg.Agents, cityDispatcher, rigDispatcher)
	h := newSpawnChurnHarness(t)
	h.guard.configure(cfg)
	for i := 0; i < 24; i++ {
		for _, a := range []config.Agent{cityDispatcher, rigDispatcher} {
			h.churn(a.QualifiedName())
		}
		h.advance(5 * time.Minute)
	}
	if got := h.guard.allowance(); got != nil {
		t.Fatalf("allowance = %v, want no entry for a control dispatcher", got)
	}
	if len(h.recorded) != 0 || h.stderr.Len() != 0 {
		t.Fatalf("recorded %+v, stderr %q; want nothing for a control dispatcher", h.recorded, h.stderr.String())
	}
	h.trip("churny")
	if got := h.guard.allowance(); len(got) != 1 || got["churny"] != 0 {
		t.Fatalf("allowance = %v, want only churny held", got)
	}
}

// A config change that makes a held template a deterministic control
// dispatcher ends its episode (resumed, cause config_changed), and its seats
// count for nothing from then on.
func TestPoolSpawnChurnConfigChangeToAControlDispatcherEndsItsEpisode(t *testing.T) {
	agent := config.Agent{Name: config.ControlDispatcherAgentName, StartCommand: "true"}
	name := agent.QualifiedName()
	h := newSpawnChurnHarness(t)
	h.guard.configure(&config.City{Agents: []config.Agent{agent}})
	h.trip(name)
	h.recorded = nil
	h.advance(time.Second)
	agent.StartCommand = config.ControlDispatcherStartCommandFor(name)
	h.guard.configure(&config.City{Agents: []config.Agent{agent}})
	if got := h.guard.allowance(); got != nil {
		t.Fatalf("allowance = %v after the change, want none", got)
	}
	if got := h.resumes(); len(got) != 1 || got[0].Template != name || got[0].Cause != events.PoolSpawnChurnCauseConfigChanged {
		t.Fatalf("resumed events = %+v, want one, cause config_changed", got)
	}
	for i := 0; i < 3; i++ {
		h.advance(time.Minute)
		h.churn(name)
	}
	if got := h.guard.allowance(); got != nil || len(h.recorded) != 1 {
		t.Fatalf("allowance = %v, recorded %+v; want the dispatcher's churn ignored", got, h.recorded)
	}
}

// One template's churn neither feeds nor holds another's: closes of A and B
// interleaved keep separate streaks, and while A is held, B's allowance is
// unlimited and the planner emits its full blind tail.
func TestPoolSpawnChurnHoldsOnlyTheChurningTemplate(t *testing.T) {
	h := newSpawnChurnHarness(t)
	h.churn("churny")
	h.advance(time.Minute)
	h.churn("steady")
	h.advance(time.Minute)
	h.churn("churny")
	if got := h.guard.allowance(); got != nil {
		t.Fatalf("allowance = %v after 2+1 closes of two templates, want none", got)
	}
	h.advance(time.Minute)
	h.churn("churny")
	got := h.guard.allowance()
	if _, ok := got["steady"]; ok || got["churny"] != 0 || len(got) != 1 {
		t.Fatalf("allowance = %v, want only churny held", got)
	}
	cfg := blindSpawnDemandCity()
	in := blindSpawnCollected()
	in.BlindSpawnAllowance = got
	merged := mergeCollectedDemand(cfg, in)
	states := computePoolDesiredStatesParkedAt(cfg, nil, nil, nil, merged.ScaleCheckCounts, merged.ScaleCheckDemand, h.now, nil, nil)
	if blind, bound := blindAndBoundNewRequests(states, "steady"); blind != 2 || bound != 1 {
		t.Fatalf("steady new requests: blind=%d bound=%d, want blind=2 bound=1", blind, bound)
	}
	if blind, bound := blindAndBoundNewRequests(states, "churny"); blind != 0 || bound != 1 {
		t.Fatalf("churny new requests: blind=%d bound=%d, want blind=0 bound=1", blind, bound)
	}
	if got := h.coolingDowns(); len(got) != 1 || got[0].Template != "churny" {
		t.Fatalf("cooling-down events = %+v, want one, for churny", got)
	}
}
