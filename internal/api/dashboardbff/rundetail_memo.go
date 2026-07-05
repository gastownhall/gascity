package dashboardbff

import (
	"container/list"
	"sync"

	"github.com/gastownhall/gascity/internal/runproj"
)

// runDetailMemoCap bounds the per-tailer detail memo. It holds the actively
// viewed runs of one city; a dashboard shows a handful of runs at a time, so a
// small cap covers the working set while an unbounded map would pin every run
// ever opened. A var (not a const) so a test can shrink it to force eviction.
var runDetailMemoCap = 128

// runDetailMemoKey identifies one built-and-marshaled run detail by everything
// that determines its bytes. A change to ANY field yields a new key → a miss →
// a rebuild, so invalidation is implicit (no explicit purge beyond LRU
// eviction).
//
//   - runID + lastSeq pin the fold generation: lastSeq is the tailer's monotonic
//     publish cursor, bumped on every build(), and the warm bead slice is
//     published under the same lock as lastSeq, so equal lastSeq ⇒ equal beads.
//   - sessionsVersion is the sessions cache entry's version (0 when sessions are
//     unavailable, so an availability flip changes the key).
//   - formulaVersion + formulaFailure pin the compiled-formula enrichment: the
//     version identifies an available compiled detail (immutable per version),
//     and the failure string distinguishes the unavailable arms (not_found vs
//     upstream_error) that also change the built output.
type runDetailMemoKey struct {
	runID           string
	lastSeq         uint64
	sessionsVersion uint64
	formulaVersion  uint64
	formulaFailure  runproj.RunFormulaDetailFetchFailure
}

// runDetailMemoValue is the immutable-after-build memoized detail: the projected
// DTO (returned to the detail() callers that need the struct) and its marshaled
// JSON (served verbatim by the GET handler to skip a re-marshal). Neither is
// mutated after store, so both are safe to share read-only across callers.
type runDetailMemoValue struct {
	detail runproj.FormulaRunDetail
	bytes  []byte
}

// runDetailMemo is a small bounded LRU of built run details keyed by fold
// generation + enrichment versions, with single-flight so concurrent misses for
// the same key build exactly once. It is safe under -race: the mutex guards the
// map, the eviction list, and the in-flight table, and is NEVER held across the
// build/marshal that produces a value (the samplers.go contract). The stored
// value is immutable, so a reader shares it without copying.
type runDetailMemo struct {
	mu       sync.Mutex
	cap      int
	entries  map[runDetailMemoKey]*list.Element
	lru      *list.List // front = most recently used
	inflight map[runDetailMemoKey]chan struct{}
}

// runDetailMemoEntry is one LRU node: the key (so eviction can delete the map
// entry) and the shared value.
type runDetailMemoEntry struct {
	key   runDetailMemoKey
	value runDetailMemoValue
}

// newRunDetailMemo builds an empty memo bounded to runDetailMemoCap.
func newRunDetailMemo() *runDetailMemo {
	return &runDetailMemo{
		cap:      runDetailMemoCap,
		entries:  make(map[runDetailMemoKey]*list.Element),
		lru:      list.New(),
		inflight: make(map[runDetailMemoKey]chan struct{}),
	}
}

// getOrBuild returns the memoized value for key, building it via build on a miss.
// Concurrent callers for the same key collapse onto one build: the elected
// caller runs build with NO memo lock held (the samplers.go contract); joiners
// wait and then re-read the now-stored value. build is invoked at most once per
// generation; a build error is NOT cached (the next caller re-elects and
// retries), so a transient projection failure never pins a run.
func (m *runDetailMemo) getOrBuild(key runDetailMemoKey, build func() (runDetailMemoValue, error)) (runDetailMemoValue, error) {
	for {
		m.mu.Lock()
		if el, ok := m.entries[key]; ok {
			m.lru.MoveToFront(el)
			v := el.Value.(*runDetailMemoEntry).value
			m.mu.Unlock()
			return v, nil
		}
		if wait, building := m.inflight[key]; building {
			m.mu.Unlock()
			<-wait
			// Re-loop: read the value the builder stored, or re-elect if the build
			// failed and left no entry.
			continue
		}
		// We are the elected builder for this key.
		done := make(chan struct{})
		m.inflight[key] = done
		m.mu.Unlock()

		var (
			value    runDetailMemoValue
			buildErr error
		)
		func() {
			// Release the in-flight handshake on every exit — including a build
			// panic — so a panicking build never orphans the channel and wedges
			// every future caller for this key. On success store under the lock;
			// on failure leave no entry so the next caller re-elects.
			defer func() {
				m.mu.Lock()
				if buildErr == nil {
					m.storeLocked(key, value)
				}
				delete(m.inflight, key)
				m.mu.Unlock()
				close(done)
			}()
			value, buildErr = build()
		}()
		if buildErr != nil {
			return runDetailMemoValue{}, buildErr
		}
		return value, nil
	}
}

// storeLocked inserts value under key as most-recently-used and evicts the
// least-recently-used entry when the cap is exceeded. The caller holds m.mu.
func (m *runDetailMemo) storeLocked(key runDetailMemoKey, value runDetailMemoValue) {
	if el, ok := m.entries[key]; ok {
		el.Value.(*runDetailMemoEntry).value = value
		m.lru.MoveToFront(el)
		return
	}
	el := m.lru.PushFront(&runDetailMemoEntry{key: key, value: value})
	m.entries[key] = el
	if m.cap > 0 && m.lru.Len() > m.cap {
		if oldest := m.lru.Back(); oldest != nil {
			m.lru.Remove(oldest)
			delete(m.entries, oldest.Value.(*runDetailMemoEntry).key)
		}
	}
}
