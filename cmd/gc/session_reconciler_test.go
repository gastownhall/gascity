package main

import (
	"bytes"
	"context"
	"fmt"
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
	return e.reconcileWithPoolDesired(sessions, map[string]int{})
}

func (e *reconcilerTestEnv) reconcileWithPoolDesired(sessions []beads.Bead, poolDesired map[string]int) int {
	cfgNames := configuredSessionNames(e.cfg, "", e.store)
	return reconcileSessionBeads(
		context.Background(), sessions, e.desiredState, cfgNames, e.cfg, e.sp,
		e.store, nil, nil, nil, e.dt, poolDesired, "",
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
	if got.Metadata["state"] != "asleep" {
		t.Fatalf("state = %q, want asleep", got.Metadata["state"])
	}
	if got.Metadata["sleep_reason"] != "drained" {
		t.Fatalf("sleep_reason = %q, want drained", got.Metadata["sleep_reason"])
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
			{Name: "worker", Dir: "frontend", DependsOn: []string{"frontend/db"}, Pool: &config.PoolConfig{Min: 1, Max: 2}},
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
			{Name: "db", Pool: &config.PoolConfig{Min: 2, Max: 2}},
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
			{Name: "db", Pool: &config.PoolConfig{Min: 2, Max: 2}},
		},
	}
	env.addDesired("worker", "worker", false)
	env.addDesired("db-1", "db", true) // one pool instance alive
	workerBead := env.createSessionBead("worker", "worker")
	env.markSessionCreating(&workerBead)

	woken := env.reconcile([]beads.Bead{workerBead})

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
		cfg, sp, store, nil, nil, nil, newDrainTracker(), map[string]int{}, "",
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
		cfg, sp, store, nil, nil, nil, newDrainTracker(), map[string]int{}, "",
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
		cfg, sp, store, nil, nil, nil, newDrainTracker(), map[string]int{}, "",
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
		cfg, sp, store, nil, nil, nil, newDrainTracker(), map[string]int{}, "",
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
		cfg, sp, store, nil, nil, nil, newDrainTracker(), map[string]int{}, "",
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
	if got.Metadata["wake_attempts"] != "1" {
		t.Fatalf("wake_attempts = %q, want 1", got.Metadata["wake_attempts"])
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
		cfg, sp, store, nil, nil, nil, newDrainTracker(), map[string]int{}, "",
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
			{Name: "worker", Pool: &config.PoolConfig{Min: 1, Max: 5}},
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
		env.cfg, env.sp, env.store, nil, nil, nil, env.dt, poolDesired, "",
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
		env.cfg, env.sp, env.store, nil, nil, nil, env.dt, map[string]int{}, "",
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
		env.cfg, env.sp, env.store, nil, nil, nil, env.dt, map[string]int{}, "",
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
		env.cfg, env.sp, env.store, nil, nil, nil, env.dt, map[string]int{}, "",
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
		env.cfg, env.sp, env.store, nil, nil, nil, env.dt, map[string]int{}, "",
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
			{Name: "worker", Pool: &config.PoolConfig{Min: 1, Max: 5}},
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
		got := resolveSessionCommand("claude --dangerously-skip-permissions", "abc-123", claude, true)
		want := "claude --dangerously-skip-permissions --session-id abc-123"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("resume uses --resume", func(t *testing.T) {
		got := resolveSessionCommand("claude --dangerously-skip-permissions", "abc-123", claude, false)
		want := "claude --dangerously-skip-permissions --resume abc-123"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("first start without SessionIDFlag falls back to resume", func(t *testing.T) {
		noSessionID := &config.ResolvedProvider{ResumeFlag: "--resume"}
		got := resolveSessionCommand("agent run", "key-1", noSessionID, true)
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
			{Name: "claude", Pool: &config.PoolConfig{Min: 0, Max: 5}},
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

// Regression: poolDesired derived from desiredState counts ALL session beads
// (including discovered ones), inflating the desired count. This test verifies
// that derivePoolDesired only counts pool sessions, not all discovered beads.
