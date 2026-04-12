package main

import (
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

func writeTemplateResolveCityConfig(t *testing.T, cityPath, beadsProvider string) {
	t.Helper()

	content := "[workspace]\nname = \"city\"\n"
	if beadsProvider != "" {
		content += "\n[beads]\nprovider = \"" + beadsProvider + "\"\n"
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(content), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
}

func TestResolveTemplateUsesWorkDirWithoutChangingRigIdentity(t *testing.T) {
	cityPath := t.TempDir()
	writeTemplateResolveCityConfig(t, cityPath, "file")
	rigRoot := filepath.Join(cityPath, "demo")
	if err := os.MkdirAll(rigRoot, 0o755); err != nil {
		t.Fatal(err)
	}

	params := &agentBuildParams{
		cityName:   "city",
		cityPath:   cityPath,
		workspace:  &config.Workspace{Provider: "test"},
		providers:  map[string]config.ProviderSpec{"test": {Command: "echo", PromptMode: "none"}},
		lookPath:   func(string) (string, error) { return "/bin/echo", nil },
		fs:         fsys.OSFS{},
		rigs:       []config.Rig{{Name: "demo", Path: rigRoot}},
		beaconTime: time.Unix(0, 0),
		beadNames:  make(map[string]string),
		stderr:     io.Discard,
	}

	agent := &config.Agent{
		Name:    "witness",
		Dir:     "demo",
		WorkDir: ".gc/agents/{{.Rig}}/witness",
	}
	tp, err := resolveTemplate(params, agent, agent.QualifiedName(), nil)
	if err != nil {
		t.Fatalf("resolveTemplate: %v", err)
	}

	wantWorkDir := filepath.Join(cityPath, ".gc", "agents", "demo", "witness")
	if tp.WorkDir != wantWorkDir {
		t.Fatalf("WorkDir = %q, want %q", tp.WorkDir, wantWorkDir)
	}
	if tp.RigName != "demo" {
		t.Fatalf("RigName = %q, want demo", tp.RigName)
	}
	if tp.RigRoot != rigRoot {
		t.Fatalf("RigRoot = %q, want %q", tp.RigRoot, rigRoot)
	}
	if tp.Env["GC_RIG"] != "demo" {
		t.Fatalf("GC_RIG = %q, want demo", tp.Env["GC_RIG"])
	}
	if tp.Env["GC_RIG_ROOT"] != rigRoot {
		t.Fatalf("GC_RIG_ROOT = %q, want %q", tp.Env["GC_RIG_ROOT"], rigRoot)
	}
	if tp.Env["BEADS_DIR"] != filepath.Join(rigRoot, ".beads") {
		t.Fatalf("BEADS_DIR = %q, want %q", tp.Env["BEADS_DIR"], filepath.Join(rigRoot, ".beads"))
	}
	if tp.Env["GT_ROOT"] != cityPath {
		t.Fatalf("GT_ROOT = %q, want city root %q", tp.Env["GT_ROOT"], cityPath)
	}
}

func TestResolveTemplateUsesWorkDirForCityScopedAgents(t *testing.T) {
	cityPath := t.TempDir()
	writeTemplateResolveCityConfig(t, cityPath, "file")

	params := &agentBuildParams{
		cityName:   "city",
		cityPath:   cityPath,
		workspace:  &config.Workspace{Provider: "test"},
		providers:  map[string]config.ProviderSpec{"test": {Command: "echo", PromptMode: "none"}},
		lookPath:   func(string) (string, error) { return "/bin/echo", nil },
		fs:         fsys.OSFS{},
		beaconTime: time.Unix(0, 0),
		beadNames:  make(map[string]string),
		stderr:     io.Discard,
	}

	agent := &config.Agent{
		Name:    "mayor",
		WorkDir: ".gc/agents/mayor",
	}
	tp, err := resolveTemplate(params, agent, agent.QualifiedName(), nil)
	if err != nil {
		t.Fatalf("resolveTemplate: %v", err)
	}

	wantWorkDir := filepath.Join(cityPath, ".gc", "agents", "mayor")
	if tp.WorkDir != wantWorkDir {
		t.Fatalf("WorkDir = %q, want %q", tp.WorkDir, wantWorkDir)
	}
	if tp.RigName != "" {
		t.Fatalf("RigName = %q, want empty", tp.RigName)
	}
	if got, ok := tp.Env["GC_RIG"]; !ok || got != "" {
		t.Fatalf("GC_RIG = %q present=%v, want explicit empty", got, ok)
	}
	if got, ok := tp.Env["GC_RIG_ROOT"]; !ok || got != "" {
		t.Fatalf("GC_RIG_ROOT = %q present=%v, want explicit empty", got, ok)
	}
	if got, ok := tp.Env["BEADS_DIR"]; !ok || got != "" {
		t.Fatalf("BEADS_DIR = %q present=%v, want explicit empty", got, ok)
	}
	if tp.Env["GT_ROOT"] != cityPath {
		t.Fatalf("GT_ROOT = %q, want %q", tp.Env["GT_ROOT"], cityPath)
	}
	if tp.Env["GC_BEADS"] != "file" {
		t.Fatalf("GC_BEADS = %q, want file", tp.Env["GC_BEADS"])
	}
}

func TestResolveTemplateDefaultsRigScopedAgentsToRigRootWithoutWorkDir(t *testing.T) {
	cityPath := t.TempDir()
	writeTemplateResolveCityConfig(t, cityPath, "file")
	rigRoot := filepath.Join(t.TempDir(), "demo")
	if err := os.MkdirAll(rigRoot, 0o755); err != nil {
		t.Fatal(err)
	}

	params := &agentBuildParams{
		cityName:   "city",
		cityPath:   cityPath,
		workspace:  &config.Workspace{Provider: "test"},
		providers:  map[string]config.ProviderSpec{"test": {Command: "echo", PromptMode: "none"}},
		lookPath:   func(string) (string, error) { return "/bin/echo", nil },
		fs:         fsys.OSFS{},
		rigs:       []config.Rig{{Name: "demo", Path: rigRoot}},
		beaconTime: time.Unix(0, 0),
		beadNames:  make(map[string]string),
		stderr:     io.Discard,
	}

	agent := &config.Agent{
		Name: "refinery",
		Dir:  "demo",
	}
	tp, err := resolveTemplate(params, agent, agent.QualifiedName(), nil)
	if err != nil {
		t.Fatalf("resolveTemplate: %v", err)
	}

	if tp.WorkDir != rigRoot {
		t.Fatalf("WorkDir = %q, want %q", tp.WorkDir, rigRoot)
	}
	if tp.RigRoot != rigRoot {
		t.Fatalf("RigRoot = %q, want %q", tp.RigRoot, rigRoot)
	}
	if tp.Env["BEADS_DIR"] != filepath.Join(rigRoot, ".beads") {
		t.Fatalf("BEADS_DIR = %q, want %q", tp.Env["BEADS_DIR"], filepath.Join(rigRoot, ".beads"))
	}
	if tp.Env["GT_ROOT"] != cityPath {
		t.Fatalf("GT_ROOT = %q, want city root %q", tp.Env["GT_ROOT"], cityPath)
	}
}

func TestResolveTemplateRigScopedEnvCarriesRigRoots(t *testing.T) {
	cityPath := t.TempDir()
	writeTemplateResolveCityConfig(t, cityPath, "file")
	rigRoot := filepath.Join(t.TempDir(), "demo")
	if err := os.MkdirAll(rigRoot, 0o755); err != nil {
		t.Fatal(err)
	}

	params := &agentBuildParams{
		cityName:   "city",
		cityPath:   cityPath,
		workspace:  &config.Workspace{Provider: "test"},
		providers:  map[string]config.ProviderSpec{"test": {Command: "echo", PromptMode: "none"}},
		lookPath:   func(string) (string, error) { return "/bin/echo", nil },
		fs:         fsys.OSFS{},
		rigs:       []config.Rig{{Name: "demo", Path: rigRoot}},
		beaconTime: time.Unix(0, 0),
		beadNames:  make(map[string]string),
		stderr:     io.Discard,
	}

	agent := &config.Agent{Name: "witness", Dir: "demo"}
	tp, err := resolveTemplate(params, agent, agent.QualifiedName(), nil)
	if err != nil {
		t.Fatalf("resolveTemplate: %v", err)
	}

	if tp.Env["GC_RIG_ROOT"] != rigRoot {
		t.Fatalf("GC_RIG_ROOT = %q, want %q", tp.Env["GC_RIG_ROOT"], rigRoot)
	}
	if tp.Env["BEADS_DIR"] != filepath.Join(rigRoot, ".beads") {
		t.Fatalf("BEADS_DIR = %q, want %q", tp.Env["BEADS_DIR"], filepath.Join(rigRoot, ".beads"))
	}
	if tp.Env["GT_ROOT"] != cityPath {
		t.Fatalf("GT_ROOT = %q, want city root %q", tp.Env["GT_ROOT"], cityPath)
	}
}

func TestResolveTemplateUsesCityManagedDoltPort(t *testing.T) {
	cityPath := t.TempDir()
	writeTemplateResolveCityConfig(t, cityPath, "")
	stateDir := filepath.Join(cityPath, ".gc", "runtime", "packs", "dolt")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close() //nolint:errcheck // test cleanup

	port := ln.Addr().(*net.TCPAddr).Port
	state := doltRuntimeState{
		Running:   true,
		PID:       os.Getpid(),
		Port:      port,
		DataDir:   filepath.Join(cityPath, ".beads", "dolt"),
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("marshal dolt state: %v", err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "dolt-state.json"), data, 0o644); err != nil {
		t.Fatalf("write dolt state: %v", err)
	}

	t.Setenv("GC_DOLT_PORT", "9999")

	params := &agentBuildParams{
		cityName:   "city",
		cityPath:   cityPath,
		workspace:  &config.Workspace{Provider: "test"},
		providers:  map[string]config.ProviderSpec{"test": {Command: "echo", PromptMode: "none"}},
		lookPath:   func(string) (string, error) { return "/bin/echo", nil },
		fs:         fsys.OSFS{},
		beaconTime: time.Unix(0, 0),
		beadNames:  make(map[string]string),
		stderr:     io.Discard,
	}

	agent := &config.Agent{Name: "worker"}
	tp, err := resolveTemplate(params, agent, agent.QualifiedName(), nil)
	if err != nil {
		t.Fatalf("resolveTemplate: %v", err)
	}

	if got := tp.Env["GC_DOLT_PORT"]; got != strconv.Itoa(port) {
		t.Fatalf("GC_DOLT_PORT = %q, want %q", got, strconv.Itoa(port))
	}
	if got := tp.Env["GC_BIN"]; got == "" {
		t.Fatalf("GC_BIN = %q, want non-empty", got)
	}
	if got := tp.Env["GC_BEADS"]; got != "exec:"+filepath.Join(cityPath, ".gc", "system", "bin", "gc-beads-bd") {
		t.Fatalf("GC_BEADS = %q, want exec gc-beads-bd provider", got)
	}
}

func TestResolveTemplatePreservesLogicalAgentNameWhenSessionBeadExists(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	sessionBead, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "agent:worker"},
		Metadata: map[string]string{
			"template":     "worker",
			"agent_name":   "worker",
			"session_name": "worker",
			"alias":        "worker",
		},
	})
	if err != nil {
		t.Fatalf("Create session bead: %v", err)
	}
	snapshot, err := loadSessionBeadSnapshot(store)
	if err != nil {
		t.Fatalf("loadSessionBeadSnapshot: %v", err)
	}

	params := &agentBuildParams{
		cityName:     "city",
		cityPath:     cityPath,
		workspace:    &config.Workspace{Provider: "test"},
		providers:    map[string]config.ProviderSpec{"test": {Command: "echo", PromptMode: "none"}},
		lookPath:     func(string) (string, error) { return "/bin/echo", nil },
		fs:           fsys.OSFS{},
		beaconTime:   time.Unix(0, 0),
		beadStore:    store,
		sessionBeads: snapshot,
		beadNames:    make(map[string]string),
		stderr:       io.Discard,
	}

	agent := &config.Agent{Name: "worker"}
	tp, err := resolveTemplate(params, agent, agent.QualifiedName(), nil)
	if err != nil {
		t.Fatalf("resolveTemplate: %v", err)
	}

	if got := tp.SessionName; got != "worker" {
		t.Fatalf("SessionName = %q, want worker", got)
	}
	if got := tp.Env["GC_SESSION_ID"]; got != sessionBead.ID {
		t.Fatalf("GC_SESSION_ID = %q, want %q", got, sessionBead.ID)
	}
	if got := tp.Env["GC_AGENT"]; got != "worker" {
		t.Fatalf("GC_AGENT = %q, want worker", got)
	}
	if got := tp.Env["GC_ALIAS"]; got != "worker" {
		t.Fatalf("GC_ALIAS = %q, want worker", got)
	}
}

