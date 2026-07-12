package beads

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// CachedReader is the cache-only eventual-consistency read handle for active
// beads. Get may return ErrNotFound for closed-but-existing beads because the
// cache does not retain complete closed history; use Live.Get for closed or
// historical lookups. List reads across both bead tiers regardless of the
// caller's TierMode; use the underlying Store directly for intentionally
// tier-scoped list queries.
//
// Ready and the other read methods may perform a one-time synchronous prime
// from the backing store when the projection is not yet live. ReadyCacheOnly is
// the strict variant that never primes — see its doc — for hot-path callers
// that must not block on the backing store.
type CachedReader interface {
	Get(id string) (Bead, error)
	List(query ListQuery) ([]Bead, error)
	Ready(query ...ReadyQuery) ([]Bead, error)
	ReadyCacheOnly(query ...ReadyQuery) ([]Bead, error)
	DepList(id, direction string) ([]Dep, error)
}

// LiveReader is the authoritative read handle for beads. List reads across both
// bead tiers regardless of the caller's TierMode; use the underlying Store
// directly for intentionally tier-scoped list queries.
type LiveReader interface {
	Get(id string) (Bead, error)
	List(query ListQuery) ([]Bead, error)
	Ready(query ...ReadyQuery) ([]Bead, error)
	DepList(id, direction string) ([]Dep, error)
}

// Writer is the mutation handle for beads.
type Writer interface {
	Create(b Bead) (Bead, error)
	Update(id string, opts UpdateOpts) error
	Close(id string) error
	Reopen(id string) error
	CloseAll(ids []string, metadata map[string]string) (int, error)
	SetMetadata(id, key, value string) error
	SetMetadataBatch(id string, kvs map[string]string) error
	Delete(id string) error
	DepAdd(issueID, dependsOnID, depType string) error
	DepRemove(issueID, dependsOnID string) error
}

// StoreHandles groups explicit bead read and write capabilities.
type StoreHandles struct {
	Cached CachedReader
	Live   LiveReader
	Writer Writer
}

// HandlesFor returns explicit cached/live reader and writer handles for a
// store. Stores with a native handle implementation keep their stronger
// guarantees; plain stores use logical wrappers that hide tier selection from
// callers.
func HandlesFor(store Store) StoreHandles {
	if provider, ok := store.(interface {
		Handles() StoreHandles
	}); ok {
		return provider.Handles()
	}
	return StoreHandles{
		Cached: logicalCachedStoreReader{store: store},
		Live:   logicalLiveStoreReader{store: store},
		Writer: store,
	}
}

// Handles returns explicit cached/live reader and writer handles that share
// this store's cache coordinator.
func (c *CachingStore) Handles() StoreHandles {
	return StoreHandles{
		Cached: cachedStoreReader{store: c},
		Live:   liveStoreReader{store: c},
		Writer: c,
	}
}

type logicalCachedStoreReader struct {
	store Store
}

func (r logicalCachedStoreReader) Get(id string) (Bead, error) {
	return r.store.Get(id)
}

func (r logicalCachedStoreReader) List(query ListQuery) ([]Bead, error) {
	query.Live = false
	query.TierMode = TierBoth
	return r.store.List(query)
}

func (r logicalCachedStoreReader) Ready(query ...ReadyQuery) ([]Bead, error) {
	return r.store.Ready(query...)
}

// ReadyCacheOnly delegates to the store's Ready: a plain (non-caching) store has
// no separate backing tier to prime, so its in-process read already cannot block
// on a remote cache prime. A CachingStore overrides this through its explicit
// Handles() implementation with a strictly non-priming projection read.
func (r logicalCachedStoreReader) ReadyCacheOnly(query ...ReadyQuery) ([]Bead, error) {
	return r.store.Ready(query...)
}

func (r logicalCachedStoreReader) DepList(id, direction string) ([]Dep, error) {
	return r.store.DepList(id, direction)
}

type logicalLiveStoreReader struct {
	store Store
}

func (r logicalLiveStoreReader) Get(id string) (Bead, error) {
	return r.store.Get(id)
}

func (r logicalLiveStoreReader) List(query ListQuery) ([]Bead, error) {
	query.Live = true
	query.TierMode = TierBoth
	return r.store.List(query)
}

func (r logicalLiveStoreReader) Ready(query ...ReadyQuery) ([]Bead, error) {
	return r.store.Ready(query...)
}

func (r logicalLiveStoreReader) DepList(id, direction string) ([]Dep, error) {
	return r.store.DepList(id, direction)
}

type cachedStoreReader struct {
	store *CachingStore
}

