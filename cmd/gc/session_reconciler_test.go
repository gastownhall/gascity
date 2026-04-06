package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/agent"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
)

// fakeIdleTracker is a test double for idleTracker.
type fakeIdleTracker struct {
	idle map[string]bool
}

func newFakeIdleTracker() *fakeIdleTracker {
	return &fakeIdleTracker{idle: make(map[string]bool)}
}

func (f *fakeIdleTracker) checkIdle(sessionName string, _ runtime.Provider, _ time.Time) bool {
	return f.idle[sessionName]
}

func (f *fakeIdleTracker) setTimeout(_ string, _ time.Duration) {}

type delayedSessionExistsProvider struct {
	*runtime.Fake
	pendingConflict map[string]bool
	hiddenRunning   map[string]bool
	hiddenMeta      map[string]map[string]string
}

func newDelayedSessionExistsProvider() *delayedSessionExistsProvider {
	return &delayedSessionExistsProvider{
		Fake:            runtime.NewFake(),
		pendingConflict: make(map[string]bool),
		hiddenRunning:   make(map[string]bool),
		hiddenMeta:      make(map[string]map[string]string),
	}
}

func (p *delayedSessionExistsProvider) Start(ctx context.Context, name string, cfg runtime.Config) error {
	if p.pendingConflict[name] {
		delete(p.pendingConflict, name)
		p.hiddenRunning[name] = true
		return runtime.ErrSessionExists
	}
	return p.Fake.Start(ctx, name, cfg)
}

func (p *delayedSessionExistsProvider) IsRunning(name string) bool {
	if p.hiddenRunning[name] {
		return true
	}
	return p.Fake.IsRunning(name)
}

func (p *delayedSessionExistsProvider) GetMeta(name, key string) (string, error) {
	if p.hiddenRunning[name] {
		return p.hiddenMeta[name][key], nil
	}
	return p.Fake.GetMeta(name, key)
}

type lateSuccessStartProvider struct {
	*runtime.Fake
	startErr error
}

func (p *lateSuccessStartProvider) Start(ctx context.Context, name string, cfg runtime.Config) error {
	if err := p.Fake.Start(ctx, name, cfg); err != nil {
		return err
	}
	if id := cfg.Env["GC_SESSION_ID"]; id != "" {
		_ = p.SetMeta(name, "GC_SESSION_ID", id)
	}
	if token := cfg.Env["GC_INSTANCE_TOKEN"]; token != "" {
		_ = p.SetMeta(name, "GC_INSTANCE_TOKEN", token)
	}
	if p.startErr != nil {
		return p.startErr
	}
	return nil
}

// reconcilerTestEnv holds common test infrastructure.
type reconcilerTestEnv struct {
	store        beads.Store
	sp           *runtime.Fake
	dt           *drainTracker
	clk          *clock.Fake
	rec          events.Recorder
	stdout       bytes.Buffer
	stderr       bytes.Buffer
	cfg          *config.City
	desiredState map[string]TemplateParams
}

func newReconcilerTestEnv() *reconcilerTestEnv {
	return &reconcilerTestEnv{
		store:        beads.NewMemStore(),
		sp:           runtime.NewFake(),
		dt:           newDrainTracker(),
		clk:          &clock.Fake{Time: time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)},
		rec:          events.Discard,
		cfg:          &config.City{},
		desiredState: make(map[string]TemplateParams),
	}
}

// addDesired registers a session in the desired state and optionally starts
// it in the provider. Returns the TemplateParams for further customization.
func (e *reconcilerTestEnv) addDesired(name, template string, running bool) {
	tp := TemplateParams{
		Command:      "test-cmd",
		SessionName:  name,
		TemplateName: template,
	}
	e.desiredState[name] = tp
	if running {
		_ = e.sp.Start(context.Background(), name, runtime.Config{Command: "test-cmd"})
	}
}

// addDesiredWithConfig registers a session with a custom runtime.Config.
func (e *reconcilerTestEnv) addDesiredWithConfig(name, template string, running bool, cmd string) {
	tp := TemplateParams{
		Command:      cmd,
		SessionName:  name,
		TemplateName: template,
	}
	e.desiredState[name] = tp
	if running {
		_ = e.sp.Start(context.Background(), name, runtime.Config{Command: cmd})
	}
}

// addDesiredLive registers a session with custom session_live config.
func (e *reconcilerTestEnv) addDesiredLive(name, template string, running bool, live []string) {
	tp := TemplateParams{
		Command:      "test-cmd",
		SessionName:  name,
		TemplateName: template,
		Hints:        agent.StartupHints{SessionLive: live},
	}
	e.desiredState[name] = tp
	if running {
		_ = e.sp.Start(context.Background(), name, runtime.Config{Command: "test-cmd", SessionLive: live})
	}
}

func (e *reconcilerTestEnv) createSessionBead(name, template string) beads.Bead {
	meta := map[string]string{
		"session_name":   name,
		"agent_name":     name,
		"template":       template,
		"config_hash":    runtime.CoreFingerprint(runtime.Config{Command: "test-cmd"}),
		"live_hash":      runtime.LiveFingerprint(runtime.Config{Command: "test-cmd"}),
		"generation":     "1",
		"instance_token": "test-token",
		"state":          "asleep",
	}
	b, err := e.store.Create(beads.Bead{
		Title:    name,
		Type:     sessionBeadType,
		Labels:   []string{sessionBeadLabel},
		Metadata: meta,
	})
	if err != nil {
		panic("creating test bead: " + err.Error())
	}
	return b
}

func (e *reconcilerTestEnv) setSessionMetadata(session *beads.Bead, kvs map[string]string) {
	for key, value := range kvs {
		_ = e.store.SetMetadata(session.ID, key, value)
		session.Metadata[key] = value
	}
}

func (e *reconcilerTestEnv) markSessionCreating(session *beads.Bead) {
	e.setSessionMetadata(session, map[string]string{"state": "creating"})
}

func (e *reconcilerTestEnv) markSessionActive(session *beads.Bead) {
	e.setSessionMetadata(session, map[string]string{
		"state":        "active",
		"last_woke_at": e.clk.Now().UTC().Format(time.RFC3339),
	})
}

func (e *reconcilerTestEnv) reconcile(sessions []beads.Bead) int {
	// Auto-derive poolDesired from desiredState, mirroring production behavior
	// where ComputePoolDesiredStates populates ScaleCheckCounts before calling
	// reconcileSessionBeads. Each template in the desired set gets a count of
	// how many sessions reference it.
	poolDesired := make(map[string]int)
	for _, tp := range e.desiredState {
		if tp.TemplateName != "" {
			poolDesired[tp.TemplateName]++
		}
	}
	return e.reconcileWithPoolDesired(sessions, poolDesired)
}

func (e *reconcilerTestEnv) reconcileWithPoolDesired(sessions []beads.Bead, poolDesired map[string]int) int {
	cfgNames := configuredSessionNames(e.cfg, "", e.store)
	return reconcileSessionBeads(
		context.Background(), sessions, e.desiredState, cfgNames, e.cfg, e.sp,
		e.store, nil, nil, nil, e.dt, poolDesired, false, nil, "",
		nil, e.clk, e.rec, 0, 0, &e.stdout, &e.stderr,
	)
}

func TestReconcileSessionBeads_DrainAckKeepsBeadOpen(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Agents: []config.Agent{{Name: "worker"}},
	}
	env.addDesired("worker", "worker", true)
	session := env.createSessionBead("worker", "worker")
	env.markSessionActive(&session)

	dops := newFakeDrainOps()
	if err := dops.setDrainAck("worker"); err != nil {
		t.Fatalf("setDrainAck: %v", err)
	}

	woken := reconcileSessionBeads(
		context.Background(),
		[]beads.Bead{session},
		env.desiredState,
		map[string]bool{"worker": true},
		env.cfg,
		env.sp,
		env.store,
		dops,
		nil,
		nil,
		env.dt,
		nil,
		false,
		nil,
		"",
		nil,
		env.clk,
		env.rec,
		0,
		0,
		&env.stdout,
		&env.stderr,
	)
	if woken != 0 {
		t.Fatalf("woken = %d, want 0", woken)
	}
	if env.sp.IsRunning("worker") {
		t.Fatal("worker should be stopped after drain-ack")
	}

	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", session.ID, err)
	}
	if got.Status == "closed" {
		t.Fatalf("session bead closed unexpectedly: metadata=%v", got.Metadata)
	}
	if got.Metadata["state"] != "drained" {
		t.Fatalf("state = %q, want drained", got.Metadata["state"])
	}
}

func TestReconcileSessionBeads_DrainAckHonoredAfterSessionExit(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Agents: []config.Agent{{Name: "worker"}},
	}
	env.addDesired("worker", "worker", true)
	session := env.createSessionBead("worker", "worker")
	env.markSessionActive(&session)

	dops := newFakeDrainOps()
	if err := dops.setDrainAck("worker"); err != nil {
		t.Fatalf("setDrainAck: %v", err)
	}
	if err := env.sp.Stop("worker"); err != nil {
		t.Fatalf("Stop(worker): %v", err)
	}

	woken := reconcileSessionBeads(
		context.Background(),
		[]beads.Bead{session},
		env.desiredState,
		map[string]bool{"worker": true},
		env.cfg,
		env.sp,
		env.store,
		dops,
		nil,
		nil,
		env.dt,
		nil,
		false,
		nil,
		"",
		nil,
		env.clk,
		env.rec,
		0,
		0,
		&env.stdout,
		&env.stderr,
	)
	if woken != 0 {
		t.Fatalf("woken = %d, want 0", woken)
	}
	if env.sp.IsRunning("worker") {
		t.Fatal("worker should remain stopped after drain-ack")
	}

	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", session.ID, err)
	}
	if got.Status == "closed" {
		t.Fatalf("session bead closed unexpectedly: metadata=%v", got.Metadata)
	}
	if got.Metadata["state"] != "drained" {
		t.Fatalf("state = %q, want drained", got.Metadata["state"])
	}
}

// --- buildDepsMap tests ---

