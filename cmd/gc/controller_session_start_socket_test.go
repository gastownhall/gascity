package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/rollout"
	"github.com/gastownhall/gascity/internal/rollout/gate"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/testutil"
)

type wakeBeforeSuspendStopProvider struct {
	*unattendedStopProvider
	once sync.Once
	wake func()
}

func (p *wakeBeforeSuspendStopProvider) ObserveFreshLiveness(target runtime.LivenessTarget) runtime.Liveness {
	p.once.Do(p.wake)
	return p.unattendedStopProvider.ObserveFreshLiveness(target)
}

func TestPokeSessionStartControllerUsesExactKey(t *testing.T) {
	var commands []string
	fallbackCalled := false
	err := pokeSessionStartControllerWith(
		"/city",
		"ga-session-1",
		func(cityPath, command string) ([]byte, error) {
			if cityPath != "/city" {
				t.Fatalf("city path = %q, want /city", cityPath)
			}
			commands = append(commands, command)
			return []byte(sessionStartSocketReplyOK), nil
		},
		func(string) error {
			fallbackCalled = true
			return nil
		},
	)
	if err != nil {
		t.Fatalf("pokeSessionStartControllerWith: %v", err)
	}
	if fallbackCalled {
		t.Fatal("generic fallback called after exact admission succeeded")
	}
	want := []string{sessionStartCommandPrefix + "ga-session-1"}
	if len(commands) != len(want) || commands[0] != want[0] {
		t.Fatalf("commands = %v, want %v", commands, want)
	}
}

func TestPokeSessionStartControllerFallsBackWhenExactIngressIsUnavailable(t *testing.T) {
	fallbackCalls := 0
	err := pokeSessionStartControllerWith(
		"/city",
		"ga-session-1",
		func(string, string) ([]byte, error) {
			return []byte(sessionStartSocketReplyFallback), nil
		},
		func(cityPath string) error {
			if cityPath != "/city" {
				t.Fatalf("fallback city path = %q, want /city", cityPath)
			}
			fallbackCalls++
			return nil
		},
	)
	if err == nil || !strings.Contains(err.Error(), "exact session-start hint for \"ga-session-1\" in city \"/city\" was not admitted") || !strings.Contains(err.Error(), "controller returned \"fallback\"") || !strings.Contains(err.Error(), "generic fallback requested") {
		t.Fatalf("pokeSessionStartControllerWith = %v, want exact-ingress fallback diagnostic", err)
	}
	if fallbackCalls != 1 {
		t.Fatalf("fallback calls = %d, want 1", fallbackCalls)
	}
}

func TestPokeSessionStartControllerDoesNotFallBackWhenRequireRefusesClosed(t *testing.T) {
	fallbackCalls := 0
	err := pokeSessionStartControllerWith(
		"/city",
		"ga-session-1",
		func(string, string) ([]byte, error) {
			return []byte(sessionStartSocketReplyBlocked), nil
		},
		func(string) error {
			fallbackCalls++
			return nil
		},
	)
	if !errors.Is(err, errSessionStartControllerBlocked) {
		t.Fatalf("require refusal error = %v, want errors.Is(errSessionStartControllerBlocked)", err)
	}
	if fallbackCalls != 0 {
		t.Fatalf("fallback calls = %d, want 0", fallbackCalls)
	}
}

func TestHandleControllerConnRoutesValidatedSessionStartKey(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close() //nolint:errcheck

	admitted := make(chan string, 1)
	go handleControllerConn(
		server,
		"/city",
		controllerHostingStandalone,
		func() {},
		&atomic.Bool{},
		make(chan convergenceRequest),
		make(chan struct{}, 1),
		make(chan struct{}, 1),
		func(sessionID string) sessionStartSocketReply {
			admitted <- sessionID
			return sessionStartSocketReplyOK
		},
	)

	if _, err := fmt.Fprintln(client, sessionStartCommandPrefix+"ga-session-1"); err != nil {
		t.Fatalf("write command: %v", err)
	}
	response, err := bufio.NewReader(client).ReadString('\n')
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if got := strings.TrimSpace(response); got != string(sessionStartSocketReplyOK) {
		t.Fatalf("response = %q, want %q", got, sessionStartSocketReplyOK)
	}
	select {
	case sessionID := <-admitted:
		if sessionID != "ga-session-1" {
			t.Fatalf("session ID = %q, want ga-session-1", sessionID)
		}
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("timed out waiting for exact session-start request")
	}
}

func TestHandleControllerConnRejectsInvalidSessionStartKey(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close() //nolint:errcheck

	admitCalled := false
	go handleControllerConn(
		server,
		"/city",
		controllerHostingStandalone,
		func() {},
		&atomic.Bool{},
		make(chan convergenceRequest),
		make(chan struct{}, 1),
		make(chan struct{}, 1),
		func(string) sessionStartSocketReply {
			admitCalled = true
			return sessionStartSocketReplyOK
		},
	)

	if _, err := fmt.Fprintln(client, sessionStartCommandPrefix+" bad key "); err != nil {
		t.Fatalf("write command: %v", err)
	}
	response, err := bufio.NewReader(client).ReadString('\n')
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if got := strings.TrimSpace(response); got != string(sessionStartSocketReplyInvalid) {
		t.Fatalf("response = %q, want %q", got, sessionStartSocketReplyInvalid)
	}
	if admitCalled {
		t.Fatal("invalid key reached session-start admission")
	}
}

func TestCityRuntimeAdmitsSessionStartSocketKey(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker", StartCommand: "true"}}}
	bead := env.createSessionBead("worker", "worker")
	if err := env.store.SetMetadataBatch(bead.ID, session.RequestExplicitWakePatch(string(session.WakeCauseExplicit), env.clk.Now())); err != nil {
		t.Fatalf("request explicit wake: %v", err)
	}
	reconciled := make(chan sessionStartAdmission, 1)
	controller, err := newSessionStartController(sessionStartControllerOptions{
		Workers:     1,
		MaxDistinct: 8,
		MaxRetries:  0,
		Reconcile: func(_ context.Context, admission sessionStartAdmission) error {
			reconciled <- admission
			return nil
		},
	})
	if err != nil {
		t.Fatalf("newSessionStartController: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err := controller.Start(ctx); err != nil {
		cancel()
		t.Fatalf("controller.Start: %v", err)
	}
	defer controller.Stop()

	cr := &CityRuntime{
		cfg:                    env.cfg,
		sp:                     env.sp,
		cs:                     coherentSessionStartControllerStateForTest(env.cfg, env.sp, env.store, rollout.Auto),
		sessionStartController: controller,
		sessionStartOwnership:  sessionStartOwnershipKeyed,
	}
	if reply := cr.admitSessionStartSocketKey(bead.ID); reply != sessionStartSocketReplyOK {
		t.Fatalf("reply = %q, want %q", reply, sessionStartSocketReplyOK)
	}
	select {
	case admission := <-reconciled:
		if admission.SessionID != bead.ID || admission.Source != sessionStartAdmissionSocket {
			t.Fatalf("admission = %+v, want exact socket key", admission)
		}
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("timed out waiting for keyed reconciliation")
	}
	cancel()
}

func TestCityRuntimeSessionStartSocketIngressAdmitsStrictDefaultPoolWake(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker", StartCommand: "true"}}}
	bead := env.createSessionBead("worker-1", "worker")
	env.setSessionMetadata(&bead, map[string]string{
		"session_name":                          PoolSessionName("worker", bead.ID),
		"agent_name":                            "worker-1",
		"session_origin":                        "ephemeral",
		poolManagedMetadataKey:                  "true",
		"pool_slot":                             "1",
		beadmeta.TriggerBeadIDMetadataKey:       "ga-work-1",
		beadmeta.TriggerBeadStoreRefMetadataKey: "city:test-city",
	})
	if err := env.store.SetMetadataBatch(bead.ID, session.RequestExplicitWakePatch(string(session.WakeCauseExplicit), env.clk.Now())); err != nil {
		t.Fatalf("request explicit wake: %v", err)
	}
	_, revision, err := getAuthoritativeSessionStartRecord(env.store, bead.ID)
	if err != nil {
		t.Fatalf("read strict-default pool member: %v", err)
	}

	reconciled := make(chan sessionStartAdmission, 1)
	controller, err := newSessionStartController(sessionStartControllerOptions{
		Workers: 1, MaxDistinct: 8, MaxRetries: 0,
		Reconcile: func(_ context.Context, admission sessionStartAdmission) error {
			reconciled <- admission
			return nil
		},
	})
	if err != nil {
		t.Fatalf("newSessionStartController: %v", err)
	}
	if err := controller.Start(t.Context()); err != nil {
		t.Fatalf("controller.Start: %v", err)
	}
	t.Cleanup(controller.Stop)

	cr := &CityRuntime{
		cfg:                    env.cfg,
		sp:                     env.sp,
		cs:                     coherentSessionStartControllerStateForTest(env.cfg, env.sp, env.store, rollout.Auto),
		sessionStartController: controller,
		sessionStartOwnership:  sessionStartOwnershipKeyed,
		stderr:                 &bytes.Buffer{},
	}
	if got := cr.admitSessionStartSocketKey(bead.ID); got != sessionStartSocketReplyOK {
		t.Fatalf("reply = %q, want %q for an existing strict-default pool member", got, sessionStartSocketReplyOK)
	}
	select {
	case admission := <-reconciled:
		if admission.SessionID != bead.ID || admission.Source != sessionStartAdmissionSocket {
			t.Fatalf("admission = %+v, want exact socket key", admission)
		}
		if admission.StrictDefaultPoolWake == nil {
			t.Fatal("strict-default pool wake admission did not retain its private witness")
		}
		if got, want := *admission.StrictDefaultPoolWake, (strictDefaultPoolWakeStartLease{
			SessionID: bead.ID, SessionName: PoolSessionName("worker", bead.ID), InstanceToken: "test-token",
			SessionRevision: revision, PoolTarget: "worker", PoolSlot: "1", TriggerBeadID: "ga-work-1",
			TriggerBeadStoreRef: "city:test-city", ControllerGeneration: 1,
		}); got != want {
			t.Fatalf("strict-default pool wake witness = %+v, want %+v", got, want)
		}
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("timed out waiting for strict-default pool wake admission")
	}
}

func TestCityRuntimeSessionStartSocketIngressLeavesUnsupportedStrictDefaultPoolWakeShapesToLegacy(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*config.City, map[string]string)
	}{
		{name: "bounded", mutate: func(cfg *config.City, _ map[string]string) { cfg.Agents[0].MaxActiveSessions = intPtr(3) }},
		{name: "singleton", mutate: func(cfg *config.City, _ map[string]string) { cfg.Agents[0].MaxActiveSessions = intPtr(1) }},
		{name: "named", mutate: func(_ *config.City, metadata map[string]string) { metadata[session.NamedSessionMetadataKey] = "true" }},
		{name: "manual", mutate: func(_ *config.City, metadata map[string]string) { metadata["manual_session"] = "true" }},
		{name: "dependency", mutate: func(cfg *config.City, _ map[string]string) {
			cfg.Agents[0].DependsOn = []string{"database"}
			cfg.Agents = append(cfg.Agents, config.Agent{Name: "database", StartCommand: "true", MaxActiveSessions: intPtr(1)})
		}},
		{name: "malformed-slot", mutate: func(_ *config.City, metadata map[string]string) { metadata["pool_slot"] = "not-a-slot" }},
		{name: "legacy-identity", mutate: func(_ *config.City, metadata map[string]string) { metadata["session_origin"] = "" }},
		{name: "workspace-cap", mutate: func(cfg *config.City, _ map[string]string) { cfg.Workspace.MaxActiveSessions = intPtr(8) }},
		{name: "custom-scaling", mutate: func(cfg *config.City, _ map[string]string) { cfg.Agents[0].ScaleCheck = "true" }},
		{name: "namepool", mutate: func(cfg *config.City, _ map[string]string) { cfg.Agents[0].Namepool = "workers" }},
		{name: "held", mutate: func(_ *config.City, metadata map[string]string) {
			metadata["held_until"] = time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
		}},
		{name: "quarantined", mutate: func(_ *config.City, metadata map[string]string) {
			metadata["quarantined_until"] = time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
		}},
		{name: "pending-create", mutate: func(_ *config.City, metadata map[string]string) {
			metadata["pending_create_claim"] = "true"
			metadata["state"] = string(session.StateCreating)
		}},
		{name: "terminal", mutate: func(_ *config.City, metadata map[string]string) {
			metadata["state"] = string(session.StateFailedCreate)
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			env := newReconcilerTestEnv()
			env.cfg = &config.City{Agents: []config.Agent{{Name: "worker", StartCommand: "true"}}}
			bead := env.createSessionBead("worker-1", "worker")
			metadata := map[string]string{
				"session_name":                          PoolSessionName("worker", bead.ID),
				"agent_name":                            "worker-1",
				"session_origin":                        "ephemeral",
				poolManagedMetadataKey:                  "true",
				"pool_slot":                             "1",
				beadmeta.TriggerBeadIDMetadataKey:       "ga-work-1",
				beadmeta.TriggerBeadStoreRefMetadataKey: "city:test-city",
			}
			test.mutate(env.cfg, metadata)
			env.setSessionMetadata(&bead, metadata)
			if err := env.store.SetMetadataBatch(bead.ID, session.RequestExplicitWakePatch(string(session.WakeCauseExplicit), env.clk.Now())); err != nil {
				t.Fatalf("request explicit wake: %v", err)
			}

			controller, err := newSessionStartController(sessionStartControllerOptions{
				Workers: 1, MaxDistinct: 8, MaxRetries: 0,
				Reconcile: func(context.Context, sessionStartAdmission) error { return nil },
			})
			if err != nil {
				t.Fatalf("newSessionStartController: %v", err)
			}
			if err := controller.Start(t.Context()); err != nil {
				t.Fatalf("controller.Start: %v", err)
			}
			t.Cleanup(controller.Stop)
			cr := &CityRuntime{
				cfg: env.cfg, sp: env.sp,
				cs:                     coherentSessionStartControllerStateForTest(env.cfg, env.sp, env.store, rollout.Auto),
				sessionStartController: controller,
				sessionStartOwnership:  sessionStartOwnershipKeyed,
				stderr:                 &bytes.Buffer{},
			}
			if got := cr.admitSessionStartSocketKey(bead.ID); got != sessionStartSocketReplyFallback {
				t.Fatalf("reply = %q, want legacy fallback", got)
			}
		})
	}
}

