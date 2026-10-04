package main

import (
	"context"
	"errors"
	"fmt"
	"io"
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
	"github.com/gastownhall/gascity/internal/session"
)

// The lane's pacing tests run in a testing/synctest bubble: the paced lane's
// timers and the test's waits share the bubble clock, and synctest.Wait
// returns once the lane goroutines are blocked again. The pass tests call
// pass and repair directly with a scripted clock.

const backstopTestInterval = 15 * time.Second

// newTestBackstopLane returns a lane over env and a count of the allocator
// wakes it asks for.
func newTestBackstopLane(env backstopEnv) (*backstopLane, *atomic.Int64) {
	wakes := &atomic.Int64{}
	lane := newBackstopLane(
		backstopTestInterval,
		func() (backstopEnv, error) { return env, nil },
		func() { wakes.Add(1) },
		func(fn func(), _ string) bool { fn(); return false },
		io.Discard,
	)
	return lane, wakes
}

// at pins the lane's clock to t.
func at(lane *backstopLane, t time.Time) *backstopLane {
	lane.now = func() time.Time { return t }
	return lane
}

// startBackstopLaneInBubble starts the lane in the current bubble and stops
// it when the bubble's test returns.
func startBackstopLaneInBubble(t *testing.T, lane *backstopLane) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := lane.start(ctx)
	t.Cleanup(func() {
		cancel()
		<-done
	})
	synctest.Wait()
}

// advanceBackstop moves the bubble clock forward by d and lets the lane settle.
func advanceBackstop(d time.Duration) {
	<-time.After(d)
	synctest.Wait()
}

