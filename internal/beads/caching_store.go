package beads

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// CachingStore wraps a Store with an in-memory cache.
// Reads are served from memory when the cache is live. Writes pass
// through to the backing store and update the cache on success.
//
// External writes (agents running bd directly) are picked up via the
// bd hook -> gc event emit -> event bus path. Call ApplyEvent when the
// event bus delivers bead.created/updated/closed events. The background
// reconciler acts as a watchdog and only performs a full scan once the
// cache has gone stale or degraded.
//
// BdStore-backed caches can filter hook events by issue prefix. Other Store
// implementations are valid backings, but run without foreign-event filtering.
type CachingStore struct {
	backing  Store // runtime: usually *BdStore; tests and projections may use any Store
	idPrefix string

	mu              sync.RWMutex
	beads           map[string]Bead
	deps            map[string][]Dep
	depsComplete    bool
	dirty           map[string]struct{}
	beadSeq         map[string]uint64
	localBeadAt     map[string]time.Time
	deletedSeq      map[string]uint64
	state           cacheState
	lastFreshAt     time.Time
	mutationSeq     uint64
	primePartialErr error

	reconciling  atomic.Bool
	syncFailures int
	stats        CacheStats
	onChange     func(eventType, beadID, runID, sessionID, stepID string, payload json.RawMessage)
	problemf     func(string)
	problemLog   map[string]cacheProblemLogState

	// lastReconcileLogAt rate-limits the per-reconcile success log line
	// emitted by runReconciliation. Without this, a busy cache at SMALL
	// cadence (30 s) would still produce 2 lines/min — and at faster test
	// cadences would flood logs. cacheReconcileSuccessLogWindow caps the
	// rate at one line per minute, matching cacheProblemLogWindow.
	lastReconcileLogAt     time.Time
	primeMu                sync.Mutex
	primeRunning           bool
	primeCycle             *fullPrimeCycle
	lastFullPrimeStartedAt time.Time
	primeRetryDelay        func(attempt int) time.Duration

	lifecycleMu sync.Mutex
	lifecycleWG sync.WaitGroup
	cancelFn    context.CancelFunc
	stopCh      chan struct{}
	stopped     bool

	// latencyWindow holds the most recent reconciliation bd-list
	// durations for adaptive cadence decisions. Bounded at
	// cacheLatencyWindowSize.
	latencyWindow []time.Duration
	// latencyDriverActive tracks whether sustained high P95 latency has
	// promoted the cadence to MEDIUM and is keeping it there. Bead-count
	// pressure is independent and not reflected here. Demotion happens
	// once the rolling window has drained — see recomputeCadenceLocked.
	latencyDriverActive bool

	applyEventBeforeCommitForTest func()
}

var (
	_ ConditionalAssignmentReleaser = (*CachingStore)(nil)
	_ AtomicTxStore                 = (*CachingStore)(nil)
)

type cacheState int

const (
	cacheUninitialized cacheState = iota
	cachePartial                  // PrimeActive loaded active beads; active queries can use the cache immediately.
	cacheLive
	cacheDegraded
)

type cacheProblemLogState struct {
	lastAt     time.Time
	suppressed int64
}

type fullPrimeCycle struct {
	done chan struct{}
	err  error
}

// CacheStats exposes cache freshness, reconciliation, and problem state.
type CacheStats struct {
	TotalBeads              int
	TotalDeps               int
	LastFreshAt             time.Time
	LastReconcileAt         time.Time
	LastReconcileMs         float64
	Adds                    int64
	Removes                 int64
	Updates                 int64
	ReconcileRecoveries     int64
	ReconcileCloseDeferrals int64
	SyncFailures            int
	ProblemCount            int64
	LastProblemAt           time.Time
	LastProblem             string
	State                   string
	// StaggerOffsetMs is the one-shot startup delay applied between Prime
	// and the first reconciler tick, in milliseconds. Set once when
	// StartReconciler runs; zero if stagger is disabled.
	StaggerOffsetMs int64
	// CurrentReconcileInterval is the effective bd-list cadence the
	// reconciler is currently using. Composed as max(bead-count cadence,
	// latency cadence) — see adaptiveIntervalLocked.
	CurrentReconcileInterval time.Duration
	// LatencyP95Ms is the P95 of the most recent N=cacheLatencyWindowSize
	// reconciliation bd-list durations, in milliseconds. Zero until the
	// window has been filled.
	LatencyP95Ms float64
	// CadenceDriver names which input drives the current cadence:
	// "default" (SMALL, nothing pressuring), "bead-count" (>=1000 beads),
	// "latency" (P95 above the high-water mark), or "both" (bead count
	// and latency both push to MEDIUM).
	CadenceDriver string
}

const (
	maxCacheSyncFailures            = 5
	cacheReconcilePollInterval      = 5 * time.Second
	cacheReconcileIntervalSmall     = 30 * time.Second
	cacheLazyFullPrimeRetryInterval = cacheReconcileIntervalSmall
	cacheReconcileIntervalMedium    = 60 * time.Second
	cacheReconcileIntervalLarge     = 120 * time.Second
	cacheProblemLogWindow           = time.Minute
	cacheReconcileFailureBackoff    = time.Minute
	// cacheReconcileSuccessLogWindow rate-limits the per-reconcile success
	// log line. Reuses the one-minute pattern from cacheProblemLogWindow so
	// the reconciler's footprint in the operator-visible log stays bounded
	// regardless of cadence.
	cacheReconcileSuccessLogWindow = time.Minute
)