func TestCityRuntimeSessionStartSocketIngressFallsBackToLegacyOwner(t *testing.T) {
	var stderr bytes.Buffer
	cr := &CityRuntime{stderr: &stderr, sessionStartOwnership: sessionStartOwnershipLegacy}
	if got := cr.admitSessionStartSocketKey("ga-session-1"); got != sessionStartSocketReplyFallback {
		t.Fatalf("reply = %q, want %q", got, sessionStartSocketReplyFallback)
	}
	if !strings.Contains(stderr.String(), "exact session-start socket fallback for ga-session-1: controller unavailable or not keyed") {
		t.Fatalf("fallback diagnostic = %q, want unavailable-owner reason", stderr.String())
	}
}

func TestCityRuntimeSessionStartSocketIngressAdmitsLiveSingletonDependency(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{
		{Name: "database", StartCommand: "true", MaxActiveSessions: intPtr(1)},
		{Name: "worker", StartCommand: "true", DependsOn: []string{"database"}},
	}}
	dependency := env.createSessionBead("database", "database")
	env.markSessionActive(&dependency)
	env.addDesired("database", "database", true)
	bead := env.createSessionBead("worker", "worker")
	if err := env.store.SetMetadataBatch(bead.ID, session.RequestExplicitWakePatch(string(session.WakeCauseExplicit), env.clk.Now())); err != nil {
		t.Fatalf("request explicit wake: %v", err)
	}

	reconciled := make(chan sessionStartAdmission, 1)
	controller, err := newSessionStartController(sessionStartControllerOptions{
		Workers:     1,
		MaxDistinct: 8,
		MaxRetries:  0,
		Reconcile: func(_ context.Context, admission sessionStartAdmission) error {
			reconciled <- admission
			return nil
		},
	})
	if err != nil {
		t.Fatalf("newSessionStartController: %v", err)
	}
	if err := controller.Start(context.Background()); err != nil {
		t.Fatalf("controller.Start: %v", err)
	}
	t.Cleanup(controller.Stop)

	var stderr bytes.Buffer
	cr := &CityRuntime{
		cfg:                    env.cfg,
		sp:                     runtime.NewFake(),
		cs:                     coherentSessionStartControllerStateForTest(env.cfg, env.sp, env.store, rollout.Auto),
		sessionStartController: controller,
		sessionStartOwnership:  sessionStartOwnershipKeyed,
		stderr:                 &stderr,
	}
	if got := cr.admitSessionStartSocketKey(bead.ID); got != sessionStartSocketReplyOK {
		t.Fatalf("reply = %q, want %q for a live singleton dependency", got, sessionStartSocketReplyOK)
	}
	select {
	case admission := <-reconciled:
		if admission.ConfiguredDependency == nil {
			t.Fatal("configured-dependency admission did not retain its private witness")
		}
		if got, want := *admission.ConfiguredDependency, (configuredDependencyStartLease{
			SessionID:               bead.ID,
			TargetTemplate:          "worker",
			DependencyTemplate:      "database",
			DependencySessionID:     dependency.ID,
			DependencySessionName:   "database",
			DependencyInstanceToken: "test-token",
			ControllerGeneration:    1,
		}); got != want {
			t.Fatalf("configured-dependency witness = %+v, want %+v", got, want)
		}
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("timed out waiting for configured-dependency admission")
	}
}

func TestCityRuntimeSessionStartSocketIngressLeavesUnsupportedDependencyShapesToLegacy(t *testing.T) {
	tests := []struct {
		name       string
		dependsOn  []string
		dependency *config.Agent
		live       []string
		metadata   map[string]string
	}{
		{name: "missing", dependsOn: []string{"missing"}},
		{name: "cold", dependsOn: []string{"database"}, dependency: &config.Agent{Name: "database", StartCommand: "true", MaxActiveSessions: intPtr(1)}},
		{name: "multiple", dependsOn: []string{"database", "cache"}, dependency: &config.Agent{Name: "database", StartCommand: "true", MaxActiveSessions: intPtr(1)}, live: []string{"database", "cache"}},
		{name: "non-singleton", dependsOn: []string{"database"}, dependency: &config.Agent{Name: "database", StartCommand: "true"}, live: []string{"database"}},
		{name: "named", dependsOn: []string{"database"}, dependency: &config.Agent{Name: "database", StartCommand: "true", MaxActiveSessions: intPtr(1)}, live: []string{"database"}, metadata: map[string]string{session.NamedSessionMetadataKey: "true"}},
		{name: "pool", dependsOn: []string{"database"}, dependency: &config.Agent{Name: "database", StartCommand: "true", MaxActiveSessions: intPtr(1)}, live: []string{"database"}, metadata: map[string]string{poolManagedMetadataKey: "true"}},
		{name: "manual", dependsOn: []string{"database"}, dependency: &config.Agent{Name: "database", StartCommand: "true", MaxActiveSessions: intPtr(1)}, live: []string{"database"}, metadata: map[string]string{"manual_session": "true"}},
		{name: "dependency-only", dependsOn: []string{"database"}, dependency: &config.Agent{Name: "database", StartCommand: "true", MaxActiveSessions: intPtr(1)}, live: []string{"database"}, metadata: map[string]string{"dependency_only": "true"}},
		{name: "pending-create", dependsOn: []string{"database"}, dependency: &config.Agent{Name: "database", StartCommand: "true", MaxActiveSessions: intPtr(1)}, live: []string{"database"}, metadata: map[string]string{"pending_create_claim": "true", "state": string(session.StateCreating)}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			env := newReconcilerTestEnv()
			agents := []config.Agent{{Name: "worker", StartCommand: "true", DependsOn: test.dependsOn}}
			if test.dependency != nil {
				agents = append([]config.Agent{*test.dependency}, agents...)
			}
			if test.name == "multiple" {
				agents = append([]config.Agent{{Name: "cache", StartCommand: "true", MaxActiveSessions: intPtr(1)}}, agents...)
			}
			env.cfg = &config.City{Agents: agents}
			for _, dependency := range test.live {
				env.addDesired(dependency, dependency, true)
			}
			bead := env.createSessionBead("worker", "worker")
			if err := env.store.SetMetadataBatch(bead.ID, session.RequestExplicitWakePatch(string(session.WakeCauseExplicit), env.clk.Now())); err != nil {
				t.Fatalf("request explicit wake: %v", err)
			}
			env.setSessionMetadata(&bead, test.metadata)

			controller, err := newSessionStartController(sessionStartControllerOptions{
				Workers: 1, MaxDistinct: 8, MaxRetries: 0,
				Reconcile: func(context.Context, sessionStartAdmission) error { return nil },
			})
			if err != nil {
				t.Fatalf("newSessionStartController: %v", err)
			}
			if err := controller.Start(t.Context()); err != nil {
				t.Fatalf("controller.Start: %v", err)
			}
			t.Cleanup(controller.Stop)
			cr := &CityRuntime{
				cfg:                    env.cfg,
				sp:                     env.sp,
				cs:                     coherentSessionStartControllerStateForTest(env.cfg, env.sp, env.store, rollout.Auto),
				sessionStartController: controller,
				sessionStartOwnership:  sessionStartOwnershipKeyed,
				stderr:                 &bytes.Buffer{},
			}
			if got := cr.admitSessionStartSocketKey(bead.ID); got != sessionStartSocketReplyFallback {
				t.Fatalf("reply = %q, want legacy fallback", got)
			}
		})
	}
}

func TestReconcileStrictDefaultPoolWakeStartsSameMemberAndFencesDrift(t *testing.T) {
	type fixture struct {
		env    *reconcilerTestEnv
		bead   beads.Bead
		lease  strictDefaultPoolWakeStartLease
		before session.Info
	}
	newFixture := func(t *testing.T) fixture {
		t.Helper()
		env := newReconcilerTestEnv()
		env.cfg = &config.City{
			Workspace: config.Workspace{Name: "test-city"},
			Agents:    []config.Agent{{Name: "worker", StartCommand: "true"}},
		}
		bead := env.createSessionBead("worker-1", "worker")
		env.setSessionMetadata(&bead, map[string]string{
			"session_name":                          PoolSessionName("worker", bead.ID),
			"agent_name":                            "worker-1",
			"session_origin":                        "ephemeral",
			poolManagedMetadataKey:                  "true",
			"pool_slot":                             "1",
			beadmeta.TriggerBeadIDMetadataKey:       "ga-work-1",
			beadmeta.TriggerBeadStoreRefMetadataKey: "city:test-city",
		})
		if err := env.store.SetMetadataBatch(bead.ID, session.RequestExplicitWakePatch(string(session.WakeCauseExplicit), env.clk.Now())); err != nil {
			t.Fatalf("request explicit wake: %v", err)
		}
		info, revision, err := getAuthoritativeSessionStartRecord(env.store, bead.ID)
		if err != nil {
			t.Fatalf("read strict-default pool member: %v", err)
		}
		lease, certified := certifyStrictDefaultPoolWakeStartLease(info, revision, env.cfg, 1, env.clk.Now().UTC())
		if !certified {
			t.Fatal("canonical strict-default pool member was not certified")
		}
		return fixture{env: env, bead: bead, lease: lease, before: info}
	}

	for _, mode := range []rollout.Mode{rollout.Auto, rollout.Require} {
		t.Run("same-member/"+string(mode), func(t *testing.T) {
			f := newFixture(t)
			params := exactSessionStartTestParams(t, f.env)
			params.Generation = 1
			params.RolloutMode = mode
			params.ValidateStrictDefaultPoolWakeStart = func(session.Info, strictDefaultPoolWakeStartLease) bool { return true }
			params.EnterStrictDefaultPoolWakeStart = func(strictDefaultPoolWakeStartLease) bool { return true }

			owner, err := reconcileExactSessionStartWithOwner(t.Context(), sessionStartAdmission{
				SessionID: f.bead.ID, Source: sessionStartAdmissionSocket, StrictDefaultPoolWake: &f.lease,
			}, params)
			if err != nil || owner != exactSessionStartKeyedOwner {
				t.Fatalf("reconcile result = (owner=%v, err=%v), want keyed success", owner, err)
			}
			started := f.env.sessionInfo(f.bead.ID)
			if started.ID != f.before.ID || started.SessionNameMetadata != f.before.SessionNameMetadata ||
				started.PoolSlot != f.before.PoolSlot || started.TriggerBeadID != f.before.TriggerBeadID ||
				started.TriggerBeadStoreRef != f.before.TriggerBeadStoreRef || started.MetadataState != string(session.StateActive) {
				t.Fatalf("started member = %+v, want same ID/name/slot/trigger active", started)
			}
			if got := f.env.sp.CountCalls("Start", f.lease.SessionName); got != 1 {
				t.Fatalf("provider Start calls = %d, want exactly 1", got)
			}
			snapshot, err := loadSessionBeadSnapshot(f.env.store)
			if err != nil {
				t.Fatalf("load sessions after exact wake: %v", err)
			}
			open := snapshot.OpenInfos()
			if len(open) != 1 || open[0].ID != f.bead.ID {
				t.Fatalf("open sessions = %+v, want only original member %q", open, f.bead.ID)
			}
		})

		for _, phase := range []struct {
			name         string
			validThrough int32
			preMutation  bool
		}{
			{name: "pre-wake", validThrough: 1, preMutation: true},
			{name: "provider-entry", validThrough: 2},
		} {
			t.Run("drift/"+phase.name+"/"+string(mode), func(t *testing.T) {
				f := newFixture(t)
				params := exactSessionStartTestParams(t, f.env)
				params.Generation = 1
				params.RolloutMode = mode
				var validations atomic.Int32
				params.ValidateStrictDefaultPoolWakeStart = func(session.Info, strictDefaultPoolWakeStartLease) bool {
					return validations.Add(1) <= phase.validThrough
				}
				var entered atomic.Bool
				params.EnterStrictDefaultPoolWakeStart = func(strictDefaultPoolWakeStartLease) bool {
					entered.Store(true)
					return true
				}

				owner, err := reconcileExactSessionStartWithOwner(t.Context(), sessionStartAdmission{
					SessionID: f.bead.ID, Source: sessionStartAdmissionSocket, StrictDefaultPoolWake: &f.lease,
				}, params)
				if phase.preMutation && mode == rollout.Auto {
					if owner != exactSessionStartLegacyOwner || err != nil {
						t.Fatalf("Auto pre-wake drift result = (owner=%v, err=%v), want clean legacy yield", owner, err)
					}
				} else if owner != exactSessionStartKeyedOwner || err == nil {
					t.Fatalf("parked drift result = (owner=%v, err=%v), want keyed error", owner, err)
				}
				if got := f.env.sp.CountCalls("Start", f.lease.SessionName); got != 0 {
					t.Fatalf("provider Start calls = %d, want 0 after witness drift", got)
				}
				if entered.Load() == phase.preMutation {
					t.Fatalf("entered after %s drift = %t, want %t", phase.name, entered.Load(), !phase.preMutation)
				}
				if phase.preMutation {
					after := f.env.sessionInfo(f.bead.ID)
					if after.InstanceToken != f.before.InstanceToken || after.MetadataState != f.before.MetadataState ||
						after.WakeRequest != f.before.WakeRequest || after.LastWokeAt != f.before.LastWokeAt {
						t.Fatalf("pre-wake drift mutated member: before=%+v after=%+v", f.before, after)
					}
				}
			})
		}
	}
}

