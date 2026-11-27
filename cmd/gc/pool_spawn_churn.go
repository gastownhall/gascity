package main

// The pool spawn-churn guard (ga-s5l1ms).
//
// A custom scale_check is a bare count: it can report more seats than the
// work beads any probe names. The planner spawns the remainder blind, with no
// work bead bound to it, and each blind seat wakes, finds nothing its hook
// serves, and drains, only to be replaced by the next pass. Nothing stops that
// churn when the count and the hook disagree for good.
//
// The guard is its mechanical backstop, per template:
//
//   - Input, off the city's event journal (the delta lanes' feed). A seat
//     COUNTS when the guard saw its session bead created, it is pool managed,
//     it was never stamped with gc.trigger_bead_id (no demand named work for
//     it, at any point of its life), and it is neither a named nor a manual
//     session. A counted seat CHURNS when it closes after it woke without
//     having claimed any work, unless it closed failed-create or its hook
//     reported a failed claim write (bead.claim_failed: it drains
//     claims_errored, a store fault the claim-loop guard watches, not churn).
//   - Trip: poolSpawnChurnTrip churned closes of a template's seats, within
//     poolSpawnChurnWindow, the first and the last at least
//     poolSpawnChurnMinSpan apart: one pass's burst cannot trip it alone.
//   - Hold: the template's count is clamped to its named work beads, so no
//     blind seat is spawned, until the hold ends; seats for named work,
//     protected seats and floors are unaffected (clampBlindSpawnDemand).
//     session.pool_spawn_churn_cooling_down names the template, the streak
//     and the hold's end.
//   - Probe: when the hold ends, one blind seat is admitted. A counted seat
//     created at or after the hold's end that churns is that probe failing:
//     the hold doubles, up to poolSpawnChurnHoldMax, and the cooling-down
//     event is recorded again in the episode. A seat created before the hold
//     ended is a straggler, and its close is ignored.
//   - Resume: the first claim by a counted seat clears the streak and ends
//     the episode (session.pool_spawn_churn_resumed); a seat claiming again
//     does not. A config change to the template's scale_check, work_query,
//     scale_check_authoritative or provider resets it the same way, and
//     seats created before the change count for nothing.
//
// It holds no judgment: the thresholds are constants, the comparisons are
// counts and times, and it never touches a bead; only the controller's spawn
// count changes. Its state is in memory: a controller restart forgets it.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/session"
)

// Thresholds are consts, not config, for the claim-stall guard's reason: each
// is a knob that could hide the churn it exists to stop.
const (
	// poolSpawnChurnTrip churned closes in a row trip a template.
	poolSpawnChurnTrip = 3
	// poolSpawnChurnMinSpan is how far apart the first and the last churned
	// close of a tripping streak must be.
	poolSpawnChurnMinSpan = 2 * time.Minute
	// poolSpawnChurnWindow is how long a churned close stays in the streak.
	poolSpawnChurnWindow = 30 * time.Minute
	// poolSpawnChurnHoldBase is the first hold of an episode; each failed
	// probe doubles it, up to poolSpawnChurnHoldMax.
	poolSpawnChurnHoldBase = 5 * time.Minute
	poolSpawnChurnHoldMax  = 30 * time.Minute
	// poolSpawnChurnForgetAfter is how long a seat the journal is silent
	// about is remembered.
	poolSpawnChurnForgetAfter = 6 * time.Hour
)

// poolSpawnChurnSeat is what the guard knows of one session bead.
type poolSpawnChurnSeat struct {
	template string
	observed bool // the guard saw its creation
	created  time.Time
	pool     bool // pool_managed
	bound    bool // stamped gc.trigger_bead_id at some point
	excluded bool // a named or manual session
	awake    bool // creation_complete_at set
	claimed  bool
	errored  bool // its hook reported a failed claim write
	closed   bool
	seen     time.Time
}

// poolSpawnChurnTemplate is one template's streak of churned closes and its
// open episode.
type poolSpawnChurnTemplate struct {
	streak  []time.Time
	episode *poolSpawnChurnEpisode
}

// poolSpawnChurnSignature is what of a template's config decides its blind
// demand and how its seats run: a change to it may be the fix.
type poolSpawnChurnSignature struct {
	scaleCheck, workQuery, provider string
	authoritative                   bool
}

