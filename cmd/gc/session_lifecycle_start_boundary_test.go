package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/agent"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/rollout/gate"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/session/sessiontest"
	"github.com/gastownhall/gascity/internal/suspensionstate"
	"github.com/gastownhall/gascity/internal/worker"
)

type blockingStartProvider struct {
	runtime.Provider
	started chan struct{}
	release chan struct{}
}

func (p *blockingStartProvider) Start(ctx context.Context, name string, cfg runtime.Config) error {
	close(p.started)
	select {
	case <-p.release:
		return p.Provider.Start(ctx, name, cfg)
	case <-ctx.Done():
		return ctx.Err()
	}
}

type startUnavailableLivenessProvider struct {
	*runtime.Fake
}

func (p *startUnavailableLivenessProvider) ObserveLivenessWithError(name string, processNames []string) (runtime.Liveness, error) {
	return runtime.ObserveLiveness(p.Fake, name, processNames), fmt.Errorf("start preflight: %w", runtime.ErrRuntimeUnavailable)
}

type startConfirmedAbsentProvider struct {
	*runtime.Fake
}

func (p *startConfirmedAbsentProvider) ObserveLivenessWithError(string, []string) (runtime.Liveness, error) {
	return runtime.Liveness{}, nil
}

type goneStartIdentityProvider struct {
	*runtime.Fake
	goneKey string
}

func (p *goneStartIdentityProvider) ObserveLivenessWithError(string, []string) (runtime.Liveness, error) {
	return runtime.Liveness{}, nil
}

func (p *goneStartIdentityProvider) GetMeta(name, key string) (string, error) {
	if key == p.goneKey {
		return "", runtime.ErrSessionNotFound
	}
	return p.Fake.GetMeta(name, key)
}

type wedgedStartIdentityProvider struct {
	*runtime.Fake
	getMetaEntered  chan struct{}
	releaseGetMeta  chan struct{}
	getMetaReturned chan struct{}
	once            sync.Once
	releaseOnce     sync.Once
	mu              sync.Mutex
	getMetaCalls    int
}

func (p *wedgedStartIdentityProvider) ObserveLivenessWithError(string, []string) (runtime.Liveness, error) {
	return runtime.Liveness{}, nil
}

func (p *wedgedStartIdentityProvider) GetMeta(name, key string) (string, error) {
	p.mu.Lock()
	p.getMetaCalls++
	call := p.getMetaCalls
	p.mu.Unlock()
	if call == 1 {
		p.once.Do(func() {
			close(p.getMetaEntered)
			<-p.releaseGetMeta
		})
		defer close(p.getMetaReturned)
	}
	return p.Fake.GetMeta(name, key)
}

func (p *wedgedStartIdentityProvider) unblockGetMeta() {
	p.releaseOnce.Do(func() { close(p.releaseGetMeta) })
}

func (p *wedgedStartIdentityProvider) metadataReadCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.getMetaCalls
}

type sequencedStartLivenessProvider struct {
	*runtime.Fake
	observations []runtime.Liveness
	errors       []error
	calls        int
}

// providerStartCollisionLivenessProvider hides a pre-existing runtime from
// the session-layer IsRunning check so the test reaches Provider.Start, where
// the fake then reports ErrSessionExists for that occupied name.
type providerStartCollisionLivenessProvider struct {
	*sequencedStartLivenessProvider
}

func (*providerStartCollisionLivenessProvider) IsRunning(string) bool { return false }

type scriptedStartBoundaryProvider struct {
	*runtime.Fake
	mu                sync.Mutex
	livenessCalls     map[string]int
	metadataCalls     map[string]int
	observeLivenessFn func(string, int, []string) (runtime.Liveness, error)
	getMetaFn         func(string, string, int) (string, error)
	startFn           func(context.Context, string, runtime.Config) error
}

func (p *scriptedStartBoundaryProvider) ObserveLivenessWithError(name string, processNames []string) (runtime.Liveness, error) {
	p.mu.Lock()
	if p.livenessCalls == nil {
		p.livenessCalls = make(map[string]int)
	}
	p.livenessCalls[name]++
	call := p.livenessCalls[name]
	fn := p.observeLivenessFn
	p.mu.Unlock()
	if fn != nil {
		return fn(name, call, processNames)
	}
	return runtime.ObserveLiveness(p.Fake, name, processNames), nil
}

func (p *scriptedStartBoundaryProvider) GetMeta(name, key string) (string, error) {
	p.mu.Lock()
	if p.metadataCalls == nil {
		p.metadataCalls = make(map[string]int)
	}
	p.metadataCalls[name]++
	call := p.metadataCalls[name]
	fn := p.getMetaFn
	p.mu.Unlock()
	if fn != nil {
		return fn(name, key, call)
	}
	return p.Fake.GetMeta(name, key)
}

func (p *scriptedStartBoundaryProvider) Start(ctx context.Context, name string, cfg runtime.Config) error {
	p.mu.Lock()
	fn := p.startFn
	p.mu.Unlock()
	if fn != nil {
		return fn(ctx, name, cfg)
	}
	return p.Fake.Start(ctx, name, cfg)
}

func (p *scriptedStartBoundaryProvider) livenessCallCount(name string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.livenessCalls[name]
}

func (p *scriptedStartBoundaryProvider) metadataCallCount(name string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.metadataCalls[name]
}

type staleIdentityStartProvider struct {
	*runtime.Fake
	sessionID  string
	token      string
	generation string
}

func (p *staleIdentityStartProvider) Start(ctx context.Context, name string, cfg runtime.Config) error {
	if err := p.Fake.Start(ctx, name, cfg); err != nil {
		return err
	}
	for key, value := range map[string]string{
		"GC_SESSION_ID":     p.sessionID,
		"GC_INSTANCE_TOKEN": p.token,
		"GC_RUNTIME_EPOCH":  p.generation,
	} {
		if err := p.Fake.SetMeta(name, key, value); err != nil {
			return err
		}
	}
	return nil
}

func (p *sequencedStartLivenessProvider) ObserveLivenessWithError(name string, processNames []string) (runtime.Liveness, error) {
	if p.calls < len(p.observations) {
		index := p.calls
		obs := p.observations[index]
		p.calls++
		var err error
		if index < len(p.errors) {
			err = p.errors[index]
		}
		return obs, err
	}
	return runtime.ObserveLiveness(p.Fake, name, processNames), nil
}

// initializingPriorIdentityProvider models a provider that refuses to start
// over a young runtime, while its pod/container metadata still identifies the
// incarnation that existed before PreWake.
type initializingPriorIdentityProvider struct {
	*runtime.Fake
	identity    map[string]string
	startErr    error
	livenessErr error
	live        bool
	starts      int
}

func (p *initializingPriorIdentityProvider) IsRunning(string) bool { return p.live }

func (p *initializingPriorIdentityProvider) ObserveLivenessWithError(string, []string) (runtime.Liveness, error) {
	return runtime.Liveness{Running: p.live, Alive: p.live}, p.livenessErr
}

func (p *initializingPriorIdentityProvider) GetMeta(_ string, key string) (string, error) {
	if value, ok := p.identity[key]; ok {
		return value, nil
	}
	return "", nil
}

func (p *initializingPriorIdentityProvider) Start(ctx context.Context, name string, cfg runtime.Config) error {
	p.starts++
	if p.startErr != nil {
		return p.startErr
	}
	return p.Fake.Start(ctx, name, cfg)
}

func TestExecutePreparedStartWaveUsesWorkerBoundaryForKnownSession(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := newSessionManagerWithConfig("", store, sp, nil)
	info, err := mgr.CreateSession(context.Background(), sessionpkg.CreateOptions{BeadOnly: true, Template: "worker", Title: "Worker", Command: "claude", WorkDir: t.TempDir(), Provider: "claude", Transport: "", Resume: sessionpkg.ProviderResume{}})
	if err != nil {
		t.Fatalf("CreateBeadOnly: %v", err)
	}
	bead, err := store.Get(info.ID)
	if err != nil {
		t.Fatalf("Get bead: %v", err)
	}

	city := t.TempDir()
	item, release := leasedStart(t, city, preparedStart{
		candidate: startCandidate{
			info: sessiontest.SeedBead(t, bead),
			tp:   TemplateParams{TemplateName: "worker"},
		},
		cfg: runtime.Config{
			Command: "claude --resume seeded-session",
			WorkDir: info.WorkDir,
		},
	})
	defer release()
	results := executePreparedStartWaveForCity(context.Background(), []preparedStart{item}, city, sp, store, nil, 10*time.Second, 1)
	if len(results) != 1 {
		t.Fatalf("len(results) = %d, want 1", len(results))
	}
	if results[0].err != nil {
		t.Fatalf("start result err = %v, want nil", results[0].err)
	}

	got, err := mgr.Get(info.ID)
	if err != nil {
		t.Fatalf("Get session: %v", err)
	}
	if got.State != sessionpkg.StateStartPending {
		t.Fatalf("state = %q, want %q before lifecycle commit", got.State, sessionpkg.StateStartPending)
	}
	updatedBead, err := store.Get(info.ID)
	if err != nil {
		t.Fatalf("Get updated bead: %v", err)
	}
	if updatedBead.Metadata["pending_create_claim"] != "true" {
		t.Fatalf("pending_create_claim = %q, want preserved before commit", updatedBead.Metadata["pending_create_claim"])
	}
	if !sp.IsRunning(info.SessionName) {
		t.Fatal("session should be running after prepared start")
	}
}

func TestStartPreparedStartCandidateUsesWorkerBoundaryForRuntimeOnlyTarget(t *testing.T) {
	city := t.TempDir()
	sp := runtime.NewFake()

	witness, err := startPreparedStartCandidate(
		context.Background(),
		context.Background(),
		preparedStart{
			candidate: startCandidate{
				info: sessionpkg.Info{SessionName: "legacy-runtime-only", SessionNameMetadata: "legacy-runtime-only"},
				tp:   TemplateParams{TemplateName: "worker"},
			},
			cfg: runtime.Config{
				Command: "claude --resume seeded",
				WorkDir: t.TempDir(),
			},
		},
		city,
		nil,
		sp,
		nil,
		nil,
		nil,
		nil,
	)
	if err != nil {
		t.Fatalf("startPreparedStartCandidate: %v", err)
	}
	if witness != sessionpkg.StartRuntimeWitnessProviderStart {
		t.Fatalf("start witness = %v, want provider start", witness)
	}
	if !sp.IsRunning("legacy-runtime-only") {
		t.Fatal("legacy-runtime-only should be running after prepared start")
	}
	var start runtime.Call
	foundStart := false
	for _, call := range sp.Calls {
		if call.Method == "Start" {
			start = call
			foundStart = true
			break
		}
	}
	if !foundStart {
		t.Fatalf("runtime calls = %#v, want Start", sp.Calls)
	}
	if start.Name != "legacy-runtime-only" {
		t.Fatalf("start name = %q, want legacy-runtime-only", start.Name)
	}
	if start.Config.Command != "claude --resume seeded" {
		t.Fatalf("start command = %q, want claude --resume seeded", start.Config.Command)
	}
}

func TestPrepareStartCandidateDoesNotRotateIdentityForLiveRuntime(t *testing.T) {
	env := newReconcilerTestEnv(t)
	bead := env.createSessionBead("worker", "worker")
	env.markSessionActive(&bead)
	const token = "test-token"
	env.setSessionMetadata(&bead, map[string]string{"instance_token": token})
	info := env.sessionInfo(bead.ID)
	if err := env.sp.Start(context.Background(), "worker", runtime.Config{
		Command:      "codex",
		ProcessNames: []string{"codex"},
	}); err != nil {
		t.Fatalf("starting existing runtime: %v", err)
	}
	for key, value := range map[string]string{
		"GC_SESSION_ID":     info.ID,
		"GC_INSTANCE_TOKEN": token,
		"GC_RUNTIME_EPOCH":  info.Generation,
	} {
		if err := env.sp.SetMeta("worker", key, value); err != nil {
			t.Fatalf("setting runtime identity %s: %v", key, err)
		}
	}

	lease, release, err := tryRuntimeLease(env.store, env.city, "worker", info.ID, time.Minute)
	if err != nil {
		t.Fatalf("taking runtime lease: %v", err)
	}
	defer release()

	candidate := startCandidate{
		info:  info,
		lease: lease,
		tp: TemplateParams{
			Command:      "codex",
			SessionName:  "worker",
			TemplateName: "worker",
			Hints:        agent.StartupHints{ProcessNames: []string{"codex"}},
		},
	}
	prepared, err := prepareStartCandidateForCity(candidate, env.city, "test-city", env.cfg, env.sp, env.store, env.clk, io.Discard, nil, dispatchOptionSources{})
	if err != nil || prepared == nil || !prepared.candidate.reuseExistingRuntime {
		t.Fatalf("prepareStartCandidateForCity = (%v, %v), want an attributed warm-reuse candidate", prepared, err)
	}

	got := env.sessionInfo(info.ID)
	if got.InstanceToken != token || got.Generation != info.Generation {
		t.Fatalf("session identity after skipped wake = (%q, %q), want unchanged (%q, %q)", got.InstanceToken, got.Generation, token, info.Generation)
	}
	if got.MetadataState != "active" || got.PendingCreateStartedAt != "" {
		t.Fatalf("session wake metadata after skipped wake = (state %q, pending_create_started_at %q), want active with no pending start", got.MetadataState, got.PendingCreateStartedAt)
	}
	runtimeToken, err := env.sp.GetMeta("worker", "GC_INSTANCE_TOKEN")
	if err != nil {
		t.Fatalf("reading runtime identity: %v", err)
	}
	if runtimeToken != got.InstanceToken {
		t.Fatalf("runtime token = %q, session token = %q; warm reuse must keep their identities aligned", runtimeToken, got.InstanceToken)
	}
}