func TestStrictDefaultPoolWakeInFlightCoalescingReleasesRetainedAdmissionAfterStart(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents:    []config.Agent{{Name: "worker", StartCommand: "true"}},
	}
	bead := env.createSessionBead("worker-1", "worker")
	env.setSessionMetadata(&bead, map[string]string{
		"session_name":                          PoolSessionName("worker", bead.ID),
		"agent_name":                            "worker-1",
		"session_origin":                        "ephemeral",
		poolManagedMetadataKey:                  "true",
		"pool_slot":                             "1",
		beadmeta.TriggerBeadIDMetadataKey:       "ga-work-1",
		beadmeta.TriggerBeadStoreRefMetadataKey: "city:test-city",
	})
	if err := env.store.SetMetadataBatch(bead.ID, session.RequestExplicitWakePatch(string(session.WakeCauseExplicit), env.clk.Now())); err != nil {
		t.Fatalf("request explicit wake: %v", err)
	}
	info, revision, err := getAuthoritativeSessionStartRecord(env.store, bead.ID)
	if err != nil {
		t.Fatalf("read strict-default pool member: %v", err)
	}
	lease, certified := certifyStrictDefaultPoolWakeStartLease(info, revision, env.cfg, 1, env.clk.Now().UTC())
	if !certified {
		t.Fatal("canonical strict-default pool member was not certified")
	}

	entered := make(chan struct{})
	releaseStart := make(chan struct{})
	var released atomic.Bool
	release := func() {
		if released.CompareAndSwap(false, true) {
			close(releaseStart)
		}
	}
	t.Cleanup(release)
	params := exactSessionStartTestParams(t, env)
	params.Generation = 1
	params.RolloutMode = rollout.Require
	params.ValidateStrictDefaultPoolWakeStart = func(session.Info, strictDefaultPoolWakeStartLease) bool { return true }
	var attempts atomic.Int32
	var controller *sessionStartController
	controller, err = newSessionStartController(sessionStartControllerOptions{
		Workers: 1, MaxDistinct: 4, MaxRetries: 0,
		Reconcile: func(ctx context.Context, admission sessionStartAdmission) error {
			attempts.Add(1)
			return reconcileExactSessionStart(ctx, admission, params)
		},
	})
	if err != nil {
		t.Fatalf("new session-start controller: %v", err)
	}
	params.EnterStrictDefaultPoolWakeStart = func(enteredLease strictDefaultPoolWakeStartLease) bool {
		if !controller.enterStrictDefaultPoolWakeStart(enteredLease) {
			return false
		}
		close(entered)
		<-releaseStart
		return true
	}
	if err := controller.Start(t.Context()); err != nil {
		t.Fatalf("start session-start controller: %v", err)
	}
	t.Cleanup(controller.Stop)
	if outcome, err := controller.AdmitStrictDefaultPoolWake(lease, sessionStartAdmissionSocket); err != nil || outcome != sessionStartAdmissionAccepted {
		t.Fatalf("admit strict-default pool wake = (%q, %v), want accepted", outcome, err)
	}
	awaitClose(t, entered, "strict-default pool wake pre-wake entry")
	creating := env.sessionInfo(bead.ID)
	if creating.MetadataState != string(session.StateCreating) || creating.InstanceToken == "" || creating.InstanceToken == lease.InstanceToken {
		t.Fatalf("entered pool member = %+v, want creating with rotated instance token", creating)
	}
	if outcome, err := controller.Admit(bead.ID, sessionStartAdmissionSocket); err != nil || outcome != sessionStartAdmissionCoalesced {
		t.Fatalf("coalesce same-key socket hint = (%q, %v), want coalesced", outcome, err)
	}
	coalesced, ok := controller.readAdmission(bead.ID)
	if !ok || coalesced.StrictDefaultPoolWake == nil || *coalesced.StrictDefaultPoolWake != lease || !coalesced.StrictDefaultPoolWakeEntered {
		t.Fatalf("coalesced admission = %+v, want retained entered strict-default pool wake", coalesced)
	}
	release()
	awaitCond(t, func() bool {
		current := env.sessionInfo(bead.ID)
		return current.MetadataState == string(session.StateActive) &&
			(controller.Pending() == 0 || attempts.Load() >= 2)
	}, "coalesced strict-default pool wake completion or redrive")
	active := env.sessionInfo(bead.ID)
	if active.InstanceToken != creating.InstanceToken {
		t.Fatalf("active instance token = %q, want exact started incarnation %q", active.InstanceToken, creating.InstanceToken)
	}
	if retained, ok := controller.readAdmission(bead.ID); ok {
		t.Fatalf("coalesced strict-default pool wake admission remained after exact incarnation became active: %+v", retained)
	}
	if got := env.sp.CountCalls("Start", lease.SessionName); got != 1 {
		t.Fatalf("provider Start calls = %d, want exactly 1", got)
	}
}

func TestConfiguredNamedSessionWakeStartsSameCanonicalSession(t *testing.T) {
	for _, mode := range []rollout.Mode{rollout.Auto, rollout.Require} {
		t.Run(string(mode), func(t *testing.T) {
			testConfiguredNamedSessionWakeStartsSameCanonicalSession(t, mode)
		})
	}
}

func TestConfiguredNamedPinnedSessionStartsSameCanonicalSession(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Workspace:     config.Workspace{Name: "test-city"},
		Agents:        []config.Agent{{Name: "worker", StartCommand: "true"}},
		NamedSessions: []config.NamedSession{{Name: "reviewer", Template: "worker", Mode: "on_demand"}},
	}
	spec, ok := findNamedSessionSpec(env.cfg, env.cfg.EffectiveCityName(), "reviewer")
	if !ok {
		t.Fatal("configured named session fixture did not resolve")
	}
	bead := env.createSessionBead(spec.SessionName, "worker")
	env.setSessionMetadata(&bead, map[string]string{
		"alias":                      spec.Identity,
		"session_name":               spec.SessionName,
		"session_origin":             "named",
		namedSessionMetadataKey:      "true",
		namedSessionIdentityMetadata: spec.Identity,
		namedSessionModeMetadata:     spec.Mode,
		"continuity_eligible":        "true",
		"pin_awake":                  "true",
	})
	before := env.sessionInfo(bead.ID)
	cr := &CityRuntime{
		cityPath: "test-city", cityName: "test-city", cfg: env.cfg, sp: env.sp,
		cs:  coherentSessionStartControllerStateForTest(env.cfg, env.sp, env.store, rollout.Require),
		rec: events.Discard, stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{},
		sessionStartOptions: []startExecutionOption{
			withStartStabilityWaiter(immediateStartStabilityWaiter),
			withSessionStaleKeyDetectionWaiter(immediateSessionStaleKeyDetectionWaiter),
		},
	}
	if err := cr.ensureSessionStartController(t.Context(), newSessionBeadSnapshotFromInfos(nil)); err != nil {
		t.Fatalf("ensure session-start controller: %v", err)
	}
	t.Cleanup(cr.stopSessionStartController)
	if got := cr.admitSessionStartSocketKey(bead.ID); got != sessionStartSocketReplyOK {
		t.Fatalf("socket reply = %q, want keyed admission", got)
	}
	awaitCond(t, func() bool { return env.sessionInfo(bead.ID).MetadataState == string(session.StateActive) }, "configured named pinned session exact wake")
	after := env.sessionInfo(bead.ID)
	if after.ID != before.ID || after.SessionNameMetadata != before.SessionNameMetadata ||
		after.ConfiguredNamedIdentity != before.ConfiguredNamedIdentity || after.PinAwake != "true" {
		t.Fatalf("pinned configured named session = %+v, want same canonical pinned identity from %+v", after, before)
	}
	if got := env.sp.CountCalls("Start", spec.SessionName); got != 1 {
		t.Fatalf("provider Start calls = %d, want exactly 1", got)
	}
}

func TestExactSuspendedSessionStopsAndRetainsDurableRow(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker", StartCommand: "true"}}}
	provider := &unattendedStopProvider{Fake: env.sp}
	bead := env.createSessionBead("worker", "worker")
	env.setSessionMetadata(&bead, map[string]string{
		"state":          string(session.StateSuspended),
		"sleep_intent":   "user-hold",
		"held_until":     time.Now().UTC().Add(time.Hour).Format(time.RFC3339),
		"instance_token": "suspend-token",
	})
	if info := env.sessionInfo(bead.ID); !resolveExactSessionStartOrDrainAckStopOwnership(info, env.cfg, time.Now().UTC()) {
		t.Fatal("exact suspended session is not owned by keyed seed/legacy exclusion")
	}
	if err := provider.Start(t.Context(), bead.Metadata["session_name"], runtime.Config{}); err != nil {
		t.Fatalf("start runtime: %v", err)
	}
	if err := provider.SetMeta(bead.Metadata["session_name"], "GC_INSTANCE_TOKEN", "suspend-token"); err != nil {
		t.Fatalf("set runtime token: %v", err)
	}
	cr := &CityRuntime{
		cityPath: "test-city", cityName: "test-city", cfg: env.cfg, sp: provider,
		cs:  coherentSessionStartControllerStateForTest(env.cfg, provider, env.store, rollout.Require),
		rec: events.Discard, stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{},
	}
	if err := cr.ensureSessionStartController(t.Context(), newSessionBeadSnapshotFromInfos(nil)); err != nil {
		t.Fatalf("ensure session-start controller: %v", err)
	}
	t.Cleanup(cr.stopSessionStartController)
	if got := cr.admitSessionStartSocketKey(bead.ID); got != sessionStartSocketReplyOK {
		t.Fatalf("socket reply = %q, want keyed admission", got)
	}
	awaitCond(t, func() bool { return !provider.IsRunning(bead.Metadata["session_name"]) }, "exact suspended session stop")
	after := env.sessionInfo(bead.ID)
	if after.Closed || after.MetadataState != string(session.StateSuspended) || after.SleepIntent != "user-hold" || after.HeldUntil == "" {
		t.Fatalf("durable suspended row = %+v, want retained user hold", after)
	}
	stored, err := env.store.Get(bead.ID)
	if err != nil {
		t.Fatalf("read suspended row: %v", err)
	}
	if stored.Metadata["drain_ack_source"] != "" || stored.Metadata["state"] == "drain-ack-stop-pending" {
		t.Fatalf("durable suspended row entered drain-ack stop path: %+v", stored)
	}
	if calls := provider.stopSnapshot(); len(calls) != 1 || calls[0].name != bead.Metadata["session_name"] || calls[0].expectedToken != "suspend-token" {
		t.Fatalf("unattended stop calls = %#v, want exact one token-bound stop", calls)
	}
}

