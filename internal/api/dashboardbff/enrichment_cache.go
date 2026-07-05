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
	// singleFlightComputeTimeout bounds the elected single-flight compute, which
	// runs under a context DETACHED from the electing caller's request (see get).
	// Detaching stops the caller disconnecting from canceling the shared upstream
	// fetch, but a detached context also loses the request deadline, so this
	// timeout backstops a wedged upstream and keeps a stuck flight from pinning the
	// elector goroutine (and every joiner) forever. It sits above the per-fetch
	// http.Client timeout (runSessionsFetchTimeout), which stays the primary bound
	// in practice. A var so tests can shorten it.
	singleFlightComputeTimeout = 30 * time.Second
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
	// staleServeable reports whether this entry may be served AFTER a failed
	// refetch (the last-good/serve-stale contract). A positive result (a real
	// success) is safe to serve stale — a slow-changing formula or a session list
	// is still a useful answer. A negative result (a cached not-found) is
	// fresh-serveable within its TTL via the fast-hit path, but must NOT be served
	// stale: once it expires, an errored refetch falls through to the caller's
	// cold-miss degrade so a stale not-found never masks a later upstream error.
	staleServeable bool
	// version is a monotonic counter that bumps by one on every successful
	// compute-and-store (a refetch that publishes a new value), and NOT on a
	// within-TTL hit or a degrade-to-last-good. It lets a downstream memo (the
	// run-detail cache) detect that a cached enrichment value changed underneath
	// it: a bumped version invalidates the memo key even when the raw value is
	// equal, so the memo never serves a stale detail after an enrichment refresh.
	version uint64
	// inflight is non-nil while exactly one caller computes this key; joiners wait
	// on its done channel and then return its published result (never re-electing),
	// so a burst of concurrent callers collapses onto ONE upstream fetch even when
	// that fetch fails. It is set under the cache lock, resolved and closed by the
	// computing caller after publishing, and cleared under the lock.
	inflight *flight[V]
}

