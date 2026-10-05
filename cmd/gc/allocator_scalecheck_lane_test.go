package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"maps"
	"math/rand/v2"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

// scriptedScaleChecks answers scale_check commands from a table and records
// the env each ran with. Safe for the lane's concurrent probes.
type scriptedScaleChecks struct {
	mu   sync.Mutex
	out  map[string]string
	envs map[string]map[string]string
	runs int
}

func (s *scriptedScaleChecks) run(command, _ string, env map[string]string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runs++
	if s.envs == nil {
		s.envs = make(map[string]map[string]string)
	}
	s.envs[command] = env
	out, ok := s.out[command]
	if !ok {
		return "", errors.New("scale_check exited 1")
	}
	return out, nil
}

func (s *scriptedScaleChecks) set(command, out string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.out[command] = out
}

func scaleCheckCity() *config.City {
	zero := 0
	return &config.City{
		Daemon: config.DaemonConfig{PatrolInterval: "10s"},
		Agents: []config.Agent{
			{Name: "ok", ScaleCheck: "check-ok"},
			{Name: "fails", ScaleCheck: "check-fails"},
			{Name: "noenv", ScaleCheck: "check-noenv"},
			{Name: "default"},
			{Name: "parked", ScaleCheck: "check-parked", Suspended: true},
			{Name: "disabled", ScaleCheck: "check-disabled", MaxActiveSessions: &zero},
			{Name: "named", ScaleCheck: "check-named"},
		},
		NamedSessions: []config.NamedSession{{Template: "named"}},
	}
}

// recoveringSafeTick is the host's safeTick for tests: it recovers a panic
// and records each pass's trigger.
type recoveringSafeTick struct {
	mu       sync.Mutex
	triggers []string
	panics   int
}

func (s *recoveringSafeTick) run(fn func(), trigger string) (panicked bool) {
	s.mu.Lock()
	s.triggers = append(s.triggers, trigger)
	s.mu.Unlock()
	defer func() {
		if recover() != nil {
			s.mu.Lock()
			s.panics++
			s.mu.Unlock()
			panicked = true
		}
	}()
	fn()
	return false
}

func newTestScaleCheckLane(cfg *config.City, checks *scriptedScaleChecks, changed *atomic.Int64) *scaleCheckLane {
	env := &reconcileEnv{Gen: 1, Cfg: cfg}
	safe := &recoveringSafeTick{}
	l := newScaleCheckLane("city", "", func() *reconcileEnv { return env }, fsys.NewFake(), func() { changed.Add(1) }, safe.run, io.Discard)
	l.runner = checks.run
	l.queryEnv = func(_ string, _ *config.City, agent *config.Agent) (map[string]string, error) {
		if agent.Name == "noenv" {
			return nil, errors.New("bd env: no port file")
		}
		return map[string]string{"GC_PROBE": agent.Name}, nil
	}
	return l
}