func backstopSeq(lane *backstopLane) uint64 {
	if rec := lane.recording(); rec != nil {
		return rec.Seq
	}
	return 0
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
		lane, wakes := newTestBackstopLane(backstopEnv{Cfg: demandReadsTestConfig(), CityStore: store})
		start := time.Now()
		startBackstopLaneInBubble(t, lane)

		check := func(when string, seq uint64, wantWakes int64, rows int) {
			t.Helper()
			rec := lane.recording()
			leg, ok := rec.leg(store)
			if rec == nil || rec.Seq != seq || wakes.Load() != wantWakes || !ok || len(leg.RawOpen) != rows {
				t.Fatalf("%s: seq=%d wakes=%d leg recorded=%t open rows=%d, want seq=%d wakes=%d rows=%d",
					when, backstopSeq(lane), wakes.Load(), ok, len(leg.RawOpen), seq, wantWakes, rows)
			}
		}
		check("first pass", 1, 1, 1)

		advanceBackstop(backstopTestInterval)
		check("unchanged pass", 2, 1, 1)
		if at := lane.recording().At; !at.Equal(start.Add(backstopTestInterval)) {
			t.Errorf("unchanged pass recorded at %v, want %v: an unchanged recording must still be republished fresh", at, start.Add(backstopTestInterval))
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
		lane, _ := newTestBackstopLane(backstopEnv{Cfg: demandReadsTestConfig(), CityStore: beads.NewMemStore()})
		startBackstopLaneInBubble(t, lane)
		if got := backstopSeq(lane); got != 1 {
			t.Fatalf("after start: %d passes, want the immediate first pass", got)
		}

		advanceBackstop(time.Second)
		lane.wake()
		synctest.Wait()
		if got := backstopSeq(lane); got != 1 {
			t.Fatalf("poke 1s after a pass: %d passes, want 1 until the %v gap closes", got, backstopLaneMinGap)
		}
		advanceBackstop(backstopLaneMinGap - time.Second - time.Millisecond)
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

// Kills: repairs on every recording pass, repairs paced by anything but
// their own minute, and the stamp running before the work-dir repair
// (POOL-019). The repairs run at once on start; re-clobbered, the run stays
// clobbered through the recording passes of the next minute and is repaired
// once the minute is up.
func TestBackstopLaneRepairsAtMostOncePerMinuteInLegacyOrder(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newClobberedRunFixture()
		lane, _ := newTestBackstopLane(backstopEnv{Cfg: gaConfig(), CityStore: f.mem, Sessions: f.sessions})
		startBackstopLaneInBubble(t, lane)
		if got := f.workDir(t); got != clobberLiveWorkDir {
			t.Fatalf("after start gc.work_dir = %q, want %q: the repairs must run, the stamp after the work-dir repair", got, clobberLiveWorkDir)
		}
		if err := f.mem.SetMetadataBatch("ga-run", clobberedWorkDir()); err != nil {
			t.Fatalf("re-clobber: %v", err)
		}
		for _, step := range []time.Duration{backstopTestInterval, backstopTestInterval, backstopRepairInterval - 2*backstopTestInterval - time.Millisecond} {
			advanceBackstop(step)
			if got := f.workDir(t); got != clobberPoolSlot {
				t.Fatalf("seq %d: gc.work_dir = %q, want it left at %q: repairs ran again within a minute", backstopSeq(lane), got, clobberPoolSlot)
			}
		}
		if got := backstopSeq(lane); got < 4 {
			t.Fatalf("recording passes within the minute = %d, want the patrol's", got)
		}
		advanceBackstop(time.Millisecond)
		if got := f.workDir(t); got != clobberLiveWorkDir {
			t.Fatalf("a minute later: gc.work_dir = %q, want %q", got, clobberLiveWorkDir)
		}
	})
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
	env      backstopEnv
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
		env: backstopEnv{
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
// to the legacy work dir. Scope gaps are published with the next recording.
func TestBackstopDemandRepairsWriteLogGoldenInLegacyOrder(t *testing.T) {
	f := newRepairGoldenFixture(t)
	lane, _ := newTestBackstopLane(f.env)

	if !lane.repair(context.Background()) {
		t.Fatal("repair declined")
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

	if !at(lane, time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)).pass(context.Background()) {
		t.Fatal("pass declined")
	}
	gaps := lane.recording().ScopeGaps
	if len(gaps) != 1 || gaps[0].RigContext != "nodisp" || gaps[0].SuppressedCount != 1 || gaps[0].SampleBeadID != "nd-ctl" {
		t.Errorf("recording scope gaps = %+v, want one for rig nodisp with nd-ctl", gaps)
	}
}

// Kills: scope gaps carried only by the first recording after a repair run
// (R38), and a repair-run number that moves without a new run, so a consumer
// would emit one run's gap events again (or never). Every recording carries
// the latest run's gaps under that run's RepairSeq; only a repair run that
// ran advances it.
func TestBackstopRecordingCarriesRepairRunGapsAndSeq(t *testing.T) {
	f := newRepairGoldenFixture(t)
	lane, _ := newTestBackstopLane(f.env)
	ctx := context.Background()
	clock := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	pass := func() *backstopRecording {
		t.Helper()
		clock = clock.Add(backstopTestInterval)
		if !at(lane, clock).pass(ctx) {
			t.Fatal("pass declined")
		}
		return lane.recording()
	}
	check := func(when string, rec *backstopRecording, seq uint64, gaps int) {
		t.Helper()
		if rec.RepairSeq != seq || len(rec.ScopeGaps) != gaps {
			t.Fatalf("%s: RepairSeq=%d gaps=%+v, want RepairSeq=%d with %d gap(s)", when, rec.RepairSeq, rec.ScopeGaps, seq, gaps)
		}
	}

	check("before any repair run", pass(), 0, 0)
	if !lane.repair(ctx) {
		t.Fatal("repair declined")
	}
	for i := range 3 {
		check(fmt.Sprintf("recording %d after the first run", i+1), pass(), 1, 1)
	}

	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if lane.repair(canceled) {
		t.Fatal("repair ran after shutdown")
	}
	check("after a declined run", pass(), 1, 1)

	if !lane.repair(ctx) {
		t.Fatal("second repair declined")
	}
	check("after the second run", pass(), 2, 1)
	check("next recording", pass(), 2, 1)
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
// replaces the last recording, and a lane that stays dark after resume. Each
// way of suspending: Workspace.Suspended, the suspension file, GC_SUSPENDED=1.
func TestBackstopLaneSkipsReadsAndRepairsWhileCitySuspended(t *testing.T) {
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
			lane, wakes := newTestBackstopLane(backstopEnv{CityPath: cityPath, Cfg: cfg, CityStore: f.mem, RigStores: nil, ProbeStores: []beads.Store{cache}, Sessions: f.sessions})
			t0 := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
			if !at(lane, t0).pass(ctx) {
				t.Fatal("pass before suspending declined")
			}
			last := lane.recording()

			tc.suspend(t, cfg, cityPath)
			backing.armed.Store(true)
			if at(lane, t0.Add(backstopTestInterval)).pass(ctx) || lane.repair(ctx) {
				t.Fatal("a pass or a repair ran while the city is suspended")
			}
			if lane.recording() != last || wakes.Load() != 1 {
				t.Errorf("suspended pass: recording replaced=%t wakes=%d, want the last recording kept and no wake", lane.recording() != last, wakes.Load())
			}
			if got := f.workDir(t); got != clobberPoolSlot {
				t.Errorf("suspended repair wrote gc.work_dir = %q", got)
			}
			if ops := backing.readLog(); len(ops) > 0 {
				t.Errorf("suspended pass read a leg: %q", ops)
			}

			tc.resume(t, cfg, cityPath)
			if !at(lane, t0.Add(2*backstopTestInterval)).pass(ctx) || !lane.repair(ctx) {
				t.Fatal("pass or repair after resume declined")
			}
			if got := backstopSeq(lane); got != 2 {
				t.Errorf("after resume: seq %d, want 2", got)
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
// A pass and a repair under a canceled context do nothing; a pass whose reads
// see shutdown begin publishes nothing.
func TestBackstopLaneNoPassOrRepairAfterShutdown(t *testing.T) {
	f := newClobberedRunFixture()
	lane, wakes := newTestBackstopLane(backstopEnv{Cfg: gaConfig(), CityStore: f.mem, Sessions: f.sessions})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if lane.pass(ctx) || lane.repair(ctx) {
		t.Fatal("a pass or a repair ran after shutdown")
	}
	if lane.recording() != nil || wakes.Load() != 0 || f.workDir(t) != clobberPoolSlot {
		t.Errorf("after shutdown: recording=%v wakes=%d work dir=%q, want nothing", lane.recording(), wakes.Load(), f.workDir(t))
	}

	ctx, cancel = context.WithCancel(context.Background())
	lane, wakes = newTestBackstopLane(backstopEnv{Cfg: demandReadsTestConfig(), CityStore: &cancelOnListStore{Store: beads.NewMemStore(), cancel: cancel}})
	if lane.pass(ctx) {
		t.Error("a pass overtaken by shutdown reported that it ran")
	}
	if ctx.Err() == nil || lane.recording() != nil || wakes.Load() != 0 {
		t.Errorf("pass overtaken by shutdown: canceled=%t recording=%v wakes=%d, want no publish and no wake", ctx.Err() != nil, lane.recording(), wakes.Load())
	}
}

// Kills: repairs over a city with no city store (R29). Legacy's demand pass
// runs its repairs only under a store; the lane declines the run and the
// repair-run number stays put.
func TestBackstopLaneNoRepairWithoutCityStore(t *testing.T) {
	f := newClobberedRunFixture()
	lane, _ := newTestBackstopLane(backstopEnv{Cfg: gaConfig(), RigStores: map[string]beads.Store{"rig": f.mem}, Sessions: f.sessions})
	if lane.repair(context.Background()) {
		t.Error("repair ran with no city store")
	}
	if !at(lane, time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)).pass(context.Background()) {
		t.Fatal("pass declined")
	}
	if got := lane.recording().RepairSeq; got != 0 {
		t.Errorf("RepairSeq = %d after a declined run, want 0", got)
	}
}

// Kills: a declined pass counted toward the lane's pacing (R1). After a pass
// skipped while the city was suspended, the resume's wake runs a pass at once
// instead of waiting out a duty cycle the skipped pass never used.
func TestBackstopLaneSkippedPassDoesNotPaceTheNextWake(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		lane, _ := newTestBackstopLane(backstopEnv{CityPath: t.TempDir(), Cfg: demandReadsTestConfig(), CityStore: beads.NewMemStore()})
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
		advanceBackstop(backstopLaneMinGap / 2)
		t.Setenv("GC_SUSPENDED", "")
		lane.wake()
		synctest.Wait()
		if got := backstopSeq(lane); got != 3 {
			t.Errorf("resume wake %v after a skipped pass: %d passes, want one at once", backstopLaneMinGap/2, got)
		}
	})
}

// slowLiveStore makes every live List and every Ready take d of bubble time.
type slowLiveStore struct {
	beads.Store
	d time.Duration
}

func (s slowLiveStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	if q.Live {
		<-time.After(s.d)
	}
	return s.Store.List(q)
}

func (s slowLiveStore) Ready(q ...beads.ReadyQuery) ([]beads.Bead, error) {
	<-time.After(s.d)
	return s.Store.Ready(q...)
}

// Kills: a recording stamped when its reads began, a fixed freshness bound
// slow reads outrun, and repairs on the recording's cadence. At bd-like read
// latencies (7.4s reads make a pass of about 15s) the recording, once
// published, is fresh at every sample over ten minutes.
func TestBackstopLaneRecordingNeverStaleInSteadyState(t *testing.T) {
	for _, d := range []time.Duration{2 * time.Second, 3600 * time.Millisecond, 7400 * time.Millisecond, 12 * time.Second} {
		t.Run(d.String(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				store := slowLiveStore{Store: beads.NewMemStore(), d: d}
				if _, err := store.Create(routedDemandBead("")); err != nil {
					t.Fatal(err)
				}
				lane, _ := newTestBackstopLane(backstopEnv{Cfg: demandReadsTestConfig(), CityStore: store, Sessions: newClobberedRunFixture().sessions})
				startBackstopLaneInBubble(t, lane)
				var samples, stale int
				var maxAge time.Duration
				for end := time.Now().Add(10 * time.Minute); time.Now().Before(end); {
					<-time.After(250 * time.Millisecond)
					rec := lane.recording()
					if rec == nil {
						continue
					}
					samples++
					maxAge = max(maxAge, time.Since(rec.At))
					if !rec.fresh(time.Now()) {
						stale++
					}
				}
				if samples == 0 || stale > 0 {
					t.Errorf("stale at %d of %d samples (max age %v)", stale, samples, maxAge)
				}
				if d >= 7400*time.Millisecond && maxAge <= backstopTestInterval {
					t.Errorf("max recording age %v: the reads were not slow enough to test the bound", maxAge)
				}
			})
		})
	}
}

