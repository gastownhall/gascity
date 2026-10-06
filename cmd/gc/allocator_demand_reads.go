package main

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

// The controller-demand read seam (CONTRACT C0.4 as amended by AM6). Legacy's
// demand collectors read through six calls: the live open List, the cached
// List, the live and the cached unfiltered Ready, the Ready limit, and the
// closed named-session index. They
// take them from a demandReads, threaded through readyDemandCache (its reads
// field) and as a parameter of collectOpenUnassignedRoutedWork, so the v2
// allocator can run the same collectors, with every partial rule they carry,
// and get parity with legacy by construction.
//
// legacyDemandReads is today's calls, unchanged: a nil reads is legacy.
// v2DemandReads does no remote I/O. An exact leg (its CachingStore's backing
// declares beads.CachedReadExact) is read through its cache, which refreshes
// dirty rows and falls back to the local backing when it cannot serve; any
// other leg's live reads come from the external-reads lane's recording
// (allocator_backstop_lane.go).
//
// Unwired in this slice: P3-5a's gather builds a v2DemandReads per pass, and
// P3-7 starts the external-reads lane.

var (
	errDemandRecordingMissing = errors.New("no external-reads recording for this leg")
	errDemandRecordingStale   = errors.New("external-reads recording is stale")
	errDemandLegUncached      = errors.New("leg has no cache to read")
)

// demandReads answers the controller-demand reads for one pass. Results are
// read-only: they may alias a memo or a recording.
type demandReads interface {
	// RawOpen is the open-status List the backing filters by raw status
	// (listOpenForControllerDemandLive).
	RawOpen(store beads.Store) ([]beads.Bead, error)
	// Cached is the cached-tier List (listBothTiersForControllerDemand).
	Cached(store beads.Store, query beads.ListQuery) ([]beads.Bead, error)
	// ReadyAll is the unfiltered live Ready, both tiers.
	ReadyAll(store beads.Store) ([]beads.Bead, error)
	// CachedReady is the unfiltered cached Ready, both tiers, that
	// controllerDemandReady tops a failed live read up from.
	CachedReady(store beads.Store) ([]beads.Bead, error)
	// ReadyLimit caps the assigned-work Ready read.
	ReadyLimit(cfg *config.City) int
	// ClosedNamedIndex is the city store's closed named-session index
	// (readyAssignedWorkAssignees). It reads closed history.
	ClosedNamedIndex(store beads.Store) (session.ClosedNamedSessionBeadIndex, error)
}

// demandReadsOrLegacy treats a nil reads as legacy.
func demandReadsOrLegacy(reads demandReads) demandReads {
	if reads == nil {
		return legacyDemandReads{}
	}
	return reads
}

// demandReads returns the reads this cache's pass uses. A nil cache is legacy.
func (c *readyDemandCache) demandReads() demandReads {
	if c == nil {
		return legacyDemandReads{}
	}
	return demandReadsOrLegacy(c.reads)
}

// newReadyDemandCacheWithReads is newReadyDemandCache for a pass that reads
// through reads.
func newReadyDemandCacheWithReads(reads demandReads) *readyDemandCache {
	c := newReadyDemandCache()
	c.reads = reads
	return c
}

// legacyDemandReads is the legacy reconciler's demand reads, call for call.
type legacyDemandReads struct{}

func (legacyDemandReads) RawOpen(store beads.Store) ([]beads.Bead, error) {
	return listOpenForControllerDemandLive(store)
}

func (legacyDemandReads) Cached(store beads.Store, query beads.ListQuery) ([]beads.Bead, error) {
	return listBothTiersForControllerDemand(store, query)
}

func (legacyDemandReads) ReadyAll(store beads.Store) ([]beads.Bead, error) {
	return beads.HandlesFor(store).Live.Ready(beads.ReadyQuery{TierMode: beads.TierBoth})
}

func (legacyDemandReads) CachedReady(store beads.Store) ([]beads.Bead, error) {
	return beads.HandlesFor(store).Cached.Ready(beads.ReadyQuery{TierMode: beads.TierBoth})
}

func (legacyDemandReads) ReadyLimit(cfg *config.City) int {
	return assignedWorkReadyLimit(cfg)
}

func (legacyDemandReads) ClosedNamedIndex(store beads.Store) (session.ClosedNamedSessionBeadIndex, error) {
	return session.BuildClosedNamedSessionBeadIndex(store)
}

