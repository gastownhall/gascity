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
// The purge (the graph-class molecule/wisp/workflow purge, the closed-session
// sweep and the read-message retention arm; wisp_gc.go) once ran as the
// serial controller tick's wisp_gc phase, after the session reconcile. On a
// city with a large closed-molecule corpus it runs for minutes — on ours, a
// 107.6s median and a 292.9s worst pass, 77 times in one day — and every
// session decision that waits for the next tick waits behind it: a restart
// poke, a drain or a spawn that lands behind the purge queues for the purge,
// then for the next whole tick (#2604). The purge decides nothing about
// sessions; it only deletes beads, so it runs on its own goroutine the way
// #5318 moved the detached-orphan sweep and the completions read, and the
// tick's wake_wisp_gc_lane slot — the exact slot the purge used to run in —
// hands every tick that reaches it one wake.
//
// The purge changes the stores while a tick may be reading them. That is safe
// for the same reason every other lane's writes are: the tick never had the
// stores to itself. Agents' `gc bd` and `bd` commands, the API and the orders
// lane write during a tick; the core pack's reaper order purges old closed
// wisps and prunes closed session beads off the tick; #5318 moved the
// detached-orphan sweep, which writes to the work store, onto its own lane.
// Every tick phase already reads stores that change under it, and the purge
// adds one more writer of the same kind.
//
// A due purge is store work from its store snapshot to the purge's end
// (storeWork): the suspend paths observe that and only retire a drained
// scope's pair once it drops, so no purge reads a pair that is being torn
// down, and a purge already reading holds the retirement (beads_quiescence.go).

// wispGCLaneTraceTrigger is the trace-cycle trigger for lane purges. The
// wisp_gc operation record keeps its site and name; only the cycle it lands
// in changes, from a controller tick to a wisp GC pass.
const wispGCLaneTraceTrigger = "wisp-gc-lane"

// wispGCLane owns the wisp GC tracker off the controller tick: the active
// tracker, a successful config reload's staged replacement for it, and the
// pass status the tick's wake slot records. Created on first use. Only the
// lane (its goroutine, or a test driving its passes) touches the active
// tracker; a reload only stages, so a reload never reads the tracker a purge
// in flight is mutating, and never waits for one either.
type wispGCLane struct {
	// wakeCh carries "a tick reached the wisp_gc slot" wakes. Buffered 1: a
	// wake that lands while a purge runs (or waits out the duty cycle) joins
	// the one pass that follows it, so a burst of ticks is never a queue of
	// purges.
	wakeCh chan struct{}

	// stageMu guards the staged policy. successful config reloads stage a
	// tracker built from the reloaded config; the lane installs it between
	// passes. The mutex is never held across a store call: a mid-purge reload
	// stages and returns at once, and takes effect on a later pass.
	stageMu sync.Mutex
	// hasStaged says a reload staged policy the lane has not installed.
	// staged is the staged tracker; staged == nil with hasStaged set means
	// the reload disabled the GC.
	hasStaged bool
	staged    wispGC
	// stagedDisable remembers a disable staged into this pending update: an
	// enable staged over it (latest policy wins) still installs the fresh
	// tracker's own state instead of carrying the retired one's cadence and
	// cursor. A disable ends a cadence: the GC turning on again is due at
	// once, as it is at process start.
	stagedDisable bool

	// active is the installed tracker: the seed the constructor built, or
	// what the last install left. Only the lane's pass goroutine reads and
	// writes it — a purge mutates lastRun and the session cursor on it — and
	// a test drives the passes one at a time.
	active wispGC

	// statusMu guards the last-pass status the tick records when it wakes the
	// lane: a lane on its own goroutine shows its liveness there.
	statusMu   sync.Mutex
	lastPassAt time.Time
	passRan    bool
}

func newWispGCLane(seed wispGC) *wispGCLane {
	return &wispGCLane{wakeCh: make(chan struct{}, 1), active: seed}
}