// StaggerOption configures the deterministic startup stagger applied
// between Prime and the first reconciler tick. N agents starting in
// lockstep would otherwise hit the shared dolt server simultaneously;
// the stagger spreads first-tick load across a 0–30 s window.
//
// Construct one via WithStaggerAuto, WithStaggerOff, or
// WithStaggerFixed at the call site for self-documenting intent. The
// zero value is equivalent to WithStaggerOff().
type StaggerOption struct {
	auto     bool
	fixed    bool
	explicit time.Duration
}

// WithStaggerAuto enables a deterministic per-agent stagger derived
// from FNV-32a(agentID) mod cacheReconcileIntervalSmall. The stagger
// is reproducible across runs given the same agent ID.
func WithStaggerAuto() StaggerOption {
	return StaggerOption{auto: true}
}

// WithStaggerOff disables stagger; the reconciler enters its loop with
// no startup delay. This is the default for tests so existing behavior
// is preserved.
func WithStaggerOff() StaggerOption {
	return StaggerOption{}
}

// WithStaggerFixed sets an explicit stagger duration regardless of
// agentID. Negative durations clamp to zero.
func WithStaggerFixed(d time.Duration) StaggerOption {
	if d < 0 {
		d = 0
	}
	return StaggerOption{fixed: true, explicit: d}
}

// resolve returns the concrete stagger duration for this option.
// agentID is consulted only when the option is WithStaggerAuto.
func (o StaggerOption) resolve(agentID string) time.Duration {
	switch {
	case o.fixed:
		return o.explicit
	case o.auto:
		return computeAutoStagger(agentID)
	}
	return 0
}

// computeAutoStagger hashes agentID with FNV-32a and reduces it modulo
// cacheReconcileIntervalSmall (in milliseconds). The result lies in
// [0, cacheReconcileIntervalSmall) and is fully deterministic — no
// time-seeding — so test runs reproduce.
func computeAutoStagger(agentID string) time.Duration {
	h := fnv.New32a()
	_, _ = h.Write([]byte(agentID))
	modMs := cacheReconcileIntervalSmall.Milliseconds()
	if modMs <= 0 {
		return 0
	}
	return time.Duration(int64(h.Sum32())%modMs) * time.Millisecond
}

// NewCachingStore wraps a Store with an in-memory read cache.
// Call Prime() before serving reads, then StartReconciler() for
// watchdog reconciliation. The onChange callback (optional) is called for
// each detected external change with event type and bead JSON.
//
// BdStore-backed caches filter hook events by issue prefix. Other Store
// implementations are valid backings, but run without foreign-event filtering.
//
// onChange receives the opaque run/session correlation ids resolved from the
// changed bead's metadata at the record site (see notifyChange); the wiring
// stamps them onto the recorded event so the redacted export can forward them
// as typed primitives without ever decoding the payload.
func NewCachingStore(backing Store, onChange func(eventType, beadID, runID, sessionID, stepID string, payload json.RawMessage)) *CachingStore {
	prefix := ""
	bdBacking := false
	nilBdBacking := false
	if bd, ok := backing.(*BdStore); ok {
		bdBacking = true
		if bd == nil {
			nilBdBacking = true
		} else {
			prefix = bd.IDPrefix()
		}
	} else if backing, ok := backing.(interface{ IDPrefix() string }); ok {
		prefix = backing.IDPrefix()
	}
	cs := newCachingStore(backing, prefix, onChange)
	switch {
	case backing == nil:
		cs.recordProblem("cache backing", errors.New("nil store backing; cache will panic on first use"))
	case nilBdBacking:
		cs.recordProblem("bd cache ownership", errors.New("nil *BdStore backing; cache will panic on first use"))
	case bdBacking && cs.idPrefix == "":
		cs.recordProblem("bd cache ownership", errors.New("missing issue prefix; foreign bead event filtering disabled"))
	}
	return cs
}

// NewCachingStoreForTest wraps any Store for testing without production prefix
// validation. It keeps the legacy 3-param onChange (tests do not exercise the
// run/session ids); adaptLegacyOnChange bridges it to the production 5-param form.
func NewCachingStoreForTest(backing Store, onChange func(eventType, beadID string, payload json.RawMessage)) *CachingStore {
	return newCachingStore(backing, "", adaptLegacyOnChange(onChange))
}

// NewCachingStoreForTestWithPrefix wraps any Store for tests that need
// production-style bead ID ownership filtering.
func NewCachingStoreForTestWithPrefix(backing Store, idPrefix string, onChange func(eventType, beadID string, payload json.RawMessage)) *CachingStore {
	return newCachingStore(backing, idPrefix, adaptLegacyOnChange(onChange))
}

// adaptLegacyOnChange bridges the legacy 3-param onChange used by the test
// constructors to the production 5-param form, dropping the run/session ids the
// tests do not exercise. Nil-safe.
func adaptLegacyOnChange(fn func(eventType, beadID string, payload json.RawMessage)) func(eventType, beadID, runID, sessionID, stepID string, payload json.RawMessage) {
	if fn == nil {
		return nil
	}
	return func(eventType, beadID string, _, _, _ string, payload json.RawMessage) {
		fn(eventType, beadID, payload)
	}
}

// SetPrimeRetryDelayForTest overrides the inter-attempt backoff Prime
// uses when the backing store's full scan fails, so tests can exercise
// prime-failure paths without real multi-second sleeps. Test-only.
func (c *CachingStore) SetPrimeRetryDelayForTest(fn func(attempt int) time.Duration) {
	c.primeRetryDelay = fn
}