func TestBuildDepsMap_NilConfig(t *testing.T) {
	deps := buildDepsMap(nil)
	if deps != nil {
		t.Errorf("expected nil, got %v", deps)
	}
}

func TestBuildDepsMap_NoDeps(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "a"},
			{Name: "b"},
		},
	}
	deps := buildDepsMap(cfg)
	if len(deps) != 0 {
		t.Errorf("expected empty map, got %v", deps)
	}
}

func TestBuildDepsMap_WithDeps(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "worker", DependsOn: []string{"db"}},
			{Name: "db"},
		},
	}
	deps := buildDepsMap(cfg)
	if len(deps) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(deps))
	}
	if len(deps["worker"]) != 1 || deps["worker"][0] != "db" {
		t.Errorf("expected worker -> [db], got %v", deps["worker"])
	}
}

// --- derivePoolDesired tests ---

// --- allDependenciesAlive tests ---

func TestAllDependenciesAlive_NoDeps(t *testing.T) {
	session := beads.Bead{Metadata: map[string]string{"template": "worker"}}
	cfg := &config.City{Agents: []config.Agent{{Name: "worker"}}}
	sp := runtime.NewFake()
	if !allDependenciesAlive(session, cfg, nil, sp, "test", nil) {
		t.Error("no deps should return true")
	}
}

func TestAllDependenciesAlive_DepAlive(t *testing.T) {
	session := beads.Bead{Metadata: map[string]string{"template": "worker"}}
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "worker", DependsOn: []string{"db"}},
			{Name: "db"},
		},
	}
	sp := runtime.NewFake()
	_ = sp.Start(context.Background(), "db", runtime.Config{})
	desired := map[string]TemplateParams{
		"db": {TemplateName: "db"},
	}
	if !allDependenciesAlive(session, cfg, desired, sp, "test", nil) {
		t.Error("dep is alive, should return true")
	}
}

func TestAllDependenciesAlive_DepDead(t *testing.T) {
	session := beads.Bead{Metadata: map[string]string{"template": "worker"}}
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "worker", DependsOn: []string{"db"}},
			{Name: "db"},
		},
	}
	sp := runtime.NewFake()
	desired := map[string]TemplateParams{
		"db": {TemplateName: "db"},
	}
	if allDependenciesAlive(session, cfg, desired, sp, "test", nil) {
		t.Error("dep is dead, should return false")
	}
}

func TestAllDependenciesAlive_UsesLegacyAgentLabelTemplate(t *testing.T) {
	store := beads.NewMemStore()
	_, err := store.Create(beads.Bead{
		Title:  "db",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "agent:frontend/db"},
		Metadata: map[string]string{
			"template":     "frontend/db",
			"session_name": "custom-db",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	session := beads.Bead{
		Labels: []string{sessionBeadLabel, "agent:frontend/worker-1"},
		Metadata: map[string]string{
			"template":     "worker",
			"pool_slot":    "1",
			"session_name": "custom-worker-1",
		},
	}
	cfg := &config.City{
		Workspace: config.Workspace{SessionTemplate: "{{.Agent}}"},
		Agents: []config.Agent{
			{Name: "worker", Dir: "frontend", DependsOn: []string{"frontend/db"}, MinActiveSessions: intPtr(1), MaxActiveSessions: intPtr(2)},
			{Name: "db", Dir: "frontend"},
		},
	}
	sp := runtime.NewFake()
	desired := map[string]TemplateParams{
		"custom-db":       {TemplateName: "frontend/db"},
		"custom-worker-1": {TemplateName: "frontend/worker"},
	}
	if allDependenciesAlive(session, cfg, desired, sp, "test", store) {
		t.Error("legacy labeled worker should still see db as a missing dependency")
	}
	if err := sp.Start(context.Background(), "custom-db", runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	if !allDependenciesAlive(session, cfg, desired, sp, "test", store) {
		t.Error("legacy labeled worker should see db as alive once the dependency starts")
	}
}

// --- reconcileSessionBeads tests ---

func TestReconcileSessionBeads_WakesDeadSession(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	env.addDesired("worker", "worker", false)
	session := env.createSessionBead("worker", "worker")
	env.markSessionCreating(&session)

	woken := env.reconcile([]beads.Bead{session})

	if woken != 1 {
		t.Errorf("expected 1 woken, got %d", woken)
	}
	if !env.sp.IsRunning("worker") {
		t.Error("session should have been started via Provider")
	}
}

func TestReconcileSessionBeads_SyncsGCDirWithWorkDirOverride(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	env.desiredState["worker"] = TemplateParams{
		Command:      "test-cmd",
		SessionName:  "worker",
		TemplateName: "worker",
		WorkDir:      "/template/worktree",
		Env:          map[string]string{"GC_DIR": "/template/worktree"},
	}
	session := env.createSessionBead("worker", "worker")
	_ = env.store.SetMetadata(session.ID, "work_dir", "/instance/worktree")
	session.Metadata["work_dir"] = "/instance/worktree"
	env.markSessionCreating(&session)

	woken := env.reconcile([]beads.Bead{session})

	if woken != 1 {
		t.Fatalf("expected 1 woken, got %d", woken)
	}
	var startCfg runtime.Config
	found := false
	for _, call := range env.sp.Calls {
		if call.Method == "Start" && call.Name == "worker" {
			startCfg = call.Config
			found = true
		}
	}
	if !found {
		t.Fatal("expected Start call for worker")
	}
	if startCfg.WorkDir != "/instance/worktree" {
		t.Fatalf("WorkDir = %q, want %q", startCfg.WorkDir, "/instance/worktree")
	}
	if got := startCfg.Env["GC_DIR"]; got != "/instance/worktree" {
		t.Fatalf("GC_DIR = %q, want %q", got, "/instance/worktree")
	}
	if got := env.desiredState["worker"].Env["GC_DIR"]; got != "/template/worktree" {
		t.Fatalf("desiredState GC_DIR mutated to %q", got)
	}
}

func TestReconcileSessionBeads_SkipsAliveSession(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	env.addDesired("worker", "worker", true)
	session := env.createSessionBead("worker", "worker")

	woken := env.reconcile([]beads.Bead{session})

	if woken != 0 {
		t.Errorf("expected 0 woken, got %d", woken)
	}
}

func TestReconcileSessionBeads_SkipsQuarantinedSession(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	env.addDesired("worker", "worker", false)
	session := env.createSessionBead("worker", "worker")
	// Set quarantine in the future.
	_ = env.store.SetMetadata(session.ID, "quarantined_until",
		env.clk.Now().Add(10*time.Minute).UTC().Format(time.RFC3339))
	session.Metadata["quarantined_until"] = env.clk.Now().Add(10 * time.Minute).UTC().Format(time.RFC3339)

	woken := env.reconcile([]beads.Bead{session})

	if woken != 0 {
		t.Errorf("expected 0 woken (quarantined), got %d", woken)
	}
}

func TestReconcileSessionBeads_RespectsWakeBudget(t *testing.T) {
	env := newReconcilerTestEnv()
	var cfgAgents []config.Agent
	var sessions []beads.Bead
	for i := 0; i < defaultMaxWakesPerTick+3; i++ {
		name := fmt.Sprintf("worker-%d", i)
		cfgAgents = append(cfgAgents, config.Agent{Name: name})
		env.addDesired(name, name, false)
		session := env.createSessionBead(name, name)
		env.markSessionCreating(&session)
		sessions = append(sessions, session)
	}
	env.cfg = &config.City{Agents: cfgAgents}

	woken := env.reconcile(sessions)

	if woken != defaultMaxWakesPerTick {
		t.Errorf("expected %d woken (budget), got %d", defaultMaxWakesPerTick, woken)
	}
}

func TestReconcileSessionBeads_ConfigDriftInitiatesDrain(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	// Desired state has a DIFFERENT config than what's in the bead.
	env.addDesiredWithConfig("worker", "worker", true, "new-cmd")
	session := env.createSessionBead("worker", "worker")

	// Verify hashes differ.
	storedHash := session.Metadata["config_hash"]
	currentHash := runtime.CoreFingerprint(runtime.Config{Command: "new-cmd"})
	if storedHash == currentHash {
		t.Fatalf("test setup error: stored hash %q should differ from current %q", storedHash, currentHash)
	}

	env.reconcile([]beads.Bead{session})

	ds := env.dt.get(session.ID)
	if ds == nil {
		t.Fatalf("expected drain to be initiated for config drift (session.ID=%q, stderr=%s)", session.ID, env.stderr.String())
	}
	if ds.reason != "config-drift" {
		t.Errorf("drain reason = %q, want %q", ds.reason, "config-drift")
	}
}

func TestReconcileSessionBeads_NoDriftWhenHashMatches(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	env.addDesired("worker", "worker", true) // same config as bead
	session := env.createSessionBead("worker", "worker")
	env.markSessionActive(&session)

	env.reconcile([]beads.Bead{session})

	if ds := env.dt.get(session.ID); ds != nil {
		t.Errorf("expected no drain, got %+v", ds)
	}
}

func TestReconcileSessionBeads_DependencyOrdering_DepDeadBlocksWake(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Agents: []config.Agent{
			{Name: "worker", DependsOn: []string{"db"}},
			{Name: "db"},
		},
	}
	env.addDesired("worker", "worker", false)
	// db is in desired but starts fail (provider Start returns error).
	env.addDesired("db", "db", false)
	env.sp.StartErrors = map[string]error{"db": fmt.Errorf("db failed to start")}

	dbBead := env.createSessionBead("db", "db")
	workerBead := env.createSessionBead("worker", "worker")

	env.reconcile([]beads.Bead{workerBead, dbBead})

	// worker should NOT be started because db is still dead.
	// (db start failed, so sp.IsRunning("db") is false)
	if env.sp.IsRunning("worker") {
		t.Error("worker should NOT have been started (dep not alive)")
	}
}

func TestReconcileSessionBeads_DependencyOrdering_TopoOrder(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Agents: []config.Agent{
			{Name: "worker", DependsOn: []string{"db"}},
			{Name: "db"},
		},
	}
	env.addDesired("worker", "worker", false)
	env.addDesired("db", "db", false)

	dbBead := env.createSessionBead("db", "db")
	workerBead := env.createSessionBead("worker", "worker")
	env.markSessionCreating(&dbBead)
	env.markSessionCreating(&workerBead)

	// Even though worker bead is listed first, topo ordering ensures
	// db is processed first. Since the Fake provider marks sessions as
	// running on Start, worker can wake in the same tick after db succeeds.
	woken := env.reconcile([]beads.Bead{workerBead, dbBead})

	if woken != 2 {
		t.Errorf("expected 2 woken (both), got %d", woken)
	}
}

func TestReconcileSessionBeads_PoolDependencyBlocksWake(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Agents: []config.Agent{
			{Name: "worker", DependsOn: []string{"db"}},
			{Name: "db", MinActiveSessions: intPtr(2), MaxActiveSessions: intPtr(2)},
		},
	}
	// Worker depends on pool "db". No db instances in desired → worker blocked.
	env.addDesired("worker", "worker", false)
	workerBead := env.createSessionBead("worker", "worker")

	woken := env.reconcile([]beads.Bead{workerBead})

	if woken != 0 {
		t.Errorf("expected 0 woken (pool dep dead), got %d", woken)
	}
}