// hangableStore delays every live List by delay; once hang is set, a live
// List blocks until release is closed.
type hangableStore struct {
	beads.Store
	delay   *atomic.Int64
	hang    *atomic.Bool
	release chan struct{}
}

func (s hangableStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	if q.Live {
		if s.hang.Load() {
			<-s.release
		}
		<-time.After(time.Duration(s.delay.Load()))
	}
	return s.Store.List(q)
}

// Kills: an expiry whose pass-time term is uncapped, so one slow pass keeps
// a dead lane's recordings fresh for that pass's length (finding 2 of the
// P3-2 re-review). After one 5-minute pass the lane hangs for good; its last
// recording goes stale 2 intervals plus cacheLagBound after its reads ended,
// and the allocator then reads the leg partial.
func TestBackstopLaneSlowPassThenHangGoesStaleByCappedBound(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		delay := &atomic.Int64{}
		delay.Store(int64(time.Second))
		hang := &atomic.Bool{}
		release := make(chan struct{})
		store := hangableStore{Store: beads.NewMemStore(), delay: delay, hang: hang, release: release}
		lane, _ := newTestBackstopLane(backstopEnv{Cfg: demandReadsTestConfig(), CityStore: store})
		startBackstopLaneInBubble(t, lane)
		defer func() {
			delay.Store(int64(time.Second))
			close(release)
		}()
		for backstopSeq(lane) < 3 {
			advanceBackstop(time.Second)
		}
		steady := backstopSeq(lane)
		delay.Store(int64(5 * time.Minute))
		for backstopSeq(lane) == steady {
			advanceBackstop(time.Second)
		}
		// The next pass starts an interval after the slow one ended; it
		// hangs.
		hang.Store(true)
		slow := lane.recording()
		if want := slow.At.Add(2*backstopTestInterval + cacheLagBound); !slow.Expires.Equal(want) {
			t.Fatalf("after a 5m pass: Expires = At + %v, want At + %v", slow.Expires.Sub(slow.At), want.Sub(slow.At))
		}
		<-time.After(time.Until(slow.Expires))
		synctest.Wait()
		if lane.recording() != slow || !slow.fresh(time.Now()) {
			t.Fatalf("at Expires: recording replaced=%t fresh=%t, want the slow pass's, still fresh", lane.recording() != slow, slow.fresh(time.Now()))
		}
		advanceBackstop(time.Nanosecond)
		if lane.recording() != slow || slow.fresh(time.Now()) {
			t.Fatalf("past Expires: recording replaced=%t fresh=%t, want the slow pass's, stale", lane.recording() != slow, slow.fresh(time.Now()))
		}
		if _, err := newV2DemandReads(time.Now(), lane.recording(), nil).RawOpen(store); !errors.Is(err, errDemandRecordingStale) {
			t.Errorf("v2 RawOpen past the capped expiry: err = %v, want the stale recording", err)
		}
	})
}

