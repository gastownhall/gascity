package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/agent"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/shellquote"
)

type failingMetadataBatchStore struct {
	*beads.MemStore
	failBatch bool
}

func (s *failingMetadataBatchStore) SetMetadataBatch(id string, kvs map[string]string) error {
	if s.failBatch {
		return errors.New("batch failed")
	}
	return s.MemStore.SetMetadataBatch(id, kvs)
}

type failNthMetadataBatchStore struct {
	*beads.MemStore
	failOn int
	calls  int
}

func (s *failNthMetadataBatchStore) SetMetadataBatch(id string, kvs map[string]string) error {
	s.calls++
	if s.calls == s.failOn {
		return errors.New("batch failed")
	}
	return s.MemStore.SetMetadataBatch(id, kvs)
}

type failSetMetadataStore struct {
	*beads.MemStore
	failKey string
}

func (s *failSetMetadataStore) SetMetadata(id, key, value string) error {
	if key == s.failKey {
		return fmt.Errorf("set metadata %s failed", key)
	}
	return s.MemStore.SetMetadata(id, key, value)
}

type panicMetadataBatchStore struct {
	*beads.MemStore
}

func (s *panicMetadataBatchStore) SetMetadataBatch(string, map[string]string) error {
	panic("metadata batch panic")
}

type getErrorStore struct {
	*beads.MemStore
}

func (s *getErrorStore) Get(string) (beads.Bead, error) {
	return beads.Bead{}, fmt.Errorf("get failed")
}

type closedMetadataMatchStore struct {
	*beads.MemStore
	matches []beads.Bead
}

func (s *closedMetadataMatchStore) ListByMetadata(filters map[string]string, _ int, _ ...beads.QueryOpt) ([]beads.Bead, error) {
	var out []beads.Bead
	for _, match := range s.matches {
		ok := true
		for key, value := range filters {
			if match.Metadata[key] != value {
				ok = false
				break
			}
		}
		if ok {
			out = append(out, match)
		}
	}
	return out, nil
}

type listMetadataErrorStore struct {
	*beads.MemStore
}

func (s *listMetadataErrorStore) ListByMetadata(map[string]string, int, ...beads.QueryOpt) ([]beads.Bead, error) {
	return nil, errors.New("list failed")
}

type gatedStartProvider struct {
	*runtime.Fake
	mu            sync.Mutex
	inFlight      int
	maxInFlight   int
	started       []string
	startSignals  chan string
	releaseByName map[string]chan struct{}
}

func newGatedStartProvider() *gatedStartProvider {
	return &gatedStartProvider{
		Fake:          runtime.NewFake(),
		startSignals:  make(chan string, 32),
		releaseByName: make(map[string]chan struct{}),
	}
}

func (p *gatedStartProvider) Start(ctx context.Context, name string, cfg runtime.Config) error {
	p.mu.Lock()
	p.inFlight++
	if p.inFlight > p.maxInFlight {
		p.maxInFlight = p.inFlight
	}
	p.started = append(p.started, name)
	ch := p.releaseByName[name]
	if ch == nil {
		ch = make(chan struct{})
		p.releaseByName[name] = ch
	}
	p.mu.Unlock()

	p.startSignals <- name

	select {
	case <-ch:
	case <-ctx.Done():
		p.mu.Lock()
		p.inFlight--
		p.mu.Unlock()
		return ctx.Err()
	}

	err := p.Fake.Start(ctx, name, cfg)
	p.mu.Lock()
	p.inFlight--
	p.mu.Unlock()
	return err
}

func (p *gatedStartProvider) release(name string) {
	p.mu.Lock()
	ch := p.releaseByName[name]
	p.mu.Unlock()
	if ch != nil {
		select {
		case <-ch:
		default:
			close(ch)
		}
	}
}

func (p *gatedStartProvider) waitForStarts(t *testing.T, n int) []string {
	t.Helper()
	var names []string
	timeout := time.After(3 * time.Second)
	for len(names) < n {
		select {
		case name := <-p.startSignals:
			names = append(names, name)
		case <-timeout:
			t.Fatalf("timed out waiting for %d starts, got %v", n, names)
		}
	}
	return names
}

func (p *gatedStartProvider) ensureNoFurtherStart(t *testing.T, wait time.Duration) {
	t.Helper()
	select {
	case name := <-p.startSignals:
		t.Fatalf("unexpected extra start signal: %s", name)
	case <-time.After(wait):
	}
}

type shutdownWaitProvider struct {
	*gatedStartProvider
	listCalled chan struct{}
	listOnce   sync.Once
}

func newShutdownWaitProvider() *shutdownWaitProvider {
	return &shutdownWaitProvider{
		gatedStartProvider: newGatedStartProvider(),
		listCalled:         make(chan struct{}),
	}
}

func (p *shutdownWaitProvider) ListRunning(prefix string) ([]string, error) {
	p.listOnce.Do(func() { close(p.listCalled) })
	return p.Fake.ListRunning(prefix)
}

func creatingMeta(meta map[string]string) map[string]string {
	cp := make(map[string]string, len(meta)+1)
	for key, value := range meta {
		cp[key] = value
	}
	cp["state"] = "creating"
	return cp
}

type gatedStopProvider struct {
	*runtime.Fake
	mu            sync.Mutex
	inFlight      int
	maxInFlight   int
	stopSignals   chan string
	interrupts    chan string
	releaseByName map[string]chan struct{}
	releaseInt    map[string]chan struct{}
}

func newGatedStopProvider() *gatedStopProvider {
	return &gatedStopProvider{
		Fake:          runtime.NewFake(),
		stopSignals:   make(chan string, 32),
		interrupts:    make(chan string, 32),
		releaseByName: make(map[string]chan struct{}),
		releaseInt:    make(map[string]chan struct{}),
	}
}

func (p *gatedStopProvider) Stop(name string) error {
	p.mu.Lock()
	p.inFlight++
	if p.inFlight > p.maxInFlight {
		p.maxInFlight = p.inFlight
	}
	ch := p.releaseByName[name]
	if ch == nil {
		ch = make(chan struct{})
		p.releaseByName[name] = ch
	}
	p.mu.Unlock()

	p.stopSignals <- name
	<-ch

	err := p.Fake.Stop(name)
	p.mu.Lock()
	p.inFlight--
	p.mu.Unlock()
	return err
}

func (p *gatedStopProvider) Interrupt(name string) error {
	p.mu.Lock()
	ch := p.releaseInt[name]
	if ch == nil {
		ch = make(chan struct{})
		p.releaseInt[name] = ch
	}
	p.mu.Unlock()

	p.interrupts <- name
	<-ch
	return p.Fake.Interrupt(name)
}

func (p *gatedStopProvider) release(name string) {
	p.mu.Lock()
	ch := p.releaseByName[name]
	p.mu.Unlock()
	if ch != nil {
		select {
		case <-ch:
		default:
			close(ch)
		}
	}
}

func (p *gatedStopProvider) releaseInterrupt(name string) {
	p.mu.Lock()
	ch := p.releaseInt[name]
	p.mu.Unlock()
	if ch != nil {
		select {
		case <-ch:
		default:
			close(ch)
		}
	}
}

func (p *gatedStopProvider) waitForStops(t *testing.T, n int) []string {
	t.Helper()
	var names []string
	timeout := time.After(3 * time.Second)
	for len(names) < n {
		select {
		case name := <-p.stopSignals:
			names = append(names, name)
		case <-timeout:
			t.Fatalf("timed out waiting for %d stops, got %v", n, names)
		}
	}
	return names
}

func (p *gatedStopProvider) ensureNoFurtherStop(t *testing.T) {
	t.Helper()
	select {
	case name := <-p.stopSignals:
		t.Fatalf("unexpected extra stop signal: %s", name)
	case <-time.After(150 * time.Millisecond):
	}
}

func (p *gatedStopProvider) waitForInterrupts(t *testing.T, n int) []string {
	t.Helper()
	var names []string
	timeout := time.After(3 * time.Second)
	for len(names) < n {
		select {
		case name := <-p.interrupts:
			names = append(names, name)
		case <-timeout:
			t.Fatalf("timed out waiting for %d interrupts, got %v", n, names)
		}
	}
	return names
}

func (p *gatedStopProvider) ensureNoFurtherInterrupt(t *testing.T, wait time.Duration) {
	t.Helper()
	select {
	case name := <-p.interrupts:
		t.Fatalf("unexpected extra interrupt signal: %s", name)
	case <-time.After(wait):
	}
}

type interruptExitProvider struct {
	*runtime.Fake
}

func (p *interruptExitProvider) Interrupt(name string) error {
	if err := p.Fake.Interrupt(name); err != nil {
		return err
	}
	return p.Stop(name)
}

type staleIsRunningAfterInterruptProvider struct {
	*runtime.Fake
	mu          sync.Mutex
	interrupted map[string]bool
}

func newStaleIsRunningAfterInterruptProvider() *staleIsRunningAfterInterruptProvider {
	return &staleIsRunningAfterInterruptProvider{
		Fake:        runtime.NewFake(),
		interrupted: make(map[string]bool),
	}
}

func (p *staleIsRunningAfterInterruptProvider) Interrupt(name string) error {
	p.mu.Lock()
	p.interrupted[name] = true
	p.mu.Unlock()
	return p.Fake.Interrupt(name)
}

func (p *staleIsRunningAfterInterruptProvider) IsRunning(name string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.interrupted[name] {
		return false
	}
	return p.Fake.IsRunning(name)
}

type dropDependencyAfterNStartsProvider struct {
	*runtime.Fake
	mu        sync.Mutex
	starts    int
	dropAfter int
	depName   string
}

func (p *dropDependencyAfterNStartsProvider) Start(ctx context.Context, name string, cfg runtime.Config) error {
	if err := p.Fake.Start(ctx, name, cfg); err != nil {
		return err
	}
	p.mu.Lock()
	p.starts++
	shouldDrop := p.starts == p.dropAfter
	p.mu.Unlock()
	if shouldDrop {
		_ = p.Stop(p.depName)
	}
	return nil
}

func (p *dropDependencyAfterNStartsProvider) waitForStarts(t *testing.T, want int) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		p.mu.Lock()
		got := p.starts
		p.mu.Unlock()
		if got >= want {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for %d starts, got %d", want, got)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

type panicStartProvider struct {
	*runtime.Fake
}

func (p *panicStartProvider) Start(context.Context, string, runtime.Config) error {
	panic("boom")
}

type multilineStopProvider struct {
	*runtime.Fake
}

func (p *multilineStopProvider) Stop(string) error {
	return fmt.Errorf("boom\nstack line")
}

func containsAll(got []string, want ...string) bool {
	if len(got) != len(want) {
		return false
	}
	seen := make(map[string]int)
	for _, name := range got {
		seen[name]++
	}
	for _, name := range want {
		if seen[name] == 0 {
			return false
		}
		seen[name]--
	}
	return true
}

func TestReconcileSessionBeads_StartsIndependentWaveInParallelBeforeDependentWave(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "worker", MaxActiveSessions: intPtr(1), DependsOn: []string{"db", "cache"}},
			{Name: "db", MaxActiveSessions: intPtr(1)},
			{Name: "cache"},
		},
	}
	store := beads.NewMemStore()
	sp := newGatedStartProvider()
	rec := events.Discard
	clk := &clock.Fake{Time: time.Date(2026, 3, 18, 12, 0, 0, 0, time.UTC)}
	desired := map[string]TemplateParams{
		"db":     {Command: "db", SessionName: "db", TemplateName: "db"},
		"cache":  {Command: "cache", SessionName: "cache", TemplateName: "cache"},
		"worker": {Command: "worker", SessionName: "worker", TemplateName: "worker"},
	}
	db := makeBead("db-id", creatingMeta(map[string]string{
		"session_name": "db", "template": "db", "generation": "1", "instance_token": "tok-db",
	}))
	cache := makeBead("cache-id", creatingMeta(map[string]string{
		"session_name": "cache", "template": "cache", "generation": "1", "instance_token": "tok-cache",
	}))
	worker := makeBead("worker-id", creatingMeta(map[string]string{
		"session_name": "worker", "template": "worker", "generation": "1", "instance_token": "tok-worker",
	}))
	for _, bead := range []beads.Bead{db, cache, worker} {
		if _, err := store.Create(beads.Bead{
			ID:       bead.ID,
			Title:    bead.Metadata["session_name"],
			Type:     sessionBeadType,
			Labels:   []string{sessionBeadLabel},
			Metadata: bead.Metadata,
		}); err != nil {
			t.Fatal(err)
		}
	}
	sessions, err := loadSessionBeads(store)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan int, 1)
	go func() {
		done <- reconcileSessionBeads(
			context.Background(), sessions, desired, configuredSessionNames(cfg, "", store),
			cfg, sp, store, nil, nil, nil, newDrainTracker(), map[string]int{"db": 1, "cache": 1, "worker": 1}, false, nil, "",
			nil, clk, rec, 5*time.Second, 0, ioDiscard{}, ioDiscard{},
		)
	}()

	firstWave := sp.waitForStarts(t, 2)
	if !containsAll(firstWave, "db", "cache") {
		t.Fatalf("first wave = %v, want db+cache", firstWave)
	}
	sp.ensureNoFurtherStart(t, 150*time.Millisecond)
	sp.release("db")
	sp.release("cache")

	secondWave := sp.waitForStarts(t, 1)
	if !containsAll(secondWave, "worker") {
		t.Fatalf("second wave = %v, want worker", secondWave)
	}
	sp.release("worker")

	select {
	case woken := <-done:
		if woken != 3 {
			t.Fatalf("woken = %d, want 3", woken)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("reconcile did not finish")
	}

	if sp.maxInFlight != 2 {
		t.Fatalf("max in-flight starts = %d, want 2", sp.maxInFlight)
	}
}

func TestReconcileSessionBeads_FailedDependencyBlocksDependentButNotSibling(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Agents: []config.Agent{
			{Name: "worker", MaxActiveSessions: intPtr(1), DependsOn: []string{"db"}},
			{Name: "db", MaxActiveSessions: intPtr(1)},
			{Name: "cache"},
		},
	}
	env.addDesired("worker", "worker", false)
	env.addDesired("db", "db", false)
	env.addDesired("cache", "cache", false)
	env.sp.StartErrors["db"] = context.DeadlineExceeded

	woken := env.reconcile([]beads.Bead{
		func() beads.Bead {
			b := env.createSessionBead("worker", "worker")
			env.markSessionCreating(&b)
			return b
		}(),
		func() beads.Bead { b := env.createSessionBead("db", "db"); env.markSessionCreating(&b); return b }(),
		func() beads.Bead { b := env.createSessionBead("cache", "cache"); env.markSessionCreating(&b); return b }(),
	})

	if woken != 1 {
		t.Fatalf("woken = %d, want 1", woken)
	}
	if env.sp.IsRunning("worker") {
		t.Fatal("worker should not be running when db failed to start")
	}
	if !env.sp.IsRunning("cache") {
		t.Fatal("cache should still start despite db failure")
	}
}