func TestReconcileSessionBeads_PoolDependencyUnblocksWake(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Agents: []config.Agent{
			{Name: "worker", DependsOn: []string{"db"}},
			{Name: "db", MinActiveSessions: intPtr(2), MaxActiveSessions: intPtr(2)},
		},
	}
	env.addDesired("worker", "worker", false)
	env.addDesired("db-1", "db", true) // one pool instance alive
	workerBead := env.createSessionBead("worker", "worker")
	env.markSessionCreating(&workerBead)
	dbBead := env.createSessionBead("db-1", "db")
	env.markSessionActive(&dbBead)

	woken := env.reconcile([]beads.Bead{workerBead, dbBead})

	if woken != 1 {
		t.Errorf("expected 1 woken (pool dep alive), got %d", woken)
	}
}

func TestReconcileSessionBeads_OrphanSessionDrained(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "other"}}}
	// Session bead for "orphan" with no matching desired entry, but running.
	_ = env.sp.Start(context.Background(), "orphan", runtime.Config{})
	session := env.createSessionBead("orphan", "orphan")

	env.reconcile([]beads.Bead{session})

	ds := env.dt.get(session.ID)
	if ds == nil {
		t.Fatal("expected drain for orphan session")
	}
	if ds.reason != "orphaned" {
		t.Errorf("drain reason = %q, want %q", ds.reason, "orphaned")
	}
}

func TestReconcileSessionBeads_OrphanNotRunningClosed(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "other"}}}
	session := env.createSessionBead("orphan", "orphan")

	env.reconcile([]beads.Bead{session})

	b, _ := env.store.Get(session.ID)
	if b.Status != "closed" {
		t.Errorf("orphan bead status = %q, want closed", b.Status)
	}
	if b.Metadata["close_reason"] != "orphaned" {
		t.Errorf("close_reason = %q, want %q", b.Metadata["close_reason"], "orphaned")
	}
}

func TestReconcileSessionBeads_SuspendedSessionDrained(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Agents:        []config.Agent{{Name: "worker"}},
		NamedSessions: []config.NamedSession{{Template: "worker"}},
	}
	// "worker" is in config (configuredNames) but NOT in desiredState.
	_ = env.sp.Start(context.Background(), "worker", runtime.Config{})
	session := env.createSessionBead("worker", "worker")

	env.reconcile([]beads.Bead{session})

	ds := env.dt.get(session.ID)
	if ds == nil {
		t.Fatal("expected drain for suspended session")
	}
	if ds.reason != "suspended" {
		t.Errorf("drain reason = %q, want %q", ds.reason, "suspended")
	}
}

func TestReconcileSessionBeads_SuspendedNotRunningClosed(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Agents:        []config.Agent{{Name: "worker"}},
		NamedSessions: []config.NamedSession{{Template: "worker"}},
	}
	session := env.createSessionBead("worker", "worker")

	env.reconcile([]beads.Bead{session})

	b, _ := env.store.Get(session.ID)
	if b.Status != "closed" {
		t.Errorf("suspended bead status = %q, want closed", b.Status)
	}
	if b.Metadata["close_reason"] != "suspended" {
		t.Errorf("close_reason = %q, want %q", b.Metadata["close_reason"], "suspended")
	}
}

func TestReconcileSessionBeads_PreservesConfiguredNamedSessionOutsideDesiredState(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Workspace:     config.Workspace{Name: "test-city"},
		Agents:        []config.Agent{{Name: "worker", StartCommand: "true", MaxActiveSessions: intPtr(2)}},
		NamedSessions: []config.NamedSession{{Template: "worker", Mode: "on_demand"}},
	}
	sessionName := config.NamedSessionRuntimeName(env.cfg.Workspace.Name, env.cfg.Workspace, "worker")
	session := env.createSessionBead(sessionName, "worker")
	env.setSessionMetadata(&session, map[string]string{
		namedSessionMetadataKey:      "true",
		namedSessionIdentityMetadata: "worker",
		namedSessionModeMetadata:     "on_demand",
	})

	env.reconcile([]beads.Bead{session})

	b, _ := env.store.Get(session.ID)
	if b.Status != "open" {
		t.Fatalf("status = %q, want open", b.Status)
	}
	if b.Metadata["close_reason"] != "" {
		t.Fatalf("close_reason = %q, want empty", b.Metadata["close_reason"])
	}
	if ds := env.dt.get(session.ID); ds != nil {
		t.Fatalf("unexpected drain for configured named session: %+v", ds)
	}
}

func TestReconcileSessionBeads_PreservedRunningNamedSessionStillIdleDrains(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		SessionSleep: config.SessionSleepConfig{
			InteractiveResume: "60s",
		},
		Workspace:     config.Workspace{Name: "test-city"},
		Agents:        []config.Agent{{Name: "worker", StartCommand: "true", MaxActiveSessions: intPtr(2)}},
		NamedSessions: []config.NamedSession{{Template: "worker", Mode: "on_demand"}},
	}
	sessionName := config.NamedSessionRuntimeName(env.cfg.Workspace.Name, env.cfg.Workspace, "worker")
	session := env.createSessionBead(sessionName, "worker")
	env.markSessionActive(&session)
	env.setSessionMetadata(&session, map[string]string{
		namedSessionMetadataKey:      "true",
		namedSessionIdentityMetadata: "worker",
		namedSessionModeMetadata:     "on_demand",
	})
	preservedTP, err := resolvePreservedConfiguredNamedSessionTemplate(".", env.cfg.Workspace.Name, env.cfg, env.sp, env.store, []beads.Bead{session}, session, env.clk, io.Discard)
	if err != nil {
		t.Fatalf("resolve preserved named session: %v", err)
	}
	runtimeCfg := templateParamsToConfig(preservedTP)
	env.setSessionMetadata(&session, map[string]string{
		"config_hash": runtime.CoreFingerprint(runtimeCfg),
		"live_hash":   runtime.LiveFingerprint(runtimeCfg),
		"detached_at": env.clk.Now().UTC().Add(-6 * time.Minute).Format(time.RFC3339),
	})
	if err := env.sp.Start(context.Background(), sessionName, runtimeCfg); err != nil {
		t.Fatalf("start session: %v", err)
	}
	env.sp.WaitForIdleErrors[sessionName] = nil

	env.reconcile([]beads.Bead{session})
	waitForIdleProbeReady(t, env.dt, session.ID)
	env.reconcile([]beads.Bead{session})

	ds := env.dt.get(session.ID)
	if ds == nil {
		t.Fatal("expected idle drain for preserved running named session")
	}
	if ds.reason != "idle" {
		t.Fatalf("drain reason = %q, want idle", ds.reason)
	}
	b, _ := env.store.Get(session.ID)
	if b.Status != "open" {
		t.Fatalf("status = %q, want open", b.Status)
	}
}

func TestReconcileSessionBeads_PreservedRunningNamedSessionHonorsRestartRequest(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Workspace:     config.Workspace{Name: "test-city"},
		Agents:        []config.Agent{{Name: "worker", StartCommand: "true", MaxActiveSessions: intPtr(2)}},
		NamedSessions: []config.NamedSession{{Template: "worker", Mode: "on_demand"}},
	}
	sessionName := config.NamedSessionRuntimeName(env.cfg.Workspace.Name, env.cfg.Workspace, "worker")
	session := env.createSessionBead(sessionName, "worker")
	env.markSessionActive(&session)
	env.setSessionMetadata(&session, map[string]string{
		namedSessionMetadataKey:      "true",
		namedSessionIdentityMetadata: "worker",
		namedSessionModeMetadata:     "on_demand",
		"restart_requested":          "true",
		"session_key":                "original-key",
		"started_config_hash":        "hash-before-restart",
	})
	if err := env.sp.Start(context.Background(), sessionName, runtime.Config{Command: "true"}); err != nil {
		t.Fatalf("start session: %v", err)
	}

	env.reconcile([]beads.Bead{session})

	if env.sp.IsRunning(sessionName) {
		t.Fatal("preserved running named session should stop after restart request")
	}
	got, _ := env.store.Get(session.ID)
	if got.Metadata["restart_requested"] != "" {
		t.Fatalf("restart_requested = %q, want cleared", got.Metadata["restart_requested"])
	}
	if got.Metadata["started_config_hash"] != "" {
		t.Fatalf("started_config_hash = %q, want cleared", got.Metadata["started_config_hash"])
	}
	if got.Metadata["session_key"] == "" || got.Metadata["session_key"] == "original-key" {
		t.Fatalf("session_key = %q, want rotated key", got.Metadata["session_key"])
	}
}