// flight is one in-flight compute for a key. It carries the channel joiners wait
// on plus the single result the elected computer publishes for every caller
// joined to this flight to return: a fresh success, a served-stale positive
// last-good, or a transient cold/expired-negative failure. Sharing one flight's
// result is what collapses a concurrent burst onto ONE upstream fetch even when
// the fetch fails — the failure is NOT written to the cache entry, so a LATER
// request still re-elects and refetches. value/version/ok are written once (under
// the cache lock, before done is closed) and read by joiners only after done
// closes, so the channel close supplies the happens-before with no extra locking.
// version is the served value's monotonic generation (see cacheEntry.version); it
// travels with the shared result so a joining getWithVersion caller returns the
// same version the elector does.
type flight[V any] struct {
	done    chan struct{}
	value   V
	version uint64
	ok      bool
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
// formula cache can pick a shorter window for a not-found outcome), ok reporting
// whether the fetch succeeded well enough to cache, and staleServeable reporting
// whether that cached value may be served AFTER a later failed refetch. A
// positive result is staleServeable (serve last-good on error); a definitive
// negative (a cached not-found) is cached but NOT staleServeable, so once it
// expires an errored refetch falls through to the caller's cold-miss degrade
// rather than surfacing the stale negative. On a compute failure with an existing
// staleServeable last-good the stale value is served (available); on a cold miss
// with no serveable last-good the zero value is returned so the caller can apply
// its own honest degrade. Exactly one caller runs compute per key per miss; the
// rest block until it publishes, then return that same shared result — so a
// concurrent burst, INCLUDING a failed one, collapses onto that single fetch.
// The elected compute runs under a context detached from the electing caller's
// request (its values kept, its cancellation and deadline dropped) plus a bounded
// timeout, so an elector that disconnects mid-fetch cannot cancel the shared
// upstream request its joiners are still waiting on; each joiner still abandons
// its own wait via ctx. A caller whose ctx is already canceled at a miss does not
// elect at all — it degrades to the serveable last-good — so only a caller that
// was live at election ever detaches a compute for nobody.
//
// The returned bool reports whether a usable value is being served: true for a
// fresh success, a within-TTL hit, or a served-stale positive last-good; false
// for a cold miss whose fetch failed and left no last-good, or an expired
// negative whose refetch failed.
func (c *singleFlightCache[K, V]) get(ctx context.Context, key K, compute func(context.Context) (V, time.Duration, bool, bool)) (V, bool) {
	v, _, ok := c.getWithVersion(ctx, key, compute)
	return v, ok
}

// getWithVersion is get with the served value's monotonic version. The version
// bumps by one on each successful compute-and-store and stays fixed across a
// within-TTL hit or a served-stale last-good, so a downstream memo can key on it
// to detect a refresh even when the raw value is unchanged. The returned version
// is meaningful only when ok is true (a served value); on a cold-miss degrade it
// is zero. Every serve-stale, single-flight, and context-detach rule get
// documents holds here unchanged — this is the full implementation get delegates
// to.
func (c *singleFlightCache[K, V]) getWithVersion(ctx context.Context, key K, compute func(context.Context) (V, time.Duration, bool, bool)) (V, uint64, bool) {
	c.mu.Lock()
	e, ok := c.entries[key]
	if !ok {
		e = &cacheEntry[V]{}
		c.entries[key] = e
	}
	// Fresh hit: serve without touching the upstream.
	if e.hasValue && e.inflight == nil && time.Since(e.computed) < e.ttl {
		v, ver := e.value, e.version
		c.mu.Unlock()
		return v, ver, true
	}
	// Someone is already computing this key: join that flight and return its
	// shared result instead of re-electing. A failed cold or expired-negative
	// flight is shared here too, so N concurrent callers collapse onto the ONE
	// upstream fetch rather than each waking, re-electing, and refetching
	// serially. A later request (after the flight clears inflight) still elects a
	// fresh compute, since the failure was never cached on the entry.
	if e.inflight != nil {
		fl := e.inflight
		c.mu.Unlock()
		select {
		case <-fl.done:
			return fl.value, fl.version, fl.ok
		case <-ctx.Done():
			// The caller gave up. Serve the last-good if we have one (never block a
			// canceled caller on an in-flight fetch); otherwise degrade.
			return c.lastGoodOrZero(key)
		}
	}
	// A caller whose request context is already canceled must NOT elect a new
	// flight. Electing sets the inflight slot and runs compute under a context
	// DETACHED from this caller (context.WithoutCancel below), so a request that
	// was already gone before the miss would still drive a full upstream fetch —
	// bounded only by the fetch/backstop timeout — on nobody's behalf. The
	// fresh-hit and join paths above are already safe for a canceled caller (a
	// hit does no upstream work; a joiner abandons via its own ctx.Done()); only
	// election has to guard. Degrade to the serveable last-good exactly as the
	// canceled-joiner branch does. WithoutCancel below therefore only ever
	// detaches a compute elected by a caller that was live at election, so a
	// mid-flight disconnect still cannot cancel the shared fetch for joiners.
	if ctx.Err() != nil {
		c.mu.Unlock()
		return c.lastGoodOrZero(key)
	}
	// We are the elected computer for this key.
	fl := &flight[V]{done: make(chan struct{})}
	e.inflight = fl
	c.mu.Unlock()

	// The in-flight handshake MUST be released on every exit — including a panic
	// in compute. The dashboardbff plane runs under the supervisor's withRecovery
	// middleware, so a compute panic is caught and turned into a 500 while the
	// process keeps serving; without a deferred release the entry's inflight
	// channel would be orphaned (never closed) and every future caller for this
	// key would block on it forever (or degrade to a frozen last-good that never
	// refetches). The deferred cleanup runs before the panic propagates, so the
	// next caller re-elects and recovers while withRecovery still logs and 500s
	// the panicking request.
	var (
		value          V
		ttl            time.Duration
		computeOK      bool
		staleServeable bool
		resultValue    V
		resultVersion  uint64
		resultOK       bool
	)
	func() {
		defer func() {
			c.mu.Lock()
			if computeOK {
				e.value = value
				e.computed = time.Now()
				e.ttl = ttl
				e.hasValue = true
				e.staleServeable = staleServeable
				// A successful store is a new generation of this value: bump the
				// version so a downstream memo keyed on it rebuilds.
				e.version++
			}
			// else: keep the prior last-good (if any) untouched — degrade,
			// don't blank.
			//
			// Resolve the single result this flight publishes to every joined caller
			// (the elector and all current waiters): a fresh success, a served-stale
			// positive last-good, or an honest degrade. Because a failure is never
			// written to the entry above, a LATER request re-elects and refetches —
			// only the concurrent burst is collapsed. The published version is the
			// served value's own generation, so a joining getWithVersion caller
			// returns the same version the elector does.
			switch {
			case computeOK:
				resultValue, resultVersion, resultOK = e.value, e.version, true
			case e.hasValue && e.staleServeable:
				// A failed refetch with a serveable positive last-good: serve it stale.
				// A negative last-good is NOT serveable stale, so it falls through to
				// the degrade below rather than masking this upstream error.
				resultValue, resultVersion, resultOK = e.value, e.version, true
			default:
				// Cold miss (or expired negative), fetch failed, no serveable
				// last-good: honest degrade to the zero value.
				var zero V
				resultValue, resultVersion, resultOK = zero, 0, false
			}
			fl.value, fl.version, fl.ok = resultValue, resultVersion, resultOK
			e.inflight = nil
			c.mu.Unlock()
			close(fl.done)
		}()
		// Compute with NO cache lock held (the samplers.go contract), under a
		// context DETACHED from the electing caller's request. This elected compute
		// is the single upstream fetch every joined caller shares; if it ran on the
		// elector's ctx, that caller disconnecting or hitting its deadline mid-fetch
		// would cancel the shared request and hand every still-waiting joiner a
		// canceled/failed result instead of their own enrichment. context.WithoutCancel
		// keeps the request's values (auth, tracing) while dropping its cancellation
		// and deadline; a bounded timeout backstops a wedged upstream so the detached
		// compute can never pin the flight forever. Joiners still abandon their own
		// wait via ctx.Done() above — only the shared compute is decoupled. A panic
		// here still runs the deferred release above, then propagates.
		computeCtx, cancelCompute := context.WithTimeout(context.WithoutCancel(ctx), singleFlightComputeTimeout)
		defer cancelCompute()
		value, ttl, computeOK, staleServeable = compute(computeCtx)
	}()

	return resultValue, resultVersion, resultOK
}

// lastGoodOrZero returns the entry's serveable last-good value and its version
// (available) if one exists, else the zero value (unavailable). Used when a
// caller's ctx is canceled while joining an in-flight fetch: a canceled caller
// must never block, but should still serve a serveable last-good if the cache
// holds one. A cached negative is not serveable stale (see
// cacheEntry.staleServeable), so it degrades here too rather than surfacing a
// stale not-found on cancellation.
func (c *singleFlightCache[K, V]) lastGoodOrZero(key K) (V, uint64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.entries[key]; ok && e.hasValue && e.staleServeable {
		return e.value, e.version, true
	}
	var zero V
	return zero, 0, false
}

// invalidate forces the next get for key to recompute — and bump the version —
// even within its TTL, while preserving the last-good value (for serve-stale)
// and the monotonic version. It expires the entry rather than deleting it so the
// version counter keeps advancing (a delete would reset it to zero and could
// collide with a memo key). Used to eagerly refresh an enrichment the moment an
// out-of-band signal says it changed (e.g. a session.* event in the tail),
// rather than waiting for the TTL to lapse. A no-op if the key is absent.
func (c *singleFlightCache[K, V]) invalidate(key K) {
	c.mu.Lock()
	if e, ok := c.entries[key]; ok {
		// ttl 0 makes the fresh-hit check (time.Since(computed) < ttl) always
		// false, so the next get recomputes. An in-flight compute is unaffected —
		// it publishes and bumps the version as usual.
		e.ttl = 0
	}
	c.mu.Unlock()
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