func identityPlannerCandidate(t *testing.T, env *reconcilerTestEnv, state string) (startCandidate, beads.Bead, TemplateParams) {
	t.Helper()
	configureAlwaysNamedSession(env)
	bead := createCircuitTestNamedSession(t, env, state)
	trigger, err := env.store.Create(beads.Bead{Title: "work", Type: "task", Status: "open"})
	if err != nil {
		t.Fatalf("creating trigger work bead: %v", err)
	}
	tp := TemplateParams{
		Command:      "codex",
		SessionName:  "session-a",
		TemplateName: "template-a",
		Hints:        agent.StartupHints{ProcessNames: []string{"codex"}},
	}
	env.setSessionMetadata(&bead, map[string]string{
		"template":                        "rig-a/template-a",
		"generation":                      "1",
		"instance_token":                  "row-token",
		"pool_managed":                    "true",
		beadmeta.TriggerBeadIDMetadataKey: trigger.ID,
	})
	if state == "active" {
		env.setSessionMetadata(&bead, map[string]string{"last_woke_at": env.clk.Now().UTC().Format(time.RFC3339)})
	}
	return startCandidate{info: env.sessionInfo(bead.ID), tp: tp}, bead, tp
}

func enableConditionalWritesForStartTest(t *testing.T, env *reconcilerTestEnv) {
	t.Helper()
	mem, ok := env.store.(*beads.MemStore)
	if !ok {
		t.Fatalf("start test store has type %T, want *beads.MemStore", env.store)
	}
	if err := beads.StampOpenedStore(mem, "MemStore", gate.Auto, false, nil, nil); err != nil {
		t.Fatalf("enabling conditional writes for start test: %v", err)
	}
}

func startIdentityRuntime(t *testing.T, env *reconcilerTestEnv, candidate startCandidate) {
	t.Helper()
	fence, ok := sessionpkg.InputFenceFor(candidate.info)
	if !ok {
		t.Fatal("candidate has no input fence")
	}
	if err := env.sp.Start(context.Background(), candidate.name(), runtime.Config{
		Command:      "codex",
		ProcessNames: []string{"codex"},
		Env: map[string]string{
			runtime.RuntimeEpochEnv:      fmt.Sprint(fence.Generation),
			runtime.ContinuationEpochEnv: fmt.Sprint(fence.Epoch),
			runtime.InstanceTokenEnv:     fence.Token,
			"BEADS_HOLDER_TOKEN":         fence.Token,
		},
	}); err != nil {
		t.Fatalf("starting existing runtime: %v", err)
	}
	for key, value := range map[string]string{
		"GC_SESSION_ID":     candidate.info.ID,
		"GC_INSTANCE_TOKEN": candidate.info.InstanceToken,
		"GC_RUNTIME_EPOCH":  candidate.info.Generation,
	} {
		if err := env.sp.SetMeta(candidate.name(), key, value); err != nil {
			t.Fatalf("setting runtime identity %s: %v", key, err)
		}
	}
}

func identityPlannerSiblingCandidate(t *testing.T, env *reconcilerTestEnv, name string) (startCandidate, TemplateParams) {
	t.Helper()
	bead := createCircuitTestNamedSessionWithIdentity(t, env, name, "template-a", "rig-a/"+name, "active")
	trigger, err := env.store.Create(beads.Bead{Title: "work-" + name, Type: "task", Status: "open"})
	if err != nil {
		t.Fatalf("creating trigger work bead for %s: %v", name, err)
	}
	env.setSessionMetadata(&bead, map[string]string{
		"template":                        "rig-a/template-a",
		"generation":                      "1",
		"instance_token":                  "row-token-" + name,
		"pool_managed":                    "true",
		beadmeta.TriggerBeadIDMetadataKey: trigger.ID,
	})
	tp := TemplateParams{
		Command:      "codex",
		SessionName:  name,
		TemplateName: "template-a",
		Hints:        agent.StartupHints{ProcessNames: []string{"codex"}},
	}
	return startCandidate{info: env.sessionInfo(bead.ID), tp: tp}, tp
}

func assertNoProviderMutationCalls(t *testing.T, calls []runtime.Call, names ...string) {
	t.Helper()
	watched := make(map[string]bool, len(names))
	for _, name := range names {
		watched[name] = true
	}
	for _, call := range calls {
		if !watched[call.Name] {
			continue
		}
		switch call.Method {
		case "SetMeta", "RemoveMeta", "Stop":
			t.Fatalf("provider call %s(%q, %q) for a store-only start result", call.Method, call.Name, call.Key)
		}
	}
}

type closeRowOnLivenessProvider struct {
	*runtime.Fake
	store beads.Store
	id    string
}

func (p *closeRowOnLivenessProvider) ObserveLivenessWithError(name string, processNames []string) (runtime.Liveness, error) {
	if err := p.store.Close(p.id); err != nil {
		return runtime.Liveness{}, err
	}
	return runtime.ObserveLiveness(p.Fake, name, processNames), nil
}

func runIdentityPlanner(t *testing.T, env *reconcilerTestEnv, candidate startCandidate, tp TemplateParams, async bool) int {
	return runIdentityPlannerWithProvider(t, env, candidate, tp, async, env.sp)
}

func runIdentityPlannerWithProvider(t *testing.T, env *reconcilerTestEnv, candidate startCandidate, tp TemplateParams, async bool, sp runtime.Provider) int {
	t.Helper()
	options := []startExecutionOption{
		withWarmClaimProbe(alwaysUnclaimed),
		withStartStabilityWaiter(immediateStartStabilityWaiter),
		withSessionStaleKeyDetectionWaiter(immediateSessionStaleKeyDetectionWaiter),
	}
	var tracker *asyncStartTracker
	if async {
		tracker = &asyncStartTracker{}
		options = append(options, withAsyncStartExecution(), withAsyncStartTracker(tracker))
	}
	woken := executePlannedStartsTraced(
		context.Background(), []startCandidate{candidate}, env.cfg,
		map[string]TemplateParams{tp.SessionName: tp}, sp, env.store,
		"test-city", env.city, env.clk, events.Discard, 5*time.Second,
		io.Discard, &env.stderr, nil, options...,
	)
	if tracker != nil && !tracker.wait(10*time.Second) {
		t.Fatal("async start tracker did not drain")
	}
	return woken
}

func TestExecutePlannedStartsSkipsLiveSnapshotMismatchWithoutRotatingIdentity(t *testing.T) {
	for _, async := range []bool{false, true} {
		name := "sync"
		if async {
			name = "async"
		}
		t.Run(name, func(t *testing.T) {
			env := newReconcilerTestEnv(t)
			candidate, bead, tp := identityPlannerCandidate(t, env, "active")
			// This test asserts the warm claim nudge path, so configure a template
			// prompt mode that permits claim instructions.
			env.cfg.Agents[0].PromptMode = "arg"
			// The candidate is the planner's not-alive snapshot. The runtime
			// becomes live before the executor takes its name lease.
			startIdentityRuntime(t, env, candidate)
			if err := setReconcilerDrainAckMetadata(env.sp, candidate.name(), &drainState{reason: "orphaned", generation: 1}); err != nil {
				t.Fatalf("setting drain ack: %v", err)
			}
			cb := newSessionCircuitBreaker(sessionCircuitBreakerConfig{})
			setSessionCircuitBreakerForTest(t, cb)
			identity := namedSessionIdentityInfo(candidate.info)
			cb.RecordRestart(identity, env.clk.Now().Add(-time.Minute))
			beforeBreaker := cb.entrySnapshot(identity)

			if got := runIdentityPlanner(t, env, candidate, tp, async); got != 1 {
				t.Fatalf("wake count = %d, want 1 for committed warm reuse", got)
			}
			got := env.sessionInfo(bead.ID)
			if got.InstanceToken != candidate.info.InstanceToken || got.Generation != candidate.info.Generation {
				t.Fatalf("session identity = (%q, %q), want unchanged (%q, %q)", got.InstanceToken, got.Generation, candidate.info.InstanceToken, candidate.info.Generation)
			}
			if got.State != sessionpkg.StateActive {
				t.Fatalf("state = %q, want active", got.State)
			}
			if got.StartedConfigHash == "" {
				t.Fatal("started_config_hash is empty after warm reuse commit")
			}
			if got.WakeRequest != "" || got.WakeRequestedAt != "" {
				t.Fatalf("wake request remains after warm reuse commit: request=%q at=%q", got.WakeRequest, got.WakeRequestedAt)
			}
			if env.sp.CountCalls("Start", candidate.name()) != 1 {
				t.Fatalf("Start calls = %d, want only the pre-existing runtime start", env.sp.CountCalls("Start", candidate.name()))
			}
			if n, _ := countNudges(env.sp); n != 1 {
				t.Fatalf("claim nudge count = %d, want exactly 1", n)
			}
			runtimeToken, err := env.sp.GetMeta(candidate.name(), "GC_INSTANCE_TOKEN")
			if err != nil || runtimeToken != got.InstanceToken {
				t.Fatalf("runtime token = %q (err %v), session token = %q", runtimeToken, err, got.InstanceToken)
			}
			if after := cb.entrySnapshot(identity); !reflect.DeepEqual(after, beforeBreaker) {
				t.Fatalf("breaker snapshot after a warm reuse = %#v, want %#v", after, beforeBreaker)
			}
			for _, key := range []string{"GC_DRAIN_ACK", reconcilerDrainAckSourceKey, reconcilerDrainAckReasonKey, reconcilerDrainAckGenerationKey} {
				value, err := env.sp.GetMeta(candidate.name(), key)
				if err != nil || value != "" {
					t.Errorf("drain metadata %s = %q (err %v), want cleared", key, value, err)
				}
			}
			raw, err := env.store.Get(bead.ID)
			if err != nil {
				t.Fatalf("reading lease record: %v", err)
			}
			if raw.Metadata[sessionpkg.RuntimeLeaseHolderKey] != "" {
				t.Fatalf("runtime lease holder = %q, want cleared", raw.Metadata[sessionpkg.RuntimeLeaseHolderKey])
			}
		})
	}
}

func TestExecutePlannedStartsKeepsPriorIdentityWhenRuntimeAppearsAfterPreWake(t *testing.T) {
	env := newReconcilerTestEnv(t)
	candidate, bead, tp := identityPlannerCandidate(t, env, "active")
	env.cfg.Agents[0].PromptMode = "arg"
	startIdentityRuntime(t, env, candidate)
	oldToken, oldGeneration, oldEpoch, oldLastWoke := candidate.info.InstanceToken, candidate.info.Generation, candidate.info.ContinuationEpoch, candidate.info.LastWokeAt
	sp := &sequencedStartLivenessProvider{
		Fake: env.sp,
		observations: []runtime.Liveness{
			{},                           // preparation sees the planner's stale not-running snapshot
			{Running: true, Alive: true}, // start-time recheck finds the existing worker
		},
	}
	setSessionCircuitBreakerForTest(t, newSessionCircuitBreaker(sessionCircuitBreakerConfig{}))
	if got := runIdentityPlannerWithProvider(t, env, candidate, tp, false, sp); got != 1 {
		t.Fatalf("wake count = %d, want 1 for warm reuse after pre-wake race", got)
	}
	got := env.sessionInfo(bead.ID)
	if got.InstanceToken != oldToken || got.Generation != oldGeneration || got.ContinuationEpoch != oldEpoch || got.LastWokeAt != oldLastWoke {
		t.Fatalf("identity after post-prewake warm reuse = (%q,%q,%q) last_woke_at=%q, want prior (%q,%q,%q) last_woke_at=%q", got.InstanceToken, got.Generation, got.ContinuationEpoch, got.LastWokeAt, oldToken, oldGeneration, oldEpoch, oldLastWoke)
	}
	runtimeToken, err := env.sp.GetMeta(candidate.name(), "GC_INSTANCE_TOKEN")
	if err != nil || runtimeToken != got.InstanceToken {
		t.Fatalf("runtime token = %q (err %v), session token = %q", runtimeToken, err, got.InstanceToken)
	}
	if got.StartedConfigHash == "" || got.State != sessionpkg.StateActive {
		t.Fatalf("warm reuse did not commit lifecycle/config state: state=%q hash=%q", got.State, got.StartedConfigHash)
	}
	if env.sp.CountCalls("Start", candidate.name()) != 1 || env.sp.CountCalls("Stop", candidate.name()) != 0 {
		t.Fatalf("runtime calls Start=%d Stop=%d, want only the existing runtime", env.sp.CountCalls("Start", candidate.name()), env.sp.CountCalls("Stop", candidate.name()))
	}
	if n, _ := countNudges(env.sp); n != 1 {
		t.Fatalf("claim nudge count = %d, want exactly 1", n)
	}
}

