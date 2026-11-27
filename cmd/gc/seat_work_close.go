package main

// The close-time re-read (mc-3ixn3.16). The tick's SeatWork read is as fresh
// as its read, so a claim assigned to a seat after it was invisible to the
// tick's gates: the seat closed holding it, and an unrouted open claim was
// stranded on the closed row for good. Before a close, the closing seat's
// narrow identities are read again:
//
//   - live on the local legs (the SQLite binding, a native rig, the uncached
//     work store of a standalone controller or a one-shot pass);
//   - from the event-fed cache on a controller's work store and on a bd rig,
//     which costs no I/O. A bd-backed store is never read live here: one with
//     no cache leaves the tick's answer standing (mc-3ixn3.19/.20).
//
// A close a gate decided ("this seat holds nothing") is refused on a hit; a
// close that releases the seat's work anyway (a corpse, a stranded repair)
// releases what the re-read found too.
//
// The parked read (parkedRead) lists a seat's parked claims for the releases
// that give them back. It reads a cached leg from the cache alone too, but
// lists a bd leg with no cache live: no other read lists a parked claim
// there. A leg it cannot list in full is named in the release log
// (errParkedUnread).

import (
	"errors"
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/mail/beadmail"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/storeref"
)

// seatLegRead is the read lateSeatWork makes of each leg (closeTimeLister).
type seatLegRead uint8

const (
	// closeReRead is the close-time re-read of a seat's claims in the
	// statuses asked.
	closeReRead seatLegRead = iota
	// parkedRead lists a seat's claims not closed, for the parked ones among
	// them (parkedSeatWork).
	parkedRead
)

// errParkedUnread is a leg the parked read could not list in full
// (parkedFromCache): a claim parked there may stay assigned to its holder.
var errParkedUnread = errors.New("parked claims not read in full")

// lateSeatWork reads, as read says, what the work scope's identities hold now
// across legs: rows in one of statuses, not session or mail beads, that keep
// (when set) accepts. A leg the read could not list in full (errParkedUnread)
// is named once in the error, beside what the read did list there.
func lateSeatWork(legs WorkLegs, scope workScope, statuses []string, keep func(beads.Store, beads.Bead) (bool, error), read seatLegRead) ([]seatWorkHit, error) {
	if err := legs.unusable(); err != nil {
		return nil, err
	}
	var hits []seatWorkHit
	var errs error
	legs.eachLeg(func(ref string, store beads.Store) {
		list := closeTimeLister(store, legs.isWork(store), read)
		if list == nil {
			return
		}
		var unread error
		for _, status := range statuses {
			for _, id := range scope.scopeIDs() {
				if id = strings.TrimSpace(id); id == "" {
					continue
				}
				items, err := list(beads.ListQuery{Assignee: id, Status: status, TierMode: beads.TierBoth})
				if errors.Is(err, errParkedUnread) {
					unread, err = err, nil
				}
				errs = errors.Join(errs, err)
				for _, b := range items {
					if sessionpkg.IsSessionBeadOrRepairable(b) || beadmail.IsMessageBead(b) {
						continue
					}
					if keep != nil {
						ok, err := keep(store, b)
						if errs = errors.Join(errs, err); !ok || err != nil {
							continue
						}
					}
					hits = append(hits, seatWorkHit{store: store, bead: b})
				}
			}
		}
		if unread != nil {
			errs = errors.Join(errs, fmt.Errorf("leg %s: %w", releaseLogLegName(ref), unread))
		}
	})
	return hits, errs
}

// releaseLogLegName is how the release log names a leg: its store ref, or
// "city work store" for the city work store's, which is empty
// (storeref.WorkRef).
func releaseLogLegName(ref string) string {
	if ref == string(storeref.WorkRef) {
		return "city work store"
	}
	return ref
}

// closeTimeLister reads one leg for the re-read:
//   - the city work store from its event-fed cache when it has one (a
//     controller's), which costs no I/O, and live when the cache declines
//     the query, unless bd backs it;
//   - any other local leg live: the SQLite binding, a native rig, and the
//     uncached work store of a standalone controller or a one-shot pass;
//   - a bd-backed leg from its cache, or not at all when it has none or the
//     cache declines: the residual of mc-3ixn3.19/.20, where the tick's read
//     stands alone.
//
// The parked read (parkedRead) lists not-closed at no more cost than the
// re-read: from the cache alone where the re-read reads the cache
// (parkedFromCache), and live where the re-read reads live. A bd leg with no
// cache (a one-shot command's) is read live too: there the parked read
// re-reads nothing, it is the one read that lists a parked claim, and it
// lists only the scope's identities.
//
// DoltliteReadStore (the gascity_native_beads read optimization over a bd
// store) is not a bd-backed leg here: storeBackedBy does not reach its
// embedded BdStore, and need not, since it lists by SQL in-process rather than
// by running bd, so the rules for a leg bd does not back read it. Its list
// filters on the raw status, as bd's does, so a cache over it primed in part
// holds no claim parked before the prime: parkedFromCache reports that read
// as in part.
func closeTimeLister(store beads.Store, isWork bool, read seatLegRead) func(beads.ListQuery) ([]beads.Bead, error) {
	cache, cached := storeLayer[cachedLister](store)
	bd := storeBackedBy[*beads.BdStore](store)
	live := func(q beads.ListQuery) ([]beads.Bead, error) {
		q.Live = true
		return store.List(q)
	}
	switch {
	case cached && (isWork || bd) && read == parkedRead:
		return func(q beads.ListQuery) ([]beads.Bead, error) {
			return parkedFromCache(cache, bd, q, live)
		}
	case cached && (isWork || bd):
		return func(q beads.ListQuery) ([]beads.Bead, error) {
			if items, ok := cache.CachedList(q); ok || bd {
				return items, nil
			}
			return live(q)
		}
	case bd && read != parkedRead:
		return nil
	}
	return live
}

// cachedLister is a cache that answers a list from memory alone when it can
// (beads.CachingStore.CachedList).
type cachedLister interface {
	CachedList(beads.ListQuery) ([]beads.Bead, bool)
}

// parkedFromCache is the parked read of a leg closeTimeLister reads from its
// cache: the claims of q's assignee that are not closed. A cache holding every
// row answers that itself. One primed in part (PrimeActive, a controller's until its full
// prime lands) answers only open and in_progress, and its open rows are not
// every parked claim: they leave out one an event parked as blocked since the
// prime, and, over a store whose list filters on the raw status (BdStore,
// DoltliteReadStore), every one parked before it. So that read is never taken
// as whole:
//   - a bd leg is not read (errParkedUnread): its open rows hold no claim
//     parked before the prime, and bd is not read live here;
//   - any other leg answers with its open rows, which hold what a folding
//     store (NativeDoltStore) primed as open, and says the read was in part
//     (errParkedUnread).
//
// When the cache answers neither (it is cold, or holds a dirty row), a bd leg
// is not read either, and any other leg is read live, as the re-read reads
// it.
func parkedFromCache(cache cachedLister, bd bool, q beads.ListQuery, live func(beads.ListQuery) ([]beads.Bead, error)) ([]beads.Bead, error) {
	q.Status = ""
	if items, ok := cache.CachedList(q); ok {
		return items, nil
	}
	if bd {
		return nil, fmt.Errorf("%w: its cache cannot answer, and a bd-backed leg is not read live here", errParkedUnread)
	}
	open := q
	open.Status = "open"
	if items, ok := cache.CachedList(open); ok {
		return items, fmt.Errorf("%w: its cache was primed in part, and its open rows are not every parked claim", errParkedUnread)
	}
	return live(q)
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