func TestExactSuspendedSessionWakeBeforeProviderEntryIsFenced(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker", StartCommand: "true"}}}
	bead := env.createSessionBead("worker", "worker")
	env.setSessionMetadata(&bead, map[string]string{
		"state": string(session.StateSuspended), "sleep_intent": "user-hold",
		"held_until": time.Now().UTC().Add(time.Hour).Format(time.RFC3339), "instance_token": "suspend-token",
	})
	base := &unattendedStopProvider{Fake: env.sp}
	provider := &wakeBeforeSuspendStopProvider{unattendedStopProvider: base, wake: func() {
		if _, err := session.NewStore(beads.SessionStore{Store: env.store}).WakeSession(bead.ID, time.Now().UTC(), session.WakeOpts{}); err != nil {
			t.Errorf("concurrent WakeSession: %v", err)
		}
	}}
	if err := provider.Start(t.Context(), bead.Metadata["session_name"], runtime.Config{}); err != nil {
		t.Fatalf("start runtime: %v", err)
	}
	if err := provider.SetMeta(bead.Metadata["session_name"], "GC_INSTANCE_TOKEN", "suspend-token"); err != nil {
		t.Fatalf("set runtime token: %v", err)
	}
	cr := &CityRuntime{
		cityPath: "test-city", cityName: "test-city", cfg: env.cfg, sp: provider,
		cs: coherentSessionStartControllerStateForTest(env.cfg, provider, env.store, rollout.Require), rec: events.Discard, stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{},
	}
	if err := cr.ensureSessionStartController(t.Context(), newSessionBeadSnapshotFromInfos(nil)); err != nil {
		t.Fatalf("ensure session-start controller: %v", err)
	}
	t.Cleanup(cr.stopSessionStartController)
	if got := cr.admitSessionStartSocketKey(bead.ID); got != sessionStartSocketReplyOK {
		t.Fatalf("socket reply = %q, want keyed admission", got)
	}
	awaitCond(t, func() bool { return env.sessionInfo(bead.ID).WakeRequest == string(session.WakeCauseExplicit) }, "concurrent public-equivalent wake")
	if calls := provider.stopSnapshot(); len(calls) != 0 {
		t.Fatalf("unattended stop calls = %#v, want zero after concurrent wake", calls)
	}
}

// resetSessionFixture persists the durable shape gc session reset leaves behind
// on one live ordinary session: an awake row with a running incarnation and the
// requested restart marker pair.
func resetSessionFixture(t *testing.T, env *reconcilerTestEnv, provider *unattendedStopProvider) beads.Bead {
	t.Helper()
	bead := env.createSessionBead("worker", "worker")
	env.setSessionMetadata(&bead, map[string]string{
		"state":                      string(session.StateActive),
		"instance_token":             "reset-token",
		"last_woke_at":               time.Now().UTC().Add(-time.Minute).Format(time.RFC3339),
		"started_config_hash":        "old-core-hash",
		"restart_requested":          "true",
		"continuation_reset_pending": "true",
	})
	if err := provider.Start(t.Context(), bead.Metadata["session_name"], runtime.Config{}); err != nil {
		t.Fatalf("start runtime: %v", err)
	}
	if err := provider.SetMeta(bead.Metadata["session_name"], "GC_INSTANCE_TOKEN", "reset-token"); err != nil {
		t.Fatalf("set runtime token: %v", err)
	}
	return bead
}

func TestExactResetSessionStopsAndRestartsSameCanonicalRow(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents:    []config.Agent{{Name: "worker", StartCommand: "true"}},
	}
	provider := &unattendedStopProvider{Fake: env.sp}
	bead := resetSessionFixture(t, env, provider)
	before := env.sessionInfo(bead.ID)
	if !resolveExactSessionStartOrDrainAckStopOwnership(before, env.cfg, time.Now().UTC()) {
		t.Fatal("reset-requested ordinary session is not owned by the keyed seed/legacy exclusion")
	}
	cr := &CityRuntime{
		cityPath: t.TempDir(), cityName: "test-city", cfg: env.cfg, sp: provider,
		cs:  coherentSessionStartControllerStateForTest(env.cfg, provider, env.store, rollout.Require),
		rec: events.Discard, stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{},
		sessionStartOptions: []startExecutionOption{
			withStartStabilityWaiter(immediateStartStabilityWaiter),
			withSessionStaleKeyDetectionWaiter(immediateSessionStaleKeyDetectionWaiter),
		},
	}
	if err := cr.ensureSessionStartController(t.Context(), newSessionBeadSnapshotFromInfos(nil)); err != nil {
		t.Fatalf("ensure session-start controller: %v", err)
	}
	t.Cleanup(cr.stopSessionStartController)
	if got := cr.admitSessionStartSocketKey(bead.ID); got != sessionStartSocketReplyOK {
		t.Fatalf("socket reply = %q, want keyed admission", got)
	}
	awaitCond(t, func() bool {
		current := env.sessionInfo(bead.ID)
		return current.MetadataState == string(session.StateActive) && current.InstanceToken != before.InstanceToken
	}, "exact reset restart")
	after := env.sessionInfo(bead.ID)
	if after.ID != before.ID || after.SessionNameMetadata != before.SessionNameMetadata {
		t.Fatalf("reset session = %+v, want the same bead and name as %+v", after, before)
	}
	if after.RestartRequested != "" || after.ContinuationResetPending != "" || after.ResetCommittedAt != "" {
		t.Fatalf("reset markers survived the restart: %+v", after)
	}
	if !provider.IsRunning(before.SessionNameMetadata) {
		t.Fatal("reset session has no live runtime after the restart")
	}
	if calls := provider.stopSnapshot(); len(calls) != 1 ||
		calls[0].name != before.SessionNameMetadata || calls[0].expectedToken != before.InstanceToken {
		t.Fatalf("unattended stop calls = %#v, want exactly one token-bound stop", calls)
	}
	if got := env.sp.CountCalls("Start", before.SessionNameMetadata); got != 2 {
		t.Fatalf("provider Start calls = %d, want the fixture start plus exactly one restart", got)
	}
	stored, err := env.store.Get(bead.ID)
	if err != nil {
		t.Fatalf("read reset row: %v", err)
	}
	if stored.Metadata["drain_ack_source"] != "" || stored.Metadata["state"] == "drain-ack-stop-pending" {
		t.Fatalf("reset row entered the drain-ack stop path: %+v", stored.Metadata)
	}
}

func TestExactResetSessionUnderLegacyDrainRetainsIntentWithoutStopping(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents:    []config.Agent{{Name: "worker", StartCommand: "true"}},
	}
	provider := &unattendedStopProvider{Fake: env.sp}
	bead := resetSessionFixture(t, env, provider)
	params := exactSessionStartTestParams(t, env)
	params.Provider = provider
	params.Generation = 1
	params.RolloutMode = rollout.Require
	params.DrainOps = newDrainOps(provider)
	params.DrainTracker = newDrainTracker()
	// The generation matches the row, so this is a CURRENT drain rather than a
	// stale one — the shape the reset must not restart through.
	params.DrainTracker.set(bead.ID, &drainState{
		reason: "user", startedAt: time.Now().UTC(),
		deadline: time.Now().UTC().Add(defaultDrainTimeout), generation: 1,
	})

	// WD.6 ownership semantics (ga-f7v2ft.112). A row carrying drain intent is
	// D-DRAIN's, and its seam case sits above every other family precisely
	// because each of those families' handlers already refused such a row. The
	// invariant this leg pins is unchanged — the reset never restarts through an
	// in-flight drain, no runtime is stopped, and the durable reset markers are
	// retained — but it is now delivered by the drain family OWNING the key
	// rather than by the reset arm parking on it. Before WD.6 the same row
	// produced an "active legacy drain" park error from the reset arm below the
	// seam; that arm is now unreachable for a row the drain family claims.
	owner, err := reconcileExactSessionStartWithOwner(t.Context(), sessionStartAdmission{
		SessionID: bead.ID, Source: sessionStartAdmissionSocket,
	}, params)
	if owner != exactSessionStartKeyedOwner || err != nil {
		t.Fatalf("legacy-drain reset result = (owner=%v, err=%v), want the drain family to own the key with no error", owner, err)
	}
	if state := params.DrainTracker.get(bead.ID); state == nil || state.reason != "user" {
		t.Fatalf("drain intent = %+v, want the in-flight drain retained rather than reset through", state)
	}
	if calls := provider.stopSnapshot(); len(calls) != 0 {
		t.Fatalf("unattended stop calls = %#v, want zero while the drain is in flight", calls)
	}
	after := env.sessionInfo(bead.ID)
	if after.RestartRequested != "true" || after.ContinuationResetPending != "true" || after.ResetCommittedAt != "" {
		t.Fatalf("durable reset intent = %+v, want the requested marker pair retained and uncommitted", after)
	}
	if got := env.sp.CountCalls("Start", after.SessionNameMetadata); got != 1 {
		t.Fatalf("provider Start calls = %d, want only the fixture start", got)
	}
}

// assertResetSessionRestarted asserts the whole reset ran in the pass just
// taken: the durable markers are consumed, exactly one token-bound stop fired
// against the incarnation the reset was admitted against, and the same bead and
// name came back up.
func assertResetSessionRestarted(t *testing.T, env *reconcilerTestEnv, provider *unattendedStopProvider, before session.Info) {
	t.Helper()
	after := env.sessionInfo(before.ID)
	if after.RestartRequested != "" || after.ContinuationResetPending != "" || after.ResetCommittedAt != "" {
		t.Fatalf("reset markers survived the pass: %+v", after)
	}
	if after.ID != before.ID || after.SessionNameMetadata != before.SessionNameMetadata {
		t.Fatalf("reset session = %+v, want the same bead and name as %+v", after, before)
	}
	if after.InstanceToken == before.InstanceToken {
		t.Fatalf("instance token = %q, want a fresh incarnation", after.InstanceToken)
	}
	if calls := provider.stopSnapshot(); len(calls) != 1 ||
		calls[0].name != before.SessionNameMetadata || calls[0].expectedToken != before.InstanceToken {
		t.Fatalf("unattended stop calls = %#v, want exactly one token-bound stop", calls)
	}
	if got := env.sp.CountCalls("Start", before.SessionNameMetadata); got != 2 {
		t.Fatalf("provider Start calls = %d, want the fixture start plus exactly one restart", got)
	}
	if !provider.IsRunning(before.SessionNameMetadata) {
		t.Fatal("reset session has no live runtime after the restart")
	}
}

// TestExactResetSessionRunsUnderAnyAdmissionSource pins ga-f7v2ft.139: the
// ordinary reset arm dispatches on the durable row, not on how the admission
// arrived. This is F2's rule (ga-f7v2ft.125) applied to the family that still
// carried the source gate. The controller coalesces admissions on a key and
// keeps ONE source — the earlier one when a pending in_process or an incoming
// anti_entropy folds, the later one otherwise (session_start_controller.go:435)
// — so any family that lands on a reset-carrying key before the reset arm sees
// it consumed the admission, and a source-gated arm then declines with no
// effect and nothing left to re-detect until the next anti-entropy pass.
//
// Each source below reaches the arm on the SAME durable row, and no detector
// family claims it: the seam's guards are row predicates, so the row that falls
// through under socket falls through under all of them.
func TestExactResetSessionRunsUnderAnyAdmissionSource(t *testing.T) {
	for _, source := range []sessionStartAdmissionSource{
		sessionStartAdmissionDeadline,
		sessionStartAdmissionInProcess,
		sessionStartAdmissionSleepDrain,
		sessionStartAdmissionExplicitWake,
		sessionStartAdmissionZombieMark,
	} {
		t.Run(string(source), func(t *testing.T) {
			env := newReconcilerTestEnv()
			env.cfg = &config.City{
				Workspace: config.Workspace{Name: "test-city"},
				Agents:    []config.Agent{{Name: "worker", StartCommand: "true"}},
			}
			provider := &unattendedStopProvider{Fake: env.sp}
			bead := resetSessionFixture(t, env, provider)
			before := env.sessionInfo(bead.ID)
			params := exactSessionStartTestParams(t, env)
			params.Provider = provider
			params.Generation = 1
			params.RolloutMode = rollout.Require

			owner, err := reconcileExactSessionStartWithOwner(t.Context(), sessionStartAdmission{
				SessionID: bead.ID, Source: source,
			}, params)
			if owner != exactSessionStartKeyedOwner || err != nil {
				t.Fatalf("reset under source %q = (owner=%v, err=%v), want the keyed owner to run it", source, owner, err)
			}
			assertResetSessionRestarted(t, env, provider, before)
		})
	}
}