func newCachingStore(backing Store, idPrefix string, onChange func(eventType, beadID, runID, sessionID, stepID string, payload json.RawMessage)) *CachingStore {
	return &CachingStore{
		backing:     backing,
		idPrefix:    normalizeIDPrefix(idPrefix),
		beads:       make(map[string]Bead),
		deps:        make(map[string][]Dep),
		dirty:       make(map[string]struct{}),
		beadSeq:     make(map[string]uint64),
		localBeadAt: make(map[string]time.Time),
		deletedSeq:  make(map[string]uint64),
		problemLog:  make(map[string]cacheProblemLogState),
		onChange:    onChange,
		problemf: func(msg string) {
			log.Printf("beads cache: %s", msg)
		},
		primeRetryDelay: defaultCachePrimeRetryDelay,
		stopCh:          make(chan struct{}),
	}
}

func defaultCachePrimeRetryDelay(attempt int) time.Duration {
	return time.Duration(attempt*5) * time.Second
}

// IDPrefix returns the bead ID prefix owned by this cache's backing store.
func (c *CachingStore) IDPrefix() string {
	if c == nil {
		return ""
	}
	return c.idPrefix
}

func normalizeIDPrefix(prefix string) string {
	return strings.Trim(strings.ToLower(strings.TrimSpace(prefix)), "-")
}

func (c *CachingStore) ownsBeadID(id string) bool {
	if c.idPrefix == "" {
		return true
	}
	id = strings.ToLower(strings.TrimSpace(id))
	return strings.HasPrefix(id, c.idPrefix+"-")
}

// WaitForParentProjection forwards the optional parent-projection wait
// capability to the backing store when available.
func (c *CachingStore) WaitForParentProjection(ctx context.Context, id, oldParentID, newParentID string) error {
	waiter, ok := c.backing.(ParentProjectionWaiter)
	if !ok {
		return nil
	}
	return waiter.WaitForParentProjection(ctx, id, oldParentID, newParentID)
}

func (c *CachingStore) noteMutationLocked(ids ...string) uint64 {
	c.mutationSeq++
	seq := c.mutationSeq
	for _, id := range ids {
		if id == "" {
			continue
		}
		c.beadSeq[id] = seq
	}
	return seq
}

func (c *CachingStore) noteLocalMutationLocked(ids ...string) uint64 {
	seq := c.noteMutationLocked(ids...)
	now := time.Now()
	for _, id := range ids {
		if id == "" {
			continue
		}
		c.localBeadAt[id] = now
	}
	return seq
}

// absorbDepsMode selects how absorbFreshLocked sources the deps row for a bead.
type absorbDepsMode int

const (
	// depsExplicit installs the caller-supplied opts.deps (cloned).
	depsExplicit absorbDepsMode = iota
	// depsFromFields recomputes deps from the bead's own fields, unconditionally
	// (setting a nil entry when the bead carries no dependency fields).
	depsFromFields
	// depsFromFieldsIfCarried recomputes deps from the bead's fields only when
	// the bead carries dependency fields; otherwise the cached deps row is left
	// untouched.
	depsFromFieldsIfCarried
	// depsKeepCached leaves the cached deps row untouched.
	depsKeepCached
	// depsDrop removes the deps row.
	depsDrop
)

// absorbSeqMode selects how absorbFreshLocked treats the beadSeq/localBeadAt
// staleness fences for a bead.
type absorbSeqMode int

const (
	// seqKeep touches neither fence. Used by write/event paths that ran
	// noteMutationLocked/noteLocalMutationLocked immediately before the absorb
	// and MUST preserve the fence they just set (the #2210 staleness defense).
	seqKeep absorbSeqMode = iota
	// seqClearGuarded clears both fences unless a recent local write-through is
	// still inside the recency window.
	seqClearGuarded
	// seqClearBeadSeqOnly clears the beadSeq fence unconditionally and leaves
	// localBeadAt untouched.
	seqClearBeadSeqOnly
)

// absorbOpts describes the two axes of variation observed across the cache's
// absorb sites: how the deps row is sourced and how the staleness fences are
// treated. clearDirty is separate because a small number of sites (prime's
// slow path, PrimeActive) deliberately leave a dirty mark in place across an
// absorb.
type absorbOpts struct {
	depsMode   absorbDepsMode
	deps       []Dep // consulted only for depsExplicit
	seqMode    absorbSeqMode
	clearDirty bool
}

// absorbFreshLocked installs a fresh row for id per opts. It is the only code
// that installs a cached row alongside clearing the row's tombstone/staleness
// state. now is the caller's clock read for the whole pass; it is consulted
// only by seqClearGuarded. Caller must hold c.mu in write mode.
func (c *CachingStore) absorbFreshLocked(id string, bead Bead, now time.Time, opts absorbOpts) {
	c.beads[id] = cloneBead(bead)
	switch opts.depsMode {
	case depsExplicit:
		c.deps[id] = cloneDeps(opts.deps)
	case depsFromFields:
		c.deps[id] = depsFromBeadFields(bead)
	case depsFromFieldsIfCarried:
		if beadCarriesDependencyFields(bead) {
			c.deps[id] = depsFromBeadFields(bead)
		}
	case depsKeepCached:
		// leave c.deps[id] untouched
	case depsDrop:
		delete(c.deps, id)
	}
	if opts.clearDirty {
		delete(c.dirty, id)
	}
	delete(c.deletedSeq, id)
	switch opts.seqMode {
	case seqClearGuarded:
		if !recentLocalMutation(c.localBeadAt[id], now) {
			delete(c.beadSeq, id)
			delete(c.localBeadAt, id)
		}
	case seqClearBeadSeqOnly:
		delete(c.beadSeq, id)
	}
}

