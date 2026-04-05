package beads

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

// CachingStore wraps a BdStore with an in-memory cache.
// Reads are served from memory when the cache is live. Writes pass
// through to the backing store and update the cache on success.
//
// External writes (agents running bd directly) are picked up via the
// bd hook → gc event emit → event bus path. Call ApplyEvent when the
// event bus delivers bead.created/updated/closed events. A background
// reconciler periodically syncs as a safety net.
//
// Only wraps BdStore because the event hook path requires dolt/bd.
type CachingStore struct {
	backing Store // runtime: always *BdStore; tests may use MemStore

	mu       sync.RWMutex
	beads    map[string]Bead
	deps     map[string][]Dep
	state    cacheState
	syncedAt time.Time

	// primeReady is closed when Prime completes (success or failure).
	// Full-scan reads (List) block on this instead of falling through
	// to a bd subprocess, preventing stampedes during startup.
	primeReady chan struct{}
	primeErr   error // set if Prime fails; readers fall through to backing

	reconciling  atomic.Bool
	syncFailures int
	stats        CacheStats
	onChange     func(eventType, beadID string, payload json.RawMessage)
	cancelFn     context.CancelFunc
}

type cacheState int

const (
	cacheUninitialized cacheState = iota
	cachePartial                  // PrimeLabel loaded a subset; ListByLabel hits cache, ListOpen() waits for full Prime
	cacheLive
	cacheDegraded
)

// CacheStats exposes reconciliation metrics for observability.
type CacheStats struct {
	TotalBeads      int
	TotalDeps       int
	LastReconcileAt time.Time
	LastReconcileMs float64
	Adds            int64
	Removes         int64
	Updates         int64
	SyncFailures    int
	State           string
}

const (
	maxCacheSyncFailures         = 5
	cacheReconcileIntervalSmall  = 30 * time.Second
	cacheReconcileIntervalMedium = 60 * time.Second
	cacheReconcileIntervalLarge  = 120 * time.Second
)

// NewCachingStore wraps a BdStore with an in-memory read cache.
// Call Prime() before serving reads, then StartReconciler() for
// background sync. The onChange callback (optional) is called for
// each detected external change with event type and bead JSON.
//
// Only BdStore is supported because the event hook path (bd hooks →
// gc event emit → event bus → ApplyEvent) requires dolt infrastructure.
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
		backing:    backing,
		beads:      make(map[string]Bead),
		deps:       make(map[string][]Dep),
		onChange:   onChange,
		primeReady: make(chan struct{}),
	}
}

