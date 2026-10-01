package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/pathutil"
	"github.com/gastownhall/gascity/internal/runtime"
)

// I5: the main reconcile consumes the count computed by the real build.
// With a live external worker at cap one, a blind fallback reports demand 1.
func TestBeadReconcileTickUsesBuildPoolCountForExternalWorker(t *testing.T) {
	_, rigRoot := initReapRig(t)
	workDir := filepath.Join(t.TempDir(), "external")
	if err := os.MkdirAll(filepath.Dir(workDir), 0o755); err != nil {
		t.Fatal(err)
	}
	mustGit(t, rigRoot, "worktree", "add", "-b", "external-reconcile", workDir)
	injectLiveness(t, liveWorktreeState{scanned: true, cwds: []string{pathutil.NormalizePathForCompare(workDir)}})
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs:      []config.Rig{{Name: reapTestRigName, Path: rigRoot}},
		Agents:    []config.Agent{poolAgent("worker", "", intPtr(1), 0)},
	}
	store := beads.NewMemStore()
	for _, bead := range []beads.Bead{
		{Title: "queued", Type: "task", Status: "open", Metadata: map[string]string{"gc.routed_to": "worker"}},
		{Title: "external", Type: "task", Status: "in_progress", Metadata: map[string]string{
			"gc.routed_to": "worker", beadmeta.WorkDirMetadataKey: workDir,
		}},
	} {
		if _, err := store.Create(bead); err != nil {
			t.Fatal(err)
		}
	}
	snapshot := newSessionBeadSnapshot(nil)
	build := buildDesiredStateWithSessionBeads("test-city", t.TempDir(), time.Now().UTC(),
		cfg, runtime.NewFake(), store, nil, snapshot, nil, io.Discard)
	if got := len(build.State); got != 0 {
		t.Fatalf("build desired sessions = %d, want 0 while external worker fills cap", got)
	}
	trace := newPoolDesiredStateTestTrace("worker")
	cr := &CityRuntime{
		cityPath: t.TempDir(), cityName: "test-city", cfg: cfg, sp: runtime.NewFake(),
		standaloneCityStore: store,
		storageRoutes:       &storageRoutes{stores: map[coordclass.Class]beads.Store{coordclass.ClassSessions: store}},
		sessionDrains:       newDrainTracker(), rec: events.Discard, stdout: io.Discard, stderr: io.Discard,
	}
	cr.beadReconcileTick(context.Background(), build, snapshot, trace, false)
	for _, rec := range trace.records {
		if rec.RecordType == TraceRecordTemplateTickSummary && rec.Template == "worker" {
			if got := rec.Fields["pool_desired"]; got != 0 {
				t.Errorf("reconcile pool desired = %v, want build's 0", got)
			}
			return
		}
	}
	t.Error("missing worker reconcile summary")
}

