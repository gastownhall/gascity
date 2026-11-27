package main

// The pool spawn-churn guard (ga-s5l1ms).
//
// A pool template's custom scale_check reports a count of wanted seats; the
// planner binds as many of them to named work beads as the demand names and
// spawns the rest "blind", on the faith that the fresh seat's own hook will
// find the work through its own query. When the two queries read different
// populations — or both agree and the claim fails for a third reason — the
// blind seats drain no_work within seconds, the next pass counts the same
// demand, and the pool burns a seat every few seconds for work nothing can
// serve: 46 seats in 18.5 minutes on one pool, none useful.
//
// The guard bounds that damage whatever the root cause is:
//
//   - Input, off the city's event journal (the delta lanes' feed). A seat
//     counts when the guard saw it created, it is pool_managed, it was never
//     stamped with a trigger (gc.trigger_bead_id — sticky: a stamp cleared
//     mid-life still binds), it is neither a named nor a manual session, and
//     the guard saw it wake. It CHURNS when it closes with a work_started
//     cell naming no claim, and its close is neither failed-create nor
//     claims_errored (an operational write failure, not blind pull).
//   - Trip: poolSpawnChurnTrip churned closes within poolSpawnChurnWindow,
//     the first and the last at least poolSpawnChurnMinSpan apart, open an
//     episode and hold the template's blind demand for poolSpawnChurnHold.
//   - While the hold stands, allowance() maps the template to 0 unnamed
//     seats: the merge clamps its scale_check count to its named work beads,
//     so no blind request is planned. Bound requests, protected and in-flight
//     seats and every floor are untouched — they never came from the count.
//   - A claim by any counted seat clears the streak and ends the episode:
//     blind seats can be served now.
//   - When a hold runs out the allowance admits one blind seat again (the
//     probe). A probe seat that churns doubles the hold, up to
//     poolSpawnChurnHoldMax; churned closes of seats created before the
//     probe are stragglers and are ignored.
//   - Reload: a template whose scale_check, work_query,
//     scale_check_authoritative or provider changed, or that disappeared,
//     drops its state: the change may have been the fix.
//
// It holds no judgment: the thresholds are constants, the comparisons are
// counts and times, and it never touches a bead; only the controller's spawn
// count changes. Its state is in memory: a controller restart forgets it and
// re-learns after poolSpawnChurnTrip more seats churn. The planner also
// derives the same trip and hold from the closed session rows a caller hands
// it directly (poolSpawnChurnHoldsFromSessions) — the snapshot the
// production callers read drops closed rows, so there the journal-fed guard
// is the mechanism and the row derivation is inert.

import (
	"bytes"
	"context"
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
)

// Thresholds are consts, not config, for the claims-stall guard's reason:
// each is a knob that could hide the churn it exists to report. They mirror
// the claims-stall guard's values so the two sibling guards behave alike.
const (
	// poolSpawnChurnTrip is how many churned blind closes open an episode.
	poolSpawnChurnTrip = 3
	// poolSpawnChurnMinSpan is the least time between the first and the last
	// churned close of a streak that trips: one tick's burst of closes is
	// one event.
	poolSpawnChurnMinSpan = 2 * time.Minute
	// poolSpawnChurnWindow bounds a streak: a close older than this drops
	// out of it.
	poolSpawnChurnWindow = 30 * time.Minute
	// poolSpawnChurnHoldBase is the first hold; each failed probe doubles
	// it, up to poolSpawnChurnHoldMax.
	poolSpawnChurnHoldBase = 5 * time.Minute
	poolSpawnChurnHoldMax  = 30 * time.Minute
	// poolSpawnChurnForgetAfter drops a seat the journal has said nothing
	// about for this long.
	poolSpawnChurnForgetAfter = 6 * time.Hour
)

// poolSpawnChurnSeat is what the guard knows of one session bead.
type poolSpawnChurnSeat struct {
	template         string
	created          time.Time // the session bead's creation; zero when unknown
	awake            time.Time // creation_complete_at; zero until the seat woke
	bound            bool      // a gc.trigger_bead_id stamp was seen at any point
	pool             bool      // pool_managed was seen
	named            bool      // a named session
	manual           bool      // a manual session
	sawCreate        bool      // the guard observed the bead's creation
	claimed          bool
	claimAt          time.Time
	failedCreateFlag bool // state == failed-create
	claimsErroredFlg bool // the hook drained it claims_errored
	settled          bool // counted: as churn, or by its claim
	closed           bool
	seen             time.Time
}

