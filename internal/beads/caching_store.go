package beads

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
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
	backing  Store // runtime: always *BdStore; tests may use MemStore
	idPrefix string

	mu              sync.RWMutex
	beads           map[string]Bead
	deps            map[string][]Dep
	depsComplete    bool
	dirty           map[string]struct{}
	beadSeq         map[string]uint64
	deletedSeq      map[string]uint64
	state           cacheState
	lastFreshAt     time.Time
	mutationSeq     uint64
	primePartialErr error

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
	prefix := ""
	if backing != nil {
		prefix = backing.IDPrefix()
	}
	cs := newCachingStore(backing, prefix, onChange)
	if cs.idPrefix == "" {
		cs.recordProblem("bd cache ownership", errors.New("missing issue prefix; foreign bead event filtering disabled"))
	}
	return cs
}

// NewCachingStoreForTest wraps any Store for testing. Production code
// must use NewCachingStore with a *BdStore.
func NewCachingStoreForTest(backing Store, onChange func(eventType, beadID string, payload json.RawMessage)) *CachingStore {
	return newCachingStore(backing, "", onChange)
}

// NewCachingStoreForTestWithPrefix wraps any Store for tests that need
// production-style bead ID ownership filtering.
func NewCachingStoreForTestWithPrefix(backing Store, idPrefix string, onChange func(eventType, beadID string, payload json.RawMessage)) *CachingStore {
	return newCachingStore(backing, idPrefix, onChange)
}

func newCachingStore(backing Store, idPrefix string, onChange func(eventType, beadID string, payload json.RawMessage)) *CachingStore {
	return &CachingStore{
		backing:    backing,
		idPrefix:   normalizeIDPrefix(idPrefix),
		beads:      make(map[string]Bead),
		deps:       make(map[string][]Dep),
		dirty:      make(map[string]struct{}),
		beadSeq:    make(map[string]uint64),
		deletedSeq: make(map[string]uint64),
		onChange:   onChange,
		problemf: func(msg string) {
			log.Printf("beads cache: %s", msg)
		},
	}
}

func normalizeIDPrefix(prefix string) string {
	return strings.Trim(strings.ToLower(strings.TrimSpace(prefix)), "-")
}

func (c *CachingStore) ownsBeadID(id string) bool {
	if c.idPrefix == "" {
		return true
	}
	id = strings.ToLower(strings.TrimSpace(id))
	return strings.HasPrefix(id, c.idPrefix+"-")
}

func (c *CachingStore) noteMutationLocked(ids ...string) uint64 {
	c.mutationSeq++
	seq := c.mutationSeq
	for _, id := range ids {
		if id == "" {
			continue
		}
		c.beadSeq[id] = seq
	}
	return seq
}

// PrimeActive loads all non-closed beads (open + in_progress) into the
// cache. These are fast indexed queries that populate enough data for
// startup paths without waiting for a full scan. The cache enters
// cachePartial state: filtered active queries and Get hit cache for primed
// beads, while closed-bead queries still delegate to the backing store.
func (c *CachingStore) PrimeActive() error {
	c.mu.RLock()
	startSeq := c.mutationSeq
	c.mu.RUnlock()

	var all []Bead
	var partialErr error
	for _, status := range []string{"open", "in_progress"} {
		beads, err := c.backing.List(ListQuery{Status: status})
		if err != nil {
			if !IsPartialResult(err) {
				return fmt.Errorf("prime active (%s): %w", status, err)
			}
			partialErr = errors.Join(partialErr, err)
			c.recordProblem(fmt.Sprintf("prime active (%s)", status), err)
		}
		all = append(all, beads...)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	for _, b := range all {
		if c.mutationSeq != startSeq {
			if c.deletedSeq[b.ID] > startSeq {
				continue
			}
			if _, exists := c.beads[b.ID]; exists {
				continue
			}
		}
		c.beads[b.ID] = cloneBead(b)
		delete(c.deletedSeq, b.ID)
	}
	if c.state == cacheUninitialized {
		c.state = cachePartial
	}
	c.primePartialErr = partialErr
	c.markFreshLocked(time.Now())
	c.updateStatsLocked()
	return nil
}

// Prime loads all active beads and deps from the backing store into memory.
// Retries up to 3 times on failure since bd list can time out under
// concurrent dolt load.
func (c *CachingStore) Prime(_ context.Context) error {
	c.mu.RLock()
	startSeq := c.mutationSeq
	c.mu.RUnlock()

	var all []Bead
	var err error
	var partialErr error
	for attempt := 1; attempt <= 3; attempt++ {
		all, err = c.backing.List(ListQuery{AllowScan: true}) // active beads only (default)
		if err == nil {
			break
		}
		if IsPartialResult(err) {
			c.recordProblem("prime cache: partial list", err)
			partialErr = err
			err = nil
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

	depMap, depsComplete, depErr := c.fetchDepsForIDs(beadIDs(beadMap))
	if depErr != nil {
		c.recordProblem("prime dep cache", depErr)
	}

	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.mutationSeq == startSeq {
		c.beads = beadMap
		c.deps = depMap
		c.depsComplete = depsComplete && depErr == nil
		c.dirty = make(map[string]struct{})
		c.beadSeq = make(map[string]uint64)
		c.deletedSeq = make(map[string]uint64)
	} else {
		for id, b := range beadMap {
			if c.deletedSeq[id] > startSeq {
				continue
			}
			if _, exists := c.beads[id]; exists {
				continue
			}
			c.beads[id] = b
			delete(c.deletedSeq, id)
			delete(c.beadSeq, id)
			if deps, ok := depMap[id]; ok {
				c.deps[id] = deps
			}
		}
		c.depsComplete = false
	}
	c.state = cacheLive
	c.syncFailures = 0
	c.stats.SyncFailures = 0
	c.primePartialErr = partialErr
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

func (c *CachingStore) fetchDepsForIDs(ids []string) (map[string][]Dep, bool, error) {
	depMap := make(map[string][]Dep)
	if len(ids) == 0 {
		return depMap, true, nil
	}

	if _, ok := c.backing.(*BdStore); ok {
		return depMap, false, nil
	}

	for _, id := range ids {
		deps, err := c.backing.DepList(id, "down")
		if err != nil {
			return depMap, false, fmt.Errorf("listing deps for %s: %w", id, err)
		}
		depMap[id] = cloneDeps(deps)
	}
	return depMap, true, nil
}

func cloneDeps(deps []Dep) []Dep {
	if len(deps) == 0 {
		return nil
	}
	cloned := make([]Dep, len(deps))
	copy(cloned, deps)
	return cloned
}
