package main

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/telemetry"
	"github.com/gastownhall/gascity/internal/workqueue"
)

// The v2 runtime's holds: the reload barrier (C4.4), which pauses every
// reconcile while a config reload applies, and the worker FS-pressure gate
// (MAINT-026), which pauses them under sustained IO pressure. Both hold the
// session queue, the allocator lane and the resync lane under their own name;
// holds are a set, so neither releases the other's. The tick runs a v2
// controller's config reload under the barrier (reloadUnderBarrier), and run
// arms the FS gate once the city is ready.

const (
	reloadReconcileDeadline = 30 * time.Second // C4.4 step 2
	v2FSGateInterval        = 5 * time.Second
)

const (
	v2HoldReload      = "reload"
	v2HoldFSPressure  = "fs-pressure"
	v2ReasonReload    = "reload"
	v2ResyncStoreSwap = "store-swap"
	v2FSGateTrigger   = "v2-workers"
)

var (
	errReloadBarrierTimeout  = errors.New("reconciles still running at the reload deadline")
	errReloadBarrierDeferred = errors.New("reconciles that outlived the last aborted reload are still running")
)

// hold stops the session workers, the allocator lane and the resync lane from
// starting anything until release(name).
func (rt *v2Runtime) hold(name string) {
	rt.sessions.Hold(name)
	rt.allocGate.hold(name)
	rt.resyncGate.hold(name)
}

// release ends the named hold. Releasing a name not held does nothing.
func (rt *v2Runtime) release(name string) {
	rt.sessions.Release(name)
	rt.allocGate.release(name)
	rt.resyncGate.release(name)
}

// waitIdle returns nil once no reconcile, allocator pass or resync pass is
// running, or ctx's error. Under a hold nothing new starts, so idle stays
// idle; holds themselves do not count.
func (rt *v2Runtime) waitIdle(ctx context.Context) error {
	if err := rt.sessions.WaitIdle(ctx); err != nil {
		return err
	}
	if err := rt.allocGate.waitIdle(ctx); err != nil {
		return err
	}
	return rt.resyncGate.waitIdle(ctx)
}

// stuckSince reports whether a reconcile, allocator pass or resync pass that
// started before t is still running.
func (rt *v2Runtime) stuckSince(t time.Time) bool {
	if oldest := rt.sessions.Stats().OldestInFlight; !oldest.IsZero() && oldest.Before(t) {
		return true
	}
	return rt.allocGate.runningSince(t) || rt.resyncGate.runningSince(t)
}

// laneGate holds a paced lane's passes. A pass that finds a hold active is
// skipped (and reports so to startGatedPacedLane, so it does not count toward
// the lane's pacing), and the release that ends the last hold wakes the lane
// to run it. waitIdle reports when no pass is running.
type laneGate struct {
	signal func() // wakes the lane

	mu      sync.Mutex
	holds   map[string]struct{}
	running bool
	started time.Time     // when the running pass entered
	skipped bool          // a pass was skipped under the current holds
	changed chan struct{} // closed and replaced when a pass ends
}

func newLaneGate(signal func()) *laneGate {
	return &laneGate{signal: signal, holds: make(map[string]struct{}), changed: make(chan struct{})}
}

func (g *laneGate) hold(name string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.holds[name] = struct{}{}
}

func (g *laneGate) release(name string) {
	g.mu.Lock()
	_, held := g.holds[name]
	delete(g.holds, name)
	wake := held && len(g.holds) == 0 && g.skipped
	if wake {
		g.skipped = false
	}
	g.mu.Unlock()
	if wake {
		g.signal()
	}
}

// enter marks a pass running and reports true, or reports false and
// remembers the skip while a hold is active. After true, call exit.
func (g *laneGate) enter() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.holds) > 0 {
		g.skipped = true
		return false
	}
	g.running, g.started = true, time.Now()
	return true
}

// runningSince reports whether a pass that entered before t is running.
func (g *laneGate) runningSince(t time.Time) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.running && g.started.Before(t)
}

