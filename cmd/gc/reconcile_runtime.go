package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/reconcilekey"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/workqueue"
)

// The keyed reconciler's runtime: session workers draining the workqueue, the
// allocator and the resync pass as paced lanes, a boot pass whose readiness
// waits for every boot key to be reconciled once, and an immutable
// per-generation environment the workers read. The controllers are
// trace-only until P3 and P4; newControllerWiring constructs the runtime when
// the controller latches v2, and the city runtime binds, boots and stops it.
//
// v2 code never reads CityRuntime. A reload writes CityRuntime fields with no
// lock and may rebuild every store (F2), so everything v2 needs from the city
// comes through v2Host's function fields, and everything a reconcile reads of
// the config comes from the published reconcileEnv.
// TestV2RuntimeDoesNotReferenceCityRuntime holds the line.

const (
	v2SessionWorkers     = 8 // CONTRACT §1.1
	v2ReconcileDeadline  = 5 * time.Minute
	v2AllocatorMinGap    = 100 * time.Millisecond
	v2ResyncInterval     = 5 * time.Minute
	v2ResyncMinGap       = 30 * time.Second // debounces unresolved-name storms
	v2LaneBaseBackoff    = time.Second
	v2LaneMaxBackoff     = 30 * time.Second
	v2Jitter             = 0.1
	v2DefaultPatrol      = 30 * time.Second
	v2DefaultStopTimeout = 5 * time.Second
)

// Reason kinds the runtime adds itself. The router's kinds pass through.
const (
	v2ReasonBoot    = "boot"
	v2ReasonResync  = "resync"
	v2ReasonRetry   = "retry"
	v2ReasonPanic   = "panic"
	v2ReasonRequeue = "requeue"
	v2ReasonPatrol  = "patrol"
)

// v2IndexOnlyResyncs are the router's resync reasons that a rebuild alone
// repairs: a failed work census leg (the router keeps the leg's old entries)
// and an overflowed rebuild log or tombstone set. No wake was lost, so they
// skip the enqueue-all. Every other reason (the backstop, an event gap, an
// unresolved name, a supervisor reload, a store swap, a router panic) may have
// lost a wake and gets the full pass.
var v2IndexOnlyResyncs = map[string]bool{"census-error": true, "rebuild-overflow": true, "tombstone-overflow": true}

var errV2Stopped = errors.New("v2 reconciler: stopped before ready")

// v2Host is the only way v2 code reaches the city (F2). newV2Host builds it
// from the city runtime (city_runtime_v2.go); tests build it from fakes.
type v2Host struct {
	sessionsLeg string // the sessions-class store's census label (rowKey.Leg)
	// sessions and censusLegs are the router's census: cached reads of the
	// open session rows and of the work census legs.
	sessions   func() ([]session.Info, error)
	censusLegs func() ([]classStoreCandidate, error)
	// snapshotEnv returns the config, provider and config revision a reload
	// last published. Only publishEnv calls it, on the maintenance goroutine.
	snapshotEnv func() (*config.City, runtime.Provider, string)
	// setInventoryHook installs fn as the inventory lane's per-pass hook.
	// Optional.
	setInventoryHook func(fn func(prev, next *ObservationSnapshot))
	// cityStore and rigStores return the city bead store and the rig bead
	// stores by rig name (cityBeadStore, rigBeadStores); the reload barrier
	// compares them across a reload to spot a store rebuild. Required.
	cityStore func() beads.Store
	rigStores func() map[string]beads.Store
	// retryReload leaves a config reload pending for the next tick without
	// poking (requestConfigReloadRetry). The barrier calls it when a reload
	// aborts or is deferred. Required.
	retryReload func()
	// beginTrace opens a trace cycle for a v2 decision, or returns nil.
	// Required. The worker FS gate calls it from its own goroutine, so it
	// must be built like beginOrdersLaneTrace (orders_lane.go): read the
	// config from the locked snapshot (serviceConfigSnapshot), never the
	// unlocked cr.cfg, and leave out the config revision, which the
	// controller goroutine writes unlocked.
	beginTrace func(trigger string) *sessionReconcilerTraceCycle
	safeTick   func(fn func(), trigger string) (panicked bool)
	stderr     io.Writer
}

