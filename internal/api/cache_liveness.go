package api

import (
	"github.com/gastownhall/gascity/internal/api/apierr"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
)

// livenessReporter is implemented by stores that expose cache liveness.
// Only *beads.CachingStore currently implements it; plain BdStore / MemStore
// have no liveness concept and pass the gate unconditionally.
type livenessReporter interface {
	IsLive() bool
	Stats() beads.CacheStats
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

// cacheLiveOr503 returns a 503 typed error when the given store is a
// CachingStore that has not yet reached the live state. Read handlers call
// this at entry so the CLI receives a fallbackable signal instead of empty
// or partial data while the cache is priming or reconciling. Non-caching
// stores pass through (there's no live/not-live concept to gate).
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

// cacheAgeSeconds returns the age in seconds of the store's latest fresh
// observation, or 0 when the store is nil, non-caching, or has never been
// primed. Handlers surface this value through the X-GC-Cache-Age-S
// response header so CLI consumers can flag stale reads.
func cacheAgeSeconds(store beads.Store) float64 {
	lr, ok := store.(livenessReporter)
	if !ok {
		return 0
	}
	s := lr.Stats()
	if s.LastFreshAt.IsZero() {
		return 0
	}
	age := livenessClock.Now().Sub(s.LastFreshAt).Seconds()
	if age < 0 {
		return 0
	}
	return age
}