// TestExactResetSessionUnderConfigDriftRunsTheResetNotTheConvergence pins the
// three-way interaction ga-f7v2ft.139 opens up: a row carrying BOTH the durable
// reset markers and real config drift, admitted under the drift family's own
// source. The pinned dispatch order resolves it in one direction only —
// reconcileExactSessionDetectorFamily runs first, D-DRIFT declines any row
// carrying restart_requested (ga-f7v2ft.138, legacy's own pass order), the key
// falls through to the reset arm, and de-source-gating that arm is what lets it
// act on the admission it was handed instead of deferring to anti-entropy.
//
// The restart is also what converges the drift: the fresh start re-stamps all
// four fingerprints, so the yield above costs no convergence.
func TestExactResetSessionUnderConfigDriftRunsTheResetNotTheConvergence(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents:    []config.Agent{{Name: "worker", StartCommand: "true"}},
	}
	provider := &unattendedStopProvider{Fake: env.sp}
	bead := resetSessionFixture(t, env, provider)
	params := exactSessionStartTestParams(t, env)
	params.Provider = provider
	params.Generation = 1
	params.RolloutMode = rollout.Require

	// Real provision drift, resolved through the same production path the drift
	// family compares against, so the baseline is one the handler can actually
	// see rather than a hand-built stub.
	stale := driftAgentConfig(t, env, params, bead.ID)
	stale.PreStart = append([]string{"echo stale"}, stale.PreStart...)
	env.setSessionMetadata(&bead, map[string]string{
		"started_config_hash":    runtime.CoreFingerprint(stale),
		"started_provision_hash": runtime.ProvisionFingerprint(stale),
		"started_launch_hash":    runtime.LaunchFingerprint(stale),
	})
	before := env.sessionInfo(bead.ID)
	if strings.TrimSpace(before.RestartRequested) != "true" {
		t.Fatalf("fixture lost the reset marker: %+v", before)
	}
	// The drift is REAL: strip the marker the .138 yield keys on and D-DRIFT
	// claims this row. Without this leg the test would pass on a fixture that
	// simply carries no drift at all.
	undriftedProbe := before
	undriftedProbe.RestartRequested = ""
	if !exactSessionConfigDriftCandidate(params, undriftedProbe, session.PersistedResponse{Revision: 1}, env.clk) {
		t.Fatal("fixture is not the three-way shape: the seeded baseline is not drift D-DRIFT would claim")
	}
	if exactSessionConfigDriftCandidate(params, before, session.PersistedResponse{Revision: 1}, env.clk) {
		t.Fatal("D-DRIFT claimed a restart-requested row; the ga-f7v2ft.138 yield regressed")
	}

	owner, err := reconcileExactSessionStartWithOwner(t.Context(), sessionStartAdmission{
		SessionID: bead.ID, Source: sessionStartAdmissionConfigDrift,
	}, params)
	if owner != exactSessionStartKeyedOwner || err != nil {
		t.Fatalf("drifted reset result = (owner=%v, err=%v), want the keyed owner to run the reset", owner, err)
	}
	assertResetSessionRestarted(t, env, provider, before)
	after := env.sessionInfo(bead.ID)
	if after.StartedConfigHash == before.StartedConfigHash {
		t.Fatalf("started_config_hash = %q, want the restart to have re-stamped the drifted baseline", after.StartedConfigHash)
	}
}

// resetHandoffRaceStore lands one concurrent writer in the window between the
// reset handoff's authority re-read and its own write. The handoff's Get on the
// row IS that re-read (with the runtime already dead the stop block never runs,
// so the first Get inside the body is the authority read), and the interloper
// fires the moment it returns.
//
// It declares its conditional-writes resolution target so the fenced write
// resolves through to the stamped store exactly as it does in production; a
// wrapper that declared nothing would collapse resolution to unset->legacy and
// the test would prove only that its own plumbing suppressed the fence.
// sawRawWrite records the unconditional spelling reaching the store after the
// race, which is the regression itself.
type resetHandoffRaceStore struct {
	beads.Store
	target      string
	interloper  map[string]string
	reads       int
	fired       bool
	sawRawWrite bool
}

func (s *resetHandoffRaceStore) ConditionalWritesResolveTarget() beads.Store { return s.Store }

func (s *resetHandoffRaceStore) Get(id string) (beads.Bead, error) {
	bead, err := s.Store.Get(id)
	if err != nil || id != s.target {
		return bead, err
	}
	s.reads++
	if s.reads == 1 && !s.fired {
		s.fired = true
		if raceErr := s.Store.SetMetadataBatch(id, s.interloper); raceErr != nil {
			return bead, fmt.Errorf("seeding the concurrent reset-handoff writer: %w", raceErr)
		}
	}
	return bead, nil
}

func (s *resetHandoffRaceStore) SetMetadataBatch(id string, metadata map[string]string) error {
	if _, handoff := metadata[session.ResetCommittedAtKey]; handoff && id == s.target {
		s.sawRawWrite = true
	}
	return s.Store.SetMetadataBatch(id, metadata)
}

// TestExactResetHandoffRefusesAWriterThatRacedTheAuthorityRead pins the
// ga-l1j53 P1 fence on commitExactSessionResetHandoff's check-then-write
// (council F14). The handoff re-reads the row, verifies the reset authority
// still holds, and then writes RestartRequestPatch; unfenced, anything that
// lands in that window is silently overwritten and a fresh-conversation restart
// commits on top of a row whose identity has already moved -- the same class of
// split-brain ga-l1j53 proved for the pre-wake commit.
//
// A lost fence must be a REFUSAL with zero effect: the durable reset markers
// are retained, so the next admission re-derives the handoff from whatever the
// winner left.
func TestExactResetHandoffRefusesAWriterThatRacedTheAuthorityRead(t *testing.T) {
	env := newReconcilerTestEnv()
	// The fence only exists on a store stamped with conditional writes, so the
	// fixture opens one through the real factory rather than the bare memstore.
	env.store = newPreWakeFenceStore(t, gate.Require)
	env.cfg = &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents:    []config.Agent{{Name: "worker", StartCommand: "true"}},
	}
	provider := &unattendedStopProvider{Fake: env.sp}
	bead := resetSessionFixture(t, env, provider)
	// The runtime is already dead, so the handoff skips the stop block entirely
	// and its first Get on the row is the authority re-read the fence protects.
	if err := provider.Stop(bead.Metadata["session_name"]); err != nil {
		t.Fatalf("stop fixture runtime: %v", err)
	}
	before := env.sessionInfo(bead.ID)
	if writer, _, err := beads.ResolveConditionalWriter(env.store); err != nil || writer == nil {
		t.Fatalf("test store resolved no conditional writer (writer=%v, err=%v); there is no fence to prove", writer, err)
	}

	const racedToken = "raced-token"
	if before.InstanceToken == racedToken {
		t.Fatal("fixture seeded the raced token; the assertions below would prove nothing")
	}
	race := &resetHandoffRaceStore{
		Store:  env.store,
		target: bead.ID,
		// The shape that makes an overwrite unsafe: another writer rotated the
		// incarnation this reset was admitted against.
		interloper: map[string]string{"instance_token": racedToken, "generation": "9"},
	}
	params := exactSessionStartTestParams(t, env)
	params.Provider = provider
	params.Generation = 1
	params.RolloutMode = rollout.Require
	params.Store = race

	tp, _, err := resolveExactSessionStartTemplate(params, before, &env.cfg.Agents[0], env.clk, io.Discard)
	if err != nil {
		t.Fatalf("resolve reset template: %v", err)
	}
	info, initial, err := getAuthoritativeSessionStartPersistedRecord(env.store, bead.ID)
	if err != nil {
		t.Fatalf("read reset row: %v", err)
	}
	_, response, err := commitExactOrdinaryResetHandoff(params, info, initial, tp, env.clk, io.Discard)
	if !race.fired {
		t.Fatal("the concurrent writer never raced the handoff; the fixture proves nothing")
	}
	if race.sawRawWrite {
		t.Fatal("the reset handoff wrote unconditionally after the race; the ga-l1j53 fence is not applied")
	}
	if err == nil {
		t.Fatalf("raced reset handoff committed (response=%+v), want a refusal", response)
	}
	after := env.sessionInfo(bead.ID)
	if after.InstanceToken != racedToken {
		t.Fatalf("instance token = %q, want the concurrent writer's %q preserved", after.InstanceToken, racedToken)
	}
	if after.ResetCommittedAt != "" {
		t.Fatalf("reset_committed_at = %q, want no handoff committed on top of the race", after.ResetCommittedAt)
	}
	if after.RestartRequested != "true" || after.ContinuationResetPending != "true" {
		t.Fatalf("durable reset intent = %+v, want the requested marker pair retained for the next admission", after)
	}
}

// TestExactResetHandoffCommitsWhenNothingRacesIt pins that the fence is
// invisible to the ordinary single-writer reset: the handoff on the revision
// its own authority re-read carried lands and consumes the requested marker.
func TestExactResetHandoffCommitsWhenNothingRacesIt(t *testing.T) {
	env := newReconcilerTestEnv()
	env.store = newPreWakeFenceStore(t, gate.Require)
	env.cfg = &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents:    []config.Agent{{Name: "worker", StartCommand: "true"}},
	}
	provider := &unattendedStopProvider{Fake: env.sp}
	bead := resetSessionFixture(t, env, provider)
	if err := provider.Stop(bead.Metadata["session_name"]); err != nil {
		t.Fatalf("stop fixture runtime: %v", err)
	}
	params := exactSessionStartTestParams(t, env)
	params.Provider = provider
	params.Generation = 1
	params.RolloutMode = rollout.Require

	info, initial, err := getAuthoritativeSessionStartPersistedRecord(env.store, bead.ID)
	if err != nil {
		t.Fatalf("read reset row: %v", err)
	}
	tp, _, err := resolveExactSessionStartTemplate(params, info, &env.cfg.Agents[0], env.clk, io.Discard)
	if err != nil {
		t.Fatalf("resolve reset template: %v", err)
	}
	committed, response, err := commitExactOrdinaryResetHandoff(params, info, initial, tp, env.clk, io.Discard)
	if err != nil {
		t.Fatalf("unraced reset handoff: %v", err)
	}
	if response.Revision == 0 || response.Revision == initial.Revision {
		t.Fatalf("committed revision = %d, want a moved revision (was %d)", response.Revision, initial.Revision)
	}
	if !exactOrdinaryResetCommitted(committed) {
		t.Fatalf("committed row = %+v, want the requested marker consumed and the handoff durable", committed)
	}
}

// seedResetRowUnderIncompleteScan is the ga-bxa8r fixture for the reset arm: the
// ordinary reset row, observed through a provider whose alive-target
// completeness is structurally unlicensable.
func seedResetRowUnderIncompleteScan(
	t *testing.T,
	env *reconcilerTestEnv,
) (*aliveIncompleteStopProvider, beads.Bead, exactSessionStartParams, TemplateParams) {
	t.Helper()
	env.cfg = &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents:    []config.Agent{{Name: "worker", StartCommand: "true"}},
	}
	provider := &aliveIncompleteStopProvider{unattendedStopProvider: &unattendedStopProvider{Fake: env.sp}}
	bead := resetSessionFixture(t, env, provider.unattendedStopProvider)
	params := exactSessionStartTestParams(t, env)
	params.Provider = provider
	params.Generation = 1
	params.RolloutMode = rollout.Require
	tp, _, err := resolveExactSessionStartTemplate(params, env.sessionInfo(bead.ID), &env.cfg.Agents[0], env.clk, io.Discard)
	if err != nil {
		t.Fatalf("resolve reset template: %v", err)
	}
	return provider, bead, params, tp
}

// TestExactResetHandoffRecyclesAliveSessionOnIncompleteScan is ga-bxa8r's reset
// specimen. The recycle's stop is destructive BY INTENT — a reset exists to kill
// the live incarnation so the restart runs a fresh conversation — so demanding
// scan completeness before it asked for a proof a live target can never supply,
// and on a busy host `gc session reset` never recycled anything.
//
// The negative arm keeps the demand, and this is the arm where completeness
// genuinely earns it: skipping the stop on an UNPROVEN absence would commit the
// restart handoff while the old incarnation may still be alive, and the start
// that follows would put a second incarnation on the same name.
func TestExactResetHandoffRecyclesAliveSessionOnIncompleteScan(t *testing.T) {
	env := newReconcilerTestEnv()
	provider, bead, params, tp := seedResetRowUnderIncompleteScan(t, env)
	name := env.sessionInfo(bead.ID).SessionNameMetadata
	if !provider.IsRunning(name) {
		t.Fatal("the fixture's runtime is not alive, so nothing here proves the alive arm was withheld")
	}
	info, initial, err := getAuthoritativeSessionStartPersistedRecord(env.store, bead.ID)
	if err != nil {
		t.Fatalf("read reset row: %v", err)
	}

	committed, _, err := commitExactOrdinaryResetHandoff(params, info, initial, tp, env.clk, io.Discard)
	if err != nil {
		t.Fatalf("an alive session's incomplete scan parked the reset recycle: %v", err)
	}
	if provider.IsRunning(name) {
		t.Fatal("the reset committed without killing the live incarnation")
	}
	if calls := provider.stopSnapshot(); len(calls) != 1 || calls[0].expectedToken != "reset-token" {
		t.Fatalf("unattended stop calls = %#v, want exactly one token-bound stop", calls)
	}
	if !exactOrdinaryResetCommitted(committed) {
		t.Fatalf("committed row = %+v, want the requested marker consumed and the handoff durable", committed)
	}
}