// reconcileEnv is immutable once published: a reload publishes a new one at
// Gen+1 and never edits an old one, so a reconcile that loaded it reads one
// consistent generation however long it runs.
type reconcileEnv struct {
	Gen       uint64
	Cfg       *config.City
	SP        runtime.Provider
	ConfigRev string
}

func (e *reconcileEnv) patrol() time.Duration {
	if e == nil || e.Cfg == nil {
		return v2DefaultPatrol
	}
	return e.Cfg.Daemon.PatrolIntervalDuration()
}

func (e *reconcileEnv) shutdownTimeout() time.Duration {
	if e == nil || e.Cfg == nil {
		return v2DefaultStopTimeout
	}
	return e.Cfg.Daemon.ShutdownTimeoutDuration()
}

// The controller groups this build carries. P3 makes the allocator real and
// P4.1 the session group; v2ControllersInBuild and defaultV2Controllers both
// derive from these, so admission and complete() cannot disagree.
const (
	v2SessionControllerReal   = false
	v2AllocatorControllerReal = false
)

// v2Enqueuer is the narrow handle a controller may keep to wake keys: the
// router's Enqueue, with the same reason and key rules.
type v2Enqueuer func(reason string, keys ...reconcilekey.Key)

// v2Controllers are the functions a reconcile runs: function values, not
// interfaces. In P2 both are trace-only; P3 replaces allocator and P4
// replaces session.
//
// session's requeueAfter of zero means no requeue. An immediate self-requeue
// (C0.2) must return a positive epsilon such as time.Nanosecond.
type v2Controllers struct {
	session   func(ctx context.Context, env *reconcileEnv, it workqueue.Item[rowKey]) (requeueAfter time.Duration, err error)
	allocator func(ctx context.Context, env *reconcileEnv, reasons []workqueue.Reason) error
	// bind, when set, receives the runtime's enqueue handle once, at
	// construction.
	bind      func(enqueue v2Enqueuer)
	traceOnly struct{ session, allocator bool }
}

// complete reports whether both controller groups are real: v2 is admissible
// only then (OQ-1).
func (c v2Controllers) complete() bool { return !c.traceOnly.session && !c.traceOnly.allocator }

// defaultV2Controllers returns this build's controllers. In P2 both are the
// skeleton: the session controller records the reason kinds it was handed,
// and both return. Neither writes, probes or starts anything. (The allocator
// lane counts its own wake reasons, whatever the controller.)
func defaultV2Controllers(m *v2Metrics) v2Controllers {
	c := v2Controllers{
		session: func(_ context.Context, _ *reconcileEnv, it workqueue.Item[rowKey]) (time.Duration, error) {
			m.recordTrace(&m.sessionReasons, it.Reasons)
			return 0, nil
		},
		allocator: func(context.Context, *reconcileEnv, []workqueue.Reason) error { return nil },
	}
	c.traceOnly.session, c.traceOnly.allocator = !v2SessionControllerReal, !v2AllocatorControllerReal
	return c
}

// v2Runtime owns the session queue, the router and the lanes that drain them.
type v2Runtime struct {
	epoch    string // C4.3
	host     v2Host
	ctrl     v2Controllers
	sessions *workqueue.Queue[rowKey]
	alloc    *allocatorLane
	resyncCh chan struct{}
	// allocGate and resyncGate hold the allocator and resync passes while
	// the session queue is held (reloadBarrier, workerFSGate).
	allocGate  *laneGate
	resyncGate *laneGate
	// resyncFull says a pending resync needs the enqueue-all, not only a
	// rebuild (v2IndexOnlyResyncs).
	resyncFull atomic.Bool
	router     *reconcileRouter
	env        atomic.Pointer[reconcileEnv]
	bootCov    atomic.Pointer[workqueue.Coverage[rowKey]] // set once the boot pass has run
	bootOnce   sync.Once                                  // records the first boot's duration
	sweep      atomic.Pointer[resyncSweep]
	ready      atomic.Bool // boot has returned ready
	noStore    atomic.Bool // the city has no bead store, so boot never runs (MAINT-003)
	barrier    *reloadBarrier
	fs         *workerFSGate
	metrics    *v2Metrics
	report     v2QueueReport
	workers    int
	rand       func() float64 // jitter for the queue and the lanes

	// mu orders lifecycle changes and env publication. It is never held
	// while waiting on a goroutine.
	mu        sync.Mutex
	ctx       context.Context
	cancel    context.CancelFunc
	started   bool
	stopped   bool
	stopCh    chan struct{} // closed by stop
	fsArmed   bool
	workersWG sync.WaitGroup
	lanes     []<-chan struct{}
}

