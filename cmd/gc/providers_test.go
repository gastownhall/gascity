package main

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/agent"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

func TestTmuxConfigFromSessionDefaultsSocketToCityName(t *testing.T) {
	sc := config.SessionConfig{}

	cfg := tmuxConfigFromSession(sc, "city", "/tmp/city-a")
	if cfg.SocketName != "city" {
		t.Fatalf("SocketName = %q, want %q", cfg.SocketName, "city")
	}
}

func TestTmuxConfigFromSessionPreservesExplicitSocket(t *testing.T) {
	sc := config.SessionConfig{Socket: "custom-socket"}

	cfg := tmuxConfigFromSession(sc, "city", "/tmp/city-a")
	if cfg.SocketName != "custom-socket" {
		t.Fatalf("SocketName = %q, want %q", cfg.SocketName, "custom-socket")
	}
}

func TestSessionProviderContextForCityUsesTargetCityAndEnvOverride(t *testing.T) {
	t.Setenv("GC_SESSION", "subprocess")

	cfg := &config.City{
		Workspace: config.Workspace{
			Name:            "bright-lights",
			SessionTemplate: "{{.Agent}}",
		},
		Session: config.SessionConfig{
			Provider: "tmux",
			Socket:   "from-config",
		},
		Agents: []config.Agent{
			{Name: "mayor"},
		},
	}

	ctx := sessionProviderContextForCity(cfg, "/tmp/city-a", os.Getenv("GC_SESSION"))
	if ctx.cityPath != "/tmp/city-a" {
		t.Fatalf("cityPath = %q, want %q", ctx.cityPath, "/tmp/city-a")
	}
	if ctx.cityName != "bright-lights" {
		t.Fatalf("cityName = %q, want %q", ctx.cityName, "bright-lights")
	}
	if ctx.providerName != "subprocess" {
		t.Fatalf("providerName = %q, want %q", ctx.providerName, "subprocess")
	}
	if ctx.sessionTemplate != "{{.Agent}}" {
		t.Fatalf("sessionTemplate = %q, want %q", ctx.sessionTemplate, "{{.Agent}}")
	}
	if len(ctx.agents) != 1 || ctx.agents[0].Name != "mayor" {
		t.Fatalf("agents = %#v, want mayor", ctx.agents)
	}
}

func TestConfiguredACPSessionNames_UsesProvidedSnapshot(t *testing.T) {
	snapshot := newSessionBeadSnapshot([]beads.Bead{{
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "agent:reviewer"},
		Metadata: map[string]string{
			"template":     "reviewer",
			"agent_name":   "reviewer",
			"session_name": "custom-reviewer",
		},
	}})

	agents := []config.Agent{
		{Name: "reviewer", Session: "acp"},
		{Name: "witness", Session: "acp"},
		{Name: "mayor"},
	}

	got := configuredACPSessionNames(snapshot, "city", "", agents)
	want := []string{
		"custom-reviewer",
		agent.SessionNameFor("city", "witness", ""),
	}
	if len(got) != len(want) {
		t.Fatalf("configuredACPSessionNames len = %d, want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("configuredACPSessionNames[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestNewSessionProvider_PreregistersACPBeadAndLegacyNames(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "fake")

	cityDir := t.TempDir()
	t.Setenv("GC_CITY", cityDir)
	writeACPRouteCityTOML(t, cityDir, "test-city")

	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	if _, err := store.Create(beads.Bead{
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "agent:reviewer"},
		Metadata: map[string]string{
			"template":     "reviewer",
			"agent_name":   "reviewer",
			"session_name": "custom-reviewer",
		},
	}); err != nil {
		t.Fatalf("Create(session bead): %v", err)
	}

	sp := newSessionProvider()

	if err := sp.Attach("custom-reviewer"); err == nil || !strings.Contains(err.Error(), "ACP transport") {
		t.Fatalf("Attach(custom-reviewer) error = %v, want ACP transport error", err)
	}

	witnessName := agent.SessionNameFor("test-city", "witness", "")
	if err := sp.Attach(witnessName); err == nil || !strings.Contains(err.Error(), "ACP transport") {
		t.Fatalf("Attach(%q) error = %v, want ACP transport error", witnessName, err)
	}

	mayorName := agent.SessionNameFor("test-city", "mayor", "")
	if err := sp.Attach(mayorName); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("Attach(%q) error = %v, want fake-provider not found", mayorName, err)
	}
}