// TestExactResetHandoffUnprovenAbsenceStillRefuses is the fail-closed control,
// and it fails DIFFERENTLY: same fixture with the runtime gone and the scan
// still unlicensable. Dead cannot be told apart from unobserved, and here that
// distinction is load-bearing — the handoff below the observation is what
// licenses a fresh incarnation — so the reset must refuse with the reset markers
// retained for the next admission rather than commit beside a runtime it never
// proved gone.
func TestExactResetHandoffUnprovenAbsenceStillRefuses(t *testing.T) {
	env := newReconcilerTestEnv()
	provider, bead, params, tp := seedResetRowUnderIncompleteScan(t, env)
	provider.alwaysIncomplete = true
	name := env.sessionInfo(bead.ID).SessionNameMetadata
	if err := provider.Stop(name); err != nil {
		t.Fatalf("stop fixture runtime: %v", err)
	}
	info, initial, err := getAuthoritativeSessionStartPersistedRecord(env.store, bead.ID)
	if err != nil {
		t.Fatalf("read reset row: %v", err)
	}

	_, _, err = commitExactOrdinaryResetHandoff(params, info, initial, tp, env.clk, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "liveness observation is incomplete") {
		t.Fatalf("err = %v, want the incomplete-liveness refusal for a negative unproven observation", err)
	}
	after := env.sessionInfo(bead.ID)
	if after.ResetCommittedAt != "" {
		t.Fatalf("reset_committed_at = %q, want no handoff committed behind an unproven absence", after.ResetCommittedAt)
	}
	if after.RestartRequested != "true" || after.ContinuationResetPending != "true" {
		t.Fatalf("durable reset intent = %+v, want the requested marker pair retained for the next admission", after)
	}
}

// killedPinnedOnDemandFixture persists the durable shape gc session kill leaves
// behind on a live pinned on-demand configured named session: asleep with a
// killed reason, the pin retained, and no synthesized wake request.
func killedPinnedOnDemandFixture(t *testing.T, env *reconcilerTestEnv) (session.NamedSessionSpec, beads.Bead) {
	t.Helper()
	spec, ok := findNamedSessionSpec(env.cfg, env.cfg.EffectiveCityName(), "reviewer")
	if !ok {
		t.Fatal("configured named session fixture did not resolve")
	}
	bead := env.createSessionBead(spec.SessionName, "worker")
	env.setSessionMetadata(&bead, map[string]string{
		"alias":                      spec.Identity,
		"session_name":               spec.SessionName,
		"session_origin":             "named",
		namedSessionMetadataKey:      "true",
		namedSessionIdentityMetadata: spec.Identity,
		namedSessionModeMetadata:     spec.Mode,
		"continuity_eligible":        "true",
		"pin_awake":                  "true",
		"state":                      string(session.StateAsleep),
		"sleep_reason":               "killed",
		"instance_token":             "killed-token",
		"slept_at":                   time.Now().UTC().Format(time.RFC3339),
	})
	return spec, bead
}

func TestExactKilledPinnedOnDemandSessionRestartsSameCanonicalSession(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Workspace:     config.Workspace{Name: "test-city"},
		Agents:        []config.Agent{{Name: "worker", StartCommand: "true"}},
		NamedSessions: []config.NamedSession{{Name: "reviewer", Template: "worker", Mode: "on_demand"}},
	}
	spec, bead := killedPinnedOnDemandFixture(t, env)
	before := env.sessionInfo(bead.ID)
	cr := &CityRuntime{
		cityPath: "test-city", cityName: "test-city", cfg: env.cfg, sp: env.sp,
		cs:  coherentSessionStartControllerStateForTest(env.cfg, env.sp, env.store, rollout.Require),
		rec: events.Discard, stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{},
		sessionStartOptions: []startExecutionOption{
			withStartStabilityWaiter(immediateStartStabilityWaiter),
			withSessionStaleKeyDetectionWaiter(immediateSessionStaleKeyDetectionWaiter),
		},
	}
	if err := cr.ensureSessionStartController(t.Context(), newSessionBeadSnapshotFromInfos(nil)); err != nil {
		t.Fatalf("ensure session-start controller: %v", err)
	}
	t.Cleanup(cr.stopSessionStartController)
	if got := cr.admitSessionStartSocketKey(bead.ID); got != sessionStartSocketReplyOK {
		t.Fatalf("socket reply = %q, want keyed admission", got)
	}
	awaitCond(t, func() bool { return env.sessionInfo(bead.ID).MetadataState == string(session.StateActive) }, "killed pinned on-demand exact restart")
	after := env.sessionInfo(bead.ID)
	if after.ID != before.ID || after.SessionNameMetadata != before.SessionNameMetadata ||
		after.ConfiguredNamedIdentity != before.ConfiguredNamedIdentity || after.PinAwake != "true" {
		t.Fatalf("restarted killed pinned session = %+v, want the same canonical pinned identity from %+v", after, before)
	}
	if strings.TrimSpace(after.InstanceToken) == "" || after.InstanceToken == before.InstanceToken {
		t.Fatalf("restarted instance token = %q, want a new incarnation distinct from %q", after.InstanceToken, before.InstanceToken)
	}
	if after.WakeRequest != "" {
		t.Fatalf("durable wake request = %q, want the pin to remain the sole wake authority", after.WakeRequest)
	}
	if got := env.sp.CountCalls("Start", spec.SessionName); got != 1 {
		t.Fatalf("provider Start calls = %d, want exactly 1", got)
	}
}

func TestExactKilledPinnedOnDemandSessionUnpinBeforeProviderEntryIsFenced(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Workspace:     config.Workspace{Name: "test-city"},
		Agents:        []config.Agent{{Name: "worker", StartCommand: "true"}},
		NamedSessions: []config.NamedSession{{Name: "reviewer", Template: "worker", Mode: "on_demand"}},
	}
	spec, bead := killedPinnedOnDemandFixture(t, env)
	info, revision, err := getAuthoritativeSessionStartRecord(env.store, bead.ID)
	if err != nil {
		t.Fatalf("read killed pinned session: %v", err)
	}
	lease, certified := certifyConfiguredNamedWakeStartLease(info, revision, env.cfg, "test-city", 1, env.clk.Now().UTC())
	if !certified || lease.Cause != session.WakeCausePinned {
		t.Fatalf("killed pinned lease = %+v, certified=%t; want pin cause", lease, certified)
	}
	params := exactSessionStartTestParams(t, env)
	params.Generation = 1
	params.RolloutMode = rollout.Require
	params.ValidateConfiguredNamedWakeStart = func(session.Info, configuredNamedWakeStartLease) bool { return true }
	params.EnterConfiguredNamedWakeStart = func(configuredNamedWakeStartLease) bool { return true }
	params.StartOptions = append(params.StartOptions, withTaskWorkDirResolver(func(startCandidate, *config.City) string {
		if err := env.store.SetMetadata(bead.ID, "pin_awake", ""); err != nil {
			t.Errorf("unpin before provider entry: %v", err)
		}
		return ""
	}))
	owner, err := reconcileExactSessionStartWithOwner(t.Context(), sessionStartAdmission{
		SessionID: bead.ID, Source: sessionStartAdmissionSocket, ConfiguredNamedWake: &lease,
	}, params)
	if owner != exactSessionStartKeyedOwner || err == nil {
		t.Fatalf("unpin-before-provider result = (owner=%v, err=%v), want keyed fence error", owner, err)
	}
	if got := env.sp.CountCalls("Start", spec.SessionName); got != 0 || env.sp.IsRunning(spec.SessionName) {
		t.Fatalf("provider Start calls = %d running=%t, want no replacement runtime after unpin", got, env.sp.IsRunning(spec.SessionName))
	}
	// The shared pre-wake commit legitimately advances the row before the
	// provider-entry fence, so "remains asleep" is proven as "never reaches a
	// started incarnation": no active state and no wake timestamps.
	after := env.sessionInfo(bead.ID)
	if after.Closed || after.MetadataState == string(session.StateActive) ||
		strings.TrimSpace(after.LastWokeAt) != "" || strings.TrimSpace(after.AwakeStartedAt) != "" {
		t.Fatalf("fenced killed session = %+v, want an open row with no started incarnation", after)
	}
}

func TestConfiguredNamedPinnedSessionUnpinBeforeProviderEntryIsFenced(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Workspace:     config.Workspace{Name: "test-city"},
		Agents:        []config.Agent{{Name: "worker", StartCommand: "true"}},
		NamedSessions: []config.NamedSession{{Name: "reviewer", Template: "worker", Mode: "on_demand"}},
	}
	spec, ok := findNamedSessionSpec(env.cfg, env.cfg.EffectiveCityName(), "reviewer")
	if !ok {
		t.Fatal("configured named session fixture did not resolve")
	}
	bead := env.createSessionBead(spec.SessionName, "worker")
	env.setSessionMetadata(&bead, map[string]string{
		"alias":                      spec.Identity,
		"session_name":               spec.SessionName,
		"session_origin":             "named",
		namedSessionMetadataKey:      "true",
		namedSessionIdentityMetadata: spec.Identity,
		namedSessionModeMetadata:     spec.Mode,
		"continuity_eligible":        "true",
		"pin_awake":                  "true",
	})
	info, revision, err := getAuthoritativeSessionStartRecord(env.store, bead.ID)
	if err != nil {
		t.Fatalf("read pinned configured named session: %v", err)
	}
	lease, certified := certifyConfiguredNamedWakeStartLease(info, revision, env.cfg, "test-city", 1, env.clk.Now().UTC())
	if !certified || lease.Cause != session.WakeCausePinned {
		t.Fatalf("pinned configured named lease = %+v, certified=%t; want pin cause", lease, certified)
	}
	params := exactSessionStartTestParams(t, env)
	params.Generation = 1
	params.RolloutMode = rollout.Require
	params.ValidateConfiguredNamedWakeStart = func(session.Info, configuredNamedWakeStartLease) bool { return true }
	params.EnterConfiguredNamedWakeStart = func(configuredNamedWakeStartLease) bool { return true }
	params.StartOptions = append(params.StartOptions, withTaskWorkDirResolver(func(startCandidate, *config.City) string {
		if err := env.store.SetMetadata(bead.ID, "pin_awake", ""); err != nil {
			t.Errorf("unpin before provider entry: %v", err)
		}
		return ""
	}))
	owner, err := reconcileExactSessionStartWithOwner(t.Context(), sessionStartAdmission{
		SessionID: bead.ID, Source: sessionStartAdmissionSocket, ConfiguredNamedWake: &lease,
	}, params)
	if owner != exactSessionStartKeyedOwner || err == nil {
		t.Fatalf("unpin-before-provider result = (owner=%v, err=%v), want keyed fence error", owner, err)
	}
	if got := env.sp.CountCalls("Start", spec.SessionName); got != 0 {
		t.Fatalf("provider Start calls = %d, want 0 after unpin", got)
	}
}

func TestExactWakeLeasesAcceptNegativeNonzeroRevisionTokens(t *testing.T) {
	configured := configuredNamedWakeStartLease{
		SessionID: "gcs-named", SessionName: "worker", InstanceToken: "instance-1",
		SessionRevision: -17, Identity: "worker", Mode: "always", Template: "worker",
		Cause: session.WakeCauseExplicit, ControllerGeneration: 1,
	}
	if err := validateConfiguredNamedWakeStartLease(configured); err != nil {
		t.Fatalf("configured named wake negative revision: %v", err)
	}

	pool := strictDefaultPoolWakeStartLease{
		SessionID: "gcs-pool", SessionName: "worker-1", InstanceToken: "instance-2",
		SessionRevision: -17, PoolTarget: "worker", PoolSlot: "1", TriggerBeadID: "ga-work",
		TriggerBeadStoreRef: "city", ControllerGeneration: 1,
	}
	if err := validateStrictDefaultPoolWakeStartLease(pool); err != nil {
		t.Fatalf("strict-default pool wake negative revision: %v", err)
	}
}

