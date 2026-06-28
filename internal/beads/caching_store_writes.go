package beads

import (
	"errors"
	"fmt"
	"time"
)

// Create passes through to the backing store and updates the cache.
func (c *CachingStore) Create(b Bead) (Bead, error) {
	return c.createWith(func() (Bead, error) {
		return c.backing.Create(b)
	})
}

// CreateWithStorage passes through a policy-selected storage class to backing
// stores that support table-specific creates, then updates the cache.
func (c *CachingStore) CreateWithStorage(b Bead, storage StorageClass) (Bead, error) {
	storageBacking, ok := c.backing.(StorageCreateStore)
	if !ok {
		return c.Create(b)
	}
	return c.createWith(func() (Bead, error) {
		return storageBacking.CreateWithStorage(b, storage)
	})
}

func (c *CachingStore) createWith(create func() (Bead, error)) (Bead, error) {
	created, err := create()
	if err != nil {
		return created, err
	}

	if fresh, err := c.backing.Get(created.ID); err == nil {
		created = fresh
	} else if !errors.Is(err, ErrNotFound) {
		c.recordProblem("refresh bead after create", fmt.Errorf("%s: %w", created.ID, err))
	}

	c.mu.Lock()
	c.noteLocalMutationLocked(created.ID)
	c.beads[created.ID] = cloneBead(created)
	c.deps[created.ID] = depsFromBeadFields(created)
	delete(c.dirty, created.ID)
	delete(c.deletedSeq, created.ID)
	c.markFreshLocked(time.Now())
	c.updateStatsLocked()
	c.mu.Unlock()

	c.notifyChange("bead.created", created)
	return created, nil
}

// Update passes through to the backing store and refreshes the cache.
func (c *CachingStore) Update(id string, opts UpdateOpts) error {
	// Idempotence: if every non-nil field in opts already matches the
	// cached bead AND the cache is primed, the backing call is a no-op.
	// Skipping it avoids the bd subprocess invocation, the on_update
	// hook, and the post-update Get refresh — same payoff as the
	// SetMetadata short-circuit at metadataAlreadyMatchesCached.
	// See gastownhall/gascity#1978 Phase 1.
	if c.updateMatchesCached(id, opts) {
		return nil
	}
	if err := c.backing.Update(id, opts); err != nil {
		return err
	}

	// Re-fetch from backing to get the authoritative state.
	fresh, err := c.backing.Get(id)
	if err != nil {
		c.mu.Lock()
		seq := c.noteLocalMutationLocked(id)
		if errors.Is(err, ErrNotFound) {
			var closed Bead
			notifyClosed := false
			if current, ok := c.beads[id]; ok && current.Status != "closed" {
				closed = cloneBead(current)
				closed.Status = "closed"
				notifyClosed = true
			}
			delete(c.beads, id)
			delete(c.deps, id)
			delete(c.dirty, id)
			delete(c.beadSeq, id)
			delete(c.localBeadAt, id)
			c.deletedSeq[id] = seq
			c.clearDependentReadyProjectionsLocked(id)
			c.markFreshLocked(time.Now())
			c.updateStatsLocked()
			c.mu.Unlock()
			if notifyClosed {
				c.notifyChange("bead.closed", closed)
			}
			return nil
		}
		if current, ok := c.beads[id]; ok {
			fresh = applyUpdateOptsToBead(current, opts)
			c.beads[id] = cloneBead(fresh)
			c.deps[id] = depsFromBeadFields(fresh)
			if opts.Status != nil {
				c.clearDependentReadyProjectionsLocked(id)
			}
			c.dirty[id] = struct{}{}
			delete(c.deletedSeq, id)
			c.updateStatsLocked()
			c.mu.Unlock()
			c.recordProblem("refresh bead after update", fmt.Errorf("%s: %w", id, err))
			c.notifyChange("bead.updated", fresh)
			return nil
		}
		c.dirty[id] = struct{}{}
		c.mu.Unlock()
		c.recordProblem("refresh bead after update", fmt.Errorf("%s: %w", id, err))
		return nil
	}
	fresh = applyUpdateOptsToBead(fresh, opts)

	c.mu.Lock()
	c.noteLocalMutationLocked(id)
	c.beads[id] = cloneBead(fresh)
	c.deps[id] = depsFromBeadFields(fresh)
	if opts.Status != nil {
		c.clearDependentReadyProjectionsLocked(id)
	}
	delete(c.dirty, id)
	delete(c.deletedSeq, id)
	c.markFreshLocked(time.Now())
	c.updateStatsLocked()
	c.mu.Unlock()

	c.notifyChange("bead.updated", fresh)
	return nil
}