func TestExecutePlannedStartsRestoresPriorIdentityFromSessionLayerReuseWitness(t *testing.T) {
	env := newReconcilerTestEnv(t)
	candidate, bead, tp := identityPlannerCandidate(t, env, "active")
	env.cfg.Agents[0].PromptMode = "arg"
	startIdentityRuntime(t, env, candidate)
	oldToken, oldGeneration, oldEpoch, oldLastWoke := candidate.info.InstanceToken, candidate.info.Generation, candidate.info.ContinuationEpoch, candidate.info.LastWokeAt
	sp := &sequencedStartLivenessProvider{
		Fake: env.sp,
		observations: []runtime.Liveness{
			{}, // preparation cannot see the already-running runtime
			{}, // start-boundary probe still reports absent
		},
	}
	setSessionCircuitBreakerForTest(t, newSessionCircuitBreaker(sessionCircuitBreakerConfig{}))
	if got := runIdentityPlannerWithProvider(t, env, candidate, tp, false, sp); got != 1 {
		t.Fatalf("wake count = %d, want 1 for the session-layer IsRunning reuse", got)
	}
	got := env.sessionInfo(bead.ID)
	if got.InstanceToken != oldToken || got.Generation != oldGeneration || got.ContinuationEpoch != oldEpoch || got.LastWokeAt != oldLastWoke {
		t.Fatalf("identity after session-layer reuse = (%q,%q,%q) last_woke_at=%q, want prior (%q,%q,%q) last_woke_at=%q", got.InstanceToken, got.Generation, got.ContinuationEpoch, got.LastWokeAt, oldToken, oldGeneration, oldEpoch, oldLastWoke)
	}
	if starts := env.sp.CountCalls("Start", candidate.name()); starts != 1 {
		t.Fatalf("provider Start calls = %d, want only the pre-existing runtime start", starts)
	}
}

func prepareIdentityTestStart(t *testing.T, env *reconcilerTestEnv, candidate startCandidate, sp runtime.Provider) (*preparedStart, func()) {
	t.Helper()
	leased, release := leasedStart(t, env.city, preparedStart{candidate: candidate})
	prepared, err := prepareStartCandidateForCity(leased.candidate, env.city, "test-city", env.cfg, sp, env.store, env.clk, io.Discard, nil, dispatchOptionSources{})
	if err != nil {
		release()
		t.Fatalf("preparing start: %v", err)
	}
	return prepared, release
}

func TestSessionLayerReuseWithGoneIdentityDefers(t *testing.T) {
	for _, key := range []string{"GC_SESSION_ID", "GC_INSTANCE_TOKEN"} {
		t.Run(key, func(t *testing.T) {
			env := newReconcilerTestEnv(t)
			candidate, _, _ := identityPlannerCandidate(t, env, "active")
			startIdentityRuntime(t, env, candidate)
			sp := &goneStartIdentityProvider{Fake: env.sp, goneKey: key}
			prepared, release := prepareIdentityTestStart(t, env, candidate, sp)
			defer release()

			witness, err := startPreparedStartCandidate(
				context.Background(), context.Background(), *prepared, env.city, env.store, sp, env.cfg,
				nil, immediateSessionStaleKeyDetectionWaiter, alwaysUnclaimed,
			)
			if witness != sessionpkg.StartRuntimeWitnessReused {
				t.Fatalf("start witness = %v, want session-layer reuse", witness)
			}
			if !errors.Is(err, runtime.ErrRuntimeUnavailable) {
				t.Fatalf("start error = %v, want runtime.ErrRuntimeUnavailable when %s is gone", err, key)
			}
			if !errors.Is(err, errStartRuntimeIdentityUnavailable) {
				t.Fatalf("start error = %v, want the unavailable identity marker", err)
			}
			if starts := env.sp.CountCalls("Start", candidate.name()); starts != 1 {
				t.Fatalf("provider Start calls = %d, want only the pre-existing runtime start", starts)
			}

			result := runPreparedStartCandidate(
				context.Background(), *prepared, env.city, sp, env.store, env.cfg,
				5*time.Second, immediateStartStabilityWaiter, immediateSessionStaleKeyDetectionWaiter, alwaysUnclaimed,
			)
			if result.outcome != TraceOutcomeDeferred || result.err != nil || !result.storeOnly {
				t.Fatalf("start result = outcome %q, err %v, store-only %v; want a store-only deferral after identity disappeared", result.outcome, result.err, result.storeOnly)
			}
			if result.providerStartCalled || result.providerStartMayHaveBeenCalled {
				t.Fatalf("start result reports Provider.Start: called=%v may-have-been-called=%v", result.providerStartCalled, result.providerStartMayHaveBeenCalled)
			}
			if starts := env.sp.CountCalls("Start", candidate.name()); starts != 1 {
				t.Fatalf("provider Start calls after deferred reuse = %d, want only the pre-existing runtime start", starts)
			}
		})
	}
}

func TestExecutePlannedStartsLabelsUnansweredStartBoundaryIdentityAsStoreOnly(t *testing.T) {
	env := newReconcilerTestEnv(t)
	candidate, _, tp := identityPlannerCandidate(t, env, "active")
	startIdentityRuntime(t, env, candidate)
	var tokenReads int
	sp := &scriptedStartBoundaryProvider{
		Fake: env.sp,
		observeLivenessFn: func(name string, call int, processNames []string) (runtime.Liveness, error) {
			if call == 1 {
				return runtime.Liveness{}, nil // preparation's old runtime observation is absent
			}
			return runtime.ObserveLiveness(env.sp, name, processNames), nil
		},
		getMetaFn: func(name, key string, _ int) (string, error) {
			if key == "GC_INSTANCE_TOKEN" {
				tokenReads++
				if tokenReads > 1 {
					return "", fmt.Errorf("start-boundary identity: %w", worker.ErrObservationUnanswered)
				}
				return "another-incarnation", nil // prior identity is not a positive match
			}
			return env.sp.GetMeta(name, key)
		},
	}
	setSessionCircuitBreakerForTest(t, newSessionCircuitBreaker(sessionCircuitBreakerConfig{}))

	if got := runIdentityPlannerWithProvider(t, env, candidate, tp, false, sp); got != 0 {
		t.Fatalf("wake count = %d, want 0 for an unanswered start-boundary identity", got)
	}
	if !strings.Contains(env.stderr.String(), "outcome=start_store_only") {
		t.Fatalf("stderr %q does not label unanswered identity as store-only", env.stderr.String())
	}
	if strings.Contains(env.stderr.String(), "outcome=start_boundary_liveness_unanswered") {
		t.Fatalf("stderr %q mislabeled an identity observation as liveness", env.stderr.String())
	}
}

func TestExecutePlannedStartsKeepsMintedIdentityWhenCurrentIdentityMatches(t *testing.T) {
	env := newReconcilerTestEnv(t)
	candidate, bead, tp := identityPlannerCandidate(t, env, "active")
	startIdentityRuntime(t, env, candidate)
	var tokenReads int
	sp := &scriptedStartBoundaryProvider{
		Fake: env.sp,
		observeLivenessFn: func(name string, call int, processNames []string) (runtime.Liveness, error) {
			switch call {
			case 1:
				return runtime.Liveness{}, fmt.Errorf("pre-PreWake liveness: %w", worker.ErrObservationUnanswered)
			case 2:
				return runtime.Liveness{}, fmt.Errorf("start-boundary liveness: %w", runtime.ErrRuntimeUnavailable)
			default:
				return runtime.ObserveLiveness(env.sp, name, processNames), nil
			}
		},
		getMetaFn: func(name, key string, _ int) (string, error) {
			if key == "GC_INSTANCE_TOKEN" {
				tokenReads++
				if tokenReads == 1 {
					return env.sp.GetMeta(name, key) // prior identity matches T1
				}
				return env.sessionInfo(bead.ID).InstanceToken, nil // later read positively matches the minted fence
			}
			return env.sp.GetMeta(name, key)
		},
	}
	setSessionCircuitBreakerForTest(t, newSessionCircuitBreaker(sessionCircuitBreakerConfig{}))

	if got := runIdentityPlannerWithProvider(t, env, candidate, tp, false, sp); got != 0 {
		t.Fatalf("wake count = %d, want 0 for the unanswered pre-start observation", got)
	}
	got := env.sessionInfo(bead.ID)
	if got.InstanceToken == candidate.info.InstanceToken || got.Generation == candidate.info.Generation {
		t.Fatalf("current-identity positive match restored T1: token=%q generation=%q", got.InstanceToken, got.Generation)
	}
	if env.sp.CountCalls("Start", candidate.name()) != 1 {
		t.Fatalf("Provider.Start calls = %d, want only the prior runtime start", env.sp.CountCalls("Start", candidate.name()))
	}
}

func TestRunPreparedStartCandidateKeepsUnansweredPreflightWithoutPreWakeUndo(t *testing.T) {
	item := waveTestStart("runtime-only")
	item.preWakeLivenessUnanswered = true
	item.preWakeLivenessUnansweredErr = worker.ErrObservationUnanswered
	sp := &startUnavailableLivenessProvider{Fake: runtime.NewFake()}
	result := runPreparedStartCandidate(
		context.Background(), item, t.TempDir(), sp, nil, nil, time.Second,
		immediateStartStabilityWaiter, immediateSessionStaleKeyDetectionWaiter, alwaysUnclaimed,
	)
	if result.prepared.preWakeUndo.written != nil {
		t.Fatal("runtime-only start unexpectedly has a persisted PreWake undo")
	}
	if !result.prepared.preWakeLivenessUnanswered || !result.storeOnly || result.outcome != TraceOutcomeDeferred {
		t.Fatalf("result retained preflight-unanswered state=%v store-only=%v outcome=%q; want store-only deferral without a row snapshot",
			result.prepared.preWakeLivenessUnanswered, result.storeOnly, result.outcome)
	}
	if startResultRestoresPreWakeIdentity(result) {
		t.Fatal("runtime-only start attempted T1 restoration without a persisted PreWake snapshot")
	}
	if result.providerStartCalled || result.providerStartMayHaveBeenCalled || sp.CountCalls("Start", "runtime-only") != 0 {
		t.Fatalf("no-start result reports provider Start: called=%v may-have-been-called=%v calls=%d",
			result.providerStartCalled, result.providerStartMayHaveBeenCalled, sp.CountCalls("Start", "runtime-only"))
	}
}

func TestReadPendingCreateIdentityRetainsBoundedReadMarker(t *testing.T) {
	for name, marker := range map[string]error{
		"unanswered":  worker.ErrObservationUnanswered,
		"outstanding": worker.ErrObservationOutstanding,
	} {
		t.Run(name, func(t *testing.T) {
			identity := readPendingCreateIdentityVia(sessionpkg.Info{ID: "session", InstanceToken: "token", Generation: "1"}, runtime.NewFake(), func(string) (string, error) {
				return "", fmt.Errorf("bounded metadata read: %w", marker)
			})
			if !errors.Is(identity.unansweredErr, marker) {
				t.Fatalf("retained unanswered marker = %v, want errors.Is(_, %v)", identity.unansweredErr, marker)
			}
		})
	}
}

func TestExecutePlannedStartsIdentityReadUnansweredIsStoreOnly(t *testing.T) {
	env := newReconcilerTestEnv(t)
	candidate, bead, tp := identityPlannerCandidate(t, env, "active")
	startIdentityRuntime(t, env, candidate)
	if err := setReconcilerDrainAckMetadata(env.sp, candidate.name(), &drainState{reason: "orphaned", generation: 1}); err != nil {
		t.Fatalf("setting drain ack: %v", err)
	}
	callsBeforePlan := env.sp.SnapshotCalls()
	sp := &wedgedStartIdentityProvider{
		Fake:            env.sp,
		getMetaEntered:  make(chan struct{}),
		releaseGetMeta:  make(chan struct{}),
		getMetaReturned: make(chan struct{}),
	}
	setSessionCircuitBreakerForTest(t, newSessionCircuitBreaker(sessionCircuitBreakerConfig{}))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer sp.unblockGetMeta()
	canceled := make(chan struct{})
	go func() {
		<-sp.getMetaEntered
		cancel()
		close(canceled)
	}()

	if got := executePlannedStartsTraced(
		ctx, []startCandidate{candidate}, env.cfg, map[string]TemplateParams{tp.SessionName: tp}, sp, env.store,
		"test-city", env.city, env.clk, events.Discard, 5*time.Second, io.Discard, &env.stderr, nil,
		withWarmClaimProbe(alwaysUnclaimed),
		withStartStabilityWaiter(immediateStartStabilityWaiter),
		withSessionStaleKeyDetectionWaiter(immediateSessionStaleKeyDetectionWaiter),
	); got != 0 {
		t.Fatalf("wake count = %d, want 0 for an unanswered runtime identity read", got)
	}
	<-canceled
	callsBeforeRelease := env.sp.SnapshotCalls()
	if reads := sp.metadataReadCount(); reads != 1 {
		t.Fatalf("provider metadata reads = %d, want only the single wedged identity read", reads)
	}
	for _, call := range callsBeforeRelease[len(callsBeforePlan):] {
		if call.Name != candidate.name() {
			continue
		}
		switch call.Method {
		case "SetMeta", "RemoveMeta", "Stop":
			t.Fatalf("provider call %s(%q, %q) after the identity read wedged; the planner must leave the runtime alone", call.Method, call.Name, call.Key)
		}
	}
	if !strings.Contains(env.stderr.String(), "outcome=start_store_only") {
		t.Fatalf("stderr %q does not identify the store-only start deferral", env.stderr.String())
	}
	gotInfo := env.sessionInfo(bead.ID)
	if gotInfo.InstanceToken == candidate.info.InstanceToken || gotInfo.Generation == candidate.info.Generation {
		t.Fatalf("unverifiable Reused identity restored the prior fence: token=%q generation=%q", gotInfo.InstanceToken, gotInfo.Generation)
	}
	sp.unblockGetMeta()
	select {
	case <-sp.getMetaReturned:
	case <-time.After(10 * time.Second):
		t.Fatal("wedged metadata read did not return after release")
	}
}

