package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
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
	h.guard.configure(stallConfig(stallProvider))
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

// claim is s claiming its trigger now: its session bead's work_started cell
// names the claim. It returns the cell.
func (h *claimStallHarness) claim(s stallSeat) string {
	h.t.Helper()
	cell := fmt.Sprintf(`{"v":1,"claims":[{"store":"city","bead":%q,"gen":2,"at":%q,"via":"claim"}]}`, s.trigger, h.now.UTC().Format(time.RFC3339Nano))
	h.cell(s, cell)
	return cell
}

// cell is a notification of s's session bead carrying the work_started cell.
func (h *claimStallHarness) cell(s stallSeat, cell string) {
	h.t.Helper()
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
	h.guard.configure(stallConfig("codex"))
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

// A seat that claimed once and claims again (a continuation session taking its
// next bead) ends the streak again: its first claim does not use up its later
// ones.
func TestClaimStallGuardARepeatedClaimResetsTheStreak(t *testing.T) {
	h := newClaimStallHarness(t)
	h.demand[stallTemplate] = []string{"step-1", "step-2", "step-3", "step-4"}
	h.pass(stallTemplate)
	healthy := h.spawn(stallTemplate, "step-1")
	h.claim(healthy)
	h.overdueSeat("step-2")
	h.overdueSeat("step-3")
	h.advance(time.Second)
	h.claim(healthy)
	h.overdueSeat("step-4")
	if len(h.stalls()) != 0 {
		t.Fatalf("stalled though the seat claimed again between the failures: %v", h.stalls())
	}
}

// A seat that claimed before the stall and claims again after it ends the
// stall, through its session bead's cell or a work bead it takes.
func TestClaimStallGuardARepeatedClaimEndsTheStall(t *testing.T) {
	for name, claimAgain := range map[string]func(h *claimStallHarness, s stallSeat){
		"work_started": func(h *claimStallHarness, s stallSeat) { h.claim(s) },
		"work bead": func(h *claimStallHarness, s stallSeat) {
			h.workEvent(events.BeadUpdated, "step-1", "in_progress", s.id)
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newClaimStallHarness(t)
			h.demand[stallTemplate] = []string{"step-1", "step-2", "step-3", "step-4"}
			h.pass(stallTemplate)
			healthy := h.spawn(stallTemplate, "step-4")
			h.claim(healthy)
			h.advance(time.Minute)
			for _, trigger := range []string{"step-1", "step-2", "step-3"} {
				h.overdueSeat(trigger)
			}
			if len(h.stalls()) != 1 {
				t.Fatalf("stalls = %d, want one", len(h.stalls()))
			}
			h.advance(10 * time.Second)
			claimAgain(h, healthy)
			if resumed := h.ofType(events.AgentClaimsResumed); len(resumed) != 1 {
				t.Fatalf("resumed %v, want the seat's new claim to end the stall", resumed)
			}
			if got := h.pass(stallTemplate); len(got) == 0 {
				t.Fatal("the template's demand is still withheld after its seat claimed again")
			}
		})
	}
}

// Every later write to a seat's session bead carries its work_started cell
// again. A cell that names no newer claim is not one: it neither resets the
// streak nor ends a stall.
func TestClaimStallGuardARepeatedCellIsNoClaim(t *testing.T) {
	h := newClaimStallHarness(t)
	h.demand[stallTemplate] = []string{"step-1", "step-2", "step-3", "step-4"}
	h.pass(stallTemplate)
	healthy := h.spawn(stallTemplate, "step-1")
	cell := h.claim(healthy)
	h.overdueSeat("step-2")
	h.overdueSeat("step-3")
	h.cell(healthy, cell)
	h.overdueSeat("step-4")
	if len(h.stalls()) != 1 {
		t.Fatalf("stalls = %d, want one: an unchanged cell reset the streak", len(h.stalls()))
	}
	h.advance(time.Minute)
	h.cell(healthy, cell)
	if resumed := h.ofType(events.AgentClaimsResumed); len(resumed) != 0 {
		t.Fatalf("resumed %v: an unchanged cell ended the stall", resumed)
	}
}