// poolSpawnChurnEpisode is one run of churn, from its trip to what ends it.
type poolSpawnChurnEpisode struct {
	id    string
	since time.Time // the first churned close of the streak that tripped it
	seats int       // churned seats in all
	holds int
	until time.Time // blind demand is withheld until this time
}

// poolSpawnChurnGuard is one city's guard. Its methods are safe for concurrent
// use: observe runs on the journal feed's goroutine, allowance on the demand
// pass's and configure on the reload's.
type poolSpawnChurnGuard struct {
	mu         sync.Mutex
	now        func() time.Time
	record     func(events.Event)
	stderr     io.Writer
	signatures map[string]poolSpawnChurnSignature // nil until configured
	// epochs are when each template's config last changed: seats created
	// before count for nothing.
	epochs    map[string]time.Time
	templates map[string]*poolSpawnChurnTemplate
	seats     map[string]*poolSpawnChurnSeat
}

func newPoolSpawnChurnGuard(record func(events.Event), stderr io.Writer) *poolSpawnChurnGuard {
	if stderr == nil {
		stderr = io.Discard
	}
	return &poolSpawnChurnGuard{
		now:       time.Now,
		record:    record,
		stderr:    stderr,
		epochs:    map[string]time.Time{},
		templates: map[string]*poolSpawnChurnTemplate{},
		seats:     map[string]*poolSpawnChurnSeat{},
	}
}

// configure adopts cfg's templates. A template whose signature changed, or
// that is gone, ends its episode (resumed, cause config_changed) and drops its
// streak; a changed one starts a new epoch.
func (g *poolSpawnChurnGuard) configure(cfg *config.City) {
	if g == nil {
		return
	}
	providers := claimStallProviders(cfg)
	signatures := map[string]poolSpawnChurnSignature{}
	if cfg != nil {
		for i := range cfg.Agents {
			a := &cfg.Agents[i]
			name := a.QualifiedName()
			signatures[name] = poolSpawnChurnSignature{
				scaleCheck:    strings.TrimSpace(a.ScaleCheck),
				workQuery:     strings.TrimSpace(a.WorkQuery),
				provider:      providers[name],
				authoritative: a.ScaleCheckAuthoritative,
			}
		}
	}
	now := g.now()
	var alerts []events.Event
	g.mu.Lock()
	previous := g.signatures
	g.signatures = signatures
	if previous != nil {
		for name, sig := range signatures {
			if old, ok := previous[name]; ok && old != sig {
				g.epochs[name] = now
			}
		}
	}
	for name, t := range g.templates {
		sig, ok := signatures[name]
		if ok && (previous == nil || previous[name] == sig) {
			continue
		}
		if ep := t.episode; ep != nil {
			fmt.Fprintf(g.stderr, "pool spawn-churn guard: %s's hold ends with a config change\n", name) //nolint:errcheck // best-effort stderr
			alerts = append(alerts, g.resumedEvent(name, ep, events.PoolSpawnChurnCauseConfigChanged, now))
		}
		delete(g.templates, name)
	}
	for name := range g.epochs {
		if _, ok := signatures[name]; !ok {
			delete(g.epochs, name)
		}
	}
	g.mu.Unlock()
	g.emit(alerts)
}

// observe folds one journal event into the guard.
func (g *poolSpawnChurnGuard) observe(evt events.Event) {
	if g == nil {
		return
	}
	switch evt.Type {
	case events.BeadCreated, events.BeadUpdated, events.BeadClosed:
	case events.BeadClaimFailed:
		g.observeClaimFailed(evt)
		return
	default:
		return
	}
	if !bytes.Contains(evt.Payload, claimStallSessionMarker) {
		return
	}
	b, ok := beads.DecodeBeadEventPayload(evt.Payload)
	if !ok || b.Type != session.BeadType {
		return
	}
	at := evt.Ts
	if at.IsZero() {
		at = g.now()
	}
	g.emit(g.observeSeat(b, evt.Type, at))
}

