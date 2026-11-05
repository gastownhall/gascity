package main

import (
	"fmt"
	"io"
	"math"

	"github.com/gastownhall/gascity/internal/api"
)

// cacheAgeFooterIntervals is how many of its own re-scan intervals the
// server's bead cache may age before a read prints the cache-age footer. A
// cache that re-scans on schedule peaks at one interval plus one scan, so
// twice the interval is past anything a healthy cache reaches.
const cacheAgeFooterIntervals = 2

// unreportedCacheRefreshIntervalS is the interval assumed when the server
// reports none: the slowest re-scan interval a supervisor's bead cache has
// used. Supervisors older than X-GC-Cache-Refresh-Interval-S report none, so
// their reads print the footer only past twice this.
const unreportedCacheRefreshIntervalS = 120

// cacheAgeFooter returns the one-line diagnostic a read prints when the
// server's bead cache is more than cacheAgeFooterIntervals of its reported
// re-scan intervals old, naming what the server reports bounds the age; ""
// otherwise. An age of 0 (the local route, an uncached body, a server that
// reports no age) and NaN never print a footer.
func cacheAgeFooter(ageS float64, r api.CacheRefresh) string {
	interval := r.IntervalS
	if !(interval > 0) {
		interval = unreportedCacheRefreshIntervalS
	}
	limit := cacheAgeFooterIntervals * interval
	if !(ageS > limit) {
		return ""
	}
	var cause string
	switch {
	case r.State == api.CacheRefreshScheduled && r.IntervalS > 0:
		cause = fmt.Sprintf("more than twice the bead cache's %.0fs re-scan interval: its re-scans of the store are not landing on schedule", r.IntervalS)
	case r.State == api.CacheRefreshPaused:
		cause = "the server has paused its bead cache's re-scans while the city is suspended; changes made to the store outside the server since then are not shown"
	case r.State == api.CacheRefreshFailing:
		cause = "the server's bead cache is failing to re-scan its store; the server's log has the error"
	case r.State == api.CacheRefreshRebuilding:
		cause = "the server answered with a response it saved that long ago and is building a fresh one; run the command again for a current view"
	default:
		cause = fmt.Sprintf("this read is over %.0fs old, and the server does not report why", limit)
	}
	// Round the shown age up so it always reads as past the limit it crossed.
	return fmt.Sprintf("(cache age: %.0fs — %s)", math.Ceil(ageS), cause)
}

// writeCacheAgeFooter writes cacheAgeFooter's line to stderr, or nothing when
// it is "". The footer is a diagnostic about the read, so it stays off stdout,
// which carries the result scripts parse.
func writeCacheAgeFooter(stderr io.Writer, ageS float64, r api.CacheRefresh) {
	if line := cacheAgeFooter(ageS, r); line != "" {
		fmt.Fprintln(stderr, line) //nolint:errcheck // best-effort diagnostic
	}
}
