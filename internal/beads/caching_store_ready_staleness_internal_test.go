package beads

import (
	"context"
	"errors"
	"testing"
)

// readyInvariantStage is one observable state of a live cache: the four cached
// readiness handles, read after some routine mutation has landed.
//
// The invariant they all owe is #3218's: a cached readiness read may never
// return a bead the backing's own Ready() excludes. Declining is a correct
// answer — the caller then takes the live verdict — but answering with MORE
// than the backing is a wrong-ready, and the control dispatcher acts on it.
type readyInvariantStage struct {
	t     *testing.T
	cache *CachingStore
	// want is the backing's own Ready() answer at this stage: the ceiling.
	want []string
}

func newReadyInvariantStage(t *testing.T, cache *CachingStore, want []string) readyInvariantStage {
	t.Helper()
	return readyInvariantStage{t: t, cache: cache, want: want}
}

// readers returns the four handles a cached readiness answer reaches
// production through: Ready (build_desired_state's live fallback owner),
// ReadyContext (/status), CachedReady (the control dispatcher) and
// Handles().Cached.Ready (the desired-state builder).
func (s readyInvariantStage) readers() []struct {
	name string
	read func() ([]Bead, error)
} {
	cache := s.cache
	return []struct {
		name string
		read func() ([]Bead, error)
	}{
		{"Ready", func() ([]Bead, error) { return cache.Ready() }},
		{"ReadyContext", func() ([]Bead, error) { return cache.ReadyContext(context.Background()) }},
		{"CachedReady", func() ([]Bead, error) {
			rows, ok := cache.CachedReady()
			if !ok {
				return nil, ErrCacheUnavailable
			}
			return rows, nil
		}},
		{"Handles().Cached.Ready", func() ([]Bead, error) { return cache.Handles().Cached.Ready() }},
	}
}

// assertNeverExceedsBacking runs every readiness handle and fails for any row
// offered beyond the backing's own verdict. hidden names the rows the backing
// hides and why, so a failure reads as the defect rather than as a diff.
func (s readyInvariantStage) assertNeverExceedsBacking(stage string, hidden map[string]string) {
	s.t.Helper()
	for _, reader := range s.readers() {
		rows, err := reader.read()
		if err != nil {
			if errors.Is(err, ErrCacheUnavailable) {
				continue
			}
			s.t.Fatalf("%s (%s): %v", reader.name, stage, err)
		}
		got := sortedIDs(rows)
		for id, why := range hidden {
			if containsID(got, id) {
				s.t.Errorf("%s (%s) offered %s, which the backing's own Ready hides: %s (backing = %v, cache = %v)",
					reader.name, stage, id, why, s.want, got)
			}
		}
		if extra := idsBeyond(got, s.want); len(extra) > 0 {
			s.t.Errorf("%s (%s) returned %v beyond the backing's own Ready (backing = %v)",
				reader.name, stage, extra, s.want)
		}
	}
}

// assertStillAnswers pins the other half. Declining is the fail-safe, not the
// fix: a cache that only ever declined would satisfy every subset check above
// while costing maintainer-city the live read this work exists to remove.
func (s readyInvariantStage) assertStillAnswers(stage string) {
	s.t.Helper()
	cached, ok := s.cache.CachedReady()
	if !ok {
		s.t.Fatalf("CachedReady declined (%s): a backing that can answer is_blocked must keep serving readiness from cache", stage)
	}
	if got := sortedIDs(cached); !equalIDs(got, s.want) {
		s.t.Fatalf("CachedReady (%s) = %v, want %v (the backing's own verdict)", stage, got, s.want)
	}
}

// assertDeclines pins the COST of the fail-safe: the named row's verdict is one
// the cache cannot reproduce, so every readiness handle takes its live fallback
// instead of guessing. Without this the subset assertions above would pass just
// as well on a cache that had silently stopped offering the row — a starvation
// bug wearing a correctness costume.
func (s readyInvariantStage) assertDeclines(stage, id string) {
	s.t.Helper()
	if !s.cache.readyProjectionUnknownForTest(id) {
		s.t.Fatalf("%s: %s is servable from cache, so the subset check above proves nothing about the fail-safe", stage, id)
	}
	if _, ok := s.cache.CachedReady(); ok {
		s.t.Errorf("CachedReady (%s) answered though %s has no verdict the cache can vouch for", stage, id)
	}
	if _, err := s.cache.ReadyContext(context.Background()); !errors.Is(err, ErrCacheUnavailable) {
		s.t.Errorf("ReadyContext (%s) error = %v, want ErrCacheUnavailable", stage, err)
	}
	if _, err := s.cache.Handles().Cached.Ready(); !errors.Is(err, ErrCacheUnavailable) {
		s.t.Errorf("cached reader Ready (%s) error = %v, want ErrCacheUnavailable", stage, err)
	}
}

// readyProjectionUnknownForTest reports whether readiness reads must decline id.
func (c *CachingStore) readyProjectionUnknownForTest(id string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.readyProjectionUnknownLocked(id)
}

// cachedIsBlockedForTest reports the cached ready projection for one row.
func cachedIsBlockedForTest(cache *CachingStore, id string) *bool {
	cache.mu.RLock()
	defer cache.mu.RUnlock()
	return cache.beads[id].IsBlocked
}