func (r cachedStoreReader) Get(id string) (Bead, error) {
	if err := r.store.ensureFullPrime(context.Background()); err != nil {
		return Bead{}, err
	}
	return r.store.cachedGetOnly(id)
}

func (r cachedStoreReader) List(query ListQuery) ([]Bead, error) {
	rows, err := r.store.cachedListOnly(logicalCachedListQuery(query))
	if err == nil || !errors.Is(err, ErrCacheUnavailable) {
		return rows, err
	}
	if err := r.store.ensureFullPrime(context.Background()); err != nil {
		return nil, err
	}
	return r.store.cachedListOnly(logicalCachedListQuery(query))
}

func (r cachedStoreReader) Ready(query ...ReadyQuery) ([]Bead, error) {
	if err := r.store.ensureFullPrime(context.Background()); err != nil {
		return nil, err
	}
	return r.store.cachedReadyOnly(readyQueryFromArgs(query))
}

// ReadyCacheOnly reads ready beads strictly from the in-memory projection and
// never triggers a backing-store prime. When the projection is not live enough
// to answer it returns ErrCacheUnavailable instead of priming, so a hot-path
// caller (the control dispatcher's GET /beads/ready?cached=true) cannot block or
// fail on the backing store the control-ready cache exists to bypass. Dirtiness
// is scoped per bead: an unconfirmed cross-process write on an unrelated bead
// omits only that bead (and any candidate that depends on it) from the result
// rather than declining the whole read, so one dirty bead cannot stall a
// cache-only reader that has no live fallback. Use Ready when a one-time
// synchronous prime is acceptable.
func (r cachedStoreReader) ReadyCacheOnly(query ...ReadyQuery) ([]Bead, error) {
	return r.store.cachedReadyOnly(readyQueryFromArgs(query))
}

func (r cachedStoreReader) DepList(id, direction string) ([]Dep, error) {
	if err := r.store.ensureFullPrime(context.Background()); err != nil {
		return nil, err
	}
	return r.store.cachedDepListOnly(id, direction)
}

type liveStoreReader struct {
	store *CachingStore
}

func (r liveStoreReader) Get(id string) (Bead, error) {
	return r.store.backing.Get(id)
}

func (r liveStoreReader) List(query ListQuery) ([]Bead, error) {
	query.Live = true
	query.TierMode = TierBoth
	return r.store.backing.List(query)
}

func (r liveStoreReader) Ready(query ...ReadyQuery) ([]Bead, error) {
	return r.store.backing.Ready(query...)
}

func (r liveStoreReader) DepList(id, direction string) ([]Dep, error) {
	return r.store.backing.DepList(id, direction)
}

func logicalCachedListQuery(query ListQuery) ListQuery {
	query.Live = false
	query.TierMode = TierBoth
	return query
}

func (c *CachingStore) cachedGetOnly(id string) (Bead, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if _, deleted := c.deletedSeq[id]; deleted {
		return Bead{}, ErrNotFound
	}
	if _, dirty := c.dirty[id]; dirty {
		return Bead{}, fmt.Errorf("getting bead %q from cache: %w", id, ErrCacheUnavailable)
	}
	b, ok := c.beads[id]
	if !ok {
		return Bead{}, ErrNotFound
	}
	return cloneBead(b), nil
}