// evictLocked removes every trace of id from the six per-row maps. It does not
// touch mutationSeq, depsComplete, state, or stats. Caller must hold c.mu in
// write mode.
func (c *CachingStore) evictLocked(id string) {
	delete(c.beads, id)
	delete(c.deps, id)
	delete(c.dirty, id)
	delete(c.deletedSeq, id)
	delete(c.beadSeq, id)
	delete(c.localBeadAt, id)
}

// tombstoneLocked evicts id and installs a deletion fence at seq. seq must be a
// mutationSeq value obtained under the same lock hold so the fence exceeds any
// startSeq captured before this section. Caller must hold c.mu in write mode.
func (c *CachingStore) tombstoneLocked(id string, seq uint64) {
	c.evictLocked(id)
	c.deletedSeq[id] = seq
}

// markDirtyLocked flags id as known-stale so reads bypass the cache until a
// refresh clears the mark. Caller must hold c.mu in write mode.
func (c *CachingStore) markDirtyLocked(id string) {
	c.dirty[id] = struct{}{}
}

// clearStalenessMarksLocked clears the dirty flag and deletion fence for id
// without touching the cached row or its deps. Used by the deps-overlay
// fallbacks that trust an in-place dependency mutation. Caller must hold c.mu
// in write mode.
func (c *CachingStore) clearStalenessMarksLocked(id string) {
	delete(c.dirty, id)
	delete(c.deletedSeq, id)
}

// dirtyOverlayMaxGets bounds the inline per-ID refresh a cached read will do
// before it declines the overlay and falls back to today's full backing scan.
// Above the cap the read degrades to prior behavior — never worse.
const dirtyOverlayMaxGets = 8

// errDirtyOverlayFallback signals that a cached read must take its existing
// fallback path (backing.List / backing.Ready / ErrCacheUnavailable / ok=false,
// each unchanged per site). It never escapes the read site.
var errDirtyOverlayFallback = errors.New("beads cache: dirty overlay fallback")

// cacheServableLocked reports whether the active read model can answer from
// cache: the cache is live or partial and the prime was not a partial error.
// Dirty is no longer a serve-blocker — it is handled by readCacheWithOverlay.
// Caller must hold c.mu (read or write).
func (c *CachingStore) cacheServableLocked() bool {
	return (c.state == cacheLive || c.state == cachePartial) && c.primePartialErr == nil
}

// readCacheWithOverlay serves a cached read after refreshing only the dirty
// rows, replacing the old "one dirty bead declines the whole cache" tripwire.
//
// gate reports, under the lock, whether the cache is servable for this read
// shape (cacheServableLocked for most sites; Ready adds depsComplete). collect
// materializes the read from the cache and is invoked exactly once, while the
// lock is held, only after every dirty row has been refreshed or confirmed
// absent — so no dirty row is ever served (I1) and no new mark can slip between
// the servability re-check and the serve (I7). suppressed holds IDs that
// backing.Get reported ErrNotFound this pass; collect must omit them, matching
// what the old full backing.List would have returned for deleted rows (I6). A
// suppressed id that a concurrent apply resurrects between fetch and re-lock is
// caught by retrySuppressedChurnLocked and re-fetched, the symmetric fence to
// the fetched-row deletedSeq/beadSeq check, so the serve never omits a now-live
// row (I6).
//
// A non-nil error means the caller must take its existing fallback path (I5):
// the dirty set exceeds dirtyOverlayMaxGets, a backing.Get failed with a
// non-NotFound error, the cache is not servable, or residual dirty churn
// survived the bounded retry. No backing I/O happens under c.mu (I7).
func (c *CachingStore) readCacheWithOverlay(gate func() bool, collect func(suppressed map[string]struct{})) error {
	suppressed := make(map[string]struct{})
	for pass := 0; pass < 2; pass++ {
		c.mu.RLock()
		if !gate() {
			c.mu.RUnlock()
			return errDirtyOverlayFallback
		}
		startSeq := c.mutationSeq
		todo := c.dirtyToRefreshLocked(suppressed)
		if len(todo) == 0 {
			// Cache is clean, or every remaining dirty row is a confirmed
			// absence: serve from cache under this same lock hold — but only
			// after re-verifying no suppressed row was resurrected (see below).
			if c.retrySuppressedChurnLocked(suppressed, startSeq) {
				c.mu.RUnlock()
				continue
			}
			collect(suppressed)
			c.mu.RUnlock()
			return nil
		}
		if len(c.dirty) > dirtyOverlayMaxGets {
			c.mu.RUnlock()
			return errDirtyOverlayFallback
		}
		c.mu.RUnlock()

		fetched, err := c.fetchDirtyOverlay(todo, suppressed)
		if err != nil {
			return errDirtyOverlayFallback
		}

		c.mu.Lock()
		if !gate() {
			c.mu.Unlock()
			return errDirtyOverlayFallback
		}
		now := time.Now()
		absorbed := 0
		for _, f := range fetched {
			// Fence discipline (I3): never overwrite a mutation that landed
			// after the snapshot. A skipped-but-still-dirty row is caught by
			// the re-check below and handled by the retry-or-fallback.
			if c.deletedSeq[f.id] > startSeq || c.beadSeq[f.id] > startSeq {
				continue
			}
			opts := absorbOpts{
				depsMode:   depsFromFields,
				seqMode:    seqClearBeadSeqOnly,
				clearDirty: true,
			}
			// R1: rows whose backing.Get carried no dependency fields had their
			// authoritative deps fetched separately; install them verbatim so the
			// overlay never clobbers a blocked bead's deps to nil.
			if f.depsFromBacking {
				opts.depsMode = depsExplicit
				opts.deps = f.deps
			}
			c.absorbFreshLocked(f.id, f.bead, now, opts)
			absorbed++
		}
		if absorbed > 0 {
			c.markFreshLocked(now)
			c.updateStatsLocked()
		}
		if len(c.dirtyToRefreshLocked(suppressed)) == 0 {
			if c.retrySuppressedChurnLocked(suppressed, startSeq) {
				c.mu.Unlock()
				continue
			}
			collect(suppressed)
			c.mu.Unlock()
			return nil
		}
		c.mu.Unlock()
	}
	return errDirtyOverlayFallback
}