func TestExecutePlannedStartsPreWakeIdentityReadUnansweredIsStoreOnly(t *testing.T) {
	env := newReconcilerTestEnv(t)
	candidate, bead, tp := identityPlannerCandidate(t, env, "active")
	startIdentityRuntime(t, env, candidate)
	if err := setReconcilerDrainAckMetadata(env.sp, candidate.name(), &drainState{reason: "orphaned", generation: 1}); err != nil {
		t.Fatalf("setting drain ack: %v", err)
	}
	before := env.sp.SnapshotCalls()
	sp := &scriptedStartBoundaryProvider{
		Fake: env.sp,
		getMetaFn: func(string, string, int) (string, error) {
			return "", fmt.Errorf("pre-wake identity probe: %w", worker.ErrObservationUnanswered)
		},
	}
	setSessionCircuitBreakerForTest(t, newSessionCircuitBreaker(sessionCircuitBreakerConfig{}))

	if got := executePlannedStartsTraced(
		context.Background(), []startCandidate{candidate}, env.cfg, map[string]TemplateParams{tp.SessionName: tp}, sp, env.store,
		"test-city", env.city, env.clk, events.Discard, 5*time.Second, io.Discard, &env.stderr, nil,
		withWarmClaimProbe(alwaysUnclaimed),
		withStartStabilityWaiter(immediateStartStabilityWaiter),
		withSessionStaleKeyDetectionWaiter(immediateSessionStaleKeyDetectionWaiter),
	); got != 0 {
		t.Fatalf("wake count = %d, want 0 for an unanswered pre-wake identity read", got)
	}
	assertNoProviderMutationCalls(t, env.sp.SnapshotCalls()[len(before):], candidate.name())
	if starts := env.sp.CountCalls("Start", candidate.name()); starts != 1 {
		t.Fatalf("provider Start calls = %d, want only the pre-existing runtime start", starts)
	}
	if !strings.Contains(env.stderr.String(), "outcome=start_store_only") {
		t.Fatalf("stderr %q does not identify the store-only preparation deferral", env.stderr.String())
	}
	if !strings.Contains(env.stderr.String(), "bead="+bead.ID) {
		t.Fatalf("stderr %q does not name the deferred session bead %s", env.stderr.String(), bead.ID)
	}
	if got := env.sessionInfo(bead.ID); got.InstanceToken != candidate.info.InstanceToken || got.Generation != candidate.info.Generation {
		t.Fatalf("pre-wake identity changed after an unanswered attribution: token=%q generation=%q", got.InstanceToken, got.Generation)
	}
}

func TestExecutePlannedStartsRetryIdentityReadUnansweredIsStoreOnly(t *testing.T) {
	env := newReconcilerTestEnv(t)
	candidate, bead, tp := identityPlannerCandidate(t, env, "active")
	startIdentityRuntime(t, env, candidate)
	if err := setReconcilerDrainAckMetadata(env.sp, candidate.name(), &drainState{reason: "orphaned", generation: 1}); err != nil {
		t.Fatalf("setting drain ack: %v", err)
	}
	before := env.sp.SnapshotCalls()
	sp := &scriptedStartBoundaryProvider{
		Fake: env.sp,
		observeLivenessFn: func(name string, call int, processNames []string) (runtime.Liveness, error) {
			switch call {
			case 1:
				return runtime.Liveness{}, nil // pre-wake sees absence
			case 2:
				return runtime.Liveness{}, fmt.Errorf("start-boundary probe: %w", runtime.ErrRuntimeUnavailable)
			default:
				return runtime.ObserveLiveness(env.sp, name, processNames), nil
			}
		},
		getMetaFn: func(string, string, int) (string, error) {
			return "", fmt.Errorf("retry identity probe: %w", worker.ErrObservationUnanswered)
		},
	}
	setSessionCircuitBreakerForTest(t, newSessionCircuitBreaker(sessionCircuitBreakerConfig{}))

	if got := executePlannedStartsTraced(
		context.Background(), []startCandidate{candidate}, env.cfg, map[string]TemplateParams{tp.SessionName: tp}, sp, env.store,
		"test-city", env.city, env.clk, events.Discard, 5*time.Second, io.Discard, &env.stderr, nil,
		withWarmClaimProbe(alwaysUnclaimed),
		withStartStabilityWaiter(immediateStartStabilityWaiter),
		withSessionStaleKeyDetectionWaiter(immediateSessionStaleKeyDetectionWaiter),
	); got != 0 {
		t.Fatalf("wake count = %d, want 0 for unanswered retry identity reads", got)
	}
	assertNoProviderMutationCalls(t, env.sp.SnapshotCalls()[len(before):], candidate.name())
	if sp.livenessCallCount(candidate.name()) != 3 {
		t.Fatalf("liveness probes = %d, want pre-wake, start-boundary and retry probes", sp.livenessCallCount(candidate.name()))
	}
	if sp.metadataCallCount(candidate.name()) != 4 {
		t.Fatalf("identity metadata reads = %d, want both keys for prior and current identities", sp.metadataCallCount(candidate.name()))
	}
	if !strings.Contains(env.stderr.String(), "outcome=start_store_only") {
		t.Fatalf("stderr %q does not identify the store-only retry deferral", env.stderr.String())
	}
	if got := env.sessionInfo(bead.ID); got.InstanceToken == candidate.info.InstanceToken || got.Generation == candidate.info.Generation {
		t.Fatalf("retry deferral restored the prior fence after later unanswered reads: token=%q generation=%q", got.InstanceToken, got.Generation)
	}
}

func TestExecutePlannedStartsCollisionObservationUnansweredIsStoreOnly(t *testing.T) {
	env := newReconcilerTestEnv(t)
	candidate, _, tp := identityPlannerCandidate(t, env, "active")
	startIdentityRuntime(t, env, candidate)
	if err := env.sp.SetMeta(candidate.name(), "GC_SESSION_ID", "another-session"); err != nil {
		t.Fatalf("setting foreign session id: %v", err)
	}
	if err := setReconcilerDrainAckMetadata(env.sp, candidate.name(), &drainState{reason: "orphaned", generation: 1}); err != nil {
		t.Fatalf("setting drain ack: %v", err)
	}
	before := env.sp.SnapshotCalls()
	sp := &scriptedStartBoundaryProvider{
		Fake: env.sp,
		observeLivenessFn: func(name string, call int, processNames []string) (runtime.Liveness, error) {
			switch call {
			case 1:
				return runtime.Liveness{}, nil // pre-wake sees absence
			case 2:
				return runtime.ObserveLiveness(env.sp, name, processNames), nil // foreign runtime appears at the start boundary
			default:
				return runtime.Liveness{}, fmt.Errorf("collision observation: %w", worker.ErrObservationUnanswered)
			}
		},
	}
	setSessionCircuitBreakerForTest(t, newSessionCircuitBreaker(sessionCircuitBreakerConfig{}))

	if got := executePlannedStartsTraced(
		context.Background(), []startCandidate{candidate}, env.cfg, map[string]TemplateParams{tp.SessionName: tp}, sp, env.store,
		"test-city", env.city, env.clk, events.Discard, 5*time.Second, io.Discard, &env.stderr, nil,
		withWarmClaimProbe(alwaysUnclaimed),
		withStartStabilityWaiter(immediateStartStabilityWaiter),
		withSessionStaleKeyDetectionWaiter(immediateSessionStaleKeyDetectionWaiter),
	); got != 0 {
		t.Fatalf("wake count = %d, want 0 for unanswered collision observation", got)
	}
	assertNoProviderMutationCalls(t, env.sp.SnapshotCalls()[len(before):], candidate.name())
	if sp.livenessCallCount(candidate.name()) != 3 {
		t.Fatalf("liveness probes = %d, want pre-wake, start-boundary and collision observations", sp.livenessCallCount(candidate.name()))
	}
	if !strings.Contains(env.stderr.String(), "outcome=start_store_only") {
		t.Fatalf("stderr %q does not identify the store-only collision deferral", env.stderr.String())
	}
}

func TestExecutePlannedStartsFailureIdentityReadUnansweredIsStoreOnly(t *testing.T) {
	env := newReconcilerTestEnv(t)
	candidate, bead, tp := identityPlannerCandidate(t, env, "creating")
	env.setSessionMetadata(&bead, map[string]string{"pending_create_claim": "true"})
	candidate.info = env.sessionInfo(bead.ID)
	env.cfg.Session.Provider = "subprocess" // skip the optional failure-screen peek
	startIdentityRuntime(t, env, candidate)
	if err := env.sp.SetMeta(candidate.name(), "GC_SESSION_ID", "another-session"); err != nil {
		t.Fatalf("setting foreign session id: %v", err)
	}
	if err := setReconcilerDrainAckMetadata(env.sp, candidate.name(), &drainState{reason: "orphaned", generation: 1}); err != nil {
		t.Fatalf("setting drain ack: %v", err)
	}
	before := env.sp.SnapshotCalls()
	sp := &scriptedStartBoundaryProvider{
		Fake: env.sp,
		observeLivenessFn: func(name string, call int, processNames []string) (runtime.Liveness, error) {
			switch call {
			case 1:
				return runtime.Liveness{}, nil // pre-wake sees absence
			case 2:
				return runtime.ObserveLiveness(env.sp, name, processNames), nil // foreign runtime appears at the start boundary
			default:
				return runtime.Liveness{}, nil // collision recheck no longer sees it
			}
		},
		getMetaFn: func(name, key string, call int) (string, error) {
			if call <= 2 {
				return env.sp.GetMeta(name, key)
			}
			return "", fmt.Errorf("failure attribution identity: %w", worker.ErrObservationUnanswered)
		},
	}
	setSessionCircuitBreakerForTest(t, newSessionCircuitBreaker(sessionCircuitBreakerConfig{}))

	if got := executePlannedStartsTraced(
		context.Background(), []startCandidate{candidate}, env.cfg, map[string]TemplateParams{tp.SessionName: tp}, sp, env.store,
		"test-city", env.city, env.clk, events.Discard, 5*time.Second, io.Discard, &env.stderr, nil,
		withWarmClaimProbe(alwaysUnclaimed),
		withStartStabilityWaiter(immediateStartStabilityWaiter),
		withSessionStaleKeyDetectionWaiter(immediateSessionStaleKeyDetectionWaiter),
	); got != 0 {
		t.Fatalf("wake count = %d, want 0 for unanswered failure attribution", got)
	}
	assertNoProviderMutationCalls(t, env.sp.SnapshotCalls()[len(before):], candidate.name())
	if sp.livenessCallCount(candidate.name()) != 3 {
		t.Fatalf("liveness probes = %d, want pre-wake, start-boundary and collision recheck", sp.livenessCallCount(candidate.name()))
	}
	if sp.metadataCallCount(candidate.name()) != 4 {
		t.Fatalf("identity metadata reads = %d, want two collision reads and both failure identity keys", sp.metadataCallCount(candidate.name()))
	}
	if !strings.Contains(env.stderr.String(), "outcome=start_store_only") {
		t.Fatalf("stderr %q does not identify the store-only failure-attribution deferral", env.stderr.String())
	}
}

func TestExecutePlannedStartsFreshStartIgnoresStaleRuntimeIdentityMetadata(t *testing.T) {
	env := newReconcilerTestEnv(t)
	candidate, bead, tp := identityPlannerCandidate(t, env, "active")
	staleToken, staleGeneration := candidate.info.InstanceToken, candidate.info.Generation
	sp := &staleIdentityStartProvider{Fake: env.sp, sessionID: candidate.info.ID, token: staleToken, generation: staleGeneration}
	setSessionCircuitBreakerForTest(t, newSessionCircuitBreaker(sessionCircuitBreakerConfig{}))
	if got := runIdentityPlannerWithProvider(t, env, candidate, tp, false, sp); got != 1 {
		t.Fatalf("wake count = %d, want 1 after a fresh provider.Start", got)
	}
	got := env.sessionInfo(bead.ID)
	if got.InstanceToken == staleToken || got.Generation == staleGeneration {
		t.Fatalf("session identity = (%q,%q), want the new pre-wake identity after Provider.Start", got.InstanceToken, got.Generation)
	}
	if got.State != sessionpkg.StateActive || got.StartedConfigHash == "" {
		t.Fatalf("fresh start did not commit: state=%q started_config_hash=%q", got.State, got.StartedConfigHash)
	}
	if runtimeToken, err := env.sp.GetMeta(candidate.name(), "GC_INSTANCE_TOKEN"); err != nil || runtimeToken != staleToken {
		t.Fatalf("stale runtime token = %q (err %v), want the deliberately stale value %q", runtimeToken, err, staleToken)
	}
}

func TestExecutePlannedStartsContinuesAfterUnansweredPreWakeLiveness(t *testing.T) {
	env := newReconcilerTestEnv(t)
	candidate, bead, tp := identityPlannerCandidate(t, env, "active")
	sp := &sequencedStartLivenessProvider{
		Fake:         env.sp,
		observations: []runtime.Liveness{{}, {}},
		errors:       []error{worker.ErrObservationUnanswered},
	}
	setSessionCircuitBreakerForTest(t, newSessionCircuitBreaker(sessionCircuitBreakerConfig{}))
	if got := runIdentityPlannerWithProvider(t, env, candidate, tp, false, sp); got != 1 {
		t.Fatalf("wake count = %d, want 1 after an unanswered pre-wake observation and confirmed absence", got)
	}
	got := env.sessionInfo(bead.ID)
	if got.InstanceToken == candidate.info.InstanceToken || got.Generation == candidate.info.Generation {
		t.Fatalf("session identity = (%q,%q), want PreWake to mint a new identity", got.InstanceToken, got.Generation)
	}
	if starts := env.sp.CountCalls("Start", candidate.name()); starts != 1 {
		t.Fatalf("provider Start calls = %d, want 1", starts)
	}
}

