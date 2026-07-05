package dashboardbff

import (
	"context"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/runproj"
)

// The two per-request loopback reads the run-detail path layers on top of the
// warm fold — GET /v0/city/{name}/sessions and GET /v0/city/{name}/formulas/... —
// are cached per city with single-flight + TTL so a burst of detail/summary GETs
// (a dashboard tab re-polling on every bead nudge) collapses onto ONE upstream
// fetch per key rather than one per request. This follows the samplers.go
// contract: the blocking upstream fetch runs with no cache lock held, exactly one
// caller performs it per key during a miss (the rest join its result), and a
// fetch failure degrades by serving the last-good value with its availability
// flag rather than blanking — except a cold miss with no last-good, which
// degrades EXACTLY as the uncached path did (so the honest partial/warming states
// are preserved).
var (
	// sessionsCacheTTL bounds how long a cached sessions read is served before a
	// refetch. A var (not a const) so tests can shorten it.
	sessionsCacheTTL = 3 * time.Second
	// formulaCacheTTL bounds how long a successfully-compiled formula detail is
	// served. Compiled formulas change rarely (an authored TOML edit), so this is
	// long. A var so tests can shorten it.
	formulaCacheTTL = 60 * time.Second
	// formulaNotFoundTTL bounds how long a 404 (genuinely-missing formula) outcome
	// is served before a re-check. It is short so a newly-added formula appears
	// promptly instead of being pinned missing for the full success TTL. A var so
	// tests can shorten it.
	formulaNotFoundTTL = 5 * time.Second
)

// ── Generic single-flight TTL cache ───────────────────────────────────────

// cacheEntry is one keyed slot in a singleFlightCache: the last-computed value,
// when it was computed (for TTL expiry), and — while a fetch is in flight — the
// channel a joining caller waits on. The value type V is copied by value on
// read, so V must be safe to share (a value type or an immutable-after-build
// pointer/slice, which both cached payloads are).
type cacheEntry[V any] struct {
	value    V
	computed time.Time
	// ttl is the entry's own expiry window, captured at compute time. The formula
	// cache uses a shorter window for a not-found outcome than for a success, so
	// the window travels with the entry rather than being a single cache-wide
	// constant.
	ttl time.Duration
	// hasValue is false until the first successful (or, for the not-found case,
	// definitively-negative) compute, so a cold miss whose fetch fails does not
	// publish a zero value as if it were last-good.
	hasValue bool
	// inflight is non-nil while exactly one caller computes this key; joiners wait
	// on it and then re-read the entry. It is set under the cache lock, closed by
	// the computing caller after publishing, and cleared under the lock.
	inflight chan struct{}
}

// singleFlightCache is a small per-key TTL cache with single-flight: concurrent
// cold-miss callers for the same key collapse to one compute; a hit within TTL
// serves the stored value with no upstream work; an expired entry triggers one
// refetch while other callers join it. The cache lock is NEVER held across the
// compute function — the samplers.go contract — so a slow upstream fetch never
// blocks a reader of a different key or a joiner re-reading a fresh value.
type singleFlightCache[K comparable, V any] struct {
	mu      sync.Mutex
	entries map[K]*cacheEntry[V]
}

func newSingleFlightCache[K comparable, V any]() *singleFlightCache[K, V] {
	return &singleFlightCache[K, V]{entries: make(map[K]*cacheEntry[V])}
}