// Kills: a silent zero (#38). A failing check and a pool whose probe env
// cannot be built are both partial, not a trusted count of zero; only custom
// scale_check pools that are enabled run; a named session's backing pool runs
// without a probe env, as legacy's does. A missing or stale result is partial
// for every template.
func TestScaleCheckLaneErrorAndEnvFailureMarkPartial(t *testing.T) {
	checks := &scriptedScaleChecks{out: map[string]string{"check-ok": "3", "check-named": "1"}}
	var changed atomic.Int64
	l := newTestScaleCheckLane(scaleCheckCity(), checks, &changed)
	maxAge := 20 * time.Second

	if !l.latest().partial("ok", censusNow, maxAge) {
		t.Fatal("before the first pass: ok trusted, want partial")
	}
	l.pass()
	r := l.latest()
	if want := map[string]int{"ok": 3, "fails": 0, "noenv": 0, "named": 1}; !maps.Equal(r.Counts, want) {
		t.Fatalf("counts = %v, want %v", r.Counts, want)
	}
	var partial []string
	for _, template := range []string{"ok", "fails", "noenv", "named", "default"} {
		if r.partial(template, r.At, maxAge) {
			partial = append(partial, template)
		}
	}
	sort.Strings(partial)
	if got := strings.Join(partial, ","); got != "default,fails,noenv" {
		t.Fatalf("partial templates = %v, want default (no check), fails and noenv", partial)
	}
	if checks.runs != 3 {
		t.Fatalf("ran %d checks, want 3 (ok, fails, named)", checks.runs)
	}
	if env := checks.envs["check-named"]; env != nil {
		t.Fatalf("named backing pool ran with env %v, want none", env)
	}
	if env := checks.envs["check-ok"]; env["GC_PROBE"] != "ok" {
		t.Fatalf("pool ran with env %v, want its probe env", env)
	}
	if !r.partial("ok", r.At.Add(maxAge+time.Second), maxAge) {
		t.Fatal("stale result: ok trusted, want partial")
	}
	if changed.Load() != 1 {
		t.Fatalf("allocator woken %d times after the first pass, want 1", changed.Load())
	}
}

// Kills: a lane that runs only on wakes or on its own free-running grid, a
// first result one patrol late, and an allocator woken by passes that changed
// nothing. The lane runs at start, then one patrol after each pass, joins a
// wake to the duty cycle, and wakes the allocator only when counts or partial
// templates change.
func TestScaleCheckLaneRunsEveryPatrolAndWakesAllocatorOnChangeOnly(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		checks := &scriptedScaleChecks{out: map[string]string{"check-ok": "3", "check-fails": "0", "check-noenv": "0", "check-named": "1"}}
		var changed atomic.Int64
		l := newTestScaleCheckLane(scaleCheckCity(), checks, &changed)
		ctx, cancel := context.WithCancel(context.Background())
		done := l.start(ctx)
		t.Cleanup(func() {
			cancel()
			<-done
		})
		want := func(passes, wakes int64, when string) {
			t.Helper()
			synctest.Wait()
			if l.passes.Load() != passes || changed.Load() != wakes {
				t.Fatalf("%s: passes=%d allocator wakes=%d, want %d and %d", when, l.passes.Load(), changed.Load(), passes, wakes)
			}
		}

		want(1, 1, "at start")
		<-time.After(9 * time.Second)
		want(1, 1, "before the first patrol")
		<-time.After(time.Second)
		want(2, 1, "one patrol later, unchanged")

		checks.set("check-ok", "5")
		<-time.After(10 * time.Second)
		want(3, 2, "two patrols later, changed")

		// A wake right after a pass waits out the minimum gap, then runs.
		l.wake()
		want(3, 2, "wake inside the minimum gap")
		<-time.After(scaleCheckMinGap)
		want(4, 2, "wake after the minimum gap")
		<-time.After(10 * time.Second)
		want(5, 2, "the backstop restarts from the woken pass")
	})
}

// writeLaneSuspension writes the lane's runtime suspension file, or removes it
// when data is empty.
func writeLaneSuspension(l *scaleCheckLane, data string) {
	fs := l.suspension.memo.fs.(*fsys.Fake)
	path := citylayout.SuspensionStateFile(l.cityPath)
	if data == "" {
		delete(fs.Files, path)
		return
	}
	fs.Files[path] = []byte(data)
}

