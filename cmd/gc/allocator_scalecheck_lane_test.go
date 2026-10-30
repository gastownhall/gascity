package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
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

// newScaleCheckSourceLane returns a lane over the city at cityPath whose only
// source is the scale_check one (no city store, so no leg), running checks
// with a scripted runner and a probe env that fails for the noenv pool.
func newScaleCheckSourceLane(cityPath string, cfg *config.City, checks *scriptedScaleChecks) (*externalReadsLane, *atomic.Int64) {
	lane, wakes := newTestBackstopLane(externalReadsEnv{CityPath: cityPath, CityName: "city", Cfg: cfg})
	lane.runner = checks.run
	lane.queryEnv = func(_ string, _ *config.City, agent *config.Agent) (map[string]string, error) {
		if agent.Name == "noenv" {
			return nil, errors.New("bd env: no port file")
		}
		return map[string]string{"GC_PROBE": agent.Name}, nil
	}
	return lane, wakes
}

// scaleCheckSource returns rec's scale_check source.
func scaleCheckSource(rec *externalReadsRecording) (sourceResult, bool) {
	for key, s := range rec.Sources {
		if key.kind == sourceScaleCheck {
			return s, true
		}
	}
	return sourceResult{}, false
}

// Kills: a silent zero (#38). A failing check and a pool whose probe env
// cannot be built are both partial, not a trusted count of zero; only custom
// scale_check pools that are enabled run; a named session's backing pool runs
// without a probe env, as legacy's does. A missing, stale or timed-out result
// is partial for every template, and the result is stamped when its run
// ended.
func TestExternalReadsScaleCheckSourcePartialOnErrorAndEnvFailure(t *testing.T) {
	checks := &scriptedScaleChecks{out: map[string]string{"check-ok": "3", "check-named": "1"}}
	lane, wakes := newScaleCheckSourceLane("", scaleCheckCity(), checks)
	maxAge := 3 * backstopTestInterval

	if !lane.recording().scaleCheck(censusNow).partial("ok") {
		t.Fatal("before the first pass: ok trusted, want partial")
	}
	if !at(lane, censusNow).pass(context.Background()) {
		t.Fatal("pass declined")
	}
	r := lane.recording().scaleCheck(censusNow)
	if src, _ := scaleCheckSource(lane.recording()); !src.EndedAt.Equal(censusNow) {
		t.Fatalf("result ended at %v, want the run's end %v", src.EndedAt, censusNow)
	}
	if want := map[string]int{"ok": 3, "fails": 0, "noenv": 0, "named": 1}; !maps.Equal(r.Counts, want) {
		t.Fatalf("counts = %v, want %v", r.Counts, want)
	}
	var partial []string
	for _, template := range []string{"ok", "fails", "noenv", "named", "default"} {
		if r.partial(template) {
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
	if !lane.recording().scaleCheck(censusNow.Add(maxAge + time.Nanosecond)).partial("ok") {
		t.Fatal("stale result: ok trusted, want partial")
	}
	if wakes.Load() != 1 {
		t.Fatalf("allocator woken %d times after the first pass, want 1", wakes.Load())
	}
	timedOut := &externalReadsRecording{FreshFor: maxAge, Sources: map[sourceKey]sourceResult{{kind: sourceScaleCheck}: {EndedAt: censusNow, Err: errSourceTimeout}}}
	if !timedOut.scaleCheck(censusNow).partial("ok") {
		t.Fatal("timed-out run: ok trusted, want partial")
	}
}

// Kills: suspended rigs read from anything but the pass's suspension state
// (the env's suspended rig paths). A suspended rig skips its pools, and runs
// them again once resumed.
func TestExternalReadsScaleCheckSourceSkipsSuspendedRigs(t *testing.T) {
	checks := &scriptedScaleChecks{out: map[string]string{"check-rigged": "2"}}
	env := externalReadsEnv{
		CityName: "city",
		Cfg: &config.City{
			Rigs:   []config.Rig{{Name: "r1", Path: "/rigs/r1"}},
			Agents: []config.Agent{{Name: "rigged", Dir: "r1", ScaleCheck: "check-rigged"}},
		},
		SuspendedRigPaths: map[string]bool{"/rigs/r1": true},
	}
	noProbeEnv := func(string, *config.City, *config.Agent) (map[string]string, error) { return nil, nil }
	if r := runCustomScaleChecks(env, checks.run, noProbeEnv, io.Discard); checks.runs != 0 || len(r.Counts) != 0 {
		t.Fatalf("suspended rig: runs=%d counts=%v, want its pool skipped", checks.runs, r.Counts)
	}
	env.SuspendedRigPaths = nil
	if r := runCustomScaleChecks(env, checks.run, noProbeEnv, io.Discard); checks.runs != 1 || r.Counts["r1/rigged"] != 2 {
		t.Fatalf("resumed rig: runs=%d counts=%v, want r1/rigged=2", checks.runs, r.Counts)
	}
}

// Kills custom scale_checks running in a suspended city (POOL-001: legacy's
// demand pass returns before them, and the decide reads no scale_check while
// the city is suspended), and a lane that keeps them off after resume. A
// suspended pass runs none and publishes no scale_check source, so the result
// read before the suspension is not served after it; the first pass after
// resume runs them again.
func TestExternalReadsScaleCheckSourceSkippedWhileCitySuspended(t *testing.T) {
	for _, tc := range suspendCases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			cityPath := t.TempDir()
			cfg := scaleCheckCity()
			checks := &scriptedScaleChecks{out: map[string]string{"check-ok": "3", "check-named": "1"}}
			lane, _ := newScaleCheckSourceLane(cityPath, cfg, checks)
			if !passAndJoin(ctx, at(lane, censusNow)) {
				t.Fatal("pass before suspending declined")
			}
			if checks.runs != 3 {
				t.Fatalf("before suspending: ran %d checks, want 3 (ok, fails, named)", checks.runs)
			}
			tc.suspend(t, cfg, cityPath)
			if !passAndJoin(ctx, at(lane, censusNow.Add(time.Second))) {
				t.Fatal("a pass declined while the city is suspended")
			}
			if checks.runs != 3 {
				t.Errorf("suspended pass ran %d checks, want none", checks.runs-3)
			}
			if src, ok := scaleCheckSource(lane.recording()); ok {
				t.Errorf("suspended pass published a scale_check source %+v, want none", src)
			}
			tc.resume(t, cfg, cityPath)
			resumed := censusNow.Add(2 * time.Second)
			if !passAndJoin(ctx, at(lane, resumed)) {
				t.Fatal("pass after resume declined")
			}
			if checks.runs != 6 {
				t.Errorf("pass after resume ran %d checks, want 3", checks.runs-3)
			}
			if r := lane.recording().scaleCheck(resumed); r.partial("ok") || r.Counts["ok"] != 3 {
				t.Errorf("after resume: scale_check result %+v, want ok trusted at 3", r)
			}
		})
	}
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

