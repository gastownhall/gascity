package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/session"
)

// The external-reads lane (CONTRACT C0.4 as amended by C4). The v2
// allocator's pass does no live I/O, but a leg its cache cannot feed (bd
// ledgers, native Dolt, Postgres) cannot answer demand from that cache: bd
// folds blocked work into "open" (EB-42o8), and out-of-process bd writes
// reach the cache only at its re-scan. So this one lane runs every live or
// slow read the allocator needs, off the pass, and publishes the results as
// one recording that v2DemandReads and the decide's scale_check input serve
// from.
//
// A pass reads its sources concurrently, each in its own goroutine, at most
// one read in flight per source: every lane-fed leg in any demand leg set
// (the work census, the default-probe target stores) through
// legacyDemandReads; when the config has an on_demand named session,
// the city store's closed named-session index on any leg, since no cache
// holds closed history; and the custom scale_check commands (I5).
//
// The source deadline bounds only how long a pass waits before it publishes;
// it never decides whether a read counts. Each read stores its result when it
// ends, whenever that is, stamped when it started (C5.4(3)) and ended. A pass
// publishes, for each source it reads, the latest ended result, which stays
// fresh for 3 × patrol from its own end; a read still in flight at publish is
// marked on that result (InFlightSince), and a read that ends after its pass
// published wakes the lane, whose next pass publishes it. Only a source with
// no result yet whose read has outlived its budget publishes
// errSourceTimeout. A leg turns partial only when its result goes stale or
// failed. The lane wakes the allocator when a publish changes content or
// turns a stale source fresh.
//
// After publishing, the pass starts its due steps, each on its own goroutine
// under a deadline, one run in flight per step, so a slow step never delays
// the reads. The one step today is legacy's demand-pass repairs
// (POOL-019/020), at most once a minute from the end of
// the last run, in legacy order over every leg: the session stamp, the
// control-dispatcher route repair (which emits control.dispatcher_scope_gap
// itself), and the four migration repairs. The four stay in the lane by owner
// decision (2026-10-05): ops step G′'s counts were never taken, and the doctor
// --fix move is re-measured after cutover. Those writes run only under v2;
// legacy keeps them in its tick, so the two never both run. P3-9 and P4.1d
// add their steps here.
//
// The lane always reads, suspended city or not (CONTRACT v5 R4), so a
// suspended city's drains keep their work reads. Only its steps keep
// legacy's suspension gate: while the city is suspended a pass starts none,
// as legacy's demand pass returns before its repairs (POOL-001). The lane
// serves only v2.
//
// The lane is paced at the patrol interval and woken by key-less socket and
// API pokes (a CLI writer such as gc sling pokes key-less), a supervisor
// reload, a store swap after the barrier, a resume, a create that wrote a row
// on a lane-fed leg, and a read that ended after its pass published.
//
// Unwired in this slice: P3-7 starts it, wires its wakes and
// joins it before closing the stores.

const (
	// externalReadsMinGap is the least idle time between two woken passes,
	// so a burst of CLI pokes costs one pass.
	externalReadsMinGap = 2 * time.Second
	// externalReadsSourceDeadline is how long a pass waits for its reads
	// before it publishes, and a store source's budget.
	externalReadsSourceDeadline = 10 * time.Second
	// externalReadsRepairInterval is the least time between the end of one
	// run of the demand repairs step and the start of the next.
	externalReadsRepairInterval = time.Minute
	// externalReadsStepDeadline bounds each step run. Steps write, so they
	// stop at their own checkpoints.
	externalReadsStepDeadline = time.Minute
	// externalReadsFreshPatrols is how many patrol intervals a source result
	// stays fresh after its read ended.
	externalReadsFreshPatrols = 3
	// externalReadsSafeTickTrigger names the lane in safeTick panic lines.
	externalReadsSafeTickTrigger = "v2-external-reads"
)

// errSourceTimeout is a source's result when it has none yet and its first
// read has outlived its budget.
var errSourceTimeout = errors.New("external reads: source read exceeded its deadline")

// sourceKind names what a source reads.
type sourceKind int

const (
	sourceDemand      sourceKind = iota // sourcePayload.Leg
	sourceClosedNamed                   // sourcePayload.ClosedNamed
	sourceScaleCheck                    // sourcePayload.ScaleCheck
)

