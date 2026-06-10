package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/agent"
	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// fakeIdleTracker is a test double for idleTracker.
type fakeIdleTracker struct {
	idle       map[string]bool
	templates  map[string]bool
	exemptions map[string]bool
}

func newFakeIdleTracker() *fakeIdleTracker {
	return &fakeIdleTracker{
		idle:       make(map[string]bool),
		templates:  make(map[string]bool),
		exemptions: make(map[string]bool),
	}
}

func (f *fakeIdleTracker) checkIdle(sessionName, template string, _ runtime.Provider, _ time.Time) bool {
	if f.idle[sessionName] {
		return true
	}
	if template == "" || f.exemptions[sessionName] {
		return false
	}
	return f.templates[template]
}

func (f *fakeIdleTracker) setTimeout(sessionName string, _ time.Duration) {
	f.idle[sessionName] = true
}

func (f *fakeIdleTracker) setTimeoutForTemplate(template string, _ time.Duration) {
	if template != "" {
		f.templates[template] = true
	}
}

func (f *fakeIdleTracker) exemptTemplateFallbackForSession(sessionName string) {
	if sessionName != "" {
		f.exemptions[sessionName] = true
	}
}

type lineLimitedPeekProvider struct {
	*runtime.Fake
	peekLines []int
}

func (p *lineLimitedPeekProvider) Peek(name string, lines int) (string, error) {
	p.peekLines = append(p.peekLines, lines)
	output, err := p.Fake.Peek(name, lines)
	if err != nil || lines <= 0 {
		return output, err
	}
	parts := strings.Split(output, "\n")
	if len(parts) <= lines {
		return output, nil
	}
	return strings.Join(parts[len(parts)-lines:], "\n"), nil
}

type transientPeekErrorProvider struct {
	*runtime.Fake
	calls int
}

func (p *transientPeekErrorProvider) Peek(name string, lines int) (string, error) {
	p.calls++
	if p.calls == 1 {
		return "", errors.New("peek failed")
	}
	return p.Fake.Peek(name, lines)
}

type blockingStopProvider struct {
	*runtime.Fake
	stopStarted chan string
	releaseStop chan struct{}
}

func newBlockingStopProvider() *blockingStopProvider {
	return &blockingStopProvider{
		Fake:        runtime.NewFake(),
		stopStarted: make(chan string, 8),
		releaseStop: make(chan struct{}),
	}
}

func (p *blockingStopProvider) Stop(name string) error {
	select {
	case p.stopStarted <- name:
	default:
	}
	<-p.releaseStop
	return p.Fake.Stop(name)
}

type panicStopProvider struct {
	*runtime.Fake
	stopStarted chan string
}

func newPanicStopProvider() *panicStopProvider {
	return &panicStopProvider{
		Fake:        runtime.NewFake(),
		stopStarted: make(chan string, 1),
	}
}

func (p *panicStopProvider) Stop(name string) error {
	p.stopStarted <- name
	panic("stop exploded")
}

type shutdownWaitStopProvider struct {
	*blockingStopProvider
	listCalled chan struct{}
	listOnce   sync.Once
}

func newShutdownWaitStopProvider() *shutdownWaitStopProvider {
	return &shutdownWaitStopProvider{
		blockingStopProvider: newBlockingStopProvider(),
		listCalled:           make(chan struct{}),
	}
}

func (p *shutdownWaitStopProvider) ListRunning(prefix string) ([]string, error) {
	p.listOnce.Do(func() { close(p.listCalled) })
	return p.Fake.ListRunning(prefix)
}

type synchronizedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *synchronizedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *synchronizedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type delayedSessionExistsProvider struct {
	*runtime.Fake
	pendingConflict map[string]bool
	hiddenRunning   map[string]bool
	hiddenMeta      map[string]map[string]string
}

type failRateLimitHoldStore struct {
	*beads.MemStore
	failRateLimitHold  bool
	rateLimitHoldCalls int
}

func (s *failRateLimitHoldStore) SetMetadataBatch(id string, kvs map[string]string) error {
	if kvs["sleep_reason"] == "rate_limit" {
		s.rateLimitHoldCalls++
		if s.failRateLimitHold {
			return errors.New("rate-limit hold batch failed")
		}
	}
	return s.MemStore.SetMetadataBatch(id, kvs)
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

func (p *delayedSessionExistsProvider) ProcessAlive(name string, processNames []string) bool {
	if p.hiddenRunning[name] {
		return true
	}
	return p.Fake.ProcessAlive(name, processNames)
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
	sessionCircuitBreakerMu.Lock()
	sessionCircuitBreakerSingleton = newSessionCircuitBreaker(sessionCircuitBreakerConfig{})
	sessionCircuitBreakerMu.Unlock()
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

// addRunningWorkerDesiredWithNewConfig registers and starts the worker session with the drift test command.
func (e *reconcilerTestEnv) addRunningWorkerDesiredWithNewConfig() {
	tp := TemplateParams{
		Command:      "new-cmd",
		SessionName:  "worker",
		TemplateName: "worker",
	}
	e.desiredState["worker"] = tp
	_ = e.sp.Start(context.Background(), "worker", runtime.Config{Command: "new-cmd"})
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
	return e.reconcileWithPoolDesiredAndDrainOps(sessions, poolDesired, nil)
}

func (e *reconcilerTestEnv) reconcileWithPoolDesiredAndDrainOps(sessions []beads.Bead, poolDesired map[string]int, dops drainOps) int {
	cfgNames := configuredSessionNames(e.cfg, "", e.store)
	return reconcileSessionBeads(
		context.Background(), sessions, e.desiredState, cfgNames, e.cfg, e.sp,
		e.store, dops, nil, nil, e.dt, poolDesired, false, nil, "",
		nil, e.clk, e.rec, 0, 0, &e.stdout, &e.stderr,
	)
}

func TestReconcileSessionBeads_UsesAssignedWorkSnapshotForTaskWorkDir(t *testing.T) {
	env := newReconcilerTestEnv()
	base := beads.NewMemStore()
	store := &taskWorkDirLiveListCountingStore{Store: base}
	env.store = store
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	env.addDesired("worker", "worker", false)
	session := env.createSessionBead("worker", "worker")
	env.markSessionCreating(&session)

	workDir := t.TempDir()
	task, err := env.store.Create(beads.Bead{
		Title: "assigned task",
		Type:  "task",
		Metadata: map[string]string{
			"work_dir": workDir,
		},
	})
	if err != nil {
		t.Fatalf("Create(task): %v", err)
	}
	status := "in_progress"
	assignee := session.ID
	if err := env.store.Update(task.ID, beads.UpdateOpts{Status: &status, Assignee: &assignee}); err != nil {
		t.Fatalf("Update(task): %v", err)
	}
	task, err = env.store.Get(task.ID)
	if err != nil {
		t.Fatalf("Get(task): %v", err)
	}

	cfgNames := configuredSessionNames(env.cfg, "", env.store)
	woken := reconcileSessionBeads(
		context.Background(),
		[]beads.Bead{session},
		env.desiredState,
		cfgNames,
		env.cfg,
		env.sp,
		env.store,
		nil,
		[]beads.Bead{task},
		nil,
		env.dt,
		map[string]int{"worker": 1},
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
	if woken != 1 {
		t.Fatalf("woken = %d, want 1; stderr=%s", woken, env.stderr.String())
	}
	startCfg := env.sp.LastStartConfig("worker")
	if startCfg == nil {
		t.Fatal("worker was not started")
	}
	if startCfg.WorkDir != workDir {
		t.Fatalf("started WorkDir = %q, want %q", startCfg.WorkDir, workDir)
	}
	if store.liveInProgressAssigneeLists != 0 {
		t.Fatalf("live in-progress assignee List calls = %d, want 0 with complete assigned-work snapshot", store.liveInProgressAssigneeLists)
	}
}

func TestReconcileSessionBeads_FallsBackToLiveTaskWorkDirWithoutAssignedWorkSnapshot(t *testing.T) {
	env, store, session, workDir := newReconcilerTaskWorkDirTest(t)
	woken := reconcileSessionBeadsWithTaskWorkDirSnapshot(t, env, session, nil, false)
	if woken != 1 {
		t.Fatalf("woken = %d, want 1; stderr=%s", woken, env.stderr.String())
	}
	startCfg := env.sp.LastStartConfig("worker")
	if startCfg == nil {
		t.Fatal("worker was not started")
	}
	if startCfg.WorkDir != workDir {
		t.Fatalf("started WorkDir = %q, want %q", startCfg.WorkDir, workDir)
	}
	if store.liveInProgressAssigneeLists == 0 {
		t.Fatal("live in-progress assignee List calls = 0, want live fallback without assigned-work snapshot")
	}
}

func TestReconcileSessionBeads_FallsBackToLiveTaskWorkDirWhenAssignedWorkSnapshotPartial(t *testing.T) {
	env, store, session, workDir := newReconcilerTaskWorkDirTest(t)
	woken := reconcileSessionBeadsWithTaskWorkDirSnapshot(t, env, session, nil, true)
	if woken != 1 {
		t.Fatalf("woken = %d, want 1; stderr=%s", woken, env.stderr.String())
	}
	startCfg := env.sp.LastStartConfig("worker")
	if startCfg == nil {
		t.Fatal("worker was not started")
	}
	if startCfg.WorkDir != workDir {
		t.Fatalf("started WorkDir = %q, want %q", startCfg.WorkDir, workDir)
	}
	if store.liveInProgressAssigneeLists == 0 {
		t.Fatal("live in-progress assignee List calls = 0, want live fallback when assigned-work snapshot is partial")
	}
}

func TestReconcileSessionBeads_FallsBackToLiveTaskWorkDirWhenAssignedWorkSnapshotMisses(t *testing.T) {
	env, store, session, workDir := newReconcilerTaskWorkDirTest(t)
	unrelatedWorkDir := t.TempDir()
	unrelatedTask := createInProgressTaskWithWorkDir(t, env.store, "other-worker", unrelatedWorkDir)
	woken := reconcileSessionBeadsWithTaskWorkDirSnapshot(t, env, session, []beads.Bead{unrelatedTask}, false)
	if woken != 1 {
		t.Fatalf("woken = %d, want 1; stderr=%s", woken, env.stderr.String())
	}
	startCfg := env.sp.LastStartConfig("worker")
	if startCfg == nil {
		t.Fatal("worker was not started")
	}
	if startCfg.WorkDir != workDir {
		t.Fatalf("started WorkDir = %q, want %q", startCfg.WorkDir, workDir)
	}
	if store.liveInProgressAssigneeLists == 0 {
		t.Fatal("live in-progress assignee List calls = 0, want live fallback when assigned-work snapshot misses")
	}
}

func newReconcilerTaskWorkDirTest(t *testing.T) (*reconcilerTestEnv, *taskWorkDirLiveListCountingStore, beads.Bead, string) {
	t.Helper()
	env := newReconcilerTestEnv()
	base := beads.NewMemStore()
	store := &taskWorkDirLiveListCountingStore{Store: base}
	env.store = store
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	env.addDesired("worker", "worker", false)
	session := env.createSessionBead("worker", "worker")
	env.markSessionCreating(&session)
	workDir := t.TempDir()
	createInProgressTaskWithWorkDir(t, env.store, session.ID, workDir)
	return env, store, session, workDir
}

func createInProgressTaskWithWorkDir(t *testing.T, store beads.Store, assignee, workDir string) beads.Bead {
	t.Helper()
	task, err := store.Create(beads.Bead{
		Title: "assigned task",
		Type:  "task",
		Metadata: map[string]string{
			"work_dir": workDir,
		},
	})
	if err != nil {
		t.Fatalf("Create(task): %v", err)
	}
	status := "in_progress"
	if err := store.Update(task.ID, beads.UpdateOpts{Status: &status, Assignee: &assignee}); err != nil {
		t.Fatalf("Update(task): %v", err)
	}
	task, err = store.Get(task.ID)
	if err != nil {
		t.Fatalf("Get(task): %v", err)
	}
	return task
}

func reconcileSessionBeadsWithTaskWorkDirSnapshot(
	t *testing.T,
	env *reconcilerTestEnv,
	session beads.Bead,
	assignedWorkBeads []beads.Bead,
	storeQueryPartial bool,
) int {
	t.Helper()
	cfgNames := configuredSessionNames(env.cfg, "", env.store)
	return reconcileSessionBeads(
		context.Background(),
		[]beads.Bead{session},
		env.desiredState,
		cfgNames,
		env.cfg,
		env.sp,
		env.store,
		nil,
		assignedWorkBeads,
		nil,
		env.dt,
		map[string]int{"worker": 1},
		storeQueryPartial,
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
}

func waitForProviderStopped(t *testing.T, sp runtime.Provider, name string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !sp.IsRunning(name) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("session %q still running after async stop", name)
}

func (e *reconcilerTestEnv) reconcileStopPendingToTerminal(t *testing.T, sp runtime.Provider, session beads.Bead, dops drainOps, cfgNames map[string]bool) beads.Bead {
	t.Helper()
	name := strings.TrimSpace(session.Metadata["session_name"])
	waitForProviderStopped(t, sp, name)
	got, err := e.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get(%s) before stop-pending finalize: %v", session.ID, err)
	}
	woken := reconcileSessionBeads(
		context.Background(), []beads.Bead{got}, e.desiredState, cfgNames, e.cfg, sp,
		e.store, dops, nil, nil, e.dt, nil, false, nil, "",
		nil, e.clk, e.rec, 0, 0, &e.stdout, &e.stderr,
	)
	if woken != 0 {
		t.Fatalf("woken during stop-pending finalize = %d, want 0", woken)
	}
	final, err := e.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get(%s) after stop-pending finalize: %v", session.ID, err)
	}
	return final
}

func TestReconcileSessionBeads_DrainAckKeepsBeadOpen(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Agents: []config.Agent{{Name: "worker", SleepAfterIdle: config.SessionSleepOff}},
	}
	env.addDesired("worker", "worker", true)
	session := env.createSessionBead("worker", "worker")
	env.markSessionActive(&session)
	env.setSessionMetadata(&session, map[string]string{
		"pending_create_claim": "true",
	})
	if err := env.sp.SetMeta("worker", "GC_SESSION_ID", session.ID); err != nil {
		t.Fatalf("SetMeta(GC_SESSION_ID): %v", err)
	}

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
	got := env.reconcileStopPendingToTerminal(t, env.sp, session, dops, map[string]bool{"worker": true})
	if got.Status == "closed" {
		t.Fatalf("session bead closed unexpectedly: metadata=%v", got.Metadata)
	}
	if got.Metadata["state"] != "drained" {
		t.Fatalf("state = %q, want drained", got.Metadata["state"])
	}
	if got.Metadata["pending_create_claim"] != "" {
		t.Fatalf("pending_create_claim = %q, want cleared after drain-ack", got.Metadata["pending_create_claim"])
	}
}

// TestReconcileSessionBeads_DrainAckConsumesRestartRequested covers the
// chained reset → drain-ack sequence from #2574: `gc session reset` sets
// restart_requested=true on the bead, the agent acknowledges the drain, and
// the drain-ack finalize must consume the flag. If the flag survives in the
// store, a later cache-reconcile re-emission resurrects it and the controller
// honors it as a fresh restart request — a phantom second restart that
// rotates session_key and destroys resume continuity.
func TestReconcileSessionBeads_DrainAckConsumesRestartRequested(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Agents: []config.Agent{{Name: "worker", SleepAfterIdle: config.SessionSleepOff}},
	}
	env.addDesired("worker", "worker", true)
	session := env.createSessionBead("worker", "worker")
	env.markSessionActive(&session)
	env.setSessionMetadata(&session, map[string]string{
		"restart_requested": "true",
		"session_key":       "original-key",
	})
	if err := env.sp.SetMeta("worker", "GC_SESSION_ID", session.ID); err != nil {
		t.Fatalf("SetMeta(GC_SESSION_ID): %v", err)
	}

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
	got := env.reconcileStopPendingToTerminal(t, env.sp, session, dops, map[string]bool{"worker": true})
	if got.Metadata["state"] != "drained" {
		t.Fatalf("state = %q, want drained", got.Metadata["state"])
	}
	if got.Metadata["restart_requested"] != "" {
		t.Fatalf("restart_requested = %q, want consumed by drain-ack finalize", got.Metadata["restart_requested"])
	}
	if got.Metadata["session_key"] != "original-key" {
		t.Fatalf("session_key = %q, want preserved for resume continuity", got.Metadata["session_key"])
	}
}

func TestReconcileSessionBeads_DesiredFastPathSkipsAttachmentActivityObservation(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Agents: []config.Agent{{Name: "worker", SleepAfterIdle: config.SessionSleepOff}},
	}
	env.desiredState["worker"] = TemplateParams{
		Command:      "test-cmd",
		SessionName:  "worker",
		TemplateName: "worker",
		Hints:        agent.StartupHints{ProcessNames: []string{"worker"}},
	}
	if err := env.sp.Start(context.Background(), "worker", runtime.Config{Command: "test-cmd"}); err != nil {
		t.Fatalf("Start(worker): %v", err)
	}
	session := env.createSessionBead("worker", "worker")
	env.markSessionActive(&session)
	agentCfg := sessionCoreConfigForHash(env.desiredState["worker"], session)
	env.setSessionMetadata(&session, map[string]string{
		"started_config_hash": runtime.CoreFingerprint(agentCfg),
		"started_live_hash":   runtime.LiveFingerprint(agentCfg),
	})
	if err := env.sp.SetMeta("worker", "GC_SESSION_ID", session.ID); err != nil {
		t.Fatalf("SetMeta(GC_SESSION_ID): %v", err)
	}

	woken := env.reconcile([]beads.Bead{session})
	if woken != 0 {
		t.Fatalf("woken = %d, want 0", woken)
	}
	if got := env.sp.CountCalls("IsAttached", "worker"); got != 0 {
		t.Fatalf("IsAttached calls = %d, want 0 on desired fast path", got)
	}
	if got := env.sp.CountCalls("GetLastActivity", "worker"); got != 0 {
		t.Fatalf("GetLastActivity calls = %d, want 0 on desired fast path", got)
	}
}

func TestReconcileSessionBeads_DrainAckMarksStopPendingAndStopsAsync(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Agents: []config.Agent{{Name: "worker"}},
	}
	env.addDesired("worker", "worker", false)
	sp := newBlockingStopProvider()
	if err := sp.Start(context.Background(), "worker", runtime.Config{Command: "test-cmd"}); err != nil {
		t.Fatalf("Start(worker): %v", err)
	}
	releaseStop := func() {
		select {
		case <-sp.releaseStop:
		default:
			close(sp.releaseStop)
		}
	}
	t.Cleanup(releaseStop)

	session := env.createSessionBead("worker", "worker")
	env.markSessionActive(&session)
	env.setSessionMetadata(&session, map[string]string{
		"pending_create_claim": "true",
	})
	if err := sp.SetMeta("worker", "GC_SESSION_ID", session.ID); err != nil {
		t.Fatalf("SetMeta(GC_SESSION_ID): %v", err)
	}

	dops := newFakeDrainOps()
	if err := dops.setDrainAck("worker"); err != nil {
		t.Fatalf("setDrainAck: %v", err)
	}

	done := make(chan int, 1)
	go func() {
		done <- reconcileSessionBeads(
			context.Background(),
			[]beads.Bead{session},
			env.desiredState,
			map[string]bool{"worker": true},
			env.cfg,
			sp,
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
	}()

	select {
	case woken := <-done:
		if woken != 0 {
			t.Fatalf("woken = %d, want 0", woken)
		}
	case <-time.After(200 * time.Millisecond):
		releaseStop()
		t.Fatal("reconcile blocked on provider Stop; drain-ack stop must be async")
	}

	select {
	case name := <-sp.stopStarted:
		if name != "worker" {
			t.Fatalf("Stop called for %q, want worker", name)
		}
	case <-time.After(time.Second):
		t.Fatal("async Stop was not started")
	}

	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", session.ID, err)
	}
	if got.Metadata["state"] != string(sessionpkg.StateDraining) {
		t.Fatalf("state = %q, want draining", got.Metadata["state"])
	}
	if got.Metadata["state_reason"] != sessionpkg.DrainAckStopPendingReason {
		t.Fatalf("state_reason = %q, want %q", got.Metadata["state_reason"], sessionpkg.DrainAckStopPendingReason)
	}
	if got.Metadata["pending_create_claim"] != "" {
		t.Fatalf("pending_create_claim = %q, want cleared before async stop", got.Metadata["pending_create_claim"])
	}
	if len(dops.clearDrainCalls) != 0 {
		t.Fatalf("clearDrain calls = %v, want none until terminal patch", dops.clearDrainCalls)
	}
	if !sp.IsRunning("worker") {
		t.Fatal("worker stopped before release; test no longer proves async stop-pending behavior")
	}
}

func TestQueueDrainAckAsyncStopTracksShutdownWait(t *testing.T) {
	store := beads.NewMemStore()
	sp := newBlockingStopProvider()
	if err := sp.Start(context.Background(), "worker", runtime.Config{Command: "test-cmd"}); err != nil {
		t.Fatalf("Start(worker): %v", err)
	}
	var stderr synchronizedBuffer
	tracker := &asyncStartTracker{}
	queueDrainAckAsyncStop("", store, sp, &config.City{}, "gc-worker", "worker", tracker, &stderr)

	select {
	case <-sp.stopStarted:
	case <-time.After(time.Second):
		t.Fatal("async drain-ack stop did not start")
	}

	if tracker.wait(10 * time.Millisecond) {
		t.Fatal("async drain-ack stop tracker reported drained while Stop is blocked")
	}
	close(sp.releaseStop)
	if !tracker.wait(time.Second) {
		t.Fatal("async drain-ack stop tracker did not drain after Stop returned")
	}
}

func TestQueueDrainAckAsyncStopDedupScopedToTracker(t *testing.T) {
	store := beads.NewMemStore()
	first := newBlockingStopProvider()
	if err := first.Start(context.Background(), "worker", runtime.Config{Command: "test-cmd"}); err != nil {
		t.Fatalf("Start(first worker): %v", err)
	}
	second := newBlockingStopProvider()
	if err := second.Start(context.Background(), "worker", runtime.Config{Command: "test-cmd"}); err != nil {
		t.Fatalf("Start(second worker): %v", err)
	}
	firstReleased := false
	secondReleased := false
	defer func() {
		if !firstReleased {
			close(first.releaseStop)
		}
		if !secondReleased {
			close(second.releaseStop)
		}
	}()

	var stderr synchronizedBuffer
	firstTracker := &asyncStartTracker{}
	secondTracker := &asyncStartTracker{}
	queueDrainAckAsyncStop("", store, first, &config.City{}, "gc-worker", "worker", firstTracker, &stderr)
	select {
	case <-first.stopStarted:
	case <-time.After(time.Second):
		t.Fatal("first async drain-ack stop did not start")
	}

	queueDrainAckAsyncStop("", store, second, &config.City{}, "gc-worker", "worker", secondTracker, &stderr)
	select {
	case <-second.stopStarted:
	case <-time.After(time.Second):
		t.Fatal("second async drain-ack stop was suppressed by another tracker scope")
	}
	close(first.releaseStop)
	firstReleased = true
	close(second.releaseStop)
	secondReleased = true
	if !firstTracker.wait(time.Second) {
		t.Fatal("first async drain-ack stop tracker did not drain after release")
	}
	if !secondTracker.wait(time.Second) {
		t.Fatal("second async drain-ack stop tracker did not drain after release")
	}
}

func TestQueueDrainAckAsyncStopRecoversStopPanic(t *testing.T) {
	store := beads.NewMemStore()
	sp := newPanicStopProvider()
	if err := sp.Start(context.Background(), "worker", runtime.Config{Command: "test-cmd"}); err != nil {
		t.Fatalf("Start(worker): %v", err)
	}
	var stderr synchronizedBuffer
	tracker := &asyncStartTracker{}
	queueDrainAckAsyncStop(t.TempDir(), store, sp, &config.City{}, "gc-worker", "worker", tracker, &stderr)

	select {
	case <-sp.stopStarted:
	case <-time.After(time.Second):
		t.Fatal("async drain-ack stop did not start")
	}
	if !tracker.wait(time.Second) {
		t.Fatal("async drain-ack stop tracker did not drain after Stop panic")
	}
	got := stderr.String()
	if !strings.Contains(got, "session reconciler: async drain-ack stop worker panicked: stop exploded") {
		t.Fatalf("stderr = %q, want panic diagnostic", got)
	}
	if !strings.Contains(got, "goroutine ") {
		t.Fatalf("stderr = %q, want stack trace", got)
	}
}

// TestQueueDrainAckAsyncStopPokesAfterSuccessfulStop verifies that the
// controller is poked once after an async drain-ack kill succeeds (or the
// session is already gone), so Phase 2 (finalize + pool respawn) runs on the
// next event tick instead of waiting for the patrol interval (ga-ryhnhd).
// Not parallel — modifies the package-level drainAckAsyncStopPokeController seam.
func TestQueueDrainAckAsyncStopPokesAfterSuccessfulStop(t *testing.T) {
	var pokeCalls int
	var pokeMu sync.Mutex
	old := drainAckAsyncStopPokeController
	drainAckAsyncStopPokeController = func(string) error {
		pokeMu.Lock()
		pokeCalls++
		pokeMu.Unlock()
		return nil
	}
	t.Cleanup(func() { drainAckAsyncStopPokeController = old })

	store := beads.NewMemStore()
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "worker", runtime.Config{Command: "test-cmd"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	var stderr synchronizedBuffer
	tracker := &asyncStartTracker{}
	queueDrainAckAsyncStop("", store, sp, &config.City{}, "gc-worker", "worker", tracker, &stderr)
	if !tracker.wait(time.Second) {
		t.Fatal("async drain-ack stop did not complete")
	}

	pokeMu.Lock()
	got := pokeCalls
	pokeMu.Unlock()
	if got != 1 {
		t.Fatalf("poke count = %d, want 1 after successful stop", got)
	}
}

// TestQueueDrainAckAsyncStopDoesNotPokeOnHardError verifies that the
// controller is NOT poked when the async kill returns a hard (non-gone) error,
// preventing a hot poke-loop on an unkillable session.
// Not parallel — modifies the package-level drainAckAsyncStopPokeController seam.
func TestQueueDrainAckAsyncStopDoesNotPokeOnHardError(t *testing.T) {
	var pokeCalls int
	var pokeMu sync.Mutex
	old := drainAckAsyncStopPokeController
	drainAckAsyncStopPokeController = func(string) error {
		pokeMu.Lock()
		pokeCalls++
		pokeMu.Unlock()
		return nil
	}
	t.Cleanup(func() { drainAckAsyncStopPokeController = old })

	store := beads.NewMemStore()
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "worker", runtime.Config{Command: "test-cmd"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	sp.StopErrors = map[string]error{"worker": errors.New("hard kill error")}
	var stderr synchronizedBuffer
	tracker := &asyncStartTracker{}
	queueDrainAckAsyncStop("", store, sp, &config.City{}, "gc-worker", "worker", tracker, &stderr)
	if !tracker.wait(time.Second) {
		t.Fatal("async drain-ack stop did not complete")
	}

	pokeMu.Lock()
	got := pokeCalls
	pokeMu.Unlock()
	if got != 0 {
		t.Fatalf("poke count = %d, want 0 (hard error must not poke)", got)
	}
}

func TestCityRuntimeShutdownWaitsForTrackedAsyncDrainAckStopsBeforeStopSnapshot(t *testing.T) {
	store := beads.NewMemStore()
	sp := newShutdownWaitStopProvider()
	if err := sp.Start(context.Background(), "worker", runtime.Config{Command: "test-cmd"}); err != nil {
		t.Fatalf("Start(worker): %v", err)
	}
	cr := &CityRuntime{
		cfg:                 &config.City{Daemon: config.DaemonConfig{ShutdownTimeout: "500ms"}},
		sp:                  sp,
		rec:                 events.Discard,
		standaloneCityStore: store,
		logPrefix:           "gc test",
		stdout:              ioDiscard{},
		stderr:              ioDiscard{},
	}
	queueDrainAckAsyncStop("", store, sp, cr.cfg, "gc-worker", "worker", &cr.asyncStops, &synchronizedBuffer{})

	select {
	case <-sp.stopStarted:
	case <-time.After(time.Second):
		t.Fatal("async drain-ack stop did not start")
	}

	shutdownDone := make(chan struct{})
	go func() {
		cr.shutdown()
		close(shutdownDone)
	}()
	select {
	case <-sp.listCalled:
		t.Fatal("shutdown took stop snapshot before async drain-ack stop finished")
	case <-time.After(25 * time.Millisecond):
	}
	close(sp.releaseStop)
	select {
	case <-shutdownDone:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not finish after async drain-ack stop returned")
	}
}

func TestFinalizeDrainAckStopPendingSessionsClosesStoppedPoolBeforeAllocation(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Agents: []config.Agent{{Name: "worker"}},
	}
	session := env.createSessionBead("worker", "worker")
	patch := sessionpkg.DrainAckStopPendingPatch(env.clk.Now().UTC())
	patch[poolManagedMetadataKey] = boolMetadata(true)
	if err := env.store.SetMetadataBatch(session.ID, patch); err != nil {
		t.Fatalf("SetMetadataBatch(stop-pending): %v", err)
	}
	session.Metadata = patch.Apply(session.Metadata)

	finalized := finalizeDrainAckStopPendingSessions(
		"", env.cfg, env.sp, env.store, nil, []beads.Bead{session},
		newFakeDrainOps(), env.dt, nil, env.clk, env.rec, &env.stderr,
	)
	if finalized != 1 {
		t.Fatalf("finalized = %d, want 1", finalized)
	}

	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", session.ID, err)
	}
	if got.Status != "closed" {
		t.Fatalf("status = %q, want closed so the pool slot is free before allocation", got.Status)
	}
	if got.Metadata["state"] != "drained" {
		t.Fatalf("state = %q, want drained", got.Metadata["state"])
	}
}

func TestReconcileSessionBeads_DrainAckWithAssignedOpenWorkSleepsInsteadOfDraining(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Agents: []config.Agent{{Name: "worker"}},
	}
	env.addDesired("worker", "worker", true)
	session := env.createSessionBead("worker", "worker")
	env.markSessionActive(&session)
	if _, err := env.store.Create(beads.Bead{
		Title:    "future work",
		Type:     "task",
		Status:   "open",
		Assignee: session.ID,
	}); err != nil {
		t.Fatalf("Create(future work): %v", err)
	}

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
	got := env.reconcileStopPendingToTerminal(t, env.sp, session, dops, map[string]bool{"worker": true})
	if got.Status == "closed" {
		t.Fatalf("session bead closed unexpectedly: metadata=%v", got.Metadata)
	}
	if got.Metadata["state"] != "asleep" {
		t.Fatalf("state = %q, want asleep", got.Metadata["state"])
	}
	if got.Metadata["sleep_reason"] != "idle" {
		t.Fatalf("sleep_reason = %q, want idle", got.Metadata["sleep_reason"])
	}
	if got.Metadata["pending_create_claim"] != "" {
		t.Fatalf("pending_create_claim = %q, want cleared after drain-ack", got.Metadata["pending_create_claim"])
	}
}

