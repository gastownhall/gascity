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
	onChange     func(eventType, beadID, runID, sessionID string, payload json.RawMessage)
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

var _ ConditionalAssignmentReleaser = (*CachingStore)(nil)

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
func NewCachingStore(backing Store, onChange func(eventType, beadID, runID, sessionID string, payload json.RawMessage)) *CachingStore {
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
func adaptLegacyOnChange(fn func(eventType, beadID string, payload json.RawMessage)) func(eventType, beadID, runID, sessionID string, payload json.RawMessage) {
	if fn == nil {
		return nil
	}
	return func(eventType, beadID string, _, _ string, payload json.RawMessage) {
		fn(eventType, beadID, payload)
	}
}

// SetPrimeRetryDelayForTest overrides the inter-attempt backoff Prime
// uses when the backing store's full scan fails, so tests can exercise
// prime-failure paths without real multi-second sleeps. Test-only.
func (c *CachingStore) SetPrimeRetryDelayForTest(fn func(attempt int) time.Duration) {
	c.primeRetryDelay = fn
}

func newCachingStore(backing Store, idPrefix string, onChange func(eventType, beadID, runID, sessionID string, payload json.RawMessage)) *CachingStore {
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
		c.beads[b.ID] = cloneBead(b)
		if depsComplete && depErr == nil {
			c.deps[b.ID] = cloneDeps(depMap[b.ID])
		} else {
			c.deps[b.ID] = depsFromBeadFields(b)
		}
		delete(c.deletedSeq, b.ID)
		if !recentLocalMutation(c.localBeadAt[b.ID], now) {
			delete(c.beadSeq, b.ID)
			delete(c.localBeadAt, b.ID)
		}
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
			c.beads[id] = b
			delete(c.deletedSeq, id)
			delete(c.beadSeq, id)
			if depsComplete && depErr == nil {
				c.deps[id] = cloneDeps(depMap[id])
			} else {
				c.deps[id] = depsFromBeadFields(b)
			}
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