// retrySuppressedChurnLocked guards the serve against a torn read caused by an
// ErrNotFound-suppressed row being re-installed by a concurrent event-apply
// between its fetch and this final lock hold (the symmetric fence to the
// fetched-row deletedSeq/beadSeq check). A suppressed id is churn if its fence
// advanced past the snapshot, or a resident non-dirty row is now present — in
// either case omitting it from collect would serve the cache MINUS a now-live
// row. Any such id is dropped from suppressed so the next pass re-fetches it,
// and the function reports true to signal the caller must retry (or, on the
// final pass, fall back). Caller must hold c.mu. Returns false when the serve
// may proceed.
func (c *CachingStore) retrySuppressedChurnLocked(suppressed map[string]struct{}, startSeq uint64) bool {
	if len(suppressed) == 0 {
		return false
	}
	var churned []string
	for id := range suppressed {
		if c.beadSeq[id] > startSeq || c.deletedSeq[id] > startSeq {
			churned = append(churned, id)
			continue
		}
		if _, resident := c.beads[id]; resident {
			if _, dirty := c.dirty[id]; !dirty {
				churned = append(churned, id)
			}
		}
	}
	for _, id := range churned {
		delete(suppressed, id)
	}
	return len(churned) > 0
}

// dirtyToRefreshLocked returns the dirty IDs still needing a backing refresh:
// every dirty mark not already confirmed absent this pass. Caller must hold
// c.mu (read or write).
func (c *CachingStore) dirtyToRefreshLocked(suppressed map[string]struct{}) []string {
	if len(c.dirty) == 0 {
		return nil
	}
	var todo []string
	for id := range c.dirty {
		if _, ok := suppressed[id]; ok {
			continue
		}
		todo = append(todo, id)
	}
	return todo
}

type overlayFetched struct {
	id   string
	bead Bead
	// deps holds the authoritative dependency row pulled from backing.DepList,
	// set only when depsFromBacking is true.
	deps []Dep
	// depsFromBacking is true when the fetched bead carried no dependency fields
	// and deps was sourced from an explicit backing.DepList instead. The absorb
	// then installs deps verbatim (depsExplicit) rather than recomputing from the
	// bead's — absent — fields.
	depsFromBacking bool
}

// fetchDirtyOverlay fetches each dirty ID via backing.Get with no lock held
// (I7). Successful Gets are queued for absorb; ErrNotFound IDs are added to
// suppressed (their dirty mark is deliberately left set, mirroring Get's dirty
// path — convergence stays with the reconciler). Any other error returns
// non-nil so the caller falls back.
//
// R1 (gastownhall/gascity#2987 class): a backing whose Get carries no dependency
// fields — the fork's flagship native DoltLite read store — would, if absorbed
// with depsFromFields, have its cached deps clobbered to nil. For such rows the
// authoritative deps are pulled here via backing.DepList (still lock-free) so the
// absorb can install them explicitly and a blocked bead is never served as ready.
func (c *CachingStore) fetchDirtyOverlay(todo []string, suppressed map[string]struct{}) ([]overlayFetched, error) {
	fetched := make([]overlayFetched, 0, len(todo))
	for _, id := range todo {
		fresh, err := c.backing.Get(id)
		switch {
		case err == nil:
			row := overlayFetched{id: id, bead: fresh}
			if !beadCarriesDependencyFields(fresh) {
				deps, depErr := c.backing.DepList(id, "down")
				if depErr != nil {
					return nil, depErr
				}
				row.deps = deps
				row.depsFromBacking = true
			}
			fetched = append(fetched, row)
		case errors.Is(err, ErrNotFound):
			suppressed[id] = struct{}{}
		default:
			return nil, err
		}
	}
	return fetched, nil
}