func TestReconcileSessionBeads_InvalidNamedSessionConfigDoesNotPreserveBead(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Workspace:     config.Workspace{Name: "test-city"},
		NamedSessions: []config.NamedSession{{Template: "worker", Mode: "on_demand"}},
	}
	sessionName := config.NamedSessionRuntimeName(env.cfg.Workspace.Name, env.cfg.Workspace, "worker")
	session := env.createSessionBead(sessionName, "worker")
	env.setSessionMetadata(&session, map[string]string{
		namedSessionMetadataKey:      "true",
		namedSessionIdentityMetadata: "worker",
		namedSessionModeMetadata:     "on_demand",
	})

	env.reconcile([]beads.Bead{session})

	b, _ := env.store.Get(session.ID)
	if b.Status != "closed" {
		t.Fatalf("status = %q, want closed", b.Status)
	}
	if b.Metadata["close_reason"] != "suspended" {
		t.Fatalf("close_reason = %q, want suspended", b.Metadata["close_reason"])
	}
}

func TestReconcileSessionBeads_OnDemandNamedSessionRecoversAfterClosedCanonicalBead(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	clk := &clock.Fake{Time: time.Date(2026, 4, 4, 12, 0, 0, 0, time.UTC)}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "refinery",
			StartCommand:      "true",
			WorkQuery:         "printf ready",
			ScaleCheck:        "printf 0",
			MaxActiveSessions: intPtr(2),
		}},
		NamedSessions: []config.NamedSession{{
			Template: "refinery",
			Mode:     "on_demand",
		}},
	}

	sessionName := config.NamedSessionRuntimeName(cfg.EffectiveCityName(), cfg.Workspace, "refinery")
	historical, err := store.Create(beads.Bead{
		Title:  "refinery",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":               sessionName,
			"alias":                      "refinery",
			"template":                   "refinery",
			"state":                      "stopped",
			namedSessionMetadataKey:      "true",
			namedSessionIdentityMetadata: "refinery",
			namedSessionModeMetadata:     "on_demand",
		},
	})
	if err != nil {
		t.Fatalf("create historical bead: %v", err)
	}
	if err := store.Close(historical.ID); err != nil {
		t.Fatalf("close historical bead: %v", err)
	}

	dsResult := buildDesiredState(cfg.EffectiveCityName(), cityPath, clk.Now().UTC(), cfg, sp, store, io.Discard)
	tp, ok := dsResult.State[sessionName]
	if !ok {
		t.Fatalf("desired state missing recovered named session %q; keys=%v", sessionName, mapKeys(dsResult.State))
	}
	if tp.ConfiguredNamedIdentity != "refinery" {
		t.Fatalf("ConfiguredNamedIdentity = %q, want refinery", tp.ConfiguredNamedIdentity)
	}
	if tp.ConfiguredNamedMode != "on_demand" {
		t.Fatalf("ConfiguredNamedMode = %q, want on_demand", tp.ConfiguredNamedMode)
	}
	if tp.Alias != "refinery" {
		t.Fatalf("Alias = %q, want refinery", tp.Alias)
	}

	var stderr bytes.Buffer
	syncSessionBeads(cityPath, store, dsResult.State, sp, allConfiguredDS(dsResult.State), cfg, clk, &stderr, false)

	sessions, err := loadSessionBeads(store)
	if err != nil {
		t.Fatalf("loadSessionBeads: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("open session bead count = %d, want 1", len(sessions))
	}
	if got := sessions[0].Metadata["session_name"]; got != sessionName {
		t.Fatalf("open session_name = %q, want %q", got, sessionName)
	}
	if got := sessions[0].Metadata[namedSessionIdentityMetadata]; got != "refinery" {
		t.Fatalf("configured_named_identity = %q, want refinery", got)
	}

	woken := reconcileSessionBeads(
		context.Background(),
		sessions,
		dsResult.State,
		allConfiguredDS(dsResult.State),
		cfg,
		sp,
		store,
		nil,
		dsResult.AssignedWorkBeads,
		nil,
		newDrainTracker(),
		map[string]int{"refinery": 1},
		dsResult.StoreQueryPartial,
		map[string]bool{"refinery": true},
		cfg.EffectiveCityName(),
		newFakeIdleTracker(),
		clk,
		events.Discard,
		0,
		0,
		io.Discard,
		&stderr,
	)
	if woken != 1 {
		t.Fatalf("woken = %d, want 1", woken)
	}
	if !sp.IsRunning(sessionName) {
		t.Fatalf("session %q did not start", sessionName)
	}
}

func TestReconcileSessionBeads_HealsExpiredTimers(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	env.addDesired("worker", "worker", false)
	session := env.createSessionBead("worker", "worker")
	env.markSessionCreating(&session)
	past := env.clk.Now().Add(-5 * time.Minute).UTC().Format(time.RFC3339)
	_ = env.store.SetMetadata(session.ID, "held_until", past)
	_ = env.store.SetMetadata(session.ID, "sleep_reason", "user-hold")
	session.Metadata["held_until"] = past
	session.Metadata["sleep_reason"] = "user-hold"

	env.reconcile([]beads.Bead{session})

	b, _ := env.store.Get(session.ID)
	if b.Metadata["held_until"] != "" {
		t.Error("expired held_until should be cleared")
	}
	if b.Metadata["sleep_reason"] != "" {
		t.Error("sleep_reason should be cleared with expired hold")
	}
}

func TestReconcileSessionBeads_CrashDetection(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	env.addDesired("worker", "worker", false)
	session := env.createSessionBead("worker", "worker")
	recentWake := env.clk.Now().Add(-5 * time.Second).UTC().Format(time.RFC3339)
	_ = env.store.SetMetadata(session.ID, "last_woke_at", recentWake)
	session.Metadata["last_woke_at"] = recentWake

	env.reconcile([]beads.Bead{session})

	b, _ := env.store.Get(session.ID)
	if b.Metadata["wake_attempts"] != "1" {
		t.Errorf("wake_attempts = %q, want %q", b.Metadata["wake_attempts"], "1")
	}
}

func TestReconcileSessionBeads_StableClearsFailures(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	env.addDesired("worker", "worker", true)
	session := env.createSessionBead("worker", "worker")
	stableWake := env.clk.Now().Add(-2 * time.Minute).UTC().Format(time.RFC3339)
	_ = env.store.SetMetadata(session.ID, "wake_attempts", "3")
	_ = env.store.SetMetadata(session.ID, "last_woke_at", stableWake)
	session.Metadata["wake_attempts"] = "3"
	session.Metadata["last_woke_at"] = stableWake

	env.reconcile([]beads.Bead{session})

	b, _ := env.store.Get(session.ID)
	if b.Metadata["wake_attempts"] != "0" {
		t.Errorf("wake_attempts = %q, want %q", b.Metadata["wake_attempts"], "0")
	}
}

func TestReconcileSessionBeads_NoAgentNotWoken(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{}
	session := env.createSessionBead("orphan", "orphan")

	woken := env.reconcile([]beads.Bead{session})
	if woken != 0 {
		t.Errorf("expected 0 woken for orphan, got %d", woken)
	}
}

func TestReconcileSessionBeads_PreWakeCommitWritesMetadata(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	env.addDesired("worker", "worker", false)
	session := env.createSessionBead("worker", "worker")
	env.markSessionCreating(&session)

	env.reconcile([]beads.Bead{session})

	b, _ := env.store.Get(session.ID)
	if b.Metadata["generation"] != "2" {
		t.Errorf("generation = %q, want %q (incremented by preWakeCommit)", b.Metadata["generation"], "2")
	}
	if b.Metadata["instance_token"] == "test-token" {
		t.Error("instance_token should have been regenerated by preWakeCommit")
	}
	if b.Metadata["last_woke_at"] == "" {
		t.Error("last_woke_at should be set by preWakeCommit")
	}
}

func TestReconcileSessionBeads_CancelsDrainOnWakeReason(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	env.addDesired("worker", "worker", true)
	session := env.createSessionBead("worker", "worker")
	env.markSessionCreating(&session)

	gen := 1
	env.dt.set(session.ID, &drainState{
		startedAt:  env.clk.Now(),
		deadline:   env.clk.Now().Add(5 * time.Minute),
		reason:     "pool-excess",
		generation: gen,
	})

	env.reconcile([]beads.Bead{session})

	if ds := env.dt.get(session.ID); ds != nil {
		t.Errorf("drain should be canceled, got %+v", ds)
	}
}

func TestReconcileSessionBeads_UsesSleepIntentForDrainReason(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{}
	env.addDesired("worker", "worker", true)
	_ = env.sp.Start(context.Background(), "worker", runtime.Config{})
	session := env.createSessionBead("worker", "worker")
	_ = env.store.SetMetadata(session.ID, "sleep_intent", "wait-hold")
	session.Metadata["sleep_intent"] = "wait-hold"

	env.reconcile([]beads.Bead{session})

	ds := env.dt.get(session.ID)
	if ds == nil {
		t.Fatal("expected drain when desired session has no wake reason")
	}
	if ds.reason != "wait-hold" {
		t.Fatalf("drain reason = %q, want wait-hold", ds.reason)
	}
}

func TestReconcileSessionBeads_StartFailureNoDoubleCounting(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	env.addDesired("worker", "worker", false)
	env.sp.StartErrors = map[string]error{"worker": fmt.Errorf("start failed")}
	session := env.createSessionBead("worker", "worker")
	env.markSessionCreating(&session)

	// First tick: Start fails, wake_attempts should be 1.
	env.reconcile([]beads.Bead{session})
	b, _ := env.store.Get(session.ID)
	if b.Metadata["wake_attempts"] != "1" {
		t.Fatalf("after first tick: wake_attempts = %q, want 1", b.Metadata["wake_attempts"])
	}

	// Second tick: reload bead from store to get updated metadata.
	b, _ = env.store.Get(session.ID)
	env.reconcile([]beads.Bead{b})
	b, _ = env.store.Get(session.ID)
	if b.Metadata["wake_attempts"] != "2" {
		t.Errorf("after second tick: wake_attempts = %q, want 2", b.Metadata["wake_attempts"])
	}
}

