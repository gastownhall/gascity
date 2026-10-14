package tmux

import (
	"errors"
	"testing"
	"time"
)

// staleForCache returns a cache over a scripted fetcher on a fake clock the
// caller advances through *now.
func staleForCache(sessions map[string]bool) (*StateCache, *mockFetcher, *time.Time) {
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	fetcher := &mockFetcher{sessions: sessions}
	cache := NewStateCache(fetcher, 2*time.Second)
	cache.now = func() time.Time { return now }
	return cache, fetcher, &now
}

func TestStateCache_StaleFor(t *testing.T) {
	t.Run("nil cache is unknown", func(t *testing.T) {
		var c *StateCache
		if age, known := c.StaleFor(); age != 0 || known {
			t.Fatalf("nil StaleFor() = (%s, %v), want (0, false)", age, known)
		}
	})

	t.Run("unprimed cache is unknown", func(t *testing.T) {
		c, _, _ := staleForCache(map[string]bool{"worker-1": true})
		if age, known := c.StaleFor(); age != 0 || known {
			t.Fatalf("unprimed StaleFor() = (%s, %v), want (0, false): no refresh has run", age, known)
		}
	})

	t.Run("latest refresh succeeded", func(t *testing.T) {
		c, _, _ := staleForCache(map[string]bool{"worker-1": true})
		c.observe()
		if age, known := c.StaleFor(); age != 0 || !known {
			t.Fatalf("StaleFor() after a successful refresh = (%s, %v), want (0, true)", age, known)
		}
	})

	t.Run("age counts from the last success while refreshes fail", func(t *testing.T) {
		c, fetcher, now := staleForCache(map[string]bool{"worker-1": true})
		c.observe()
		fetcher.mu.Lock()
		fetcher.err = errors.New("list-panes: socket stalled")
		fetcher.mu.Unlock()
		*now = now.Add(45 * time.Second)
		c.observe()
		age, known := c.StaleFor()
		if !known || age != 45*time.Second {
			t.Fatalf("StaleFor() = (%s, %v), want (45s, true): the cache last refreshed 45s ago and has failed since", age, known)
		}
		fetcher.mu.Lock()
		fetcher.err = nil
		fetcher.mu.Unlock()
		*now = now.Add(5 * time.Second)
		c.observe()
		if age, known := c.StaleFor(); age != 0 || !known {
			t.Fatalf("StaleFor() after recovery = (%s, %v), want (0, true)", age, known)
		}
	})

	t.Run("a city that never had a tmux server is not stale", func(t *testing.T) {
		c, fetcher, now := staleForCache(nil)
		fetcher.mu.Lock()
		fetcher.err = errFetchNoServer
		fetcher.mu.Unlock()
		c.observe()
		*now = now.Add(10 * time.Minute)
		c.observe()
		if age, known := c.StaleFor(); age != 0 || known {
			t.Fatalf("StaleFor() with no tmux server ever reachable = (%s, %v), want (0, false): the empty snapshot is the true answer, not a stale one", age, known)
		}
	})
}