// newV2Runtime constructs an unstarted runtime. The router is live at once,
// so keys routed before boot wait in the queue.
func newV2Runtime(host v2Host, ctrl v2Controllers, metrics *v2Metrics) *v2Runtime {
	if host.stderr == nil {
		host.stderr = io.Discard
	}
	rt := &v2Runtime{
		epoch:    newInventoryEpoch(),
		host:     host,
		ctrl:     ctrl,
		resyncCh: make(chan struct{}, 1),
		stopCh:   make(chan struct{}),
		metrics:  metrics,
		workers:  v2SessionWorkers,
		rand:     rand.Float64,
	}
	rt.barrier = &reloadBarrier{rt: rt, deadline: reloadReconcileDeadline}
	rt.fs = &workerFSGate{sample: func() (fsPressureStatus, bool) { return currentFSPressureStatus(host.stderr) }}
	jitter := func() float64 { return rt.rand() }
	rt.sessions = workqueue.New(workqueue.Config[rowKey]{Jitter: v2Jitter, Rand: jitter})
	rt.alloc = newAllocatorLane(jitter)
	rt.allocGate = newLaneGate(rt.alloc.signal)
	rt.resyncGate = newLaneGate(rt.signalResync)
	rt.router = newReconcileRouter(host.sessionsLeg, routerSink{
		addSession: func(k rowKey, r routeReason) {
			rt.sessions.Add(k, workqueue.LaneHot, workqueue.Reason(r))
		},
		wakeAllocator: func(r routeReason) { rt.alloc.wake(workqueue.Reason(r)) },
		requestResync: rt.requestResync,
	}, host.stderr)
	if ctrl.bind != nil {
		ctrl.bind(rt.router.Enqueue)
	}
	return rt
}

// bindHost hands an unstarted runtime the city it reconciles. The controller
// wiring builds the runtime before the city runtime exists, so its router can
// take socket keys from the first one; only the sessions leg and stderr are
// known then. newCityRuntime binds the rest once, before run can start it, so
// every goroutine that reads the host starts after the bind.
func (rt *v2Runtime) bindHost(host v2Host) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.started || rt.stopped {
		panic("v2 reconciler: host bound after start")
	}
	host.sessionsLeg = rt.host.sessionsLeg
	if host.stderr == nil {
		host.stderr = rt.host.stderr
	}
	rt.host = host
}

// publishEnv publishes the host's current config as the next generation,
// unless the current env already holds the same config, provider and
// revision, and returns the current env. Boot publishes Gen 1; the reload
// barrier calls it after every reload, so the env follows what the host
// serves rather than what apply reported.
func (rt *v2Runtime) publishEnv() *reconcileEnv {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	// Read under mu, so concurrent publishes take generations in the order
	// they read the config.
	cfg, sp, rev := rt.host.snapshotEnv()
	old := rt.env.Load()
	if old != nil && old.Cfg == cfg && old.SP == sp && old.ConfigRev == rev {
		return old
	}
	e := &reconcileEnv{Gen: 1, Cfg: cfg, SP: sp, ConfigRev: rev}
	if old != nil {
		e.Gen = old.Gen + 1
	}
	rt.env.Store(e)
	return e
}

