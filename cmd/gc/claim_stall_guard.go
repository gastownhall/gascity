package main

// The claims-stall guard (ga-ylpbsy.73).
//
// A pool seat exists to claim the routed work it was spawned for. When a
// template's seats stop claiming — its provider out of usage, an expired
// account, a CLI that starts and never reads its prompt — the controller keeps
// counting the waiting work as demand, and each seat it spawns idles until it
// is drained and is replaced by the next. On 2026-10-31 one template drew about
// 1,400 seats in ten hours, none claimed, 48 runs waited a median 412 minutes,
// and nothing told a person (formula-overhead-audit-6 §6.1).
//
// The guard is the mechanical backstop for that class, whatever the cause:
//
//   - Input, off the city's event journal (the delta lanes' feed). A seat is a
//     session bead of a template that names a trigger (gc.trigger_bead_id, the
//     routed work it was spawned for). It FAILS, once, when, while its trigger
//     still waits as the template's routed demand:
//   - its session bead closes failed-create;
//   - the work ladder escalates its trigger (work.escalated
//     trigger_unanswered: a nudge about it went unanswered a base later);
//   - it has been awake claimStallDeadline without claiming; or
//   - its session bead closes unclaimed after being awake that long.
//     A seat CLAIMED when its session bead carries a work_started claim or a
//     work bead it was waiting on moves to in_progress under its session id.
//     Which work waits is the template's default routed demand at the last
//     pass, less every bead the journal has since shown leave the open state.
//   - Scope: a template, on the provider its seats run on now. A config change
//     that moves the template to another provider starts a new epoch: seats
//     created before it count for nothing, and a stall it ends is recorded as
//     resumed by the change.
//   - Trip: claimStallTrip seat failures in a row with no claim between,
//     within claimStallWindow, the first and the last at least
//     claimStallMinSpan apart (one tick's burst of failed creates is one
//     event, not a dead provider).
//   - Hold: the template's default routed-work demand stops counting, so no
//     seat is spawned for it, and agent.claims_stalled names the template, its
//     provider and the hold's end for the operator (the core pack mails it).
//     When the hold runs out, one probe seat is admitted; a probe that fails
//     doubles the hold, up to claimStallHoldMax, and raises the alert again in
//     the same episode.
//   - Resume: a claim by any counted seat of the template ends the episode,
//     lifts the hold and records agent.claims_resumed.
//
// It holds no judgment: the thresholds are constants, the comparisons are
// counts and times, and it never touches a bead; only the controller's spawn
// count changes. It draws no conclusion about a provider: a pack that
// classifies provider errors says so through provider-health.json. Its state is
// in memory: a controller restart forgets it, and the guard re-learns after
// claimStallTrip more seats fail. Like the claim-loop guard, it withholds only
// default routed-work demand: a custom scale_check is a number with no beads
// behind it. Named sessions and seats no demand spawned are never counted.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/workladder"
)

// Thresholds are consts, not config, for the claim-loop guard's reason: each
// is a knob that could hide the outage it exists to report. They were chosen by
// replaying eleven days of maintainer-city seats (Oct 21 - Nov 1): three
// failures in a row trip only during real outages, two also trip on healthy
// lanes' blips, and five add about seven minutes to every alert.
const (
	// claimStallTrip is how many seat failures in a row trip a stall.
	claimStallTrip = 3
	// claimStallMinSpan is the least time between the first and the last
	// failure of a streak that trips: a burst of failed creates in one tick
	// (a restart, a runtime hiccup) is one event.
	claimStallMinSpan = 2 * time.Minute
	// claimStallWindow bounds a streak: a failure older than this drops out
	// of it. Three seats spawned one after another for a single waiting bead
	// fail within about 25 minutes.
	claimStallWindow = 30 * time.Minute
	// claimStallDeadline is how long a seat may be awake without claiming
	// before it counts as failed. A healthy seat claims within seconds of
	// waking (p99 under two minutes across every provider in the replay).
	claimStallDeadline = 3 * time.Minute
	// claimStallHoldBase is the first hold; each failed probe doubles it, up
	// to claimStallHoldMax.
	claimStallHoldBase = 5 * time.Minute
	claimStallHoldMax  = 30 * time.Minute
	// claimStallProbeGrace is how long a probe seat has to claim or fail
	// before another probe is admitted: its spawn, its wake and the claim
	// deadline, with room.
	claimStallProbeGrace = 10 * time.Minute
	// claimStallForgetAfter drops a seat the journal has said nothing about
	// for this long.
	claimStallForgetAfter = 6 * time.Hour
)

