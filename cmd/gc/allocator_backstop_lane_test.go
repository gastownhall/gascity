package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/session"
)

// The lane's pacing tests run in a testing/synctest bubble: the paced lane's
// timers and the test's waits share the bubble clock, and synctest.Wait
// returns once the lane goroutines are blocked again. The pass tests call
// pass directly with a pinned clock.

const backstopTestInterval = 15 * time.Second

// newTestBackstopLane returns a lane over env and a count of the allocator
// wakes it asks for.
func newTestBackstopLane(env externalReadsEnv) (*externalReadsLane, *atomic.Int64) {
	wakes := &atomic.Int64{}
	lane := newExternalReadsLane(
		backstopTestInterval,
		func() (externalReadsEnv, error) { return env, nil },
		func() { wakes.Add(1) },
		func(fn func(), _ string) bool { fn(); return false },
		nil,
		io.Discard,
	)
	return lane, wakes
}

// at pins the lane's clock to t, once the reads and steps of earlier passes
// have ended.
func at(lane *externalReadsLane, t time.Time) *externalReadsLane {
	_ = lane.join(context.Background())
	lane.now = func() time.Time { return t }
	return lane
}

// startBackstopLaneInBubble starts the lane in the current bubble and, when
// the bubble's test returns, stops it and joins its reads and steps.
func startBackstopLaneInBubble(t *testing.T, lane *externalReadsLane) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := lane.start(ctx)
	t.Cleanup(func() {
		cancel()
		<-done
		if err := lane.join(context.Background()); err != nil {
			t.Errorf("join: %v", err)
		}
	})
	synctest.Wait()
}

// passAndJoin runs one pass and waits for the reads and steps it started.
func passAndJoin(ctx context.Context, lane *externalReadsLane) bool {
	ran := lane.pass(ctx)
	_ = lane.join(context.Background())
	return ran
}

// advanceBackstop moves the bubble clock forward by d and lets the lane settle.
func advanceBackstop(d time.Duration) {
	<-time.After(d)
	synctest.Wait()
}

func backstopSeq(lane *externalReadsLane) uint64 {
	if rec := lane.recording(); rec != nil {
		return rec.Seq
	}
	return 0
}

// recordedLeg returns store's recorded demand reads.
func recordedLeg(rec *externalReadsRecording, store beads.Store) (legRecording, bool) {
	s, ok := rec.source(keyOf(sourceDemand, store))
	return s.Leg, ok
}

// recordedClosedNamed returns store's recorded closed named-session index
// source.
func recordedClosedNamed(rec *externalReadsRecording, store beads.Store) (sourceResult, bool) {
	return rec.source(keyOf(sourceClosedNamed, store))
}

// scopeGapEvents counts the control.dispatcher_scope_gap events rec holds.
func scopeGapEvents(t *testing.T, rec *events.Fake) int {
	t.Helper()
	evts, err := rec.List(events.Filter{Type: events.ControlDispatcherScopeGap})
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	return len(evts)
}

func backstopSessionBead(id, name string) beads.Bead {
	return beads.Bead{
		ID: id, Title: name, Type: sessionBeadType, Status: "open", Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{"session_name": name, "template": "worker", "state": "active"},
	}
}

func sessionInfoIDs(infos []session.Info) []string {
	var out []string
	for _, info := range infos {
		out = append(out, info.ID)
	}
	slices.Sort(out)
	return out
}

// Kills: a recording that never wakes the allocator, and one that wakes it
// on every pass. Each pass publishes (so the recording stays fresh); only a
// pass whose reads changed wakes the allocator.
func TestBackstopLanePublishesAndWakesOnChangeOnly(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := beads.NewMemStore()
		if _, err := store.Create(routedDemandBead("")); err != nil {
			t.Fatalf("seed routed work: %v", err)
		}
		lane, wakes := newTestBackstopLane(externalReadsEnv{Cfg: demandReadsTestConfig(), CityStore: store})
		start := time.Now()
		startBackstopLaneInBubble(t, lane)

		check := func(when string, seq uint64, wantWakes int64, rows int) {
			t.Helper()
			rec := lane.recording()
			leg, ok := recordedLeg(rec, store)
			if rec == nil || rec.Seq != seq || wakes.Load() != wantWakes || !ok || len(leg.RawOpen) != rows {
				t.Fatalf("%s: seq=%d wakes=%d leg recorded=%t open rows=%d, want seq=%d wakes=%d rows=%d",
					when, backstopSeq(lane), wakes.Load(), ok, len(leg.RawOpen), seq, wantWakes, rows)
			}
		}
		check("first pass", 1, 1, 1)

		advanceBackstop(backstopTestInterval)
		check("unchanged pass", 2, 1, 1)
		if src, _ := lane.recording().source(keyOf(sourceDemand, store)); !src.EndedAt.Equal(start.Add(backstopTestInterval)) {
			t.Errorf("unchanged pass recorded at %v, want %v: an unchanged recording must still be republished fresh", src.EndedAt, start.Add(backstopTestInterval))
		}

		if _, err := store.Create(routedDemandBead("")); err != nil {
			t.Fatalf("add routed work: %v", err)
		}
		advanceBackstop(backstopTestInterval)
		check("changed pass", 3, 2, 2)
	})
}

// Kills: a CLI hint ignored. A key-less poke inside the minimum gap runs one
// pass as soon as the gap closes, and a poke after it runs one at once, far
// ahead of the patrol backstop.
func TestBackstopLaneKeylessPokeTriggersPassWithinMinGap(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		lane, _ := newTestBackstopLane(externalReadsEnv{Cfg: demandReadsTestConfig(), CityStore: beads.NewMemStore()})
		startBackstopLaneInBubble(t, lane)
		if got := backstopSeq(lane); got != 1 {
			t.Fatalf("after start: %d passes, want the immediate first pass", got)
		}

		advanceBackstop(time.Second)
		lane.wake()
		synctest.Wait()
		if got := backstopSeq(lane); got != 1 {
			t.Fatalf("poke 1s after a pass: %d passes, want 1 until the %v gap closes", got, externalReadsMinGap)
		}
		advanceBackstop(externalReadsMinGap - time.Second - time.Millisecond)
		if got := backstopSeq(lane); got != 1 {
			t.Fatalf("just before the gap closes: %d passes, want 1", got)
		}
		advanceBackstop(time.Millisecond)
		if got := backstopSeq(lane); got != 2 {
			t.Fatalf("gap closed: %d passes, want the poke's pass", got)
		}

		advanceBackstop(5 * time.Second)
		lane.wake()
		synctest.Wait()
		if got := backstopSeq(lane); got != 3 {
			t.Fatalf("poke after the gap: %d passes, want a pass at once", got)
		}
	})
}

// clobberedRunFixture is the clobbered in-progress run of
// TestRepairPoolSlotWorkDirClobberThenStampPreservesLiveWorkDir: run in
// legacy order, the work-dir repair restores the legacy work dir and the
// stamp then writes the session's live one; the other way round the repair
// reverts the stamp.
type clobberedRunFixture struct {
	mem      *beads.MemStore
	sessions *sessionBeadSnapshot
}

const (
	clobberSessionName = "gascity--worker-gc-7"
	clobberLiveWorkDir = "/home/ds/gascity-worktrees/ga-live"
	clobberStaleLegacy = "/home/ds/gascity-worktrees/ga-stale"
	clobberPoolSlot    = ".gc/worktrees/gascity/builder-1"
)

// clobberedWorkDir is a fresh clobbered metadata map each time: MemStore
// keeps the seed's map, so a shared one would carry the stamp back into a
// re-clobber.
func clobberedWorkDir() map[string]string {
	return map[string]string{beadmeta.WorkDirMetadataKey: clobberPoolSlot, beadmeta.LegacyWorkDirMetadataKey: clobberStaleLegacy}
}

func newClobberedRunFixture() clobberedRunFixture {
	return clobberedRunFixture{
		mem:      beads.NewMemStoreFrom(0, []beads.Bead{{ID: "ga-run", Type: "task", Status: "in_progress", Assignee: clobberSessionName, Metadata: clobberedWorkDir()}}, nil),
		sessions: newSessionBeadSnapshot([]beads.Bead{stampTestSession(clobberSessionName, clobberLiveWorkDir)}),
	}
}

func (f clobberedRunFixture) workDir(t *testing.T) string {
	t.Helper()
	b, err := f.mem.Get("ga-run")
	if err != nil {
		t.Fatalf("Get(ga-run): %v", err)
	}
	return b.Metadata[beadmeta.WorkDirMetadataKey]
}

// writeLogStore logs every write, in order, into a log shared by several
// stores. Embedding the Store interface hides the inner store's optional
// capabilities, so every write passes through the methods below.
type writeLogStore struct {
	beads.Store
	log *writeLog
}

type writeLog struct {
	mu  sync.Mutex
	ops []string
}

func (l *writeLog) add(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ops = append(l.ops, fmt.Sprintf(format, args...))
}