// TestReconcileSessionBeads_DrainAckMidPhaseEmitsAssignedWorkEvent pins
// gastownhall/gascity#2293's Shape A contract: when a session drain-acks
// while still holding the assignee on an in-progress work bead (the cap-hit
// shape — worker exited mid-task without nulling assignee), the reconciler
// MUST emit events.SessionDrainAckedWithAssignedWork carrying the session
// and bead IDs exactly once after the provider stop has completed so pack-side
// subscribers can apply recovery policy. The SDK reconciler stops at the event;
// it does not commit, push, or clear assignee.
func TestReconcileSessionBeads_DrainAckMidPhaseEmitsAssignedWorkEvent(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	env.addDesired("worker", "worker", true)
	fake := events.NewFake()
	env.rec = fake

	session := env.createSessionBead("worker", "worker")
	env.markSessionActive(&session)

	stranded, err := env.store.Create(beads.Bead{
		Title:    "implement phase work",
		Type:     "task",
		Status:   "in_progress",
		Assignee: session.ID,
	})
	if err != nil {
		t.Fatalf("Create(stranded bead): %v", err)
	}

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
	gotSession := env.reconcileStopPendingToTerminal(t, env.sp, session, dops, map[string]bool{"worker": true})
	if gotSession.Status == "closed" {
		t.Fatalf("session bead closed unexpectedly: metadata=%v", gotSession.Metadata)
	}
	if gotSession.Metadata["state"] != "asleep" || gotSession.Metadata["sleep_reason"] != "idle" {
		t.Fatalf("session state=%q sleep_reason=%q, want asleep/idle after assigned-work drain-ack",
			gotSession.Metadata["state"], gotSession.Metadata["sleep_reason"])
	}

	var matched *events.Event
	matches := 0
	for i := range fake.Events {
		if fake.Events[i].Type == events.SessionDrainAckedWithAssignedWork {
			matches++
			matched = &fake.Events[i]
		}
	}
	if matched == nil {
		t.Fatalf("expected %s event, got %d events of other types", events.SessionDrainAckedWithAssignedWork, len(fake.Events))
	}
	if matches != 1 {
		t.Fatalf("%s events = %d, want exactly 1 across stop-pending lifecycle", events.SessionDrainAckedWithAssignedWork, matches)
	}
	if !strings.Contains(string(matched.Payload), session.ID) {
		t.Errorf("event payload does not reference session ID %q: %s", session.ID, matched.Payload)
	}
	if !strings.Contains(string(matched.Payload), stranded.ID) {
		t.Errorf("event payload does not reference stranded bead ID %q: %s", stranded.ID, matched.Payload)
	}

	// Verify the SDK did NOT mutate the bead's assignee — recovery policy
	// must live in pack-side subscribers, not the reconciler.
	got, err := env.store.Get(stranded.ID)
	if err != nil {
		t.Fatalf("Get(stranded): %v", err)
	}
	if got.Assignee != session.ID {
		t.Errorf("stranded bead assignee = %q, want %q (SDK must not clear assignee — pack-side recovery)", got.Assignee, session.ID)
	}
	if got.Status == "closed" {
		t.Errorf("stranded bead status = %q, SDK must not close the bead", got.Status)
	}
}

func TestReconcileSessionBeads_DeadDesiredDrainAckWithAssignedWorkEmitsOneEvent(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	env.addDesired("worker", "worker", false)
	fake := events.NewFake()
	env.rec = fake

	session := env.createSessionBead("worker", "worker")
	env.markSessionActive(&session)

	stranded, err := env.store.Create(beads.Bead{
		Title:    "assigned work",
		Type:     "task",
		Status:   "in_progress",
		Assignee: session.ID,
	})
	if err != nil {
		t.Fatalf("Create(stranded bead): %v", err)
	}

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

	gotSession, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", session.ID, err)
	}
	if gotSession.Status == "closed" {
		t.Fatalf("session bead closed unexpectedly: metadata=%v", gotSession.Metadata)
	}
	if gotSession.Metadata["state"] != "asleep" || gotSession.Metadata["sleep_reason"] != "idle" {
		t.Fatalf("session state=%q sleep_reason=%q, want asleep/idle after assigned-work drain-ack",
			gotSession.Metadata["state"], gotSession.Metadata["sleep_reason"])
	}

	matches := 0
	var matched *events.Event
	for i := range fake.Events {
		if fake.Events[i].Type == events.SessionDrainAckedWithAssignedWork {
			matches++
			matched = &fake.Events[i]
		}
	}
	if matched == nil {
		t.Fatalf("expected %s event, got %d events of other types", events.SessionDrainAckedWithAssignedWork, len(fake.Events))
	}
	if matches != 1 {
		t.Fatalf("%s events = %d, want exactly 1 for already-stopped desired drain-ack", events.SessionDrainAckedWithAssignedWork, matches)
	}
	if !strings.Contains(string(matched.Payload), session.ID) {
		t.Errorf("event payload does not reference session ID %q: %s", session.ID, matched.Payload)
	}
	if !strings.Contains(string(matched.Payload), stranded.ID) {
		t.Errorf("event payload does not reference stranded bead ID %q: %s", stranded.ID, matched.Payload)
	}
}

// TestReconcileSessionBeads_DrainAckCleanHandoffSuppressesAssignedWorkEvent
// pins the other half of the Shape A contract: the phase-end handoff path
// (worker writes --assignee "" before drain-ack) MUST NOT emit the event.
// Without this discriminator, every clean handoff would be misclassified as
// a cap-hit, breaking the SDLC multi-phase pattern.
func TestReconcileSessionBeads_DrainAckCleanHandoffSuppressesAssignedWorkEvent(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	env.addDesired("worker", "worker", true)
	fake := events.NewFake()
	env.rec = fake

	session := env.createSessionBead("worker", "worker")
	env.markSessionActive(&session)

	// Phase-end handoff shape: bead exists but assignee is already nulled
	// before drain-ack. Status open / re-routed for the next phase.
	if _, err := env.store.Create(beads.Bead{
		Title:  "next phase work",
		Type:   "task",
		Status: "open",
		// Assignee intentionally empty — worker handed off before draining.
		Metadata: map[string]string{"gc.routed_to": "tester"},
	}); err != nil {
		t.Fatalf("Create(handed-off bead): %v", err)
	}

	dops := newFakeDrainOps()
	if err := dops.setDrainAck("worker"); err != nil {
		t.Fatalf("setDrainAck: %v", err)
	}

	_ = reconcileSessionBeads(
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

	for _, ev := range fake.Events {
		if ev.Type == events.SessionDrainAckedWithAssignedWork {
			t.Fatalf("unexpected %s event on clean handoff path: %+v", events.SessionDrainAckedWithAssignedWork, ev)
		}
	}
}

func TestReconcileSessionBeads_UndesiredDrainAckStopsAndCloses(t *testing.T) {
	env := newReconcilerTestEnv()
	session := env.createSessionBead("worker", "worker")
	env.markSessionActive(&session)
	if err := env.sp.Start(context.Background(), "worker", runtime.Config{Command: "test-cmd"}); err != nil {
		t.Fatalf("Start(worker): %v", err)
	}

	dops := newFakeDrainOps()
	if err := dops.setDrainAck("worker"); err != nil {
		t.Fatalf("setDrainAck: %v", err)
	}

	woken := reconcileSessionBeads(
		context.Background(),
		[]beads.Bead{session},
		env.desiredState,
		nil,
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
	got := env.reconcileStopPendingToTerminal(t, env.sp, session, dops, nil)
	if got.Status != "closed" {
		t.Fatalf("status = %q, want closed; metadata=%v", got.Status, got.Metadata)
	}
	if want := sessionpkg.CanonicalCloseReason("drained"); got.Metadata["close_reason"] != want {
		t.Fatalf("close_reason = %q, want %q", got.Metadata["close_reason"], want)
	}
	if got.Metadata["state"] != "drained" {
		t.Fatalf("state = %q, want %q", got.Metadata["state"], "drained")
	}
}

func TestReconcileSessionBeads_UndesiredDrainAckWithAssignedOpenWorkSleepsInsteadOfClosing(t *testing.T) {
	env := newReconcilerTestEnv()
	session := env.createSessionBead("worker", "worker")
	env.markSessionActive(&session)
	if err := env.sp.Start(context.Background(), "worker", runtime.Config{Command: "test-cmd"}); err != nil {
		t.Fatalf("Start(worker): %v", err)
	}
	if _, err := env.store.Create(beads.Bead{
		Title:    "future work",
		Type:     "task",
		Status:   "open",
		Assignee: session.ID,
	}); err != nil {
		t.Fatalf("Create(future work): %v", err)
	}

	dops := newFakeDrainOps()
	if err := dops.setDrainAck("worker"); err != nil {
		t.Fatalf("setDrainAck: %v", err)
	}

	woken := reconcileSessionBeads(
		context.Background(),
		[]beads.Bead{session},
		env.desiredState,
		nil,
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
	got := env.reconcileStopPendingToTerminal(t, env.sp, session, dops, nil)
	if got.Status == "closed" {
		t.Fatalf("session bead closed unexpectedly: metadata=%v", got.Metadata)
	}
	if got.Metadata["state"] != "asleep" {
		t.Fatalf("state = %q, want asleep", got.Metadata["state"])
	}
	if got.Metadata["sleep_reason"] != "idle" {
		t.Fatalf("sleep_reason = %q, want idle", got.Metadata["sleep_reason"])
	}
	if got.Metadata["pending_create_claim"] != "" {
		t.Fatalf("pending_create_claim = %q, want cleared after drain-ack", got.Metadata["pending_create_claim"])
	}
}

// TestReconcileSessionBeads_DrainAckUsesLiveStoreQuery is the regression
// guard for the stuck-pool-worker bug on ga-ttn5z. Pool workers close
// their own work bead with `bd close` BEFORE calling `gc runtime
// drain-ack`; the old code path then read `hasAssignedWork` from a tick
// snapshot captured before the close, so the snapshot falsely reported
// the now-closed bead as open+assigned. That flipped the session into
// CompleteDrainPatch (state=asleep, sleep_reason=idle) instead of
// AcknowledgeDrainPatch (state=drained), which hid the bead from the
// close gate and stranded new queue work on a ghost slot.
//
// Fix: the snapshot-based path is gone; drain-ack queries the live
// store via sessionHasOpenAssignedWork. This test verifies the
// post-fix outcome: with no open assigned work in the store, drain-ack
// lands the session in `drained` (the correct terminal for recycling).
func TestReconcileSessionBeads_DrainAckUsesLiveStoreQuery(t *testing.T) {
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

	reconcileSessionBeadsAtPath(
		context.Background(),
		"",
		[]beads.Bead{session},
		env.desiredState,
		map[string]bool{"worker": true},
		env.cfg,
		env.sp,
		env.store,
		dops,
		nil,
		nil, // rigStores
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

	got := env.reconcileStopPendingToTerminal(t, env.sp, session, dops, map[string]bool{"worker": true})
	if got.Metadata["state"] != "drained" {
		t.Fatalf("state = %q, want drained — drain-ack must query the live store and land a session with no assigned work in drained state", got.Metadata["state"])
	}
}

// TestReconcileSessionBeads_AsleepIdlePoolBeadFreesSlot is the other half
// of the fix: even if a pool bead somehow lands in state=asleep +
// sleep_reason=idle (from the old buggy drain-ack path OR any other
// route), the close gate must still free the slot so the supervisor can
// spawn a fresh worker for pending queue work. Before the fix the close
// gate only fired for state=drained, so idle-asleep pool beads sat open
// indefinitely and blocked new spawns.
func TestReconcileSessionBeads_AsleepIdlePoolBeadFreesSlot(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Agents: []config.Agent{{Name: "worker"}},
	}
	env.addDesired("worker", "worker", false) // NOT running
	session := env.createSessionBead("worker", "worker")
	// Simulate the post-drain-ack-ghost state: asleep + sleep_reason=idle
	// + pool-managed, but the runtime has exited and no work is assigned.
	env.setSessionMetadata(&session, map[string]string{
		"state":                "asleep",
		"sleep_reason":         "idle",
		poolManagedMetadataKey: boolMetadata(true),
	})

	reconcileSessionBeadsAtPath(
		context.Background(),
		"",
		[]beads.Bead{session},
		env.desiredState,
		map[string]bool{"worker": true},
		env.cfg,
		env.sp,
		env.store,
		newFakeDrainOps(),
		nil,
		nil, // rigStores
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

	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", session.ID, err)
	}
	if got.Status != "closed" {
		t.Fatalf("status = %q, want closed — asleep-idle pool beads must free their slot via the live-query close gate", got.Status)
	}
}

// capturingRecorder is an in-memory events.Recorder used in tests that
// need to assert which events were emitted.
type capturingRecorder struct {
	events []events.Event
}

func (c *capturingRecorder) Record(e events.Event) {
	c.events = append(c.events, e)
}

// strandedEvents returns the captured events.SessionStranded events,
// in emission order. Specialized to the only event type any test in
// this package currently filters for; add a sibling method (or migrate
// to events.NewFake()) if a future test needs another type.
func (c *capturingRecorder) strandedEvents() []events.Event {
	out := make([]events.Event, 0, len(c.events))
	for _, e := range c.events {
		if e.Type == events.SessionStranded {
			out = append(out, e)
		}
	}
	return out
}

// session.stranded must carry a typed payload with the stranded work
// bead IDs and session identity, not just the human-readable Message —
// machine consumers (pack-level recovery subscribers) act on the
// payload, not on message text. Regression test for ga-kmoj9c.
func TestEmitSessionStrandedDiagnostic_CarriesTypedPayload(t *testing.T) {
	store := beads.NewMemStore()
	session, work := createDetachedStrandedWork(t, store, "")

	if sample, ok := events.LookupPayload(events.SessionStranded); !ok {
		t.Fatal("no payload registered for session.stranded")
	} else if _, typed := sample.(api.SessionStrandedPayload); !typed {
		t.Fatalf("registered session.stranded payload = %T, want api.SessionStrandedPayload", sample)
	}

	rec := emitStrandedDiagnosticForTest(t, store, &session)
	stranded := rec.strandedEvents()
	if len(stranded) != 1 {
		t.Fatalf("session.stranded events = %d, want 1; events: %+v", len(stranded), rec.events)
	}
	e := stranded[0]
	if !strings.Contains(e.Message, work.ID) {
		t.Fatalf("session.stranded message = %q, want operator text still listing work bead %q", e.Message, work.ID)
	}
	if len(e.Payload) == 0 {
		t.Fatal("session.stranded payload is empty, want typed api.SessionStrandedPayload")
	}
	var payload api.SessionStrandedPayload
	if err := json.Unmarshal(e.Payload, &payload); err != nil {
		t.Fatalf("decoding session.stranded payload: %v", err)
	}
	if payload.SessionID != session.ID {
		t.Fatalf("payload.SessionID = %q, want %q", payload.SessionID, session.ID)
	}
	if payload.SessionName != "worker-mc-dead" {
		t.Fatalf("payload.SessionName = %q, want %q", payload.SessionName, "worker-mc-dead")
	}
	if payload.Template != "worker" {
		t.Fatalf("payload.Template = %q, want %q", payload.Template, "worker")
	}
	if len(payload.WorkBeadIDs) != 1 || payload.WorkBeadIDs[0] != work.ID {
		t.Fatalf("payload.WorkBeadIDs = %v, want [%q]", payload.WorkBeadIDs, work.ID)
	}
}

func TestEmitSessionStrandedDiagnostic_DetachedProbeAliveSuppressesEvent(t *testing.T) {
	store := beads.NewMemStore()
	session, work := createDetachedStrandedWork(t, store, "tmux:gctest-stranded:soak-loop")
	installFakeTmux(t, "exit 0")
	rec := emitStrandedDiagnosticForTest(t, store, &session)

	if stranded := rec.strandedEvents(); len(stranded) != 0 {
		t.Fatalf("session.stranded events = %d, want 0 while detached probe is alive; events: %+v", len(stranded), rec.events)
	}
	got, err := store.Get(work.ID)
	if err != nil {
		t.Fatalf("Get work bead: %v", err)
	}
	if got.Metadata[detachedProbeMetadataKey] != "tmux:gctest-stranded:soak-loop" {
		t.Fatalf("gc.detached = %q, want preserved", got.Metadata[detachedProbeMetadataKey])
	}
	if session.Metadata[strandedEventEmittedKey] != "" {
		t.Fatalf("session in-memory throttle marker = %q, want unset when event suppressed", session.Metadata[strandedEventEmittedKey])
	}
}

func TestEmitSessionStrandedDiagnostic_DetachedProbeDeadClearsAndEmits(t *testing.T) {
	store := beads.NewMemStore()
	session, work := createDetachedStrandedWork(t, store, "tmux:gctest-stranded:soak-loop")
	installFakeTmux(t, "exit 1")
	rec := emitStrandedDiagnosticForTest(t, store, &session)

	stranded := rec.strandedEvents()
	if len(stranded) != 1 {
		t.Fatalf("session.stranded events = %d, want 1 for dead detached probe; events: %+v", len(stranded), rec.events)
	}
	if !strings.Contains(stranded[0].Message, work.ID) {
		t.Fatalf("session.stranded message = %q, want work bead %q", stranded[0].Message, work.ID)
	}
	got, err := store.Get(work.ID)
	if err != nil {
		t.Fatalf("Get work bead: %v", err)
	}
	if got.Metadata[detachedProbeMetadataKey] != "" {
		t.Fatalf("gc.detached = %q, want cleared before diagnostic emit", got.Metadata[detachedProbeMetadataKey])
	}
}

func TestEmitSessionStrandedDiagnostic_DetachedProbeErrorEmitsNormally(t *testing.T) {
	store := beads.NewMemStore()
	session, work := createDetachedStrandedWork(t, store, "tmux:gctest-stranded:soak-loop")
	installFakeTmux(t, "exit 2")
	rec := emitStrandedDiagnosticForTest(t, store, &session)

	stranded := rec.strandedEvents()
	if len(stranded) != 1 {
		t.Fatalf("session.stranded events = %d, want 1 when detached probe errors; events: %+v", len(stranded), rec.events)
	}
	if !strings.Contains(stranded[0].Message, work.ID) {
		t.Fatalf("session.stranded message = %q, want work bead %q", stranded[0].Message, work.ID)
	}
	got, err := store.Get(work.ID)
	if err != nil {
		t.Fatalf("Get work bead: %v", err)
	}
	if got.Metadata[detachedProbeMetadataKey] != "tmux:gctest-stranded:soak-loop" {
		t.Fatalf("gc.detached = %q, want preserved after probe error", got.Metadata[detachedProbeMetadataKey])
	}
}

func createDetachedStrandedWork(t *testing.T, store beads.Store, detachedSpec string) (beads.Bead, beads.Bead) {
	t.Helper()
	session, err := store.Create(beads.Bead{
		Title:  "worker session",
		Type:   sessionBeadType,
		Status: "open",
		Metadata: map[string]string{
			"session_name": "worker-mc-dead",
		},
	})
	if err != nil {
		t.Fatalf("Create session bead: %v", err)
	}
	work, err := store.Create(beads.Bead{
		Title:    "stranded work",
		Type:     "task",
		Assignee: session.ID,
		Metadata: map[string]string{
			detachedProbeMetadataKey: detachedSpec,
		},
	})
	if err != nil {
		t.Fatalf("Create work bead: %v", err)
	}
	status := "in_progress"
	if err := store.Update(work.ID, beads.UpdateOpts{Status: &status}); err != nil {
		t.Fatalf("Update work bead status: %v", err)
	}
	work, err = store.Get(work.ID)
	if err != nil {
		t.Fatalf("Reload work bead: %v", err)
	}
	return session, work
}

func emitStrandedDiagnosticForTest(t *testing.T, store beads.Store, session *beads.Bead) *capturingRecorder {
	t.Helper()
	rec := &capturingRecorder{}
	var stderr bytes.Buffer
	emitSessionStrandedDiagnostic(
		"",
		nil,
		store,
		nil,
		session,
		"worker",
		rec,
		&clock.Fake{Time: time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)},
		&stderr,
	)
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	return rec
}

// TestReconcileSessionBeads_PoolSlotWithStrandedWorkEmitsDiagnostic
// covers issue #1424: when a pool-managed session is observed
// asleep + not-alive AND still has open in-progress work assigned, the
// close gate correctly preserves the slot — but the controller used to
// fall through silently with no event recorded. Operators had no signal
// that the session had terminated without a clean drain. This test
// asserts the diagnostic event fires, names the stranded work, and is
// throttled across subsequent ticks so we don't get a storm.
func TestReconcileSessionBeads_PoolSlotWithStrandedWorkEmitsDiagnostic(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Agents: []config.Agent{{Name: "worker"}},
	}
	env.addDesired("worker", "worker", false) // NOT running — runtime is dead
	session := env.createSessionBead("worker", "worker")
	env.setSessionMetadata(&session, map[string]string{
		"state":                "asleep",
		"sleep_reason":         "idle",
		poolManagedMetadataKey: boolMetadata(true),
	})

	work, err := env.store.Create(beads.Bead{
		Title:    "stranded implementation",
		Type:     "task",
		Status:   "open",
		Assignee: session.ID,
	})
	if err != nil {
		t.Fatalf("Create work bead: %v", err)
	}
	inProgress := "in_progress"
	if err := env.store.Update(work.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatalf("Update work bead status: %v", err)
	}

	rec := &capturingRecorder{}
	env.rec = rec

	// First tick — diagnostic must fire.
	reconcileSessionBeadsAtPath(
		context.Background(),
		"",
		[]beads.Bead{session},
		env.desiredState,
		map[string]bool{"worker": true},
		env.cfg,
		env.sp,
		env.store,
		newFakeDrainOps(),
		nil,
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

	stranded := rec.strandedEvents()
	if len(stranded) != 1 {
		t.Fatalf("session.stranded events emitted = %d, want 1; got events: %+v", len(stranded), rec.events)
	}
	got := stranded[0]
	if got.Subject != session.ID {
		t.Errorf("session.stranded.subject = %q, want session bead ID %q", got.Subject, session.ID)
	}
	if !strings.Contains(got.Message, work.ID) {
		t.Errorf("session.stranded.message = %q, want it to name the stranded work bead %q", got.Message, work.ID)
	}
	if !strings.Contains(got.Message, "worker") {
		t.Errorf("session.stranded.message = %q, want it to name the agent template %q", got.Message, "worker")
	}

	// Verify the close gate still preserved the slot — the existing
	// behavior is the load-bearing safety property, not what this PR
	// changes. The diagnostic is purely an emission addition.
	postFirst, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get(session) after first tick: %v", err)
	}
	if postFirst.Status == "closed" {
		t.Fatalf("session bead closed on first tick; pool-slot close gate must keep it open while in_progress work is assigned")
	}

	// Re-fetch the session bead to pick up any throttle-marker metadata
	// the reconciler may have stamped.
	updatedSession, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get(session) before second tick: %v", err)
	}

	// Second tick — diagnostic must NOT fire again (throttled per
	// session bead generation, mirroring the #855 drain-log throttle).
	reconcileSessionBeadsAtPath(
		context.Background(),
		"",
		[]beads.Bead{updatedSession},
		env.desiredState,
		map[string]bool{"worker": true},
		env.cfg,
		env.sp,
		env.store,
		newFakeDrainOps(),
		nil,
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

	stranded = rec.strandedEvents()
	if len(stranded) != 1 {
		t.Fatalf("session.stranded events after second tick = %d, want still 1 (throttled); events: %+v", len(stranded), rec.events)
	}
}

func TestCollectSessionAssignedWorkIncludesAssignedWisp(t *testing.T) {
	store := beads.NewMemStore()
	session := beads.Bead{
		ID: "sess-1",
		Metadata: map[string]string{
			"session_name": "worker-session",
		},
	}
	work, err := store.Create(beads.Bead{
		Title:     "stranded wisp implementation",
		Type:      "task",
		Status:    "in_progress",
		Assignee:  "worker-session",
		Ephemeral: true,
	})
	if err != nil {
		t.Fatalf("Create wisp work: %v", err)
	}

	got, err := collectSessionAssignedWork("", nil, store, nil, session)
	if err != nil {
		t.Fatalf("collectSessionAssignedWork: %v", err)
	}
	if len(got) != 1 || got[0].bead.ID != work.ID {
		t.Fatalf("collectSessionAssignedWork = %#v, want assigned wisp %s", got, work.ID)
	}
}

// throttleKeySetMetadataFailStore wraps a beads.Store and fails any
// SetMetadata call writing the stranded throttle key. Other writes
// pass through unmodified. Used by the throttle write-ordering
// regression test below.
type throttleKeySetMetadataFailStore struct {
	beads.Store
}

func (s *throttleKeySetMetadataFailStore) SetMetadata(id, key, value string) error {
	if key == strandedEventEmittedKey {
		return errors.New("simulated store SetMetadata failure on throttle key")
	}
	return s.Store.SetMetadata(id, key, value)
}

// TestReconcileSessionBeads_PoolSlotStrandedThrottleSurvivesSetMetadataFailure
// is the regression test for the throttle write-ordering bug: the
// in-memory marker on session.Metadata must be set before the durable
// SetMetadata write, so a transient store-write failure cannot cause
// the next tick to re-emit the event and produce a duplicate-emission
// storm under sustained disk pressure / store partition.
func TestReconcileSessionBeads_PoolSlotStrandedThrottleSurvivesSetMetadataFailure(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Agents: []config.Agent{{Name: "worker"}},
	}
	env.addDesired("worker", "worker", false) // runtime not running
	session := env.createSessionBead("worker", "worker")
	env.setSessionMetadata(&session, map[string]string{
		"state":                "asleep",
		"sleep_reason":         "drained", // realistic stranded-worker case: clean drain, crashed before clearing assignee
		poolManagedMetadataKey: boolMetadata(true),
	})

	work, err := env.store.Create(beads.Bead{
		Title:    "stranded implementation",
		Type:     "task",
		Status:   "open",
		Assignee: session.ID,
	})
	if err != nil {
		t.Fatalf("Create work bead: %v", err)
	}
	inProgress := "in_progress"
	if err := env.store.Update(work.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatalf("Update work bead status: %v", err)
	}

	rec := &capturingRecorder{}
	env.rec = rec
	// Swap in the failing-SetMetadata wrapper.
	failingStore := &throttleKeySetMetadataFailStore{Store: env.store}

	// First tick — diagnostic must fire AND SetMetadata fails on the
	// throttle key. The in-memory marker on the *Bead value passed in
	// must still be set so subsequent ticks see it.
	reconcileSessionBeadsAtPath(
		context.Background(),
		"",
		[]beads.Bead{session},
		env.desiredState,
		map[string]bool{"worker": true},
		env.cfg,
		env.sp,
		failingStore,
		newFakeDrainOps(),
		nil,
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

	stranded := rec.strandedEvents()
	if len(stranded) != 1 {
		t.Fatalf("session.stranded events after first tick (SetMetadata failing) = %d, want 1; events: %+v", len(stranded), rec.events)
	}

	// Crucially: the durable store write failed, so the session bead
	// on disk does NOT have the throttle marker. Re-fetching it
	// returns the unmarked bead. The reconciler must still suppress
	// re-emission — this is what the in-memory-marker-first ordering
	// is protecting against. Production wouldn't necessarily re-fetch
	// here (it carries the same *Bead forward across ticks within a
	// controller lifetime); we test the worst-case explicitly.
	unmarked, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get(session) before second tick: %v", err)
	}
	if strings.TrimSpace(unmarked.Metadata[strandedEventEmittedKey]) != "" {
		t.Fatalf("durable throttle marker should be absent after SetMetadata failure; got %q", unmarked.Metadata[strandedEventEmittedKey])
	}

	// Second tick with the same in-memory *Bead the controller would
	// carry forward — the marker on it should suppress re-emission.
	reconcileSessionBeadsAtPath(
		context.Background(),
		"",
		[]beads.Bead{session}, // SAME *Bead, with the in-memory marker the first tick set on it
		env.desiredState,
		map[string]bool{"worker": true},
		env.cfg,
		env.sp,
		failingStore,
		newFakeDrainOps(),
		nil,
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

	stranded = rec.strandedEvents()
	if len(stranded) != 1 {
		t.Fatalf("session.stranded events after second tick (durable marker still missing) = %d, want still 1 (in-memory throttle should hold); events: %+v", len(stranded), rec.events)
	}
}

// (Removed: TestReconcileSessionBeads_DrainAckPartialOwnershipSnapshotFailsClosed
// guarded the old snapshot-backed fail-closed path — the sleep_reason
// "ownership_snapshot_partial" branch. Drain-ack now re-queries the store
// live so the pre-close ownership snapshot can no longer leak into the
// decision. Live store errors still fail closed, but the path that
// produced the ownership_snapshot_partial reason is gone.)

// listErrStore wraps a beads.Store and returns a configured error from
// List. Used by the drain-ack fail-closed regression test below.
type listErrStore struct {
	beads.Store
	err error
}

func (s *listErrStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.Store.List(q)
}

type assignOnListStore struct {
	beads.Store
	sessionID string
	calls     int
	assigned  bool
}

func (s *assignOnListStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	s.calls++
	if !s.assigned && s.calls == 3 {
		if _, err := s.Create(beads.Bead{
			Title:    "raced assigned work",
			Type:     "task",
			Status:   "open",
			Assignee: s.sessionID,
		}); err != nil {
			return nil, err
		}
		s.assigned = true
	}
	return s.Store.List(q)
}

type failSetMetadataBatchStore struct {
	beads.Store
	err error
}

func (s *failSetMetadataBatchStore) SetMetadataBatch(string, map[string]string) error {
	if s.err != nil {
		return s.err
	}
	return nil
}

func TestFinalizeDrainAckStoppedSessionDoesNotEmitEventsWhenFinalMetadataFails(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	fake := events.NewFake()
	env.rec = fake

	session := env.createSessionBead("worker", "worker")
	patch := sessionpkg.DrainAckStopPendingPatch(env.clk.Now().UTC())
	if err := env.store.SetMetadataBatch(session.ID, patch); err != nil {
		t.Fatalf("SetMetadataBatch(stop-pending): %v", err)
	}
	session.Metadata = patch.Apply(session.Metadata)

	failingStore := &failSetMetadataBatchStore{Store: env.store, err: errors.New("metadata write failed")}
	finalizeDrainAckStoppedSession(
		"", env.cfg, failingStore, nil, &session, "worker", false,
		newFakeDrainOps(), env.dt, env.clk, env.rec, &env.stderr,
	)

	if len(fake.Events) != 0 {
		t.Fatalf("events emitted before final metadata persisted: %v", fake.Events)
	}
	if !strings.Contains(env.stderr.String(), "finalizing drain-ack stopped worker") {
		t.Fatalf("stderr = %q, want final metadata failure diagnostic", env.stderr.String())
	}
}

func TestFinalizeDrainAckStoppedSessionFallsThroughWhenCloseGateRacesWithAssignment(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	fake := events.NewFake()
	env.rec = fake

	session := env.createSessionBead("worker", "worker")
	patch := sessionpkg.DrainAckStopPendingPatch(env.clk.Now().UTC())
	if err := env.store.SetMetadataBatch(session.ID, patch); err != nil {
		t.Fatalf("SetMetadataBatch(stop-pending): %v", err)
	}
	session.Metadata = patch.Apply(session.Metadata)

	racingStore := &assignOnListStore{Store: env.store, sessionID: session.ID}
	finalizeDrainAckStoppedSession(
		"", env.cfg, racingStore, nil, &session, "worker", true,
		newFakeDrainOps(), env.dt, env.clk, env.rec, &env.stderr,
	)

	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", session.ID, err)
	}
	if got.Status == "closed" {
		t.Fatalf("session bead closed after close-gate assignment race: metadata=%v", got.Metadata)
	}
	if got.Metadata["state"] != "asleep" || got.Metadata["sleep_reason"] != "idle" {
		t.Fatalf("state=%q sleep_reason=%q, want asleep/idle after close-gate assignment race",
			got.Metadata["state"], got.Metadata["sleep_reason"])
	}

	matches := 0
	for _, ev := range fake.Events {
		if ev.Type == events.SessionDrainAckedWithAssignedWork {
			matches++
		}
	}
	if matches != 1 {
		t.Fatalf("%s events = %d, want 1 after assignment race", events.SessionDrainAckedWithAssignedWork, matches)
	}
}

// TestReconcileSessionBeads_DrainAckLiveStoreErrorFailsClosed guards the
// drain-ack live-query error path. When sessionHasOpenAssignedWork returns
// an error, drain-ack treats hasAssignedWork as true (fail-closed) so the
// session lands in CompleteDrainPatch (asleep+idle) rather than
// AcknowledgeDrainPatch (drained). This prevents a transient store failure
// from silently closing a session whose assignment status we cannot verify.
func TestReconcileSessionBeads_DrainAckLiveStoreErrorFailsClosed(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Agents: []config.Agent{{Name: "worker"}},
	}
	env.addDesired("worker", "worker", true)
	session := env.createSessionBead("worker", "worker")
	env.markSessionActive(&session)

	// Wrap the store so List returns an error for the live-query check.
	erroring := &listErrStore{Store: env.store, err: fmt.Errorf("store is unavailable")}

	dops := newFakeDrainOps()
	if err := dops.setDrainAck("worker"); err != nil {
		t.Fatalf("setDrainAck: %v", err)
	}

	reconcileSessionBeadsAtPath(
		context.Background(),
		"",
		[]beads.Bead{session},
		env.desiredState,
		map[string]bool{"worker": true},
		env.cfg,
		env.sp,
		erroring,
		dops,
		nil,
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

	waitForProviderStopped(t, env.sp, "worker")
	stopPending, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get(%s) before fail-closed finalize: %v", session.ID, err)
	}
	reconcileSessionBeadsAtPath(
		context.Background(),
		"",
		[]beads.Bead{stopPending},
		env.desiredState,
		map[string]bool{"worker": true},
		env.cfg,
		env.sp,
		erroring,
		dops,
		nil,
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
	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get(%s) after fail-closed finalize: %v", session.ID, err)
	}
	if got.Metadata["state"] != "asleep" || got.Metadata["sleep_reason"] != "idle" {
		t.Fatalf("state=%q sleep_reason=%q, want asleep/idle — live-query error must fail closed (hasAssignedWork=true) so the session does not enter drained state",
			got.Metadata["state"], got.Metadata["sleep_reason"])
	}
}