// claimStallSessionMarker is how a session bead's notification payload spells
// its type; a payload without it is not a session bead's, which spares every
// other bead's notification a decode.
var claimStallSessionMarker = []byte(`"issue_type":"` + session.BeadType + `"`)

// claimStallSeat is what the guard knows of one session bead.
type claimStallSeat struct {
	template string
	created  time.Time // the session bead's creation; zero when unknown
	awake    time.Time // creation_complete_at; zero until the seat woke
	trigger  string    // gc.trigger_bead_id; "" for a seat no demand spawned
	claimed  bool
	settled  bool // counted: as a failure, or by its claim
	closed   bool
	seen     time.Time
}

// claimStallTemplate is one template's streak, its routed demand and its
// episodes.
type claimStallTemplate struct {
	provider string    // the provider its seats run on now ("" when unknown)
	epoch    time.Time // when that provider took effect; zero: since the guard began
	streak   []time.Time
	waiting  map[string]struct{} // its routed demand: the last pass's, less what has left the open state since
	// episodes are the open stalls, by the provider that stalled.
	episodes map[string]*claimStallEpisode
}

// claimStallEpisode is one stall, from its trip to the claim that ends it.
type claimStallEpisode struct {
	id       string
	provider string
	since    time.Time // the first failure of the streak that tripped it
	failures int       // seats failed in all
	holds    int
	until    time.Time // withheld until this time
	probing  bool      // a probe was admitted and has not reported
	probeAt  time.Time // when; only seats created since are its outcome
}

// claimStallGuard is one city's guard. Its methods are safe for concurrent
// use: observe runs on the journal feed's goroutine, withhold on the demand
// pass's and configure on the reload's.
type claimStallGuard struct {
	mu        sync.Mutex
	now       func() time.Time
	record    func(events.Event)
	stderr    io.Writer
	providers map[string]string // template → the provider its seats run on now
	templates map[string]*claimStallTemplate
	seats     map[string]*claimStallSeat
	// waitedBy is the routed demand's reverse index: a waiting bead → the
	// templates whose demand lists it.
	waitedBy map[string]map[string]struct{}
	// left is the beads the journal showed leave the open state, and when:
	// a demand pass that still lists one read a snapshot from before, and
	// it does not wait again until claimStallLeftMemory has passed.
	left map[string]time.Time
}

// claimStallLeftMemory is how long a bead seen leaving the open state is kept
// out of a template's waiting work: longer than any demand snapshot is reused.
const claimStallLeftMemory = 10 * time.Minute

func newClaimStallGuard(record func(events.Event), stderr io.Writer) *claimStallGuard {
	if stderr == nil {
		stderr = io.Discard
	}
	return &claimStallGuard{
		now:       time.Now,
		record:    record,
		stderr:    stderr,
		providers: map[string]string{},
		templates: map[string]*claimStallTemplate{},
		seats:     map[string]*claimStallSeat{},
		waitedBy:  map[string]map[string]struct{}{},
		left:      map[string]time.Time{},
	}
}

// claimStallProviders is each agent template's provider in cfg, by qualified
// name: the agent's own, else the workspace default for an agent that runs a
// provider at all ("" for a start_command agent).
func claimStallProviders(cfg *config.City) map[string]string {
	out := map[string]string{}
	if cfg == nil {
		return out
	}
	for i := range cfg.Agents {
		a := &cfg.Agents[i]
		provider := strings.TrimSpace(a.Provider)
		if provider == "" && strings.TrimSpace(a.StartCommand) == "" {
			provider = strings.TrimSpace(cfg.Workspace.Provider)
		}
		out[a.QualifiedName()] = provider
	}
	return out
}

