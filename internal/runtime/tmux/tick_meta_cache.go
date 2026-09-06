package tmux

import (
	"errors"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// metaCacheTTL bounds how stale a memoized session environment may be when
// GetMeta is served outside a reconcile tick. Inside a tick the window is
// reset by ResetTickCache, so a tick still sees at most one fork per session;
// between ticks the supervisor's API handlers (/rigs, /sessions) query GetMeta
// for every configured session slot on every request, and this TTL is what
// keeps those from forking `tmux show-environment` per slot per request
// (sys-4za3nm) while never serving metadata older than a few seconds.
const metaCacheTTL = 5 * time.Second

// tickMetaCache memoizes each session's full tmux environment for the
// lifetime of one reconcile tick, or metaCacheTTL, whichever ends first. The
// supervisor's beadReconcileTick queries GetMeta for the same session from
// several independent call sites within a single pass (worker-handle
// resolution, drain-ack check, restart-request check, pending-create-info
// matching, drain-ack-queue dedup, stale-drain-ack detection) — each an
// otherwise-uncached `tmux show-environment -t <session> <key>` fork
// (gastownhall/gascity#<bead>). Fetching the whole environment once per
// session per tick and serving every key from it collapses that
// N-forks-per-session-per-tick cost to one.
type tickMetaCache struct {
	tm  *Tmux
	sf  singleflight.Group
	now func() time.Time

	mu      sync.Mutex
	entries map[string]metaCacheEntry
}

type metaCacheEntry struct {
	env       map[string]string
	err       error
	fetchedAt time.Time
}

func newTickMetaCache(tm *Tmux) *tickMetaCache {
	return &tickMetaCache{
		tm:      tm,
		now:     time.Now,
		entries: make(map[string]metaCacheEntry),
	}
}

// get returns the value of key in session's environment, matching
// [Provider.GetMeta]'s contract: ("", nil) when the key (or session) is
// absent, and the propagated error only for [ErrSessionNotFound] /
// [ErrNoServer].
func (c *tickMetaCache) get(name, key string) (string, error) {
	c.mu.Lock()
	entry, have := c.entries[name]
	if have && c.now().Sub(entry.fetchedAt) > metaCacheTTL {
		delete(c.entries, name)
		have = false
	}
	c.mu.Unlock()

	if !have {
		v, sfErr, _ := c.sf.Do(name, func() (any, error) {
			e, ferr := c.tm.GetAllEnvironment(name)
			c.mu.Lock()
			c.entries[name] = metaCacheEntry{env: e, err: ferr, fetchedAt: c.now()}
			c.mu.Unlock()
			return e, ferr
		})
		if sfErr != nil {
			entry = metaCacheEntry{err: sfErr}
		} else {
			entry = metaCacheEntry{env: v.(map[string]string)}
		}
	}

	if entry.err != nil {
		if errors.Is(entry.err, ErrSessionNotFound) || errors.Is(entry.err, ErrNoServer) {
			return "", entry.err
		}
		return "", nil
	}
	return entry.env[key], nil
}

// invalidate discards any cached environment for name, so the next get
// re-fetches. Called after a SetMeta/RemoveMeta so a write is never masked by
// a stale cached read.
func (c *tickMetaCache) invalidate(name string) {
	c.mu.Lock()
	delete(c.entries, name)
	c.mu.Unlock()
}