// poolSpawnChurnTemplate is one template's streak and episode.
type poolSpawnChurnTemplate struct {
	signature string // the config fingerprint it learned at configure time
	streak    []time.Time
	episode   *poolSpawnChurnEpisode
}

// poolSpawnChurnEpisode is one churn hold, from its trip to what ends it.
type poolSpawnChurnEpisode struct {
	id      string
	since   time.Time // the first churned close of the streak that tripped it
	seats   int       // seats churned in all
	holds   int
	until   time.Time // blind demand withheld until this time
	probing bool      // a probe seat was admitted and has not reported
	probeAt time.Time // when; only seats created since are its outcome
}

// poolSpawnChurnGuard is one city's guard. Its methods are safe for
// concurrent use: observe runs on the journal feed's goroutine, allowance on
// the demand pass's and configure on the reload's.
type poolSpawnChurnGuard struct {
	mu         sync.Mutex
	now        func() time.Time
	record     func(events.Event)
	stderr     io.Writer
	signatures map[string]string // template → the config fingerprint it runs under
	templates  map[string]*poolSpawnChurnTemplate
	seats      map[string]*poolSpawnChurnSeat
}

func newPoolSpawnChurnGuard(record func(events.Event), stderr io.Writer) *poolSpawnChurnGuard {
	if stderr == nil {
		stderr = io.Discard
	}
	return &poolSpawnChurnGuard{
		now:        time.Now,
		record:     record,
		stderr:     stderr,
		signatures: map[string]string{},
		templates:  map[string]*poolSpawnChurnTemplate{},
		seats:      map[string]*poolSpawnChurnSeat{},
	}
}

// poolSpawnChurnSignature fingerprints the config a template's churn behavior
// depends on: its custom scale_check, work_query, authoritative flag and
// provider. A change to any of them may have been the fix, so the guard drops
// the template's state when it moves.
func poolSpawnChurnSignature(a *config.Agent) string {
	return strings.Join([]string{
		a.ScaleCheck,
		a.WorkQuery,
		fmt.Sprintf("%t", a.ScaleCheckAuthoritative),
		a.Provider,
	}, "\x00")
}

// configure adopts cfg's templates. A template gone from the config, or whose
// scale_check, work_query, scale_check_authoritative or provider changed,
// drops its streak and its episode; the change may have been the fix.
func (g *poolSpawnChurnGuard) configure(cfg *config.City) {
	if g == nil {
		return
	}
	signatures := map[string]string{}
	if cfg != nil {
		for i := range cfg.Agents {
			a := &cfg.Agents[i]
			signatures[a.QualifiedName()] = poolSpawnChurnSignature(a)
		}
	}
	g.mu.Lock()
	g.signatures = signatures
	for name, t := range g.templates {
		signature, ok := signatures[name]
		if ok && (t.signature == signature || t.signature == "") {
			if t.signature == "" {
				t.signature = signature
			}
			continue
		}
		if t.episode != nil {
			fmt.Fprintf(g.stderr, "pool spawn-churn guard: %s's hold ends with a config change\n", name) //nolint:errcheck // best-effort stderr
		}
		delete(g.templates, name)
	}
	g.mu.Unlock()
}

// templateFor is name's state, created on first use. Caller holds g.mu.
func (g *poolSpawnChurnGuard) templateFor(name string) *poolSpawnChurnTemplate {
	t := g.templates[name]
	if t == nil {
		t = &poolSpawnChurnTemplate{signature: g.signatures[name]}
		g.templates[name] = t
	}
	return t
}