// sourceKey names one source: its kind and the store behind the policy front
// door (demandLabelKey), or for the scale_check source the config it runs, so
// a run of an older config is never published.
type sourceKey struct {
	kind  sourceKind
	store beads.Store
	cfg   *config.City
}

func keyOf(kind sourceKind, store beads.Store) sourceKey {
	if store != nil {
		store = demandLabelKey(store)
	}
	return sourceKey{kind: kind, store: store}
}

// sourcePayload is what a source read: only its kind's field is set.
type sourcePayload struct {
	Leg         legRecording
	ClosedNamed session.ClosedNamedSessionBeadIndex
	ScaleCheck  *scaleCheckResult
}

// sourceResult is one source's latest ended read: when it started (C5.4(3))
// and ended, its error, and what it read, which a failed read keeps when the
// reader returned rows with its error. InFlightSince, set only on a published
// copy, is when the source's next read started, still running at publish.
type sourceResult struct {
	StartedAt     time.Time
	EndedAt       time.Time
	InFlightSince time.Time
	Err           error
	sourcePayload
}

// externalReadsRecording is one pass's source results. Immutable once
// published: readers copy before editing a row.
type externalReadsRecording struct {
	Seq      uint64
	FreshFor time.Duration
	Sources  map[sourceKey]sourceResult
}

// legRecording is one leg's legacy live reads, each with its own error so a
// partial read keeps the rows legacy would keep.
type legRecording struct {
	RawOpen     []beads.Bead
	RawOpenErr  error
	ReadyAll    []beads.Bead
	ReadyAllErr error
}

func (r *externalReadsRecording) source(key sourceKey) (sourceResult, bool) {
	if r == nil {
		return sourceResult{}, false
	}
	s, ok := r.Sources[key]
	return s, ok
}

// fresh reports whether s may be served at now.
func (r *externalReadsRecording) fresh(s sourceResult, now time.Time) bool {
	return r != nil && !now.After(s.EndedAt.Add(r.FreshFor))
}

// lookup returns the result of store's kind source, or
// errDemandRecordingMissing when the recording holds none and
// errDemandRecordingStale when it is no longer fresh at now.
func (r *externalReadsRecording) lookup(kind sourceKind, store beads.Store, now time.Time) (sourceResult, error) {
	s, ok := r.source(keyOf(kind, store))
	switch {
	case !ok:
		return sourceResult{}, errDemandRecordingMissing
	case !r.fresh(s, now):
		return sourceResult{}, errDemandRecordingStale
	}
	return s, nil
}

// scaleCheck returns the scale_check result, or nil when there is none, the
// run failed, or it is stale at now, so every custom template reads partial
// (scaleCheckResult.partial).
func (r *externalReadsRecording) scaleCheck(now time.Time) *scaleCheckResult {
	if r == nil {
		return nil
	}
	for key, s := range r.Sources {
		if key.kind == sourceScaleCheck && s.Err == nil && r.fresh(s, now) {
			return s.ScaleCheck
		}
	}
	return nil
}

// failing reports whether a kind source r publishes holds a failed read.
func (r *externalReadsRecording) failing(kind sourceKind) bool {
	if r == nil {
		return false
	}
	for key, s := range r.Sources {
		if key.kind == kind && (s.Err != nil || s.Leg.RawOpenErr != nil || s.Leg.ReadyAllErr != nil) {
			return true
		}
	}
	return false
}

// sameContent reports whether r and o hold the same sources with the same
// payloads and errors. Seq and the read times are not content.
func (r *externalReadsRecording) sameContent(o *externalReadsRecording) bool {
	if r == nil || o == nil {
		return r == o
	}
	if len(r.Sources) != len(o.Sources) {
		return false
	}
	for key, a := range r.Sources {
		b, ok := o.Sources[key]
		if !ok || errorText(a.Err) != errorText(b.Err) || !reflect.DeepEqual(a.sourcePayload, b.sourcePayload) {
			return false
		}
	}
	return true
}

