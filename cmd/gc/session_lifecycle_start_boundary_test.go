package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/agent"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/session/sessiontest"
	"github.com/gastownhall/gascity/internal/suspensionstate"
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

type sequencedStartLivenessProvider struct {
	*runtime.Fake
	observations []runtime.Liveness
	errors       []error
	calls        int
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

	usedWorker, err := startPreparedStartCandidate(
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
	if !usedWorker {
		t.Fatal("usedWorker = false, want true")
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

func TestExecutePlannedStartsRestoresPriorIdentityAfterSessionExistsCollision(t *testing.T) {
	env := newReconcilerTestEnv(t)
	candidate, bead, tp := identityPlannerCandidate(t, env, "active")
	env.cfg.Agents[0].PromptMode = "arg"
	startIdentityRuntime(t, env, candidate)
	oldToken, oldGeneration, oldEpoch, oldLastWoke := candidate.info.InstanceToken, candidate.info.Generation, candidate.info.ContinuationEpoch, candidate.info.LastWokeAt
	env.sp.StartErrors[candidate.name()] = runtime.ErrSessionExists
	sp := &sequencedStartLivenessProvider{
		Fake: env.sp,
		observations: []runtime.Liveness{
			{},                           // preparation's stale snapshot
			{},                           // pre-Start check
			{Running: true, Alive: true}, // collision observation
		},
	}
	setSessionCircuitBreakerForTest(t, newSessionCircuitBreaker(sessionCircuitBreakerConfig{}))
	if got := runIdentityPlannerWithProvider(t, env, candidate, tp, false, sp); got != 1 {
		t.Fatalf("wake count = %d, want 1 after attributed session collision", got)
	}
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
	setSessionCircuitBreakerForTest(t, newSessionCircuitBreaker(sessionCircuitBreakerConfig{}))
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
	_, err := startPreparedStartCandidate(context.Background(), preparedStart{
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
		_, err := startPreparedStartCandidate(context.Background(), preparedStart{
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
	if started {
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
	if err != nil || !started {
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
	if _, err := startPreparedStartCandidate(context.Background(), item(lease), cityPath, nil, sp, &config.City{}, nil, nil, nil); err != nil {
		t.Fatalf("start under the candidate's lease = %v, want started", err)
	}
	if sp.CountCalls("Start", name) != 1 || time.Since(began) > 2*time.Second {
		t.Fatalf("provider Starts = %d after %v, want one at once", sp.CountCalls("Start", name), time.Since(began))
	}
	if err := sp.Stop(name); err != nil {
		t.Fatal(err)
	}
	began = time.Now()
	_, err = startPreparedStartCandidate(context.Background(), item(nil), cityPath, nil, sp, &config.City{}, nil, nil, nil)
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