func TestLoadProviderSessionSnapshotLoadsStoreWithoutACPAgents(t *testing.T) {
	oldOpen := openSessionProviderStore
	t.Cleanup(func() { openSessionProviderStore = oldOpen })

	calls := 0
	openSessionProviderStore = func(string) (beads.Store, error) {
		calls++
		return beads.NewMemStore(), nil
	}

	snapshot := loadProviderSessionSnapshot(sessionProviderContext{
		providerName: "tmux",
		cityPath:     "/tmp/city",
		agents: []config.Agent{
			{Name: "mayor"},
		},
	})
	if snapshot == nil {
		t.Fatal("loadProviderSessionSnapshot() = nil, want empty snapshot")
	}
	if calls != 1 {
		t.Fatalf("openSessionProviderStore called %d times, want 1", calls)
	}
}

func TestLoadProviderSessionSnapshotLoadsOpenACPAgents(t *testing.T) {
	oldOpen := openSessionProviderStore
	t.Cleanup(func() { openSessionProviderStore = oldOpen })

	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "agent:reviewer"},
		Metadata: map[string]string{
			"template":     "reviewer",
			"agent_name":   "reviewer",
			"session_name": "custom-reviewer",
		},
	}); err != nil {
		t.Fatalf("Create(session bead): %v", err)
	}

	calls := 0
	openSessionProviderStore = func(string) (beads.Store, error) {
		calls++
		return store, nil
	}

	snapshot := loadProviderSessionSnapshot(sessionProviderContext{
		providerName: "tmux",
		cityPath:     "/tmp/city",
		agents: []config.Agent{
			{Name: "reviewer", Session: "acp"},
		},
	})
	if calls != 1 {
		t.Fatalf("openSessionProviderStore called %d times, want 1", calls)
	}
	if snapshot == nil {
		t.Fatal("loadProviderSessionSnapshot() = nil, want snapshot")
	}
	if got := snapshot.FindSessionNameByTemplate("reviewer"); got != "custom-reviewer" {
		t.Fatalf("snapshot.FindSessionNameByTemplate(reviewer) = %q, want %q", got, "custom-reviewer")
	}
}

func TestNewSessionProviderRoutesRemoteWorkerExecAgent(t *testing.T) {
	cityDir := t.TempDir()
	scriptDir := filepath.Join(cityDir, "scripts")
	if err := os.MkdirAll(scriptDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(scriptDir): %v", err)
	}
	logPath := filepath.Join(cityDir, "exec.log")
	scriptPath := filepath.Join(scriptDir, "remote-worker")
	script := `#!/usr/bin/env bash
set -euo pipefail
log=` + strconv.Quote(logPath) + `
case "$1" in
  profile)
    echo "remote-worker/v1"
    ;;
  start)
    cat >/dev/null
    printf '%s|%s|%s|%s\n' "${GC_EXEC_PROFILE:-}" "${GC_EXEC_STATE_DIR:-}" "$1" "$2" >> "$log"
    ;;
  *)
    exit 2
    ;;
esac
`
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile(script): %v", err)
	}

	worker := config.Agent{Name: "worker", Session: "exec:scripts/remote-worker"}
	ctx := sessionProviderContext{
		providerName: "fake",
		cityName:     "city",
		cityPath:     cityDir,
		agents:       []config.Agent{worker},
	}

	sp := newSessionProviderFromContext(ctx, nil)
	name := agent.SessionNameFor("city", "worker", "")
	if err := sp.Start(context.Background(), name, runtime.Config{}); err != nil {
		t.Fatalf("Start(%q): %v", name, err)
	}

	logBytes, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("ReadFile(log): %v", err)
	}
	fields := strings.Split(strings.TrimSpace(string(logBytes)), "|")
	if len(fields) != 4 {
		t.Fatalf("exec log = %q, want 4 pipe-delimited fields", strings.TrimSpace(string(logBytes)))
	}
	if fields[0] != remoteWorkerProfile {
		t.Fatalf("GC_EXEC_PROFILE = %q, want %q", fields[0], remoteWorkerProfile)
	}
	if fields[1] == "" {
		t.Fatal("GC_EXEC_STATE_DIR is empty")
	}
	if fields[2] != "start" || fields[3] != name {
		t.Fatalf("operation/name = %q/%q, want start/%q", fields[2], fields[3], name)
	}
}

