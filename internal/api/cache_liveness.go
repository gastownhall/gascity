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
// (the _cache_age_s field and the >30s stale-read banner) deterministically.
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

// cacheAgeSeconds returns the seconds since the store's bead cache last took
// in fresh data from its backing store: a full re-scan, or a write, event or
// row re-read it applied. It is the cache's age, not each row's, so it grows
// between re-scans and while the city is suspended (re-scans pause). It is 0
// when the store is nil, reports no cache stats, or has never been primed.
// Handlers whose body is read from the cache surface it through the
// X-GC-Cache-Age-S response header so CLI consumers can flag stale reads.
func cacheAgeSeconds(store beads.Store) float64 {
	sr, ok := store.(cacheStatsReporter)
	if !ok {
		return 0
	}
	s := sr.Stats()
	if s.LastFreshAt.IsZero() {
		return 0
	}
	age := livenessClock.Now().Sub(s.LastFreshAt).Seconds()
	if age < 0 {
		return 0
	}
	return age
}