// requestResync wakes the resync lane. Wakes fold into the lane's next pass,
// which runs the enqueue-all if any folded reason needs it.
func (rt *v2Runtime) requestResync(reason string) {
	rt.metrics.recordResyncRequest(reason)
	if !v2IndexOnlyResyncs[reason] {
		rt.resyncFull.Store(true)
	}
	rt.signalResync()
}

func (rt *v2Runtime) signalResync() {
	select {
	case rt.resyncCh <- struct{}{}:
	default:
	}
}

// boot brings the runtime up and returns once it is ready: every open session
// row has been reconciled once and the allocator has completed a pass
// (MAINT-010, MAINT-012, C4.5), or with ctx's error. It is idempotent, so a
// startup retry after a panic neither re-runs a boot pass that finished nor
// starts a second set of goroutines (MAINT-005).
//
//  1. Publish env Gen 1.
//  2. Run the first resync pass synchronously, covering every key it adds. A
//     failed sessions census is retried with backoff: an error is not an
//     empty city.
//  3. Install the inventory pass hook, then start the workers and lanes. The
//     boot pass's allocator wake is buffered, so the lane's first pass runs
//     at once.
//  4. Wait for the boot coverage and the allocator's first pass.
//
// While it waits, a non-nil patrol runs at every patrol interval: the caller
// records what boot still waits on, and the stuck-reconcile check, which
// otherwise run only on maintenance ticks, after readiness.
func (rt *v2Runtime) boot(ctx context.Context, patrol func()) error {
	started := time.Now()
	if rt.env.Load() == nil {
		rt.publishEnv()
	}
	var tick <-chan time.Time
	if patrol != nil {
		t := time.NewTicker(rt.env.Load().patrol())
		defer t.Stop()
		tick = t.C
	}
	cov, err := rt.bootPass(ctx, tick, patrol)
	if err != nil {
		return err
	}
	runCtx, ok := rt.start(ctx)
	if !ok {
		return errV2Stopped
	}
	for _, ready := range []<-chan struct{}{cov.Done(), rt.alloc.primed} {
	wait:
		for {
			select {
			case <-ready:
				break wait
			case <-tick:
				patrol()
			case <-ctx.Done():
				return ctx.Err()
			case <-runCtx.Done():
				return errV2Stopped
			}
		}
	}
	rt.bootOnce.Do(func() { rt.metrics.recordBoot(time.Since(started)) })
	rt.ready.Store(true)
	return nil
}

// bootPass runs the boot resync pass once, retrying a failed sessions census
// with backoff, and returns its coverage. patrol runs on every tick while it
// waits to retry.
func (rt *v2Runtime) bootPass(ctx context.Context, tick <-chan time.Time, patrol func()) (*workqueue.Coverage[rowKey], error) {
	cov := rt.bootCov.Load()
	for failures := 0; cov == nil; {
		// This pass satisfies every resync requested before it, including
		// the one a failed attempt's rebuild asked for.
		select {
		case <-rt.resyncCh:
		default:
		}
		rt.resyncFull.Store(false)
		var err error
		if cov, err = rt.resyncPass(v2ReasonBoot); err == nil {
			break
		}
		failures++
		d := laneBackoff(failures, rt.rand)
		fmt.Fprintf(rt.host.stderr, "v2 reconciler: boot census failed (retry in %s): %v\n", d.Round(time.Millisecond), err) //nolint:errcheck // best-effort stderr
		t := time.NewTimer(d)
	retry:
		for {
			select {
			case <-t.C:
				break retry
			case <-tick:
				patrol()
			case <-ctx.Done():
				t.Stop()
				return nil, ctx.Err()
			case <-rt.stopCh:
				t.Stop()
				return nil, errV2Stopped
			}
		}
	}
	rt.bootCov.Store(cov)
	return cov, nil
}