func sortedKVs(kvs map[string]string) string {
	var out []string
	for k, v := range kvs {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return strings.Join(out, " ")
}

func (s writeLogStore) SetMetadata(id, key, value string) error {
	s.log.add("SetMetadata %s %s=%s", id, key, value)
	return s.Store.SetMetadata(id, key, value)
}

func (s writeLogStore) SetMetadataBatch(id string, kvs map[string]string) error {
	s.log.add("SetMetadataBatch %s %s", id, sortedKVs(kvs))
	return s.Store.SetMetadataBatch(id, kvs)
}

func (s writeLogStore) Update(id string, opts beads.UpdateOpts) error {
	assignee := "-"
	if opts.Assignee != nil {
		assignee = *opts.Assignee
	}
	s.log.add("Update %s assignee=%s %s", id, assignee, sortedKVs(opts.Metadata))
	return s.Store.Update(id, opts)
}

// repairGoldenFixture gives every demand-pass repair a row to write: the
// clobbered run (work-dir repair, then the stamp), a workflow step whose
// non-pool session stamps its root (gc.root_bead_id) while the root, open
// and routed, carries a clobbered pool-slot work dir, work assigned to a
// legacy bound identity, an unassigned clobbered row, an unassigned route to
// a legacy bound identity, a slot-suffixed route, and a rig-owned control
// row routed to the city dispatcher. A second rig with no dispatcher is a
// scope gap: no write, one gap. Every write goes to log.
type repairGoldenFixture struct {
	env      externalReadsEnv
	log      *writeLog
	rigRoute string
}

const (
	goldenLegacyPlanner    = "rig-A/gc.planner"
	goldenCanonicalPlanner = "rig-A/planner"
	// goldenStepSession is the non-pool session the workflow step is
	// assigned to; goldenStepWorkDir is its work dir.
	goldenStepSession = "mayor-session"
	goldenStepWorkDir = "/home/ds/gascity-worktrees/ga-mayor"
)

func newRepairGoldenFixture(t *testing.T) repairGoldenFixture {
	t.Helper()
	cfg := classBindingDispatcherFixtureConfig(t)
	cfg.Workspace.Prefix = "ga"
	cfg.Rigs = append(cfg.Rigs, config.Rig{Name: "nodisp", Path: t.TempDir()})
	cfg.Agents = append(cfg.Agents, poolAgent("planner", "rig-A", intPtr(5), 0))
	cityRoute := cfg.Agents[0].QualifiedName()

	routedClobbered := workBead("ga-rclob", goldenCanonicalPlanner, "", "open", 5)
	routedClobbered.Metadata[beadmeta.WorkDirMetadataKey] = clobberPoolSlot
	routedClobbered.Metadata[beadmeta.LegacyWorkDirMetadataKey] = clobberStaleLegacy
	// The root's legacy work dir is not the step session's, so a routed-side
	// work-dir repair over a snapshot taken before the stamp would revert the
	// stamp to it (M19).
	root := workBead("ga-root", goldenCanonicalPlanner, "", "open", 5)
	root.Metadata[beadmeta.KindMetadataKey] = beadmeta.KindWorkflow
	root.Metadata[beadmeta.WorkDirMetadataKey] = clobberPoolSlot
	root.Metadata[beadmeta.LegacyWorkDirMetadataKey] = clobberStaleLegacy
	control := func(id, rig string) beads.Bead {
		return beads.Bead{ID: id, Title: id, Type: "task", Status: "open", Metadata: map[string]string{
			beadmeta.KindMetadataKey:         beadmeta.KindWorkflowFinalize,
			beadmeta.RoutedToMetadataKey:     cityRoute,
			beadmeta.RootStoreRefMetadataKey: "rig:" + rig,
		}}
	}
	log := &writeLog{}
	city := writeLogStore{Store: beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "ga-run", Type: "task", Status: "in_progress", Assignee: clobberSessionName, Metadata: clobberedWorkDir()},
		{ID: "ga-step", Type: "task", Status: "in_progress", Assignee: goldenStepSession, Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: "ga-root"}},
		root,
		workBead("ga-asg", goldenLegacyPlanner, goldenLegacyPlanner, "in_progress", 5),
		routedClobbered,
		workBead("ga-rlegacy", goldenLegacyPlanner, "", "open", 5),
		workBead("ga-slot", goldenCanonicalPlanner+"-2", "", "open", 5),
	}, nil), log: log}
	rigs := map[string]beads.Store{
		"fixture": writeLogStore{Store: beads.NewMemStoreFrom(0, []beads.Bead{control("fx-ctl", "fixture")}, nil), log: log},
		"nodisp":  writeLogStore{Store: beads.NewMemStoreFrom(0, []beads.Bead{control("nd-ctl", "nodisp")}, nil), log: log},
	}
	return repairGoldenFixture{
		env: externalReadsEnv{
			CityPath: t.TempDir(), Cfg: cfg, CityStore: city, RigStores: rigs,
			Sessions: newSessionBeadSnapshot([]beads.Bead{
				stampTestSession(clobberSessionName, clobberLiveWorkDir),
				stampTestSession(goldenStepSession, goldenStepWorkDir),
			}),
		},
		log:      log,
		rigRoute: cfg.Agents[1].QualifiedName(),
	}
}

func (f repairGoldenFixture) workDir(t *testing.T, id string) string {
	t.Helper()
	b, err := f.env.CityStore.Get(id)
	if err != nil {
		t.Fatalf("Get(%s): %v", id, err)
	}
	return b.Metadata[beadmeta.WorkDirMetadataKey]
}

// Kills: any repair dropped from the sequence or run out of legacy order
// (M15-M20), including the routed collection read before the assigned
// repairs (M19): its stale snapshot of the root would revert the root stamp
// to the legacy work dir. The step emits the run's scope gap itself.
func TestBackstopDemandRepairsWriteLogGoldenInLegacyOrder(t *testing.T) {
	f := newRepairGoldenFixture(t)
	lane, _ := newTestBackstopLane(f.env)
	rec := events.NewFake()
	lane.events = rec

	if !passAndJoin(context.Background(), at(lane, time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC))) {
		t.Fatal("pass declined")
	}

	want := []string{
		"SetMetadataBatch ga-run " + beadmeta.WorkDirMetadataKey + "=" + clobberStaleLegacy,
		"SetMetadataBatch ga-run gc.session_name=" + clobberSessionName + " " + beadmeta.WorkDirMetadataKey + "=" + clobberLiveWorkDir,
		"SetMetadataBatch ga-step gc.session_name=" + goldenStepSession + " " + beadmeta.WorkDirMetadataKey + "=" + goldenStepWorkDir,
		"SetMetadataBatch ga-root gc.session_name=" + goldenStepSession + " " + beadmeta.WorkDirMetadataKey + "=" + goldenStepWorkDir,
		"Update ga-asg assignee=" + goldenCanonicalPlanner + " " + beadmeta.RoutedToMetadataKey + "=" + goldenCanonicalPlanner,
		"SetMetadataBatch ga-rclob " + beadmeta.WorkDirMetadataKey + "=" + clobberStaleLegacy,
		"Update ga-rlegacy assignee=- " + beadmeta.RoutedToMetadataKey + "=" + goldenCanonicalPlanner,
		"Update ga-slot assignee=- " + beadmeta.RoutedToMetadataKey + "=" + goldenCanonicalPlanner,
		"Update fx-ctl assignee=- " + beadmeta.RoutedToMetadataKey + "=" + f.rigRoute,
	}
	if !slices.Equal(f.log.ops, want) {
		t.Errorf("repair writes\n got  %q\n want %q", f.log.ops, want)
	}
	if got := f.workDir(t, "ga-root"); got != goldenStepWorkDir {
		t.Errorf("root gc.work_dir = %q, want the step session's %q", got, goldenStepWorkDir)
	}

	gaps, err := rec.List(events.Filter{Type: events.ControlDispatcherScopeGap})
	if err != nil || len(gaps) != 1 || !strings.Contains(gaps[0].Subject, "nodisp") || !strings.Contains(gaps[0].Message, "nd-ctl") {
		t.Errorf("scope gap events = %+v (err %v), want one for rig nodisp with nd-ctl", gaps, err)
	}
}

// Kills: repairs on every pass, repairs paced by anything but their own
// minute, the stamp running before the work-dir repair (POOL-019), and scope
// gaps not emitted, or emitted by passes that ran no repairs. The repairs run
// on the first pass; re-clobbered, the run stays clobbered through the passes
// of the next minute and is repaired once the minute is up, each run emitting
// its gap once.
func TestExternalReadsInLaneRepairsAtMostOncePerMinuteEmitGaps(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newRepairGoldenFixture(t)
		lane, _ := newTestBackstopLane(f.env)
		rec := events.NewFake()
		lane.events = rec
		startBackstopLaneInBubble(t, lane)
		if got, gaps := f.workDir(t, "ga-run"), scopeGapEvents(t, rec); got != clobberLiveWorkDir || gaps != 1 {
			t.Fatalf("after start: gc.work_dir = %q, gap events %d; want %q and 1: the repairs must run, the stamp after the work-dir repair", got, gaps, clobberLiveWorkDir)
		}
		if err := f.env.CityStore.SetMetadataBatch("ga-run", clobberedWorkDir()); err != nil {
			t.Fatalf("re-clobber: %v", err)
		}
		for _, step := range []time.Duration{backstopTestInterval, backstopTestInterval, externalReadsRepairInterval - 2*backstopTestInterval - time.Millisecond} {
			advanceBackstop(step)
			if got, gaps := f.workDir(t, "ga-run"), scopeGapEvents(t, rec); got != clobberPoolSlot || gaps != 1 {
				t.Fatalf("seq %d: gc.work_dir = %q, gap events %d; want %q and 1: repairs ran again within a minute", backstopSeq(lane), got, gaps, clobberPoolSlot)
			}
		}
		if got := backstopSeq(lane); got < 3 {
			t.Fatalf("passes within the minute = %d, want the patrol's", got)
		}
		advanceBackstop(time.Millisecond)
		if got, gaps := f.workDir(t, "ga-run"), scopeGapEvents(t, rec); got != clobberLiveWorkDir || gaps != 2 {
			t.Fatalf("a minute later: gc.work_dir = %q, gap events %d; want %q and 2", got, gaps, clobberLiveWorkDir)
		}
	})
}

// writeSuspension writes the city's runtime suspension file; an empty body
// removes it.
func writeSuspension(t *testing.T, cityPath, body string) {
	t.Helper()
	path := citylayout.SuspensionStateFile(cityPath)
	if body == "" {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			t.Fatalf("remove suspension file: %v", err)
		}
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir suspension dir: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write suspension file: %v", err)
	}
}