// PrimeActive loads the common active bead statuses (open + in_progress) across
// both persistent issues and ephemeral wisps into the cache. These are fast indexed
// queries that populate enough data for
// startup paths without waiting for a full scan. The cache enters
// cachePartial state: filtered active queries and Get hit cache for primed
// beads, while closed-bead queries still delegate to the backing store.
func (c *CachingStore) PrimeActive() error {
	c.mu.RLock()
	startSeq := c.mutationSeq
	c.mu.RUnlock()

	var all []Bead
	var partialErr error
	for _, status := range []string{"open", "in_progress"} {
		beads, err := c.backing.List(ListQuery{Status: status, TierMode: TierBoth})
		if err != nil {
			if !IsPartialResult(err) {
				return fmt.Errorf("prime active (%s): %w", status, err)
			}
			partialErr = errors.Join(partialErr, err)
			c.recordProblem(fmt.Sprintf("prime active (%s)", status), err)
		}
		all = append(all, beads...)
	}
	if enriched, err := c.enrichReadyProjectionForCache(all); err != nil {
		partialErr = errors.Join(partialErr, err)
		c.recordProblem("prime active ready projection", err)
	} else {
		all = enriched
	}

	beadMap := make(map[string]Bead, len(all))
	for _, b := range all {
		beadMap[b.ID] = cloneBead(b)
	}
	depMap, depsComplete, depErr := c.fetchDepsForBeads(beadMap)
	if depErr != nil {
		partialErr = errors.Join(partialErr, depErr)
		c.recordProblem("prime active dep cache", depErr)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	for _, b := range all {
		if c.mutationSeq != startSeq {
			if c.deletedSeq[b.ID] > startSeq {
				continue
			}
			if _, exists := c.beads[b.ID]; exists {
				continue
			}
		}
		if _, keep := c.recentLocalBeadConflictLocked(b.ID, b, now, false); keep {
			continue
		}
		opts := absorbOpts{seqMode: seqClearGuarded, clearDirty: false}
		if depsComplete && depErr == nil {
			opts.depsMode = depsExplicit
			opts.deps = depMap[b.ID]
		} else {
			opts.depsMode = depsFromFields
		}
		c.absorbFreshLocked(b.ID, b, now, opts)
	}
	if c.state == cacheUninitialized {
		c.state = cachePartial
	}
	c.primePartialErr = partialErr
	c.markFreshLocked(now)
	c.updateStatsLocked()
	return nil
}

// Prime loads all active beads and deps from the backing store into memory.
// Retries up to 3 times on failure since bd list can time out under
// concurrent dolt load.
func (c *CachingStore) Prime(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := c.beginCacheWorker(); err != nil {
		return err
	}
	defer c.endCacheWorker()
	if err := c.cacheContextErr(ctx); err != nil {
		return err
	}

	done, owner := c.beginFullPrime()
	if !owner {
		return c.waitForFullPrimeDone(ctx, done)
	}
	err := c.prime(ctx)
	c.finishFullPrime(done, err)
	return err
}

func (c *CachingStore) prime(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := c.cacheContextErr(ctx); err != nil {
		return err
	}

	c.mu.RLock()
	startSeq := c.mutationSeq
	c.mu.RUnlock()

	var all []Bead
	var err error
	var partialErr error
	for attempt := 1; attempt <= 3; attempt++ {
		all, err = c.backing.List(cacheFullScanQuery()) // active beads only; see cacheFullScanQuery
		if err == nil {
			break
		}
		if IsPartialResult(err) {
			c.recordProblem("prime cache: partial list", err)
			partialErr = err
			err = nil
			break
		}
		c.recordProblem(fmt.Sprintf("prime cache attempt %d/3", attempt), err)
		if attempt < 3 {
			delay := defaultCachePrimeRetryDelay(attempt)
			if c.primeRetryDelay != nil {
				delay = c.primeRetryDelay(attempt)
			}
			if delay > 0 {
				if err := c.cacheSleep(ctx, delay); err != nil {
					return err
				}
			}
		}
	}
	if err != nil {
		return fmt.Errorf("prime list: %w", err)
	}
	if enriched, enrichErr := c.enrichReadyProjectionForCache(all); enrichErr != nil {
		c.recordProblem("prime ready projection", enrichErr)
		partialErr = errors.Join(partialErr, enrichErr)
	} else {
		all = enriched
	}
	if err := c.cacheContextErr(ctx); err != nil {
		return err
	}

	beadMap := make(map[string]Bead, len(all))
	for _, b := range all {
		beadMap[b.ID] = cloneBead(b)
	}

	depMap, depsComplete, depErr := c.fetchDepsForBeads(beadMap)
	if depErr != nil {
		c.recordProblem("prime dep cache", depErr)
	}
	if err := c.cacheContextErr(ctx); err != nil {
		return err
	}

	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.mutationSeq == startSeq {
		nextBeads := beadMap
		nextDeps := depsFromBeads(beadMap, depMap, depsComplete && depErr == nil)
		nextDirty := make(map[string]struct{})
		nextBeadSeq := make(map[string]uint64)
		nextLocalBeadAt := make(map[string]time.Time)
		for id, current := range c.beads {
			if fresh, exists := beadMap[id]; exists {
				if _, keep := c.recentLocalBeadConflictLocked(id, fresh, now, true); keep {
					nextBeads[id] = cloneBead(current)
					if deps, ok := c.deps[id]; ok {
						nextDeps[id] = cloneDeps(deps)
					}
					c.carryRecentLocalMutationLocked(id, nextDirty, nextBeadSeq, nextLocalBeadAt)
				}
				continue
			}
			if current.Status != "closed" && recentLocalMutation(c.localBeadAt[id], now) {
				nextBeads[id] = cloneBead(current)
				if deps, ok := c.deps[id]; ok {
					nextDeps[id] = cloneDeps(deps)
				}
				c.carryRecentLocalMutationLocked(id, nextDirty, nextBeadSeq, nextLocalBeadAt)
			}
		}
		c.beads = nextBeads
		c.deps = nextDeps
		c.depsComplete = depsComplete && depErr == nil
		c.dirty = nextDirty
		c.beadSeq = nextBeadSeq
		c.localBeadAt = nextLocalBeadAt
		c.deletedSeq = make(map[string]uint64)
	} else {
		for id, b := range beadMap {
			if c.deletedSeq[id] > startSeq {
				continue
			}
			if _, exists := c.beads[id]; exists {
				continue
			}
			opts := absorbOpts{seqMode: seqClearBeadSeqOnly, clearDirty: false}
			if depsComplete && depErr == nil {
				opts.depsMode = depsExplicit
				opts.deps = depMap[id]
			} else {
				opts.depsMode = depsFromFields
			}
			c.absorbFreshLocked(id, b, now, opts)
		}
		c.depsComplete = false
	}
	c.state = cacheLive
	c.syncFailures = 0
	c.stats.SyncFailures = 0
	c.primePartialErr = partialErr
	c.markFreshLocked(now)
	c.updateStatsLocked()
	return nil
}

func (c *CachingStore) beginFullPrime() (*fullPrimeCycle, bool) {
	c.primeMu.Lock()
	defer c.primeMu.Unlock()
	if c.primeRunning {
		return c.primeCycle, false
	}
	return c.startFullPrimeLocked(), true
}

func (c *CachingStore) finishFullPrime(cycle *fullPrimeCycle, err error) {
	c.primeMu.Lock()
	defer c.primeMu.Unlock()
	if c.primeCycle != cycle {
		return
	}
	cycle.err = err
	c.primeRunning = false
	close(cycle.done)
}

func (c *CachingStore) waitForFullPrimeDone(ctx context.Context, cycle *fullPrimeCycle) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if cycle == nil || cycle.done == nil {
		return ErrCacheUnavailable
	}
	select {
	case <-cycle.done:
	case <-ctx.Done():
		return ctx.Err()
	}
	c.primeMu.Lock()
	defer c.primeMu.Unlock()
	return cycle.err
}

