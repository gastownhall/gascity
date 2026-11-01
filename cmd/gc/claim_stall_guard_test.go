package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/workladder"
)

const (
	stallTemplate = "gascity/review-codex"
	stallProvider = "cursor-grok"
	otherTemplate = "gascity/review-claude"
)

// claimStallHarness drives a guard on a fake clock: seats come and go as the
// session-bead notifications the journal carries, the demand passes call
// withhold with the routed work each template waits on, and the alerts the
// guard records are kept.
type claimStallHarness struct {
	t      *testing.T
	guard  *claimStallGuard
	now    time.Time
	seats  int
	alerts []events.Event
	stderr bytes.Buffer
	// demand is each template's routed work, in ready order.
	demand map[string][]string
}

func newClaimStallHarness(t *testing.T) *claimStallHarness {
	t.Helper()
	h := &claimStallHarness{t: t, now: time.Date(2026, 10, 31, 7, 0, 0, 0, time.UTC), demand: map[string][]string{}}
	h.guard = newClaimStallGuard(func(e events.Event) { h.alerts = append(h.alerts, e) }, &h.stderr)
	h.guard.now = func() time.Time { return h.now }
	h.guard.configure(stallConfig(stallProvider), true)
	return h
}

func stallConfig(provider string) *config.City {
	return &config.City{Agents: []config.Agent{
		{Name: "review-codex", Dir: "gascity", Provider: provider},
		{Name: "review-claude", Dir: "gascity", Provider: "opus"},
	}}
}

func (h *claimStallHarness) advance(d time.Duration) { h.now = h.now.Add(d) }