func TestNewSessionProviderProfilesExecAgentWhenDefaultUsesSameScript(t *testing.T) {
	cityDir := t.TempDir()
	logPath := filepath.Join(cityDir, "exec.log")
	scriptPath := filepath.Join(cityDir, "remote-worker")
	script := `#!/usr/bin/env bash
set -euo pipefail
log=` + strconv.Quote(logPath) + `
case "$1" in
  profile)
    echo "remote-worker/v1"
    ;;
  start)
    cat >/dev/null
    printf '%s|%s\n' "${GC_EXEC_PROFILE:-}" "$2" >> "$log"
    ;;
  *)
    exit 2
    ;;
esac
`
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile(script): %v", err)
	}

	worker := config.Agent{Name: "worker", Session: "exec:" + scriptPath}
	ctx := sessionProviderContext{
		providerName: "exec:" + scriptPath,
		cityName:     "city",
		cityPath:     cityDir,
		agents:       []config.Agent{worker},
	}

	sp := newSessionProviderFromContext(ctx, nil)
	name := agent.SessionNameFor("city", "worker", "")
	if err := sp.Start(context.Background(), name, runtime.Config{}); err != nil {
		t.Fatalf("Start(%q): %v", name, err)
	}

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("ReadFile(log): %v", err)
	}
	got := strings.TrimSpace(string(data))
	want := remoteWorkerProfile + "|" + name
	if got != want {
		t.Fatalf("exec log = %q, want %q", got, want)
	}
}

func TestConfiguredSessionProviderRoutesPreservesOwnedSnapshotRoute(t *testing.T) {
	cityDir := t.TempDir()
	scriptPath := filepath.Join(cityDir, "remote-worker")
	if err := os.WriteFile(scriptPath, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("WriteFile(script): %v", err)
	}
	snapshot := newSessionBeadSnapshot([]beads.Bead{{
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "agent:worker"},
		Metadata: map[string]string{
			"template":         "worker",
			"agent_name":       "worker",
			"session_name":     "worker-existing",
			"session_provider": "acp",
		},
	}})

	routes, err := configuredSessionProviderRoutes(snapshot, "city", cityDir, "", "tmux", []config.Agent{{
		Name:    "worker",
		Session: "exec:remote-worker",
	}})
	if err != nil {
		t.Fatalf("configuredSessionProviderRoutes: %v", err)
	}
	found := false
	for _, route := range routes {
		if route.name == "worker-existing" && route.provider == "acp" && !route.deferRoute {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("routes = %#v, want preserved worker-existing/acp route", routes)
	}
}

