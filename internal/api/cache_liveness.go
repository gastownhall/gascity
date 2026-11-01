package api

import (
	"github.com/gastownhall/gascity/internal/api/apierr"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
)

// cacheStatsReporter is a store that reports its bead cache's statistics:
// *beads.CachingStore, and the controller's bead-policy layer over a city or
// rig cache, which forwards the cache's Stats. Plain BdStore / MemStore report
// no age.
type cacheStatsReporter interface {
	Stats() beads.CacheStats
}

// livenessReporter is a store that reports whether its bead cache is live.
// Only a bare *beads.CachingStore does: the bead-policy layer the controller
// serves city and rig stores through forwards Stats but not IsLive (see
// cacheLiveOr503).
type livenessReporter interface {
	IsLive() bool
}

// livenessClock is the clock cacheAgeSeconds reads to compute cache age. It is
// clock.Real in production; SetLivenessClockForTest swaps it so the
// CLI-unification characterization harness can freeze the Tier-B cache-age lane
// (the _cache_age_s field and the CLI's cache-age footer) deterministically.
// Process-global: bracket clock-sensitive lanes serially, never concurrently.
var livenessClock clock.Clock = clock.Real{}

// SetLivenessClockForTest overrides the clock cacheAgeSeconds uses and returns a
// restore func. Test/harness only — production never mutates it.
func SetLivenessClockForTest(c clock.Clock) (restore func()) {
	prev := livenessClock
	livenessClock = c
	return func() { livenessClock = prev }
}

// cacheLiveOr503 returns a 503 typed error when the given store reports that
// its bead cache is not live (priming, reconciling or degraded). Stores that
// do not report liveness pass through.
//
// In practice the gate fires only on a bare CachingStore, which today means a
// relocated class-binding cache. City and rig stores reach the API through the
// controller's bead-policy layer, which deliberately does not forward IsLive:
// those caches are not live from every controller start and store rebuild
// until the full prime lands, remote CLI targets never fall back on this 503,
// and the dashboard does not retry it. Correctness does not need the gate
// there, because a CachingStore that cannot serve a query from cache answers
// it from its backing store.
//
// The error's detail string is prefixed with "cache_not_live:" so
// internal/api.Client can classify the 503 into *cacheNotLiveError, which
// api.ShouldFallback reports as fallbackable.
func cacheLiveOr503(store beads.Store) error {
	lr, ok := store.(livenessReporter)
	if !ok {
		return nil
	}
	if lr.IsLive() {
		return nil
	}
	return apierr.StoreUnavailable.Msg("cache_not_live: supervisor cache is priming or reconciling; retry via fallback")
}

// CacheRefreshState names what bounds a read's X-GC-Cache-Age-S: what the
// server's bead cache is doing about taking in fresh data.
type CacheRefreshState string

const (
	// CacheRefreshScheduled means the cache re-scans its store every
	// X-GC-Cache-Refresh-Interval-S seconds.
	CacheRefreshScheduled CacheRefreshState = "scheduled"
	// CacheRefreshPaused means the server has paused the cache's re-scans while the
	// city is suspended with no session running.
	CacheRefreshPaused CacheRefreshState = "paused"
	// CacheRefreshFailing means the cache's last re-scan of its store failed; it
	// retries with backoff.
	CacheRefreshFailing CacheRefreshState = "failing"
	// CacheRefreshRebuilding means the body is a response the server saved
	// X-GC-Cache-Age-S seconds ago, served while it builds a fresh one.
	CacheRefreshRebuilding CacheRefreshState = "rebuilding"
)

// CacheRefresh is how the server's bead cache takes in fresh data: its
// re-scan cadence and what bounds the reported age. Output types whose body is
// read from the bead cache embed it beside their X-GC-Cache-Age-S field. The
// zero value means the response reported neither header (a body not read from
// a primed cache, or an older server).
//
// X-GC-Cache-Age-S stays a field of each output type rather than moving in
// here: the Go compiler hashes generic shape names longer than 500 bytes, and
// the output types' long header doc tags keep the shape names of their many
// generic instantiations (huma.Register, bindCity) over that bound. Moving
// the age's doc tag into an embedded struct shortens them below it and spells
// out ~600 symbol names in full, ~370 KB of binary.
type CacheRefresh struct {
	IntervalS float64           `header:"X-GC-Cache-Refresh-Interval-S" doc:"Seconds between the scheduled re-scans of its store by the bead cache X-GC-Cache-Age-S describes (its current adaptive cadence). 0 when the body was not read from a primed cache."`
	State     CacheRefreshState `header:"X-GC-Cache-Refresh" enum:"scheduled,paused,failing,rebuilding" doc:"What bounds X-GC-Cache-Age-S. scheduled: the cache re-scans its store every X-GC-Cache-Refresh-Interval-S seconds. paused: the server has paused those re-scans while the city is suspended with no session running. failing: the cache's last re-scan of its store failed; it retries with backoff. rebuilding: the body is a response the server saved X-GC-Cache-Age-S seconds ago, served while it builds a fresh one. Omitted when the body was not read from a primed cache, except rebuilding: a saved /status response reports rebuilding whatever it was built from, and its X-GC-Cache-Refresh-Interval-S is then 0."`
}

// cacheFreshness is the bead-cache freshness a read reports: ageS in its
// X-GC-Cache-Age-S header, refresh in its CacheRefresh headers.
type cacheFreshness struct {
	ageS    float64
	refresh CacheRefresh
}

// cacheAgeSeconds reports the store's bead-cache freshness: the seconds since
// the cache last took in fresh data from its backing store (a full re-scan, or
// a write, event or row re-read it applied), the re-scan interval its loop is
// using, and whether those re-scans are scheduled, paused or failing. The age
// is the cache's, not each row's, so it grows between re-scans and while the
// city is suspended (re-scans pause). It is the zero value when the store is
// nil, reports no cache stats, or has never been primed. It reads values only:
// whether the age is too old is the client's call.
//
// The storeclass baseline keys this function's raw-store debt (its beads.Store
// parameter and Stats assertion) by its name: under another name that debt is
// a new baseline row, a raise only the typed-store coordinator approves. Keep
// the name until a typed handle retires the debt.
func cacheAgeSeconds(store beads.Store) cacheFreshness {
	sr, ok := store.(cacheStatsReporter)
	if !ok {
		return cacheFreshness{}
	}
	s := sr.Stats()
	if s.LastFreshAt.IsZero() {
		return cacheFreshness{}
	}
	age := max(livenessClock.Now().Sub(s.LastFreshAt).Seconds(), 0)
	state := CacheRefreshScheduled
	switch {
	case s.ReconcilePaused:
		state = CacheRefreshPaused
	case s.SyncFailures > 0:
		state = CacheRefreshFailing
	}
	return cacheFreshness{
		ageS: age,
		refresh: CacheRefresh{
			// The loop schedules re-scans at the Backstop-scaled interval.
			IntervalS: clock.Backstop(s.CurrentReconcileInterval).Seconds(),
			State:     state,
		},
	}
}