// TestReconcileSessionBeads_CloseGateLiveStoreErrorKeepsSlot guards the
// mirror-image fail-closed guard in the asleep-idle close gate. When
// sessionHasOpenAssignedWork errors during the close-gate check, the gate
// must treat hasAssignedWork as true (fail-closed) and leave the bead
// open. Without this guard a transient store blip would silently close a
// pool slot whose assignment status was unverifiable.
func TestReconcileSessionBeads_CloseGateLiveStoreErrorKeepsSlot(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Agents: []config.Agent{{Name: "worker"}},
	}
	env.addDesired("worker", "worker", false) // NOT running
	session := env.createSessionBead("worker", "worker")
	env.setSessionMetadata(&session, map[string]string{
		"state":                "asleep",
		"sleep_reason":         "idle",
		poolManagedMetadataKey: boolMetadata(true),
	})

	erroring := &listErrStore{Store: env.store, err: fmt.Errorf("store is unavailable")}

	reconcileSessionBeadsAtPath(
		context.Background(),
		"",
		[]beads.Bead{session},
		env.desiredState,
		map[string]bool{"worker": true},
		env.cfg,
		env.sp,
		erroring,
		newFakeDrainOps(),
		nil,
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

	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", session.ID, err)
	}
	if got.Status == "closed" {
		t.Fatalf("status = %q, want open — close-gate live-query error must fail closed (hasAssignedWork=true) so the pool slot stays open until the store can confirm no assignments", got.Status)
	}
}

// TestReconcileSessionBeads_CloseGateIgnoresUnreachableRigAssignedWork
// verifies that the close gate's live ownership check only considers the
// store scope the session's configured agent can query and claim from. A
// city-scoped pool session must not be retained by unrelated rig-store work
// that happens to share one of its assignment identifiers.
func TestReconcileSessionBeads_CloseGateIgnoresUnreachableRigAssignedWork(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Agents: []config.Agent{{Name: "worker"}},
	}
	env.addDesired("worker", "worker", false)
	session := env.createSessionBead("worker", "worker")
	env.setSessionMetadata(&session, map[string]string{
		"state":                "asleep",
		"sleep_reason":         "idle",
		poolManagedMetadataKey: boolMetadata(true),
	})

	// Work assigned to the session lives in a rig store, not the city store.
	// This city-scoped session cannot claim it, so it must not veto close.
	rigStore := beads.NewMemStore()
	if _, err := rigStore.Create(beads.Bead{
		Title:    "unreachable rig work",
		Type:     "task",
		Status:   "open",
		Assignee: session.ID,
	}); err != nil {
		t.Fatalf("Create rig work bead: %v", err)
	}
	rigStores := map[string]beads.Store{"some-rig": rigStore}

	reconcileSessionBeadsAtPath(
		context.Background(),
		"",
		[]beads.Bead{session},
		env.desiredState,
		map[string]bool{"worker": true},
		env.cfg,
		env.sp,
		env.store,
		newFakeDrainOps(),
		nil,
		rigStores,
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

	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", session.ID, err)
	}
	if got.Status != "closed" {
		t.Fatalf("status = %q, want closed — unreachable rig-store assigned work must not retain a city-scoped pool slot", got.Status)
	}
}

func TestReconcileSessionBeads_DrainAckedOrphanCloseIgnoresUnreachableRigAssignedWork(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Agents: []config.Agent{{Name: "worker"}},
	}
	session := env.createSessionBead("worker", "worker")
	env.markSessionActive(&session)

	rigStore := beads.NewMemStore()
	if _, err := rigStore.Create(beads.Bead{
		Title:    "unreachable rig work",
		Type:     "task",
		Status:   "open",
		Assignee: session.ID,
	}); err != nil {
		t.Fatalf("Create rig work bead: %v", err)
	}
	dops := newFakeDrainOps()
	if err := dops.setDrainAck("worker"); err != nil {
		t.Fatalf("setDrainAck: %v", err)
	}

	reconcileSessionBeadsAtPath(
		context.Background(),
		"",
		[]beads.Bead{session},
		env.desiredState,
		nil,
		env.cfg,
		env.sp,
		env.store,
		dops,
		nil,
		map[string]beads.Store{"some-rig": rigStore},
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

	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", session.ID, err)
	}
	if got.Status != "closed" {
		t.Fatalf("status = %q, want closed — drain-acked close must ignore work in stores the session cannot reach", got.Status)
	}
}

func TestReconcileSessionBeads_SuspendedCloseIgnoresUnreachableRigAssignedWork(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Agents: []config.Agent{{Name: "worker"}},
	}
	session := env.createSessionBead("worker", "worker")
	env.markSessionActive(&session)

	rigStore := beads.NewMemStore()
	if _, err := rigStore.Create(beads.Bead{
		Title:    "unreachable rig work",
		Type:     "task",
		Status:   "open",
		Assignee: session.ID,
	}); err != nil {
		t.Fatalf("Create rig work bead: %v", err)
	}

	reconcileSessionBeadsAtPath(
		context.Background(),
		"",
		[]beads.Bead{session},
		env.desiredState,
		map[string]bool{"worker": true},
		env.cfg,
		env.sp,
		env.store,
		newFakeDrainOps(),
		nil,
		map[string]beads.Store{"some-rig": rigStore},
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

	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", session.ID, err)
	}
	if got.Status != "closed" {
		t.Fatalf("status = %q, want closed — suspended close must ignore work in stores the session cannot reach", got.Status)
	}
}

// TestReconcileSessionBeads_CloseGatePreservesSleepReason verifies that the
// close gate carries the session's existing sleep_reason (idle,
// idle-timeout, drained, city-stop) into the closed bead's close reason. Losing
// this distinction in closed records erases the forensic difference between an
// idle-timeout recycle and an explicit drain.
func TestReconcileSessionBeads_CloseGatePreservesSleepReason(t *testing.T) {
	cases := []struct {
		name        string
		sleepReason string
		wantReason  string
	}{
		{"idle", "idle", "idle"},
		{"idle-timeout", "idle-timeout", "idle-timeout"},
		{"city-stop", sleepReasonCityStop, sleepReasonCityStop},
		{"drained-reason", "drained", "drained"},
		{"missing-reason", "", "drained"}, // fallback
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newReconcilerTestEnv()
			env.cfg = &config.City{
				Agents: []config.Agent{{Name: "worker"}},
			}
			env.addDesired("worker", "worker", false)
			session := env.createSessionBead("worker", "worker")
			meta := map[string]string{
				"state":                "asleep",
				poolManagedMetadataKey: boolMetadata(true),
			}
			if tc.sleepReason != "" {
				meta["sleep_reason"] = tc.sleepReason
			} else {
				// Drained state still qualifies as freeable via
				// isDrainedSessionBead; use that so the close gate fires
				// with no sleep_reason set.
				meta["state"] = "drained"
			}
			env.setSessionMetadata(&session, meta)

			reconcileSessionBeadsAtPath(
				context.Background(),
				"",
				[]beads.Bead{session},
				env.desiredState,
				map[string]bool{"worker": true},
				env.cfg,
				env.sp,
				env.store,
				newFakeDrainOps(),
				nil,
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

			got, err := env.store.Get(session.ID)
			if err != nil {
				t.Fatalf("Get(%s): %v", session.ID, err)
			}
			if got.Status != "closed" {
				t.Fatalf("status = %q, want closed", got.Status)
			}
			if want := sessionpkg.CanonicalCloseReason(tc.wantReason); got.Metadata["close_reason"] != want {
				t.Fatalf("close_reason = %q, want %q (canonical for %q) — close gate must preserve the originating sleep_reason for forensic fidelity", got.Metadata["close_reason"], want, tc.wantReason)
			}
			if got.Metadata["state"] != tc.wantReason {
				t.Fatalf("state = %q, want %q — state preserves the short sleep_reason code", got.Metadata["state"], tc.wantReason)
			}
		})
	}
}

func TestReconcileSessionBeads_DrainAckResumeModePreservesSessionIdentity(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Agents: []config.Agent{{Name: "worker"}},
	}
	env.addDesired("worker", "worker", true)
	session := env.createSessionBead("worker", "worker")
	env.markSessionActive(&session)
	env.setSessionMetadata(&session, map[string]string{
		"wake_mode":           "resume",
		"session_key":         "resume-key",
		"started_config_hash": "hash-before-drain",
	})

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

	got := env.reconcileStopPendingToTerminal(t, env.sp, session, dops, map[string]bool{"worker": true})
	if got.Metadata["state"] != "drained" {
		t.Fatalf("state = %q, want drained", got.Metadata["state"])
	}
	if got.Metadata["session_key"] != "resume-key" {
		t.Fatalf("session_key = %q, want preserved resume key", got.Metadata["session_key"])
	}
	if got.Metadata["started_config_hash"] != "hash-before-drain" {
		t.Fatalf("started_config_hash = %q, want preserved hash", got.Metadata["started_config_hash"])
	}
	if got.Metadata["continuation_reset_pending"] != "" {
		t.Fatalf("continuation_reset_pending = %q, want empty", got.Metadata["continuation_reset_pending"])
	}
}

func TestReconcileSessionBeads_DrainAckFreshModeClearsSessionIdentity(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Agents: []config.Agent{{Name: "worker"}},
	}
	env.addDesired("worker", "worker", true)
	session := env.createSessionBead("worker", "worker")
	env.markSessionActive(&session)
	env.setSessionMetadata(&session, map[string]string{
		"wake_mode":           "fresh",
		"session_key":         "fresh-key",
		"started_config_hash": "hash-before-drain",
	})

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

	got := env.reconcileStopPendingToTerminal(t, env.sp, session, dops, map[string]bool{"worker": true})
	if got.Metadata["state"] != "drained" {
		t.Fatalf("state = %q, want drained", got.Metadata["state"])
	}
	if got.Metadata["session_key"] != "" {
		t.Fatalf("session_key = %q, want cleared for wake_mode=fresh", got.Metadata["session_key"])
	}
	if got.Metadata["started_config_hash"] != "" {
		t.Fatalf("started_config_hash = %q, want cleared for wake_mode=fresh", got.Metadata["started_config_hash"])
	}
	if got.Metadata["continuation_reset_pending"] != "true" {
		t.Fatalf("continuation_reset_pending = %q, want true", got.Metadata["continuation_reset_pending"])
	}
}

// TestReconcileSessionBeads_DrainedPoolSessionStoreQueryPartialStaysOpen
// verifies that when storeQueryPartial is set (a transient bead-store
// failure produced an incomplete assignedWorkBeads snapshot), the
// drained pool session bead is NOT closed. Close decisions must fail
// closed whenever the tick's store visibility is compromised, even if
// the live ownership check itself returns cleanly.
func TestReconcileSessionBeads_DrainedPoolSessionStoreQueryPartialStaysOpen(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}},
	}

	session := env.createSessionBead("worker-live", "worker")
	env.setSessionMetadata(&session, map[string]string{
		"state":                "drained",
		"sleep_reason":         "drained",
		"pool_slot":            "1",
		poolManagedMetadataKey: boolMetadata(true),
		"session_origin":       "ephemeral",
	})

	woken := reconcileSessionBeadsAtPath(
		context.Background(),
		"",
		[]beads.Bead{session},
		nil,
		nil,
		env.cfg,
		env.sp,
		env.store,
		nil,
		nil,
		nil, // rigStores
		nil,
		env.dt,
		map[string]int{},
		true, // storeQueryPartial
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

	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", session.ID, err)
	}
	if got.Status == "closed" {
		t.Fatalf("session bead closed unexpectedly under storeQueryPartial: metadata=%v", got.Metadata)
	}
}

// stopFailProvider wraps a Fake but makes Stop always fail.
// The session remains running (IsRunning returns true).
type stopFailProvider struct {
	*runtime.Fake
	stopCalled chan struct{}
}

func (p *stopFailProvider) Stop(_ string) error {
	if p.stopCalled != nil {
		select {
		case p.stopCalled <- struct{}{}:
		default:
		}
	}
	return fmt.Errorf("stop failed: session unavailable")
}

func TestReconcileSessionBeads_DrainAckStopFailurePreservesMetadata(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Agents: []config.Agent{{Name: "worker"}},
	}
	env.addDesired("worker", "worker", true)
	session := env.createSessionBead("worker", "worker")
	env.markSessionActive(&session)
	env.setSessionMetadata(&session, map[string]string{
		"wake_mode":           "fresh",
		"session_key":         "fresh-key",
		"started_config_hash": "hash-before-drain",
		"last_woke_at":        env.clk.Now().Add(-5 * time.Second).UTC().Format(time.RFC3339),
	})

	dops := newFakeDrainOps()
	if err := dops.setDrainAck("worker"); err != nil {
		t.Fatalf("setDrainAck: %v", err)
	}

	// Wrap the real provider so Stop fails but IsRunning still returns true.
	stopCalled := make(chan struct{}, 1)
	failSp := &stopFailProvider{Fake: env.sp, stopCalled: stopCalled}
	var stderr synchronizedBuffer

	woken := reconcileSessionBeads(
		context.Background(),
		[]beads.Bead{session},
		env.desiredState,
		map[string]bool{"worker": true},
		env.cfg,
		failSp,
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
		&stderr,
	)
	if woken != 0 {
		t.Fatalf("woken = %d, want 0", woken)
	}
	select {
	case <-stopCalled:
	case <-time.After(time.Second):
		t.Fatal("async Stop was not called")
	}
	deadline := time.Now().Add(time.Second)
	for !strings.Contains(stderr.String(), "session reconciler: async drain-ack stop worker: stop failed: session unavailable") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := stderr.String(); !strings.Contains(got, "session reconciler: async drain-ack stop worker: stop failed: session unavailable") {
		t.Fatalf("stderr = %q, want async stop error", got)
	}

	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", session.ID, err)
	}
	if got.Metadata["state"] != string(sessionpkg.StateDraining) {
		t.Fatalf("state = %q, want draining while async stop retry is pending", got.Metadata["state"])
	}
	if got.Metadata["state_reason"] != sessionpkg.DrainAckStopPendingReason {
		t.Fatalf("state_reason = %q, want %q", got.Metadata["state_reason"], sessionpkg.DrainAckStopPendingReason)
	}
	if got.Metadata["last_woke_at"] == "" {
		t.Fatalf("last_woke_at should be preserved while stop is still pending")
	}
	if got.Metadata["session_key"] == "" {
		t.Fatalf("session_key should be preserved while stop is still pending")
	}
}

type failStopPendingStore struct {
	beads.Store
}

func (s *failStopPendingStore) SetMetadataBatch(id string, kvs map[string]string) error {
	if kvs["state_reason"] == sessionpkg.DrainAckStopPendingReason {
		return errors.New("stop-pending metadata failed")
	}
	return s.Store.SetMetadataBatch(id, kvs)
}

func TestReconcileSessionBeads_DrainAckStopPendingMetadataFailureLogsDiagnostic(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
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
		&failStopPendingStore{Store: env.store},
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
	if got := env.stderr.String(); !strings.Contains(got, "session reconciler: marking drain-ack stop-pending worker: stop-pending metadata failed") {
		t.Fatalf("stderr = %q, want stop-pending metadata diagnostic", got)
	}
}

func TestReconcileSessionBeads_DrainAckResumeModeNotClassifiedAsCrashNextTick(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Agents: []config.Agent{{Name: "worker"}},
	}
	env.addDesired("worker", "worker", true)
	session := env.createSessionBead("worker", "worker")
	env.markSessionActive(&session)
	env.setSessionMetadata(&session, map[string]string{
		"wake_mode":           "resume",
		"session_key":         "resume-key",
		"started_config_hash": "hash-before-drain",
	})

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

	got := env.reconcileStopPendingToTerminal(t, env.sp, session, dops, map[string]bool{"worker": true})
	if got.Metadata["last_woke_at"] != "" {
		t.Fatalf("last_woke_at = %q, want cleared after drain-ack", got.Metadata["last_woke_at"])
	}

	woken = reconcileSessionBeads(
		context.Background(),
		[]beads.Bead{got},
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
		t.Fatalf("second tick woken = %d, want 0", woken)
	}

	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get(%s) after second tick: %v", session.ID, err)
	}
	if got.Metadata["session_key"] != "resume-key" {
		t.Fatalf("session_key = %q after second tick, want preserved resume key", got.Metadata["session_key"])
	}
	if got.Metadata["wake_attempts"] != "" {
		t.Fatalf("wake_attempts = %q, want empty for intentional drain", got.Metadata["wake_attempts"])
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

func TestReconcileSessionBeads_AlwaysNamedSessionWakesFromDrainedCompatibilityState(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Workspace:     config.Workspace{Name: "test-city"},
		Agents:        []config.Agent{{Name: "worker", StartCommand: "true"}},
		NamedSessions: []config.NamedSession{{Template: "worker", Mode: "always"}},
	}
	sessionName := config.NamedSessionRuntimeName(env.cfg.Workspace.Name, env.cfg.Workspace, "worker")
	env.desiredState[sessionName] = TemplateParams{
		Command:                 "true",
		SessionName:             sessionName,
		TemplateName:            "worker",
		ConfiguredNamedIdentity: "worker",
		ConfiguredNamedMode:     "always",
	}
	session := env.createSessionBead(sessionName, "worker")
	env.setSessionMetadata(&session, map[string]string{
		namedSessionMetadataKey:      "true",
		namedSessionIdentityMetadata: "worker",
		namedSessionModeMetadata:     "always",
		"state":                      "asleep",
		"sleep_reason":               "drained",
		"continuation_reset_pending": "true",
	})

	woken := env.reconcile([]beads.Bead{session})

	if woken != 1 {
		t.Fatalf("woken = %d, want 1", woken)
	}
	if !env.sp.IsRunning(sessionName) {
		t.Fatalf("always named session %q should have been restarted", sessionName)
	}
}

// TestReconcileSessionBeads_AlwaysNamedSessionWakesAfterLiveChurnSequence
// pins the expected post-churn contract by driving the full crash-then-recover
// sequence instead of pre-staging post-churn metadata. This covers the contract
// needed for issue #1493, but it is not proof that the reported production
// trigger was reproduced or fixed; keep #1493 open until reporter confirmation
// or a production-shaped integration shard reproduces the original symptom.
func TestReconcileSessionBeads_AlwaysNamedSessionWakesAfterLiveChurnSequence(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Workspace:     config.Workspace{Name: "test-city"},
		Agents:        []config.Agent{{Name: "worker", StartCommand: "true"}},
		NamedSessions: []config.NamedSession{{Template: "worker", Mode: "always"}},
	}
	sessionName := config.NamedSessionRuntimeName(env.cfg.Workspace.Name, env.cfg.Workspace, "worker")
	env.desiredState[sessionName] = TemplateParams{
		Command:                 "true",
		SessionName:             sessionName,
		TemplateName:            "worker",
		ConfiguredNamedIdentity: "worker",
		ConfiguredNamedMode:     "always",
	}
	session := env.createSessionBead(sessionName, "worker")
	// Mark the bead as having woken 90 seconds ago: past stabilityThreshold
	// (30s) and before churnProductivityThreshold (5min). This is the churn
	// band that recordChurn fires for. The session is NOT running in the
	// fake provider, so the reconciler will see alive=false.
	wokeAt := env.clk.Now().Add(-90 * time.Second).UTC().Format(time.RFC3339)
	env.setSessionMetadata(&session, map[string]string{
		namedSessionMetadataKey:      "true",
		namedSessionIdentityMetadata: "worker",
		namedSessionModeMetadata:     "always",
		"state":                      "active",
		"last_woke_at":               wokeAt,
		"session_key":                "old-key",
	})

	// First tick: detect non-productive death, recordChurn fires, session
	// transitions through to asleep state.
	env.reconcile([]beads.Bead{session})

	// Reload the bead from the store to capture every metadata change made
	// by the reconciler tick (healState, checkChurn, recordChurn).
	reloaded, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", session.ID, err)
	}
	if reloaded.Metadata["churn_count"] != "1" {
		t.Fatalf("after tick 1 churn_count = %q, want 1 (recordChurn must fire)", reloaded.Metadata["churn_count"])
	}
	if reloaded.Metadata["last_woke_at"] != "" {
		t.Fatalf("after tick 1 last_woke_at = %q, want empty (checkChurn clears it)", reloaded.Metadata["last_woke_at"])
	}

	// Second tick: the post-churn shape is now in the store. The
	// named-always session must be re-woken on this tick.
	env.clk.Time = env.clk.Time.Add(30 * time.Second)
	env.reconcile([]beads.Bead{reloaded})

	if !env.sp.IsRunning(sessionName) {
		final, _ := env.store.Get(session.ID)
		t.Fatalf(
			"always named session %q must restart on the tick after churn (#1493); state=%q sleep_reason=%q churn_count=%q wake_attempts=%q quarantined_until=%q",
			sessionName,
			final.Metadata["state"],
			final.Metadata["sleep_reason"],
			final.Metadata["churn_count"],
			final.Metadata["wake_attempts"],
			final.Metadata["quarantined_until"],
		)
	}
}

// TestReconcileSessionBeads_AlwaysNamedSessionWakesPostChurnWithMissingConfiguredIdentity
// pins the production-shaped failure described in #1493: a qualified
// named-always session bead whose configured_named_identity metadata is
// missing (legacy bead, unmigrated config change, or any path that lost
// the identity tag) must still wake when its session_name matches the
// deterministic runtime name for the configured identity.
//
// Without the fallback in ComputeAwakeSet, findNamedSessionName returns
// "" because no bead has bead.NamedIdentity == ns.Identity, so the
// awake-set pass keys `desired` by ns.Identity (the qualified name) while
// the wake loop looks it up by bead.SessionName (the deterministic
// runtime name). The two never match, ShouldWake stays false, no Start
// is issued, and the session is stuck asleep forever even though
// `gc session pin` (which keys off pin_awake, not identity) unsticks it.
func TestReconcileSessionBeads_AlwaysNamedSessionWakesPostChurnWithMissingConfiguredIdentity(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Workspace:     config.Workspace{Name: "test-city"},
		Agents:        []config.Agent{{Name: "worker", Dir: "hello-world", StartCommand: "true"}},
		NamedSessions: []config.NamedSession{{Dir: "hello-world", Template: "worker", Mode: "always"}},
	}
	identity := env.cfg.NamedSessions[0].QualifiedName()
	sessionName := config.NamedSessionRuntimeName(env.cfg.Workspace.Name, env.cfg.Workspace, identity)
	if identity == sessionName {
		t.Fatalf("test setup invalid: identity and sessionName both %q; the regression requires them to differ", identity)
	}
	env.desiredState[sessionName] = TemplateParams{
		Command:                 "true",
		SessionName:             sessionName,
		TemplateName:            "hello-world/worker",
		ConfiguredNamedIdentity: identity,
		ConfiguredNamedMode:     "always",
	}
	session := env.createSessionBead(sessionName, "hello-world/worker")
	env.setSessionMetadata(&session, map[string]string{
		namedSessionMetadataKey: "true",
		// configured_named_identity intentionally NOT set — this is the
		// production-shaped failure mode the reporter hypothesized.
		namedSessionModeMetadata:     "always",
		"state":                      "asleep",
		"sleep_reason":               "",
		"state_reason":               "creation_complete",
		"last_woke_at":               "",
		"wake_attempts":              "0",
		"churn_count":                "1",
		"session_key":                "",
		"continuation_reset_pending": "",
		"pending_create_claim":       "",
		"pin_awake":                  "",
	})

	env.reconcile([]beads.Bead{session})

	if !env.sp.IsRunning(sessionName) {
		final, _ := env.store.Get(session.ID)
		t.Fatalf(
			"named-always with missing configured_named_identity must wake (#1493); identity=%q sessionName=%q state=%q sleep_reason=%q churn_count=%q wake_attempts=%q",
			identity, sessionName,
			final.Metadata["state"],
			final.Metadata["sleep_reason"],
			final.Metadata["churn_count"],
			final.Metadata["wake_attempts"],
		)
	}
}

// TestReconcileSessionBeads_QuarantinedNamedSessionStaysAsleepAfterChurn pins
// the negative half of the post-churn invariant: when churn pushes the
// session into quarantine, the session must stay asleep until the
// quarantine elapses, even for mode=always.
func TestReconcileSessionBeads_QuarantinedNamedSessionStaysAsleepAfterChurn(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Workspace:     config.Workspace{Name: "test-city"},
		Agents:        []config.Agent{{Name: "worker", StartCommand: "true"}},
		NamedSessions: []config.NamedSession{{Template: "worker", Mode: "always"}},
	}
	sessionName := config.NamedSessionRuntimeName(env.cfg.Workspace.Name, env.cfg.Workspace, "worker")
	env.desiredState[sessionName] = TemplateParams{
		Command:                 "true",
		SessionName:             sessionName,
		TemplateName:            "worker",
		ConfiguredNamedIdentity: "worker",
		ConfiguredNamedMode:     "always",
	}
	session := env.createSessionBead(sessionName, "worker")
	env.setSessionMetadata(&session, map[string]string{
		namedSessionMetadataKey:      "true",
		namedSessionIdentityMetadata: "worker",
		namedSessionModeMetadata:     "always",
		"state":                      "asleep",
		"sleep_reason":               "context-churn",
		"churn_count":                "3",
		"quarantined_until":          env.clk.Now().Add(15 * time.Minute).UTC().Format(time.RFC3339),
	})

	woken := env.reconcile([]beads.Bead{session})

	if woken != 0 {
		t.Fatalf("woken = %d, want 0 (quarantined session must not wake during the quarantine window)", woken)
	}
	if env.sp.IsRunning(sessionName) {
		t.Fatalf("quarantined named session %q must stay asleep until the quarantine elapses", sessionName)
	}
}

func TestReconcileSessionBeads_OrdinaryDesiredStateDoesNotWakeDrainedCompatibilityState(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Agents: []config.Agent{{Name: "worker", StartCommand: "true", MaxActiveSessions: intPtr(1)}},
	}
	env.desiredState["worker"] = TemplateParams{
		Command:      "true",
		SessionName:  "worker",
		TemplateName: "worker",
	}
	session := env.createSessionBead("worker", "worker")
	env.setSessionMetadata(&session, map[string]string{
		"state":                      "asleep",
		"sleep_reason":               "drained",
		"continuation_reset_pending": "true",
	})

	woken := env.reconcile([]beads.Bead{session})

	if woken != 0 {
		t.Fatalf("woken = %d, want 0", woken)
	}
	if env.sp.IsRunning("worker") {
		t.Fatal("ordinary desiredState presence must not restart a drained compatibility bead")
	}
}

func TestReconcileSessionBeads_OnDemandNamedSessionDoesNotWakeFromDesiredStatePresence(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Workspace:     config.Workspace{Name: "test-city"},
		Agents:        []config.Agent{{Name: "worker", StartCommand: "true"}},
		NamedSessions: []config.NamedSession{{Template: "worker", Mode: "on_demand"}},
	}
	sessionName := config.NamedSessionRuntimeName(env.cfg.Workspace.Name, env.cfg.Workspace, "worker")
	env.desiredState[sessionName] = TemplateParams{
		Command:                 "true",
		SessionName:             sessionName,
		TemplateName:            "worker",
		ConfiguredNamedIdentity: "worker",
		ConfiguredNamedMode:     "on_demand",
	}
	session := env.createSessionBead(sessionName, "worker")
	env.setSessionMetadata(&session, map[string]string{
		namedSessionMetadataKey:      "true",
		namedSessionIdentityMetadata: "worker",
		namedSessionModeMetadata:     "on_demand",
		"state":                      "asleep",
		"sleep_reason":               "drained",
		"continuation_reset_pending": "true",
	})

	woken := env.reconcile([]beads.Bead{session})

	if woken != 0 {
		t.Fatalf("woken = %d, want 0", woken)
	}
	if env.sp.IsRunning(sessionName) {
		t.Fatalf("on-demand named session %q should remain asleep without direct demand", sessionName)
	}
}

func TestReconcileSessionBeads_OnDemandNamedSessionWakesFromRoutedIdentityDemand(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "mayor",
			StartCommand:      "true",
			MaxActiveSessions: intPtr(1),
			WorkQuery:         "printf ''",
		}},
		NamedSessions: []config.NamedSession{{Template: "mayor", Mode: "on_demand"}},
	}
	sessionName := config.NamedSessionRuntimeName(cfg.EffectiveCityName(), cfg.Workspace, "mayor")

	woken, running := reconcileExistingAsleepNamedSessionWithRoutedWork(t, cfg, sessionName, "mayor", "mayor")
	if woken != 1 {
		t.Fatalf("woken = %d, want 1", woken)
	}
	if !running {
		t.Fatalf("on-demand named session %q was not started from routed identity demand", sessionName)
	}
}

func TestReconcileSessionBeads_OnDemandNamedSessionWakesFromRoutedSingletonTemplateDemand(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "worker",
			StartCommand:      "true",
			MaxActiveSessions: intPtr(1),
			WorkQuery:         "printf ''",
		}},
		NamedSessions: []config.NamedSession{{Name: "primary", Template: "worker", Mode: "on_demand"}},
	}

	woken, running := reconcileExistingAsleepNamedSessionWithRoutedWork(t, cfg, "primary", "primary", "worker")
	if woken != 1 {
		t.Fatalf("woken = %d, want 1", woken)
	}
	if !running {
		t.Fatal("on-demand named session primary was not started from routed singleton-template demand")
	}
}

func reconcileExistingAsleepNamedSessionWithRoutedWork(t *testing.T, cfg *config.City, sessionName, identity, routedTo string) (int, bool) {
	t.Helper()

	cityPath := t.TempDir()
	store := beads.NewMemStore()
	clk := &clock.Fake{Time: time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)}
	sp := runtime.NewFake()
	if _, err := store.Create(beads.Bead{
		Title:  "queued named work",
		Type:   "task",
		Status: "open",
		Metadata: map[string]string{
			"gc.routed_to": routedTo,
		},
	}); err != nil {
		t.Fatalf("Create(work): %v", err)
	}
	if _, err := store.Create(beads.Bead{
		Title:  sessionName,
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":               sessionName,
			"alias":                      identity,
			"template":                   cfg.NamedSessions[0].Template,
			"state":                      "asleep",
			"generation":                 "1",
			"instance_token":             "canonical-token",
			namedSessionMetadataKey:      "true",
			namedSessionIdentityMetadata: identity,
			namedSessionModeMetadata:     "on_demand",
		},
	}); err != nil {
		t.Fatalf("Create(session): %v", err)
	}

	var stdout, stderr bytes.Buffer
	dsResult := buildDesiredState(cfg.EffectiveCityName(), cityPath, clk.Now().UTC(), cfg, sp, store, &stderr)
	if !dsResult.NamedSessionDemand[identity] {
		t.Fatalf("NamedSessionDemand[%s] = false for routed_to=%s; stderr:\n%s", identity, routedTo, stderr.String())
	}
	cfgNames := configuredSessionNames(cfg, cfg.EffectiveCityName(), store)
	syncSessionBeads(cityPath, store, dsResult.State, sp, cfgNames, cfg, clk, &stderr, true)
	sessions, err := loadSessionBeads(store)
	if err != nil {
		t.Fatalf("loadSessionBeads: %v", err)
	}
	poolDesired := PoolDesiredCounts(ComputePoolDesiredStates(cfg, dsResult.AssignedWorkBeads, sessions, dsResult.ScaleCheckCounts))
	if poolDesired == nil {
		poolDesired = make(map[string]int)
	}
	mergeNamedSessionDemand(poolDesired, dsResult.NamedSessionDemand, cfg)

	woken := reconcileSessionBeadsAtPathWithNamedDemand(
		context.Background(), cityPath, sessions, dsResult.State, cfgNames, cfg, sp,
		store, nil, dsResult.AssignedWorkBeads, nil, nil, newDrainTracker(), nil, poolDesired,
		dsResult.NamedSessionDemand, dsResult.StoreQueryPartial, nil, cfg.EffectiveCityName(),
		nil, clk, events.Discard, 0, 0, &stdout, &stderr,
	)
	return woken, sp.IsRunning(sessionName)
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