func (c *CachingStore) ensureFullPrime(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if c.cacheFullyPrimed() {
		return nil
	}
	if err := c.beginCacheWorker(); err != nil {
		return errors.Join(ErrCacheUnavailable, err)
	}
	defer c.endCacheWorker()
	if err := c.cacheContextErr(ctx); err != nil {
		return errors.Join(ErrCacheUnavailable, err)
	}
	cycle, owner, suppressed := c.beginLazyFullPrime(time.Now())
	if suppressed {
		return ErrCacheUnavailable
	}
	var err error
	if owner {
		err = c.prime(ctx)
		c.finishFullPrime(cycle, err)
	} else {
		err = c.waitForFullPrimeDone(ctx, cycle)
	}
	if err != nil {
		return errors.Join(ErrCacheUnavailable, fmt.Errorf("prime cache: %w", err))
	}
	if !c.cacheFullyPrimed() {
		return ErrCacheUnavailable
	}
	return nil
}

func (c *CachingStore) beginLazyFullPrime(now time.Time) (*fullPrimeCycle, bool, bool) {
	c.mu.RLock()
	state := c.state
	partial := c.primePartialErr != nil
	c.mu.RUnlock()

	if state == cacheDegraded {
		return nil, false, true
	}

	c.primeMu.Lock()
	defer c.primeMu.Unlock()
	if c.primeRunning {
		return c.primeCycle, false, state != cacheUninitialized
	}
	if c.lastFullPrimeStartedAt.IsZero() {
		return c.startFullPrimeLocked(), true, false
	}
	if now.Sub(c.lastFullPrimeStartedAt) < cacheLazyFullPrimeRetryInterval && (state != cacheUninitialized || partial) {
		return nil, false, true
	}
	return c.startFullPrimeLocked(), true, false
}

func (c *CachingStore) startFullPrimeLocked() *fullPrimeCycle {
	cycle := &fullPrimeCycle{done: make(chan struct{})}
	c.primeRunning = true
	c.primeCycle = cycle
	c.lastFullPrimeStartedAt = time.Now()
	return cycle
}

func (c *CachingStore) cacheFullyPrimed() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.state == cacheLive && c.primePartialErr == nil
}

func (c *CachingStore) beginCacheWorker() error {
	c.lifecycleMu.Lock()
	defer c.lifecycleMu.Unlock()
	if c.stopped {
		return context.Canceled
	}
	c.lifecycleWG.Add(1)
	return nil
}

func (c *CachingStore) endCacheWorker() {
	c.lifecycleWG.Done()
}

func (c *CachingStore) cacheContextErr(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.stopCh:
		return context.Canceled
	default:
		return nil
	}
}

func (c *CachingStore) cacheSleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.stopCh:
		return context.Canceled
	case <-timer.C:
		return nil
	}
}

// StartReconciler launches watchdog reconciliation. Cancel ctx to stop.
// The stagger applies a one-time delay between this call and the first
// reconciler tick (see StaggerOption); agentID is consulted only when
// stagger is WithStaggerAuto. A single "beads cache: stagger=Nms
// agent=..." log line is emitted before the loop starts, even when the
// resolved stagger is zero, so absence is unambiguous.
func (c *CachingStore) StartReconciler(ctx context.Context, stagger StaggerOption, agentID string) {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithCancel(ctx)
	c.lifecycleMu.Lock()
	if c.stopped {
		c.lifecycleMu.Unlock()
		cancel()
		return
	}
	c.cancelFn = cancel
	c.lifecycleWG.Add(1)
	c.lifecycleMu.Unlock()

	offset := stagger.resolve(agentID)

	c.mu.Lock()
	c.stats.StaggerOffsetMs = offset.Milliseconds()
	c.mu.Unlock()

	log.Printf("beads cache: stagger=%dms agent=%s", offset.Milliseconds(), agentID)

	go func() {
		defer c.lifecycleWG.Done()
		c.reconcileLoop(ctx, offset)
	}()
}

// StopReconciler cancels and waits for cache-owned background work.
func (c *CachingStore) StopReconciler() {
	c.lifecycleMu.Lock()
	if !c.stopped {
		close(c.stopCh)
		c.stopped = true
	}
	cancel := c.cancelFn
	c.cancelFn = nil
	c.lifecycleMu.Unlock()
	if cancel != nil {
		cancel()
	}
	c.lifecycleWG.Wait()
}

// Stats returns current cache statistics.
func (c *CachingStore) Stats() CacheStats {
	c.mu.RLock()
	defer c.mu.RUnlock()

	s := c.stats
	switch c.state {
	case cachePartial:
		s.State = "partial"
	case cacheLive:
		s.State = "live"
	case cacheDegraded:
		s.State = "degraded"
	default:
		s.State = "uninitialized"
	}
	return s
}