// An overdue seat's failure is dated at its deadline but counted only at the
// next demand pass. Failures dated before the newest claim are answered by
// it, even when that pass runs after the claim: a delayed pass cannot trip a
// lane that claims again.
func TestClaimStallGuardFailuresBeforeAClaimAreAnswered(t *testing.T) {
	h := newClaimStallHarness(t)
	h.demand[stallTemplate] = []string{"step-1", "step-2", "step-3", "step-4"}
	h.pass(stallTemplate)
	for _, trigger := range []string{"step-1", "step-2", "step-3"} {
		h.wake(h.spawn(stallTemplate, trigger))
		h.advance(time.Minute)
	}
	h.advance(4 * time.Minute)
	h.claim(h.spawn(stallTemplate, "step-4"))
	h.pass(stallTemplate) // the first pass since: three deadlines from before the claim
	if len(h.stalls()) != 0 {
		t.Fatalf("failures dated before a newer claim tripped a stall: %v", h.stalls())
	}
}

// A claim made while the streak is empty still answers a failure dated before
// it and counted after it: seat A's deadline (T+3m30s) passes, seat B claims
// (T+3m40s), and the pass that counts A runs after B's claim. Two more
// failures after the claim are two, not three.
func TestClaimStallGuardAClaimOnAnEmptyStreakAnswersOlderFailures(t *testing.T) {
	h := newClaimStallHarness(t)
	h.demand[stallTemplate] = []string{"step-1", "step-2", "step-3", "step-4"}
	h.pass(stallTemplate)
	a := h.spawn(stallTemplate, "step-1")
	h.advance(30 * time.Second)
	h.wake(a)
	h.advance(claimStallDeadline + 10*time.Second)
	h.claim(h.spawn(stallTemplate, "step-2"))
	h.pass(stallTemplate)
	h.overdueSeat("step-3")
	h.overdueSeat("step-4")
	if len(h.stalls()) != 0 {
		t.Fatalf("a failure dated before the claim completed a streak with two after it: %v", h.stalls())
	}
}

// A claim reported late (a seat first seen carrying a claim older than
// failures already counted) answers only the failures before it: a newer one
// stays in the streak.
func TestClaimStallGuardALateClaimKeepsNewerFailures(t *testing.T) {
	h := newClaimStallHarness(t)
	h.demand[stallTemplate] = []string{"step-1", "step-2", "step-3", "step-4"}
	h.pass(stallTemplate)
	h.overdueSeat("step-1")
	claimedAt := h.now
	h.overdueSeat("step-2")
	unseen := stallSeat{id: "gcg-session-unseen", template: stallTemplate, trigger: "step-9", created: claimedAt.Add(-time.Minute)}
	h.cell(unseen, fmt.Sprintf(`{"v":1,"claims":[{"store":"city","bead":"step-9","gen":2,"at":%q,"via":"claim"}]}`, claimedAt.Format(time.RFC3339Nano)))
	h.overdueSeat("step-3")
	if len(h.stalls()) != 0 {
		t.Fatalf("stalled on %v: the late claim answered nothing", h.stalls())
	}
	h.overdueSeat("step-4")
	if len(h.stalls()) != 1 {
		t.Fatalf("stalls = %d, want one: the late claim erased the failure after it", len(h.stalls()))
	}
}

// A claim is never newer than the notification that carries it: a seat first
// seen with a claim its hook dated ahead of the controller's clock answers no
// failure after that notification.
func TestClaimStallGuardAClaimDatedAheadAnswersNothingLater(t *testing.T) {
	h := newClaimStallHarness(t)
	h.demand[stallTemplate] = []string{"step-1", "step-2", "step-3"}
	h.pass(stallTemplate)
	ahead := stallSeat{id: "gcg-session-ahead", template: stallTemplate, trigger: "step-9", created: h.now}
	h.cell(ahead, fmt.Sprintf(`{"v":1,"claims":[{"store":"city","bead":"step-9","gen":2,"at":%q,"via":"claim"}]}`, h.now.Add(time.Hour).Format(time.RFC3339Nano)))
	for _, trigger := range h.demand[stallTemplate] {
		h.overdueSeat(trigger)
	}
	if len(h.stalls()) != 1 {
		t.Fatalf("stalls = %d, want one: a claim dated an hour ahead answered the failures after it", len(h.stalls()))
	}
}