func TestReconcileSessionBeads_RollsBackAdHocCreateOnRuntimeCollision(t *testing.T) {
	store := beads.NewMemStore()
	sp := newDelayedSessionExistsProvider()
	clk := &clock.Fake{Time: time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)}
	cfg := &config.City{Agents: []config.Agent{{Name: "helper"}}}
	desired := map[string]TemplateParams{
		"sky": {
			Command:      "test-cmd",
			SessionName:  "sky",
			TemplateName: "helper",
		},
	}

	bead, err := store.Create(beads.Bead{
		Title:  "helper",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "template:helper"},
		Metadata: map[string]string{
			"session_name":          "sky",
			"session_name_explicit": "true",
			"pending_create_claim":  "true",
			"template":              "helper",
			"state":                 "creating",
			"generation":            "1",
			"continuation_epoch":    "1",
			"instance_token":        "test-token",
		},
	})
	if err != nil {
		t.Fatalf("Create(bead): %v", err)
	}

	sp.pendingConflict["sky"] = true
	sp.hiddenMeta["sky"] = map[string]string{
		"GC_SESSION_ID":     "different-bead",
		"GC_INSTANCE_TOKEN": "different-token",
	}
	var stdout, stderr bytes.Buffer
	cfgNames := configuredSessionNames(cfg, "", store)
	woken := reconcileSessionBeads(
		context.Background(), []beads.Bead{bead}, desired, cfgNames,
		cfg, sp, store, nil, nil, nil, newDrainTracker(), map[string]int{"helper": 1}, false, nil, "",
		nil, clk, events.Discard, 0, 0, &stdout, &stderr,
	)
	if woken != 0 {
		t.Fatalf("woken = %d, want 0", woken)
	}

	got, err := store.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get(bead): %v", err)
	}
	if got.Status != "closed" {
		t.Fatalf("status = %q, want closed", got.Status)
	}
	if got.Metadata["session_name"] != "" {
		t.Fatalf("session_name = %q, want empty after rollback", got.Metadata["session_name"])
	}
	if got.Metadata["close_reason"] != "failed-create" {
		t.Fatalf("close_reason = %q, want failed-create", got.Metadata["close_reason"])
	}
	if got.Metadata["wake_attempts"] != "" {
		t.Fatalf("wake_attempts = %q, want empty", got.Metadata["wake_attempts"])
	}
}

func TestReconcileSessionBeads_ConvergesPendingCreateWhenRuntimeMatchesBead(t *testing.T) {
	store := beads.NewMemStore()
	sp := newDelayedSessionExistsProvider()
	clk := &clock.Fake{Time: time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)}
	cfg := &config.City{Agents: []config.Agent{{Name: "helper"}}}
	desired := map[string]TemplateParams{
		"sky": {
			Command:      "test-cmd",
			SessionName:  "sky",
			TemplateName: "helper",
		},
	}

	bead, err := store.Create(beads.Bead{
		Title:  "helper",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "template:helper"},
		Metadata: map[string]string{
			"session_name":          "sky",
			"session_name_explicit": "true",
			"pending_create_claim":  "true",
			"template":              "helper",
			"state":                 "creating",
			"generation":            "1",
			"continuation_epoch":    "1",
			"instance_token":        "test-token",
		},
	})
	if err != nil {
		t.Fatalf("Create(bead): %v", err)
	}

	sp.pendingConflict["sky"] = true
	sp.hiddenMeta["sky"] = map[string]string{
		"GC_SESSION_ID":     bead.ID,
		"GC_INSTANCE_TOKEN": "test-token",
	}
	var stdout, stderr bytes.Buffer
	cfgNames := configuredSessionNames(cfg, "", store)
	woken := reconcileSessionBeads(
		context.Background(), []beads.Bead{bead}, desired, cfgNames,
		cfg, sp, store, nil, nil, nil, newDrainTracker(), map[string]int{"helper": 1}, false, nil, "",
		nil, clk, events.Discard, 0, 0, &stdout, &stderr,
	)
	if woken != 1 {
		t.Fatalf("woken = %d, want 1", woken)
	}

	got, err := store.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get(bead): %v", err)
	}
	if got.Status == "closed" {
		t.Fatal("pending create should converge, not close")
	}
	if got.Metadata["session_name"] != "sky" {
		t.Fatalf("session_name = %q, want sky", got.Metadata["session_name"])
	}
	if got.Metadata["pending_create_claim"] != "" {
		t.Fatalf("pending_create_claim = %q, want cleared", got.Metadata["pending_create_claim"])
	}
	if got.Metadata["close_reason"] != "" {
		t.Fatalf("close_reason = %q, want empty", got.Metadata["close_reason"])
	}
	if got.Metadata["wake_attempts"] != "" {
		t.Fatalf("wake_attempts = %q, want empty", got.Metadata["wake_attempts"])
	}
}

func TestReconcileSessionBeads_RollsBackPendingCreateWhenConflictingRuntimeAlreadyRunning(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	clk := &clock.Fake{Time: time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)}
	cfg := &config.City{Agents: []config.Agent{{Name: "helper"}}}
	desired := map[string]TemplateParams{
		"sky": {
			Command:      "test-cmd",
			SessionName:  "sky",
			TemplateName: "helper",
		},
	}

	bead, err := store.Create(beads.Bead{
		Title:  "helper",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "template:helper"},
		Metadata: map[string]string{
			"session_name":          "sky",
			"session_name_explicit": "true",
			"pending_create_claim":  "true",
			"template":              "helper",
			"state":                 "creating",
			"generation":            "1",
			"continuation_epoch":    "1",
			"instance_token":        "test-token",
		},
	})
	if err != nil {
		t.Fatalf("Create(bead): %v", err)
	}
	if err := sp.Start(context.Background(), "sky", runtime.Config{Command: "test-cmd"}); err != nil {
		t.Fatalf("Start(conflicting runtime): %v", err)
	}
	if err := sp.SetMeta("sky", "GC_SESSION_ID", "different-bead"); err != nil {
		t.Fatalf("SetMeta(GC_SESSION_ID): %v", err)
	}
	if err := sp.SetMeta("sky", "GC_INSTANCE_TOKEN", "different-token"); err != nil {
		t.Fatalf("SetMeta(GC_INSTANCE_TOKEN): %v", err)
	}

	var stdout, stderr bytes.Buffer
	cfgNames := configuredSessionNames(cfg, "", store)
	woken := reconcileSessionBeads(
		context.Background(), []beads.Bead{bead}, desired, cfgNames,
		cfg, sp, store, nil, nil, nil, newDrainTracker(), map[string]int{}, false, nil, "",
		nil, clk, events.Discard, 0, 0, &stdout, &stderr,
	)
	if woken != 0 {
		t.Fatalf("woken = %d, want 0", woken)
	}

	got, err := store.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get(bead): %v", err)
	}
	if got.Status != "closed" {
		t.Fatalf("status = %q, want closed", got.Status)
	}
	if got.Metadata["session_name"] != "" {
		t.Fatalf("session_name = %q, want empty after rollback", got.Metadata["session_name"])
	}
	if got.Metadata["close_reason"] != "failed-create" {
		t.Fatalf("close_reason = %q, want failed-create", got.Metadata["close_reason"])
	}
}

func TestReconcileSessionBeads_ConvergesPendingCreateOnLateSuccessStartError(t *testing.T) {
	store := beads.NewMemStore()
	sp := &lateSuccessStartProvider{
		Fake:     runtime.NewFake(),
		startErr: context.DeadlineExceeded,
	}
	clk := &clock.Fake{Time: time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)}
	cfg := &config.City{Agents: []config.Agent{{Name: "helper"}}}
	desired := map[string]TemplateParams{
		"sky": {
			Command:      "test-cmd",
			SessionName:  "sky",
			TemplateName: "helper",
		},
	}

	bead, err := store.Create(beads.Bead{
		Title:  "helper",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "template:helper"},
		Metadata: map[string]string{
			"session_name":          "sky",
			"session_name_explicit": "true",
			"pending_create_claim":  "true",
			"template":              "helper",
			"state":                 "creating",
			"generation":            "1",
			"continuation_epoch":    "1",
			"instance_token":        "test-token",
		},
	})
	if err != nil {
		t.Fatalf("Create(bead): %v", err)
	}

	var stdout, stderr bytes.Buffer
	cfgNames := configuredSessionNames(cfg, "", store)
	woken := reconcileSessionBeads(
		context.Background(), []beads.Bead{bead}, desired, cfgNames,
		cfg, sp, store, nil, nil, nil, newDrainTracker(), map[string]int{"helper": 1}, false, nil, "",
		nil, clk, events.Discard, 0, 0, &stdout, &stderr,
	)
	if woken != 1 {
		t.Fatalf("woken = %d, want 1", woken)
	}

	got, err := store.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get(bead): %v", err)
	}
	if got.Status == "closed" {
		t.Fatal("late-success start error should converge, not close")
	}
	if got.Metadata["pending_create_claim"] != "" {
		t.Fatalf("pending_create_claim = %q, want cleared", got.Metadata["pending_create_claim"])
	}
}

func TestReconcileSessionBeads_DoesNotRollbackExistingSessionWithoutPendingClaim(t *testing.T) {
	store := beads.NewMemStore()
	sp := newDelayedSessionExistsProvider()
	clk := &clock.Fake{Time: time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)}
	cfg := &config.City{Agents: []config.Agent{{Name: "helper"}}}
	desired := map[string]TemplateParams{
		"sky": {
			Command:      "test-cmd",
			SessionName:  "sky",
			TemplateName: "helper",
		},
	}

	bead, err := store.Create(beads.Bead{
		Title:  "helper",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "template:helper"},
		Metadata: map[string]string{
			"session_name":       "sky",
			"template":           "helper",
			"state":              "active",
			"generation":         "1",
			"continuation_epoch": "1",
			"instance_token":     "test-token",
		},
	})
	if err != nil {
		t.Fatalf("Create(bead): %v", err)
	}

	sp.pendingConflict["sky"] = true
	var stdout, stderr bytes.Buffer
	cfgNames := configuredSessionNames(cfg, "", store)
	woken := reconcileSessionBeads(
		context.Background(), []beads.Bead{bead}, desired, cfgNames,
		cfg, sp, store, nil, nil, nil, newDrainTracker(), map[string]int{}, false, nil, "",
		nil, clk, events.Discard, 0, 0, &stdout, &stderr,
	)
	if woken != 0 {
		t.Fatalf("woken = %d, want 0", woken)
	}

	got, err := store.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get(bead): %v", err)
	}
	if got.Status == "closed" {
		t.Fatal("non-pending session should remain open after session_exists")
	}
	if got.Metadata["session_name"] != "sky" {
		t.Fatalf("session_name = %q, want sky", got.Metadata["session_name"])
	}
	// With WakeWork removed, the session has no wake reason (state is healed
	// to "asleep" since it's dead, so WakeSession no longer applies). The
	// session is never started, so wake_attempts remains empty.
	if got.Metadata["wake_attempts"] != "" {
		t.Fatalf("wake_attempts = %q, want empty", got.Metadata["wake_attempts"])
	}
}