// Kills: At stamped at the start of the reads, an expiry not derived from
// the cadence, and a window that keeps one slow pass forever. A 7s pass
// publishes At at the end and Expires two intervals plus 7s later; shorter
// passes keep the 7s in the bound until it leaves the window.
func TestBackstopLaneStampsAtWhenReadsEndWithDerivedExpiry(t *testing.T) {
	lane, _ := newTestBackstopLane(backstopEnv{Cfg: demandReadsTestConfig(), CityStore: beads.NewMemStore()})
	clock := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	passFor := func(d time.Duration) *backstopRecording {
		t.Helper()
		calls := 0
		lane.now = func() time.Time {
			calls++
			if calls == 1 {
				return clock
			}
			return clock.Add(d)
		}
		if !lane.pass(context.Background()) {
			t.Fatal("pass declined")
		}
		clock = clock.Add(d + backstopTestInterval)
		return lane.recording()
	}
	start := clock
	rec := passFor(7 * time.Second)
	if want := start.Add(7 * time.Second); !rec.At.Equal(want) {
		t.Errorf("At = %v, want the end of the reads %v", rec.At, want)
	}
	if want := rec.At.Add(2*backstopTestInterval + 7*time.Second); !rec.Expires.Equal(want) {
		t.Errorf("Expires = %v, want %v", rec.Expires, want)
	}
	for i := 1; i < backstopPassWindow; i++ {
		rec = passFor(time.Second)
		if want := rec.At.Add(2*backstopTestInterval + 7*time.Second); !rec.Expires.Equal(want) {
			t.Fatalf("pass %d: Expires = %v, want the window's slow pass still in the bound (%v)", i, rec.Expires, want)
		}
	}
	rec = passFor(time.Second)
	if want := rec.At.Add(2*backstopTestInterval + time.Second); !rec.Expires.Equal(want) {
		t.Errorf("after the window: Expires = %v, want %v", rec.Expires, want)
	}
}

