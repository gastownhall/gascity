package main

// The close-time re-read (mc-3ixn3.16). The tick's seat work index is as fresh
// as its read, so a claim assigned to a seat after it was invisible to the
// tick's gates: the seat closed holding it, and an unrouted open claim was
// stranded on the closed row for good. Before a close, the closing seat's
// narrow identities are read again:
//
//   - live on the local legs (the SQLite binding, a native rig);
//   - from the event-fed cache on the work store and on a bd rig, which costs
//     no I/O. A remote store is never read live here: a cache that cannot
//     answer leaves the index's answer standing.
//
// A close a gate decided ("this seat holds nothing") is refused on a hit; a
// close that releases the seat's work anyway (a corpse, a stranded repair)
// releases what the re-read found too.

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/mail/beadmail"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/storeref"
)

// lateSeatWork re-reads the work ids hold now, across legs: rows in one of
// statuses, not session or mail beads, that keep (when set) accepts.
func lateSeatWork(legs workLegs, store beads.Store, ids, statuses []string, keep func(beads.Store, beads.Bead) (bool, error)) ([]seatWorkHit, error) {
	plan, err := assignedWorkSweepPlan(legs.cityPath, legs.cfg, store, legs.rigs, ids)
	if err != nil {
		return nil, err
	}
	work := unwrapClassStore(censusWorkLeg(legs.cityPath, store))
	var hits []seatWorkHit
	var errs error
	storeref.EachLeg(plan, func(leg storeref.Leg, _ storeref.Role, _ storeref.ErrPolicy) { // residency:allow — re-reads assignedWorkSweepPlan's own legs
		list := closeTimeLister(leg.Store, unwrapClassStore(leg.Store) == work)
		for _, status := range statuses {
			for _, id := range ids {
				if id = strings.TrimSpace(id); id == "" || list == nil {
					continue
				}
				items, err := list(beads.ListQuery{Assignee: id, Status: status, TierMode: beads.TierBoth})
				errs = errors.Join(errs, err)
				for _, b := range items {
					if sessionpkg.IsSessionBeadOrRepairable(b) || beadmail.IsMessageBead(b) {
						continue
					}
					if keep != nil {
						ok, err := keep(leg.Store, b)
						if errs = errors.Join(errs, err); !ok || err != nil {
							continue
						}
					}
					hits = append(hits, seatWorkHit{store: leg.Store, bead: b})
				}
			}
		}
	})
	return hits, errs
}

// closeTimeLister reads one leg for the re-read: a live list on a local leg,
// the cache on the work store or a bd rig, and nil when such a leg has no
// cache to answer from.
func closeTimeLister(store beads.Store, isWork bool) func(beads.ListQuery) ([]beads.Bead, error) {
	if !isWork && !storeBackedBy[*beads.BdStore](store) {
		return func(q beads.ListQuery) ([]beads.Bead, error) {
			q.Live = true
			return store.List(q)
		}
	}
	type cachedLister interface {
		CachedList(beads.ListQuery) ([]beads.Bead, bool)
	}
	cache, ok := storeLayer[cachedLister](store)
	if !ok {
		return nil
	}
	return func(q beads.ListQuery) ([]beads.Bead, error) {
		items, _ := cache.CachedList(q)
		return items, nil
	}
}

// storeLayer finds the first layer of store, through the class, policy and
// cache wrappers, that is a T.
func storeLayer[T any](store beads.Store) (T, bool) {
	for i := 0; store != nil && i < 8; i++ {
		if t, ok := store.(T); ok {
			return t, true
		}
		switch v := store.(type) {
		case beads.SessionStore:
			store = v.Store
		case beads.WorkStore:
			store = v.Store
		case interface{ Backing() beads.Store }:
			store = v.Backing()
		case interface{ ConditionalWritesResolveTarget() beads.Store }:
			store = v.ConditionalWritesResolveTarget()
		default:
			store = nil
		}
	}
	var zero T
	return zero, false
}

func storeBackedBy[T any](store beads.Store) bool {
	_, ok := storeLayer[T](store)
	return ok
}

// closeBeadUnlessLateWork is closeBead for a close a gate decided: it refuses
// when the close-time re-read finds work the gate's read did not, or cannot
// read a local leg. keep is the gate's own filter (the drain-finalize gate
// excludes the seat's own drain step).
func closeBeadUnlessLateWork(store beads.Store, legs workLegs, info sessionpkg.Info, reason string, now time.Time, stderr io.Writer, keep func(beads.Store, beads.Bead) (bool, error)) bool {
	if stderr == nil {
		stderr = io.Discard
	}
	late, err := lateSeatWork(legs, store, sessionAssignmentIdentifiersForConfigInfo(info, legs.cfg), seatWorkStatuses, keep)
	switch {
	case err != nil:
		fmt.Fprintf(stderr, "session beads: close of %s refused: re-reading its work: %v\n", info.ID, err) //nolint:errcheck
		return false
	case len(late) > 0:
		fmt.Fprintf(stderr, "session beads: close of %s refused: %s was assigned to it since this tick's read\n", info.ID, late[0].bead.ID) //nolint:errcheck
		return false
	}
	return closeBead(store, legs, info, reason, now, stderr)
}

// appendUniqueSeatWork adds the rows of more that rows does not already hold.
func appendUniqueSeatWork(rows, more []seatWorkHit) []seatWorkHit {
	for _, m := range more {
		dup := false
		for _, r := range rows {
			if r.store == m.store && r.bead.ID == m.bead.ID {
				dup = true
				break
			}
		}
		if !dup {
			rows = append(rows, m)
		}
	}
	return rows
}
