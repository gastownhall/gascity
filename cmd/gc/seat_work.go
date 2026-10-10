package main

// The seat work index: one read of every work leg per reconciler tick, which
// the tick's "does this seat still hold work?" questions answer from in memory.
//
// mc-3ixn3.16 put the city work store, bd over a remote database on
// maintainer-city (seconds a call), into every gate's legs; a live probe per
// identity, status and tier cost tens of seconds per decision. The index pays
// each leg once per tick, and only on a tick that asks. Its answers:
//
//   - a hit is re-read live and counts only if the live bead still matches;
//   - a miss on a leg read completely is no work there (as of the read);
//   - an unreadable leg is unknown: with no confirmed hit elsewhere the
//     question fails closed, as assignedWorkScanComplete does.
//
// The legs are assignedWorkSweepPlan's, the census's, so the index and the live
// path agree on the stores. The minimal form of ARCH-RESTRUCTURE K-b.

import (
	"errors"
	"log"
	"path/filepath"
	"strings"
	"sync"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/mail/beadmail"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/storeref"
)

// seatWorkStatuses are the statuses a seat's work can hold: what the index reads.
var seatWorkStatuses = []string{"open", "in_progress"}

// seatWorkIndex is one tick's read of every work leg, keyed by assignee.
type seatWorkIndex struct {
	cityPath string
	cfg      *config.City
	leading  beads.Store
	rigs     map[string]beads.Store

	once sync.Once
	err  error // the leg set itself could not be resolved
	legs []seatWorkLeg
}

type seatWorkLeg struct {
	store      beads.Store
	err        error // the leg could not be read: whatever it holds is unknown
	byAssignee map[string][]beads.Bead
}

// seatWorkQuery is one question about a seat's work: beads in one of statuses,
// assigned to one of ids, that are not session or mail beads and that keep (when
// set) accepts. keep sees the live bead and the store it lives in.
type seatWorkQuery struct {
	ids      []string
	statuses []string
	keep     func(store beads.Store, b beads.Bead) (bool, error)
}

// seatWorkHit is a live-confirmed answer to a seatWorkQuery.
type seatWorkHit struct {
	store beads.Store
	bead  beads.Bead
}

func newSeatWorkIndex(cityPath string, cfg *config.City, leading beads.Store, rigs map[string]beads.Store) *seatWorkIndex {
	return &seatWorkIndex{cityPath: cityPath, cfg: cfg, leading: leading, rigs: rigs}
}

// read lists every leg once, concurrently: one live list per status, both tiers.
func (x *seatWorkIndex) read() {
	plan, err := assignedWorkSweepPlan(x.cityPath, x.cfg, x.leading, x.rigs, nil)
	if err != nil {
		x.err = err
		return
	}
	storeref.EachLeg(plan, func(leg storeref.Leg, _ storeref.Role, _ storeref.ErrPolicy) { // residency:allow — enumerates assignedWorkSweepPlan's own legs to read each once
		x.legs = append(x.legs, seatWorkLeg{store: leg.Store})
	})
	// Every (leg, status) list runs at once: on a remote store the index costs
	// one list's latency per tick, not one per status and leg.
	lists := make([][]beads.Bead, len(x.legs)*len(seatWorkStatuses))
	errs := make([]error, len(lists))
	var wg sync.WaitGroup
	for i := range lists {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			defer recoverLeg(&errs[i], "seat work index leg", log.Writer())
			leg, status := x.legs[i/len(seatWorkStatuses)], seatWorkStatuses[i%len(seatWorkStatuses)]
			lists[i], errs[i] = leg.store.List(beads.ListQuery{Status: status, TierMode: beads.TierBoth, Live: true})
		}(i)
	}
	wg.Wait()
	for i := range lists {
		leg := &x.legs[i/len(seatWorkStatuses)]
		if leg.byAssignee == nil {
			leg.byAssignee = make(map[string][]beads.Bead)
		}
		if errs[i] != nil {
			leg.err = errs[i]
			continue
		}
		for _, b := range lists[i] {
			if assignee := strings.TrimSpace(b.Assignee); assignee != "" {
				leg.byAssignee[assignee] = append(leg.byAssignee[assignee], b)
			}
		}
	}
}

// match answers q: every live-confirmed hit in leg order, or only the first when
// first is set. Without a first hit, an unreadable leg is the error.
func (x *seatWorkIndex) match(q seatWorkQuery, first bool) ([]seatWorkHit, error) {
	return x.find(q, first, true)
}

// snapshot is match on the rows the index read, without the live re-read. It
// serves answers that are safe on a stale row: a started-work hit only keeps a
// seat, and a release is fenced on the row's status and assignee at the write.
func (x *seatWorkIndex) snapshot(q seatWorkQuery, first bool) ([]seatWorkHit, error) {
	return x.find(q, first, false)
}