// PrimeActive loads all non-closed beads (open + in_progress) into the
// cache. These are fast indexed queries (~1-2s total) that populate
// enough data for the startup path (adoption, session snapshot, desired
// state) without waiting for the full Prime. The cache enters
// cachePartial state: ListByLabel and Get hit cache for primed beads,
// ListOpen() still waits for full Prime to backfill closed/historical beads.
func (c *CachingStore) PrimeActive() error {
	var all []Bead
	for _, status := range []string{"open", "in_progress"} {
		beads, err := c.backing.ListOpen(status)
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
	log.Printf("caching-store: pre-primed %d active beads", len(all))
	return nil
}

// Prime loads all beads and deps from the backing store into memory.
// Closes the primeReady channel on completion so blocked ListOpen() callers
// can proceed. Retries up to 3 times on failure since bd list can time
// out under concurrent dolt load.
func (c *CachingStore) Prime(_ context.Context) error {
	var all []Bead
	var err error
	for attempt := 1; attempt <= 3; attempt++ {
		all, err = c.backing.ListOpen() // non-closed beads only (default)
		if err == nil {
			break
		}
		log.Printf("caching-store: prime attempt %d/3 failed: %v", attempt, err)
		if attempt < 3 {
			time.Sleep(time.Duration(attempt*5) * time.Second)
		}
	}
	if err != nil {
		c.primeErr = err
		c.signalPrimeReady()
		return fmt.Errorf("prime list: %w", err)
	}

	beadMap := make(map[string]Bead, len(all))
	for _, b := range all {
		beadMap[b.ID] = cloneBead(b)
	}
	// Batch-fetch deps in one subprocess call (if backing is BdStore).
	depMap := make(map[string][]Dep)
	if bdStore, ok := c.backing.(*BdStore); ok && len(beadMap) > 0 {
		ids := make([]string, 0, len(beadMap))
		for id := range beadMap {
			ids = append(ids, id)
		}
		if batchDeps, err := bdStore.DepListBatch(ids); err == nil {
			for id, deps := range batchDeps {
				depMap[id] = slices.Clone(deps)
			}
		}
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.beads = beadMap
	c.deps = depMap
	c.state = cacheLive
	c.syncedAt = time.Now()
	c.syncFailures = 0
	c.updateStatsLocked()
	log.Printf("caching-store: primed %d beads, %d dep entries", len(beadMap), len(depMap))
	c.signalPrimeReady()
	return nil
}

// signalPrimeReady closes the primeReady channel exactly once.
func (c *CachingStore) signalPrimeReady() {
	select {
	case <-c.primeReady:
		// Already closed (from a previous Prime call).
	default:
		close(c.primeReady)
	}
}

// StartReconciler launches background periodic sync. Cancel ctx to stop.
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

// ApplyEvent updates the cache from a bd hook event. Call this when the
// event bus delivers a bead.created, bead.updated, or bead.closed event
// with the full bead JSON payload. This provides sub-second cache
// freshness for agent-initiated bd mutations without waiting for
// reconciliation.
func (c *CachingStore) ApplyEvent(eventType string, payload json.RawMessage) {
	if len(payload) == 0 {
		return
	}
	var b Bead
	if err := json.Unmarshal(payload, &b); err != nil {
		return
	}
	if b.ID == "" {
		return
	}
	// bd hook payloads use "issue_type" while Bead uses "type". If Type
	// wasn't populated from "type", check for the bd field name.
	if b.Type == "" {
		var bdCompat struct {
			IssueType string `json:"issue_type"`
		}
		if err := json.Unmarshal(payload, &bdCompat); err == nil && bdCompat.IssueType != "" {
			b.Type = bdCompat.IssueType
		}
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state != cacheLive {
		return
	}

	switch eventType {
	case "bead.created":
		if _, exists := c.beads[b.ID]; !exists {
			c.beads[b.ID] = cloneBead(b)
			c.updateStatsLocked()
		}
	case "bead.updated":
		c.beads[b.ID] = cloneBead(b)
	case "bead.closed":
		if existing, ok := c.beads[b.ID]; ok {
			existing.Status = "closed"
			// Merge metadata from the event — close events often carry
			// gc.outcome and other fields set in the same bd update.
			for k, v := range b.Metadata {
				if existing.Metadata == nil {
					existing.Metadata = make(map[string]string)
				}
				existing.Metadata[k] = v
			}
			c.beads[b.ID] = existing
		} else {
			// Bead not in cache yet — store the closed version.
			c.beads[b.ID] = cloneBead(b)
			c.updateStatsLocked()
		}
	}
}

// ApplyDepEvent updates the dep cache for a bead. Call after dep
// mutations are detected via events or write-through.
func (c *CachingStore) ApplyDepEvent(beadID string, deps []Dep) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state != cacheLive {
		return
	}
	c.deps[beadID] = slices.Clone(deps)
}

// ── Read methods (cache when live, fallback to backing) ─────────────

// ListOpen returns all cached beads, optionally filtered by status.
// When fully primed (cacheLive), serves from memory. When partially
// primed (cachePartial) with a status filter for pre-primed statuses
// (open, in_progress), serves from the partial cache. Otherwise blocks
// until full Prime completes.
func (c *CachingStore) ListOpen(status ...string) ([]Bead, error) {
	c.mu.RLock()
	state := c.state
	if state == cacheLive || (state == cachePartial && len(status) > 0 && isPrePrimedStatus(status[0])) {
		filterStatus := ""
		if len(status) > 0 {
			filterStatus = status[0]
		}
		result := make([]Bead, 0, len(c.beads))
		for _, b := range c.beads {
			if filterStatus != "" && b.Status != filterStatus {
				continue
			}
			result = append(result, cloneBead(b))
		}
		c.mu.RUnlock()
		return result, nil
	}
	c.mu.RUnlock()

	// Wait for Prime to complete instead of stampeding bd subprocess.
	<-c.primeReady

	// Prime succeeded → cache is live, serve from memory.
	c.mu.RLock()
	if c.state == cacheLive {
		filterStatus := ""
		if len(status) > 0 {
			filterStatus = status[0]
		}
		result := make([]Bead, 0, len(c.beads))
		for _, b := range c.beads {
			if filterStatus != "" && b.Status != filterStatus {
				continue
			}
			result = append(result, cloneBead(b))
		}
		c.mu.RUnlock()
		return result, nil
	}
	c.mu.RUnlock()

	// Prime failed → fall through to backing store as last resort.
	return c.backing.ListOpen(status...)
}

// Get returns a single bead by ID from the cache or backing store.
func (c *CachingStore) Get(id string) (Bead, error) {
	c.mu.RLock()
	if c.state == cacheLive || c.state == cachePartial {
		if b, ok := c.beads[id]; ok {
			c.mu.RUnlock()
			return cloneBead(b), nil
		}
		if c.state == cacheLive {
			// Fully primed — bead doesn't exist.
			c.mu.RUnlock()
			return Bead{}, fmt.Errorf("getting bead %q: %w", id, ErrNotFound)
		}
		// Partial — bead might exist but wasn't in the primed label set.
		c.mu.RUnlock()
		return c.backing.Get(id)
	}
	c.mu.RUnlock()
	return c.backing.Get(id)
}

// Ready returns open beads whose blocking deps are all closed.
func (c *CachingStore) Ready() ([]Bead, error) {
	c.mu.RLock()
	if c.state == cacheLive {
		statusByID := make(map[string]string, len(c.beads))
		for _, b := range c.beads {
			statusByID[b.ID] = b.Status
		}
		var result []Bead
		for _, b := range c.beads {
			if b.Status != "open" {
				continue
			}
			blocked := false
			if deps, ok := c.deps[b.ID]; ok {
				for _, dep := range deps {
					switch dep.Type {
					case "blocks", "waits-for", "conditional-blocks":
					default:
						continue
					}
					if statusByID[dep.DependsOnID] != "closed" {
						blocked = true
						break
					}
				}
			}
			if !blocked {
				result = append(result, cloneBead(b))
			}
		}
		c.mu.RUnlock()
		return result, nil
	}
	c.mu.RUnlock()
	return c.backing.Ready()
}

// Children returns all beads with the given parent ID.
// Children always delegates to the backing store because all callers need
// closed children (molecule tree walks) and the cache never has them.
func (c *CachingStore) Children(parentID string, opts ...QueryOpt) ([]Bead, error) {
	return c.backing.Children(parentID, opts...)
}

// ListByLabel returns beads matching the given label. By default, serves from
// cache only (non-closed beads). Pass IncludeClosed to also query the backing
// store for closed beads and merge results.
func (c *CachingStore) ListByLabel(label string, limit int, opts ...QueryOpt) ([]Bead, error) {
	c.mu.RLock()
	if c.state == cacheLive || c.state == cachePartial {
		var cached []Bead
		seen := make(map[string]bool)
		for _, b := range c.beads {
			for _, l := range b.Labels {
				if l == label {
					cached = append(cached, cloneBead(b))
					seen[b.ID] = true
					break
				}
			}
		}
		c.mu.RUnlock()

		if !HasOpt(opts, IncludeClosed) {
			if limit > 0 && len(cached) > limit {
				cached = cached[:limit]
			}
			return cached, nil
		}

		// IncludeClosed: merge with backing store for closed beads.
		all, err := c.backing.ListByLabel(label, limit)
		if err != nil {
			return cached, nil
		}
		for _, b := range all {
			if !seen[b.ID] {
				cached = append(cached, b)
				seen[b.ID] = true
			}
		}
		if limit > 0 && len(cached) > limit {
			cached = cached[:limit]
		}
		return cached, nil
	}
	c.mu.RUnlock()
	return c.backing.ListByLabel(label, limit)
}

// ListByAssignee returns beads assigned to the given agent with matching status.
func (c *CachingStore) ListByAssignee(assignee, status string, limit int) ([]Bead, error) {
	c.mu.RLock()
	if c.state == cacheLive {
		var result []Bead
		for _, b := range c.beads {
			if b.Assignee == assignee && b.Status == status {
				result = append(result, cloneBead(b))
				if limit > 0 && len(result) >= limit {
					c.mu.RUnlock()
					return result, nil
				}
			}
		}
		c.mu.RUnlock()
		return result, nil
	}
	c.mu.RUnlock()
	return c.backing.ListByAssignee(assignee, status, limit)
}

// ListByMetadata filters beads by metadata key-value pairs.
// This is an extension method not on the base Store interface — it's
// available on BdStore and CachingStore wrapping BdStore.
func (c *CachingStore) ListByMetadata(filters map[string]string, limit int) ([]Bead, error) {
	c.mu.RLock()
	if c.state == cacheLive {
		var result []Bead
		for _, b := range c.beads {
			if matchesMetadata(b, filters) {
				result = append(result, cloneBead(b))
				if limit > 0 && len(result) >= limit {
					c.mu.RUnlock()
					return result, nil
				}
			}
		}
		c.mu.RUnlock()
		return result, nil
	}
	c.mu.RUnlock()

	// Cache not live — wait for prime then serve from memory.
	<-c.primeReady
	c.mu.RLock()
	if c.state == cacheLive {
		var result []Bead
		for _, b := range c.beads {
			if matchesMetadata(b, filters) {
				result = append(result, cloneBead(b))
				if limit > 0 && len(result) >= limit {
					c.mu.RUnlock()
					return result, nil
				}
			}
		}
		c.mu.RUnlock()
		return result, nil
	}
	c.mu.RUnlock()
	// Prime failed — fall through to backing store.
	return c.backing.ListByMetadata(filters, limit)
}

func matchesMetadata(b Bead, filters map[string]string) bool {
	for k, v := range filters {
		if b.Metadata[k] != v {
			return false
		}
	}
	return true
}

// DepList returns dependencies for a bead in the given direction.
func (c *CachingStore) DepList(id, direction string) ([]Dep, error) {
	c.mu.RLock()
	if c.state == cacheLive {
		if direction == "down" || direction == "" {
			if deps, ok := c.deps[id]; ok {
				c.mu.RUnlock()
				return slices.Clone(deps), nil
			}
			// Dep not cached yet — fetch from backing and cache it.
			c.mu.RUnlock()
			deps, err := c.backing.DepList(id, direction)
			if err != nil {
				return nil, err
			}
			c.mu.Lock()
			c.deps[id] = slices.Clone(deps)
			c.mu.Unlock()
			return deps, nil
		}
		// "up" — reverse lookup (best-effort from cache, fall through for uncached)
		var result []Dep
		for _, deps := range c.deps {
			for _, d := range deps {
				if d.DependsOnID == id {
					result = append(result, d)
				}
			}
		}
		c.mu.RUnlock()
		if len(result) > 0 {
			return result, nil
		}
		// Cache might be incomplete for "up" — fall through.
		return c.backing.DepList(id, direction)
	}
	c.mu.RUnlock()
	return c.backing.DepList(id, direction)
}

// Ping delegates to the backing store.
func (c *CachingStore) Ping() error {
	return c.backing.Ping()
}

// ── Write methods (pass through + update cache) ─────────────────────

// Create passes through to the backing store and updates the cache.
func (c *CachingStore) Create(b Bead) (Bead, error) {
	created, err := c.backing.Create(b)
	if err != nil {
		return created, err
	}
	c.mu.Lock()
	c.beads[created.ID] = cloneBead(created)
	c.mu.Unlock()
	c.notifyChange("bead.created", created)
	return created, nil
}

// Update passes through to the backing store and refreshes the cache.
func (c *CachingStore) Update(id string, opts UpdateOpts) error {
	if err := c.backing.Update(id, opts); err != nil {
		return err
	}
	// Re-fetch from backing to get the authoritative state.
	if fresh, err := c.backing.Get(id); err == nil {
		c.mu.Lock()
		c.beads[id] = cloneBead(fresh)
		c.mu.Unlock()
		c.notifyChange("bead.updated", fresh)
	}
	return nil
}

// Close marks a bead as closed in the backing store and cache.
func (c *CachingStore) Close(id string) error {
	if err := c.backing.Close(id); err != nil {
		return err
	}
	c.mu.Lock()
	if b, ok := c.beads[id]; ok {
		b.Status = "closed"
		c.beads[id] = b
		c.mu.Unlock()
		c.notifyChange("bead.closed", b)
	} else {
		c.mu.Unlock()
	}
	return nil
}

// CloseAll closes multiple beads and sets metadata on each.
func (c *CachingStore) CloseAll(ids []string, metadata map[string]string) (int, error) {
	n, err := c.backing.CloseAll(ids, metadata)
	if err != nil {
		return n, err
	}
	c.mu.Lock()
	for _, id := range ids {
		if b, ok := c.beads[id]; ok {
			b.Status = "closed"
			if b.Metadata == nil {
				b.Metadata = make(map[string]string, len(metadata))
			}
			for k, v := range metadata {
				b.Metadata[k] = v
			}
			c.beads[id] = b
		}
	}
	c.mu.Unlock()
	return n, nil
}

// SetMetadata sets a single metadata key-value on a bead.
func (c *CachingStore) SetMetadata(id, key, value string) error {
	if err := c.backing.SetMetadata(id, key, value); err != nil {
		return err
	}
	c.mu.Lock()
	if b, ok := c.beads[id]; ok {
		if b.Metadata == nil {
			b.Metadata = make(map[string]string)
		}
		b.Metadata[key] = value
		c.beads[id] = b
	}
	c.mu.Unlock()
	return nil
}

// SetMetadataBatch sets multiple metadata key-values on a bead.
func (c *CachingStore) SetMetadataBatch(id string, kvs map[string]string) error {
	if err := c.backing.SetMetadataBatch(id, kvs); err != nil {
		return err
	}
	c.mu.Lock()
	if b, ok := c.beads[id]; ok {
		if b.Metadata == nil {
			b.Metadata = make(map[string]string, len(kvs))
		}
		for k, v := range kvs {
			b.Metadata[k] = v
		}
		c.beads[id] = b
	}
	c.mu.Unlock()
	return nil
}

// DepAdd adds a dependency and updates the cache.
func (c *CachingStore) DepAdd(issueID, dependsOnID, depType string) error {
	if err := c.backing.DepAdd(issueID, dependsOnID, depType); err != nil {
		return err
	}
	c.mu.Lock()
	deps := c.deps[issueID]
	for i, d := range deps {
		if d.DependsOnID == dependsOnID {
			deps[i].Type = depType
			c.deps[issueID] = deps
			c.mu.Unlock()
			return nil
		}
	}
	c.deps[issueID] = append(deps, Dep{IssueID: issueID, DependsOnID: dependsOnID, Type: depType})
	c.mu.Unlock()
	return nil
}

// DepRemove removes a dependency and updates the cache.
func (c *CachingStore) DepRemove(issueID, dependsOnID string) error {
	if err := c.backing.DepRemove(issueID, dependsOnID); err != nil {
		return err
	}
	c.mu.Lock()
	deps := c.deps[issueID]
	for i, d := range deps {
		if d.DependsOnID == dependsOnID {
			c.deps[issueID] = append(deps[:i], deps[i+1:]...)
			break
		}
	}
	c.mu.Unlock()
	return nil
}

// ── Background reconciler ───────────────────────────────────────────

func (c *CachingStore) reconcileLoop(ctx context.Context) {
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}

		if c.reconciling.CompareAndSwap(false, true) {
			c.runReconciliation()
			c.reconciling.Store(false)
		}

		timer.Reset(c.adaptiveInterval())
	}
}