func TestExecutePlannedStartsDoesNotCountRepeatedWedgedPreWakeAsRestarts(t *testing.T) {
	env := newReconcilerTestEnv(t)
	enableConditionalWritesForStartTest(t, env)
	candidate, bead, tp := identityPlannerCandidate(t, env, "active")
	sp := newWedgedLivenessProvider(t, candidate.name())
	fence, ok := sessionpkg.InputFenceFor(candidate.info)
	if !ok {
		t.Fatal("candidate has no input fence")
	}
	if err := sp.Fake.Start(context.Background(), candidate.name(), runtime.Config{
		Command:      "codex",
		ProcessNames: []string{"codex"},
		Env: map[string]string{
			runtime.RuntimeEpochEnv:      fmt.Sprint(fence.Generation),
			runtime.ContinuationEpochEnv: fmt.Sprint(fence.Epoch),
			runtime.InstanceTokenEnv:     fence.Token,
			"BEADS_HOLDER_TOKEN":         fence.Token,
		},
	}); err != nil {
		t.Fatalf("starting existing runtime: %v", err)
	}
	for key, value := range map[string]string{
		"GC_SESSION_ID":     candidate.info.ID,
		"GC_INSTANCE_TOKEN": candidate.info.InstanceToken,
		"GC_RUNTIME_EPOCH":  candidate.info.Generation,
	} {
		if err := sp.Fake.SetMeta(candidate.name(), key, value); err != nil {
			t.Fatalf("setting runtime identity %s: %v", key, err)
		}
	}
	prior := mustGetBead(t, env.store, bead.ID).Metadata
	maxRestarts := 1
	env.cfg.Daemon.SessionCircuitBreakerMaxRestarts = &maxRestarts
	env.cfg.Daemon.SessionCircuitBreakerWindow = "30m"
	cb := newSessionCircuitBreaker(sessionCircuitBreakerConfig{Window: 30 * time.Minute, MaxRestarts: maxRestarts})
	setSessionCircuitBreakerForTest(t, cb)
	initialBreakerState, err := sessionFrontDoor(env.store).CircuitState(bead.ID)
	if err != nil {
		t.Fatalf("reading initial circuit state: %v", err)
	}
	initialRestartCount, err := parseCircuitTimeList(initialBreakerState.Restarts)
	if err != nil || len(initialRestartCount) != 0 {
		t.Fatalf("initial persisted restart count = %d (err %v), want 0", len(initialRestartCount), err)
	}
	assertNoBreakerRestarts := func(attempt int) {
		state, err := sessionFrontDoor(env.store).CircuitState(bead.ID)
		if err != nil {
			t.Fatalf("reading persisted circuit state after attempt %d: %v", attempt, err)
		}
		restarts, err := parseCircuitTimeList(state.Restarts)
		if err != nil || len(restarts) != 0 {
			t.Fatalf("persisted restart count after attempt %d = %d (err %v), want unchanged at 0", attempt, len(restarts), err)
		}
		for _, snapshot := range cb.Snapshot(env.clk.Now().UTC()) {
			if snapshot.Identity == namedSessionIdentityInfo(candidate.info) && snapshot.RestartCount != 0 {
				t.Fatalf("in-memory restart count after attempt %d = %d, want unchanged at 0", attempt, snapshot.RestartCount)
			}
		}
		meta := mustGetBead(t, env.store, bead.ID).Metadata
		if meta["instance_token"] != prior["instance_token"] || meta["generation"] != prior["generation"] {
			t.Fatalf("attempt %d did not restore T1: token=%q generation=%q, want (%q,%q)", attempt, meta["instance_token"], meta["generation"], prior["instance_token"], prior["generation"])
		}
	}
	providerCallsBefore := sp.Fake.SnapshotCalls()
	finished := make(chan struct{})
	var woken int
	go func() {
		woken = runIdentityPlannerWithProvider(t, env, candidate, tp, false, sp)
		close(finished)
	}()
	awaitCond(t, func() bool { return sp.probeCount(candidate.name()) > 0 }, "wedged pre-PreWake observation to start")
	awaitClose(t, finished, "planner to finish after the wedged pre-PreWake observation timed out")
	if woken != 0 {
		t.Fatalf("wake count = %d, want 0 for an unanswered same-city observation", woken)
	}
	assertNoBreakerRestarts(1)
	for attempt := 1; attempt < 3; attempt++ {
		if got := runIdentityPlannerWithProvider(t, env, candidate, tp, false, sp); got != 0 {
			t.Fatalf("attempt %d wake count = %d, want 0 for an unanswered same-city observation", attempt+1, got)
		}
		assertNoBreakerRestarts(attempt + 1)
	}
	if got := sp.probeCount(candidate.name()); got != 1 {
		t.Fatalf("provider liveness probes = %d, want only the genuinely wedged preflight probe across repeated attempts", got)
	}
	assertNoProviderMutationCalls(t, sp.Fake.SnapshotCalls()[len(providerCallsBefore):], candidate.name())
	if starts := sp.CountCalls("Start", candidate.name()); starts != 1 {
		t.Fatalf("provider Start calls = %d, want only the pre-existing runtime start across repeated attempts", starts)
	}
	deferred := mustGetBead(t, env.store, bead.ID).Metadata
	for _, key := range []string{
		"instance_token", "generation", "continuation_epoch", "continuation_reset_pending",
		"state", "sleep_reason", "detached_at", "wake_request", "wake_requested_at",
		"last_woke_at", "pending_create_started_at",
	} {
		if deferred[key] != prior[key] {
			t.Errorf("deferred %s = %q, want pre-PreWake value %q", key, deferred[key], prior[key])
		}
	}
	if deferred["instance_token"] != "row-token" {
		t.Fatalf("deferred instance_token = %q, want prior token T1 (row-token)", deferred["instance_token"])
	}
	if !sp.Fake.IsRunning(candidate.name()) {
		t.Fatal("the existing runtime stopped during the deferred start")
	}
	if !strings.Contains(env.stderr.String(), string(TraceOutcomeDeferred)) {
		t.Fatalf("unanswered preflight was not logged as deferred: %q", env.stderr.String())
	}
	if got := strings.Count(env.stderr.String(), "outcome=start_boundary_liveness_unanswered"); got != 3 {
		t.Fatalf("store-only unanswered liveness outcomes = %d, want one for each of 3 attempts; stderr=%q", got, env.stderr.String())
	}
	if starts := sp.CountCalls("Start", candidate.name()); starts != 1 {
		t.Fatalf("provider Start calls = %d, want only the existing runtime start", starts)
	}

	sp.unblock()
	awaitCond(t, func() bool {
		running, alive, err := observeRuntimeProviderLivenessBounded(context.Background(), env.city, sp, candidate.name(), startCandidateProcessNames(candidate, env.cfg))
		return err == nil && running && alive
	}, "same-city probe to recover the existing runtime")
	retry := candidate
	retry.info = env.sessionInfo(bead.ID)
	if got := runIdentityPlannerWithProvider(t, env, retry, tp, false, sp); got != 1 {
		t.Fatalf("retry wake count = %d, want 1 after the probe recovered", got)
	}
	got := env.sessionInfo(bead.ID)
	if got.InstanceToken != prior["instance_token"] || got.Generation != prior["generation"] || got.ContinuationEpoch != prior["continuation_epoch"] {
		t.Fatalf("retry identity = (%q,%q,%q), want prior fence (%q,%q,%q)", got.InstanceToken, got.Generation, got.ContinuationEpoch, prior["instance_token"], prior["generation"], prior["continuation_epoch"])
	}
	runtimeToken, err := sp.Fake.GetMeta(candidate.name(), "GC_INSTANCE_TOKEN")
	if err != nil || runtimeToken != got.InstanceToken {
		t.Fatalf("retry runtime token = %q (err %v), session token = %q", runtimeToken, err, got.InstanceToken)
	}
	if starts := sp.CountCalls("Start", candidate.name()); starts != 1 {
		t.Fatalf("provider Start calls after retry = %d, want only the existing runtime start", starts)
	}
}

func TestExecutePlannedStartsT1RestoreWithoutConditionalWritesKeepsAttemptedIdentity(t *testing.T) {
	env := newReconcilerTestEnv(t)
	mem, ok := env.store.(*beads.MemStore)
	if !ok {
		t.Fatalf("no-CAS start test store has type %T, want *beads.MemStore", env.store)
	}
	mem.DisableConditionalWrites = true
	candidate, bead, tp := identityPlannerCandidate(t, env, "active")
	env.setSessionMetadata(&bead, map[string]string{
		"continuation_epoch":         "4",
		"continuation_reset_pending": "true",
	})
	candidate.info = env.sessionInfo(bead.ID)
	startIdentityRuntime(t, env, candidate)
	prior := mustGetBead(t, env.store, bead.ID).Metadata
	if prior["continuation_epoch"] != "4" || prior["continuation_reset_pending"] != "true" {
		t.Fatalf("test precondition did not persist T1 continuation state: %v", prior)
	}
	maxRestarts := 1
	env.cfg.Daemon.SessionCircuitBreaker = true
	env.cfg.Daemon.SessionCircuitBreakerMaxRestarts = &maxRestarts
	env.cfg.Daemon.SessionCircuitBreakerWindow = "30m"
	cb := breakerAt(30*time.Minute, maxRestarts)
	setSessionCircuitBreakerForTest(t, cb)
	if err := setReconcilerDrainAckMetadata(env.sp, candidate.name(), &drainState{reason: "orphaned", generation: 1}); err != nil {
		t.Fatalf("setting drain ack: %v", err)
	}
	sp := newWedgedLivenessProvider(t, candidate.name())

	if got := runIdentityPlannerWithProvider(t, env, candidate, tp, false, sp); got != 0 {
		t.Fatalf("wake count = %d, want 0 for an unanswered pre-start observation", got)
	}
	got := mustGetBead(t, env.store, bead.ID).Metadata
	if got["instance_token"] == prior["instance_token"] || got["generation"] == prior["generation"] || got["continuation_epoch"] == prior["continuation_epoch"] {
		t.Fatalf("no-CAS T1 cleanup restored old identity: prior=(%q,%q,%q) current=(%q,%q,%q)",
			prior["instance_token"], prior["generation"], prior["continuation_epoch"],
			got["instance_token"], got["generation"], got["continuation_epoch"])
	}
	if got["state"] != prior["state"] || got["continuation_reset_pending"] != prior["continuation_reset_pending"] {
		t.Fatalf("no-CAS T1 cleanup did not restore lifecycle state: state=%q reset_pending=%q, want %q and %q",
			got["state"], got["continuation_reset_pending"], prior["state"], prior["continuation_reset_pending"])
	}
	if got["last_woke_at"] != "" {
		t.Fatalf("no-CAS T1 cleanup left the in-flight marker: last_woke_at=%q", got["last_woke_at"])
	}
	if got["pending_create_started_at"] != prior["pending_create_started_at"] {
		t.Fatalf("pending_create_started_at=%q, want pre-wake value %q", got["pending_create_started_at"], prior["pending_create_started_at"])
	}
	requireSessionCircuitRestartCount(t, cb, env.store, bead.ID, namedSessionIdentityInfo(candidate.info), env.clk.Now().UTC(), 0)
}