// The probe's bead counts pass after pass until a seat created since the hold
// ran out wakes: the v2 allocator writes the probe's row in one pass and
// starts it only in a later one that still wants it. Once the probe is awake
// its bead stops counting, so no second seat spawns while the claim deadline
// measures it.
func TestClaimStallGuardKeepsTheProbeAdmittedUntilItWakes(t *testing.T) {
	h := newClaimStallHarness(t)
	h.demand[stallTemplate] = []string{"step-1", "step-2", "step-3", "step-4"}
	for _, trigger := range []string{"step-1", "step-2", "step-3"} {
		h.overdueSeat(trigger)
	}
	h.advance(claimStallHoldBase)
	probe1 := []string{"step-1"}
	if got := h.pass(stallTemplate); !reflect.DeepEqual(got, probe1) {
		t.Fatalf("demand %v when the hold ran out, want one probe bead", got)
	}
	h.advance(15 * time.Second)
	if got := h.pass(stallTemplate); !reflect.DeepEqual(got, probe1) {
		t.Fatalf("demand %v before the probe's row exists, want its bead still counted", got)
	}
	probe := h.spawn(stallTemplate, "step-1")
	h.advance(15 * time.Second)
	if got := h.pass(stallTemplate); !reflect.DeepEqual(got, probe1) {
		t.Fatalf("demand %v while the probe's row waits to start, want its bead still counted", got)
	}
	h.wake(probe)
	if got := h.pass(stallTemplate); len(got) != 0 {
		t.Fatalf("demand %v with the probe awake, want none", got)
	}
}

// A probe that has not woken by the end of claimStallProbeGrace failed: its
// row never started. The hold doubles and the alert is raised again, saying
// so; the row's rollback, when it comes, counts nothing more.
func TestClaimStallGuardAProbeThatNeverStartsFails(t *testing.T) {
	h := newClaimStallHarness(t)
	h.demand[stallTemplate] = []string{"step-1", "step-2", "step-3", "step-4"}
	for _, trigger := range []string{"step-1", "step-2", "step-3"} {
		h.overdueSeat(trigger)
	}
	episode := stallPayload(t, h.stalls()[0]).Episode
	h.advance(claimStallHoldBase)
	h.pass(stallTemplate)
	probe := h.spawn(stallTemplate, "step-1")
	h.advance(claimStallProbeGrace)
	if got := h.pass(stallTemplate); len(got) != 0 {
		t.Fatalf("demand %v when the probe never started, want all withheld", got)
	}
	stalls := h.stalls()
	if len(stalls) != 2 {
		t.Fatalf("stalls = %d, want a second alert for the probe that never started", len(stalls))
	}
	p := stallPayload(t, stalls[1])
	if p.Cause != events.AgentClaimsCauseProbe || p.Episode != episode || p.Until == nil || p.Until.Sub(h.now) != 2*claimStallHoldBase ||
		!strings.Contains(stalls[1].Message, "did not start within "+claimStallProbeGrace.String()) {
		t.Fatalf("alert %+v %q, want the same episode held %v more for a probe that did not start", p, stalls[1].Message, 2*claimStallHoldBase)
	}
	h.close(probe, "failed-create", time.Time{})
	if len(h.stalls()) != 2 {
		t.Fatalf("stalls = %d: the never-started probe's rollback was counted again", len(h.stalls()))
	}
	h.advance(2 * claimStallHoldBase)
	if got := h.pass(stallTemplate); len(got) != 1 {
		t.Fatalf("demand %v when the doubled hold ran out, want the next probe", got)
	}
}