func TestResolveTemplateUsesRigManagedDoltPortAndPinsHome(t *testing.T) {
	cityPath := t.TempDir()
	rigRoot := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(filepath.Join(rigRoot, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close() //nolint:errcheck // test cleanup

	port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	if err := os.WriteFile(filepath.Join(rigRoot, ".beads", "dolt-server.port"), []byte(port), 0o644); err != nil {
		t.Fatal(err)
	}

	gcHome := filepath.Join(t.TempDir(), "gc-home")
	t.Setenv("GC_HOME", gcHome)
	t.Setenv("GC_DOLT_PORT", "9999")

	params := &agentBuildParams{
		cityName:   "city",
		cityPath:   cityPath,
		workspace:  &config.Workspace{Provider: "test"},
		providers:  map[string]config.ProviderSpec{"test": {Command: "echo", PromptMode: "none"}},
		lookPath:   func(string) (string, error) { return "/bin/echo", nil },
		fs:         fsys.OSFS{},
		rigs:       []config.Rig{{Name: "repo", Path: rigRoot}},
		beaconTime: time.Unix(0, 0),
		beadNames:  make(map[string]string),
		stderr:     io.Discard,
	}

	agent := &config.Agent{Name: "polecat", Dir: "repo"}
	tp, err := resolveTemplate(params, agent, agent.QualifiedName(), nil)
	if err != nil {
		t.Fatalf("resolveTemplate: %v", err)
	}

	if got := tp.Env["GC_DOLT_PORT"]; got != port {
		t.Fatalf("GC_DOLT_PORT = %q, want %q", got, port)
	}
	if got := tp.Env["BEADS_DOLT_SERVER_PORT"]; got != port {
		t.Fatalf("BEADS_DOLT_SERVER_PORT = %q, want %q", got, port)
	}
	// HOME is intentionally passed through to agents (PR #272:
	// HOME/USER/XDG env passthrough for macOS Keychain and config access).
	// Verify it's present and matches the parent process.
	if got := tp.Env["HOME"]; got == "" {
		t.Fatalf("HOME should be passed through to agent env")
	}
}
