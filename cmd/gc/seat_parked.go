package main

// A pool seat parked on pre-assigned blocked work (owner ruling 2026-10-18,
// ga-ylpbsy.16): "preassigned but blocked is a valid state. once it is
// unblocked that same session should be woken with all the context."
//
// A claim pre-assigns the open siblings of its continuation group to the
// claiming seat (preassignHookContinuationGroup). The seat finishes its step,
// finds the pin blocked and drain-acks, and drain-ack keeps the pin on purpose
// (releasePolicies: "open claims are its continuation preassignments"). The
// finalize then sleeps the seat holding it (CompleteDrainPatch(idle)).
//
// Such a seat is parked, not stranded: its runtime stopped by its own choice,
// and its work cannot run yet. The pool-slot close (A21, legacy's pool-free
// branch) holds it without the stranded marker, the event or the repair, and
// the assigned-work wake restarts the same seat, resuming its conversation,
// once the pin is ready. The pool planner keeps the seat selected but counts
// it against no cap until then (parkedSeats, the planner's Parked request).
//
// Parked is derived each pass from persisted facts, never stored, so it
// survives a controller restart unchanged and needs no marker of its own.
// Anything that is not plainly parked keeps today's path: a seat holding
// started work, a seat whose runtime died (runtime-missing), a seat past its
// bound or whose slept_at sits ahead of the clock, a fresh-wake seat, and a
// slot-bounded pool's oldest seats while its slots are full. A seat a pass
// sent down the stranded path parks again once it is parked-shaped and
// eligible: the hold comes before the repair, so a one-pass fallback (a full
// pool, a read that failed) loses no context. The park ends that stranding
// episode: it clears the marker the fallback left, as a seat seen alive again
// clears it, so a later fallback ages a fresh one through the confirm grace
// rather than repairing at once.