// Kills: a lane that runs scale_checks, or publishes, while the city is
// suspended (legacy: TestBuildDesiredState_ProductionDemandSkipsAllScaleChecksWhenCitySuspended),
// for each source of city suspension, and a lane that stays dark after the
// city resumes.
func TestScaleCheckLaneSkipsPassesWhileCitySuspended(t *testing.T) {
	for _, tc := range []struct {
		name            string
		suspend, resume func(t *testing.T, l *scaleCheckLane, cfg *config.City)
	}{{
		name:    "workspace suspended",
		suspend: func(_ *testing.T, _ *scaleCheckLane, cfg *config.City) { cfg.Workspace.Suspended = true },
		resume:  func(_ *testing.T, _ *scaleCheckLane, cfg *config.City) { cfg.Workspace.Suspended = false },
	}, {
		name: "suspension file",
		suspend: func(_ *testing.T, l *scaleCheckLane, _ *config.City) {
			writeLaneSuspension(l, `{"city":{"suspended":true}}`)
		},
		resume: func(_ *testing.T, l *scaleCheckLane, _ *config.City) { writeLaneSuspension(l, "") },
	}, {
		name:    "GC_SUSPENDED=1",
		suspend: func(t *testing.T, _ *scaleCheckLane, _ *config.City) { t.Setenv("GC_SUSPENDED", "1") },
		resume:  func(t *testing.T, _ *scaleCheckLane, _ *config.City) { t.Setenv("GC_SUSPENDED", "") },
	}} {
		t.Run(tc.name, func(t *testing.T) {
			checks := &scriptedScaleChecks{out: map[string]string{"check-ok": "3", "check-named": "1"}}
			var changed atomic.Int64
			cfg := scaleCheckCity()
			l := newTestScaleCheckLane(cfg, checks, &changed)
			tc.suspend(t, l, cfg)
			for i := 0; i < 2; i++ {
				if l.pass() {
					t.Fatalf("pass %d while suspended reported it ran", i)
				}
			}
			if checks.runs != 0 || l.latest() != nil || l.passes.Load() != 0 || changed.Load() != 0 {
				t.Fatalf("while suspended: runs=%d latest=%v passes=%d wakes=%d, want nothing", checks.runs, l.latest(), l.passes.Load(), changed.Load())
			}
			tc.resume(t, l, cfg)
			if !l.pass() {
				t.Fatal("pass after resume reported it did not run")
			}
			if checks.runs != 3 || l.latest() == nil || l.latest().Counts["ok"] != 3 || changed.Load() != 1 {
				t.Fatalf("after resume: runs=%d latest=%v wakes=%d, want 3 checks published", checks.runs, l.latest(), changed.Load())
			}
		})
	}
}

// Kills: suspended rigs read from anything but the pass's suspension state. A
// rig the suspension file suspends skips its pools, and runs them again once
// the file resumes it.
func TestScaleCheckLaneReadsSuspendedRigsFromSuspensionState(t *testing.T) {
	checks := &scriptedScaleChecks{out: map[string]string{"check-rigged": "2"}}
	var changed atomic.Int64
	cfg := &config.City{
		Rigs:   []config.Rig{{Name: "r1", Path: "/rigs/r1"}},
		Agents: []config.Agent{{Name: "rigged", Dir: "r1", ScaleCheck: "check-rigged"}},
	}
	l := newTestScaleCheckLane(cfg, checks, &changed)
	writeLaneSuspension(l, `{"city":{},"rigs":{"r1":{"suspended":true}}}`)
	l.pass()
	if checks.runs != 0 || len(l.latest().Counts) != 0 {
		t.Fatalf("suspended rig: runs=%d counts=%v, want its pool skipped", checks.runs, l.latest().Counts)
	}
	writeLaneSuspension(l, "")
	l.pass()
	if checks.runs != 1 || l.latest().Counts["r1/rigged"] != 2 {
		t.Fatalf("resumed rig: runs=%d counts=%v, want r1/rigged=2", checks.runs, l.latest().Counts)
	}
}

// Kills: a skipped pass counted toward the duty cycle. A lane that starts
// suspended and is woken on resume runs at once, not a minimum gap later.
func TestScaleCheckLaneSkippedPassDoesNotPaceTheNextWake(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		checks := &scriptedScaleChecks{out: map[string]string{"check-ok": "3"}}
		var changed atomic.Int64
		l := newTestScaleCheckLane(scaleCheckCity(), checks, &changed)
		writeLaneSuspension(l, `{"city":{"suspended":true}}`)
		ctx, cancel := context.WithCancel(context.Background())
		done := l.start(ctx)
		t.Cleanup(func() {
			cancel()
			<-done
		})
		synctest.Wait()
		if checks.runs != 0 || l.passes.Load() != 0 {
			t.Fatalf("suspended at start: runs=%d passes=%d, want none", checks.runs, l.passes.Load())
		}
		writeLaneSuspension(l, "")
		l.wake()
		synctest.Wait()
		if l.passes.Load() != 1 {
			t.Fatalf("wake on resume: passes=%d, want 1 at once", l.passes.Load())
		}
	})
}