// seatEvent feeds one session-bead notification about id, stamped now.
func (h *claimStallHarness) seatEvent(typ, id, template string, created time.Time, md map[string]string) {
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

// seat is a live seat of template spawned now for trigger, as the journal
// shows it: its row created, then woken a few seconds later.
type stallSeat struct {
	id, template, trigger string
	created               time.Time
}

func (h *claimStallHarness) spawn(template, trigger string) stallSeat {
	h.t.Helper()
	h.seats++
	s := stallSeat{id: fmt.Sprintf("gcg-session-%d", h.seats), template: template, trigger: trigger, created: h.now}
	h.seatEvent(events.BeadCreated, s.id, template, s.created, map[string]string{beadmeta.TriggerBeadIDMetadataKey: trigger, "state": "start-pending"})
	return s
}

func (h *claimStallHarness) wake(s stallSeat) {
	h.t.Helper()
	h.seatEvent(events.BeadUpdated, s.id, s.template, s.created, map[string]string{
		beadmeta.TriggerBeadIDMetadataKey: s.trigger, "state": "awake", "creation_complete_at": h.now.UTC().Format(time.RFC3339),
	})
}

func (h *claimStallHarness) claim(s stallSeat) {
	h.t.Helper()
	cell := fmt.Sprintf(`{"v":1,"claims":[{"store":"city","bead":%q,"gen":2,"at":%q,"via":"claim"}]}`, s.trigger, h.now.UTC().Format(time.RFC3339Nano))
	h.seatEvent(events.BeadUpdated, s.id, s.template, s.created, map[string]string{
		beadmeta.TriggerBeadIDMetadataKey: s.trigger, "state": "awake", session.WorkStartedKey: cell,
	})
}

func (h *claimStallHarness) close(s stallSeat, state string, awake time.Time) {
	h.t.Helper()
	md := map[string]string{beadmeta.TriggerBeadIDMetadataKey: s.trigger, "state": state}
	if !awake.IsZero() {
		md["creation_complete_at"] = awake.UTC().Format(time.RFC3339)
	}
	h.seatEvent(events.BeadClosed, s.id, s.template, s.created, md)
}

// failCreate is a seat of the stalling template that failed to start:
// created and closed failed-create.
func (h *claimStallHarness) failCreate(trigger string) {
	h.t.Helper()
	s := h.spawn(stallTemplate, trigger)
	h.close(s, "failed-create", time.Time{})
}

func (h *claimStallHarness) escalate(s stallSeat) {
	h.t.Helper()
	payload, err := json.Marshal(events.WorkLadderPayload{SessionID: s.id, Template: s.template, Provider: stallProvider, BeadID: s.trigger, Kind: "trigger", Rung: "assigned", Reason: workladder.AlertTriggerUnanswered})
	if err != nil {
		h.t.Fatal(err)
	}
	h.guard.observe(events.Event{Type: events.WorkEscalated, Ts: h.now, Subject: s.trigger, SessionID: s.id, Payload: payload})
}

// pass runs one demand pass over h.demand and returns what the guard leaves
// of template's demand.
func (h *claimStallHarness) pass(template string) []string {
	h.t.Helper()
	counts := map[string]int{}
	demand := map[string]scaleCheckDemand{}
	for name, ids := range h.demand {
		counts[name] = len(ids)
		demand[name] = scaleCheckDemand{Count: len(ids), WorkBeadIDs: append([]string(nil), ids...)}
	}
	gotCounts, gotDemand := h.guard.withhold(counts, demand)
	if got := gotCounts[template]; got != len(gotDemand[template].WorkBeadIDs) || got != gotDemand[template].Count {
		h.t.Fatalf("count %d, demand %+v: the count and the bead list disagree", got, gotDemand[template])
	}
	return gotDemand[template].WorkBeadIDs
}

// overdueSeat is a seat of the stalling template spawned for trigger that
// wakes and then never claims: the demand pass after the claim deadline
// counts it.
func (h *claimStallHarness) overdueSeat(trigger string) stallSeat {
	h.t.Helper()
	s := h.spawn(stallTemplate, trigger)
	h.advance(30 * time.Second)
	h.wake(s)
	h.pass(stallTemplate)
	h.advance(claimStallDeadline)
	h.pass(stallTemplate)
	return s
}

func (h *claimStallHarness) stalls() []events.Event { return h.ofType(events.AgentClaimsStalled) }

func (h *claimStallHarness) ofType(typ string) []events.Event {
	var out []events.Event
	for _, e := range h.alerts {
		if e.Type == typ {
			out = append(out, e)
		}
	}
	return out
}

// workEvent feeds one notification about a work bead, as the cache records
// it: its status and, once claimed, the claiming session.
func (h *claimStallHarness) workEvent(typ, id, status, sessionID string) {
	h.t.Helper()
	h.workEventBead(typ, beads.Bead{ID: id, Title: id, Status: status, Type: "task", Metadata: map[string]string{}}, sessionID)
}

// workEventBead feeds one notification about b, with the claiming session
// in gc.session_id when sessionID is set.
func (h *claimStallHarness) workEventBead(typ string, b beads.Bead, sessionID string) {
	h.t.Helper()
	if sessionID != "" {
		b.Metadata[beadmeta.SessionIDMetadataKey] = sessionID
	}
	payload, err := beads.EncodeBeadEventPayload(b)
	if err != nil {
		h.t.Fatal(err)
	}
	h.guard.observe(events.Event{Type: typ, Ts: h.now, Subject: b.ID, SessionID: sessionID, Payload: payload})
}

func stallPayload(t *testing.T, e events.Event) events.AgentClaimsPayload {
	t.Helper()
	var p events.AgentClaimsPayload
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		t.Fatalf("payload of %s: %v", e.Type, err)
	}
	return p
}