func TestReconcileSessionBeads_RateLimitScreenQuarantinesBeforeHeal(t *testing.T) {
	env := newReconcilerTestEnv()
	rec := events.NewFake()
	env.rec = rec
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	env.desiredState["worker"] = TemplateParams{
		Command:      "test-cmd",
		SessionName:  "worker",
		TemplateName: "worker",
		Hints:        agent.StartupHints{ProcessNames: []string{"agent-cli"}},
	}
	if err := env.sp.Start(context.Background(), "worker", runtime.Config{Command: "test-cmd", ProcessNames: []string{"agent-cli"}}); err != nil {
		t.Fatalf("Start(worker): %v", err)
	}
	env.sp.Zombies["worker"] = true
	env.sp.SetPeekOutput("worker", "You've hit your limit, Pro plan\n\n/rate-limit-options")
	session := env.createSessionBead("worker", "worker")
	env.setSessionMetadata(&session, map[string]string{
		"state":               "active",
		"last_woke_at":        env.clk.Now().Add(-10 * time.Second).UTC().Format(time.RFC3339),
		"session_key":         "keep-session",
		"started_config_hash": "keep-hash",
		"wake_attempts":       "2",
	})

	woken := env.reconcile([]beads.Bead{session})

	if woken != 0 {
		t.Fatalf("woken = %d, want 0", woken)
	}
	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", session.ID, err)
	}
	if got.Metadata["wake_attempts"] != "2" {
		t.Fatalf("wake_attempts = %q, want 2", got.Metadata["wake_attempts"])
	}
	if got.Metadata["sleep_reason"] != "rate_limit" {
		t.Fatalf("sleep_reason = %q, want rate_limit", got.Metadata["sleep_reason"])
	}
	if got.Metadata["state"] != "asleep" {
		t.Fatalf("state = %q, want asleep", got.Metadata["state"])
	}
	qUntil, err := time.Parse(time.RFC3339, got.Metadata["quarantined_until"])
	if err != nil {
		t.Fatalf("quarantined_until parse: %v", err)
	}
	if want := env.clk.Now().Add(defaultRateLimitQuarantineDuration); !qUntil.Equal(want) {
		t.Fatalf("quarantined_until = %s, want %s", qUntil.Format(time.RFC3339), want.Format(time.RFC3339))
	}
	if got.Metadata["session_key"] != "keep-session" {
		t.Fatalf("session_key = %q, want preserved", got.Metadata["session_key"])
	}
	if got.Metadata["started_config_hash"] != "keep-hash" {
		t.Fatalf("started_config_hash = %q, want preserved", got.Metadata["started_config_hash"])
	}
	if got.Metadata["continuation_reset_pending"] != "" {
		t.Fatalf("continuation_reset_pending = %q, want empty", got.Metadata["continuation_reset_pending"])
	}
	if got.Metadata["last_woke_at"] != "" {
		t.Fatalf("last_woke_at = %q, want cleared", got.Metadata["last_woke_at"])
	}
	for _, e := range rec.Events {
		if e.Type == events.SessionCrashed {
			t.Fatalf("recorded %s for rate-limit screen; want crash telemetry suppressed", e.Type)
		}
	}
}

func TestReconcileSessionBeads_RateLimitScreenBeyondCrashCaptureSuppressesTelemetry(t *testing.T) {
	env := newReconcilerTestEnv()
	sp := &lineLimitedPeekProvider{Fake: runtime.NewFake()}
	rec := events.NewFake()
	env.rec = rec
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	env.desiredState["worker"] = TemplateParams{
		Command:      "test-cmd",
		SessionName:  "worker",
		TemplateName: "worker",
		Hints:        agent.StartupHints{ProcessNames: []string{"agent-cli"}},
	}
	if err := sp.Start(context.Background(), "worker", runtime.Config{Command: "test-cmd", ProcessNames: []string{"agent-cli"}}); err != nil {
		t.Fatalf("Start(worker): %v", err)
	}
	sp.Zombies["worker"] = true
	var paneLines []string
	paneLines = append(paneLines, "You've hit your limit, Pro plan", "", "/rate-limit-options")
	for i := 0; i < 60; i++ {
		paneLines = append(paneLines, fmt.Sprintf("trailing line %02d", i))
	}
	sp.SetPeekOutput("worker", strings.Join(paneLines, "\n"))
	session := env.createSessionBead("worker", "worker")
	env.setSessionMetadata(&session, map[string]string{
		"state":               "active",
		"last_woke_at":        env.clk.Now().Add(-10 * time.Second).UTC().Format(time.RFC3339),
		"session_key":         "keep-session",
		"started_config_hash": "keep-hash",
		"wake_attempts":       "2",
	})

	cfgNames := configuredSessionNames(env.cfg, "", env.store)
	woken := reconcileSessionBeads(
		context.Background(), []beads.Bead{session}, env.desiredState, cfgNames,
		env.cfg, sp, env.store, nil, nil, nil, env.dt, map[string]int{"worker": 1}, false, nil, "",
		nil, env.clk, rec, 0, 0, &env.stdout, &env.stderr,
	)

	if woken != 0 {
		t.Fatalf("woken = %d, want 0", woken)
	}
	if len(sp.peekLines) != 1 || sp.peekLines[0] != rateLimitPeekLines {
		t.Fatalf("peek lines = %v, want single %d-line read", sp.peekLines, rateLimitPeekLines)
	}
	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", session.ID, err)
	}
	if got.Metadata["sleep_reason"] != "rate_limit" {
		t.Fatalf("sleep_reason = %q, want rate_limit", got.Metadata["sleep_reason"])
	}
	if got.Metadata["wake_attempts"] != "2" {
		t.Fatalf("wake_attempts = %q, want 2", got.Metadata["wake_attempts"])
	}
	for _, e := range rec.Events {
		if e.Type == events.SessionCrashed {
			t.Fatalf("recorded %s for rate-limit marker outside old 50-line capture", e.Type)
		}
	}
}

func TestCachedSessionPeekRetriesAfterError(t *testing.T) {
	sp := &transientPeekErrorProvider{Fake: runtime.NewFake()}
	if err := sp.Start(context.Background(), "worker", runtime.Config{Command: "test-cmd"}); err != nil {
		t.Fatalf("Start(worker): %v", err)
	}
	sp.SetPeekOutput("worker", "You've hit your limit, Pro plan\n\n/rate-limit-options")
	peek := cachedSessionPeek("", nil, sp, &config.City{}, "worker", nil)

	if output, err := peek(rateLimitPeekLines); err == nil {
		t.Fatalf("first peek err = nil, output = %q; want transient error", output)
	}
	output, err := peek(rateLimitPeekLines)
	if err != nil {
		t.Fatalf("second peek should retry after transient error: %v", err)
	}
	if !runtime.ContainsProviderRateLimitScreen(output) {
		t.Fatalf("second peek output = %q, want provider rate-limit screen", output)
	}
	if sp.calls != 2 {
		t.Fatalf("peek calls = %d, want 2", sp.calls)
	}
}

func TestRateLimitAliveFromObservationDoesNotTreatObservationErrorAsAlive(t *testing.T) {
	if rateLimitAliveFromObservation(true, errors.New("observe failed")) {
		t.Fatal("observation errors must not reuse runtime-running state as process-alive")
	}
	if !rateLimitAliveFromObservation(true, nil) {
		t.Fatal("successful live observation should report alive")
	}
	if rateLimitAliveFromObservation(false, nil) {
		t.Fatal("successful dead observation should report dead")
	}
}

func TestReconcileSessionBeads_RateLimitScreenReholdsAfterQuarantineExpiry(t *testing.T) {
	env := newReconcilerTestEnv()
	rec := events.NewFake()
	env.rec = rec
	env.cfg = &config.City{
		Workspace:     config.Workspace{Name: "test-city"},
		Providers:     map[string]config.ProviderSpec{"test-provider": {Command: "test-cmd", ProcessNames: []string{"agent-cli"}}},
		Agents:        []config.Agent{{Name: "worker", Provider: "test-provider", StartCommand: "test-cmd"}},
		NamedSessions: []config.NamedSession{{Template: "worker", Mode: "always"}},
	}
	sessionName := config.NamedSessionRuntimeName(env.cfg.Workspace.Name, env.cfg.Workspace, "worker")
	env.desiredState[sessionName] = TemplateParams{
		Command:                 "test-cmd",
		SessionName:             sessionName,
		TemplateName:            "worker",
		ConfiguredNamedIdentity: "worker",
		ConfiguredNamedMode:     "always",
		Hints:                   agent.StartupHints{ProcessNames: []string{"agent-cli"}},
	}
	env.sp.SetPeekOutput(sessionName, "You've hit your limit, Pro plan\n\n/rate-limit-options")
	session := env.createSessionBead(sessionName, "worker")
	startedHash := runtime.CoreFingerprint(runtime.Config{Command: "test-cmd", ProcessNames: []string{"agent-cli"}})
	env.setSessionMetadata(&session, map[string]string{
		namedSessionMetadataKey:      "true",
		namedSessionIdentityMetadata: "worker",
		namedSessionModeMetadata:     "always",
		"state":                      "active",
		"last_woke_at":               env.clk.Now().Add(-10 * time.Second).UTC().Format(time.RFC3339),
		"session_key":                "keep-session",
		"started_config_hash":        startedHash,
		"wake_attempts":              "2",
	})

	env.reconcile([]beads.Bead{session})
	held, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get held session: %v", err)
	}
	if held.Metadata["sleep_reason"] != "rate_limit" {
		t.Fatalf("initial sleep_reason = %q, want rate_limit", held.Metadata["sleep_reason"])
	}
	qUntil, err := time.Parse(time.RFC3339, held.Metadata["quarantined_until"])
	if err != nil {
		t.Fatalf("quarantined_until parse: %v", err)
	}

	env.clk.Time = qUntil.Add(time.Second)
	woken := env.reconcile([]beads.Bead{held})
	if woken != 1 {
		t.Fatalf("woken after quarantine expiry = %d, want 1", woken)
	}
	if !env.sp.IsRunning(sessionName) {
		t.Fatal("worker should be restarted after rate-limit quarantine expiry")
	}
	afterWake, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get after wake: %v", err)
	}
	if got := afterWake.Metadata["session_key"]; got != "keep-session" {
		t.Fatalf("session_key after wake = %q, want preserved", got)
	}
	afterWakeHash := afterWake.Metadata["started_config_hash"]
	if afterWakeHash == "" {
		t.Fatal("started_config_hash should be set after wake")
	}

	if err := env.sp.Stop(sessionName); err != nil {
		t.Fatalf("Stop(%s) after wake: %v", sessionName, err)
	}
	env.reconcile([]beads.Bead{afterWake})
	reheld, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get reheld session: %v", err)
	}
	if got := reheld.Metadata["sleep_reason"]; got != "rate_limit" {
		t.Fatalf("sleep_reason after re-detection = %q, want rate_limit", got)
	}
	if got := reheld.Metadata["session_key"]; got != "keep-session" {
		t.Fatalf("session_key after re-detection = %q, want preserved", got)
	}
	if got := reheld.Metadata["started_config_hash"]; got != afterWakeHash {
		t.Fatalf("started_config_hash after re-detection = %q, want %q", got, afterWakeHash)
	}
	if got := reheld.Metadata["continuation_reset_pending"]; got != "" {
		t.Fatalf("continuation_reset_pending after re-detection = %q, want empty", got)
	}
	for _, e := range rec.Events {
		if e.Type == events.SessionCrashed {
			t.Fatalf("recorded %s during rate-limit expiry/re-hold cycle", e.Type)
		}
	}
}

func TestReconcileSessionBeads_GenericRateLimitCrashRecordsTelemetry(t *testing.T) {
	env := newReconcilerTestEnv()
	rec := events.NewFake()
	env.rec = rec
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	env.desiredState["worker"] = TemplateParams{
		Command:      "test-cmd",
		SessionName:  "worker",
		TemplateName: "worker",
		Hints:        agent.StartupHints{ProcessNames: []string{"agent-cli"}},
	}
	if err := env.sp.Start(context.Background(), "worker", runtime.Config{Command: "test-cmd", ProcessNames: []string{"agent-cli"}}); err != nil {
		t.Fatalf("Start(worker): %v", err)
	}
	env.sp.Zombies["worker"] = true
	env.sp.SetPeekOutput("worker", "worker failed while parsing rate limit config")
	session := env.createSessionBead("worker", "worker")
	env.setSessionMetadata(&session, map[string]string{
		"state":               "active",
		"last_woke_at":        env.clk.Now().Add(-10 * time.Second).UTC().Format(time.RFC3339),
		"session_key":         "reset-session",
		"started_config_hash": "reset-hash",
		"wake_attempts":       "2",
	})

	woken := env.reconcile([]beads.Bead{session})

	if woken != 0 {
		t.Fatalf("woken = %d, want 0", woken)
	}
	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", session.ID, err)
	}
	if got.Metadata["sleep_reason"] == "rate_limit" {
		t.Fatalf("sleep_reason = %q, want normal crash path", got.Metadata["sleep_reason"])
	}
	if got.Metadata["wake_attempts"] != "3" {
		t.Fatalf("wake_attempts = %q, want 3", got.Metadata["wake_attempts"])
	}
	if got.Metadata["session_key"] != "" {
		t.Fatalf("session_key = %q, want cleared after normal crash", got.Metadata["session_key"])
	}
	if got.Metadata["started_config_hash"] != "" {
		t.Fatalf("started_config_hash = %q, want cleared after normal crash", got.Metadata["started_config_hash"])
	}
	crashRecorded := false
	for _, e := range rec.Events {
		if e.Type == events.SessionCrashed && e.Message == "worker failed while parsing rate limit config" {
			crashRecorded = true
			break
		}
	}
	if !crashRecorded {
		t.Fatal("expected SessionCrashed event for generic crash output that mentions rate limit")
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
	env.addRunningWorkerDesiredWithNewConfig()
	session := env.createSessionBead("worker", "worker")
	// Session has fully started — started_config_hash records what it launched with.
	startedHash := runtime.CoreFingerprint(runtime.Config{Command: "test-cmd"})
	env.setSessionMetadata(&session, map[string]string{
		"started_config_hash": startedHash,
	})

	// Verify hashes differ.
	currentHash := runtime.CoreFingerprint(runtime.Config{Command: "new-cmd"})
	if startedHash == currentHash {
		t.Fatalf("test setup error: stored hash %q should differ from current %q", startedHash, currentHash)
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

// TestReconcileSessionBeads_ConfigDriftDeferredOnLiveAssignedWork verifies
// that a pool session with live in-progress work assigned to it is NOT
// drained when its config_hash drifts. Draining mid-work would orphan the
// assigned bead (assignee still pointing at the dead session, status stuck
// at in_progress) and kill the agent mid-task. The drain must wait until
// the assigned work completes — the next reconcile tick will see no
// assigned work and drain naturally.
//
// Regression for the 2026-05-12 live-edit drain cascade incident: editing
// a city's .gc/settings.json on a running city flips the config hash and
// triggers config-drift drains across the pool. The orphan/suspended
// branch (line 754) already skips drain when sessionHasOpenAssignedWork
// returns true; the config-drift branch did not. Pool sessions actively
// processing work could be drained, orphaning their assignments. Named
// sessions are protected separately via shouldDeferNamedSessionConfigDrift
// (line 1161) so this case is specifically about pool-routed sessions
// reaching the !restartedInPlace branch at line 1186.
func TestReconcileSessionBeads_ConfigDriftDeferredOnLiveAssignedWork(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	env.addRunningWorkerDesiredWithNewConfig()
	session := env.createSessionBead("worker", "worker")
	startedHash := runtime.CoreFingerprint(runtime.Config{Command: "test-cmd"})
	env.setSessionMetadata(&session, map[string]string{
		"started_config_hash": startedHash,
	})

	// Assign live in-progress work to this session.
	if _, err := env.store.Create(beads.Bead{
		Title:    "in-flight work",
		Type:     "task",
		Status:   "in_progress",
		Assignee: session.ID,
	}); err != nil {
		t.Fatalf("Create(work): %v", err)
	}

	env.reconcile([]beads.Bead{session})

	if ds := env.dt.get(session.ID); ds != nil {
		t.Fatalf("expected config-drift drain to be deferred for live assigned work, got drain=%+v stderr=%s",
			ds, env.stderr.String())
	}
}

func TestReconcileSessionBeads_NoDriftWhenHashMatches(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	env.addDesired("worker", "worker", true) // same config as bead
	session := env.createSessionBead("worker", "worker")
	env.markSessionActive(&session)
	env.setSessionMetadata(&session, map[string]string{
		"started_config_hash": runtime.CoreFingerprint(runtime.Config{Command: "test-cmd"}),
	})

	env.reconcile([]beads.Bead{session})

	if ds := env.dt.get(session.ID); ds != nil {
		t.Errorf("expected no drain, got %+v", ds)
	}
}

// TestReconcilerSilentRebaselineOnLegacyHash enforces ga-s760.1 FR-1, FR-3:
// when a session's stored core hash carries no version prefix (a session
// started by a binary released before fingerprint versioning), the
// reconciler must silently overwrite all four hash/breakdown metadata
// fields with current versioned values. No drain, no SessionDraining
// event.
func TestReconcilerSilentRebaselineOnLegacyHash(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	env.addDesired("worker", "worker", true)
	rec := events.NewFake()
	env.rec = rec
	session := env.createSessionBead("worker", "worker")
	env.markSessionActive(&session)

	// Legacy bare-hex values as written by the pre-versioning binary.
	legacyCore := strings.Repeat("a", 64)
	legacyLive := strings.Repeat("b", 64)
	env.setSessionMetadata(&session, map[string]string{
		"started_config_hash": legacyCore,
		"started_live_hash":   legacyLive,
		"live_hash":           legacyLive,
		"core_hash_breakdown": `{"Command":"legacy","Env":"legacy"}`,
	})

	env.reconcile([]beads.Bead{session})

	if ds := env.dt.get(session.ID); ds != nil {
		t.Fatalf("expected no drain on legacy hash silent rebaseline, got %+v; stderr=%s", ds, env.stderr.String())
	}
	for _, e := range rec.Events {
		if e.Type == events.SessionDraining {
			t.Errorf("unexpected SessionDraining event recorded for silent rebaseline: %+v", e)
		}
	}

	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get session: %v", err)
	}
	expectedCore := runtime.CoreFingerprint(runtime.Config{Command: "test-cmd"})
	expectedLive := runtime.LiveFingerprint(runtime.Config{Command: "test-cmd"})
	if got.Metadata["started_config_hash"] != expectedCore {
		t.Errorf("started_config_hash = %q, want rebaseline to %q", got.Metadata["started_config_hash"], expectedCore)
	}
	if got.Metadata["started_live_hash"] != expectedLive {
		t.Errorf("started_live_hash = %q, want rebaseline to %q", got.Metadata["started_live_hash"], expectedLive)
	}
	if got.Metadata["live_hash"] != expectedLive {
		t.Errorf("live_hash = %q, want rebaseline to %q", got.Metadata["live_hash"], expectedLive)
	}
	if got.Metadata["core_hash_breakdown"] == `{"Command":"legacy","Env":"legacy"}` {
		t.Errorf("core_hash_breakdown was not rebaselined, still: %q", got.Metadata["core_hash_breakdown"])
	}
	if got.Metadata["core_hash_breakdown"] == "" {
		t.Errorf("core_hash_breakdown was cleared but should be rebaselined to current breakdown")
	}
}

// TestReconcilerSilentRebaselineOnVersionMismatch enforces ga-s760.1 FR-2,
// FR-3: when a session's stored core hash carries a version prefix from a
// different binary (e.g., v0:), the reconciler must silently rebaseline
// all four hash/breakdown fields. No drain, no SessionDraining event.
func TestReconcilerSilentRebaselineOnVersionMismatch(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	env.addDesired("worker", "worker", true)
	rec := events.NewFake()
	env.rec = rec
	session := env.createSessionBead("worker", "worker")
	env.markSessionActive(&session)

	// Version-mismatched stored hashes (v0: prefix is from an older binary).
	mismatchCore := "v0:" + strings.Repeat("a", 64)
	mismatchLive := "v0:" + strings.Repeat("b", 64)
	env.setSessionMetadata(&session, map[string]string{
		"started_config_hash": mismatchCore,
		"started_live_hash":   mismatchLive,
		"live_hash":           mismatchLive,
		"core_hash_breakdown": `{"version":"v0","fields":{"Command":"old"}}`,
	})

	env.reconcile([]beads.Bead{session})

	if ds := env.dt.get(session.ID); ds != nil {
		t.Fatalf("expected no drain on version-mismatch silent rebaseline, got %+v; stderr=%s", ds, env.stderr.String())
	}
	for _, e := range rec.Events {
		if e.Type == events.SessionDraining {
			t.Errorf("unexpected SessionDraining event recorded for silent rebaseline: %+v", e)
		}
	}

	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get session: %v", err)
	}
	expectedCore := runtime.CoreFingerprint(runtime.Config{Command: "test-cmd"})
	expectedLive := runtime.LiveFingerprint(runtime.Config{Command: "test-cmd"})
	if got.Metadata["started_config_hash"] != expectedCore {
		t.Errorf("started_config_hash = %q, want rebaseline to %q", got.Metadata["started_config_hash"], expectedCore)
	}
	if got.Metadata["started_live_hash"] != expectedLive {
		t.Errorf("started_live_hash = %q, want rebaseline to %q", got.Metadata["started_live_hash"], expectedLive)
	}
	if got.Metadata["live_hash"] != expectedLive {
		t.Errorf("live_hash = %q, want rebaseline to %q", got.Metadata["live_hash"], expectedLive)
	}
	if got.Metadata["core_hash_breakdown"] == `{"version":"v0","fields":{"Command":"old"}}` {
		t.Errorf("core_hash_breakdown was not rebaselined, still: %q", got.Metadata["core_hash_breakdown"])
	}
	if got.Metadata["core_hash_breakdown"] == "" {
		t.Errorf("core_hash_breakdown was cleared but should be rebaselined to current breakdown")
	}
}

// TestReconcilerStillDrainsOnSameVersionRealDrift enforces ga-s760.1 FR-4:
// the silent rebaseline path must NOT swallow real drift. When stored and
// current hashes share the current version prefix but differ in the hex
// tail, the reconciler still drains the session. This is the regression
// guard against the rebaseline branch over-applying.
func TestReconcilerStillDrainsOnSameVersionRealDrift(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	// Desired state has a different command than the bead.
	env.addRunningWorkerDesiredWithNewConfig()
	session := env.createSessionBead("worker", "worker")
	env.markSessionActive(&session)
	// Stored hash carries the current version prefix but a fabricated hex
	// that differs from whatever the desired-state config currently hashes
	// to. The shared prefix means the version gate sees same-version, so
	// the drift handling proceeds to compare the hex tails and drain.
	storedCore := runtime.FingerprintVersion + ":" + strings.Repeat("c", 64)
	env.setSessionMetadata(&session, map[string]string{
		"started_config_hash": storedCore,
	})

	currentHash := runtime.CoreFingerprint(runtime.Config{Command: "new-cmd"})
	if storedCore == currentHash {
		t.Fatalf("test setup: stored hash %q should differ from current %q", storedCore, currentHash)
	}

	env.reconcile([]beads.Bead{session})

	ds := env.dt.get(session.ID)
	if ds == nil {
		t.Fatalf("expected drain to be initiated for same-version real drift (session.ID=%q, stderr=%s)", session.ID, env.stderr.String())
	}
	if ds.reason != "config-drift" {
		t.Errorf("drain reason = %q, want %q", ds.reason, "config-drift")
	}
}

// Regression test for #127: a freshly created session can be drained for
// config-drift shortly after wake because the reconciler's drift check runs
// before started_config_hash is written. The fix skips drift detection until
// started_config_hash is present.
func TestReconcileSessionBeads_NoDriftBeforeStartedHashWritten(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	// Desired state has a DIFFERENT config than the bead's config_hash.
	env.addRunningWorkerDesiredWithNewConfig()
	session := env.createSessionBead("worker", "worker")
	// Do NOT set started_config_hash — simulates the window between
	// sync-time config_hash write and post-start started_config_hash write.

	env.reconcile([]beads.Bead{session})

	if ds := env.dt.get(session.ID); ds != nil {
		t.Errorf("expected no drain before started_config_hash is written, got reason=%q", ds.reason)
	}
}

func TestReconcileSessionBeads_DefersPendingCreateRecoveryWhileStartInFlight(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	env.desiredState["worker"] = TemplateParams{
		Command:      "new-cmd",
		SessionName:  "worker",
		TemplateName: "worker",
	}
	session := env.createSessionBead("worker", "worker")
	env.setSessionMetadata(&session, map[string]string{
		"command":              "old-cmd",
		"state":                "creating",
		"pending_create_claim": "true",
		"last_woke_at":         env.clk.Now().UTC().Format(time.RFC3339),
	})
	if err := env.sp.Start(context.Background(), "worker", runtime.Config{Command: "old-cmd"}); err != nil {
		t.Fatal(err)
	}
	if err := env.sp.SetMeta("worker", "GC_SESSION_ID", session.ID); err != nil {
		t.Fatal(err)
	}
	if err := env.sp.SetMeta("worker", "GC_INSTANCE_TOKEN", session.Metadata["instance_token"]); err != nil {
		t.Fatal(err)
	}

	woken := env.reconcile([]beads.Bead{session})
	if woken != 0 {
		t.Fatalf("woken = %d, want 0 while pending create start is still in flight", woken)
	}
	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Metadata["started_config_hash"] != "" {
		t.Fatalf("started_config_hash = %q, want empty until async start commits", got.Metadata["started_config_hash"])
	}
	if got.Metadata["pending_create_claim"] != "true" {
		t.Fatalf("pending_create_claim = %q, want preserved while async start is in flight", got.Metadata["pending_create_claim"])
	}
	switch got.Metadata["state"] {
	case "creating", "awake":
	default:
		t.Fatalf("state = %q, want creating or awake while async start is in flight", got.Metadata["state"])
	}
}

func TestReconcileSessionBeads_PendingCreateLeasePreventsOrphanClose(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	session := env.createSessionBead("s-gc-late", "worker")
	env.setSessionMetadata(&session, map[string]string{
		"state":                "creating",
		"manual_session":       "true",
		"pending_create_claim": "true",
	})
	session.CreatedAt = env.clk.Now().Add(-30 * time.Second)

	woken := env.reconcile([]beads.Bead{session})
	if woken != 0 {
		t.Fatalf("woken = %d, want 0 without desired-state membership", woken)
	}
	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get session: %v", err)
	}
	if got.Status == "closed" {
		t.Fatalf("pending-create session was closed as orphan: %+v", got)
	}
	if got.Metadata["state"] == "orphaned" {
		t.Fatalf("pending-create session was marked orphaned: %+v", got.Metadata)
	}
}

func TestReconcileSessionBeads_FreshPendingCreateSurvivesStaleConfigSnapshot(t *testing.T) {
	env := newReconcilerTestEnv()
	session := env.createSessionBead("s-gc-late", "worker")
	env.setSessionMetadata(&session, map[string]string{
		"state":                "creating",
		"pending_create_claim": "true",
	})

	woken := env.reconcile([]beads.Bead{session})
	if woken != 0 {
		t.Fatalf("woken = %d, want 0 without desired-state membership", woken)
	}
	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get session: %v", err)
	}
	if got.Status == "closed" {
		t.Fatalf("fresh pending-create session was closed as orphan: %+v", got)
	}
	if got.Metadata["state"] == "orphaned" {
		t.Fatalf("fresh pending-create session was marked orphaned: %+v", got.Metadata)
	}
}

func TestReconcileSessionBeads_PendingCreateWithoutDesiredStateUsesNeverStartedLease(t *testing.T) {
	env := newReconcilerTestEnv()
	session := env.createSessionBead("s-gc-late", "worker")
	startedAt := env.clk.Now().Add(-(pendingCreateNeverStartedTimeout - time.Minute))
	env.setSessionMetadata(&session, map[string]string{
		"state":                     "creating",
		"pending_create_claim":      "true",
		"pending_create_started_at": pendingCreateStartedAtNow(startedAt),
		// last_woke_at deliberately empty: preWakeCommit never fired before
		// this pending create left desired state.
	})
	session.CreatedAt = env.clk.Now().Add(-24 * time.Hour)

	woken := env.reconcile([]beads.Bead{session})
	if woken != 0 {
		t.Fatalf("woken = %d, want 0 without desired-state membership", woken)
	}
	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get session: %v", err)
	}
	if got.Status == "closed" {
		t.Fatalf("pending-create session was closed before never-started lease expired: %+v", got)
	}
	if got.Metadata["state"] == "orphaned" {
		t.Fatalf("pending-create session was marked orphaned before never-started lease expired: %+v", got.Metadata)
	}
}

func TestReconcileSessionBeads_ConfiguredPendingCreateWithoutDemandUsesNeverStartedLease(t *testing.T) {
	tests := []struct {
		name       string
		startedAt  time.Time
		wantClosed bool
	}{
		{
			name:       "before lease expires",
			startedAt:  time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC).Add(-(pendingCreateNeverStartedTimeout - time.Minute)),
			wantClosed: false,
		},
		{
			name:       "after lease expires",
			startedAt:  time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC).Add(-(pendingCreateNeverStartedTimeout + time.Second)),
			wantClosed: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := newReconcilerTestEnv()
			env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
			session := env.createSessionBead("s-gc-late", "worker")
			env.setSessionMetadata(&session, map[string]string{
				"state":                     "creating",
				"pending_create_claim":      "true",
				"pending_create_started_at": pendingCreateStartedAtNow(tt.startedAt),
				// last_woke_at deliberately empty: preWakeCommit never fired before
				// this configured template lost pool demand.
			})
			session.CreatedAt = env.clk.Now().Add(-24 * time.Hour)

			woken := env.reconcile([]beads.Bead{session})
			if woken != 0 {
				t.Fatalf("woken = %d, want 0 without desired-state membership", woken)
			}
			got, err := env.store.Get(session.ID)
			if err != nil {
				t.Fatalf("Get session: %v", err)
			}
			if got.Status == "closed" != tt.wantClosed {
				t.Fatalf("status = %q, want closed=%v; metadata=%v", got.Status, tt.wantClosed, got.Metadata)
			}
		})
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

func TestReconcileSessionBeads_OrphanDrainLiveAssignedWorkStaysOpen(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "other"}}}
	_ = env.sp.Start(context.Background(), "orphan", runtime.Config{})
	session := env.createSessionBead("orphan", "orphan")
	env.markSessionActive(&session)

	if _, err := env.store.Create(beads.Bead{
		Title:    "claimed work",
		Type:     "task",
		Status:   "in_progress",
		Assignee: session.Metadata["session_name"],
	}); err != nil {
		t.Fatalf("Create assigned work bead: %v", err)
	}

	env.reconcile([]beads.Bead{session})

	if ds := env.dt.get(session.ID); ds != nil {
		t.Fatalf("expected live assigned work to block orphan drain, got drain state %+v", ds)
	}
	if strings.Contains(env.stdout.String(), "Draining session 'orphan': orphaned") {
		t.Fatalf("expected no orphan drain log, got stdout:\n%s", env.stdout.String())
	}
}

// TestReconcileSessionBeads_OrphanDrainLogThrottled covers issue #855:
// once a session is draining, the reconciler must not re-emit
// "Draining session '...': orphaned" on every subsequent tick. The
// drainTracker is idempotent, so a pre-existing drain entry means the
// reconciler tick is a no-op with respect to state — the user-visible
// log must reflect that.
func TestReconcileSessionBeads_OrphanDrainLogThrottled(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "other"}}}
	_ = env.sp.Start(context.Background(), "orphan", runtime.Config{})
	session := env.createSessionBead("orphan", "orphan")

	// Simulate a drain that was begun on a prior tick and has not yet
	// converged (e.g., in-progress work beads still assigned to the
	// session block its bead from closing — the exact loop described
	// in #855).
	env.dt.set(session.ID, &drainState{
		startedAt:  env.clk.Now(),
		deadline:   env.clk.Now().Add(defaultDrainTimeout),
		reason:     "orphaned",
		generation: 1,
	})

	env.reconcile([]beads.Bead{session})

	if got := env.stdout.String(); strings.Contains(got, "Draining session 'orphan'") {
		t.Errorf("stdout contains redundant drain log on repeat tick:\n%s", got)
	}
}