func TestExecutePlannedStartsCancellationBeforeLaunchCleansPreparedStarts(t *testing.T) {
	for _, async := range []bool{false, true} {
		name := "sync"
		if async {
			name = "async"
		}
		t.Run(name, func(t *testing.T) {
			env := newReconcilerTestEnv(t)
			candidate, bead, tp := identityPlannerCandidate(t, env, "active")
			sibling, siblingTP := identityPlannerSiblingCandidate(t, env, "session-b")
			candidates := []startCandidate{candidate, sibling}
			byName := map[string]struct {
				candidate startCandidate
				bead      beads.Bead
				prior     map[string]string
			}{
				candidate.name(): {candidate: candidate, bead: bead, prior: mustGetBead(t, env.store, bead.ID).Metadata},
				sibling.name():   {candidate: sibling, bead: mustGetBead(t, env.store, sibling.info.ID), prior: mustGetBead(t, env.store, sibling.info.ID).Metadata},
			}
			maxRestarts := 1
			env.cfg.Daemon.SessionCircuitBreaker = true
			env.cfg.Daemon.SessionCircuitBreakerMaxRestarts = &maxRestarts
			env.cfg.Daemon.SessionCircuitBreakerWindow = "30m"
			cb := breakerAt(30*time.Minute, maxRestarts)
			setSessionCircuitBreakerForTest(t, cb)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var callsAtCancellation int
			resolverName := ""
			resolver := func(resolving startCandidate, _ *config.City) string {
				if resolverName != "" {
					t.Fatalf("task work-dir resolver called more than once; first candidate %q, next %q", resolverName, resolving.name())
				}
				resolverName = resolving.name()
				callsAtCancellation = len(env.sp.SnapshotCalls())
				cancel()
				return ""
			}
			options := []startExecutionOption{
				withWarmClaimProbe(alwaysUnclaimed),
				withStartStabilityWaiter(immediateStartStabilityWaiter),
				withSessionStaleKeyDetectionWaiter(immediateSessionStaleKeyDetectionWaiter),
				withTaskWorkDirResolver(resolver),
			}
			var tracker *asyncStartTracker
			var limiter *asyncStartLimiter
			if async {
				tracker = &asyncStartTracker{}
				limiter = newAsyncStartLimiter(1)
				options = append(options, withAsyncStartExecution(), withAsyncStartTracker(tracker), withAsyncStartLimiter(limiter))
			}
			if got := executePlannedStartsTraced(
				ctx, candidates, env.cfg, map[string]TemplateParams{tp.SessionName: tp, siblingTP.SessionName: siblingTP}, env.sp, env.store,
				"test-city", env.city, env.clk, events.Discard, 5*time.Second, io.Discard, &env.stderr, nil, options...,
			); got != 0 {
				t.Fatalf("wake count = %d, want 0 for cancellation before launch", got)
			}
			if resolverName == "" {
				t.Fatal("task work-dir resolver was not reached after PreWake")
			}
			prepared := byName[resolverName]
			if prepared.candidate.name() == "" {
				t.Fatalf("resolver ran for unexpected candidate %q", resolverName)
			}
			var untouched struct {
				candidate startCandidate
				bead      beads.Bead
				prior     map[string]string
			}
			for name, entry := range byName {
				if name != resolverName {
					untouched = entry
				}
			}
			if callsAfter := len(env.sp.SnapshotCalls()); callsAfter != callsAtCancellation {
				t.Fatalf("provider calls after cancellation = %d, want none after %d calls (planner started a provider operation)", callsAfter-callsAtCancellation, callsAtCancellation)
			}
			for _, entry := range byName {
				if env.sp.CountCalls("Start", entry.candidate.name()) != 0 {
					t.Fatalf("Provider.Start calls for %s = %d, want none", entry.candidate.name(), env.sp.CountCalls("Start", entry.candidate.name()))
				}
			}
			got := mustGetBead(t, env.store, prepared.bead.ID).Metadata
			for _, key := range []string{"state", "sleep_reason", "detached_at", "continuation_epoch", "continuation_reset_pending", "wake_request", "wake_requested_at", "pending_create_started_at"} {
				if got[key] != prepared.prior[key] {
					t.Errorf("%s=%q after canceled preparation of %s, want pre-wake value %q", key, got[key], resolverName, prepared.prior[key])
				}
			}
			if got["instance_token"] == prepared.prior["instance_token"] || got["generation"] == prepared.prior["generation"] {
				t.Errorf("never-launched cleanup restored the prior identity: token=%q generation=%q", got["instance_token"], got["generation"])
			}
			if got["last_woke_at"] != "" {
				t.Errorf("never-launched cleanup left the in-flight marker: %q", got["last_woke_at"])
			}
			requireSessionCircuitRestartCount(t, cb, env.store, prepared.bead.ID, namedSessionIdentityInfo(prepared.candidate.info), env.clk.Now().UTC(), 0)
			untouchedAfter := mustGetBead(t, env.store, untouched.bead.ID).Metadata
			for _, key := range []string{"state", "instance_token", "generation", "continuation_epoch", "last_woke_at"} {
				if untouchedAfter[key] != untouched.prior[key] {
					t.Errorf("unprepared %s %s=%q, want unchanged value %q", untouched.candidate.name(), key, untouchedAfter[key], untouched.prior[key])
				}
			}
			requireSessionCircuitRestartCount(t, cb, env.store, untouched.bead.ID, namedSessionIdentityInfo(untouched.candidate.info), env.clk.Now().UTC(), 0)
			if tracker != nil && !tracker.wait(time.Second) {
				t.Fatal("canceled async preparation left an outstanding tracker entry")
			}
			if limiter != nil {
				release, reserved, outcome := reserveAsyncStartSlot(context.Background(), limiter)
				if !reserved {
					t.Fatalf("async start slot remained reserved after cleanup: %s", outcome)
				}
				release()
			}
		})
	}
}

func TestCircuitBreakerUndoSurvivesMovedLeaseWithoutRevertingInMemory(t *testing.T) {
	env := newReconcilerTestEnv(t)
	candidate, bead, _ := identityPlannerCandidate(t, env, "active")
	identity := namedSessionIdentityInfo(candidate.info)
	cb := newSessionCircuitBreaker(sessionCircuitBreakerConfig{})
	lease, release, err := tryRuntimeLease(env.store, env.city, candidate.name(), bead.ID, sessionpkg.RuntimeLeaseTTL(0))
	if err != nil {
		t.Fatalf("taking runtime lease: %v", err)
	}
	defer release()

	now := env.clk.Now().UTC()
	_, snapshot, recorded, err := recordSessionCircuitBreakerRestart(sessionFrontDoor(env.store), bead.ID, cb, identity, now)
	if err != nil || !recorded {
		t.Fatalf("recording restart = recorded %v, err %v; want one persisted attempt", recorded, err)
	}
	if err := sessionFrontDoor(env.store).ApplyPatch(bead.ID, sessionpkg.MetadataPatch{
		sessionpkg.RuntimeLeaseHolderKey: "successor/1/nonce",
		sessionpkg.RuntimeLeaseEpochKey:  "999",
	}); err != nil {
		t.Fatalf("moving runtime lease record: %v", err)
	}

	cb.restoreEntrySnapshot(sessionFrontDoor(env.store), lease, bead.ID, identity, snapshot, now, &env.stderr)

	for _, got := range cb.Snapshot(now) {
		if got.Identity == identity && got.RestartCount != 0 {
			t.Fatalf("in-memory restart count after lease loss = %d, want 0", got.RestartCount)
		}
	}
	persisted, err := sessionFrontDoor(env.store).CircuitState(bead.ID)
	if err != nil {
		t.Fatalf("reading persisted breaker state: %v", err)
	}
	restarts, err := parseCircuitTimeList(persisted.Restarts)
	if err != nil || len(restarts) != 1 {
		t.Fatalf("persisted restart count after lease loss = %d (err %v), want the row unchanged at 1", len(restarts), err)
	}
}

func TestExecutePlannedStartsDefersAfterAnsweredPreWakeDeadlineError(t *testing.T) {
	env := newReconcilerTestEnv(t)
	candidate, bead, tp := identityPlannerCandidate(t, env, "active")
	sp := &sequencedStartLivenessProvider{
		Fake:         env.sp,
		observations: []runtime.Liveness{{}},
		errors:       []error{context.DeadlineExceeded},
	}
	oldToken, oldGeneration := candidate.info.InstanceToken, candidate.info.Generation
	if got := runIdentityPlannerWithProvider(t, env, candidate, tp, false, sp); got != 0 {
		t.Fatalf("wake count = %d, want 0 after an answered pre-wake deadline error", got)
	}
	got := env.sessionInfo(bead.ID)
	if got.InstanceToken != oldToken || got.Generation != oldGeneration {
		t.Fatalf("identity after answered pre-wake error = (%q,%q), want prior (%q,%q)", got.InstanceToken, got.Generation, oldToken, oldGeneration)
	}
	if starts := env.sp.CountCalls("Start", candidate.name()); starts != 0 {
		t.Fatalf("provider Start calls = %d, want 0 after answered pre-wake error", starts)
	}
}

func requireSessionCircuitRestartCount(t *testing.T, cb *sessionCircuitBreaker, store beads.Store, id, identity string, now time.Time, want int) {
	t.Helper()
	var inMemoryCount int
	for _, snapshot := range cb.Snapshot(now) {
		if snapshot.Identity == identity {
			inMemoryCount = snapshot.RestartCount
			break
		}
	}
	if inMemoryCount != want {
		t.Fatalf("in-memory restart count = %d, want %d", inMemoryCount, want)
	}
	persisted, err := sessionFrontDoor(store).CircuitState(id)
	if err != nil {
		t.Fatalf("reading persisted breaker state: %v", err)
	}
	restarts, err := parseCircuitTimeList(persisted.Restarts)
	if err != nil || len(restarts) != want {
		t.Fatalf("persisted restart count = %d (err %v), want %d", len(restarts), err, want)
	}
}

func TestExecutePlannedStartsRestoresPriorIdentityAfterSessionExistsCollision(t *testing.T) {
	env := newReconcilerTestEnv(t)
	candidate, bead, tp := identityPlannerCandidate(t, env, "active")
	env.cfg.Agents[0].PromptMode = "arg"
	maxRestarts := 5
	env.cfg.Daemon.SessionCircuitBreaker = true
	env.cfg.Daemon.SessionCircuitBreakerMaxRestarts = &maxRestarts
	env.cfg.Daemon.SessionCircuitBreakerWindow = "30m"
	cb := breakerAt(30*time.Minute, maxRestarts)
	setSessionCircuitBreakerForTest(t, cb)
	startIdentityRuntime(t, env, candidate)
	startsBefore := env.sp.CountCalls("Start", candidate.name())
	oldToken, oldGeneration, oldEpoch, oldLastWoke := candidate.info.InstanceToken, candidate.info.Generation, candidate.info.ContinuationEpoch, candidate.info.LastWokeAt
	sp := &providerStartCollisionLivenessProvider{sequencedStartLivenessProvider: &sequencedStartLivenessProvider{
		Fake: env.sp,
		observations: []runtime.Liveness{
			{},                           // preparation's stale snapshot
			{},                           // pre-Start check
			{Running: true, Alive: true}, // collision observation
		},
	}}
	if got := runIdentityPlannerWithProvider(t, env, candidate, tp, false, sp); got != 1 {
		t.Fatalf("wake count = %d, want 1 after attributed session collision", got)
	}
	if starts := env.sp.CountCalls("Start", candidate.name()); starts != startsBefore+1 {
		t.Fatalf("Provider.Start calls during collision handling = %d, want exactly one attempt after %d setup call(s)", starts-startsBefore, startsBefore)
	}
	requireSessionCircuitRestartCount(t, cb, env.store, bead.ID, namedSessionIdentityInfo(candidate.info), env.clk.Now().UTC(), 1)
	got := env.sessionInfo(bead.ID)
	if got.InstanceToken != oldToken || got.Generation != oldGeneration || got.ContinuationEpoch != oldEpoch || got.LastWokeAt != oldLastWoke {
		t.Fatalf("identity after session collision = (%q,%q,%q) last_woke_at=%q, want prior (%q,%q,%q) last_woke_at=%q", got.InstanceToken, got.Generation, got.ContinuationEpoch, got.LastWokeAt, oldToken, oldGeneration, oldEpoch, oldLastWoke)
	}
	runtimeToken, err := env.sp.GetMeta(candidate.name(), "GC_INSTANCE_TOKEN")
	if err != nil || runtimeToken != got.InstanceToken {
		t.Fatalf("runtime token = %q (err %v), session token = %q", runtimeToken, err, got.InstanceToken)
	}
	if got.StartedConfigHash == "" || got.State != sessionpkg.StateActive {
		t.Fatalf("collision reuse did not commit lifecycle/config state: state=%q hash=%q", got.State, got.StartedConfigHash)
	}
}

func TestExecutePlannedStartsDefersWhenSessionLayerFindsForeignRuntime(t *testing.T) {
	env := newReconcilerTestEnv(t)
	candidate, bead, tp := identityPlannerCandidate(t, env, "active")
	env.cfg.Agents[0].PromptMode = "arg"
	if err := env.sp.Start(context.Background(), candidate.name(), runtime.Config{
		Command:      "codex",
		ProcessNames: []string{"codex"},
	}); err != nil {
		t.Fatalf("starting foreign runtime: %v", err)
	}
	for key, value := range map[string]string{
		"GC_SESSION_ID":     candidate.info.ID,
		"GC_INSTANCE_TOKEN": "foreign-token",
		"GC_RUNTIME_EPOCH":  "99",
	} {
		if err := env.sp.SetMeta(candidate.name(), key, value); err != nil {
			t.Fatalf("setting foreign runtime identity %s: %v", key, err)
		}
	}
	sp := &sequencedStartLivenessProvider{
		Fake: env.sp,
		observations: []runtime.Liveness{
			{},                           // preparation's stale snapshot
			{},                           // start-boundary stale snapshot
			{Running: true, Alive: true}, // provider sees the existing process after StartResolved
			{Running: true, Alive: true}, // collision attribution
		},
	}

	if got := runIdentityPlannerWithProvider(t, env, candidate, tp, false, sp); got != 0 {
		t.Fatalf("wake count = %d, want a deferral without committing the foreign runtime", got)
	}
	got := env.sessionInfo(bead.ID)
	if got.StartedConfigHash != candidate.info.StartedConfigHash {
		t.Fatalf("started_config_hash = %q, want the prior value %q after foreign-runtime deferral", got.StartedConfigHash, candidate.info.StartedConfigHash)
	}
	if got.State != sessionpkg.StateActive || got.LastWokeAt != "" {
		t.Fatalf("session lifecycle after foreign-runtime deferral = (%q, %q), want active with released in-flight marker", got.State, got.LastWokeAt)
	}
	runtimeToken, err := env.sp.GetMeta(candidate.name(), "GC_INSTANCE_TOKEN")
	if err != nil || runtimeToken != "foreign-token" {
		t.Fatalf("foreign runtime token = %q (err %v), want it left untouched", runtimeToken, err)
	}
}