// scaleCheckLegCity is demandReadsTestConfig plus a custom scale_check
// pool, so the lane reads a lane-fed leg and the scale_check source.
func scaleCheckLegCity() *config.City {
	cfg := demandReadsTestConfig()
	cfg.Agents = append(cfg.Agents, config.Agent{Name: "checked", ScaleCheck: "check"})
	return cfg
}

// Kills a reload that holds every custom template's creates for the new
// config's run (PAR-SC review), and a carried count for a template whose
// check changed (nit 5). Until the new config's run ends, the lane serves the
// result the last publish served, aged from its own end, so it reads partial
// once 3 × patrol old: w's check is unchanged, so its count is served; v's
// changed, so v reads partial. The new run's result then replaces it.
func TestExternalReadsReloadServesLastScaleCheckUntilNewRunEnds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		var releaseOnce sync.Once
		defer releaseOnce.Do(func() { close(release) })
		oldCfg := &config.City{Agents: []config.Agent{{Name: "w", ScaleCheck: "check-w"}, {Name: "v", ScaleCheck: "check-v-old"}}}
		newCfg := &config.City{Agents: []config.Agent{{Name: "w", ScaleCheck: "check-w"}, {Name: "v", ScaleCheck: "check-v-new"}}}
		var cfg atomic.Pointer[config.City]
		cfg.Store(oldCfg)
		lane := newExternalReadsLane(backstopTestInterval, func() (externalReadsEnv, error) {
			return externalReadsEnv{CityName: "city", Cfg: cfg.Load()}, nil
		}, nil, func(fn func(), _ string) bool { fn(); return false }, nil, io.Discard)
		lane.steps = nil
		lane.queryEnv = func(string, *config.City, *config.Agent) (map[string]string, error) { return nil, nil }
		lane.runner = func(command, _ string, _ map[string]string) (string, error) {
			switch command {
			case "check-v-new":
				<-release
				return "2", nil
			case "check-v-old":
				return "5", nil
			}
			return "7", nil
		}
		startBackstopLaneInBubble(t, lane)
		synctest.Wait()
		served, ok := scaleCheckSource(lane.recording())
		if r := lane.recording().scaleCheck(time.Now()); !ok || r == nil || r.Counts["w"] != 7 || r.Counts["v"] != 5 {
			t.Fatalf("before the reload: scale_check %+v, want the old config's w=7, v=5", r)
		}
		cfg.Store(newCfg)
		lane.wake()
		advanceBackstop(externalReadsMinGap + externalReadsSourceDeadline)
		src, ok := scaleCheckSource(lane.recording())
		r := lane.recording().scaleCheck(time.Now())
		if !ok || r == nil || r.Counts["w"] != 7 || r.partial("w") || !r.partial("v") || !src.EndedAt.Equal(served.EndedAt) || src.InFlightSince.IsZero() {
			t.Fatalf("new config's run in flight: source %+v, result %+v; want w=7 carried, v partial, aged from %v, marked in flight", src, r, served.EndedAt)
		}
		advanceBackstop(time.Until(served.EndedAt.Add(3*backstopTestInterval)) + time.Second)
		if r := lane.recording().scaleCheck(time.Now()); r != nil {
			t.Fatalf("3 patrols past the old result with the new run hung: %+v, want partial", r)
		}
		releaseOnce.Do(func() { close(release) })
		advanceBackstop(externalReadsMinGap)
		if r := lane.recording().scaleCheck(time.Now()); r == nil || r.Counts["w"] != 7 || r.Counts["v"] != 2 {
			t.Fatalf("after the new run ended: %+v, want the new config's w=7, v=2", r)
		}
	})
}

