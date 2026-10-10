package main

// The autoclose sweep: the backstop for close-triggered autoclose.
//
// Convoy, wisp and molecule autoclose run when applyBeadEventToStores sees a
// bead.closed. Nothing else runs them, so a close that never reaches the bus
// skipped them forever: the event log can drop the notification
// (CACHE-LAYERING-REVIEW F2). An unconfirmable scan-derived close
// (applyInferredClose) is deferred here too.
//
// The sweep diffs each live cache's active census (open and in-progress rows,
// both tiers) against the previous pass. A row that left the census was closed
// or deleted, notified or not. After one pass of grace, so the event path
// normally gets there first, the sweep re-reads the row live and acts only if
// it is closed: the completion fact (deduplicated against the journal) and
// autoclose. Autoclose recomputes from store state (a convoy with every member
// terminal, a closed parent's open attachments, a molecule with every step
// terminal), so running it twice for one close is safe; the ran set only saves
// the reads. The event path marks a close ran only once its autoclose finished
// (autocloseSweep.settle); a run a read error or a refused close left
// undecided is owed a check here instead.
//
// The confirming read goes to the store whose configured prefix owns the id,
// or on the unconfigured fallback to the first store that holds the row
// (liveReadOwner).
//
// Known limits: the first census of a cache only seeds it, so a close made
// while the controller was down, or a row opened and closed between two
// passes whose notification was also lost, is not swept. Deriving autoclose
// from journal ops (CACHE-LAYERING-REVIEW step 5) removes both. Autoclose's
// cross-row premises (every member terminal, a parent closed) are read-based;
// only the row it closes is fenced.

import (
	"context"
	"log"
	"runtime/debug"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
)

const (
	// autocloseSweepInterval is the pass cadence. A pass reads only cached
	// censuses unless a row left one, so a quiet city pays no store read.
	autocloseSweepInterval = time.Minute
	// autocloseSweepGrace delays a departure by one pass so the bus delivers
	// the close it usually has, and the event path runs autoclose first.
	autocloseSweepGrace = autocloseSweepInterval
	// autocloseSweepBatch caps the live reads (and autoclose runs) per pass.
	// 64 a minute is well above the 50-250 closes an hour a city's caches see.
	autocloseSweepBatch = 64
	// autocloseSweepPendingCap bounds the ids owed a check. Past it a departure
	// is dropped and counted: that is the one way this backstop misses.
	autocloseSweepPendingCap = 4096
	// autocloseSweepRanCap bounds the ran set. Overflow clears it, which costs
	// only a redundant autoclose recompute.
	autocloseSweepRanCap = 4096
)

type autocloseSweep struct {
	mu sync.Mutex
	// census is each cache's active ids at its last clean read.
	census map[*beads.CachingStore]map[string]struct{}
	// pending maps an id owed a check to when it is due.
	pending map[string]time.Time
	// ran holds ids the event path ran autoclose to the end for since they
	// last arrived in a census.
	ran map[string]struct{}

	batch, pendingCap, ranCap int
	dropped                   int
}

func newAutocloseSweep() *autocloseSweep {
	return &autocloseSweep{
		census:     map[*beads.CachingStore]map[string]struct{}{},
		pending:    map[string]time.Time{},
		ran:        map[string]struct{}{},
		batch:      autocloseSweepBatch,
		pendingCap: autocloseSweepPendingCap,
		ranCap:     autocloseSweepRanCap,
	}
}

// noteRan records that the event path ran autoclose for id to the end.
func (s *autocloseSweep) noteRan(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.noteRanLocked(id)
}

func (s *autocloseSweep) noteRanLocked(id string) {
	if len(s.ran) >= s.ranCap {
		s.ran = map[string]struct{}{}
	}
	s.ran[id] = struct{}{}
}