// Kills: reads or repairs while the city is suspended (POOL-001: legacy's
// demand pass returns before any of them), a suspended pass that drops or
// replaces the last recording, and a lane that stays dark, or does not wake
// the allocator, after resume. Each way of suspending: Workspace.Suspended,
// the suspension file, GC_SUSPENDED=1.
func TestExternalReadsSkipsWhileCitySuspendedAndWakesOnResume(t *testing.T) {
	for _, tc := range []struct {
		name            string
		suspend, resume func(t *testing.T, cfg *config.City, cityPath string)
	}{{
		name:    "workspace suspended",
		suspend: func(_ *testing.T, cfg *config.City, _ string) { cfg.Workspace.Suspended = true },
		resume:  func(_ *testing.T, cfg *config.City, _ string) { cfg.Workspace.Suspended = false },
	}, {
		name: "suspension file",
		suspend: func(t *testing.T, _ *config.City, cityPath string) {
			writeSuspension(t, cityPath, `{"city":{"suspended":true}}`)
		},
		resume: func(t *testing.T, _ *config.City, cityPath string) { writeSuspension(t, cityPath, "") },
	}, {
		name:    "GC_SUSPENDED=1",
		suspend: func(t *testing.T, _ *config.City, _ string) { t.Setenv("GC_SUSPENDED", "1") },
		resume:  func(t *testing.T, _ *config.City, _ string) { t.Setenv("GC_SUSPENDED", "") },
	}} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			cityPath := t.TempDir()
			cfg := gaConfig()
			f := newClobberedRunFixture()
			cache, backing := newDemandCache(t, false, routedDemandBead("gc-r1"))
			lane, wakes := newTestBackstopLane(externalReadsEnv{CityPath: cityPath, Cfg: cfg, CityStore: f.mem, RigStores: nil, ProbeStores: []beads.Store{cache}, Sessions: f.sessions})
			t0 := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
			if !passAndJoin(ctx, at(lane, t0)) {
				t.Fatal("pass before suspending declined")
			}
			last := lane.recording()
			if err := f.mem.SetMetadataBatch("ga-run", clobberedWorkDir()); err != nil {
				t.Fatalf("re-clobber: %v", err)
			}

			// Each pass is past the repairs' minute, so only suspension
			// stops them.
			tc.suspend(t, cfg, cityPath)
			backing.armed.Store(true)
			if at(lane, t0.Add(2*externalReadsRepairInterval)).pass(ctx) {
				t.Fatal("a pass ran while the city is suspended")
			}
			if lane.recording() != last || wakes.Load() != 1 {
				t.Errorf("suspended pass: recording replaced=%t wakes=%d, want the last recording kept and no wake", lane.recording() != last, wakes.Load())
			}
			if got := f.workDir(t); got != clobberPoolSlot {
				t.Errorf("suspended pass repaired gc.work_dir = %q", got)
			}
			if ops := backing.readLog(); len(ops) > 0 {
				t.Errorf("suspended pass read a leg: %q", ops)
			}

			tc.resume(t, cfg, cityPath)
			if !passAndJoin(ctx, at(lane, t0.Add(4*externalReadsRepairInterval))) {
				t.Fatal("pass after resume declined")
			}
			if got := backstopSeq(lane); got != 2 {
				t.Errorf("after resume: seq %d, want 2", got)
			}
			if got := wakes.Load(); got != 2 {
				t.Errorf("after resume: wakes = %d, want 2: the stale sources turned fresh", got)
			}
			if got := f.workDir(t); got != clobberLiveWorkDir {
				t.Errorf("after resume: gc.work_dir = %q, want %q", got, clobberLiveWorkDir)
			}
		})
	}
}

// cancelOnListStore cancels a context on every List.
type cancelOnListStore struct {
	beads.Store
	cancel context.CancelFunc
}

func (s *cancelOnListStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	s.cancel()
	return s.Store.List(q)
}

// Kills: reads or repairs after shutdown began, and a recording published
// (with its allocator wake) by a pass that shutdown overtook mid-read (R3).
// A pass under a canceled context does nothing; a pass whose reads see
// shutdown begin publishes nothing and runs no step.
func TestBackstopLaneNoPassOrRepairAfterShutdown(t *testing.T) {
	f := newClobberedRunFixture()
	lane, wakes := newTestBackstopLane(externalReadsEnv{Cfg: gaConfig(), CityStore: f.mem, Sessions: f.sessions})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if lane.pass(ctx) {
		t.Fatal("a pass ran after shutdown")
	}
	if lane.recording() != nil || wakes.Load() != 0 || f.workDir(t) != clobberPoolSlot {
		t.Errorf("after shutdown: recording=%v wakes=%d work dir=%q, want nothing", lane.recording(), wakes.Load(), f.workDir(t))
	}

	ctx, cancel = context.WithCancel(context.Background())
	lane, wakes = newTestBackstopLane(externalReadsEnv{Cfg: demandReadsTestConfig(), CityStore: &cancelOnListStore{Store: beads.NewMemStore(), cancel: cancel}})
	if lane.pass(ctx) {
		t.Error("a pass overtaken by shutdown reported that it ran")
	}
	if ctx.Err() == nil || lane.recording() != nil || wakes.Load() != 0 {
		t.Errorf("pass overtaken by shutdown: canceled=%t recording=%v wakes=%d, want no publish and no wake", ctx.Err() != nil, lane.recording(), wakes.Load())
	}
}

// Kills: a declined pass counted toward the lane's pacing (R1). After a pass
// skipped while the city was suspended, the resume's wake runs a pass at once
// instead of waiting out a duty cycle the skipped pass never used.
func TestBackstopLaneSkippedPassDoesNotPaceTheNextWake(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		lane, _ := newTestBackstopLane(externalReadsEnv{CityPath: t.TempDir(), Cfg: demandReadsTestConfig(), CityStore: beads.NewMemStore()})
		startBackstopLaneInBubble(t, lane)
		advanceBackstop(backstopTestInterval)
		if got := backstopSeq(lane); got != 2 {
			t.Fatalf("before suspending: %d passes, want 2", got)
		}
		t.Setenv("GC_SUSPENDED", "1")
		advanceBackstop(backstopTestInterval)
		if got := backstopSeq(lane); got != 2 {
			t.Fatalf("suspended: %d passes, want the backstop's pass skipped", got)
		}
		advanceBackstop(externalReadsMinGap / 2)
		t.Setenv("GC_SUSPENDED", "")
		lane.wake()
		synctest.Wait()
		if got := backstopSeq(lane); got != 3 {
			t.Errorf("resume wake %v after a skipped pass: %d passes, want one at once", externalReadsMinGap/2, got)
		}
	})
}

// hungLiveStore blocks every live List until release is closed, counting
// them.
type hungLiveStore struct {
	beads.Store
	lists   *atomic.Int64
	release chan struct{}
}

func (s hungLiveStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	if q.Live {
		s.lists.Add(1)
		<-s.release
	}
	return s.Store.List(q)
}

// Kills: a pass that waits on a hung source past its deadline, a second read
// started while the first is still in flight, a timed-out leg read as zero
// demand, and a late result dropped or published with the wrong read times.
// With one leg hung and no result yet, the first pass publishes at the source
// deadline with the hung leg as errSourceTimeout and the others read; the
// next pass publishes at once, keeping the timeout and starting no second
// read; once the read returns it wakes the lane, whose next pass publishes it
// stamped with its own start and end.
func TestExternalReadsSlowSourceDoesNotDelayOthers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fast := beads.NewMemStore()
		if _, err := fast.Create(routedDemandBead("")); err != nil {
			t.Fatal(err)
		}
		hung := hungLiveStore{Store: beads.NewMemStoreFrom(0, []beads.Bead{routedDemandBead("gc-h1")}, nil), lists: &atomic.Int64{}, release: make(chan struct{})}
		var release sync.Once
		defer release.Do(func() { close(hung.release) })
		lane, _ := newTestBackstopLane(externalReadsEnv{Cfg: demandReadsTestConfig(), CityStore: fast, ProbeStores: []beads.Store{hung}})
		var mu sync.Mutex
		var published []sourceResult
		lane.onChange = func() {
			mu.Lock()
			defer mu.Unlock()
			if src, ok := lane.recording().source(keyOf(sourceDemand, hung)); ok {
				published = append(published, src)
			}
		}
		start := time.Now()
		startBackstopLaneInBubble(t, lane)
		if got := backstopSeq(lane); got != 0 {
			t.Fatalf("before the source deadline: seq %d, want no publish yet", got)
		}
		advanceBackstop(externalReadsSourceDeadline)
		check := func(when string, seq uint64, lists int64) {
			t.Helper()
			rec := lane.recording()
			if leg, ok := recordedLeg(rec, fast); backstopSeq(lane) != seq || !ok || leg.RawOpenErr != nil || len(leg.RawOpen) != 1 {
				t.Fatalf("%s: seq %d, fast leg recorded=%t %+v; want seq %d with the fast leg read", when, backstopSeq(lane), ok, leg, seq)
			}
			if src, ok := rec.source(keyOf(sourceDemand, hung)); !ok || !errors.Is(src.Err, errSourceTimeout) || !src.StartedAt.Equal(start) {
				t.Fatalf("%s: hung source %+v (recorded=%t), want errSourceTimeout from the read that started at %v", when, src, ok, start)
			}
			if got := hung.lists.Load(); got != lists {
				t.Fatalf("%s: %d live reads of the hung leg, want %d", when, got, lists)
			}
			if rows, err := newV2DemandReads(time.Now(), rec).RawOpen(hung); !errors.Is(err, errSourceTimeout) || len(rows) != 0 {
				t.Fatalf("%s: v2 RawOpen of the hung leg = %d rows, %v; want errSourceTimeout and no rows", when, len(rows), err)
			}
		}
		check("at the deadline", 1, 1)
		advanceBackstop(backstopTestInterval)
		check("next pass", 2, 1)

		release.Do(func() { close(hung.release) })
		released := time.Now()
		synctest.Wait()
		advanceBackstop(externalReadsMinGap)
		mu.Lock()
		defer mu.Unlock()
		if n := len(published); n != 2 || published[1].Err != nil || !published[1].StartedAt.Equal(start) || !published[1].EndedAt.Equal(released) || len(published[1].Leg.RawOpen) != 1 {
			t.Errorf("allocator wakes saw hung-leg results %+v; want the timeout, then the late read of gc-h1 started at %v and ended at %v", published, start, released)
		}
		if src, _ := lane.recording().source(keyOf(sourceDemand, hung)); hung.lists.Load() != 2 || !src.StartedAt.After(released) {
			t.Errorf("after the late result: %d live reads of the hung leg, source %+v; want a fresh read published", hung.lists.Load(), src)
		}
	})
}