// ReleaseIfCurrent clears an in-progress assignment through the backing store
// and refreshes the cache only when the conditional release succeeds.
func (c *CachingStore) ReleaseIfCurrent(id, expectedAssignee string) (bool, error) {
	releaser, ok := c.backing.(ConditionalAssignmentReleaser)
	if !ok {
		return false, ErrConditionalReleaseUnsupported
	}
	released, err := releaser.ReleaseIfCurrent(id, expectedAssignee)
	if err != nil || !released {
		return released, err
	}

	fresh, refreshed := c.refreshBeadAfterWrite(id, "refresh bead after release-if-current")
	var updated Bead
	notify := false
	c.mu.Lock()
	c.noteLocalMutationLocked(id)
	if refreshed {
		c.beads[id] = cloneBead(fresh)
		c.deps[id] = depsFromBeadFields(fresh)
		delete(c.dirty, id)
		delete(c.deletedSeq, id)
		updated = cloneBead(fresh)
		notify = true
	} else if b, ok := c.beads[id]; ok {
		b.Status = "open"
		b.Assignee = ""
		b.UpdatedAt = time.Now()
		c.beads[id] = b
		c.dirty[id] = struct{}{}
		delete(c.deletedSeq, id)
		updated = cloneBead(b)
		notify = true
	} else {
		c.dirty[id] = struct{}{}
	}
	c.clearDependentReadyProjectionsLocked(id)
	c.markFreshLocked(time.Now())
	c.updateStatsLocked()
	c.mu.Unlock()
	if notify {
		c.notifyChange("bead.updated", updated)
	}
	return true, nil
}

// Close marks a bead as closed in the backing store and cache.
func (c *CachingStore) Close(id string) error {
	// Idempotence: if the cached bead status is already "closed" AND the
	// cache is primed, the backing call is a no-op. Skipping it avoids
	// the bd subprocess invocation, the on_update hook, and the
	// post-close Get refresh. See gastownhall/gascity#1978 Phase 1.
	if c.closeAlreadyMatchesCached(id) {
		return nil
	}
	if err := c.backing.Close(id); err != nil {
		return err
	}

	var closed Bead
	var found bool
	if fresh, err := c.backing.Get(id); err == nil {
		closed = fresh
		closed.Status = "closed"
		found = true
	} else if !errors.Is(err, ErrNotFound) {
		c.recordProblem("refresh bead after close", fmt.Errorf("%s: %w", id, err))
	}

	c.mu.Lock()
	c.noteLocalMutationLocked(id)
	if b, ok := c.beads[id]; ok {
		b.Status = "closed"
		c.beads[id] = b
		delete(c.dirty, id)
		delete(c.deletedSeq, id)
		closed = cloneBead(b)
		found = true
	} else if found {
		c.beads[id] = cloneBead(closed)
		delete(c.dirty, id)
		delete(c.deletedSeq, id)
	}
	dependentProjectionCleared := c.clearDependentReadyProjectionsLocked(id)
	if found || dependentProjectionCleared {
		c.markFreshLocked(time.Now())
		c.updateStatsLocked()
	}
	c.mu.Unlock()

	if found {
		c.notifyChange("bead.closed", closed)
	}
	return nil
}