func TestExecutePlannedStartsWarmReuseKeepsPriorFenceUsable(t *testing.T) {
	env := newReconcilerTestEnv(t)
	candidate, _, tp := identityPlannerCandidate(t, env, "active")
	env.cfg.Agents[0].PromptMode = "arg"
	startIdentityRuntime(t, env, candidate)
	priorFence, ok := sessionpkg.InputFenceFor(candidate.info)
	if !ok {
		t.Fatal("candidate has no input fence")
	}

	if got := runIdentityPlanner(t, env, candidate, tp, false); got != 1 {
		t.Fatalf("wake count = %d, want 1 for warm reuse", got)
	}
	if n, _ := countNudges(env.sp); n != 1 {
		t.Fatalf("warm claim nudge count = %d, want exactly 1", n)
	}
	got := env.sessionInfo(candidate.info.ID)
	gotFence, ok := sessionpkg.InputFenceFor(got)
	if !ok || gotFence != priorFence {
		t.Fatalf("session fence after warm reuse = %+v (valid %v), want prior fence %+v", gotFence, ok, priorFence)
	}
	if err := env.sp.NudgeFenced(context.Background(), candidate.name(), runtime.TextContent("fenced after warm reuse"), runtime.FencedNudge{Fence: priorFence}); err != nil {
		t.Fatalf("fenced nudge after warm reuse = %v, want the prior runtime fence to remain valid", err)
	}
}

func TestExecutePlannedStartsDoesNotNudgeHeldOrClosedLiveRows(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*testing.T, *reconcilerTestEnv, beads.Bead)
	}{
		{name: "held", mutate: func(_ *testing.T, env *reconcilerTestEnv, bead beads.Bead) {
			env.setSessionMetadata(&bead, map[string]string{"sleep_intent": "user-hold"})
		}},
		{name: "closed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newReconcilerTestEnv(t)
			candidate, bead, tp := identityPlannerCandidate(t, env, "active")
			startIdentityRuntime(t, env, candidate)
			sp := runtime.Provider(env.sp)
			if tc.mutate != nil {
				tc.mutate(t, env, bead)
			} else {
				// Close after executePlannedStartsTraced takes the lease record,
				// during its fresh liveness observation, so preparation exercises
				// the locked closed-row check.
				sp = &closeRowOnLivenessProvider{Fake: env.sp, store: env.store, id: bead.ID}
			}
			if got := runIdentityPlannerWithProvider(t, env, candidate, tp, false, sp); got != 0 {
				t.Fatalf("wake count = %d, want 0", got)
			}
			if n, _ := countNudges(env.sp); n != 0 {
				t.Fatalf("claim nudge count = %d, want 0", n)
			}
			if env.sp.CountCalls("Start", candidate.name()) != 1 {
				t.Fatalf("Start calls = %d, want only the pre-existing runtime start", env.sp.CountCalls("Start", candidate.name()))
			}
		})
	}
}

func TestExecutePlannedStartsDefersLivenessProbeErrorAndRestoresBreaker(t *testing.T) {
	env := newReconcilerTestEnv(t)
	candidate, bead, tp := identityPlannerCandidate(t, env, "active")
	startIdentityRuntime(t, env, candidate)
	fake := env.sp
	cb := newSessionCircuitBreaker(sessionCircuitBreakerConfig{})
	setSessionCircuitBreakerForTest(t, cb)
	identity := namedSessionIdentityInfo(candidate.info)
	cb.RecordRestart(identity, env.clk.Now().Add(-time.Minute))
	beforeBreaker := cb.entrySnapshot(identity)
	// The provider can report a live-looking result and still fail its
	// authoritative liveness observation.
	if got := executePlannedStartsTraced(
		context.Background(), []startCandidate{candidate}, env.cfg,
		map[string]TemplateParams{tp.SessionName: tp}, &startUnavailableLivenessProvider{Fake: fake}, env.store,
		"test-city", env.city, env.clk, events.Discard, 5*time.Second,
		io.Discard, &env.stderr, nil,
		withWarmClaimProbe(alwaysUnclaimed),
		withStartStabilityWaiter(immediateStartStabilityWaiter),
		withSessionStaleKeyDetectionWaiter(immediateSessionStaleKeyDetectionWaiter),
	); got != 0 {
		t.Fatalf("wake count = %d, want 0", got)
	}
	if fake.CountCalls("Start", candidate.name()) != 1 {
		t.Fatalf("Start calls = %d, want only the pre-existing runtime start", fake.CountCalls("Start", candidate.name()))
	}
	if n, _ := countNudges(fake); n != 0 {
		t.Fatalf("claim nudge count = %d, want 0 on an uncertain observation", n)
	}
	if after := cb.entrySnapshot(identity); !reflect.DeepEqual(after, beforeBreaker) {
		t.Fatalf("breaker snapshot after deferred observation = %#v, want %#v", after, beforeBreaker)
	}
	if !strings.Contains(env.stderr.String(), string(TraceOutcomeDeferred)) || strings.Contains(env.stderr.String(), "outcome=failed") {
		t.Fatalf("liveness error should be logged as deferred, stderr=%q", env.stderr.String())
	}
	if got := env.sessionInfo(bead.ID); got.InstanceToken != candidate.info.InstanceToken || got.Generation != candidate.info.Generation {
		t.Fatalf("identity after deferred observation = (%q, %q), want unchanged", got.InstanceToken, got.Generation)
	}
	if got := env.sessionInfo(bead.ID); got.LastWokeAt != candidate.info.LastWokeAt {
		t.Fatalf("last_woke_at after pre-wake deferral = %q, want preserved %q", got.LastWokeAt, candidate.info.LastWokeAt)
	}
}

func TestExecutePlannedStartsPreservesPendingCreateMarkerWhenPreflightDefers(t *testing.T) {
	env := newReconcilerTestEnv(t)
	candidate, bead, tp := identityPlannerCandidate(t, env, "creating")
	startedAt := "2026-10-23T01:00:00Z"
	lastWokeAt := "2026-10-23T01:01:00Z"
	env.setSessionMetadata(&bead, map[string]string{
		"pending_create_started_at": startedAt,
		"last_woke_at":              lastWokeAt,
	})
	candidate.info = env.sessionInfo(bead.ID)
	if got := runIdentityPlannerWithProvider(t, env, candidate, tp, false, &startUnavailableLivenessProvider{Fake: env.sp}); got != 0 {
		t.Fatalf("wake count = %d, want 0 on uncertain preflight", got)
	}
	got := env.sessionInfo(bead.ID)
	if got.MetadataState != "creating" || got.PendingCreateStartedAt != startedAt || got.LastWokeAt != lastWokeAt {
		t.Fatalf("claimless create after preflight deferral = state %q pending_create_started_at %q last_woke_at %q, want all prior markers preserved", got.MetadataState, got.PendingCreateStartedAt, got.LastWokeAt)
	}
	if env.sp.CountCalls("Start", candidate.name()) != 0 {
		t.Fatal("provider Start ran after an uncertain preflight")
	}
}

func TestWarmReuseCommitsDormantLifecycleAndWakeRequest(t *testing.T) {
	for _, state := range []string{"asleep", "drained", "creating"} {
		t.Run(state, func(t *testing.T) {
			env := newReconcilerTestEnv(t)
			candidate, bead, tp := identityPlannerCandidate(t, env, state)
			env.cfg.Agents[0].PromptMode = "arg"
			env.setSessionMetadata(&bead, map[string]string{
				"sleep_reason":              "idle_timeout",
				"wake_request":              string(sessionpkg.WakeCauseExplicit),
				"wake_requested_at":         "2026-10-23T01:00:00Z",
				"pending_create_started_at": "2026-10-23T00:59:00Z",
			})
			candidate.info = env.sessionInfo(bead.ID)
			startIdentityRuntime(t, env, candidate)
			setSessionCircuitBreakerForTest(t, newSessionCircuitBreaker(sessionCircuitBreakerConfig{}))
			if got := runIdentityPlanner(t, env, candidate, tp, false); got != 1 {
				t.Fatalf("wake count = %d, want 1 for warm reuse", got)
			}
			got := env.sessionInfo(bead.ID)
			if got.State != sessionpkg.StateActive || got.SleepReason != "" || got.WakeRequest != "" || got.WakeRequestedAt != "" {
				t.Fatalf("warm reuse left lifecycle state=%q sleep_reason=%q wake_request=%q wake_requested_at=%q", got.State, got.SleepReason, got.WakeRequest, got.WakeRequestedAt)
			}
			if got.StartedConfigHash == "" || got.InstanceToken != candidate.info.InstanceToken || got.Generation != candidate.info.Generation {
				t.Fatalf("warm reuse commit hash=%q identity=(%q,%q), expected non-empty hash and unchanged identity", got.StartedConfigHash, got.InstanceToken, got.Generation)
			}
		})
	}
}

func TestExecutePlannedStartsPendingCreateWarmReusePreservesIdentity(t *testing.T) {
	env := newReconcilerTestEnv(t)
	candidate, bead, tp := identityPlannerCandidate(t, env, "start-pending")
	env.setSessionMetadata(&bead, map[string]string{"pending_create_claim": "true"})
	candidate.info = env.sessionInfo(bead.ID)
	startIdentityRuntime(t, env, candidate)
	// The existing worker carries the row ID and old token but has no epoch,
	// which the pending-create attribution contract still classifies as ours.
	if err := env.sp.RemoveMeta(candidate.name(), "GC_RUNTIME_EPOCH"); err != nil {
		t.Fatalf("removing runtime epoch: %v", err)
	}
	if got := runIdentityPlanner(t, env, candidate, tp, false); got != 1 {
		t.Fatalf("wake count = %d, want 1 after pending create converges", got)
	}
	got := env.sessionInfo(bead.ID)
	if got.InstanceToken != candidate.info.InstanceToken || got.Generation != candidate.info.Generation {
		t.Fatalf("pending-create warm reuse rotated identity to (%q, %q), want unchanged (%q, %q)", got.InstanceToken, got.Generation, candidate.info.InstanceToken, candidate.info.Generation)
	}
	if got.PendingCreateClaim || got.State != sessionpkg.StateActive {
		t.Fatalf("pending-create convergence left claim=%v state=%q, want cleared and active", got.PendingCreateClaim, got.State)
	}
	if env.sp.CountCalls("Start", candidate.name()) != 1 {
		t.Fatalf("Start calls = %d, want only the pre-existing runtime start", env.sp.CountCalls("Start", candidate.name()))
	}
}

func TestExecutePlannedStartsPendingCreateWithoutRuntimeStillPreWakes(t *testing.T) {
	env := newReconcilerTestEnv(t)
	candidate, bead, tp := identityPlannerCandidate(t, env, "start-pending")
	env.setSessionMetadata(&bead, map[string]string{"pending_create_claim": "true"})
	candidate.info = env.sessionInfo(bead.ID)
	oldToken, oldGeneration := candidate.info.InstanceToken, candidate.info.Generation
	if got := runIdentityPlanner(t, env, candidate, tp, false); got != 1 {
		t.Fatalf("wake count = %d, want 1", got)
	}
	got := env.sessionInfo(bead.ID)
	if got.InstanceToken == oldToken || got.Generation == oldGeneration {
		t.Fatalf("pending create did not mint identity: got (%q, %q), old (%q, %q)", got.InstanceToken, got.Generation, oldToken, oldGeneration)
	}
	if got.PendingCreateClaim || got.State != sessionpkg.StateActive {
		t.Fatalf("pending create ended claim=%v state=%q, want cleared and active", got.PendingCreateClaim, got.State)
	}
	if env.sp.CountCalls("Start", candidate.name()) != 1 {
		t.Fatalf("Start calls = %d, want 1", env.sp.CountCalls("Start", candidate.name()))
	}
}

func TestExecutePlannedStartsStillRecyclesZombieRuntime(t *testing.T) {
	env := newReconcilerTestEnv(t)
	candidate, _, tp := identityPlannerCandidate(t, env, "active")
	// The configured-session refresh replaces candidate.tp with the resolved
	// template. Give that template process hints so the prepared start sees the
	// dead agent process and recycles the still-running runtime as a zombie.
	env.cfg.Agents[0].ProcessNames = []string{"codex"}
	if err := env.sp.Start(context.Background(), candidate.name(), runtime.Config{Command: "codex", ProcessNames: []string{"codex"}}); err != nil {
		t.Fatalf("starting zombie runtime: %v", err)
	}
	env.sp.Zombies[candidate.name()] = true
	if got := runIdentityPlanner(t, env, candidate, tp, false); got != 1 {
		t.Fatalf("wake count = %d, want 1 after recycling zombie", got)
	}
	if env.sp.CountCalls("Stop", candidate.name()) != 1 || env.sp.CountCalls("Start", candidate.name()) != 2 {
		t.Fatalf("zombie recycle calls: Stop=%d Start=%d, want one stop and one new start after existing runtime", env.sp.CountCalls("Stop", candidate.name()), env.sp.CountCalls("Start", candidate.name()))
	}
}

