package main

// The one release verb (ARCH-RESTRUCTURE-2 R9, the REL half; pass-1 R4).
//
// Eight paths give a seat's claims back: the killed-seat release (B1), the
// drain-ack release (D5), the close cascade, the stranded repair, the
// confirmed-orphan release, the dead-assignee sweep, the retired-session
// sweep and the work ladder's release of an unstarted claim. Each decided for
// itself which claims it released and into which lane, and four of them
// released claims no lane serves: an unserved route, or an expanded workflow
// root with no route of its own (NEW-5, mc-zndi7.87). Released, such a claim
// is open, unassigned, demanded by nothing and claimable by no one.
//
// Here the paths differ only by a row of releasePolicies, and every claim
// takes the same guard: its route must resolve to a configured agent, and a
// workflow root's run_target counts only while the root is fallback-eligible
// (not yet expanded, #5900). A claim that fails the guard stays assigned
// (owner ruling O5, revised 2026-10-10): the operation still completes, and
// once the claim is held by a closed row it is stranded work, which
// session.stranded_claims reports (strandedClaimAlerts), again on D6's
// cadence until it is rerouted or released.

import (
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// releaseWhy names the path a release runs for: its releasePolicies row.
type releaseWhy uint8

const (
	releaseKilledSeat releaseWhy = iota
	releaseDrainAck
	releaseCloseCascade
	releaseStrandedRepair
	releaseOrphan // the confirmed orphan and the dead-assignee sweep: one policy
	releaseRetired
	releaseLadder // the work ladder's release of an unstarted claim (PHASE1-2 §6.5)
)

// laneRule says when a released claim's route counts as served.
type laneRule uint8

const (
	// laneDirect: demanded and claimable as released — gc.routed_to, or a
	// fallback-eligible workflow root's run_target (else the fallback) — by a
	// configured agent that runs generic sessions (CONTRACT C3 rule 2).
	laneDirect laneRule = iota
	// laneCarried: laneDirect's routes, plus a plain bead's run_target or the
	// fallback ReleaseWorkBead stamps on it, which route recovery carries into
	// gc.routed_to (carriedPoolRoute); a configured agent serves it if it runs
	// generic sessions or a named session backs it.
	laneCarried
)

// releasePolicy is what one path releases and how.
type releasePolicy struct {
	statuses     []string // the claim statuses the path gives back
	startedKeeps bool     // a started claim keeps the seat: nothing is released
	// startedNamedKeeps keeps each claim the seat's own started records name
	// (releaseSeat.keepsStarted); the rest are released.
	startedNamedKeeps bool
	lane              laneRule
	seatRoute         bool // the seat's route is the fallback, stamped on a routeless claim
	pool              bool // the orphan write: live re-check, detached probe, pool release
}

var (
	openOrStarted = []string{"open", "in_progress"}
	startedOnly   = []string{"in_progress"}
)

// releasePolicies is every release path's started-work policy, in one place
// (TestReleasePoliciesTable pins it):
//   - a killed seat keeps everything when anything is started (owner ruling
//     B1): only the seat that started work may resume it;
//   - drain-ack gives back only in_progress claims (D5): the agent said it
//     holds nothing, and open claims are its continuation preassignments; a
//     routeless claim takes the seat's route, as on the paths below, since
//     kept it would leave the acked seat asleep holding it (mc-sca8r). A
//     claim the seat's work_started cell or execution_claim_started stamp
//     names is kept (PHASE1-2 §6.5), unless it is on a dispatch hold: the ack
//     is no proof it never ran, and giving it back lost started work
//     (mc-ilr3b);
//   - the cascade, the stranded repair, both orphan releases and the
//     retired-session sweep (`gc session close`, a removed named session)
//     give back started work too: the seat is gone;
//   - the work ladder gives back one in_progress claim of a live seat, which
//     its CAS proved unstarted (PHASE1-2 §6.5); a routeless claim takes the
//     seat's route, as drain-ack's.
var releasePolicies = map[releaseWhy]releasePolicy{
	releaseKilledSeat:     {statuses: openOrStarted, startedKeeps: true, lane: laneDirect, seatRoute: true},
	releaseDrainAck:       {statuses: startedOnly, startedNamedKeeps: true, lane: laneCarried, seatRoute: true},
	releaseCloseCascade:   {statuses: openOrStarted, lane: laneCarried, seatRoute: true},
	releaseStrandedRepair: {statuses: openOrStarted, lane: laneCarried, seatRoute: true},
	releaseOrphan:         {statuses: openOrStarted, lane: laneDirect, pool: true},
	releaseRetired:        {statuses: openOrStarted, lane: laneCarried, seatRoute: true},
	releaseLadder:         {statuses: startedOnly, lane: laneCarried, seatRoute: true},
}

// Why a claim was kept or failed.
const (
	keptStarted       = "started"
	keptUnservedRoute = "unserved-route"
	keptMoved         = "moved"
	keptDetachedAlive = "detached-alive"
	failedBudget      = "budget"
)

// errReleaseNoConfig defers a release of the claims a seat holds: without the
// city config no route can be judged served, so nothing is released and
// nothing is kept as unserved.
var errReleaseNoConfig = errors.New("release: no city config to judge routes by")

// releaseSeat is what a release reads off the seat whose claims it gives back.
type releaseSeat struct {
	id, name, template string
	fallback           string // the seat's own route (template, else agent_name)
	// started is every claim the seat names as executing: its current bead
	// and claim stamps and its started records. The killed-seat release (B1)
	// keeps the seat's work when any of them is held.
	started map[string]bool
	// recorded is the claims the seat's started records name: its
	// work_started cell (PHASE1-2 §6.4) and its execution_claim_started stamp,
	// whichever incarnation wrote it. Narrower than started: the current
	// claim stamps name a claim the hook handed out, not one a turn began,
	// and the drain-ack keep (D5) follows the design's two sources only.
	recorded map[string]bool
	// recordedUnread: the work_started cell could not be read, so it may name
	// any claim (PHASE1-2 §6.1: unreadable reads as every claim started).
	recordedUnread bool
}

func releaseSeatOfBead(b beads.Bead) releaseSeat {
	s := releaseSeat{id: b.ID, name: b.Metadata["session_name"], template: b.Metadata["template"], fallback: retiredSessionFallbackRoute(b)}
	s.nameStarted(b.Metadata[sessionpkg.CurrentBeadIDKey], b.Metadata[beadmeta.CurrentClaimBeadIDMetadataKey])
	s.nameRecorded(startedClaimBead(b.Metadata[executionClaimStartedKey]))
	s.nameWorkStarted(b.Metadata[sessionpkg.WorkStartedKey])
	return s
}

func releaseSeatOfInfo(info sessionpkg.Info) releaseSeat {
	s := releaseSeat{id: info.ID, name: info.SessionNameMetadata, template: info.Template, fallback: retiredSessionFallbackRouteInfo(info)}
	s.nameStarted(info.CurrentlyProcessingBeadID, info.CurrentClaimBeadID)
	s.nameRecorded(startedClaimBead(info.ExecutionClaimStarted))
	s.nameWorkStarted(info.WorkStartedRaw)
	return s
}

// nameStarted adds ids to the claims the seat names as executing.
func (s *releaseSeat) nameStarted(ids ...string) {
	for _, id := range ids {
		if id = strings.TrimSpace(id); id != "" {
			if s.started == nil {
				s.started = make(map[string]bool, len(ids))
			}
			s.started[id] = true
		}
	}
}

// nameRecorded adds ids to the claims the seat's started records name, and
// to those it names as executing.
func (s *releaseSeat) nameRecorded(ids ...string) {
	s.nameStarted(ids...)
	for _, id := range ids {
		if id = strings.TrimSpace(id); id != "" {
			if s.recorded == nil {
				s.recorded = make(map[string]bool, len(ids))
			}
			s.recorded[id] = true
		}
	}
}

// nameWorkStarted adds the claims the seat's work_started cell names: the
// claims its own hook handed to a turn (PHASE1-2 §6.4).
func (s *releaseSeat) nameWorkStarted(raw string) {
	cell, err := sessionpkg.DecodeWorkStarted(raw)
	if err != nil {
		s.recordedUnread = true
		return
	}
	for _, e := range cell.Claims {
		s.nameRecorded(e.Bead)
	}
}

// keepsStarted reports whether the drain-ack release keeps b as the seat's
// started work: its started records name it, or may, and it is on no
// dispatch hold (hold:mayor, hold:external), whose claim goes back as it
// always has (gas-kg6).
func (s releaseSeat) keepsStarted(b beads.Bead) bool {
	return (s.recordedUnread || s.recorded[strings.TrimSpace(b.ID)]) && !beadmeta.HasDispatchHold(b.Labels)
}

// startedClaimBead is the bead an execution_claim_started stamp names
// ("<instance_token>|<bead>", startedValue), whichever incarnation wrote it.
func startedClaimBead(stamp string) string {
	_, bead, ok := strings.Cut(strings.TrimSpace(stamp), "|")
	if !ok {
		return ""
	}
	return strings.TrimSpace(bead)
}

// ReleaseOpts adjusts one release.
type ReleaseOpts struct {
	Late     bool      // add the close-time re-read of the seat's legs (the cascade)
	Deadline time.Time // zero: none; a write past it fails with failedBudget
	// RefuseStarted refuses the whole release when the claims read include
	// one the seat keeps as started (releaseSeat.keepsStarted): nothing is
	// released, and the outcome is Refused with those claims Kept. A seat's
	// own drain-ack uses it: it must finish such work before it acks.
	RefuseStarted bool
}

// ReleaseOutcome is what a release did with each claim.
type ReleaseOutcome struct {
	Released, Kept, Failed []seatWorkHit
	Why                    map[string]string // by bead ID, for Kept and Failed
	Route                  map[string]string // by bead ID, for a claim kept unserved: the route it would take
	Refused                bool              // RefuseStarted refused the release: Kept are the started claims
}

// strandedClaim is one claim kept because no lane serves it.
type strandedClaim struct{ id, route, reason string }

// Unserved are the claims kept because no lane serves them, by ID.
func (o ReleaseOutcome) Unserved() []strandedClaim {
	var out []strandedClaim
	for _, k := range o.Kept {
		if id := k.bead.ID; o.Why[id] == keptUnservedRoute {
			out = append(out, strandedClaim{id: id, route: o.Route[id], reason: keptUnservedRoute})
		}
	}
	slices.SortFunc(out, func(a, b strandedClaim) int { return strings.Compare(a.id, b.id) })
	return out
}

// ReleaseClaims gives back the claims scope holds on sw's legs, as why's
// policy says. A read that fails releases nothing on a startedKeeps path,
// whose gate needs the whole seat; elsewhere what was read is released.
func ReleaseClaims(sw *SeatWork, scope ReleaseScope, seat releaseSeat, why releaseWhy, opts ReleaseOpts) (ReleaseOutcome, error) {
	p := releasePolicies[why]
	claims, err := sw.snapshot(seatWorkQuery{scope: scope, statuses: p.statuses}, false)
	if opts.Late {
		late, lateErr := lateSeatWork(sw.Legs(), scope, p.statuses, nil)
		claims, err = appendUniqueSeatWork(claims, late), errors.Join(err, lateErr)
	}
	// The refusal needs no config: it releases nothing, so a seat whose
	// config did not load is still refused, not acked past its started work.
	if opts.RefuseStarted {
		out := ReleaseOutcome{Why: map[string]string{}, Route: map[string]string{}}
		for _, c := range claims {
			if seat.keepsStarted(c.bead) {
				out.Kept, out.Why[c.bead.ID], out.Refused = append(out.Kept, c), keptStarted, true
			}
		}
		if out.Refused {
			return out, err
		}
	}
	if sw.Legs().cfg == nil && len(claims) > 0 {
		return ReleaseOutcome{}, errors.Join(err, errReleaseNoConfig)
	}
	if err != nil && p.startedKeeps {
		return ReleaseOutcome{}, err
	}
	return releaseListed(claims, why, seat, sw.Legs().cfg, opts), err
}

// releaseListed is ReleaseClaims' body for claims a caller already listed
// (the orphan releases list them from the tick's census).
func releaseListed(claims []seatWorkHit, why releaseWhy, seat releaseSeat, cfg *config.City, opts ReleaseOpts) ReleaseOutcome {
	p := releasePolicies[why]
	out := ReleaseOutcome{Why: map[string]string{}, Route: map[string]string{}}
	keep := func(c seatWorkHit, why string) { out.Kept = append(out.Kept, c); out.Why[c.bead.ID] = why }
	var mine []seatWorkHit
	for _, c := range claims {
		if slices.Contains(p.statuses, c.bead.Status) {
			mine = append(mine, c)
		}
	}
	if p.startedKeeps && slices.ContainsFunc(mine, func(c seatWorkHit) bool { return c.bead.Status == "in_progress" || seat.started[c.bead.ID] }) {
		for _, c := range mine {
			keep(c, keptStarted)
		}
		return out
	}
	fallback := ""
	if p.seatRoute {
		fallback = seat.fallback
	}
	for _, c := range mine {
		if p.startedNamedKeeps && seat.keepsStarted(c.bead) {
			keep(c, keptStarted)
			continue
		}
		if route, served := releaseRoute(cfg, c.bead, fallback, p.lane); !served {
			keep(c, keptUnservedRoute)
			out.Route[c.bead.ID] = route
			continue
		}
		if !opts.Deadline.IsZero() && time.Now().After(opts.Deadline) {
			out.Failed = append(out.Failed, c)
			out.Why[c.bead.ID] = failedBudget
			continue
		}
		if p.pool {
			assignee := strings.TrimSpace(c.bead.Assignee)
			if !liveWorkAssignmentStillReleasable(c.store, c.bead.ID, c.bead.Status, assignee) {
				keep(c, keptMoved)
				continue
			}
			allowed, clearDetached := detachedProbeAllowsOrphanRelease(c.bead)
			if !allowed {
				keep(c, keptDetachedAlive)
				continue
			}
			if !releaseOrphanedPoolAssignment(c.store, c.bead, clearDetached) {
				keep(c, keptMoved)
				continue
			}
		} else if err := workAssignmentForStore(beads.WorkStore{Store: c.store}).ReleaseWorkBead(c.bead, fallback); err != nil {
			out.Failed = append(out.Failed, c)
			out.Why[c.bead.ID] = err.Error()
			continue
		}
		out.Released = append(out.Released, c)
	}
	return out
}

// releaseRoute is the lane a released claim is demanded and claimed through,
// and whether a configured agent serves it under lane.
func releaseRoute(cfg *config.City, b beads.Bead, fallback string, lane laneRule) (string, bool) {
	route := strings.TrimSpace(b.Metadata[beadmeta.RoutedToMetadataKey])
	if route == "" {
		runTarget := strings.TrimSpace(b.Metadata[beadmeta.RunTargetMetadataKey])
		switch kind := strings.TrimSpace(b.Metadata[beadmeta.KindMetadataKey]); {
		case kind == beadmeta.KindWorkflow && workflowRunTargetFallbackEligible(b):
			route = firstNonEmpty(runTarget, fallback)
		case kind == "" && lane == laneCarried:
			route = firstNonEmpty(runTarget, fallback)
		}
	}
	if route == "" {
		return "", false
	}
	agent := findAgentByTemplate(cfg, route)
	if lane == laneDirect {
		return route, agent.SupportsGenericEphemeralSessions()
	}
	return route, agent.SupportsGenericEphemeralSessions() || namedSessionBacks(cfg, agent)
}

// namedSessionBacks reports a configured named session on agent's template:
// a max_active_sessions = 0 agent still serves through it.
func namedSessionBacks(cfg *config.City, agent *config.Agent) bool {
	if agent == nil {
		return false
	}
	for _, ns := range cfg.NamedSessions {
		if findAgentByTemplate(cfg, ns.Template) == agent {
			return true
		}
	}
	return false
}

// onlyStrandedWork reports that info holds claims, and that every one would
// stay on its closed row under the cascade's rule (no lane serves it): a
// close may land over them, keep them and alert (O5, revised). sw's rows are
// read first, so a served claim among them answers false with no live read;
// only then is the seat read live, which must agree. A failed read, or no
// claim at all, reports false.
func onlyStrandedWork(sw *SeatWork, info sessionpkg.Info) bool {
	scope := releaseScope(info, sw.Legs().cfg)
	q := seatWorkQuery{scope: scope, statuses: seatWorkStatuses}
	seat := releaseSeatOfInfo(info)
	stranded := func(hits []seatWorkHit, err error) bool {
		if err != nil || len(hits) == 0 {
			return false
		}
		return !slices.ContainsFunc(hits, func(h seatWorkHit) bool {
			_, served := releaseRoute(sw.Legs().cfg, h.bead, seat.fallback, laneCarried)
			return served
		})
	}
	return stranded(sw.snapshot(q, false)) && stranded(sw.fresh(scope).match(q, false))
}

// strand marks each claim out kept because no lane serves it as stranded
// on seat's closed row, as assigned when it was kept (stampStranded), which
// the detector reads, and alerts on them for the close kind; a close that
// kept none ends the seat's episode. A stamp that fails is logged: the claim
// still stays, and the close's own alert names it.
func strand(alerts *strandedClaimAlerts, seat releaseSeat, kind string, out ReleaseOutcome, now time.Time, stderr io.Writer) {
	claims := out.Unserved() // none ends the seat's episode
	left := make(map[string]seatWorkHit, len(out.Kept)+len(out.Failed))
	for _, k := range out.Kept {
		left[k.bead.ID] = k
	}
	// A claim the release's budget ran out on stays too: stamped and named,
	// never left on the closed row silent (O5, revised).
	for _, f := range out.Failed {
		if id := f.bead.ID; out.Why[id] == failedBudget {
			left[id] = f
			claims = append(claims, strandedClaim{id: id, reason: failedBudget})
		}
	}
	slices.SortFunc(claims, func(a, b strandedClaim) int { return strings.Compare(a.id, b.id) })
	for _, c := range claims {
		k := left[c.id]
		if err := stampStranded(k.store, c.id, seat.id, strings.TrimSpace(k.bead.Assignee)); err != nil {
			fmt.Fprintf(stderr, "session beads: marking claim %s stranded on closed session %s: %v\n", c.id, seat.id, err) //nolint:errcheck
		}
	}
	alerts.observe(seat, kind, claims, now)
}

// strandedClaimAlerts raises session.stranded_claims for claims a seat
// keeps because no lane serves them, once its row is closed (or retired):
// stranded work, which no sweep releases. It alerts when the operation that
// closed the seat completes (observe), and the detector (detectStrandedClaims)
// observes again on D6's cadence while the claims stay on the closed row: an
// episode alerts when its bead set changes, and again every
// strandedRealertEvery while it lasts; a seat with nothing stranded ends it.
// The controller owns one; it outlives ticks, not the process.
type strandedClaimAlerts struct {
	rec      events.Recorder
	mu       sync.Mutex
	episodes map[string]strandedEpisode // by session ID
	scanned  time.Time                  // the detector's last scan
	seats    map[string]strandedSeat    // closed seats the detector has read, by ID
}

// strandedSeat is a closed seat as the detector alerts on it.
type strandedSeat struct {
	seat releaseSeat
	kind string
}

type strandedEpisode struct {
	set string
	at  time.Time
}

// strandedRealertEvery is D6's repeat cadence (CONTRACT D6).
const strandedRealertEvery = 15 * time.Minute

// observe records that seat, closed by kind, holds claims; it alerts on a
// new bead set or a due repeat, and ends the episode when claims is empty. A
// nil receiver (tests, one-shot commands without a recorder) alerts nothing.
func (a *strandedClaimAlerts) observe(seat releaseSeat, kind string, claims []strandedClaim, now time.Time) {
	if a == nil {
		return
	}
	ids := make([]string, len(claims))
	for i, c := range claims {
		ids[i] = c.id
	}
	set := strings.Join(ids, ",")
	a.mu.Lock()
	last, ok := a.episodes[seat.id]
	due := len(claims) > 0 && (!ok || last.set != set || now.Sub(last.at) >= strandedRealertEvery)
	switch {
	case len(claims) == 0:
		delete(a.episodes, seat.id)
	case due:
		if a.episodes == nil {
			a.episodes = make(map[string]strandedEpisode)
		}
		a.episodes[seat.id] = strandedEpisode{set: set, at: now}
	}
	a.mu.Unlock()
	if !due || a.rec == nil {
		return
	}
	payload := make([]api.StrandedClaim, len(claims))
	for i, c := range claims {
		payload[i] = api.StrandedClaim{BeadID: c.id, Route: c.route, Reason: c.reason}
	}
	a.rec.Record(events.Event{
		Type:      events.SessionStrandedClaims,
		Actor:     "gc",
		Subject:   seat.id,
		Message:   fmt.Sprintf("%s session %s left %d claim(s) no lane serves assigned to it: %s", kind, seat.id, len(claims), set),
		SessionID: seat.id,
		Payload:   api.SessionStrandedClaimsPayloadJSON(seat.id, seat.name, seat.template, kind, payload),
	})
}

// scanDue reports whether the detector's cadence has come round, and starts
// the next interval when it has. The first call starts the clock and scans
// nothing: a fresh controller first scans one interval in, so a boot or a
// short-lived pass reads nothing for it (#3288's boot tick reads no wisp tier).
func (a *strandedClaimAlerts) scanDue(now time.Time) bool {
	if a == nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	switch {
	case a.scanned.IsZero():
		a.scanned = now
		return false
	case now.Sub(a.scanned) < strandedRealertEvery:
		return false
	}
	a.scanned = now
	return true
}

// sweep ends every episode the detector's scan no longer found.
func (a *strandedClaimAlerts) sweep(found map[string][]strandedClaim) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for id := range a.episodes {
		if _, ok := found[id]; !ok {
			delete(a.episodes, id)
		}
	}
}

// seat is closed seat id as the detector alerts on it, read from sessions
// on first sight and remembered: a closed row's identity does not change. An
// unreadable row alerts under its ID alone, and is read again next scan.
func (a *strandedClaimAlerts) seat(sessions beads.Store, id string) strandedSeat {
	a.mu.Lock()
	known, ok := a.seats[id]
	a.mu.Unlock()
	if ok {
		return known
	}
	got := strandedSeat{seat: releaseSeat{id: id}}
	if sessions == nil {
		return got
	}
	b, err := sessions.Get(id)
	if err != nil {
		return got
	}
	got = strandedSeat{seat: releaseSeatOfBead(b), kind: strings.TrimSpace(b.Metadata["state"])}
	a.mu.Lock()
	if a.seats == nil {
		a.seats = make(map[string]strandedSeat)
	}
	a.seats[id] = got
	a.mu.Unlock()
	return got
}
