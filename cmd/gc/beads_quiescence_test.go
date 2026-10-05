package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/suspensionstate"
)

// quiescenceRuntime is a CityRuntime over providerOwnedHealthFixture's city:
// one provider-owned rig whose provider script logs every op.
func quiescenceRuntime(t *testing.T, rigExtra string) (*CityRuntime, *runtime.Fake, string, string) {
	t.Helper()
	city, rig, logPath := providerOwnedHealthFixture(t, rigExtra)
	cfg, err := loadCityConfig(city, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	resolveRigPaths(city, cfg.Rigs)
	sp := runtime.NewFake()
	return &CityRuntime{cityPath: city, cfg: cfg, sp: sp, stdout: io.Discard, stderr: io.Discard}, sp, rig, logPath
}

// A suspended city with nothing running is quiescent: the tick stops before
// its phases, every cache pauses, and the provider-owned scopes are stopped
// once. A session still running keeps it out of quiescence.
func TestBeadsQuiescenceRetiresASuspendedCityOnceItHasDrained(t *testing.T) {
	cr, sp, _, logPath := quiescenceRuntime(t, "")
	t.Setenv("GC_SUSPENDED", "1")
	cr.cs = &controllerState{beadsQuiescent: new(atomic.Bool)}

	if err := sp.Start(context.Background(), "city--worker", runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	if cr.enterBeadsQuiescenceIfDue(context.Background()) {
		t.Fatal("quiescent while a session is still running")
	}
	if ops := providerOpsLogged(t, logPath); ops != "" {
		t.Fatalf("scopes stopped before the sessions drained: %q", ops)
	}

	if err := sp.Stop("city--worker"); err != nil {
		t.Fatal(err)
	}
	if !cr.enterBeadsQuiescenceIfDue(context.Background()) {
		t.Fatal("a suspended city with nothing running is not quiescent")
	}
	if !cr.beadsQuiescent.Load() || !cr.cs.beadsQuiescent.Load() {
		t.Fatal("quiescence was not published to the runtime and its caches")
	}
	if ops := providerOpsLogged(t, logPath); ops != "stop" {
		t.Fatalf("provider ops = %q, want one stop", ops)
	}
	cr.enterBeadsQuiescenceIfDue(context.Background())
	if ops := providerOpsLogged(t, logPath); ops != "stop" {
		t.Fatalf("provider ops after a second quiescent tick = %q, want still one stop", ops)
	}

	t.Setenv("GC_SUSPENDED", "")
	if cr.enterBeadsQuiescenceIfDue(context.Background()) || cr.cs.beadsQuiescent.Load() {
		t.Fatal("a resumed city is still quiescent")
	}
	if cr.retiredScopes.done(cr.beadsScopeRoots()[1], time.Time{}) {
		t.Fatal("a resumed scope is still recorded as retired")
	}
}

// A suspended rig's pair is stopped once its sessions have drained, and only
// then.
func TestRetireSuspendedRigScopeAfterItsSessionsDrain(t *testing.T) {
	cr, sp, rig, logPath := quiescenceRuntime(t, "suspended_on_start = true\n")
	t.Setenv("GC_SUSPENDED", "")
	p := &tickPass{ctx: context.Background(), sessionBeads: newSessionBeadSnapshotFromInfos([]sessionpkg.Info{{
		ID: "gc-1", SessionName: "city--r1-worker", WorkDir: filepath.Join(rig, "work"), State: "active",
	}})}
	if err := sp.Start(context.Background(), "city--r1-worker", runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	cr.tickRetireSuspendedRigScopes(p)
	if ops := providerOpsLogged(t, logPath); ops != "" {
		t.Fatalf("the rig was stopped while its session ran: %q", ops)
	}
	if err := sp.Stop("city--r1-worker"); err != nil {
		t.Fatal(err)
	}
	cr.tickRetireSuspendedRigScopes(p)
	cr.tickRetireSuspendedRigScopes(p)
	if ops := providerOpsLogged(t, logPath); ops != "stop" {
		t.Fatalf("provider ops = %q, want exactly one stop", ops)
	}
}

// A suspended rig's store is not primed: any read would go to bd and restart
// its retired proxy.
func TestWrapWithCachingStoreLeavesASuspendedRigCold(t *testing.T) {
	backing := &primeCountingStore{Store: beads.NewMemStore()}
	wrapWithCachingStore(context.Background(), backing, nil, false)
	if backing.lists != 0 {
		t.Fatalf("a suspended rig's store was listed %d time(s) at wrap", backing.lists)
	}
}

type primeCountingStore struct {
	beads.Store
	lists int
}

func (s *primeCountingStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	s.lists++
	return s.Store.List(q)
}

// The order-tracking watchdogs and `gc order sweep-tracking` leave a
// suspended rig alone.
func TestOrderTrackingSweepTargetsSkipSuspendedRigs(t *testing.T) {
	cr, _, _, _ := quiescenceRuntime(t, "suspended_on_start = true\n")
	t.Setenv("GC_SUSPENDED", "")
	for _, target := range orderTrackingSweepTargetsForConfig(cr.cityPath, cr.cfg) {
		if strings.Contains(target.label, "r1") {
			t.Fatalf("sweep targets include the suspended rig: %+v", target)
		}
	}
}

// The core maintenance orders enumerate rigs through scope_bd.sh and their own
// jq filters; every one of them leaves suspended rigs out.
func TestCoreMaintenanceScriptsSkipSuspendedRigs(t *testing.T) {
	dir := filepath.Join(repoRootForLint(t), "internal", "bootstrap", "packs", "core", "assets", "scripts")
	for _, name := range []string{"scope_bd.sh", "orphan-sweep.sh", "renudge-stale-human-gates.sh"} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), ".suspended") {
			t.Errorf("%s enumerates rigs without skipping suspended ones", name)
		}
	}
}