// Kills a run read before a suspension served after resume (PAR-SC review):
// once a pass has seen the city suspended, neither the result it published
// before nor a run that started before it and ended during the suspension
// is served, so after resume the custom templates read partial until the
// first new run ends, as legacy's first tick after resume runs the checks
// before it trusts them.
func TestExternalReadsResumeServesNoPreSuspensionScaleCheck(t *testing.T) {
	for _, straddle := range []bool{false, true} {
		t.Run(fmt.Sprintf("straddle=%t", straddle), func(t *testing.T) {
			tc := suspendCases[0]
			ctx := context.Background()
			cityPath := t.TempDir()
			cfg := scaleCheckCity()
			checks := &scriptedScaleChecks{out: map[string]string{"check-ok": "3", "check-named": "1"}}
			lane, _ := newScaleCheckSourceLane(cityPath, cfg, checks)
			lane.sourceDeadline = time.Millisecond
			if !passAndJoin(ctx, at(lane, censusNow)) || lane.recording().scaleCheck(censusNow).partial("ok") {
				t.Fatal("before suspending: ok not trusted")
			}
			gate := make(chan struct{})
			lane.runner = func(command, dir string, env map[string]string) (string, error) {
				<-gate
				return checks.run(command, dir, env)
			}
			if straddle { // a run starts before the suspension and ends during it
				if !at(lane, censusNow.Add(time.Second)).pass(ctx) {
					t.Fatal("pass before suspending declined")
				}
			}
			tc.suspend(t, cfg, cityPath)
			lane.now = func() time.Time { return censusNow.Add(2 * time.Second) }
			if !lane.pass(ctx) {
				t.Fatal("a pass declined while the city is suspended")
			}
			close(gate)
			_ = lane.join(ctx) // the straddling run ends while the city is suspended
			tc.resume(t, cfg, cityPath)
			gate2 := make(chan struct{})
			lane.runner = func(command, dir string, env map[string]string) (string, error) {
				<-gate2
				return checks.run(command, dir, env)
			}
			resumed := censusNow.Add(3 * time.Second)
			if !at(lane, resumed).pass(ctx) {
				t.Fatal("pass after resume declined")
			}
			src, _ := scaleCheckSource(lane.recording())
			if r := lane.recording().scaleCheck(resumed); !r.partial("ok") {
				t.Errorf("after resume, before the new run ended: %+v served (started %v), want ok partial", r, src.StartedAt)
			}
			close(gate2)
			if !passAndJoin(ctx, at(lane, resumed)) || lane.recording().scaleCheck(resumed).partial("ok") {
				t.Error("after the new run ended: ok still partial")
			}
		})
	}
}