// TestClaimStallGuardTripsWhenSeatsInARowNeverClaim is the incident's shape:
// seats of one template wake and never claim the work they were spawned for.
// The third in a row trips the stall: one alert names the template, its
// provider and the hold, the template's routed demand stops counting, and no
// other template's does.
func TestClaimStallGuardTripsWhenSeatsInARowNeverClaim(t *testing.T) {
	h := newClaimStallHarness(t)
	h.demand[stallTemplate] = []string{"step-1", "step-2", "step-3"}
	h.demand[otherTemplate] = []string{"step-9"}
	h.claim(h.spawn(otherTemplate, "step-8")) // the other template's seats claim throughout
	first := h.now.Add(30*time.Second + claimStallDeadline)
	for i, trigger := range h.demand[stallTemplate] {
		h.overdueSeat(trigger)
		if i < claimStallTrip-1 && len(h.stalls()) != 0 {
			t.Fatalf("stalled after %d seat(s), want a stall only at %d", i+1, claimStallTrip)
		}
	}
	stalls := h.stalls()
	if len(stalls) != 1 {
		t.Fatalf("stalls = %d, want exactly one", len(stalls))
	}
	p := stallPayload(t, stalls[0])
	if p.Template != stallTemplate || p.Provider != stallProvider || p.Seats != claimStallTrip ||
		p.Action != events.AgentClaimsHeld || p.Cause != events.AgentClaimsCauseSeats || !p.Since.Equal(first) || p.Until == nil || p.Episode == "" {
		t.Fatalf("payload %+v, want the template, its provider, %d seats held since %v", p, claimStallTrip, first)
	}
	if stalls[0].Subject != stallTemplate || !strings.Contains(stalls[0].Message, stallProvider) {
		t.Fatalf("alert %+v, want it about the template and naming the provider", stalls[0])
	}
	if got := h.pass(stallTemplate); len(got) != 0 {
		t.Fatalf("stalled template's demand %v, want all withheld", got)
	}
	if got := h.pass(otherTemplate); !reflect.DeepEqual(got, []string{"step-9"}) {
		t.Fatalf("another template's demand %v, want it untouched", got)
	}
	if !strings.Contains(h.stderr.String(), "claims-stall guard: "+stallTemplate) {
		t.Fatalf("stderr = %q, want the stall named", h.stderr.String())
	}
}

// A burst of failed creates in one tick (a restart, a runtime hiccup) cannot
// trip a stall by itself: only failures that go on claimStallMinSpan later do.
func TestClaimStallGuardABurstIsOneEvent(t *testing.T) {
	h := newClaimStallHarness(t)
	h.demand[stallTemplate] = []string{"step-1"}
	h.pass(stallTemplate)
	for range 6 {
		h.failCreate("step-1")
	}
	if len(h.stalls()) != 0 {
		t.Fatal("a one-tick burst of failed creates tripped a stall")
	}
	h.advance(claimStallMinSpan)
	h.failCreate("step-1")
	if len(h.stalls()) != 1 {
		t.Fatalf("stalls = %d after failures spread over %v, want one", len(h.stalls()), claimStallMinSpan)
	}
}

// A claim by any seat of the template ends the streak: failures before it
// and after it are not in a row.
func TestClaimStallGuardAClaimResetsTheStreak(t *testing.T) {
	h := newClaimStallHarness(t)
	h.demand[stallTemplate] = []string{"step-1", "step-2", "step-3", "step-4", "step-5"}
	h.overdueSeat("step-1")
	h.overdueSeat("step-2")
	ok := h.spawn(stallTemplate, "step-3")
	h.claim(ok)
	h.demand[stallTemplate] = []string{"step-1", "step-2", "step-4", "step-5"}
	h.overdueSeat("step-4")
	h.overdueSeat("step-5")
	if len(h.stalls()) != 0 {
		t.Fatal("stalled across a claim")
	}
	h.overdueSeat("step-1")
	if len(h.stalls()) != 1 {
		t.Fatalf("stalls = %d, want one at the third failure after the claim", len(h.stalls()))
	}
}

// A seat counts once, however many of its notifications say it failed: the
// ladder's escalation, the deadline and its idle close are one seat.
func TestClaimStallGuardCountsASeatOnce(t *testing.T) {
	h := newClaimStallHarness(t)
	h.demand[stallTemplate] = []string{"step-1", "step-2"}
	for _, trigger := range []string{"step-1", "step-2"} {
		s := h.spawn(stallTemplate, trigger)
		h.advance(30 * time.Second)
		h.wake(s)
		woke := h.now
		h.advance(4*time.Minute + 30*time.Second)
		h.escalate(s)
		h.pass(stallTemplate)
		h.advance(6 * time.Minute)
		h.close(s, "idle", woke)
	}
	if len(h.stalls()) != 0 {
		t.Fatalf("two seats, each failing three ways, tripped a stall: %v", h.stalls())
	}
}

