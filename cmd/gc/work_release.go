package main

// The one release verb (ARCH-RESTRUCTURE-2 R9, the REL half; pass-1 R4).
//
// Seven paths give a seat's claims back: the killed-seat release (B1), the
// drain-ack release (D5), the close cascade, the stranded repair, the
// confirmed-orphan release, the dead-assignee sweep and the retired-session
// sweep. Each decided for
// itself which claims it released and into which lane, and four of them
// released claims no lane serves: an unserved route, or an expanded workflow
// root with no route of its own (NEW-5, mc-zndi7.87). Released, such a claim
// is open, unassigned, demanded by nothing and claimable by no one.
//
// Here the paths differ only by a row of releasePolicies, and every claim
// takes the same guard: its route must resolve to a configured agent, and a
// workflow root's run_target counts only while the root is fallback-eligible
// (not yet expanded, #5900). A claim that fails the guard stays assigned
// (owner ruling O5): the close that would orphan it refuses with
// unserved-claims and the seat raises session.unserved_claims.

import (
	"errors"
	"fmt"
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
	releaseOrphan
	releaseDeadAssignee
	releaseRetired
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
	// gc.routed_to (carriedPoolRoute); any configured agent serves it.
	laneCarried
)

// releasePolicy is what one path releases and how.
type releasePolicy struct {
	statuses     []string // the claim statuses the path gives back
	startedKeeps bool     // a started claim keeps the seat: nothing is released
	lane         laneRule
	seatRoute    bool // the seat's route is the fallback, stamped on a routeless claim
	pool         bool // the orphan write: live re-check, detached probe, pool release
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
//     holds nothing, and open claims are its continuation preassignments;
//   - the cascade, the stranded repair, both orphan releases and the
//     retired-session sweep (`gc session close`, a removed named session)
//     give back started work too: the seat is gone.
var releasePolicies = map[releaseWhy]releasePolicy{
	releaseKilledSeat:     {statuses: openOrStarted, startedKeeps: true, lane: laneDirect, seatRoute: true},
	releaseDrainAck:       {statuses: startedOnly, lane: laneCarried},
	releaseCloseCascade:   {statuses: openOrStarted, lane: laneCarried, seatRoute: true},
	releaseStrandedRepair: {statuses: openOrStarted, lane: laneCarried, seatRoute: true},
	releaseOrphan:         {statuses: openOrStarted, lane: laneDirect, pool: true},
	releaseDeadAssignee:   {statuses: openOrStarted, lane: laneDirect, pool: true},
	releaseRetired:        {statuses: openOrStarted, lane: laneCarried, seatRoute: true},
}

// Why a claim was kept or failed.
const (
	keptStarted        = "started"
	keptUnservedRoute  = "unserved-route"
	keptMoved          = "moved"
	keptDetachedAlive  = "detached-alive"
	failedBudget       = "budget"
	refusedUnservedWhy = "unserved-claims"
)

// releaseSeat is what a release reads off the seat whose claims it gives back.
type releaseSeat struct {
	id, name, template string
	fallback           string          // the seat's own route (template, else agent_name)
	started            map[string]bool // claims the seat names as executing
}

func releaseSeatOfBead(b beads.Bead) releaseSeat {
	s := releaseSeat{id: b.ID, name: b.Metadata["session_name"], template: b.Metadata["template"], fallback: retiredSessionFallbackRoute(b)}
	for _, key := range []string{sessionpkg.CurrentBeadIDKey, beadmeta.CurrentClaimBeadIDMetadataKey} {
		if id := strings.TrimSpace(b.Metadata[key]); id != "" {
			if s.started == nil {
				s.started = make(map[string]bool, 2)
			}
			s.started[id] = true
		}
	}
	return s
}

func releaseSeatOfInfo(info sessionpkg.Info) releaseSeat {
	return releaseSeat{id: info.ID, name: info.SessionNameMetadata, template: info.Template, fallback: retiredSessionFallbackRouteInfo(info)}
}

// ReleaseOpts adjusts one release.
type ReleaseOpts struct {
	DryRun   bool      // decide only: Released lists what would be released
	Late     bool      // add the close-time re-read of the seat's legs (the cascade)
	Deadline time.Time // zero: none; a write past it fails with failedBudget
}

// ReleaseOutcome is what a release did with each claim.
type ReleaseOutcome struct {
	Released, Kept, Failed []seatWorkHit
	Why                    map[string]string // by bead ID, for Kept and Failed
}

// Unserved are the IDs of the claims kept because no lane serves them.
func (o ReleaseOutcome) Unserved() []string {
	var ids []string
	for _, k := range o.Kept {
		if o.Why[k.bead.ID] == keptUnservedRoute {
			ids = append(ids, k.bead.ID)
		}
	}
	slices.Sort(ids)
	return ids
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
	if err != nil && p.startedKeeps {
		return ReleaseOutcome{}, err
	}
	return releaseListed(claims, why, seat, sw.Legs().cfg, opts), err
}

// releaseListed is ReleaseClaims' body for claims a caller already listed
// (the orphan releases list them from the tick's census).
func releaseListed(claims []seatWorkHit, why releaseWhy, seat releaseSeat, cfg *config.City, opts ReleaseOpts) ReleaseOutcome {
	p := releasePolicies[why]
	out := ReleaseOutcome{Why: map[string]string{}}
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
		if _, served := releaseRoute(cfg, c.bead, fallback, p.lane); !served {
			keep(c, keptUnservedRoute)
			continue
		}
		if opts.DryRun {
			out.Released = append(out.Released, c)
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
	return route, agent != nil
}

// unservedClaimAlerts raises session.unserved_claims once per refusal
// episode: again only when the seat's set of unserved claims changes, and
// forgotten when the seat closes. The controller owns one; it outlives ticks.
type unservedClaimAlerts struct {
	rec  events.Recorder
	mu   sync.Mutex
	last map[string]string // by session ID: the alerted bead set
}

// refuse records that a close of seat (for reason) refused over the unserved
// claims ids, and alerts unless this episode already did. A nil receiver
// (tests, one-shot commands) alerts nothing.
func (a *unservedClaimAlerts) refuse(seat releaseSeat, reason string, ids []string) {
	if a == nil || a.rec == nil || len(ids) == 0 {
		return
	}
	set := strings.Join(ids, ",")
	a.mu.Lock()
	if a.last == nil {
		a.last = make(map[string]string)
	}
	repeat := a.last[seat.id] == set
	a.last[seat.id] = set
	a.mu.Unlock()
	if repeat {
		return
	}
	a.rec.Record(events.Event{
		Type:      events.SessionUnservedClaims,
		Actor:     "gc",
		Subject:   seat.id,
		Message:   fmt.Sprintf("close of %s refused (%s): %d claim(s) no lane serves stay assigned: %s", seat.id, refusedUnservedWhy, len(ids), set),
		SessionID: seat.id,
		Payload:   api.SessionUnservedClaimsPayloadJSON(seat.id, seat.name, seat.template, reason, ids),
	})
}

// closed ends seat's episode.
func (a *unservedClaimAlerts) closed(id string) {
	if a == nil {
		return
	}
	a.mu.Lock()
	delete(a.last, id)
	a.mu.Unlock()
}