// configure adopts cfg's templates. A template that moved to another provider
// starts a new epoch on it: the move may have been the fix, so its stall ends
// (agent.claims_resumed, cause config_changed) and its streak is dropped. A
// template gone from the config is forgotten.
func (g *claimStallGuard) configure(cfg *config.City) {
	if g == nil {
		return
	}
	providers := claimStallProviders(cfg)
	now := g.now()
	var alerts []events.Event
	g.mu.Lock()
	g.providers = providers
	for name, t := range g.templates {
		provider, ok := providers[name]
		if ok && provider == t.provider {
			continue
		}
		for _, ep := range t.episodes {
			fmt.Fprintf(g.stderr, "claims-stall guard: %s's stall on %s ends with a config change\n", name, claimStallProviderLabel(ep.provider)) //nolint:errcheck // best-effort stderr
			alerts = append(alerts, g.resumedEvent(name, ep, events.AgentClaimsCauseConfigChanged, now))
		}
		if !ok {
			delete(g.templates, name)
			continue
		}
		t.provider, t.epoch, t.streak, t.episodes = provider, now, nil, nil
	}
	g.mu.Unlock()
	g.emit(alerts)
}

// templateFor is name's state, created on first use. Caller holds g.mu.
func (g *claimStallGuard) templateFor(name string) *claimStallTemplate {
	t := g.templates[name]
	if t == nil {
		t = &claimStallTemplate{provider: g.providers[name]}
		g.templates[name] = t
	}
	return t
}

// counts reports whether seat counts toward t: created in t's epoch. A seat
// whose creation the guard never learned counts only while t has had one
// epoch since the guard began.
func (t *claimStallTemplate) counts(seat *claimStallSeat) bool {
	if t.epoch.IsZero() {
		return true
	}
	return !seat.created.IsZero() && !seat.created.Before(t.epoch)
}

// waits reports whether trigger is in t's routed demand.
func (t *claimStallTemplate) waits(trigger string) bool {
	if trigger == "" {
		return false
	}
	_, ok := t.waiting[trigger]
	return ok
}

// stalled is t's open episode on the provider its seats run on now, or nil.
func (t *claimStallTemplate) stalled() *claimStallEpisode {
	return t.episodes[t.provider]
}

// observe folds one journal event into the guard.
func (g *claimStallGuard) observe(evt events.Event) {
	if g == nil {
		return
	}
	var alerts []events.Event
	switch evt.Type {
	case events.BeadCreated, events.BeadUpdated, events.BeadClosed:
		if bytes.Contains(evt.Payload, claimStallSessionMarker) {
			b, ok := beads.DecodeBeadEventPayload(evt.Payload)
			if !ok || b.Type != session.BeadType {
				return
			}
			alerts = g.observeSeat(b, evt.Type == events.BeadClosed, g.eventTime(evt))
			break
		}
		alerts = g.observeWork(evt)
	case events.WorkEscalated:
		var p events.WorkLadderPayload
		if err := json.Unmarshal(evt.Payload, &p); err != nil || p.Reason != workladder.AlertTriggerUnanswered {
			return
		}
		alerts = g.observeUnanswered(firstNonEmpty(p.SessionID, evt.SessionID), p.Template, firstNonEmpty(p.BeadID, evt.Subject), g.eventTime(evt))
	}
	g.emit(alerts)
}

func (g *claimStallGuard) eventTime(evt events.Event) time.Time {
	if evt.Ts.IsZero() {
		return g.now()
	}
	return evt.Ts
}

// emit records alerts outside the lock.
func (g *claimStallGuard) emit(alerts []events.Event) {
	if g.record == nil {
		return
	}
	for _, e := range alerts {
		g.record(e)
	}
}

