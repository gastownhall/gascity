package beads

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"
)

// CachingStore wraps a BdStore with an in-memory cache.
// Reads are served from memory when the cache is live. Writes pass
// through to the backing store and update the cache on success.
//
// External writes (agents running bd directly) are picked up via the
// bd hook -> gc event emit -> event bus path. Call ApplyEvent when the
// event bus delivers bead.created/updated/closed events. The background
// reconciler acts as a watchdog and only performs a full scan once the
// cache has gone stale or degraded.
//
// Only wraps BdStore because the event hook path requires dolt/bd.
type CachingStore struct {
	backing Store // runtime: always *BdStore; tests may use MemStore

	mu          sync.RWMutex
	beads       map[string]Bead
	deps        map[string][]Dep
	dirty       map[string]struct{}
	state       cacheState
	lastFreshAt time.Time

	reconciling  atomic.Bool
	syncFailures int
	stats        CacheStats
	onChange     func(eventType, beadID string, payload json.RawMessage)
	cancelFn     context.CancelFunc
	problemf     func(string)
}

type cacheState int

const (
	cacheUninitialized cacheState = iota
	cachePartial                  // PrimeActive loaded active beads; active queries can use the cache immediately.
	cacheLive
	cacheDegraded
)

// CacheStats exposes cache freshness, reconciliation, and problem state.
type CacheStats struct {
	TotalBeads      int
	TotalDeps       int
	LastFreshAt     time.Time
	LastReconcileAt time.Time
	LastReconcileMs float64
	Adds            int64
	Removes         int64
	Updates         int64
	SyncFailures    int
	ProblemCount    int64
	LastProblemAt   time.Time
	LastProblem     string
	State           string
}

const (
	maxCacheSyncFailures         = 5
	cacheReconcilePollInterval   = 5 * time.Second
	cacheReconcileIntervalSmall  = 30 * time.Second
	cacheReconcileIntervalMedium = 60 * time.Second
	cacheReconcileIntervalLarge  = 120 * time.Second
)

// NewCachingStore wraps a BdStore with an in-memory read cache.
// Call Prime() before serving reads, then StartReconciler() for
// watchdog reconciliation. The onChange callback (optional) is called for
// each detected external change with event type and bead JSON.
//
// Only BdStore is supported because the event hook path (bd hooks ->
// gc event emit -> event bus -> ApplyEvent) requires dolt infrastructure.
func NewCachingStore(backing *BdStore, onChange func(eventType, beadID string, payload json.RawMessage)) *CachingStore {
	return newCachingStore(backing, onChange)
}

// NewCachingStoreForTest wraps any Store for testing. Production code
// must use NewCachingStore with a *BdStore.
func NewCachingStoreForTest(backing Store, onChange func(eventType, beadID string, payload json.RawMessage)) *CachingStore {
	return newCachingStore(backing, onChange)
}

func newCachingStore(backing Store, onChange func(eventType, beadID string, payload json.RawMessage)) *CachingStore {
	return &CachingStore{
		backing:  backing,
		beads:    make(map[string]Bead),
		deps:     make(map[string][]Dep),
		dirty:    make(map[string]struct{}),
		onChange: onChange,
		problemf: func(msg string) {
			log.Printf("beads cache: %s", msg)
		},
	}
}

