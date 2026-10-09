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

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
)

// The planner as the controller runs it behind session_reconciler=v2, beside
// its executor and the external-reads lane (K1), trace-only until C9. It
// reaches the city only through plannerHost (F2), which newPlannerHost builds.

// Boot states, for the pass record and the startup watchdog.
const (
	plannerBootCensus  = "census" // C11's boot census has not passed
	plannerBootGate    = "gate"   // no pass has completed with the boot gate open
	plannerBootReady   = "ready"
	plannerBootNoStore = "no-store" // no bead store: boot never runs (MAINT-003)
)

// The boot census retry backoff (laneBackoff).
const (
	v2LaneBaseBackoff = time.Second
	v2LaneMaxBackoff  = 30 * time.Second
	v2Jitter          = 0.1
)

var errV2Stopped = errors.New("v2 reconciler: stopped before ready")

// plannerHost is the only way the planner reaches the city (F2). bindHost
// fills gather's Env and Recording; only publishEnv calls snapshotEnv; a nil
// bootCensus skips C11's check, and a nil capabilities C0.7's.
type plannerHost struct {
	gather           gatherEnv
	snapshotEnv      func() (*config.City, runtime.Provider, string)
	setInventoryHook func(fn func(prev, next *ObservationSnapshot))
	bootCensus       func() (v2SessionMigration, error)
	capabilities     func() error
	beginTrace       func(trigger string) *sessionReconcilerTraceCycle // off the controller goroutine
	inventoryFields  func() map[string]any                             // the pass record's view of the inventory lane
	safeTick         func(fn func(), trigger string) (panicked bool)
	rec              events.Recorder
	stderr           io.Writer
}

// plannerRuntime owns the planner and what runs beside it. mu orders
// lifecycle changes and env publication.
type plannerRuntime struct {
	planner *planner
	host    plannerHost
	exec    *effectExecutor
	lane    atomic.Pointer[externalReadsLane] // built at start
	env     atomic.Pointer[reconcileEnv]
	// softReload amends a reload's reply; v2SoftReloadUnavailable until C7c
	// hands the planner a soft-reload request instead (v5 M2).
	softReload func(intent reloadIntent, r *reloadControlReply)
	census     atomic.Bool   // C11's boot census passed
	ready      chan struct{} // closed by the first completed pass with the gate open
	readyOnce  sync.Once
	noStore    atomic.Bool

	mu               sync.Mutex
	started, stopped bool
	cancel           context.CancelFunc
	laneDone         <-chan struct{}
}

// newDefaultPlanner constructs this build's planner runtime, unbound: the
// city runtime binds its host (bindHost) before run can start it.
func newDefaultPlanner(stderr io.Writer) *plannerRuntime {
	if stderr == nil {
		stderr = io.Discard
	}
	rt := &plannerRuntime{host: plannerHost{stderr: stderr}, softReload: v2SoftReloadUnavailable, ready: make(chan struct{})}
	rt.exec = newEffectExecutor(func(s settlement) { rt.planner.settlements.post(s) }, stderr)
	rt.planner = newPlanner(realPlannerClock{}, func() time.Duration { return rt.env.Load().patrol() }, rt.pass, newInflightMap(), rt.exec.stop, stderr)
	if v2EffectsEnabled(reconcilerModeLookupEnv) {
		rt.planner.effects = rt.exec
	}
	return rt
}

// bindHost hands an unstarted runtime the city it plans for, once, before
// any goroutine that reads the host starts.
func (rt *plannerRuntime) bindHost(h plannerHost) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.started || rt.stopped {
		panic("v2 planner: host bound after start")
	}
	if h.stderr == nil {
		h.stderr = rt.host.stderr
	}
	h.gather.Env, h.gather.Stderr = rt.env.Load, h.stderr
	h.gather.Recording = func() *externalReadsRecording {
		if l := rt.lane.Load(); l != nil { // nil: a test's pass before start
			return l.recording()
		}
		return nil
	}
	h.gather.ReadyWaits = func() map[string]bool {
		if l := rt.lane.Load(); l != nil {
			return l.readyWaitSet()
		}
		return nil
	}
	rt.host = h
	rt.planner.rec = h.rec
	rt.planner.emitRecord = rt.emitPassRecord
	creates, err := newCreateEffects(createEffectHost{cityPath: h.gather.CityPath, cityName: h.gather.CityName, lookPath: h.gather.LookPath, stderr: h.stderr})
	if err != nil {
		fmt.Fprintf(h.stderr, "v2 planner: creates disabled: %v\n", err) //nolint:errcheck // best-effort stderr
	}
	rt.planner.creates = creates
}