// wispGCLaneOf returns this runtime's lane, creating it on first use and
// seeding it from the tracker the constructor built for the boot config
// (cr.wg) — that tracker is the initial policy, and from here on the lane
// owns the tracker while reloads stage theirs.
func (cr *CityRuntime) wispGCLaneOf() *wispGCLane {
	cr.wispGCLaneOnce.Do(func() { cr.wispGCLane = newWispGCLane(cr.wg) })
	return cr.wispGCLane
}

// wake asks the lane for a purge. Non-blocking; harmless when no lane
// goroutine is running.
func (l *wispGCLane) wake() {
	select {
	case l.wakeCh <- struct{}{}:
	default:
	}
}

// stage leaves a reload's policy for the lane to install between passes.
// next is built from the reloaded config; nil is a disable.
func (l *wispGCLane) stage(next wispGC) {
	l.stageMu.Lock()
	defer l.stageMu.Unlock()
	l.hasStaged = true
	l.staged = next
	if next == nil {
		l.stagedDisable = true
	}
}

// stagedPending reports whether a reload staged policy the lane has not
// installed.
func (l *wispGCLane) stagedPending() bool {
	l.stageMu.Lock()
	defer l.stageMu.Unlock()
	return l.hasStaged
}

// installStaged installs the policy a successful reload staged, if it left
// one, as the active tracker — the lane runs this between passes. The latest
// staged policy wins. A disable that the update ends disabled clears the
// tracker; one it ends enabled clears with it installs the fresh tracker's
// own cadence and cursor, dropping the retired tracker's progress (that
// disable ended the cadence); an enabled-to-enabled replacement carries the
// last completed purge's lastRun and session cursor onto the new policy.
func (l *wispGCLane) installStaged() {
	l.stageMu.Lock()
	hasStaged, staged, disable := l.hasStaged, l.staged, l.stagedDisable
	l.hasStaged, l.staged, l.stagedDisable = false, nil, false
	l.stageMu.Unlock()
	if !hasStaged {
		return
	}
	switch {
	case staged == nil:
		l.active = nil
	case disable:
		l.active = staged
	default:
		l.active = carryWispGCState(l.active, staged)
	}
}

// lastPass reports when the lane's last purge finished and whether one has.
func (l *wispGCLane) lastPass() (at time.Time, ran bool) {
	l.statusMu.Lock()
	defer l.statusMu.Unlock()
	return l.lastPassAt, l.passRan
}

func (l *wispGCLane) notePass(at time.Time) {
	l.statusMu.Lock()
	defer l.statusMu.Unlock()
	l.lastPassAt, l.passRan = at, true
}

// tickWakeWispGCLane wakes the wisp GC lane. The purge itself used to run in
// this exact slot as the tick's wisp_gc phase; now it runs on the lane
// (wisp_gc_lane.go), and a tick that reaches this slot — any patrol, poke or
// reload-followup tick the pressure gate did not skip — hands the lane one
// wake at the point the purge used to start. The lane runs the woken purge at
// once when it is idle past its duty-cycle gap; the tick never waits for it,
// and never calls into the tracker.
//
// The lane runs on its own goroutine, so the tick's record is where its
// liveness shows: the age of its last pass that purged.
func (cr *CityRuntime) tickWakeWispGCLane(p *tickPass) bool {
	phaseStart := time.Now()
	lane := cr.wispGCLaneOf()
	lane.wake()
	fields := map[string]any{}
	passAt, passRan := lane.lastPass()
	addBackstopAgeFields(fields, passAt, "purge", passRan)
	p.recordPhase(TraceSiteControllerTickPhase, "wake_wisp_gc_lane", phaseStart, fields)
	return false
}

