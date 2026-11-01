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
//     work bead it was waiting on moves to in_progress under its session id;
//     only a claim newer than the failures it would answer counts as one.
//     Which work waits is the template's default routed demand at the last
//     pass, less every bead the journal has since shown leave the open state.
//   - Scope: a template, on the provider its seats run on now. A config change
//     that moves the template to another provider starts a new epoch: seats
//     created before it count for nothing, and a stall it ends is recorded as
//     resumed by the change.
//   - Trip: claimStallTrip seat failures in a row with no claim between,
//     within claimStallWindow, the first and the last at least
//     claimStallMinSpan apart: one tick's burst of failed creates cannot trip
//     a stall by itself, only failures that go on.
//   - Hold: the template's default routed-work demand stops counting, so no
//     seat is spawned for it, and agent.claims_stalled names the template, its
//     provider and the hold's end for the operator (the core pack mails it).
//     When the hold runs out, one probe seat is admitted; a probe that fails
//     doubles the hold, up to claimStallHoldMax, and raises the alert again in
//     the same episode.
//   - Standby: a template whose config declares a usable standby_provider is
//     moved onto it instead of being held, when the stall is on its own
//     provider, the v2 session reconciler runs (only v2 keeps a busy seat
//     alive across a provider move), the evidence is the provider's (the
//     failed seats waited on at least two beads of work, and a seat on some
//     other provider claimed meanwhile, so the city itself works), and the
//     standby can take its seats: the provider-health record does not mark it
//     unhealthy, and the reconciler's own transport check passes for the
//     agent moved there (the v2 planner refuses every seat of a template that
//     fails it). The move is recorded in .gc/runtime/standby.toml, which
//     every gc process composes with city.toml, and the config reloads: new
//     seats start on the standby, idle seats of the own provider are
//     replaced, busy ones finish. agent.standby_engaged records it. A pack
//     that classifies provider errors moves it too, by marking the own
//     provider unhealthy in provider-health.json. The standby's own failures
//     count only after claimStallStandbyGrace, so its cold start is not a
//     stall.
//   - Probe: a move lasts claimStallStandbyHold, doubling with each move in
//     the same episode up to claimStallStandbyHoldMax, and ends early when a
//     seat of another template claims on the own provider. A move the
//     provider-health record made ends when the record stops marking the own
//     provider unhealthy instead, and no move ends while it does (the
//     reconciler would start no probe seat there). Then the template returns
//     to its own provider (agent.standby_released) and one probe seat is
//     admitted there: a claim ends the episode; a failure moves it back, or,
//     while the standby cannot take it, holds it as any failed probe does.
//   - Resume: a claim by any counted seat of the template on the provider that
//     stalled ends the episode, lifts the hold and records
//     agent.claims_resumed.
//
// It holds no judgment: the thresholds are constants, the comparisons are
// counts and times, the standby is configuration, and it never touches a
// bead; only the controller's spawn count and the standby record change. It
// draws no conclusion about a provider from error text: a pack that classifies
// provider errors says so through provider-health.json. Its state is in memory
// but for the standby record, which a restarted controller resumes from: a
// restart otherwise forgets it, and the guard re-learns after claimStallTrip
// more seats fail. Like the claim-loop guard, it withholds only
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
	"github.com/gastownhall/gascity/internal/fsys"
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
	// claimStallStandbyHold is a standby move's first hold; each further
	// move in the same episode doubles it, up to claimStallStandbyHoldMax.
	// An outage of a provider pool lasts hours: the doubling keeps the
	// probes of a dead own provider to a few.
	claimStallStandbyHold    = 30 * time.Minute
	claimStallStandbyHoldMax = 4 * time.Hour
	// claimStallStandbyGrace is how long after a move the standby's seats'
	// failures do not count: a provider woken cold for a burst of seats can
	// take minutes to claim the first.
	claimStallStandbyGrace = 15 * time.Minute
	// claimStallStandbyEvidence is how many distinct beads of work the
	// failing seats must have waited on before the stall is moved to a
	// standby: one bead failing for every seat may be that bead.
	claimStallStandbyEvidence = 2
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
	claimAt  time.Time // the newest claim its work_started cell has named
	settled  bool      // counted: as a failure, or by its claim
	closed   bool
	seen     time.Time
}

// claimStallTemplate is one template's streak, its routed demand and its
// episodes.
type claimStallTemplate struct {
	provider string    // the provider its seats run on now ("" when unknown)
	epoch    time.Time // when that provider took effect; zero: since the guard began
	streak   []claimStallFailure
	waiting  map[string]struct{} // its routed demand: the last pass's, less what has left the open state since
	// episodes are the open stalls, by the provider that stalled.
	episodes map[string]*claimStallEpisode
}

// claimStallFailure is one failed seat in a streak.
type claimStallFailure struct {
	at      time.Time
	trigger string
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
	// action is what the guard did: held, or moved to the standby.
	action string
	// engagements counts the standby moves in the episode.
	engagements int
	// movable: the stall is its provider's (its seats' evidence, or the
	// provider-health record), so the template moves to its standby
	// whenever the standby can take it, at the trip or when a probe fails.
	movable bool
}

// claimStallLane is one template's providers in the composed config.
type claimStallLane struct {
	own     string // the provider its config names
	current string // the provider its seats start on now: the standby while moved
	standby string // its standby_provider, when it can be used
	// refused is why the standby_provider its config accepts cannot start
	// its seats here (the reconciler's transport check refused the agent
	// moved there); "" when it can, or the config declares none it accepts.
	refused string
}