func (g *laneGate) exit() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.running = false
	close(g.changed)
	g.changed = make(chan struct{})
}

func (g *laneGate) waitIdle(ctx context.Context) error {
	for {
		g.mu.Lock()
		idle, changed := !g.running, g.changed
		g.mu.Unlock()
		if idle {
			return nil
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// reloadIntent is what the tick knows about the reload it hands the barrier:
// where it came from, and whether it is soft (accept config drift instead of
// draining, city_runtime.go's soft-acceptance guard). Hooks receive it so
// P4.4 can accept drift only on a soft reload.
type reloadIntent struct {
	Source reloadSource
	Soft   bool
}

// reloadHook runs inside the barrier after a reload that did not fail, with
// the env the reconciles will resume on. It may amend the reply, which is
// sent after every hook.
type reloadHook func(ctx context.Context, intent reloadIntent, env *reconcileEnv, reply *reloadControlReply)

// reloadBarrier runs a config reload with every reconcile paused (C4.4,
// amended by A3). The tick calls run on the maintenance goroutine, so the
// reload stays serialized with every maintenance phase that reads the fields
// it writes (F2).
type reloadBarrier struct {
	rt       *v2Runtime
	deadline time.Duration
	// afterApply runs first: P3's allocator pass and S_{g+1} publish.
	afterApply []reloadHook
	// afterPublish runs after afterApply: P4.4's soft drift-hash rewrite
	// (MAINT-025), which needs the post-reload desired state.
	afterPublish []reloadHook

	// mu makes run exclusive and guards the retry state below.
	mu sync.Mutex
	// abortedAt is when the last wait missed its deadline, zero once a wait
	// succeeds. While something that started before it still runs, a
	// non-manual reload is deferred without a hold.
	abortedAt time.Time
	// pendingSoft carries a soft intent across an aborted or deferred reload
	// to the retry that applies it.
	pendingSoft bool
}

// run applies a config reload under the barrier and returns apply's reply,
// or a failed reply and an error when the reload did not apply:
// errReloadBarrierDeferred, errReloadBarrierTimeout, or ctx's error. reply,
// when set, receives the final reply before the reconciles resume.
//
//  0. After an abort, defer a non-manual reload without holding anything
//     while a reconcile or lane pass older than the abort is still running:
//     leave it pending and keep its soft intent. A hung reconcile would
//     otherwise hold every worker for the deadline at every patrol. A manual
//     reload always tries.
//  1. Hold the session queue, the allocator lane and the resync lane.
//  2. Wait until no reconcile, allocator pass or resync pass is running, up to
//     the deadline. On timeout, abort: leave the reload pending for the next
//     patrol without a poke, alert, reply failed and release the holds.
//     apply never runs, so the old config stays (R31). On ctx cancellation
//     (shutdown), reply failed and leave no retry.
//  3. Run apply (reloadConfigTraced, unchanged: its storage refusal, provider
//     swap listing rule and superseded-reload rejection all hold). A rebuilt
//     city or rig store asks for a resync, so the router leaves the old
//     stores. Stores compare by handle: a standalone city (no controller
//     state) reopens its stores on every reload, so there every reload asks
//     for one resync. Telling a reopen of the same store from a swap would
//     need the stores' identity, which beads.Store does not expose; the
//     extra resync is idempotent.
//  4. Publish the next env if the host now serves a different config,
//     provider or revision, whatever apply reported, and by defer even if
//     apply panics after publishing. A failed or superseded reload changes
//     nothing on the host, so it publishes nothing (GUAR-014);
//     rejectSuperseded has already left its retry pending.
//  5. Unless the reload failed, run the afterApply hooks, then the
//     afterPublish hooks, each under safeTick. A no-change reload runs them on
//     the current env: a soft reload accepts drift on no change too
//     (MAINT-025). A hook panic stops the hooks and adds a warning.
//  6. Reply, wake the allocator and release the holds.
//
// The deadline bounds only the wait, not apply: a provider-swap apply may stop
// every session while the workers stay held. A panic in apply leaks no hold;
// the tick's safeTick and its dirty-restore defer handle the rest.
func (b *reloadBarrier) run(ctx context.Context, intent reloadIntent, apply func() reloadControlReply, reply func(reloadControlReply)) (reloadControlReply, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	rt := b.rt
	intent.Soft = intent.Soft || b.pendingSoft
	if intent.Source != reloadSourceManual && !b.abortedAt.IsZero() && rt.stuckSince(b.abortedAt) {
		b.leavePending(intent)
		return b.fail(reply, errReloadBarrierDeferred,
			"Reload deferred: reconciles that outlived the last aborted reload are still running; keeping the current config and retrying at the next patrol.")
	}
	rt.hold(v2HoldReload)
	defer rt.release(v2HoldReload)

	wctx, cancel := context.WithTimeout(ctx, b.deadline)
	err := rt.waitIdle(wctx)
	cancel()
	switch {
	case ctx.Err() != nil:
		return b.canceled(ctx, intent, reply)
	case err != nil:
		b.abortedAt = time.Now()
		err = fmt.Errorf("%w (%s): %w", errReloadBarrierTimeout, b.deadline, err)
		fmt.Fprintf(rt.host.stderr, "v2 reconciler: config reload aborted: %v; keeping current config, retry at next patrol\n", err) //nolint:errcheck // best-effort stderr
		telemetry.RecordConfigReload(ctx, "", string(intent.Source), string(reloadOutcomeFailed), 0, err)
		b.leavePending(intent)
		return b.fail(reply, err,
			fmt.Sprintf("Reload aborted: reconciles were still running after %s; keeping the current config and retrying at the next patrol.", b.deadline))
	}
	b.abortedAt, b.pendingSoft = time.Time{}, false

	city, rigs := rt.host.cityStore(), rt.host.rigStores()
	defer rt.publishEnv()
	r := apply()
	if rt.host.cityStore() != city || !maps.Equal(rt.host.rigStores(), rigs) {
		rt.requestResync(v2ResyncStoreSwap)
	}
	env := rt.publishEnv()
	if r.Outcome != reloadOutcomeFailed {
		b.runHooks(ctx, intent, env, &r)
	}
	if reply != nil {
		reply(r)
	}
	// The wake's pass waits out the hold, which the deferred release ends.
	rt.alloc.wake(workqueue.Reason{Kind: v2ReasonReload})
	return r, nil
}

// runHooks runs the afterApply hooks, then the afterPublish hooks. A hook
// panic stops the rest: the config is applied, so the reply keeps its outcome
// and gains a warning.
func (b *reloadBarrier) runHooks(ctx context.Context, intent reloadIntent, env *reconcileEnv, r *reloadControlReply) {
	for _, h := range slices.Concat(b.afterApply, b.afterPublish) {
		if b.rt.host.safeTick(func() { h(ctx, intent, env, r) }, "v2-reload-hook") {
			r.Warnings = append(r.Warnings, "config applied, but a post-reload step panicked; see the controller log")
			return
		}
	}
}

// leavePending leaves a reload that did not apply, with its soft intent,
// pending for the next patrol. The deferral alerts nothing: the abort that
// started it already did.
func (b *reloadBarrier) leavePending(intent reloadIntent) {
	b.rt.host.retryReload()
	b.pendingSoft = intent.Soft
}

// canceled ends a reload whose ctx ended during the wait: the controller is
// stopping, so nothing is retried.
func (b *reloadBarrier) canceled(ctx context.Context, intent reloadIntent, reply func(reloadControlReply)) (reloadControlReply, error) {
	err := fmt.Errorf("config reload canceled: %w", ctx.Err())
	telemetry.RecordConfigReload(ctx, "", string(intent.Source), string(reloadOutcomeFailed), 0, err)
	return b.fail(reply, err, "Reload canceled: the controller is stopping; keeping the current config.")
}

func (b *reloadBarrier) fail(reply func(reloadControlReply), err error, msg string) (reloadControlReply, error) {
	r := reloadControlReply{Outcome: reloadOutcomeFailed, Error: msg}
	if reply != nil {
		reply(r)
	}
	return r, err
}

// workerFSGate pauses the v2 reconciles under sustained FS pressure, as the
// tick's gate skips ticks (MAINT-026): while pressure is high it holds every
// lane, and after maxConsecutiveFSPressureSkips patrol intervals of continuous
// hold it releases for one interval (the forced window) so reconciles cannot
// starve. Low pressure or an unreadable reading releases at once (fail open).
// It is armed only after readiness (F8), so it never delays boot. It records
// no supervisor skipped-tick event, so that event's counts keep meaning ticks.
// The reload barrier ignores this hold, as a config change bypasses the
// tick's gate.
type workerFSGate struct {
	sample func() (fsPressureStatus, bool)

	// Owned by the gate goroutine.
	held        bool
	heldSince   time.Time
	forcedUntil time.Time // end of the forced window, zero outside one
	logged      bool      // this episode's hold was logged
}

// armFSGate starts the worker FS gate. It arms nothing and reports false
// before boot has returned ready (F8) or after stop. It is idempotent.
func (rt *v2Runtime) armFSGate() bool {
	if !rt.ready.Load() {
		return false
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.stopped {
		return false
	}
	if rt.fsArmed {
		return true
	}
	rt.fsArmed = true
	ctx, done := rt.ctx, make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(v2FSGateInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-t.C:
				rt.host.safeTick(func() { rt.fsGateSample(now) }, "v2-fs-gate")
			}
		}
	}()
	rt.lanes = append(rt.lanes, done)
	return true
}

// fsGateSample takes one pressure sample at now and moves the gate.
func (rt *v2Runtime) fsGateSample(now time.Time) {
	g := rt.fs
	status, ok := g.sample()
	patrol := rt.env.Load().patrol()
	switch {
	case !ok || !status.High:
		if g.held {
			rt.release(v2HoldFSPressure)
		}
		g.held, g.forcedUntil, g.logged = false, time.Time{}, false
	case now.Before(g.forcedUntil):
	case !g.held:
		rt.hold(v2HoldFSPressure)
		g.held, g.heldSince, g.forcedUntil = true, now, time.Time{}
		if !g.logged {
			fmt.Fprintf(rt.host.stderr, "v2 reconciler: FS pressure high (some avg60=%.2f > threshold=%.1f), holding reconciles\n", //nolint:errcheck // best-effort stderr
				status.Avg60, status.Threshold)
			g.logged = true
		}
		rt.traceFSGate(func(trace *sessionReconcilerTraceCycle) {
			recordFSPressureSkippedTickTrace(trace, v2FSGateTrigger, status, 0)
		})
	case now.Sub(g.heldSince) >= maxConsecutiveFSPressureSkips*patrol:
		rt.release(v2HoldFSPressure)
		g.held, g.forcedUntil = false, now.Add(patrol)
		fmt.Fprintf(rt.host.stderr, "v2 reconciler: FS pressure high (some avg60=%.2f > threshold=%.1f), releasing reconciles for %s after %d held patrols\n", //nolint:errcheck // best-effort stderr
			status.Avg60, status.Threshold, patrol, maxConsecutiveFSPressureSkips)
		rt.traceFSGate(func(trace *sessionReconcilerTraceCycle) {
			recordFSPressureForcedTickTrace(trace, v2FSGateTrigger, status, maxConsecutiveFSPressureSkips)
		})
	}
}

func (rt *v2Runtime) traceFSGate(record func(*sessionReconcilerTraceCycle)) {
	trace := rt.host.beginTrace(v2FSGateTrigger)
	record(trace)
	trace.end(TraceCompletionCompleted, traceRecordPayload{"phase": "fs_gate"})
}