// settle records how an event-path autoclose run for id ended. A finished run
// marks id handled, so its departure costs the sweep no read. An unfinished
// one (a read failed, or a fenced close stayed refused) is unmarked and owed
// a check at due, so the sweep re-decides it.
func (s *autocloseSweep) settle(id string, finished bool, due time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if finished {
		s.noteRanLocked(id)
		return
	}
	delete(s.ran, id)
	s.deferLocked(id, due)
}

// deferID owes id a check at due.
func (s *autocloseSweep) deferID(id string, due time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deferLocked(id, due)
}

func (s *autocloseSweep) deferLocked(id string, due time.Time) {
	if _, ok := s.pending[id]; ok {
		return
	}
	if len(s.pending) >= s.pendingCap {
		s.dropped++
		return
	}
	s.pending[id] = due
}

// observe folds one cache's active census in. A departure is owed a check
// after the grace; an arrival forgets the id, so a reopened row's next close
// is checked afresh. The first census of a cache only seeds it.
func (s *autocloseSweep) observe(cache *beads.CachingStore, active map[string]struct{}, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	prev, seen := s.census[cache]
	s.census[cache] = active
	if !seen {
		return
	}
	for id := range prev {
		if _, still := active[id]; !still {
			s.deferLocked(id, now.Add(autocloseSweepGrace))
		}
	}
	for id := range active {
		if _, was := prev[id]; !was {
			delete(s.ran, id)
			delete(s.pending, id)
		}
	}
}

// retain drops the census of every cache not in keep (a reload replaced it).
func (s *autocloseSweep) retain(keep map[*beads.CachingStore]struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for cache := range s.census {
		if _, ok := keep[cache]; !ok {
			delete(s.census, cache)
		}
	}
}

// due pops up to batch ids due by now, minus the ones the event path ran.
func (s *autocloseSweep) due(now time.Time) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for id, at := range s.pending {
		if len(out) >= s.batch {
			break
		}
		if at.After(now) {
			continue
		}
		delete(s.pending, id)
		if _, ran := s.ran[id]; ran {
			delete(s.ran, id)
			continue
		}
		out = append(out, id)
	}
	return out
}

func (s *autocloseSweep) isPending(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.pending[id]
	return ok
}

func (s *autocloseSweep) hasRan(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.ran[id]
	return ok
}

// autocloseSweepOf returns the controller's sweep, creating it on first use so
// a directly constructed controllerState needs no wiring.
func (cs *controllerState) autocloseSweepOf() *autocloseSweep {
	cs.autocloseSweepOnce.Do(func() { cs.autocloseSweep = newAutocloseSweep() })
	return cs.autocloseSweep
}

// startAutocloseSweep runs the sweep every autocloseSweepInterval until ctx
// ends.
func (cs *controllerState) startAutocloseSweep(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(clock.Backstop(autocloseSweepInterval))
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				cs.safeAutocloseSweepPass(now)
			}
		}
	}()
}

// safeAutocloseSweepPass runs one pass and recovers a panic, as the safeTick
// lanes do, so one bad row cannot end the backstop for the controller's life.
// The ids that pass had already popped are lost; the log names the bug.
func (cs *controllerState) safeAutocloseSweepPass(now time.Time) (panicked bool) {
	defer func() {
		if r := recover(); r != nil {
			panicked = true
			log.Printf("autoclose-sweep: pass panicked: %v (type=%T)\n%s", r, r, debug.Stack())
		}
	}()
	cs.runAutocloseSweepPass(now)
	return false
}

// autocloseSweepActor stamps the completion facts the sweep records for a
// close no notification delivered.
const autocloseSweepActor = "autoclose-sweep"

type autocloseSweepResult struct {
	Ran, Refuted, Retried int
}