// Kills: no allocator wake when a fresh recording replaces an expired one
// with the same content: the allocator read the legs partial and would wait
// for a content change to read them again.
func TestBackstopLaneWakesOnStaleToFreshPublish(t *testing.T) {
	lane, wakes := newTestBackstopLane(backstopEnv{Cfg: demandReadsTestConfig(), CityStore: beads.NewMemStore()})
	t0 := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	at(lane, t0).pass(context.Background())
	at(lane, t0.Add(backstopTestInterval)).pass(context.Background())
	if got := wakes.Load(); got != 1 {
		t.Fatalf("fresh unchanged pass: wakes = %d, want 1", got)
	}
	expired := lane.recording().Expires.Add(time.Nanosecond)
	at(lane, expired).pass(context.Background())
	if got := wakes.Load(); got != 2 {
		t.Errorf("unchanged pass replacing an expired recording: wakes = %d, want 2", got)
	}
}

// Kills: the stale-to-fresh wake judged when the reads began (R6). The
// previous recording is fresh when the pass starts and expired when its
// reads end; the allocator read the legs partial in between, so the
// unchanged recording must still wake it.
func TestBackstopLaneJudgesStaleToFreshWakeAtEndOfReads(t *testing.T) {
	lane, wakes := newTestBackstopLane(backstopEnv{Cfg: demandReadsTestConfig(), CityStore: beads.NewMemStore()})
	at(lane, time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)).pass(context.Background())
	expires := lane.recording().Expires
	calls := 0
	lane.now = func() time.Time {
		calls++
		if calls == 1 {
			return expires.Add(-time.Second)
		}
		return expires.Add(time.Second)
	}
	if !lane.pass(context.Background()) {
		t.Fatal("pass declined")
	}
	if got := wakes.Load(); got != 2 {
		t.Errorf("recording expired during the reads: wakes = %d, want 2", got)
	}
}

// Kills: a session census that misses out-of-process writes on a bd leg. A
// session row created, and one rewritten, behind the leg's cache (no event)
// must reach the recording on the next pass, not wait for the cache's
// re-scan; and the change must wake the allocator.
func TestBackstopLaneRecordsOutOfProcessSessionWriteOnNonExactLeg(t *testing.T) {
	cache, backing := newDemandCache(t, false, backstopSessionBead("gc-s1", "worker-1"))
	lane, wakes := newTestBackstopLane(backstopEnv{Cfg: demandReadsTestConfig(), CityStore: cache})
	t0 := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)

	at(lane, t0).pass(context.Background())
	first := lane.recording()
	leg, ok := first.sessionLeg(cache)
	if !ok || leg.Err != nil || !leg.At.Equal(t0) || !leg.Expires.Equal(first.Expires) || !slices.Equal(sessionInfoIDs(leg.Rows), []string{"gc-s1"}) {
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
	sessionsOnly := *lane.recording()
	sessionsOnly.Legs = first.Legs
	if first.sameContent(&sessionsOnly) {
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
	lane, _ := newTestBackstopLane(backstopEnv{Cfg: demandReadsTestConfig(), CityStore: cache})
	at(lane, time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)).pass(context.Background())
	leg, ok := lane.recording().leg(cache)
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
	lane, _ := newTestBackstopLane(backstopEnv{Cfg: cfg, CityStore: front})
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	at(lane, now).pass(context.Background())
	rec := lane.recording()
	for name, store := range map[string]beads.Store{"front door": front, "cache": cache} {
		if _, ok := rec.leg(store); !ok {
			t.Errorf("%s: demand leg not found", name)
		}
		if _, ok := rec.sessionLeg(store); !ok {
			t.Errorf("%s: session leg not found", name)
		}
	}
	reads := newV2DemandReads(now, rec, newDemandLastGood())
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
	lane, _ := newTestBackstopLane(backstopEnv{Cfg: cfg, CityStore: front})
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	at(lane, now).pass(context.Background())
	for name, store := range map[string]beads.Store{"front door": front, "cache": cache} {
		idx, err := newV2DemandReads(now, lane.recording(), nil).ClosedNamedIndex(store)
		if _, found := idx.Find("mayor"); err != nil || !found {
			t.Errorf("%s: v2 closed index found mayor=%t err=%v, want the recorded index", name, found, err)
		}
	}
}