// settableClock is a planner clock that reads at, so a test pins when the
// gather stamps first sights of claims (after its work read).
type settableClock struct {
	realPlannerClock
	at time.Time
}

func (c *settableClock) Now() time.Time { return c.at }

// claimSkewDecide gathers over store as the planner p does at now, with rec
// as K1's recording and wakes counting gather's lane wakes, and decides.
func claimSkewDecide(t *testing.T, p *planner, cfg *config.City, cityPath string, store beads.Store, alive []string, rec *externalReadsRecording, now time.Time, wakes *atomic.Int64) allocDecision {
	t.Helper()
	sp := diffProvider(t, alive)
	env := &reconcileEnv{Gen: 1, Cfg: cfg, SP: sp, ConfigRev: "rev-1"}
	obs := NewObservationCache(&clock.Fake{Time: now}, time.Minute, "e1")
	attrs := make(map[string]InventoryAttrs, len(alive))
	for _, name := range alive {
		attrs[name] = liveAttrs("")
	}
	obs.PublishInventory(InventoryPass{
		Epoch: "e1", Seq: 1, ProviderGen: 1, StartedAt: now, FinishedAt: now,
		MergedNames: alive, Backends: []BackendPass{completeBackend("", alive...)},
	}, attrs)
	w, err := gather(gatherEnv{
		CityPath: cityPath, CityName: cfg.Workspace.Name,
		Env:          func() *reconcileEnv { return env },
		Sessions:     func() beads.Store { return store },
		RigStores:    func() map[string]beads.Store { return nil },
		Recording:    func() *externalReadsRecording { return rec },
		WakeReads:    func() { wakes.Add(1) },
		Observations: func() *ObservationCache { return obs },
		Episodes:     func() (map[string]session.StartupHealthEpisode, error) { return readStartupHealthEpisodes(store) },
	}, p, now)
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	return mustDecide(t, allocInputsOf(w, cityPath, cfg.Workspace.Name))
}

// Kills a claimed bead counted twice (PAR-SC re-review must-fix): a custom
// check counts open unassigned work routed to worker, and a session claims
// the work after the check's run started. The pass counts the claim from
// the cache at once, but the run's count still holds the work as new
// demand, so without the pairing guard v2 plans a session for work a
// session holds. With it, the claim is subtracted from the count
// (scale-check-stale-vs-claim): no create, the row not drained, and the
// gather wakes the lane. The subtraction ends with the next run, which
// counts the claim itself; a new unassigned bead is then demand again.
// Claimed by the row's ID and its session name, the work is the row's, so
// the new bead plans a create; claimed by its agent name, pool demand does
// not count it as the row's, so the awake row takes the new bead.
func TestScaleCheckClaimSkewSubtractsTheClaim(t *testing.T) {
	for who, creates := range map[string]int{"gc-1": 1, "s-gc-1": 1, "worker-1": 0} {
		t.Run(who, func(t *testing.T) { claimSkewCase(t, who, creates) })
	}
}