// observe folds one journal event into the guard. Only session beads carry
// seats; every other notification is ignored.
func (g *poolSpawnChurnGuard) observe(evt events.Event) {
	if g == nil {
		return
	}
	switch evt.Type {
	case events.BeadCreated, events.BeadUpdated, events.BeadClosed:
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
	g.observeSeat(b, evt.Type == events.BeadCreated, evt.Type == events.BeadClosed, g.eventTime(evt))
}

func (g *poolSpawnChurnGuard) eventTime(evt events.Event) time.Time {
	if evt.Ts.IsZero() {
		return g.now()
	}
	return evt.Ts
}

// observeSeat folds one session bead's snapshot.
func (g *poolSpawnChurnGuard) observeSeat(b beads.Bead, created, closed bool, at time.Time) {
	template := strings.TrimSpace(b.Metadata["template"])
	if template == "" || b.ID == "" {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.sweep(at)
	seat := g.seatFor(b.ID, template)
	seat.seen = at
	if created {
		seat.sawCreate = true
	}
	if !b.CreatedAt.IsZero() {
		seat.created = b.CreatedAt
	}
	if strings.TrimSpace(b.Metadata["pool_managed"]) == "true" {
		seat.pool = true
	}
	if strings.TrimSpace(b.Metadata[session.NamedSessionMetadataKey]) == "true" {
		seat.named = true
	}
	if strings.TrimSpace(b.Metadata["manual_session"]) == "true" {
		seat.manual = true
	}
	if strings.TrimSpace(b.Metadata["state"]) == "failed-create" {
		seat.failedCreateFlag = true
	}
	if strings.TrimSpace(b.Metadata["sleep_reason"]) == hookClaimReasonClaimsErrored || strings.TrimSpace(b.Metadata["state_reason"]) == hookClaimReasonClaimsErrored {
		seat.claimsErroredFlg = true
	}
	if trigger := strings.TrimSpace(b.Metadata[beadmeta.TriggerBeadIDMetadataKey]); trigger != "" {
		seat.bound = true
	}
	if seat.awake.IsZero() {
		if woke, err := time.Parse(time.RFC3339, strings.TrimSpace(b.Metadata["creation_complete_at"])); err == nil {
			seat.awake = woke
		}
	}
	if claimedAt, claimed := claimStallNewestClaim(b.Metadata[session.WorkStartedKey]); claimed && (!seat.claimed || claimedAt.After(seat.claimAt)) {
		seat.claimAt = claimStallLater(seat.claimAt, claimedAt)
		g.claimedLocked(seat)
	}
	if closed && !seat.closed {
		seat.closed = true
		g.churnedLocked(seat, at)
	}
}

// seatFor is id's seat, created on first sight. Caller holds g.mu.
func (g *poolSpawnChurnGuard) seatFor(id, template string) *poolSpawnChurnSeat {
	seat := g.seats[id]
	if seat == nil {
		seat = &poolSpawnChurnSeat{template: template}
		g.seats[id] = seat
	}
	return seat
}

// countable reports whether seat's close could count as churn: the guard saw
// it created (a seat first seen through an update or close never counts, so
// after a restart only new seats do), it is pool_managed, it was never bound
// to a trigger bead, it is neither named nor manual, and it woke.
func (s *poolSpawnChurnSeat) countable() bool {
	return s.sawCreate && s.pool && !s.bound && !s.named && !s.manual && !s.awake.IsZero()
}

// churnedLocked counts seat's close, dated at, once, and trips its template's
// hold on the poolSpawnChurnTrip-th churned close with the required span.
// Caller holds g.mu.
func (g *poolSpawnChurnGuard) churnedLocked(seat *poolSpawnChurnSeat, at time.Time) {
	if seat.settled || seat.claimed || !seat.countable() {
		return
	}
	if seat.failedCreate() || seat.claimsErrored() {
		seat.settled = true
		return
	}
	seat.settled = true
	t := g.templateFor(seat.template)
	if t.episode != nil {
		t.episode.seats++
		if !t.episode.probes(seat) {
			return // a seat already running when the hold began: a straggler
		}
		g.probeFailedLocked(seat.template, t.episode, at)
		return
	}
	newest := at
	for _, c := range t.streak {
		newest = claimStallLater(newest, c)
	}
	streak := t.streak[:0]
	for _, c := range append(t.streak, at) {
		if newest.Sub(c) <= poolSpawnChurnWindow {
			streak = append(streak, c)
		}
	}
	t.streak = streak
	if len(t.streak) < poolSpawnChurnTrip {
		return
	}
	first, last := t.streak[0], t.streak[0]
	for _, c := range t.streak {
		first, last = claimStallEarlier(first, c), claimStallLater(last, c)
	}
	if last.Sub(first) < poolSpawnChurnMinSpan {
		return
	}
	n := len(t.streak)
	t.streak = nil
	g.tripLocked(seat.template, t, n, first, at)
}

// failedCreate and claimsErrored mark the closes that never count as churn:
// a seat that failed its create never pulled anything, and one drained
// claims_errored lost an operational race — the store refused a read, not the
// pull coming up empty.
func (s *poolSpawnChurnSeat) failedCreate() bool  { return s.failedCreateFlag }
func (s *poolSpawnChurnSeat) claimsErrored() bool { return s.claimsErroredFlg }

// tripLocked opens template's episode and holds its blind demand. Caller
// holds g.mu.
func (g *poolSpawnChurnGuard) tripLocked(template string, t *poolSpawnChurnTemplate, seats int, since, at time.Time) {
	t.episode = &poolSpawnChurnEpisode{
		id:    poolSpawnChurnEpisodeID(template, since),
		since: since,
		seats: seats,
		holds: 1,
		until: claimStallLater(at, g.now()).Add(poolSpawnChurnHold(1)),
	}
	fmt.Fprintf(g.stderr, "pool spawn-churn guard: %s: %d blind seats in a row woke, closed with no claim and drained since %s; withholding its blind demand until %s\n", //nolint:errcheck // best-effort stderr
		template, seats, since.UTC().Format(time.RFC3339), t.episode.until.UTC().Format(time.RFC3339))
}

// probeFailedLocked ends the probe as failed: the hold doubles, up to
// poolSpawnChurnHoldMax. Caller holds g.mu.
func (g *poolSpawnChurnGuard) probeFailedLocked(template string, ep *poolSpawnChurnEpisode, at time.Time) {
	ep.probing = false
	ep.holds++
	ep.until = claimStallLater(at, g.now()).Add(poolSpawnChurnHold(ep.holds))
	fmt.Fprintf(g.stderr, "pool spawn-churn guard: %s's probe seat churned too (%d seats in all); withholding its blind demand until %s\n", //nolint:errcheck // best-effort stderr
		template, ep.seats, ep.until.UTC().Format(time.RFC3339))
}

// probes reports whether seat is the probe ep admitted: created since the
// admission. A store may keep a creation time to the second only, so the
// admission's is compared to the second too.
func (ep *poolSpawnChurnEpisode) probes(seat *poolSpawnChurnSeat) bool {
	return ep.probing && !seat.created.IsZero() && !seat.created.Before(ep.probeAt.Truncate(time.Second))
}

// claimedLocked counts seat's claim: it clears the template's streak and ends
// any open episode — blind seats can be served now. Caller holds g.mu.
func (g *poolSpawnChurnGuard) claimedLocked(seat *poolSpawnChurnSeat) {
	seat.claimed, seat.settled = true, true
	if !seat.countableForClaim() {
		return
	}
	t := g.templateFor(seat.template)
	t.streak = nil
	if t.episode != nil {
		fmt.Fprintf(g.stderr, "pool spawn-churn guard: %s claims work again; its blind demand counts again\n", seat.template) //nolint:errcheck // best-effort stderr
		t.episode = nil
	}
}

// countableForClaim is countable without the awake requirement: a seat that
// claims before the guard saw its wake stamp still proves servability.
func (s *poolSpawnChurnSeat) countableForClaim() bool {
	return s.sawCreate && s.pool && !s.bound && !s.named && !s.manual
}

// poolSpawnChurnHold is how long the holds-th hold of an episode lasts.
func poolSpawnChurnHold(holds int) time.Duration {
	hold := poolSpawnChurnHoldBase
	for i := 1; i < holds && hold < poolSpawnChurnHoldMax; i++ {
		hold *= 2
	}
	return min(hold, poolSpawnChurnHoldMax)
}

// poolSpawnChurnEpisodeID names an episode stably: its template and when its
// first churned close happened.
func poolSpawnChurnEpisodeID(template string, since time.Time) string {
	return fmt.Sprintf("%s@%s", template, since.UTC().Format(time.RFC3339))
}

// sweep forgets seats the journal has gone quiet about. Caller holds g.mu.
func (g *poolSpawnChurnGuard) sweep(now time.Time) {
	for id, seat := range g.seats {
		if now.Sub(seat.seen) > poolSpawnChurnForgetAfter {
			delete(g.seats, id)
		}
	}
}

// allowance is how many unnamed scale_check seats each held template may
// still spawn: 0 while its hold stands, 1 (the probe) once the hold runs
// out. A template with no episode is absent, which means unlimited; a nil
// guard has no opinion. The admission of the probe is recorded here, on the
// demand pass, so the first pass after the hold ends is the one that spawns
// it.
func (g *poolSpawnChurnGuard) allowance() map[string]int {
	if g == nil {
		return nil
	}
	now := g.now()
	g.mu.Lock()
	defer g.mu.Unlock()
	g.sweep(now)
	out := make(map[string]int, len(g.templates))
	for name, t := range g.templates {
		ep := t.episode
		if ep == nil {
			continue
		}
		if now.Before(ep.until) && !ep.probing {
			out[name] = 0
			continue
		}
		if !ep.probing {
			ep.probing = true
			ep.probeAt = now
			fmt.Fprintf(g.stderr, "pool spawn-churn guard: admitting one probe seat for %s\n", name) //nolint:errcheck // best-effort stderr
		}
		out[name] = 1
	}
	return out
}

// poolSpawnChurnGuards maps a normalized city path to its controller's
// guard, as claimStallGuards does: the demand passes reach it by city path,
// and a process that runs no controller registers none.
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

// clampBlindScaleCount caps a scale_check count at the demand its named work
// beads plus allow unnamed seats justify.
func clampBlindScaleCount(count, named, allow int) int {
	if count > named+allow {
		return named + allow
	}
	return count
}

// clampBlindSpawnDemand caps each listed template's merged scale_check count
// at its named work beads plus its allowance of unnamed seats, and its
// demand's count the same way. Control-dispatcher templates never clamp:
// their demand is the controller's own machinery, never a blind pull. It
// edits only the merged maps, never a caller's.
func clampBlindSpawnDemand(cfg *config.City, allowance map[string]int, counts map[string]int, demand map[string]scaleCheckDemand) (map[string]int, map[string]scaleCheckDemand) {
	for template, allow := range allowance {
		if agent := findAgentByTemplate(cfg, template); agent != nil && config.IsDeterministicControlDispatcher(agent) {
			continue
		}
		named := len(demand[template].WorkBeadIDs)
		if counts != nil {
			if c, ok := counts[template]; ok {
				counts[template] = clampBlindScaleCount(c, named, allow)
			}
		}
		if demand != nil {
			if d, ok := demand[template]; ok && d.Count > named+allow {
				d.Count = named + allow
				demand[template] = d
			}
		}
	}
	return counts, demand
}

// poolSpawnChurnHoldsFromSessions derives per-template churn holds from the
// session rows a planner caller hands in: a template whose pool-managed seats
// closed churned — drained, never bound to a trigger bead, no claim in their
// work_started cell, not failed-create, not claims_errored, and showing
// activity after creation — poolSpawnChurnTrip times within
// poolSpawnChurnWindow with the required span withholds its blind spawns
// until poolSpawnChurnHold after the newest close. The production callers
// read snapshots without closed rows, where the journal-fed guard is the
// mechanism; this is the same rule for a caller that has the rows.
func poolSpawnChurnHoldsFromSessions(cfg *config.City, infos []session.Info, now time.Time) map[string]time.Time {
	closes := make(map[string][]time.Time)
	for _, sb := range infos {
		if !poolSpawnChurnRowChurned(sb) {
			continue
		}
		template := strings.TrimSpace(normalizedSessionTemplateInfo(sb, cfg))
		if template == "" {
			continue
		}
		at := sb.LastActive
		if at.IsZero() || at.Before(sb.CreatedAt) {
			at = sb.CreatedAt
		}
		closes[template] = append(closes[template], at)
	}
	holds := make(map[string]time.Time)
	for template, times := range closes {
		sort.Slice(times, func(i, j int) bool { return times[i].Before(times[j]) })
		newest := times[len(times)-1]
		streak := times[:0]
		for _, c := range times {
			if newest.Sub(c) <= poolSpawnChurnWindow {
				streak = append(streak, c)
			}
		}
		if len(streak) < poolSpawnChurnTrip {
			continue
		}
		if streak[len(streak)-1].Sub(streak[0]) < poolSpawnChurnMinSpan {
			continue
		}
		if end := streak[len(streak)-1].Add(poolSpawnChurnHold(1)); now.Before(end) {
			holds[template] = end
		}
	}
	return holds
}

// poolSpawnChurnRowChurned is the row-level form of the guard's counted
// churned close, for callers that hold session rows instead of the journal.
func poolSpawnChurnRowChurned(sb session.Info) bool {
	if !sb.Closed || !sb.PoolManaged {
		return false
	}
	if isNamedSessionInfo(sb) || sb.ManualSession {
		return false
	}
	if strings.TrimSpace(sb.TriggerBeadID) != "" {
		return false
	}
	if _, claimed := claimStallNewestClaim(sb.WorkStartedRaw); claimed {
		return false
	}
	if sb.LastActive.IsZero() || !sb.LastActive.After(sb.CreatedAt) {
		return false // never woke
	}
	if string(sb.State) == "failed-create" {
		return false
	}
	if sb.SleepReason == hookClaimReasonClaimsErrored || sb.StateReason == hookClaimReasonClaimsErrored {
		return false
	}
	return true
}
