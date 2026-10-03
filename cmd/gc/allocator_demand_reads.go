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
// v2DemandReads does no backing I/O. An exact leg (its CachingStore's backing
// declares beads.CachedReadExact) is read from the cache with strict reads and
// a last-good fallback; any other leg's live reads come from the backstop
// lane's recording (allocator_backstop_lane.go).
//
// Unwired in this slice: P3-5a's gather builds a v2DemandReads per pass, and
// P3-7 starts the backstop lane and owns the demandLastGood.

// cacheLagBound bounds how long an exact leg's read may be served from its
// last good answer (P3 spec §4.2 rule 4). An older answer is refused, so the
// leg reads partial. It is the one last-good bound of the allocator's reads:
// the session census and its episode reader use it too, and it caps the
// pass-time term of a backstop recording's expiry.
const cacheLagBound = 60 * time.Second

var (
	errDemandRecordingMissing = errors.New("no backstop recording for this leg")
	errDemandRecordingStale   = errors.New("backstop recording is stale")
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
//   - RawOpen and ReadyAll on an exact leg: the strict cached List{open} and
//     ReadyContext. On an exact backing the cached status is the raw status
//     and the cached ready projection is complete.
//   - RawOpen and ReadyAll on any other leg: the recording's copy of
//     legacy's own live read. A missing recording, or one past its Expires,
//     is a PartialResultError with no rows: the collectors
//     mark the leg's templates partial (retain, block create) instead of
//     reading zero demand. A bd leg's cache folds blocked work into "open"
//     (EB-42o8), so it is never read for these.
//   - Cached: the strict cached List on every leg, as legacy reads it from
//     the cache, but with no live fallback.
//   - CachedReady: unavailable, so ReadyAll is the whole Ready answer.
//   - ReadyLimit: none. The assigned Ready set is not truncated by
//     max_wakes_per_tick (BEHAVIORS #31 F).
//   - ClosedNamedIndex: the recording's copy, on every leg. No cache holds
//     closed history, so the lane reads it live whether or not the leg is
//     exact. Missing or expired, it is the zero index with an error, which
//     readyAssignedWorkAssignees reads as no closed phantom (fail open, as
//     legacy does on a failed read).
//
// A strict read the cache refuses (a dirty row, a busy or unprimed cache)
// serves the leg's last good answer while it is at most cacheLagBound old,
// and records the fallback; past that it is a
// PartialResultError. Reads are memoized per leg and shape for the pass and
// are safe for the collectors' concurrent legs.
type v2DemandReads struct {
	now      time.Time
	rec      *backstopRecording
	lastGood *demandLastGood

	mu        sync.Mutex
	memo      map[demandLegRead]*readyDemandEntry
	fallbacks []demandReadFallback
}

// demandLegRead names one read of one leg: the store behind the policy front
// door, and the read's shape.
type demandLegRead struct {
	leg   beads.Store
	shape string
}

// demandReadFallback records a read served from its last good answer.
type demandReadFallback struct {
	Shape string
	Age   time.Duration
}

// newV2DemandReads returns the reads for one pass at now. rec is the backstop
// lane's latest recording (nil before its first pass), served until its
// Expires. lastGood carries exact legs' answers across passes; nil keeps
// none, so every refused strict read is partial.
func newV2DemandReads(now time.Time, rec *backstopRecording, lastGood *demandLastGood) *v2DemandReads {
	return &v2DemandReads{
		now:      now,
		rec:      rec,
		lastGood: lastGood,
		memo:     make(map[demandLegRead]*readyDemandEntry),
	}
}

func (r *v2DemandReads) RawOpen(store beads.Store) ([]beads.Bead, error) {
	cache, exact := demandLegCache(store)
	if !exact {
		return r.recorded(store, "raw_open", func(l legRecording) ([]beads.Bead, error) { return l.RawOpen, l.RawOpenErr })
	}
	query := beads.ListQuery{Status: "open", AllowScan: true, TierMode: beads.TierBoth}
	return r.strict(demandLegRead{leg: cache, shape: "raw_open"}, func() ([]beads.Bead, bool) {
		return cache.CachedList(query)
	})
}

// Cached memoizes by status alone: the collectors' cached Lists differ only
// in their Status.
func (r *v2DemandReads) Cached(store beads.Store, query beads.ListQuery) ([]beads.Bead, error) {
	query.Live, query.TierMode = false, beads.TierBoth
	shape := "cached:" + query.Status
	cache, _ := demandLegCache(store)
	if cache == nil {
		return nil, &beads.PartialResultError{Op: "v2 demand " + shape, Err: errDemandLegUncached}
	}
	return r.strict(demandLegRead{leg: cache, shape: shape}, func() ([]beads.Bead, bool) {
		return cache.CachedList(query)
	})
}

func (r *v2DemandReads) ReadyAll(store beads.Store) ([]beads.Bead, error) {
	cache, exact := demandLegCache(store)
	if !exact {
		return r.recorded(store, "ready", func(l legRecording) ([]beads.Bead, error) { return l.ReadyAll, l.ReadyAllErr })
	}
	return r.strict(demandLegRead{leg: cache, shape: "ready"}, func() ([]beads.Bead, bool) {
		rows, err := cache.ReadyContext(context.Background(), beads.ReadyQuery{TierMode: beads.TierBoth})
		return rows, err == nil
	})
}

func (r *v2DemandReads) CachedReady(beads.Store) ([]beads.Bead, error) {
	return nil, beads.ErrCacheUnavailable
}

func (r *v2DemandReads) ReadyLimit(*config.City) int { return 0 }

func (r *v2DemandReads) ClosedNamedIndex(store beads.Store) (session.ClosedNamedSessionBeadIndex, error) {
	const shape = "closed_named_index"
	l, ok := r.rec.closedNamed(store)
	switch {
	case !ok:
		return session.ClosedNamedSessionBeadIndex{}, &beads.PartialResultError{Op: "v2 demand " + shape, Err: errDemandRecordingMissing}
	case !r.rec.fresh(r.now):
		return session.ClosedNamedSessionBeadIndex{}, &beads.PartialResultError{Op: "v2 demand " + shape, Err: errDemandRecordingStale}
	}
	return l.Index, l.Err
}

// Fallbacks returns the reads this pass served from their last good answer.
func (r *v2DemandReads) Fallbacks() []demandReadFallback {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]demandReadFallback(nil), r.fallbacks...)
}