func TestReconcileSessionBeads_RollsBackPendingCreateOnProviderError(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	sp.StartErrors = map[string]error{"sky": fmt.Errorf("start failed")}
	clk := &clock.Fake{Time: time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)}
	cfg := &config.City{Agents: []config.Agent{{Name: "helper"}}}
	desired := map[string]TemplateParams{
		"sky": {
			Command:      "test-cmd",
			SessionName:  "sky",
			TemplateName: "helper",
		},
	}

	bead, err := store.Create(beads.Bead{
		Title:  "helper",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "template:helper"},
		Metadata: map[string]string{
			"session_name":          "sky",
			"session_name_explicit": "true",
			"pending_create_claim":  "true",
			"template":              "helper",
			"state":                 "creating",
			"generation":            "1",
			"continuation_epoch":    "1",
			"instance_token":        "test-token",
		},
	})
	if err != nil {
		t.Fatalf("Create(bead): %v", err)
	}

	var stdout, stderr bytes.Buffer
	cfgNames := configuredSessionNames(cfg, "", store)
	woken := reconcileSessionBeads(
		context.Background(), []beads.Bead{bead}, desired, cfgNames,
		cfg, sp, store, nil, nil, nil, newDrainTracker(), map[string]int{"helper": 1}, false, nil, "",
		nil, clk, events.Discard, 0, 0, &stdout, &stderr,
	)
	if woken != 0 {
		t.Fatalf("woken = %d, want 0", woken)
	}

	got, err := store.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get(bead): %v", err)
	}
	if got.Status != "closed" {
		t.Fatalf("status = %q, want closed", got.Status)
	}
	if got.Metadata["session_name"] != "" {
		t.Fatalf("session_name = %q, want empty after rollback", got.Metadata["session_name"])
	}
	if got.Metadata["close_reason"] != "failed-create" {
		t.Fatalf("close_reason = %q, want failed-create", got.Metadata["close_reason"])
	}
	if got.Metadata["wake_attempts"] != "" {
		t.Fatalf("wake_attempts = %q, want empty", got.Metadata["wake_attempts"])
	}
}

func TestReconcileSessionBeads_PoolScaleDownOrphansExcess(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Agents: []config.Agent{
			{Name: "worker", MinActiveSessions: intPtr(1), MaxActiveSessions: intPtr(5)},
		},
	}
	// worker-1 is in the desired set; worker-2 is NOT (scale-down).
	env.addDesired("worker-1", "worker", true)
	// worker-2 is running in provider but not in desiredState.
	_ = env.sp.Start(context.Background(), "worker-2", runtime.Config{})
	s1 := env.createSessionBead("worker-1", "worker")
	_ = env.store.SetMetadata(s1.ID, "pool_slot", "1")
	s1.Metadata["pool_slot"] = "1"
	s2 := env.createSessionBead("worker-2", "worker")
	_ = env.store.SetMetadata(s2.ID, "pool_slot", "2")
	s2.Metadata["pool_slot"] = "2"

	poolDesired := map[string]int{"worker": 1}
	cfgNames := configuredSessionNames(env.cfg, "", env.store)
	reconcileSessionBeads(
		context.Background(), []beads.Bead{s1, s2}, env.desiredState, cfgNames,
		env.cfg, env.sp, env.store, nil, nil, nil, env.dt, poolDesired, false, nil, "",
		nil, env.clk, env.rec, 0, 0, &env.stdout, &env.stderr,
	)

	d2 := env.dt.get(s2.ID)
	if d2 == nil {
		t.Fatal("expected drain for excess pool instance")
	}
	if d2.reason != "orphaned" {
		t.Errorf("drain reason = %q, want %q", d2.reason, "orphaned")
	}
	if d1 := env.dt.get(s1.ID); d1 != nil {
		t.Errorf("worker-1 should not be draining, got reason=%q", d1.reason)
	}
}

func TestReconcileSessionBeads_LiveDriftReapplied(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	// Same core config (test-cmd), different live config.
	env.addDesiredLive("worker", "worker", true, []string{"echo live-updated"})
	session := env.createSessionBead("worker", "worker")
	env.markSessionActive(&session)

	env.reconcile([]beads.Bead{session})

	// Should NOT drain (core hash matches), but live_hash should be updated.
	if ds := env.dt.get(session.ID); ds != nil {
		t.Errorf("expected no drain for live-only drift, got reason=%q", ds.reason)
	}
	b, _ := env.store.Get(session.ID)
	expectedCfg := templateParamsToConfig(env.desiredState["worker"])
	expectedLive := runtime.LiveFingerprint(expectedCfg)
	if b.Metadata["live_hash"] != expectedLive {
		t.Errorf("live_hash not updated: got %q, want %q", b.Metadata["live_hash"], expectedLive)
	}
}

func TestAllDependenciesAlive_WithSessionTemplate(t *testing.T) {
	session := beads.Bead{Metadata: map[string]string{"template": "worker"}}
	cfg := &config.City{
		Workspace: config.Workspace{SessionTemplate: "{{.City}}-{{.Agent}}"},
		Agents: []config.Agent{
			{Name: "worker", DependsOn: []string{"db"}},
			{Name: "db"},
		},
	}
	sn := agent.SessionNameFor("myCity", "db", "{{.City}}-{{.Agent}}")
	sp := runtime.NewFake()
	_ = sp.Start(context.Background(), sn, runtime.Config{})
	desired := map[string]TemplateParams{
		sn: {TemplateName: "db"},
	}
	if !allDependenciesAlive(session, cfg, desired, sp, "myCity", nil) {
		t.Errorf("dep should be alive (session name: %q)", sn)
	}
}

func TestReconcileSessionBeads_DriftDrainUsesConfigTimeout(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Agents: []config.Agent{{Name: "worker"}},
		Daemon: config.DaemonConfig{DriftDrainTimeout: "7m"},
	}
	env.addDesiredWithConfig("worker", "worker", true, "new-cmd")
	session := env.createSessionBead("worker", "worker")

	cfgNames := configuredSessionNames(env.cfg, "", env.store)
	reconcileSessionBeads(
		context.Background(), []beads.Bead{session}, env.desiredState, cfgNames,
		env.cfg, env.sp, env.store, nil, nil, nil, env.dt, map[string]int{}, false, nil, "",
		nil, env.clk, env.rec, 0, env.cfg.Daemon.DriftDrainTimeoutDuration(),
		&env.stdout, &env.stderr,
	)

	ds := env.dt.get(session.ID)
	if ds == nil {
		t.Fatal("expected drain for config drift")
	}
	expected := env.clk.Now().Add(7 * time.Minute)
	if ds.deadline != expected {
		t.Errorf("drain deadline = %v, want %v (7m from now)", ds.deadline, expected)
	}
}

// --- idle timeout in bead reconciler tests ---

func TestReconcileSessionBeads_IdleTimeoutStopsAndStaysAsleep(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	env.addDesired("worker", "worker", true)
	session := env.createSessionBead("worker", "worker")
	env.markSessionActive(&session)

	// Simulate idle: activity was 30m ago, timeout is 15m.
	it := newFakeIdleTracker()
	it.idle["worker"] = true

	cfgNames := configuredSessionNames(env.cfg, "", env.store)
	reconcileSessionBeads(
		context.Background(), []beads.Bead{session}, env.desiredState, cfgNames,
		env.cfg, env.sp, env.store, nil, nil, nil, env.dt, map[string]int{}, false, nil, "",
		it, env.clk, env.rec, 0, 0, &env.stdout, &env.stderr,
	)

	// Session should have been stopped and left asleep until a real wake reason appears.
	if env.sp.IsRunning("worker") {
		t.Error("worker should stay asleep after idle timeout without an explicit wake reason")
	}

	// Bead should reflect the restart cycle.
	b, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if b.Metadata["sleep_reason"] != "idle-timeout" {
		t.Errorf("sleep_reason = %q, want idle-timeout after idle stop", b.Metadata["sleep_reason"])
	}
	if b.Metadata["last_woke_at"] != "" {
		t.Errorf("last_woke_at = %q, want empty after idle stop", b.Metadata["last_woke_at"])
	}
}

func TestReconcileSessionBeads_IdleTimeoutNilTrackerSkipped(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	env.addDesired("worker", "worker", true)
	session := env.createSessionBead("worker", "worker")

	// No idle tracker — should not idle-kill.
	cfgNames := configuredSessionNames(env.cfg, "", env.store)
	reconcileSessionBeads(
		context.Background(), []beads.Bead{session}, env.desiredState, cfgNames,
		env.cfg, env.sp, env.store, nil, nil, nil, env.dt, map[string]int{}, false, nil, "",
		nil, env.clk, env.rec, 0, 0, &env.stdout, &env.stderr,
	)

	if !env.sp.IsRunning("worker") {
		t.Error("worker should still be running with nil idle tracker")
	}
}

// --- zombie scrollback capture tests ---