func (c *CachingStore) adaptiveInterval() time.Duration {
	c.mu.RLock()
	total := len(c.beads)
	c.mu.RUnlock()
	switch {
	case total >= 5000:
		return cacheReconcileIntervalLarge
	case total >= 1000:
		return cacheReconcileIntervalMedium
	default:
		return cacheReconcileIntervalSmall
	}
}

func (c *CachingStore) runReconciliation() {
	start := time.Now()
	fresh, err := c.backing.ListOpen()
	if err != nil {
		c.mu.Lock()
		c.syncFailures++
		if c.syncFailures >= maxCacheSyncFailures && c.state == cacheLive {
			c.state = cacheDegraded
			log.Printf("caching-store: degraded after %d consecutive failures", c.syncFailures)
		}
		c.mu.Unlock()
		log.Printf("caching-store: reconcile failed: %v", err)
		return
	}

	freshByID := make(map[string]Bead, len(fresh))
	for _, b := range fresh {
		freshByID[b.ID] = b
	}

	c.mu.Lock()

	var adds, removes, updates int64
	var changedIDs []string

	// Detect new and updated
	for id, fb := range freshByID {
		old, exists := c.beads[id]
		if !exists {
			c.beads[id] = cloneBead(fb)
			adds++
			changedIDs = append(changedIDs, id)
			c.notifyChangeLocked("bead.created", fb)
		} else if beadChanged(old, fb) {
			c.beads[id] = cloneBead(fb)
			updates++
			changedIDs = append(changedIDs, id)
			c.notifyChangeLocked("bead.updated", fb)
		}
	}

	// Detect removed
	for id, old := range c.beads {
		if _, exists := freshByID[id]; !exists {
			delete(c.beads, id)
			delete(c.deps, id)
			removes++
			c.notifyChangeLocked("bead.closed", old)
		}
	}

	c.syncFailures = 0
	c.syncedAt = time.Now()
	if c.state == cacheDegraded {
		c.state = cacheLive
		log.Printf("caching-store: recovered to live")
	}

	durMs := float64(time.Since(start).Microseconds()) / 1000.0
	c.stats.LastReconcileAt = time.Now()
	c.stats.LastReconcileMs = durMs
	c.stats.Adds += adds
	c.stats.Removes += removes
	c.stats.Updates += updates
	c.stats.SyncFailures = c.syncFailures
	c.updateStatsLocked()
	c.mu.Unlock()

	// Batch-refresh deps for changed beads (one subprocess call).
	if len(changedIDs) > 0 {
		if bdStore, ok := c.backing.(*BdStore); ok {
			if depMap, err := bdStore.DepListBatch(changedIDs); err == nil {
				c.mu.Lock()
				for id, deps := range depMap {
					c.deps[id] = slices.Clone(deps)
				}
				c.mu.Unlock()
			}
		} else {
			// Non-BdStore fallback: per-ID dep fetch.
			for _, id := range changedIDs {
				if deps, err := c.backing.DepList(id, "down"); err == nil {
					c.mu.Lock()
					c.deps[id] = slices.Clone(deps)
					c.mu.Unlock()
				}
			}
		}
	}

	if adds > 0 || removes > 0 || updates > 0 {
		log.Printf("caching-store: reconciled in %.0fms (+%d -%d ~%d, %d total)",
			durMs, adds, removes, updates, len(c.beads))
	}
}