// Seats that did not get their chance are neutral: one closed before the
// claim deadline (a config drift, an operator stop), one whose trigger another
// seat served, and one no demand spawned. None counts, none resets.
func TestClaimStallGuardNeutralSeats(t *testing.T) {
	h := newClaimStallHarness(t)
	h.demand[stallTemplate] = []string{"step-1", "step-2", "step-3"}
	h.overdueSeat("step-1")
	h.overdueSeat("step-2")

	early := h.spawn(stallTemplate, "step-3")
	h.advance(30 * time.Second)
	h.wake(early)
	woke := h.now
	h.advance(time.Minute)
	h.close(early, "drained", woke)

	served := h.spawn(stallTemplate, "step-9") // step-9 is not waiting: another seat took it
	h.advance(30 * time.Second)
	h.wake(served)
	h.advance(claimStallDeadline)
	h.pass(stallTemplate)

	nobody := h.spawn(stallTemplate, "")
	h.advance(30 * time.Second)
	h.wake(nobody)
	h.advance(claimStallDeadline)
	h.pass(stallTemplate)

	if len(h.stalls()) != 0 {
		t.Fatalf("neutral seats tripped a stall: %v", h.stalls())
	}
	h.overdueSeat("step-3")
	if len(h.stalls()) != 1 {
		t.Fatalf("stalls = %d, want the third real failure to trip", len(h.stalls()))
	}
}

// The work ladder's trigger escalation is a failure on its own while the
// trigger waits; an escalation about work that no longer waits is not.
func TestClaimStallGuardCountsTheLaddersEscalation(t *testing.T) {
	h := newClaimStallHarness(t)
	h.demand[stallTemplate] = []string{"step-1", "step-2", "step-3"}
	h.pass(stallTemplate)
	h.escalate(h.spawn(stallTemplate, "step-9")) // step-9 does not wait
	for i := 1; i <= claimStallTrip; i++ {
		s := h.spawn(stallTemplate, fmt.Sprintf("step-%d", i))
		h.advance(5 * time.Minute)
		h.escalate(s)
		if i < claimStallTrip && len(h.stalls()) != 0 {
			t.Fatalf("stalled after %d waiting escalation(s): the one about work that does not wait counted", i)
		}
	}
	if len(h.stalls()) != 1 {
		t.Fatalf("stalls = %d, want one from %d escalations", len(h.stalls()), claimStallTrip)
	}
}

// After the hold runs out one probe seat is admitted. A probe that fails
// doubles the hold and alerts again in the same episode; seats already running
// when the hold began neither lengthen it nor end it; a probe that claims ends
// the episode, lifts the hold and records agent.claims_resumed.
func TestClaimStallGuardProbesAndResumes(t *testing.T) {
	h := newClaimStallHarness(t)
	h.demand[stallTemplate] = []string{"step-1", "step-2", "step-3", "step-4"}
	var old []stallSeat
	for _, trigger := range []string{"step-1", "step-2", "step-3"} {
		old = append(old, h.overdueSeat(trigger))
	}
	stuck := h.spawn(stallTemplate, "step-4")
	h.advance(30 * time.Second)
	h.wake(stuck)
	episode := stallPayload(t, h.stalls()[0]).Episode

	h.advance(claimStallHoldBase - time.Minute)
	if got := h.pass(stallTemplate); len(got) != 0 {
		t.Fatalf("demand %v inside the hold", got)
	}
	// A seat that was running when the hold began fails: it does not
	// lengthen the hold.
	h.close(old[0], "idle", old[0].created.Add(30*time.Second))
	h.advance(time.Minute)
	if got := h.pass(stallTemplate); !reflect.DeepEqual(got, []string{"step-1"}) {
		t.Fatalf("demand %v when the hold ran out, want one probe bead", got)
	}
	if got := h.pass(stallTemplate); len(got) != 0 {
		t.Fatalf("demand %v while the probe reports, want none", got)
	}
	probe := h.overdueSeat("step-1")
	stalls := h.stalls()
	if len(stalls) != 2 {
		t.Fatalf("stalls = %d after the probe failed, want a second alert", len(stalls))
	}
	p := stallPayload(t, stalls[1])
	if p.Cause != events.AgentClaimsCauseProbe || p.Episode != episode || p.Until == nil || p.Until.Sub(h.now) != 2*claimStallHoldBase {
		t.Fatalf("probe alert %+v, want the same episode held %v more", p, 2*claimStallHoldBase)
	}
	_ = probe

	h.advance(2 * claimStallHoldBase)
	if got := h.pass(stallTemplate); len(got) != 1 {
		t.Fatalf("demand %v when the doubled hold ran out, want one probe", got)
	}
	h.advance(time.Minute)
	ok := h.spawn(stallTemplate, "step-1")
	h.claim(ok)
	resumed := h.ofType(events.AgentClaimsResumed)
	if len(resumed) != 1 || stallPayload(t, resumed[0]).Episode != episode || stallPayload(t, resumed[0]).Cause != events.AgentClaimsCauseClaimed {
		t.Fatalf("resumed %v, want one ending episode %s", resumed, episode)
	}
	if got := h.pass(stallTemplate); len(got) != 4 {
		t.Fatalf("demand %v after the claim, want it all counted again", got)
	}
}