// Kills: a census feed whose recorded leg does not match the census's leg
// keys (backstopRecording.sessionLeg is the feed's recorded function, so
// both key by the store behind the policy front door), and a census that
// ages the lane's leg on anything but the recording's Expires. The census
// reads the lane's recording through the front door and the bare cache.
func TestCensusReadsBackstopSessionLegThroughPolicyFrontDoor(t *testing.T) {
	cfg := demandReadsTestConfig()
	cache, _ := newDemandCache(t, false, backstopSessionBead("gc-s1", "worker-1"))
	front := wrapStoreWithBeadPolicies(cache, cfg)
	if front == beads.Store(cache) {
		t.Fatal("the policy front door did not wrap the cache; the test needs distinct stores")
	}
	lane, _ := newTestBackstopLane(backstopEnv{Cfg: cfg, CityStore: front})
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	at(lane, now).pass(context.Background())
	rec := lane.recording()
	feed := censusLegFeed{
		exact:    func(s beads.Store) bool { _, exact := demandLegCache(s); return exact },
		recorded: rec.sessionLeg,
	}
	for name, store := range map[string]beads.Store{"front door": front, "cache": cache} {
		r := newCensusReader(feed)
		legs := []classStoreCandidate{{store: store, ref: "class:sessions"}}
		c, err := r.read(rec.Expires, cfg, legs)
		if err != nil {
			t.Fatalf("%s: census read: %v", name, err)
		}
		if leg := c.Legs[0]; leg.State != legRead || !leg.ReadAt.Equal(rec.At) {
			t.Errorf("%s: leg %+v, want read at the recording's At", name, leg)
		}
		if _, ok := c.Rows[rowKey{Leg: "class:sessions", ID: "gc-s1"}]; !ok {
			t.Errorf("%s: recorded session row missing from the census", name)
		}
		if c, err = r.read(rec.Expires.Add(time.Nanosecond), cfg, legs); err != nil || c.Legs[0].State != legStale {
			t.Errorf("%s: past the recording's Expires: err=%v leg=%+v, want stale", name, err, c.Legs)
		}
	}
}

