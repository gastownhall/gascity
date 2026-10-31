package main

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// syncManualSession writes a manual session bead for template, as
// POST /sessions does, runs it through discovery and one legacy sync, and
// returns the bead's metadata afterward.
func syncManualSession(t *testing.T, agent config.Agent) map[string]string {
	t.Helper()
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	clk := &clock.Fake{Time: time.Date(2026, 3, 7, 12, 0, 0, 0, time.UTC)}
	sp := runtime.NewFake()
	sessionName := "s-gc-chat"
	if err := sp.Start(context.TODO(), sessionName, runtime.Config{Command: "claude"}); err != nil {
		t.Fatalf("start runtime session: %v", err)
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Providers: map[string]config.ProviderSpec{"claude": {Command: "claude"}},
		Agents:    []config.Agent{agent},
	}
	bead, err := store.Create(beads.Bead{
		Title:  "chat",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "template:" + agent.Name},
		Metadata: map[string]string{
			"template":       agent.Name,
			"session_name":   sessionName,
			"alias":          "my-chat",
			"state":          "active",
			"session_origin": "manual",
		},
	})
	if err != nil {
		t.Fatalf("create manual bead: %v", err)
	}

	bp := newAgentBuildParams("test-city", cityPath, cfg, sp, clk.Now(), store, io.Discard)
	desired := make(map[string]TemplateParams)
	discoverSessionBeads(bp, cfg, desired, io.Discard)
	if _, ok := desired[sessionName]; !ok {
		t.Fatalf("discoverSessionBeads() missing the manual session, got keys: %v", mapKeys(desired))
	}
	var stderr bytes.Buffer
	syncSessionBeads(cityPath, store, desired, sp, configuredSessionNames(cfg, "test-city", store), cfg, clk, &stderr, false)

	got, err := store.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", bead.ID, err)
	}
	return got.Metadata
}

// TestSyncSessionBeads_ClientProviderChatGetsNoAgentIdentity pins the kind a
// client's provider chat keeps. POST /sessions with kind=provider creates a
// manual session whose template is the provider's implicit agent, because
// that agent is named after the provider. Stamping agent_name onto it made
// every later read (GET /session/{id}, the worker, resume) resolve it as
// kind "agent", agent_kind "pool": the chat turned into a pool worker after
// its first reconcile tick, though its create had reported kind "provider".
func TestSyncSessionBeads_ClientProviderChatGetsNoAgentIdentity(t *testing.T) {
	meta := syncManualSession(t, config.Agent{Name: "claude", Provider: "claude", Implicit: true})
	if got := meta["agent_name"]; got != "" {
		t.Fatalf("agent_name = %q after sync, want none on a client's provider chat", got)
	}
	if session.UseAgentTemplateForProviderResolution("", meta, "claude", "claude", true) {
		t.Fatal("the synced provider chat resolves as an agent session; want a provider session")
	}
}

// A manual session of a configured agent is an instance of that agent and
// still gets its identity (its alias).
func TestSyncSessionBeads_ManualSessionOfConfiguredAgentKeepsAgentIdentity(t *testing.T) {
	meta := syncManualSession(t, config.Agent{Name: "helper", StartCommand: "echo"})
	if got := meta["agent_name"]; got != "my-chat" {
		t.Fatalf("agent_name = %q after sync, want the session's identity my-chat", got)
	}
}