// A store may keep a row's creation time to the second only. The probe's row,
// created in the second the probe was admitted, is still the probe's.
func TestClaimStallGuardAProbeCreatedInItsAdmissionSecondCounts(t *testing.T) {
	h := newClaimStallHarness(t)
	h.demand[stallTemplate] = []string{"step-1", "step-2", "step-3", "step-4"}
	for _, trigger := range []string{"step-1", "step-2", "step-3"} {
		h.overdueSeat(trigger)
	}
	h.advance(claimStallHoldBase + 700*time.Millisecond)
	h.pass(stallTemplate)
	probe := h.spawn(stallTemplate, "step-1")
	probe.created = probe.created.Truncate(time.Second)
	h.advance(30 * time.Second)
	h.wake(probe)
	if got := h.pass(stallTemplate); len(got) != 0 {
		t.Fatalf("demand %v: the probe's row, stored to the second, was not taken for the probe", got)
	}
}

// heldClaimStall is a guard on now's clock whose stall on template tripped
// while demand waited, its hold run out: the next demand pass admits the
// probe. It is registered for cityPath, where both demand passes find it,
// until the test ends.
func heldClaimStall(t *testing.T, cityPath string, cfg *config.City, template string, demand []string, now func() time.Time) *claimStallHarness {
	t.Helper()
	h := newClaimStallHarness(t)
	h.guard.configure(cfg)
	h.now = now().Add(-claimStallHoldBase - 5*time.Minute)
	h.demand = map[string][]string{template: demand}
	h.pass(template)
	for range claimStallTrip {
		h.close(h.spawn(template, demand[0]), "failed-create", time.Time{})
		h.advance(time.Minute)
	}
	if len(h.stalls()) != 1 {
		t.Fatalf("stalls = %d, want %s held", len(h.stalls()), template)
	}
	h.guard.now = now
	t.Cleanup(registerClaimStallGuard(context.Background(), cityPath, h.guard))
	return h
}

// sessionBeadEvent is the journal's notification of the session bead b.
func sessionBeadEvent(t *testing.T, typ string, b beads.Bead, at time.Time) events.Event {
	t.Helper()
	payload, err := beads.EncodeBeadEventPayload(b)
	if err != nil {
		t.Fatal(err)
	}
	return events.Event{Type: typ, Ts: at, Subject: b.ID, Payload: payload}
}

// openSessionBeads is store's open session beads of template.
func openSessionBeads(t *testing.T, store beads.Store, template string) []beads.Bead {
	t.Helper()
	all, err := store.List(beads.ListQuery{AllowScan: true})
	if err != nil {
		t.Fatal(err)
	}
	var out []beads.Bead
	for _, b := range all {
		if b.Type == sessionBeadType && b.Status != "closed" && b.Metadata["template"] == template {
			out = append(out, b)
		}
	}
	return out
}

// simStallJournal feeds a guard one template's session rows from the sim's
// store, as the journal does: each row when it appears, again whenever it
// changes, and when it closes. The store stamps creations with the wall clock
// while the guard runs on the sim clock, so a fed row carries its creation on
// the sim clock instead: the time of the feed that first saw it, which is the
// time of the pass that created it.
type simStallJournal struct {
	t        *testing.T
	s        *sim
	guard    *claimStallGuard
	template string
	order    []string             // the rows, in the order they appeared
	created  map[string]time.Time // each row's creation on the sim clock
	last     map[string]beads.Bead
}

// newSimStallJournal is a journal of template's rows in s for guard, which has
// fed it nothing yet.
func newSimStallJournal(t *testing.T, s *sim, guard *claimStallGuard, template string) *simStallJournal {
	return &simStallJournal{t: t, s: s, guard: guard, template: template, created: map[string]time.Time{}, last: map[string]beads.Bead{}}
}

