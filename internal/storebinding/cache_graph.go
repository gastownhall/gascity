package storebinding

// Provider-neutral Graph read cache.
//
// The cache the deployed system runs today lives underneath the class front
// doors, inside the bead engine, so it is only available to one provider. This
// is the same idea moved ABOVE the closed contract: it caches
// [GraphStore.Get], the hottest read in the system, and it works identically
// over the canonical Beads engine, a SQLite binding, or an out-of-tree
// provider's binding, because it only ever speaks the contract.
//
// Two properties make it safe to put in front of a live store.
//
// AGGRESSIVE INVALIDATION. Any contract operation that is not a pure read
// discards the whole cache. The wrapper does not model what a mutation touched
// — modeling that is exactly where a cache starts lying — and a mutation it
// failed to classify would show up immediately as a stale read in the class
// corpus.
//
// A GENERATION FENCE. A read that started before a mutation must never install
// its result afterwards, or the cache resurrects a pre-mutation bead. Every
// read snapshots the generation before it calls the store and installs its
// result only if the generation has not moved and no mutation is in flight.
// That is the race the corpus cannot see (it is sequential) and the one a
// concurrent production caller hits constantly, so it is pinned by its own
// -race test.
//
// Cached values are DETACHED. A bead carries slices, maps and pointers; handing
// a caller the cached value directly would let it mutate the cache in place.
// The copy is reflect-driven over exported fields rather than hand-written, so
// no field added to beads.Bead later can be missed — and a companion test
// proves the type graph contains nothing the copier cannot detach.

import (
	"reflect"
	"sync"

	"github.com/gastownhall/gascity/internal/beads"
)

// graphBeadCache is the cache state behind the Graph wrapper. It is not a
// GraphStore: the wrapper owns the contract, the cache owns only memory.
type graphBeadCache struct {
	mu         sync.Mutex
	beads      map[string]beads.Bead
	generation uint64
	inFlight   int
	stats      GraphCacheStats
}

// GraphCacheStats is the observable counter set of one Graph cache. It is
// reported through status; the wrapper never changes behavior based on it.
type GraphCacheStats struct {
	Hits          uint64
	Misses        uint64
	Invalidations uint64
	// Skipped counts reads whose result was discarded rather than installed
	// because a mutation raced them. A persistently non-zero value is a
	// contention signal, not an error.
	Skipped uint64
}

func newGraphBeadCache() *graphBeadCache {
	return &graphBeadCache{beads: make(map[string]beads.Bead)}
}

// lookup returns a detached copy of a cached bead.
func (c *graphBeadCache) lookup(id string) (beads.Bead, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cached, found := c.beads[id]
	if !found {
		c.stats.Misses++
		return beads.Bead{}, false
	}
	c.stats.Hits++
	return deepCopyBead(cached), true
}

// begin snapshots the generation a read is about to run against.
func (c *graphBeadCache) begin() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.generation
}

// install stores a read result only if nothing invalidated the cache while the
// read was in flight.
func (c *graphBeadCache) install(id string, bead beads.Bead, generation uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.generation != generation || c.inFlight > 0 {
		c.stats.Skipped++
		return
	}
	c.beads[id] = deepCopyBead(bead)
}

// enterMutation marks a mutation as in flight so no concurrent read installs a
// result that predates it.
func (c *graphBeadCache) enterMutation() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.inFlight++
}

// leaveMutation invalidates and clears the in-flight mark. It invalidates
// whether or not the mutation succeeded: a failed write may still have applied
// partially, and a cache that trusts an error to mean "nothing changed" is a
// cache that serves the pre-write value forever.
func (c *graphBeadCache) leaveMutation() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.invalidateLocked()
	c.inFlight--
}

func (c *graphBeadCache) invalidateLocked() {
	c.generation++
	c.stats.Invalidations++
	if len(c.beads) > 0 {
		c.beads = make(map[string]beads.Bead, len(c.beads))
	}
}

// Stats returns a snapshot of the cache counters.
func (c *graphBeadCache) Stats() GraphCacheStats {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stats
}

// GraphCacheStatsOf reports the cache counters of a wrapped Graph front door.
// It returns false for a front door that is not wrapped or runs without a
// cache, so a status view can say "no cache" rather than print zeros that look
// like a cache with no traffic.
func GraphCacheStatsOf(store GraphStore) (GraphCacheStats, bool) {
	wrapped, ok := store.(*wrappedGraphStore)
	if !ok || wrapped == nil || wrapped.cache == nil {
		return GraphCacheStats{}, false
	}
	return wrapped.cache.Stats(), true
}

// deepCopyBead returns a bead that shares no mutable memory with its argument.
func deepCopyBead(bead beads.Bead) beads.Bead {
	out := bead
	value := reflect.ValueOf(&out).Elem()
	for index := 0; index < value.NumField(); index++ {
		if !value.Type().Field(index).IsExported() {
			continue
		}
		value.Field(index).Set(deepCopyValue(value.Field(index)))
	}
	return out
}

// deepCopyValue detaches slices, maps, pointers and nested structs. Scalars and
// unexported struct state are already copied by value.
func deepCopyValue(value reflect.Value) reflect.Value {
	switch value.Kind() {
	case reflect.Slice:
		if value.IsNil() {
			return value
		}
		out := reflect.MakeSlice(value.Type(), value.Len(), value.Len())
		for index := 0; index < value.Len(); index++ {
			out.Index(index).Set(deepCopyValue(value.Index(index)))
		}
		return out
	case reflect.Map:
		if value.IsNil() {
			return value
		}
		out := reflect.MakeMapWithSize(value.Type(), value.Len())
		iterator := value.MapRange()
		for iterator.Next() {
			out.SetMapIndex(deepCopyValue(iterator.Key()), deepCopyValue(iterator.Value()))
		}
		return out
	case reflect.Pointer:
		if value.IsNil() {
			return value
		}
		out := reflect.New(value.Type().Elem())
		out.Elem().Set(deepCopyValue(value.Elem()))
		return out
	case reflect.Struct:
		out := reflect.New(value.Type()).Elem()
		out.Set(value)
		for index := 0; index < value.NumField(); index++ {
			if !value.Type().Field(index).IsExported() {
				continue
			}
			out.Field(index).Set(deepCopyValue(value.Field(index)))
		}
		return out
	default:
		return value
	}
}
