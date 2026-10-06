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
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
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

// newScaleCheckSourceLane returns a lane whose only source is the
// scale_check one (no city store, so no leg), running checks with a scripted
// runner and a probe env that fails for the noenv pool.
func newScaleCheckSourceLane(cfg *config.City, checks *scriptedScaleChecks) (*externalReadsLane, *atomic.Int64) {
	lane, wakes := newTestBackstopLane(externalReadsEnv{CityName: "city", Cfg: cfg})
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
	lane, wakes := newScaleCheckSourceLane(scaleCheckCity(), checks)
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
