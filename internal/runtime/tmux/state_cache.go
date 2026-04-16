package tmux

import (
	"context"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// defaultCacheTTL is the default time-to-live for cached session state.
const defaultCacheTTL = 2 * time.Second

// defaultStaleTTL is the maximum age of cached data before it is considered
// too stale to trust. After this duration, IsRunning returns false for all
// sessions and logs a degraded warning.
const defaultStaleTTL = 30 * time.Second

// fetchTimeout is the hard timeout for a single FetchRunning call.
const fetchTimeout = 3 * time.Second

// StateFetcher abstracts tmux subprocess calls for testability.
type StateFetcher interface {
	// FetchRunning returns the set of session names with live (non-dead) panes.
	// Sessions with remain-on-exit corpses (pane_dead=1) are excluded.
	FetchRunning(ctx context.Context) (map[string]bool, error)
}

// StateCache caches the set of running tmux sessions to avoid
// spawning N subprocess calls per status check. Concurrent callers
// are coalesced via singleflight so at most one tmux list-sessions
// subprocess runs at a time.
type StateCache struct {
	mu        sync.RWMutex
	sessions  map[string]bool
	fetchedAt time.Time
	lastError error
	dirty     bool // set by Invalidate(); cleared on successful refresh
	ttl       time.Duration
	staleTTL  time.Duration
	sf        singleflight.Group
	fetcher   StateFetcher
}

// NewStateCache creates a new cache with the given fetcher and TTL.
// staleTTL defaults to 30s.
func NewStateCache(fetcher StateFetcher, ttl time.Duration) *StateCache {
	return &StateCache{
		fetcher:  fetcher,
		ttl:      ttl,
		staleTTL: defaultStaleTTL,
	}
}

// IsRunning reports whether the named session exists in the cached set.
// If the cache is stale, a refresh is triggered (coalesced via singleflight).
// On refresh failure, the last-known-good cache is preserved up to staleTTL.
func (c *StateCache) IsRunning(name string) bool {
	c.mu.RLock()
	sessions := c.sessions
	fetchedAt := c.fetchedAt
	dirty := c.dirty
	c.mu.RUnlock()

	// Cache hit: fresh data, not invalidated.
	if sessions != nil && !fetchedAt.IsZero() && !dirty && time.Since(fetchedAt) < c.ttl {
		return sessions[name]
	}

	// Stale, empty, or dirty — trigger refresh.
	// When dirty, forget any in-flight singleflight so we get a fresh fetch
	// instead of coalescing with a pre-invalidation call.
	if dirty {
		c.sf.Forget("refresh")
	}
	c.refresh()

	// Read the (potentially updated) cache.
	c.mu.RLock()
	sessions = c.sessions
	fetchedAt = c.fetchedAt
	c.mu.RUnlock()

	// If the cache is older than staleTTL, report all sessions as not running.
	// Note: fetchedAt is preserved on failure (never zeroed), so this only
	// triggers after staleTTL of real wall-clock time since last success.
	if sessions == nil || fetchedAt.IsZero() || time.Since(fetchedAt) > c.staleTTL {
		return false
	}
	return sessions[name]
}

// Invalidate marks the cache as dirty, forcing the next IsRunning call
// to trigger a refresh. The session data and fetchedAt are preserved as
// last-known-good until the refresh completes — even if the refresh fails.
func (c *StateCache) Invalidate() {
	c.mu.Lock()
	c.dirty = true
	c.mu.Unlock()
}

// EvictSession removes a specific session from the cache and marks it dirty.
// Used by Stop to immediately reflect the killed session without waiting for
// the next refresh cycle (which may race with singleflight coalescing).
func (c *StateCache) EvictSession(name string) {
	c.mu.Lock()
	delete(c.sessions, name)
	c.dirty = true
	c.mu.Unlock()
}

// refresh executes a single coalesced fetch. If the fetch fails, the
// last-known-good cache is preserved and the error is logged.
func (c *StateCache) refresh() {
	_, _, _ = c.sf.Do("refresh", func() (interface{}, error) {
		ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
		defer cancel()

		start := time.Now()
		sessions, err := c.fetcher.FetchRunning(ctx)
		elapsed := time.Since(start)

		if err != nil {
			log.Printf("tmux state cache: refresh failed in %v: %v", elapsed, err)
			c.mu.Lock()
			c.lastError = err
			c.mu.Unlock()
			// Preserve last-known-good — do NOT update fetchedAt or sessions.
			return nil, err
		}

		// Successful refresh is noisy on the session loop; opt-in via env var
		// keeps it available for diagnostics without polluting normal CLI use.
		if os.Getenv("GC_LOG_TMUX_CACHE") == "true" {
			log.Printf("tmux state cache: refreshed %d sessions in %v", len(sessions), elapsed)
		}

		c.mu.Lock()
		c.sessions = sessions
		c.fetchedAt = time.Now()
		c.lastError = nil
		c.dirty = false
		c.mu.Unlock()
		return nil, nil
	})
}

// tmuxFetcher implements StateFetcher using a real Tmux instance.
type tmuxFetcher struct {
	tm *Tmux
}

// FetchRunning runs `tmux list-panes -a -F '#{session_name}\t#{pane_dead}'`
// and returns a map of session names that have at least one live pane.
// Sessions where remain-on-exit has kept a dead pane (pane_dead=1) are
// excluded — they represent exited processes, not running ones.
func (f *tmuxFetcher) FetchRunning(ctx context.Context) (map[string]bool, error) {
	out, err := f.tm.runCtx(ctx, "list-panes", "-a", "-F", "#{session_name}\t#{pane_dead}")
	if err != nil {
		if isNoServerError(err) {
			return map[string]bool{}, nil // No server = no sessions
		}
		return nil, err
	}
	if out == "" {
		return map[string]bool{}, nil
	}

	// Track which sessions have dead panes vs live panes.
	// A session is "running" if it has at least one live pane.
	dead := make(map[string]bool)
	alive := make(map[string]bool)
	for _, line := range strings.Split(out, "\n") {
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) != 2 || parts[0] == "" {
			continue
		}
		name := parts[0]
		if parts[1] == "1" {
			dead[name] = true
		} else {
			alive[name] = true
		}
	}

	// alive wins over dead — if any pane is alive, session is running.
	sessions := make(map[string]bool, len(alive))
	for name := range alive {
		sessions[name] = true
	}
	return sessions, nil
}

// isNoServerError checks if the error is a "no server running" error.
func isNoServerError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "no server running")
}

// cacheTTLFromEnv reads GC_TMUX_CACHE_TTL from the environment and parses
// it as a duration. Returns defaultCacheTTL if the env var is unset, empty,
// or cannot be parsed. Accepts:
//   - integer: interpreted as milliseconds (e.g., "2000" = 2s)
//   - Go duration string: (e.g., "2s", "500ms")
func cacheTTLFromEnv() time.Duration {
	v := os.Getenv("GC_TMUX_CACHE_TTL")
	if v == "" {
		return defaultCacheTTL
	}

	// Try Go duration string first (e.g., "2s", "500ms").
	if d, err := time.ParseDuration(v); err == nil {
		return d
	}

	// Try integer milliseconds (e.g., "2000").
	if strings.TrimSpace(v) == v {
		if ms, err := strconv.Atoi(v); err == nil && ms > 0 {
			return time.Duration(ms) * time.Millisecond
		}
	}

	log.Printf("tmux state cache: invalid GC_TMUX_CACHE_TTL=%q, using default %v", v, defaultCacheTTL)
	return defaultCacheTTL
}