// slowLiveStore delays every live List and every Ready by d of bubble time.
type slowLiveStore struct {
	beads.Store
	d *atomic.Int64
}

func newSlowLiveStore(d time.Duration) slowLiveStore {
	v := &atomic.Int64{}
	v.Store(int64(d))
	return slowLiveStore{Store: beads.NewMemStore(), d: v}
}

func (s slowLiveStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	if q.Live {
		<-time.After(time.Duration(s.d.Load()))
	}
	return s.Store.List(q)
}

func (s slowLiveStore) Ready(q ...beads.ReadyQuery) ([]beads.Bead, error) {
	<-time.After(time.Duration(s.d.Load()))
	return s.Store.Ready(q...)
}

// Kills: a deadline that decides whether a read counts (M-deadline: a read
// slower than the source deadline published as errSourceTimeout forever), a
// late result dropped, and a slow source restarted only a duty cycle after its
// read ended. Once the leg has been served, its demand and session reads stay
// served at every sample over ten minutes at bd-like latencies, up to reads
// slower than the source deadline (maintainer-city patrol). Only the warm-up,
// before the first read of each source ends, may read partial.
func TestExternalReadsSteadyStateAtBdLatencies(t *testing.T) {
	for _, d := range []time.Duration{2 * time.Second, 3600 * time.Millisecond, 7400 * time.Millisecond, 12 * time.Second} {
		t.Run(d.String(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				store := newSlowLiveStore(d)
				if _, err := store.Create(routedDemandBead("")); err != nil {
					t.Fatal(err)
				}
				if _, err := store.Create(backstopSessionBead("", "worker-1")); err != nil {
					t.Fatal(err)
				}
				lane, _ := newTestBackstopLane(externalReadsEnv{Cfg: demandReadsTestConfig(), CityStore: store})
				lane.steps = nil
				start := time.Now()
				startBackstopLaneInBubble(t, lane)
				var warmUp time.Duration
				var samples, partial int
				for end := start.Add(10 * time.Minute); time.Now().Before(end); {
					<-time.After(time.Second)
					rec := lane.recording()
					_, derr := newV2DemandReads(time.Now(), rec).RawOpen(store)
					sl, ok := rec.sessionLeg(store)
					served := derr == nil && ok && sl.Err == nil
					if warmUp == 0 {
						if served {
							warmUp = time.Since(start)
						}
						continue
					}
					samples++
					if !served {
						partial++
					}
				}
				if warmUp == 0 || warmUp > 4*d+externalReadsSourceDeadline || partial > 0 {
					t.Errorf("per-read %v: served after %v, then partial at %d of %d samples; want 0", d, warmUp, partial, samples)
				}
			})
		})
	}
}

// Kills: a missed deadline that drops the previous result (the leg partial
// at once), a held-over result re-aged from the pass rather than its own end,
// and one served past its freshness. A hung read republishes the last good
// result, marked in flight, until 3 × patrol after that result's end; then
// the leg is stale. When the read finally ends, it is published with its true
// read times and wakes the allocator.
func TestExternalReadsMissServesPreviousResultUntilStale(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := newSlowLiveStore(0)
		if _, err := store.Create(routedDemandBead("")); err != nil {
			t.Fatal(err)
		}
		lane, wakes := newTestBackstopLane(externalReadsEnv{Cfg: demandReadsTestConfig(), CityStore: store})
		lane.steps = nil
		t0 := time.Now()
		startBackstopLaneInBubble(t, lane)
		const hang = 10 * time.Minute
		store.d.Store(int64(hang))
		advanceBackstop(backstopTestInterval + externalReadsSourceDeadline)
		hungSince := t0.Add(backstopTestInterval)
		src, _ := lane.recording().source(keyOf(sourceDemand, store))
		if src.Err != nil || !src.StartedAt.Equal(t0) || !src.EndedAt.Equal(t0) || !src.InFlightSince.Equal(hungSince) || len(src.Leg.RawOpen) != 1 {
			t.Fatalf("missed deadline: demand source %+v, want the result read at %v, in flight since %v", src, t0, hungSince)
		}
		for time.Now().Before(t0.Add(3 * backstopTestInterval)) {
			advanceBackstop(time.Second)
			if _, err := newV2DemandReads(time.Now(), lane.recording()).RawOpen(store); err != nil {
				t.Fatalf("held over at %v: %v, want the previous result served", time.Since(t0), err)
			}
		}
		advanceBackstop(time.Nanosecond)
		if _, err := newV2DemandReads(time.Now(), lane.recording()).RawOpen(store); !errors.Is(err, errDemandRecordingStale) {
			t.Fatalf("past 3 patrols from the result's end: %v, want stale", err)
		}
		before := wakes.Load()
		store.d.Store(0)
		advanceBackstop(time.Until(hungSince.Add(hang + externalReadsMinGap)))
		src, _ = lane.recording().source(keyOf(sourceDemand, store))
		if wakes.Load() == before || src.Err != nil || !src.StartedAt.After(t0) || !lane.recording().fresh(src, time.Now()) {
			t.Fatalf("after the hung read ended: wakes %d -> %d, source %+v; want a wake and a fresh result", before, wakes.Load(), src)
		}
	})
}

// Kills: a late scale_check run of an older config published after a reload,
// one config's run blocking the next config's, and a scale_check source
// timed out at the store deadline rather than its own (legacy's per-probe)
// budget. The old config's run hangs past the source deadline without
// publishing a timeout; the new config's runs and publishes; the old run's
// late result is never published, nor held.
func TestExternalReadsDropsLateScaleCheckOfOlderConfig(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		var releaseOnce sync.Once
		defer releaseOnce.Do(func() { close(release) })
		oldCfg := &config.City{Agents: []config.Agent{{Name: "w", ScaleCheck: "check-old"}}}
		newCfg := &config.City{Agents: []config.Agent{{Name: "w", ScaleCheck: "check-new"}}}
		var cfg atomic.Pointer[config.City]
		cfg.Store(oldCfg)
		lane := newExternalReadsLane(backstopTestInterval, func() (externalReadsEnv, error) {
			return externalReadsEnv{CityName: "city", Cfg: cfg.Load()}, nil
		}, nil, func(fn func(), _ string) bool { fn(); return false }, nil, io.Discard)
		lane.steps = nil
		lane.queryEnv = func(string, *config.City, *config.Agent) (map[string]string, error) { return nil, nil }
		lane.runner = func(command, _ string, _ map[string]string) (string, error) {
			if command == "check-old" {
				<-release
				return "7", nil
			}
			return "2", nil
		}
		startBackstopLaneInBubble(t, lane)
		advanceBackstop(externalReadsSourceDeadline)
		if src, ok := scaleCheckSource(lane.recording()); ok {
			t.Fatalf("old config's run within its budget: published %+v, want nothing yet", src)
		}
		cfg.Store(newCfg)
		advanceBackstop(backstopTestInterval)
		if r := lane.recording().scaleCheck(time.Now()); r == nil || r.Counts["w"] != 2 {
			t.Fatalf("after the reload: scale_check %+v, want the new config's count 2", r)
		}
		releaseOnce.Do(func() { close(release) })
		advanceBackstop(backstopTestInterval)
		var keys int
		for key := range lane.recording().Sources {
			if key.kind == sourceScaleCheck {
				keys++
				if key.cfg != newCfg {
					t.Errorf("published a scale_check source of an older config")
				}
			}
		}
		if r := lane.recording().scaleCheck(time.Now()); keys != 1 || r == nil || r.Counts["w"] != 2 {
			t.Errorf("after the old run ended: %d scale_check sources, result %+v; want only the new config's count 2", keys, r)
		}
		lane.mu.Lock()
		_, kept := lane.results[sourceKey{kind: sourceScaleCheck, cfg: oldCfg}]
		lane.mu.Unlock()
		if kept {
			t.Error("the old config's late result is still held: every reload would leak one")
		}
	})
}

// Kills: a freshness bound other than 3 × patrol from when a source's read
// ended, a census expiry that differs from the demand reads', and no wake
// when a fresh source replaces a stale one with the same content.
func TestExternalReadsSourceFreshForThreePatrols(t *testing.T) {
	cache, _ := newDemandCache(t, false, routedDemandBead("gc-r1"), backstopSessionBead("gc-s1", "worker-1"))
	lane, wakes := newTestBackstopLane(externalReadsEnv{Cfg: demandReadsTestConfig(), CityStore: cache})
	t0 := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	at(lane, t0).pass(context.Background())
	rec := lane.recording()
	bound := t0.Add(3 * backstopTestInterval)
	if _, err := newV2DemandReads(bound, rec).RawOpen(cache); err != nil {
		t.Errorf("RawOpen at 3 patrols: %v, want served", err)
	}
	if _, err := newV2DemandReads(bound.Add(time.Nanosecond), rec).RawOpen(cache); !errors.Is(err, errDemandRecordingStale) {
		t.Errorf("RawOpen past 3 patrols: err = %v, want the stale source", err)
	}
	if leg, ok := rec.sessionLeg(cache); !ok || !leg.At.Equal(t0) || !leg.StartedAt.Equal(t0) || !leg.Expires.Equal(bound) {
		t.Errorf("census session leg %+v (recorded=%t), want read at %v and expiring at %v", leg, ok, t0, bound)
	}

	at(lane, t0.Add(backstopTestInterval)).pass(context.Background())
	if got := wakes.Load(); got != 1 {
		t.Fatalf("fresh unchanged pass: wakes = %d, want 1", got)
	}
	at(lane, t0.Add(4*backstopTestInterval+time.Nanosecond)).pass(context.Background())
	if got := wakes.Load(); got != 2 {
		t.Errorf("unchanged pass replacing stale sources: wakes = %d, want 2", got)
	}
}