func TestConfiguredSessionProviderRoutesUnroutesRemovedLegacyACPOverride(t *testing.T) {
	cityDir := t.TempDir()
	snapshot := newSessionBeadSnapshot([]beads.Bead{{
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "agent:worker"},
		Metadata: map[string]string{
			"template":     "worker",
			"agent_name":   "worker",
			"session_name": "worker-existing",
			"transport":    "acp",
		},
	}})

	routes, err := configuredSessionProviderRoutes(snapshot, "city", cityDir, "", "tmux", []config.Agent{{
		Name: "worker",
	}})
	if err != nil {
		t.Fatalf("configuredSessionProviderRoutes: %v", err)
	}
	var defaultRoute, deferredACP bool
	for _, route := range routes {
		switch {
		case route.name == "worker-existing" && route.provider == "tmux" && !route.deferRoute:
			defaultRoute = true
		case route.name == "worker-existing" && route.provider == "acp" && route.deferRoute:
			deferredACP = true
		}
	}
	if !defaultRoute || !deferredACP {
		t.Fatalf("routes = %#v, want default route plus deferred ACP backend candidate", routes)
	}
}

func TestConfiguredSessionProviderRoutesRegistersDesiredExecWhenSnapshotPreserved(t *testing.T) {
	cityDir := t.TempDir()
	scriptPath := filepath.Join(cityDir, "remote-worker")
	if err := os.WriteFile(scriptPath, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("WriteFile(script): %v", err)
	}
	snapshot := newSessionBeadSnapshot([]beads.Bead{{
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "agent:worker"},
		Metadata: map[string]string{
			"template":         "worker",
			"agent_name":       "worker",
			"session_name":     "worker-existing",
			"session_provider": "acp",
			"state":            "active",
		},
	}})

	routes, err := configuredSessionProviderRoutes(snapshot, "city", cityDir, "", "tmux", []config.Agent{{
		Name:    "worker",
		Session: "exec:remote-worker",
	}})
	if err != nil {
		t.Fatalf("configuredSessionProviderRoutes: %v", err)
	}
	if len(routes) != 2 {
		t.Fatalf("routes len = %d, want 2 (%#v)", len(routes), routes)
	}
	var preserved, candidate bool
	for _, route := range routes {
		switch {
		case route.provider == "acp" && !route.deferRoute:
			preserved = true
		case strings.HasPrefix(route.provider, "exec:") && route.profile == remoteWorkerProfile && route.deferRoute:
			candidate = true
		}
	}
	if !preserved || !candidate {
		t.Fatalf("routes = %#v, want preserved acp route and deferred exec backend candidate", routes)
	}
}

func TestConfiguredSessionProviderRoutesDefersUnownedExecRoute(t *testing.T) {
	cityDir := t.TempDir()
	scriptPath := filepath.Join(cityDir, "remote-worker")
	if err := os.WriteFile(scriptPath, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("WriteFile(script): %v", err)
	}
	snapshot := newSessionBeadSnapshot([]beads.Bead{{
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "agent:worker"},
		Metadata: map[string]string{
			"template":     "worker",
			"agent_name":   "worker",
			"session_name": "worker-existing",
			"state":        "active",
		},
	}})

	routes, err := configuredSessionProviderRoutes(snapshot, "city", cityDir, "", "tmux", []config.Agent{{
		Name:    "worker",
		Session: "exec:remote-worker",
	}})
	if err != nil {
		t.Fatalf("configuredSessionProviderRoutes: %v", err)
	}
	if len(routes) != 1 {
		t.Fatalf("routes len = %d, want 1 (%#v)", len(routes), routes)
	}
	if !routes[0].deferRoute {
		t.Fatalf("route = %#v, want deferred backend registration", routes[0])
	}
}

func writeACPRouteCityTOML(t *testing.T, dir, cityName string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".gc"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.gc): %v", err)
	}
	data := []byte(`[workspace]
name = "` + cityName + `"

[beads]
provider = "file"

[[agent]]
name = "reviewer"
provider = "claude"
start_command = "echo"
session = "acp"

[[agent]]
name = "witness"
provider = "claude"
start_command = "echo"
session = "acp"

[[agent]]
name = "mayor"
provider = "claude"
start_command = "echo"
`)
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), data, 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}
}