// observeSeat folds one session bead's snapshot.
func (g *claimStallGuard) observeSeat(b beads.Bead, closed bool, at time.Time) []events.Event {
	template := strings.TrimSpace(b.Metadata["template"])
	if template == "" || b.ID == "" {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.sweep(at)
	seat := g.seatFor(b.ID, template)
	seat.seen = at
	if !b.CreatedAt.IsZero() {
		seat.created = b.CreatedAt
	}
	if trigger := strings.TrimSpace(b.Metadata[beadmeta.TriggerBeadIDMetadataKey]); trigger != "" {
		seat.trigger = trigger
	}
	if seat.awake.IsZero() {
		if woke, err := time.Parse(time.RFC3339, strings.TrimSpace(b.Metadata["creation_complete_at"])); err == nil {
			seat.awake = woke
		}
	}
	var alerts []events.Event
	if claimStallClaimed(b.Metadata[session.WorkStartedKey]) {
		alerts = append(alerts, g.claimedLocked(seat, at)...)
	}
	if closed && !seat.closed {
		seat.closed = true
		failedCreate := strings.TrimSpace(b.Metadata["state"]) == "failed-create"
		awakeLongEnough := !seat.awake.IsZero() && at.Sub(seat.awake) >= claimStallDeadline
		if !seat.claimed && (failedCreate || awakeLongEnough) && g.templateFor(template).waits(seat.trigger) {
			alerts = append(alerts, g.failedLocked(seat, at)...)
		}
	}
	return alerts
}

// observeWork folds a notification about a bead some template's demand waits
// on: one that left the open state no longer waits (a seat took it, or it was
// closed), and one that moved to in_progress under a seat's session id is that
// seat's claim. Only waiting beads are decoded.
func (g *claimStallGuard) observeWork(evt events.Event) []events.Event {
	id := strings.TrimSpace(evt.Subject)
	if id == "" {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	templates, ok := g.waitedBy[id]
	if !ok {
		return nil
	}
	b, ok := beads.DecodeBeadEventPayload(evt.Payload)
	if !ok || (evt.Type != events.BeadClosed && b.Status == "open") {
		return nil
	}
	for name := range templates {
		if t := g.templates[name]; t != nil {
			delete(t.waiting, id)
		}
	}
	delete(g.waitedBy, id)
	g.left[id] = g.eventTime(evt)
	if b.Status != "in_progress" {
		return nil
	}
	seat := g.seats[strings.TrimSpace(firstNonEmpty(b.Metadata[beadmeta.SessionIDMetadataKey], evt.SessionID))]
	if seat == nil {
		return nil
	}
	return g.claimedLocked(seat, g.eventTime(evt))
}

// claimStallClaimed reports whether a work_started cell names a claim. An
// unreadable cell names none: the guard's fail-safe is to keep counting, and
// a claim always rewrites the cell.
func claimStallClaimed(raw string) bool {
	if strings.TrimSpace(raw) == "" {
		return false
	}
	cell, err := session.DecodeWorkStarted(raw)
	return err == nil && len(cell.Claims) > 0
}

// observeUnanswered folds the work ladder's escalation of a seat's trigger.
// The ladder re-read the trigger live before it nudged, so the trigger is
// known to wait.
func (g *claimStallGuard) observeUnanswered(sessionID, template, trigger string, at time.Time) []events.Event {
	sessionID, template = strings.TrimSpace(sessionID), strings.TrimSpace(template)
	if sessionID == "" || template == "" {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.sweep(at)
	seat := g.seatFor(sessionID, template)
	seat.seen = at
	if seat.trigger == "" {
		seat.trigger = strings.TrimSpace(trigger)
	}
	if seat.claimed {
		return nil
	}
	return g.failedLocked(seat, at)
}

// seatFor is id's seat, created on first sight. Caller holds g.mu.
func (g *claimStallGuard) seatFor(id, template string) *claimStallSeat {
	seat := g.seats[id]
	if seat == nil {
		seat = &claimStallSeat{template: template}
		g.seats[id] = seat
	}
	return seat
}

// failedLocked counts seat's failure, once, and trips its template's stall on
// the claimStallTrip-th in a row. Inside an episode a failure trips nothing:
// it is the probe's report when the probe seat failed. Caller holds g.mu.
func (g *claimStallGuard) failedLocked(seat *claimStallSeat, at time.Time) []events.Event {
	if seat.settled {
		return nil
	}
	t := g.templateFor(seat.template)
	if !t.counts(seat) {
		return nil
	}
	seat.settled = true
	if ep := t.stalled(); ep != nil {
		ep.failures++
		if !ep.probing || seat.created.IsZero() || seat.created.Before(ep.probeAt) {
			return nil // a seat already running when the hold began; it neither lengthens the hold nor ends it
		}
		ep.probing = false
		ep.holds++
		ep.until = at.Add(claimStallHold(ep.holds))
		fmt.Fprintf(g.stderr, "claims-stall guard: the probe seat of %s on %s failed to claim too (%d seats in all); holding its routed demand until %s\n", //nolint:errcheck // best-effort stderr
			seat.template, claimStallProviderLabel(ep.provider), ep.failures, ep.until.UTC().Format(time.RFC3339))
		return []events.Event{g.stalledEvent(seat.template, ep, events.AgentClaimsCauseProbe, ep.failures, at)}
	}
	streak := t.streak[:0]
	for _, f := range t.streak {
		if at.Sub(f) <= claimStallWindow {
			streak = append(streak, f)
		}
	}
	streak = append(streak, at)
	t.streak = streak
	n := len(t.streak)
	if n < claimStallTrip {
		return nil
	}
	// The failures arrive about in time order, not exactly (an overdue
	// seat's is dated at its deadline): the span is the streak's range.
	first, last := t.streak[0], t.streak[0]
	for _, f := range t.streak {
		first, last = claimStallEarlier(first, f), claimStallLater(last, f)
	}
	if last.Sub(first) < claimStallMinSpan {
		return nil
	}
	t.streak = nil
	return g.tripLocked(seat.template, t, n, first, at)
}

func claimStallEarlier(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

func claimStallLater(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// tripLocked opens template's episode on its current provider and holds its
// routed demand. Caller holds g.mu.
func (g *claimStallGuard) tripLocked(template string, t *claimStallTemplate, seats int, since, at time.Time) []events.Event {
	ep := &claimStallEpisode{
		id:       claimStallEpisodeID(template, t.provider, since),
		provider: t.provider,
		since:    since,
		failures: seats,
		holds:    1,
		until:    at.Add(claimStallHold(1)),
	}
	if t.episodes == nil {
		t.episodes = map[string]*claimStallEpisode{}
	}
	t.episodes[t.provider] = ep
	fmt.Fprintf(g.stderr, "claims-stall guard: %s on %s: %d seats in a row failed to claim the work routed to them since %s; holding its routed demand until %s\n", //nolint:errcheck // best-effort stderr
		template, claimStallProviderLabel(t.provider), seats, since.UTC().Format(time.RFC3339), ep.until.UTC().Format(time.RFC3339))
	return []events.Event{g.stalledEvent(template, ep, events.AgentClaimsCauseSeats, seats, at)}
}

// claimedLocked counts seat's claim: it ends the streak and, on the provider
// that stalled, the episode. Caller holds g.mu.
func (g *claimStallGuard) claimedLocked(seat *claimStallSeat, at time.Time) []events.Event {
	if seat.claimed {
		return nil
	}
	seat.claimed, seat.settled = true, true
	t := g.templateFor(seat.template)
	if !t.counts(seat) {
		return nil
	}
	t.streak = nil
	ep := t.stalled()
	if ep == nil {
		return nil
	}
	delete(t.episodes, t.provider)
	fmt.Fprintf(g.stderr, "claims-stall guard: %s claims its work again on %s; its routed demand counts again\n", seat.template, claimStallProviderLabel(ep.provider)) //nolint:errcheck // best-effort stderr
	return []events.Event{g.resumedEvent(seat.template, ep, events.AgentClaimsCauseClaimed, at)}
}

// claimStallHold is how long the holds-th hold of an episode lasts.
func claimStallHold(holds int) time.Duration {
	hold := claimStallHoldBase
	for i := 1; i < holds && hold < claimStallHoldMax; i++ {
		hold *= 2
	}
	return min(hold, claimStallHoldMax)
}

// claimStallEpisodeID names an episode stably: its template, its provider and
// when its first failure happened.
func claimStallEpisodeID(template, provider string, since time.Time) string {
	return fmt.Sprintf("%s@%s@%s", template, provider, since.UTC().Format(time.RFC3339))
}

func claimStallProviderLabel(provider string) string {
	if provider == "" {
		return "its configured command"
	}
	return provider
}

// stalledEvent is the agent.claims_stalled alert for ep.
func (g *claimStallGuard) stalledEvent(template string, ep *claimStallEpisode, cause string, seats int, at time.Time) events.Event {
	until := ep.until.UTC()
	payload, _ := json.Marshal(events.AgentClaimsPayload{ //nolint:errchkjson // a struct of strings, ints and times always marshals
		Template: template,
		Provider: ep.provider,
		Episode:  ep.id,
		Action:   events.AgentClaimsHeld,
		Cause:    cause,
		Seats:    seats,
		Since:    ep.since.UTC(),
		Until:    &until,
	})
	what := fmt.Sprintf("%d of its seats in a row failed to claim the work routed to them while it waited", seats)
	if cause == events.AgentClaimsCauseProbe {
		what = fmt.Sprintf("its probe seat failed to claim too (%d seats in all)", seats)
	}
	return events.Event{
		Type:    events.AgentClaimsStalled,
		Ts:      at.UTC(),
		Actor:   "controller",
		Subject: template,
		Message: fmt.Sprintf("Agent %s stopped claiming its work on %s: %s (since %s). No new seats are spawned for its routed work until %s, then one probe seat; a claim ends the stall (a custom scale_check count is not withheld).",
			template, claimStallProviderLabel(ep.provider), what, ep.since.UTC().Format(time.RFC3339), until.Format(time.RFC3339)),
		Payload: payload,
	}
}

// resumedEvent is the agent.claims_resumed record that ends ep, by a claim or
// by a config change.
func (g *claimStallGuard) resumedEvent(template string, ep *claimStallEpisode, cause string, at time.Time) events.Event {
	payload, _ := json.Marshal(events.AgentClaimsPayload{ //nolint:errchkjson // a struct of strings, ints and times always marshals
		Template: template,
		Provider: ep.provider,
		Episode:  ep.id,
		Cause:    cause,
		Seats:    ep.failures,
		Since:    ep.since.UTC(),
	})
	how := "claims its work again on " + claimStallProviderLabel(ep.provider)
	if cause == events.AgentClaimsCauseConfigChanged {
		how = "was moved off " + claimStallProviderLabel(ep.provider) + " by a config change"
	}
	return events.Event{
		Type:    events.AgentClaimsResumed,
		Ts:      at.UTC(),
		Actor:   "controller",
		Subject: template,
		Message: fmt.Sprintf("Agent %s %s; the stall that began %s (%d seats failed) is over and its routed work counts as demand again.",
			template, how, ep.since.UTC().Format(time.RFC3339), ep.failures),
		Payload: payload,
	}
}

// sweep forgets seats the journal has gone quiet about, and beads that left
// the open state long enough ago. Caller holds g.mu.
func (g *claimStallGuard) sweep(now time.Time) {
	for id, seat := range g.seats {
		if now.Sub(seat.seen) > claimStallForgetAfter {
			delete(g.seats, id)
		}
	}
	for id, at := range g.left {
		if now.Sub(at) > claimStallLeftMemory {
			delete(g.left, id)
		}
	}
}

// withhold records each template's default routed demand, counts the seats
// that have been awake past the claim deadline without claiming it, and
// removes the demand of every held template from one pass: all of it while
// the hold stands, all but one bead (the probe) when the hold runs out. It
// never edits its arguments; with nothing held it returns them unchanged.
func (g *claimStallGuard) withhold(counts map[string]int, demand map[string]scaleCheckDemand) (map[string]int, map[string]scaleCheckDemand) {
	if g == nil {
		return counts, demand
	}
	now := g.now()
	g.mu.Lock()
	g.sweep(now)
	g.recordDemandLocked(demand)
	alerts := g.overdueLocked(now)
	var outCounts map[string]int
	var outDemand map[string]scaleCheckDemand
	for _, name := range sortedDemandTemplates(demand) {
		t := g.templates[name]
		if t == nil {
			continue
		}
		ep := t.stalled()
		if ep == nil {
			continue
		}
		entry := demand[name]
		keep := g.admittedLocked(name, ep, now)
		if keep >= len(entry.WorkBeadIDs) {
			continue
		}
		if outDemand == nil {
			outDemand = cloneScaleCheckDemandMap(demand)
			outCounts = make(map[string]int, len(counts))
			for k, v := range counts {
				outCounts[k] = v
			}
		}
		withheld := len(entry.WorkBeadIDs) - keep
		entry.WorkBeadIDs = append([]string(nil), entry.WorkBeadIDs[:keep]...)
		entry.Count = max(entry.Count-withheld, 0)
		outDemand[name] = entry
		outCounts[name] = max(outCounts[name]-withheld, 0)
	}
	g.mu.Unlock()
	g.emit(alerts)
	if outDemand == nil {
		return counts, demand
	}
	return outCounts, outDemand
}

// recordDemandLocked replaces every template's waiting work with this pass's
// default routed demand, less the beads the journal showed leave the open
// state since the snapshot was read, and the reverse index with it. Caller
// holds g.mu.
func (g *claimStallGuard) recordDemandLocked(demand map[string]scaleCheckDemand) {
	for _, t := range g.templates {
		t.waiting = nil
	}
	clear(g.waitedBy)
	for name, entry := range demand {
		t := g.templateFor(name)
		t.waiting = make(map[string]struct{}, len(entry.WorkBeadIDs))
		for _, id := range entry.WorkBeadIDs {
			if _, gone := g.left[id]; gone {
				continue
			}
			t.waiting[id] = struct{}{}
			by := g.waitedBy[id]
			if by == nil {
				by = map[string]struct{}{}
				g.waitedBy[id] = by
			}
			by[name] = struct{}{}
		}
	}
}

// sortedDemandTemplates is demand's templates in a stable order, so a pass's
// probe admissions do not depend on map order.
func sortedDemandTemplates(demand map[string]scaleCheckDemand) []string {
	names := make([]string, 0, len(demand))
	for name := range demand {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// overdueLocked counts every live, unclaimed seat awake for the claim
// deadline whose trigger still waits. Caller holds g.mu.
func (g *claimStallGuard) overdueLocked(now time.Time) []events.Event {
	ids := make([]string, 0, len(g.seats))
	for id, seat := range g.seats {
		if seat.settled || seat.closed || seat.claimed || seat.awake.IsZero() || now.Sub(seat.awake) < claimStallDeadline {
			continue
		}
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return g.seats[ids[i]].awake.Before(g.seats[ids[j]].awake) })
	var alerts []events.Event
	for _, id := range ids {
		seat := g.seats[id]
		if !g.templateFor(seat.template).waits(seat.trigger) {
			continue
		}
		alerts = append(alerts, g.failedLocked(seat, seat.awake.Add(claimStallDeadline))...)
	}
	return alerts
}

// admittedLocked is how many of the held template's beads this pass counts:
// none while the hold stands; one, the probe, when it has run out (or when a
// probe admitted earlier never reported within claimStallProbeGrace), after
// which the template is withheld again while the probe reports. Caller holds
// g.mu.
func (g *claimStallGuard) admittedLocked(template string, ep *claimStallEpisode, now time.Time) int {
	if now.Before(ep.until) {
		return 0
	}
	ep.probing = true
	ep.probeAt = now
	ep.until = now.Add(claimStallProbeGrace)
	fmt.Fprintf(g.stderr, "claims-stall guard: admitting one probe seat for %s on %s\n", template, claimStallProviderLabel(ep.provider)) //nolint:errcheck // best-effort stderr
	return 1
}

// claimStallGuards maps a normalized city path to its controller's guard, as
// claimLoopGuards does for the claim-loop guard: both demand passes reach it by
// city path, and a process that runs no controller registers none.
var claimStallGuards sync.Map // normalized city path → *claimStallGuard

// registerClaimStallGuard publishes g for cityPath until ctx ends, as
// registerClaimLoopGuard does.
func registerClaimStallGuard(ctx context.Context, cityPath string, g *claimStallGuard) (unregister func()) {
	key := normalizePathForCompare(cityPath)
	claimStallGuards.Store(key, g)
	unregister = func() { claimStallGuards.CompareAndDelete(key, g) }
	if ctx != nil {
		context.AfterFunc(ctx, unregister)
	}
	return unregister
}

// claimStallGuardFor is cityPath's registered guard, or nil.
func claimStallGuardFor(cityPath string) *claimStallGuard {
	if strings.TrimSpace(cityPath) == "" {
		return nil
	}
	g, ok := claimStallGuards.Load(normalizePathForCompare(cityPath))
	if !ok {
		return nil
	}
	return g.(*claimStallGuard)
}
