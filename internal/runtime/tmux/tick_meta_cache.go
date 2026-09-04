package tmux

import (
	"errors"
	"sync"

	"golang.org/x/sync/singleflight"
)

// tickMetaCache memoizes each session's full tmux environment for the
// lifetime of one reconcile tick. The supervisor's beadReconcileTick queries
// GetMeta for the same session from several independent call sites within a
// single pass (worker-handle resolution, drain-ack check, restart-request
// check, pending-create-info matching, drain-ack-queue dedup, stale-drain-ack
// detection) — each an otherwise-uncached `tmux show-environment -t <session>
// <key>` fork (gastownhall/gascity#<bead>). Fetching the whole environment
// once per session per tick and serving every key from it collapses that
// N-forks-per-session-per-tick cost to one.
type tickMetaCache struct {
	tm *Tmux
	sf singleflight.Group

	mu   sync.Mutex
	envs map[string]map[string]string
	errs map[string]error
}

func newTickMetaCache(tm *Tmux) *tickMetaCache {
	return &tickMetaCache{
		tm:   tm,
		envs: make(map[string]map[string]string),
		errs: make(map[string]error),
	}
}

// get returns the value of key in session's environment, matching
// [Provider.GetMeta]'s contract: ("", nil) when the key (or session) is
// absent, and the propagated error only for [ErrSessionNotFound] /
// [ErrNoServer].
func (c *tickMetaCache) get(name, key string) (string, error) {
	c.mu.Lock()
	env, haveEnv := c.envs[name]
	fetchErr, haveErr := c.errs[name]
	c.mu.Unlock()

	if !haveEnv && !haveErr {
		v, sfErr, _ := c.sf.Do(name, func() (any, error) {
			e, ferr := c.tm.GetAllEnvironment(name)
			c.mu.Lock()
			if ferr != nil {
				c.errs[name] = ferr
			} else {
				c.envs[name] = e
			}
			c.mu.Unlock()
			return e, ferr
		})
		if sfErr != nil {
			fetchErr, haveErr = sfErr, true
		} else {
			env, haveEnv = v.(map[string]string), true
		}
	}

	if haveErr {
		if errors.Is(fetchErr, ErrSessionNotFound) || errors.Is(fetchErr, ErrNoServer) {
			return "", fetchErr
		}
		return "", nil
	}
	return env[key], nil
}

// invalidate discards any cached environment for name, so the next get
// re-fetches. Called after a same-tick SetMeta/RemoveMeta so a write is never
// masked by a stale cached read.
func (c *tickMetaCache) invalidate(name string) {
	c.mu.Lock()
	delete(c.envs, name)
	delete(c.errs, name)
	c.mu.Unlock()
}