func TestExecutePlannedStartsRestoresPriorIdentityForExistingK8sInitializingSession(t *testing.T) {
	env := newReconcilerTestEnv(t)
	candidate, bead, tp := identityPlannerCandidate(t, env, "active")
	maxRestarts := 5
	env.cfg.Daemon.SessionCircuitBreaker = true
	env.cfg.Daemon.SessionCircuitBreakerMaxRestarts = &maxRestarts
	env.cfg.Daemon.SessionCircuitBreakerWindow = "30m"
	cb := breakerAt(30*time.Minute, maxRestarts)
	setSessionCircuitBreakerForTest(t, cb)
	old := candidate.info
	sp := &initializingPriorIdentityProvider{
		Fake: env.sp,
		identity: map[string]string{
			"GC_SESSION_ID":     old.ID,
			"GC_INSTANCE_TOKEN": old.InstanceToken,
			"GC_RUNTIME_EPOCH":  old.Generation,
		},
		startErr: fmt.Errorf("%w: %w: young pod", runtime.ErrSessionInitializing, runtime.ErrExistingSessionInitializing),
	}
	if got := runIdentityPlannerWithProvider(t, env, candidate, tp, false, sp); got != 0 {
		t.Fatalf("wake count = %d, want 0 for initializing existing pod", got)
	}
	requireSessionCircuitRestartCount(t, cb, env.store, bead.ID, namedSessionIdentityInfo(candidate.info), env.clk.Now().UTC(), 1)
	got := env.sessionInfo(bead.ID)
	if got.InstanceToken != old.InstanceToken || got.Generation != old.Generation || got.ContinuationEpoch != old.ContinuationEpoch {
		t.Fatalf("identity after existing-pod initialization = (%q, %q, %q), want prior (%q, %q, %q)", got.InstanceToken, got.Generation, got.ContinuationEpoch, old.InstanceToken, old.Generation, old.ContinuationEpoch)
	}
	if got.State != sessionpkg.StateActive || got.LastWokeAt != old.LastWokeAt {
		t.Fatalf("lifecycle after deferred existing pod = state %q last_woke_at %q, want prior active state and marker %q", got.State, got.LastWokeAt, old.LastWokeAt)
	}
	candidate.info = got
	sp.startErr = nil
	sp.live = true
	if got := runIdentityPlannerWithProvider(t, env, candidate, tp, false, sp); got != 1 {
		t.Fatalf("retry wake count = %d, want 1 after the initializing runtime becomes live", got)
	}
	requireSessionCircuitRestartCount(t, cb, env.store, bead.ID, namedSessionIdentityInfo(candidate.info), env.clk.Now().UTC(), 1)
	retried := env.sessionInfo(bead.ID)
	if retried.InstanceToken != old.InstanceToken || retried.Generation != old.Generation || retried.ContinuationEpoch != old.ContinuationEpoch {
		t.Fatalf("identity after initializing runtime converges = (%q, %q, %q), want retained identity (%q, %q, %q)", retried.InstanceToken, retried.Generation, retried.ContinuationEpoch, old.InstanceToken, old.Generation, old.ContinuationEpoch)
	}
	if sp.starts != 1 {
		t.Fatalf("Start calls = %d, want only the initial initializing response", sp.starts)
	}
}

func TestStartPreparedStartCandidateRefusesRigSuspendedBeforeSpawn(t *testing.T) {
	cityPath := t.TempDir()
	suspended := true
	if err := suspensionstate.SetRigSuspended(fsys.OSFS{}, cityPath, "aoa", &suspended); err != nil {
		t.Fatalf("SetRigSuspended: %v", err)
	}
	sp := runtime.NewFake()
	rig := config.Rig{Name: "aoa", Path: "/rig/aoa"}
	_, err := startPreparedStartCandidate(context.Background(), context.Background(), preparedStart{
		candidate: startCandidate{
			info: sessionpkg.Info{SessionName: "aoa--worker-1", SessionNameMetadata: "aoa--worker-1"},
			tp:   TemplateParams{TemplateName: "worker", RigName: "aoa"},
		},
		cfg: runtime.Config{Command: "claude", WorkDir: t.TempDir()},
	}, cityPath, nil, sp, &config.City{Rigs: []config.Rig{rig}}, nil, nil, nil)
	if err == nil || !strings.Contains(err.Error(), `rig "aoa" is suspended`) {
		t.Fatalf("error = %v, want suspended rig refusal", err)
	}
	if sp.CountCalls("Start", "aoa--worker-1") != 0 {
		t.Fatal("provider Start ran after suspension landed before spawn")
	}
}

func TestStartPreparedStartCandidateDoesNotKillSessionSuspendedDuringSpawn(t *testing.T) {
	cityPath := t.TempDir()
	fake := runtime.NewFake()
	sp := &blockingStartProvider{Provider: fake, started: make(chan struct{}), release: make(chan struct{})}
	rig := config.Rig{Name: "aoa", Path: "/rig/aoa"}
	done := make(chan error, 1)
	go func() {
		_, err := startPreparedStartCandidate(context.Background(), context.Background(), preparedStart{
			candidate: startCandidate{
				info: sessionpkg.Info{SessionName: "aoa--worker-1", SessionNameMetadata: "aoa--worker-1"},
				tp:   TemplateParams{TemplateName: "worker", RigName: "aoa"},
			},
			cfg: runtime.Config{Command: "claude", WorkDir: t.TempDir()},
		}, cityPath, nil, sp, &config.City{Rigs: []config.Rig{rig}}, nil, nil, nil)
		done <- err
	}()
	<-sp.started
	suspended := true
	if err := suspensionstate.SetRigSuspended(fsys.OSFS{}, cityPath, "aoa", &suspended); err != nil {
		t.Fatalf("SetRigSuspended: %v", err)
	}
	close(sp.release)
	if err := <-done; err != nil {
		t.Fatalf("in-flight start returned %v, want completion", err)
	}
	if !fake.IsRunning("aoa--worker-1") {
		t.Fatal("session crossing the suspension boundary was killed")
	}
	if fake.CountCalls("Stop", "aoa--worker-1") != 0 {
		t.Fatal("suspension killed an in-flight session")
	}
}

func TestStartPreparedStartCandidateDefersWhenLivenessUnavailable(t *testing.T) {
	testCity := t.TempDir()
	sp := &startUnavailableLivenessProvider{Fake: runtime.NewFake()}

	started, err := startPreparedStartCandidate(
		context.Background(),
		context.Background(),
		preparedStart{
			candidate: startCandidate{
				info: sessionpkg.Info{SessionName: "legacy-runtime-only", SessionNameMetadata: "legacy-runtime-only"},
				tp:   TemplateParams{TemplateName: "worker"},
			},
			cfg: runtime.Config{Command: "claude", WorkDir: t.TempDir()},
		},
		testCity,
		nil,
		sp,
		nil,
		nil,
		nil,
		nil,
	)
	if !errors.Is(err, runtime.ErrRuntimeUnavailable) {
		t.Fatalf("startPreparedStartCandidate error = %v, want runtime unavailable", err)
	}
	if started == sessionpkg.StartRuntimeWitnessProviderStart {
		t.Fatal("started = true, want false while runtime absence is unconfirmed")
	}
	if got := sp.CountCalls("Start", "legacy-runtime-only"); got != 0 {
		t.Fatalf("Start calls = %d, want 0", got)
	}
	if got := sp.CountCalls("Stop", "legacy-runtime-only"); got != 0 {
		t.Fatalf("Stop calls = %d, want 0", got)
	}
}

func TestStartPreparedStartCandidateConvergesFromConfirmedAbsence(t *testing.T) {
	city := t.TempDir()
	sp := &startConfirmedAbsentProvider{Fake: runtime.NewFake()}
	started, err := startPreparedStartCandidate(
		context.Background(),
		context.Background(),
		preparedStart{
			candidate: startCandidate{
				info: sessionpkg.Info{SessionName: "worker", SessionNameMetadata: "worker"},
				tp:   TemplateParams{TemplateName: "worker"},
			},
			cfg: runtime.Config{Command: "claude", WorkDir: t.TempDir()},
		},
		city,
		nil, sp, nil, nil, nil, nil,
	)
	if err != nil || started != sessionpkg.StartRuntimeWitnessProviderStart {
		t.Fatalf("startPreparedStartCandidate = (%v, %v), want started without error", started, err)
	}
	if got := sp.CountCalls("Start", "worker"); got != 1 {
		t.Fatalf("Start calls = %d, want 1 after confirmed absence", got)
	}
}

func TestExecutePreparedStartWaveDefersUnavailableWithoutRollbackOrWakeFailure(t *testing.T) {
	for _, tc := range []struct {
		name          string
		pendingCreate bool
	}{
		{name: "ordinary wake"},
		{name: "pending create", pendingCreate: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := beads.NewMemStore()
			clk := &clock.Fake{Time: time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)}
			metadata := map[string]string{
				"session_name":  "worker",
				"template":      "worker",
				"state":         "asleep",
				"last_woke_at":  clk.Now().Format(time.RFC3339),
				"wake_attempts": "2",
			}
			if tc.pendingCreate {
				metadata["state"] = "creating"
				metadata["pending_create_claim"] = "true"
			}
			bead, err := store.Create(beads.Bead{
				ID: "gc-worker", Title: "worker", Type: sessionBeadType,
				Labels: []string{sessionBeadLabel}, Metadata: metadata,
			})
			if err != nil {
				t.Fatalf("Create session: %v", err)
			}
			sp := &startUnavailableLivenessProvider{Fake: runtime.NewFake()}
			results := executePreparedStartWaveForCity(
				context.Background(),
				[]preparedStart{{
					candidate: startCandidate{
						info: sessiontest.SeedBead(t, bead),
						tp:   TemplateParams{Command: "claude", SessionName: "worker", TemplateName: "worker"},
					},
					cfg: runtime.Config{Command: "claude", WorkDir: t.TempDir()},
				}},
				t.TempDir(),
				sp,
				store,
				nil,
				10*time.Second, 1,
			)
			if len(results) != 1 {
				t.Fatalf("len(results) = %d, want 1", len(results))
			}
			result := results[0]
			if result.err != nil || result.outcome != TraceOutcomeDeferred || result.rollbackPending || result.rateLimitScreen {
				t.Fatalf("result = %#v, want deferred without error, rollback, or rate-limit screening", result)
			}
			if commitStartResult(result, sessionFrontDoor(store), clk, events.Discard, 0, ioDiscard{}, ioDiscard{}) {
				t.Fatal("deferred result counted as committed wake")
			}
			got, err := store.Get(bead.ID)
			if err != nil {
				t.Fatalf("Get session: %v", err)
			}
			if got.Status != "open" || got.Metadata["wake_attempts"] != "2" {
				t.Fatalf("session after deferral = %#v, want open state and unchanged failure budget", got)
			}
			if tc.pendingCreate && got.Metadata["pending_create_claim"] != "true" {
				t.Fatalf("pending_create_claim = %q, want true", got.Metadata["pending_create_claim"])
			}
			if got.Metadata["last_woke_at"] != "" {
				t.Fatalf("last_woke_at = %q, want cleared for retry", got.Metadata["last_woke_at"])
			}
			if starts := sp.CountCalls("Start", "worker"); starts != 0 {
				t.Fatalf("Start calls = %d, want 0", starts)
			}
			if stops := sp.CountCalls("Stop", "worker"); stops != 0 {
				t.Fatalf("Stop calls = %d, want 0", stops)
			}
		})
	}
}

// TestStartPreparedStartCandidateRuntimeOnlyBorrowsTheCandidateLease: a
// bead-less legacy start runs its runtime-only handle under the candidate's
// lease (the name's flock the start already holds), instead of taking that
// flock a second time and failing on itself. Without the candidate's lease,
// a flock another holder has defers the controller's start at once.
func TestStartPreparedStartCandidateRuntimeOnlyBorrowsTheCandidateLease(t *testing.T) {
	cityPath := t.TempDir()
	const name = "worker-1"
	item := func(lease *sessionpkg.RuntimeLease) preparedStart {
		return preparedStart{
			candidate: startCandidate{
				info:  sessionpkg.Info{SessionName: name, SessionNameMetadata: name},
				tp:    TemplateParams{TemplateName: "worker"},
				lease: lease,
			},
			cfg: runtime.Config{Command: "claude", WorkDir: t.TempDir()},
		}
	}
	lease, release, err := tryRuntimeLease(nil, cityPath, name, "", 0)
	if err != nil {
		t.Fatalf("taking the candidate's flock: %v", err)
	}
	sp := runtime.NewFake()
	began := time.Now()
	if _, err := startPreparedStartCandidate(context.Background(), context.Background(), item(lease), cityPath, nil, sp, &config.City{}, nil, nil, nil); err != nil {
		t.Fatalf("start under the candidate's lease = %v, want started", err)
	}
	if sp.CountCalls("Start", name) != 1 || time.Since(began) > 2*time.Second {
		t.Fatalf("provider Starts = %d after %v, want one at once", sp.CountCalls("Start", name), time.Since(began))
	}
	if err := sp.Stop(name); err != nil {
		t.Fatal(err)
	}
	began = time.Now()
	_, err = startPreparedStartCandidate(context.Background(), context.Background(), item(nil), cityPath, nil, sp, &config.City{}, nil, nil, nil)
	if !errors.Is(err, sessionpkg.ErrRuntimeLeaseBusy) || errors.Is(err, sessionpkg.ErrSessionStarting) || time.Since(began) > 2*time.Second {
		t.Fatalf("start without the lease under another holder's flock = %v after %v, want ErrRuntimeLeaseBusy at once", err, time.Since(began))
	}
	if sp.CountCalls("Start", name) != 1 {
		t.Fatal("the refused start reached the provider")
	}
	release()
}

// TestStartLeaseNameIsTheStartsName: a start's lease names the runtime the
// start starts: the candidate's own name, or, with none, the row's resolved
// one.
func TestStartLeaseNameIsTheStartsName(t *testing.T) {
	for _, c := range []struct {
		meta, resolved, want string
	}{
		{"worker-1", "worker-1", "worker-1"},
		{"worker-1", "s-gc-1", "worker-1"},
		{"", "s-gc-1", "s-gc-1"},
		{"", "", ""},
	} {
		cand := startCandidate{info: sessionpkg.Info{SessionNameMetadata: c.meta, SessionName: c.resolved}}
		if got := startLeaseName(cand); got != c.want {
			t.Errorf("startLeaseName(meta %q, resolved %q) = %q, want %q", c.meta, c.resolved, got, c.want)
		}
	}
}