// wakes reports whether publishing r after prev at now must wake the
// allocator: new content, or a source stale in prev that r serves fresh.
func (r *externalReadsRecording) wakes(prev *externalReadsRecording, now time.Time) bool {
	if !prev.sameContent(r) {
		return true
	}
	for key, s := range r.Sources {
		if !prev.fresh(prev.Sources[key], now) && r.fresh(s, now) {
			return true
		}
	}
	return false
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// externalReadsEnv is what one pass reads: the stores and config of the
// environment it runs against. It is a snapshot: a reload replaces Cfg rather
// than editing it, and the scale_check source is keyed by that pointer.
type externalReadsEnv struct {
	CityPath          string
	CityName          string
	Cfg               *config.City
	CityStore         beads.Store
	RigStores         map[string]beads.Store
	SuspendedRigPaths map[string]bool
	// ProbeStores are the default scale_check target stores. They resolve
	// outside the census plan (defaultScaleCheckTargetForAgent takes a rig
	// store by name, ownScaleCheckTarget the control binding), so the lane
	// records them too rather than assume every target is a census leg.
	ProbeStores []beads.Store
	// Sessions is the open session census the stamp and the assigned-work
	// canonicalization read; nil leaves both inert, as in legacy.
	Sessions *sessionBeadSnapshot
}

// laneStep is work a pass starts after it publishes, at most once per every
// from the end of its last run, and not while the legs of kind legs it reads
// just failed.
type laneStep struct {
	name  string
	every time.Duration
	legs  sourceKind
	run   func(ctx context.Context, env externalReadsEnv)
	// running and last (when its last run ended) are guarded by the lane's
	// mu.
	running bool
	last    time.Time
}

// externalReadsLane owns the latest recording.
type externalReadsLane struct {
	// interval may change only while the lane is stopped (a patrol change):
	// a restart keeps the recording, the reads in flight and the steps'
	// clocks.
	interval time.Duration
	env      func() (externalReadsEnv, error)
	onChange func()
	safeTick func(fn func(), trigger string) (panicked bool)
	events   events.Recorder
	stderr   io.Writer
	now      func() time.Time

	sourceDeadline   time.Duration
	scaleCheckBudget time.Duration
	runner           ScaleCheckRunner
	queryEnv         probeEnvFunc
	wakeCh           chan struct{}
	rec              atomic.Pointer[externalReadsRecording]
	// wg counts the source and step goroutines (join).
	wg sync.WaitGroup

	mu       sync.Mutex
	results  map[sourceKey]sourceResult // each source's latest ended read
	inFlight map[sourceKey]*sourceRead
	late     bool // a late read ended since the last collect
	steps    []*laneStep

	// seq belongs to the lane goroutine.
	seq uint64
}

// sourceRead is one read in flight.
type sourceRead struct {
	started time.Time
	// late: a pass has published without it, so its end wakes the lane.
	late bool
}

// newExternalReadsLane returns a lane paced at interval (the patrol) that
// reads env each pass, calls onChange (the allocator's wake) when a pass
// publishes new content or turns a stale source fresh, and records the
// repairs' scope gaps to rec. A nil onChange wakes nothing.
func newExternalReadsLane(interval time.Duration, env func() (externalReadsEnv, error), onChange func(), safeTick func(fn func(), trigger string) bool, rec events.Recorder, stderr io.Writer) *externalReadsLane {
	if onChange == nil {
		onChange = func() {}
	}
	l := &externalReadsLane{
		interval: interval, env: env, onChange: onChange, safeTick: safeTick, events: rec, stderr: stderr, now: time.Now,
		sourceDeadline: externalReadsSourceDeadline, scaleCheckBudget: parseBDProbeTimeout(stderr),
		runner: shellScaleCheck, queryEnv: controllerQueryRuntimeEnv, wakeCh: make(chan struct{}, 1),
		results: make(map[sourceKey]sourceResult), inFlight: make(map[sourceKey]*sourceRead),
	}
	l.steps = []*laneStep{{name: "demand-repairs", every: externalReadsRepairInterval, legs: sourceDemand, run: l.demandRepairs}}
	return l
}

// wake asks for a pass. Non-blocking; wakes that wait out the duty cycle
// join the one pass that follows.
func (l *externalReadsLane) wake() {
	select {
	case l.wakeCh <- struct{}{}:
	default:
	}
}

// recording returns the latest published recording, or nil before the first.
func (l *externalReadsLane) recording() *externalReadsRecording { return l.rec.Load() }

// start runs the lane until ctx ends and returns a channel closed when it
// has. The first pass runs at once: until a recording exists every lane-fed
// leg reads partial. A pass that declines (held, no env, shutdown) does not
// pace the next wake. start may run again once the
// channel closed.
func (l *externalReadsLane) start(ctx context.Context) <-chan struct{} {
	l.wake()
	return startGatedPacedLane(ctx, l.interval, externalReadsMinGap, l.wakeCh, func(bool) bool {
		ran := false
		panicked := l.safeTick(func() { ran = l.pass(ctx) }, externalReadsSafeTickTrigger)
		return ran || panicked
	})
}

// join waits until every read and step the lane started has ended, or ctx
// ends. Call it once the lane stopped, before closing the stores it reads.
func (l *externalReadsLane) join(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		l.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// pass publishes the reads that ended after the last publish, reads the
// sources, publishes, then starts the steps that are due unless the city is
// suspended (POOL-001; like legacy, an unreadable suspension file reads as
// the zero state). It reports whether it ran.
func (l *externalReadsLane) pass(ctx context.Context) bool {
	env, ok := l.passEnv(ctx)
	if !ok {
		return false
	}
	sources := l.sources(env)
	if l.lateEnded() {
		l.publish(l.collect(sources))
	}
	published := l.read(ctx, sources)
	if ctx.Err() != nil {
		return false
	}
	rec := l.publish(published)
	if !effectiveCitySuspended(env.Cfg, loadSuspensionStateBestEffort(env.CityPath)) {
		l.startSteps(ctx, env, rec)
	}
	return true
}

// publish publishes sources as the next recording and wakes the allocator on
// new content or on a stale source turned fresh, judged now.
func (l *externalReadsLane) publish(sources map[sourceKey]sourceResult) *externalReadsRecording {
	prev := l.rec.Load()
	l.seq++
	next := &externalReadsRecording{Seq: l.seq, FreshFor: externalReadsFreshPatrols * l.interval, Sources: sources}
	l.rec.Store(next)
	if next.wakes(prev, l.now()) {
		l.onChange()
	}
	return next
}

// lateEnded reports whether a late read ended since the last collect.
func (l *externalReadsLane) lateEnded() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.late
}

// passEnv returns the environment a pass runs against, and false when the
// pass must not run: shutdown has begun, or the env cannot be built.
func (l *externalReadsLane) passEnv(ctx context.Context) (externalReadsEnv, bool) {
	if ctx.Err() != nil {
		return externalReadsEnv{}, false
	}
	env, err := l.env()
	if err != nil {
		fmt.Fprintf(l.stderr, "external reads: %v\n", err) //nolint:errcheck
		return externalReadsEnv{}, false
	}
	return env, true
}

// externalSource is one source a pass reads. budget is how long its first
// read may run before the source publishes errSourceTimeout.
type externalSource struct {
	key    sourceKey
	budget time.Duration
	read   func() (sourcePayload, error)
}

// read starts every source not already in flight, each in its own goroutine,
// waits until they have all ended or the source deadline passed, and returns
// what the pass publishes (collect). A source an earlier pass started whose
// read ended meanwhile starts again before the publish, rather than a duty
// cycle later. It returns nil once shutdown begins.
func (l *externalReadsLane) read(ctx context.Context, sources []externalSource) map[sourceKey]sourceResult {
	ended := make(chan struct{}, len(sources))
	started := make(map[sourceKey]bool, len(sources))
	deadline := time.NewTimer(l.sourceDeadline)
	defer deadline.Stop()
wait:
	for pending := l.launch(sources, started, false, ended); pending > 0; pending-- {
		select {
		case <-ended:
		case <-deadline.C:
			break wait
		case <-ctx.Done():
			return nil
		}
	}
	l.launch(sources, started, true, ended)
	return l.collect(sources)
}

// launch starts a read of each source neither in flight nor in started, adds
// it to started, and returns how many it started. late marks reads the pass
// does not wait for.
func (l *externalReadsLane) launch(sources []externalSource, started map[sourceKey]bool, late bool, ended chan<- struct{}) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, s := range sources {
		if l.inFlight[s.key] != nil || started[s.key] {
			continue
		}
		r := &sourceRead{started: l.now(), late: late}
		l.inFlight[s.key] = r
		started[s.key] = true
		n++
		l.wg.Add(1)
		go l.readSource(s, r, ended)
	}
	return n
}

