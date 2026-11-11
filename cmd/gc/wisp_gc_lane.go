package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// The wisp GC purge, on its own lane.
//
// The purge (wispGC.runGC) can run for minutes on a city with a large
// closed-molecule corpus. It decides nothing about sessions, so it runs on a
// paced background lane rather than in the serial tick: the tick only wakes
// the lane, at the slot the purge used to hold (tickWakeWispGCLane), and no
// later phase, and no later tick, waits for a purge.
//
// The tick stays the purge's only trigger. A tick that stops before the wake
// slot (a quiescent city, an FS-pressure skip) wakes nothing, as it ran no
// purge before; the lane's own timer only paces wakes and installs reloaded
// policy, and never starts a purge by itself.

const (
	// wispGCLaneSafeTickTrigger names the lane in safeTick panic lines.
	wispGCLaneSafeTickTrigger = "wisp-gc-lane"
	// wispGCLaneTraceTrigger is the trace-cycle trigger for a purge. The
	// wisp_gc operation record keeps its site and name; only the cycle it
	// lands in changes, from a controller tick to a purge pass.
	wispGCLaneTraceTrigger TraceTickTrigger = "wisp_gc"
	// wispGCLaneTraceDetail is the trigger detail of a purge cycle: every
	// purge runs on a tick's wake.
	wispGCLaneTraceDetail = "tick_wake"
	// wispGCLaneFallbackInterval paces the lane when the config read at
	// start sets no wisp GC interval, as the work-arm lane does.
	wispGCLaneFallbackInterval = 10 * time.Minute
)

// wispGCLane runs the wisp GC purge off the controller tick. It owns the
// tracker: only a pass reads or writes the installed one, so its cadence and
// cursor change only between purges.
type wispGCLane struct {
	wakeCh chan struct{}

	// passMu serializes passes and guards active.
	passMu sync.Mutex
	// active is the installed tracker; nil while wisp GC is disabled.
	active wispGC

	stageMu sync.Mutex
	// staged is the tracker the last successful reload built, waiting for
	// the next pass to install it, when stagedPending is set. A nil staged
	// tracker with stagedPending set is a disable, not "no update".
	staged        wispGC
	stagedPending bool

	statusMu   sync.Mutex
	lastPassAt time.Time
}

func newWispGCLane(seed wispGC) *wispGCLane {
	return &wispGCLane{wakeCh: make(chan struct{}, 1), active: seed}
}

// wispGCLaneOf returns this runtime's lane, creating it on first use. The
// lane is seeded once with the tracker the runtime was constructed with
// (cr.wg); every later policy reaches it through stage.
func (cr *CityRuntime) wispGCLaneOf() *wispGCLane {
	cr.wispGCLaneOnce.Do(func() { cr.wispGCLane = newWispGCLane(cr.wg) })
	return cr.wispGCLane
}

// wake asks the lane for a pass. Non-blocking; wakes that land while one is
// pending join it.
func (l *wispGCLane) wake() {
	select {
	case l.wakeCh <- struct{}{}:
	default:
	}
}

// stage hands the lane the tracker a successful config reload built from the
// config it applied, nil when that config disables wisp GC. It never waits
// for a purge: the lane installs it before its next pass.
func (l *wispGCLane) stage(next wispGC) {
	l.stageMu.Lock()
	defer l.stageMu.Unlock()
	l.staged, l.stagedPending = next, true
}

// installStagedLocked installs the latest staged tracker, if any, carrying
// the last completed purge's progress over to it (carryWispGCState). passMu
// must be held.
func (l *wispGCLane) installStagedLocked() {
	l.stageMu.Lock()
	next, pending := l.staged, l.stagedPending
	l.staged, l.stagedPending = nil, false
	l.stageMu.Unlock()
	if !pending {
		return
	}
	l.active = carryWispGCState(l.active, next)
}

// notePass records a purge that finished.
func (l *wispGCLane) notePass(at time.Time) {
	l.statusMu.Lock()
	defer l.statusMu.Unlock()
	l.lastPassAt = at
}

// lastPassAge is how long ago the last purge finished, and whether one has.
func (l *wispGCLane) lastPassAge(now time.Time) (time.Duration, bool) {
	l.statusMu.Lock()
	defer l.statusMu.Unlock()
	if l.lastPassAt.IsZero() {
		return 0, false
	}
	return now.Sub(l.lastPassAt), true
}

// startWispGCLane starts the lane goroutine and returns the func that stops
// it and waits for it to exit, a purge in flight included. A wake starts a
// pass at once if the lane has idled as long as its last purge ran, and
// otherwise once it has (startGatedPacedLane); the pacing interval is the
// wisp GC interval of the config published at start.
func (cr *CityRuntime) startWispGCLane(ctx context.Context) (stop func()) {
	lane := cr.wispGCLaneOf()
	interval := wispGCLaneFallbackInterval
	if cfg := cr.serviceConfigSnapshot(); cfg != nil {
		if d := cfg.Daemon.WispGCIntervalDuration(); d > 0 {
			interval = d
		}
	}
	laneCtx, cancel := context.WithCancel(ctx)
	done := startGatedPacedLane(laneCtx, interval, 0, lane.wakeCh, func(wake bool) bool {
		ran := false
		cr.safeTick(func() { cr.runWispGCLanePass(laneCtx, wake, &ran) }, wispGCLaneSafeTickTrigger)
		return ran
	})
	return func() {
		cancel()
		<-done
	}
}