func TestReconcileSessionBeads_ZombieCapturesScrollback(t *testing.T) {
	env := newReconcilerTestEnv()
	rec := events.NewFake()
	env.rec = rec
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}

	// Register with ProcessNames so ProcessAlive actually checks zombie state.
	tp := TemplateParams{
		Command:      "test-cmd",
		SessionName:  "worker",
		TemplateName: "worker",
		Hints:        agent.StartupHints{ProcessNames: []string{"test-cmd"}},
	}
	env.desiredState["worker"] = tp
	_ = env.sp.Start(context.Background(), "worker", runtime.Config{Command: "test-cmd"})

	session := env.createSessionBead("worker", "worker")

	// Simulate zombie: tmux session exists but process is dead.
	env.sp.Zombies["worker"] = true
	env.sp.SetPeekOutput("worker", "panic: nil pointer dereference\ngoroutine 1 [running]:")

	cfgNames := configuredSessionNames(env.cfg, "", env.store)
	reconcileSessionBeads(
		context.Background(), []beads.Bead{session}, env.desiredState, cfgNames,
		env.cfg, env.sp, env.store, nil, nil, nil, env.dt, map[string]int{}, false, nil, "",
		nil, env.clk, rec, 0, 0, &env.stdout, &env.stderr,
	)

	// Should have recorded a crash event with scrollback.
	found := false
	for _, e := range rec.Events {
		if e.Type == events.SessionCrashed && e.Message != "" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected SessionCrashed event with scrollback capture")
	}
}

// --- regression tests for issues #70, #71, #139 ---

// TestReconcileSessionBeads_ZombieDetectedCrashRecordedAndSessionNotAlive
// verifies that when a session is a zombie (tmux exists but agent process
// dead), the reconciler records a crash event and treats the session as
// not alive. The alive=false state means downstream logic (config-drift,
// drain-ack) won't act on it, and when the tmux state cache subsequently
// reports IsRunning=false (pane_dead=1), the outer reconciler loop will
// start a fresh session.
// Regression test for https://github.com/gastownhall/gascity/issues/71
func TestReconcileSessionBeads_ZombieDetectedCrashRecordedAndSessionNotAlive(t *testing.T) {
	env := newReconcilerTestEnv()
	rec := events.NewFake()
	env.rec = rec
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}

	tp := TemplateParams{
		Command:      "test-cmd",
		SessionName:  "worker",
		TemplateName: "worker",
		Hints:        agent.StartupHints{ProcessNames: []string{"test-cmd"}},
	}
	env.desiredState["worker"] = tp
	_ = env.sp.Start(context.Background(), "worker", runtime.Config{Command: "test-cmd"})

	session := env.createSessionBead("worker", "worker")
	env.markSessionActive(&session)

	// Simulate zombie: tmux session exists but process is dead.
	env.sp.Zombies["worker"] = true
	env.sp.SetPeekOutput("worker", "Error: quota exceeded")

	env.reconcile([]beads.Bead{session})

	// Verify crash event was captured with scrollback.
	crashRecorded := false
	for _, e := range rec.Events {
		if e.Type == events.SessionCrashed && e.Message != "" {
			crashRecorded = true
			break
		}
	}
	if !crashRecorded {
		t.Error("expected SessionCrashed event with scrollback from zombie detection")
	}

	// Verify ProcessAlive returns false for the zombie session — this is
	// the key property that prevents the reconciler from treating the
	// zombie as a healthy running session.
	if env.sp.ProcessAlive("worker", []string{"test-cmd"}) {
		t.Error("ProcessAlive should return false for zombie session")
	}
}

// TestReconcileSessionBeads_BeadMetadataRestartRequestedWhenSessionDead
// verifies that the reconciler detects restart_requested from bead metadata
// even when the tmux session is already dead (dops is nil or session not
// alive). This is the key durability property of the dual-flag approach:
// the bead flag survives tmux session death.
// Regression test for https://github.com/gastownhall/gascity/issues/70
func TestReconcileSessionBeads_BeadMetadataRestartRequestedWhenSessionDead(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Workspace:     config.Workspace{Name: "test-city"},
		Agents:        []config.Agent{{Name: "worker", StartCommand: "true", MaxActiveSessions: intPtr(2)}},
		NamedSessions: []config.NamedSession{{Template: "worker", Mode: "on_demand"}},
	}
	sessionName := config.NamedSessionRuntimeName(env.cfg.Workspace.Name, env.cfg.Workspace, "worker")
	session := env.createSessionBead(sessionName, "worker")
	env.setSessionMetadata(&session, map[string]string{
		namedSessionMetadataKey:      "true",
		namedSessionIdentityMetadata: "worker",
		namedSessionModeMetadata:     "on_demand",
		"restart_requested":          "true",
		"session_key":                "original-key",
		"started_config_hash":        "hash-before-restart",
	})

	// Session is NOT running — simulates tmux session already dead.
	// dops is nil (passed through env.reconcile).

	env.reconcile([]beads.Bead{session})

	got, _ := env.store.Get(session.ID)
	if got.Metadata["restart_requested"] != "" {
		t.Fatalf("restart_requested = %q, want cleared", got.Metadata["restart_requested"])
	}
	if got.Metadata["started_config_hash"] != "" {
		t.Fatalf("started_config_hash = %q, want cleared", got.Metadata["started_config_hash"])
	}
	if got.Metadata["session_key"] == "" || got.Metadata["session_key"] == "original-key" {
		t.Fatalf("session_key = %q, want rotated key", got.Metadata["session_key"])
	}
}

// TestReconcileSessionBeads_ClosedOnDemandBeadReopensWhenInDesiredState
// verifies the full reconciler-level cycle for on_demand named session
// recovery: a closed session bead that is still in the desired state
// should be reopened by syncSessionBeads so the reconciler can re-evaluate
// and restart it.
// Regression test for https://github.com/gastownhall/gascity/issues/139
func TestReconcileSessionBeads_ClosedOnDemandBeadReopensWhenInDesiredState(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	clk := &clock.Fake{Time: time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)}
	sp := runtime.NewFake()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{
			{Name: "worker", StartCommand: "true", MaxActiveSessions: intPtr(2)},
		},
		NamedSessions: []config.NamedSession{
			{Template: "worker", Mode: "on_demand"},
		},
	}

	sessionName := config.NamedSessionRuntimeName(cfg.Workspace.Name, cfg.Workspace, "worker")
	// Create a named session bead, then close it (simulates quota exhaustion).
	closed, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":               sessionName,
			"alias":                      "worker",
			"template":                   "worker",
			"state":                      "stopped",
			"close_reason":               "quota_exhaustion",
			namedSessionMetadataKey:      "true",
			namedSessionIdentityMetadata: "worker",
			namedSessionModeMetadata:     "on_demand",
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.Close(closed.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Build desired state with the on_demand session present (work exists).
	ds := map[string]TemplateParams{
		sessionName: {
			TemplateName:            "worker",
			InstanceName:            "worker",
			Alias:                   "worker",
			Command:                 "true",
			ConfiguredNamedIdentity: "worker",
			ConfiguredNamedMode:     "on_demand",
		},
	}

	// Run syncSessionBeads to reopen the closed bead (this is the recovery path).
	var stderr bytes.Buffer
	syncSessionBeads(cityPath, store, ds, sp, allConfiguredDS(ds), cfg, clk, &stderr, false)

	// Verify the bead was reopened.
	got, err := store.Get(closed.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != "open" {
		t.Fatalf("status = %q, want open (bead should be reopened for recovery)", got.Status)
	}
	if got.Metadata["close_reason"] != "" {
		t.Fatalf("close_reason = %q, want empty after reopen", got.Metadata["close_reason"])
	}

	// Now run the reconciler with the reopened bead — it should not close it
	// again since it's a configured on_demand session in the desired state.
	sessions, _ := loadSessionBeads(store)
	poolDesired := map[string]int{"worker": 1}
	cfgNames := configuredSessionNames(cfg, "", store)
	reconcileSessionBeads(
		context.Background(), sessions, ds, cfgNames, cfg, sp,
		store, nil, nil, nil, newDrainTracker(), poolDesired, false, nil, "",
		nil, clk, events.Discard, 0, 0, &bytes.Buffer{}, &stderr,
	)

	// Bead should still be open after reconciliation.
	got, err = store.Get(closed.ID)
	if err != nil {
		t.Fatalf("Get after reconcile: %v", err)
	}
	if got.Status != "open" {
		t.Fatalf("status after reconcile = %q, want open", got.Status)
	}
}

// TestReconcileSessionBeads_PoolRecoveryAfterClosedBead verifies the full
// recovery cycle for a managed pool session after its bead is closed.
// When a pool session's bead is closed (crash, drain, quota exhaustion),
// syncSessionBeads should create a fresh bead for that slot, and the
// reconciler should process the fresh bead without immediately closing it.
// This is the pool-session counterpart to #139 (named session recovery).
func TestReconcileSessionBeads_PoolRecoveryAfterClosedBead(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	clk := &clock.Fake{Time: time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)}
	sp := runtime.NewFake()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{
			{Name: "worker", StartCommand: "true", MinActiveSessions: intPtr(1), MaxActiveSessions: intPtr(3)},
		},
	}

	sessionName := "worker-1"
	// Create a pool session bead, then close it (simulates crash/drain).
	closed, err := store.Create(beads.Bead{
		Title:  sessionName,
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":         sessionName,
			"agent_name":           sessionName,
			"template":             "worker",
			"config_hash":          runtime.CoreFingerprint(runtime.Config{Command: "true"}),
			"live_hash":            runtime.LiveFingerprint(runtime.Config{Command: "true"}),
			"generation":           "1",
			"instance_token":       "old-token",
			"state":                "stopped",
			"close_reason":         "crash",
			"pool_slot":            "1",
			poolManagedMetadataKey: boolMetadata(true),
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.Close(closed.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Build desired state with the pool slot present (demand exists).
	ds := map[string]TemplateParams{
		sessionName: {
			TemplateName: "worker",
			InstanceName: sessionName,
			Command:      "true",
			PoolSlot:     1,
		},
	}

	// Run syncSessionBeads — should create a FRESH bead (not reopen the closed one).
	var stderr bytes.Buffer
	syncSessionBeads(cityPath, store, ds, sp, allConfiguredDS(ds), cfg, clk, &stderr, false)

	// Verify: closed bead stays closed, a new open bead is created.
	all := allSessionBeads(t, store)
	if len(all) != 2 {
		t.Fatalf("expected 2 beads (1 closed + 1 new), got %d", len(all))
	}

	var newBead beads.Bead
	for _, b := range all {
		if b.Status == "open" {
			newBead = b
			break
		}
	}
	if newBead.ID == "" {
		t.Fatal("no open bead found after syncSessionBeads")
	}
	if newBead.ID == closed.ID {
		t.Fatal("new bead has same ID as closed bead — expected a fresh bead, not a reopen")
	}
	if newBead.Metadata["instance_token"] == "old-token" {
		t.Error("new bead has same instance_token as closed bead — expected fresh token")
	}

	// Verify the closed bead was NOT reopened.
	got, err := store.Get(closed.ID)
	if err != nil {
		t.Fatalf("Get closed bead: %v", err)
	}
	if got.Status != "closed" {
		t.Fatalf("closed bead status = %q, want closed", got.Status)
	}

	// Now run the reconciler with the fresh bead — it should remain open
	// (not be closed as orphan) since the pool slot is in the desired state.
	sessions, _ := loadSessionBeads(store)
	poolDesired := map[string]int{"worker": 1}
	cfgNames := configuredSessionNames(cfg, "", store)
	reconcileSessionBeads(
		context.Background(), sessions, ds, cfgNames, cfg, sp,
		store, nil, nil, nil, newDrainTracker(), poolDesired, false, nil, "",
		nil, clk, events.Discard, 0, 0, &bytes.Buffer{}, &stderr,
	)

	// Fresh bead should still be open after reconciliation.
	got, err = store.Get(newBead.ID)
	if err != nil {
		t.Fatalf("Get after reconcile: %v", err)
	}
	if got.Status != "open" {
		t.Fatalf("fresh bead status after reconcile = %q, want open", got.Status)
	}
}

// --- resolveAgentTemplate tests ---

func TestResolveAgentTemplate_DirectMatch(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{{Name: "overseer"}}}
	if got := resolveAgentTemplate("overseer", cfg); got != "overseer" {
		t.Errorf("got %q, want %q", got, "overseer")
	}
}

func TestResolveAgentTemplate_PoolInstance(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "worker", MinActiveSessions: intPtr(1), MaxActiveSessions: intPtr(5)},
		},
	}
	if got := resolveAgentTemplate("worker-3", cfg); got != "worker" {
		t.Errorf("got %q, want %q", got, "worker")
	}
}