// A config change that moves the template to another provider starts a new
// epoch: the stall is forgotten, the demand counts, and seats created before
// the move count for nothing.
func TestClaimStallGuardAProviderMoveStartsAfresh(t *testing.T) {
	h := newClaimStallHarness(t)
	h.demand[stallTemplate] = []string{"step-1", "step-2", "step-3"}
	var old []stallSeat
	for _, trigger := range []string{"step-1", "step-2", "step-3"} {
		old = append(old, h.overdueSeat(trigger))
	}
	if len(h.stalls()) != 1 {
		t.Fatal("no stall")
	}
	lingering := h.spawn(stallTemplate, "step-1")
	h.advance(30 * time.Second)
	h.wake(lingering)
	h.advance(time.Minute)
	h.guard.configure(stallConfig("codex"), true)
	if got := h.pass(stallTemplate); len(got) != 3 {
		t.Fatalf("demand %v after the move, want it all counted", got)
	}
	resumed := h.ofType(events.AgentClaimsResumed)
	if len(resumed) != 1 || stallPayload(t, resumed[0]).Cause != events.AgentClaimsCauseConfigChanged || stallPayload(t, resumed[0]).Provider != stallProvider {
		t.Fatalf("resumed %v, want the move to end the stall on %s", resumed, stallProvider)
	}
	h.advance(claimStallDeadline)
	h.pass(stallTemplate)
	for _, s := range old {
		h.close(s, "idle", s.created.Add(30*time.Second))
	}
	if len(h.stalls()) != 1 {
		t.Fatalf("stalls = %d: seats from before the move counted on the new provider", len(h.stalls()))
	}
	h.overdueSeat("step-1")
	h.overdueSeat("step-2")
	h.overdueSeat("step-3")
	stalls := h.stalls()
	if len(stalls) != 2 || stallPayload(t, stalls[1]).Provider != "codex" {
		t.Fatalf("stalls %v, want a second one on the new provider", stalls)
	}
}

// Without a stall the guard returns the demand it was given, untouched.
func TestClaimStallGuardLeavesHealthyDemandAlone(t *testing.T) {
	h := newClaimStallHarness(t)
	counts := map[string]int{stallTemplate: 2}
	demand := map[string]scaleCheckDemand{stallTemplate: {Count: 2, WorkBeadIDs: []string{"a", "b"}}}
	gotCounts, gotDemand := h.guard.withhold(counts, demand)
	if !reflect.DeepEqual(gotCounts, counts) || !reflect.DeepEqual(gotDemand, demand) {
		t.Fatalf("withhold changed healthy demand: %v %v", gotCounts, gotDemand)
	}
	var nilGuard *claimStallGuard
	if c, d := nilGuard.withhold(counts, demand); !reflect.DeepEqual(c, counts) || !reflect.DeepEqual(d, demand) {
		t.Fatal("a nil guard changed the demand")
	}
}