func claimSkewCase(t *testing.T, who string, finalCreates int) {
	ctx := context.Background()
	cityPath := t.TempDir()
	cfg := diffConfig(customPool("worker", "count-ready", 3))
	row := poolRow("gc-1", "worker", 1, "awake")
	work := beads.Bead{
		ID: "w-1", Title: "w-1", Type: "task", Status: "open", CreatedAt: diffNow.Add(-time.Hour),
		Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "worker"},
	}
	cache, backing := newDemandCache(t, false, row, work)
	lane, _ := newTestBackstopLane(externalReadsEnv{CityPath: cityPath, CityName: cfg.Workspace.Name, Cfg: cfg, CityStore: cache})
	lane.steps = nil
	lane.queryEnv = func(string, *config.City, *config.Agent) (map[string]string, error) { return nil, nil }
	lane.runner = func(string, string, map[string]string) (string, error) {
		rows, err := backing.Store.List(beads.ListQuery{Status: "open"})
		if err != nil {
			return "", err
		}
		n := 0
		for _, b := range rows {
			if b.Type == "task" && b.Assignee == "" && b.Metadata[beadmeta.RoutedToMetadataKey] == "worker" {
				n++
			}
		}
		return fmt.Sprint(n), nil
	}
	clk := &settableClock{}
	p := newPlanner(clk, func() time.Duration { return time.Minute }, nil, newInflightMap(), nil, io.Discard)
	alive := []string{"s-gc-1"}
	var wakes atomic.Int64
	cut := func(d allocDecision) bool {
		return slices.Contains(d.Snapshot.Partial.Templates["worker"].Causes, causeScaleCheckStaleClaim)
	}

	t0 := diffNow
	if !passAndJoin(ctx, at(lane, t0)) {
		t.Fatal("pass 1 declined")
	}
	clk.at = t0
	if d := claimSkewDecide(t, p, cfg, cityPath, cache, alive, lane.recording(), t0, &wakes); cut(d) {
		t.Fatalf("before any claim: cut %+v", d.Snapshot.Partial.Templates["worker"])
	}

	assignee, inProgress := who, "in_progress"
	if err := cache.Update("w-1", beads.UpdateOpts{Assignee: &assignee, Status: &inProgress}); err != nil {
		t.Fatal(err)
	}
	t1 := t0.Add(time.Second)
	clk.at = t1
	d := claimSkewDecide(t, p, cfg, cityPath, cache, alive, lane.recording(), t1, &wakes)
	if len(d.Plans) != 0 || !cut(d) || wakes.Load() != 1 {
		t.Fatalf("claim newer than the run: %d create(s), causes %v, %d lane wakes; want none, %s, one wake",
			len(d.Plans), d.Snapshot.Partial.Templates["worker"].Causes, wakes.Load(), causeScaleCheckStaleClaim)
	}
	if e := entryOf(t, d, "gc-1"); e.Desired == desireDrain || e.Desired == desireSleep {
		t.Fatalf("the claimant's row = %s; want not drained", e.Desired)
	}
	clk.at = t1.Add(time.Second)
	if d := claimSkewDecide(t, p, cfg, cityPath, cache, alive, lane.recording(), t1.Add(time.Second), &wakes); len(d.Plans) != 0 || !cut(d) || wakes.Load() != 1 {
		t.Fatalf("same claim, next pass on the same run: %d create(s), cut %t, %d wakes; want none, cut, no new wake", len(d.Plans), cut(d), wakes.Load())
	}

	t2 := t0.Add(3 * time.Second)
	if !passAndJoin(ctx, at(lane, t2)) {
		t.Fatal("pass 2 declined")
	}
	clk.at = t2
	if d := claimSkewDecide(t, p, cfg, cityPath, cache, alive, lane.recording(), t2, &wakes); len(d.Plans) != 0 || cut(d) {
		t.Fatalf("run newer than the claim: %d create(s), cut %t; want none and no cut", len(d.Plans), cut(d))
	}

	if _, err := cache.Create(beads.Bead{
		ID: "w-2", Title: "w-2", Type: "task", Status: "open", CreatedAt: diffNow,
		Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "worker"},
	}); err != nil {
		t.Fatal(err)
	}
	t3 := t0.Add(4 * time.Second)
	if !passAndJoin(ctx, at(lane, t3)) {
		t.Fatal("pass 3 declined")
	}
	clk.at = t3
	if d := claimSkewDecide(t, p, cfg, cityPath, cache, alive, lane.recording(), t3, &wakes); len(d.Plans) != finalCreates || cut(d) {
		t.Fatalf("new unassigned work after the cut: %d create(s), cut %t; want %d and no cut", len(d.Plans), cut(d), finalCreates)
	}
}