// claimStallMove is a standby move the guard wants recorded.
type claimStallMove struct {
	provider    string // the standby
	replaced    string // the template's own provider
	since       time.Time
	until       time.Time
	engagements int
	episode     string
	reason      string
	// cause is what made the move: failed seats (seats, probe) or the
	// pack's provider-health record (provider_health). A move the record
	// made ends when the record no longer marks the own provider unhealthy.
	cause string
}

// claimStallGuard is one city's guard. Its methods are safe for concurrent
// use: observe runs on the journal feed's goroutine, withhold on the demand
// pass's and configure on the reload's.
type claimStallGuard struct {
	mu        sync.Mutex
	now       func() time.Time
	record    func(events.Event)
	stderr    io.Writer
	lanes     map[string]claimStallLane // template → its providers in the config
	templates map[string]*claimStallTemplate
	seats     map[string]*claimStallSeat
	// standbyAllowed: the v2 session reconciler runs, which keeps a busy
	// seat alive across a provider move (legacy judges a seat's liveness
	// by the current config's process names).
	standbyAllowed bool
	// moves are the standby moves the guard wants, and recorded the ones
	// .gc/runtime/standby.toml holds; syncStandby writes the one to the
	// other. fs and cityPath reach the record; a nil fs keeps no record.
	moves    map[string]*claimStallMove
	recorded map[string]config.StandbyEntry
	// dropped is why each move dropped since the last sync ended.
	dropped  map[string]string
	restored bool
	fs       fsys.FS
	cityPath string
	// launch is the reconciler's transport check on an agent as a config
	// composes it: a standby the agent fails it on is never moved to. nil
	// checks nothing.
	launch func(*config.City, *config.Agent) error
	// lastClaim is each provider's newest claim by a counted seat: evidence
	// that a provider, or the city, works.
	lastClaim map[string]time.Time
	// health is the provider-health record syncStandby read last, nil before
	// the first: a standby it marks unhealthy is not moved to.
	health *providerHealthSnapshot
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
		lanes:     map[string]claimStallLane{},
		templates: map[string]*claimStallTemplate{},
		seats:     map[string]*claimStallSeat{},
		waitedBy:  map[string]map[string]struct{}{},
		left:      map[string]time.Time{},
		moves:     map[string]*claimStallMove{},
		recorded:  map[string]config.StandbyEntry{},
		dropped:   map[string]string{},
		lastClaim: map[string]time.Time{},
	}
}

// withLaunchCheck has the guard move an agent only to a standby its seats can
// start on: check is the reconciler's own transport check.
func (g *claimStallGuard) withLaunchCheck(check func(*config.City, *config.Agent) error) *claimStallGuard {
	g.launch = check
	return g
}

// withStandbyRecord points the guard at a city's standby record.
func (g *claimStallGuard) withStandbyRecord(fs fsys.FS, cityPath string) *claimStallGuard {
	g.fs, g.cityPath = fs, cityPath
	return g
}

// claimStallLanes is each agent template's providers in cfg, by qualified
// name: the one its seats start on now, its own (the workspace default for an
// agent that names none and runs a provider at all, "" for a start_command
// agent), and its standby when it can use one. launch, when not nil, is the
// reconciler's transport check a standby must pass.
func claimStallLanes(cfg *config.City, launch func(*config.City, *config.Agent) error) map[string]claimStallLane {
	out := map[string]claimStallLane{}
	if cfg == nil {
		return out
	}
	for i := range cfg.Agents {
		a := &cfg.Agents[i]
		out[a.QualifiedName()] = claimStallLaneOf(a, cfg, launch)
	}
	return out
}

// claimStallLaneOf is one agent's lane. The standby_provider its config
// accepts (config.StandbyUsable) is its standby only while launch passes on
// the agent moved there: the v2 planner refuses every seat of a template that
// fails the check, so a move there would leave the lane seatless.
func claimStallLaneOf(a *config.Agent, cfg *config.City, launch func(*config.City, *config.Agent) error) claimStallLane {
	lane := claimStallLane{current: claimStallCurrentProvider(a, &cfg.Workspace), own: config.AgentOwnProvider(a, &cfg.Workspace)}
	if config.StandbyUsable(a, &cfg.Workspace, cfg.Providers) != "" {
		return lane
	}
	standby := strings.TrimSpace(a.StandbyProvider)
	if launch != nil {
		moved := config.OnStandby(*a, &cfg.Workspace)
		if err := launch(cfg, &moved); err != nil {
			lane.refused = fmt.Sprintf("its standby_provider %s cannot start its seats: %v", standby, err)
			return lane
		}
	}
	lane.standby = standby
	return lane
}

// claimStallCurrentProvider is the provider a's seats start on now: its
// provider (the standby while the record moves it), else the workspace's,
// for an agent that runs a provider at all.
func claimStallCurrentProvider(a *config.Agent, workspace *config.Workspace) string {
	if strings.TrimSpace(a.StartCommand) != "" {
		return ""
	}
	if p := strings.TrimSpace(a.Provider); p != "" {
		return p
	}
	return strings.TrimSpace(workspace.Provider)
}

