package main

import (
	"context"
	"fmt"
	"io"
	"reflect"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

// The demand backstop lane (design §4a's periodic per-store demand backstop,
// CONTRACT C0.4 as amended by AM6). The v2 allocator's pass does no backing
// I/O, but a leg whose cache is not exact (bd ledgers, native Dolt, Postgres)
// cannot answer demand from its cache: bd folds blocked work into "open"
// (EB-42o8), and out-of-process bd writes reach the cache only at its re-scan.
// So this lane runs legacy's own live reads of those legs, off the pass, and
// publishes them as a recording that v2DemandReads and the allocator's
// session census serve from.
//
// A pass reads, concurrently, every non-exact leg in any demand leg set (the
// work census, the default-probe target stores) through legacyDemandReads,
// every non-exact session census leg through the session front door's live
// ListAll, as legacy's census reads it (collectOpenSessionInfos), and, when
// the config has an on_demand named session, the city store's closed
// named-session index on any leg, since no cache holds closed history. It
// publishes the recording, stamped when its reads ended and fresh until an
// expiry derived from the lane's cadence, and wakes the allocator when its
// content changed or it replaces an expired recording. The allocator's
// session census serves those legs through sessionLeg, and so takes the same
// expiry.
//
// Legacy's demand-pass repair writes (POOL-019/020) run on their own paced
// goroutine, at most once per backstopRepairInterval, in legacy order over
// every leg, so they never delay a recording. Those writes run only under
// v2; legacy keeps them in its tick, so the two never both run. The lane
// writes work beads through those repairs and nothing else. Every recording
// carries the scope gaps of the latest repair run with that run's sequence
// number, so a consumer emits each run's gaps once.
//
// While the city is suspended neither goroutine reads or writes, as legacy's
// demand pass returns before any of it (POOL-001); the last recording stays
// published and expires.
//
// The recording goroutine is paced at the patrol interval and woken by
// key-less socket and API pokes (a CLI writer such as gc sling pokes
// key-less), a supervisor reload, a store swap after the barrier, a resume,
// and the allocator when a recording has expired.
//
// Unwired in this slice: P3-7 starts it and wires its wakes.

const (
	// backstopLaneMinGap is the least idle time between two woken passes, so
	// a burst of CLI pokes costs one pass.
	backstopLaneMinGap = 2 * time.Second
	// backstopRepairInterval is the least idle time between two repair runs.
	backstopRepairInterval = time.Minute
	// backstopPassWindow is how many recent pass durations a recording's
	// expiry takes its maximum over. The maximum is capped at cacheLagBound,
	// so one slow pass cannot keep a dead lane's recordings fresh for its
	// own length.
	backstopPassWindow = 8
	// backstopSafeTickTrigger and backstopRepairSafeTickTrigger name the
	// lane's goroutines in safeTick panic lines.
	backstopSafeTickTrigger       = "v2-demand-backstop"
	backstopRepairSafeTickTrigger = "v2-demand-backstop-repairs"
)

// backstopRecording is one pass's live reads of the non-exact legs, keyed by
// the store behind the policy front door (demandLabelKey): demand reads in
// Legs, session census reads in Sessions, the closed named-session index in
// ClosedNamed. Immutable once published: readers copy before editing a row.
type backstopRecording struct {
	Seq uint64
	// StartedAt is when the pass's reads started (C5.4(3)), and At when
	// they ended. The recording is fresh through
	// Expires: At plus twice the lane interval plus the longest recent pass
	// (at most cacheLagBound), so a lane keeping its cadence never serves a
	// stale recording.
	StartedAt time.Time
	At        time.Time
	Expires   time.Time

	Legs        map[beads.Store]legRecording
	Sessions    map[beads.Store]sessionLegRecording
	ClosedNamed map[beads.Store]closedNamedRecording
	// ScopeGaps are the control-dispatcher scope gaps the latest repair run
	// found, for P3-7 to emit, and RepairSeq numbers that run (0 before the
	// first). Successive recordings repeat a run's gaps under the same
	// RepairSeq. Neither is content: no demand reads them.
	ScopeGaps []ControlDispatcherScopeGap
	RepairSeq uint64
}

// legRecording is one leg's legacy live reads, each with its own error so a
// partial read keeps the rows legacy would keep.
type legRecording struct {
	RawOpen     []beads.Bead
	RawOpenErr  error
	ReadyAll    []beads.Bead
	ReadyAllErr error
}

// sessionLegRecording is one session census leg's legacy live read, as
// ListAll returns it: all of a clean read, what a partial result returned,
// nothing of a hard failure.
type sessionLegRecording struct {
	Rows []session.Info
	Err  error
}

// closedNamedRecording is legacy's closed named-session index read, as
// BuildClosedNamedSessionBeadIndex returns it.
type closedNamedRecording struct {
	Index session.ClosedNamedSessionBeadIndex
	Err   error
}

// backstopRepairRun is one repair run's scope gaps and its sequence number.
type backstopRepairRun struct {
	seq  uint64
	gaps []ControlDispatcherScopeGap
}

// fresh reports whether the recording may be served at now. A nil recording
// is never fresh.
func (r *backstopRecording) fresh(now time.Time) bool {
	return r != nil && !now.After(r.Expires)
}

// sessionLeg returns store's recorded session census read, as the census
// reads it (censusLegFeed.recorded). A non-nil Err makes the leg partial: a
// beads.PartialResultError when Rows holds what a partial read returned, any
// other error when Rows is empty. ok is false for a leg the recording does
// not hold: an exact leg, or any leg before the first pass.
func (r *backstopRecording) sessionLeg(store beads.Store) (censusRecording, bool) {
	if r == nil {
		return censusRecording{}, false
	}
	l, ok := r.Sessions[demandLabelKey(store)]
	if !ok {
		return censusRecording{}, false
	}
	return censusRecording{Rows: l.Rows, StartedAt: r.StartedAt, At: r.At, Expires: r.Expires, Err: l.Err}, true
}

// leg returns store's recorded demand reads. A nil recording holds no leg.
func (r *backstopRecording) leg(store beads.Store) (legRecording, bool) {
	if r == nil {
		return legRecording{}, false
	}
	l, ok := r.Legs[demandLabelKey(store)]
	return l, ok
}

// closedNamed returns store's recorded closed named-session index.
func (r *backstopRecording) closedNamed(store beads.Store) (closedNamedRecording, bool) {
	if r == nil {
		return closedNamedRecording{}, false
	}
	l, ok := r.ClosedNamed[demandLabelKey(store)]
	return l, ok
}

// sameContent reports whether r and o recorded the same legs, rows, indexes
// and errors. Seq, StartedAt, At, Expires, ScopeGaps and RepairSeq are not
// content.
func (r *backstopRecording) sameContent(o *backstopRecording) bool {
	if r == nil || o == nil {
		return r == o
	}
	if len(r.Legs) != len(o.Legs) || len(r.Sessions) != len(o.Sessions) || len(r.ClosedNamed) != len(o.ClosedNamed) {
		return false
	}
	for store, a := range r.Legs {
		b, ok := o.Legs[store]
		if !ok || errorText(a.RawOpenErr) != errorText(b.RawOpenErr) || errorText(a.ReadyAllErr) != errorText(b.ReadyAllErr) ||
			!reflect.DeepEqual(a.RawOpen, b.RawOpen) || !reflect.DeepEqual(a.ReadyAll, b.ReadyAll) {
			return false
		}
	}
	for store, a := range r.Sessions {
		b, ok := o.Sessions[store]
		if !ok || errorText(a.Err) != errorText(b.Err) || !reflect.DeepEqual(a.Rows, b.Rows) {
			return false
		}
	}
	for store, a := range r.ClosedNamed {
		b, ok := o.ClosedNamed[store]
		if !ok || errorText(a.Err) != errorText(b.Err) || !reflect.DeepEqual(a.Index, b.Index) {
			return false
		}
	}
	return true
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// backstopEnv is what one pass reads: the stores and config of the
// environment it runs against.
type backstopEnv struct {
	CityPath          string
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

// backstopLane owns the latest recording.
type backstopLane struct {
	interval time.Duration
	env      func() (backstopEnv, error)
	onChange func()
	safeTick func(fn func(), trigger string) (panicked bool)
	stderr   io.Writer
	now      func() time.Time
	wakeCh   chan struct{}
	rec      atomic.Pointer[backstopRecording]
	repairs  atomic.Pointer[backstopRepairRun]

	// seq and passTimes belong to the recording goroutine.
	seq       uint64
	passTimes [backstopPassWindow]time.Duration
}

// newBackstopLane returns a lane paced at interval (the patrol) that reads
// env each pass and calls onChange (the allocator's wake) when a pass records
// new content or refreshes an expired recording. A nil onChange wakes nothing.
func newBackstopLane(interval time.Duration, env func() (backstopEnv, error), onChange func(), safeTick func(fn func(), trigger string) bool, stderr io.Writer) *backstopLane {
	if onChange == nil {
		onChange = func() {}
	}
	return &backstopLane{interval: interval, env: env, onChange: onChange, safeTick: safeTick, stderr: stderr, now: time.Now, wakeCh: make(chan struct{}, 1)}
}

// wake asks for a recording pass. Non-blocking; wakes that wait out the duty
// cycle join the one pass that follows.
func (l *backstopLane) wake() {
	select {
	case l.wakeCh <- struct{}{}:
	default:
	}
}

// recording returns the latest published recording, or nil before the first.
func (l *backstopLane) recording() *backstopRecording { return l.rec.Load() }

// start runs the recording and repair goroutines until ctx ends and returns
// a channel closed when both have. Each runs its first pass at once: until a
// recording exists every non-exact leg reads partial. A pass that declines
// (suspended city, no env, shutdown) does not pace the next wake.
func (l *backstopLane) start(ctx context.Context) <-chan struct{} {
	l.wake()
	recording := startGatedPacedLane(ctx, l.interval, backstopLaneMinGap, l.wakeCh, func(bool) bool {
		ran := false
		panicked := l.safeTick(func() { ran = l.pass(ctx) }, backstopSafeTickTrigger)
		return ran || panicked
	})
	// The repairs' only wake today is this first one, so a declined run
	// pacing the next wake would change nothing yet. The gated shape is for
	// P3-7: a gate release re-runs a skipped repair through a wake, which a
	// skipped run must not pace.
	repairWake := make(chan struct{}, 1)
	repairWake <- struct{}{}
	repairs := startGatedPacedLane(ctx, backstopRepairInterval, backstopRepairInterval, repairWake, func(bool) bool {
		ran := false
		panicked := l.safeTick(func() { ran = l.repair(ctx) }, backstopRepairSafeTickTrigger)
		return ran || panicked
	})
	done := make(chan struct{})
	go func() {
		<-recording
		<-repairs
		close(done)
	}()
	return done
}

// pass records the non-exact legs and publishes, waking the allocator on new
// content or on a fresh recording replacing an expired one. It reports
// whether it ran.
func (l *backstopLane) pass(ctx context.Context) bool {
	env, ok := l.passEnv(ctx)
	if !ok {
		return false
	}
	start := l.now()
	next := recordBackstop(env, l.stderr)
	end := l.now()
	if ctx.Err() != nil {
		return false
	}
	l.passTimes[l.seq%backstopPassWindow] = end.Sub(start)
	l.seq++
	next.Seq, next.StartedAt, next.At, next.Expires = l.seq, start, end, end.Add(2*l.interval+min(slices.Max(l.passTimes[:]), cacheLagBound))
	if run := l.repairs.Load(); run != nil {
		next.ScopeGaps, next.RepairSeq = run.gaps, run.seq
	}
	if prev := l.rec.Swap(next); !prev.sameContent(next) || !prev.fresh(end) {
		l.onChange()
	}
	return true
}

// repair runs legacy's demand-pass repairs once and keeps the scope gaps
// they found, numbered by the run, for the next recordings. It reports
// whether it ran.
func (l *backstopLane) repair(ctx context.Context) bool {
	env, ok := l.passEnv(ctx)
	if !ok || env.CityStore == nil {
		return false
	}
	run := &backstopRepairRun{seq: 1, gaps: runBackstopDemandRepairs(ctx, env, l.stderr)}
	if prev := l.repairs.Load(); prev != nil {
		run.seq = prev.seq + 1
	}
	l.repairs.Store(run)
	return true
}

// passEnv returns the environment a pass runs against, and false when the
// pass must not run: shutdown has begun, the env cannot be built, or the city
// is suspended (legacy's demand pass returns before any read or repair,
// POOL-001). Like legacy, an unreadable suspension file reads as the zero
// state.
func (l *backstopLane) passEnv(ctx context.Context) (backstopEnv, bool) {
	if ctx.Err() != nil {
		return backstopEnv{}, false
	}
	env, err := l.env()
	if err != nil {
		fmt.Fprintf(l.stderr, "demand backstop: %v\n", err) //nolint:errcheck
		return backstopEnv{}, false
	}
	if effectiveCitySuspended(env.Cfg, loadSuspensionStateBestEffort(env.CityPath)) {
		return backstopEnv{}, false
	}
	return env, true
}

// recordBackstop reads every non-exact leg concurrently: the demand legs
// through legacyDemandReads, the session census legs through the session
// front door's live ListAll, and the city store's closed named-session index
// when an on_demand named session can consult it. Seq, StartedAt, At and
// Expires are the caller's.
func recordBackstop(env backstopEnv, stderr io.Writer) *backstopRecording {
	rec := &backstopRecording{
		Legs:        make(map[beads.Store]legRecording),
		Sessions:    make(map[beads.Store]sessionLegRecording),
		ClosedNamed: make(map[beads.Store]closedNamedRecording),
	}
	demandLegs, sessionLegs := backstopLegs(env, stderr)
	var mu sync.Mutex
	var wg sync.WaitGroup
	reads := legacyDemandReads{}
	for _, leg := range demandLegs {
		wg.Go(func() {
			var l legRecording
			l.RawOpen, l.RawOpenErr = guardedLegRead(func() ([]beads.Bead, error) { return reads.RawOpen(leg.store) })
			l.ReadyAll, l.ReadyAllErr = guardedLegRead(func() ([]beads.Bead, error) { return reads.ReadyAll(leg.store) })
			mu.Lock()
			defer mu.Unlock()
			rec.Legs[demandLabelKey(leg.store)] = l
		})
	}
	for _, leg := range sessionLegs {
		wg.Go(func() {
			rows, err := guardedLegRead(func() ([]session.Info, error) {
				return sessionFrontDoor(leg.store).ListAll(session.ListAllOptions{Live: true})
			})
			mu.Lock()
			defer mu.Unlock()
			rec.Sessions[demandLabelKey(leg.store)] = sessionLegRecording{Rows: rows, Err: err}
		})
	}
	if env.CityStore != nil && env.Cfg != nil && slices.ContainsFunc(env.Cfg.NamedSessions, func(n config.NamedSession) bool { return n.Mode == "on_demand" }) {
		wg.Go(func() {
			idx, err := guardedLegRead(func() (session.ClosedNamedSessionBeadIndex, error) { return reads.ClosedNamedIndex(env.CityStore) })
			mu.Lock()
			defer mu.Unlock()
			rec.ClosedNamed[demandLabelKey(env.CityStore)] = closedNamedRecording{Index: idx, Err: err}
		})
	}
	wg.Wait()
	return rec
}

// guardedLegRead runs one leg read and turns a panic into the read's error,
// so one bad leg is recorded as failed instead of killing the process from
// its goroutine (mc-zndi7.40).
func guardedLegRead[T any](read func() (T, error)) (v T, err error) {
	defer func() {
		if p := recover(); p != nil {
			var zero T
			v, err = zero, fmt.Errorf("demand backstop: leg read panicked: %v", p)
		}
	}()
	return read()
}

// backstopLegs returns the non-exact legs a pass reads, each once per set:
// the demand legs (the work census and the default-probe target stores) and
// the session census legs. The routed-work legs need no set of their own:
// Plan(RoutedWork) is the census's work federation narrowed to the bindings,
// so every routed-work leg is a census leg. A leg set the topology refuses is
// skipped: the collectors and the census report it partial themselves.
func backstopLegs(env backstopEnv, stderr io.Writer) (demand, sessions []classStoreCandidate) {
	demandSeen, sessionSeen := make(map[beads.Store]bool), make(map[beads.Store]bool)
	add := func(into *[]classStoreCandidate, seen map[beads.Store]bool, set string, legs []classStoreCandidate, err error) {
		if err != nil {
			fmt.Fprintf(stderr, "demand backstop: %s legs: %v\n", set, err) //nolint:errcheck
			return
		}
		for _, leg := range legs {
			key := demandLabelKey(leg.store)
			if leg.store == nil || seen[key] {
				continue
			}
			seen[key] = true
			if _, exact := demandLegCache(leg.store); !exact {
				*into = append(*into, leg)
			}
		}
	}
	legs, err := censusStoreCandidates(env.CityPath, env.Cfg, env.CityStore, env.RigStores, env.SuspendedRigPaths, censusRefBare)
	add(&demand, demandSeen, "census", legs, err)
	probes := make([]classStoreCandidate, 0, len(env.ProbeStores))
	for _, store := range env.ProbeStores {
		probes = append(probes, classStoreCandidate{store: store})
	}
	add(&demand, demandSeen, "default probe", probes, nil)
	legs, err = sessionCensusStoreCandidates(env.CityPath, env.Cfg, env.CityStore, env.RigStores, env.SuspendedRigPaths)
	add(&sessions, sessionSeen, "session census", legs, err)
	return demand, sessions
}

// runBackstopDemandRepairs is legacy's demand-pass repair sequence
// (buildDesiredStateWithSessionBeadsAt), collections included, in legacy
// order: each collection is read live right before the repairs over it, so
// the unassigned collection sees the assigned repairs' writes, and the work
// dir repair runs before the stamp that must get the last word. It stops
// between the halves once shutdown has begun, and returns the control
// dispatcher scope gaps it found.
func runBackstopDemandRepairs(ctx context.Context, env backstopEnv, stderr io.Writer) []ControlDispatcherScopeGap {
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