// PrimeActive loads all non-closed beads (open + in_progress) into the
// cache. These are fast indexed queries that populate enough data for
// startup paths without waiting for a full scan. The cache enters
// cachePartial state: filtered active queries and Get hit cache for primed
// beads, while closed-bead queries still delegate to the backing store.
func (c *CachingStore) PrimeActive() error {
	var all []Bead
	for _, status := range []string{"open", "in_progress"} {
		beads, err := c.backing.List(ListQuery{Status: status})
		if err != nil {
			return fmt.Errorf("prime active (%s): %w", status, err)
		}
		all = append(all, beads...)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	for _, b := range all {
		c.beads[b.ID] = cloneBead(b)
	}
	if c.state == cacheUninitialized {
		c.state = cachePartial
	}
	c.markFreshLocked(time.Now())
	c.updateStatsLocked()
	return nil
}

// Prime loads all active beads and deps from the backing store into memory.
// Retries up to 3 times on failure since bd list can time out under
// concurrent dolt load.
func (c *CachingStore) Prime(_ context.Context) error {
	var all []Bead
	var err error
	for attempt := 1; attempt <= 3; attempt++ {
		all, err = c.backing.List(ListQuery{AllowScan: true}) // active beads only (default)
		if err == nil {
			break
		}
		c.recordProblem(fmt.Sprintf("prime cache attempt %d/3", attempt), err)
		if attempt < 3 {
			time.Sleep(time.Duration(attempt*5) * time.Second)
		}
	}
	if err != nil {
		return fmt.Errorf("prime list: %w", err)
	}

	beadMap := make(map[string]Bead, len(all))
	for _, b := range all {
		beadMap[b.ID] = cloneBead(b)
	}

	depMap, depErr := c.fetchDepsForIDs(beadIDs(beadMap))
	if depErr != nil {
		c.recordProblem("prime dep cache", depErr)
	}

	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.beads = beadMap
	c.deps = depMap
	c.dirty = make(map[string]struct{})
	c.state = cacheLive
	c.syncFailures = 0
	c.stats.SyncFailures = 0
	c.markFreshLocked(now)
	c.updateStatsLocked()
	return nil
}

// StartReconciler launches watchdog reconciliation. Cancel ctx to stop.
func (c *CachingStore) StartReconciler(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	c.cancelFn = cancel
	go c.reconcileLoop(ctx)
}

// StopReconciler cancels the background reconciler.
func (c *CachingStore) StopReconciler() {
	if c.cancelFn != nil {
		c.cancelFn()
	}
}

// Stats returns current cache statistics.
func (c *CachingStore) Stats() CacheStats {
	c.mu.RLock()
	defer c.mu.RUnlock()

	s := c.stats
	switch c.state {
	case cachePartial:
		s.State = "partial"
	case cacheLive:
		s.State = "live"
	case cacheDegraded:
		s.State = "degraded"
	default:
		s.State = "uninitialized"
	}
	return s
}

// IsLive reports whether reads are served from the cache.
func (c *CachingStore) IsLive() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.state == cacheLive
}

// Backing returns the underlying store.
func (c *CachingStore) Backing() Store { return c.backing }

func (c *CachingStore) markFreshLocked(now time.Time) {
	c.lastFreshAt = now
	c.stats.LastFreshAt = now
}

func (c *CachingStore) recordProblem(op string, err error) {
	if err == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.recordProblemLocked(op, err)
}

func (c *CachingStore) recordProblemLocked(op string, err error) {
	if err == nil {
		return
	}
	msg := fmt.Sprintf("%s: %v", op, err)
	c.stats.ProblemCount++
	c.stats.LastProblemAt = time.Now()
	c.stats.LastProblem = msg
	if c.problemf != nil {
		c.problemf(msg)
	}
}

func (c *CachingStore) updateStatsLocked() {
	c.stats.TotalBeads = len(c.beads)
	totalDeps := 0
	for _, deps := range c.deps {
		totalDeps += len(deps)
	}
	c.stats.TotalDeps = totalDeps
	c.stats.SyncFailures = c.syncFailures
}

func beadIDs(beadMap map[string]Bead) []string {
	ids := make([]string, 0, len(beadMap))
	for id := range beadMap {
		ids = append(ids, id)
	}
	return ids
}

func (c *CachingStore) fetchDepsForIDs(ids []string) (map[string][]Dep, error) {
	depMap := make(map[string][]Dep)
	if len(ids) == 0 {
		return depMap, nil
	}

	if bdStore, ok := c.backing.(*BdStore); ok {
		batchDeps, err := bdStore.DepListBatch(ids)
		if err != nil {
			return depMap, err
		}
		for id, deps := range batchDeps {
			depMap[id] = cloneDeps(deps)
		}
		return depMap, nil
	}

	for _, id := range ids {
		deps, err := c.backing.DepList(id, "down")
		if err != nil {
			return depMap, fmt.Errorf("listing deps for %s: %w", id, err)
		}
		depMap[id] = cloneDeps(deps)
	}
	return depMap, nil
}

func cloneDeps(deps []Dep) []Dep {
	if len(deps) == 0 {
		return nil
	}
	cloned := make([]Dep, len(deps))
	copy(cloned, deps)
	return cloned
}