// The journal keeps the waiting work current between demand passes: work that
// left the open state (a canceled run's steps closed, another seat took it)
// does not wait, even while a reused demand snapshot still lists it, so the
// seats spawned for it are not failures.
func TestClaimStallGuardFollowsTheWorkBeads(t *testing.T) {
	h := newClaimStallHarness(t)
	h.demand[stallTemplate] = []string{"step-1", "step-2", "step-3"}
	h.pass(stallTemplate)
	var seats []stallSeat
	for _, trigger := range h.demand[stallTemplate] {
		s := h.spawn(stallTemplate, trigger)
		h.advance(30 * time.Second)
		h.wake(s)
		seats = append(seats, s)
	}
	for _, id := range []string{"step-1", "step-2", "step-3"} {
		h.workEvent(events.BeadClosed, id, "closed", "")
	}
	h.advance(claimStallDeadline)
	h.pass(stallTemplate) // a snapshot read before the closes still lists them
	for _, s := range seats {
		h.close(s, "idle", s.created.Add(30*time.Second))
	}
	if len(h.stalls()) != 0 {
		t.Fatalf("seats whose work closed under them tripped a stall: %v", h.stalls())
	}
}

// A work bead moving to in_progress under a seat's session id is that seat's
// claim, before its session bead says so: it ends the streak, and the episode.
func TestClaimStallGuardAWorkBeadClaimCounts(t *testing.T) {
	h := newClaimStallHarness(t)
	h.demand[stallTemplate] = []string{"step-1", "step-2", "step-3", "step-4"}
	for _, trigger := range []string{"step-1", "step-2", "step-3"} {
		h.overdueSeat(trigger)
	}
	if len(h.stalls()) != 1 {
		t.Fatal("no stall")
	}
	h.advance(claimStallHoldBase)
	if got := h.pass(stallTemplate); len(got) != 1 {
		t.Fatalf("demand %v, want one probe", got)
	}
	probe := h.spawn(stallTemplate, "step-1")
	h.advance(30 * time.Second)
	h.wake(probe)
	h.workEvent(events.BeadUpdated, "step-1", "in_progress", probe.id)
	if resumed := h.ofType(events.AgentClaimsResumed); len(resumed) != 1 {
		t.Fatalf("resumed %v, want the work bead's claim to end the stall", resumed)
	}
	if got := h.pass(stallTemplate); len(got) != 4 {
		t.Fatalf("demand %v after the claim, want all of it counted again", got)
	}
}

// A streak is bounded in time: failures more than claimStallWindow apart are
// not a dead provider, they are unrelated bad luck.
func TestClaimStallGuardStreakWindow(t *testing.T) {
	h := newClaimStallHarness(t)
	h.demand[stallTemplate] = []string{"step-1", "step-2", "step-3", "step-4"}
	h.overdueSeat("step-1")
	h.overdueSeat("step-2")
	h.advance(claimStallWindow)
	h.overdueSeat("step-3")
	if len(h.stalls()) != 0 {
		t.Fatal("failures more than a window apart tripped a stall")
	}
	h.overdueSeat("step-4")
	if len(h.stalls()) != 0 {
		t.Fatal("two failures within the window tripped a stall")
	}
	h.overdueSeat("step-1")
	if len(h.stalls()) != 1 {
		t.Fatalf("stalls = %d, want one at the third failure within the window", len(h.stalls()))
	}
}

// A demand pass that builds one template's demand (the legacy control
// dispatcher's) says nothing about the others: their waiting work stands, and
// a seat's failure after it still counts.
func TestClaimStallGuardAPartialPassKeepsOtherTemplatesWaiting(t *testing.T) {
	h := newClaimStallHarness(t)
	h.demand[stallTemplate] = []string{"step-1", "step-2", "step-3"}
	h.pass(stallTemplate)
	partial := func() {
		h.guard.withhold(map[string]int{"core.control-dispatcher": 1}, map[string]scaleCheckDemand{"core.control-dispatcher": {Count: 1, WorkBeadIDs: []string{"ctl-1"}}})
	}
	for _, trigger := range []string{"step-1", "step-2", "step-3"} {
		s := h.spawn(stallTemplate, trigger)
		partial()
		h.close(s, "failed-create", time.Time{})
		h.advance(time.Minute)
	}
	if len(h.stalls()) != 1 {
		t.Fatalf("stalls = %d: a control-dispatcher-only pass erased the template's waiting work", len(h.stalls()))
	}
}