func (c *CachingStore) notifyChange(eventType string, b Bead) {
	if c.onChange == nil {
		return
	}
	payload, _ := json.Marshal(b)
	c.onChange(eventType, b.ID, payload)
}

func (c *CachingStore) notifyChangeLocked(eventType string, b Bead) {
	if c.onChange == nil {
		return
	}
	payload, _ := json.Marshal(b)
	// Unlock before callback to avoid holding the lock during event recording.
	c.mu.Unlock()
	c.onChange(eventType, b.ID, payload)
	c.mu.Lock()
}

func (c *CachingStore) updateStatsLocked() {
	c.stats.TotalBeads = len(c.beads)
	totalDeps := 0
	for _, deps := range c.deps {
		totalDeps += len(deps)
	}
	c.stats.TotalDeps = totalDeps
}

func beadChanged(old, fresh Bead) bool {
	if old.Status != fresh.Status {
		return true
	}
	if old.Title != fresh.Title {
		return true
	}
	if old.Assignee != fresh.Assignee {
		return true
	}
	if old.Description != fresh.Description {
		return true
	}
	if len(old.Metadata) != len(fresh.Metadata) {
		return true
	}
	for k, v := range old.Metadata {
		if fresh.Metadata[k] != v {
			return true
		}
	}
	return len(old.Labels) != len(fresh.Labels)
}

// Delete passes through to the backing store and removes from cache.
func (c *CachingStore) Delete(id string) error {
	if err := c.backing.Delete(id); err != nil {
		return err
	}
	c.mu.Lock()
	delete(c.beads, id)
	delete(c.deps, id)
	c.mu.Unlock()
	return nil
}

// isPrePrimedStatus returns true for statuses loaded by PrimeActive.
func isPrePrimedStatus(status string) bool {
	return status == "open" || status == "in_progress"
}
