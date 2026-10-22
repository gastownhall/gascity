package main

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// mkWorktreeModeCity builds a loaded-shaped city in worktree isolation mode:
// one rig, one injected rig-scoped implicit provider agent.
func mkWorktreeModeCity(t *testing.T) *config.City {
	t.Helper()
	cfg := &config.City{
		Workspace: config.Workspace{ImplicitAgentIsolation: "worktree"},
		Providers: map[string]config.ProviderSpec{"claude": {}},
		Rigs:      []config.Rig{{Name: "demo", Path: "/tmp/demo"}},
	}
	config.InjectImplicitAgents(cfg)
	return cfg
}

// TestValidateImplicitAgentIsolationRuntimeRejectsNonHostProviders covers the
// pre-expansion gate from #1181's worktree mode: the injected pre_start runs
// on the host, so only providers that execute pre_start on the host may
// carry a rig-scoped implicit pool in worktree mode.
func TestValidateImplicitAgentIsolationRuntimeRejectsNonHostProviders(t *testing.T) {
	for _, provider := range []string{
		"subprocess", "acp", "t3bridge", "k8s", "hybrid", "exec:/bin/sh",
	} {
		t.Run(provider, func(t *testing.T) {
			cfg := mkWorktreeModeCity(t)
			cfg.Session.Provider = provider
			err := validateImplicitAgentIsolationRuntime(cfg)
			if err == nil {
				t.Fatalf("want an error for provider %q", provider)
			}
			for _, want := range []string{
				`workspace.implicit_agent_isolation = "worktree"`,
				"pre_start",
				"demo/claude",
				`"` + provider + `"`,
			} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q missing %q", err.Error(), want)
				}
			}
		})
	}
}

// TestValidateImplicitAgentIsolationRuntimeAllowsHostProviders pins the
// allowed set: unset (the default), tmux and herdr all run pre_start on the
// host.
func TestValidateImplicitAgentIsolationRuntimeAllowsHostProviders(t *testing.T) {
	for _, provider := range []string{"", "tmux", "herdr"} {
		t.Run(provider, func(t *testing.T) {
			cfg := mkWorktreeModeCity(t)
			cfg.Session.Provider = provider
			if err := validateImplicitAgentIsolationRuntime(cfg); err != nil {
				t.Fatalf("provider %q: %v", provider, err)
			}
		})
	}
}

// TestValidateImplicitAgentIsolationRuntimeAgentOverride proves the gate keys
// on the effective provider: an agent-level session override to a non-host
// provider is rejected even when the city provider is fine, and vice versa.
func TestValidateImplicitAgentIsolationRuntimeAgentOverride(t *testing.T) {
	cfg := mkWorktreeModeCity(t)
	cfg.Session.Provider = "tmux"
	if err := validateImplicitAgentIsolationRuntime(cfg); err != nil {
		t.Fatalf("city tmux: %v", err)
	}
	implicit := findImplicitRigAgentForRuntimeTest(cfg, "demo")
	implicit.Session = "subprocess"
	err := validateImplicitAgentIsolationRuntime(cfg)
	if err == nil {
		t.Fatal("want an error when the agent overrides to a non-host provider")
	}
	if !strings.Contains(err.Error(), "subprocess") {
		t.Fatalf("error = %q, want it to name the agent's provider", err.Error())
	}

	// A host override over a non-host city provider passes.
	implicit.Session = "herdr"
	cfg.Session.Provider = "k8s"
	if err := validateImplicitAgentIsolationRuntime(cfg); err != nil {
		t.Fatalf("agent herdr over city k8s: %v", err)
	}
}

// TestValidateImplicitAgentIsolationRuntimeGCSession proves the GC_SESSION
// env override participates in the gate, matching the city provider rule.
func TestValidateImplicitAgentIsolationRuntimeGCSession(t *testing.T) {
	cfg := mkWorktreeModeCity(t)
	cfg.Session.Provider = "subprocess"
	t.Setenv("GC_SESSION", "tmux")
	if err := validateImplicitAgentIsolationRuntime(cfg); err != nil {
		t.Fatalf("GC_SESSION=tmux must rescue a subprocess city: %v", err)
	}

	t.Setenv("GC_SESSION", "subprocess")
	cfg.Session.Provider = "tmux"
	err := validateImplicitAgentIsolationRuntime(cfg)
	if err == nil {
		t.Fatal("GC_SESSION=subprocess must reject the gate even with a tmux city provider")
	}
}

// TestValidateImplicitAgentIsolationRuntimeSkipsIrrelevantShapes covers the
// silent cases: shared mode (the default), explicit agents, and city-scoped
// implicit agents never trip the gate.
func TestValidateImplicitAgentIsolationRuntimeSkipsIrrelevantShapes(t *testing.T) {
	t.Run("shared mode ignores non-host provider", func(t *testing.T) {
		for _, mode := range []string{"", "shared"} {
			cfg := &config.City{
				Workspace: config.Workspace{ImplicitAgentIsolation: mode},
				Rigs:      []config.Rig{{Name: "demo", Path: "/tmp/demo"}},
			}
			config.InjectImplicitAgents(cfg)
			cfg.Session.Provider = "subprocess"
			if err := validateImplicitAgentIsolationRuntime(cfg); err != nil {
				t.Fatalf("mode %q: %v", mode, err)
			}
		}
	})
	t.Run("explicit agent in worktree mode is its author's call", func(t *testing.T) {
		cfg := &config.City{
			Workspace: config.Workspace{ImplicitAgentIsolation: "worktree"},
			Rigs:      []config.Rig{{Name: "demo", Path: "/tmp/demo"}},
			Agents:    []config.Agent{{Name: "worker", Dir: "demo", Session: "subprocess"}},
		}
		if err := validateImplicitAgentIsolationRuntime(cfg); err != nil {
			t.Fatalf("explicit agent: %v", err)
		}
	})
	t.Run("city-scoped implicit agent has no injected pre_start", func(t *testing.T) {
		cfg := &config.City{
			Workspace: config.Workspace{ImplicitAgentIsolation: "worktree"},
			Providers: map[string]config.ProviderSpec{"claude": {}},
		}
		config.InjectImplicitAgents(cfg)
		cfg.Session.Provider = "subprocess"
		if err := validateImplicitAgentIsolationRuntime(cfg); err != nil {
			t.Fatalf("city-scoped implicit agent: %v", err)
		}
	})
	t.Run("nil config", func(t *testing.T) {
		if err := validateImplicitAgentIsolationRuntime(nil); err != nil {
			t.Fatalf("nil config: %v", err)
		}
	})
}

// findImplicitRigAgentForRuntimeTest returns the injected rig-scoped implicit
// agent for the named rig, assuming exactly one provider.
func findImplicitRigAgentForRuntimeTest(cfg *config.City, rigName string) *config.Agent {
	for i := range cfg.Agents {
		a := &cfg.Agents[i]
		if a.Implicit && a.Dir == rigName {
			return a
		}
	}
	return nil
}