func testConfiguredNamedSessionWakeStartsSameCanonicalSession(t *testing.T, mode rollout.Mode) {
	t.Helper()
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Workspace:     config.Workspace{Name: "test-city"},
		Agents:        []config.Agent{{Name: "worker", StartCommand: "true"}},
		NamedSessions: []config.NamedSession{{Name: "reviewer", Template: "worker", Mode: "on_demand"}},
	}
	spec, ok := findNamedSessionSpec(env.cfg, env.cfg.EffectiveCityName(), "reviewer")
	if !ok {
		t.Fatal("configured named session fixture did not resolve")
	}
	bead := env.createSessionBead(spec.SessionName, "worker")
	env.setSessionMetadata(&bead, map[string]string{
		"alias":                      spec.Identity,
		"session_name":               spec.SessionName,
		"session_origin":             "named",
		namedSessionMetadataKey:      "true",
		namedSessionIdentityMetadata: spec.Identity,
		namedSessionModeMetadata:     spec.Mode,
		"continuity_eligible":        "true",
	})
	if err := env.store.SetMetadataBatch(bead.ID, session.RequestExplicitWakePatch(string(session.WakeCauseExplicit), env.clk.Now())); err != nil {
		t.Fatalf("request explicit wake: %v", err)
	}
	before := env.sessionInfo(bead.ID)
	cr := &CityRuntime{
		cityPath: "test-city", cityName: "test-city", cfg: env.cfg, sp: env.sp,
		cs:  coherentSessionStartControllerStateForTest(env.cfg, env.sp, env.store, mode),
		rec: events.Discard, stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{},
		sessionStartOptions: []startExecutionOption{
			withStartStabilityWaiter(immediateStartStabilityWaiter),
			withSessionStaleKeyDetectionWaiter(immediateSessionStaleKeyDetectionWaiter),
		},
	}
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	if err := cr.ensureSessionStartController(ctx, newSessionBeadSnapshotFromInfos(nil)); err != nil {
		t.Fatalf("ensure session-start controller: %v", err)
	}
	t.Cleanup(cr.stopSessionStartController)
	if got := cr.admitSessionStartSocketKey(bead.ID); got != sessionStartSocketReplyOK {
		t.Fatalf("socket reply = %q, want keyed admission", got)
	}
	awaitCond(t, func() bool {
		return env.sessionInfo(bead.ID).MetadataState == string(session.StateActive) || cr.sessionStartController.Pending() == 0
	}, "configured named session exact wake")
	after := env.sessionInfo(bead.ID)
	if after.MetadataState != string(session.StateActive) || after.ID != before.ID ||
		after.SessionNameMetadata != before.SessionNameMetadata || after.ConfiguredNamedIdentity != before.ConfiguredNamedIdentity ||
		after.ConfiguredNamedMode != before.ConfiguredNamedMode || after.Template != before.Template {
		t.Fatalf("woken configured named session = %+v, want same canonical identity active from %+v", after, before)
	}
	if got := env.sp.CountCalls("Start", spec.SessionName); got != 1 {
		t.Fatalf("provider Start calls = %d, want exactly 1", got)
	}
}

func TestConfiguredNamedSessionWakeLeavesStaleIdentityToLegacy(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Workspace:     config.Workspace{Name: "test-city"},
		Agents:        []config.Agent{{Name: "worker", StartCommand: "true"}},
		NamedSessions: []config.NamedSession{{Name: "reviewer", Template: "worker", Mode: "on_demand"}},
	}
	spec, ok := findNamedSessionSpec(env.cfg, env.cfg.EffectiveCityName(), "reviewer")
	if !ok {
		t.Fatal("configured named session fixture did not resolve")
	}
	bead := env.createSessionBead(spec.SessionName+"-stale", "worker")
	env.setSessionMetadata(&bead, map[string]string{
		"session_origin":             "named",
		namedSessionMetadataKey:      "true",
		namedSessionIdentityMetadata: spec.Identity,
		namedSessionModeMetadata:     spec.Mode,
	})
	if err := env.store.SetMetadataBatch(bead.ID, session.RequestExplicitWakePatch(string(session.WakeCauseExplicit), env.clk.Now())); err != nil {
		t.Fatalf("request explicit wake: %v", err)
	}
	controller, err := newSessionStartController(sessionStartControllerOptions{
		Workers: 1, MaxDistinct: 4, MaxRetries: 0,
		Reconcile: func(context.Context, sessionStartAdmission) error {
			t.Fatal("stale configured named identity reached keyed reconciliation")
			return nil
		},
	})
	if err != nil {
		t.Fatalf("new session-start controller: %v", err)
	}
	if err := controller.Start(t.Context()); err != nil {
		t.Fatalf("start session-start controller: %v", err)
	}
	t.Cleanup(controller.Stop)
	cr := &CityRuntime{
		cfg: env.cfg, sp: env.sp,
		cs:                     coherentSessionStartControllerStateForTest(env.cfg, env.sp, env.store, rollout.Auto),
		sessionStartController: controller,
		sessionStartOwnership:  sessionStartOwnershipKeyed,
		stderr:                 &bytes.Buffer{},
	}
	if got := cr.admitSessionStartSocketKey(bead.ID); got != sessionStartSocketReplyFallback {
		t.Fatalf("socket reply = %q, want legacy fallback for stale configured identity", got)
	}
	if got := env.sp.CountCalls("Start", spec.SessionName+"-stale"); got != 0 {
		t.Fatalf("provider Start calls = %d, want 0", got)
	}
}

func TestReconcileConfiguredNamedSessionWakeFencesDrift(t *testing.T) {
	type fixture struct {
		env    *reconcilerTestEnv
		bead   beads.Bead
		lease  configuredNamedWakeStartLease
		before session.Info
	}
	newFixture := func(t *testing.T) fixture {
		t.Helper()
		env := newReconcilerTestEnv()
		env.cfg = &config.City{
			Workspace:     config.Workspace{Name: "test-city"},
			Agents:        []config.Agent{{Name: "worker", StartCommand: "true"}},
			NamedSessions: []config.NamedSession{{Name: "reviewer", Template: "worker", Mode: "on_demand"}},
		}
		spec, ok := findNamedSessionSpec(env.cfg, env.cfg.EffectiveCityName(), "reviewer")
		if !ok {
			t.Fatal("configured named session fixture did not resolve")
		}
		bead := env.createSessionBead(spec.SessionName, "worker")
		env.setSessionMetadata(&bead, map[string]string{
			"alias":                      spec.Identity,
			"session_origin":             "named",
			namedSessionMetadataKey:      "true",
			namedSessionIdentityMetadata: spec.Identity,
			namedSessionModeMetadata:     spec.Mode,
			"continuity_eligible":        "true",
		})
		if err := env.store.SetMetadataBatch(bead.ID, session.RequestExplicitWakePatch(string(session.WakeCauseExplicit), env.clk.Now())); err != nil {
			t.Fatalf("request explicit wake: %v", err)
		}
		info, revision, err := getAuthoritativeSessionStartRecord(env.store, bead.ID)
		if err != nil {
			t.Fatalf("read configured named session: %v", err)
		}
		lease, certified := certifyConfiguredNamedWakeStartLease(info, revision, env.cfg, "test-city", 1, env.clk.Now().UTC())
		if !certified {
			t.Fatal("canonical configured named session was not certified")
		}
		return fixture{env: env, bead: bead, lease: lease, before: info}
	}

	for _, mode := range []rollout.Mode{rollout.Auto, rollout.Require} {
		for _, phase := range []struct {
			name         string
			validThrough int32
			preMutation  bool
		}{
			{name: "pre-wake", validThrough: 1, preMutation: true},
			{name: "provider-entry", validThrough: 2},
		} {
			t.Run(phase.name+"/"+string(mode), func(t *testing.T) {
				f := newFixture(t)
				params := exactSessionStartTestParams(t, f.env)
				params.Generation = 1
				params.RolloutMode = mode
				var validations atomic.Int32
				params.ValidateConfiguredNamedWakeStart = func(session.Info, configuredNamedWakeStartLease) bool {
					return validations.Add(1) <= phase.validThrough
				}
				var entered atomic.Bool
				params.EnterConfiguredNamedWakeStart = func(configuredNamedWakeStartLease) bool {
					entered.Store(true)
					return true
				}

				owner, err := reconcileExactSessionStartWithOwner(t.Context(), sessionStartAdmission{
					SessionID: f.bead.ID, Source: sessionStartAdmissionSocket, ConfiguredNamedWake: &f.lease,
				}, params)
				if phase.preMutation && mode == rollout.Auto {
					if owner != exactSessionStartLegacyOwner || err != nil {
						t.Fatalf("Auto pre-wake drift result = (owner=%v, err=%v), want clean legacy yield", owner, err)
					}
				} else if owner != exactSessionStartKeyedOwner || err == nil {
					t.Fatalf("parked drift result = (owner=%v, err=%v), want keyed error", owner, err)
				}
				if got := f.env.sp.CountCalls("Start", f.lease.SessionName); got != 0 {
					t.Fatalf("provider Start calls = %d, want 0 after witness drift", got)
				}
				if entered.Load() == phase.preMutation {
					t.Fatalf("entered after %s drift = %t, want %t", phase.name, entered.Load(), !phase.preMutation)
				}
				if phase.preMutation {
					after := f.env.sessionInfo(f.bead.ID)
					if after.InstanceToken != f.before.InstanceToken || after.MetadataState != f.before.MetadataState || after.WakeRequest != f.before.WakeRequest {
						t.Fatalf("pre-wake drift mutated configured named session: before=%+v after=%+v", f.before, after)
					}
				}
			})
		}
	}
}

// Lease-mechanics pin: the bare origin-less target below is a pre-sync shape production does not sustain, so this asserts exactly-once entry, not reachability (tracked by ga-ij8mh; re-anchored on the canonical singleton shape in WD.10a).
func TestReconcileConfiguredDependencyWakeStartsTargetOnceWithoutChangingDependency(t *testing.T) {
	for _, mode := range []rollout.Mode{rollout.Auto, rollout.Require} {
		t.Run(string(mode), func(t *testing.T) {
			env := newReconcilerTestEnv()
			env.cfg = &config.City{
				Workspace: config.Workspace{Name: "test-city"},
				Agents: []config.Agent{
					{Name: "database", StartCommand: "true", MaxActiveSessions: intPtr(1)},
					{Name: "worker", StartCommand: "true", DependsOn: []string{"database"}},
				},
			}
			dependency := env.createSessionBead("database", "database")
			env.markSessionActive(&dependency)
			env.addDesired("database", "database", true)
			dependencyBefore := env.sessionInfo(dependency.ID)
			target := env.createSessionBead("worker", "worker")
			if err := env.store.SetMetadataBatch(target.ID, session.RequestExplicitWakePatch(string(session.WakeCauseExplicit), env.clk.Now())); err != nil {
				t.Fatalf("request explicit wake: %v", err)
			}
			lease, certified := certifyConfiguredDependencyStartLease(
				env.sessionInfo(target.ID), env.cfg, env.sp, "test-city", env.store, 1, env.clk.Now().UTC(),
			)
			if !certified {
				t.Fatal("live canonical singleton dependency was not certified")
			}
			params := exactSessionStartTestParams(t, env)
			params.Generation = 1
			params.RolloutMode = mode
			params.ValidateConfiguredDependencyStart = func(info session.Info, retained configuredDependencyStartLease) bool {
				return configuredDependencyStartTargetMatches(info, env.cfg, retained) &&
					allDependenciesAliveForTemplateWithClock(retained.TargetTemplate, env.cfg, nil, env.sp, "test-city", env.store, env.clk)
			}
			params.EnterConfiguredDependencyStart = func(configuredDependencyStartLease) bool { return true }

			owner, err := reconcileExactSessionStartWithOwner(t.Context(), sessionStartAdmission{
				SessionID:            target.ID,
				Source:               sessionStartAdmissionSocket,
				ConfiguredDependency: &lease,
			}, params)
			if err != nil || owner != exactSessionStartKeyedOwner {
				t.Fatalf("reconcile result = (owner=%v, err=%v), want keyed success", owner, err)
			}
			if got := env.sp.CountCalls("Start", "worker"); got != 1 {
				t.Fatalf("target provider Start calls = %d, want 1", got)
			}
			started := env.sessionInfo(target.ID)
			if started.ID != target.ID || started.SessionName != "worker" || started.MetadataState != string(session.StateActive) || started.WakeRequest != "" {
				t.Fatalf("started target = %+v, want same keyed worker active with wake cleared", started)
			}
			dependencyAfter := env.sessionInfo(dependency.ID)
			if dependencyAfter.ID != dependencyBefore.ID || dependencyAfter.InstanceToken != dependencyBefore.InstanceToken {
				t.Fatalf("dependency identity changed: before=%+v after=%+v", dependencyBefore, dependencyAfter)
			}
			if !env.sp.IsRunning("database") || env.sp.CountCalls("Start", "database") != 1 || env.sp.CountCalls("Stop", "database") != 0 {
				t.Fatalf("dependency runtime changed: running=%t starts=%d stops=%d", env.sp.IsRunning("database"), env.sp.CountCalls("Start", "database"), env.sp.CountCalls("Stop", "database"))
			}
		})
	}
}