// start launches the workers, the allocator lane and the resync lane once,
// and returns the runtime's context, which parent bounds. It reports false
// after stop.
func (rt *v2Runtime) start(parent context.Context) (context.Context, bool) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.stopped {
		return nil, false
	}
	if rt.started {
		return rt.ctx, true
	}
	// The hook goes in before any worker can read an observation: a flip
	// committed while a boot reconcile runs must still route its key.
	if rt.host.setInventoryHook != nil {
		rt.host.setInventoryHook(rt.router.OnInventoryPass)
	}
	ctx, cancel := context.WithCancel(parent)
	rt.started, rt.ctx, rt.cancel = true, ctx, cancel
	for range rt.workers {
		rt.workersWG.Add(1)
		go func() {
			defer rt.workersWG.Done()
			rt.work(ctx)
		}()
	}
	rt.metrics.startAllocator(time.Now())
	rt.lanes = append(rt.lanes,
		startGatedPacedLane(ctx, rt.env.Load().patrol(), v2AllocatorMinGap, rt.alloc.wakeCh, func(bool) bool { return rt.allocatorPass(ctx) }),
		startGatedPacedLane(ctx, v2ResyncInterval, v2ResyncMinGap, rt.resyncCh, rt.resyncLanePass),
	)
	return ctx, true
}

// stop shuts the runtime down (C1.8, GUAR-013, INC-026): cancel, close the
// queue's admission so queued keys are never started, then join the workers
// and lanes, bounded by the shutdown timeout. A goroutine still running at the
// bound is logged and abandoned; it holds no lock stop needs. No lock is held
// while joining.
func (rt *v2Runtime) stop() {
	rt.mu.Lock()
	if rt.stopped {
		rt.mu.Unlock()
		return
	}
	rt.stopped = true
	close(rt.stopCh)
	cancel, lanes := rt.cancel, rt.lanes
	rt.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	rt.sessions.ShutDown()
	rt.alloc.stopTimer()

	joined := make(chan struct{})
	go func() {
		rt.workersWG.Wait()
		for _, done := range lanes {
			<-done
		}
		close(joined)
	}()
	bound := rt.env.Load().shutdownTimeout()
	t := time.NewTimer(bound)
	defer t.Stop()
	select {
	case <-joined:
	case <-t.C:
		fmt.Fprintf(rt.host.stderr, "v2 reconciler: workers still running %s after stop; abandoning them\n", bound) //nolint:errcheck // best-effort stderr
	}
}

// work is one session worker: it reconciles keys until the queue shuts down.
func (rt *v2Runtime) work(ctx context.Context) {
	for {
		it, ok := rt.sessions.Get(ctx)
		if !ok {
			return
		}
		started := time.Now()
		outcome := rt.reconcile(ctx, it)
		// A retry's wait includes its backoff, so only a first attempt
		// samples enqueue-to-start latency.
		rt.metrics.recordReconcile(started.Sub(it.AddedAt), it.Failures == 0, v2BeadEventWoke(it.Reasons), time.Since(started), outcome)
		rt.observe(it)
	}
}

// reconcile runs the session controller for one item and reports its outcome
// to the queue before Done (which is deferred, so it runs exactly once per
// Get): a panic or an error backs the key off, success forgets its failures
// and arms requeueAfter.
func (rt *v2Runtime) reconcile(ctx context.Context, it workqueue.Item[rowKey]) v2Outcome {
	defer rt.sessions.Done(it.Key)
	env := rt.env.Load()
	var requeue time.Duration
	var err error
	panicked := rt.host.safeTick(func() {
		rctx, cancel := context.WithTimeout(ctx, v2ReconcileDeadline)
		defer cancel()
		requeue, err = rt.ctrl.session(rctx, env, it)
	}, "v2-reconcile "+it.Key.Leg+"/"+it.Key.ID)
	switch {
	case panicked:
		rt.sessions.AddRateLimited(it.Key, workqueue.Reason{Kind: v2ReasonPanic})
		return v2Panicked
	case err != nil:
		rt.sessions.AddRateLimited(it.Key, workqueue.Reason{Kind: v2ReasonRetry, Detail: err.Error()})
		return v2Failed
	}
	rt.sessions.Forget(it.Key)
	if requeue > 0 {
		rt.sessions.AddAfter(it.Key, requeue, workqueue.Reason{Kind: v2ReasonRequeue})
	}
	return v2Succeeded
}