// A seat first seen already holding a claim made before the stall (after a
// controller restart the journal re-announces live seats) neither resets the
// streak nor ends the stall: only a claim newer than the failures answers
// them.
func TestClaimStallGuardAnOldClaimAnswersNothing(t *testing.T) {
	h := newClaimStallHarness(t)
	h.demand[stallTemplate] = []string{"step-1", "step-2", "step-3", "step-4"}
	old := stallSeat{id: "gcg-session-old", template: stallTemplate, trigger: "step-0", created: h.now.Add(-2 * time.Hour)}
	oldClaim := fmt.Sprintf(`{"v":1,"claims":[{"store":"city","bead":"step-0","gen":2,"at":%q,"via":"claim"}]}`, h.now.Add(-time.Hour).Format(time.RFC3339Nano))
	h.overdueSeat("step-1")
	h.seatEvent(events.BeadUpdated, old.id, old.template, old.created, map[string]string{beadmeta.TriggerBeadIDMetadataKey: "step-0", "state": "awake", session.WorkStartedKey: oldClaim})
	h.overdueSeat("step-2")
	h.overdueSeat("step-3")
	if len(h.stalls()) != 1 {
		t.Fatalf("stalls = %d: an hour-old claim reset the streak", len(h.stalls()))
	}
	again := stallSeat{id: "gcg-session-old-2", template: stallTemplate, trigger: "step-0", created: h.now.Add(-2 * time.Hour)}
	h.seatEvent(events.BeadUpdated, again.id, again.template, again.created, map[string]string{beadmeta.TriggerBeadIDMetadataKey: "step-0", "state": "awake", session.WorkStartedKey: oldClaim})
	if resumed := h.ofType(events.AgentClaimsResumed); len(resumed) != 0 {
		t.Fatalf("resumed %v: a claim from before the stall ended it", resumed)
	}
	if got := h.pass(stallTemplate); len(got) != 0 {
		t.Fatalf("demand %v: the hold was lifted by an old claim", got)
	}
}

// A claim's first write sets the bead's assignee to the seat's session id;
// gc.session_id follows in a second write. The first already counts.
func TestClaimStallGuardAnAssigneeClaimCounts(t *testing.T) {
	h := newClaimStallHarness(t)
	h.demand[stallTemplate] = []string{"step-1", "step-2", "step-3", "step-4"}
	for _, trigger := range []string{"step-1", "step-2", "step-3"} {
		h.overdueSeat(trigger)
	}
	h.advance(claimStallHoldBase)
	h.pass(stallTemplate)
	probe := h.spawn(stallTemplate, "step-4")
	h.advance(30 * time.Second)
	h.wake(probe)
	h.workEventBead(events.BeadUpdated, beads.Bead{ID: "step-4", Title: "step-4", Status: "in_progress", Type: "task", Assignee: probe.id, Metadata: map[string]string{}}, "")
	if resumed := h.ofType(events.AgentClaimsResumed); len(resumed) != 1 {
		t.Fatalf("resumed %v, want the assignee write to count as the probe's claim", resumed)
	}
}

// A failure dated in the past (an overdue seat's is dated at its deadline;
// a legacy demand snapshot is reused for minutes) holds from now: the alert
// never names a hold already over.
func TestClaimStallGuardHoldsFromNow(t *testing.T) {
	h := newClaimStallHarness(t)
	h.demand[stallTemplate] = []string{"step-1", "step-2", "step-3"}
	for _, trigger := range h.demand[stallTemplate] {
		s := h.spawn(stallTemplate, trigger)
		h.advance(time.Minute)
		h.wake(s)
	}
	h.pass(stallTemplate)
	h.advance(claimStallDeadline + claimStallHoldBase)
	h.pass(stallTemplate) // the first pass after the deadlines, long after them
	stalls := h.stalls()
	if len(stalls) != 1 {
		t.Fatalf("stalls = %d, want one", len(stalls))
	}
	if p := stallPayload(t, stalls[0]); p.Until == nil || !p.Until.After(h.now) {
		t.Fatalf("stall %+v at %v: the hold was already over when it was raised", p, h.now)
	}
	if !stalls[0].Ts.Equal(h.now) {
		t.Fatalf("stall recorded at %v, want when the guard noticed it (%v): a gc events --since reader must see it", stalls[0].Ts, h.now)
	}
}