// Lease-mechanics pin: the bare origin-less target below is a pre-sync shape production does not sustain, so this asserts witness revalidation, not reachability (tracked by ga-ij8mh; re-anchored on the canonical singleton shape in WD.10a).
func TestReconcileConfiguredDependencyWakeRechecksWitnessBeforeEffects(t *testing.T) {
	for _, phase := range []struct {
		name         string
		validThrough int32
		preMutation  bool
	}{
		{name: "pre-wake", validThrough: 1, preMutation: true},
		{name: "provider-entry", validThrough: 2},
	} {
		for _, mode := range []rollout.Mode{rollout.Auto, rollout.Require} {
			t.Run(phase.name+"/"+string(mode), func(t *testing.T) {
				env := newReconcilerTestEnv()
				env.cfg = &config.City{
					Workspace: config.Workspace{Name: "test-city"},
					Agents: []config.Agent{
						{Name: "database", StartCommand: "true", MaxActiveSessions: intPtr(1)},
						{Name: "worker", StartCommand: "true", DependsOn: []string{"database"}},
					},
				}
				dependency := env.createSessionBead("database", "database")
				env.markSessionActive(&dependency)
				env.addDesired("database", "database", true)
				target := env.createSessionBead("worker", "worker")
				if err := env.store.SetMetadataBatch(target.ID, session.RequestExplicitWakePatch(string(session.WakeCauseExplicit), env.clk.Now())); err != nil {
					t.Fatalf("request explicit wake: %v", err)
				}
				before := env.sessionInfo(target.ID)
				lease := configuredDependencyStartLease{
					SessionID: target.ID, TargetTemplate: "worker", DependencyTemplate: "database",
					DependencySessionID: dependency.ID, DependencySessionName: "database", DependencyInstanceToken: "test-token",
					ControllerGeneration: 1,
				}
				params := exactSessionStartTestParams(t, env)
				params.Generation = 1
				params.RolloutMode = mode
				var validations atomic.Int32
				params.ValidateConfiguredDependencyStart = func(session.Info, configuredDependencyStartLease) bool {
					return validations.Add(1) <= phase.validThrough
				}
				params.EnterConfiguredDependencyStart = func(configuredDependencyStartLease) bool { return true }

				owner, err := reconcileExactSessionStartWithOwner(t.Context(), sessionStartAdmission{
					SessionID: target.ID, Source: sessionStartAdmissionSocket, ConfiguredDependency: &lease,
				}, params)
				if phase.preMutation && mode == rollout.Auto {
					if owner != exactSessionStartLegacyOwner || err != nil {
						t.Fatalf("Auto pre-wake result = (owner=%v, err=%v), want clean legacy yield", owner, err)
					}
				} else if owner != exactSessionStartKeyedOwner || err == nil {
					t.Fatalf("parked result = (owner=%v, err=%v), want keyed error", owner, err)
				}
				if got := env.sp.CountCalls("Start", "worker"); got != 0 {
					t.Fatalf("target provider Start calls = %d, want 0 after witness drift", got)
				}
				if phase.preMutation {
					after := env.sessionInfo(target.ID)
					if after.Generation != before.Generation || after.InstanceToken != before.InstanceToken || after.MetadataState != before.MetadataState || after.WakeRequest != before.WakeRequest || after.LastWokeAt != before.LastWokeAt {
						t.Fatalf("pre-wake drift mutated target: before=%+v after=%+v", before, after)
					}
				}
			})
		}
	}
}

// Lease-mechanics pin: the bare origin-less target below is a pre-sync shape production does not sustain, so this asserts dependency-identity fencing, not reachability (tracked by ga-ij8mh; re-anchored on the canonical singleton shape in WD.10a).
func TestReconcileConfiguredDependencyWakeRejectsDependencyReplacementIdentity(t *testing.T) {
	for _, phase := range []string{"pre-wake", "provider-entry"} {
		for _, mode := range []rollout.Mode{rollout.Auto, rollout.Require} {
			t.Run(phase+"/"+string(mode), func(t *testing.T) {
				env := newReconcilerTestEnv()
				env.cfg = &config.City{
					Workspace: config.Workspace{Name: "test-city"},
					Agents: []config.Agent{
						{Name: "database", StartCommand: "true", MaxActiveSessions: intPtr(1)},
						{Name: "worker", StartCommand: "true", DependsOn: []string{"database"}},
					},
				}
				dependency := env.createSessionBead("database", "database")
				env.markSessionActive(&dependency)
				env.addDesired("database", "database", true)
				target := env.createSessionBead("worker", "worker")
				if err := env.store.SetMetadataBatch(target.ID, session.RequestExplicitWakePatch(string(session.WakeCauseExplicit), env.clk.Now())); err != nil {
					t.Fatalf("request explicit wake: %v", err)
				}
				lease, certified := certifyConfiguredDependencyStartLease(
					env.sessionInfo(target.ID), env.cfg, env.sp, "test-city", env.store, 1, env.clk.Now().UTC(),
				)
				if !certified {
					t.Fatal("live canonical dependency was not certified")
				}
				before := env.sessionInfo(target.ID)
				var replaced atomic.Bool
				replaceDependency := func() {
					if !replaced.CompareAndSwap(false, true) {
						return
					}
					if err := env.store.Close(dependency.ID); err != nil {
						t.Fatalf("close certified dependency: %v", err)
					}
					replacement := env.createSessionBead("database", "database")
					env.setSessionMetadata(&replacement, map[string]string{
						"state":          string(session.StateActive),
						"instance_token": "replacement-token",
					})
				}
				cr := &CityRuntime{cfg: env.cfg}
				snapshot := controllerSessionStartSnapshot{
					Generation: 1, CityName: "test-city", Config: env.cfg, Provider: env.sp, Store: env.store,
				}
				params := exactSessionStartTestParams(t, env)
				params.Generation = 1
				params.RolloutMode = mode
				params.ValidateConfiguredDependencyStart = func(info session.Info, retained configuredDependencyStartLease) bool {
					current := cr.configuredDependencyStartWitnessCurrent(snapshot, info, retained)
					if phase == "pre-wake" {
						replaceDependency()
					}
					return current
				}
				params.EnterConfiguredDependencyStart = func(configuredDependencyStartLease) bool { return true }
				if phase == "provider-entry" {
					params.StartOptions = append(params.StartOptions, withTaskWorkDirResolver(func(startCandidate, *config.City) string {
						replaceDependency()
						return ""
					}))
				}

				owner, err := reconcileExactSessionStartWithOwner(t.Context(), sessionStartAdmission{
					SessionID: target.ID, Source: sessionStartAdmissionSocket, ConfiguredDependency: &lease,
				}, params)
				if phase == "pre-wake" && mode == rollout.Auto {
					if owner != exactSessionStartLegacyOwner || err != nil {
						t.Fatalf("Auto pre-wake replacement result = (owner=%v, err=%v), want clean legacy yield", owner, err)
					}
				} else if owner != exactSessionStartKeyedOwner || err == nil {
					t.Fatalf("replacement result = (owner=%v, err=%v), want keyed park", owner, err)
				}
				if got := env.sp.CountCalls("Start", "worker"); got != 0 {
					t.Fatalf("target provider Start calls = %d, want 0 after dependency replacement", got)
				}
				if phase == "pre-wake" {
					after := env.sessionInfo(target.ID)
					if after.Generation != before.Generation || after.InstanceToken != before.InstanceToken || after.WakeRequest != before.WakeRequest {
						t.Fatalf("pre-wake replacement mutated target: before=%+v after=%+v", before, after)
					}
				}
			})
		}
	}
}

// Lease-mechanics pin: the bare origin-less target below is a pre-sync shape production does not sustain, so this asserts keyed redrive after a pre-wake, not reachability (tracked by ga-ij8mh; re-anchored on the canonical singleton shape in WD.10a).
func TestConfiguredDependencyWakeRedriveStaysKeyedAfterPreWake(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{
			{Name: "database", StartCommand: "true", MaxActiveSessions: intPtr(1)},
			{Name: "worker", StartCommand: "true", DependsOn: []string{"database"}},
		},
	}
	dependency := env.createSessionBead("database", "database")
	env.markSessionActive(&dependency)
	env.addDesired("database", "database", true)
	target := env.createSessionBead("worker", "worker")
	if err := env.store.SetMetadataBatch(target.ID, session.RequestExplicitWakePatch(string(session.WakeCauseExplicit), env.clk.Now())); err != nil {
		t.Fatalf("request explicit wake: %v", err)
	}
	dependencyStopped := make(chan struct{})
	var stopOnce atomic.Bool
	cs := coherentSessionStartControllerStateForTest(env.cfg, env.sp, env.store, rollout.Auto)
	cr := &CityRuntime{
		cityPath: "test-city", cityName: "test-city", cfg: env.cfg, sp: env.sp, cs: cs,
		rec: events.Discard, stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{},
		sessionStartOptions: []startExecutionOption{
			withStartStabilityWaiter(immediateStartStabilityWaiter),
			withSessionStaleKeyDetectionWaiter(immediateSessionStaleKeyDetectionWaiter),
			withTaskWorkDirResolver(func(startCandidate, *config.City) string {
				if stopOnce.CompareAndSwap(false, true) {
					if err := env.sp.Stop("database"); err != nil {
						t.Errorf("stop dependency before provider entry: %v", err)
					}
					close(dependencyStopped)
				}
				return ""
			}),
		},
	}
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	if err := cr.ensureSessionStartController(ctx, newSessionBeadSnapshotFromInfos(nil)); err != nil {
		t.Fatalf("ensure session-start controller: %v", err)
	}
	t.Cleanup(cr.stopSessionStartController)
	if got := cr.admitSessionStartSocketKey(target.ID); got != sessionStartSocketReplyOK {
		t.Fatalf("socket reply = %q, want keyed admission", got)
	}
	awaitClose(t, dependencyStopped, "configured dependency stop after pre-wake")
	awaitCond(t, func() bool {
		controller := cr.sessionStartController
		controller.mu.Lock()
		defer controller.mu.Unlock()
		_, retained := controller.admissions[target.ID]
		_, inFlight := controller.inFlight[target.ID]
		return retained && !inFlight
	}, "parked configured-dependency admission")
	if got := env.sp.CountCalls("Start", "worker"); got != 0 {
		t.Fatalf("first-attempt target starts = %d, want 0 after provider-entry drift", got)
	}
	if err := env.sp.Start(t.Context(), "database", runtime.Config{Command: "test-cmd"}); err != nil {
		t.Fatalf("restore certified dependency runtime: %v", err)
	}
	if _, err := cr.sessionStartController.Admit(target.ID, sessionStartAdmissionInProcess); err != nil {
		t.Fatalf("redrive configured-dependency admission: %v", err)
	}
	awaitCond(t, func() bool {
		return env.sp.CountCalls("Start", "worker") == 1 || cr.sessionStartController.Pending() == 0
	}, "configured-dependency redrive completion")
	if got := env.sp.CountCalls("Start", "worker"); got != 1 {
		t.Fatalf("redrive target starts = %d, want 1 from retained keyed ownership", got)
	}
	if got := env.sessionInfo(target.ID); got.MetadataState != string(session.StateActive) || got.WakeRequest != "" {
		t.Fatalf("redriven target = %+v, want active with wake cleared", got)
	}
}

func TestCityRuntimeSessionStartSocketIngressClassifiesFromAuthoritativeRow(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker", StartCommand: "true"}}}
	backing := env.store
	bead := env.createSessionBead("worker", "worker")
	env.setSessionMetadata(&bead, map[string]string{"session_origin": "manual"})
	if err := backing.SetMetadataBatch(bead.ID, session.RequestExplicitWakePatch(string(session.WakeCauseExplicit), env.clk.Now())); err != nil {
		t.Fatalf("request explicit wake: %v", err)
	}
	cache := beads.NewCachingStore(backing, nil)
	if err := cache.PrimeActive(); err != nil {
		t.Fatalf("prime controller cache: %v", err)
	}
	if err := backing.SetMetadata(bead.ID, poolManagedMetadataKey, "true"); err != nil {
		t.Fatalf("mark backing row pool-managed: %v", err)
	}

	controller, err := newSessionStartController(sessionStartControllerOptions{
		Workers:     1,
		MaxDistinct: 8,
		MaxRetries:  0,
		Reconcile: func(context.Context, sessionStartAdmission) error {
			return nil
		},
	})
	if err != nil {
		t.Fatalf("newSessionStartController: %v", err)
	}
	if err := controller.Start(context.Background()); err != nil {
		t.Fatalf("controller.Start: %v", err)
	}
	t.Cleanup(controller.Stop)

	cr := &CityRuntime{
		cfg:                    env.cfg,
		sp:                     env.sp,
		cs:                     coherentSessionStartControllerStateForTest(env.cfg, env.sp, cache, rollout.Auto),
		sessionStartController: controller,
		sessionStartOwnership:  sessionStartOwnershipKeyed,
	}
	if got := cr.admitSessionStartSocketKey(bead.ID); got != sessionStartSocketReplyFallback {
		t.Fatalf("reply = %q, want %q for live pool row hidden by stale cache", got, sessionStartSocketReplyFallback)
	}
}