// I5/I6: the fresh build's pool count is the count passed to the awake
// decision. A liveness-blind second compute keeps an idle gc session awake
// alongside a live external worker at a cap of one.
func TestDemandSnapshotUsesBuildPoolCountForExternalWorkerAwakeDecision(t *testing.T) {
	const template = "worker"
	_, rigRoot := initReapRig(t)
	workDir := filepath.Join(t.TempDir(), "external-awake")
	mustGit(t, rigRoot, "worktree", "add", "-b", "external-awake", workDir)
	injectLiveness(t, liveWorktreeState{
		scanned: true, cwds: []string{pathutil.NormalizePathForCompare(workDir)},
	})
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs:      []config.Rig{{Name: reapTestRigName, Path: rigRoot}},
		Agents:    []config.Agent{poolAgent("worker", "", intPtr(1), 0)},
	}
	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{
		Title: "queued", Type: "task", Status: "open",
		Metadata: map[string]string{"gc.routed_to": template},
	}); err != nil {
		t.Fatal(err)
	}
	external, err := store.Create(beads.Bead{
		Title: "external", Type: "task", Status: "in_progress", Assignee: "human",
		Metadata: map[string]string{"gc.routed_to": template, beadmeta.WorkDirMetadataKey: workDir},
	})
	if err != nil {
		t.Fatal(err)
	}
	buildCalls := 0
	cityPath := t.TempDir()
	cr := &CityRuntime{
		cityName: "test-city", cityPath: cityPath, cfg: cfg, sp: runtime.NewFake(),
		cs: &controllerState{
			cityName: "test-city", cityPath: cityPath,
			cityBeadStore: store, eventProv: events.NewFake(),
		}, stderr: io.Discard,
	}
	cr.buildFnWithSessionBeads = func(cfg *config.City, sp runtime.Provider, store beads.Store,
		rigStores map[string]beads.Store, sessionBeads *sessionBeadSnapshot,
		trace *sessionReconcilerTraceCycle,
	) DesiredStateResult {
		buildCalls++
		return buildDesiredStateWithSessionBeads("test-city", cityPath, time.Now().UTC(),
			cfg, sp, store, rigStores, sessionBeads, trace, io.Discard)
	}
	awake := func(counts map[string]int, assigned []AwakeWorkBead) map[string]AwakeDecision {
		return ComputeAwakeSet(AwakeInput{
			Agents: []AwakeAgent{{QualifiedName: template}},
			SessionBeads: []AwakeSessionBead{{
				ID: "session-s", SessionName: "worker-s", Template: template, State: "active",
			}},
			WorkBeads: assigned, ScaleCheckCounts: counts, Now: time.Now(),
		})
	}
	first := cr.loadDemandSnapshot(newSessionBeadSnapshot(nil), nil, "patrol", false)
	if got := first.result.PoolDesiredCounts[template]; got != 0 {
		t.Errorf("pool desired with live external worker = %d, want 0", got)
	}
	assertAsleep(t, awake(first.result.PoolDesiredCounts, nil), "worker-s")
	if buildCalls != 1 {
		t.Fatalf("fresh build calls = %d, want 1", buildCalls)
	}

	// A stable event-backed snapshot does not rebuild or create a pool row.
	second := cr.loadDemandSnapshot(newSessionBeadSnapshot(nil), nil, "patrol", false)
	if buildCalls != 1 || second.result.PoolDesiredCounts[template] != 0 {
		t.Errorf("cache hit: builds=%d pool desired=%v, want one build and zero demand", buildCalls, second.result.PoolDesiredCounts)
	}

	closed := "closed"
	if err := store.Update(external.ID, beads.UpdateOpts{Status: &closed}); err != nil {
		t.Fatal(err)
	}
	third := cr.loadDemandSnapshot(newSessionBeadSnapshot(nil), nil, "poke", false)
	if got := third.result.PoolDesiredCounts[template]; got != 1 {
		t.Errorf("pool desired after external exit = %d, want 1", got)
	}
	assertReason(t, awake(third.result.PoolDesiredCounts, nil), "worker-s", "scaled:demand")
	assertReason(t, awake(map[string]int{template: 0}, []AwakeWorkBead{{
		ID: "owned", Assignee: "session-s", Status: "in_progress", Ready: true,
	}}), "worker-s", "assigned-work")
}

// I8 / ga-ymfa3l: a refresh with no unowned worktree candidate has no need
// for a process-table scan. This is the separate lazy-gather follow-up.
func TestBuildDesiredStateWithoutExternalCandidateSkipsLivenessGather(t *testing.T) {
	cityPath, rigRoot := initReapRig(t)
	cfg := reapTestConfig(rigRoot)
	cfg.Agents = []config.Agent{poolAgent("worker", reapTestRigName, intPtr(1), 0)}
	scans := injectCountedLiveness(t, liveWorktreeState{scanned: true})
	_ = buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg,
		runtime.NewFake(), beads.NewMemStore(), io.Discard)
	if *scans != 0 {
		t.Errorf("liveness gathers with no external candidate = %d, want 0 (ga-ymfa3l)", *scans)
	}
}