// suspendSpanLane starts, in the current bubble, a lane over the city at
// cityPath with one custom scale_check pool, w, whose nth run waits until
// the test calls end(n) and then counts n; runs counts the runs started.
func suspendSpanLane(t *testing.T, cityPath string) (lane *externalReadsLane, runs *atomic.Int64, end func(n int)) {
	t.Helper()
	cfg := &config.City{Agents: []config.Agent{{Name: "w", ScaleCheck: "check-w"}}}
	lane = newExternalReadsLane(backstopTestInterval, func() (externalReadsEnv, error) {
		return externalReadsEnv{CityName: "city", CityPath: cityPath, Cfg: cfg}, nil
	}, nil, func(fn func(), _ string) bool { fn(); return false }, nil, io.Discard)
	lane.steps = nil
	lane.queryEnv = func(string, *config.City, *config.Agent) (map[string]string, error) { return nil, nil }
	gates := [2]chan struct{}{make(chan struct{}), make(chan struct{})}
	var ended [2]sync.Once
	end = func(n int) { ended[n-1].Do(func() { close(gates[n-1]) }) }
	runs = &atomic.Int64{}
	lane.runner = func(string, string, map[string]string) (string, error) {
		n := runs.Add(1)
		if n > int64(len(gates)) {
			return "", fmt.Errorf("scale_check run %d, want at most %d", n, len(gates))
		}
		<-gates[n-1]
		return fmt.Sprint(n), nil
	}
	startBackstopLaneInBubble(t, lane)
	// Registered after the lane's stop and join, so it runs before them.
	t.Cleanup(func() { end(1); end(2) })
	return lane, runs, end
}

// Kills a scale_check run that spans a suspended pass being served after
// resume (PAR-SC review): a run that started before the city suspended and
// ended while it was suspended is discarded, so when the city resumes before
// the next patrol, the custom templates read partial until the run the
// resumed pass starts has ended.
func TestExternalReadsScaleCheckRunEndedWhileSuspendedNotServedAfterResume(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cityPath := t.TempDir()
		lane, runs, end := suspendSpanLane(t, cityPath)
		writeSuspension(t, cityPath, `{"city":{"suspended":true}}`)
		// The first pass waits out the source deadline on run 1; the next,
		// suspended, pass comes a patrol after.
		advanceBackstop(externalReadsSourceDeadline + backstopTestInterval)
		if got := runs.Load(); got != 1 {
			t.Fatalf("after a suspended pass: %d scale_check runs, want 1 (a suspended pass runs none)", got)
		}
		end(1)
		advanceBackstop(externalReadsMinGap)
		if r := lane.recording().scaleCheck(time.Now()); r != nil {
			t.Fatalf("run 1 ended while suspended: %+v served, want partial", r)
		}
		writeSuspension(t, cityPath, "")
		lane.wake()
		advanceBackstop(externalReadsMinGap)
		if got := runs.Load(); got != 2 {
			t.Fatalf("after resume: %d scale_check runs, want 2 (the resumed pass starts a new run)", got)
		}
		if r := lane.recording().scaleCheck(time.Now()); r != nil {
			t.Fatalf("after resume, run 2 in flight: %+v served (run 1 started before the suspension), want partial", r)
		}
		end(2)
		synctest.Wait()
		if r := lane.recording().scaleCheck(time.Now()); r == nil || r.Counts["w"] != 2 {
			t.Fatalf("after run 2 ended: %+v, want w=2", r)
		}
	})
}

