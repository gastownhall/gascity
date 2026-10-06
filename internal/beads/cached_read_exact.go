package beads

// CachedReadExact is an optional declaration on a backing store: when
// CachedReadExact reports true, a CachingStore over it holds rows exactly as
// the store's own live reads return them. Cached statuses are raw (no fold of
// richer statuses into "open"), labels and metadata are complete, and the
// cached ready projection answers every row the way the store's live Ready
// does.
//
// The controller's v2 demand reads serve a leg from its cache only when the
// leg's backing declares this; every other leg is read live by the demand
// backstop lane. The declaration lives on the backing because only the store
// knows what it hands the cache: a caller inferring it from a concrete type
// would go stale the first time a store's read path changes.
//
// BdStore does not declare it (bd folds blocked, deferred, review and testing
// into "open" on decode, and bd-1.0.5 cache rows drop labels); neither do
// NativeDoltStore, MemStore and FileStore, whose cached ready projections still
// differ from their live Ready. A wrapper reports false, even over a store
// that declares it: a CachingStore stamps its ready projection only through
// the concrete engine it wraps (SQLite's is unexported), so a cache over a
// wrapper misses blockers the engine's live Ready honors.
type CachedReadExact interface {
	CachedReadExact() bool
}

// CachedReadExact reports that a CachingStore over SQLite reads exactly:
// SQLite stores raw statuses and full rows, and enrichReadyProjectionForCache
// stamps every row with the verdict sqliteReadySQL computes.
func (s *SQLiteStore) CachedReadExact() bool { return true }