// startWispGCLane starts the lane goroutine and returns a channel closed when
// it exits. The lane is paced by startGatedPacedLane: a tick's wake runs a
// due purge at once once the duty-cycle gap since the previous purge has
// passed (never longer than the wisp-GC interval below), and the backstop
// timer fires once per interval otherwise — a timer-only pass declines, so
// only a wake (including one the timer delivers after a duty-cycle wait) may
// start a purge. The interval is read once, at start, for pacing and the
// backstop; when the GC is off at start (or the config is unreadable) the
// work arm's 10-minute fallback paces the lane until a reload turns the GC
// on, because the reloadable due-time authority is the installed tracker's
// shouldRun, not this number.
func (cr *CityRuntime) startWispGCLane(ctx context.Context) <-chan struct{} {
	lane := cr.wispGCLaneOf()
	interval := 10 * time.Minute
	if cfg := cr.serviceConfigSnapshot(); cfg != nil {
		if d := cfg.Daemon.WispGCIntervalDuration(); d > 0 {
			interval = d
		}
	}
	return startGatedPacedLane(ctx, interval, 0, lane.wakeCh, func(wake bool) (ran bool) {
		return cr.wispGCLanePass(ctx, time.Now(), wake)
	})
}

// wispGCLanePass is one lane pass: install the policy a reload staged, decline
// what may not purge, and run one due purge as store work. ran reports whether
// a purge ran: a declined pass costs the duty cycle nothing, so the wake that
// later runs the overdue purge is not paced against a pass that did nothing.
func (cr *CityRuntime) wispGCLanePass(ctx context.Context, now time.Time, wake bool) (ran bool) {
	lane := cr.wispGCLaneOf()
	// A reload's stage is consumed between purges; a reload mid-purge lands
	// on a later pass, whether this one declines or runs.
	lane.installStaged()
	if !wake || ctx.Err() != nil {
		return false
	}
	tracker := lane.active
	if tracker == nil || !tracker.shouldRun(now) {
		return false
	}
	// Register the pass as store work before taking its store snapshot: the
	// suspend paths observe this (storeWorkInFlight) and hold a drained
	// scope's retirement while the purge reads it, and a city that goes
	// quiescent under this pass is what the recheck below declines for.
	var endStoreWork func()
	if cr.cs != nil {
		endStoreWork = cr.cs.beginStoreWork()
		defer endStoreWork()
	}
	if ctx.Err() != nil || cr.beadsQuiescent.Load() {
		return false
	}
	graphStore := cr.graphBeadStore()
	if graphStore == (beads.GraphStore{}) {
		return false
	}
	var legs wispGCLegs
	if _, relocated := graphClassBinding(cr.storageRoutes); relocated { // residency:allow — asks only whether the graph class is relocated; the legs come from the census plan (workLegs)
		legs = cr.workLegs()
	}
	// The ran flag flips before the purge: a panic safeTick recovers below
	// still counts as a pass for the duty cycle, so the next wake is paced
	// against a purge that happened, and the trace cycle and the store-work
	// registration both end with it.
	ran = true
	cr.runWispGCPurge(lane, tracker, legs, graphStore, now)
	return true
}

// runWispGCPurge runs one due purge and reports it: the wisp_gc operation
// record, in the purge's own trace cycle rather than a tick's, the purge-count
// and per-line error output the tick's phase printed unchanged, and the work
// arm's wake on a city with relocated work legs. A panic inside the purge is
// recovered here (safeTick); the pass still reports it (the cycle ends
// aborted) and the duty cycle and the store-work accounting still see it.
func (cr *CityRuntime) runWispGCPurge(lane *wispGCLane, tracker wispGC, legs wispGCLegs, graphStore beads.GraphStore, now time.Time) {
	phaseStart := now
	trace := cr.beginTraceCycle(wispGCLaneTraceTrigger, "wisp_gc", nil)
	completion := TraceCompletionAborted
	defer func() {
		lane.notePass(time.Now())
		if trace != nil {
			trace.end(completion, traceRecordPayload{"phase": "wisp_gc", "trigger": wispGCLaneTraceTrigger})
		}
	}()
	var purged int
	var gcErr error
	panicked := cr.safeTick(func() {
		purged, gcErr = tracker.runGC(legs, graphStore, cr.infraSessionLedger(), cr.mailBeadStore(), now)
	}, "wisp-gc-lane")
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
	if !panicked {
		completion = TraceCompletionCompleted
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
}