// Kills: a pass that ignores the reload barrier's or the FS gate's hold, and
// a held pass that never runs once released. A held pass declines; the
// release wakes the lane, which runs at once.
func TestExternalReadsPassHeldByGate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		lane, _ := newTestBackstopLane(externalReadsEnv{Cfg: demandReadsTestConfig(), CityStore: beads.NewMemStore()})
		lane.gate = newLaneGate(lane.wake)
		lane.gate.hold("reload")
		startBackstopLaneInBubble(t, lane)
		if got := backstopSeq(lane); got != 0 {
			t.Fatalf("held: %d passes, want none", got)
		}
		lane.gate.release("reload")
		synctest.Wait()
		if got := backstopSeq(lane); got != 1 {
			t.Errorf("released: %d passes, want one at once", got)
		}
	})
}

// Kills: steps run on the lane goroutine (a slow step stalls the reads),
// steps outside the gate (M6: waitIdle idle while a step writes), and a second
// run of a step while one is in flight. With a step blocked, passes keep
// publishing at the patrol, the step does not start again though its interval
// is up, and waitIdle waits for it.
func TestExternalReadsStepRunsOffTheReadPathInsideTheGate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		lane, _ := newTestBackstopLane(externalReadsEnv{Cfg: demandReadsTestConfig(), CityStore: beads.NewMemStore()})
		lane.gate = newLaneGate(lane.wake)
		var runs atomic.Int64
		release := make(chan struct{})
		lane.steps = []*laneStep{{name: "slow", every: time.Second, run: func(context.Context, externalReadsEnv) {
			runs.Add(1)
			<-release
		}}}
		startBackstopLaneInBubble(t, lane)
		advanceBackstop(3 * backstopTestInterval)
		if seq, n := backstopSeq(lane), runs.Load(); seq != 4 || n != 1 {
			t.Fatalf("with the step blocked: %d passes, %d step runs; want the patrol's 4 passes and one run", seq, n)
		}
		idle := make(chan error, 1)
		go func() { idle <- lane.gate.waitIdle(context.Background()) }()
		synctest.Wait()
		select {
		case <-idle:
			t.Fatal("waitIdle reported idle while a step was running")
		default:
		}
		close(release)
		synctest.Wait()
		select {
		case err := <-idle:
			if err != nil {
				t.Fatalf("waitIdle = %v", err)
			}
		default:
			t.Fatal("waitIdle still waiting after the step ended")
		}
	})
}

// Kills: a step with no deadline (M7) and a step interval measured from the
// run's start. Blocked on its context, the step ends at the step deadline;
// its next run waits a whole interval from that end.
func TestExternalReadsStepDeadlineAndIntervalFromItsEnd(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		lane, _ := newTestBackstopLane(externalReadsEnv{Cfg: demandReadsTestConfig(), CityStore: beads.NewMemStore()})
		var runs, ends atomic.Int64
		const every = 2 * time.Minute
		lane.steps = []*laneStep{{name: "stuck", every: every, run: func(ctx context.Context, _ externalReadsEnv) {
			runs.Add(1)
			<-ctx.Done()
			ends.Add(1)
		}}}
		startBackstopLaneInBubble(t, lane)
		advanceBackstop(externalReadsStepDeadline - time.Nanosecond)
		if got := ends.Load(); got != 0 {
			t.Fatalf("before the step deadline: %d runs ended, want 0", got)
		}
		advanceBackstop(time.Nanosecond)
		if got := ends.Load(); got != 1 {
			t.Fatalf("at the step deadline: %d runs ended, want 1", got)
		}
		advanceBackstop(every - time.Millisecond)
		if got := runs.Load(); got != 1 {
			t.Fatalf("within the interval from the run's end: %d runs, want 1", got)
		}
		advanceBackstop(time.Millisecond)
		if got := runs.Load(); got != 2 {
			t.Fatalf("an interval after the run ended: %d runs, want 2", got)
		}
	})
}

// failingLiveStore fails every live List while fail is set.
type failingLiveStore struct {
	beads.Store
	fail *atomic.Bool
}

func (s failingLiveStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	if q.Live && s.fail.Load() {
		return nil, errDemandBackingDown
	}
	return s.Store.List(q)
}

// Kills: a step judged on the previous recording rather than the one its
// pass publishes (M5: steps before the publish), and a step run while the
// legs it reads just failed. A pass whose demand leg read fails skips the
// step; the pass whose read recovers runs it.
func TestExternalReadsStepSkippedWhileItsLegsFail(t *testing.T) {
	fail := &atomic.Bool{}
	lane, _ := newTestBackstopLane(externalReadsEnv{Cfg: demandReadsTestConfig(), CityStore: failingLiveStore{Store: beads.NewMemStore(), fail: fail}})
	var runs atomic.Int64
	lane.steps = []*laneStep{{name: "repairs", every: time.Minute, legs: sourceDemand, run: func(context.Context, externalReadsEnv) { runs.Add(1) }}}
	t0 := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	fail.Store(true)
	passAndJoin(context.Background(), at(lane, t0))
	if got := runs.Load(); got != 0 {
		t.Fatalf("legs failing: %d step runs, want the step skipped", got)
	}
	fail.Store(false)
	passAndJoin(context.Background(), at(lane, t0.Add(backstopTestInterval)))
	if got := runs.Load(); got != 1 {
		t.Fatalf("legs recovered: %d step runs, want the step run by the pass that read them", got)
	}
}

// Kills: a step panic not counted as a run (M16), so a broken step retries
// every pass, and a panic that stops the step for good.
func TestExternalReadsStepPanicCountsAsRun(t *testing.T) {
	lane, _ := newTestBackstopLane(externalReadsEnv{Cfg: demandReadsTestConfig(), CityStore: beads.NewMemStore()})
	var runs atomic.Int64
	lane.steps = []*laneStep{{name: "boom", every: time.Minute, run: func(context.Context, externalReadsEnv) {
		runs.Add(1)
		panic("step exploded")
	}}}
	t0 := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		after time.Duration
		runs  int64
	}{{0, 1}, {backstopTestInterval, 1}, {time.Minute, 2}} {
		passAndJoin(context.Background(), at(lane, t0.Add(tc.after)))
		if got := runs.Load(); got != tc.runs {
			t.Fatalf("pass at +%v: %d step runs, want %d", tc.after, got, tc.runs)
		}
	}
}

// clockStore runs onList before every live List.
type clockStore struct {
	beads.Store
	onList func()
}

func (s clockStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	if q.Live {
		s.onList()
	}
	return s.Store.List(q)
}

// Kills: the stale-to-fresh wake judged when the reads start (M2). The pass
// starts while the previous result is fresh, and its reads end after that
// result went stale: the unchanged content, fresh again, must wake the
// allocator.
func TestExternalReadsStaleTurnedFreshJudgedAtReadEnd(t *testing.T) {
	t0 := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	var clock atomic.Int64
	store := clockStore{Store: beads.NewMemStoreFrom(0, []beads.Bead{routedDemandBead("gc-r1")}, nil), onList: func() {}}
	lane, wakes := newTestBackstopLane(externalReadsEnv{Cfg: demandReadsTestConfig(), CityStore: &store})
	lane.steps = nil
	lane.now = func() time.Time { return t0.Add(time.Duration(clock.Load())) }
	lane.pass(context.Background())
	clock.Store(int64(3*backstopTestInterval - time.Second))
	store.onList = func() { clock.Store(int64(3*backstopTestInterval + time.Second)) }
	lane.pass(context.Background())
	if got := wakes.Load(); got != 2 {
		t.Errorf("reads that end after the previous result went stale: wakes = %d, want 2", got)
	}
}

// Kills: a session read stamped with its end as its start (M1, M24), which
// would clear an ambiguous create against a read that started before its
// write landed (C5.4(3)). A 4s session read publishes its start, not its end,
// through sessionLeg and into the census ledger view.
func TestExternalReadsSessionReadStartReachesLedger(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := newSlowLiveStore(2 * time.Second)
		if _, err := store.Create(backstopSessionBead("", "worker-1")); err != nil {
			t.Fatal(err)
		}
		lane, _ := newTestBackstopLane(externalReadsEnv{Cfg: demandReadsTestConfig(), CityStore: store})
		lane.steps = nil
		start := time.Now()
		startBackstopLaneInBubble(t, lane)
		advanceBackstop(5 * time.Second)
		rec := lane.recording()
		sl, ok := rec.sessionLeg(store)
		if !ok || sl.Err != nil || !sl.StartedAt.Equal(start) || !sl.At.Equal(start.Add(4*time.Second)) {
			t.Fatalf("session leg %+v: want StartedAt %v and At %v", sl, start, start.Add(4*time.Second))
		}
		r := newCensusReader(censusLegFeed{exact: func(beads.Store) bool { return false }, recorded: rec.sessionLeg})
		c, err := r.read(sl.At, demandReadsTestConfig(), []classStoreCandidate{{store: store, ref: "class:sessions"}})
		if err != nil {
			t.Fatal(err)
		}
		if got := c.Ledger(demandReadsTestConfig()).ReadStarted["class:sessions"]; !got.Equal(start) {
			t.Fatalf("ledger ReadStarted = %v, want the read's start %v", got, start)
		}
	})
}

// Kills: a join that returns while a read is in flight (the store-close path
// would close a store under it), and a restart that forgets the reads in
// flight (a second read of a hung source) or the recording, or keeps the old
// patrol's freshness.
func TestExternalReadsRestartKeepsReadsInFlightAndJoinWaits(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		hung := hungLiveStore{Store: beads.NewMemStore(), lists: &atomic.Int64{}, release: make(chan struct{})}
		defer close(hung.release)
		lane, _ := newTestBackstopLane(externalReadsEnv{Cfg: demandReadsTestConfig(), CityStore: beads.NewMemStore(), ProbeStores: []beads.Store{hung}})
		lane.steps = nil
		ctx, cancel := context.WithCancel(context.Background())
		done := lane.start(ctx)
		advanceBackstop(externalReadsSourceDeadline)
		cancel()
		<-done
		jctx, jcancel := context.WithTimeout(context.Background(), time.Minute)
		defer jcancel()
		if err := lane.join(jctx); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("join with a read in flight = %v, want it to wait", err)
		}
		seq := backstopSeq(lane)
		lane.interval = 2 * backstopTestInterval
		startBackstopLaneInBubble(t, lane)
		if got, lists := backstopSeq(lane), hung.lists.Load(); got != seq+1 || lists != 1 || lane.recording().FreshFor != 6*backstopTestInterval {
			t.Fatalf("after restart: seq %d (was %d), %d reads of the hung leg, fresh for %v; want one more publish, no second read, 3 × the new patrol", got, seq, lists, lane.recording().FreshFor)
		}
	})
}