import (
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/mail/beadmail"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// parkedWorkMaxHold bounds how long a seat parks on blocked pre-assigned
// work, from its slept_at. Past it, the seat takes the stranded path (marker,
// session.stranded, the confirm grace, the repair), which returns the work to
// the pool, so a seat that cannot come back never holds work forever. It
// covers a long review loop (hours) and a day of waiting on a human.
const parkedWorkMaxHold = 24 * time.Hour

// parkedSleptAtFutureSkew bounds how far past the pass time a seat's slept_at
// may sit and still park it: a later one proves nothing (a clock stepped
// back, a bad write), and must never stretch parkedWorkMaxHold.
const parkedSleptAtFutureSkew = 5 * time.Minute

// parkVerdict is one row's park decision for a pass.
type parkVerdict struct {
	// Eligible: the row may park. It is an asleep pool seat on a voluntary
	// stop (sleep_reason idle), not named, under no hold, not fresh-wake,
	// inside its bound, and its pool has a free slot for it (a slot-bounded
	// pool's oldest seats fall back first).
	Eligible bool
	// Parked: Eligible, and the pass's view of its assigned work is
	// non-empty and all open, unstarted and not ready.
	Parked bool
	// Until is when the hold ends: slept_at plus parkedWorkMaxHold.
	Until time.Time
}

// parkedSeats is one pass's verdicts, by session bead ID. A row with no
// entry has the zero verdict: not eligible.
type parkedSeats map[string]parkVerdict

// parked reports whether the row id is parked this pass.
func (p parkedSeats) parked(id string) bool { return p[strings.TrimSpace(id)].Parked }

// parkSeats decides, for one pass, which pool seats park. rows are the
// pass's open session rows, in one or more sets (a set may stand in for rows
// the census does not show yet); work is its assigned work and ready the
// store-scoped readiness verdict index-aligned with it (a missing flag reads
// not ready, as the awake set reads it). With no readiness verdict, no
// decision time or no config it decides nothing: a pass that cannot tell
// blocked from ready keeps today's path. It runs every pass over every row,
// so it touches only asleep pool rows until a slot-bounded pool has a seat
// to park, and only then counts that pool's open rows.
func parkSeats(cfg *config.City, rows [][]sessionpkg.Info, work []beads.Bead, ready []bool, now time.Time) parkedSeats {
	if cfg == nil || ready == nil || now.IsZero() {
		return nil
	}
	byAssignee := make(map[string][]int)
	for i, b := range work {
		if assignee := strings.TrimSpace(b.Assignee); assignee != "" && parkCountsAsWork(b) {
			byAssignee[assignee] = append(byAssignee[assignee], i)
		}
	}
	type candidate struct {
		info   *sessionpkg.Info
		until  time.Time
		slept  time.Time
		marked bool // a stranded marker: on the stranded path, its repair freeing its slot
		shaped bool // its work is all open, unstarted, not deferred and not ready
		pool   string
		agent  *config.Agent
	}
	var cands []candidate
	for _, set := range rows {
		for i := range set {
			info := &set[i]
			if !parkRowInScope(*info) || strings.TrimSpace(info.MetadataState) != string(sessionpkg.StateAsleep) {
				continue
			}
			template := normalizedSessionTemplateInfo(*info, cfg)
			agent := findAgentByTemplate(cfg, template)
			slept, until, ok := parkableRow(*info, agent, now)
			if !ok {
				continue
			}
			cands = append(cands, candidate{
				info: info, until: until, slept: slept, pool: template, agent: agent,
				marked: strings.TrimSpace(info.StrandedEventEmittedAt) != "",
				shaped: parkedWorkShape(*info, work, ready, seatWorkIndexes(*info, byAssignee), now),
			})
		}
	}
	if len(cands) == 0 {
		return nil
	}
	// A slot-bounded pool parks only while a slot stays free: its oldest
	// parked-shaped seats fall back until one is. A seat already on the
	// stranded path falls back first: its repair is what frees a slot.
	bounded := make(map[string]int) // slot-bounded pools with a seat to park, to their slots
	for _, c := range cands {
		if slots, ok := poolSlotCapacity(c.agent); ok && c.shaped {
			bounded[c.pool] = slots
		}
	}
	fallback := make(map[string]bool)
	if len(bounded) > 0 {
		open := make(map[string]int, len(bounded)) // slot-holding open rows, by bounded pool
		for _, set := range rows {
			for i := range set {
				info := &set[i]
				if !parkRowInScope(*info) {
					continue
				}
				template := normalizedSessionTemplateInfo(*info, cfg)
				if _, ok := bounded[template]; !ok {
					continue
				}
				if holdsPoolSlot(*info, len(seatWorkIndexes(*info, byAssignee)) > 0, now) {
					open[template]++
				}
			}
		}
		for pool, slots := range bounded {
			excess := open[pool] - slots + 1
			if excess <= 0 {
				continue
			}
			var shaped []candidate
			for _, c := range cands {
				if c.pool == pool && c.shaped {
					shaped = append(shaped, c)
				}
			}
			slices.SortFunc(shaped, func(a, b candidate) int {
				switch {
				case a.marked != b.marked:
					if a.marked {
						return -1
					}
					return 1
				case !a.slept.Equal(b.slept):
					return a.slept.Compare(b.slept)
				}
				return strings.Compare(a.info.ID, b.info.ID)
			})
			for _, c := range shaped[:min(excess, len(shaped))] {
				fallback[c.info.ID] = true
			}
		}
	}
	out := make(parkedSeats, len(cands))
	for _, c := range cands {
		if fallback[c.info.ID] {
			continue
		}
		out[c.info.ID] = parkVerdict{Eligible: true, Parked: c.shaped, Until: c.until}
	}
	return out
}

// parkRowInScope reports an open, unnamed pool row: what holds a pool slot,
// and what may park.
func parkRowInScope(info sessionpkg.Info) bool {
	return !info.Closed && isPoolManagedSessionInfo(info) && !isNamedSessionInfo(info)
}

// holdsPoolSlot reports whether an open pool row holds a numbered slot for
// the headroom count: every open row but a failed create (which holds its
// name, not its slot) and a workless seat asleep in a freeable state, which
// the pool-slot close ends this pass.
func holdsPoolSlot(info sessionpkg.Info, hasWork bool, now time.Time) bool {
	switch {
	case isFailedCreateSessionInfo(info):
		return false
	case !hasWork && strings.TrimSpace(info.MetadataState) == string(sessionpkg.StateAsleep) && isPoolSessionSlotFreeableInfo(info, now):
		return false
	}
	return true
}

// parkableRow is the row half of the verdict: an asleep pool seat on a
// voluntary stop (the drain-ack finalize's and the idle stop's sleep_reason
// idle), under no hold, whose template is configured and not fresh-wake
// (wake_mode=fresh keeps no conversation to resume, and the planner never
// reuses such a row asleep), inside its bound from slept_at, which sits less
// than parkedSleptAtFutureSkew past now. It returns the slept_at and the
// bound's end.
func parkableRow(info sessionpkg.Info, agent *config.Agent, now time.Time) (slept, until time.Time, ok bool) {
	switch {
	case agent == nil,
		strings.TrimSpace(info.MetadataState) != string(sessionpkg.StateAsleep),
		strings.TrimSpace(info.SleepReason) != string(sessionpkg.SleepReasonIdle),
		strings.TrimSpace(info.WakeMode) == "fresh",
		agent.EffectiveWakeMode() == "fresh",
		sessionpkg.DispositionOfInfo(info).HoldsDeliberately(now):
		return time.Time{}, time.Time{}, false
	}
	slept, err := time.Parse(time.RFC3339, strings.TrimSpace(info.SleptAt))
	if err != nil || slept.Sub(now) >= parkedSleptAtFutureSkew {
		return time.Time{}, time.Time{}, false
	}
	until = slept.Add(parkedWorkMaxHold)
	if !now.Before(until) {
		return time.Time{}, time.Time{}, false
	}
	return slept, until, true
}

// seatWorkIndexes are the indexes of the work assigned to one of the row's
// assignee identities, in work order, each once.
func seatWorkIndexes(info sessionpkg.Info, byAssignee map[string][]int) []int {
	var idx []int
	for _, id := range sessionpkg.AssigneeIdentities(info) {
		idx = append(idx, byAssignee[id]...)
	}
	slices.Sort(idx)
	return slices.Compact(idx)
}

// parkedWorkShape is the work half of the verdict: the row holds work, and
// every item is open, not started by the seat (its claim and wake stamps, its
// work_started cell, its execution_claim_started stamp; an unreadable cell
// reads every claim started), not deferred and not ready. A deferred bead is
// no pool demand (filterAssignedWorkBeadsForPoolDemandAt), so no request
// would keep the seat selected: it closes as today.
func parkedWorkShape(info sessionpkg.Info, work []beads.Bead, ready []bool, idx []int, now time.Time) bool {
	if len(idx) == 0 {
		return false
	}
	seat := releaseSeatOfInfo(info)
	if seat.recordedUnread {
		return false
	}
	for _, i := range idx {
		b := work[i]
		if !parkableItem(b, seat, now) || i < len(ready) && ready[i] {
			return false
		}
	}
	return true
}

// parkableItem reports whether one of a seat's work beads may be parked on:
// open, not started by the seat, not deferred.
func parkableItem(b beads.Bead, seat releaseSeat, now time.Time) bool {
	return strings.TrimSpace(b.Status) == "open" && !seat.started[strings.TrimSpace(b.ID)] && !beads.IsDeferred(b, now)
}

// parkCountsAsWork is the work a seat can hold: no session bead and no mail,
// as the seat-work reads skip them.
func parkCountsAsWork(b beads.Bead) bool {
	return !sessionpkg.IsSessionBeadOrRepairable(b) && !beadmail.IsMessageBead(b)
}

// poolSlotCapacity is a pool's numbered-slot bound (freshPoolSlotUpperBound):
// max_active_sessions or its namepool, one for a canonical singleton, none
// when unbounded. A pool that allows no fresh slot has none free.
func poolSlotCapacity(agent *config.Agent) (slots int, bounded bool) {
	upper, unlimited, canonical, err := freshPoolSlotUpperBound(agent)
	switch {
	case err != nil:
		return 0, true
	case canonical:
		return 1, true
	case unlimited:
		return 0, false
	}
	return upper, true
}

// liveParkedWork is the work half of the verdict over a live read of the
// seat's work (collectSessionAssignedWorkInfo, readErr its error), with
// readiness read live: every item open, unstarted and not deferred, and none
// ready to the store's ready query for its assignee on either tier (unheld
// non-session work, as the wake reads it). A read that failed is no answer:
// it returns the error, and the caller must neither park nor strand on it.
func liveParkedWork(work []strandedAssignedWork, readErr error, seat releaseSeat, now time.Time) (bool, error) {
	if readErr != nil {
		return false, fmt.Errorf("reading the seat's work: %w", readErr)
	}
	if len(work) == 0 || seat.recordedUnread {
		return false, nil
	}
	type probe struct {
		store    beads.Store
		assignee string
	}
	var probes []probe
	seen := make(map[probe]bool)
	for _, item := range work {
		if !parkableItem(item.bead, seat, now) {
			return false, nil
		}
		p := probe{store: item.store, assignee: strings.TrimSpace(item.bead.Assignee)}
		if !seen[p] {
			seen[p] = true
			probes = append(probes, p)
		}
	}
	for _, p := range probes {
		wa := workAssignmentForStore(beads.WorkStore{Store: p.store})
		for _, tier := range []beads.TierMode{beads.TierIssues, beads.TierWisps} {
			ready, err := wa.ReadyAssignedTo(p.assignee, tier)
			if err != nil {
				return false, fmt.Errorf("reading %s's ready work: %w", p.assignee, err)
			}
			for _, r := range ready {
				if assignedOpenWorkHeld(r) || !parkCountsAsWork(r) {
					continue
				}
				if slices.ContainsFunc(work, func(item strandedAssignedWork) bool { return item.store == p.store && item.bead.ID == r.ID }) {
					return false, nil
				}
			}
		}
	}
	return true, nil
}

// holdsParkedSeat is the pool-free branch's park decision (legacy): held
// when the tick's verdict parks the seat, or the seat was eligible and its
// own live read, through the tick's seat work, finds only parked work; parked
// when either accepts the park. A live read that failed holds the seat for
// this tick, logged, and accepts no park: it neither parks nor strands on no
// answer.
func holdsParkedSeat(sw *SeatWork, info sessionpkg.Info, v parkVerdict, now time.Time, stderr io.Writer) (held, parked bool) {
	switch {
	case v.Parked:
		return true, true
	case !v.Eligible:
		return false, false
	}
	work, readErr := collectSessionAssignedWorkInfo(sw, info)
	parked, err := liveParkedWork(work, readErr, releaseSeatOfInfo(info), now)
	if err != nil {
		fmt.Fprintf(stderr, "session reconciler: holding %s this tick: parked check: %v\n", info.ID, err) //nolint:errcheck // best-effort stderr
		return true, false
	}
	return parked, parked
}

// parkSeatsForDemand is parkSeats over a demand read: its assigned work, the
// store refs index-aligned with it, and its store-scoped readiness. A partial
// read parks nothing: it cannot tell blocked from ready.
func parkSeatsForDemand(cfg *config.City, rows [][]sessionpkg.Info, work []beads.Bead, refs []string, ready map[storeScopedBeadKey]bool, partial bool, now time.Time) parkedSeats {
	if partial {
		return nil
	}
	flags := readyAssignedFlagsForBeads(ready, work, refs)
	if flags == nil {
		flags = []bool{}
	}
	return parkSeats(cfg, rows, work, flags, now)
}

// parkSeatsForResult is parkSeatsForDemand over a desired-state build's
// assigned-work read.
func parkSeatsForResult(cfg *config.City, rows []sessionpkg.Info, r DesiredStateResult, now time.Time) parkedSeats {
	return parkSeatsForDemand(cfg, [][]sessionpkg.Info{rows}, r.AssignedWorkBeads, r.AssignedWorkStoreRefs, r.ReadyAssigned, r.snapshotQueryPartial(), now)
}