// I8: a fresh build shares one scan; reusing its snapshot repeats neither the
// scan nor the pool-session create side effect.
func TestDemandSnapshotHitDoesNotRescanOrCreatePoolSession(t *testing.T) {
	_, rigRoot := initReapRig(t)
	workDir := filepath.Join(t.TempDir(), "external-snapshot")
	mustGit(t, rigRoot, "worktree", "add", "-b", "external-snapshot", workDir)
	scans := injectCountedLiveness(t, liveWorktreeState{
		scanned: true, cwds: []string{pathutil.NormalizePathForCompare(workDir)},
	})
	const template = "worker"
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs:      []config.Rig{{Name: reapTestRigName, Path: rigRoot}},
		Agents:    []config.Agent{poolAgent(template, "", intPtr(1), 0)},
	}
	store := beads.NewMemStore()
	for _, bead := range []beads.Bead{
		{Title: "queued", Type: "task", Status: "open", Metadata: map[string]string{"gc.routed_to": template}},
		{Title: "external", Type: "task", Status: "in_progress", Assignee: "human", Metadata: map[string]string{
			"gc.routed_to": template, beadmeta.WorkDirMetadataKey: workDir,
		}},
	} {
		if _, err := store.Create(bead); err != nil {
			t.Fatal(err)
		}
	}
	cityPath := t.TempDir()
	cr := &CityRuntime{
		cityName: "test-city", cityPath: cityPath, cfg: cfg, sp: runtime.NewFake(), stderr: io.Discard,
		cs: &controllerState{cityName: "test-city", cityPath: cityPath, cityBeadStore: store, eventProv: events.NewFake()},
	}
	cr.buildFnWithSessionBeads = func(cfg *config.City, sp runtime.Provider, store beads.Store,
		rigStores map[string]beads.Store, sessionBeads *sessionBeadSnapshot,
		trace *sessionReconcilerTraceCycle,
	) DesiredStateResult {
		return buildDesiredStateWithSessionBeads("test-city", cityPath, time.Now().UTC(),
			cfg, sp, store, rigStores, sessionBeads, trace, io.Discard)
	}
	snapshot := newSessionBeadSnapshot(nil)
	_ = cr.loadDemandSnapshot(snapshot, nil, "patrol", false)
	if *scans != 1 {
		t.Fatalf("fresh snapshot liveness gathers = %d, want 1", *scans)
	}
	_ = cr.loadDemandSnapshot(snapshot, nil, "patrol", false)
	if *scans != 1 {
		t.Errorf("cache hit liveness gathers = %d, want still 1", *scans)
	}
	sessions, err := store.ListByLabel(sessionBeadLabel, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 0 {
		t.Errorf("pool sessions after cache hit = %d, want 0 while external work occupies cap", len(sessions))
	}
}

// I5: one-shot start must use the build's external-aware count when deciding
// whether an already-running idle pool session remains awake.
func TestStartStandaloneUsesBuildPoolCountForExternalWorker(t *testing.T) {
	clearInheritedBeadsEnv(t)
	t.Chdir(t.TempDir())
	cityPath := t.TempDir()
	_, rigRoot := initReapRig(t)
	workDir := filepath.Join(t.TempDir(), "external-start")
	mustGit(t, rigRoot, "worktree", "add", "-b", "external-start", workDir)
	injectLiveness(t, liveWorktreeState{
		scanned: true, cwds: []string{pathutil.NormalizePathForCompare(workDir)},
	})
	cityTOML := fmt.Sprintf(`[workspace]
name = "test-city"
provider = "shell"

[providers.shell]
command = "echo"

[beads]
provider = "file"

[[agent]]
name = "worker"
scope = "city"
max_active_sessions = 1
sleep_after_idle = "0s"
attach = false

[[rigs]]
name = %q
path = %q
prefix = "mr"
`, reapTestRigName, rigRoot)
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityTOML), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ensureScopedFileStoreLayout(cityPath); err != nil {
		t.Fatal(err)
	}
	if err := ensurePersistedScopeLocalFileStore(cityPath); err != nil {
		t.Fatal(err)
	}
	store, err := openScopeLocalFileStoreForCity(cityPath, cityPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, bead := range []beads.Bead{
		{Title: "queued", Type: "task", Status: "open", Metadata: map[string]string{"gc.routed_to": "worker"}},
		{Title: "external", Type: "task", Status: "in_progress", Metadata: map[string]string{
			"gc.routed_to": "worker", beadmeta.WorkDirMetadataKey: workDir,
		}},
		{
			Title: "idle worker", Type: sessionBeadType, Status: "open", Labels: []string{sessionBeadLabel, "template:worker"},
			Metadata: map[string]string{
				"template": "worker", "session_name": "worker-s", "state": "active",
				"pool_slot": "1", poolManagedMetadataKey: boolMetadata(true),
			},
		},
	} {
		if _, err := store.Create(bead); err != nil {
			t.Fatal(err)
		}
	}
	fake := runtime.NewFake()
	if err := fake.Start(context.Background(), "worker-s", runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	previous := buildSessionProviderByName
	t.Cleanup(func() { buildSessionProviderByName = previous })
	buildSessionProviderByName = func(*config.City, string, config.SessionConfig, string, string) (runtime.Provider, error) {
		return fake, nil
	}
	var stdout, stderr bytes.Buffer
	if code := doStartStandalone([]string{cityPath}, false, &stdout, &stderr); code != 0 {
		t.Fatalf("standalone start exit = %d; stderr=%s", code, stderr.String())
	}
	sessions, err := loadSessionBeads(store)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 {
		t.Fatalf("standalone start session beads = %d, want the existing worker only; stdout=%q stderr=%q", len(sessions), stdout.String(), stderr.String())
	}
	if got := sessions[0].Metadata["config_wake_suppressed"]; got != "true" {
		t.Errorf("idle worker config_wake_suppressed = %q, want true while external work occupies cap; stdout=%q stderr=%q", got, stdout.String(), stderr.String())
	}
}