// publishEnv publishes the host's config as the next generation unless the
// current env holds the same config, provider and revision, and returns the
// current env. reloadV2 calls it after every reload (CONTRACT v5 P7).
func (rt *plannerRuntime) publishEnv() *reconcileEnv {
	rt.mu.Lock()
	defer rt.mu.Unlock()
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

// boot refuses stores that cannot fence (C0.7), publishes env Gen 1, refuses
// enterprise-era session rows on a live census (C11; a failed census is
// retried with backoff, as an error is not an empty city), starts the
// planner, and returns once a pass has completed with the boot gate open
// (CONTRACT v5 P2), or with ctx's error. It is idempotent, so a startup
// retry after a panic resumes it (MAINT-005).
func (rt *plannerRuntime) boot(ctx context.Context) error {
	if rt.host.capabilities != nil {
		if refusal := rt.host.capabilities(); refusal != nil {
			rt.planner.alert(alertBootRefused, "", refusal.Error())
			return refusal
		}
	}
	if rt.env.Load() == nil {
		rt.publishEnv()
	}
	for failures := 0; !rt.census.Load(); {
		var m v2SessionMigration
		var err error
		if rt.host.bootCensus != nil {
			m, err = rt.host.bootCensus()
		}
		if err == nil {
			if refusal := m.refusal(); refusal != nil {
				rt.planner.alert(alertBootRefused, "", refusal.Error())
				return refusal
			}
			rt.census.Store(true)
			break
		}
		failures++
		d := laneBackoff(failures, rand.Float64)
		fmt.Fprintf(rt.host.stderr, "v2 planner: boot census failed (retry in %s): %v\n", d.Round(time.Millisecond), err) //nolint:errcheck // best-effort stderr
		t := time.NewTimer(d)
		select {
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		}
	}
	if !rt.start(ctx) {
		return errV2Stopped
	}
	select {
	case <-rt.ready:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// start launches the lane, the inventory hook and the planner loop once,
// under a context parent bounds, and reports false after stop.
func (rt *plannerRuntime) start(parent context.Context) bool {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.stopped {
		return false
	}
	if rt.started {
		return true
	}
	rt.started = true
	ctx, cancel := context.WithCancel(parent)
	rt.cancel = cancel
	h := rt.host
	lane := newExternalReadsLane(rt.env.Load().patrol(), h.gather.externalReadsEnv, func() { rt.planner.markDirty("external-reads") }, h.safeTick, h.rec, h.stderr)
	lane.addWaitsStep()
	lane.addPoolSteps(rt.planner.out.summary.Load, rt.planner.postExecutionStalled)
	rt.lane.Store(lane)
	if h.setInventoryHook != nil {
		h.setInventoryHook(func(prev, next *ObservationSnapshot) {
			if inventoryChanged(prev, next) {
				rt.planner.markDirty("inventory")
			}
		})
	}
	rt.laneDone = lane.start(ctx)
	go rt.planner.run(ctx)
	return true
}

// waitDependencyClosed runs the lane's waits step alone when a pending deps
// wait watches id (GUAR-010). It never blocks, and does nothing before start.
func (rt *plannerRuntime) waitDependencyClosed(id string) {
	if l := rt.lane.Load(); l != nil {
		l.dependencyClosed(id)
	}
}

// pass is a trace-only pass over the host's city, whose changed rows go to
// the trace (R6). The first that completes with the boot gate open makes the
// runtime ready.
func (rt *plannerRuntime) pass(now time.Time) passResult {
	res := rt.planner.tracePass(rt.host.gather, now)
	rec := rt.planner.out.record.Load()
	if rec.Err != "" {
		return res
	}
	if rt.planner.boot.open() {
		rt.readyOnce.Do(func() { close(rt.ready) })
	}
	rt.traceRows(rec.Rows)
	return res
}

// traceRows records each changed row's decision, in one cycle per pass.
func (rt *plannerRuntime) traceRows(rows []rowTrace) {
	if len(rows) == 0 || rt.host.beginTrace == nil {
		return
	}
	t := rt.host.beginTrace("v2-pass")
	for _, r := range rows {
		t.RecordDecision(TraceSiteV2SessionDecision, TraceReasonCode(r.Reason), TraceOutcomeCode(r.Outcome), r.Template, r.SessionName, map[string]any{
			"key": r.Key.Leg + "/" + r.Key.ID,
		})
	}
	t.end(TraceCompletionCompleted, traceRecordPayload{"phase": "pass"})
}

// stop shuts the planner down (CONTRACT v5 P6, GUAR-013): admission, then
// the executor until the shutdown deadline, then the lane, whose reads and
// steps are joined by the same deadline or abandoned.
func (rt *plannerRuntime) stop() {
	deadline := time.Now().Add(rt.env.Load().shutdownTimeout())
	rt.mu.Lock()
	if rt.stopped {
		rt.mu.Unlock()
		return
	}
	rt.stopped = true
	cancel, lane, laneDone := rt.cancel, rt.lane.Load(), rt.laneDone
	rt.mu.Unlock()
	rt.planner.stop(deadline)
	if cancel == nil {
		return
	}
	cancel()
	ctx, done := context.WithDeadline(context.Background(), deadline)
	defer done()
	select {
	case <-laneDone:
	case <-ctx.Done():
	}
	if lane.join(ctx) != nil {
		fmt.Fprintln(rt.host.stderr, "v2 planner: external reads still running at the shutdown deadline; abandoning them") //nolint:errcheck // best-effort stderr
	}
}

func (rt *plannerRuntime) bootState() string {
	switch {
	case rt.noStore.Load():
		return plannerBootNoStore
	case !rt.census.Load():
		return plannerBootCensus
	}
	select {
	case <-rt.ready:
		return plannerBootReady
	default:
		return plannerBootGate
	}
}

// passRecord is a maintenance tick's reconcile_pass record, a summary of
// the planner's v2_pass record (emitPassRecord): what boot waits on, the
// passes and their wakes, what admission let through and held back, and
// legacyEntries, the refused legacy session entries, which must stay 0.
func (rt *plannerRuntime) passRecord(now time.Time, legacyEntries int64) map[string]any {
	m := rt.planner.metrics.snapshot(now)
	return map[string]any{
		"boot": rt.bootState(), "legacy_session_entries": legacyEntries, "passes": m.Passes,
		"wakes": m.Wakes, "admitted": m.Admitted, "deferred": deferralKeys(m.Deferred),
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