// demandLegCache classifies a demand leg: its CachingStore, behind the
// bead-policy front door, and whether the leg is exact (the cache's backing
// declares beads.CachedReadExact). A leg with no CachingStore is not exact.
func demandLegCache(store beads.Store) (cache *beads.CachingStore, exact bool) {
	cache, ok := demandLabelKey(store).(*beads.CachingStore)
	if !ok || cache == nil {
		return nil, false
	}
	declarer, ok := cache.Backing().(beads.CachedReadExact)
	return cache, ok && declarer.CachedReadExact()
}

// v2DemandReads is the v2 allocator's demand reads for one pass, at one
// clock (now). Per read:
//
//   - RawOpen and ReadyAll on an exact leg: the cache's List{open} and its
//     strict ReadyContext, falling back to Ready, which reads the local
//     backing. On an exact backing the cached status is the raw status and
//     the cached ready projection is complete.
//   - RawOpen and ReadyAll on any other leg: the recording's copy of
//     legacy's own live read. A missing source, or one past its freshness,
//     is a PartialResultError with no rows: the collectors
//     mark the leg's templates partial (retain, block create) instead of
//     reading zero demand. A bd leg's cache folds blocked work into "open"
//     (EB-42o8), so it is never read for these.
//   - Cached: the cache's List on an exact leg; the strict cached List on
//     any other, with no live fallback.
//   - CachedReady: unavailable, so ReadyAll is the whole Ready answer.
//   - ReadyLimit: none. The assigned Ready set is not truncated by
//     max_wakes_per_tick (BEHAVIORS #31 F).
//   - ClosedNamedIndex: the recording's copy, on every leg. No cache holds
//     closed history, so the lane reads it live whether or not the leg is
//     exact. Missing or expired, it is the zero index with an error, which
//     readyAssignedWorkAssignees reads as no closed phantom (fail open, as
//     legacy does on a failed read).
//
// A strict read the cache refuses (a dirty row, a busy or unprimed cache) is
// a PartialResultError; an exact leg never reads strict alone. Reads are
// memoized per leg and shape for the pass and are safe for the collectors'
// concurrent legs.
type v2DemandReads struct {
	now time.Time
	rec *externalReadsRecording

	mu   sync.Mutex
	memo map[demandLegRead]*readyDemandEntry
}

// demandLegRead names one read of one leg: the store behind the policy front
// door, and the read's shape.
type demandLegRead struct {
	leg   beads.Store
	shape string
}

// newV2DemandReads returns the reads for one pass at now. rec is the
// external-reads lane's latest recording (nil before its first pass), each
// source served while fresh.
func newV2DemandReads(now time.Time, rec *externalReadsRecording) *v2DemandReads {
	return &v2DemandReads{
		now:  now,
		rec:  rec,
		memo: make(map[demandLegRead]*readyDemandEntry),
	}
}

func (r *v2DemandReads) RawOpen(store beads.Store) ([]beads.Bead, error) {
	cache, exact := demandLegCache(store)
	if !exact {
		return r.recorded(store, "raw_open", func(l legRecording) ([]beads.Bead, error) { return l.RawOpen, l.RawOpenErr })
	}
	return r.once(demandLegRead{leg: cache, shape: "raw_open"}, func() ([]beads.Bead, error) {
		return cache.List(beads.ListQuery{Status: "open", AllowScan: true, TierMode: beads.TierBoth})
	})
}

// Cached memoizes by status alone: the collectors' cached Lists differ only
// in their Status.
func (r *v2DemandReads) Cached(store beads.Store, query beads.ListQuery) ([]beads.Bead, error) {
	query.Live, query.TierMode = false, beads.TierBoth
	shape := "cached:" + query.Status
	cache, exact := demandLegCache(store)
	if cache == nil {
		return nil, &beads.PartialResultError{Op: "v2 demand " + shape, Err: errDemandLegUncached}
	}
	return r.once(demandLegRead{leg: cache, shape: shape}, func() ([]beads.Bead, error) {
		if exact {
			return cache.List(query)
		}
		if rows, ok := cache.CachedList(query); ok {
			return rows, nil
		}
		return nil, &beads.PartialResultError{Op: "v2 demand " + shape, Err: beads.ErrCacheUnavailable}
	})
}