// Kills: a session census that misses out-of-process writes on a bd leg. A
// session row created, and one rewritten, behind the leg's cache (no event)
// must reach the recording on the next pass, not wait for the cache's
// re-scan; and the change must wake the allocator.
func TestBackstopLaneRecordsOutOfProcessSessionWriteOnNonExactLeg(t *testing.T) {
	cache, backing := newDemandCache(t, false, backstopSessionBead("gc-s1", "worker-1"))
	lane, wakes := newTestBackstopLane(externalReadsEnv{Cfg: demandReadsTestConfig(), CityStore: cache})
	t0 := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)

	at(lane, t0).pass(context.Background())
	first := lane.recording()
	leg, ok := first.sessionLeg(cache)
	if !ok || leg.Err != nil || !leg.At.Equal(t0) || !leg.StartedAt.Equal(t0) ||
		!leg.Expires.Equal(t0.Add(3*backstopTestInterval)) || !slices.Equal(sessionInfoIDs(leg.Rows), []string{"gc-s1"}) {
		t.Fatalf("first pass: session leg recorded=%t %+v, want gc-s1 at %v", ok, leg, t0)
	}

	// The out-of-process writer: straight to the backing, so the cache sees
	// neither write.
	created, err := backing.Create(backstopSessionBead("", "worker-2"))
	if err != nil {
		t.Fatalf("out-of-process create: %v", err)
	}
	if err := backing.SetMetadata("gc-s1", "state", "asleep"); err != nil {
		t.Fatalf("out-of-process update: %v", err)
	}
	if cached, _ := cache.CachedList(beads.ListQuery{Type: sessionBeadType}); len(cached) != 1 {
		t.Fatalf("the cache saw the out-of-process create (%d rows); the test needs it not to", len(cached))
	}

	at(lane, t0.Add(backstopTestInterval)).pass(context.Background())
	leg, ok = lane.recording().sessionLeg(cache)
	if !ok || leg.Err != nil || !leg.At.Equal(t0.Add(backstopTestInterval)) {
		t.Fatalf("second pass: session leg recorded=%t %+v", ok, leg)
	}
	if got, want := sessionInfoIDs(leg.Rows), slices.Sorted(slices.Values([]string{"gc-s1", created.ID})); !slices.Equal(got, want) {
		t.Errorf("second pass session rows = %v, want %v", got, want)
	}
	for _, info := range leg.Rows {
		if info.ID == "gc-s1" && info.MetadataState != "asleep" {
			t.Errorf("gc-s1 recorded in state %q, want the out-of-process asleep", info.MetadataState)
		}
	}
	if got := wakes.Load(); got != 2 {
		t.Errorf("allocator wakes = %d, want 2: the session change must wake it", got)
	}
	// The session rows sit in the leg's open List too; the session change
	// alone must still count as new content.
	sessionsOnly := &externalReadsRecording{Sources: maps.Clone(first.Sources)}
	key := keyOf(sourceSessions, cache)
	sessionsOnly.Sources[key] = lane.recording().Sources[key]
	if first.sameContent(sessionsOnly) {
		t.Error("a recording whose only change is session rows reads as unchanged")
	}

	// A failed live read is recorded as the failure, with no rows, so the
	// census can tell it from a partial read.
	backing.failLive.Store(true)
	at(lane, t0.Add(2*backstopTestInterval)).pass(context.Background())
	leg, ok = lane.recording().sessionLeg(cache)
	if !ok || !errors.Is(leg.Err, errDemandBackingDown) || beads.IsPartialResult(leg.Err) || len(leg.Rows) != 0 {
		t.Errorf("failed pass: session leg recorded=%t %+v, want the hard error and no rows", ok, leg)
	}
}

// Kills: the lane recording cached rows instead of live ones (M31, M31b). A
// non-exact cache misses rows written straight to its backing; the
// recording's open List and Ready must carry them.
func TestBackstopLaneRecordsLiveReadsNotTheCache(t *testing.T) {
	cache, backing := newDemandCache(t, false, routedDemandBead("gc-r1"))
	late, err := backing.Create(routedDemandBead(""))
	if err != nil {
		t.Fatalf("backing-only create: %v", err)
	}
	if cached, _ := cache.CachedList(beads.ListQuery{Status: "open"}); len(cached) != 1 {
		t.Fatalf("the cache saw the backing-only row (%d rows); the test needs it not to", len(cached))
	}
	lane, _ := newTestBackstopLane(externalReadsEnv{Cfg: demandReadsTestConfig(), CityStore: cache})
	at(lane, time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)).pass(context.Background())
	leg, ok := recordedLeg(lane.recording(), cache)
	want := slices.Sorted(slices.Values([]string{"gc-r1", late.ID}))
	if !ok || !slices.Equal(slices.Sorted(slices.Values(ids(leg.RawOpen))), want) || !slices.Equal(slices.Sorted(slices.Values(ids(leg.ReadyAll))), want) {
		t.Errorf("recorded open=%v ready=%v (recorded=%t), want both %v", ids(leg.RawOpen), ids(leg.ReadyAll), ok, want)
	}
}

// Kills: recording or looking a leg up by the policy front door instead of
// the store behind it (M26, M26b). The lane is handed the front door; the
// recording answers for the front door and the bare cache alike, and v2 reads
// through the front door serve it.
func TestBackstopLaneRecordsAndServesThroughPolicyFrontDoor(t *testing.T) {
	cfg := demandReadsTestConfig()
	cache, _ := newDemandCache(t, false, routedDemandBead("gc-r1"), backstopSessionBead("gc-s1", "worker-1"))
	front := wrapStoreWithBeadPolicies(cache, cfg)
	if front == beads.Store(cache) {
		t.Fatal("the policy front door did not wrap the cache; the test needs distinct stores")
	}
	lane, _ := newTestBackstopLane(externalReadsEnv{Cfg: cfg, CityStore: front})
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	at(lane, now).pass(context.Background())
	rec := lane.recording()
	for name, store := range map[string]beads.Store{"front door": front, "cache": cache} {
		if _, ok := recordedLeg(rec, store); !ok {
			t.Errorf("%s: demand leg not found", name)
		}
		if _, ok := rec.sessionLeg(store); !ok {
			t.Errorf("%s: session leg not found", name)
		}
	}
	reads := newV2DemandReads(now, rec)
	if rows, err := reads.RawOpen(front); err != nil || !slices.Contains(ids(rows), "gc-r1") {
		t.Errorf("v2 RawOpen through the front door = %v, %v; want the recorded gc-r1", ids(rows), err)
	}
}

// Kills: the closed named-session index recorded under the policy front
// door (R8) or looked up without unwrapping it (R9). The lane is handed the
// front door; v2 asked through the front door serves the recorded index.
func TestBackstopLaneClosedNamedIndexThroughPolicyFrontDoor(t *testing.T) {
	cfg := demandReadsTestConfig()
	cfg.NamedSessions = []config.NamedSession{{Name: "mayor", Template: "worker", Mode: "on_demand"}}
	cache, _ := newDemandCache(t, false, closedNamedSessionBead("gc-closed", "mayor"))
	front := wrapStoreWithBeadPolicies(cache, cfg)
	if front == beads.Store(cache) {
		t.Fatal("the policy front door did not wrap the cache; the test needs distinct stores")
	}
	lane, _ := newTestBackstopLane(externalReadsEnv{Cfg: cfg, CityStore: front})
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	at(lane, now).pass(context.Background())
	for name, store := range map[string]beads.Store{"front door": front, "cache": cache} {
		idx, err := newV2DemandReads(now, lane.recording()).ClosedNamedIndex(store)
		if _, found := idx.Find("mayor"); err != nil || !found {
			t.Errorf("%s: v2 closed index found mayor=%t err=%v, want the recorded index", name, found, err)
		}
	}
}

// Kills: a census feed whose recorded leg does not match the census's leg
// keys (externalReadsRecording.sessionLeg is the feed's recorded function,
// so both key by the store behind the policy front door), and a census that
// ages the lane's leg on anything but the source's expiry. The census
// reads the lane's recording through the front door and the bare cache.
func TestCensusReadsBackstopSessionLegThroughPolicyFrontDoor(t *testing.T) {
	cfg := demandReadsTestConfig()
	cache, _ := newDemandCache(t, false, backstopSessionBead("gc-s1", "worker-1"))
	front := wrapStoreWithBeadPolicies(cache, cfg)
	if front == beads.Store(cache) {
		t.Fatal("the policy front door did not wrap the cache; the test needs distinct stores")
	}
	lane, _ := newTestBackstopLane(externalReadsEnv{Cfg: cfg, CityStore: front})
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	at(lane, now).pass(context.Background())
	rec := lane.recording()
	sl, _ := rec.sessionLeg(cache)
	feed := censusLegFeed{
		exact:    func(s beads.Store) bool { _, exact := demandLegCache(s); return exact },
		recorded: rec.sessionLeg,
	}
	for name, store := range map[string]beads.Store{"front door": front, "cache": cache} {
		r := newCensusReader(feed)
		legs := []classStoreCandidate{{store: store, ref: "class:sessions"}}
		c, err := r.read(sl.Expires, cfg, legs)
		if err != nil {
			t.Fatalf("%s: census read: %v", name, err)
		}
		if leg := c.Legs[0]; leg.State != legRead || !leg.ReadAt.Equal(sl.At) {
			t.Errorf("%s: leg %+v, want read at the recording's At", name, leg)
		}
		if _, ok := c.Rows[rowKey{Leg: "class:sessions", ID: "gc-s1"}]; !ok {
			t.Errorf("%s: recorded session row missing from the census", name)
		}
		if c, err = r.read(sl.Expires.Add(time.Nanosecond), cfg, legs); err != nil || c.Legs[0].State != legStale {
			t.Errorf("%s: past the recording's Expires: err=%v leg=%+v, want stale", name, err, c.Legs)
		}
	}
}