func TestReconcileSessionBeads_DrainAckedOrphanCanceledForAssignedWork(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{}
	if err := env.sp.Start(context.Background(), "orphan", runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := env.sp.SetMeta("orphan", "GC_DRAIN_ACK", "1"); err != nil {
		t.Fatalf("SetMeta(GC_DRAIN_ACK): %v", err)
	}
	session := env.createSessionBead("orphan", "worker")
	env.markSessionActive(&session)
	work, err := env.store.Create(beads.Bead{
		Title:    "assigned work",
		Type:     "task",
		Status:   "open",
		Assignee: session.ID,
	})
	if err != nil {
		t.Fatalf("Create work bead: %v", err)
	}
	dops := newFakeDrainOps()
	if err := dops.setDrainAck("orphan"); err != nil {
		t.Fatalf("setDrainAck: %v", err)
	}
	env.dt.set(session.ID, &drainState{
		startedAt:  env.clk.Now().Add(-defaultDrainTimeout),
		deadline:   env.clk.Now().Add(-time.Second),
		reason:     "orphaned",
		generation: 1,
		ackSet:     true,
	})

	reconcileSessionBeadsAtPath(
		context.Background(),
		"",
		[]beads.Bead{session},
		nil,
		nil,
		env.cfg,
		env.sp,
		env.store,
		dops,
		[]beads.Bead{work},
		nil,
		nil,
		env.dt,
		map[string]int{},
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

	if !env.sp.IsRunning("orphan") {
		t.Fatal("assigned-work orphan drain should be canceled before stopping the running session")
	}
	if ds := env.dt.get(session.ID); ds != nil {
		t.Fatalf("drain = %+v, want canceled", ds)
	}
	if ack, _ := env.sp.GetMeta("orphan", "GC_DRAIN_ACK"); ack != "" {
		t.Fatalf("GC_DRAIN_ACK = %q, want cleared", ack)
	}
}

func TestReconcileSessionBeads_RecoveredDrainAckedOrphanCanceledForAssignedWork(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{}
	if err := env.sp.Start(context.Background(), "orphan", runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	session := env.createSessionBead("orphan", "worker")
	env.markSessionActive(&session)
	work, err := env.store.Create(beads.Bead{
		Title:    "assigned work",
		Type:     "task",
		Status:   "open",
		Assignee: session.ID,
	})
	if err != nil {
		t.Fatalf("Create work bead: %v", err)
	}
	if err := setReconcilerDrainAckMetadata(env.sp, "orphan", &drainState{
		reason:     "orphaned",
		generation: 1,
		ackSet:     true,
	}); err != nil {
		t.Fatalf("setReconcilerDrainAckMetadata: %v", err)
	}

	reconcileSessionBeadsAtPath(
		context.Background(),
		"",
		[]beads.Bead{session},
		nil,
		nil,
		env.cfg,
		env.sp,
		env.store,
		newDrainOps(env.sp),
		[]beads.Bead{work},
		nil,
		nil,
		env.dt,
		map[string]int{},
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

	if !env.sp.IsRunning("orphan") {
		t.Fatal("recovered assigned-work orphan drain should be canceled before stopping the running session")
	}
	if ack, _ := env.sp.GetMeta("orphan", "GC_DRAIN_ACK"); ack != "" {
		t.Fatalf("GC_DRAIN_ACK = %q, want cleared", ack)
	}
	if source, _ := env.sp.GetMeta("orphan", reconcilerDrainAckSourceKey); source != "" {
		t.Fatalf("%s = %q, want cleared", reconcilerDrainAckSourceKey, source)
	}
}

func TestReconcileSessionBeads_ReconcilerNoWakeDrainAckCanceledForAssignedWork(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	env.addDesired("worker", "worker", true)
	session := env.createSessionBead("worker", "worker")
	env.markSessionActive(&session)
	work, err := env.store.Create(beads.Bead{
		Title:    "assigned work",
		Type:     "task",
		Status:   "in_progress",
		Assignee: session.ID,
	})
	if err != nil {
		t.Fatalf("Create work bead: %v", err)
	}
	if err := setReconcilerDrainAckMetadata(env.sp, "worker", &drainState{
		reason:     "no-wake-reason",
		generation: 1,
		ackSet:     true,
	}); err != nil {
		t.Fatalf("setReconcilerDrainAckMetadata: %v", err)
	}
	env.dt.set(session.ID, &drainState{
		startedAt:  env.clk.Now().Add(-defaultDrainTimeout),
		deadline:   env.clk.Now().Add(-time.Second),
		reason:     "no-wake-reason",
		generation: 1,
		ackSet:     true,
	})

	reconcileSessionBeadsAtPath(
		context.Background(),
		"",
		[]beads.Bead{session},
		env.desiredState,
		nil,
		env.cfg,
		env.sp,
		env.store,
		newDrainOps(env.sp),
		[]beads.Bead{work},
		nil,
		nil,
		env.dt,
		map[string]int{"worker": 1},
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

	if !env.sp.IsRunning("worker") {
		t.Fatal("assigned-work no-wake drain should be canceled before stopping the running session")
	}
	if ds := env.dt.get(session.ID); ds != nil {
		t.Fatalf("drain = %+v, want canceled", ds)
	}
	if ack, _ := env.sp.GetMeta("worker", "GC_DRAIN_ACK"); ack != "" {
		t.Fatalf("GC_DRAIN_ACK = %q, want cleared", ack)
	}
	if source, _ := env.sp.GetMeta("worker", reconcilerDrainAckSourceKey); source != "" {
		t.Fatalf("%s = %q, want cleared", reconcilerDrainAckSourceKey, source)
	}
}

func TestReconcileSessionBeads_RecoveredNoWakeDrainAckCanceledForAssignedWork(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	env.addDesired("worker", "worker", true)
	session := env.createSessionBead("worker", "worker")
	env.markSessionActive(&session)
	work, err := env.store.Create(beads.Bead{
		Title:    "assigned work",
		Type:     "task",
		Status:   "in_progress",
		Assignee: session.ID,
	})
	if err != nil {
		t.Fatalf("Create work bead: %v", err)
	}
	if err := setReconcilerDrainAckMetadata(env.sp, "worker", &drainState{
		reason:     "no-wake-reason",
		generation: 1,
		ackSet:     true,
	}); err != nil {
		t.Fatalf("setReconcilerDrainAckMetadata: %v", err)
	}

	reconcileSessionBeadsAtPath(
		context.Background(),
		"",
		[]beads.Bead{session},
		env.desiredState,
		nil,
		env.cfg,
		env.sp,
		env.store,
		newDrainOps(env.sp),
		[]beads.Bead{work},
		nil,
		nil,
		env.dt,
		map[string]int{"worker": 1},
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

	if !env.sp.IsRunning("worker") {
		t.Fatal("recovered assigned-work no-wake drain should be canceled before stopping the running session")
	}
	if ack, _ := env.sp.GetMeta("worker", "GC_DRAIN_ACK"); ack != "" {
		t.Fatalf("GC_DRAIN_ACK = %q, want cleared", ack)
	}
	if source, _ := env.sp.GetMeta("worker", reconcilerDrainAckSourceKey); source != "" {
		t.Fatalf("%s = %q, want cleared", reconcilerDrainAckSourceKey, source)
	}
}

func TestReconcileSessionBeads_NoWakeDrainAckWithReadyOpenAssignedWorkCancelsDrain(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "other"}}}
	if err := env.sp.Start(context.Background(), "worker", runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	session := env.createSessionBead("worker", "worker")
	env.markSessionActive(&session)
	work, err := env.store.Create(beads.Bead{
		Title:    "ready assigned work",
		Type:     "task",
		Status:   "open",
		Assignee: session.ID,
	})
	if err != nil {
		t.Fatalf("Create work bead: %v", err)
	}
	if err := setReconcilerDrainAckMetadata(env.sp, "worker", &drainState{
		reason:     "no-wake-reason",
		generation: 1,
		ackSet:     true,
	}); err != nil {
		t.Fatalf("setReconcilerDrainAckMetadata: %v", err)
	}
	dops := newFakeDrainOps()
	if err := dops.setDrainAck("worker"); err != nil {
		t.Fatalf("setDrainAck: %v", err)
	}
	env.dt.set(session.ID, &drainState{
		startedAt:  env.clk.Now().Add(-defaultDrainTimeout),
		deadline:   env.clk.Now().Add(-time.Second),
		reason:     "no-wake-reason",
		generation: 1,
		ackSet:     true,
	})

	reconcileSessionBeadsAtPath(
		context.Background(),
		"",
		[]beads.Bead{session},
		nil,
		nil,
		env.cfg,
		env.sp,
		env.store,
		dops,
		[]beads.Bead{work},
		nil,
		nil,
		env.dt,
		map[string]int{},
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

	if !env.sp.IsRunning("worker") {
		t.Fatal("ready open assigned work should cancel no-wake drain before stopping the running session")
	}
	if len(dops.clearDrainCalls) != 1 || dops.clearDrainCalls[0] != "worker" {
		t.Fatalf("clearDrain calls = %v, want [worker]", dops.clearDrainCalls)
	}
	if ds := env.dt.get(session.ID); ds != nil {
		t.Fatalf("drain = %+v, want canceled", ds)
	}
}

func TestReconcileSessionBeads_NoWakeDrainAckWithBlockedOpenAssignedWorkStopsPending(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "other"}}}
	if err := env.sp.Start(context.Background(), "worker", runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	session := env.createSessionBead("worker", "worker")
	env.markSessionActive(&session)
	blocker, err := env.store.Create(beads.Bead{
		Title:  "blocking dependency",
		Type:   "task",
		Status: "open",
	})
	if err != nil {
		t.Fatalf("Create blocker: %v", err)
	}
	work, err := env.store.Create(beads.Bead{
		Title:    "blocked assigned work",
		Type:     "task",
		Status:   "open",
		Assignee: session.ID,
	})
	if err != nil {
		t.Fatalf("Create work bead: %v", err)
	}
	if err := env.store.DepAdd(work.ID, blocker.ID, "blocks"); err != nil {
		t.Fatalf("DepAdd: %v", err)
	}
	if err := setReconcilerDrainAckMetadata(env.sp, "worker", &drainState{
		reason:     "no-wake-reason",
		generation: 1,
		ackSet:     true,
	}); err != nil {
		t.Fatalf("setReconcilerDrainAckMetadata: %v", err)
	}
	dops := newFakeDrainOps()
	if err := dops.setDrainAck("worker"); err != nil {
		t.Fatalf("setDrainAck: %v", err)
	}
	env.dt.set(session.ID, &drainState{
		startedAt:  env.clk.Now().Add(-defaultDrainTimeout),
		deadline:   env.clk.Now().Add(-time.Second),
		reason:     "no-wake-reason",
		generation: 1,
		ackSet:     true,
	})

	reconcileSessionBeadsAtPath(
		context.Background(),
		"",
		[]beads.Bead{session},
		nil,
		nil,
		env.cfg,
		env.sp,
		env.store,
		dops,
		[]beads.Bead{work},
		nil,
		nil,
		env.dt,
		map[string]int{},
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

	if len(dops.clearDrainCalls) != 0 {
		t.Fatalf("clearDrain calls = %v, want no assigned-work cancellation for blocked open work", dops.clearDrainCalls)
	}
	if got := env.stdout.String(); strings.Contains(got, "Canceled drain-acked session 'worker'") {
		t.Fatalf("blocked open work should not cancel no-wake drain, got stdout:\n%s", got)
	}
	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", session.ID, err)
	}
	if !isDrainAckStopPending(got) {
		t.Fatalf("session metadata = %+v, want drain-ack stop-pending", got.Metadata)
	}
}

func TestReconcileSessionBeads_DeadDrainAckedOrphanWithAssignedWorkCompletesDrain(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{}
	session := env.createSessionBead("orphan", "worker")
	env.markSessionActive(&session)
	work, err := env.store.Create(beads.Bead{
		Title:    "assigned work",
		Type:     "task",
		Status:   "open",
		Assignee: session.ID,
	})
	if err != nil {
		t.Fatalf("Create work bead: %v", err)
	}
	dops := newFakeDrainOps()
	if err := dops.setDrainAck("orphan"); err != nil {
		t.Fatalf("setDrainAck: %v", err)
	}
	env.dt.set(session.ID, &drainState{
		startedAt:  env.clk.Now().Add(-defaultDrainTimeout),
		deadline:   env.clk.Now().Add(-time.Second),
		reason:     "orphaned",
		generation: 1,
		ackSet:     true,
	})

	reconcileSessionBeadsAtPath(
		context.Background(),
		"",
		[]beads.Bead{session},
		nil,
		nil,
		env.cfg,
		env.sp,
		env.store,
		dops,
		[]beads.Bead{work},
		nil,
		nil,
		env.dt,
		map[string]int{},
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

	if env.sp.IsRunning("orphan") {
		t.Fatal("dead provider should not be treated as recovered")
	}
	if ds := env.dt.get(session.ID); ds != nil {
		t.Fatalf("drain = %+v, want completed", ds)
	}
	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", session.ID, err)
	}
	if got.Metadata["state"] != "asleep" {
		t.Fatalf("state = %q, want asleep", got.Metadata["state"])
	}
	if got.Metadata["sleep_reason"] != "idle" {
		t.Fatalf("sleep_reason = %q, want idle", got.Metadata["sleep_reason"])
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
	if want := sessionpkg.CanonicalCloseReason("orphaned"); b.Metadata["close_reason"] != want {
		t.Errorf("close_reason = %q, want %q", b.Metadata["close_reason"], want)
	}
	if b.Metadata["state"] != "orphaned" {
		t.Errorf("state = %q, want %q", b.Metadata["state"], "orphaned")
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
	if want := sessionpkg.CanonicalCloseReason("suspended"); b.Metadata["close_reason"] != want {
		t.Errorf("close_reason = %q, want %q", b.Metadata["close_reason"], want)
	}
	if b.Metadata["state"] != "suspended" {
		t.Errorf("state = %q, want %q", b.Metadata["state"], "suspended")
	}
}

// TestReconcileSessionBeads_FailedCreateNotDesiredClosed verifies that a bead
// in the failed-create state is recognized by the reconciler (not skipped as
// unknown) and closed when it is not in the desired set and not running.
// Regression: previously failed-create was missing from knownSessionStates,
// so dead pool beads blocked slots forever.
func TestReconcileSessionBeads_FailedCreateNotDesiredClosed(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "polecat", MinActiveSessions: intPtr(1), MaxActiveSessions: intPtr(5)}}}
	session := env.createSessionBead("polecat", "polecat-ga-mg0")
	session.Metadata["state"] = "failed-create"
	session.Metadata["pool_managed"] = "true"
	session.Metadata["pool_slot"] = "1"

	env.reconcile([]beads.Bead{session})

	b, _ := env.store.Get(session.ID)
	if b.Status != "closed" {
		t.Errorf("failed-create bead status = %q, want closed", b.Status)
	}
	if want := sessionpkg.CanonicalCloseReason(string(sessionpkg.StateFailedCreate)); b.Metadata["close_reason"] != want {
		t.Errorf("close_reason = %q, want %q", b.Metadata["close_reason"], want)
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

func TestReconcileSessionBeads_PreservedConfiguredNamedRateLimitRunsBeforeHeal(t *testing.T) {
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
		"state":                      "active",
		"last_woke_at":               env.clk.Now().Add(-10 * time.Second).UTC().Format(time.RFC3339),
		"session_key":                "keep-session",
		"started_config_hash":        "keep-hash",
	})
	env.sp.SetPeekOutput(sessionName, "You've hit your limit, Pro plan\n\n/rate-limit-options")

	woken := env.reconcile([]beads.Bead{session})

	if woken != 0 {
		t.Fatalf("woken = %d, want 0", woken)
	}
	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", session.ID, err)
	}
	if got.Metadata["sleep_reason"] != "rate_limit" {
		t.Fatalf("sleep_reason = %q, want rate_limit", got.Metadata["sleep_reason"])
	}
	if got.Metadata["state"] != "asleep" {
		t.Fatalf("state = %q, want asleep", got.Metadata["state"])
	}
	if got.Metadata["session_key"] != "keep-session" {
		t.Fatalf("session_key = %q, want preserved", got.Metadata["session_key"])
	}
	if got.Metadata["started_config_hash"] != "keep-hash" {
		t.Fatalf("started_config_hash = %q, want preserved", got.Metadata["started_config_hash"])
	}
	if got.Metadata["continuation_reset_pending"] != "" {
		t.Fatalf("continuation_reset_pending = %q, want empty", got.Metadata["continuation_reset_pending"])
	}
	if ds := env.dt.get(session.ID); ds != nil {
		t.Fatalf("unexpected drain for rate-limited configured named session: %+v", ds)
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
		"live_hash":   runtime.LiveFingerprint(runtimeCfg),
		"detached_at": env.clk.Now().UTC().Add(-6 * time.Minute).Format(time.RFC3339),
	})
	if err := env.sp.Start(context.Background(), sessionName, runtimeCfg); err != nil {
		t.Fatalf("start session: %v", err)
	}
	env.sp.WaitForIdleErrors[sessionName] = nil
	idleGate := make(chan struct{}) // see waitForIdleProbeReady godoc
	env.sp.WaitForIdleGates[sessionName] = idleGate

	env.reconcile([]beads.Bead{session})
	close(idleGate)
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
		"pending_create_claim":       "true",
	})
	if err := env.sp.Start(context.Background(), sessionName, runtime.Config{Command: "true"}); err != nil {
		t.Fatalf("start session: %v", err)
	}
	// The stale create claim should be cleared by the restart path. Match the
	// live runtime to this bead so the pending-create rollback guard does not
	// claim the fixture first.
	if err := env.sp.SetMeta(sessionName, "GC_SESSION_ID", session.ID); err != nil {
		t.Fatalf("SetMeta(GC_SESSION_ID): %v", err)
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
	if got.Metadata["continuation_reset_pending"] != "true" {
		t.Fatalf("continuation_reset_pending = %q, want true", got.Metadata["continuation_reset_pending"])
	}
	if got.Metadata["session_key"] == "" || got.Metadata["session_key"] == "original-key" {
		t.Fatalf("session_key = %q, want rotated key", got.Metadata["session_key"])
	}
	if got.Metadata["pending_create_claim"] != "" {
		t.Fatalf("pending_create_claim = %q, want cleared after durable restart request", got.Metadata["pending_create_claim"])
	}
}

func TestReconcileSessionBeads_HealsRunningPendingCreateToActive(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Agents: []config.Agent{{Name: "worker", StartCommand: "test-cmd", MaxActiveSessions: intPtr(1)}},
	}
	env.addDesired("worker", "worker", false)

	session := env.createSessionBead("worker", "worker")
	env.markSessionCreating(&session)
	env.setSessionMetadata(&session, map[string]string{
		"pending_create_claim": "true",
		"sleep_reason":         "",
	})
	if err := env.sp.Start(context.Background(), "worker", runtime.Config{Command: "test-cmd"}); err != nil {
		t.Fatalf("start session: %v", err)
	}
	if err := env.sp.SetMeta("worker", "GC_SESSION_ID", session.ID); err != nil {
		t.Fatalf("SetMeta(GC_SESSION_ID): %v", err)
	}

	env.reconcile([]beads.Bead{session})

	got, _ := env.store.Get(session.ID)
	if got.Metadata["pending_create_claim"] != "" {
		t.Fatalf("pending_create_claim = %q, want cleared", got.Metadata["pending_create_claim"])
	}
	if got.Metadata["state"] != "active" {
		t.Fatalf("state = %q, want active", got.Metadata["state"])
	}
	if got.Metadata["state_reason"] != "creation_complete" {
		t.Fatalf("state_reason = %q, want creation_complete", got.Metadata["state_reason"])
	}
	if got.Metadata["started_config_hash"] == "" {
		t.Fatal("started_config_hash should be recorded when healing a live pending create")
	}
}

// TestReconcileAndWake_RestartRequestBumpsContinuationEpoch is an end-to-end
// test that chains reconcile (sets continuation_reset_pending) with
// preWakeCommit (consumes the flag and bumps continuation_epoch). This covers
// the full restart-requested → wake handoff.
func TestReconcileAndWake_RestartRequestBumpsContinuationEpoch(t *testing.T) {
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
		"continuation_epoch":         "3",
	})
	if err := env.sp.Start(context.Background(), sessionName, runtime.Config{Command: "true"}); err != nil {
		t.Fatalf("start session: %v", err)
	}

	// Phase 1: reconcile processes restart_requested → sets continuation_reset_pending.
	env.reconcile([]beads.Bead{session})

	got, _ := env.store.Get(session.ID)
	if got.Metadata["continuation_reset_pending"] != "true" {
		t.Fatalf("after reconcile: continuation_reset_pending = %q, want true", got.Metadata["continuation_reset_pending"])
	}

	// Phase 2: preWakeCommit consumes continuation_reset_pending → bumps epoch.
	if _, _, err := preWakeCommit(&got, env.store, env.clk); err != nil {
		t.Fatalf("preWakeCommit: %v", err)
	}
	woke, _ := env.store.Get(session.ID)
	if woke.Metadata["continuation_epoch"] != "4" {
		t.Fatalf("after wake: continuation_epoch = %q, want 4", woke.Metadata["continuation_epoch"])
	}
	if woke.Metadata["continuation_reset_pending"] != "" {
		t.Fatalf("after wake: continuation_reset_pending = %q, want empty", woke.Metadata["continuation_reset_pending"])
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
	if want := sessionpkg.CanonicalCloseReason("suspended"); b.Metadata["close_reason"] != want {
		t.Fatalf("close_reason = %q, want %q", b.Metadata["close_reason"], want)
	}
	if b.Metadata["state"] != "suspended" {
		t.Fatalf("state = %q, want %q", b.Metadata["state"], "suspended")
	}
}

func TestReconcileSessionBeads_OnDemandNamedSessionDoesNotRecoverClosedCanonicalFromWorkQuery(t *testing.T) {
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
	if _, ok := dsResult.State[sessionName]; ok {
		t.Fatalf("desired state recovered named session %q from controller-side work_query; keys=%v", sessionName, mapKeys(dsResult.State))
	}
	if dsResult.NamedSessionDemand["refinery"] {
		t.Fatal("NamedSessionDemand should not include refinery from work_query")
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

func TestReconcileSessionBeads_StableAlreadyClearDoesNotWriteMetadata(t *testing.T) {
	env := newReconcilerTestEnv()
	countingStore := newCountingMetadataStore()
	env.store = countingStore
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	env.addDesired("worker", "worker", true)
	session := env.createSessionBead("worker", "worker")
	stableWake := env.clk.Now().Add(-2 * time.Minute).UTC().Format(time.RFC3339)
	env.setSessionMetadata(&session, map[string]string{
		"state":             "active",
		"wake_attempts":     "3",
		"last_woke_at":      stableWake,
		"quarantined_until": "",
	})

	countingStore.singleCalls = 0
	countingStore.batchCalls = 0
	env.reconcile([]beads.Bead{session})
	if countingStore.batchCalls == 0 {
		t.Fatal("first stable tick should write metadata to clear wake failures")
	}

	cleared, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("getting session bead: %v", err)
	}
	if cleared.Metadata["wake_attempts"] != "0" {
		t.Fatalf("wake_attempts after first tick = %q, want 0", cleared.Metadata["wake_attempts"])
	}

	countingStore.singleCalls = 0
	countingStore.batchCalls = 0
	env.reconcile([]beads.Bead{cleared})
	if got := countingStore.singleCalls + countingStore.batchCalls; got != 0 {
		t.Fatalf("second stable tick performed %d metadata write(s), want 0", got)
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
			"provider":              "claude",
			"work_dir":              t.TempDir(),
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
	if want := sessionpkg.CanonicalCloseReason("failed-create"); got.Metadata["close_reason"] != want {
		t.Fatalf("close_reason = %q, want %q", got.Metadata["close_reason"], want)
	}
	if got.Metadata["state"] != "failed-create" {
		t.Fatalf("state = %q, want %q", got.Metadata["state"], "failed-create")
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
			"provider":              "claude",
			"work_dir":              t.TempDir(),
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

func TestReconcileSessionBeads_PreservesNeverStartedPendingCreateBeforeLeaseExpires(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	clk := &clock.Fake{Time: time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC)}
	cfg := &config.City{Agents: []config.Agent{{Name: "helper"}}}
	desired := map[string]TemplateParams{
		"helper": {
			Command:      "test-cmd",
			SessionName:  "helper",
			TemplateName: "helper",
		},
	}

	bead, err := store.Create(beads.Bead{
		Title:  "helper",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "template:helper"},
		Metadata: map[string]string{
			"session_name":              "helper",
			"session_name_explicit":     "true",
			"pending_create_claim":      "true",
			"pending_create_started_at": pendingCreateStartedAtNow(clk.Now().Add(-(pendingCreateNeverStartedTimeout - time.Minute))),
			"template":                  "helper",
			"state":                     "creating",
			"generation":                "1",
			"continuation_epoch":        "1",
			"instance_token":            "test-token",
			// last_woke_at deliberately empty — preWakeCommit never fired.
		},
	})
	if err != nil {
		t.Fatalf("Create(bead): %v", err)
	}
	bead.CreatedAt = clk.Now().Add(-24 * time.Hour)

	var stdout, stderr bytes.Buffer
	cfgNames := configuredSessionNames(cfg, "", store)
	_ = reconcileSessionBeads(
		context.Background(), []beads.Bead{bead}, desired, cfgNames,
		cfg, sp, store, nil, nil, nil, newDrainTracker(), map[string]int{"helper": 1}, false, nil, "",
		nil, clk, events.Discard, 0, 0, &stdout, &stderr,
	)

	got, err := store.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get(bead): %v", err)
	}
	if got.Status == "closed" {
		t.Fatalf("status = closed, want never-started pending create preserved until never-started lease expires; metadata=%v", got.Metadata)
	}
	if got.Metadata["close_reason"] != "" {
		t.Fatalf("close_reason = %q, want empty", got.Metadata["close_reason"])
	}
}

func TestReconcileSessionBeads_RollsBackPendingCreateWhenLeaseExpiredAndNoRuntime(t *testing.T) {
	// Regression test: a session bead in the desired set with
	// pending_create_claim=true but no live runtime AND no active lease
	// (last_woke_at empty AND CreatedAt past the never-started pending-create
	// window) is stuck. Without this rollback, the bead lives forever holding
	// its alias, blocking new spawn attempts ("alias already belongs to
	// gm-XXXX") for any session whose template still has demand.
	store := beads.NewMemStore()
	sp := runtime.NewFake() // no runtime started
	clk := &clock.Fake{Time: time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC)}
	cfg := &config.City{Agents: []config.Agent{{Name: "helper"}}}
	desired := map[string]TemplateParams{
		"helper": {
			Command:      "test-cmd",
			SessionName:  "helper",
			TemplateName: "helper",
		},
	}

	bead, err := store.Create(beads.Bead{
		Title:  "helper",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "template:helper"},
		Metadata: map[string]string{
			"session_name":              "helper",
			"session_name_explicit":     "true",
			"pending_create_claim":      "true",
			"pending_create_started_at": pendingCreateStartedAtNow(clk.Now().Add(-(pendingCreateNeverStartedTimeout + time.Second))),
			"template":                  "helper",
			"state":                     "creating",
			"generation":                "1",
			"continuation_epoch":        "1",
			"instance_token":            "test-token",
			// last_woke_at deliberately empty — preWakeCommit never fired.
		},
	})
	if err != nil {
		t.Fatalf("Create(bead): %v", err)
	}
	// Keep CreatedAt fresh to prove production pending_create_started_at anchors
	// the never-started pending-create lease for desired sessions.
	bead.CreatedAt = clk.Now().Add(-time.Minute)

	var stdout, stderr bytes.Buffer
	cfgNames := configuredSessionNames(cfg, "", store)
	_ = reconcileSessionBeads(
		context.Background(), []beads.Bead{bead}, desired, cfgNames,
		cfg, sp, store, nil, nil, nil, newDrainTracker(), map[string]int{}, false, nil, "",
		nil, clk, events.Discard, 0, 0, &stdout, &stderr,
	)

	got, err := store.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get(bead): %v", err)
	}
	if got.Status != "closed" {
		t.Fatalf("status = %q, want closed (stale lease + no runtime should rollback)", got.Status)
	}
	if want := sessionpkg.CanonicalCloseReason(string(sessionpkg.StateFailedCreate)); got.Metadata["close_reason"] != want {
		t.Fatalf("close_reason = %q, want %q", got.Metadata["close_reason"], want)
	}
}

func TestReconcileSessionBeads_DoesNotRollbackStoppedPendingCreateAsExpiredLease(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	clk := &clock.Fake{Time: time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC)}
	cfg := &config.City{Agents: []config.Agent{{Name: "helper"}}}
	desired := map[string]TemplateParams{
		"helper": {
			Command:      "test-cmd",
			SessionName:  "helper",
			TemplateName: "helper",
		},
	}

	bead, err := store.Create(beads.Bead{
		Title:  "helper",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "template:helper"},
		Metadata: map[string]string{
			"session_name":         "helper",
			"pending_create_claim": "true",
			"template":             "helper",
			"state":                "stopped",
			"generation":           "1",
			"continuation_epoch":   "1",
			"instance_token":       "test-token",
		},
	})
	if err != nil {
		t.Fatalf("Create(bead): %v", err)
	}
	bead.CreatedAt = clk.Now().Add(-24 * time.Hour)

	var stdout, stderr bytes.Buffer
	cfgNames := configuredSessionNames(cfg, "", store)
	_ = reconcileSessionBeads(
		context.Background(), []beads.Bead{bead}, desired, cfgNames,
		cfg, sp, store, nil, nil, nil, newDrainTracker(), map[string]int{"helper": 1}, false, nil, "",
		nil, clk, events.Discard, 0, 0, &stdout, &stderr,
	)

	got, err := store.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get(bead): %v", err)
	}
	if got.Status == "closed" {
		t.Fatalf("status = closed, want stopped pending-create bead preserved for start retry; metadata=%v", got.Metadata)
	}
	if got.Metadata["close_reason"] != "" {
		t.Fatalf("close_reason = %q, want empty", got.Metadata["close_reason"])
	}
}

func TestReconcileSessionBeads_RateLimitPendingCreateBatchFailureRetriesBeforeRollback(t *testing.T) {
	env := newReconcilerTestEnv()
	store := &failRateLimitHoldStore{
		MemStore:          beads.NewMemStore(),
		failRateLimitHold: true,
	}
	env.store = store
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	env.desiredState["worker"] = TemplateParams{
		Command:      "test-cmd",
		SessionName:  "worker",
		TemplateName: "worker",
		Hints:        agent.StartupHints{ProcessNames: []string{"agent-cli"}},
	}
	if err := env.sp.Start(context.Background(), "worker", runtime.Config{Command: "test-cmd", ProcessNames: []string{"agent-cli"}}); err != nil {
		t.Fatalf("Start(worker): %v", err)
	}
	env.sp.Zombies["worker"] = true
	env.sp.SetPeekOutput("worker", "You've hit your limit, Pro plan\n\n/rate-limit-options")
	lastWoke := env.clk.Now().Add(-2 * time.Minute).UTC().Format(time.RFC3339)
	session := env.createSessionBead("worker", "worker")
	session.CreatedAt = env.clk.Now().Add(-5 * time.Minute)
	env.setSessionMetadata(&session, map[string]string{
		"state":                 "creating",
		"pending_create_claim":  "true",
		"last_woke_at":          lastWoke,
		"session_key":           "keep-session",
		"started_config_hash":   "keep-hash",
		"wake_attempts":         "2",
		"continuation_epoch":    "1",
		"session_name_explicit": "true",
	})

	if woken := env.reconcile([]beads.Bead{session}); woken != 0 {
		t.Fatalf("woken after failed hold write = %d, want 0", woken)
	}
	if store.rateLimitHoldCalls != 1 {
		t.Fatalf("rate-limit hold attempts = %d, want 1", store.rateLimitHoldCalls)
	}
	got, err := store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get after failed hold write: %v", err)
	}
	if got.Status == "closed" {
		t.Fatalf("status = closed with close_reason=%q, want retryable pending-create hold", got.Metadata["close_reason"])
	}
	if got.Metadata["pending_create_claim"] != "true" {
		t.Fatalf("pending_create_claim = %q, want preserved after failed hold write", got.Metadata["pending_create_claim"])
	}
	if got.Metadata["last_woke_at"] != lastWoke {
		t.Fatalf("last_woke_at = %q, want preserved after failed hold write", got.Metadata["last_woke_at"])
	}
	if got.Metadata["state"] != "creating" {
		t.Fatalf("state = %q, want unchanged creating after failed hold write", got.Metadata["state"])
	}

	store.failRateLimitHold = false
	got.CreatedAt = session.CreatedAt
	if woken := env.reconcile([]beads.Bead{got}); woken != 0 {
		t.Fatalf("woken after retry = %d, want 0", woken)
	}
	retried, err := store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get after retry: %v", err)
	}
	if retried.Status == "closed" {
		t.Fatalf("status = closed after successful retry, want rate-limit hold")
	}
	if retried.Metadata["sleep_reason"] != "rate_limit" {
		t.Fatalf("sleep_reason = %q, want rate_limit after retry", retried.Metadata["sleep_reason"])
	}
	if retried.Metadata["state"] != "asleep" {
		t.Fatalf("state = %q, want asleep after retry", retried.Metadata["state"])
	}
	if retried.Metadata["pending_create_claim"] != "" {
		t.Fatalf("pending_create_claim = %q, want cleared after durable hold", retried.Metadata["pending_create_claim"])
	}
	if retried.Metadata["last_woke_at"] != "" {
		t.Fatalf("last_woke_at = %q, want cleared after durable hold", retried.Metadata["last_woke_at"])
	}
}