// IsLive reports whether reads are served from the cache.
func (c *CachingStore) IsLive() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.state == cacheLive
}

// Backing returns the underlying store.
func (c *CachingStore) Backing() Store { return c.backing }

func (c *CachingStore) markFreshLocked(now time.Time) {
	c.lastFreshAt = now
	c.stats.LastFreshAt = now
}

func (c *CachingStore) recordProblem(op string, err error) {
	if err == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.recordProblemLocked(op, err)
}

func (c *CachingStore) recordProblemLocked(op string, err error) {
	if err == nil {
		return
	}
	msg := fmt.Sprintf("%s: %v", op, err)
	now := time.Now()
	c.stats.ProblemCount++
	c.stats.LastProblemAt = now
	c.stats.LastProblem = msg
	if c.problemf != nil {
		if logMsg, ok := c.problemLogMessageLocked(msg, now); ok {
			c.problemf(logMsg)
		}
	}
}

func (c *CachingStore) problemLogMessageLocked(msg string, now time.Time) (string, bool) {
	if c.problemLog == nil {
		c.problemLog = make(map[string]cacheProblemLogState)
	}
	state := c.problemLog[msg]
	if !state.lastAt.IsZero() && now.Sub(state.lastAt) < cacheProblemLogWindow {
		state.suppressed++
		c.problemLog[msg] = state
		return "", false
	}

	logMsg := msg
	if state.suppressed > 0 {
		logMsg = fmt.Sprintf("%s (suppressed %d duplicate logs)", msg, state.suppressed)
	}
	c.problemLog[msg] = cacheProblemLogState{lastAt: now}
	return logMsg, true
}

func (c *CachingStore) updateStatsLocked() {
	c.stats.TotalBeads = len(c.beads)
	totalDeps := 0
	for _, deps := range c.deps {
		totalDeps += len(deps)
	}
	c.stats.TotalDeps = totalDeps
	c.stats.SyncFailures = c.syncFailures
	c.updateCadenceStatsLocked()
}

func beadIDs(beadMap map[string]Bead) []string {
	ids := make([]string, 0, len(beadMap))
	for id := range beadMap {
		ids = append(ids, id)
	}
	return ids
}

type listDependencyCompletenessStore interface {
	listIncludesCompleteDependencies() bool
}

type cacheDependencySnapshotStore interface {
	dependencySnapshotForCache(ids []string) (map[string][]Dep, bool, error)
}

type readyProjectionEnrichmentStore interface {
	enrichReadyProjectionForCache([]Bead) ([]Bead, error)
}

func (c *CachingStore) enrichReadyProjectionForCache(items []Bead) ([]Bead, error) {
	if backing, ok := c.backing.(readyProjectionEnrichmentStore); ok {
		return backing.enrichReadyProjectionForCache(items)
	}
	return items, nil
}

func (c *CachingStore) fetchDepsForBeads(beadMap map[string]Bead) (map[string][]Dep, bool, error) {
	ids := beadIDs(beadMap)
	if backing, ok := c.backing.(cacheDependencySnapshotStore); ok {
		return backing.dependencySnapshotForCache(ids)
	}
	if backing, ok := c.backing.(listDependencyCompletenessStore); ok {
		return depsFromBeads(beadMap, nil, false), backing.listIncludesCompleteDependencies(), nil
	}
	return c.fetchDepsForIDs(ids)
}

func (c *CachingStore) fetchDepsForIDs(ids []string) (map[string][]Dep, bool, error) {
	depMap := make(map[string][]Dep)
	if len(ids) == 0 {
		return depMap, true, nil
	}

	for _, id := range ids {
		deps, err := c.backing.DepList(id, "down")
		if err != nil {
			return depMap, false, fmt.Errorf("listing deps for %s: %w", id, err)
		}
		depMap[id] = cloneDeps(deps)
	}
	return depMap, true, nil
}

func depsFromBeads(beadMap map[string]Bead, depMap map[string][]Dep, useDepMap bool) map[string][]Dep {
	deps := make(map[string][]Dep, len(beadMap))
	for id, b := range beadMap {
		if useDepMap {
			deps[id] = cloneDeps(depMap[id])
			continue
		}
		deps[id] = depsFromBeadFields(b)
	}
	return deps
}

func depsFromBeadFields(b Bead) []Dep {
	// Structured dependencies are the authoritative bead representation when
	// present; Needs is the legacy shorthand used when no dependency objects
	// were carried on the bead payload.
	if len(b.Dependencies) > 0 {
		return cloneDeps(b.Dependencies)
	}
	if len(b.Needs) == 0 {
		return nil
	}
	deps := make([]Dep, 0, len(b.Needs))
	for _, need := range b.Needs {
		depType := "blocks"
		dependsOnID := need
		if strings.Contains(need, ":") {
			parts := strings.SplitN(need, ":", 2)
			if parts[0] != "" && parts[1] != "" {
				depType = parts[0]
				dependsOnID = parts[1]
			}
		}
		deps = append(deps, Dep{IssueID: b.ID, DependsOnID: dependsOnID, Type: depType})
	}
	return deps
}

func beadCarriesDependencyFields(b Bead) bool {
	return len(b.Dependencies) > 0 || len(b.Needs) > 0
}

func cloneDeps(deps []Dep) []Dep {
	if len(deps) == 0 {
		return nil
	}
	cloned := make([]Dep, len(deps))
	copy(cloned, deps)
	return cloned
}
