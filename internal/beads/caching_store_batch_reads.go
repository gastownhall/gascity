package beads

// The CachingStore's batched reads.
//
// The cache wraps its backing store as a concrete type, so an optional
// capability of the backing is invisible to a caller holding the cache unless
// the cache forwards it. Without these forwards, a cached bd store answered a
// molecule walk with one `bd list --parent` (and one `bd query` for the wisp
// tier) per member, a close-order read with one `bd dep list` per member, and
// a CloseAll refresh with one `bd show` per closed bead: about a hundred bd
// subprocesses to cancel a run of sixteen beads, which took 81 s on a loaded
// two-vCPU host.

var (
	_ ChildrenBatchLister   = (*CachingStore)(nil)
	_ DependencyBatchLister = (*CachingStore)(nil)
)

// ChildrenOfAny reads the children of every parent through the backing
// store's batched read when it has one, and through this store's own
// Children, one parent at a time, otherwise.
//
// The batched rows install into the cache exactly as Children's rows do: each
// parent's children go through the refresh a List over that ParentID runs, so
// a cached child the backing no longer lists under that parent is re-read and
// the listed rows are absorbed under the same fences. A row whose ParentID
// names none of the requested parents is returned as the backing answered it
// and not installed, since no per-parent List would have read it.
func (c *CachingStore) ChildrenOfAny(parentIDs []string, opts ...QueryOpt) ([]Bead, error) {
	if len(parentIDs) == 0 {
		return nil, nil
	}
	batch, ok := c.backing.(ChildrenBatchLister)
	if !ok {
		return ChildrenOfAnyPerParent(c, parentIDs, opts...)
	}
	c.mu.RLock()
	startSeq, startScan := c.mutationSeq, c.scanGen
	c.mu.RUnlock()
	items, err := batch.ChildrenOfAny(parentIDs, opts...)
	if err != nil {
		return nil, err
	}

	requested := make(map[string]bool, len(parentIDs))
	for _, id := range parentIDs {
		requested[id] = true
	}
	byParent := make(map[string][]Bead, len(parentIDs))
	var unrequested []Bead
	for _, item := range items {
		if requested[item.ParentID] {
			byParent[item.ParentID] = append(byParent[item.ParentID], item)
			continue
		}
		unrequested = append(unrequested, item)
	}

	tier := TierModeFromOpts(opts)
	out := make([]Bead, 0, len(items))
	refreshed := make(map[string]bool, len(parentIDs))
	for _, parentID := range parentIDs {
		if refreshed[parentID] {
			continue
		}
		refreshed[parentID] = true
		query := ListQuery{
			ParentID:      parentID,
			IncludeClosed: true,
			Sort:          SortCreatedAsc,
			TierMode:      tier,
		}
		out = append(out, c.refreshCachedBeads(query, startSeq, startScan, byParent[parentID])...)
	}
	out = append(out, unrequested...)
	// As a ParentID List does: a batched read is often the first reader to
	// see a close or a claim made by another process; announce what it
	// installed.
	c.announceReadInstalls()
	return out, nil
}

// DepListBatch reads the DOWN edges of many anchors, answering each anchor
// exactly as DepList(id, "down") on this store would.
//
// A live cache whose edge set is complete answers the anchors it holds from
// memory, as DepList does, and asks the backing store's batched read for the
// rest in one round trip. Otherwise the whole batch goes to the backing store.
// A backing store without the batched read, or whose backend refuses it,
// reports ErrDepListBatchUnsupported, which callers treat as the cue to fall
// back to DepList per anchor (served from memory where this cache holds it).
func (c *CachingStore) DepListBatch(ids []string) (map[string][]Dep, error) {
	batch, ok := DepListBatchFor(c.backing)
	if !ok {
		return nil, ErrDepListBatchUnsupported
	}
	deps := make(map[string][]Dep, len(ids))
	misses := ids
	c.mu.RLock()
	if c.state == cacheLive && c.depsComplete {
		misses = make([]string, 0, len(ids))
		for _, id := range ids {
			if cached, held := c.deps[id]; held {
				deps[id] = cloneDeps(cached)
				continue
			}
			misses = append(misses, id)
		}
	}
	c.mu.RUnlock()
	if len(misses) == 0 {
		return deps, nil
	}
	fetched, err := batch.DepListBatch(misses)
	if err != nil {
		return nil, err
	}
	for id, edges := range fetched {
		deps[id] = edges
	}
	return deps, nil
}

// backingRead is one id's answer from readBackingRows: the row, or the error
// Get returned for it.
type backingRead struct {
	bead Bead
	err  error
}

// readBackingRows reads ids from the backing store, answering each one as
// backing.Get(id) would. A backing store with an exact batched read answers
// them in one round trip (PrefetchExact); every id the batch does not answer
// exactly, and every id when the batch read fails, is read with Get, so a
// missing bead, a substring collision and a transient failure each surface
// with Get's own error.
func (c *CachingStore) readBackingRows(ids []string) []backingRead {
	reads := make([]backingRead, len(ids))
	prefetched, err := PrefetchExact(c.backing, ids)
	if err != nil {
		prefetched = nil
	}
	for i, id := range ids {
		if bead, ok := prefetched[id]; ok {
			reads[i] = backingRead{bead: bead}
			continue
		}
		bead, err := c.backing.Get(id)
		reads[i] = backingRead{bead: bead, err: err}
	}
	return reads
}
