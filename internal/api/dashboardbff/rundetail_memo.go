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

// runDetailMemo is the per-tailer LRU of built run details keyed by fold
// generation + enrichment versions.
type runDetailMemo = lruSingleFlight[runDetailMemoKey, runDetailMemoValue]

// newRunDetailMemo builds an empty detail memo bounded to runDetailMemoCap.
func newRunDetailMemo() *runDetailMemo {
	return newLRUSingleFlight[runDetailMemoKey, runDetailMemoValue](runDetailMemoCap)
}

// runSnapshotCacheCap bounds the per-tailer run-snapshot cache. Like the detail
// memo it holds only the actively viewed runs' folded snapshots, so a small cap
// covers the working set while an unbounded map would pin every run ever opened.
// A var (not a const) so a test can shrink it to force eviction.
var runSnapshotCacheCap = 128

// runSnapshotCacheKey identifies one folded run snapshot by the run and its fold
// generation. The snapshot — and the compiled-formula target derived from it —
// are pure functions of the run's beads, and lastSeq pins those beads (equal
// lastSeq ⇒ equal beads), so a new bead event (lastSeq++) is the only thing that
// invalidates the entry.
type runSnapshotCacheKey struct {
	runID   string
	lastSeq uint64
}

// runSnapshotCacheValue is the folded snapshot plus the compiled-formula target
// resolved off it. Both are version/seq-independent, so detail() reuses them
// across every same-generation request: a repeat GET at an unchanged lastSeq
// pays no SnapshotForRun scan, only a new fold generation does. targetOK mirrors
// FormulaTargetFromSnapshot's ok (false when the run is not a fetchable graph.v2
// run, lacks a name+target, or has no valid scope). The stored value is
// immutable after the fold, so callers share it read-only.
type runSnapshotCacheValue struct {
	snap      runproj.RunSnapshot
	name      string
	target    string
	scopeKind string
	scopeRef  string
	targetOK  bool
}

// runSnapshotCache is the per-tailer LRU of folded run snapshots keyed by fold
// generation.
type runSnapshotCache = lruSingleFlight[runSnapshotCacheKey, runSnapshotCacheValue]

// newRunSnapshotCache builds an empty snapshot cache bounded to runSnapshotCacheCap.
func newRunSnapshotCache() *runSnapshotCache {
	return newLRUSingleFlight[runSnapshotCacheKey, runSnapshotCacheValue](runSnapshotCacheCap)
}

// lruSingleFlight is a small bounded LRU with single-flight: concurrent misses
// for the same key build exactly once. It is safe under -race: the mutex guards
// the map, the eviction list, and the in-flight table, and is NEVER held across
// the build that produces a value (the samplers.go contract). Stored values are
// immutable, so a reader shares one without copying. A build error is NOT cached
// (the next caller re-elects and retries), and a build PANIC likewise stores
// nothing — so a transient failure or a panicking build never pins a key with a
// bogus (zero) value.
type lruSingleFlight[K comparable, V any] struct {
	mu       sync.Mutex
	cap      int
	entries  map[K]*list.Element
	lru      *list.List // front = most recently used
	inflight map[K]chan struct{}
}

// lruEntry is one LRU node: the key (so eviction can delete the map entry) and
// the shared value.
type lruEntry[K comparable, V any] struct {
	key   K
	value V
}

// newLRUSingleFlight builds an empty cache bounded to capacity entries.
func newLRUSingleFlight[K comparable, V any](capacity int) *lruSingleFlight[K, V] {
	return &lruSingleFlight[K, V]{
		cap:      capacity,
		entries:  make(map[K]*list.Element),
		lru:      list.New(),
		inflight: make(map[K]chan struct{}),
	}
}

// getOrBuild returns the cached value for key, building it via build on a miss.
// Concurrent callers for the same key collapse onto one build: the elected
// caller runs build with NO lock held (the samplers.go contract); joiners wait
// and then re-read the now-stored value. build runs at most once per key per
// miss; a build error is NOT cached (the next caller re-elects), so a transient
// failure never pins a key.
func (m *lruSingleFlight[K, V]) getOrBuild(key K, build func() (V, error)) (V, error) {
	for {
		m.mu.Lock()
		if el, ok := m.entries[key]; ok {
			m.lru.MoveToFront(el)
			v := el.Value.(*lruEntry[K, V]).value
			m.mu.Unlock()
			return v, nil
		}
		if wait, building := m.inflight[key]; building {
			m.mu.Unlock()
			<-wait
			// Re-loop: read the value the builder stored, or re-elect if the build
			// failed (or panicked) and left no entry.
			continue
		}
		// We are the elected builder for this key.
		done := make(chan struct{})
		m.inflight[key] = done
		m.mu.Unlock()

		var (
			value     V
			buildErr  error
			completed bool
		)
		func() {
			// Release the in-flight handshake on every exit — including a build
			// panic — so a panicking build never orphans the channel and wedges
			// every future caller for this key. Store ONLY after build() returned
			// normally without error: a panic unwinds through build() without ever
			// assigning buildErr (it stays nil) or reaching completed=true, so
			// gating the store on buildErr alone would cache the zero value and
			// later callers would serve it as a valid result. completed stays false
			// on a panic, so nothing is stored, the next caller re-elects, and the
			// recovery middleware turns the panic into a 500.
			defer func() {
				m.mu.Lock()
				if completed && buildErr == nil {
					m.storeLocked(key, value)
				}
				delete(m.inflight, key)
				m.mu.Unlock()
				close(done)
			}()
			value, buildErr = build()
			completed = true
		}()
		if buildErr != nil {
			var zero V
			return zero, buildErr
		}
		return value, nil
	}
}

// storeLocked inserts value under key as most-recently-used and evicts the
// least-recently-used entry when the cap is exceeded. The caller holds m.mu.
func (m *lruSingleFlight[K, V]) storeLocked(key K, value V) {
	if el, ok := m.entries[key]; ok {
		el.Value.(*lruEntry[K, V]).value = value
		m.lru.MoveToFront(el)
		return
	}
	el := m.lru.PushFront(&lruEntry[K, V]{key: key, value: value})
	m.entries[key] = el
	if m.cap > 0 && m.lru.Len() > m.cap {
		if oldest := m.lru.Back(); oldest != nil {
			m.lru.Remove(oldest)
			delete(m.entries, oldest.Value.(*lruEntry[K, V]).key)
		}
	}
}