// observe credits a finished reconcile to the boot and resync coverages.
func (rt *v2Runtime) observe(it workqueue.Item[rowKey]) {
	if boot := rt.bootCov.Load(); boot != nil {
		boot.Observe(it.Key, it.LastSeq)
	}
	if s := rt.sweep.Load(); s != nil {
		s.cov.Observe(it.Key, it.LastSeq)
		rt.finishSweep(s)
	}
}

// resyncSweep is one resync pass's enqueue-all, timed until every key it
// added has been reconciled (target 6).
type resyncSweep struct {
	cov     *workqueue.Coverage[rowKey]
	started time.Time
	once    sync.Once
}

func (rt *v2Runtime) finishSweep(s *resyncSweep) {
	select {
	case <-s.cov.Done():
		s.once.Do(func() { rt.metrics.recordSweep(time.Since(s.started), false) })
	default:
	}
}

// census is what a rebuild reads: the host's cached census.
func (rt *v2Runtime) census() routerCensus {
	return routerCensus{sessions: rt.host.sessions, legs: rt.host.censusLegs}
}

// resyncPass rebuilds the router's indexes, adds every open session row on
// the resync lane and wakes the allocator. It returns the sweep's coverage,
// or the sessions census error, in which case it adds nothing: a failed read
// is not an empty city. A failed work census leg is not an error here; the
// router keeps that leg's old entries and asks for another resync.
func (rt *v2Runtime) resyncPass(kind string) (*workqueue.Coverage[rowKey], error) {
	rows, sessErr, _ := rt.router.rebuild(rt.census())
	if sessErr != nil {
		return nil, sessErr
	}
	// The sweep is published before its Adds: an open coverage remembers what
	// workers finish before Track catches up.
	s := &resyncSweep{cov: &workqueue.Coverage[rowKey]{}, started: time.Now()}
	if old := rt.sweep.Swap(s); old != nil {
		// A superseded sweep records how long it ran before this one
		// replaced it.
		old.once.Do(func() { rt.metrics.recordSweep(time.Since(old.started), true) })
	}
	for _, k := range rows {
		if seq, ok := rt.sessions.Add(k, workqueue.LaneResync, workqueue.Reason{Kind: kind}); ok {
			s.cov.Track(k, seq)
		}
	}
	s.cov.Seal()
	rt.finishSweep(s)
	rt.alloc.wake(workqueue.Reason{Kind: kind})
	return s.cov, nil
}

// resyncLanePass runs one resync lane pass: the enqueue-all for the backstop
// and for any folded reason that needs it, else a rebuild alone. A full pass
// that fails or panics, or a held backstop, leaves the enqueue-all pending
// for the next pass. It reports false when held.
func (rt *v2Runtime) resyncLanePass(wake bool) bool {
	if !rt.resyncGate.enter() {
		if !wake {
			rt.resyncFull.Store(true)
		}
		return false
	}
	defer rt.resyncGate.exit()
	full := rt.resyncFull.Swap(false) || !wake
	ok := false
	rt.host.safeTick(func() {
		var err error
		if full {
			_, err = rt.resyncPass(v2ReasonResync)
		} else {
			_, err, _ = rt.router.rebuild(rt.census())
		}
		if err != nil {
			fmt.Fprintf(rt.host.stderr, "v2 reconciler: resync: %v\n", err) //nolint:errcheck // best-effort stderr
			return
		}
		ok = true
	}, "v2-resync")
	if full && !ok {
		rt.resyncFull.Store(true)
	}
	return true
}

// allocatorPass runs one allocator pass over the reasons the lane collected,
// unless the lane is held or its backoff gate is closed and nothing urgent is
// waiting. A held pass keeps its reasons for the pass the release wakes. It
// reports false only when held, so a held pass does not count toward the
// lane's pacing.
func (rt *v2Runtime) allocatorPass(ctx context.Context) bool {
	if !rt.allocGate.enter() {
		return false
	}
	defer rt.allocGate.exit()
	reasons, ok := rt.alloc.take()
	if !ok {
		return true
	}
	rt.metrics.recordTrace(&rt.metrics.allocatorWakes, reasons)
	env := rt.env.Load()
	started := time.Now()
	var err error
	panicked := rt.host.safeTick(func() { err = rt.ctrl.allocator(ctx, env, reasons) }, "v2-allocator")
	switch {
	case panicked:
		rt.alloc.fail(workqueue.Reason{Kind: v2ReasonPanic})
	case err != nil:
		rt.alloc.fail(workqueue.Reason{Kind: v2ReasonRetry, Detail: err.Error()})
	default:
		rt.alloc.succeed()
	}
	rt.metrics.recordAllocatorPass(time.Now(), time.Since(started), panicked || err != nil)
	return true
}