// Kills: a lane that wakes the allocator only on count changes. A pool whose
// check starts failing keeps its count (0 either way) but turns partial, which
// the allocator must see.
func TestScaleCheckLaneWakesAllocatorOnPartialChangeAlone(t *testing.T) {
	checks := &scriptedScaleChecks{out: map[string]string{"check-ok": "3", "check-fails": "0", "check-named": "1"}}
	var changed atomic.Int64
	l := newTestScaleCheckLane(scaleCheckCity(), checks, &changed)
	l.pass()
	before := l.latest()
	checks.mu.Lock()
	delete(checks.out, "check-fails")
	checks.mu.Unlock()
	l.pass()
	after := l.latest()
	if !maps.Equal(before.Counts, after.Counts) || maps.Equal(before.Partial, after.Partial) {
		t.Fatalf("fixture: counts %v -> %v, partial %v -> %v, want equal counts and new partials", before.Counts, after.Counts, before.Partial, after.Partial)
	}
	if changed.Load() != 2 {
		t.Fatalf("allocator woken %d times, want 2 (first pass, then the partial change)", changed.Load())
	}
}

// Kills: a lane pass that runs outside safeTick, so a panic kills the lane
// (MAINT-020), and a panicking pass left out of the duty cycle, so wakes
// could rerun it back to back. Each pass runs under safeTick with the lane's
// trigger; after a panicking pass a wake waits out the minimum gap, and the
// lane keeps its cadence.
func TestScaleCheckLaneRunsEachPassUnderSafeTick(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		checks := &scriptedScaleChecks{out: map[string]string{"check-ok": "3"}}
		var changed atomic.Int64
		l := newTestScaleCheckLane(scaleCheckCity(), checks, &changed)
		safe := &recoveringSafeTick{}
		l.safeTick = safe.run
		var envCalls atomic.Int64
		build := l.queryEnv
		l.queryEnv = func(cityPath string, cfg *config.City, agent *config.Agent) (map[string]string, error) {
			if envCalls.Add(1) == 1 {
				panic("probe env exploded")
			}
			return build(cityPath, cfg, agent)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := l.start(ctx)
		t.Cleanup(func() {
			cancel()
			<-done
		})
		triggers := func() int {
			safe.mu.Lock()
			defer safe.mu.Unlock()
			return len(safe.triggers)
		}
		synctest.Wait()
		l.wake()
		synctest.Wait()
		if n := triggers(); n != 1 {
			t.Fatalf("wake right after the panicking pass: %d passes, want 1 (paced)", n)
		}
		<-time.After(scaleCheckMinGap)
		synctest.Wait()
		<-time.After(10 * time.Second)
		synctest.Wait()
		safe.mu.Lock()
		defer safe.mu.Unlock()
		if len(safe.triggers) != 3 || slices.ContainsFunc(safe.triggers, func(s string) bool { return s != scaleCheckSafeTickTrigger }) || safe.panics != 1 {
			t.Fatalf("safeTick triggers=%v panics=%d, want three %q passes and one recovered panic", safe.triggers, safe.panics, scaleCheckSafeTickTrigger)
		}
		if l.passes.Load() != 2 {
			t.Fatalf("published passes = %d, want 2 (the woken pass and the backstop)", l.passes.Load())
		}
	})
}