// Kills: one panicking leg read killing the process from the lane's
// goroutine (mc-zndi7.40), and a nil allocator wake. The leg is recorded with
// the panic as its error and the pass completes.
func TestBackstopLaneRecordsPanickingLegAsError(t *testing.T) {
	store := panickingListStore{beads.NewMemStore()}
	lane := newBackstopLane(backstopTestInterval, func() (backstopEnv, error) {
		return backstopEnv{Cfg: demandReadsTestConfig(), CityStore: store}, nil
	}, nil, func(fn func(), _ string) bool { fn(); return false }, io.Discard)
	if !at(lane, time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)).pass(context.Background()) {
		t.Fatal("pass declined")
	}
	leg, ok := lane.recording().leg(store)
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
		lane, _ := newTestBackstopLane(backstopEnv{Cfg: demandReadsTestConfig(), CityStore: cache})
		backing.armed.Store(true)

		at(lane, t0).pass(context.Background())

		rec := lane.recording()
		_, sessionRecorded := rec.sessionLeg(cache)
		_, demandRecorded := rec.leg(cache)
		_, closedRecorded := rec.closedNamed(cache)
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
		lane, wakes := newTestBackstopLane(backstopEnv{Cfg: cfg, CityStore: cache})
		at(lane, now).pass(context.Background())
		rec := lane.recording()
		if l, ok := rec.closedNamed(cache); !ok || l.Err != nil {
			t.Fatalf("exact=%t: closed index recorded=%t err=%v, want recorded", exact, ok, l.Err)
		}

		legacy := readyAssignedWorkAssignees(cfg, cache, nil, nil, nil, "", nil)
		backing.armed.Store(true)
		v2 := readyAssignedWorkAssignees(cfg, cache, nil, nil, nil, "", newV2DemandReads(now, rec, nil))
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
	lane, _ := newTestBackstopLane(backstopEnv{Cfg: demandReadsTestConfig(), CityStore: cache})
	backing.armed.Store(true)
	at(lane, now).pass(context.Background())
	if _, ok := lane.recording().closedNamed(cache); ok || len(backing.readLog()) > 0 {
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
	lane, wakes := newTestBackstopLane(backstopEnv{Cfg: demandReadsTestConfig(), CityStore: mem})
	t0 := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	at(lane, t0).pass(context.Background())
	before, _ := lane.recording().leg(mem)
	if err := mem.Close(blocker.ID); err != nil {
		t.Fatal(err)
	}
	at(lane, t0.Add(backstopTestInterval)).pass(context.Background())
	after, _ := lane.recording().leg(mem)
	if !slices.Equal(ids(before.RawOpen), ids(after.RawOpen)) || slices.Contains(ids(before.ReadyAll), dep.ID) || !slices.Contains(ids(after.ReadyAll), dep.ID) {
		t.Fatalf("fixture: open %v -> %v, ready %v -> %v; want the open rows unchanged and %s newly ready",
			ids(before.RawOpen), ids(after.RawOpen), ids(before.ReadyAll), ids(after.ReadyAll), dep.ID)
	}
	if got := wakes.Load(); got != 2 {
		t.Errorf("Ready-only change: wakes = %d, want 2", got)
	}
}

// Kills: a content comparison that ignores errors or the closed index (M12,
// M12b), or that counts the stamps or the scope gaps as content.
func TestBackstopRecordingSameContentComparesRowsErrorsAndIndexes(t *testing.T) {
	store := beads.NewMemStore()
	idx, _ := session.BuildClosedNamedSessionBeadIndex(beads.NewMemStoreFrom(0, []beads.Bead{closedNamedSessionBead("gc-c", "mayor")}, nil))
	base := func() *backstopRecording {
		return &backstopRecording{
			Legs:        map[beads.Store]legRecording{store: {RawOpen: []beads.Bead{{ID: "gc-1"}}}},
			Sessions:    map[beads.Store]sessionLegRecording{store: {Rows: []session.Info{{ID: "gc-s"}}}},
			ClosedNamed: map[beads.Store]closedNamedRecording{store: {}},
		}
	}
	boom := errors.New("boom")
	for _, tc := range []struct {
		name string
		edit func(*backstopRecording)
		same bool
	}{
		{"identical", func(*backstopRecording) {}, true},
		{"stamps and gaps", func(r *backstopRecording) {
			r.Seq, r.At, r.Expires = 9, time.Now(), time.Now()
			r.ScopeGaps = []ControlDispatcherScopeGap{{RigContext: "r"}}
		}, true},
		{"open rows", func(r *backstopRecording) { r.Legs[store] = legRecording{} }, false},
		{"open error", func(r *backstopRecording) {
			r.Legs[store] = legRecording{RawOpen: []beads.Bead{{ID: "gc-1"}}, RawOpenErr: boom}
		}, false},
		{"ready error", func(r *backstopRecording) {
			r.Legs[store] = legRecording{RawOpen: []beads.Bead{{ID: "gc-1"}}, ReadyAllErr: boom}
		}, false},
		{"session error", func(r *backstopRecording) {
			r.Sessions[store] = sessionLegRecording{Rows: []session.Info{{ID: "gc-s"}}, Err: boom}
		}, false},
		{"closed index", func(r *backstopRecording) { r.ClosedNamed[store] = closedNamedRecording{Index: idx} }, false},
		{"closed index error", func(r *backstopRecording) { r.ClosedNamed[store] = closedNamedRecording{Err: boom} }, false},
		{"closed index dropped", func(r *backstopRecording) { delete(r.ClosedNamed, store) }, false},
	} {
		r := base()
		tc.edit(r)
		if got := base().sameContent(r); got != tc.same {
			t.Errorf("%s: sameContent = %t, want %t", tc.name, got, tc.same)
		}
	}
}