// get returns the value for key, computing it via compute on a miss or expiry.
// compute returns the fetched value, the TTL that value should live for (so the
// formula cache can pick a shorter window for a not-found outcome), and ok
// reporting whether the fetch succeeded well enough to cache as last-good. On a
// compute failure with an existing last-good value the stale value is served
// (available); on a cold miss with no last-good the zero value is returned so the
// caller can apply its own honest degrade. Exactly one caller runs compute per
// key per miss; the rest block until it publishes, then re-read.
//
// The returned bool reports whether a usable value is being served: true for a
// fresh success, a within-TTL hit, or a served-stale last-good; false only for a
// cold miss whose fetch failed and left no last-good.
func (c *singleFlightCache[K, V]) get(ctx context.Context, key K, compute func(context.Context) (V, time.Duration, bool)) (V, bool) {
	for {
		c.mu.Lock()
		e, ok := c.entries[key]
		if !ok {
			e = &cacheEntry[V]{}
			c.entries[key] = e
		}
		// Fresh hit: serve without touching the upstream.
		if e.hasValue && e.inflight == nil && time.Since(e.computed) < e.ttl {
			v := e.value
			c.mu.Unlock()
			return v, true
		}
		// Someone is already computing this key: join and re-read once they publish.
		if e.inflight != nil {
			wait := e.inflight
			c.mu.Unlock()
			select {
			case <-wait:
				// Loop: re-read the now-published entry (or re-elect a computer if the
				// prior compute failed and left the entry stale/empty and expired).
				continue
			case <-ctx.Done():
				// The caller gave up. Serve the last-good if we have one (never block a
				// canceled caller on an in-flight fetch); otherwise degrade.
				return c.lastGoodOrZero(key)
			}
		}
		// We are the elected computer for this key.
		done := make(chan struct{})
		e.inflight = done
		c.mu.Unlock()

		// The in-flight handshake MUST be released on every exit — including a
		// panic in compute. The dashboardbff plane runs under the supervisor's
		// withRecovery middleware, so a compute panic is caught and turned into a
		// 500 while the process keeps serving; without a deferred release the
		// entry's inflight channel would be orphaned (never closed) and every
		// future caller for this key would block on it forever (or degrade to a
		// frozen last-good that never refetches). The deferred cleanup runs
		// before the panic propagates, so the next caller re-elects and recovers
		// while withRecovery still logs and 500s the panicking request.
		var (
			value     V
			ttl       time.Duration
			computeOK bool
			served    V
			hadValue  bool
		)
		func() {
			defer func() {
				c.mu.Lock()
				if computeOK {
					e.value = value
					e.computed = time.Now()
					e.ttl = ttl
					e.hasValue = true
				}
				// else: keep the prior last-good (if any) untouched — degrade,
				// don't blank.
				served, hadValue = e.value, e.hasValue
				e.inflight = nil
				c.mu.Unlock()
				close(done)
			}()
			// Compute with NO cache lock held (the samplers.go contract). A panic
			// here still runs the deferred release above, then propagates.
			value, ttl, computeOK = compute(ctx)
		}()

		if computeOK {
			return value, true
		}
		if hadValue {
			// A failed refetch with a prior success: serve the stale last-good.
			return served, true
		}
		// Cold miss, fetch failed, no last-good: honest degrade to the zero value.
		var zero V
		return zero, false
	}
}

// lastGoodOrZero returns the entry's last-good value (available) if one exists,
// else the zero value (unavailable). Used when a caller's ctx is canceled while
// joining an in-flight fetch: a canceled caller must never block, but should
// still serve the last-good if the cache holds one.
func (c *singleFlightCache[K, V]) lastGoodOrZero(key K) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.entries[key]; ok && e.hasValue {
		return e.value, true
	}
	var zero V
	return zero, false
}

// ── Cached payload shapes ─────────────────────────────────────────────────

// cachedSessions is the value stored in the sessions cache: the projected
// dashboard session slice. Immutable after build (fetchSessionsUpstream returns a
// fresh slice), so it is safe to share across callers by value.
type cachedSessions struct {
	items []runproj.DashboardSession
}

// cachedFormulaDetail is the value stored in the formula cache. It preserves the
// full fetch outcome — the compiled detail on success, or the NotFound vs
// UpstreamError distinction on a definitive failure — so runproj renders the
// right operator diagnostic. A cached not-found is a real (negative) cache entry,
// distinct from a transient upstream error which is not cached (the cold-miss
// degrade path handles it).
type cachedFormulaDetail struct {
	detail  *runproj.FormulaOrderingDetail
	failure runproj.RunFormulaDetailFetchFailure
}

// formulaCacheKey is the full identity a compiled formula resolves against: the
// city, the formula name, the run target, and the scope (kind+ref) that selects
// the formula search layer. Two runs that differ in any of these resolve to
// different compiled formulas, so all four are part of the key.
type formulaCacheKey struct {
	name      string
	formula   string
	target    string
	scopeKind string
	scopeRef  string
}