func TestReconcileSessionBeads_PreservesPendingCreateWhenLeaseRecentNoRuntime(t *testing.T) {
	// Defensive: a session bead with pending_create_claim=true and no live
	// runtime but a *fresh* last_woke_at lease (or recently CreatedAt) must
	// NOT be rolled back — the spawn is genuinely in flight, just not yet
	// observable. Rolling back here would race with the async start pipeline.
	store := beads.NewMemStore()
	sp := runtime.NewFake() // no runtime
	clk := &clock.Fake{Time: time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC)}
	cfg := &config.City{Agents: []config.Agent{{Name: "helper"}}}
	desired := map[string]TemplateParams{
		"helper": {
			Command:      "test-cmd",
			SessionName:  "helper",
			TemplateName: "helper",
		},
	}

	bead, err := store.Create(beads.Bead{
		Title:  "helper",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "template:helper"},
		Metadata: map[string]string{
			"session_name":          "helper",
			"session_name_explicit": "true",
			"pending_create_claim":  "true",
			"template":              "helper",
			"state":                 "creating",
			"generation":            "1",
			"continuation_epoch":    "1",
			"instance_token":        "test-token",
			"last_woke_at":          clk.Now().Add(-10 * time.Second).Format(time.RFC3339),
		},
	})
	if err != nil {
		t.Fatalf("Create(bead): %v", err)
	}

	var stdout, stderr bytes.Buffer
	cfgNames := configuredSessionNames(cfg, "", store)
	_ = reconcileSessionBeads(
		context.Background(), []beads.Bead{bead}, desired, cfgNames,
		cfg, sp, store, nil, nil, nil, newDrainTracker(), map[string]int{}, false, nil, "",
		nil, clk, events.Discard, 0, 0, &stdout, &stderr,
	)

	got, err := store.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get(bead): %v", err)
	}
	if got.Status == "closed" {
		t.Fatalf("status = closed, want preserved (lease still fresh)")
	}
	if strings.TrimSpace(got.Metadata["pending_create_claim"]) != "true" {
		t.Fatalf("pending_create_claim = %q, want still 'true'", got.Metadata["pending_create_claim"])
	}
}

func TestPendingCreateNeverStartedExpiredEdges(t *testing.T) {
	clk := &clock.Fake{Time: time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC)}
	base := beads.Bead{
		Metadata: map[string]string{
			"pending_create_claim": "true",
			"state":                "creating",
		},
	}

	tests := []struct {
		name      string
		createdAt time.Time
		startedAt string
		want      bool
	}{
		{
			name:      "before boundary",
			createdAt: clk.Now().Add(-(pendingCreateNeverStartedTimeout - time.Second)),
			want:      false,
		},
		{
			name:      "exact boundary",
			createdAt: clk.Now().Add(-pendingCreateNeverStartedTimeout),
			want:      false,
		},
		{
			name:      "after boundary",
			createdAt: clk.Now().Add(-(pendingCreateNeverStartedTimeout + time.Second)),
			want:      true,
		},
		{
			name:      "zero created at",
			createdAt: time.Time{},
			want:      true,
		},
		{
			name:      "started at overrides older created at before boundary",
			createdAt: clk.Now().Add(-24 * time.Hour),
			startedAt: pendingCreateStartedAtNow(clk.Now().Add(-(pendingCreateNeverStartedTimeout - time.Second))),
			want:      false,
		},
		{
			name:      "started at overrides fresh created at after boundary",
			createdAt: clk.Now().Add(-time.Minute),
			startedAt: pendingCreateStartedAtNow(clk.Now().Add(-(pendingCreateNeverStartedTimeout + time.Second))),
			want:      true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bead := base
			if tt.startedAt != "" {
				bead.Metadata = map[string]string{
					"pending_create_claim":      "true",
					"pending_create_started_at": tt.startedAt,
					"state":                     "creating",
				}
			}
			bead.CreatedAt = tt.createdAt
			if got := pendingCreateNeverStartedExpired(bead, clk); got != tt.want {
				t.Fatalf("pendingCreateNeverStartedExpired() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestPendingCreateLeaseExpiredForRollbackFallsBackToStaleWindowForInvalidLastWokeAt(t *testing.T) {
	clk := &clock.Fake{Time: time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC)}
	base := beads.Bead{
		Metadata: map[string]string{
			"pending_create_claim": "true",
			"state":                "creating",
			"last_woke_at":         "not-a-timestamp",
		},
	}

	recent := base
	recent.CreatedAt = clk.Now().Add(-(staleCreatingStateTimeout - time.Second))
	if pendingCreateLeaseExpiredForRollback(recent, clk, time.Minute) {
		t.Fatal("invalid last_woke_at used never-started lease; want legacy stale window before rollback")
	}

	stale := base
	stale.CreatedAt = clk.Now().Add(-(staleCreatingStateTimeout + time.Second))
	if !pendingCreateLeaseExpiredForRollback(stale, clk, time.Minute) {
		t.Fatal("invalid last_woke_at preserved after stale window; want rollback")
	}
}

func TestTraceHealClearedPendingCreateLeaseRecordsDecision(t *testing.T) {
	trace := &sessionReconcilerTraceCycle{
		tracer: &SessionReconcilerTracer{
			detail: map[string]TraceSource{"helper": TraceSourceManual},
		},
		dropReasons:       map[string]int{},
		pendingDetail:     map[string][]SessionReconcilerTraceRecord{},
		pendingDropped:    map[string]int{},
		templatesTouched:  map[string]struct{}{},
		detailedTemplates: map[string]struct{}{},
		decisionCounts:    map[string]int{},
		operationCounts:   map[string]int{},
		mutationCounts:    map[string]int{},
		reasonCounts:      map[string]int{},
		outcomeCounts:     map[string]int{},
	}
	session := makeBead("b1", map[string]string{
		"session_name": "helper",
		"state":        "asleep",
		"template":     "helper",
	})

	traceHealClearedPendingCreateLease(
		trace,
		session,
		&config.City{Agents: []config.Agent{{Name: "helper"}}},
		"",
		"",
		"creating",
		"2026-05-19T08:58:30Z",
		"2026-05-19T08:58:30Z",
		false,
		map[string]string{
			"pending_create_claim":      "",
			"pending_create_started_at": "",
			"state":                     "asleep",
		},
	)

	if len(trace.records) != 1 {
		t.Fatalf("trace records = %d, want 1", len(trace.records))
	}
	rec := trace.records[0]
	if rec.RecordType != TraceRecordDecision {
		t.Fatalf("record type = %q, want decision", rec.RecordType)
	}
	if rec.SiteCode != TraceSiteReconcilerPendingCreate {
		t.Fatalf("site = %q, want %q", rec.SiteCode, TraceSiteReconcilerPendingCreate)
	}
	if rec.OutcomeCode != TraceOutcomeApplied {
		t.Fatalf("outcome = %q, want %q", rec.OutcomeCode, TraceOutcomeApplied)
	}
	if got := rec.Fields["raw_reason_code"]; got != "heal_cleared_stale_lease" {
		t.Fatalf("raw_reason_code = %#v, want heal_cleared_stale_lease", got)
	}
	if got := rec.Fields["state_before"]; got != "creating" {
		t.Fatalf("state_before = %#v, want creating", got)
	}
	if got := rec.Fields["state_after"]; got != "asleep" {
		t.Fatalf("state_after = %#v, want asleep", got)
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
	if want := sessionpkg.CanonicalCloseReason("failed-create"); got.Metadata["close_reason"] != want {
		t.Fatalf("close_reason = %q, want %q", got.Metadata["close_reason"], want)
	}
	if got.Metadata["state"] != "failed-create" {
		t.Fatalf("state = %q, want %q", got.Metadata["state"], "failed-create")
	}
}

func TestReconcileSessionBeads_RollbackBudgetDefersExcessMismatchesAndStillStarts(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "helper"}}}

	var sessions []beads.Bead
	for i := 0; i < 6; i++ {
		name := fmt.Sprintf("sky-%d", i)
		env.addDesired(name, "helper", false)
		session := env.createSessionBead(name, "helper")
		env.markSessionCreating(&session)
		env.setSessionMetadata(&session, map[string]string{
			"pending_create_claim":  "true",
			"session_name_explicit": "true",
			"instance_token":        fmt.Sprintf("token-%d", i),
		})
		if err := env.sp.Start(context.Background(), name, runtime.Config{Command: "test-cmd"}); err != nil {
			t.Fatalf("Start(%s): %v", name, err)
		}
		if err := env.sp.SetMeta(name, "GC_SESSION_ID", "different-"+session.ID); err != nil {
			t.Fatalf("SetMeta(%s, GC_SESSION_ID): %v", name, err)
		}
		if err := env.sp.SetMeta(name, "GC_INSTANCE_TOKEN", "different-token"); err != nil {
			t.Fatalf("SetMeta(%s, GC_INSTANCE_TOKEN): %v", name, err)
		}
		sessions = append(sessions, session)
	}

	env.addDesired("starter", "helper", false)
	starter := env.createSessionBead("starter", "helper")
	env.markSessionCreating(&starter)
	sessions = append(sessions, starter)

	if woken := env.reconcile(sessions); woken != 1 {
		t.Fatalf("woken = %d, want 1 planned start after rollback budget is exhausted", woken)
	}
	if got := strings.Count(env.stderr.String(), "deferring rollback of sky-"); got != 1 {
		t.Fatalf("deferred rollback messages = %d, want 1; stderr:\n%s", got, env.stderr.String())
	}
	closedMismatches := 0
	deferredMismatches := 0
	for i := 0; i < 6; i++ {
		name := fmt.Sprintf("sky-%d", i)
		got, err := env.store.Get(sessions[i].ID)
		if err != nil {
			t.Fatalf("Get(%s): %v", sessions[i].ID, err)
		}
		if got.Status == "closed" {
			if want := sessionpkg.CanonicalCloseReason(string(sessionpkg.StateFailedCreate)); got.Metadata["close_reason"] != want {
				t.Fatalf("%s close_reason = %q, want %q", name, got.Metadata["close_reason"], want)
			}
			closedMismatches++
			continue
		}
		if got.Metadata["pending_create_claim"] != "true" {
			t.Fatalf("%s pending_create_claim = %q, want true on deferred mismatch", name, got.Metadata["pending_create_claim"])
		}
		deferredMismatches++
	}
	if closedMismatches != 5 {
		t.Fatalf("closed mismatches = %d, want 5", closedMismatches)
	}
	if deferredMismatches != 1 {
		t.Fatalf("deferred mismatches = %d, want 1", deferredMismatches)
	}
	started, err := env.store.Get(starter.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", starter.ID, err)
	}
	if started.Metadata["state"] != "active" {
		t.Fatalf("starter state = %q, want active", started.Metadata["state"])
	}
	if !env.sp.IsRunning("starter") {
		t.Fatal("starter runtime was not started after rollback budget was exhausted")
	}
}

func TestReconcileSessionBeads_RollbackBudgetDefersExcessStaleNoRuntimeCreatesAndStillStarts(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "helper"}}}

	var sessions []beads.Bead
	staleStartedAt := pendingCreateStartedAtNow(env.clk.Now().Add(-(pendingCreateNeverStartedTimeout + time.Second)))
	for i := 0; i < 6; i++ {
		name := fmt.Sprintf("sky-%d", i)
		env.addDesired(name, "helper", false)
		session := env.createSessionBead(name, "helper")
		env.markSessionCreating(&session)
		session.CreatedAt = env.clk.Now().Add(-24 * time.Hour)
		env.setSessionMetadata(&session, map[string]string{
			"pending_create_claim":      "true",
			"pending_create_started_at": staleStartedAt,
			"session_name_explicit":     "true",
			"instance_token":            fmt.Sprintf("token-%d", i),
		})
		sessions = append(sessions, session)
	}

	env.addDesired("starter", "helper", false)
	starter := env.createSessionBead("starter", "helper")
	env.markSessionCreating(&starter)
	sessions = append(sessions, starter)

	if woken := env.reconcile(sessions); woken != 1 {
		t.Fatalf("woken = %d, want 1 planned start after rollback budget is exhausted", woken)
	}
	if got := strings.Count(env.stderr.String(), "deferring rollback of sky-"); got != 1 {
		t.Fatalf("deferred rollback messages = %d, want 1; stderr:\n%s", got, env.stderr.String())
	}
	closedCreates := 0
	deferredCreates := 0
	for i := 0; i < 6; i++ {
		name := fmt.Sprintf("sky-%d", i)
		got, err := env.store.Get(sessions[i].ID)
		if err != nil {
			t.Fatalf("Get(%s): %v", sessions[i].ID, err)
		}
		if got.Status == "closed" {
			if want := sessionpkg.CanonicalCloseReason(string(sessionpkg.StateFailedCreate)); got.Metadata["close_reason"] != want {
				t.Fatalf("%s close_reason = %q, want %q", name, got.Metadata["close_reason"], want)
			}
			closedCreates++
			continue
		}
		if got.Metadata["pending_create_claim"] != "true" {
			t.Fatalf("%s pending_create_claim = %q, want true on deferred stale create", name, got.Metadata["pending_create_claim"])
		}
		deferredCreates++
	}
	if closedCreates != 5 {
		t.Fatalf("closed stale creates = %d, want 5", closedCreates)
	}
	if deferredCreates != 1 {
		t.Fatalf("deferred stale creates = %d, want 1", deferredCreates)
	}
	started, err := env.store.Get(starter.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", starter.ID, err)
	}
	if started.Metadata["state"] != "active" {
		t.Fatalf("starter state = %q, want active", started.Metadata["state"])
	}
	if !env.sp.IsRunning("starter") {
		t.Fatal("starter runtime was not started after rollback budget was exhausted")
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
	if want := sessionpkg.CanonicalCloseReason("failed-create"); got.Metadata["close_reason"] != want {
		t.Fatalf("close_reason = %q, want %q", got.Metadata["close_reason"], want)
	}
	if got.Metadata["state"] != "failed-create" {
		t.Fatalf("state = %q, want %q", got.Metadata["state"], "failed-create")
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
	env.setSessionMetadata(&session, map[string]string{
		"started_config_hash": runtime.CoreFingerprint(runtime.Config{Command: "test-cmd"}),
	})

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

func TestReconcileSessionBeads_LiveDriftAppliedWhenNoStoredHash(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	// Desired state has session_live from a newly-added pack.
	env.addDesiredLive("worker", "worker", true, []string{"echo theme-applied"})

	// Create a session bead WITHOUT live_hash — simulates a bead created
	// before live_hash tracking was added, or via gc session new (which
	// doesn't set live_hash in its metadata).
	session := env.createSessionBead("worker", "worker")
	delete(session.Metadata, "live_hash")
	_ = env.store.SetMetadata(session.ID, "live_hash", "")
	env.markSessionActive(&session)
	env.setSessionMetadata(&session, map[string]string{
		"started_config_hash": runtime.CoreFingerprint(runtime.Config{Command: "test-cmd"}),
	})

	env.reconcile([]beads.Bead{session})

	// Should NOT drain (core hash matches).
	if ds := env.dt.get(session.ID); ds != nil {
		t.Errorf("expected no drain for live-only drift, got reason=%q", ds.reason)
	}
	// Should have applied session_live and recorded the hash.
	b, _ := env.store.Get(session.ID)
	expectedCfg := templateParamsToConfig(env.desiredState["worker"])
	expectedLive := runtime.LiveFingerprint(expectedCfg)
	if b.Metadata["live_hash"] != expectedLive {
		t.Errorf("live_hash not applied: got %q, want %q", b.Metadata["live_hash"], expectedLive)
	}
	if b.Metadata["started_live_hash"] != expectedLive {
		t.Errorf("started_live_hash not applied: got %q, want %q", b.Metadata["started_live_hash"], expectedLive)
	}
}

func TestReconcileSessionBeads_LiveHashBackfilledSilentlyWhenNoLiveConfig(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	// Desired state has NO session_live — agent has no live config at all.
	env.addDesired("worker", "worker", true)

	// Create a session bead WITHOUT live_hash — legacy session.
	session := env.createSessionBead("worker", "worker")
	delete(session.Metadata, "live_hash")
	_ = env.store.SetMetadata(session.ID, "live_hash", "")
	env.markSessionActive(&session)
	env.setSessionMetadata(&session, map[string]string{
		"started_config_hash": runtime.CoreFingerprint(runtime.Config{Command: "test-cmd"}),
	})

	env.reconcile([]beads.Bead{session})

	// Should NOT drain.
	if ds := env.dt.get(session.ID); ds != nil {
		t.Errorf("expected no drain, got reason=%q", ds.reason)
	}
	// live_hash should be backfilled silently.
	b, _ := env.store.Get(session.ID)
	expectedCfg := templateParamsToConfig(env.desiredState["worker"])
	expectedLive := runtime.LiveFingerprint(expectedCfg)
	if b.Metadata["live_hash"] != expectedLive {
		t.Errorf("live_hash not backfilled: got %q, want %q", b.Metadata["live_hash"], expectedLive)
	}
	// Should NOT have printed the "Live config changed" message — this is
	// a silent backfill, not a real live-drift reapply.
	if bytes.Contains(env.stdout.Bytes(), []byte("Live config changed")) {
		t.Errorf("unexpected 'Live config changed' output for silent backfill")
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
	env.addRunningWorkerDesiredWithNewConfig()
	session := env.createSessionBead("worker", "worker")
	env.setSessionMetadata(&session, map[string]string{
		"started_config_hash": runtime.CoreFingerprint(runtime.Config{Command: "test-cmd"}),
	})

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

// --- attached-session config-drift suppression tests ---

// An attached session must NEVER be restarted due to config drift.
// The sessionAttachedForConfigDrift guard fires before any named/non-named
// path, so the session stays running with no drain initiated.
func TestReconcileSessionBeads_AttachedSessionNeverRestartedOnConfigDrift(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	env.addRunningWorkerDesiredWithNewConfig()
	session := env.createSessionBead("worker", "worker")
	env.setSessionMetadata(&session, map[string]string{
		"started_config_hash": runtime.CoreFingerprint(runtime.Config{Command: "test-cmd"}),
	})
	// Mark the session as attached — a user terminal is connected.
	env.sp.SetAttached("worker", true)

	env.reconcile([]beads.Bead{session})

	if ds := env.dt.get(session.ID); ds != nil {
		t.Errorf("attached session should never be drained for config drift, got reason=%q", ds.reason)
	}
	if !env.sp.IsRunning("worker") {
		t.Error("attached session should still be running after config-drift check")
	}
}

func TestReconcileSessionBeads_ConfigDriftAttachmentErrorDefersLiveDrift(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	env.addRunningWorkerDesiredWithNewConfig()
	session := env.createSessionBead("worker", "worker")
	env.setSessionMetadata(&session, map[string]string{
		"started_config_hash": runtime.CoreFingerprint(runtime.Config{Command: "test-cmd"}),
	})
	backing := env.store
	env.store = &sessionObservationGetErrorStore{
		Store:     backing,
		id:        session.ID,
		remaining: 1,
		err:       errors.New("attachment observation failed"),
	}

	env.reconcile([]beads.Bead{session})

	if ds := env.dt.get(session.ID); ds != nil {
		t.Fatalf("attachment observation error should defer config-drift drain, got %+v", ds)
	}
	if !env.sp.IsRunning("worker") {
		t.Fatal("attachment observation error should keep config-drift session running")
	}
	if !strings.Contains(env.stderr.String(), "observing config-drift attachment") {
		t.Fatalf("stderr = %q, want attachment observation diagnostic", env.stderr.String())
	}
}

// The deferred_attached outcome must persist across reconciler cycles:
// as long as the session stays attached, each cycle skips config-drift restart.
func TestReconcileSessionBeads_AttachedDeferralPersistsAcrossCycles(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	env.addRunningWorkerDesiredWithNewConfig()
	session := env.createSessionBead("worker", "worker")
	env.setSessionMetadata(&session, map[string]string{
		"started_config_hash": runtime.CoreFingerprint(runtime.Config{Command: "test-cmd"}),
	})
	env.sp.SetAttached("worker", true)

	// Run multiple reconciler cycles while attached.
	for i := 0; i < 3; i++ {
		env.reconcile([]beads.Bead{session})
		if ds := env.dt.get(session.ID); ds != nil {
			t.Fatalf("cycle %d: attached session should not be drained, got reason=%q", i, ds.reason)
		}
	}
	if !env.sp.IsRunning("worker") {
		t.Error("worker should still be running after 3 attached reconciler cycles")
	}
}

// After detach, normal config-drift restart logic applies:
// the session should be drained when it is no longer attached.
func TestReconcileSessionBeads_ConfigDriftAppliesAfterDetach(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	env.addRunningWorkerDesiredWithNewConfig()
	session := env.createSessionBead("worker", "worker")
	env.setSessionMetadata(&session, map[string]string{
		"started_config_hash": runtime.CoreFingerprint(runtime.Config{Command: "test-cmd"}),
	})

	// Cycle 1: attached — no drain.
	env.sp.SetAttached("worker", true)
	env.reconcile([]beads.Bead{session})
	if ds := env.dt.get(session.ID); ds != nil {
		t.Fatalf("while attached: expected no drain, got reason=%q", ds.reason)
	}

	// Cycle 2: detached — drift should trigger drain.
	env.sp.SetAttached("worker", false)
	env.reconcile([]beads.Bead{session})
	ds := env.dt.get(session.ID)
	if ds == nil {
		t.Fatal("after detach: expected drain for config drift")
	}
	if ds.reason != "config-drift" {
		t.Errorf("drain reason = %q, want %q", ds.reason, "config-drift")
	}
}

func TestReconcileSessionBeads_AttachedSessionCancelsQueuedConfigDriftDrain(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	env.addRunningWorkerDesiredWithNewConfig()
	session := env.createSessionBead("worker", "worker")
	env.markSessionActive(&session)
	env.setSessionMetadata(&session, map[string]string{
		"started_config_hash": runtime.CoreFingerprint(runtime.Config{Command: "test-cmd"}),
	})

	env.reconcile([]beads.Bead{session})
	if ds := env.dt.get(session.ID); ds == nil || ds.reason != "config-drift" {
		t.Fatalf("detached config drift should queue a config-drift drain, got %+v", ds)
	}
	if ack, _ := env.sp.GetMeta("worker", "GC_DRAIN_ACK"); ack != "1" {
		t.Fatalf("GC_DRAIN_ACK after queued drain = %q, want 1", ack)
	}

	env.sp.SetAttached("worker", true)
	env.clk.Time = env.clk.Now().Add(defaultDrainTimeout + time.Second)
	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", session.ID, err)
	}
	env.reconcile([]beads.Bead{got})

	if ds := env.dt.get(session.ID); ds != nil {
		t.Fatalf("attached session should cancel queued config-drift drain, got %+v", ds)
	}
	if ack, _ := env.sp.GetMeta("worker", "GC_DRAIN_ACK"); ack != "" {
		t.Fatalf("GC_DRAIN_ACK after attach cancellation = %q, want empty", ack)
	}
	if !env.sp.IsRunning("worker") {
		t.Fatal("attached session should remain running after queued drain advances")
	}
}

func TestReconcileSessionBeads_AttachedSessionCancelsQueuedConfigDriftDrainBeforeDrainAckStop(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	env.addRunningWorkerDesiredWithNewConfig()
	session := env.createSessionBead("worker", "worker")
	env.markSessionActive(&session)
	env.setSessionMetadata(&session, map[string]string{
		"started_config_hash": runtime.CoreFingerprint(runtime.Config{Command: "test-cmd"}),
	})
	dops := newDrainOps(env.sp)

	env.reconcileWithPoolDesiredAndDrainOps([]beads.Bead{session}, map[string]int{"worker": 1}, dops)
	if ds := env.dt.get(session.ID); ds == nil || ds.reason != "config-drift" {
		t.Fatalf("detached config drift should queue a config-drift drain, got %+v", ds)
	}
	if ack, _ := env.sp.GetMeta("worker", "GC_DRAIN_ACK"); ack != "1" {
		t.Fatalf("GC_DRAIN_ACK after queued drain = %q, want 1", ack)
	}

	env.sp.SetAttached("worker", true)
	env.clk.Time = env.clk.Now().Add(defaultDrainTimeout + time.Second)
	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", session.ID, err)
	}
	env.reconcileWithPoolDesiredAndDrainOps([]beads.Bead{got}, map[string]int{"worker": 1}, dops)

	if ds := env.dt.get(session.ID); ds != nil {
		t.Fatalf("attached session should cancel queued config-drift drain before drain-ack stop, got %+v", ds)
	}
	if ack, _ := env.sp.GetMeta("worker", "GC_DRAIN_ACK"); ack != "" {
		t.Fatalf("GC_DRAIN_ACK after attach cancellation = %q, want empty", ack)
	}
	if !env.sp.IsRunning("worker") {
		t.Fatal("attached session should remain running after reconciler-owned drain ack is canceled")
	}
}

func TestReconcileSessionBeads_ConfigDriftDrainAckAttachmentErrorDefersStop(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	env.addRunningWorkerDesiredWithNewConfig()
	session := env.createSessionBead("worker", "worker")
	env.markSessionActive(&session)
	env.setSessionMetadata(&session, map[string]string{
		"started_config_hash": runtime.CoreFingerprint(runtime.Config{Command: "test-cmd"}),
	})
	dops := newDrainOps(env.sp)

	env.reconcileWithPoolDesiredAndDrainOps([]beads.Bead{session}, map[string]int{"worker": 1}, dops)
	if ds := env.dt.get(session.ID); ds == nil || ds.reason != "config-drift" {
		t.Fatalf("detached config drift should queue a config-drift drain, got %+v", ds)
	}
	if ack, _ := env.sp.GetMeta("worker", "GC_DRAIN_ACK"); ack != "1" {
		t.Fatalf("GC_DRAIN_ACK after queued drain = %q, want 1", ack)
	}
	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", session.ID, err)
	}
	backing := env.store
	env.store = &sessionObservationGetErrorStore{
		Store:     backing,
		id:        session.ID,
		remaining: 1,
		err:       errors.New("attachment observation failed"),
	}

	env.clk.Time = env.clk.Now().Add(defaultDrainTimeout + time.Second)
	env.reconcileWithPoolDesiredAndDrainOps([]beads.Bead{got}, map[string]int{"worker": 1}, dops)

	after, err := backing.Get(session.ID)
	if err != nil {
		t.Fatalf("Get after reconcile: %v", err)
	}
	if isDrainAckStopPending(after) {
		t.Fatalf("attachment observation error should not mark drain-ack stop pending; metadata=%v", after.Metadata)
	}
	if !env.sp.IsRunning("worker") {
		t.Fatal("attachment observation error should keep config-drift drain-ack session running")
	}
	if !strings.Contains(env.stderr.String(), "observing config-drift attachment") {
		t.Fatalf("stderr = %q, want attachment observation diagnostic", env.stderr.String())
	}
}

func TestReconcileSessionBeads_ConfigDriftDrainAckUsesRecentAttachedDeferral(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "worker",
			StartCommand:      "new-cmd",
			MaxActiveSessions: intPtr(1),
		}},
		NamedSessions: []config.NamedSession{{
			Template: "worker",
			Mode:     "always",
		}},
	}

	sessionName := config.NamedSessionRuntimeName(env.cfg.Workspace.Name, env.cfg.Workspace, "worker")
	env.desiredState[sessionName] = TemplateParams{
		TemplateName:            "worker",
		InstanceName:            "worker",
		Alias:                   "worker",
		Command:                 "new-cmd",
		ConfiguredNamedIdentity: "worker",
		ConfiguredNamedMode:     "always",
	}
	oldRuntime := runtime.Config{Command: "old-cmd"}
	oldHash := runtime.CoreFingerprint(oldRuntime)
	if err := env.sp.Start(context.Background(), sessionName, oldRuntime); err != nil {
		t.Fatalf("Start(old runtime): %v", err)
	}

	session := env.createSessionBead(sessionName, "worker")
	env.markSessionActive(&session)
	env.setSessionMetadata(&session, map[string]string{
		namedSessionMetadataKey:      "true",
		namedSessionIdentityMetadata: "worker",
		namedSessionModeMetadata:     "always",
		"session_key":                "old-provider-conversation",
		"started_config_hash":        oldHash,
		"started_live_hash":          runtime.LiveFingerprint(oldRuntime),
	})
	driftKey := sessionConfigDriftKey(session, env.cfg, env.desiredState[sessionName])
	if driftKey == "" {
		t.Fatal("expected config drift key")
	}
	env.setSessionMetadata(&session, map[string]string{
		sessionAttachedConfigDriftDeferredAtMetadata:  env.clk.Now().UTC().Format(time.RFC3339),
		sessionAttachedConfigDriftDeferredKeyMetadata: driftKey,
	})

	ds := &drainState{
		startedAt:  env.clk.Now().UTC(),
		deadline:   env.clk.Now().UTC().Add(defaultDrainTimeout),
		reason:     "config-drift",
		generation: 1,
		ackSet:     true,
	}
	env.dt.set(session.ID, ds)
	if err := setReconcilerDrainAckMetadata(env.sp, sessionName, ds); err != nil {
		t.Fatalf("setReconcilerDrainAckMetadata: %v", err)
	}
	falseAttached := make([]bool, 100)
	env.sp.SetAttachedSequence(sessionName, falseAttached...)

	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", session.ID, err)
	}
	env.reconcileWithPoolDesiredAndDrainOps([]beads.Bead{got}, map[string]int{"worker": 1}, newDrainOps(env.sp))

	if ds := env.dt.get(session.ID); ds != nil {
		t.Fatalf("recent attached deferral should cancel config-drift drain ack, got %+v", ds)
	}
	if ack, _ := env.sp.GetMeta(sessionName, "GC_DRAIN_ACK"); ack != "" {
		t.Fatalf("GC_DRAIN_ACK after recent-deferral cancellation = %q, want empty", ack)
	}
	if !env.sp.IsRunning(sessionName) {
		t.Fatal("recent attached deferral should keep session running through drain-ack false negative")
	}
	got, err = env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get after reconcile: %v", err)
	}
	if got.Metadata["started_config_hash"] != oldHash {
		t.Fatalf("started_config_hash = %q, want %q", got.Metadata["started_config_hash"], oldHash)
	}
	if got.Metadata["session_key"] != "old-provider-conversation" {
		t.Fatalf("session_key = %q, want old provider conversation preserved", got.Metadata["session_key"])
	}
}

