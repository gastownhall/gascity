package api

import (
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/gastownhall/gascity/internal/beads"
)

// livenessReporter is implemented by stores that expose cache liveness.
// Only *beads.CachingStore currently implements it; plain BdStore / MemStore
// have no liveness concept and pass the gate unconditionally.
type livenessReporter interface {
	IsLive() bool
	Stats() beads.CacheStats
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
	return huma.Error503ServiceUnavailable("cache_not_live: supervisor cache is priming or reconciling; retry via fallback")
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
	age := time.Since(s.LastFreshAt).Seconds()
	if age < 0 {
		return 0
	}
	return age
}