// observeClaimFailed marks the seat whose claim write failed: unless it
// claims after all, it closes claims_errored, which is not churn.
func (g *poolSpawnChurnGuard) observeClaimFailed(evt events.Event) {
	var p events.BeadClaimFailedPayload
	if err := json.Unmarshal(evt.Payload, &p); err != nil {
		return
	}
	id := strings.TrimSpace(firstNonEmpty(p.SessionID, evt.SessionID))
	g.mu.Lock()
	defer g.mu.Unlock()
	if seat := g.seats[id]; seat != nil {
		seat.errored = true
	}
}

// emit records alerts outside the lock.
func (g *poolSpawnChurnGuard) emit(alerts []events.Event) {
	if g.record == nil {
		return
	}
	for _, e := range alerts {
		g.record(e)
	}
}

// observeSeat folds one session bead's snapshot.
func (g *poolSpawnChurnGuard) observeSeat(b beads.Bead, kind string, at time.Time) []events.Event {
	template := strings.TrimSpace(b.Metadata["template"])
	if template == "" || b.ID == "" {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.sweep(at)
	seat := g.seats[b.ID]
	if seat == nil {
		seat = &poolSpawnChurnSeat{template: template, observed: kind == events.BeadCreated, created: at}
		if !b.CreatedAt.IsZero() {
			seat.created = b.CreatedAt
		}
		g.seats[b.ID] = seat
	}
	seat.seen = at
	if strings.TrimSpace(b.Metadata["pool_managed"]) == "true" {
		seat.pool = true
	}
	if strings.TrimSpace(b.Metadata[beadmeta.TriggerBeadIDMetadataKey]) != "" {
		seat.bound = true
	}
	if session.IsNamedSessionBead(b) || isManualSessionBead(b) {
		seat.excluded = true
	}
	if strings.TrimSpace(b.Metadata["creation_complete_at"]) != "" {
		seat.awake = true
	}
	var alerts []events.Event
	if _, claimed := claimStallNewestClaim(b.Metadata[session.WorkStartedKey]); claimed && !seat.claimed {
		seat.claimed = true
		if g.countsLocked(seat) {
			alerts = g.claimedLocked(seat, at)
		}
	}
	if kind != events.BeadClosed || seat.closed {
		return alerts
	}
	seat.closed = true
	failedCreate := strings.TrimSpace(b.Metadata["state"]) == "failed-create"
	if !g.countsLocked(seat) || !seat.awake || seat.claimed || seat.errored || failedCreate {
		return alerts
	}
	return append(alerts, g.churnedLocked(seat, at)...)
}

// countsLocked reports whether seat counts toward its template: a blind pool
// seat whose creation the guard saw, since the template's config last
// changed. A store may keep a creation time to the second only, so the
// change's is compared to the second too. Caller holds g.mu.
func (g *poolSpawnChurnGuard) countsLocked(seat *poolSpawnChurnSeat) bool {
	if !seat.observed || !seat.pool || seat.bound || seat.excluded {
		return false
	}
	epoch, ok := g.epochs[seat.template]
	return !ok || !seat.created.Before(epoch.Truncate(time.Second))
}

// claimedLocked counts a counted seat's first claim: the template's streak
// clears and its episode ends. Caller holds g.mu.
func (g *poolSpawnChurnGuard) claimedLocked(seat *poolSpawnChurnSeat, at time.Time) []events.Event {
	t := g.templates[seat.template]
	if t == nil {
		return nil
	}
	t.streak = nil
	ep := t.episode
	if ep == nil {
		return nil
	}
	t.episode = nil
	fmt.Fprintf(g.stderr, "pool spawn-churn guard: a seat of %s claimed work; its blind demand counts again\n", seat.template) //nolint:errcheck // best-effort stderr
	return []events.Event{g.resumedEvent(seat.template, ep, events.PoolSpawnChurnCauseClaimed, at)}
}

// churnedLocked counts seat's churned close at at. Caller holds g.mu.
func (g *poolSpawnChurnGuard) churnedLocked(seat *poolSpawnChurnSeat, at time.Time) []events.Event {
	t := g.templateFor(seat.template)
	if ep := t.episode; ep != nil {
		if seat.created.Before(ep.until.Truncate(time.Second)) {
			return nil
		}
		return g.probeFailedLocked(seat.template, ep, at)
	}
	streak := t.streak[:0]
	for _, c := range t.streak {
		if at.Sub(c) < poolSpawnChurnWindow {
			streak = append(streak, c)
		}
	}
	streak = append(streak, at)
	t.streak = streak
	first := t.streak[0]
	if len(t.streak) < poolSpawnChurnTrip || at.Sub(first) < poolSpawnChurnMinSpan {
		return nil
	}
	return g.tripLocked(seat.template, t, len(t.streak), first, at)
}

// templateFor is name's state, created on first use. Caller holds g.mu.
func (g *poolSpawnChurnGuard) templateFor(name string) *poolSpawnChurnTemplate {
	t := g.templates[name]
	if t == nil {
		t = &poolSpawnChurnTemplate{}
		g.templates[name] = t
	}
	return t
}

// tripLocked opens template's episode and holds its blind demand. Caller
// holds g.mu.
func (g *poolSpawnChurnGuard) tripLocked(template string, t *poolSpawnChurnTemplate, seats int, since, at time.Time) []events.Event {
	ep := &poolSpawnChurnEpisode{
		id:    fmt.Sprintf("%s@%s", template, since.UTC().Format(time.RFC3339)),
		since: since,
		seats: seats,
		holds: 1,
		until: claimStallLater(at, g.now()).Add(poolSpawnChurnHold(1)),
	}
	t.episode, t.streak = ep, nil
	fmt.Fprintf(g.stderr, "pool spawn-churn guard: %d seats of %s in a row woke and closed without claiming work since %s; spawning no blind seat for it until %s\n", //nolint:errcheck // best-effort stderr
		seats, template, since.UTC().Format(time.RFC3339), ep.until.UTC().Format(time.RFC3339))
	return []events.Event{g.coolingDownEvent(template, ep, events.PoolSpawnChurnCauseTripped, at)}
}

// probeFailedLocked counts ep's probe seat churning: the hold doubles, up to
// poolSpawnChurnHoldMax, and the cooling-down event is recorded again. Caller
// holds g.mu.
func (g *poolSpawnChurnGuard) probeFailedLocked(template string, ep *poolSpawnChurnEpisode, at time.Time) []events.Event {
	ep.seats++
	ep.holds++
	ep.until = claimStallLater(at, g.now()).Add(poolSpawnChurnHold(ep.holds))
	fmt.Fprintf(g.stderr, "pool spawn-churn guard: the probe seat of %s woke and closed without claiming work (%d seats in all); spawning no blind seat for it until %s\n", //nolint:errcheck // best-effort stderr
		template, ep.seats, ep.until.UTC().Format(time.RFC3339))
	return []events.Event{g.coolingDownEvent(template, ep, events.PoolSpawnChurnCauseProbeFailed, at)}
}

// poolSpawnChurnHold is how long the holds-th hold of an episode lasts.
func poolSpawnChurnHold(holds int) time.Duration {
	hold := poolSpawnChurnHoldBase
	for i := 1; i < holds && hold < poolSpawnChurnHoldMax; i++ {
		hold *= 2
	}
	return min(hold, poolSpawnChurnHoldMax)
}

// resumedEvent is the session.pool_spawn_churn_resumed record that ends ep.
func (g *poolSpawnChurnGuard) resumedEvent(template string, ep *poolSpawnChurnEpisode, cause string, at time.Time) events.Event {
	payload, _ := json.Marshal(events.PoolSpawnChurnPayload{ //nolint:errchkjson // a struct of strings, ints and times always marshals
		Template: template,
		Episode:  ep.id,
		Cause:    cause,
		Seats:    ep.seats,
		Since:    ep.since.UTC(),
	})
	how := "a seat claimed work"
	if cause == events.PoolSpawnChurnCauseConfigChanged {
		how = "its config changed"
	}
	return events.Event{
		Type:    events.SessionPoolSpawnChurnResumed,
		Ts:      claimStallLater(at, g.now()).UTC(),
		Actor:   "controller",
		Subject: template,
		Message: fmt.Sprintf("Pool %s: %s; the spawn churn that began %s (%d seats) is over and its full blind demand counts again.",
			template, how, ep.since.UTC().Format(time.RFC3339), ep.seats),
		Payload: payload,
	}
}

// coolingDownEvent is the session.pool_spawn_churn_cooling_down record of ep.
func (g *poolSpawnChurnGuard) coolingDownEvent(template string, ep *poolSpawnChurnEpisode, cause string, at time.Time) events.Event {
	until := ep.until.UTC()
	payload, _ := json.Marshal(events.PoolSpawnChurnPayload{ //nolint:errchkjson // a struct of strings, ints and times always marshals
		Template: template,
		Episode:  ep.id,
		Cause:    cause,
		Seats:    ep.seats,
		Since:    ep.since.UTC(),
		Until:    &until,
	})
	return events.Event{
		Type:    events.SessionPoolSpawnChurnCoolingDown,
		Ts:      claimStallLater(at, g.now()).UTC(),
		Actor:   "controller",
		Subject: template,
		Message: fmt.Sprintf("Pool %s: %d seats spawned for demand that named no work woke and closed without claiming any (since %s). No such seat is spawned until %s; seats for named work beads are unaffected.",
			template, ep.seats, ep.since.UTC().Format(time.RFC3339), until.Format(time.RFC3339)),
		Payload: payload,
	}
}

// sweep forgets seats the journal has gone quiet about. Caller holds g.mu.
func (g *poolSpawnChurnGuard) sweep(now time.Time) {
	for id, seat := range g.seats {
		if now.Sub(seat.seen) > poolSpawnChurnForgetAfter {
			delete(g.seats, id)
		}
	}
}

// allowance is, for each template with an open episode, how many seats beyond
// its named work beads the demand merge may count: none while the hold
// stands, one (the probe) once it has ended. A template with no episode is
// absent; a nil guard allows everything.
func (g *poolSpawnChurnGuard) allowance() map[string]int {
	if g == nil {
		return nil
	}
	now := g.now()
	g.mu.Lock()
	defer g.mu.Unlock()
	var out map[string]int
	for name, t := range g.templates {
		if t.episode == nil {
			continue
		}
		if out == nil {
			out = map[string]int{}
		}
		out[name] = 0
		if !now.Before(t.episode.until) {
			out[name] = 1
		}
	}
	return out
}

// poolSpawnChurnGuards maps a normalized city path to its controller's guard,
// as claimStallGuards does: both demand passes reach it by city path, and a
// process that runs no controller registers none.
var poolSpawnChurnGuards sync.Map // normalized city path → *poolSpawnChurnGuard

// registerPoolSpawnChurnGuard publishes g for cityPath until ctx ends.
func registerPoolSpawnChurnGuard(ctx context.Context, cityPath string, g *poolSpawnChurnGuard) (unregister func()) {
	key := normalizePathForCompare(cityPath)
	poolSpawnChurnGuards.Store(key, g)
	unregister = func() { poolSpawnChurnGuards.CompareAndDelete(key, g) }
	if ctx != nil {
		context.AfterFunc(ctx, unregister)
	}
	return unregister
}

// poolSpawnChurnGuardFor is cityPath's registered guard, or nil.
func poolSpawnChurnGuardFor(cityPath string) *poolSpawnChurnGuard {
	if strings.TrimSpace(cityPath) == "" {
		return nil
	}
	g, ok := poolSpawnChurnGuards.Load(normalizePathForCompare(cityPath))
	if !ok {
		return nil
	}
	return g.(*poolSpawnChurnGuard)
}

// clampBlindSpawnDemand limits each template allowance lists to its named
// work beads plus its allowance: the blind remainder of its count, the seats
// no work bead is named for, is withheld. A control dispatcher is never
// limited: its count is the control work it must serve. It edits counts and
// demand in place, which the merge built for this pass; a template allowance
// does not list keeps its count.
func clampBlindSpawnDemand(cfg *config.City, allowance map[string]int, counts map[string]int, demand map[string]scaleCheckDemand) {
	for template, allowed := range allowance {
		count, ok := counts[template]
		if !ok {
			continue
		}
		if agent := findAgentByTemplate(cfg, template); agent != nil && config.IsDeterministicControlDispatcher(agent) {
			continue
		}
		entry, named := demand[template]
		limit := len(entry.WorkBeadIDs) + max(allowed, 0)
		if count > limit {
			counts[template] = limit
		}
		if named && entry.Count > limit {
			entry.Count = limit
			demand[template] = entry
		}
	}
}