// Reopen marks a bead as open in the backing store and cache.
func (c *CachingStore) Reopen(id string) error {
	if err := c.backing.Reopen(id); err != nil {
		return err
	}

	var reopened Bead
	var found bool
	if fresh, err := c.backing.Get(id); err == nil {
		reopened = fresh
		reopened.Status = "open"
		found = true
	} else if !errors.Is(err, ErrNotFound) {
		c.recordProblem("refresh bead after reopen", fmt.Errorf("%s: %w", id, err))
	}

	c.mu.Lock()
	c.noteLocalMutationLocked(id)
	if b, ok := c.beads[id]; ok {
		b.Status = "open"
		c.beads[id] = b
		delete(c.dirty, id)
		delete(c.deletedSeq, id)
		reopened = cloneBead(b)
		found = true
	} else if found {
		c.beads[id] = cloneBead(reopened)
		delete(c.dirty, id)
		delete(c.deletedSeq, id)
	}
	dependentProjectionCleared := c.clearDependentReadyProjectionsLocked(id)
	if found || dependentProjectionCleared {
		c.markFreshLocked(time.Now())
		c.updateStatsLocked()
	}
	c.mu.Unlock()

	if found {
		c.notifyChange("bead.updated", reopened)
	}
	return nil
}

// CloseAll closes multiple beads and sets metadata on each.
func (c *CachingStore) CloseAll(ids []string, metadata map[string]string) (int, error) {
	n, err := c.backing.CloseAll(ids, metadata)
	if err != nil && n == 0 {
		return n, err
	}

	type refreshedBead struct {
		id   string
		bead Bead
	}
	refreshed := make([]refreshedBead, 0, len(ids))
	var refreshErr error
	refreshFailed := make(map[string]struct{})
	for _, id := range ids {
		fresh, getErr := c.backing.Get(id)
		if getErr != nil {
			refreshFailed[id] = struct{}{}
			refreshErr = errors.Join(refreshErr, fmt.Errorf("refresh bead after close-all %s: %w", id, getErr))
			continue
		}
		refreshed = append(refreshed, refreshedBead{id: id, bead: fresh})
	}

	notifications := make([]cacheNotification, 0, len(refreshed))
	c.mu.Lock()
	c.noteLocalMutationLocked(ids...)
	if refreshErr != nil {
		c.recordProblemLocked("close-all refresh", refreshErr)
	}
	for id := range refreshFailed {
		c.dirty[id] = struct{}{}
	}
	for _, item := range refreshed {
		previous, hadPrevious := c.beads[item.id]
		c.beads[item.id] = cloneBead(item.bead)
		delete(c.dirty, item.id)
		delete(c.deletedSeq, item.id)
		if item.bead.Status == "closed" {
			delete(c.deps, item.id)
			c.clearDependentReadyProjectionsLocked(item.id)
		}
		if hadPrevious && previous.Status != "closed" && item.bead.Status == "closed" {
			notifications = append(notifications, cacheNotification{
				eventType: "bead.closed",
				bead:      cloneBead(item.bead),
			})
		}
	}
	c.markFreshLocked(time.Now())
	c.updateStatsLocked()
	c.mu.Unlock()
	c.notifyChanges(notifications)
	return n, errors.Join(err, refreshErr)
}