// runWispGCLanePass is one lane pass: install any staged policy, then purge if
// a tick's wake asked for it, the installed tracker is due, the city is not
// quiescent and it has a graph store. It sets
// *ran just before the purge starts, so a purge that panics still counts
// toward the lane's pacing.
func (cr *CityRuntime) runWispGCLanePass(ctx context.Context, wake bool, ran *bool) {
	if ctx.Err() != nil {
		return
	}
	lane := cr.wispGCLaneOf()
	lane.passMu.Lock()
	defer lane.passMu.Unlock()
	lane.installStagedLocked()
	wg := lane.active
	if !wake || wg == nil || !wg.shouldRun(time.Now()) {
		return
	}
	if cr.beadsQuiescent.Load() {
		// The city is suspended with nothing running: its stores are not
		// touched until it resumes.
		return
	}
	graphStore := cr.graphBeadStore()
	if graphStore.Store == nil {
		return
	}
	*ran = true
	cr.purgeWispGC(wg, graphStore)
	lane.notePass(time.Now())
}

// purgeWispGC runs one purge and reports it. The molecule/wisp/workflow purge
// arm routes through the typed graph-class store; the read-message retention
// arm through the typed messaging-class store. The closed session purge arm
// gets the sessions class only when it is relocated onto a SQLite infra
// ledger; on an unsplit city it gets nothing and never touches the work
// store. The purge is recorded as the wisp_gc operation in its own trace
// cycle, aborted if the purge panics.
func (cr *CityRuntime) purgeWispGC(wg wispGC, graphStore beads.GraphStore) {
	trace := cr.beginWispGCLaneTrace()
	completion := TraceCompletionAborted
	defer func() {
		if trace != nil {
			trace.end(completion, traceRecordPayload{"phase": "wisp_gc"})
		}
	}()
	phaseStart := time.Now()
	// On a city that relocates the graph class, the attachment holders and
	// the v1 poured molecule roots live on the work legs, not the binding
	// the GC sweeps (wispGCLegs). A single-store city hands none: its
	// graph-class store is its work store.
	var legs wispGCLegs
	if _, relocated := graphClassBinding(cr.storageRoutes); relocated { // residency:allow — asks only whether the graph class is relocated; the legs come from the census plan (workLegs)
		legs = cr.workLegs()
	}
	purged, gcErr := wg.runGC(legs, graphStore, cr.infraSessionLedger(), cr.mailBeadStore(), time.Now())
	payload := map[string]any{"purged": purged}
	if legs != nil {
		// The work arm runs on its own lane; the purge only wakes it and
		// records how stale its last pass is.
		workLane := cr.workGCLaneOf()
		workLane.wake()
		if age, ran := workLane.lastPassAge(time.Now()); ran {
			payload["work_arm_last_pass_age_ms"] = age.Milliseconds()
		}
	}
	if trace != nil {
		trace.RecordControllerOperation(TraceSiteControllerTickPhase, TraceReasonRetained, TraceOutcomeComplete,
			"wisp_gc", time.Since(phaseStart), payload)
	}
	if gcErr != nil {
		for _, line := range strings.Split(gcErr.Error(), "\n") {
			if line == "" {
				continue
			}
			fmt.Fprintf(cr.stderr, "%s: wisp gc: %s\n", cr.logPrefix, line) //nolint:errcheck // best-effort stderr
		}
	}
	if purged > 0 {
		fmt.Fprintf(cr.stdout, "Bead GC: purged %d expired bead(s)\n", purged) //nolint:errcheck // best-effort stdout
	}
	completion = TraceCompletionCompleted
}

// beginWispGCLaneTrace opens a purge's trace cycle from the published config
// snapshot. The config revision is omitted: it is written unlocked on the
// controller goroutine, and a purge is not a config-revision boundary.
func (cr *CityRuntime) beginWispGCLaneTrace() *sessionReconcilerTraceCycle {
	if cr.trace == nil {
		return nil
	}
	return cr.trace.beginCycle(sessionReconcilerTraceCycleInfo{
		TickTrigger:   string(wispGCLaneTraceTrigger),
		TriggerDetail: wispGCLaneTraceDetail,
		CityPath:      cr.cityPath,
	}, cr.serviceConfigSnapshot(), nil)
}

// tickWakeWispGCLane wakes the wisp GC lane, at the slot the purge held when
// it ran in the tick, so the ticks that used to reach a purge (startup, event
// pokes, patrols) still ask for one and those stopped before it do not. The
// lane decides whether a purge is due. The lane runs on its own goroutine, so
// the tick's record is where its liveness shows: the age of its last purge.
func (cr *CityRuntime) tickWakeWispGCLane(p *tickPass) bool {
	phaseStart := time.Now()
	lane := cr.wispGCLaneOf()
	lane.wake()
	fields := map[string]any{}
	if age, ran := lane.lastPassAge(phaseStart); ran {
		fields["last_pass_age_ms"] = age.Milliseconds()
	}
	p.recordPhase(TraceSiteControllerTickPhase, "wake_wisp_gc_lane", phaseStart, fields)
	return false
}