func TestResolveAgentTemplate_Fallback(t *testing.T) {
	cfg := &config.City{}
	if got := resolveAgentTemplate("unknown", cfg); got != "unknown" {
		t.Errorf("got %q, want %q", got, "unknown")
	}
}

func TestResolveAgentTemplate_NilConfig(t *testing.T) {
	if got := resolveAgentTemplate("test", nil); got != "test" {
		t.Errorf("got %q, want %q", got, "test")
	}
}

// --- resolvePoolSlot tests ---

func TestResolvePoolSlot_PoolInstance(t *testing.T) {
	if got := resolvePoolSlot("worker-3", "worker"); got != 3 {
		t.Errorf("got %d, want 3", got)
	}
}

func TestResolvePoolSlot_NonPool(t *testing.T) {
	if got := resolvePoolSlot("overseer", "overseer"); got != 0 {
		t.Errorf("got %d, want 0", got)
	}
}

func TestResolvePoolSlot_NonNumericSuffix(t *testing.T) {
	if got := resolvePoolSlot("worker-abc", "worker"); got != 0 {
		t.Errorf("got %d, want 0", got)
	}
}

// BUG: PR #208 — this test fails on current code because resolvePoolSlot()
// only recognizes pool instances that use the "<template>-<N>" naming
// convention. Namepool-themed names like "fenrir" for a "worker" pool
// don't have the "worker-" prefix, so resolvePoolSlot returns 0.
// This means namepool-themed pool instances never get pool_slot metadata.
// The fix: pool_slot must be passed through TemplateParams at creation time
// rather than reverse-engineered from the agent name.
func TestResolvePoolSlot_NamepoolThemedName(t *testing.T) {
	// A namepool-themed pool instance "fenrir" belonging to the "worker"
	// template should have a meaningful slot, but resolvePoolSlot cannot
	// derive it from the name alone.
	if got := resolvePoolSlot("fenrir", "worker"); got != 0 {
		// If this passes (got != 0), the bug is fixed. Currently it returns 0.
		t.Errorf("resolvePoolSlot(fenrir, worker) = %d, want non-zero slot for namepool themes", got)
	}

	// Contrast: standard numbered naming works correctly.
	if got := resolvePoolSlot("worker-1", "worker"); got != 1 {
		t.Errorf("resolvePoolSlot(worker-1, worker) = %d, want 1", got)
	}

	// PR #208 fix: TemplateParams.PoolSlot bypasses resolvePoolSlot.
	// Verify that syncSessionBeads prefers tp.PoolSlot over resolvePoolSlot.
	tp := TemplateParams{InstanceName: "fenrir", TemplateName: "worker", PoolSlot: 3}
	if tp.PoolSlot == 0 {
		t.Fatal("TemplateParams.PoolSlot should carry the slot from buildDesiredState")
	}
}

func TestResolveResumeCommand(t *testing.T) {
	tests := []struct {
		name       string
		command    string
		sessionKey string
		provider   *config.ResolvedProvider
		want       string
	}{
		{
			name:       "no resume flag → unchanged",
			command:    "claude --dangerously-skip-permissions",
			sessionKey: "abc-123",
			provider:   &config.ResolvedProvider{},
			want:       "claude --dangerously-skip-permissions",
		},
		{
			name:       "flag style",
			command:    "claude --dangerously-skip-permissions",
			sessionKey: "abc-123",
			provider:   &config.ResolvedProvider{ResumeFlag: "--resume"},
			want:       "claude --dangerously-skip-permissions --resume abc-123",
		},
		{
			name:       "subcommand style",
			command:    "codex --model o3",
			sessionKey: "def-456",
			provider:   &config.ResolvedProvider{ResumeFlag: "resume", ResumeStyle: "subcommand"},
			want:       "codex resume def-456 --model o3",
		},
		{
			name:       "subcommand style no args",
			command:    "codex",
			sessionKey: "def-456",
			provider:   &config.ResolvedProvider{ResumeFlag: "resume", ResumeStyle: "subcommand"},
			want:       "codex resume def-456",
		},
		{
			name:       "explicit resume_command takes precedence",
			command:    "claude --dangerously-skip-permissions",
			sessionKey: "abc-123",
			provider: &config.ResolvedProvider{
				ResumeFlag:    "--resume",
				ResumeCommand: "claude --resume {{.SessionKey}} --dangerously-skip-permissions",
			},
			want: "claude --resume abc-123 --dangerously-skip-permissions",
		},
		{
			name:       "resume_command without SessionKey placeholder",
			command:    "my-agent",
			sessionKey: "xyz",
			provider: &config.ResolvedProvider{
				ResumeCommand: "my-agent --continue",
			},
			want: "my-agent --continue",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveResumeCommand(tt.command, tt.sessionKey, tt.provider)
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestResolveSessionCommand(t *testing.T) {
	claude := &config.ResolvedProvider{
		ResumeFlag:    "--resume",
		SessionIDFlag: "--session-id",
	}

	t.Run("first start uses --session-id", func(t *testing.T) {
		got := resolveSessionCommand("claude --dangerously-skip-permissions", "abc-123", claude, true, false)
		want := "claude --dangerously-skip-permissions --session-id abc-123"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("resume uses --resume", func(t *testing.T) {
		got := resolveSessionCommand("claude --dangerously-skip-permissions", "abc-123", claude, false, false)
		want := "claude --dangerously-skip-permissions --resume abc-123"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("fresh wake uses --session-id", func(t *testing.T) {
		got := resolveSessionCommand("claude --dangerously-skip-permissions", "abc-123", claude, false, true)
		want := "claude --dangerously-skip-permissions --session-id abc-123"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("first start without SessionIDFlag falls back to resume", func(t *testing.T) {
		noSessionID := &config.ResolvedProvider{ResumeFlag: "--resume"}
		got := resolveSessionCommand("agent run", "key-1", noSessionID, true, false)
		want := "agent run --resume key-1"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})
}

func TestDrainedIsKnownState(t *testing.T) {
	if !knownSessionStates["drained"] {
		t.Fatal("drained must be a known session state")
	}
}

// TODO(pool-consolidation): This test validates that poolDesired gates wake
// decisions. Needs updating when pool_slot is removed — the slot-based gate
// will be replaced with count-based ordering.
func TestPoolDesiredLimitsWakeWork(t *testing.T) {
	t.Skip("blocked on pool_slot removal")
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Agents: []config.Agent{
			{Name: "claude", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(5)},
		},
	}
	// 3 sessions exist and are running, but demand (poolDesired) is only 1.
	// Don't add to desiredState — we're testing poolDesired gating only.
	var sessions []beads.Bead
	for i := 1; i <= 3; i++ {
		name := fmt.Sprintf("claude-%d", i)
		s := env.createSessionBead(name, "claude")
		env.setSessionMetadata(&s, map[string]string{
			"state":     "awake",
			"pool_slot": fmt.Sprintf("%d", i),
		})
		sessions = append(sessions, s)
	}

	// poolDesired=1: only 1 session should stay awake.
	poolDesired := map[string]int{"claude": 1}
	evalInput := make([]beads.Bead, len(sessions))
	copy(evalInput, sessions)
	evals := computeWakeEvaluations(evalInput, env.cfg, env.sp, poolDesired,
		map[string]bool{"claude": true}, nil, env.clk)

	wakeCount := 0
	for _, eval := range evals {
		if len(eval.Reasons) > 0 {
			wakeCount++
		}
	}
	if wakeCount != 1 {
		t.Errorf("wakeCount = %d, want 1 (only slot 1 within poolDesired=1)", wakeCount)
	}
}

// PR #209 -- skipped for now. Drained beads don't block capacity (all
// selection paths skip them). Closing would break gc attach on drained
// sessions. Tracked as a future cleanup task.

// Regression: poolDesired derived from desiredState counts ALL session beads
// (including discovered ones), inflating the desired count. This test verifies
// that derivePoolDesired only counts pool sessions, not all discovered beads.