// Kills: a race between the lane's recording passes, its repairs, and
// allocator passes reading the recording, the cache and the last good
// answers, while an out-of-process writer adds rows (run under -race). The
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
	lane := newBackstopLane(backstopTestInterval, func() (backstopEnv, error) {
		return backstopEnv{CityPath: cityPath, Cfg: cfg, CityStore: front, RigStores: rigs, SuspendedRigPaths: suspended, ProbeStores: []beads.Store{probe, rigFixture}, Sessions: sessions}, nil
	}, func() { wakes.Add(1) }, func(fn func(), _ string) bool { fn(); return false }, io.Discard)
	ctx := context.Background()
	lastGood := newDemandLastGood()
	var wg sync.WaitGroup
	wg.Go(func() {
		for range rounds {
			lane.pass(ctx)
		}
	})
	wg.Go(func() {
		for range rounds / 3 {
			lane.repair(ctx)
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
				reads := newV2DemandReads(time.Now(), rec, lastGood)
				_ = runDemandCollectors(cfg, front, func() *readyDemandCache { return newReadyDemandCacheWithReads(reads) }, reads)
				rows, _, refs, _ := collectOpenUnassignedRoutedWork(cityPath, cfg, front, rigs, suspended, io.Discard, nil, reads)
				projected, _ := projectControlDispatcherRoutes(cfg, rows, refs)
				_ = openControlDispatcherDemand(cfg, projected)
				_ = readyAssignedWorkAssignees(cfg, front, sessions, nil, nil, "", reads)
				if rec != nil {
					_, _ = rec.sessionLeg(front)
					_ = rec.ScopeGaps
				}
			}
		})
	}
	wg.Wait()
	lane.pass(ctx)
	rec := lane.recording()
	leg, _ := rec.leg(front)
	if rec.Seq != rounds+1 || wakes.Load() < 1 || len(leg.RawOpen) < rounds {
		t.Errorf("lane seq=%d wakes=%d open rows=%d, want %d passes, a wake, and every written row", rec.Seq, wakes.Load(), len(leg.RawOpen), rounds+1)
	}
	if _, ok := rec.closedNamed(front); !ok || rec.RepairSeq != rounds/3 || len(rec.ScopeGaps) != 1 {
		t.Errorf("closed index recorded=%t RepairSeq=%d gaps=%+v, want recorded, %d runs and the nodisp gap", ok, rec.RepairSeq, rec.ScopeGaps, rounds/3)
	}
}

// Kills: the default-probe target stores left unrecorded (M22). A target
// store outside the census plan (here, a store no rig or binding names) is
// recorded with the census legs.
func TestBackstopLaneRecordsDefaultProbeStoresOutsideCensus(t *testing.T) {
	probe := beads.NewMemStoreFrom(0, []beads.Bead{routedDemandBead("gc-p1")}, nil)
	lane, _ := newTestBackstopLane(backstopEnv{Cfg: demandReadsTestConfig(), CityStore: beads.NewMemStore(), ProbeStores: []beads.Store{probe}})
	at(lane, time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)).pass(context.Background())
	if leg, ok := lane.recording().leg(probe); !ok || !slices.Equal(ids(leg.RawOpen), []string{"gc-p1"}) {
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

// Kills: repairs running on after shutdown began mid-run (N30). Shutdown
// lands during the assigned half; the routed half writes nothing.
func TestBackstopLaneRepairStopsBetweenHalvesOnShutdown(t *testing.T) {
	cfg := gaConfig()
	cfg.Agents = []config.Agent{poolAgent("planner", "rig-A", intPtr(5), 0)}
	ctx, cancel := context.WithCancel(context.Background())
	mem := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "ga-run", Type: "task", Status: "in_progress", Assignee: clobberSessionName, Metadata: clobberedWorkDir()},
		workBead("ga-slot", "rig-A/planner-2", "", "open", 5),
	}, nil)
	lane, _ := newTestBackstopLane(backstopEnv{Cfg: cfg, CityStore: cancelOnWriteStore{Store: mem, cancel: cancel}, Sessions: newSessionBeadSnapshot([]beads.Bead{stampTestSession(clobberSessionName, clobberLiveWorkDir)})})
	lane.repair(ctx)
	if ctx.Err() == nil {
		t.Fatal("fixture: the assigned half wrote nothing, so shutdown never began")
	}
	if b, _ := mem.Get("ga-slot"); b.Metadata[beadmeta.RoutedToMetadataKey] != "rig-A/planner-2" {
		t.Errorf("the routed half ran after shutdown: ga-slot route = %q", b.Metadata[beadmeta.RoutedToMetadataKey])
	}
}