// recorded serves a non-exact leg's live read from the backstop recording.
func (r *v2DemandReads) recorded(store beads.Store, shape string, pick func(legRecording) ([]beads.Bead, error)) ([]beads.Bead, error) {
	leg, ok := r.rec.leg(store)
	switch {
	case !ok:
		return nil, &beads.PartialResultError{Op: "v2 demand " + shape, Err: errDemandRecordingMissing}
	case !r.rec.fresh(r.now):
		return nil, &beads.PartialResultError{Op: "v2 demand " + shape, Err: errDemandRecordingStale}
	}
	return pick(leg)
}

// strict runs a cache-only read once per pass, falling back to the last good
// answer when the cache refuses.
func (r *v2DemandReads) strict(key demandLegRead, read func() ([]beads.Bead, bool)) ([]beads.Bead, error) {
	r.mu.Lock()
	e := r.memo[key]
	if e == nil {
		e = &readyDemandEntry{}
		r.memo[key] = e
	}
	r.mu.Unlock()
	e.once.Do(func() {
		if rows, ok := read(); ok {
			r.lastGood.put(key, rows, r.now)
			e.rows = rows
			return
		}
		if rows, at, ok := r.lastGood.get(key); ok && r.now.Sub(at) <= cacheLagBound {
			r.mu.Lock()
			r.fallbacks = append(r.fallbacks, demandReadFallback{Shape: key.shape, Age: r.now.Sub(at)})
			r.mu.Unlock()
			e.rows = rows
			return
		}
		e.err = &beads.PartialResultError{Op: "v2 demand " + key.shape, Err: beads.ErrCacheUnavailable}
	})
	return e.rows, e.err
}

// demandLastGood keeps each exact leg's last good read across passes. The
// allocator owns one; each pass's v2DemandReads reads and refreshes it. A nil
// demandLastGood keeps nothing.
type demandLastGood struct {
	mu      sync.Mutex
	answers map[demandLegRead]demandLastGoodAnswer
}

type demandLastGoodAnswer struct {
	rows []beads.Bead
	at   time.Time
}

func newDemandLastGood() *demandLastGood {
	return &demandLastGood{answers: make(map[demandLegRead]demandLastGoodAnswer)}
}

// put records a good answer and drops answers too old to serve, so a leg a
// reload replaced does not keep its rows alive.
func (g *demandLastGood) put(key demandLegRead, rows []beads.Bead, at time.Time) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	for k, a := range g.answers {
		if at.Sub(a.at) > cacheLagBound {
			delete(g.answers, k)
		}
	}
	g.answers[key] = demandLastGoodAnswer{rows: rows, at: at}
}

func (g *demandLastGood) get(key demandLegRead) ([]beads.Bead, time.Time, bool) {
	if g == nil {
		return nil, time.Time{}, false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	a, ok := g.answers[key]
	return a.rows, a.at, ok
}

// projectControlDispatcherRoutes is the demand half of
// repairControlDispatcherRoutesForStoreScope with no write budget: what
// legacy's in-tick repair leaves in the rows that openControlDispatcherDemand
// counts, minus the writes. It drops gc.routed_to from a control row whose
// owning scope has no dispatcher (a scope gap) and from one whose stored route
// differs from its scope's dispatcher (a repair the backstop lane has not
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