func (r *v2DemandReads) ReadyAll(store beads.Store) ([]beads.Bead, error) {
	cache, exact := demandLegCache(store)
	if !exact {
		return r.recorded(store, "ready", func(l legRecording) ([]beads.Bead, error) { return l.ReadyAll, l.ReadyAllErr })
	}
	return r.once(demandLegRead{leg: cache, shape: "ready"}, func() ([]beads.Bead, error) {
		query := beads.ReadyQuery{TierMode: beads.TierBoth}
		// Ready's dirty-row overlay serves only the zero query, so a refused
		// strict read takes Ready's local backing read.
		if rows, err := cache.ReadyContext(context.Background(), query); err == nil {
			return rows, nil
		}
		return cache.Ready(query)
	})
}

func (r *v2DemandReads) CachedReady(beads.Store) ([]beads.Bead, error) {
	return nil, beads.ErrCacheUnavailable
}

func (r *v2DemandReads) ReadyLimit(*config.City) int { return 0 }

func (r *v2DemandReads) ClosedNamedIndex(store beads.Store) (session.ClosedNamedSessionBeadIndex, error) {
	const shape = "closed_named_index"
	s, err := r.rec.lookup(sourceClosedNamed, store, r.now)
	if err != nil {
		return session.ClosedNamedSessionBeadIndex{}, &beads.PartialResultError{Op: "v2 demand " + shape, Err: err}
	}
	return s.ClosedNamed, s.Err
}

// recorded serves a lane-fed leg's live read from the external-reads
// recording. A source that failed whole (a timeout) fails both reads.
func (r *v2DemandReads) recorded(store beads.Store, shape string, pick func(legRecording) ([]beads.Bead, error)) ([]beads.Bead, error) {
	s, err := r.rec.lookup(sourceDemand, store, r.now)
	if err != nil {
		return nil, &beads.PartialResultError{Op: "v2 demand " + shape, Err: err}
	}
	if s.Err != nil {
		return nil, s.Err
	}
	return pick(s.Leg)
}

// once runs a leg's read once per pass.
func (r *v2DemandReads) once(key demandLegRead, read func() ([]beads.Bead, error)) ([]beads.Bead, error) {
	r.mu.Lock()
	e := r.memo[key]
	if e == nil {
		e = &readyDemandEntry{}
		r.memo[key] = e
	}
	r.mu.Unlock()
	e.once.Do(func() { e.rows, e.err = read() })
	return e.rows, e.err
}

// projectControlDispatcherRoutes is the demand half of
// repairControlDispatcherRoutesForStoreScope with no write budget: what
// legacy's in-tick repair leaves in the rows that openControlDispatcherDemand
// counts, minus the writes. It drops gc.routed_to from a control row whose
// owning scope has no dispatcher (a scope gap) and from one whose stored route
// differs from its scope's dispatcher (a repair the external-reads lane has not
// persisted yet); a row needing only its fallback marker cleared keeps its
// route. The writes are the lane's (runBackstopDemandRepairs), so a row the
// lane repaired counts on the first pass after its recording shows the new
// route.
//
// rows and refs are index-aligned, as the routed collection returns them. The
// projection is copy-on-write: rows may alias a recording, so they are never
// edited; the result shares rows with the input except the ones it changed.
// It returns the scope gaps the rows show.
func projectControlDispatcherRoutes(cfg *config.City, rows []beads.Bead, refs []string) ([]beads.Bead, []ControlDispatcherScopeGap) {
	if cfg == nil || len(rows) == 0 {
		return rows, nil
	}
	// With no store the repair defers every route it would rewrite.
	repair := newControlDispatcherRouteRepair(cfg, nil)
	// A misaligned input suppresses every control route and reports no gap,
	// as legacy does.
	aligned := len(rows) == len(refs)
	out, copied := rows, false
	for i := range rows {
		if !beadmeta.IsControlKind(strings.TrimSpace(rows[i].Metadata[beadmeta.KindMetadataKey])) {
			continue
		}
		b := rows[i]
		b.Metadata = maps.Clone(b.Metadata)
		if aligned {
			repair.repairBead(&b, nil, refs[i])
		} else {
			delete(b.Metadata, beadmeta.RoutedToMetadataKey)
		}
		if maps.Equal(b.Metadata, rows[i].Metadata) {
			continue
		}
		if !copied {
			out, copied = slices.Clone(rows), true
		}
		out[i] = b
	}
	if !aligned {
		return out, nil
	}
	return out, repair.scopeGaps
}
