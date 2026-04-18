package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
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

type injectPendingCreateAfterClearStore struct {
	*beads.MemStore
}

func (s *injectPendingCreateAfterClearStore) SetMetadata(id, key, value string) error {
	if err := s.MemStore.SetMetadata(id, key, value); err != nil {
		return err
	}
	if key == "pending_create_claim" && value == "" {
		return s.MemStore.SetMetadata(id, key, "true")
	}
	return nil
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
	if got := startCfg.Command; got != "claude --dangerously-skip-permissions --session-id fresh-key-123" {
		t.Fatalf("Start command = %q, want fresh session-id launch", got)
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
	sp := &dropDependencyAfterNStartsProvider{
		Fake:      runtime.NewFake(),
		dropAfter: defaultMaxParallelStartsPerWave,
		depName:   "db",
	}
	if err := sp.Start(context.Background(), "db", runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
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
	woken := reconcileSessionBeads(
		context.Background(), sessions, desired, configuredSessionNames(cfg, "", store),
		cfg, sp, store, nil, nil, nil, newDrainTracker(), poolDesired, false, nil, "",
		nil, clk, events.Discard, 5*time.Second, 0, ioDiscard{}, ioDiscard{},
	)

	if woken != defaultMaxParallelStartsPerWave {
		t.Fatalf("woken = %d, want %d", woken, defaultMaxParallelStartsPerWave)
	}
	for _, name := range []string{"app-1", "app-2", "app-3"} {
		if !sp.IsRunning(name) {
			t.Fatalf("%s should have started before dependency loss", name)
		}
	}
	if sp.IsRunning("app-4") {
		t.Fatal("app-4 should be blocked after db dies between wave batches")
	}
}

func TestCommitStartResult_ClearsPendingCreateClaimBeforeHashBatch(t *testing.T) {
	store := &failingMetadataBatchStore{MemStore: beads.NewMemStore(), failBatch: true}
	bead, err := store.Create(beads.Bead{
		Title:  "helper",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":          "sky",
			"session_name_explicit": "true",
			"pending_create_claim":  "true",
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
	if got.Metadata["pending_create_claim"] != "" {
		t.Fatalf("pending_create_claim = %q, want cleared", got.Metadata["pending_create_claim"])
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

func TestCommitStartResult_DoesNotClearFreshPendingCreateClaimInHashBatch(t *testing.T) {
	store := &injectPendingCreateAfterClearStore{MemStore: beads.NewMemStore()}
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

	ok := commitStartResult(result, store, &clock.Fake{Time: time.Date(2026, 3, 18, 12, 0, 1, 0, time.UTC)}, events.Discard, 0, ioDiscard{}, ioDiscard{})
	if !ok {
		t.Fatal("commitStartResult returned false for successful start")
	}

	got, err := store.Get(bead.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Metadata["pending_create_claim"] != "true" {
		t.Fatalf("pending_create_claim = %q, want fresh claim preserved by hash batch", got.Metadata["pending_create_claim"])
	}
}

func TestExecutePlannedStarts_UsesLogicalTemplateForDependencyRechecks(t *testing.T) {
	sp := &dropDependencyAfterNStartsProvider{
		Fake:      runtime.NewFake(),
		dropAfter: defaultMaxParallelStartsPerWave,
		depName:   "db",
	}
	if err := sp.Start(context.Background(), "db", runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
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

	woken := executePlannedStarts(
		context.Background(), candidates, cfg, desired, sp, store, "",
		clk, events.Discard, 5*time.Second, ioDiscard{}, ioDiscard{},
	)

	if woken != defaultMaxParallelStartsPerWave {
		t.Fatalf("woken = %d, want %d", woken, defaultMaxParallelStartsPerWave)
	}
	for _, name := range []string{"app-1", "app-2", "app-3"} {
		if !sp.IsRunning(name) {
			t.Fatalf("%s should have started before dependency loss", name)
		}
	}
	if sp.IsRunning("app-4") {
		t.Fatal("app-4 should be blocked after db dies between batches even when bead template is stale")
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
		}, cfg, sp, rec, "gc", &stdout, &stderr)
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
		}, cfg, sp, rec, "gc", &stdout, &stderr)
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
	sent := interruptTargetsBounded([]stopTarget{{name: "worker", template: "worker", resolved: true}}, sp, &stderr)
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
		done <- interruptTargetsBounded(targets, sp, ioDiscard{})
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
		done <- interruptTargetsBounded(targets, sp, ioDiscard{})
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
	// Start both sessions.
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
	sent := interruptTargetsBounded(targets, sp, &stderr)
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
		[]preparedStart{{candidate: startCandidate{session: &beads.Bead{Metadata: map[string]string{"session_name": "worker"}}}}},
		&panicStartProvider{Fake: runtime.NewFake()},
		time.Second,
		1,
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
	}, sp, rec, "gc", &stdout, &stderr)
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

	waves, ok := candidateWaveOrder(candidates, cfg, map[string]TemplateParams{}, runtime.NewFake(), "city", nil)
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

	waves, ok := candidateWaveOrder(candidates, cfg, map[string]TemplateParams{}, runtime.NewFake(), "city", store)
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

func TestExecutePreparedStartWave_StaleSessionKeyDetected(t *testing.T) {
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
		10*time.Second,
		1,
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
		10*time.Second,
		1,
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