// SetMetadata sets a single metadata key-value on a bead.
func (c *CachingStore) SetMetadata(id, key, value string) error {
	// Idempotence: if the cached bead already has metadata[key] == value,
	// the backing call is a no-op semantically. Skipping it avoids the
	// bd subprocess invocation and — crucially — avoids firing bd's
	// on_update hook, which calls "gc event emit bead.updated" and
	// appends a line to the city's events.jsonl. Reconciler tick logic
	// repeatedly writes the same heartbeat / deferral fields every ~2s,
	// producing thousands of no-op events per hour. The cache is the
	// supervisor's authoritative read source, so a value-match here is
	// a value-match in the store.
	if c.metadataAlreadyMatchesCached(id, map[string]string{key: value}) {
		return nil
	}
	if err := c.backing.SetMetadata(id, key, value); err != nil {
		return err
	}

	fresh, refreshed := c.refreshBeadAfterWrite(id, "refresh bead after metadata")
	var updated Bead
	notify := false
	c.mu.Lock()
	c.noteLocalMutationLocked(id)
	if refreshed {
		c.beads[id] = cloneBead(fresh)
		c.deps[id] = depsFromBeadFields(fresh)
		delete(c.dirty, id)
		delete(c.deletedSeq, id)
		updated = cloneBead(fresh)
		notify = true
	} else if b, ok := c.beads[id]; ok {
		if b.Metadata == nil {
			b.Metadata = make(map[string]string)
		}
		b.Metadata[key] = value
		c.beads[id] = b
		delete(c.dirty, id)
		delete(c.deletedSeq, id)
		updated = cloneBead(b)
		notify = true
	} else {
		c.dirty[id] = struct{}{}
	}
	c.markFreshLocked(time.Now())
	c.updateStatsLocked()
	c.mu.Unlock()
	if notify {
		c.notifyChange("bead.updated", updated)
	}
	return nil
}

// SetMetadataBatch sets multiple metadata key-values on a bead.
func (c *CachingStore) SetMetadataBatch(id string, kvs map[string]string) error {
	if len(kvs) == 0 {
		return nil
	}
	// Idempotence: see SetMetadata. If every kv pair already matches the
	// cached bead's metadata, skip the backing write — no bd subprocess,
	// no on_update hook fire, no events.jsonl entry. Reconciler ticks
	// re-stamp deferral timestamps and other "I observed this" markers
	// on every cycle; without this guard each cycle generates a
	// bead.updated event even when nothing changed.
	if c.metadataAlreadyMatchesCached(id, kvs) {
		return nil
	}
	if err := c.backing.SetMetadataBatch(id, kvs); err != nil {
		return err
	}

	fresh, refreshed := c.refreshBeadAfterWrite(id, "refresh bead after metadata batch")
	var updated Bead
	notify := false
	c.mu.Lock()
	c.noteLocalMutationLocked(id)
	if refreshed {
		c.beads[id] = cloneBead(fresh)
		c.deps[id] = depsFromBeadFields(fresh)
		delete(c.dirty, id)
		delete(c.deletedSeq, id)
		updated = cloneBead(fresh)
		notify = true
	} else if b, ok := c.beads[id]; ok {
		if b.Metadata == nil {
			b.Metadata = make(map[string]string, len(kvs))
		}
		for k, v := range kvs {
			b.Metadata[k] = v
		}
		c.beads[id] = b
		delete(c.dirty, id)
		delete(c.deletedSeq, id)
		updated = cloneBead(b)
		notify = true
	} else {
		c.dirty[id] = struct{}{}
	}
	c.markFreshLocked(time.Now())
	c.updateStatsLocked()
	c.mu.Unlock()
	if notify {
		c.notifyChange("bead.updated", updated)
	}
	return nil
}

func (c *CachingStore) refreshBeadAfterWrite(id, op string) (Bead, bool) {
	fresh, err := c.backing.Get(id)
	if err != nil {
		c.recordProblem(op, fmt.Errorf("%s: %w", id, err))
		return Bead{}, false
	}
	return fresh, true
}

func (c *CachingStore) refreshBeadWithDepsAfterWrite(id, op string) (Bead, []Dep, bool) {
	fresh, ok := c.refreshBeadAfterWrite(id, op)
	if !ok {
		return Bead{}, nil, false
	}
	deps, err := c.backing.DepList(id, "down")
	if err != nil {
		c.recordProblem(op+" deps", fmt.Errorf("%s: %w", id, err))
		fresh.Dependencies = depsFromBeadFields(fresh)
		return fresh, cloneDeps(fresh.Dependencies), true
	}
	fresh.Dependencies = cloneDeps(deps)
	fresh.Needs = nil
	return fresh, cloneDeps(deps), true
}