// Kills: one panicking leg read killing the process from the lane's
// goroutine (mc-zndi7.40), and a nil allocator wake. The leg is recorded with
// the panic as its error and the pass completes.
func TestBackstopLaneRecordsPanickingLegAsError(t *testing.T) {
	store := panickingListStore{beads.NewMemStore()}
	lane := newExternalReadsLane(time.Minute, func() (externalReadsEnv, error) {
		return externalReadsEnv{Cfg: demandReadsTestConfig(), CityStore: store}, nil
	}, nil, func(fn func(), _ string) bool { fn(); return false }, nil, io.Discard)
	// The repairs' collectors read in goroutines of their own, as legacy's
	// tick does; this test is about the lane's source goroutines.
	lane.steps = nil
	if !at(lane, time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)).pass(context.Background()) {
		t.Fatal("pass declined")
	}
	leg, ok := recordedLeg(lane.recording(), store)
	if !ok || leg.RawOpenErr == nil || !strings.Contains(leg.RawOpenErr.Error(), "panicked") {
		t.Errorf("panicking leg recorded=%t open err=%v, want the panic as its error", ok, leg.RawOpenErr)
	}
	if s, ok := lane.recording().sessionLeg(store); !ok || s.Err == nil || !strings.Contains(s.Err.Error(), "panicked") {
		t.Errorf("panicking session leg recorded=%t err=%v, want the panic as its error", ok, s.Err)
	}
}

type panickingListStore struct{ beads.Store }

func (panickingListStore) List(beads.ListQuery) ([]beads.Bead, error) { panic("list exploded") }

// Kills: an exact leg recorded, which would put live reads of the sessions
// binding back on the lane. A pass over an exact leg records neither its
// demand nor its session rows and never reads its backing; the same leg not
// exact is the control.
func TestBackstopLaneNeverRecordsExactLeg(t *testing.T) {
	t0 := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	for _, exact := range []bool{true, false} {
		cache, backing := newDemandCache(t, exact, backstopSessionBead("gc-s1", "worker-1"), routedDemandBead("gc-r1"))
		lane, _ := newTestBackstopLane(externalReadsEnv{Cfg: demandReadsTestConfig(), CityStore: cache})
		// The repairs step reads every leg live, as legacy's does; this
		// test is about the sources.
		lane.steps = nil
		backing.armed.Store(true)

		at(lane, t0).pass(context.Background())

		rec := lane.recording()
		_, sessionRecorded := rec.sessionLeg(cache)
		_, demandRecorded := recordedLeg(rec, cache)
		_, closedRecorded := recordedClosedNamed(rec, cache)
		backingRead := len(backing.readLog()) > 0
		if want := !exact; sessionRecorded != want || demandRecorded != want || backingRead != want || closedRecorded {
			t.Errorf("exact=%t: session recorded=%t, demand recorded=%t, closed index recorded=%t, backing read=%t; want %t, %t, false, %t",
				exact, sessionRecorded, demandRecorded, closedRecorded, backingRead, want, want, want)
		}
	}
}

// closedNamedSessionBead is a closed session row for a configured named
// identity: the closed phantom readyAssignedWorkAssignees looks up.
func closedNamedSessionBead(id, identity string) beads.Bead {
	return beads.Bead{
		ID: id, Title: identity, Type: sessionBeadType, Status: "closed", Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{session.NamedSessionIdentityMetadata: identity, "session_name": "s-" + identity},
	}
}

// Kills: the closed named-session index served from a cache (no cache holds
// closed history), skipped on an exact leg, recorded with no on_demand named
// session, or left out of the content comparison. With an on_demand named
// session the lane reads the city store's index live even on an exact leg,
// and v2 serves it as legacy reads it.
func TestBackstopLaneRecordsClosedNamedIndexOnEveryLeg(t *testing.T) {
	cfg := demandReadsTestConfig()
	cfg.NamedSessions = []config.NamedSession{{Name: "mayor", Template: "worker", Mode: "on_demand"}}
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	for _, exact := range []bool{true, false} {
		cache, backing := newDemandCache(t, exact, closedNamedSessionBead("gc-closed", "mayor"))
		lane, wakes := newTestBackstopLane(externalReadsEnv{Cfg: cfg, CityStore: cache})
		// The repairs step reads every leg live, as legacy's does; this
		// test is about the sources.
		lane.steps = nil
		at(lane, now).pass(context.Background())
		rec := lane.recording()
		if l, ok := recordedClosedNamed(rec, cache); !ok || l.Err != nil {
			t.Fatalf("exact=%t: closed index recorded=%t err=%v, want recorded", exact, ok, l.Err)
		}

		legacy := readyAssignedWorkAssignees(cfg, cache, nil, nil, nil, "", nil)
		backing.armed.Store(true)
		v2 := readyAssignedWorkAssignees(cfg, cache, nil, nil, nil, "", newV2DemandReads(now, rec))
		if runtime := config.NamedSessionRuntimeName(config.EffectiveCityName(cfg, ""), cfg.Workspace, "mayor"); !slices.Equal(v2, legacy) || !slices.Contains(v2, runtime) {
			t.Errorf("exact=%t: v2 assignees %v, legacy %v; want equal, with the closed phantom's %q", exact, v2, legacy, runtime)
		}
		if ops := backing.readLog(); len(ops) > 0 {
			t.Errorf("exact=%t: v2 read the backing for the closed index: %q", exact, ops)
		}

		// A phantom closed behind the cache changes the index alone (the
		// session census drops closed rows) and wakes the allocator.
		phantom, err := backing.Create(closedNamedSessionBead("", "mayor-2"))
		if err != nil {
			t.Fatal(err)
		}
		if err := backing.Close(phantom.ID); err != nil {
			t.Fatal(err)
		}
		before := wakes.Load()
		at(lane, now.Add(backstopTestInterval)).pass(context.Background())
		if got := wakes.Load(); got != before+1 {
			t.Errorf("exact=%t: a changed closed index woke the allocator %d times, want once", exact, got-before)
		}
	}

	cache, backing := newDemandCache(t, true, closedNamedSessionBead("gc-closed", "mayor"))
	lane, _ := newTestBackstopLane(externalReadsEnv{Cfg: demandReadsTestConfig(), CityStore: cache})
	lane.steps = nil
	backing.armed.Store(true)
	at(lane, now).pass(context.Background())
	if _, ok := recordedClosedNamed(lane.recording(), cache); ok || len(backing.readLog()) > 0 {
		t.Errorf("no on_demand named session: closed index recorded=%t backing reads=%q, want neither", ok, backing.readLog())
	}
}

// Kills: a content comparison that ignores the Ready rows (R13). An
// in_progress blocker closes: the dependent's open row is unchanged and only
// the Ready read changes, which must wake the allocator.
func TestBackstopLaneWakesOnReadyOnlyChange(t *testing.T) {
	mem := beads.NewMemStore()
	blocker, err := mem.Create(beads.Bead{Title: "blocker", Type: "task", Assignee: "someone"})
	if err != nil {
		t.Fatal(err)
	}
	inProgress := "in_progress"
	if err := mem.Update(blocker.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatal(err)
	}
	dep, err := mem.Create(routedDemandBead(""))
	if err != nil {
		t.Fatal(err)
	}
	if err := mem.DepAdd(dep.ID, blocker.ID, "blocks"); err != nil {
		t.Fatal(err)
	}
	lane, wakes := newTestBackstopLane(externalReadsEnv{Cfg: demandReadsTestConfig(), CityStore: mem})
	t0 := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	at(lane, t0).pass(context.Background())
	before, _ := recordedLeg(lane.recording(), mem)
	if err := mem.Close(blocker.ID); err != nil {
		t.Fatal(err)
	}
	at(lane, t0.Add(backstopTestInterval)).pass(context.Background())
	after, _ := recordedLeg(lane.recording(), mem)
	if !slices.Equal(ids(before.RawOpen), ids(after.RawOpen)) || slices.Contains(ids(before.ReadyAll), dep.ID) || !slices.Contains(ids(after.ReadyAll), dep.ID) {
		t.Fatalf("fixture: open %v -> %v, ready %v -> %v; want the open rows unchanged and %s newly ready",
			ids(before.RawOpen), ids(after.RawOpen), ids(before.ReadyAll), ids(after.ReadyAll), dep.ID)
	}
	if got := wakes.Load(); got != 2 {
		t.Errorf("Ready-only change: wakes = %d, want 2", got)
	}
}