// configure adopts cfg's templates; standbyAllowed says whether the v2
// session reconciler runs. A template whose own provider changed, or that
// left the config, was changed by a person: the change may have been the fix,
// so its stalls end (agent.claims_resumed, cause config_changed) and its move,
// if any, is dropped. So is a recorded move cfg no longer applies (a person
// deleted it from the record, or changed the agent under it): the template
// returns to its own provider for a probe. A template whose seats start on
// another provider now (the standby record moved it, or put it back) starts a
// new epoch there: its streak is dropped and seats created before count for
// nothing, but its episodes stand. The first call also resumes the moves the
// standby record holds, so a restarted controller keeps them.
func (g *claimStallGuard) configure(cfg *config.City, standbyAllowed bool) {
	if g == nil {
		return
	}
	lanes := claimStallLanes(cfg, g.launch)
	now := g.now()
	var alerts []events.Event
	g.mu.Lock()
	previous := g.lanes
	g.lanes, g.standbyAllowed = lanes, standbyAllowed
	for _, name := range g.sortedLaneNamesLocked() {
		if why := lanes[name].refused; why != "" && why != previous[name].refused {
			fmt.Fprintf(g.stderr, "claims-stall guard: %s: %s; a stall holds it instead\n", name, why) //nolint:errcheck // best-effort stderr
		}
	}
	for name, t := range g.templates {
		lane, ok := lanes[name]
		if !ok || lane.own != previous[name].own {
			for _, ep := range t.episodes {
				fmt.Fprintf(g.stderr, "claims-stall guard: %s's stall on %s ends with a config change\n", name, claimStallProviderLabel(ep.provider)) //nolint:errcheck // best-effort stderr
				alerts = append(alerts, g.resumedEvent(name, ep, events.AgentClaimsCauseConfigChanged, now))
			}
			t.episodes = nil
			g.dropMoveLocked(name, events.AgentClaimsCauseConfigChanged, now)
			if !ok {
				delete(g.templates, name)
				continue
			}
		}
		if lane.current != t.provider {
			t.provider, t.epoch, t.streak = lane.current, now, nil
		}
	}
	for _, name := range g.sortedMoveNamesLocked() {
		m := g.moves[name]
		lane := lanes[name]
		r, recorded := g.recorded[name]
		switch {
		case !standbyAllowed || lane.standby != m.provider || lane.own != m.replaced:
			fmt.Fprintf(g.stderr, "claims-stall guard: %s's move to %s ends with a config change\n", name, m.provider) //nolint:errcheck // best-effort stderr
		case recorded && r.Since.Equal(m.since) && lane.current != m.provider:
			fmt.Fprintf(g.stderr, "claims-stall guard: %s's move to %s is no longer in the config; it returns to %s\n", name, m.provider, m.replaced) //nolint:errcheck // best-effort stderr
		default:
			continue
		}
		g.dropMoveLocked(name, events.AgentClaimsCauseConfigChanged, now)
	}
	if !g.restored {
		g.restored = true
		g.restoreLocked()
	}
	g.mu.Unlock()
	g.emit(alerts)
}

// restoreLocked resumes the moves the standby record holds that still match
// the config, each with its episode, so a restarted controller probes the own
// provider when the hold it recorded runs out. Caller holds g.mu.
func (g *claimStallGuard) restoreLocked() {
	if g.fs == nil {
		return
	}
	f, _, err := config.LoadStandby(g.fs, g.cityPath)
	if err != nil {
		fmt.Fprintf(g.stderr, "claims-stall guard: %v; resuming no standby move\n", err) //nolint:errcheck // best-effort stderr
		return
	}
	for _, e := range f.Agents {
		g.recorded[e.Name] = e
		lane, ok := g.lanes[e.Name]
		if !ok || !g.standbyAllowed || lane.standby != e.Provider || lane.own != e.Replaced {
			continue // syncStandby drops it from the record
		}
		g.moves[e.Name] = &claimStallMove{provider: e.Provider, replaced: e.Replaced, since: e.Since, until: e.Until, engagements: e.Engagements, episode: e.Episode, reason: e.Reason, cause: e.Cause}
		t := g.templateFor(e.Name)
		t.provider, t.epoch = lane.current, e.Since // seats from before the move are the own provider's
		if t.episodes == nil {
			t.episodes = map[string]*claimStallEpisode{}
		}
		id := e.Episode
		if id == "" {
			id = claimStallEpisodeID(e.Name, e.Replaced, e.Since)
		}
		t.episodes[e.Replaced] = &claimStallEpisode{id: id, provider: e.Replaced, since: e.Since, holds: 1, until: e.Until, action: events.AgentClaimsStandby, engagements: e.Engagements, movable: true}
		fmt.Fprintf(g.stderr, "claims-stall guard: %s stays on its standby %s until %s (from the standby record)\n", e.Name, e.Provider, e.Until.UTC().Format(time.RFC3339)) //nolint:errcheck // best-effort stderr
	}
}