// Tx executes fn through the backing store transaction and refreshes touched
// cache entries after a successful commit.
func (c *CachingStore) Tx(commitMsg string, fn func(Tx) error) error {
	if fn == nil {
		return errors.New("beads tx: nil callback")
	}
	tx := newCachingStoreTx()
	if err := c.backing.Tx(commitMsg, func(backingTx Tx) error {
		tx.backing = backingTx
		return fn(tx)
	}); err != nil {
		return err
	}
	c.refreshTxTouchedBeads(tx.ids, tx.closed)
	return nil
}

type cachingStoreTx struct {
	backing Tx
	seen    map[string]struct{}
	closed  map[string]struct{}
	ids     []string
}

func newCachingStoreTx() *cachingStoreTx {
	return &cachingStoreTx{
		seen:   make(map[string]struct{}),
		closed: make(map[string]struct{}),
	}
}

func (tx *cachingStoreTx) Create(b Bead) (Bead, error) {
	created, err := tx.backing.Create(b)
	if err != nil {
		return Bead{}, err
	}
	tx.touch(created.ID)
	return created, nil
}

func (tx *cachingStoreTx) Update(id string, opts UpdateOpts) error {
	if err := tx.backing.Update(id, opts); err != nil {
		return err
	}
	tx.touch(id)
	return nil
}

func (tx *cachingStoreTx) SetMetadataBatch(id string, kvs map[string]string) error {
	if len(kvs) == 0 {
		return nil
	}
	if err := tx.backing.SetMetadataBatch(id, kvs); err != nil {
		return err
	}
	tx.touch(id)
	return nil
}

func (tx *cachingStoreTx) Close(id string) error {
	if err := tx.backing.Close(id); err != nil {
		return err
	}
	tx.touch(id)
	tx.closed[id] = struct{}{}
	return nil
}

func (tx *cachingStoreTx) touch(id string) {
	if id == "" {
		return
	}
	if _, ok := tx.seen[id]; ok {
		return
	}
	tx.seen[id] = struct{}{}
	tx.ids = append(tx.ids, id)
}

type txTouchedBead struct {
	id     string
	bead   Bead
	found  bool
	closed bool
	err    error
}

func (c *CachingStore) refreshTxTouchedBeads(ids []string, closed map[string]struct{}) {
	if len(ids) == 0 {
		return
	}

	refreshed := make([]txTouchedBead, 0, len(ids))
	var refreshErr error
	for _, id := range ids {
		_, wasClosed := closed[id]
		fresh, err := c.backing.Get(id)
		item := txTouchedBead{id: id, closed: wasClosed, err: err}
		if err == nil {
			item.bead = fresh
			item.found = true
		} else if !wasClosed || !errors.Is(err, ErrNotFound) {
			refreshErr = errors.Join(refreshErr, fmt.Errorf("refresh bead after tx %s: %w", id, err))
		}
		refreshed = append(refreshed, item)
	}

	notifications := make([]cacheNotification, 0, len(refreshed))
	now := time.Now()
	c.mu.Lock()
	c.noteLocalMutationLocked(ids...)
	if refreshErr != nil {
		c.recordProblemLocked("tx refresh", refreshErr)
	}
	for _, item := range refreshed {
		if item.found {
			previous, hadPrevious := c.beads[item.id]
			fresh := cloneBead(item.bead)
			statusChanged := item.closed || fresh.Status == "closed"
			if hadPrevious && previous.Status != fresh.Status {
				statusChanged = true
			}
			c.beads[item.id] = fresh
			c.deps[item.id] = depsFromBeadFields(fresh)
			delete(c.dirty, item.id)
			delete(c.deletedSeq, item.id)
			if statusChanged {
				c.clearDependentReadyProjectionsLocked(item.id)
			}
			eventType := "bead.updated"
			if fresh.Status == "closed" {
				eventType = "bead.closed"
			}
			if !hadPrevious || beadChanged(previous, fresh, false) || fresh.Status == "closed" {
				notifications = append(notifications, cacheNotification{
					eventType: eventType,
					bead:      cloneBead(fresh),
				})
			}
			continue
		}
		if item.closed {
			if b, ok := c.beads[item.id]; ok {
				b.Status = "closed"
				c.beads[item.id] = b
				delete(c.dirty, item.id)
				delete(c.deletedSeq, item.id)
				c.clearDependentReadyProjectionsLocked(item.id)
				notifications = append(notifications, cacheNotification{
					eventType: "bead.closed",
					bead:      cloneBead(b),
				})
			}
			continue
		}
		if item.err != nil {
			c.dirty[item.id] = struct{}{}
		}
	}
	c.markFreshLocked(now)
	c.updateStatsLocked()
	c.mu.Unlock()

	c.notifyChanges(notifications)
}