// readSource runs one read and stores its result, waking the lane when the
// pass that started it has already published.
func (l *externalReadsLane) readSource(s externalSource, r *sourceRead, ended chan<- struct{}) {
	defer l.wg.Done()
	p, err := guardedRead(s.read)
	res := sourceResult{StartedAt: r.started, EndedAt: l.now(), Err: err, sourcePayload: p}
	l.mu.Lock()
	l.results[s.key] = res
	delete(l.inFlight, s.key)
	late := r.late
	l.late = l.late || late
	l.mu.Unlock()
	ended <- struct{}{}
	if late {
		l.wake()
	}
}

// collect returns what a pass publishes for sources: each one's latest ended
// result, with InFlightSince when a newer read is still running, or
// errSourceTimeout for a source with no result whose read has outlived its
// budget. It marks the reads still in flight late and drops the results of
// sources no longer read.
func (l *externalReadsLane) collect(sources []externalSource) map[sourceKey]sourceResult {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.late = false
	out := make(map[sourceKey]sourceResult, len(sources))
	for _, s := range sources {
		res, ok := l.results[s.key]
		if r := l.inFlight[s.key]; r != nil {
			r.late = true
			switch {
			case ok:
				res.InFlightSince = r.started
			case now.Sub(r.started) >= s.budget:
				res, ok = sourceResult{StartedAt: r.started, EndedAt: now, Err: errSourceTimeout}, true
			}
		}
		if ok {
			out[s.key] = res
		}
	}
	for key := range l.results {
		if _, keep := out[key]; !keep {
			delete(l.results, key)
		}
	}
	return out
}