// scaleCheckProbeEnv is a probe-env builder that fails for the agents in fail.
func scaleCheckProbeEnv(fail map[string]bool) probeEnvFunc {
	return func(_ string, _ *config.City, agent *config.Agent) (map[string]string, error) {
		if fail[agent.QualifiedName()] {
			return nil, errors.New("bd env: no port file")
		}
		return map[string]string{"GC_PROBE": agent.QualifiedName()}, nil
	}
}

// scaleCheckDemandCities mirrors build_desired_state_test.go's custom
// scale_check fixtures, with a city path and rig directories made for each.
func scaleCheckDemandCities(t *testing.T) []demandFixture {
	t.Helper()
	addRig := func(f *demandFixture, rig, path string, suspended bool) {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
		f.cfg.Rigs = append(f.cfg.Rigs, config.Rig{Name: rig, Path: path})
		f.rigStores[rig] = beads.NewMemStore()
		if suspended {
			f.suspendedRigPaths[filepath.Clean(path)] = true
		}
	}
	city := func(rigs []string, suspended []string, agents []config.Agent, named []config.NamedSession) demandFixture {
		cityPath := t.TempDir()
		f := demandFixture{
			cityPath:          cityPath,
			cfg:               &config.City{Workspace: config.Workspace{Name: "test-city"}, Agents: agents, NamedSessions: named},
			store:             beads.NewMemStore(),
			rigStores:         map[string]beads.Store{},
			suspendedRigPaths: map[string]bool{},
		}
		for _, rig := range rigs {
			addRig(&f, rig, filepath.Join(cityPath, rig), slices.Contains(suspended, rig))
		}
		return f
	}
	// Rigs whose directories lie outside the city dir, one suspended: an
	// agent's rig is resolved by rig name, not by joining its dir to the city.
	outside := city(nil, nil, []config.Agent{
		{Name: "far", Dir: "away", ScaleCheck: "printf 1"},
		{Name: "near", Dir: "live", ScaleCheck: "printf 2"},
	}, nil)
	addRig(&outside, "away", t.TempDir(), true)
	addRig(&outside, "live", t.TempDir(), false)
	return []demandFixture{
		outside,
		// Same-named agents in two rigs, only one backing a named session:
		// the match is by qualified name, so the other runs with its env.
		city([]string{"r1", "r2"}, nil, []config.Agent{
			{Name: "w", Dir: "r1", ScaleCheck: "printf 1"},
			{Name: "w", Dir: "r2", ScaleCheck: "printf 2"},
		}, []config.NamedSession{{Template: "w", Dir: "r1"}}),
		// TestBuildDesiredState_ProductionDemandSkipsSuspendedAgentScaleCheck.
		city(nil, nil, []config.Agent{{Name: "live", ScaleCheck: "printf 0"}, {Name: "parked", Suspended: true, ScaleCheck: "printf 1"}}, nil),
		// TestBuildDesiredState_ProductionDemandSkipsSuspendedRigScaleCheck.
		city([]string{"live-rig", "parked-rig"}, []string{"parked-rig"}, []config.Agent{
			{Name: "alpha", Dir: "live-rig", ScaleCheck: "printf 0"},
			{Name: "beta", Dir: "parked-rig", ScaleCheck: "printf 1"},
		}, nil),
		// TestBuildDesiredState_RigScopedScaleCheckExpandsRigTemplate.
		city([]string{"alpha", "beta"}, nil, []config.Agent{
			{Name: "ant", Dir: "alpha", StartCommand: "true", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(5), ScaleCheck: "echo {{.Rig}} | grep -c alpha"},
			{Name: "ant", Dir: "beta", StartCommand: "true", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(5), ScaleCheck: "echo {{.Rig}} | grep -c beta"},
		}, nil),
		// TestBuildDesiredStateTranslatesFloorDemandIntoCreateBudget.
		city(nil, nil, []config.Agent{
			{Name: "alpha", StartCommand: "true", ScaleCheck: "printf 5", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(5)},
			{Name: "zulu", StartCommand: "true", ScaleCheck: "printf 0", MinActiveSessions: intPtr(1), MaxActiveSessions: intPtr(5)},
		}, nil),
		// A named session's backing pool with a custom check (always and
		// on_demand), a disabled pool, and a default-probe pool.
		city([]string{"r1"}, nil, []config.Agent{
			{Name: "mayor", ScaleCheck: "printf 1"},
			{Name: "deacon", Dir: "r1", ScaleCheck: "printf 2"},
			{Name: "off", ScaleCheck: "printf 1", MaxActiveSessions: intPtr(0)},
			{Name: "plain", Dir: "r1"},
			{Name: "worker", Dir: "r1", ScaleCheck: "printf {{.AgentBase}}"},
		}, []config.NamedSession{{Template: "mayor", Mode: "always"}, {Template: "deacon", Dir: "r1", Mode: "on_demand"}}),
	}
}

