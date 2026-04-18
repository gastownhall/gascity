package main

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
)

type restartRequestTestEnv struct {
	store        beads.Store
	sp           *runtime.Fake
	dt           *drainTracker
	clk          *clock.Fake
	rec          events.Recorder
	cfg          *config.City
	desiredState map[string]TemplateParams
	stdout       bytes.Buffer
	stderr       bytes.Buffer
}

func newRestartRequestTestEnv() *restartRequestTestEnv {
	return &restartRequestTestEnv{
		store:        beads.NewMemStore(),
		sp:           runtime.NewFake(),
		dt:           newDrainTracker(),
		clk:          &clock.Fake{Time: time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)},
		rec:          events.Discard,
		cfg:          &config.City{},
		desiredState: make(map[string]TemplateParams),
	}
}

func (e *restartRequestTestEnv) createSessionBead(name, template string) beads.Bead {
	b, err := e.store.Create(beads.Bead{
		Title:  name,
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":   name,
			"agent_name":     name,
			"template":       template,
			"generation":     "1",
			"instance_token": "test-token",
			"state":          "asleep",
		},
	})
	if err != nil {
		panic("creating test bead: " + err.Error())
	}
	return b
}

func (e *restartRequestTestEnv) setSessionMetadata(session *beads.Bead, kvs map[string]string) {
	for key, value := range kvs {
		_ = e.store.SetMetadata(session.ID, key, value)
		session.Metadata[key] = value
	}
}

func (e *restartRequestTestEnv) reconcile(sessions []beads.Bead) {
	poolDesired := make(map[string]int)
	for _, tp := range e.desiredState {
		if tp.TemplateName != "" {
			poolDesired[tp.TemplateName]++
		}
	}
	cfgNames := configuredSessionNames(e.cfg, "", e.store)
	_ = reconcileSessionBeads(
		context.Background(),
		sessions,
		e.desiredState,
		cfgNames,
		e.cfg,
		e.sp,
		e.store,
		nil,
		nil,
		nil,
		e.dt,
		poolDesired,
		false,
		nil,
		"",
		nil,
		e.clk,
		e.rec,
		0,
		0,
		&e.stdout,
		&e.stderr,
	)
}

func TestReconcileSessionBeads_RestartRequestRotatesKeyForSessionIDProviders(t *testing.T) {
	env := newRestartRequestTestEnv()
	env.cfg = &config.City{
		Workspace:     config.Workspace{Name: "test-city"},
		Agents:        []config.Agent{{Name: "worker", StartCommand: "true", MaxActiveSessions: restartRequestTestIntPtr(1)}},
		NamedSessions: []config.NamedSession{{Template: "worker", Mode: "on_demand"}},
	}
	sessionName := config.NamedSessionRuntimeName(env.cfg.Workspace.Name, env.cfg.Workspace, "worker")
	env.desiredState[sessionName] = TemplateParams{
		Command:      "true",
		SessionName:  sessionName,
		TemplateName: "worker",
		ResolvedProvider: &config.ResolvedProvider{
			SessionIDFlag: "--session-id",
		},
	}

	session := env.createSessionBead(sessionName, "worker")
	env.setSessionMetadata(&session, map[string]string{
		namedSessionMetadataKey:      "true",
		namedSessionIdentityMetadata: "worker",
		namedSessionModeMetadata:     "on_demand",
		"restart_requested":          "true",
		"session_key":                "original-key",
		"started_config_hash":        "hash-before-restart",
	})

	env.reconcile([]beads.Bead{session})

	got, _ := env.store.Get(session.ID)
	if got.Metadata["session_key"] == "" {
		t.Fatal("session_key = empty, want rotated key for SessionIDFlag provider")
	}
	if got.Metadata["session_key"] == "original-key" {
		t.Fatalf("session_key = %q, want rotated key", got.Metadata["session_key"])
	}
	if got.Metadata["started_config_hash"] != "" {
		t.Fatalf("started_config_hash = %q, want empty", got.Metadata["started_config_hash"])
	}
	if got.Metadata["continuation_reset_pending"] != "true" {
		t.Fatalf("continuation_reset_pending = %q, want true", got.Metadata["continuation_reset_pending"])
	}
}

func TestReconcileSessionBeads_RestartRequestClearsKeyForResumeOnlyProviders(t *testing.T) {
	env := newRestartRequestTestEnv()
	env.cfg = &config.City{
		Workspace:     config.Workspace{Name: "test-city"},
		Agents:        []config.Agent{{Name: "worker", StartCommand: "true", MaxActiveSessions: restartRequestTestIntPtr(1)}},
		NamedSessions: []config.NamedSession{{Template: "worker", Mode: "on_demand"}},
	}
	sessionName := config.NamedSessionRuntimeName(env.cfg.Workspace.Name, env.cfg.Workspace, "worker")
	env.desiredState[sessionName] = TemplateParams{
		Command:      "true",
		SessionName:  sessionName,
		TemplateName: "worker",
		ResolvedProvider: &config.ResolvedProvider{
			ResumeFlag:  "--resume",
			ResumeStyle: "flag",
		},
	}

	session := env.createSessionBead(sessionName, "worker")
	env.setSessionMetadata(&session, map[string]string{
		namedSessionMetadataKey:      "true",
		namedSessionIdentityMetadata: "worker",
		namedSessionModeMetadata:     "on_demand",
		"restart_requested":          "true",
		"session_key":                "original-key",
		"started_config_hash":        "hash-before-restart",
	})

	env.reconcile([]beads.Bead{session})

	got, _ := env.store.Get(session.ID)
	if got.Metadata["session_key"] != "" {
		t.Fatalf("session_key = %q, want empty for resume-only provider", got.Metadata["session_key"])
	}
	if got.Metadata["started_config_hash"] != "" {
		t.Fatalf("started_config_hash = %q, want empty", got.Metadata["started_config_hash"])
	}
	if got.Metadata["continuation_reset_pending"] != "true" {
		t.Fatalf("continuation_reset_pending = %q, want true", got.Metadata["continuation_reset_pending"])
	}
}

func restartRequestTestIntPtr(n int) *int { return &n }