// A held template whose demand this pass is empty (the claim-loop guard
// withheld its one bead) spends no probe on it.
func TestClaimStallGuardSpendsNoProbeOnNothing(t *testing.T) {
	h := newClaimStallHarness(t)
	h.demand[stallTemplate] = []string{"step-1", "step-2", "step-3"}
	for _, trigger := range []string{"step-1", "step-2", "step-3"} {
		h.overdueSeat(trigger)
	}
	h.advance(claimStallHoldBase)
	h.demand[stallTemplate] = nil
	h.guard.withhold(map[string]int{stallTemplate: 0}, map[string]scaleCheckDemand{stallTemplate: {}})
	h.demand[stallTemplate] = []string{"step-1"}
	if got := h.pass(stallTemplate); len(got) != 1 {
		t.Fatalf("demand %v, want the probe admitted when there is work for it", got)
	}
}

// The streak window is the newest failure's: a failure that arrives late but
// is dated more than claimStallWindow before the newest does not complete a
// streak with the fresh ones.
func TestClaimStallGuardWindowIsTheNewestFailures(t *testing.T) {
	h := newClaimStallHarness(t)
	h.demand[stallTemplate] = []string{"step-1", "step-2", "step-3"}
	h.pass(stallTemplate)
	old := h.spawn(stallTemplate, "step-3")
	h.advance(30 * time.Second)
	h.wake(old)
	woke := h.now
	h.advance(claimStallWindow + 5*time.Minute)
	h.failCreate("step-1")
	h.advance(3 * time.Minute)
	h.failCreate("step-2")
	// The old seat's close arrives now, dated by the event; its overdue
	// failure is dated at its deadline, long before the window.
	h.guard.mu.Lock()
	alerts := h.guard.failedLocked(h.guard.seats[old.id], woke.Add(claimStallDeadline))
	h.guard.mu.Unlock()
	if len(alerts) != 0 || len(h.stalls()) != 0 {
		t.Fatalf("a failure dated %v before the newest completed a streak", h.now.Sub(woke.Add(claimStallDeadline)))
	}
}

// A partial pass keeps the other templates' waiting work, and the index that
// follows work beads covers it too: a waiting bead of an unprobed template
// that closes no longer waits.
func TestClaimStallGuardAPartialPassKeepsTheIndex(t *testing.T) {
	h := newClaimStallHarness(t)
	h.demand[stallTemplate] = []string{"step-1"}
	h.pass(stallTemplate)
	h.guard.withhold(map[string]int{"core.control-dispatcher": 1}, map[string]scaleCheckDemand{"core.control-dispatcher": {Count: 1, WorkBeadIDs: []string{"ctl-1"}}})
	h.workEvent(events.BeadClosed, "step-1", "closed", "")
	h.guard.mu.Lock()
	waits := h.guard.templates[stallTemplate].waits("step-1")
	h.guard.mu.Unlock()
	if waits {
		t.Fatal("a closed bead of an unprobed template still waits: the partial pass dropped it from the index")
	}
}

// A seat the guard watched that claims just after it was counted overdue (a
// slow claim, or a claim the journal reports late) answers its own failure.
// Its claim is news when the guard sees it, whatever the hook's clock said.
func TestClaimStallGuardALateClaimAnswersItsOwnFailure(t *testing.T) {
	h := newClaimStallHarness(t)
	h.demand[stallTemplate] = []string{"step-1", "step-2", "step-3"}
	h.overdueSeat("step-1")
	h.overdueSeat("step-2")
	late := h.overdueSeat("step-3")
	if len(h.stalls()) != 1 {
		t.Fatal("no stall")
	}
	h.advance(10 * time.Second)
	skewed := fmt.Sprintf(`{"v":1,"claims":[{"store":"city","bead":"step-3","gen":2,"at":%q,"via":"claim"}]}`, h.now.Add(-30*time.Minute).Format(time.RFC3339Nano))
	h.seatEvent(events.BeadUpdated, late.id, late.template, late.created, map[string]string{beadmeta.TriggerBeadIDMetadataKey: "step-3", "state": "awake", session.WorkStartedKey: skewed})
	if resumed := h.ofType(events.AgentClaimsResumed); len(resumed) != 1 {
		t.Fatalf("resumed %v, want the watched seat's late claim to end the stall despite its lagging clock", resumed)
	}
}
