package beads

import "sync"

// localWritesInFlight counts a CachingStore's local writes from their start
// to their return: per id for a write that names its rows before its backing
// call; in creates for a create, whose one new row the cache does not hold;
// and in blind for any other write whose rows its backing call decides (Tx,
// a graph apply). Between a write's backing commit and its absorb, a backing
// read of the row sees the write while the cache does not hold it yet; a
// refresh that yields (RowRefreshOptions.YieldToLocalWrites) checks here and
// leaves the row to the write. It has its own lock, taken after c.mu, never
// before it.
//
// A blind write fences every row and a create every row the cache does not
// hold, so writes of that kind back to back (a bulk create, a convoy
// fan-out, Tx after Tx) can make every yielding refresh of such a row yield
// until they stop, and one whose backing call never returns stalls those
// rows for as long. The ledger feed bounds the wait (ledgerFeedYieldLimit)
// and falls back to a re-scan.
type localWritesInFlight struct {
	mu      sync.Mutex
	ids     map[string]int
	creates int
	blind   int
}

// mayTouch reports whether a write in flight may have written id, held
// saying whether the cache holds a row for id: a create writes only a row
// the cache does not hold.
func (w *localWritesInFlight) mayTouch(id string, held bool) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.blind > 0 || (w.creates > 0 && !held) || w.ids[id] > 0
}

// beginLocalWrite registers a local write of ids, or, with no ids, a write
// whose rows its backing call decides, and returns the mutation sequence at
// its start (the fence its install checks) and the func that ends it. The
// caller must call end once the write has absorbed its rows or failed,
// typically with defer.
func (c *CachingStore) beginLocalWrite(ids ...string) (startSeq uint64, end func()) {
	if len(ids) == 0 {
		return c.beginLocalWriteOf(&c.localWrites.blind, nil)
	}
	return c.beginLocalWriteOf(nil, ids)
}

// beginLocalCreate is beginLocalWrite for a create: the backing call decides
// its row, which is new, so the write fences only the rows the cache does
// not hold.
func (c *CachingStore) beginLocalCreate() (startSeq uint64, end func()) {
	return c.beginLocalWriteOf(&c.localWrites.creates, nil)
}

// beginLocalWriteOf registers a write in counter, or, with a nil counter,
// under ids, for beginLocalWrite and beginLocalCreate.
func (c *CachingStore) beginLocalWriteOf(counter *int, ids []string) (startSeq uint64, end func()) {
	startSeq = c.currentMutationSeq()
	w := &c.localWrites
	w.mu.Lock()
	if counter != nil {
		*counter++
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
			if counter != nil {
				*counter--
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