// sources lists what a pass reads: each lane-fed demand leg (RawOpen and
// ReadyAll), the city store's closed named-session index when an on_demand named session can consult it, and
// the custom scale_checks.
func (l *externalReadsLane) sources(env externalReadsEnv) []externalSource {
	var out []externalSource
	add := func(key sourceKey, read func() (sourcePayload, error)) {
		out = append(out, externalSource{key: key, budget: l.sourceDeadline, read: read})
	}
	demandLegs := externalReadLegs(env, l.stderr)
	reads := legacyDemandReads{}
	for _, leg := range demandLegs {
		add(keyOf(sourceDemand, leg.store), func() (sourcePayload, error) {
			var r legRecording
			r.RawOpen, r.RawOpenErr = guardedRead(func() ([]beads.Bead, error) { return reads.RawOpen(leg.store) })
			r.ReadyAll, r.ReadyAllErr = guardedRead(func() ([]beads.Bead, error) { return reads.ReadyAll(leg.store) })
			return sourcePayload{Leg: r}, nil
		})
	}
	if env.CityStore != nil && env.Cfg != nil && slices.ContainsFunc(env.Cfg.NamedSessions, func(n config.NamedSession) bool { return n.Mode == "on_demand" }) {
		add(keyOf(sourceClosedNamed, env.CityStore), func() (sourcePayload, error) {
			idx, err := reads.ClosedNamedIndex(env.CityStore)
			return sourcePayload{ClosedNamed: idx}, err
		})
	}
	if env.Cfg != nil {
		out = append(out, externalSource{key: sourceKey{kind: sourceScaleCheck, cfg: env.Cfg}, budget: l.scaleCheckBudget, read: func() (sourcePayload, error) {
			return sourcePayload{ScaleCheck: runCustomScaleChecks(env, l.runner, l.queryEnv, l.stderr)}, nil
		}})
	}
	return out
}

// guardedRead runs one read and turns a panic into the read's error, so one
// bad source is recorded as failed instead of killing the process from its
// goroutine (mc-zndi7.40).
func guardedRead[T any](read func() (T, error)) (v T, err error) {
	defer func() {
		if p := recover(); p != nil {
			var zero T
			v, err = zero, fmt.Errorf("external reads: read panicked: %v", p)
		}
	}()
	return read()
}