func (c *CachingStore) cachedListOnly(query ListQuery) ([]Bead, error) {
	if !query.HasFilter() && !query.AllowScan {
		return nil, fmt.Errorf("listing beads from cache: %w", ErrQueryRequiresScan)
	}
	if query.IncludesClosed() {
		return nil, fmt.Errorf("listing closed beads from cache: %w", ErrCacheUnavailable)
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if (c.state != cacheLive && c.state != cachePartial) || c.primePartialErr != nil || len(c.dirty) > 0 {
		return nil, fmt.Errorf("listing beads from cache: %w", ErrCacheUnavailable)
	}
	rows := make([]Bead, 0, len(c.beads))
	for _, b := range c.beads {
		if !query.Matches(b) {
			continue
		}
		rows = append(rows, cloneBead(b))
	}
	sortBeadsForQuery(rows, query.Sort)
	if query.Limit > 0 && len(rows) > query.Limit {
		rows = rows[:query.Limit]
	}
	return rows, nil
}

func (c *CachingStore) cachedReadyOnly(query ReadyQuery) ([]Bead, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if (c.state != cacheLive && c.state != cachePartial) || c.primePartialErr != nil {
		return nil, fmt.Errorf("reading ready beads from cache: %w", ErrCacheUnavailable)
	}
	// A dirty bead is an unconfirmed cross-process write-through conflict, not a
	// reason to decline the whole read: scope the decline to the dirty beads
	// themselves (and any candidate that depends on one) rather than returning
	// ErrCacheUnavailable for every caller when len(c.dirty) > 0. The control
	// dispatcher reads this path cache-only with no live fallback, so a
	// whole-store decline let one unrelated dirty bead (mail, a session-lifecycle
	// projection, an unrelated step) stall all graph control-ready dispatch until
	// the next reconcile. Per-bead scoping mirrors cachedGetOnly's existing dirty
	// decline and keeps the false-positive guarantee below.
	hasDirty := len(c.dirty) > 0

	statusByID := make(map[string]string, len(c.beads))
	openBeads := make([]Bead, 0, len(c.beads))
	now := time.Now().UTC()
	for _, b := range c.beads {
		statusByID[b.ID] = b.Status
		if hasDirty {
			if _, dirty := c.dirty[b.ID]; dirty {
				// Skip only the dirty bead: its cached status/assignee/routing may
				// be stale, so serving it as ready off that row would strand it (the
				// #2927 class). It is picked up once a confirming event or reconcile
				// clears its dirty flag.
				continue
			}
		}
		if !IsReadyCandidateForTier(b, now, query.TierMode) {
			continue
		}
		if query.Assignee != "" && b.Assignee != query.Assignee {
			continue
		}
		openBeads = append(openBeads, cloneBead(b))
	}
	// Sort candidates before the limit-bounded loop below: c.beads is a map,
	// so without this a Limit cuts an arbitrary, per-call-different subset —
	// the #3208 bug class. Canonical ready order matches the SQL-backed
	// ready readers.
	sortBeadsReadyOrder(openBeads)

	result := make([]Bead, 0, len(openBeads))
	for _, b := range openBeads {
		deps, ok := c.deps[b.ID]
		switch {
		case ok:
		case c.depsComplete:
			deps = nil
		default:
			return nil, fmt.Errorf("reading ready deps from cache: %w", ErrCacheUnavailable)
		}
		if hasDirty && dependencyDirty(c.dirty, deps) {
			// A candidate whose blocking dependency is dirty cannot have its
			// readiness confirmed from cache: the dep's cached status may be stale,
			// so treating it as closed could dispatch a not-actually-ready bead (the
			// #2927 false-positive class). Skip until the dep's dirty flag clears.
			continue
		}
		if !cachedBeadReady(b, statusByID, deps) {
			continue
		}
		result = append(result, cloneBead(b))
		if query.Limit > 0 && len(result) >= query.Limit {
			break
		}
	}
	return result, nil
}

// dependencyDirty reports whether any of a candidate's ready-blocking
// dependencies is marked dirty. A dirty dependency's cached status is an
// unconfirmed cross-process write, so it cannot be trusted to decide the
// dependent bead's readiness from cache. Only ready-blocking types
// (blocks/waits-for/conditional-blocks) feed cachedBeadReady, so a dirty
// non-blocking edge — such as the parent-child edge every graph.v2 step holds
// to its workflow root — cannot change cached readiness and must not suppress
// the candidate; gating on isReadyBlockingDependencyType keeps the skip set to
// exactly the deps whose staleness could flip the answer, mirroring
// cachedBeadReady.
func dependencyDirty(dirty map[string]struct{}, deps []Dep) bool {
	for _, dep := range deps {
		if !isReadyBlockingDependencyType(dep.Type) {
			continue
		}
		if _, ok := dirty[dep.DependsOnID]; ok {
			return true
		}
	}
	return false
}

func (c *CachingStore) cachedDepListOnly(id, direction string) ([]Dep, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if (c.state != cacheLive && c.state != cachePartial) || c.primePartialErr != nil || len(c.dirty) > 0 {
		return nil, fmt.Errorf("listing deps from cache: %w", ErrCacheUnavailable)
	}
	if direction == "" || direction == "down" {
		deps, ok := c.deps[id]
		if ok || c.depsComplete {
			return cloneDeps(deps), nil
		}
		return nil, fmt.Errorf("listing deps from cache: %w", ErrCacheUnavailable)
	}
	if direction != "up" {
		return nil, fmt.Errorf("listing deps from cache: unsupported direction %q", direction)
	}
	if !c.depsComplete {
		return nil, fmt.Errorf("listing reverse deps from cache: %w", ErrCacheUnavailable)
	}
	var deps []Dep
	for _, beadDeps := range c.deps {
		for _, dep := range beadDeps {
			if dep.DependsOnID == id {
				deps = append(deps, dep)
			}
		}
	}
	return cloneDeps(deps), nil
}