// Kills: a content comparison that ignores errors, a source's payload or the
// closed index (M12, M12b), or that counts the sequence or the read times as
// content.
func TestBackstopRecordingSameContentComparesRowsErrorsAndIndexes(t *testing.T) {
	store := beads.NewMemStore()
	idx, _ := session.BuildClosedNamedSessionBeadIndex(beads.NewMemStoreFrom(0, []beads.Bead{closedNamedSessionBead("gc-c", "mayor")}, nil))
	demand, sessions, closed, scale := keyOf(sourceDemand, store), keyOf(sourceSessions, store), keyOf(sourceClosedNamed, store), sourceKey{kind: sourceScaleCheck}
	base := func() *externalReadsRecording {
		return &externalReadsRecording{Sources: map[sourceKey]sourceResult{
			demand:   {sourcePayload: sourcePayload{Leg: legRecording{RawOpen: []beads.Bead{{ID: "gc-1"}}}}},
			sessions: {sourcePayload: sourcePayload{Sessions: []session.Info{{ID: "gc-s"}}}},
			closed:   {},
			scale:    {sourcePayload: sourcePayload{ScaleCheck: &scaleCheckResult{Counts: map[string]int{"w": 1}}}},
		}}
	}
	boom := errors.New("boom")
	set := func(key sourceKey, s sourceResult) func(*externalReadsRecording) {
		return func(r *externalReadsRecording) { r.Sources[key] = s }
	}
	for _, tc := range []struct {
		name string
		edit func(*externalReadsRecording)
		same bool
	}{
		{"identical", func(*externalReadsRecording) {}, true},
		{"sequence and read times", func(r *externalReadsRecording) {
			r.Seq, r.FreshFor = 9, time.Hour
			s := r.Sources[demand]
			s.StartedAt, s.EndedAt = time.Now(), time.Now()
			r.Sources[demand] = s
		}, true},
		{"open rows", set(demand, sourceResult{sourcePayload: sourcePayload{Leg: legRecording{}}}), false},
		{"open error", set(demand, sourceResult{sourcePayload: sourcePayload{Leg: legRecording{RawOpen: []beads.Bead{{ID: "gc-1"}}, RawOpenErr: boom}}}), false},
		{"ready error", set(demand, sourceResult{sourcePayload: sourcePayload{Leg: legRecording{RawOpen: []beads.Bead{{ID: "gc-1"}}, ReadyAllErr: boom}}}), false},
		{"source timeout", set(demand, sourceResult{Err: errSourceTimeout}), false},
		{"session error", set(sessions, sourceResult{sourcePayload: sourcePayload{Sessions: []session.Info{{ID: "gc-s"}}}, Err: boom}), false},
		{"closed index", set(closed, sourceResult{sourcePayload: sourcePayload{ClosedNamed: idx}}), false},
		{"closed index error", set(closed, sourceResult{Err: boom}), false},
		{"closed index dropped", func(r *externalReadsRecording) { delete(r.Sources, closed) }, false},
		{"scale_check count", set(scale, sourceResult{sourcePayload: sourcePayload{ScaleCheck: &scaleCheckResult{Counts: map[string]int{"w": 2}}}}), false},
	} {
		r := base()
		tc.edit(r)
		if got := base().sameContent(r); got != tc.same {
			t.Errorf("%s: sameContent = %t, want %t", tc.name, got, tc.same)
		}
	}
}

// Kills: a race between the lane's passes (their source goroutines and the
// repairs step), and allocator passes reading the recording, the cache and
// the last good answers, while an out-of-process writer adds rows (run under
// -race). The
// env is rich: rigs, an on_demand named session (the closed index), a scope
// gap, probe stores, a session snapshot, and rows every repair writes,
// re-clobbered between runs so the repairs keep writing; the allocator side
// runs every collector, the control-dispatcher projection and the assignees.
func TestBackstopLaneConcurrentWithAllocatorPasses(t *testing.T) {
	const rounds = 30
	cityPath := t.TempDir()
	cfg := classBindingDispatcherFixtureConfig(t)
	cfg.Workspace.Prefix = "ga"
	cfg.Rigs = append(cfg.Rigs, config.Rig{Name: "nodisp", Path: t.TempDir()})
	cfg.Agents = append(cfg.Agents, poolAgent("planner", "rig-A", intPtr(5), 0))
	cfg.NamedSessions = []config.NamedSession{{Name: "mayor", Template: "planner", Dir: "rig-A", Mode: "on_demand"}}
	cityRoute := cfg.Agents[0].QualifiedName()
	control := func(id, rig string) beads.Bead {
		return beads.Bead{ID: id, Title: id, Type: "task", Status: "open", Metadata: map[string]string{
			beadmeta.KindMetadataKey:         beadmeta.KindWorkflowFinalize,
			beadmeta.RoutedToMetadataKey:     cityRoute,
			beadmeta.RootStoreRefMetadataKey: "rig:" + rig,
		}}
	}
	routedClobbered := workBead("ga-rclob", goldenCanonicalPlanner, "", "open", 5)
	routedClobbered.Metadata[beadmeta.WorkDirMetadataKey] = clobberPoolSlot
	routedClobbered.Metadata[beadmeta.LegacyWorkDirMetadataKey] = clobberStaleLegacy
	cityCache, cityBacking := newDemandCache(t, false,
		beads.Bead{ID: "ga-run", Type: "task", Status: "in_progress", Assignee: clobberSessionName, Metadata: clobberedWorkDir()},
		workBead("ga-asg", goldenLegacyPlanner, goldenLegacyPlanner, "in_progress", 5),
		routedClobbered,
		workBead("ga-rlegacy", goldenLegacyPlanner, "", "open", 5),
		workBead("ga-slot", goldenCanonicalPlanner+"-2", "", "open", 5),
		backstopSessionBead("gc-s1", "worker-1"),
		closedNamedSessionBead("gc-closed", "rig-A/mayor"),
	)
	rigFixture := beads.NewMemStoreFrom(0, []beads.Bead{control("fx-ctl", "fixture")}, nil)
	rigs := map[string]beads.Store{
		"fixture": rigFixture,
		"nodisp":  beads.NewMemStoreFrom(0, []beads.Bead{control("nd-ctl", "nodisp")}, nil),
	}
	probe := beads.NewMemStoreFrom(0, []beads.Bead{routedDemandBead("gc-p1")}, nil)
	sessions := newSessionBeadSnapshot([]beads.Bead{stampTestSession(clobberSessionName, clobberLiveWorkDir)})
	front := wrapStoreWithBeadPolicies(cityCache, cfg)
	suspended := map[string]bool{}
	var wakes atomic.Int64
	gapEvents := events.NewFake()
	lane := newExternalReadsLane(backstopTestInterval, func() (externalReadsEnv, error) {
		return externalReadsEnv{CityPath: cityPath, Cfg: cfg, CityStore: front, RigStores: rigs, SuspendedRigPaths: suspended, ProbeStores: []beads.Store{probe, rigFixture}, Sessions: sessions}, nil
	}, func() { wakes.Add(1) }, func(fn func(), _ string) bool { fn(); return false }, gapEvents, io.Discard)
	ctx := context.Background()
	var wg sync.WaitGroup
	wg.Go(func() {
		for range rounds {
			lane.pass(ctx)
		}
		_ = lane.join(ctx)
	})
	wg.Go(func() {
		for range rounds / 3 {
			_ = cityBacking.SetMetadataBatch("ga-run", clobberedWorkDir())
		}
	})
	wg.Go(func() {
		for range rounds {
			_, _ = cityBacking.Create(routedDemandBead(""))
		}
	})
	for range 4 {
		wg.Go(func() {
			for range rounds {
				rec := lane.recording()
				reads := newV2DemandReads(time.Now(), rec)
				_ = runDemandCollectors(cfg, front, func() *readyDemandCache { return newReadyDemandCacheWithReads(reads) }, reads)
				rows, _, refs, _ := collectOpenUnassignedRoutedWork(cityPath, cfg, front, rigs, suspended, io.Discard, nil, reads)
				projected, _ := projectControlDispatcherRoutes(cfg, rows, refs)
				_ = openControlDispatcherDemand(cfg, projected)
				_ = readyAssignedWorkAssignees(cfg, front, sessions, nil, nil, "", reads)
				_, _ = rec.sessionLeg(front)
				_ = rec.scaleCheck(time.Now())
			}
		})
	}
	wg.Wait()
	passAndJoin(ctx, lane)
	rec := lane.recording()
	leg, _ := recordedLeg(rec, front)
	if rec.Seq != rounds+1 || wakes.Load() < 1 || len(leg.RawOpen) < rounds {
		t.Errorf("lane seq=%d wakes=%d open rows=%d, want %d passes, a wake, and every written row", rec.Seq, wakes.Load(), len(leg.RawOpen), rounds+1)
	}
	if _, ok := recordedClosedNamed(rec, front); !ok || scopeGapEvents(t, gapEvents) != 1 {
		t.Errorf("closed index recorded=%t gap events=%d, want recorded and one repair run's nodisp gap", ok, scopeGapEvents(t, gapEvents))
	}
}

// Kills: the default-probe target stores left unrecorded (M22). A target
// store outside the census plan (here, a store no rig or binding names) is
// recorded with the census legs.
func TestBackstopLaneRecordsDefaultProbeStoresOutsideCensus(t *testing.T) {
	probe := beads.NewMemStoreFrom(0, []beads.Bead{routedDemandBead("gc-p1")}, nil)
	lane, _ := newTestBackstopLane(externalReadsEnv{Cfg: demandReadsTestConfig(), CityStore: beads.NewMemStore(), ProbeStores: []beads.Store{probe}})
	at(lane, time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)).pass(context.Background())
	if leg, ok := recordedLeg(lane.recording(), probe); !ok || !slices.Equal(ids(leg.RawOpen), []string{"gc-p1"}) {
		t.Errorf("probe store recorded=%t open=%v, want gc-p1", ok, ids(leg.RawOpen))
	}
}

// cancelOnWriteStore cancels a context on its first metadata write.
type cancelOnWriteStore struct {
	beads.Store
	cancel context.CancelFunc
}

func (s cancelOnWriteStore) SetMetadataBatch(id string, kvs map[string]string) error {
	s.cancel()
	return s.Store.SetMetadataBatch(id, kvs)
}

// Kills: repairs running on after shutdown or the step deadline ended their
// context mid-run (N30). It ends during the assigned half; the routed half
// writes nothing.
func TestBackstopLaneRepairStopsBetweenHalvesOnShutdown(t *testing.T) {
	cfg := gaConfig()
	cfg.Agents = []config.Agent{poolAgent("planner", "rig-A", intPtr(5), 0)}
	ctx, cancel := context.WithCancel(context.Background())
	mem := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "ga-run", Type: "task", Status: "in_progress", Assignee: clobberSessionName, Metadata: clobberedWorkDir()},
		workBead("ga-slot", "rig-A/planner-2", "", "open", 5),
	}, nil)
	runBackstopDemandRepairs(ctx, externalReadsEnv{Cfg: cfg, CityStore: cancelOnWriteStore{Store: mem, cancel: cancel}, Sessions: newSessionBeadSnapshot([]beads.Bead{stampTestSession(clobberSessionName, clobberLiveWorkDir)})}, io.Discard)
	if ctx.Err() == nil {
		t.Fatal("fixture: the assigned half wrote nothing, so shutdown never began")
	}
	if b, _ := mem.Get("ga-slot"); b.Metadata[beadmeta.RoutedToMetadataKey] != "rig-A/planner-2" {
		t.Errorf("the routed half ran after shutdown: ga-slot route = %q", b.Metadata[beadmeta.RoutedToMetadataKey])
	}
}