// feed notifies the guard of every row that appeared, changed or closed since
// the last feed.
func (j *simStallJournal) feed() {
	j.t.Helper()
	all, err := j.s.legs[0].backing.List(beads.ListQuery{AllowScan: true, IncludeClosed: true})
	if err != nil {
		j.t.Fatal(err)
	}
	now := j.s.clk.Now()
	for _, b := range all {
		if b.Type != sessionBeadType || b.Metadata["template"] != j.template {
			continue
		}
		prev, seen := j.last[b.ID]
		if seen && prev.Status == b.Status && maps.Equal(prev.Metadata, b.Metadata) {
			continue
		}
		j.last[b.ID] = beads.Bead{Status: b.Status, Metadata: maps.Clone(b.Metadata)}
		if !seen {
			j.order = append(j.order, b.ID)
			j.created[b.ID] = now
		}
		b.CreatedAt = j.created[b.ID]
		if !seen {
			j.guard.observe(sessionBeadEvent(j.t, events.BeadCreated, b, now))
			if b.Status != "closed" {
				continue
			}
		}
		typ := events.BeadUpdated
		if b.Status == "closed" {
			typ = events.BeadClosed
		}
		j.guard.observe(sessionBeadEvent(j.t, typ, b, now))
	}
}

// TestSimClaimStallProbeStarts runs a held template's probes through the v2
// pass with the real create and start effects, the guard fed the rows as the
// journal feeds them. The pass that admits a probe creates its row; the next
// pass must still count the probe's bead, or the template has no demand, the
// pending create is only kept, and the probe never starts, so the stall never
// ends. Once the guard sees the probe awake, its bead stops counting: no
// second seat is created after the probe's post-create protection lapses. A
// probe that never claims fails at its claim deadline and the hold doubles;
// when that runs out the next probe is created and started in turn. The guard
// tells it from the first probe, awake before the admission and drained
// since, by its creation alone, so a third seat is never created either.
func TestSimClaimStallProbeStarts(t *testing.T) {
	demand := []string{"gc-r1", "gc-r2", "gc-r3"}
	s := scripted(t, nil, routedDemandBead(demand[0]), routedDemandBead(demand[1]), routedDemandBead(demand[2]))
	h := heldClaimStall(t, s.env.CityPath, s.cfg, "worker", demand, s.clk.Now)
	journal := newSimStallJournal(t, s, h.guard, "worker")
	admitted := func() []string { return intentKeys(s.p.out.record.Load().Admitted) }
	settle := func() {
		t.Helper()
		s.settle()
		journal.feed()
	}
	noCreate := func(when string) {
		t.Helper()
		if got := admitted(); slices.Contains(got, "create:") {
			t.Fatalf("admitted %v %s, want no create", got, when)
		}
	}

	// probe settles the pass the hold runs out at, which must admit the
	// probe's create alone, then runs passes until the probe's row starts,
	// with no second create meanwhile, and returns the awake row. One intent
	// per row per pass: the row's own writes come first, its start after them.
	probe := func() beads.Bead {
		t.Helper()
		rows := len(journal.order)
		settle()
		if got := admitted(); !slices.Equal(got, []string{"create:"}) {
			t.Fatalf("admitted %v when the hold ran out, want the probe's create alone", got)
		}
		if len(journal.order) != rows+1 {
			t.Fatalf("worker rows %v after the probe's create, want one more than %d", journal.order, rows)
		}
		id := journal.order[rows]
		started := func() bool {
			return slices.ContainsFunc(s.sp.starts, func(st simStart) bool { return st.ID == id })
		}
		for passes := 0; !started(); passes++ {
			if passes == 4 {
				t.Fatalf("the probe %s never started in %d passes; starts %+v", id, passes, s.sp.starts)
			}
			s.advance(time.Second)
			settle()
			noCreate("while the probe waits to start")
		}
		woke, err := s.legs[0].backing.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if woke.Metadata["creation_complete_at"] == "" {
			t.Fatalf("probe %s after its start: %v, want creation_complete_at", id, woke.Metadata)
		}
		s.advance(postCreateProtectionTimeout + simPatrol)
		settle()
		noCreate("with the probe awake past its post-create protection")
		return woke
	}

	first := probe()
	awake, err := time.Parse(time.RFC3339, first.Metadata["creation_complete_at"])
	if err != nil {
		t.Fatal(err)
	}
	s.advance(awake.Add(claimStallDeadline).Sub(s.clk.Now()))
	settle()
	if got := len(h.stalls()); got != 2 {
		t.Fatalf("stalls = %d at the probe's claim deadline, want its failure alerted again", got)
	}
	for holdEnds := s.clk.Now().Add(claimStallHold(2)); ; {
		s.advance(simPatrol)
		if !s.clk.Now().Before(holdEnds) {
			break
		}
		settle()
		noCreate("inside the doubled hold")
	}
	probe()
	if len(journal.order) != 2 {
		t.Fatalf("worker rows %v, want the two probes alone", journal.order)
	}
	if got := len(h.stalls()); got != 2 {
		t.Fatalf("stalls = %d with the second probe awake, want no alert since the first probe's failure", got)
	}
	s.noViolations(t)
}