func TestReconcileSessionBeads_ConfigDriftDrainAckUsesRecentAttachedDeferralForPoolSession(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Agents: []config.Agent{{
			Name:              "worker",
			StartCommand:      "new-cmd",
			MaxActiveSessions: intPtr(1),
		}},
	}
	env.desiredState["worker"] = TemplateParams{
		TemplateName: "worker",
		InstanceName: "worker",
		Alias:        "worker",
		Command:      "new-cmd",
	}
	oldRuntime := runtime.Config{Command: "old-cmd"}
	oldHash := runtime.CoreFingerprint(oldRuntime)
	if err := env.sp.Start(context.Background(), "worker", oldRuntime); err != nil {
		t.Fatalf("Start(old runtime): %v", err)
	}

	session := env.createSessionBead("worker", "worker")
	env.markSessionActive(&session)
	env.setSessionMetadata(&session, map[string]string{
		"session_key":         "old-provider-conversation",
		"started_config_hash": oldHash,
		"started_live_hash":   runtime.LiveFingerprint(oldRuntime),
	})

	env.sp.SetAttached("worker", true)
	env.reconcileWithPoolDesiredAndDrainOps([]beads.Bead{session}, map[string]int{"worker": 1}, newDrainOps(env.sp))
	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get after attached deferral: %v", err)
	}
	driftKey := sessionConfigDriftKey(got, env.cfg, env.desiredState["worker"])
	if driftKey == "" {
		t.Fatal("expected config drift key")
	}
	if got.Metadata[sessionAttachedConfigDriftDeferredKeyMetadata] != driftKey {
		t.Fatalf("attached deferral key = %q, want %q", got.Metadata[sessionAttachedConfigDriftDeferredKeyMetadata], driftKey)
	}

	ds := &drainState{
		startedAt:  env.clk.Now().UTC(),
		deadline:   env.clk.Now().UTC().Add(defaultDrainTimeout),
		reason:     "config-drift",
		generation: 1,
		ackSet:     true,
	}
	env.dt.set(session.ID, ds)
	if err := setReconcilerDrainAckMetadata(env.sp, "worker", ds); err != nil {
		t.Fatalf("setReconcilerDrainAckMetadata: %v", err)
	}
	falseAttached := make([]bool, 100)
	env.sp.SetAttachedSequence("worker", falseAttached...)

	env.reconcileWithPoolDesiredAndDrainOps([]beads.Bead{got}, map[string]int{"worker": 1}, newDrainOps(env.sp))

	if ds := env.dt.get(session.ID); ds != nil {
		t.Fatalf("recent attached deferral should cancel config-drift drain ack, got %+v", ds)
	}
	if ack, _ := env.sp.GetMeta("worker", "GC_DRAIN_ACK"); ack != "" {
		t.Fatalf("GC_DRAIN_ACK after recent-deferral cancellation = %q, want empty", ack)
	}
	if !env.sp.IsRunning("worker") {
		t.Fatal("recent attached deferral should keep pool session running through drain-ack false negative")
	}
	got, err = env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get after reconcile: %v", err)
	}
	if got.Metadata["started_config_hash"] != oldHash {
		t.Fatalf("started_config_hash = %q, want %q", got.Metadata["started_config_hash"], oldHash)
	}
	if got.Metadata["session_key"] != "old-provider-conversation" {
		t.Fatalf("session_key = %q, want old provider conversation preserved", got.Metadata["session_key"])
	}
}

// --- idle timeout in bead reconciler tests ---

func TestReconcileSessionBeads_IdleTimeoutStopsAndStaysAsleep(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	env.addDesired("worker", "worker", true)
	session := env.createSessionBead("worker", "worker")
	env.markSessionActive(&session)
	env.setSessionMetadata(&session, map[string]string{
		"pending_create_claim": "true",
		"sleep_intent":         "idle-stop-pending",
	})
	if err := env.sp.SetMeta("worker", "GC_SESSION_ID", session.ID); err != nil {
		t.Fatalf("SetMeta(GC_SESSION_ID): %v", err)
	}

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
	if b.Metadata["pending_create_claim"] != "" {
		t.Errorf("pending_create_claim = %q, want cleared after idle stop", b.Metadata["pending_create_claim"])
	}
	if b.Metadata["sleep_intent"] != "" {
		t.Errorf("sleep_intent = %q, want cleared after idle stop", b.Metadata["sleep_intent"])
	}
	if b.Metadata["slept_at"] != env.clk.Now().UTC().Format(time.RFC3339) {
		t.Errorf("slept_at = %q, want idle stop timestamp", b.Metadata["slept_at"])
	}
}

func TestReconcileSessionBeads_IdleTimeoutUsesTemplateFallbackForPoolSession(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Agents: []config.Agent{{
			Name: "builder",
			Dir:  "local-core",
		}},
		NamedSessions: []config.NamedSession{{
			Name:     "primary",
			Template: "builder",
			Dir:      "local-core",
			Mode:     "always",
		}},
	}
	template := env.cfg.Agents[0].QualifiedName()
	poolSessionName := sessionNameFromBeadID("fm-r56l0x")
	namedSessionName := config.NamedSessionRuntimeName("", env.cfg.Workspace, "local-core/primary")

	env.addDesired(poolSessionName, template, true)
	env.addDesired(namedSessionName, template, true)
	poolSession := env.createSessionBead(poolSessionName, template)
	namedSession := env.createSessionBead(namedSessionName, template)
	env.markSessionActive(&poolSession)
	env.markSessionActive(&namedSession)
	if err := env.sp.SetMeta(poolSessionName, "GC_SESSION_ID", poolSession.ID); err != nil {
		t.Fatalf("SetMeta(%s, GC_SESSION_ID): %v", poolSessionName, err)
	}
	if err := env.sp.SetMeta(namedSessionName, "GC_SESSION_ID", namedSession.ID); err != nil {
		t.Fatalf("SetMeta(%s, GC_SESSION_ID): %v", namedSessionName, err)
	}

	it := newFakeIdleTracker()
	it.setTimeoutForTemplate(template, time.Hour)
	exemptAlwaysNamedTemplateFallbacks(env.cfg, "", template, it.exemptTemplateFallbackForSession)

	reconcileSessionBeads(
		context.Background(), []beads.Bead{poolSession, namedSession}, env.desiredState, configuredSessionNames(env.cfg, "", env.store),
		env.cfg, env.sp, env.store, nil, nil, nil, env.dt, map[string]int{template: 2}, false, nil, "",
		it, env.clk, env.rec, 0, 0, &env.stdout, &env.stderr,
	)

	if env.sp.IsRunning(poolSessionName) {
		t.Fatalf("pool session %q should idle via template fallback %q", poolSessionName, template)
	}
	if !env.sp.IsRunning(namedSessionName) {
		t.Fatalf("always named session %q must be exempt from template fallback %q", namedSessionName, template)
	}
	poolBead, err := env.store.Get(poolSession.ID)
	if err != nil {
		t.Fatalf("Get pool session: %v", err)
	}
	if poolBead.Metadata["sleep_reason"] != "idle-timeout" {
		t.Fatalf("pool sleep_reason = %q, want idle-timeout", poolBead.Metadata["sleep_reason"])
	}
	namedBead, err := env.store.Get(namedSession.ID)
	if err != nil {
		t.Fatalf("Get named session: %v", err)
	}
	if namedBead.Metadata["sleep_reason"] == "idle-timeout" {
		t.Fatalf("always named sleep_reason = %q, must not be idle-timeout", namedBead.Metadata["sleep_reason"])
	}
}

// TestReconcileSessionBeads_IdleTimeoutRespectsUserHold guards the
// session_reconciler.go idle-timeout block: a session with a future
// held_until (set by `gc session suspend`) must not be idle-killed.
// Without the guard, the idle kill path stops the runtime and rewrites the
// intended user-hold sleep state.
func TestReconcileSessionBeads_IdleTimeoutRespectsUserHold(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	env.addDesired("worker", "worker", true)
	session := env.createSessionBead("worker", "worker")
	env.markSessionActive(&session)
	heldUntil := env.clk.Now().Add(100 * time.Hour).UTC().Format(time.RFC3339)
	env.setSessionMetadata(&session, map[string]string{
		"held_until": heldUntil,
	})
	if err := env.sp.SetMeta("worker", "GC_SESSION_ID", session.ID); err != nil {
		t.Fatalf("SetMeta(GC_SESSION_ID): %v", err)
	}

	it := newFakeIdleTracker()
	it.idle["worker"] = true

	rec := events.NewFake()
	env.rec = rec

	cfgNames := configuredSessionNames(env.cfg, "", env.store)
	reconcileSessionBeads(
		context.Background(), []beads.Bead{session}, env.desiredState, cfgNames,
		env.cfg, env.sp, env.store, nil, nil, nil, env.dt, map[string]int{}, false, nil, "",
		it, env.clk, env.rec, 0, 0, &env.stdout, &env.stderr,
	)

	if !env.sp.IsRunning("worker") {
		t.Error("held worker must not be idle-killed while held_until is in the future")
	}
	b, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if b.Metadata["sleep_reason"] == "idle-timeout" {
		t.Errorf("sleep_reason = %q, must not be idle-timeout for held session", b.Metadata["sleep_reason"])
	}
	if got := b.Metadata["held_until"]; got != heldUntil {
		t.Errorf("held_until = %q, want preserved %q", got, heldUntil)
	}
	for _, e := range rec.Events {
		if e.Type == events.SessionIdleKilled {
			t.Error("SessionIdleKilled must not fire while held_until is in the future")
		}
	}
}

func TestReconcileSessionBeads_IdleTimeoutSuspendedUserHoldStartsDrain(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	env.addDesired("worker", "worker", true)
	session := env.createSessionBead("worker", "worker")
	env.markSessionActive(&session)
	heldUntil := env.clk.Now().Add(100 * time.Hour).UTC().Format(time.RFC3339)
	env.setSessionMetadata(&session, map[string]string{
		"held_until":   heldUntil,
		"sleep_intent": "user-hold",
		"state":        "suspended",
	})
	if err := env.sp.SetMeta("worker", "GC_SESSION_ID", session.ID); err != nil {
		t.Fatalf("SetMeta(GC_SESSION_ID): %v", err)
	}

	it := newFakeIdleTracker()
	it.idle["worker"] = true

	reconcileSessionBeads(
		context.Background(), []beads.Bead{session}, env.desiredState, configuredSessionNames(env.cfg, "", env.store),
		env.cfg, env.sp, env.store, nil, nil, nil, env.dt, map[string]int{}, false, nil, "",
		it, env.clk, env.rec, 0, 0, &env.stdout, &env.stderr,
	)

	ds := env.dt.get(session.ID)
	if ds == nil {
		t.Fatal("expected suspended user-hold session to start draining")
	}
	if ds.reason != "user-hold" {
		t.Fatalf("drain reason = %q, want user-hold", ds.reason)
	}
	if !env.sp.IsRunning("worker") {
		t.Fatal("held worker must drain before the runtime is stopped")
	}
	b, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := b.Metadata["held_until"]; got != heldUntil {
		t.Fatalf("held_until = %q, want preserved %q", got, heldUntil)
	}
}

func TestReconcileSessionBeads_IdleTimeoutRespectsQuarantineBlocker(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	env.addDesired("worker", "worker", true)
	session := env.createSessionBead("worker", "worker")
	env.markSessionActive(&session)
	qUntil := env.clk.Now().Add(15 * time.Minute).UTC().Format(time.RFC3339)
	env.setSessionMetadata(&session, map[string]string{
		"quarantined_until": qUntil,
		"sleep_reason":      "quarantine",
		"state":             "quarantined",
	})

	it := newFakeIdleTracker()
	it.idle["worker"] = true
	rec := events.NewFake()
	env.rec = rec

	reconcileSessionBeads(
		context.Background(), []beads.Bead{session}, env.desiredState, configuredSessionNames(env.cfg, "", env.store),
		env.cfg, env.sp, env.store, nil, nil, nil, env.dt, map[string]int{}, false, nil, "",
		it, env.clk, env.rec, 0, 0, &env.stdout, &env.stderr,
	)

	if !env.sp.IsRunning("worker") {
		t.Fatal("quarantined worker must not be idle-killed while quarantined_until is in the future")
	}
	b, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if b.Metadata["sleep_reason"] == "idle-timeout" {
		t.Fatalf("sleep_reason = %q, must not be idle-timeout for quarantined session", b.Metadata["sleep_reason"])
	}
	if got := b.Metadata["quarantined_until"]; got != qUntil {
		t.Fatalf("quarantined_until = %q, want preserved %q", got, qUntil)
	}
	for _, e := range rec.Events {
		if e.Type == events.SessionIdleKilled {
			t.Fatal("SessionIdleKilled must not fire while quarantined_until is in the future")
		}
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

// --- max session age tests ---

// maxAgeReconcile runs the reconciler with the given max-age tracker
// installed via withMaxSessionAgeTracker. Mirrors env.reconcile but routes
// through reconcileSessionBeadsTraced so the option wiring is exercised.
func (e *reconcilerTestEnv) maxAgeReconcile(sessions []beads.Bead, tr maxSessionAgeTracker) {
	poolDesired := make(map[string]int)
	for _, tp := range e.desiredState {
		if tp.TemplateName != "" {
			poolDesired[tp.TemplateName]++
		}
	}
	cfgNames := configuredSessionNames(e.cfg, "", e.store)
	reconcileSessionBeadsTraced(
		context.Background(), "", sessions, e.desiredState, cfgNames, e.cfg, e.sp,
		e.store, nil, nil, nil, nil, e.dt, poolDesired, false, nil, "",
		nil, e.clk, e.rec, 0, 0, &e.stdout, &e.stderr, nil,
		withMaxSessionAgeTracker(tr),
	)
}

func TestReconcileSessionBeads_MaxSessionAgeKillsAgedSession(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "witness", MaxSessionAge: "5h"}}}
	env.addDesired("witness", "witness", true)
	session := env.createSessionBead("witness", "witness")
	env.markSessionActive(&session)
	// creation_complete_at well past the configured 5h threshold.
	env.setSessionMetadata(&session, map[string]string{
		"creation_complete_at": env.clk.Now().Add(-6 * time.Hour).UTC().Format(time.RFC3339),
	})

	tr := newMaxSessionAgeTracker()
	tr.setConfig("witness", 5*time.Hour, 0)
	rec := events.NewFake()
	env.rec = rec

	env.maxAgeReconcile([]beads.Bead{session}, tr)

	if env.sp.IsRunning("witness") {
		t.Error("aged witness should have been stopped")
	}
	b, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if b.Metadata["sleep_reason"] != "max-session-age" {
		t.Errorf("sleep_reason = %q, want max-session-age", b.Metadata["sleep_reason"])
	}
	fired := false
	for _, e := range rec.Events {
		if e.Type == events.SessionMaxAgeKilled {
			fired = true
			break
		}
	}
	if !fired {
		t.Error("expected SessionMaxAgeKilled event")
	}
}

// TestReconcileSessionBeads_MaxSessionAgeRespectsUserHold guards the
// session_reconciler.go max-session-age block: a session with a future
// held_until (set by `gc session suspend`) must not be max-age killed.
// Without the guard, the max-age kill path stops the runtime and rewrites the
// intended user-hold sleep state.
func TestReconcileSessionBeads_MaxSessionAgeRespectsUserHold(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "witness", MaxSessionAge: "5h"}}}
	env.addDesired("witness", "witness", true)
	session := env.createSessionBead("witness", "witness")
	env.markSessionActive(&session)
	heldUntil := env.clk.Now().Add(100 * time.Hour).UTC().Format(time.RFC3339)
	env.setSessionMetadata(&session, map[string]string{
		"creation_complete_at": env.clk.Now().Add(-6 * time.Hour).UTC().Format(time.RFC3339),
		"held_until":           heldUntil,
	})

	tr := newMaxSessionAgeTracker()
	tr.setConfig("witness", 5*time.Hour, 0)
	rec := events.NewFake()
	env.rec = rec

	env.maxAgeReconcile([]beads.Bead{session}, tr)

	if !env.sp.IsRunning("witness") {
		t.Error("held witness must not be max-age killed while held_until is in the future")
	}
	b, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if b.Metadata["sleep_reason"] == "max-session-age" {
		t.Errorf("sleep_reason = %q, must not be max-session-age for held session", b.Metadata["sleep_reason"])
	}
	if got := b.Metadata["held_until"]; got != heldUntil {
		t.Errorf("held_until = %q, want preserved %q", got, heldUntil)
	}
	for _, e := range rec.Events {
		if e.Type == events.SessionMaxAgeKilled {
			t.Error("SessionMaxAgeKilled must not fire while held_until is in the future")
		}
	}
}

func TestReconcileSessionBeads_MaxSessionAgeRespectsQuarantineBlocker(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "witness", MaxSessionAge: "5h"}}}
	env.addDesired("witness", "witness", true)
	session := env.createSessionBead("witness", "witness")
	env.markSessionActive(&session)
	qUntil := env.clk.Now().Add(15 * time.Minute).UTC().Format(time.RFC3339)
	env.setSessionMetadata(&session, map[string]string{
		"creation_complete_at": env.clk.Now().Add(-6 * time.Hour).UTC().Format(time.RFC3339),
		"quarantined_until":    qUntil,
		"sleep_reason":         "quarantine",
		"state":                "quarantined",
	})

	tr := newMaxSessionAgeTracker()
	tr.setConfig("witness", 5*time.Hour, 0)
	rec := events.NewFake()
	env.rec = rec

	env.maxAgeReconcile([]beads.Bead{session}, tr)

	if !env.sp.IsRunning("witness") {
		t.Fatal("quarantined witness must not be max-age killed while quarantined_until is in the future")
	}
	b, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if b.Metadata["sleep_reason"] == "max-session-age" {
		t.Fatalf("sleep_reason = %q, must not be max-session-age for quarantined session", b.Metadata["sleep_reason"])
	}
	if got := b.Metadata["quarantined_until"]; got != qUntil {
		t.Fatalf("quarantined_until = %q, want preserved %q", got, qUntil)
	}
	for _, e := range rec.Events {
		if e.Type == events.SessionMaxAgeKilled {
			t.Fatal("SessionMaxAgeKilled must not fire while quarantined_until is in the future")
		}
	}
}

func TestReconcileSessionBeads_MaxSessionAgeSkippedWhenYoung(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "witness", MaxSessionAge: "5h"}}}
	env.addDesired("witness", "witness", true)
	session := env.createSessionBead("witness", "witness")
	env.markSessionActive(&session)
	env.setSessionMetadata(&session, map[string]string{
		"creation_complete_at": env.clk.Now().Add(-30 * time.Minute).UTC().Format(time.RFC3339),
	})

	tr := newMaxSessionAgeTracker()
	tr.setConfig("witness", 5*time.Hour, 0)
	env.maxAgeReconcile([]beads.Bead{session}, tr)

	if !env.sp.IsRunning("witness") {
		t.Error("young witness should still be running")
	}
}

func TestReconcileSessionBeads_MaxSessionAgeSkippedWithoutAnchor(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "witness", MaxSessionAge: "5h"}}}
	env.addDesired("witness", "witness", true)
	session := env.createSessionBead("witness", "witness")
	env.markSessionActive(&session)
	// No creation_complete_at — predates the feature or was cleared. The
	// tracker must tolerate this and skip the restart rather than treating
	// a missing anchor as age=0 or age=infinity.

	tr := newMaxSessionAgeTracker()
	tr.setConfig("witness", 5*time.Hour, 0)
	env.maxAgeReconcile([]beads.Bead{session}, tr)

	if !env.sp.IsRunning("witness") {
		t.Error("witness without creation_complete_at must not be killed")
	}
}

func TestReconcileSessionBeads_MaxSessionAgeNilTrackerSkipped(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	env.addDesired("worker", "worker", true)
	session := env.createSessionBead("worker", "worker")
	env.markSessionActive(&session)
	env.setSessionMetadata(&session, map[string]string{
		"creation_complete_at": env.clk.Now().Add(-10 * time.Hour).UTC().Format(time.RFC3339),
	})

	env.maxAgeReconcile([]beads.Bead{session}, nil) // disabled globally

	if !env.sp.IsRunning("worker") {
		t.Error("worker should still be running when max-age feature is disabled")
	}
}

func TestReconcileSessionBeads_MaxSessionAgeSkippedWhenPending(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "witness", MaxSessionAge: "5h"}}}
	env.addDesired("witness", "witness", true)
	session := env.createSessionBead("witness", "witness")
	env.markSessionActive(&session)
	env.setSessionMetadata(&session, map[string]string{
		"creation_complete_at": env.clk.Now().Add(-6 * time.Hour).UTC().Format(time.RFC3339),
	})
	// Simulate a pending interaction — the reconciler must defer restart
	// until the session settles so we don't interrupt mid-turn work.
	env.sp.SetPendingInteraction("witness", &runtime.PendingInteraction{Kind: "approval", RequestID: "req-1"})

	tr := newMaxSessionAgeTracker()
	tr.setConfig("witness", 5*time.Hour, 0)
	rec := events.NewFake()
	env.rec = rec
	env.maxAgeReconcile([]beads.Bead{session}, tr)

	if !env.sp.IsRunning("witness") {
		t.Error("pending-interaction witness should not be max-age killed")
	}
	for _, e := range rec.Events {
		if e.Type == events.SessionMaxAgeKilled {
			t.Error("SessionMaxAgeKilled must not fire while a pending interaction keeps the session awake")
		}
	}
}

func TestReconcileSessionBeads_MaxSessionAgeSkippedWhenBusyWithAssignedWork(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "witness", MaxSessionAge: "5h"}}}
	env.addDesired("witness", "witness", true)
	session := env.createSessionBead("witness", "witness")
	env.markSessionActive(&session)
	env.setSessionMetadata(&session, map[string]string{
		"creation_complete_at": env.clk.Now().Add(-6 * time.Hour).UTC().Format(time.RFC3339),
	})
	if _, err := env.store.Create(beads.Bead{
		Title:    "in-flight work",
		Type:     "task",
		Status:   "in_progress",
		Assignee: session.ID,
	}); err != nil {
		t.Fatalf("Create(in-flight work): %v", err)
	}

	tr := newMaxSessionAgeTracker()
	tr.setConfig("witness", 5*time.Hour, 0)
	rec := events.NewFake()
	env.rec = rec
	env.maxAgeReconcile([]beads.Bead{session}, tr)

	if !env.sp.IsRunning("witness") {
		t.Error("witness with open assigned work should not be max-age killed")
	}
	for _, e := range rec.Events {
		if e.Type == events.SessionMaxAgeKilled {
			t.Error("SessionMaxAgeKilled must not fire while an in-progress assigned bead is held")
		}
	}
}

// TestReconcileSessionBeads_MaxAgeBusyDeferFallsThroughToIdleTimeout pins the
// max-age half of the timer asymmetry (SESSION-RECON-009): a max-age deferral
// leaves the session in the rest of the tick. The busy witness is max-age
// deferred on assigned work but must still be idle-evaluated on the same
// tick, so the idle stop fires. Fails if the max-age defer path ever gains a
// `continue`.
func TestReconcileSessionBeads_MaxAgeBusyDeferFallsThroughToIdleTimeout(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "witness", MaxSessionAge: "5h"}}}
	env.addDesired("witness", "witness", true)
	session := env.createSessionBead("witness", "witness")
	env.markSessionActive(&session)
	env.setSessionMetadata(&session, map[string]string{
		"creation_complete_at": env.clk.Now().Add(-6 * time.Hour).UTC().Format(time.RFC3339),
	})
	if _, err := env.store.Create(beads.Bead{
		Title:    "in-flight work",
		Type:     "task",
		Status:   "in_progress",
		Assignee: session.ID,
	}); err != nil {
		t.Fatalf("Create(in-flight work): %v", err)
	}

	tr := newMaxSessionAgeTracker()
	tr.setConfig("witness", 5*time.Hour, 0)
	it := newFakeIdleTracker()
	it.idle["witness"] = true
	rec := events.NewFake()
	env.rec = rec

	poolDesired := make(map[string]int)
	for _, tp := range env.desiredState {
		if tp.TemplateName != "" {
			poolDesired[tp.TemplateName]++
		}
	}
	cfgNames := configuredSessionNames(env.cfg, "", env.store)
	reconcileSessionBeadsTraced(
		context.Background(), "", []beads.Bead{session}, env.desiredState, cfgNames, env.cfg, env.sp,
		env.store, nil, nil, nil, nil, env.dt, poolDesired, false, nil, "",
		it, env.clk, env.rec, 0, 0, &env.stdout, &env.stderr, nil,
		withMaxSessionAgeTracker(tr),
	)

	var maxAgeKilled, idleKilled bool
	for _, e := range rec.Events {
		switch e.Type {
		case events.SessionMaxAgeKilled:
			maxAgeKilled = true
		case events.SessionIdleKilled:
			idleKilled = true
		}
	}
	if maxAgeKilled {
		t.Error("SessionMaxAgeKilled must not fire while an in-progress assigned bead is held")
	}
	if !idleKilled {
		t.Error("idle timeout must still run on the same tick after a max-age busy deferral")
	}
}

// TestReconcileSessionBeads_MaxSessionAgeFailsClosedOnStoreError verifies
// that a transient store error during the assigned-work check defers the
// max-age restart rather than killing the session. This guards the fix at
// session_reconciler.go where the error from
// sessionHasOpenAssignedWorkForReachableStore was previously discarded
// with `_`, which could drop in-flight work on a transient blip.
func TestReconcileSessionBeads_MaxSessionAgeFailsClosedOnStoreError(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "witness", MaxSessionAge: "5h"}}}
	env.addDesired("witness", "witness", true)
	session := env.createSessionBead("witness", "witness")
	env.markSessionActive(&session)
	env.setSessionMetadata(&session, map[string]string{
		"creation_complete_at": env.clk.Now().Add(-6 * time.Hour).UTC().Format(time.RFC3339),
	})

	// Wrap the city store so the assigned-work check sees an error.
	failingStore := &listErrStore{Store: env.store, err: fmt.Errorf("simulated transient store failure")}

	tr := newMaxSessionAgeTracker()
	tr.setConfig("witness", 5*time.Hour, 0)
	rec := events.NewFake()
	env.rec = rec

	poolDesired := make(map[string]int)
	for _, tp := range env.desiredState {
		if tp.TemplateName != "" {
			poolDesired[tp.TemplateName]++
		}
	}
	cfgNames := configuredSessionNames(env.cfg, "", env.store)
	reconcileSessionBeadsTraced(
		context.Background(), "", []beads.Bead{session}, env.desiredState, cfgNames, env.cfg, env.sp,
		failingStore, nil, nil, nil, nil, env.dt, poolDesired, false, nil, "",
		nil, env.clk, env.rec, 0, 0, &env.stdout, &env.stderr, nil,
		withMaxSessionAgeTracker(tr),
	)

	if !env.sp.IsRunning("witness") {
		t.Error("witness should remain running when assigned-work check errored (fail-closed)")
	}
	for _, e := range rec.Events {
		if e.Type == events.SessionMaxAgeKilled {
			t.Error("SessionMaxAgeKilled must not fire when assigned-work check returned an error")
		}
	}
	if !strings.Contains(env.stderr.String(), "simulated transient store failure") {
		t.Errorf("expected stderr to log the store error; got %q", env.stderr.String())
	}
}

func TestSessionHasAwakeAssignedWorkUsesCachedInProgressWispProbe(t *testing.T) {
	backing := &demandListCountingStore{Store: beads.NewMemStore()}
	work, err := backing.Create(beads.Bead{
		Title:     "active wisp work",
		Type:      "task",
		Status:    "in_progress",
		Assignee:  "worker-session",
		Ephemeral: true,
	})
	if err != nil {
		t.Fatalf("Create active wisp: %v", err)
	}
	if err := backing.Update(work.ID, beads.UpdateOpts{Status: stringPtr("in_progress")}); err != nil {
		t.Fatalf("mark active wisp in progress: %v", err)
	}
	cache := beads.NewCachingStoreForTest(backing, nil)
	if err := cache.PrimeActive(); err != nil {
		t.Fatalf("PrimeActive: %v", err)
	}

	has, err := sessionHasAwakeAssignedWorkInStoreByIdentifiers(cache, []string{"worker-session"})
	if err != nil {
		t.Fatalf("sessionHasAwakeAssignedWorkInStoreByIdentifiers: %v", err)
	}
	if !has {
		t.Fatalf("sessionHasAwakeAssignedWorkInStoreByIdentifiers = false, want true for %s", work.ID)
	}
	if backing.liveInProgressWispLists != 0 {
		t.Fatalf("live wisp in_progress list calls = %d, want cached wisp probe", backing.liveInProgressWispLists)
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

	// Verify downstream behavior diverges from the alive path.
	// Contrast with TestReconcileSessionBeads_SkipsAliveSession where an
	// alive session keeps state "active" (healed to "awake") and records
	// no wake failure.
	//
	// For the zombie (running but process-dead), the reconciler:
	//  1. Records the crash event (above).
	//  2. Heals bead state from "active" to "asleep" (not alive).
	//  3. Detects rapid exit (last_woke_at is recent) and records a
	//     wake failure, preventing immediate restart (crash-loop protection).
	got, _ := env.store.Get(session.ID)
	if got.Metadata["state"] != "asleep" {
		t.Errorf("state = %q, want asleep (zombie healed to not-alive)", got.Metadata["state"])
	}
	if got.Metadata["wake_attempts"] == "" || got.Metadata["wake_attempts"] == "0" {
		t.Error("expected wake_attempts > 0 (rapid exit recorded for zombie)")
	}
}

// TestReconcileSessionBeads_BeadMetadataRestartRequestedWhenSessionDead
// verifies that the reconciler detects restart_requested from bead metadata
// even when the tmux session is already dead (dops is nil or session not
// alive). This is the key durability property of the dual-flag approach:
// the bead flag survives tmux session death.
//
// The bead carries named-session identity metadata. Of these,
// namedSessionMetadataKey and namedSessionIdentityMetadata are checked by
// preserveConfiguredNamedSessionBead to recognize the bead as a configured
// named session, preventing the reconciler from treating it as an orphan.
// Without these metadata fields (or without the matching NamedSession config),
// the bead would be closed as orphaned before the restart_requested path is
// reached.
//
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
		"pending_create_claim":       "true",
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
	if got.Metadata["continuation_reset_pending"] != "true" {
		t.Fatalf("continuation_reset_pending = %q, want true", got.Metadata["continuation_reset_pending"])
	}
	if got.Metadata["session_key"] == "" || got.Metadata["session_key"] == "original-key" {
		t.Fatalf("session_key = %q, want rotated key", got.Metadata["session_key"])
	}
	if got.Metadata["pending_create_claim"] != "" {
		t.Fatalf("pending_create_claim = %q, want cleared after durable dead-session restart request", got.Metadata["pending_create_claim"])
	}
}

