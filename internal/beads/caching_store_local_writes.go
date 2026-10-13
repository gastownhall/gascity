package beads

import "sync"

// localWritesInFlight counts a CachingStore's local writes from their start
// to their return: per id for a write that names its rows before its backing
// call, and in blind for one whose rows its backing call decides (Create, Tx,
// a graph apply). Between a write's backing commit and its absorb, a backing
// read of the row sees the write while the cache does not hold it yet; a
// refresh that yields (RowRefreshOptions.YieldToLocalWrites) checks here and
// leaves the row to the write. It has its own lock, taken after c.mu, never
// before it.
type localWritesInFlight struct {
	mu    sync.Mutex
	ids   map[string]int
	blind int
}

// mayTouch reports whether a write in flight may have written id.
func (w *localWritesInFlight) mayTouch(id string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.blind > 0 || w.ids[id] > 0
}

// beginLocalWrite registers a local write of ids, or, with no ids, a write
// whose rows its backing call decides, and returns the mutation sequence at
// its start (the fence its install checks) and the func that ends it. The
// caller must call end once the write has absorbed its rows or failed,
// typically with defer.
func (c *CachingStore) beginLocalWrite(ids ...string) (startSeq uint64, end func()) {
	startSeq = c.currentMutationSeq()
	w := &c.localWrites
	w.mu.Lock()
	if len(ids) == 0 {
		w.blind++
	} else {
		if w.ids == nil {
			w.ids = make(map[string]int)
		}
		for _, id := range ids {
			w.ids[id]++
		}
	}
	w.mu.Unlock()
	var once sync.Once
	return startSeq, func() {
		once.Do(func() {
			w.mu.Lock()
			defer w.mu.Unlock()
			if len(ids) == 0 {
				w.blind--
				return
			}
			for _, id := range ids {
				if w.ids[id]--; w.ids[id] <= 0 {
					delete(w.ids, id)
				}
			}
		})
	}
}