// TestClaimStallLegacyPassKeepsTheProbeDesired runs a held template's probe
// through the legacy desired-state build. The build that admits the probe
// creates its row. Until the probe wakes, every later build still wants that
// row, so a start the creating tick deferred still happens, and creates no
// other; once it is awake, none does either.
func TestClaimStallLegacyPassKeepsTheProbeDesired(t *testing.T) {
	cityPath := t.TempDir()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "gc"},
		Agents:    []config.Agent{{Name: "dog", StartCommand: "true", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(5)}},
	}
	city := beads.NewMemStore()
	var demand []string
	for range 3 {
		b, err := city.Create(beads.Bead{Title: "routed", Type: "task", Status: "open", Metadata: map[string]string{"gc.routed_to": "dog"}})
		if err != nil {
			t.Fatal(err)
		}
		demand = append(demand, b.ID)
	}
	h := heldClaimStall(t, cityPath, cfg, "dog", demand, time.Now)
	build := func() DesiredStateResult {
		snap, err := loadSessionBeadSnapshot(city)
		if err != nil {
			t.Fatal(err)
		}
		return buildDesiredStateWithSessionBeads("gc", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), city, nil, snap, nil, io.Discard)
	}

	ds := build()
	rows := openSessionBeads(t, city, "dog")
	if len(rows) != 1 {
		t.Fatalf("dog rows %v after the build that admitted the probe, want one", rows)
	}
	probe := rows[0]
	name := probe.Metadata["session_name"]
	if _, ok := ds.State[name]; !ok || len(ds.State) != 1 {
		t.Fatalf("desired %v, want the probe %s alone", desiredNames(ds), name)
	}
	h.guard.observe(sessionBeadEvent(t, events.BeadCreated, probe, time.Now()))

	ds = build()
	if rows := openSessionBeads(t, city, "dog"); len(rows) != 1 {
		t.Fatalf("dog rows %v while the probe waits to start, want the probe alone", rows)
	}
	if _, ok := ds.State[name]; !ok {
		t.Fatalf("desired %v while the probe waits to start, want the probe %s", desiredNames(ds), name)
	}

	now := time.Now().UTC().Format(time.RFC3339)
	if err := city.SetMetadataBatch(probe.ID, map[string]string{
		"state": "active", "state_reason": "creation_complete", "creation_complete_at": now, "last_woke_at": now, "pending_create_claim": "",
	}); err != nil {
		t.Fatal(err)
	}
	woke, err := city.Get(probe.ID)
	if err != nil {
		t.Fatal(err)
	}
	h.guard.observe(sessionBeadEvent(t, events.BeadUpdated, woke, time.Now()))
	ds = build()
	if rows := openSessionBeads(t, city, "dog"); len(rows) != 1 {
		t.Fatalf("dog rows %v with the probe awake, want the probe alone", rows)
	}
	if _, ok := ds.State[name]; !ok {
		t.Fatalf("desired %v with the probe awake, want the probe %s", desiredNames(ds), name)
	}
}
