package main

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
)

type secondLivenessObservationUnavailableProvider struct {
	*runtime.Fake
	mu               sync.Mutex
	observationCalls int
}

func (p *secondLivenessObservationUnavailableProvider) ObserveLivenessWithError(name string, _ []string) (runtime.Liveness, error) {
	p.mu.Lock()
	p.observationCalls++
	call := p.observationCalls
	p.mu.Unlock()
	if call == 2 {
		return runtime.Liveness{}, fmt.Errorf("preserved named observation: %w", runtime.ErrRuntimeUnavailable)
	}
	running := p.IsRunning(name)
	return runtime.Liveness{Running: running, Alive: running}, nil
}

func (p *secondLivenessObservationUnavailableProvider) observationCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.observationCalls
}

func TestProviderDecoratorsPreserveLivenessObservationUncertainty(t *testing.T) {
	base := &startUnavailableLivenessProvider{Fake: runtime.NewFake()}
	for name, sp := range map[string]runtime.Provider{
		"bounded status":   newBoundedStatusProvider(base),
		"attachment cache": &attachmentCachingProvider{Provider: base, cache: map[string]bool{}},
	} {
		t.Run(name, func(t *testing.T) {
			obs, err := runtime.ObserveLivenessWithError(sp, "worker", nil)
			if obs != (runtime.Liveness{}) || !errors.Is(err, runtime.ErrRuntimeUnavailable) {
				t.Fatalf("decorated observation = (%+v, %v), want zero + runtime unavailable", obs, err)
			}
		})
	}
}

func TestReconcileSessionBeadsNamedSpecReappearsDuringLivenessErrorClearsDeferral(t *testing.T) {
	env := newReconcilerTestEnv()
	sessionName := config.NamedSessionRuntimeName("test-city", config.Workspace{Name: "test-city"}, "worker")
	withSpec := &config.City{
		Workspace:     config.Workspace{Name: "test-city"},
		Agents:        []config.Agent{{Name: "worker", StartCommand: "true"}},
		NamedSessions: []config.NamedSession{{Template: "worker", Mode: "always"}},
	}
	withoutSpec := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents:    []config.Agent{{Name: "worker", StartCommand: "true"}},
	}
	if err := env.sp.Start(context.Background(), sessionName, runtime.Config{Command: "true"}); err != nil {
		t.Fatalf("start named runtime: %v", err)
	}
	session := env.createSessionBead(sessionName, "worker")
	env.setSessionMetadata(&session, map[string]string{
		namedSessionMetadataKey:      "true",
		namedSessionIdentityMetadata: "worker",
		namedSessionModeMetadata:     "always",
		"state":                      "active",
		"last_woke_at":               env.clk.Now().UTC().Format(time.RFC3339),
		"session_key":                "resume-worker",
		"started_config_hash":        "config-worker",
		"continuation_reset_pending": "",
	})
	runTick := func(sp runtime.Provider) {
		configuredNames := configuredSessionNames(env.cfg, "", env.store)
		reconcileSessionBeads(
			context.Background(), []beads.Bead{session}, env.desiredState,
			configuredNames, env.cfg, sp, env.store, nil, nil, nil, env.dt,
			nil, false, nil, "", nil, env.clk, env.rec, 0, 0,
			&env.stdout, &env.stderr, env.startOptions...,
		)
	}

	env.cfg = withoutSpec
	runTick(env.sp)
	if ds := env.dt.get(session.ID); ds != nil {
		t.Fatalf("first absent tick started drain: reason=%q", ds.reason)
	}
	before, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get before unavailable tick: %v", err)
	}

	env.cfg = withSpec
	runTick(&startUnavailableLivenessProvider{Fake: env.sp})
	after, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get after unavailable tick: %v", err)
	}
	if !maps.Equal(after.Metadata, before.Metadata) {
		t.Fatalf("metadata mutated during liveness deferral: before=%#v after=%#v", before.Metadata, after.Metadata)
	}

	env.cfg = withoutSpec
	runTick(env.sp)
	if ds := env.dt.get(session.ID); ds != nil {
		t.Fatalf("non-consecutive spec absence started drain: reason=%q", ds.reason)
	}
	if !env.sp.IsRunning(sessionName) {
		t.Fatalf("named runtime %q stopped after non-consecutive spec absence", sessionName)
	}
}

func TestReconcileSessionBeadsPreservedNamedSecondaryLivenessErrorDefersLifecycle(t *testing.T) {
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
		"pending_create_claim":       "true",
		"pending_create_started_at":  env.clk.Now().UTC().Format(time.RFC3339),
		"last_woke_at":               env.clk.Now().Add(-10 * time.Second).UTC().Format(time.RFC3339),
		"session_key":                "keep-session",
		"started_config_hash":        "keep-hash",
		"continuation_reset_pending": "keep-reset",
		"wake_attempts":              "2",
	})
	before, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get before reconcile: %v", err)
	}
	sp := &secondLivenessObservationUnavailableProvider{Fake: env.sp}
	rec := events.NewFake()

	woken := reconcileSessionBeads(
		context.Background(), []beads.Bead{before}, env.desiredState,
		configuredSessionNames(env.cfg, "", env.store), env.cfg, sp, env.store,
		nil, nil, nil, env.dt, map[string]int{}, false, nil, "", nil, env.clk,
		rec, 0, 0, &env.stdout, &env.stderr, env.startOptions...,
	)
	if woken != 0 {
		t.Fatalf("woken = %d, want 0", woken)
	}
	after, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get after reconcile: %v", err)
	}
	if after.Status != before.Status || !maps.Equal(after.Metadata, before.Metadata) {
		t.Fatalf("session mutated during secondary liveness error: before=%#v after=%#v", before, after)
	}
	if got := sp.observationCount(); got != 2 {
		t.Fatalf("liveness observations = %d, want 2", got)
	}
	if sp.CountCalls("Start", sessionName) != 0 || sp.CountCalls("Stop", sessionName) != 0 || len(rec.Events) != 0 || env.dt.get(session.ID) != nil {
		t.Fatal("secondary liveness uncertainty caused a lifecycle side effect")
	}
}