// updateMatchesCached returns true when every non-nil field in opts already
// reflects the cached bead's state AND the cache is primed. Returns false on
// cache miss, uninitialized cache, or any field mismatch — in which case the
// caller falls through to the backing write. Companion to
// metadataAlreadyMatchesCached but covers the full UpdateOpts surface
// (Title, Status, Type, Priority, Description, ParentID, Assignee, Metadata,
// Labels, RemoveLabels). See gastownhall/gascity#1978 Phase 1.
//
// The short-circuit path skips the deduplication that
// applyUpdateOptsToBead performs on the non-short-circuit pass. Cached
// bead labels come from bd/dolt's canonical state, which never produces
// duplicates, so a Labels-equal match here is a Labels-equal match in
// the store after applyUpdateOptsToBead would have run. If a future
// path injects duplicate labels into the cache, this short-circuit
// would skip the dedup-fixup — file an issue rather than relaxing the
// invariant here.
func (c *CachingStore) updateMatchesCached(id string, opts UpdateOpts) bool {
	if id == "" {
		return false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.state != cacheLive && c.state != cachePartial {
		return false
	}
	if _, dirty := c.dirty[id]; dirty {
		return false
	}
	b, ok := c.beads[id]
	if !ok {
		return false
	}
	if opts.Title != nil && b.Title != *opts.Title {
		return false
	}
	if opts.Status != nil && b.Status != *opts.Status {
		return false
	}
	if opts.Type != nil && b.Type != *opts.Type {
		return false
	}
	if opts.Priority != nil {
		if b.Priority == nil || *b.Priority != *opts.Priority {
			return false
		}
	}
	if opts.Description != nil && b.Description != *opts.Description {
		return false
	}
	if opts.ParentID != nil && b.ParentID != *opts.ParentID {
		return false
	}
	if opts.Assignee != nil && b.Assignee != *opts.Assignee {
		return false
	}
	for k, v := range opts.Metadata {
		if b.Metadata == nil {
			if v != "" {
				return false
			}
			continue
		}
		if b.Metadata[k] != v {
			return false
		}
	}
	if len(opts.Labels) > 0 || len(opts.RemoveLabels) > 0 {
		// Set-equality check: opts.Labels ⊆ existing AND
		// (opts.RemoveLabels ∩ existing) = ∅ implies the final label set
		// after applyUpdateOptsToBead equals the current set. We skip
		// that function's dedup pass here — see the doc comment above
		// for why that's safe under bd/dolt's canonical labels.
		existing := make(map[string]struct{}, len(b.Labels))
		for _, l := range b.Labels {
			existing[l] = struct{}{}
		}
		for _, l := range opts.Labels {
			if _, present := existing[l]; !present {
				return false
			}
		}
		for _, l := range opts.RemoveLabels {
			if _, present := existing[l]; present {
				return false
			}
		}
	}
	return true
}

// closeAlreadyMatchesCached returns true when the cached bead status is
// already "closed" AND the cache is primed. Returns false on cache miss or
// uninitialized cache. See gastownhall/gascity#1978 Phase 1.
func (c *CachingStore) closeAlreadyMatchesCached(id string) bool {
	if id == "" {
		return false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.state != cacheLive && c.state != cachePartial {
		return false
	}
	if _, dirty := c.dirty[id]; dirty {
		return false
	}
	b, ok := c.beads[id]
	if !ok {
		return false
	}
	return b.Status == "closed"
}

// metadataAlreadyMatchesCached returns true when the cache holds a primed
// copy of the bead and every key/value in kvs is already present with the
// same value. A cache miss returns false (we cannot prove no-op), so the
// caller falls through to the backing write. Empty maps (no keys) match
// trivially, but callers should handle len==0 explicitly to avoid acquiring
// the lock for a guaranteed no-op.
func (c *CachingStore) metadataAlreadyMatchesCached(id string, kvs map[string]string) bool {
	if id == "" {
		return false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.state != cacheLive && c.state != cachePartial {
		return false
	}
	if _, dirty := c.dirty[id]; dirty {
		return false
	}
	b, ok := c.beads[id]
	if !ok {
		return false
	}
	if b.Metadata == nil {
		// Cache has the bead but no metadata map — any non-empty value
		// would be a write; an empty value (clearing a never-set key)
		// is already the desired state.
		for _, v := range kvs {
			if v != "" {
				return false
			}
		}
		return true
	}
	for k, v := range kvs {
		if b.Metadata[k] != v {
			return false
		}
	}
	return true
}

// DepAdd adds a dependency and updates the cache.
func (c *CachingStore) DepAdd(issueID, dependsOnID, depType string) error {
	if err := c.backing.DepAdd(issueID, dependsOnID, depType); err != nil {
		return err
	}

	fresh, deps, refreshed := c.refreshBeadWithDepsAfterWrite(issueID, "refresh bead after dependency add")
	c.mu.Lock()
	c.noteLocalMutationLocked(issueID)
	if refreshed {
		c.beads[issueID] = cloneBead(fresh)
		c.deps[issueID] = cloneDeps(deps)
		c.clearReadyProjectionLocked(issueID)
		delete(c.dirty, issueID)
		delete(c.deletedSeq, issueID)
		c.markFreshLocked(time.Now())
		c.updateStatsLocked()
		c.mu.Unlock()
		c.notifyChange("bead.updated", fresh)
		return nil
	}
	if !c.depsComplete {
		if _, known := c.deps[issueID]; !known {
			delete(c.dirty, issueID)
			delete(c.deletedSeq, issueID)
			c.markFreshLocked(time.Now())
			c.updateStatsLocked()
			c.mu.Unlock()
			return nil
		}
	}
	cachedDeps := c.deps[issueID]
	for i, d := range cachedDeps {
		if d.DependsOnID == dependsOnID {
			cachedDeps[i].Type = depType
			c.deps[issueID] = cachedDeps
			c.clearReadyProjectionLocked(issueID)
			delete(c.dirty, issueID)
			delete(c.deletedSeq, issueID)
			c.markFreshLocked(time.Now())
			c.updateStatsLocked()
			c.mu.Unlock()
			return nil
		}
	}
	c.deps[issueID] = append(cachedDeps, Dep{IssueID: issueID, DependsOnID: dependsOnID, Type: depType})
	c.clearReadyProjectionLocked(issueID)
	delete(c.dirty, issueID)
	delete(c.deletedSeq, issueID)
	c.markFreshLocked(time.Now())
	c.updateStatsLocked()
	c.mu.Unlock()
	return nil
}

// DepRemove removes a dependency and updates the cache.
func (c *CachingStore) DepRemove(issueID, dependsOnID string) error {
	if err := c.backing.DepRemove(issueID, dependsOnID); err != nil {
		return err
	}

	fresh, deps, refreshed := c.refreshBeadWithDepsAfterWrite(issueID, "refresh bead after dependency remove")
	c.mu.Lock()
	c.noteLocalMutationLocked(issueID)
	if refreshed {
		c.beads[issueID] = cloneBead(fresh)
		c.deps[issueID] = cloneDeps(deps)
		c.clearReadyProjectionLocked(issueID)
		delete(c.dirty, issueID)
		delete(c.deletedSeq, issueID)
		c.markFreshLocked(time.Now())
		c.updateStatsLocked()
		c.mu.Unlock()
		c.notifyChange("bead.updated", fresh)
		return nil
	}
	if !c.depsComplete {
		if _, known := c.deps[issueID]; !known {
			delete(c.dirty, issueID)
			delete(c.deletedSeq, issueID)
			c.markFreshLocked(time.Now())
			c.updateStatsLocked()
			c.mu.Unlock()
			return nil
		}
	}
	cachedDeps := c.deps[issueID]
	for i, d := range cachedDeps {
		if d.DependsOnID == dependsOnID {
			c.deps[issueID] = append(cachedDeps[:i], cachedDeps[i+1:]...)
			c.clearReadyProjectionLocked(issueID)
			delete(c.dirty, issueID)
			delete(c.deletedSeq, issueID)
			break
		}
	}
	c.markFreshLocked(time.Now())
	c.updateStatsLocked()
	c.mu.Unlock()
	return nil
}

// Delete passes through to the backing store and removes from cache.
func (c *CachingStore) Delete(id string) error {
	deleted, haveDeleted := c.snapshotBeadBeforeDelete(id)
	if err := c.backing.Delete(id); err != nil {
		return err
	}

	c.mu.Lock()
	seq := c.noteLocalMutationLocked(id)
	delete(c.beads, id)
	delete(c.deps, id)
	delete(c.dirty, id)
	delete(c.beadSeq, id)
	delete(c.localBeadAt, id)
	c.deletedSeq[id] = seq
	c.clearDependentReadyProjectionsLocked(id)
	c.markFreshLocked(time.Now())
	c.updateStatsLocked()
	c.mu.Unlock()
	if haveDeleted {
		c.notifyChange("bead.deleted", deleted)
	}
	return nil
}

func (c *CachingStore) snapshotBeadBeforeDelete(id string) (Bead, bool) {
	deleted, err := c.backing.Get(id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return Bead{}, false
		}
		c.recordProblem("snapshot bead before delete", fmt.Errorf("%s: %w", id, err))
		return Bead{}, false
	}
	return deleted, true
}

func applyUpdateOptsToBead(bead Bead, opts UpdateOpts) Bead {
	if opts.Title != nil {
		bead.Title = *opts.Title
	}
	if opts.Status != nil {
		bead.Status = *opts.Status
	}
	if opts.Type != nil {
		bead.Type = *opts.Type
	}
	if opts.Priority != nil {
		bead.Priority = cloneIntPtr(opts.Priority)
	}
	if opts.Description != nil {
		bead.Description = *opts.Description
	}
	if opts.ParentID != nil {
		bead.ParentID = *opts.ParentID
	}
	if opts.Assignee != nil {
		bead.Assignee = *opts.Assignee
	}
	if len(opts.Metadata) > 0 {
		if bead.Metadata == nil {
			bead.Metadata = make(map[string]string, len(opts.Metadata))
		}
		for key, value := range opts.Metadata {
			bead.Metadata[key] = value
		}
	}
	if len(opts.Labels) > 0 || len(opts.RemoveLabels) > 0 {
		remove := make(map[string]struct{}, len(opts.RemoveLabels))
		for _, label := range opts.RemoveLabels {
			remove[label] = struct{}{}
		}

		labels := make([]string, 0, len(bead.Labels)+len(opts.Labels))
		seen := make(map[string]struct{}, len(bead.Labels)+len(opts.Labels))
		for _, label := range bead.Labels {
			if _, drop := remove[label]; drop {
				continue
			}
			if _, exists := seen[label]; exists {
				continue
			}
			labels = append(labels, label)
			seen[label] = struct{}{}
		}
		for _, label := range opts.Labels {
			if _, drop := remove[label]; drop {
				continue
			}
			if _, exists := seen[label]; exists {
				continue
			}
			labels = append(labels, label)
			seen[label] = struct{}{}
		}
		bead.Labels = labels
	}
	return bead
}