// A resume and a new suspension that no tick observed in between still stop
// the scope again: the retirement belongs to the suspension episode.
func TestRetireSuspendedRigScopeAgainInANewEpisode(t *testing.T) {
	cr, _, _, logPath := quiescenceRuntime(t, "")
	t.Setenv("GC_SUSPENDED", "")
	p := &tickPass{ctx: context.Background(), sessionBeads: newSessionBeadSnapshotFromInfos(nil)}
	suspend := func(v bool) {
		t.Helper()
		if err := suspensionstate.SetRigSuspended(fsys.OSFS{}, cr.cityPath, "r1", &v); err != nil {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond) // distinct UpdatedAt stamps
	}
	suspend(true)
	cr.tickRetireSuspendedRigScopes(p)
	suspend(false)
	suspend(true)
	cr.tickRetireSuspendedRigScopes(p)
	if ops := providerOpsLogged(t, logPath); ops != "stop\nstop" {
		t.Fatalf("provider ops = %q, want a stop per suspension episode", ops)
	}
}

// The completions sweep leaves a suspended rig's store out of its fan.
func TestCompletionReconcileInputsSkipSuspendedRigs(t *testing.T) {
	t.Setenv("GC_SUSPENDED", "")
	cs := &controllerState{
		cityPath: t.TempDir(),
		cfg: &config.City{
			Workspace: config.Workspace{Name: "test-city"},
			Rigs:      []config.Rig{{Name: "alpha", Path: "alpha"}, {Name: "beta", Path: "beta", SuspendedOnStart: true}},
		},
		cityBeadStore: beads.NewMemStore(),
		beadStores:    map[string]beads.Store{"alpha": beads.NewMemStore(), "beta": beads.NewMemStore()},
		eventProv:     events.NewFake(),
	}
	_, fan := cs.completionReconcileInputs(reconcilePlane)
	if len(fan) != 2 {
		t.Fatalf("the sweep fans out to %d store(s), want 2 (city work + the unsuspended rig)", len(fan))
	}
}

// gc start's is_blocked repair leaves suspended scopes cold, and the
// controller runs it for a scope on the first tick after that scope resumes.
func TestBlockedRepairSkipsSuspendedScopesAndRunsOnResume(t *testing.T) {
	cr, _, rig, _ := quiescenceRuntime(t, "")
	t.Setenv("GC_SUSPENDED", "")
	scopes := []blockedRepairScope{{id: "city", root: cr.cityPath}, {id: "rig/r1", root: rig}}

	suspendRig := func(v bool) {
		t.Helper()
		if err := suspensionstate.SetRigSuspended(fsys.OSFS{}, cr.cityPath, "r1", &v); err != nil {
			t.Fatal(err)
		}
	}
	suspendRig(true)
	kept := withoutSuspendedRepairScopes(scopes, suspendedBeadsScopes(cr.cityPath, cr.cfg))
	if len(kept) != 1 || kept[0].id != "city" {
		t.Fatalf("start repair scopes = %+v, want only the city", kept)
	}

	oldRepair, oldCandidates := resumeRepairBlockedFlags, resumeRepairCandidates
	t.Cleanup(func() { resumeRepairBlockedFlags, resumeRepairCandidates = oldRepair, oldCandidates })
	resumeRepairCandidates = func(string, *config.City) []blockedRepairScope { return scopes }
	var repaired []string
	resumeRepairBlockedFlags = func(_ string, _ *config.City, got []blockedRepairScope, _ io.Writer, _ string) {
		for _, s := range got {
			repaired = append(repaired, s.id)
		}
	}
	cr.enterBeadsQuiescenceIfDue(context.Background()) // first tick: rig suspended
	cr.enterBeadsQuiescenceIfDue(context.Background()) // still suspended
	if len(repaired) != 0 {
		t.Fatalf("repaired %v while the rig was suspended", repaired)
	}
	suspendRig(false)
	cr.enterBeadsQuiescenceIfDue(context.Background())
	cr.enterBeadsQuiescenceIfDue(context.Background())
	if strings.Join(repaired, ",") != "rig/r1" {
		t.Fatalf("repaired %v after resume, want exactly rig/r1 once", repaired)
	}
}
