package dashboardbff

import (
	"sync"
	"time"
)

// unknownRunWarmingGrace is how long run point-read endpoints keep answering
// the retryable 503 "run view is warming" — instead of 404 — for a runId the
// WARM projection does not know, measured from the FIRST request for that
// runId. A run slung from the CLI is invisible to this projection until the
// controller's cache-reconcile emits its bead events onto the city's event
// log, a 30-120s cadence, so the window must exceed that cadence for a
// just-slung run's dashboard deep link to survive the gap. The contract is
// server-held: the server keeps answering "warming" (with reason unknown_run)
// for the whole window, the graced response's Retry-After header tells
// clients how often to poll, and the SPA's run-detail loader polls within its
// own retry budget (being extended in a sibling change) while treating a 404
// as terminal. Once the window expires the endpoints restore the plain 404.
const unknownRunWarmingGrace = 180 * time.Second

// unknownRunGraceMaxIDLen bounds the runId length inGrace will track. The
// entry cap (unknownRunGraceCap) bounds ENTRIES, not bytes: the map stores
// each runId verbatim, and the id arrives straight from an HTTP request path,
// so without a length bound a scanner
// spraying maximum-length URIs could pin ~cap x URI-length bytes of
// attacker-chosen data per city (~1 GiB with 1 MiB URIs). Real run roots are
// short bead IDs (tens of bytes), so 128 is generous headroom, never a
// functional limit.
const unknownRunGraceMaxIDLen = 128

// unknownRunGraceCap bounds how many unknown runIds one city's tracker holds
// at once, so a scanner spraying random runIds cannot grow the first-seen map
// without bound. When the map is full of live windows, a NEW unknown runId is
// simply not tracked (it degrades to today's immediate 404) rather than
// evicting a live window out from under an in-flight deep link.
const unknownRunGraceCap = 1024

// unknownRunGrace tracks the first time each truly-unknown runId was requested
// so run point-read endpoints can serve the retryable warming 503 for a grace
// window before falling back to the terminal 404. It is concurrency-safe (the
// BFF serves concurrent requests) and bounded (unknownRunGraceCap). The clock
// is injectable for tests.
type unknownRunGrace struct {
	window   time.Duration
	capacity int
	now      func() time.Time

	mu        sync.Mutex
	firstSeen map[string]time.Time
}

// newUnknownRunGrace builds a tracker with the production window, cap, and
// wall clock.
func newUnknownRunGrace() *unknownRunGrace {
	return &unknownRunGrace{
		window:    unknownRunWarmingGrace,
		capacity:  unknownRunGraceCap,
		now:       time.Now,
		firstSeen: make(map[string]time.Time),
	}
}

// inGrace reports whether runID is inside its warming-grace window, recording
// the first sighting when the runId is new. An expired entry is left in place
// (pruned lazily when the map needs room) so repeat polls for a dead runId
// keep getting the 404 instead of restarting the window.
func (g *unknownRunGrace) inGrace(runID string) bool {
	// Refuse to track oversized runIds at all: an id longer than any real run
	// root is never a legitimate just-slung run, and inserting it verbatim
	// would let a request flood fill the map with megabytes of
	// attacker-chosen bytes per entry (see unknownRunGraceMaxIDLen). It
	// degrades to the immediate 404.
	if len(runID) > unknownRunGraceMaxIDLen {
		return false
	}
	now := g.now()
	g.mu.Lock()
	defer g.mu.Unlock()
	if first, ok := g.firstSeen[runID]; ok {
		return now.Sub(first) < g.window
	}
	if len(g.firstSeen) >= g.capacity {
		g.pruneExpiredLocked(now)
	}
	if len(g.firstSeen) >= g.capacity {
		// Still full of live windows: do not track — the new unknown runId
		// degrades to today's immediate 404 rather than evicting a live window.
		return false
	}
	g.firstSeen[runID] = now
	return true
}

// forget drops runID's first-seen marker. Called when the projection resolves
// the run (it became known), so a known runId never lingers in the map.
// Idempotent and cheap for runIds that were never tracked.
func (g *unknownRunGrace) forget(runID string) {
	g.mu.Lock()
	delete(g.firstSeen, runID)
	g.mu.Unlock()
}

// pruneExpiredLocked removes every entry whose window has expired. The caller
// holds g.mu.
func (g *unknownRunGrace) pruneExpiredLocked(now time.Time) {
	for id, first := range g.firstSeen {
		if now.Sub(first) >= g.window {
			delete(g.firstSeen, id)
		}
	}
}
