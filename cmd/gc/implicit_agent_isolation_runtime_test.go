package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// implicitWorktreeCityConfig returns an in-memory worktree-mode city with a
// rig-scoped and a city-scoped implicit "claude" agent on cityProvider.
func implicitWorktreeCityConfig(cityProvider string) *config.City {
	return &config.City{
		Workspace: config.Workspace{Name: "city", ImplicitAgentIsolation: config.ImplicitAgentIsolationWorktree},
		Session:   config.SessionConfig{Provider: cityProvider},
		Rigs:      []config.Rig{{Name: "demo", Path: "/srv/demo"}},
		Agents: []config.Agent{
			{Name: "claude", Implicit: true},
			{Name: "claude", Dir: "demo", Implicit: true},
		},
	}
}

func assertImplicitWorktreeRuntimeError(t *testing.T, err error, provider string) {
	t.Helper()
	if err == nil {
		t.Fatalf("provider %q accepted, want the implicit_agent_isolation runtime error", provider)
	}
	for _, want := range []string{
		`workspace.implicit_agent_isolation = "worktree"`,
		"(tmux, herdr)",
		`agent "demo/claude"`,
		`uses "` + provider + `"`,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not mention %q", err, want)
		}
	}
}

// AC-6: worktree mode accepts only runtimes that run pre_start on the host,
// judged on each rig-scoped implicit agent's effective session provider.
func TestValidateImplicitAgentIsolationRuntime(t *testing.T) {
	t.Setenv("GC_SESSION", "")
	for _, provider := range []string{"", "tmux", "herdr"} {
		if err := validateImplicitAgentIsolationRuntime(implicitWorktreeCityConfig(provider)); err != nil {
			t.Errorf("provider %q rejected: %v", provider, err)
		}
	}
	for _, provider := range []string{"subprocess", "acp", "t3bridge", "k8s", "hybrid", "exec:/opt/runtime.sh"} {
		assertImplicitWorktreeRuntimeError(t, validateImplicitAgentIsolationRuntime(implicitWorktreeCityConfig(provider)), provider)
	}

	t.Run("agent session override", func(t *testing.T) {
		cfg := implicitWorktreeCityConfig("tmux")
		cfg.Agents[1].Session = "acp"
		assertImplicitWorktreeRuntimeError(t, validateImplicitAgentIsolationRuntime(cfg), "acp")
	})
	t.Run("GC_SESSION overrides the configured provider", func(t *testing.T) {
		t.Setenv("GC_SESSION", "subprocess")
		assertImplicitWorktreeRuntimeError(t, validateImplicitAgentIsolationRuntime(implicitWorktreeCityConfig("tmux")), "subprocess")
		t.Setenv("GC_SESSION", "tmux")
		if err := validateImplicitAgentIsolationRuntime(implicitWorktreeCityConfig("subprocess")); err != nil {
			t.Fatalf("GC_SESSION=tmux rejected: %v", err)
		}
	})
	t.Run("shared mode", func(t *testing.T) {
		cfg := implicitWorktreeCityConfig("subprocess")
		cfg.Workspace.ImplicitAgentIsolation = config.ImplicitAgentIsolationShared
		if err := validateImplicitAgentIsolationRuntime(cfg); err != nil {
			t.Fatalf("shared mode rejected: %v", err)
		}
	})
	t.Run("only city-scoped and explicit agents", func(t *testing.T) {
		cfg := implicitWorktreeCityConfig("subprocess")
		cfg.Agents[1].Implicit = false
		if err := validateImplicitAgentIsolationRuntime(cfg); err != nil {
			t.Fatalf("non-implicit rig agent rejected: %v", err)
		}
	})
}

// implicitWorktreeCityTOMLTail is the city.toml tail of a worktree-mode
// city: the "claude" provider on the subprocess runtime and rig "demo".
const implicitWorktreeCityTOMLTail = "\n[session]\nprovider = \"subprocess\"\n\n[providers.claude]\nbase = \"builtin:claude\"\n\n[[rigs]]\nname = \"demo\"\n"

// bindImplicitWorktreeCity gives a city with implicitWorktreeCityTOMLTail the
// core pack import, its materialized cache and rig "demo"'s site binding.
func bindImplicitWorktreeCity(t *testing.T, cityPath, workspaceName string) {
	t.Helper()
	writeBuiltinImportsFixture(t, cityPath, "core")
	materializeBuiltinPacksForTest(t, cityPath)
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	site := "workspace_name = \"" + workspaceName + "\"\n\n[[rig]]\nname = \"demo\"\npath = \"" + filepath.Join(cityPath, "repos", "demo") + "\"\n"
	if err := os.WriteFile(filepath.Join(cityPath, ".gc", "site.toml"), []byte(site), 0o644); err != nil {
		t.Fatal(err)
	}
}

// AC-6: the controller rejects a reload into worktree mode on subprocess, so
// the running controller keeps its last good config.
func TestTryReloadConfigRejectsImplicitWorktreeOnSubprocess(t *testing.T) {
	t.Setenv("GC_SESSION", "")
	cityDir := t.TempDir()
	cityTOML := "[workspace]\nname = \"city\"\nimplicit_agent_isolation = \"worktree\"\n" + implicitWorktreeCityTOMLTail
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(cityTOML), 0o644); err != nil {
		t.Fatal(err)
	}
	bindImplicitWorktreeCity(t, cityDir, "city")
	// A reload error is what keeps the controller on its last good config;
	// any result returned beside it carries only warnings to report.
	_, err := tryReloadConfig(filepath.Join(cityDir, "city.toml"), "city", cityDir)
	assertImplicitWorktreeRuntimeError(t, err, "subprocess")
}

// AC-6: gc start refuses worktree mode on subprocess before any init.
func TestStartStandaloneRejectsImplicitWorktreeOnSubprocess(t *testing.T) {
	cityPath, opsLog := newForegroundStartLockCity(t, "0")
	t.Setenv("GC_SESSION", "")
	tomlPath := filepath.Join(cityPath, "city.toml")
	data, err := os.ReadFile(tomlPath)
	if err != nil {
		t.Fatal(err)
	}
	body := strings.Replace(string(data), "name = \"lock-order-city\"\n",
		"name = \"lock-order-city\"\nimplicit_agent_isolation = \"worktree\"\n", 1)
	if err := os.WriteFile(tomlPath, []byte(body+implicitWorktreeCityTOMLTail), 0o644); err != nil {
		t.Fatal(err)
	}
	bindImplicitWorktreeCity(t, cityPath, "lock-order-city")

	var stdout, stderr bytes.Buffer
	if code := doStartStandalone([]string{cityPath}, false, &stdout, &stderr); code != 1 {
		t.Fatalf("doStartStandalone exit = %d, want 1\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), `requires a runtime that runs pre_start on the host (tmux, herdr); agent "demo/claude" uses "subprocess"`) {
		t.Fatalf("stderr = %q, want the implicit_agent_isolation runtime error", stderr.String())
	}
	assertRefusedCityUntouched(t, cityPath, opsLog)
}

// AC-6: the supervisor refuses to prepare a worktree-mode city on subprocess.
func TestPrepareCityForSupervisorRejectsImplicitWorktreeOnSubprocess(t *testing.T) {
	t.Setenv("GC_SESSION", "")
	err := prepareCityForSupervisor(t.TempDir(), "city", implicitWorktreeCityConfig("subprocess"), io.Discard, nil)
	assertImplicitWorktreeRuntimeError(t, err, "subprocess")
}