// Kills a scale_check run still in flight when the city resumes being served
// once it ends (PAR-SC review): the resumed pass starts no second run beside
// it, its result is discarded, and its end starts the run whose result is
// the first served after resume.
func TestExternalReadsScaleCheckRunInFlightAtResumeNotServed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cityPath := t.TempDir()
		lane, runs, end := suspendSpanLane(t, cityPath)
		writeSuspension(t, cityPath, `{"city":{"suspended":true}}`)
		// The first pass waits out the source deadline on run 1; the next,
		// suspended, pass comes a patrol after.
		advanceBackstop(externalReadsSourceDeadline + backstopTestInterval)
		writeSuspension(t, cityPath, "")
		lane.wake()
		advanceBackstop(externalReadsMinGap)
		if got := runs.Load(); got != 1 {
			t.Fatalf("after resume with run 1 in flight: %d scale_check runs, want 1 (one run in flight per source)", got)
		}
		end(1)
		synctest.Wait()
		if r := lane.recording().scaleCheck(time.Now()); r != nil {
			t.Fatalf("run 1 ended after resume: %+v served (it started before the suspension), want partial", r)
		}
		advanceBackstop(externalReadsMinGap)
		if got := runs.Load(); got != 2 {
			t.Fatalf("after run 1 ended: %d scale_check runs, want 2 (its end starts a new run)", got)
		}
		if r := lane.recording().scaleCheck(time.Now()); r != nil {
			t.Fatalf("run 2 in flight after run 1 ended: %+v served (run 1 started before the suspension), want partial", r)
		}
		end(2)
		synctest.Wait()
		if r := lane.recording().scaleCheck(time.Now()); r == nil || r.Counts["w"] != 2 {
			t.Fatalf("after run 2 ended: %+v, want w=2", r)
		}
	})
}

// Kills a newer scale_check run that does not wake the planner (PAR-SC
// re-review): the decide's claim subtraction is measured from the run's
// start, so a run that started after a claim ends the subtraction even when
// it publishes the same counts. Its publish wakes the planner; a pass that
// publishes the same run again does not.
func TestExternalReadsNewScaleCheckRunWakesThePlanner(t *testing.T) {
	ctx := context.Background()
	cfg := &config.City{Agents: []config.Agent{{Name: "worker", ScaleCheck: "constant"}}}
	lane, wakes := newTestBackstopLane(externalReadsEnv{CityName: "city", Cfg: cfg})
	lane.steps = nil
	lane.queryEnv = func(string, *config.City, *config.Agent) (map[string]string, error) { return nil, nil }
	lane.runner = func(string, string, map[string]string) (string, error) { return "2", nil }
	t0 := censusNow
	if !passAndJoin(ctx, at(lane, t0)) {
		t.Fatal("pass 1 declined")
	}
	before := wakes.Load()
	if !passAndJoin(ctx, at(lane, t0.Add(2*time.Second))) {
		t.Fatal("pass 2 declined")
	}
	if wakes.Load() != before+1 {
		t.Fatalf("a newer run with the same counts woke the planner %d times, want once", wakes.Load()-before)
	}
	rec := lane.recording()
	same := &externalReadsRecording{Seq: rec.Seq + 1, FreshFor: rec.FreshFor, Sources: rec.Sources}
	if same.wakes(rec, t0.Add(2*time.Second)) {
		t.Fatal("republishing the same run would wake the planner, want not")
	}
}