// templateFor is name's state, created on first use. Caller holds g.mu.
func (g *claimStallGuard) templateFor(name string) *claimStallTemplate {
	t := g.templates[name]
	if t == nil {
		t = &claimStallTemplate{provider: g.lanes[name].current}
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
	_, known := g.seats[b.ID]
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
	// A claim is new when the cell names one newer than any it named before:
	// the cell rides along on every write to the session bead, and repeating
	// a claim the guard has seen proves nothing again.
	if claimedAt, claimed := claimStallNewestClaim(b.Metadata[session.WorkStartedKey]); claimed && (!seat.claimed || claimedAt.After(seat.claimAt)) {
		seat.claimAt = claimStallLater(seat.claimAt, claimedAt)
		if known {
			// A seat the guard watched claim: the claim is news now, on
			// the controller's clock, whatever the hook's clock says.
			claimedAt = at
		}
		alerts = append(alerts, g.claimedLocked(seat, claimedAt)...)
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
	// The claim's first write sets the assignee to the seat's session id;
	// gc.session_id follows in a second write, and a release leaves the
	// previous claimer's there, so the assignee is asked first.
	for _, id := range []string{b.Assignee, b.Metadata[beadmeta.SessionIDMetadataKey], evt.SessionID} {
		if seat := g.seats[strings.TrimSpace(id)]; seat != nil {
			return g.claimedLocked(seat, g.eventTime(evt))
		}
	}
	return nil
}

// claimStallNewestClaim is when the newest claim a work_started cell names
// was handed to a turn, and whether it names one. The cell keeps a row's
// claims across its restarts, so a seat first seen after a controller restart
// can carry claims from long before: only the claim's own time says whether
// it answers a failure. An unreadable cell names none: the guard's fail-safe is
// to keep counting, and a claim always rewrites the cell.
func claimStallNewestClaim(raw string) (time.Time, bool) {
	if strings.TrimSpace(raw) == "" {
		return time.Time{}, false
	}
	cell, err := session.DecodeWorkStarted(raw)
	if err != nil || len(cell.Claims) == 0 {
		return time.Time{}, false
	}
	var newest time.Time
	for _, c := range cell.Claims {
		newest = claimStallLater(newest, c.At)
	}
	return newest, true
}

// observeUnanswered folds the work ladder's escalation of a seat's trigger:
// a failure while the trigger still waits as the template's routed demand.
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
	if seat.claimed || !g.templateFor(template).waits(seat.trigger) {
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
	if m := g.moves[seat.template]; m != nil && t.provider == m.provider && at.Before(m.since.Add(claimStallStandbyGrace)) {
		return nil // the standby's cold start, not a verdict on it
	}
	if ep := t.stalled(); ep != nil {
		ep.failures++
		if !ep.probing || seat.created.IsZero() || seat.created.Before(ep.probeAt) {
			return nil // a seat already running when the hold began; it neither lengthens the hold nor ends it
		}
		ep.probing = false
		var why string
		if ep.movable {
			if why = g.standbyRefusalLocked(seat.template); why == "" {
				fmt.Fprintf(g.stderr, "claims-stall guard: the probe seat of %s on %s failed to claim too (%d seats in all); moving it to its standby\n", //nolint:errcheck // best-effort stderr
					seat.template, claimStallProviderLabel(ep.provider), ep.failures)
				g.engageLocked(seat.template, ep, at, events.AgentClaimsCauseProbe, fmt.Sprintf("its probe seat on %s failed to claim", claimStallProviderLabel(ep.provider)))
				return []events.Event{g.stalledEvent(seat.template, ep, events.AgentClaimsCauseProbe, ep.failures, at)}
			}
			why = " (no standby: " + why + ")"
		}
		g.holdLocked(ep, at)
		fmt.Fprintf(g.stderr, "claims-stall guard: the probe seat of %s on %s failed to claim too (%d seats in all); holding its routed demand until %s%s\n", //nolint:errcheck // best-effort stderr
			seat.template, claimStallProviderLabel(ep.provider), ep.failures, ep.until.UTC().Format(time.RFC3339), why)
		return []events.Event{g.stalledEvent(seat.template, ep, events.AgentClaimsCauseProbe, ep.failures, at)}
	}
	// An overdue seat's failure is dated at its deadline, so failures arrive
	// out of order: the window is the newest failure's.
	newest := at
	for _, f := range t.streak {
		newest = claimStallLater(newest, f.at)
	}
	streak := t.streak[:0]
	for _, f := range append(t.streak, claimStallFailure{at: at, trigger: seat.trigger}) {
		if newest.Sub(f.at) <= claimStallWindow {
			streak = append(streak, f)
		}
	}
	t.streak = streak
	n := len(t.streak)
	if n < claimStallTrip {
		return nil
	}
	// The failures arrive about in time order, not exactly (an overdue
	// seat's is dated at its deadline): the span is the streak's range.
	first, last := t.streak[0].at, t.streak[0].at
	triggers := map[string]struct{}{}
	for _, f := range t.streak {
		first, last = claimStallEarlier(first, f.at), claimStallLater(last, f.at)
		if f.trigger != "" {
			triggers[f.trigger] = struct{}{}
		}
	}
	if last.Sub(first) < claimStallMinSpan {
		return nil
	}
	t.streak = nil
	return g.tripLocked(seat.template, t, n, len(triggers), first, at)
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

// tripLocked opens template's episode on its current provider and moves the
// template to its standby when the standby can take it (standbyRefusalLocked)
// and the evidence is the provider's (providerEvidenceLocked), or else holds
// its routed demand. beads is how many distinct beads of work the failed
// seats waited on. Caller holds g.mu.
func (g *claimStallGuard) tripLocked(template string, t *claimStallTemplate, seats, beads int, since, at time.Time) []events.Event {
	ep := &claimStallEpisode{
		id:       claimStallEpisodeID(template, t.provider, since),
		provider: t.provider,
		since:    since,
		failures: seats,
		holds:    1,
		until:    claimStallLater(at, g.now()).Add(claimStallHold(1)),
		action:   events.AgentClaimsHeld,
	}
	if t.episodes == nil {
		t.episodes = map[string]*claimStallEpisode{}
	}
	t.episodes[t.provider] = ep
	evidence := g.providerEvidenceLocked(template, t, beads, since)
	ep.movable = evidence == ""
	why := g.standbyRefusalLocked(template)
	if why == "" {
		why = evidence
	}
	if why == "" {
		g.engageLocked(template, ep, at, events.AgentClaimsCauseSeats, fmt.Sprintf("%d seats in a row failed to claim on %s", seats, claimStallProviderLabel(t.provider)))
		fmt.Fprintf(g.stderr, "claims-stall guard: %s on %s: %d seats in a row failed to claim the work routed to them since %s; moving it to its standby %s until %s\n", //nolint:errcheck // best-effort stderr
			template, claimStallProviderLabel(t.provider), seats, since.UTC().Format(time.RFC3339), g.moves[template].provider, ep.until.UTC().Format(time.RFC3339))
		return []events.Event{g.stalledEvent(template, ep, events.AgentClaimsCauseSeats, seats, at)}
	}
	fmt.Fprintf(g.stderr, "claims-stall guard: %s on %s: %d seats in a row failed to claim the work routed to them since %s; holding its routed demand until %s (no standby: %s)\n", //nolint:errcheck // best-effort stderr
		template, claimStallProviderLabel(t.provider), seats, since.UTC().Format(time.RFC3339), ep.until.UTC().Format(time.RFC3339), why)
	return []events.Event{g.stalledEvent(template, ep, events.AgentClaimsCauseSeats, seats, at)}
}

// standbyRefusalLocked is why template cannot be moved to its standby now, or
// "" when it can: it has a standby its config accepts and its seats can start
// on (claimStallLaneOf), the v2 reconciler runs, and the provider-health
// record does not mark the standby unhealthy (the reconciler would start no
// seat there). Caller holds g.mu.
func (g *claimStallGuard) standbyRefusalLocked(template string) string {
	lane := g.lanes[template]
	switch {
	case lane.standby == "" && lane.refused != "":
		return lane.refused
	case lane.standby == "":
		return "it has no usable standby_provider"
	case !g.standbyAllowed:
		return "a standby move needs the v2 session reconciler"
	case claimStallRed(g.health, lane.standby):
		return "the provider-health record marks its standby_provider " + lane.standby + " unhealthy"
	}
	return ""
}

// providerEvidenceLocked is why template's stall on its current provider is
// not its own provider's to move off, or "" when it is: the stall is on its
// own provider, the failed seats waited on at least claimStallStandbyEvidence
// beads of work (one bead failing for every seat may be that bead), and a seat
// on some other provider claimed since the streak began (no claim anywhere
// may be the city failing, which a standby does not fix). Caller holds g.mu.
func (g *claimStallGuard) providerEvidenceLocked(template string, t *claimStallTemplate, beads int, since time.Time) string {
	lane := g.lanes[template]
	switch {
	case t.provider != lane.own:
		return "it already runs on its standby"
	case beads < claimStallStandbyEvidence:
		return fmt.Sprintf("its seats waited on %d bead(s) of work only", beads)
	case !g.otherProviderClaimedLocked(lane.own, since):
		return "no seat on another provider claimed meanwhile"
	}
	return ""
}

// claimStallRed reports whether the provider-health record marks provider
// unhealthy.
func claimStallRed(health *providerHealthSnapshot, provider string) bool {
	healthy, present := health.check(provider)
	return present && !healthy
}

// otherProviderClaimedLocked reports whether a seat on a provider other than
// provider claimed at or after since. Caller holds g.mu.
func (g *claimStallGuard) otherProviderClaimedLocked(provider string, since time.Time) bool {
	for p, at := range g.lastClaim {
		if p != provider && !at.Before(since) {
			return true
		}
	}
	return false
}

// engageLocked moves template to its standby for ep: the move's hold doubles
// with each move in the episode. While the template still starts seats on
// the stalled provider (until the reload), its demand stays held. Caller
// holds g.mu.
func (g *claimStallGuard) engageLocked(template string, ep *claimStallEpisode, at time.Time, cause, reason string) {
	lane := g.lanes[template]
	from := claimStallLater(at, g.now()) // a failure may be dated in the past; the move is made now
	if t := g.templates[template]; t != nil && lane.standby != lane.own {
		delete(t.episodes, lane.standby) // a stall of the standby itself, from an earlier move, is over
	}
	ep.engagements++
	ep.action = events.AgentClaimsStandby
	ep.until = from.Add(claimStallStandbyHoldFor(ep.engagements))
	ep.probing = false
	ep.movable = true
	g.moves[template] = &claimStallMove{
		provider: lane.standby, replaced: lane.own, since: from, until: ep.until,
		engagements: ep.engagements, episode: ep.id, reason: reason, cause: cause,
	}
}

// dropMoveLocked drops template's move, for cause: when its episode still
// stands, the template returns to its own provider and one probe seat is
// admitted there at once. Caller holds g.mu.
func (g *claimStallGuard) dropMoveLocked(template, cause string, now time.Time) {
	m := g.moves[template]
	if m == nil {
		return
	}
	delete(g.moves, template)
	g.dropped[template] = cause
	if t := g.templates[template]; t != nil {
		if ep := t.episodes[m.replaced]; ep != nil {
			ep.until, ep.probing = now, false
		}
	}
}

// unmakeMoveLocked takes back template's move m, chosen since the record was
// last written, whose standby the provider-health record marks unhealthy
// before the write: the move was never made, so it is not counted, and the
// template is held as a failed probe holds it, where ending a made move would
// probe the own provider at once. The episode keeps its standby for a later
// failed probe. Caller holds g.mu.
func (g *claimStallGuard) unmakeMoveLocked(template string, m *claimStallMove, now time.Time) []events.Event {
	delete(g.moves, template)
	t := g.templates[template]
	if t == nil || t.episodes[m.replaced] == nil {
		return nil
	}
	ep := t.episodes[m.replaced]
	ep.engagements = m.engagements - 1
	g.holdLocked(ep, now)
	fmt.Fprintf(g.stderr, "claims-stall guard: %s's move to %s is not made: the provider-health record marks %s unhealthy; holding its routed demand until %s\n", //nolint:errcheck // best-effort stderr
		template, m.provider, m.provider, ep.until.UTC().Format(time.RFC3339))
	return []events.Event{g.stalledEvent(template, ep, m.cause, ep.failures, now)}
}

// holdLocked holds ep's template's routed demand again, from at or now,
// whichever is later: each hold of an episode doubles the last, up to
// claimStallHoldMax. Caller holds g.mu.
func (g *claimStallGuard) holdLocked(ep *claimStallEpisode, at time.Time) {
	ep.action = events.AgentClaimsHeld
	ep.holds++
	ep.until = claimStallLater(at, g.now()).Add(claimStallHold(ep.holds))
	ep.probing = false
}

// claimStallStandbyHoldFor is how long the n-th standby move of an episode
// lasts.
func claimStallStandbyHoldFor(n int) time.Duration {
	hold := claimStallStandbyHold
	for i := 1; i < n && hold < claimStallStandbyHoldMax; i++ {
		hold *= 2
	}
	return min(hold, claimStallStandbyHoldMax)
}

// claimedLocked counts a new claim of seat's, made at claimedAt. Every claim
// of a counted seat, a reused seat's later ones too, is its provider's
// evidence that it works (lastClaim). The seat's first claim also settles it:
// one newer than the streak's first failure ends the streak, and one newer
// than the stall's start ends the stall on the provider that stalled. An older
// claim (a seat first seen after a controller restart, carrying claims from
// before) only settles the seat. Caller holds g.mu.
func (g *claimStallGuard) claimedLocked(seat *claimStallSeat, claimedAt time.Time) []events.Event {
	t := g.templateFor(seat.template)
	counts := t.counts(seat) && !claimedAt.IsZero()
	if counts && t.provider != "" {
		g.lastClaim[t.provider] = claimStallLater(g.lastClaim[t.provider], claimedAt)
	}
	if seat.claimed {
		return nil
	}
	seat.claimed, seat.settled = true, true
	if !counts {
		return nil
	}
	if len(t.streak) > 0 {
		first := t.streak[0].at
		for _, f := range t.streak {
			first = claimStallEarlier(first, f.at)
		}
		if !claimedAt.Before(first) {
			t.streak = nil
		}
	}
	ep := t.stalled()
	if ep == nil || claimedAt.Before(ep.since) {
		return nil
	}
	delete(t.episodes, t.provider)
	if m := g.moves[seat.template]; m != nil && ep.provider == m.replaced {
		// The own provider claims again: the stall is over, and so is the
		// move. A stall of the standby ending changes neither.
		g.dropMoveLocked(seat.template, events.AgentClaimsCauseClaimed, g.now())
	}
	fmt.Fprintf(g.stderr, "claims-stall guard: %s claims its work again on %s; its routed demand counts again\n", seat.template, claimStallProviderLabel(ep.provider)) //nolint:errcheck // best-effort stderr
	return []events.Event{g.resumedEvent(seat.template, ep, events.AgentClaimsCauseClaimed, claimedAt)}
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

// stalledEvent is the agent.claims_stalled alert for ep. Caller holds g.mu.
func (g *claimStallGuard) stalledEvent(template string, ep *claimStallEpisode, cause string, seats int, at time.Time) events.Event {
	until := ep.until.UTC()
	p := events.AgentClaimsPayload{
		Template: template,
		Provider: ep.provider,
		Episode:  ep.id,
		Action:   ep.action,
		Cause:    cause,
		Seats:    seats,
		Since:    ep.since.UTC(),
		Until:    &until,
	}
	var what string
	switch cause {
	case events.AgentClaimsCauseProbe:
		what = fmt.Sprintf("its probe seat failed to claim too (%d seats in all)", seats)
	case events.AgentClaimsCauseProviderHealth:
		what = "the provider-health record marks it unhealthy"
	default:
		what = fmt.Sprintf("%d of its seats in a row failed to claim the work routed to them while it waited", seats)
	}
	did := fmt.Sprintf("No new seats are spawned for its routed work until %s, then one probe seat; a claim ends the stall (a custom scale_check count is not withheld).", until.Format(time.RFC3339))
	if m := g.moves[template]; ep.action == events.AgentClaimsStandby && m != nil {
		p.Standby, p.Engagements = m.provider, ep.engagements
		did = fmt.Sprintf("Its new seats start on its standby %s (move %d of this stall) until %s, when one probe seat goes back to %s; a claim there ends the stall. Recorded in %s.",
			m.provider, ep.engagements, until.Format(time.RFC3339), claimStallProviderLabel(ep.provider), config.StandbyPath(g.cityPath))
	}
	payload, _ := json.Marshal(p) //nolint:errchkjson // a struct of strings, ints and times always marshals
	return events.Event{
		Type:    events.AgentClaimsStalled,
		Ts:      claimStallLater(at, g.now()).UTC(),
		Actor:   "controller",
		Subject: template,
		Message: fmt.Sprintf("Agent %s stopped claiming its work on %s: %s (since %s). %s",
			template, claimStallProviderLabel(ep.provider), what, ep.since.UTC().Format(time.RFC3339), did),
		Payload: payload,
	}
}

// standbyEvent is the agent.standby_engaged or agent.standby_released record
// of a move.
func standbyEvent(typ, template string, m config.StandbyEntry, cause string, at time.Time) events.Event {
	until := m.Until.UTC()
	payload, _ := json.Marshal(events.AgentClaimsPayload{ //nolint:errchkjson // a struct of strings, ints and times always marshals
		Template:    template,
		Provider:    m.Replaced,
		Episode:     m.Episode,
		Action:      events.AgentClaimsStandby,
		Cause:       cause,
		Standby:     m.Provider,
		Engagements: m.Engagements,
		Since:       m.Since.UTC(),
		Until:       &until,
	})
	msg := fmt.Sprintf("Agent %s now starts its seats on its standby %s in place of %s, until %s (%s).", template, m.Provider, m.Replaced, until.Format(time.RFC3339), m.Reason)
	if typ == events.AgentStandbyReleased {
		msg = fmt.Sprintf("Agent %s starts its seats on its own provider %s again (it ran on %s from %s): %s.", template, m.Replaced, m.Provider, m.Since.UTC().Format(time.RFC3339), cause)
	}
	return events.Event{Type: typ, Ts: at.UTC(), Actor: "controller", Subject: template, Message: msg, Payload: payload}
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
		Ts:      claimStallLater(at, g.now()).UTC(),
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

// withhold is withholdUnder with no config to check against.
func (g *claimStallGuard) withhold(counts map[string]int, demand map[string]scaleCheckDemand) (map[string]int, map[string]scaleCheckDemand) {
	return g.withholdUnder(nil, counts, demand)
}

// withholdUnder records each template's default routed demand, counts the
// seats that have been awake past the claim deadline without claiming it, and
// removes the demand of every held template from one pass: all of it while
// the hold stands, all but one bead (the probe) when the hold runs out. cfg is
// the config the pass launches seats under: a template the guard is managing
// (a stall or a standby move) whose provider there differs from the guard's
// is mid-reload (the guard adopted a config the pass has not, or the other
// way round), and spawns nothing until both agree, so no seat starts on a
// provider the guard does not count it under. It never edits its arguments;
// with nothing held it returns them unchanged.
func (g *claimStallGuard) withholdUnder(cfg *config.City, counts map[string]int, demand map[string]scaleCheckDemand) (map[string]int, map[string]scaleCheckDemand) {
	if g == nil {
		return counts, demand
	}
	now := g.now()
	passProviders := claimStallPassProviders(cfg, demand)
	g.mu.Lock()
	g.sweep(now)
	g.recordDemandLocked(counts, demand)
	alerts := g.overdueLocked(now)
	var outCounts map[string]int
	var outDemand map[string]scaleCheckDemand
	for _, name := range sortedDemandTemplates(demand) {
		t := g.templates[name]
		if t == nil {
			continue
		}
		entry := demand[name]
		if len(entry.WorkBeadIDs) == 0 {
			continue // nothing to spawn for: a probe admitted now would be spent on nothing
		}
		_, recorded := g.recorded[name]
		managed := len(t.episodes) > 0 || g.moves[name] != nil || recorded
		passProvider, known := passProviders[name]
		var keep int
		switch ep := t.stalled(); {
		case managed && known && passProvider != t.provider:
			keep = 0
		case ep != nil:
			keep = g.admittedLocked(name, ep, now)
		default:
			continue
		}
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

// claimStallPassProviders is the provider each template in demand starts its
// seats on under cfg; nil when there is no cfg to say.
func claimStallPassProviders(cfg *config.City, demand map[string]scaleCheckDemand) map[string]string {
	if cfg == nil || len(demand) == 0 {
		return nil
	}
	out := make(map[string]string, len(demand))
	for i := range cfg.Agents {
		a := &cfg.Agents[i]
		if name := a.QualifiedName(); hasDemandEntry(demand, name) {
			out[name] = claimStallCurrentProvider(a, &cfg.Workspace)
		}
	}
	return out
}

func hasDemandEntry(demand map[string]scaleCheckDemand, name string) bool {
	_, ok := demand[name]
	return ok
}

// recordDemandLocked replaces the waiting work of every template this pass
// probed (a key of counts) with its default routed demand, less the beads the
// journal showed leave the open state since the snapshot was read, and
// rebuilds the reverse index. A template the pass did not probe keeps its
// waiting work: a pass that builds one template's demand (the control
// dispatcher's) says nothing about the others. Caller holds g.mu.
func (g *claimStallGuard) recordDemandLocked(counts map[string]int, demand map[string]scaleCheckDemand) {
	probed := make(map[string]struct{}, len(counts)+len(demand))
	for name := range counts {
		probed[name] = struct{}{}
	}
	for name := range demand {
		probed[name] = struct{}{}
	}
	for name := range probed {
		t := g.templateFor(name)
		entry := demand[name]
		t.waiting = make(map[string]struct{}, len(entry.WorkBeadIDs))
		for _, id := range entry.WorkBeadIDs {
			if _, gone := g.left[id]; gone {
				continue
			}
			t.waiting[id] = struct{}{}
		}
	}
	clear(g.waitedBy)
	for name, t := range g.templates {
		for id := range t.waiting {
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

// syncStandby is the tick's step for the standby record, before the config
// reload: it moves a template whose own provider the provider-health record
// marks unhealthy onto its standby, ends each move whose hold ran out or
// whose own provider another template's seat proved by claiming, writes the
// record when the moves the guard wants differ from it, and records
// agent.standby_engaged and agent.standby_released for the difference. It
// reports whether the record changed, so the caller reloads the config.
func (g *claimStallGuard) syncStandby(health *providerHealthSnapshot) (bool, error) {
	if g == nil {
		return false, nil
	}
	now := g.now()
	var alerts []events.Event
	g.mu.Lock()
	g.health = health
	g.rereadRecordLocked(now)
	for _, name := range g.sortedLaneNamesLocked() {
		lane := g.lanes[name]
		if g.moves[name] != nil || lane.current != lane.own {
			continue
		}
		if healthy, present := health.check(lane.own); !present || healthy {
			continue
		}
		if g.standbyRefusalLocked(name) != "" {
			continue // no standby that can take its seats, a red one included
		}
		t := g.templateFor(name)
		if len(t.waiting) == 0 {
			continue // an idle lane has nothing to move for: it moves when work waits
		}
		ep := t.episodes[lane.own]
		if ep == nil {
			ep = &claimStallEpisode{id: claimStallEpisodeID(name, lane.own, now), provider: lane.own, since: now, holds: 1}
			if t.episodes == nil {
				t.episodes = map[string]*claimStallEpisode{}
			}
			t.episodes[lane.own] = ep
		}
		g.engageLocked(name, ep, now, events.AgentClaimsCauseProviderHealth, fmt.Sprintf("the provider-health record marks %s unhealthy", lane.own))
		alerts = append(alerts, g.stalledEvent(name, ep, events.AgentClaimsCauseProviderHealth, ep.failures, now))
	}
	for _, name := range g.sortedMoveNamesLocked() {
		m := g.moves[name]
		r, recorded := g.recorded[name]
		switch {
		case claimStallRed(health, m.replaced):
			// The pack still marks the own provider unhealthy, and the
			// reconciler would start no probe seat on it: the move stands.
		case claimStallRed(health, m.provider) && (!recorded || !r.Since.Equal(m.since)):
			// The standby went red after the move was chosen on an older
			// record, and before it was written: it was never made.
			alerts = append(alerts, g.unmakeMoveLocked(name, m, now)...)
		case claimStallRed(health, m.provider):
			// The standby went red too, and the reconciler starts no seat
			// on it: back to the own provider, which may have recovered.
			g.dropMoveLocked(name, events.AgentClaimsCauseProviderHealth, now)
		case m.cause == events.AgentClaimsCauseProviderHealth:
			g.dropMoveLocked(name, events.AgentClaimsCauseProviderHealth, now)
		case !now.Before(m.until):
			g.dropMoveLocked(name, events.AgentClaimsCauseHoldEnded, now)
		case recorded && m.engagements <= 1 && g.lastClaim[m.replaced].After(m.since):
			// Another agent's seat claimed on the own provider: the outage
			// may be over. Only for a first move: once a probe has failed,
			// the failure was this agent's, whatever its neighbors do.
			g.dropMoveLocked(name, events.AgentClaimsCauseClaimed, now)
		}
	}
	want := map[string]config.StandbyEntry{}
	for name, m := range g.moves {
		want[name] = config.StandbyEntry{Name: name, Provider: m.provider, Replaced: m.replaced, Since: m.since.UTC(), Until: m.until.UTC(), Engagements: m.engagements, Episode: m.episode, Cause: m.cause, Reason: m.reason}
	}
	changed := len(want) != len(g.recorded)
	for name, e := range want {
		if r, ok := g.recorded[name]; !ok || !reflectStandbyEntryEqual(r, e) {
			changed = true
		}
	}
	if !changed || g.fs == nil {
		g.mu.Unlock()
		g.emit(alerts)
		return false, nil
	}
	f := config.StandbyFile{}
	for _, e := range want {
		f.Agents = append(f.Agents, e)
	}
	if err := config.WriteStandby(g.fs, g.cityPath, f); err != nil {
		g.mu.Unlock()
		g.emit(alerts)
		return false, err
	}
	for _, name := range sortedStandbyNames(want, g.recorded) {
		e, engaged := want[name]
		r, was := g.recorded[name]
		switch {
		case engaged && (!was || r.Provider != e.Provider || !r.Since.Equal(e.Since)):
			fmt.Fprintf(g.stderr, "claims-stall guard: %s now runs on its standby %s in place of %s until %s\n", name, e.Provider, e.Replaced, e.Until.Format(time.RFC3339)) //nolint:errcheck // best-effort stderr
			alerts = append(alerts, standbyEvent(events.AgentStandbyEngaged, name, e, "", now))
		case !engaged && was:
			cause := g.dropped[name]
			if cause == "" {
				cause = events.AgentClaimsCauseConfigChanged // the record named a move the config never allowed
			}
			fmt.Fprintf(g.stderr, "claims-stall guard: %s returns to its own provider %s (%s)\n", name, r.Replaced, cause) //nolint:errcheck // best-effort stderr
			alerts = append(alerts, standbyEvent(events.AgentStandbyReleased, name, r, cause, now))
		}
	}
	g.recorded = want
	clear(g.dropped)
	g.mu.Unlock()
	g.emit(alerts)
	return true, nil
}

// rereadRecordLocked ends each move whose entry a person deleted from the
// standby record (or deleted the record): the template returns to its own
// provider for a probe, and the next write does not bring the entry back.
// A record that cannot be read changes nothing. Caller holds g.mu.
func (g *claimStallGuard) rereadRecordLocked(now time.Time) {
	if g.fs == nil || len(g.recorded) == 0 {
		return
	}
	f, _, err := config.LoadStandby(g.fs, g.cityPath)
	if err != nil {
		return
	}
	onDisk := make(map[string]bool, len(f.Agents))
	for _, e := range f.Agents {
		onDisk[e.Name] = true
	}
	for _, name := range sortedStandbyNames(g.recorded, nil) {
		if !onDisk[name] && g.moves[name] != nil {
			fmt.Fprintf(g.stderr, "claims-stall guard: %s's move was deleted from the standby record; it returns to its own provider\n", name) //nolint:errcheck // best-effort stderr
			g.dropMoveLocked(name, events.AgentClaimsCauseConfigChanged, now)
		}
	}
}

// sortedMoveNamesLocked is the moved templates in order. Caller holds g.mu.
func (g *claimStallGuard) sortedMoveNamesLocked() []string {
	names := make([]string, 0, len(g.moves))
	for name := range g.moves {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func reflectStandbyEntryEqual(a, b config.StandbyEntry) bool {
	return a.Name == b.Name && a.Provider == b.Provider && a.Replaced == b.Replaced && a.Since.Equal(b.Since) &&
		a.Until.Equal(b.Until) && a.Engagements == b.Engagements && a.Episode == b.Episode && a.Cause == b.Cause && a.Reason == b.Reason
}

// sortedStandbyNames is every template either set names, in order.
func sortedStandbyNames(a, b map[string]config.StandbyEntry) []string {
	seen := map[string]struct{}{}
	for name := range a {
		seen[name] = struct{}{}
	}
	for name := range b {
		seen[name] = struct{}{}
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// sortedLaneNamesLocked is the configured templates in order. Caller holds
// g.mu.
func (g *claimStallGuard) sortedLaneNamesLocked() []string {
	names := make([]string, 0, len(g.lanes))
	for name := range g.lanes {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
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