func TestPrepareStartCandidate_UsesSessionIDForTaskWorkDir(t *testing.T) {
	store := beads.NewMemStore()
	session, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "agent:frontend/worker-1"},
		Metadata: map[string]string{
			"template":     "worker",
			"session_name": "custom-worker-1",
			"pool_slot":    "1",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	workDir := t.TempDir()
	task, err := store.Create(beads.Bead{
		Title: "task",
		Metadata: map[string]string{
			"work_dir": workDir,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	status := "in_progress"
	assignee := session.ID
	if err := store.Update(task.ID, beads.UpdateOpts{Status: &status, Assignee: &assignee}); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(workDir); err != nil || !info.IsDir() {
		t.Fatalf("temp workDir not available: %v", err)
	}

	prepared, err := prepareStartCandidate(startCandidate{
		session: &session,
		tp: TemplateParams{
			TemplateName: "frontend/worker",
			SessionName:  "custom-worker-1",
		},
		order: 0,
	}, &config.City{
		Agents: []config.Agent{
			{Name: "worker", Dir: "frontend", MinActiveSessions: intPtr(1), MaxActiveSessions: intPtr(2)},
		},
	}, store, &clock.Fake{Time: time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatalf("prepareStartCandidate: %v", err)
	}
	if prepared.cfg.WorkDir != workDir {
		t.Fatalf("prepared.cfg.WorkDir = %q, want %q", prepared.cfg.WorkDir, workDir)
	}
}

func TestExecutePlannedStarts_FreshWakeAfterDrainRetainsStartupContext(t *testing.T) {
	skipSlowCmdGCTest(t, "waits through stale session-key detection; run make test-cmd-gc-process for full coverage")
	sp := runtime.NewFake()
	store := beads.NewMemStore()
	clk := &clock.Fake{Time: time.Date(2026, 4, 7, 12, 0, 0, 0, time.UTC)}
	cfg := &config.City{
		Agents: []config.Agent{{Name: "mayor"}},
	}
	tp := TemplateParams{
		Command:      "claude --dangerously-skip-permissions",
		SessionName:  "mayor",
		TemplateName: "mayor",
		Prompt:       "You are the mayor. Read the city state and coordinate next actions.",
		Hints: agent.StartupHints{
			Nudge: "Check mail and hook status, then act accordingly.",
		},
		ResolvedProvider: &config.ResolvedProvider{
			Name:          "claude",
			Command:       "claude",
			PromptMode:    "arg",
			ResumeFlag:    "--resume",
			ResumeStyle:   "flag",
			SessionIDFlag: "--session-id",
		},
	}
	overrides, err := json.Marshal(map[string]string{
		"initial_message": "Handoff context: check your mail before taking action.",
	})
	if err != nil {
		t.Fatalf("Marshal(template_overrides): %v", err)
	}
	session, err := store.Create(beads.Bead{
		Title:  "mayor",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":        "mayor",
			"template":            "mayor",
			"state":               "asleep",
			"sleep_reason":        "drained",
			"wake_mode":           "fresh",
			"session_key":         "fresh-key-123",
			"started_config_hash": "previous-start",
			"template_overrides":  string(overrides),
			"generation":          "1",
			"instance_token":      "test-token",
		},
	})
	if err != nil {
		t.Fatalf("Create(session): %v", err)
	}

	woken := executePlannedStarts(
		context.Background(),
		[]startCandidate{{session: &session, tp: tp, order: 0}},
		cfg,
		map[string]TemplateParams{"mayor": tp},
		sp,
		store,
		"",
		clk,
		events.Discard,
		5*time.Second,
		ioDiscard{},
		ioDiscard{},
	)
	if woken != 1 {
		t.Fatalf("woken = %d, want 1", woken)
	}

	var startCfg *runtime.Config
	for _, call := range sp.Calls {
		if call.Method == "Start" && call.Name == "mayor" {
			cfgCopy := call.Config
			startCfg = &cfgCopy
			break
		}
	}
	if startCfg == nil {
		t.Fatalf("expected Start call for mayor, calls=%#v", sp.Calls)
	}
	if !strings.HasPrefix(startCfg.Command, "claude --dangerously-skip-permissions --session-id ") {
		t.Fatalf("Start command = %q, want fresh session-id launch", startCfg.Command)
	}
	gotSessionKey := strings.TrimPrefix(startCfg.Command, "claude --dangerously-skip-permissions --session-id ")
	if gotSessionKey == "" {
		t.Fatalf("Start command = %q, want non-empty generated session key", startCfg.Command)
	}
	if gotSessionKey == "fresh-key-123" {
		t.Fatalf("Start command reused stale session key %q", gotSessionKey)
	}
	updated, err := store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get(session): %v", err)
	}
	if updated.Metadata["session_key"] != gotSessionKey {
		t.Fatalf("stored session_key = %q, want generated key from command %q", updated.Metadata["session_key"], gotSessionKey)
	}
	if startCfg.Nudge != "Check mail and hook status, then act accordingly." {
		t.Fatalf("Start nudge = %q, want startup nudge preserved", startCfg.Nudge)
	}
	if startCfg.PromptSuffix == "" {
		t.Fatal("PromptSuffix should be present on fresh wake after drain")
	}
	parts := shellquote.Split(startCfg.PromptSuffix)
	if len(parts) != 1 {
		t.Fatalf("PromptSuffix parsed parts = %#v, want single prompt payload", parts)
	}
	if !strings.Contains(parts[0], "You are the mayor. Read the city state and coordinate next actions.") {
		t.Fatalf("prompt payload missing base prompt: %q", parts[0])
	}
	if !strings.Contains(parts[0], "Handoff context: check your mail before taking action.") {
		t.Fatalf("prompt payload missing initial_message on fresh wake: %q", parts[0])
	}
}

func TestPrepareStartCandidate_GeneratesMissingSessionKeyBeforeWake(t *testing.T) {
	store := beads.NewMemStore()
	session, err := store.Create(beads.Bead{
		Title:  "wendy",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "agent:wendy"},
		Metadata: map[string]string{
			"template":     "wendy",
			"session_name": "wendy",
			"wake_mode":    "fresh",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	prepared, err := prepareStartCandidate(startCandidate{
		session: &session,
		tp: TemplateParams{
			TemplateName: "wendy",
			SessionName:  "wendy",
			Command:      "aimux run claude --",
			ResolvedProvider: &config.ResolvedProvider{
				Name:          "claude",
				ResumeFlag:    "--resume",
				ResumeStyle:   "flag",
				SessionIDFlag: "--session-id",
			},
		},
		order: 0,
	}, &config.City{}, store, &clock.Fake{Time: time.Date(2026, 4, 9, 1, 26, 41, 0, time.UTC)})
	if err != nil {
		t.Fatalf("prepareStartCandidate: %v", err)
	}

	sessionKey := session.Metadata["session_key"]
	if sessionKey == "" {
		t.Fatal("session_key should be generated before wake")
	}
	if !strings.Contains(prepared.cfg.Command, "--session-id "+sessionKey) {
		t.Fatalf("prepared.cfg.Command = %q, want --session-id %s", prepared.cfg.Command, sessionKey)
	}

	stored, err := store.Get(session.ID)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if stored.Metadata["session_key"] != sessionKey {
		t.Fatalf("stored session_key = %q, want %q", stored.Metadata["session_key"], sessionKey)
	}
}

func TestReconcileSessionBeads_BlockedCandidatesDoNotConsumeWakeBudget(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Agents: []config.Agent{
			{Name: "blocked", MaxActiveSessions: intPtr(1), DependsOn: []string{"missing-dep"}},
			{Name: "missing-dep"},
			{Name: "ready-1"},
			{Name: "ready-2"},
			{Name: "ready-3"},
			{Name: "ready-4"},
			{Name: "ready-5"},
		},
	}
	for _, name := range []string{"blocked", "ready-1", "ready-2", "ready-3", "ready-4", "ready-5"} {
		env.addDesired(name, name, false)
	}

	woken := env.reconcile([]beads.Bead{
		func() beads.Bead {
			b := env.createSessionBead("blocked", "blocked")
			env.markSessionCreating(&b)
			return b
		}(),
		func() beads.Bead {
			b := env.createSessionBead("ready-1", "ready-1")
			env.markSessionCreating(&b)
			return b
		}(),
		func() beads.Bead {
			b := env.createSessionBead("ready-2", "ready-2")
			env.markSessionCreating(&b)
			return b
		}(),
		func() beads.Bead {
			b := env.createSessionBead("ready-3", "ready-3")
			env.markSessionCreating(&b)
			return b
		}(),
		func() beads.Bead {
			b := env.createSessionBead("ready-4", "ready-4")
			env.markSessionCreating(&b)
			return b
		}(),
		func() beads.Bead {
			b := env.createSessionBead("ready-5", "ready-5")
			env.markSessionCreating(&b)
			return b
		}(),
	})

	if woken != defaultMaxWakesPerTick {
		t.Fatalf("woken = %d, want %d", woken, defaultMaxWakesPerTick)
	}
	if env.sp.IsRunning("blocked") {
		t.Fatal("blocked session should not have started")
	}
	for _, name := range []string{"ready-1", "ready-2", "ready-3", "ready-4", "ready-5"} {
		if !env.sp.IsRunning(name) {
			t.Fatalf("%s should have started despite blocked candidate ahead of it", name)
		}
	}
}

// TestReconcileSessionBeads_DaemonMaxWakesPerTickOverride covers the fix for
// issue #772: [daemon].max_wakes_per_tick = N raises (or lowers) the wake
// budget away from the 5-per-tick default. Cities with slow cold-starts
// need this to drain the candidate queue.
func TestReconcileSessionBeads_DaemonMaxWakesPerTickOverride(t *testing.T) {
	env := newReconcilerTestEnv()
	override := 8
	env.cfg = &config.City{
		Daemon: config.DaemonConfig{MaxWakesPerTick: &override},
		Agents: []config.Agent{
			{Name: "ready-1"},
			{Name: "ready-2"},
			{Name: "ready-3"},
			{Name: "ready-4"},
			{Name: "ready-5"},
			{Name: "ready-6"},
			{Name: "ready-7"},
			{Name: "ready-8"},
			{Name: "ready-9"},
		},
	}
	names := []string{"ready-1", "ready-2", "ready-3", "ready-4", "ready-5", "ready-6", "ready-7", "ready-8", "ready-9"}
	for _, name := range names {
		env.addDesired(name, name, false)
	}

	var seeded []beads.Bead
	for _, name := range names {
		b := env.createSessionBead(name, name)
		env.markSessionCreating(&b)
		seeded = append(seeded, b)
	}

	woken := env.reconcile(seeded)

	if woken != override {
		t.Fatalf("woken = %d, want %d (overridden wake budget)", woken, override)
	}
	// First 8 run, 9th is deferred_by_wake_budget.
	for _, name := range names[:override] {
		if !env.sp.IsRunning(name) {
			t.Fatalf("%s should have started under override=%d", name, override)
		}
	}
	if env.sp.IsRunning(names[override]) {
		t.Fatalf("%s should have been deferred once budget hit", names[override])
	}
}

func TestPrepareStartCandidate_NoneModeInitialMessageStaysInNudge(t *testing.T) {
	store := beads.NewMemStore()
	bead, err := store.Create(beads.Bead{
		ID:     "gc-1",
		Title:  "mayor",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":       "mayor",
			"template":           "mayor",
			"generation":         "1",
			"instance_token":     "tok-mayor",
			"state":              "creating",
			"template_overrides": `{"initial_message":"hello from the user"}`,
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	prepared, err := prepareStartCandidate(startCandidate{
		session: &bead,
		tp: TemplateParams{
			TemplateName: "mayor",
			SessionName:  "mayor",
			Prompt:       "startup prompt",
			ResolvedProvider: &config.ResolvedProvider{
				Name:       "gemini",
				PromptMode: "none",
			},
		},
		order: 0,
	}, &config.City{
		Agents: []config.Agent{
			{Name: "mayor"},
		},
	}, store, &clock.Fake{Time: time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatalf("prepareStartCandidate: %v", err)
	}

	if prepared.cfg.PromptSuffix != "" {
		t.Fatalf("prepared.cfg.PromptSuffix = %q, want empty for prompt_mode none", prepared.cfg.PromptSuffix)
	}
	wantNudge := "startup prompt\n\n---\n\nUser message:\nhello from the user"
	if prepared.cfg.Nudge != wantNudge {
		t.Fatalf("prepared.cfg.Nudge = %q, want %q", prepared.cfg.Nudge, wantNudge)
	}
}

func TestExecutePlannedStarts_RevalidatesDependenciesBetweenWaveBatches(t *testing.T) {
	maxWakes := 8
	dropAfter := 3
	sp := &dropDependencyAfterNStartsProvider{
		Fake:      runtime.NewFake(),
		dropAfter: dropAfter,
		depName:   "db",
	}
	if err := sp.Start(context.Background(), "db", runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Daemon: config.DaemonConfig{MaxWakesPerTick: &maxWakes},
		Agents: []config.Agent{
			{Name: "app-1", DependsOn: []string{"db"}},
			{Name: "app-2", DependsOn: []string{"db"}},
			{Name: "app-3", DependsOn: []string{"db"}},
			{Name: "app-4", DependsOn: []string{"db"}},
			{Name: "db", MaxActiveSessions: intPtr(1)},
		},
	}
	store := beads.NewMemStore()
	clk := &clock.Fake{Time: time.Date(2026, 3, 18, 12, 0, 0, 0, time.UTC)}
	desired := map[string]TemplateParams{}
	var sessions []beads.Bead
	for _, name := range []string{"app-1", "app-2", "app-3", "app-4"} {
		desired[name] = TemplateParams{Command: name, SessionName: name, TemplateName: name}
		bead := makeBead(name+"-id", creatingMeta(map[string]string{
			"session_name":   name,
			"template":       name,
			"generation":     "1",
			"instance_token": "tok-" + name,
		}))
		created, err := store.Create(beads.Bead{
			ID:       bead.ID,
			Title:    name,
			Type:     sessionBeadType,
			Labels:   []string{sessionBeadLabel},
			Metadata: bead.Metadata,
		})
		if err != nil {
			t.Fatal(err)
		}
		sessions = append(sessions, created)
	}

	poolDesired := map[string]int{"app-1": 1, "app-2": 1, "app-3": 1, "app-4": 1}
	var stderr bytes.Buffer
	woken := reconcileSessionBeads(
		context.Background(), sessions, desired, configuredSessionNames(cfg, "", store),
		cfg, sp, store, nil, nil, nil, newDrainTracker(), poolDesired, false, nil, "",
		nil, clk, events.Discard, 5*time.Second, 0, ioDiscard{}, &stderr,
	)

	if woken != dropAfter {
		t.Fatalf("woken = %d, want %d", woken, dropAfter)
	}
	for _, name := range []string{"app-1", "app-2", "app-3"} {
		if !sp.IsRunning(name) {
			t.Fatalf("%s should have started before dependency loss", name)
		}
	}
	if sp.IsRunning("app-4") {
		t.Fatal("app-4 should be blocked after db dies between wave batches")
	}
	gotLog := stderr.String()
	if !strings.Contains(gotLog, "session=app-4") || !strings.Contains(gotLog, "outcome=blocked_on_dependencies") {
		t.Fatalf("app-4 log = %q, want blocked_on_dependencies", gotLog)
	}
	if strings.Contains(gotLog, "session=app-4 template=app-4 outcome=deferred_by_wake_budget") {
		t.Fatalf("app-4 was deferred by wake budget instead of dependency recheck: %q", gotLog)
	}
}

func TestExecutePlannedStartsTraced_AsyncRevalidatesDependenciesBetweenBatches(t *testing.T) {
	maxWakes := 8
	dropAfter := 3
	sp := &dropDependencyAfterNStartsProvider{
		Fake:      runtime.NewFake(),
		dropAfter: dropAfter,
		depName:   "db",
	}
	if err := sp.Start(context.Background(), "db", runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Daemon: config.DaemonConfig{MaxWakesPerTick: &maxWakes},
		Agents: []config.Agent{
			{Name: "app-1", DependsOn: []string{"db"}},
			{Name: "app-2", DependsOn: []string{"db"}},
			{Name: "app-3", DependsOn: []string{"db"}},
			{Name: "app-4", DependsOn: []string{"db"}},
			{Name: "db", MaxActiveSessions: intPtr(1)},
		},
	}
	store := beads.NewMemStore()
	clk := &clock.Fake{Time: time.Date(2026, 3, 18, 12, 0, 0, 0, time.UTC)}
	desired := map[string]TemplateParams{}
	candidates := make([]startCandidate, 0, 4)
	for _, name := range []string{"app-1", "app-2", "app-3", "app-4"} {
		tp := TemplateParams{Command: name, SessionName: name, TemplateName: name}
		desired[name] = tp
		created, err := store.Create(beads.Bead{
			ID:     name + "-id",
			Title:  name,
			Type:   sessionBeadType,
			Labels: []string{sessionBeadLabel},
			Metadata: creatingMeta(map[string]string{
				"session_name":         name,
				"template":             name,
				"generation":           "1",
				"continuation_epoch":   "1",
				"instance_token":       "tok-" + name,
				"pending_create_claim": "true",
			}),
		})
		if err != nil {
			t.Fatal(err)
		}
		candidate := created
		candidates = append(candidates, startCandidate{session: &candidate, tp: tp})
	}

	woken := executePlannedStartsTraced(
		context.Background(),
		candidates,
		cfg,
		desired,
		sp,
		store,
		"test-city",
		"",
		clk,
		events.Discard,
		5*time.Second,
		ioDiscard{},
		ioDiscard{},
		nil,
		withAsyncStartExecution(),
	)
	if woken != defaultStartDependencyRecheckBatchSize {
		t.Fatalf("first async woken = %d, want dependency-recheck batch size %d", woken, defaultStartDependencyRecheckBatchSize)
	}
	sp.waitForStarts(t, 1+defaultStartDependencyRecheckBatchSize)
	for _, name := range []string{"app-1", "app-2", "app-3"} {
		if !sp.IsRunning(name) {
			t.Fatalf("%s should have started before dependency loss was observed", name)
		}
	}
	if sp.IsRunning("db") {
		t.Fatal("db should have stopped after the dependency provider drop point")
	}
	if sp.IsRunning("app-4") {
		t.Fatal("app-4 should not be enqueued in the async batch after db dies")
	}

	var secondStderr bytes.Buffer
	woken = executePlannedStartsTraced(
		context.Background(),
		[]startCandidate{candidates[3]},
		cfg,
		desired,
		sp,
		store,
		"test-city",
		"",
		clk,
		events.Discard,
		5*time.Second,
		ioDiscard{},
		&secondStderr,
		nil,
		withAsyncStartExecution(),
	)
	if woken != 0 {
		t.Fatalf("second async woken = %d, want 0 after dependency loss", woken)
	}
	if sp.IsRunning("app-4") {
		t.Fatal("app-4 should remain blocked after dependency loss")
	}
	gotLog := secondStderr.String()
	if !strings.Contains(gotLog, "session=app-4") || !strings.Contains(gotLog, "outcome=blocked_on_dependencies") {
		t.Fatalf("app-4 log = %q, want blocked_on_dependencies", gotLog)
	}
}

func TestExecutePlannedStartsTraced_AsyncReturnsBeforeProviderStartCompletes(t *testing.T) {
	store := beads.NewMemStore()
	clk := &clock.Fake{Time: time.Date(2026, 4, 26, 12, 0, 0, 0, time.UTC)}
	session, err := store.Create(beads.Bead{
		ID:     "gc-worker",
		Title:  "worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: creatingMeta(map[string]string{
			"session_name":         "worker",
			"template":             "worker",
			"generation":           "1",
			"continuation_epoch":   "1",
			"instance_token":       "tok-worker",
			"pending_create_claim": "true",
			"last_woke_at":         clk.Now().Format(time.RFC3339),
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	sp := newGatedStartProvider()
	t.Cleanup(func() { sp.release("worker") })
	cfg := &config.City{
		Agents: []config.Agent{{Name: "worker"}},
	}
	tp := TemplateParams{
		Command:      "worker",
		SessionName:  "worker",
		TemplateName: "worker",
	}
	desired := map[string]TemplateParams{"worker": tp}

	done := make(chan int, 1)
	go func() {
		done <- executePlannedStartsTraced(
			context.Background(),
			[]startCandidate{{session: &session, tp: tp}},
			cfg,
			desired,
			sp,
			store,
			"test-city",
			"",
			clk,
			events.Discard,
			time.Minute,
			ioDiscard{},
			ioDiscard{},
			nil,
			withAsyncStartExecution(),
		)
	}()

	select {
	case woken := <-done:
		if woken != 1 {
			t.Fatalf("woken = %d, want 1", woken)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("async planned start blocked waiting for provider Start to finish")
	}
	sp.waitForStarts(t, 1)

	inFlight, err := store.Get(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if inFlight.Metadata["pending_create_claim"] != "true" {
		t.Fatalf("pending_create_claim = %q, want true until async start commits", inFlight.Metadata["pending_create_claim"])
	}
	if inFlight.Metadata["last_woke_at"] == "" {
		t.Fatal("last_woke_at was not stamped before async start")
	}

	sp.release("worker")
	deadline := time.After(2 * time.Second)
	for {
		updated, err := store.Get(session.ID)
		if err != nil {
			t.Fatal(err)
		}
		if updated.Metadata["state"] == "active" && updated.Metadata["pending_create_claim"] == "" {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("async start did not commit active state; metadata=%v", updated.Metadata)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestExecutePlannedStartsTraced_AsyncLimitsEnqueuedStartsPerTick(t *testing.T) {
	store := beads.NewMemStore()
	clk := &clock.Fake{Time: time.Date(2026, 4, 26, 12, 1, 0, 0, time.UTC)}
	sp := newGatedStartProvider()
	maxWakes := 4
	cfg := &config.City{Daemon: config.DaemonConfig{MaxWakesPerTick: &maxWakes}}
	desired := map[string]TemplateParams{}
	var candidates []startCandidate
	for _, name := range []string{"worker-1", "worker-2", "worker-3", "worker-4", "worker-5"} {
		session, err := store.Create(beads.Bead{
			ID:     "gc-" + name,
			Title:  name,
			Type:   sessionBeadType,
			Labels: []string{sessionBeadLabel},
			Metadata: creatingMeta(map[string]string{
				"session_name":         name,
				"template":             name,
				"generation":           "1",
				"continuation_epoch":   "1",
				"instance_token":       "tok-" + name,
				"pending_create_claim": "true",
			}),
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { sp.release(name) })
		cfg.Agents = append(cfg.Agents, config.Agent{Name: name})
		tp := TemplateParams{Command: name, SessionName: name, TemplateName: name}
		desired[name] = tp
		candidates = append(candidates, startCandidate{session: &session, tp: tp})
	}

	woken := executePlannedStartsTraced(
		context.Background(),
		candidates,
		cfg,
		desired,
		sp,
		store,
		"test-city",
		"",
		clk,
		events.Discard,
		time.Minute,
		ioDiscard{},
		ioDiscard{},
		nil,
		withAsyncStartExecution(),
	)
	wantWoken := maxWakes
	if woken != wantWoken {
		t.Fatalf("woken = %d, want configured wake budget %d", woken, wantWoken)
	}
	sp.waitForStarts(t, wantWoken)
	sp.ensureNoFurtherStart(t, 100*time.Millisecond)
	if sp.maxInFlight > wantWoken {
		t.Fatalf("max in-flight starts = %d, want <= %d", sp.maxInFlight, wantWoken)
	}
}

func TestExecutePlannedStartsTraced_AsyncLimiterSharedAcrossTicks(t *testing.T) {
	store := beads.NewMemStore()
	clk := &clock.Fake{Time: time.Date(2026, 4, 26, 12, 1, 15, 0, time.UTC)}
	sp := newGatedStartProvider()
	cfg := &config.City{
		Agents: []config.Agent{{Name: "worker-1"}, {Name: "worker-2"}},
	}
	desired := map[string]TemplateParams{}
	makeCandidate := func(name string) startCandidate {
		session, err := store.Create(beads.Bead{
			ID:     "gc-" + name,
			Title:  name,
			Type:   sessionBeadType,
			Labels: []string{sessionBeadLabel},
			Metadata: creatingMeta(map[string]string{
				"session_name":         name,
				"template":             name,
				"generation":           "1",
				"continuation_epoch":   "1",
				"instance_token":       "tok-" + name,
				"pending_create_claim": "true",
			}),
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { sp.release(name) })
		tp := TemplateParams{Command: name, SessionName: name, TemplateName: name}
		desired[name] = tp
		return startCandidate{session: &session, tp: tp}
	}
	limiter := newAsyncStartLimiter(1)
	first := makeCandidate("worker-1")
	second := makeCandidate("worker-2")

	if got := executePlannedStartsTraced(
		context.Background(),
		[]startCandidate{first},
		cfg,
		desired,
		sp,
		store,
		"test-city",
		"",
		clk,
		events.Discard,
		time.Minute,
		ioDiscard{},
		ioDiscard{},
		nil,
		withAsyncStartExecution(),
		withAsyncStartLimiter(limiter),
	); got != 1 {
		t.Fatalf("first woken = %d, want 1", got)
	}
	sp.waitForStarts(t, 1)
	if got := executePlannedStartsTraced(
		context.Background(),
		[]startCandidate{second},
		cfg,
		desired,
		sp,
		store,
		"test-city",
		"",
		clk,
		events.Discard,
		time.Minute,
		ioDiscard{},
		ioDiscard{},
		nil,
		withAsyncStartExecution(),
		withAsyncStartLimiter(limiter),
	); got != 0 {
		t.Fatalf("second woken = %d, want 0 while shared limiter is full", got)
	}
	sp.ensureNoFurtherStart(t, 100*time.Millisecond)
	deferred, err := store.Get(second.session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := deferred.Metadata["last_woke_at"]; got != "" {
		t.Fatalf("deferred last_woke_at = %q, want empty until limiter slot is reserved", got)
	}
	sp.release("worker-1")
	deadline := time.After(2 * time.Second)
	for {
		updated, err := store.Get(first.session.ID)
		if err != nil {
			t.Fatal(err)
		}
		if updated.Metadata["state"] == "active" {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("first async start did not commit active state; metadata=%v", updated.Metadata)
		case <-time.After(10 * time.Millisecond):
		}
	}
	if got := executePlannedStartsTraced(
		context.Background(),
		[]startCandidate{second},
		cfg,
		desired,
		sp,
		store,
		"test-city",
		"",
		clk,
		events.Discard,
		time.Minute,
		ioDiscard{},
		ioDiscard{},
		nil,
		withAsyncStartExecution(),
		withAsyncStartLimiter(limiter),
	); got != 1 {
		t.Fatalf("second woken after release = %d, want 1", got)
	}
	started := sp.waitForStarts(t, 1)
	if len(started) != 1 || started[0] != "worker-2" {
		t.Fatalf("second start = %v, want [worker-2]", started)
	}
}

func TestExecutePlannedStartsTraced_AsyncLimiterDeferredStartDoesNotRunAfterCancel(t *testing.T) {
	store := beads.NewMemStore()
	clk := &clock.Fake{Time: time.Date(2026, 4, 26, 12, 1, 20, 0, time.UTC)}
	session, err := store.Create(beads.Bead{
		ID:     "gc-worker",
		Title:  "worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: creatingMeta(map[string]string{
			"session_name":         "worker",
			"template":             "worker",
			"generation":           "1",
			"continuation_epoch":   "1",
			"instance_token":       "tok-worker",
			"pending_create_claim": "true",
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	sp := newGatedStartProvider()
	t.Cleanup(func() { sp.release("worker") })
	cfg := &config.City{Agents: []config.Agent{{Name: "worker"}}}
	tp := TemplateParams{Command: "worker", SessionName: "worker", TemplateName: "worker"}
	limiter := newAsyncStartLimiter(1)
	releaseLimiter, reserved, outcome := reserveAsyncStartSlot(context.Background(), limiter)
	if !reserved {
		t.Fatalf("reserve limiter = %s, want success", outcome)
	}
	ctx, cancel := context.WithCancel(context.Background())

	if got := executePlannedStartsTraced(
		ctx,
		[]startCandidate{{session: &session, tp: tp}},
		cfg,
		map[string]TemplateParams{"worker": tp},
		sp,
		store,
		"test-city",
		"",
		clk,
		events.Discard,
		time.Minute,
		ioDiscard{},
		ioDiscard{},
		nil,
		withAsyncStartExecution(),
		withAsyncStartLimiter(limiter),
	); got != 0 {
		t.Fatalf("woken = %d, want 0 while async limiter is full", got)
	}
	cancel()
	releaseLimiter()
	sp.ensureNoFurtherStart(t, 100*time.Millisecond)
	updated, err := store.Get(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := updated.Metadata["last_woke_at"]; got != "" {
		t.Fatalf("last_woke_at = %q, want empty because no async start was queued", got)
	}
}

func TestAsyncStartLimiterNilReceiverMethodsAreNoops(t *testing.T) {
	var limiter *asyncStartLimiter
	limiter.resize(5)
	if got := limiter.capacity(); got != 0 {
		t.Fatalf("nil limiter capacity = %d, want 0", got)
	}
	release, reserved, outcome := reserveAsyncStartSlot(context.Background(), limiter)
	if !reserved || outcome != "" {
		t.Fatalf("nil limiter reserve = reserved %v outcome %q, want success", reserved, outcome)
	}
	release()
}

func TestCityRuntimeShutdownWaitsForTrackedAsyncStartsBeforeStopSnapshot(t *testing.T) {
	store := beads.NewMemStore()
	clk := &clock.Fake{Time: time.Date(2026, 4, 26, 12, 1, 25, 0, time.UTC)}
	session, err := store.Create(beads.Bead{
		ID:     "gc-worker",
		Title:  "worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: creatingMeta(map[string]string{
			"session_name":         "worker",
			"template":             "worker",
			"generation":           "1",
			"continuation_epoch":   "1",
			"instance_token":       "tok-worker",
			"pending_create_claim": "true",
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	sp := newShutdownWaitProvider()
	t.Cleanup(func() { sp.release("worker") })
	cfg := &config.City{
		Daemon: config.DaemonConfig{ShutdownTimeout: "500ms"},
		Agents: []config.Agent{{Name: "worker"}},
	}
	cr := &CityRuntime{
		cfg:                 cfg,
		sp:                  sp,
		rec:                 events.Discard,
		standaloneCityStore: store,
		asyncStartLimiter:   newAsyncStartLimiter(maxParallelStartsPerTick(cfg)),
		logPrefix:           "gc test",
		stdout:              ioDiscard{},
		stderr:              ioDiscard{},
	}
	tp := TemplateParams{Command: "worker", SessionName: "worker", TemplateName: "worker"}
	if got := executePlannedStartsTraced(
		context.Background(),
		[]startCandidate{{session: &session, tp: tp}},
		cfg,
		map[string]TemplateParams{"worker": tp},
		sp,
		store,
		"test-city",
		"",
		clk,
		events.Discard,
		time.Minute,
		ioDiscard{},
		ioDiscard{},
		nil,
		withAsyncStartExecution(),
		withAsyncStartLimiter(cr.ensureAsyncStartLimiter()),
		withAsyncStartTracker(&cr.asyncStarts),
	); got != 1 {
		t.Fatalf("woken = %d, want 1", got)
	}
	sp.waitForStarts(t, 1)

	shutdownDone := make(chan struct{})
	go func() {
		cr.shutdown()
		close(shutdownDone)
	}()
	select {
	case <-sp.listCalled:
		t.Fatal("shutdown listed running sessions before the async start completed")
	case <-shutdownDone:
		t.Fatal("shutdown returned before the async start completed")
	case <-time.After(100 * time.Millisecond):
	}

	sp.release("worker")
	select {
	case <-shutdownDone:
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown did not finish after the async start completed")
	}
	select {
	case <-sp.listCalled:
	default:
		t.Fatal("shutdown did not list running sessions after waiting for async starts")
	}
	if sp.IsRunning("worker") {
		t.Fatal("shutdown should stop the runtime that the async start created")
	}
}

func TestExecutePlannedStartsTraced_AsyncPrepareFailureClearsPreWakeLease(t *testing.T) {
	store := &failSetMetadataStore{MemStore: beads.NewMemStore(), failKey: "session_key"}
	clk := &clock.Fake{Time: time.Date(2026, 4, 26, 12, 1, 27, 0, time.UTC)}
	session, err := store.Create(beads.Bead{
		ID:     "gc-worker",
		Title:  "worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: creatingMeta(map[string]string{
			"session_name":         "worker",
			"template":             "worker",
			"generation":           "1",
			"continuation_epoch":   "1",
			"instance_token":       "tok-worker",
			"pending_create_claim": "true",
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	sp := newGatedStartProvider()
	t.Cleanup(func() { sp.release("worker") })
	cfg := &config.City{Agents: []config.Agent{{Name: "worker"}}}
	tp := TemplateParams{
		Command:          "worker",
		SessionName:      "worker",
		TemplateName:     "worker",
		ResolvedProvider: &config.ResolvedProvider{SessionIDFlag: "--session-id"},
	}
	if got := executePlannedStartsTraced(
		context.Background(),
		[]startCandidate{{session: &session, tp: tp}},
		cfg,
		map[string]TemplateParams{"worker": tp},
		sp,
		store,
		"test-city",
		"",
		clk,
		events.Discard,
		time.Minute,
		ioDiscard{},
		ioDiscard{},
		nil,
		withAsyncStartExecution(),
	); got != 0 {
		t.Fatalf("woken = %d, want 0 when async preparation fails after preWake", got)
	}
	sp.ensureNoFurtherStart(t, 100*time.Millisecond)
	updated, err := store.Get(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := updated.Metadata["last_woke_at"]; got != "" {
		t.Fatalf("last_woke_at = %q, want cleared after async preparation failure", got)
	}
}

func TestExecutePlannedStartsTraced_AsyncRequestsFollowUpAfterCommit(t *testing.T) {
	store := beads.NewMemStore()
	clk := &clock.Fake{Time: time.Date(2026, 4, 26, 12, 1, 30, 0, time.UTC)}
	session, err := store.Create(beads.Bead{
		ID:     "gc-worker",
		Title:  "worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: creatingMeta(map[string]string{
			"session_name":         "worker",
			"template":             "worker",
			"generation":           "1",
			"continuation_epoch":   "1",
			"instance_token":       "tok-worker",
			"pending_create_claim": "true",
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	sp := newGatedStartProvider()
	t.Cleanup(func() { sp.release("worker") })
	cfg := &config.City{Agents: []config.Agent{{Name: "worker"}}}
	tp := TemplateParams{Command: "worker", SessionName: "worker", TemplateName: "worker"}
	followUp := make(chan struct{}, 1)

	woken := executePlannedStartsTraced(
		context.Background(),
		[]startCandidate{{session: &session, tp: tp}},
		cfg,
		map[string]TemplateParams{"worker": tp},
		sp,
		store,
		"test-city",
		"",
		clk,
		events.Discard,
		time.Minute,
		ioDiscard{},
		ioDiscard{},
		nil,
		withAsyncStartExecution(),
		withAsyncStartFollowUp(func() {
			select {
			case followUp <- struct{}{}:
			default:
			}
		}),
	)
	if woken != 1 {
		t.Fatalf("woken = %d, want 1", woken)
	}
	sp.waitForStarts(t, 1)
	select {
	case <-followUp:
		t.Fatal("follow-up requested before async provider start finished")
	case <-time.After(100 * time.Millisecond):
	}

	sp.release("worker")
	select {
	case <-followUp:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for async completion follow-up")
	}
}

func TestAllDependenciesAliveForTemplate_TreatsPendingCreateDependencyAsNotAlive(t *testing.T) {
	store := beads.NewMemStore()
	now := time.Now().UTC()
	dep, err := store.Create(beads.Bead{
		ID:     "gc-db",
		Title:  "db",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: creatingMeta(map[string]string{
			"session_name":         "db",
			"template":             "db",
			"generation":           "1",
			"continuation_epoch":   "1",
			"instance_token":       "tok-db",
			"pending_create_claim": "true",
			"last_woke_at":         now.Format(time.RFC3339),
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "db", runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "worker", DependsOn: []string{"db"}},
			{Name: "db"},
		},
	}
	desired := map[string]TemplateParams{
		"worker": {Command: "worker", SessionName: "worker", TemplateName: "worker"},
		"db":     {Command: "db", SessionName: "db", TemplateName: "db"},
	}

	if allDependenciesAliveForTemplate("worker", cfg, desired, sp, "test-city", store) {
		t.Fatal("worker dependency should stay blocked while db start is still in flight")
	}
	if err := store.SetMetadataBatch(dep.ID, map[string]string{
		"state":                string(sessionpkg.StateActive),
		"pending_create_claim": "",
		"creation_complete_at": now.Add(time.Second).Format(time.RFC3339),
	}); err != nil {
		t.Fatal(err)
	}
	if !allDependenciesAliveForTemplate("worker", cfg, desired, sp, "test-city", store) {
		t.Fatal("worker dependency should be alive after db start is committed")
	}
}

func TestDependencySessionStartInFlightIgnoresClosedMetadataMatches(t *testing.T) {
	now := time.Now().UTC()
	store := &closedMetadataMatchStore{
		MemStore: beads.NewMemStore(),
		matches: []beads.Bead{{
			ID:     "gc-db-old",
			Title:  "db",
			Status: "closed",
			Type:   sessionBeadType,
			Labels: []string{sessionBeadLabel},
			Metadata: creatingMeta(map[string]string{
				"session_name":         "db",
				"template":             "db",
				"pending_create_claim": "true",
				"last_woke_at":         now.Format(time.RFC3339),
			}),
		}},
	}

	if dependencySessionStartInFlight(store, "db", &config.City{}, clock.Real{}) {
		t.Fatal("closed failed-create bead should not count as an in-flight dependency start")
	}
}

func TestDependencySessionStartInFlightFailsClosedOnMetadataListError(t *testing.T) {
	store := &listMetadataErrorStore{MemStore: beads.NewMemStore()}
	if !dependencySessionStartInFlight(store, "db", &config.City{}, clock.Real{}) {
		t.Fatal("metadata query errors should block dependent starts until the store recovers")
	}
}

func TestPendingCreateStartInFlight_ZeroStartupTimeoutUsesRecoveryLease(t *testing.T) {
	now := time.Date(2026, 4, 26, 12, 1, 40, 0, time.UTC)
	recent := beads.Bead{
		Metadata: map[string]string{
			"pending_create_claim": "true",
			"last_woke_at":         now.Add(-10 * time.Second).Format(time.RFC3339),
		},
	}
	if !pendingCreateStartInFlight(recent, &clock.Fake{Time: now}, 0) {
		t.Fatal("explicit zero startup timeout should still use a finite recovery lease while recent")
	}
	stale := beads.Bead{
		Metadata: map[string]string{
			"pending_create_claim": "true",
			"last_woke_at":         now.Add(-24 * time.Hour).Format(time.RFC3339),
		},
	}
	if pendingCreateStartInFlight(stale, &clock.Fake{Time: now}, 0) {
		t.Fatal("explicit zero startup timeout should not suppress recovery forever")
	}
}

func TestAsyncStartTrackerWaitZeroDoesNotBlock(t *testing.T) {
	var tracker asyncStartTracker
	done, ok := tracker.start()
	if !ok {
		t.Fatal("tracker should accept work before shutdown")
	}
	if tracker.wait(0) {
		t.Fatal("zero-timeout wait should not report completion while async work is still running")
	}
	done()
	if !tracker.wait(time.Second) {
		t.Fatal("tracker should report completion after async work finishes")
	}
}

func TestReconcileSessionBeads_RollsBackPendingCreateWhenRuntimeTokenMismatches(t *testing.T) {
	store := beads.NewMemStore()
	clk := &clock.Fake{Time: time.Date(2026, 4, 26, 12, 1, 45, 0, time.UTC)}
	session, err := store.Create(beads.Bead{
		ID:     "gc-worker",
		Title:  "worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: creatingMeta(map[string]string{
			"session_name":         "worker",
			"template":             "worker",
			"generation":           "2",
			"continuation_epoch":   "1",
			"instance_token":       "tok-new",
			"pending_create_claim": "true",
			"last_woke_at":         clk.Now().Format(time.RFC3339),
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "worker", runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	if err := sp.SetMeta("worker", "GC_SESSION_ID", session.ID); err != nil {
		t.Fatal(err)
	}
	if err := sp.SetMeta("worker", "GC_INSTANCE_TOKEN", "tok-old"); err != nil {
		t.Fatal(err)
	}
	if err := sp.SetMeta("worker", "GC_RUNTIME_EPOCH", "1"); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{Agents: []config.Agent{{Name: "worker"}}}
	tp := TemplateParams{Command: "worker", SessionName: "worker", TemplateName: "worker"}

	woken := reconcileSessionBeads(
		context.Background(),
		[]beads.Bead{session},
		map[string]TemplateParams{"worker": tp},
		configuredSessionNames(cfg, "test-city", store),
		cfg,
		sp,
		store,
		nil,
		nil,
		nil,
		newDrainTracker(),
		map[string]int{"worker": 1},
		false,
		map[string]bool{"worker": true},
		"test-city",
		nil,
		clk,
		events.Discard,
		time.Minute,
		0,
		ioDiscard{},
		ioDiscard{},
	)
	if woken != 0 {
		t.Fatalf("woken = %d, want 0", woken)
	}
	updated, err := store.Get(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != "closed" {
		t.Fatalf("status = %q, want closed so stale runtime is not recovered", updated.Status)
	}
}

func TestRunningSessionMatchesPendingCreateAcceptsTokenOnlyRuntime(t *testing.T) {
	session := &beads.Bead{
		ID: "gc-worker",
		Metadata: map[string]string{
			"session_name":   "worker",
			"generation":     "2",
			"instance_token": "tok-worker",
		},
	}
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "worker", runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	if err := sp.SetMeta("worker", "GC_INSTANCE_TOKEN", "tok-worker"); err != nil {
		t.Fatal(err)
	}

	if !runningSessionMatchesPendingCreate(session, "worker", sp) {
		t.Fatal("runtime with matching token and no session id should match pending create")
	}
}

func TestRunningSessionMatchesPendingCreateAcceptsIDOnlyRuntime(t *testing.T) {
	session := &beads.Bead{
		ID: "gc-worker",
		Metadata: map[string]string{
			"session_name": "worker",
			"generation":   "2",
		},
	}
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "worker", runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	if err := sp.SetMeta("worker", "GC_SESSION_ID", session.ID); err != nil {
		t.Fatal(err)
	}

	if !runningSessionMatchesPendingCreate(session, "worker", sp) {
		t.Fatal("runtime with matching session id and no token should match pending create")
	}
}

func TestReconcileSessionBeads_SkipsPendingCreateStartAlreadyInFlight(t *testing.T) {
	store := beads.NewMemStore()
	clk := &clock.Fake{Time: time.Date(2026, 4, 26, 12, 0, 30, 0, time.UTC)}
	session, err := store.Create(beads.Bead{
		ID:     "gc-worker",
		Title:  "worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: creatingMeta(map[string]string{
			"session_name":         "worker",
			"template":             "worker",
			"generation":           "2",
			"continuation_epoch":   "1",
			"instance_token":       "tok-worker",
			"pending_create_claim": "true",
			"last_woke_at":         clk.Now().Add(-10 * time.Second).UTC().Format(time.RFC3339),
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	sp := newGatedStartProvider()
	cfg := &config.City{
		Agents: []config.Agent{{Name: "worker"}},
	}
	tp := TemplateParams{
		Command:      "worker",
		SessionName:  "worker",
		TemplateName: "worker",
	}
	woken := reconcileSessionBeads(
		context.Background(),
		[]beads.Bead{session},
		map[string]TemplateParams{"worker": tp},
		configuredSessionNames(cfg, "", store),
		cfg,
		sp,
		store,
		nil,
		nil,
		nil,
		newDrainTracker(),
		map[string]int{"worker": 1},
		false,
		map[string]bool{"worker": true},
		"test-city",
		nil,
		clk,
		events.Discard,
		time.Minute,
		0,
		ioDiscard{},
		ioDiscard{},
	)
	if woken != 0 {
		t.Fatalf("woken = %d, want 0 while start is already in flight", woken)
	}
	sp.ensureNoFurtherStart(t, 100*time.Millisecond)
}

func TestCommitAsyncStartResult_IgnoresStaleSessionSnapshot(t *testing.T) {
	store := beads.NewMemStore()
	clk := &clock.Fake{Time: time.Date(2026, 4, 26, 12, 2, 0, 0, time.UTC)}
	session, err := store.Create(beads.Bead{
		ID:     "gc-worker",
		Title:  "worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: creatingMeta(map[string]string{
			"session_name":         "worker",
			"template":             "worker",
			"generation":           "2",
			"continuation_epoch":   "1",
			"instance_token":       "tok-old",
			"pending_create_claim": "true",
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetMetadataBatch(session.ID, map[string]string{
		"generation":     "3",
		"instance_token": "tok-new",
	}); err != nil {
		t.Fatal(err)
	}
	result := startResult{
		prepared: preparedStart{
			candidate: startCandidate{
				session: &session,
				tp: TemplateParams{
					Command:      "worker",
					SessionName:  "worker",
					TemplateName: "worker",
				},
			},
		},
		outcome:  "success",
		started:  clk.Now(),
		finished: clk.Now(),
	}

	if commitAsyncStartResultWithContext(context.Background(), result, nil, store, clk, events.Discard, 0, ioDiscard{}, ioDiscard{}, nil) {
		t.Fatal("stale async start result should not commit")
	}
	updated, err := store.Get(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := updated.Metadata["state"]; got != "creating" {
		t.Fatalf("state = %q, want creating", got)
	}
	if got := updated.Metadata["pending_create_claim"]; got != "true" {
		t.Fatalf("pending_create_claim = %q, want true", got)
	}
	if got := updated.Metadata["instance_token"]; got != "tok-new" {
		t.Fatalf("instance_token = %q, want tok-new", got)
	}
}

func TestCommitAsyncStartResult_IgnoresClosedSessionSnapshot(t *testing.T) {
	store := beads.NewMemStore()
	clk := &clock.Fake{Time: time.Date(2026, 4, 26, 12, 2, 30, 0, time.UTC)}
	session, err := store.Create(beads.Bead{
		ID:     "gc-worker",
		Title:  "worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: creatingMeta(map[string]string{
			"session_name":         "worker",
			"template":             "worker",
			"generation":           "2",
			"continuation_epoch":   "1",
			"instance_token":       "tok-worker",
			"pending_create_claim": "true",
			"last_woke_at":         clk.Now().Format(time.RFC3339),
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(session.ID); err != nil {
		t.Fatal(err)
	}
	result := startResult{
		prepared: preparedStart{
			candidate: startCandidate{
				session: &session,
				tp: TemplateParams{
					Command:      "worker",
					SessionName:  "worker",
					TemplateName: "worker",
				},
			},
		},
		outcome:  "success",
		started:  clk.Now(),
		finished: clk.Now(),
	}

	if commitAsyncStartResultWithContext(context.Background(), result, nil, store, clk, events.Discard, 0, ioDiscard{}, ioDiscard{}, nil) {
		t.Fatal("closed async start result should not commit")
	}
	updated, err := store.Get(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != "closed" {
		t.Fatalf("status = %q, want closed", updated.Status)
	}
	if got := updated.Metadata["state"]; got != "creating" {
		t.Fatalf("state = %q, want creating", got)
	}
	if got := updated.Metadata["pending_create_claim"]; got != "true" {
		t.Fatalf("pending_create_claim = %q, want true", got)
	}
}

func TestCommitAsyncStartResult_StopsMatchingRuntimeForStaleSnapshot(t *testing.T) {
	store := beads.NewMemStore()
	clk := &clock.Fake{Time: time.Date(2026, 4, 26, 12, 2, 45, 0, time.UTC)}
	session, err := store.Create(beads.Bead{
		ID:     "gc-worker",
		Title:  "worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: creatingMeta(map[string]string{
			"session_name":         "worker",
			"template":             "worker",
			"generation":           "2",
			"continuation_epoch":   "1",
			"instance_token":       "tok-old",
			"pending_create_claim": "true",
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetMetadataBatch(session.ID, map[string]string{
		"generation":     "3",
		"instance_token": "tok-new",
	}); err != nil {
		t.Fatal(err)
	}
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "worker", runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	if err := sp.SetMeta("worker", "GC_SESSION_ID", session.ID); err != nil {
		t.Fatal(err)
	}
	if err := sp.SetMeta("worker", "GC_INSTANCE_TOKEN", "tok-old"); err != nil {
		t.Fatal(err)
	}
	if err := sp.SetMeta("worker", "GC_RUNTIME_EPOCH", "2"); err != nil {
		t.Fatal(err)
	}
	result := startResult{
		prepared: preparedStart{
			candidate: startCandidate{
				session: &session,
				tp: TemplateParams{
					Command:      "worker",
					SessionName:  "worker",
					TemplateName: "worker",
				},
			},
		},
		outcome:  "success",
		started:  clk.Now(),
		finished: clk.Now(),
	}

	if commitAsyncStartResultWithContext(context.Background(), result, sp, store, clk, events.Discard, 0, ioDiscard{}, ioDiscard{}, nil) {
		t.Fatal("stale async start result should not commit")
	}
	if sp.IsRunning("worker") {
		t.Fatal("stale runtime with matching old session metadata should be stopped")
	}
}

func TestCommitAsyncStartResult_IgnoresCommandChangedDuringStartup(t *testing.T) {
	store := beads.NewMemStore()
	clk := &clock.Fake{Time: time.Date(2026, 4, 28, 13, 6, 0, 0, time.UTC)}
	session, err := store.Create(beads.Bead{
		ID:     "gc-drifter",
		Title:  "drifter",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: creatingMeta(map[string]string{
			"session_name":         "drifter",
			"template":             "drifter",
			"generation":           "2",
			"continuation_epoch":   "1",
			"instance_token":       "tok-drifter",
			"pending_create_claim": "true",
			"last_woke_at":         clk.Now().Format(time.RFC3339),
			"command":              "CUSTOM_VERSION=v1 report",
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetMetadata(session.ID, "command", "CUSTOM_VERSION=v2 report"); err != nil {
		t.Fatal(err)
	}
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "drifter", runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	if err := sp.SetMeta("drifter", "GC_SESSION_ID", session.ID); err != nil {
		t.Fatal(err)
	}
	if err := sp.SetMeta("drifter", "GC_INSTANCE_TOKEN", "tok-drifter"); err != nil {
		t.Fatal(err)
	}
	if err := sp.SetMeta("drifter", "GC_RUNTIME_EPOCH", "2"); err != nil {
		t.Fatal(err)
	}
	result := startResult{
		prepared: preparedStart{
			candidate: startCandidate{
				session: &session,
				tp: TemplateParams{
					Command:      "CUSTOM_VERSION=v1 report",
					SessionName:  "drifter",
					TemplateName: "drifter",
				},
			},
			coreHash: "core-v1",
			liveHash: "live-v1",
		},
		outcome:  "success",
		started:  clk.Now(),
		finished: clk.Now(),
	}

	if commitAsyncStartResultWithContext(context.Background(), result, sp, store, clk, events.Discard, 0, ioDiscard{}, ioDiscard{}, nil) {
		t.Fatal("async start with stale command should not commit")
	}
	updated, err := store.Get(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if sp.IsRunning("drifter") {
		t.Fatal("stale runtime with old command should be stopped")
	}
	if got := updated.Metadata["started_config_hash"]; got != "" {
		t.Fatalf("started_config_hash = %q, want empty until fresh command starts", got)
	}
	if got := updated.Metadata["last_woke_at"]; got != "" {
		t.Fatalf("last_woke_at = %q, want cleared so the new command can retry next tick", got)
	}
	if got := updated.Metadata["pending_create_claim"]; got != "true" {
		t.Fatalf("pending_create_claim = %q, want true for pending-create retry", got)
	}
	if got := updated.Metadata["command"]; got != "CUSTOM_VERSION=v2 report" {
		t.Fatalf("command = %q, want current config preserved", got)
	}
}

func TestCommitAsyncStartResult_PreservesRuntimeWhenRefreshFails(t *testing.T) {
	store := &getErrorStore{MemStore: beads.NewMemStore()}
	clk := &clock.Fake{Time: time.Date(2026, 4, 26, 12, 2, 50, 0, time.UTC)}
	session, err := store.Create(beads.Bead{
		ID:     "gc-worker",
		Title:  "worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: creatingMeta(map[string]string{
			"session_name":         "worker",
			"template":             "worker",
			"generation":           "2",
			"continuation_epoch":   "1",
			"instance_token":       "tok-worker",
			"pending_create_claim": "true",
			"last_woke_at":         clk.Now().Format(time.RFC3339),
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "worker", runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	if err := sp.SetMeta("worker", "GC_SESSION_ID", session.ID); err != nil {
		t.Fatal(err)
	}
	if err := sp.SetMeta("worker", "GC_INSTANCE_TOKEN", "tok-worker"); err != nil {
		t.Fatal(err)
	}
	result := startResult{
		prepared: preparedStart{
			candidate: startCandidate{
				session: &session,
				tp: TemplateParams{
					Command:      "worker",
					SessionName:  "worker",
					TemplateName: "worker",
				},
			},
		},
		outcome:  "success",
		started:  clk.Now(),
		finished: clk.Now(),
	}

	if commitAsyncStartResultWithContext(context.Background(), result, sp, store, clk, events.Discard, 0, ioDiscard{}, ioDiscard{}, nil) {
		t.Fatal("async result should not commit when refresh fails")
	}
	if !sp.IsRunning("worker") {
		t.Fatal("refresh failure should not stop a runtime without proving staleness")
	}
	updated, err := store.MemStore.Get(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := updated.Metadata["last_woke_at"]; got != "" {
		t.Fatalf("last_woke_at = %q, want cleared so the next tick can recover or retry", got)
	}
}

func TestCommitAsyncStartResult_RecoversCommitPanic(t *testing.T) {
	store := &panicMetadataBatchStore{MemStore: beads.NewMemStore()}
	clk := &clock.Fake{Time: time.Date(2026, 4, 26, 12, 3, 0, 0, time.UTC)}
	session, err := store.Create(beads.Bead{
		ID:     "gc-worker",
		Title:  "worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: creatingMeta(map[string]string{
			"session_name":         "worker",
			"template":             "worker",
			"generation":           "2",
			"continuation_epoch":   "1",
			"instance_token":       "tok-worker",
			"pending_create_claim": "true",
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	result := startResult{
		prepared: preparedStart{
			candidate: startCandidate{
				session: &session,
				tp: TemplateParams{
					Command:      "worker",
					SessionName:  "worker",
					TemplateName: "worker",
				},
			},
		},
		outcome:  "success",
		started:  clk.Now(),
		finished: clk.Now(),
	}

	if commitAsyncStartResultWithContext(context.Background(), result, nil, store, clk, events.Discard, 0, ioDiscard{}, ioDiscard{}, nil) {
		t.Fatal("async commit with panic should report not committed")
	}
	updated, err := store.Get(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := updated.Metadata["last_woke_at"]; got != "" {
		t.Fatalf("last_woke_at = %q, want cleared after async commit panic", got)
	}
}

func TestCommitAsyncStartResultWithContext_SkipsCanceledCommit(t *testing.T) {
	store := beads.NewMemStore()
	clk := &clock.Fake{Time: time.Date(2026, 4, 26, 12, 4, 0, 0, time.UTC)}
	session, err := store.Create(beads.Bead{
		ID:     "gc-worker",
		Title:  "worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: creatingMeta(map[string]string{
			"session_name":         "worker",
			"template":             "worker",
			"generation":           "2",
			"continuation_epoch":   "1",
			"instance_token":       "tok-worker",
			"pending_create_claim": "true",
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	result := startResult{
		prepared: preparedStart{
			candidate: startCandidate{
				session: &session,
				tp: TemplateParams{
					Command:      "worker",
					SessionName:  "worker",
					TemplateName: "worker",
				},
			},
		},
		outcome:  "success",
		started:  clk.Now(),
		finished: clk.Now(),
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if commitAsyncStartResultWithContext(ctx, result, nil, store, clk, events.Discard, 0, ioDiscard{}, ioDiscard{}, nil) {
		t.Fatal("canceled async commit should report not committed")
	}
	updated, err := store.Get(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := updated.Metadata["state"]; got != "creating" {
		t.Fatalf("state = %q, want creating", got)
	}
	if got := updated.Metadata["pending_create_claim"]; got != "true" {
		t.Fatalf("pending_create_claim = %q, want true", got)
	}
}

func TestCommitAsyncStartResultWithContext_StopsCanceledSuccessfulPendingCreateRuntime(t *testing.T) {
	store := beads.NewMemStore()
	clk := &clock.Fake{Time: time.Date(2026, 4, 26, 12, 4, 15, 0, time.UTC)}
	session, err := store.Create(beads.Bead{
		ID:     "gc-worker",
		Title:  "worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: creatingMeta(map[string]string{
			"session_name":         "worker",
			"template":             "worker",
			"generation":           "2",
			"continuation_epoch":   "1",
			"instance_token":       "tok-worker",
			"pending_create_claim": "true",
			"last_woke_at":         clk.Now().Format(time.RFC3339),
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "worker", runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	if err := sp.SetMeta("worker", "GC_SESSION_ID", session.ID); err != nil {
		t.Fatal(err)
	}
	if err := sp.SetMeta("worker", "GC_INSTANCE_TOKEN", "tok-worker"); err != nil {
		t.Fatal(err)
	}
	if err := sp.SetMeta("worker", "GC_RUNTIME_EPOCH", "2"); err != nil {
		t.Fatal(err)
	}
	result := startResult{
		prepared: preparedStart{
			candidate: startCandidate{
				session: &session,
				tp: TemplateParams{
					Command:      "worker",
					SessionName:  "worker",
					TemplateName: "worker",
				},
			},
		},
		outcome:  "success",
		started:  clk.Now(),
		finished: clk.Now(),
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if commitAsyncStartResultWithContext(ctx, result, sp, store, clk, events.Discard, 0, ioDiscard{}, ioDiscard{}, nil) {
		t.Fatal("canceled async success should report not committed")
	}
	if sp.IsRunning("worker") {
		t.Fatal("canceled async success should stop the runtime it started")
	}
	updated, err := store.Get(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := updated.Metadata["last_woke_at"]; got != "" {
		t.Fatalf("last_woke_at = %q, want cleared so the next controller can retry", got)
	}
	if got := updated.Metadata["pending_create_claim"]; got != "true" {
		t.Fatalf("pending_create_claim = %q, want true for next-tick retry", got)
	}
}

func TestCommitAsyncStartResultWithContext_RollsBackCanceledPendingCreateError(t *testing.T) {
	store := beads.NewMemStore()
	clk := &clock.Fake{Time: time.Date(2026, 4, 26, 12, 4, 30, 0, time.UTC)}
	session, err := store.Create(beads.Bead{
		ID:     "gc-worker",
		Title:  "worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: creatingMeta(map[string]string{
			"session_name":         "worker",
			"template":             "worker",
			"generation":           "2",
			"continuation_epoch":   "1",
			"instance_token":       "tok-worker",
			"pending_create_claim": "true",
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	result := startResult{
		prepared: preparedStart{
			candidate: startCandidate{
				session: &session,
				tp: TemplateParams{
					Command:      "worker",
					SessionName:  "worker",
					TemplateName: "worker",
				},
			},
		},
		err:             context.Canceled,
		outcome:         "canceled",
		started:         clk.Now(),
		finished:        clk.Now(),
		rollbackPending: true,
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if commitAsyncStartResultWithContext(ctx, result, nil, store, clk, events.Discard, 0, ioDiscard{}, ioDiscard{}, nil) {
		t.Fatal("canceled async error commit should report not committed")
	}
	updated, err := store.Get(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != "closed" {
		t.Fatalf("status = %q, want closed so pending-create can be retried by replacement bead", updated.Status)
	}
}

func TestCommitStartResult_SessionInitializingClearsInFlightLease(t *testing.T) {
	store := beads.NewMemStore()
	clk := &clock.Fake{Time: time.Date(2026, 4, 26, 12, 5, 0, 0, time.UTC)}
	session, err := store.Create(beads.Bead{
		ID:     "gc-worker",
		Title:  "worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: creatingMeta(map[string]string{
			"session_name":         "worker",
			"template":             "worker",
			"generation":           "2",
			"continuation_epoch":   "1",
			"instance_token":       "tok-worker",
			"pending_create_claim": "true",
			"last_woke_at":         clk.Now().Format(time.RFC3339),
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	result := startResult{
		prepared: preparedStart{
			candidate: startCandidate{
				session: &session,
				tp: TemplateParams{
					Command:      "worker",
					SessionName:  "worker",
					TemplateName: "worker",
				},
			},
		},
		outcome:         "session_initializing",
		started:         clk.Now(),
		finished:        clk.Now(),
		rollbackPending: true,
	}

	if commitStartResult(result, store, clk, events.Discard, 0, ioDiscard{}, ioDiscard{}) {
		t.Fatal("session_initializing result should not count as committed")
	}
	updated, err := store.Get(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != "open" {
		t.Fatalf("status = %q, want open", updated.Status)
	}
	if got := updated.Metadata["last_woke_at"]; got != "" {
		t.Fatalf("last_woke_at = %q, want cleared for next-tick retry", got)
	}
	if got := updated.Metadata["pending_create_claim"]; got != "true" {
		t.Fatalf("pending_create_claim = %q, want true", got)
	}
}

func TestCommitStartResult_RollbackPendingErrorClearsInFlightLeaseWhenCloseFails(t *testing.T) {
	store := &failingCloseStore{MemStore: beads.NewMemStore()}
	clk := &clock.Fake{Time: time.Date(2026, 4, 28, 13, 0, 0, 0, time.UTC)}
	session, err := store.Create(beads.Bead{
		ID:     "gc-shortlived",
		Title:  "shortlived",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: creatingMeta(map[string]string{
			"session_name":         "shortlived",
			"template":             "shortlived",
			"generation":           "2",
			"instance_token":       "tok-shortlived",
			"pending_create_claim": "true",
			"last_woke_at":         clk.Now().Format(time.RFC3339),
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	result := startResult{
		prepared: preparedStart{
			candidate: startCandidate{
				session: &session,
				tp: TemplateParams{
					Command:      "exit 0",
					SessionName:  "shortlived",
					TemplateName: "shortlived",
				},
			},
		},
		err:             errors.New("session died during startup"),
		outcome:         "provider_error",
		started:         clk.Now(),
		finished:        clk.Now(),
		rollbackPending: true,
	}

	if commitStartResult(result, store, clk, events.Discard, 0, ioDiscard{}, ioDiscard{}) {
		t.Fatal("rollback-pending error should not count as committed")
	}
	updated, err := store.Get(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != "open" {
		t.Fatalf("status = %q, want open after injected close failure", updated.Status)
	}
	if got := updated.Metadata["last_woke_at"]; got != "" {
		t.Fatalf("last_woke_at = %q, want cleared so the next reconciler tick can retry", got)
	}
	if got := updated.Metadata["pending_create_claim"]; got != "true" {
		t.Fatalf("pending_create_claim = %q, want true for pending-create retry", got)
	}
	if pendingCreateStartInFlight(updated, clk, 0) {
		t.Fatal("rollback-pending error left the pending-create bead leased")
	}
}

// When the atomic start batch fails, NO state change lands: state stays
// "creating", pending_create_claim stays "true", and the post-create marker
// is absent. The reconciler's next tick retries via recoverRunningPendingCreate.
// This is the intentional consequence of folding the claim clear into the
// same SetMetadataBatch as the state/state_reason/creation_complete_at
// transition so the sweep never observes a transient state without either
// the claim or the marker.
func TestCommitStartResult_AtomicBatchFailureLeavesClaimIntact(t *testing.T) {
	store := &failingMetadataBatchStore{MemStore: beads.NewMemStore(), failBatch: true}
	bead, err := store.Create(beads.Bead{
		Title:  "helper",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":          "sky",
			"session_name_explicit": "true",
			"pending_create_claim":  "true",
			"state":                 "creating",
			"last_woke_at":          "2026-03-18T12:00:00Z",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	result := startResult{
		prepared: preparedStart{
			candidate: startCandidate{
				session: &bead,
				tp: TemplateParams{
					SessionName:  "sky",
					TemplateName: "helper",
				},
			},
			coreHash: "core",
			liveHash: "live",
		},
		outcome:  "success",
		started:  time.Date(2026, 3, 18, 12, 0, 0, 0, time.UTC),
		finished: time.Date(2026, 3, 18, 12, 0, 1, 0, time.UTC),
	}

	ok := commitStartResult(result, store, &clock.Fake{Time: time.Date(2026, 3, 18, 12, 0, 1, 0, time.UTC)}, events.Discard, 0, ioDiscard{}, ioDiscard{})
	if ok {
		t.Fatal("commitStartResult returned true, want false when metadata batch fails (state transition lost)")
	}

	got, err := store.Get(bead.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Metadata["pending_create_claim"] != "true" {
		t.Fatalf("pending_create_claim = %q, want preserved (atomic batch failed, state unchanged)", got.Metadata["pending_create_claim"])
	}
	if got.Metadata["state"] != "creating" {
		t.Fatalf("state = %q, want creating (atomic batch failed)", got.Metadata["state"])
	}
	if got.Metadata["creation_complete_at"] != "" {
		t.Fatalf("creation_complete_at = %q, want empty (atomic batch failed)", got.Metadata["creation_complete_at"])
	}
	if got.Metadata["last_woke_at"] != "" {
		t.Fatalf("last_woke_at = %q, want cleared so a failed metadata commit can retry", got.Metadata["last_woke_at"])
	}
}

func TestRefreshConfiguredNamedStartCandidateAddsCurrentSkillFingerprint(t *testing.T) {
	resetSkillCatalogCache()
	cityPath := t.TempDir()
	writeTemplateResolveCityConfig(t, cityPath, "file")
	if err := os.WriteFile(filepath.Join(cityPath, "pack.toml"),
		[]byte("[pack]\nname = \"named-refresh-test\"\nversion = \"0.1.0\"\nschema = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	skillDir := filepath.Join(cityPath, "skills", "plan")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"),
		[]byte("---\nname: plan\ndescription: test skill\n---\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{
		Workspace:     config.Workspace{Name: "test-city", Provider: "claude"},
		Session:       config.SessionConfig{Provider: "tmux"},
		PackSkillsDir: filepath.Join(cityPath, "skills"),
		Providers: map[string]config.ProviderSpec{
			"claude": {Command: "true", PromptMode: "none", SupportsACP: boolPtr(true)},
		},
		Agents: []config.Agent{{
			Name:     "mayor",
			Scope:    "city",
			Provider: "claude",
		}},
		NamedSessions: []config.NamedSession{{
			Template: "mayor",
			Mode:     "always",
		}},
	}
	store := beads.NewMemStore()
	bead, err := store.Create(beads.Bead{
		Title:  "mayor",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":               "mayor",
			"session_name_explicit":      boolMetadata(true),
			"template":                   "mayor",
			"agent_name":                 "mayor",
			"state":                      string(sessionpkg.StateCreating),
			"pending_create_claim":       "true",
			namedSessionMetadataKey:      boolMetadata(true),
			namedSessionIdentityMetadata: "mayor",
			namedSessionModeMetadata:     "always",
			"continuation_epoch":         "1",
			"generation":                 "1",
			"instance_token":             sessionpkg.NewInstanceToken(),
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	stale := TemplateParams{
		TemplateName: "mayor",
		SessionName:  "mayor",
		InstanceName: "mayor",
		Command:      "true",
		WorkDir:      cityPath,
	}
	candidate := startCandidate{session: &bead, tp: stale}
	refreshed := refreshConfiguredNamedStartCandidate(
		candidate,
		cityPath,
		cfg.Workspace.Name,
		cfg,
		runtime.NewFake(),
		store,
		&clock.Fake{Time: time.Date(2026, 4, 26, 12, 0, 0, 0, time.UTC)},
		ioDiscard{},
	)

	if _, ok := stale.FPExtra["skills:plan"]; ok {
		t.Fatal("test setup invalid: stale candidate already had skills fingerprint")
	}
	if got := refreshed.tp.FPExtra["skills:plan"]; got == "" {
		t.Fatalf("refreshed FPExtra missing skills:plan: %#v", refreshed.tp.FPExtra)
	}
	if refreshed.tp.ConfiguredNamedIdentity != "mayor" {
		t.Fatalf("ConfiguredNamedIdentity = %q, want mayor", refreshed.tp.ConfiguredNamedIdentity)
	}
	if runtime.CoreFingerprint(templateParamsToConfig(refreshed.tp)) == runtime.CoreFingerprint(templateParamsToConfig(stale)) {
		t.Fatal("refreshed candidate core fingerprint did not change after skill FPExtra refresh")
	}
}

func TestExecutePlannedStartsClearsLegacyDrainAckAfterProviderStartBeforeMetadataRetry(t *testing.T) {
	store := &failNthMetadataBatchStore{MemStore: beads.NewMemStore(), failOn: 2}
	sp := runtime.NewFake()
	clk := &clock.Fake{Time: time.Date(2026, 3, 18, 12, 0, 0, 0, time.UTC)}
	tp := TemplateParams{
		Command:      "echo ready",
		SessionName:  "sky",
		TemplateName: "helper",
	}
	bead, err := store.Create(beads.Bead{
		Title:  "helper",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":          "sky",
			"session_name_explicit": "true",
			"template":              "helper",
			"state":                 "asleep",
			"generation":            "1",
			"instance_token":        "old-token",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := sp.SetMeta("sky", "GC_DRAIN_ACK", "1"); err != nil {
		t.Fatalf("SetMeta(GC_DRAIN_ACK): %v", err)
	}
	if err := sp.SetMeta("sky", "GC_DRAIN", "manual"); err != nil {
		t.Fatalf("SetMeta(GC_DRAIN): %v", err)
	}

	woken := executePlannedStarts(
		context.Background(),
		[]startCandidate{{session: &bead, tp: tp, order: 0}},
		&config.City{Agents: []config.Agent{{Name: "helper"}}},
		map[string]TemplateParams{"sky": tp},
		sp,
		store,
		"",
		clk,
		events.Discard,
		5*time.Second,
		ioDiscard{},
		ioDiscard{},
	)
	if woken != 0 {
		t.Fatalf("woken = %d, want 0 after metadata batch retry", woken)
	}
	if !sp.IsRunning("sky") {
		t.Fatal("provider start should have succeeded before metadata retry")
	}
	if ack, _ := sp.GetMeta("sky", "GC_DRAIN_ACK"); ack != "" {
		t.Fatalf("GC_DRAIN_ACK = %q, want cleared after provider start", ack)
	}
	if drain, _ := sp.GetMeta("sky", "GC_DRAIN"); drain != "manual" {
		t.Fatalf("GC_DRAIN = %q, want explicit drain preserved", drain)
	}
}

// recoverRunningPendingCreate heals an already-active bead whose runtime
// is alive but whose pending_create_claim flag was left set (typically
// after a partial write on a prior tick). The heal MUST stamp a fresh
// creation_complete_at alongside the claim clear, otherwise the sweep's
// post-create guard treats the healed bead as stale and the bead can be
// closed on the next tick if the runtime briefly dies — re-opening the
// spin loop this PR is meant to close.
func TestRecoverRunningPendingCreate_StampsCreationCompleteAtForAlreadyActive(t *testing.T) {
	store := beads.NewMemStore()
	bead, err := store.Create(beads.Bead{
		Title:  "helper",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":         "sky",
			"pending_create_claim": "true",
			"state":                "active",
			"state_reason":         "creation_complete",
			// No creation_complete_at from the original start — the
			// exact legacy shape recovery needs to heal.
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{Agents: []config.Agent{{Name: "helper"}}}
	tp := TemplateParams{SessionName: "sky", TemplateName: "helper"}
	clkTime := time.Date(2026, 3, 18, 12, 0, 1, 0, time.UTC)

	if !recoverRunningPendingCreate(&bead, tp, cfg, store, &clock.Fake{Time: clkTime}, nil) {
		t.Fatal("recoverRunningPendingCreate returned false, want true")
	}

	got, err := store.Get(bead.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Metadata["pending_create_claim"] != "" {
		t.Fatalf("pending_create_claim = %q, want cleared", got.Metadata["pending_create_claim"])
	}
	if got.Metadata["creation_complete_at"] != clkTime.Format(time.RFC3339) {
		t.Fatalf("creation_complete_at = %q, want %q — sweep guard would treat healed bead as stale without this stamp",
			got.Metadata["creation_complete_at"], clkTime.Format(time.RFC3339))
	}
}

// A successful atomic start batch must land state=active, state_reason,
// creation_complete_at, AND the pending_create_claim clear together —
// downstream readers (e.g. the pool bead sweep) rely on this atomicity so
// they never observe state=active without either the claim or the
// creation_complete_at marker that the post-create guard keys on.
func TestCommitStartResult_AtomicBatchLandsStateAndClaimClearTogether(t *testing.T) {
	store := beads.NewMemStore()
	bead, err := store.Create(beads.Bead{
		Title:  "helper",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":         "sky",
			"pending_create_claim": "true",
			"state":                "creating",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	result := startResult{
		prepared: preparedStart{
			candidate: startCandidate{
				session: &bead,
				tp: TemplateParams{
					SessionName:  "sky",
					TemplateName: "helper",
				},
			},
			coreHash: "core",
			liveHash: "live",
		},
		outcome:  "success",
		started:  time.Date(2026, 3, 18, 12, 0, 0, 0, time.UTC),
		finished: time.Date(2026, 3, 18, 12, 0, 1, 0, time.UTC),
	}

	clkTime := time.Date(2026, 3, 18, 12, 0, 1, 0, time.UTC)
	ok := commitStartResult(result, store, &clock.Fake{Time: clkTime}, events.Discard, 0, ioDiscard{}, ioDiscard{})
	if !ok {
		t.Fatal("commitStartResult returned false for successful start")
	}

	got, err := store.Get(bead.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Metadata["state"] != "active" {
		t.Fatalf("state = %q, want active", got.Metadata["state"])
	}
	if got.Metadata["state_reason"] != "creation_complete" {
		t.Fatalf("state_reason = %q, want creation_complete", got.Metadata["state_reason"])
	}
	if got.Metadata["creation_complete_at"] == "" {
		t.Fatal("creation_complete_at empty — sweep guard would not key on post-create window")
	}
	if got.Metadata["pending_create_claim"] != "" {
		t.Fatalf("pending_create_claim = %q, want cleared atomically with state transition", got.Metadata["pending_create_claim"])
	}
}

func TestExecutePlannedStarts_UsesLogicalTemplateForDependencyRechecks(t *testing.T) {
	maxWakes := 8
	dropAfter := 3
	sp := &dropDependencyAfterNStartsProvider{
		Fake:      runtime.NewFake(),
		dropAfter: dropAfter,
		depName:   "db",
	}
	if err := sp.Start(context.Background(), "db", runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Daemon: config.DaemonConfig{MaxWakesPerTick: &maxWakes},
		Agents: []config.Agent{
			{Name: "app-1", DependsOn: []string{"db"}},
			{Name: "app-2", DependsOn: []string{"db"}},
			{Name: "app-3", DependsOn: []string{"db"}},
			{Name: "app-4", DependsOn: []string{"db"}},
			{Name: "db", MaxActiveSessions: intPtr(1)},
		},
	}
	store := beads.NewMemStore()
	clk := &clock.Fake{Time: time.Date(2026, 3, 18, 12, 0, 0, 0, time.UTC)}
	desired := map[string]TemplateParams{}
	candidates := make([]startCandidate, 0, 4)
	for idx, name := range []string{"app-1", "app-2", "app-3", "app-4"} {
		tp := TemplateParams{Command: name, SessionName: name, TemplateName: name}
		desired[name] = tp
		bead := makeBead(name+"-id", map[string]string{
			"session_name":   name,
			"template":       "stale-" + name,
			"generation":     "1",
			"instance_token": "tok-" + name,
			"state":          "asleep",
		})
		created, err := store.Create(beads.Bead{
			ID:       bead.ID,
			Title:    name,
			Type:     sessionBeadType,
			Labels:   []string{sessionBeadLabel},
			Metadata: bead.Metadata,
		})
		if err != nil {
			t.Fatal(err)
		}
		candidate := created
		candidates = append(candidates, startCandidate{
			session: &candidate,
			tp:      tp,
			order:   idx,
		})
	}

	var stderr bytes.Buffer
	woken := executePlannedStarts(
		context.Background(), candidates, cfg, desired, sp, store, "",
		clk, events.Discard, 5*time.Second, ioDiscard{}, &stderr,
	)

	if woken != dropAfter {
		t.Fatalf("woken = %d, want %d", woken, dropAfter)
	}
	for _, name := range []string{"app-1", "app-2", "app-3"} {
		if !sp.IsRunning(name) {
			t.Fatalf("%s should have started before dependency loss", name)
		}
	}
	if sp.IsRunning("app-4") {
		t.Fatal("app-4 should be blocked after db dies between batches even when bead template is stale")
	}
	gotLog := stderr.String()
	if !strings.Contains(gotLog, "session=app-4") || !strings.Contains(gotLog, "template=app-4") || !strings.Contains(gotLog, "outcome=blocked_on_dependencies") {
		t.Fatalf("app-4 log = %q, want logical template dependency block", gotLog)
	}
	if strings.Contains(gotLog, "session=app-4 template=app-4 outcome=deferred_by_wake_budget") {
		t.Fatalf("app-4 was deferred by wake budget instead of dependency recheck: %q", gotLog)
	}
}

func TestStopSessionsBounded_StopsDependentsBeforeDependencies(t *testing.T) {
	sp := newGatedStopProvider()
	for _, name := range []string{"db", "api", "worker", "audit"} {
		if err := sp.Start(context.Background(), name, runtime.Config{}); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "worker", MaxActiveSessions: intPtr(1), DependsOn: []string{"api"}},
			{Name: "audit", MaxActiveSessions: intPtr(1), DependsOn: []string{"db"}},
			{Name: "api", MaxActiveSessions: intPtr(1), DependsOn: []string{"db"}},
			{Name: "db", MaxActiveSessions: intPtr(1)},
		},
	}
	rec := events.NewFake()
	var stdout, stderr bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- stopSessionsBounded([]string{"db", "api", "worker", "audit"}, cfg, nil, sp, rec, "gc", &stdout, &stderr)
	}()

	firstWave := sp.waitForStops(t, 1)
	if !containsAll(firstWave, "worker") {
		t.Fatalf("first stop wave = %v, want worker", firstWave)
	}
	sp.ensureNoFurtherStop(t)
	sp.release("worker")

	secondWave := sp.waitForStops(t, 2)
	if !containsAll(secondWave, "api", "audit") {
		t.Fatalf("second stop wave = %v, want api+audit", secondWave)
	}
	sp.release("api")
	sp.release("audit")

	thirdWave := sp.waitForStops(t, 1)
	if !containsAll(thirdWave, "db") {
		t.Fatalf("third stop wave = %v, want db", thirdWave)
	}
	sp.release("db")

	select {
	case stopped := <-done:
		if stopped != 4 {
			t.Fatalf("stopped = %d, want 4", stopped)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("stopSessionsBounded did not finish")
	}
}

func TestStopSessionsBounded_UsesSessionBeadTemplateForCustomSessionNames(t *testing.T) {
	sp := newGatedStopProvider()
	store := beads.NewMemStore()
	cfg := &config.City{
		Workspace: config.Workspace{SessionTemplate: "{{.City}}-{{.Agent}}"},
		Agents: []config.Agent{
			{Name: "worker", MaxActiveSessions: intPtr(1), DependsOn: []string{"db"}},
			{Name: "db", MaxActiveSessions: intPtr(1)},
		},
	}
	for _, bead := range []beads.Bead{
		{
			Title:  "db",
			Type:   sessionBeadType,
			Labels: []string{sessionBeadLabel},
			Metadata: map[string]string{
				"template":     "db",
				"session_name": "custom-db",
			},
		},
		{
			Title:  "worker",
			Type:   sessionBeadType,
			Labels: []string{sessionBeadLabel},
			Metadata: map[string]string{
				"template":     "worker",
				"session_name": "custom-worker",
			},
		},
	} {
		if _, err := store.Create(bead); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"custom-db", "custom-worker"} {
		if err := sp.Start(context.Background(), name, runtime.Config{}); err != nil {
			t.Fatal(err)
		}
	}
	rec := events.NewFake()
	var stdout, stderr bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- stopSessionsBounded([]string{"custom-db", "custom-worker"}, cfg, store, sp, rec, "gc", &stdout, &stderr)
	}()

	firstWave := sp.waitForStops(t, 1)
	if !containsAll(firstWave, "custom-worker") {
		t.Fatalf("first stop wave = %v, want custom-worker", firstWave)
	}
	sp.ensureNoFurtherStop(t)
	sp.release("custom-worker")

	secondWave := sp.waitForStops(t, 1)
	if !containsAll(secondWave, "custom-db") {
		t.Fatalf("second stop wave = %v, want custom-db", secondWave)
	}
	sp.release("custom-db")

	select {
	case stopped := <-done:
		if stopped != 2 {
			t.Fatalf("stopped = %d, want 2", stopped)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("stopSessionsBounded did not finish")
	}
}

func TestStopSessionsBounded_UsesLegacyAgentLabelTemplateForOrdering(t *testing.T) {
	sp := newGatedStopProvider()
	store := beads.NewMemStore()
	cfg := &config.City{
		Workspace: config.Workspace{SessionTemplate: "{{.Agent}}"},
		Agents: []config.Agent{
			{Name: "worker", Dir: "frontend", DependsOn: []string{"frontend/db"}, MinActiveSessions: intPtr(1), MaxActiveSessions: intPtr(2)},
			{Name: "db", Dir: "frontend", MaxActiveSessions: intPtr(1)},
		},
	}
	for _, bead := range []beads.Bead{
		{
			Title:  "db",
			Type:   sessionBeadType,
			Labels: []string{sessionBeadLabel, "agent:frontend/db"},
			Metadata: map[string]string{
				"template":     "frontend/db",
				"session_name": "custom-db",
			},
		},
		{
			Title:  "worker",
			Type:   sessionBeadType,
			Labels: []string{sessionBeadLabel, "agent:frontend/worker-1"},
			Metadata: map[string]string{
				"template":     "worker",
				"session_name": "custom-worker-1",
				"pool_slot":    "1",
			},
		},
	} {
		if _, err := store.Create(bead); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"custom-db", "custom-worker-1"} {
		if err := sp.Start(context.Background(), name, runtime.Config{}); err != nil {
			t.Fatal(err)
		}
	}
	rec := events.NewFake()
	var stdout, stderr bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- stopSessionsBounded([]string{"custom-db", "custom-worker-1"}, cfg, store, sp, rec, "gc", &stdout, &stderr)
	}()

	firstWave := sp.waitForStops(t, 1)
	if !containsAll(firstWave, "custom-worker-1") {
		t.Fatalf("first stop wave = %v, want custom-worker-1", firstWave)
	}
	sp.ensureNoFurtherStop(t)
	sp.release("custom-worker-1")

	secondWave := sp.waitForStops(t, 1)
	if !containsAll(secondWave, "custom-db") {
		t.Fatalf("second stop wave = %v, want custom-db", secondWave)
	}
	sp.release("custom-db")

	select {
	case stopped := <-done:
		if stopped != 2 {
			t.Fatalf("stopped = %d, want 2", stopped)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("stopSessionsBounded did not finish")
	}
}

func TestInterruptSessionsBounded_BroadcastsAllTargets(t *testing.T) {
	sp := newGatedStopProvider()
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "worker", MaxActiveSessions: intPtr(1), DependsOn: []string{"api"}},
			{Name: "audit", MaxActiveSessions: intPtr(1), DependsOn: []string{"db"}},
			{Name: "api", MaxActiveSessions: intPtr(1), DependsOn: []string{"db"}},
			{Name: "db", MaxActiveSessions: intPtr(1)},
		},
	}
	done := make(chan int, 1)
	go func() {
		done <- interruptSessionsBounded([]string{"db", "api", "worker", "audit"}, cfg, nil, sp, ioDiscard{})
	}()

	firstBatch := sp.waitForInterrupts(t, 4)
	if !containsAll(firstBatch, "db", "api", "worker", "audit") {
		t.Fatalf("interrupt batch = %v, want all targets", firstBatch)
	}
	sp.ensureNoFurtherInterrupt(t, 150*time.Millisecond)
	for _, name := range firstBatch {
		sp.releaseInterrupt(name)
	}

	select {
	case interrupted := <-done:
		if interrupted != 4 {
			t.Fatalf("interrupted = %d, want 4", interrupted)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("interruptSessionsBounded did not finish")
	}
}

func TestGracefulStopAll_UsesLogicalSubjectForGracefulExit(t *testing.T) {
	sp := &interruptExitProvider{Fake: runtime.NewFake()}
	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{
		Title:  "frontend/worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"template":     "frontend/worker",
			"agent_name":   "frontend/worker",
			"session_name": "custom-worker",
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := sp.Start(context.Background(), "custom-worker", runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	rec := events.NewFake()
	cfg := &config.City{Agents: []config.Agent{{Name: "worker", Dir: "frontend", MaxActiveSessions: intPtr(1)}}}
	var stdout, stderr bytes.Buffer

	gracefulStopAll([]string{"custom-worker"}, sp, 50*time.Millisecond, rec, cfg, store, &stdout, &stderr)

	if len(rec.Events) != 1 {
		t.Fatalf("got %d events, want 1", len(rec.Events))
	}
	if rec.Events[0].Subject != "frontend/worker" {
		t.Fatalf("event subject = %q, want %q", rec.Events[0].Subject, "frontend/worker")
	}
}

func TestGracefulStopAll_ReconstructsPoolSubjectFromLegacyBead(t *testing.T) {
	sp := &interruptExitProvider{Fake: runtime.NewFake()}
	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{
		Title:  "frontend/worker-2",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"template":     "frontend/worker",
			"pool_slot":    "2",
			"session_name": "custom-worker-2",
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := sp.Start(context.Background(), "custom-worker-2", runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	rec := events.NewFake()
	cfg := &config.City{Agents: []config.Agent{{Name: "worker", Dir: "frontend", MinActiveSessions: intPtr(1), MaxActiveSessions: intPtr(3)}}}
	var stdout, stderr bytes.Buffer

	gracefulStopAll([]string{"custom-worker-2"}, sp, 50*time.Millisecond, rec, cfg, store, &stdout, &stderr)

	if len(rec.Events) != 1 {
		t.Fatalf("got %d events, want 1", len(rec.Events))
	}
	if rec.Events[0].Subject != "frontend/worker-2" {
		t.Fatalf("event subject = %q, want %q", rec.Events[0].Subject, "frontend/worker-2")
	}
}

func TestGracefulStopAll_UsesLegacyAgentLabelForPoolSubject(t *testing.T) {
	sp := &interruptExitProvider{Fake: runtime.NewFake()}
	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "agent:frontend/worker-4"},
		Metadata: map[string]string{
			"template":     "worker",
			"pool_slot":    "4",
			"session_name": "custom-worker-4",
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := sp.Start(context.Background(), "custom-worker-4", runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	rec := events.NewFake()
	cfg := &config.City{Agents: []config.Agent{{Name: "worker", Dir: "frontend", MinActiveSessions: intPtr(1), MaxActiveSessions: intPtr(5)}}}
	var stdout, stderr bytes.Buffer

	gracefulStopAll([]string{"custom-worker-4"}, sp, 50*time.Millisecond, rec, cfg, store, &stdout, &stderr)

	if len(rec.Events) != 1 {
		t.Fatalf("got %d events, want 1", len(rec.Events))
	}
	if rec.Events[0].Subject != "frontend/worker-4" {
		t.Fatalf("event subject = %q, want %q", rec.Events[0].Subject, "frontend/worker-4")
	}
}

func TestGracefulStopAll_UsesListRunningToStopLingeringSessions(t *testing.T) {
	sp := newStaleIsRunningAfterInterruptProvider()
	if err := sp.Start(context.Background(), "custom-worker", runtime.Config{}); err != nil {
		t.Fatal(err)
	}

	rec := events.NewFake()
	var stdout, stderr bytes.Buffer

	gracefulStopAll([]string{"custom-worker"}, sp, 20*time.Millisecond, rec, nil, nil, &stdout, &stderr)

	var stopCalls int
	for _, call := range sp.Calls {
		if call.Method == "Stop" && call.Name == "custom-worker" {
			stopCalls++
		}
	}
	if stopCalls == 0 {
		t.Fatalf("expected gracefulStopAll to force-stop lingering session, calls=%+v", sp.Calls)
	}
	if !strings.Contains(stdout.String(), "Stopped agent 'custom-worker'") {
		t.Fatalf("stdout = %q, want forced stop message", stdout.String())
	}
}

func TestStopWaveOrder_HandlesUnknownTemplateWithoutSerialFallback(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "worker", MaxActiveSessions: intPtr(1), DependsOn: []string{"db"}},
			{Name: "db", MaxActiveSessions: intPtr(1)},
		},
	}
	targets := []stopTarget{
		{name: "removed-worker", template: "removed-worker", order: 0},
		{name: "worker", template: "worker", order: 1},
		{name: "db", template: "db", order: 2},
	}

	waves, ok := stopWaveOrder(targets, cfg)
	if !ok {
		t.Fatal("unexpected serial fallback for unknown template")
	}
	if waves[0] != 1 || waves[1] != 0 || waves[2] != 1 {
		t.Fatalf("waves = %#v, want worker in wave 0 and unknown+db in wave 1", waves)
	}
}

func TestStopTargetsBounded_FallsBackToSerialWhenTemplateUnresolved(t *testing.T) {
	sp := newGatedStopProvider()
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "worker", MaxActiveSessions: intPtr(1), DependsOn: []string{"db"}},
			{Name: "db", MaxActiveSessions: intPtr(1)},
		},
	}
	for _, name := range []string{"db", "worker", "custom"} {
		if err := sp.Start(context.Background(), name, runtime.Config{}); err != nil {
			t.Fatal(err)
		}
	}
	rec := events.NewFake()
	var stdout, stderr bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- stopTargetsBounded([]stopTarget{
			{name: "db", template: "db", subject: "db", order: 0, resolved: true},
			{name: "worker", template: "worker", subject: "worker", order: 1, resolved: true},
			{name: "custom", template: "custom-session", subject: "custom", order: 2, resolved: false},
		}, cfg, nil, sp, rec, "gc", &stdout, &stderr)
	}()

	first := sp.waitForStops(t, 1)
	if !containsAll(first, "db") {
		t.Fatalf("first serial stop = %v, want db", first)
	}
	sp.ensureNoFurtherStop(t)
	sp.release("db")

	second := sp.waitForStops(t, 1)
	if !containsAll(second, "worker") {
		t.Fatalf("second serial stop = %v, want worker", second)
	}
	sp.ensureNoFurtherStop(t)
	sp.release("worker")

	third := sp.waitForStops(t, 1)
	if !containsAll(third, "custom") {
		t.Fatalf("third serial stop = %v, want custom", third)
	}
	sp.release("custom")

	select {
	case stopped := <-done:
		if stopped != 3 {
			t.Fatalf("stopped = %d, want 3", stopped)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("stopTargetsBounded did not finish")
	}

	if !strings.Contains(stderr.String(), "falling back to serial stop order") {
		t.Fatalf("stderr = %q, want fallback diagnostic", stderr.String())
	}
	if !strings.Contains(stderr.String(), "op=stop") || !strings.Contains(stderr.String(), "outcome=success") {
		t.Fatalf("stderr = %q, want successful stop lifecycle log", stderr.String())
	}
}

func TestStopTargetsBounded_AllUnresolvedFallsBackToSerial(t *testing.T) {
	sp := newGatedStopProvider()
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "worker"},
		},
	}
	for _, name := range []string{"orphan-a", "orphan-b", "orphan-c", "orphan-d"} {
		if err := sp.Start(context.Background(), name, runtime.Config{}); err != nil {
			t.Fatal(err)
		}
	}
	rec := events.NewFake()
	var stdout, stderr bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- stopTargetsBounded([]stopTarget{
			{name: "orphan-a", subject: "orphan-a", order: 0},
			{name: "orphan-b", subject: "orphan-b", order: 1},
			{name: "orphan-c", subject: "orphan-c", order: 2},
			{name: "orphan-d", subject: "orphan-d", order: 3},
		}, cfg, nil, sp, rec, "gc", &stdout, &stderr)
	}()

	first := sp.waitForStops(t, 1)
	if !containsAll(first, "orphan-a") {
		t.Fatalf("first serial stop = %v, want orphan-a", first)
	}
	sp.ensureNoFurtherStop(t)
	sp.release("orphan-a")

	second := sp.waitForStops(t, 1)
	if !containsAll(second, "orphan-b") {
		t.Fatalf("second serial stop = %v, want orphan-b", second)
	}
	sp.ensureNoFurtherStop(t)
	sp.release("orphan-b")

	third := sp.waitForStops(t, 1)
	if !containsAll(third, "orphan-c") {
		t.Fatalf("third serial stop = %v, want orphan-c", third)
	}
	sp.ensureNoFurtherStop(t)
	sp.release("orphan-c")

	fourth := sp.waitForStops(t, 1)
	if !containsAll(fourth, "orphan-d") {
		t.Fatalf("fourth serial stop = %v, want orphan-d", fourth)
	}
	sp.release("orphan-d")

	select {
	case stopped := <-done:
		if stopped != 4 {
			t.Fatalf("stopped = %d, want 4", stopped)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("stopTargetsBounded did not finish")
	}

	if sp.maxInFlight != 1 {
		t.Fatalf("max in-flight stops = %d, want 1", sp.maxInFlight)
	}
	if !strings.Contains(stderr.String(), "falling back to serial stop order") {
		t.Fatalf("stderr = %q, want serial fallback diagnostic", stderr.String())
	}
}

func TestCommitStartResult_LogsSuccessOutcome(t *testing.T) {
	store := newTestStore()
	session := makeBead("b1", map[string]string{
		"template":     "worker",
		"session_name": "worker",
	})
	candidate := startCandidate{
		session: &session,
		tp:      TemplateParams{TemplateName: "worker", InstanceName: "worker"},
	}
	result := startResult{
		prepared: preparedStart{
			candidate: candidate,
			coreHash:  "core-hash",
			liveHash:  "live-hash",
		},
		outcome:  "success",
		started:  time.Unix(1, 0),
		finished: time.Unix(2, 0),
	}
	rec := events.NewFake()
	var stdout, stderr bytes.Buffer
	ok := commitStartResult(result, store, &clock.Fake{Time: time.Unix(3, 0)}, rec, 0, &stdout, &stderr)
	if !ok {
		t.Fatal("commitStartResult returned false for success")
	}
	if !strings.Contains(stderr.String(), "op=start") || !strings.Contains(stderr.String(), "outcome=success") {
		t.Fatalf("stderr = %q, want successful start lifecycle log", stderr.String())
	}
}

func TestCommitStartResult_SanitizesMultilineError(t *testing.T) {
	store := newTestStore()
	session := makeBead("b1", map[string]string{
		"template":     "worker",
		"session_name": "worker",
	})
	candidate := startCandidate{
		session: &session,
		tp:      TemplateParams{TemplateName: "worker", InstanceName: "worker"},
	}
	result := startResult{
		prepared: preparedStart{candidate: candidate},
		err:      fmt.Errorf("boom\nstack line"),
		outcome:  "panic_recovered",
	}
	var stderr bytes.Buffer
	ok := commitStartResult(result, store, &clock.Fake{Time: time.Unix(3, 0)}, events.NewFake(), 0, ioDiscard{}, &stderr)
	if ok {
		t.Fatal("commitStartResult returned true for error result")
	}
	got := stderr.String()
	if strings.Contains(got, "boom\nstack line") {
		t.Fatalf("stderr = %q, want escaped multiline error", got)
	}
	if !strings.Contains(got, "boom\\nstack line") {
		t.Fatalf("stderr = %q, want escaped multiline error", got)
	}
}

func TestInterruptTargetsBounded_LogsSuccessOutcome(t *testing.T) {
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "worker", runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	sent := interruptTargetsBounded([]stopTarget{{name: "worker", template: "worker", resolved: true}}, nil, nil, sp, &stderr)
	if sent != 1 {
		t.Fatalf("sent = %d, want 1", sent)
	}
	if !strings.Contains(stderr.String(), "op=interrupt") || !strings.Contains(stderr.String(), "outcome=success") {
		t.Fatalf("stderr = %q, want successful interrupt lifecycle log", stderr.String())
	}
}

func TestInterruptTargetsBounded_BroadcastsAllTargetsConcurrently(t *testing.T) {
	sp := newGatedStopProvider()
	targets := []stopTarget{
		{name: "db", template: "db", resolved: true},
		{name: "api", template: "api", resolved: true},
		{name: "worker", template: "worker", resolved: true},
		{name: "audit", template: "audit", resolved: true},
	}
	for _, target := range targets {
		if err := sp.Start(context.Background(), target.name, runtime.Config{}); err != nil {
			t.Fatal(err)
		}
	}

	done := make(chan int, 1)
	go func() {
		done <- interruptTargetsBounded(targets, nil, nil, sp, ioDiscard{})
	}()

	first := sp.waitForInterrupts(t, len(targets))
	if !containsAll(first, "db", "api", "worker", "audit") {
		t.Fatalf("interrupt wave = %v, want all targets", first)
	}
	sp.ensureNoFurtherInterrupt(t, 150*time.Millisecond)
	for _, name := range first {
		sp.releaseInterrupt(name)
	}

	select {
	case sent := <-done:
		if sent != len(targets) {
			t.Fatalf("sent = %d, want %d", sent, len(targets))
		}
	case <-time.After(3 * time.Second):
		t.Fatal("interruptTargetsBounded did not finish")
	}
}

func TestInterruptTargetsBounded_RespectsInterruptCap(t *testing.T) {
	sp := newGatedStopProvider()
	targets := make([]stopTarget, 0, defaultMaxParallelInterrupts+1)
	for i := 0; i < defaultMaxParallelInterrupts+1; i++ {
		name := fmt.Sprintf("worker-%d", i+1)
		if err := sp.Start(context.Background(), name, runtime.Config{}); err != nil {
			t.Fatal(err)
		}
		targets = append(targets, stopTarget{name: name, template: name, resolved: true})
	}

	done := make(chan int, 1)
	go func() {
		done <- interruptTargetsBounded(targets, nil, nil, sp, ioDiscard{})
	}()

	first := sp.waitForInterrupts(t, defaultMaxParallelInterrupts)
	sp.ensureNoFurtherInterrupt(t, 150*time.Millisecond)
	for _, name := range first {
		sp.releaseInterrupt(name)
	}

	second := sp.waitForInterrupts(t, 1)
	sp.releaseInterrupt(second[0])

	select {
	case sent := <-done:
		if sent != len(targets) {
			t.Fatalf("sent = %d, want %d", sent, len(targets))
		}
	case <-time.After(3 * time.Second):
		t.Fatal("interruptTargetsBounded did not finish")
	}
}

func TestInterruptTargetsBounded_StopsPoolManagedSessions(t *testing.T) {
	sp := runtime.NewFake()
	for _, name := range []string{"human-worker", "pool-worker"} {
		if err := sp.Start(context.Background(), name, runtime.Config{}); err != nil {
			t.Fatal(err)
		}
	}
	targets := []stopTarget{
		{name: "human-worker", template: "worker", resolved: true, poolManaged: false},
		{name: "pool-worker", template: "pool", resolved: true, poolManaged: true},
	}
	var stderr bytes.Buffer
	sent := interruptTargetsBounded(targets, nil, nil, sp, &stderr)
	if sent != 1 {
		t.Fatalf("sent = %d, want 1 (only human-worker)", sent)
	}
	if !strings.Contains(stderr.String(), "stopped_pool_managed") {
		t.Fatalf("stderr = %q, want stopped_pool_managed log entry", stderr.String())
	}
	// Pool-managed session should have been stopped, not interrupted.
	if sp.IsRunning("pool-worker") {
		t.Fatal("pool-worker should have been stopped")
	}
	// Human worker should still be running (only interrupted, not stopped).
	if !sp.IsRunning("human-worker") {
		t.Fatal("human-worker should still be running (only interrupted)")
	}
}

func TestExecutePreparedStartWave_PanicIncludesStackTrace(t *testing.T) {
	results := executePreparedStartWave(
		context.Background(),
		[]preparedStart{{
			candidate: startCandidate{session: &beads.Bead{Metadata: map[string]string{"session_name": "worker"}}},
			cfg:       runtime.Config{Command: "panic-provider"},
		}},
		&panicStartProvider{Fake: runtime.NewFake()},
		nil,
		time.Second,
	)
	if len(results) != 1 {
		t.Fatalf("len(results) = %d, want 1", len(results))
	}
	if results[0].outcome != "panic_recovered" {
		t.Fatalf("outcome = %q, want panic_recovered", results[0].outcome)
	}
	if results[0].err == nil || !strings.Contains(results[0].err.Error(), "goroutine") {
		t.Fatalf("err = %v, want stack trace", results[0].err)
	}
}

func TestExecuteTargetWave_PanicIncludesStackTrace(t *testing.T) {
	results := executeTargetWave([]stopTarget{{name: "worker"}}, 1, func(stopTarget) error {
		panic("boom")
	})
	if len(results) != 1 {
		t.Fatalf("len(results) = %d, want 1", len(results))
	}
	if results[0].outcome != "panic_recovered" {
		t.Fatalf("outcome = %q, want panic_recovered", results[0].outcome)
	}
	if results[0].err == nil || !strings.Contains(results[0].err.Error(), "goroutine") {
		t.Fatalf("err = %v, want stack trace", results[0].err)
	}
}

func TestStopTargetsBounded_SanitizesMultilineStopError(t *testing.T) {
	sp := &multilineStopProvider{Fake: runtime.NewFake()}
	if err := sp.Start(context.Background(), "worker", runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	rec := events.NewFake()
	var stdout, stderr bytes.Buffer
	stopped := stopTargetsBounded([]stopTarget{{name: "worker", template: "worker", subject: "worker", resolved: true}}, &config.City{
		Agents: []config.Agent{{Name: "worker"}},
	}, nil, sp, rec, "gc", &stdout, &stderr)
	if stopped != 0 {
		t.Fatalf("stopped = %d, want 0", stopped)
	}
	got := stderr.String()
	if strings.Contains(got, "boom\nstack line") {
		t.Fatalf("stderr = %q, want escaped multiline error", got)
	}
	if !strings.Contains(got, "boom\\nstack line") {
		t.Fatalf("stderr = %q, want escaped multiline error", got)
	}
}

func TestLogLifecycleOutcome_SanitizesMultilineErrors(t *testing.T) {
	var stderr bytes.Buffer
	logLifecycleOutcome(&stderr, "stop", 0, "worker", "worker", "panic_recovered",
		time.Unix(1, 0), time.Unix(2, 0), fmt.Errorf("boom\nstack line"))
	got := stderr.String()
	if strings.Contains(got, "boom\nstack line") {
		t.Fatalf("stderr = %q, want escaped newline in error text", got)
	}
	if !strings.Contains(got, "err=boom\\nstack line") {
		t.Fatalf("stderr = %q, want escaped multiline error", got)
	}
}

func TestStopWaveOrder_PreservesTransitiveSubsetOrdering(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "api", MaxActiveSessions: intPtr(1), DependsOn: []string{"cache"}},
			{Name: "cache", MaxActiveSessions: intPtr(1), DependsOn: []string{"db"}},
			{Name: "db", MaxActiveSessions: intPtr(1)},
		},
	}
	targets := []stopTarget{
		{name: "api", template: "api", order: 0},
		{name: "db", template: "db", order: 1},
	}

	waves, ok := stopWaveOrder(targets, cfg)
	if !ok {
		t.Fatal("unexpected serial fallback for transitive subset")
	}
	if waves[0] != 0 || waves[1] != 1 {
		t.Fatalf("waves = %#v, want api before db via transitive dependency", waves)
	}
}

func TestStopWaveOrder_FallsBackToSerialOnCycle(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "api", MaxActiveSessions: intPtr(1), DependsOn: []string{"db"}},
			{Name: "db", MaxActiveSessions: intPtr(1), DependsOn: []string{"api"}},
		},
	}
	targets := []stopTarget{
		{name: "api", template: "api", order: 0},
		{name: "db", template: "db", order: 1},
	}

	waves, ok := stopWaveOrder(targets, cfg)
	if ok {
		t.Fatal("expected serial fallback for cycle")
	}
	if waves[0] != 0 || waves[1] != 1 {
		t.Fatalf("waves = %#v, want strict serial fallback", waves)
	}
}

func TestCandidateWaveOrder_FallsBackToSerialOnCycle(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "api", MaxActiveSessions: intPtr(1), DependsOn: []string{"db"}},
			{Name: "db", MaxActiveSessions: intPtr(1), DependsOn: []string{"api"}},
		},
	}
	candidates := []startCandidate{
		{
			session: &beads.Bead{Metadata: map[string]string{"session_name": "api", "template": "api"}},
			tp:      TemplateParams{TemplateName: "api"},
			order:   0,
		},
		{
			session: &beads.Bead{Metadata: map[string]string{"session_name": "db", "template": "db"}},
			tp:      TemplateParams{TemplateName: "db"},
			order:   1,
		},
	}

	waves, ok := candidateWaveOrder(candidates, cfg, map[string]TemplateParams{}, runtime.NewFake(), "city", nil, clock.Real{})
	if ok {
		t.Fatal("expected serial fallback for cycle")
	}
	if waves[0] != 0 || waves[1] != 1 {
		t.Fatalf("waves = %#v, want strict serial fallback", waves)
	}
}

func TestCandidateWaveOrder_UsesLegacyAgentLabelTemplate(t *testing.T) {
	store := beads.NewMemStore()
	for _, bead := range []beads.Bead{
		{
			Title:  "db",
			Type:   sessionBeadType,
			Labels: []string{sessionBeadLabel, "agent:frontend/db"},
			Metadata: map[string]string{
				"template":     "frontend/db",
				"session_name": "custom-db",
			},
		},
		{
			Title:  "worker",
			Type:   sessionBeadType,
			Labels: []string{sessionBeadLabel, "agent:frontend/worker-1"},
			Metadata: map[string]string{
				"template":     "worker",
				"session_name": "custom-worker-1",
				"pool_slot":    "1",
			},
		},
	} {
		if _, err := store.Create(bead); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &config.City{
		Workspace: config.Workspace{SessionTemplate: "{{.Agent}}"},
		Agents: []config.Agent{
			{Name: "worker", Dir: "frontend", DependsOn: []string{"frontend/db"}, MinActiveSessions: intPtr(1), MaxActiveSessions: intPtr(2)},
			{Name: "db", Dir: "frontend", MaxActiveSessions: intPtr(1)},
		},
	}
	candidates := []startCandidate{
		{
			session: &beads.Bead{
				Labels: []string{sessionBeadLabel, "agent:frontend/worker-1"},
				Metadata: map[string]string{
					"template":     "worker",
					"session_name": "custom-worker-1",
					"pool_slot":    "1",
				},
			},
			tp:    TemplateParams{TemplateName: "frontend/worker"},
			order: 0,
		},
		{
			session: &beads.Bead{Metadata: map[string]string{
				"template":     "frontend/db",
				"session_name": "custom-db",
			}},
			tp:    TemplateParams{TemplateName: "frontend/db"},
			order: 1,
		},
	}

	waves, ok := candidateWaveOrder(candidates, cfg, map[string]TemplateParams{}, runtime.NewFake(), "city", store, clock.Real{})
	if !ok {
		t.Fatal("unexpected serial fallback")
	}
	if waves[0] != 1 || waves[1] != 0 {
		t.Fatalf("waves = %#v, want legacy worker after db", waves)
	}
}

// dieAfterStartProvider starts the session successfully, then immediately
// removes it so IsRunning returns false. This simulates a session that
// dies immediately after start (e.g., stale resume key).
type dieAfterStartProvider struct {
	*runtime.Fake
}

func (p *dieAfterStartProvider) Start(ctx context.Context, name string, cfg runtime.Config) error {
	if err := p.Fake.Start(ctx, name, cfg); err != nil {
		return err
	}
	// Simulate the pane dying immediately.
	_ = p.Stop(name)
	return nil
}

func (p *dieAfterStartProvider) IsRunning(name string) bool {
	return p.Fake.IsRunning(name)
}

// zombieAfterStartProvider leaves the runtime container/pane present but marks
// the actual agent process dead. This matches wrappers that keep tmux alive
// after the CLI exits with a stale resume-session error.
type zombieAfterStartProvider struct {
	*runtime.Fake
}

func (p *zombieAfterStartProvider) Start(ctx context.Context, name string, cfg runtime.Config) error {
	if err := p.Fake.Start(ctx, name, cfg); err != nil {
		return err
	}
	p.Zombies[name] = true
	return nil
}

func TestExecutePreparedStartWave_StaleSessionKeyDetected(t *testing.T) {
	skipSlowCmdGCTest(t, "waits through stale session-key detection; run make test-cmd-gc-process for full coverage")
	sp := &dieAfterStartProvider{Fake: runtime.NewFake()}
	item := preparedStart{
		candidate: startCandidate{
			session: &beads.Bead{
				ID: "gc-99",
				Metadata: map[string]string{
					"session_name": "test-agent",
					"session_key":  "stale-key-abc",
					"template":     "worker",
				},
			},
			tp: TemplateParams{
				Command:      "claude --resume stale-key-abc",
				SessionName:  "test-agent",
				TemplateName: "worker",
			},
		},
		cfg: runtime.Config{Command: "claude --resume stale-key-abc"},
	}

	results := executePreparedStartWave(
		context.Background(),
		[]preparedStart{item},
		sp,
		nil,
		10*time.Second,
	)

	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	r := results[0]
	if r.err == nil {
		t.Fatal("expected error for session that died during startup with stale key")
	}
	if !strings.Contains(r.err.Error(), "died during startup") {
		t.Fatalf("unexpected error: %v", r.err)
	}
}

func TestExecutePreparedStartWave_StaleSessionKeyDetectedWhenPaneSurvives(t *testing.T) {
	sp := &zombieAfterStartProvider{Fake: runtime.NewFake()}
	item := preparedStart{
		candidate: startCandidate{
			session: &beads.Bead{
				ID: "gc-99",
				Metadata: map[string]string{
					"session_name": "test-agent",
					"session_key":  "stale-key-abc",
					"template":     "worker",
				},
			},
			tp: TemplateParams{
				Command:      "claude --resume stale-key-abc",
				SessionName:  "test-agent",
				TemplateName: "worker",
			},
		},
		cfg: runtime.Config{
			Command:      "claude --resume stale-key-abc",
			ProcessNames: []string{"claude"},
		},
	}

	results := executePreparedStartWave(
		context.Background(),
		[]preparedStart{item},
		sp,
		nil,
		10*time.Second,
	)

	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	r := results[0]
	if r.err == nil {
		t.Fatal("expected error for dead agent process left behind in a live pane")
	}
	if !strings.Contains(r.err.Error(), "died during startup") {
		t.Fatalf("unexpected error: %v", r.err)
	}
}

func TestExecutePreparedStartWave_NoStaleCheckWithoutSessionKey(t *testing.T) {
	// Session without a session_key should not trigger stale detection,
	// even if the session dies after start.
	sp := &dieAfterStartProvider{Fake: runtime.NewFake()}
	item := preparedStart{
		candidate: startCandidate{
			session: &beads.Bead{
				ID: "gc-99",
				Metadata: map[string]string{
					"session_name": "test-agent",
					"template":     "worker",
				},
			},
			tp: TemplateParams{
				Command:      "claude",
				SessionName:  "test-agent",
				TemplateName: "worker",
			},
		},
		cfg: runtime.Config{Command: "claude"},
	}

	results := executePreparedStartWave(
		context.Background(),
		[]preparedStart{item},
		sp,
		nil,
		10*time.Second,
	)

	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	r := results[0]
	if r.err != nil {
		t.Fatalf("session without session_key should not get stale key error, got: %v", r.err)
	}
}

type ioDiscard struct{}

func (ioDiscard) Write(p []byte) (int, error) { return len(p), nil }

func TestPrepareStartCandidate_PreservesRuntimeConfigAndProviderEnv(t *testing.T) {
	store := beads.NewMemStore()
	bead, err := store.Create(beads.Bead{
		Title: "mayor",
		Type:  "task",
		Metadata: map[string]string{
			"session_name": "s-gc-test",
			"provider":     "gemini",
			"alias":        "mayor",
			"state":        "creating",
		},
	})
	if err != nil {
		t.Fatalf("store.Create: %v", err)
	}

	tp := TemplateParams{
		Command: "aimux run gemini -- --approval-mode yolo",
		Prompt:  "system prompt",
		Env: map[string]string{
			"BASE":    "1",
			"GC_HOME": "/tmp/gc-home",
		},
		Hints: agent.StartupHints{
			ReadyPromptPrefix:      "> ",
			ReadyDelayMs:           17,
			ProcessNames:           []string{"gemini", "node"},
			EmitsPermissionWarning: true,
			Nudge:                  "hello",
			PreStart:               []string{"echo pre"},
			SessionSetup:           []string{"echo setup"},
			SessionSetupScript:     "/tmp/setup.sh",
			SessionLive:            []string{"echo live"},
			PackOverlayDirs:        []string{"/tmp/pack"},
			OverlayDir:             "/tmp/overlay",
			CopyFiles:              []runtime.CopyEntry{{Src: "/tmp/src", RelDst: "dst"}},
		},
		WorkDir:          t.TempDir(),
		SessionName:      "s-gc-test",
		Alias:            "mayor",
		FPExtra:          map[string]string{"pool": "1"},
		ResolvedProvider: &config.ResolvedProvider{Name: "gemini", PromptMode: "none"},
		TemplateName:     "mayor",
		InstanceName:     "mayor",
	}

	prepared, err := prepareStartCandidate(
		startCandidate{
			session: &bead,
			tp:      tp,
		},
		&config.City{},
		store,
		clock.Real{},
	)
	if err != nil {
		t.Fatalf("prepareStartCandidate: %v", err)
	}

	generation, err := strconv.Atoi(bead.Metadata["generation"])
	if err != nil {
		t.Fatalf("generation metadata = %q: %v", bead.Metadata["generation"], err)
	}
	continuationEpoch, err := strconv.Atoi(bead.Metadata["continuation_epoch"])
	if err != nil {
		t.Fatalf("continuation_epoch metadata = %q: %v", bead.Metadata["continuation_epoch"], err)
	}

	expected := templateParamsToConfig(tp)
	expected.Env = mergeEnv(expected.Env, sessionpkg.RuntimeEnvWithSessionContext(
		bead.ID,
		tp.SessionName,
		tp.Alias,
		bead.Metadata["template"],
		bead.Metadata["session_origin"],
		generation,
		continuationEpoch,
		bead.Metadata["instance_token"],
	))
	expected.Env = mergeEnv(expected.Env, map[string]string{"GC_PROVIDER": "gemini"})
	expected = runtime.SyncWorkDirEnv(expected)

	if !reflect.DeepEqual(prepared.cfg, expected) {
		t.Fatalf("prepared cfg mismatch\n got: %#v\nwant: %#v", prepared.cfg, expected)
	}
	if got := prepared.cfg.Env["GC_HOME"]; got != "/tmp/gc-home" {
		t.Fatalf("GC_HOME = %q, want %q", got, "/tmp/gc-home")
	}
}

func TestPrepareStartCandidateUsesBuiltinAncestorForGCProviderEnv(t *testing.T) {
	store := beads.NewMemStore()
	bead, err := store.Create(beads.Bead{
		Title: "mayor",
		Type:  "task",
		Metadata: map[string]string{
			"session_name":       "s-gc-test",
			"template":           "mayor",
			"session_origin":     "manual",
			"provider":           "claude-max",
			"provider_kind":      "claude-max",
			"builtin_ancestor":   "claude",
			"generation":         "1",
			"continuation_epoch": "1",
			"instance_token":     "tok",
		},
	})
	if err != nil {
		t.Fatalf("create bead: %v", err)
	}
	tp := TemplateParams{
		Command:     "claude",
		WorkDir:     t.TempDir(),
		SessionName: "s-gc-test",
		Alias:       "mayor",
		ResolvedProvider: &config.ResolvedProvider{
			Name:            "claude-max",
			Kind:            "claude",
			BuiltinAncestor: "claude",
		},
		TemplateName: "mayor",
		InstanceName: "mayor",
	}

	prepared, err := prepareStartCandidate(
		startCandidate{
			session: &bead,
			tp:      tp,
		},
		&config.City{},
		store,
		clock.Real{},
	)
	if err != nil {
		t.Fatalf("prepareStartCandidate: %v", err)
	}
	if got := prepared.cfg.Env["GC_PROVIDER"]; got != "claude" {
		t.Fatalf("GC_PROVIDER = %q, want claude", got)
	}
}

func TestPrepareStartCandidate_EmptyBeadAliasPreservesTemplateGCAlias(t *testing.T) {
	store := beads.NewMemStore()
	bead, err := store.Create(beads.Bead{
		Title: "ants-ant-1",
		Type:  "task",
		Metadata: map[string]string{
			"session_name": "ants-ant-1",
			"provider":     "claude",
			"state":        "creating",
		},
	})
	if err != nil {
		t.Fatalf("store.Create: %v", err)
	}

	tp := TemplateParams{
		Command: "claude",
		// Shape matches setTemplateEnvIdentity output (GC_ALIAS+GC_AGENT stamped)
		// plus an unrelated template key to verify the merge preserves it.
		Env:                map[string]string{"GC_ALIAS": "ants-ant-1", "GC_AGENT": "ants-ant-1", "TEMPLATE_KEY": "keep"},
		WorkDir:            t.TempDir(),
		SessionName:        "ants-ant-1",
		InstanceName:       "ants-ant-1",
		PoolSlot:           1,
		EnvIdentityStamped: true,
		ResolvedProvider:   &config.ResolvedProvider{Name: "claude", PromptMode: "none"},
		TemplateName:       "ants",
	}

	prepared, err := prepareStartCandidate(
		startCandidate{session: &bead, tp: tp},
		&config.City{},
		store,
		clock.Real{},
	)
	if err != nil {
		t.Fatalf("prepareStartCandidate: %v", err)
	}

	if got := prepared.cfg.Env["GC_ALIAS"]; got != "ants-ant-1" {
		t.Fatalf("GC_ALIAS = %q, want %q (template value must survive merge when bead alias is empty)", got, "ants-ant-1")
	}
	if got := prepared.cfg.Env["GC_AGENT"]; got != "ants-ant-1" {
		t.Fatalf("GC_AGENT = %q, want %q (companion identity key must also survive)", got, "ants-ant-1")
	}
	if got := prepared.cfg.Env["TEMPLATE_KEY"]; got != "keep" {
		t.Fatalf("TEMPLATE_KEY = %q, want %q (unrelated template env must survive merge)", got, "keep")
	}
}

func TestPrepareStartCandidate_EmptyAliasEverywhereKeepsEmptyForTmuxScrub(t *testing.T) {
	store := beads.NewMemStore()
	bead, err := store.Create(beads.Bead{
		Title: "s-gc-test",
		Type:  "task",
		Metadata: map[string]string{
			"session_name": "s-gc-test",
			"provider":     "claude",
			"state":        "creating",
		},
	})
	if err != nil {
		t.Fatalf("store.Create: %v", err)
	}

	tp := TemplateParams{
		Command: "claude",
		// Shape matches resolveTemplate output: GC_ALIAS is unconditionally
		// seeded with qualifiedName on every session, pool or not. The guard
		// must distinguish identity-stamped templates from the resolver's
		// default stamping so that the empty runtime value still wins here
		// and tmux emits `env -u GC_ALIAS`.
		Env:              map[string]string{"GC_ALIAS": "s-gc-test", "GC_AGENT": "s-gc-test", "BASE": "1"},
		WorkDir:          t.TempDir(),
		SessionName:      "s-gc-test",
		InstanceName:     "s-gc-test",
		ResolvedProvider: &config.ResolvedProvider{Name: "claude", PromptMode: "none"},
		TemplateName:     "s",
		// EnvIdentityStamped is false — setTemplateEnvIdentity was not called.
	}

	prepared, err := prepareStartCandidate(
		startCandidate{session: &bead, tp: tp},
		&config.City{},
		store,
		clock.Real{},
	)
	if err != nil {
		t.Fatalf("prepareStartCandidate: %v", err)
	}

	got, ok := prepared.cfg.Env["GC_ALIAS"]
	if !ok {
		t.Fatalf("GC_ALIAS should be present with empty value so tmux emits `env -u GC_ALIAS`; got absent")
	}
	if got != "" {
		t.Fatalf("GC_ALIAS = %q, want empty (tmux env -u scrub)", got)
	}
}

func TestPrepareStartCandidate_NonEmptyBeadAliasOverridesTemplate(t *testing.T) {
	store := beads.NewMemStore()
	bead, err := store.Create(beads.Bead{
		Title: "mayor",
		Type:  "task",
		Metadata: map[string]string{
			"session_name": "s-mayor",
			"provider":     "claude",
			"alias":        "mayor",
			"state":        "creating",
		},
	})
	if err != nil {
		t.Fatalf("store.Create: %v", err)
	}

	tp := TemplateParams{
		Command:          "claude",
		Env:              map[string]string{"GC_ALIAS": "stale-from-template"},
		WorkDir:          t.TempDir(),
		SessionName:      "s-mayor",
		InstanceName:     "mayor",
		ResolvedProvider: &config.ResolvedProvider{Name: "claude", PromptMode: "none"},
		TemplateName:     "mayor",
	}

	prepared, err := prepareStartCandidate(
		startCandidate{session: &bead, tp: tp},
		&config.City{},
		store,
		clock.Real{},
	)
	if err != nil {
		t.Fatalf("prepareStartCandidate: %v", err)
	}

	if got := prepared.cfg.Env["GC_ALIAS"]; got != "mayor" {
		t.Fatalf("GC_ALIAS = %q, want %q (runtime alias must override stale template value)", got, "mayor")
	}
}

func TestConfirmPendingStart(t *testing.T) {
	// commitStartResultTraced must transition freshly-spawned pool
	// session beads from the pending states ("", creating, asleep,
	// drained) to active. Running states ("awake", "active") are left
	// alone to avoid wasteful metadata rewrites on every reconcile
	// cycle; terminal and transitional states ("draining", "archived",
	// "quarantined", "suspended") are likewise ignored so we don't
	// resurrect a session the reconciler deliberately wound down.
	cases := []struct {
		name  string
		state string
		want  bool
	}{
		{"<empty>", "", true},
		{"creating", "creating", true},
		{"asleep", "asleep", true},
		{"drained", "drained", true},
		{"creating_with_whitespace", "  creating  ", true},
		{"active", "active", false},
		{"awake", "awake", false},
		{"draining", "draining", false},
		{"archived", "archived", false},
		{"quarantined", "quarantined", false},
		{"suspended", "suspended", false},
		{"unknown-future-state", "unknown-future-state", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := confirmPendingStart(tc.state); got != tc.want {
				t.Errorf("confirmPendingStart(%q) = %v, want %v", tc.state, got, tc.want)
			}
		})
	}
}

func TestCommitStartResult_TransitionsCreatingToActive(t *testing.T) {
	// Verify that commitStartResult persists state=active and
	// state_reason=creation_complete when the session starts in
	// "creating" state. This is the critical integration seam for
	// the creating→active fix.
	store := beads.NewMemStore()
	session, err := store.Create(beads.Bead{
		Title:  "worker-session",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"template":     "worker",
			"session_name": "worker-1",
			"state":        "creating",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	candidate := startCandidate{
		session: &session,
		tp:      TemplateParams{TemplateName: "worker", InstanceName: "worker-1"},
	}
	result := startResult{
		prepared: preparedStart{
			candidate: candidate,
			coreHash:  "core-abc",
			liveHash:  "live-xyz",
		},
		outcome:  "success",
		started:  time.Unix(100, 0),
		finished: time.Unix(101, 0),
	}
	rec := events.NewFake()
	ok := commitStartResult(result, store, &clock.Fake{Time: time.Unix(102, 0)}, rec, 0, ioDiscard{}, ioDiscard{})
	if !ok {
		t.Fatal("commitStartResult returned false for successful start")
	}
	got, err := store.Get(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Metadata["state"] != "active" {
		t.Errorf("state = %q, want %q", got.Metadata["state"], "active")
	}
	if got.Metadata["state_reason"] != "creation_complete" {
		t.Errorf("state_reason = %q, want %q", got.Metadata["state_reason"], "creation_complete")
	}
	if got.Metadata["started_config_hash"] != "core-abc" {
		t.Errorf("started_config_hash = %q, want %q", got.Metadata["started_config_hash"], "core-abc")
	}
}

func TestCommitStartResult_PersistsMCPIdentityForACPStart(t *testing.T) {
	store := beads.NewMemStore()
	session, err := store.Create(beads.Bead{
		Title:  "worker-session",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"template":     "worker",
			"agent_name":   "myrig/worker-adhoc-123",
			"session_name": "worker-1",
			"state":        "creating",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	candidate := startCandidate{
		session: &session,
		tp: TemplateParams{
			TemplateName: "worker",
			InstanceName: "worker-1",
			IsACP:        true,
		},
	}
	result := startResult{
		prepared: preparedStart{
			candidate: candidate,
			cfg: runtime.Config{
				MCPServers: []runtime.MCPServerConfig{{
					Name:      "filesystem",
					Transport: runtime.MCPTransportStdio,
					Command:   "/bin/mcp",
				}},
			},
			coreHash: "core-abc",
			liveHash: "live-xyz",
		},
		outcome:  "success",
		started:  time.Unix(100, 0),
		finished: time.Unix(101, 0),
	}
	rec := events.NewFake()
	ok := commitStartResult(result, store, &clock.Fake{Time: time.Unix(102, 0)}, rec, 0, ioDiscard{}, ioDiscard{})
	if !ok {
		t.Fatal("commitStartResult returned false for successful start")
	}
	got, err := store.Get(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Metadata[sessionpkg.MCPIdentityMetadataKey] != "myrig/worker-adhoc-123" {
		t.Fatalf("mcp_identity = %q, want %q", got.Metadata[sessionpkg.MCPIdentityMetadataKey], "myrig/worker-adhoc-123")
	}
	if got.Metadata[sessionpkg.MCPServersSnapshotMetadataKey] == "" {
		t.Fatal("mcp_servers_snapshot = empty, want persisted snapshot")
	}
}

func TestStopTargetThroughWorkerBoundary_CityStopLeavesSessionAsleep(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	session, err := store.Create(beads.Bead{
		Title:  "control-dispatcher",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name": "control-dispatcher",
			"template":     "control-dispatcher",
			"state":        "active",
			"sleep_reason": sleepReasonCityStop,
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := sp.Start(context.Background(), "control-dispatcher", runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	err = stopTargetThroughWorkerBoundary(stopTarget{
		sessionID: session.ID,
		name:      "control-dispatcher",
		resolved:  true,
	}, store, sp, &config.City{})
	if err != nil {
		t.Fatalf("stopTargetThroughWorkerBoundary: %v", err)
	}

	got, err := store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Metadata["state"] != string(sessionpkg.StateAsleep) {
		t.Fatalf("state = %q, want %q", got.Metadata["state"], sessionpkg.StateAsleep)
	}
	if got.Metadata["sleep_reason"] != sleepReasonCityStop {
		t.Fatalf("sleep_reason = %q, want %q", got.Metadata["sleep_reason"], sleepReasonCityStop)
	}
	if got.Metadata["suspended_at"] != "" {
		t.Fatalf("suspended_at = %q, want empty", got.Metadata["suspended_at"])
	}
}