// Kills a reassigned claim kept at its first sight (PAR-SC re-review): a
// claim is its bead and assignee, so a new assignee is first seen when the
// pass sees it, an unchanged one keeps its first sight, and a claim no
// longer held is forgotten.
func TestClaimsSeenAtReassignedClaimIsNew(t *testing.T) {
	p := newPlanner(realPlannerClock{}, func() time.Duration { return time.Minute }, nil, newInflightMap(), nil, io.Discard)
	view := func(assignees ...string) demandView {
		var d demandView
		for i, a := range assignees {
			d.AssignedWork = append(d.AssignedWork, beads.Bead{ID: fmt.Sprintf("w-%d", i+1), Assignee: a, Status: "in_progress"})
			d.AssignedStoreRefs = append(d.AssignedStoreRefs, "")
		}
		return d
	}
	t0, t1, t2 := censusNow, censusNow.Add(time.Second), censusNow.Add(2*time.Second)
	claim := func(id, assignee string) workClaim {
		return workClaim{bead: storeScopedBeadKey{StoreRef: normalizeDemandStoreRef(""), ID: id}, assignee: assignee}
	}
	if seen, fresh := p.claimsSeenAt(view("gc-1", "gc-2"), t0); !fresh || !seen[claim("w-1", "gc-1")].Equal(t0) {
		t.Fatalf("first pass: %v, new %t; want both seen at %v", seen, fresh, t0)
	}
	seen, fresh := p.claimsSeenAt(view("gc-3", "gc-2"), t1)
	if !fresh || !seen[claim("w-1", "gc-3")].Equal(t1) || !seen[claim("w-2", "gc-2")].Equal(t0) {
		t.Fatalf("w-1 reassigned to gc-3: %v, new %t; want w-1 seen at %v, w-2 still at %v", seen, fresh, t1, t0)
	}
	if _, ok := seen[claim("w-1", "gc-1")]; ok {
		t.Fatal("the old assignee's claim is still held after the reassignment")
	}
	if seen, fresh := p.claimsSeenAt(view("gc-3"), t2); fresh || len(seen) != 1 || !seen[claim("w-1", "gc-3")].Equal(t1) {
		t.Fatalf("w-2 released: %v, new %t; want only w-1 at %v, nothing new", seen, fresh, t1)
	}
}

// Kills a claim stamped before the work read (PAR-SC review): the gather
// stamps a first sight when its work read has shown the claim, not at the
// pass's start. A run that started during the read, after the pass began,
// counted w-1 as unassigned; the read then shows gc-3's claim on it, so the
// claim is newer than the run and is cut, and no session is planned for
// work a session holds.
func TestScaleCheckClaimStampedAfterTheWorkRead(t *testing.T) {
	cityPath := t.TempDir()
	cfg := diffConfig(customPool("worker", "count-ready", 4))
	claimed := beads.Bead{
		ID: "w-1", Title: "w-1", Type: "task", Status: "in_progress", Assignee: "gc-3", CreatedAt: diffNow.Add(-time.Hour),
		Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "worker"},
	}
	cache, _ := newDemandCache(t, false, poolRow("gc-1", "worker", 1, "active"), poolRow("gc-3", "worker", 2, "active"), claimed)
	passStart := diffNow
	runStart := passStart.Add(50 * time.Millisecond)
	rec := &externalReadsRecording{Seq: 1, FreshFor: time.Minute, Sources: map[sourceKey]sourceResult{
		{kind: sourceScaleCheck, cfg: cfg}: {StartedAt: runStart, EndedAt: runStart.Add(10 * time.Millisecond), sourcePayload: sourcePayload{ScaleCheck: &scaleCheckResult{Counts: map[string]int{"worker": 1}}}},
	}}
	clk := &settableClock{at: passStart.Add(100 * time.Millisecond)} // after the work read
	p := newPlanner(clk, func() time.Duration { return time.Minute }, nil, newInflightMap(), nil, io.Discard)
	var wakes atomic.Int64
	d := claimSkewDecide(t, p, cfg, cityPath, cache, []string{"s-gc-1", "s-gc-3"}, rec, passStart, &wakes)
	if len(d.Plans) != 0 || !slices.Contains(d.Snapshot.Partial.Templates["worker"].Causes, causeScaleCheckStaleClaim) {
		t.Fatalf("a claim the read showed after the run started: %d create(s), causes %v; want none, cut",
			len(d.Plans), d.Snapshot.Partial.Templates["worker"].Causes)
	}
}