// Kills: the lane's pools drifting from legacy's demand pass (POOL-005). Over
// P3-1's randomized cities and build_desired_state_test.go's scale_check
// fixtures, the lane's work equals buildDemandTargets' pendingPools element
// by element (agent index, scale params including the expanded check, pool
// dir, probe env, new demand), and the pools whose probe env fails are
// exactly the pools legacy silently skips (BEHAVIORS #38).
func TestScaleCheckLaneWorkMatchesBuildDemandTargets(t *testing.T) {
	fixtures := scaleCheckDemandCities(t)
	for seed := uint64(0); seed < poolPiecesSeeds; seed++ {
		f := randDemandFixture(t, rand.New(rand.NewPCG(seed, 6)))
		if f.store == nil {
			// The lane serves v2, which always has a store.
			f.store = beads.NewMemStore()
		}
		fixtures = append(fixtures, f)
	}
	suspendedRigSeen := false
	for i, f := range fixtures {
		r := rand.New(rand.NewPCG(uint64(i), 7))
		fail := map[string]bool{}
		for j := range f.cfg.Agents {
			if r.IntN(3) == 0 {
				fail[f.cfg.Agents[j].QualifiedName()] = true
			}
		}
		legacyAll := buildDemandTargets("city", f.cityPath, f.cfg, f.store, f.rigStores, f.suspendedRigPaths, f.sessions, scaleCheckProbeEnv(nil), io.Discard).pendingPools
		legacy := buildDemandTargets("city", f.cityPath, f.cfg, f.store, f.rigStores, f.suspendedRigPaths, f.sessions, scaleCheckProbeEnv(fail), io.Discard).pendingPools
		var skipped []string
		for _, w := range legacyAll {
			if !slices.ContainsFunc(legacy, func(l poolEvalWork) bool { return l.agentIdx == w.agentIdx }) {
				skipped = append(skipped, f.cfg.Agents[w.agentIdx].QualifiedName())
			}
		}

		var stderr bytes.Buffer
		work, envFailed := customScaleCheckWork("city", f.cityPath, f.cfg, f.suspendedRigPaths, scaleCheckProbeEnv(fail), &stderr)
		if !reflect.DeepEqual(work, legacy) {
			t.Fatalf("fixture %d: lane work differs from legacy pendingPools:\n lane=%+v\n legacy=%+v", i, work, legacy)
		}
		if !slices.Equal(envFailed, skipped) {
			t.Fatalf("fixture %d: lane env failures %v, legacy skipped %v", i, envFailed, skipped)
		}
		for j := range f.cfg.Agents {
			agent := &f.cfg.Agents[j]
			if rig := configuredRigName(f.cityPath, agent, f.cfg.Rigs); rig != "" && agent.ScaleCheck != "" && !agent.Suspended &&
				f.suspendedRigPaths[filepath.Clean(rigRootForName(rig, f.cfg.Rigs))] {
				suspendedRigSeen = true
			}
		}
	}
	if !suspendedRigSeen {
		t.Fatal("no fixture has a custom scale_check pool on a suspended rig")
	}
}