// stores answers which legs hold a row for q, the pre-filter for questions the
// index cannot answer itself (readiness): a leg with none holds no answer.
// residency:allow — a subset of the plan's legs, in plan order; resolves nothing.
func (x *seatWorkIndex) stores(q seatWorkQuery) ([]beads.Store, error) {
	hits, err := x.find(q, false, false)
	var out []beads.Store
	for _, hit := range hits {
		if len(out) == 0 || out[len(out)-1] != hit.store {
			out = append(out, hit.store)
		}
	}
	return out, err
}

func (x *seatWorkIndex) find(q seatWorkQuery, first, recheck bool) ([]seatWorkHit, error) {
	x.once.Do(x.read)
	if x.err != nil {
		return nil, x.err
	}
	ids := make(map[string]bool, len(q.ids))
	var order []string
	for _, id := range q.ids {
		if id = strings.TrimSpace(id); id != "" && !ids[id] {
			ids[id] = true
			order = append(order, id)
		}
	}
	statuses := make(map[string]bool, len(q.statuses))
	for _, s := range q.statuses {
		statuses[s] = true
	}
	var hits []seatWorkHit
	var unknown error
	for _, leg := range x.legs {
		if leg.err != nil {
			unknown = errors.Join(unknown, leg.err)
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

// The index is installed for the duration of one legacy reconcile tick and
// found by the city path plus the leading store the tick reads through, so a
// caller handing a different store (the API, a one-shot command) keeps the live
// path.
var (
	seatWorkIndexesMu sync.Mutex
	seatWorkIndexes   map[string]*seatWorkIndex
)

// installSeatWorkIndex installs a lazy index for one tick of a controller
// serving cityPath and returns its removal. A city no controller here serves
// (tests, one-shot callers) gets none.
//
// An index already installed for the city stays: the controller installs one
// for its whole tick (runTickPhases), and the reconcile pass inside it shares
// that read.
func installSeatWorkIndex(cityPath string, cfg *config.City, leading beads.Store, rigs map[string]beads.Store) func() {
	leading = unwrapClassStore(leading)
	if _, ok := registeredResidencyEntry(cityPath); !ok || leading == nil || seatWorkIndexFor(cityPath, leading) != nil {
		return func() {}
	}
	key := filepath.Clean(cityPath)
	idx := newSeatWorkIndex(cityPath, cfg, leading, rigs)
	seatWorkIndexesMu.Lock()
	if seatWorkIndexes == nil {
		seatWorkIndexes = make(map[string]*seatWorkIndex, 1)
	}
	seatWorkIndexes[key] = idx
	seatWorkIndexesMu.Unlock()
	return func() {
		seatWorkIndexesMu.Lock()
		if seatWorkIndexes[key] == idx {
			delete(seatWorkIndexes, key)
		}
		seatWorkIndexesMu.Unlock()
	}
}

// seatWorkIndexFor returns the installed index for cityPath when the caller
// reads through the tick's own leading store, else nil.
func seatWorkIndexFor(cityPath string, leading beads.Store) *seatWorkIndex {
	if cityPath == "" {
		return nil
	}
	seatWorkIndexesMu.Lock()
	idx := seatWorkIndexes[filepath.Clean(cityPath)]
	seatWorkIndexesMu.Unlock()
	if idx == nil || idx.leading != unwrapClassStore(leading) {
		return nil
	}
	return idx
}

// unwrapClassStore peels the typed class wrappers, which callers pass for the
// same store the tick installed under.
func unwrapClassStore(store beads.Store) beads.Store {
	switch v := store.(type) {
	case beads.SessionStore:
		return v.Store
	case beads.WorkStore:
		return v.Store
	}
	return store
}

// seatHasWork answers an existence gate from the tick's index when the caller
// reads through it, else by its live probe.
func seatHasWork(cityPath string, store beads.Store, q seatWorkQuery, live func() (bool, error)) (bool, error) {
	if idx := seatWorkIndexFor(cityPath, store); idx != nil {
		hits, err := idx.match(q, true)
		return len(hits) > 0, err
	}
	return live()
}

// seatFirstWork is seatHasWork for the gates that name the bead they found.
func seatFirstWork(cityPath string, store beads.Store, q seatWorkQuery, live func() (beads.Bead, bool, error)) (beads.Bead, bool, error) {
	if idx := seatWorkIndexFor(cityPath, store); idx != nil {
		hits, err := idx.match(q, true)
		if len(hits) == 0 {
			return beads.Bead{}, false, err
		}
		return hits[0].bead, true, nil
	}
	return live()
}