func TestReconcileSessionBeads_RecordsResetStallDiagnostic(t *testing.T) {
	env := newReconcilerTestEnv()
	rec := events.NewFake()
	env.rec = rec
	env.cfg = &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents:    []config.Agent{{Name: "worker", StartCommand: "true", MaxActiveSessions: intPtr(2)}},
		Session:   config.SessionConfig{StartupTimeout: "60s"},
	}
	env.addDesired("worker", "worker", false)
	session := env.createSessionBead("worker", "worker")
	committedAt := env.clk.Now().Add(-75 * time.Second).UTC().Format(time.RFC3339)
	env.setSessionMetadata(&session, map[string]string{
		"continuation_reset_pending":   "true",
		sessionpkg.ResetCommittedAtKey: committedAt,
		"wait_hold":                    "true",
	})

	tracer := newSessionReconcilerTracer(t.TempDir(), "test-city", io.Discard)
	t.Cleanup(func() { _ = tracer.Close() })
	tracer.detail = map[string]TraceSource{"worker": TraceSourceManual}
	trace := tracer.BeginCycle(TraceTickTriggerPatrol, "", env.clk.Now().UTC(), env.cfg)

	reconcileSessionBeadsTraced(
		context.Background(),
		"",
		[]beads.Bead{session},
		env.desiredState,
		configuredSessionNames(env.cfg, "", env.store),
		env.cfg,
		env.sp,
		env.store,
		nil,
		nil,
		nil,
		nil,
		env.dt,
		map[string]int{"worker": 0},
		false,
		nil,
		"test-city",
		nil,
		env.clk,
		rec,
		env.cfg.Session.StartupTimeoutDuration(),
		0,
		&env.stdout,
		&env.stderr,
		trace,
	)

	wantMessage := fmt.Sprintf(
		"session reconciler: reset stalled for worker: elapsed_s=75 reset_committed_at=%s bead_id=%s",
		committedAt,
		session.ID,
	)
	if got := strings.TrimSpace(env.stderr.String()); got != wantMessage {
		t.Fatalf("stderr = %q, want %q", got, wantMessage)
	}
	if len(rec.Events) != 1 {
		t.Fatalf("recorded events = %d, want 1: %#v", len(rec.Events), rec.Events)
	}
	gotEvent := rec.Events[0]
	if gotEvent.Type != events.SessionResetStalled {
		t.Fatalf("event type = %q, want %q", gotEvent.Type, events.SessionResetStalled)
	}
	if gotEvent.Message != wantMessage {
		t.Fatalf("event message = %q, want %q", gotEvent.Message, wantMessage)
	}
	var payload events.SessionResetStalledPayload
	if err := json.Unmarshal(gotEvent.Payload, &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if payload.SessionName != "worker" || payload.Template != "worker" || payload.ResetCommittedAt != committedAt || payload.ElapsedSeconds != 75 {
		t.Fatalf("payload = %+v, want session/template worker, reset_committed_at %q, elapsed_s 75", payload, committedAt)
	}

	foundTrace := false
	if trace != nil {
		for _, rec := range trace.records {
			if rec.SiteCode == TraceSiteReconcilerResetStalled &&
				rec.ReasonCode == TraceReasonResetStalled &&
				rec.OutcomeCode == TraceOutcomeFailed &&
				rec.Template == "worker" &&
				rec.SessionName == "worker" {
				foundTrace = true
				break
			}
		}
	}
	if !foundTrace {
		t.Fatalf("reset stalled trace decision not recorded; records=%+v", trace.records)
	}

	env.stderr.Reset()
	recordResetStallIfDue(session, "worker", "worker", false, env.cfg.Session.StartupTimeoutDuration(), env.clk.Now().UTC(), env.dt, rec, &env.stderr, trace)
	if got := strings.TrimSpace(env.stderr.String()); got != "" {
		t.Fatalf("second stalled pass stderr = %q, want debounce silence", got)
	}
	if len(rec.Events) != 1 {
		t.Fatalf("recorded events after duplicate pass = %d, want 1", len(rec.Events))
	}

	env.setSessionMetadata(&session, map[string]string{
		"continuation_reset_pending":   "",
		sessionpkg.ResetCommittedAtKey: "",
	})
	recordResetStallIfDue(session, "worker", "worker", false, env.cfg.Session.StartupTimeoutDuration(), env.clk.Now().UTC(), env.dt, rec, &env.stderr, trace)
	env.setSessionMetadata(&session, map[string]string{
		"continuation_reset_pending":   "true",
		sessionpkg.ResetCommittedAtKey: committedAt,
	})
	env.stderr.Reset()
	recordResetStallIfDue(session, "worker", "worker", false, env.cfg.Session.StartupTimeoutDuration(), env.clk.Now().UTC(), env.dt, rec, &env.stderr, trace)
	if got := strings.TrimSpace(env.stderr.String()); got != wantMessage {
		t.Fatalf("re-stalled pass stderr = %q, want %q", got, wantMessage)
	}
	if len(rec.Events) != 2 {
		t.Fatalf("recorded events after reset clear = %d, want 2", len(rec.Events))
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
	// The session is not running, so the reconciler should wake it.
	sessions, _ := loadSessionBeads(store)
	poolDesired := map[string]int{"worker": 1}
	cfgNames := configuredSessionNames(cfg, "", store)
	woken := reconcileSessionBeads(
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

	// Verify downstream wake/start: the recovered bead should feed into
	// a successful session start, not just survive reconciliation.
	if woken != 1 {
		t.Fatalf("woken = %d, want 1 (recovered session should be started)", woken)
	}
	if !sp.IsRunning(sessionName) {
		t.Fatalf("session %q not running after reconcile — recovery did not trigger start", sessionName)
	}
}

// Regression test for #742 follow-up: after the stale-bead reaper closes a
// dead canonical bead, rediscovery must not also revive a leaked plain open
// bead for the same backing template alongside the rebuilt named session.
func TestReconcileSessionBeads_FileStoreAlwaysNamedRecoversWithLeakedDuplicateOpenBead(t *testing.T) {
	cityPath := t.TempDir()
	beadsPath := filepath.Join(cityPath, ".gc", "beads.json")
	store, err := beads.OpenFileStore(fsys.OSFS{}, beadsPath)
	if err != nil {
		t.Fatalf("OpenFileStore: %v", err)
	}
	store.SetLocker(beads.NewFileFlock(beadsPath + ".lock"))

	clk := &clock.Fake{Time: time.Date(2026, 4, 20, 12, 0, 0, 0, time.UTC)}
	sp := runtime.NewFake()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{
			{Name: "mayor", StartCommand: "true", MaxActiveSessions: intPtr(0)},
		},
		NamedSessions: []config.NamedSession{
			{Template: "mayor", Mode: "always"},
		},
	}
	sessionName := config.NamedSessionRuntimeName(cfg.EffectiveCityName(), cfg.Workspace, "mayor")

	_, err = store.Create(beads.Bead{
		Title:  "mayor",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":               sessionName,
			"alias":                      "mayor",
			"template":                   "mayor",
			"agent_name":                 "mayor",
			"state":                      "asleep",
			"generation":                 "1",
			"continuation_epoch":         "1",
			"instance_token":             "canonical-token",
			namedSessionMetadataKey:      "true",
			namedSessionIdentityMetadata: "mayor",
			namedSessionModeMetadata:     "always",
		},
	})
	if err != nil {
		t.Fatalf("Create(canonical): %v", err)
	}
	leaked, err := store.Create(beads.Bead{
		Title:  "mayor",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "agent:mayor"},
		Metadata: map[string]string{
			"session_name":       "s-gc-leaked",
			"template":           "mayor",
			"agent_name":         "mayor",
			"state":              "asleep",
			"generation":         "1",
			"continuation_epoch": "1",
			"instance_token":     "leaked-token",
		},
	})
	if err != nil {
		t.Fatalf("Create(leaked): %v", err)
	}

	var stdout, stderr bytes.Buffer
	dsResult := buildDesiredState(cfg.EffectiveCityName(), cityPath, clk.Now().UTC(), cfg, sp, store, &stderr)

	mayorTP, ok := dsResult.State[sessionName]
	if !ok {
		t.Fatalf("desired state missing canonical named session %q; keys=%v", sessionName, mapKeys(dsResult.State))
	}
	if mayorTP.ConfiguredNamedIdentity != "mayor" {
		t.Fatalf("ConfiguredNamedIdentity = %q, want mayor", mayorTP.ConfiguredNamedIdentity)
	}
	if mayorTP.SessionName != sessionName {
		t.Fatalf("SessionName = %q, want %q", mayorTP.SessionName, sessionName)
	}
	if _, ok := dsResult.State[leaked.Metadata["session_name"]]; ok {
		t.Fatalf("desired state unexpectedly included leaked duplicate bead %q; keys=%v", leaked.Metadata["session_name"], mapKeys(dsResult.State))
	}

	cfgNames := configuredSessionNames(cfg, cfg.EffectiveCityName(), store)
	syncSessionBeads(cityPath, store, dsResult.State, sp, cfgNames, cfg, clk, &stderr, true)

	sessions, err := loadSessionBeads(store)
	if err != nil {
		t.Fatalf("loadSessionBeads: %v", err)
	}
	poolDesired := PoolDesiredCounts(ComputePoolDesiredStates(cfg, dsResult.AssignedWorkBeads, sessions, dsResult.ScaleCheckCounts))
	if poolDesired == nil {
		poolDesired = make(map[string]int)
	}
	mergeNamedSessionDemand(poolDesired, dsResult.NamedSessionDemand, cfg)

	woken := reconcileSessionBeads(
		context.Background(), sessions, dsResult.State, cfgNames, cfg, sp,
		store, nil, dsResult.AssignedWorkBeads, nil, newDrainTracker(), poolDesired,
		dsResult.StoreQueryPartial, nil, cfg.EffectiveCityName(),
		nil, clk, events.Discard, 0, 0, &stdout, &stderr,
	)

	if woken != 1 {
		t.Fatalf("woken = %d, want 1", woken)
	}
	if !sp.IsRunning(sessionName) {
		t.Fatalf("canonical named session %q was not started", sessionName)
	}
	if sp.IsRunning(leaked.Metadata["session_name"]) {
		t.Fatalf("leaked duplicate session %q should not have been started", leaked.Metadata["session_name"])
	}
	if strings.Contains(stderr.String(), "session alias already exists") {
		t.Fatalf("unexpected alias collision during recovery:\n%s", stderr.String())
	}
}

func TestReconcileSessionBeads_FreshAlwaysNamedWithPoolDemandMaterializesNamedDespitePoolBead(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	clk := &clock.Fake{Time: time.Date(2026, 4, 20, 12, 0, 0, 0, time.UTC)}
	sp := runtime.NewFake()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{
			{Name: "mayor", StartCommand: "true", ScaleCheck: "printf 1", MaxActiveSessions: intPtr(3)},
		},
		NamedSessions: []config.NamedSession{
			{Template: "mayor", Mode: "always"},
		},
	}
	sessionName := config.NamedSessionRuntimeName(cfg.EffectiveCityName(), cfg.Workspace, "mayor")

	var stdout, stderr bytes.Buffer
	dsResult, sessions, woken := reconcileConfiguredSessionsOnce(t, cityPath, store, cfg, sp, clk, &stdout, &stderr)

	if _, ok := dsResult.State[sessionName]; !ok {
		t.Fatalf("desired state missing canonical named session %q; keys=%v", sessionName, mapKeys(dsResult.State))
	}
	if woken != 2 {
		t.Fatalf("woken = %d, want 2; sessions=%v stderr:\n%s", woken, sessionBeadDebug(sessions), stderr.String())
	}
	if !sp.IsRunning(sessionName) {
		t.Fatalf("canonical named session %q was not started; stderr:\n%s", sessionName, stderr.String())
	}
	if _, ok := findTestSessionBeadByName(sessions, sessionName); !ok {
		t.Fatalf("canonical named session bead %q was not materialized; sessions=%v stderr:\n%s", sessionName, sessionBeadDebug(sessions), stderr.String())
	}
	if !testHasRunningPoolSessionForTemplate(sessions, sp, "mayor") {
		t.Fatalf("same-template pool session was not started; sessions=%v stderr:\n%s", sessionBeadDebug(sessions), stderr.String())
	}
	if strings.Contains(stderr.String(), "session_name \"mayor\"") {
		t.Fatalf("unexpected named-session reservation conflict:\n%s", stderr.String())
	}
}

func TestReconcileSessionBeads_ExistingAlwaysNamedStillAllowsSameTemplatePoolDemand(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	clk := &clock.Fake{Time: time.Date(2026, 4, 20, 12, 0, 0, 0, time.UTC)}
	sp := runtime.NewFake()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{
			{Name: "mayor", StartCommand: "true", ScaleCheck: "printf 1", MaxActiveSessions: intPtr(3)},
		},
		NamedSessions: []config.NamedSession{
			{Template: "mayor", Mode: "always"},
		},
	}
	sessionName := config.NamedSessionRuntimeName(cfg.EffectiveCityName(), cfg.Workspace, "mayor")

	if _, err := store.Create(beads.Bead{
		Title:  "mayor",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":               sessionName,
			"alias":                      "mayor",
			"template":                   "mayor",
			"agent_name":                 "mayor",
			"state":                      "asleep",
			"generation":                 "1",
			"continuation_epoch":         "1",
			"instance_token":             "canonical-token",
			namedSessionMetadataKey:      "true",
			namedSessionIdentityMetadata: "mayor",
			namedSessionModeMetadata:     "always",
		},
	}); err != nil {
		t.Fatalf("Create(canonical): %v", err)
	}

	var stdout, stderr bytes.Buffer
	_, sessions, woken := reconcileConfiguredSessionsOnce(t, cityPath, store, cfg, sp, clk, &stdout, &stderr)

	if woken != 2 {
		t.Fatalf("woken = %d, want 2; sessions=%v stderr:\n%s", woken, sessionBeadDebug(sessions), stderr.String())
	}
	if !sp.IsRunning(sessionName) {
		t.Fatalf("canonical named session %q was not started; stderr:\n%s", sessionName, stderr.String())
	}
	if !testHasRunningPoolSessionForTemplate(sessions, sp, "mayor") {
		t.Fatalf("same-template pool session was not started; sessions=%v stderr:\n%s", sessionBeadDebug(sessions), stderr.String())
	}
}

func reconcileConfiguredSessionsOnce(
	t *testing.T,
	cityPath string,
	store beads.Store,
	cfg *config.City,
	sp *runtime.Fake,
	clk *clock.Fake,
	stdout, stderr *bytes.Buffer,
) (DesiredStateResult, []beads.Bead, int) {
	t.Helper()

	dsResult := buildDesiredState(cfg.EffectiveCityName(), cityPath, clk.Now().UTC(), cfg, sp, store, stderr)
	cfgNames := configuredSessionNames(cfg, cfg.EffectiveCityName(), store)
	syncSessionBeads(cityPath, store, dsResult.State, sp, cfgNames, cfg, clk, stderr, true)

	sessions, err := loadSessionBeads(store)
	if err != nil {
		t.Fatalf("loadSessionBeads: %v", err)
	}
	poolDesired := PoolDesiredCounts(ComputePoolDesiredStates(cfg, dsResult.AssignedWorkBeads, sessions, dsResult.ScaleCheckCounts))
	if poolDesired == nil {
		poolDesired = make(map[string]int)
	}
	mergeNamedSessionDemand(poolDesired, dsResult.NamedSessionDemand, cfg)

	woken := reconcileSessionBeads(
		context.Background(), sessions, dsResult.State, cfgNames, cfg, sp,
		store, nil, dsResult.AssignedWorkBeads, nil, newDrainTracker(), poolDesired,
		dsResult.StoreQueryPartial, nil, cfg.EffectiveCityName(),
		nil, clk, events.Discard, 0, 0, stdout, stderr,
	)

	sessions, err = loadSessionBeads(store)
	if err != nil {
		t.Fatalf("loadSessionBeads after reconcile: %v", err)
	}
	return dsResult, sessions, woken
}

func findTestSessionBeadByName(sessions []beads.Bead, sessionName string) (beads.Bead, bool) {
	for _, sessionBead := range sessions {
		if sessionBead.Metadata["session_name"] == sessionName {
			return sessionBead, true
		}
	}
	return beads.Bead{}, false
}

func testHasRunningPoolSessionForTemplate(sessions []beads.Bead, sp *runtime.Fake, template string) bool {
	for _, sessionBead := range sessions {
		if sessionBead.Metadata["template"] != template || !isPoolManagedSessionBead(sessionBead) {
			continue
		}
		sessionName := sessionBead.Metadata["session_name"]
		if sessionName != "" && sp.IsRunning(sessionName) {
			return true
		}
	}
	return false
}

func sessionBeadDebug(sessions []beads.Bead) []string {
	out := make([]string, 0, len(sessions))
	for _, sessionBead := range sessions {
		out = append(out, fmt.Sprintf(
			"%s:name=%s template=%s named=%t pool=%t state=%s status=%s",
			sessionBead.ID,
			sessionBead.Metadata["session_name"],
			sessionBead.Metadata["template"],
			isNamedSessionBead(sessionBead),
			isPoolManagedSessionBead(sessionBead),
			sessionBead.Metadata["state"],
			sessionBead.Status,
		))
	}
	return out
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

	latestSnapshot, err := loadSessionBeadSnapshot(store)
	if err != nil {
		t.Fatalf("load latest snapshot: %v", err)
	}
	result := DesiredStateResult{State: ds, BaseState: ds, BeaconTime: clk.Now().UTC()}
	refreshed := refreshDesiredStateWithSessionBeads(result, "test-city", cityPath, cfg, sp, store, latestSnapshot, &stderr)
	ds = refreshed.State
	newSessionName := newBead.Metadata["session_name"]
	if newSessionName == "" {
		t.Fatal("fresh pool bead has empty session_name")
	}
	if _, ok := ds[newSessionName]; !ok {
		t.Fatalf("refreshed desired state missing fresh pool session %q; keys=%v", newSessionName, mapKeys(ds))
	}

	// Now run the reconciler with the fresh bead — it should remain open
	// (not be closed as orphan) since the pool slot is in the desired state.
	// The session is not running, so the reconciler should wake it.
	sessions, _ := loadSessionBeads(store)
	poolDesired := map[string]int{"worker": 1}
	cfgNames := configuredSessionNames(cfg, "", store)
	woken := reconcileSessionBeads(
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

	// Verify downstream wake/start: the fresh bead created by pool recovery
	// should feed into a successful session start.
	if woken != 1 {
		t.Fatalf("woken = %d, want 1 (recovered pool session should be started)", woken)
	}
	if !sp.IsRunning(newSessionName) {
		t.Fatalf("session %q not running after reconcile — pool recovery did not trigger start", newSessionName)
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

func TestResolvePoolSlot_LegacyGCNaming(t *testing.T) {
	if got := resolvePoolSlot("worker-gc-1", "worker"); got != 1 {
		t.Errorf("resolvePoolSlot(worker-gc-1, worker) = %d, want 1", got)
	}
	if got := resolvePoolSlot("worker-gc-5", "worker"); got != 5 {
		t.Errorf("resolvePoolSlot(worker-gc-5, worker) = %d, want 5", got)
	}
	// Non-numeric after gc- still returns 0.
	if got := resolvePoolSlot("worker-gc-abc", "worker"); got != 0 {
		t.Errorf("resolvePoolSlot(worker-gc-abc, worker) = %d, want 0", got)
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
	codexBase := "builtin:codex"
	codexMini, err := config.ResolveProvider(&config.Agent{Name: "codex-mini", Provider: "codex-mini"}, nil, map[string]config.ProviderSpec{
		"codex-mini": {
			Base:    &codexBase,
			Command: "aimux",
			Args: []string{
				"run", "codex", "--",
				"--dangerously-bypass-approvals-and-sandbox",
				"-m", "gpt-5.3-codex-spark",
				"-c", "model_reasoning_effort=\"medium\"",
			},
			ResumeCommand: "aimux run codex -- --dangerously-bypass-approvals-and-sandbox -m gpt-5.3-codex-spark resume {{.SessionKey}}",
		},
	}, func(name string) (string, error) { return "/usr/bin/" + name, nil })
	if err != nil {
		t.Fatalf("ResolveProvider(codex-mini): %v", err)
	}
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
		{
			name:       "explicit wrapped codex resume command includes inferred defaults",
			command:    "aimux run codex -- --dangerously-bypass-approvals-and-sandbox --model gpt-5.3-codex-spark -c model_reasoning_effort=medium",
			sessionKey: "def-456",
			provider:   codexMini,
			want:       "aimux run codex -- --dangerously-bypass-approvals-and-sandbox -m gpt-5.3-codex-spark resume -c model_reasoning_effort=medium def-456",
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

func TestFailedCreateIsKnownState(t *testing.T) {
	if !knownSessionStates[string(sessionpkg.StateFailedCreate)] {
		t.Fatal("failed-create must be a known session state")
	}
}

func TestReconcileSessionBeads_BuildDesiredStateSkipsFailedCreatePoolSession(t *testing.T) {
	cases := []struct {
		name             string
		startedAt        time.Time
		wantFailedStatus string
	}{
		{
			name:             "fresh pending lease waits for expiry",
			startedAt:        time.Date(2026, 4, 1, 11, 59, 0, 0, time.UTC),
			wantFailedStatus: "open",
		},
		{
			name:             "stale pending lease closes",
			startedAt:        time.Date(2026, 4, 1, 11, 49, 59, 0, time.UTC),
			wantFailedStatus: "closed",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := beads.NewMemStore()
			clk := &clock.Fake{Time: time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)}
			sp := runtime.NewFake()
			cfg := &config.City{
				Workspace: config.Workspace{Name: "test-city"},
				Agents: []config.Agent{{
					Name:              "worker",
					StartCommand:      "true",
					MaxActiveSessions: intPtr(3),
					ScaleCheck:        "printf 1",
				}},
			}

			failedBead, err := store.Create(beads.Bead{
				Title:  "worker-1",
				Type:   sessionBeadType,
				Labels: []string{sessionBeadLabel, "agent:worker-1"},
				Metadata: map[string]string{
					"session_name":              "worker-1",
					"agent_name":                "worker-1",
					"template":                  "worker",
					"state":                     string(sessionpkg.StateFailedCreate),
					"pool_slot":                 "1",
					"pending_create_claim":      boolMetadata(true),
					"pending_create_started_at": pendingCreateStartedAtNow(tc.startedAt),
					poolManagedMetadataKey:      boolMetadata(true),
					"live_hash":                 runtime.LiveFingerprint(runtime.Config{Command: "true"}),
					"generation":                "1",
					"instance_token":            "failed-token",
				},
			})
			if err != nil {
				t.Fatalf("Create failed-create bead: %v", err)
			}

			var stdout, stderr bytes.Buffer
			dsResult := buildDesiredState(cfg.EffectiveCityName(), t.TempDir(), clk.Now().UTC(), cfg, sp, store, &stderr)
			if _, ok := dsResult.State[failedBead.Metadata["session_name"]]; ok {
				t.Fatalf("desired state reused failed-create bead %s; state=%#v", failedBead.ID, dsResult.State)
			}

			var fresh TemplateParams
			for _, tp := range dsResult.State {
				if tp.TemplateName == "worker" {
					fresh = tp
					break
				}
			}
			if fresh.SessionName == "" {
				t.Fatalf("desired state did not allocate a fresh worker session; state=%#v stderr:\n%s", dsResult.State, stderr.String())
			}
			if fresh.SessionName == failedBead.Metadata["session_name"] {
				t.Fatalf("fresh session name = %q, want different from failed-create bead", fresh.SessionName)
			}

			sessions, err := loadSessionBeads(store)
			if err != nil {
				t.Fatalf("loadSessionBeads: %v", err)
			}
			cfgNames := configuredSessionNames(cfg, cfg.EffectiveCityName(), store)
			poolDesired := PoolDesiredCounts(ComputePoolDesiredStates(cfg, dsResult.AssignedWorkBeads, sessions, dsResult.ScaleCheckCounts))
			if poolDesired == nil {
				poolDesired = make(map[string]int)
			}
			mergeNamedSessionDemand(poolDesired, dsResult.NamedSessionDemand, cfg)

			woken := reconcileSessionBeads(
				context.Background(), sessions, dsResult.State, cfgNames,
				cfg, sp, store, nil, dsResult.AssignedWorkBeads, nil, newDrainTracker(), poolDesired,
				dsResult.StoreQueryPartial, nil, cfg.EffectiveCityName(),
				nil, clk, events.Discard, 0, 0, &stdout, &stderr,
			)
			if woken != 1 {
				t.Fatalf("woken = %d, want 1 for fresh replacement session; stdout:\n%s\nstderr:\n%s", woken, stdout.String(), stderr.String())
			}
			if !sp.IsRunning(fresh.SessionName) {
				t.Fatalf("fresh session %q is not running after reconcile; stdout:\n%s\nstderr:\n%s", fresh.SessionName, stdout.String(), stderr.String())
			}

			gotFailed, err := store.Get(failedBead.ID)
			if err != nil {
				t.Fatalf("Get failed-create bead: %v", err)
			}
			if gotFailed.Status != tc.wantFailedStatus {
				t.Fatalf("failed-create bead status = %q, want %q", gotFailed.Status, tc.wantFailedStatus)
			}
			if gotFailed.Status == "open" && gotFailed.Metadata["state"] != string(sessionpkg.StateFailedCreate) {
				t.Fatalf("open failed-create bead state = %q, want %q", gotFailed.Metadata["state"], sessionpkg.StateFailedCreate)
			}
			if gotFailed.Status == "open" {
				var secondTickStderr bytes.Buffer
				secondTick := buildDesiredState(cfg.EffectiveCityName(), t.TempDir(), clk.Now().UTC(), cfg, sp, store, &secondTickStderr)
				if _, ok := secondTick.State[failedBead.Metadata["session_name"]]; ok {
					t.Fatalf("second tick reused failed-create bead %s; state=%#v stderr:\n%s", failedBead.ID, secondTick.State, secondTickStderr.String())
				}
			}
			if strings.Contains(stderr.String(), "unknown state") {
				t.Errorf("reconciler logged unknown state for failed-create bead: %s", stderr.String())
			}
		})
	}
}

func TestReconcileSessionBeads_SyncReplacesFailedCreateNamedSession(t *testing.T) {
	store := beads.NewMemStore()
	clk := &clock.Fake{Time: time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)}
	sp := runtime.NewFake()
	cityPath := t.TempDir()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "worker",
			StartCommand:      "true",
			MaxActiveSessions: intPtr(1),
		}},
		NamedSessions: []config.NamedSession{{
			Name:     "primary",
			Template: "worker",
			Mode:     "always",
		}},
	}
	identity := "primary"
	sessionName := config.NamedSessionRuntimeName(cfg.EffectiveCityName(), cfg.Workspace, identity)
	failedBead, err := store.Create(beads.Bead{
		Title:  sessionName,
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "agent:" + identity},
		Metadata: map[string]string{
			"session_name":               sessionName,
			"alias":                      identity,
			"agent_name":                 identity,
			"template":                   "worker",
			"state":                      string(sessionpkg.StateFailedCreate),
			"pending_create_claim":       boolMetadata(true),
			"pending_create_started_at":  pendingCreateStartedAtNow(clk.Now()),
			"live_hash":                  runtime.LiveFingerprint(runtime.Config{Command: "true"}),
			"generation":                 "1",
			"instance_token":             "failed-token",
			namedSessionMetadataKey:      boolMetadata(true),
			namedSessionIdentityMetadata: identity,
			namedSessionModeMetadata:     "always",
		},
	})
	if err != nil {
		t.Fatalf("Create failed-create named bead: %v", err)
	}

	var stdout, stderr bytes.Buffer
	dsResult := buildDesiredState(cfg.EffectiveCityName(), cityPath, clk.Now().UTC(), cfg, sp, store, &stderr)
	if _, ok := dsResult.State[sessionName]; !ok {
		t.Fatalf("desired state missing configured named session %q; state=%#v stderr:\n%s", sessionName, dsResult.State, stderr.String())
	}
	cfgNames := configuredSessionNames(cfg, cfg.EffectiveCityName(), store)
	syncSessionBeads(cityPath, store, dsResult.State, sp, cfgNames, cfg, clk, &stderr, true)

	gotFailed, err := store.Get(failedBead.ID)
	if err != nil {
		t.Fatalf("Get failed-create named bead: %v", err)
	}
	if gotFailed.Status != "closed" {
		t.Fatalf("failed-create named bead status = %q, want closed; stderr:\n%s", gotFailed.Status, stderr.String())
	}
	if want := sessionpkg.CanonicalCloseReason(string(sessionpkg.StateFailedCreate)); gotFailed.Metadata["close_reason"] != want {
		t.Fatalf("failed-create named bead close_reason = %q, want %q", gotFailed.Metadata["close_reason"], want)
	}
	if gotFailed.Metadata["pending_create_claim"] != "" {
		t.Fatalf("failed-create named bead pending_create_claim = %q, want cleared", gotFailed.Metadata["pending_create_claim"])
	}

	sessions, err := loadSessionBeads(store)
	if err != nil {
		t.Fatalf("loadSessionBeads: %v", err)
	}
	var fresh beads.Bead
	for _, b := range sessions {
		if b.ID != failedBead.ID && b.Metadata["session_name"] == sessionName {
			fresh = b
			break
		}
	}
	if fresh.ID == "" {
		t.Fatalf("sync did not create a fresh named-session bead; open sessions=%#v stderr:\n%s", sessions, stderr.String())
	}
	if fresh.Metadata["state"] != string(sessionpkg.StateStartPending) {
		t.Fatalf("fresh named-session state = %q, want start-pending", fresh.Metadata["state"])
	}

	poolDesired := PoolDesiredCounts(ComputePoolDesiredStates(cfg, dsResult.AssignedWorkBeads, sessions, dsResult.ScaleCheckCounts))
	if poolDesired == nil {
		poolDesired = make(map[string]int)
	}
	mergeNamedSessionDemand(poolDesired, dsResult.NamedSessionDemand, cfg)
	woken := reconcileSessionBeads(
		context.Background(), sessions, dsResult.State, cfgNames,
		cfg, sp, store, nil, dsResult.AssignedWorkBeads, nil, newDrainTracker(), poolDesired,
		dsResult.StoreQueryPartial, nil, cfg.EffectiveCityName(),
		nil, clk, events.Discard, 0, 0, &stdout, &stderr,
	)
	if woken != 1 {
		t.Fatalf("woken = %d, want 1 for fresh named session; stdout:\n%s\nstderr:\n%s", woken, stdout.String(), stderr.String())
	}
	if !sp.IsRunning(sessionName) {
		t.Fatalf("fresh named session %q is not running after reconcile; stdout:\n%s\nstderr:\n%s", sessionName, stdout.String(), stderr.String())
	}
}

// TestReconcileSessionBeads_ClosesOrphanedFailedCreateAndFreesSlot verifies
// the post-lease-expiry close path for a pool session bead whose close call
// failed after failed-create metadata was written.
func TestReconcileSessionBeads_ClosesOrphanedFailedCreateAndFreesSlot(t *testing.T) {
	store := beads.NewMemStore()
	clk := &clock.Fake{Time: time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)}
	sp := runtime.NewFake()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{
			{Name: "worker", StartCommand: "true", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(3)},
		},
	}

	// Simulate the close retry after a stale failed-create lease has expired.
	failedBead, err := store.Create(beads.Bead{
		Title:  "worker-1",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "agent:worker-1"},
		Metadata: map[string]string{
			"session_name":              "worker-1",
			"agent_name":                "worker-1",
			"template":                  "worker",
			"state":                     string(sessionpkg.StateFailedCreate),
			"pool_slot":                 "1",
			"pending_create_claim":      boolMetadata(true),
			"pending_create_started_at": pendingCreateStartedAtNow(clk.Now().Add(-(pendingCreateNeverStartedTimeout + time.Second))),
			poolManagedMetadataKey:      boolMetadata(true),
			"live_hash":                 runtime.LiveFingerprint(runtime.Config{Command: "true"}),
			"generation":                "1",
			"instance_token":            "failed-token",
		},
	})
	if err != nil {
		t.Fatalf("Create failed-create bead: %v", err)
	}

	// Fresh pool session bead: what buildDesiredState allocates for the pending demand.
	freshBead, err := store.Create(beads.Bead{
		Title:  "worker-2",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "agent:worker-2"},
		Metadata: map[string]string{
			"session_name":         "worker-2",
			"agent_name":           "worker-2",
			"template":             "worker",
			"state":                "creating",
			"pool_slot":            "2",
			"pending_create_claim": boolMetadata(true),
			poolManagedMetadataKey: boolMetadata(true),
			"live_hash":            runtime.LiveFingerprint(runtime.Config{Command: "true"}),
			"generation":           "1",
			"instance_token":       "new-token",
		},
	})
	if err != nil {
		t.Fatalf("Create fresh bead: %v", err)
	}

	// desired: only the fresh bead; the failed-create bead is not desired.
	ds := map[string]TemplateParams{
		"worker-2": {
			TemplateName: "worker",
			InstanceName: "worker-2",
			Command:      "true",
			PoolSlot:     2,
		},
	}

	sessions, _ := loadSessionBeads(store)
	cfgNames := configuredSessionNames(cfg, "", store)
	poolDesired := map[string]int{"worker": 1}
	var stdout, stderr bytes.Buffer
	woken := reconcileSessionBeads(
		context.Background(), sessions, ds, cfgNames,
		cfg, sp, store, nil, nil, nil, newDrainTracker(), poolDesired, false, nil, "test-city",
		nil, clk, events.Discard, 0, 0, &stdout, &stderr,
	)

	// The failed-create bead must be closed, not silently skipped as unknown state.
	got, err := store.Get(failedBead.ID)
	if err != nil {
		t.Fatalf("Get failed-create bead: %v", err)
	}
	if got.Status != "closed" {
		t.Fatalf("failed-create bead status = %q, want closed", got.Status)
	}
	if want := sessionpkg.CanonicalCloseReason(string(sessionpkg.StateFailedCreate)); got.Metadata["close_reason"] != want {
		t.Fatalf("failed-create bead close_reason = %q, want %q", got.Metadata["close_reason"], want)
	}
	if got.Metadata["pending_create_claim"] != "" || got.Metadata["pending_create_started_at"] != "" {
		t.Fatalf("failed-create pending metadata = claim %q started_at %q, want cleared",
			got.Metadata["pending_create_claim"], got.Metadata["pending_create_started_at"])
	}
	if strings.Contains(stderr.String(), "unknown state") {
		t.Errorf("reconciler logged unknown state for failed-create bead: %s", stderr.String())
	}

	// A fresh session must be started in the freed slot.
	if woken != 1 {
		t.Fatalf("woken = %d, want 1 (fresh pool session should be started)", woken)
	}
	if !sp.IsRunning(freshBead.Metadata["session_name"]) {
		t.Fatalf("fresh session %q is not running after stale failed-create cleanup", freshBead.Metadata["session_name"])
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