// allocatorLane collects allocator wakes for the paced lane and owns the
// allocator's backoff gate (A2): after a failed pass, non-urgent wakes are
// held until the retry time, when a timer wakes the lane.
type allocatorLane struct {
	wakeCh chan struct{}
	primed chan struct{} // closed after the first successful pass
	rand   func() float64

	mu        sync.Mutex
	reasons   []workqueue.Reason // deduped by Kind, at most workqueue.MaxReasons
	urgent    bool
	failures  int
	retryAt   time.Time
	timer     *time.Timer
	primeOnce sync.Once
}

func newAllocatorLane(rand func() float64) *allocatorLane {
	return &allocatorLane{wakeCh: make(chan struct{}, 1), primed: make(chan struct{}), rand: rand}
}

// wake records r for the next pass and signals the lane, unless the backoff
// gate is closed and r is not urgent.
func (a *allocatorLane) wake(r workqueue.Reason) {
	a.mu.Lock()
	a.addReasonLocked(r)
	a.urgent = a.urgent || r.Urgent
	gated := !r.Urgent && time.Now().Before(a.retryAt)
	a.mu.Unlock()
	if !gated {
		a.signal()
	}
}

func (a *allocatorLane) signal() {
	select {
	case a.wakeCh <- struct{}{}:
	default:
	}
}

func (a *allocatorLane) addReasonLocked(r workqueue.Reason) {
	for _, have := range a.reasons {
		if have.Kind == r.Kind {
			return
		}
	}
	if len(a.reasons) < workqueue.MaxReasons {
		a.reasons = append(a.reasons, r)
	}
}

// take returns the collected reasons for a pass, or false while the backoff
// gate is closed and nothing urgent is waiting. A pass with no reasons is the
// patrol backstop.
func (a *allocatorLane) take() ([]workqueue.Reason, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.urgent && time.Now().Before(a.retryAt) {
		return nil, false
	}
	reasons := a.reasons
	if len(reasons) == 0 {
		reasons = []workqueue.Reason{{Kind: v2ReasonPatrol}}
	}
	a.reasons, a.urgent = nil, false
	return reasons, true
}

// fail closes the gate until a jittered retry time (1s doubling to 30s,
// C1.4) and arms the timer that wakes the lane then.
func (a *allocatorLane) fail(r workqueue.Reason) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.addReasonLocked(r)
	a.failures++
	d := laneBackoff(a.failures, a.rand)
	a.retryAt = time.Now().Add(d)
	if a.timer != nil {
		a.timer.Stop()
	}
	a.timer = time.AfterFunc(d, a.signal)
}

func (a *allocatorLane) succeed() {
	a.mu.Lock()
	a.failures, a.retryAt = 0, time.Time{}
	if a.timer != nil {
		a.timer.Stop()
		a.timer = nil
	}
	a.mu.Unlock()
	a.primeOnce.Do(func() { close(a.primed) })
}

func (a *allocatorLane) stopTimer() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.timer != nil {
		a.timer.Stop()
		a.timer = nil
	}
}

// laneBackoff is min(1s·2^(failures-1), 30s), jittered by ±10%.
func laneBackoff(failures int, rand func() float64) time.Duration {
	d := v2LaneBaseBackoff
	for i := 1; i < failures && d < v2LaneMaxBackoff; i++ {
		d *= 2
	}
	d = min(d, v2LaneMaxBackoff)
	return time.Duration(float64(d) * (1 + v2Jitter*(2*rand()-1)))
}