// externalReadLegs returns the lane-fed demand legs a pass reads, each once:
// the work census and the default-probe target stores. The routed-work legs
// need no set of their own: Plan(RoutedWork) is the census's work federation
// narrowed to the bindings, so every routed-work leg is a census leg. The
// session census reads the cache on every leg, so it has no lane-fed set. A
// leg set the topology refuses is skipped: the collectors report it partial
// themselves.
func externalReadLegs(env externalReadsEnv, stderr io.Writer) (demand []classStoreCandidate) {
	seen := make(map[beads.Store]bool)
	add := func(set string, legs []classStoreCandidate, err error) {
		if err != nil {
			fmt.Fprintf(stderr, "external reads: %s legs: %v\n", set, err) //nolint:errcheck
			return
		}
		for _, leg := range legs {
			key := demandLabelKey(leg.store)
			if leg.store == nil || seen[key] {
				continue
			}
			seen[key] = true
			if _, cacheFed := demandLegCache(leg.store); !cacheFed {
				demand = append(demand, leg)
			}
		}
	}
	legs, err := censusStoreCandidates(env.CityPath, env.Cfg, env.CityStore, env.RigStores, env.SuspendedRigPaths, censusRefBare)
	add("census", legs, err)
	probes := make([]classStoreCandidate, 0, len(env.ProbeStores))
	for _, store := range env.ProbeStores {
		probes = append(probes, classStoreCandidate{store: store})
	}
	add("default probe", probes, nil)
	return demand
}

// startSteps starts each due step on its own goroutine: one not already
// running, whose interval since its last run ended is up, and whose legs read
// fine in rec.
func (l *externalReadsLane) startSteps(ctx context.Context, env externalReadsEnv, rec *externalReadsRecording) {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, step := range l.steps {
		if step.running || (!step.last.IsZero() && now.Sub(step.last) < step.every) || rec.failing(step.legs) {
			continue
		}
		step.running = true
		l.wg.Add(1)
		go l.runStep(ctx, env, step)
	}
}

// runStep runs step once under the step deadline. A panic is reported and
// counts as a run, so a broken step is not retried every pass.
func (l *externalReadsLane) runStep(ctx context.Context, env externalReadsEnv, step *laneStep) {
	defer l.wg.Done()
	stepCtx, cancel := context.WithTimeout(ctx, externalReadsStepDeadline)
	defer cancel()
	if _, err := guardedRead(func() (struct{}, error) { step.run(stepCtx, env); return struct{}{}, nil }); err != nil {
		fmt.Fprintf(l.stderr, "external reads: step %s: %v\n", step.name, err) //nolint:errcheck
	}
	l.mu.Lock()
	step.running, step.last = false, l.now()
	l.mu.Unlock()
}

// demandRepairs is the demand repairs step: legacy's repair sequence, then
// one control.dispatcher_scope_gap event per scope gap the run found.
func (l *externalReadsLane) demandRepairs(ctx context.Context, env externalReadsEnv) {
	gaps := runBackstopDemandRepairs(ctx, env, l.stderr)
	emitControlDispatcherScopeGapEvents(l.events, env.CityName, gaps, l.now())
}

// runBackstopDemandRepairs is legacy's demand-pass repair sequence
// (buildDesiredStateWithSessionBeadsAt), collections included, in legacy
// order: each collection is read live right before the repairs over it, so
// the unassigned collection sees the assigned repairs' writes, and the work
// dir repair runs before the stamp that must get the last word. It stops
// between the halves once ctx ends (shutdown or the step deadline), and
// returns the control dispatcher scope gaps it found.
func runBackstopDemandRepairs(ctx context.Context, env externalReadsEnv, stderr io.Writer) []ControlDispatcherScopeGap {
	cfg := env.Cfg
	assigned, assignedStores, _, _, _ := collectAssignedWorkBeadsWithStores(env.CityPath, cfg, env.CityStore, env.RigStores, env.SuspendedRigPaths, env.Sessions, newReadyDemandCache())
	repairPoolSlotWorkDirClobber(cfg, assigned, assignedStores, stderr)
	stampRunSessionIdentity(cfg, assigned, assignedStores, env.Sessions, stderr)
	canonicalizeLegacyBoundAssignedWork(cfg, assigned, assignedStores, env.Sessions, stderr)
	if ctx.Err() != nil {
		return nil
	}
	routed, routedStores, routedRefs, _ := collectOpenUnassignedRoutedWork(env.CityPath, cfg, env.CityStore, env.RigStores, env.SuspendedRigPaths, stderr, nil, nil)
	repairPoolSlotWorkDirClobber(cfg, routed, routedStores, stderr)
	canonicalizeLegacyBoundUnassignedRoutedWork(cfg, routed, routedStores, stderr)
	collapseSlotSuffixedRoutedWork(cfg, routed, routedStores, stderr)
	return repairControlDispatcherRoutesForStoreScope(env.CityPath, cfg, routed, routedStores, routedRefs, stderr)
}
