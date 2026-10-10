package main

// SeatWork: one read of every work leg, which "does this seat still hold
// work?" questions answer from in memory.
//
// mc-3ixn3.16 put the city work store, bd over a remote database on
// maintainer-city (seconds a call), into every gate's legs; a live probe per
// identity, status and tier cost tens of seconds per decision. A SeatWork pays
// each leg once, and only when a question is asked. Its answers:
//
//   - a hit is re-read live and counts only if the live bead still matches
//     (match); a question whose answer only keeps a seat may take the row as
//     read (snapshot);
//   - a miss on a leg read completely is no work there (as of the read);
//   - an unreadable leg is unknown: with no confirmed hit elsewhere the
//     question fails closed;
//   - a RefuseScope question (it gates ending a live runtime) is never
//     answered from the tick's read: it reads the seat live.
//
// It is an explicit value over a WorkLegs (work_location.go). The legacy tick
// reads every assignee once per tick (newSeatWork) and hands it to every phase
// that asks; a v2 effect or a one-shot command reads one seat's identities
// live (seatWorkFor).

import (
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/mail/beadmail"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// seatWorkStatuses are the statuses a seat's work can hold: what a SeatWork reads.
var seatWorkStatuses = []string{"open", "in_progress"}

// SeatWork is one read of every leg of a WorkLegs, keyed by assignee.
type SeatWork struct {
	legs WorkLegs
	// ids, when set, is the one seat's read: only these assignees were listed,
	// live. A question about any other identity is unknown. Nil reads every
	// assignee.
	ids map[string]bool

	// alerts raises session.unserved_claims for a close this read refuses
	// (work_release.go); nil alerts nothing.
	alerts *unservedClaimAlerts

	once sync.Once
	read []seatWorkLeg
}

type seatWorkLeg struct {
	store      beads.Store
	errs       map[string]error // by status: a list that failed leaves that status unknown on the leg
	byAssignee map[string][]beads.Bead
}

// unread is why the leg cannot answer for statuses: a failed list of one of
// them.
func (l seatWorkLeg) unread(statuses []string) error {
	var err error
	for _, s := range statuses {
		err = errors.Join(err, l.errs[s])
	}
	return err
}

// seatWorkQuery is one question about a seat's work: beads in one of statuses,
// assigned to one of scope's identities, that are not session or mail beads and
// that keep (when set) accepts. keep sees the live bead and the store it lives
// in.
type seatWorkQuery struct {
	scope    workScope
	statuses []string
	keep     func(store beads.Store, b beads.Bead) (bool, error)
}

// seatWorkHit is one answer to a seatWorkQuery: a bead and the leg it lives in.
type seatWorkHit struct {
	store beads.Store
	bead  beads.Bead
}

// newSeatWork is the tick's read: every assignee on every leg, read on the
// first question.
func newSeatWork(legs WorkLegs) *SeatWork {
	return &SeatWork{legs: legs}
}

// seatWorkFor is one seat's read: scope's identities only, listed live on
// every leg.
func seatWorkFor(legs WorkLegs, scope workScope) *SeatWork {
	ids := make(map[string]bool)
	for _, id := range scope.scopeIDs() {
		if id = strings.TrimSpace(id); id != "" {
			ids[id] = true
		}
	}
	return &SeatWork{legs: legs, ids: ids}
}

// fresh is one seat's read of sw's legs now, for a question a snapshot "no
// work" must not answer.
func (sw *SeatWork) fresh(scope workScope) *SeatWork {
	return seatWorkFor(sw.Legs(), scope)
}

// unserved is sw's alerter; nil for a nil SeatWork.
func (sw *SeatWork) unserved() *unservedClaimAlerts {
	if sw == nil {
		return nil
	}
	return sw.alerts
}

// Legs is the leg set sw reads. A nil SeatWork reads the zero WorkLegs,
// which answers every question unknown.
func (sw *SeatWork) Legs() WorkLegs {
	if sw == nil {
		return WorkLegs{}
	}
	return sw.legs
}

// seatWorkList is one list a read makes, and the leg it fills.
type seatWorkList struct {
	leg int
	q   beads.ListQuery
}

// load lists every leg once, concurrently: one live list per status (per
// identity on a seat's read), both tiers.
func (sw *SeatWork) load() {
	sw.legs.each(func(s beads.Store) { sw.read = append(sw.read, seatWorkLeg{store: s}) })
	var lists []seatWorkList
	for i := range sw.read {
		sw.read[i].byAssignee = make(map[string][]beads.Bead)
		sw.read[i].errs = make(map[string]error)
		for _, status := range seatWorkStatuses {
			if sw.ids == nil {
				lists = append(lists, seatWorkList{leg: i, q: beads.ListQuery{Status: status, TierMode: beads.TierBoth, Live: true}})
				continue
			}
			for id := range sw.ids {
				lists = append(lists, seatWorkList{leg: i, q: beads.ListQuery{Assignee: id, Status: status, TierMode: beads.TierBoth, Live: true}})
			}
		}
	}
	// Every list runs at once: on a remote store a read costs one list's
	// latency, not one per status and leg.
	rows := make([][]beads.Bead, len(lists))
	errs := make([]error, len(lists))
	var wg sync.WaitGroup
	for i, l := range lists {
		wg.Add(1)
		go func(i int, l seatWorkList) {
			defer wg.Done()
			defer recoverLeg(&errs[i], "seat work leg", log.Writer())
			rows[i], errs[i] = sw.read[l.leg].store.List(l.q)
		}(i, l)
	}
	wg.Wait()
	for i, l := range lists {
		leg := &sw.read[l.leg]
		if errs[i] != nil {
			leg.errs[l.q.Status] = errors.Join(leg.errs[l.q.Status], errs[i])
			continue
		}
		for _, b := range rows[i] {
			if assignee := strings.TrimSpace(b.Assignee); assignee != "" {
				leg.byAssignee[assignee] = append(leg.byAssignee[assignee], b)
			}
		}
	}
}

// match answers q: every live-confirmed hit in leg order, or only the first when
// first is set. Without a first hit, an unreadable leg is the error.
func (sw *SeatWork) match(q seatWorkQuery, first bool) ([]seatWorkHit, error) {
	return sw.find(q, first, sw != nil && sw.ids == nil)
}

// snapshot is match on the rows as read, without the live re-read. It serves
// answers that are safe on a stale row: a started-work hit only keeps a seat,
// and a release is fenced on the row's status and assignee at the write.
func (sw *SeatWork) snapshot(q seatWorkQuery, first bool) ([]seatWorkHit, error) {
	return sw.find(q, first, false)
}

// stores answers which legs hold a row for q, the pre-filter for questions a
// SeatWork cannot answer itself (readiness): a leg with none holds no answer.
// residency:allow — a subset of the plan's legs, in plan order; resolves nothing.
func (sw *SeatWork) stores(q seatWorkQuery) ([]beads.Store, error) {
	hits, err := sw.find(q, false, false)
	var out []beads.Store
	for _, hit := range hits {
		if len(out) == 0 || out[len(out)-1] != hit.store {
			out = append(out, hit.store)
		}
	}
	return out, err
}

func (sw *SeatWork) find(q seatWorkQuery, first, recheck bool) ([]seatWorkHit, error) {
	if err := sw.Legs().unusable(); err != nil {
		return nil, err
	}
	// A refusing question gates ending a live runtime, which a snapshot "no
	// work" never authorizes: on the tick's read every form of it (match,
	// snapshot, stores) is answered by a live read of the seat.
	if q.scope.refuses() && sw.ids == nil {
		return sw.fresh(q.scope).find(q, first, false)
	}
	sw.once.Do(sw.load)
	ids := make(map[string]bool)
	var order []string
	var unknown error
	for _, id := range q.scope.scopeIDs() {
		if id = strings.TrimSpace(id); id == "" || ids[id] {
			continue
		}
		if sw.ids != nil && !sw.ids[id] {
			unknown = errors.Join(unknown, fmt.Errorf("seat work: %q was not read", id))
			continue
		}
		ids[id] = true
		order = append(order, id)
	}
	statuses := make(map[string]bool, len(q.statuses))
	for _, s := range q.statuses {
		statuses[s] = true
	}
	var hits []seatWorkHit
	for _, leg := range sw.read {
		if err := leg.unread(q.statuses); err != nil {
			unknown = errors.Join(unknown, err)
			continue
		}
		seen := make(map[string]bool)
		for _, id := range order {
			for _, row := range leg.byAssignee[id] {
				if !statuses[row.Status] || seen[row.ID] {
					continue
				}
				seen[row.ID] = true
				if recheck {
					live, err := liveBeadRead(leg.store, row.ID)
					if errors.Is(err, beads.ErrNotFound) {
						continue
					}
					if err != nil {
						unknown = errors.Join(unknown, err)
						continue
					}
					row = live
				}
				ok, err := q.accepts(leg.store, row, ids, statuses)
				if err != nil {
					return hits, err
				}
				if !ok {
					continue
				}
				hits = append(hits, seatWorkHit{store: leg.store, bead: row})
				if first {
					return hits, nil
				}
			}
		}
	}
	// Only reached without a first hit: in the all-hits form an unreadable leg
	// still means the answer is incomplete.
	return hits, unknown
}

func (q seatWorkQuery) accepts(store beads.Store, b beads.Bead, ids, statuses map[string]bool) (bool, error) {
	if !statuses[b.Status] || !ids[strings.TrimSpace(b.Assignee)] {
		return false, nil
	}
	if sessionpkg.IsSessionBeadOrRepairable(b) || beadmail.IsMessageBead(b) {
		return false, nil
	}
	if q.keep == nil {
		return true, nil
	}
	return q.keep(store, b)
}

// unwrapClassStore peels the typed class wrappers, which callers pass for the
// same store.
func unwrapClassStore(store beads.Store) beads.Store {
	switch v := store.(type) {
	case beads.SessionStore:
		return v.Store
	case beads.WorkStore:
		return v.Store
	}
	return store
}

// has answers an existence gate.
func (sw *SeatWork) has(q seatWorkQuery) (bool, error) {
	hits, err := sw.match(q, true)
	return len(hits) > 0, err
}

// first is has for the gates that name the bead they found.
func (sw *SeatWork) first(q seatWorkQuery) (beads.Bead, bool, error) {
	hits, err := sw.match(q, true)
	if len(hits) == 0 {
		return beads.Bead{}, false, err
	}
	return hits[0].bead, true, nil
}