// runAutocloseSweepPass takes every live cache's census, then checks the ids
// due by now: a row read closed gets its completion fact (once) and autoclose,
// an open or gone row is dropped, and an unreadable one, or one whose
// autoclose did not finish, is retried next pass.
func (cs *controllerState) runAutocloseSweepPass(now time.Time) autocloseSweepResult {
	if cs.storesQuiescent() {
		// The city is suspended with nothing running: its stores are not
		// touched until it resumes.
		return autocloseSweepResult{}
	}
	sweep := cs.autocloseSweepOf()
	keep := map[*beads.CachingStore]struct{}{}
	for _, cache := range cs.sweepCaches() {
		keep[cache] = struct{}{}
		// Both tiers: a wisp molecule's steps are ephemeral rows.
		rows, ok := cache.CachedList(beads.ListQuery{AllowScan: true, TierMode: beads.TierBoth})
		if !ok {
			continue // not servable this pass; keep the last census
		}
		active := make(map[string]struct{}, len(rows))
		for _, b := range rows {
			active[b.ID] = struct{}{}
		}
		sweep.observe(cache, active, now)
	}
	sweep.retain(keep)

	var res autocloseSweepResult
	for _, id := range sweep.due(now) {
		if cs.storesQuiescent() {
			// The city went quiescent mid-pass: leave the rest for after
			// resume rather than restart the pairs it just retired.
			res.Retried++
			sweep.deferID(id, now)
			continue
		}
		cs.mu.RLock()
		stores := cs.beadEventStoresLocked(id)
		storeRef := cs.autocloseStoreRefLocked(id)
		suspendedStore := cs.anySuspendedRigStoreLocked(stores)
		cs.mu.RUnlock()
		if len(stores) == 0 {
			continue
		}
		if suspendedStore {
			// A suspended rig's store is not read: a read restarts its
			// retired proxy. The close is confirmed after the rig resumes.
			res.Retried++
			sweep.deferID(id, now.Add(autocloseSweepInterval))
			continue
		}
		store, live, err := liveReadOwner(stores, id)
		switch confirmInferredClose(live, err) {
		case closeConfirmed:
			cs.emitCompletedFact(live, autocloseSweepActor)
			if cs.beadCloseAutoclose(id, store, storeRef)() {
				res.Ran++
			} else {
				res.Retried++
				sweep.deferID(id, now.Add(autocloseSweepInterval))
			}
		case closeRefuted:
			res.Refuted++
		default:
			res.Retried++
			sweep.deferID(id, now.Add(autocloseSweepInterval))
		}
	}
	sweep.mu.Lock()
	dropped := sweep.dropped
	sweep.dropped = 0
	sweep.mu.Unlock()
	if res.Ran > 0 || dropped > 0 {
		log.Printf("autoclose-sweep: ran=%d refuted=%d retried=%d dropped=%d", res.Ran, res.Refuted, res.Retried, dropped)
	}
	return res
}

// anySuspendedRigStoreLocked reports whether any of stores is the store of a
// rig the city runtime last saw suspended. cs.mu must be held.
func (cs *controllerState) anySuspendedRigStoreLocked(stores []beads.Store) bool {
	for name, rigStore := range cs.beadStores {
		if !cs.rigSuspended(name) {
			continue
		}
		for _, store := range stores {
			if store == rigStore {
				return true
			}
		}
	}
	return false
}

// sweepCaches returns the distinct CachingStores the bead event watcher feeds.
func (cs *controllerState) sweepCaches() []*beads.CachingStore {
	cs.mu.RLock()
	stores := cs.beadEventStoresLocked("")
	for _, class := range infraMigrationClasses {
		if store, relocated := cs.storageRoutes.storeFor(coordclassFor(string(class))); relocated { // residency:allow — censuses the caches the bead event watcher feeds; resolves no bead
			stores = append(stores, store)
		}
	}
	cs.mu.RUnlock()
	seen := map[*beads.CachingStore]struct{}{}
	var out []*beads.CachingStore
	for _, store := range stores {
		store, _, _ = unwrapBeadPolicyStore(store)
		cache, ok := store.(*beads.CachingStore)
		if !ok || cache == nil {
			continue
		}
		if _, dup := seen[cache]; dup {
			continue
		}
		seen[cache] = struct{}{}
		out = append(out, cache)
	}
	return out
}
