package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/account"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

// TestResolveTemplate_AccountInjection verifies that when an agent has
// account = "work1" configured and the account exists in the registry with
// a valid config_dir, resolveTemplate sets CLAUDE_CONFIG_DIR in the returned
// environment to the account's config_dir path.
func TestResolveTemplate_AccountInjection(t *testing.T) {
	cityPath := t.TempDir()
	configDir := filepath.Join(t.TempDir(), "work1-config")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}

	reg := account.TestRegistry(t,
		account.Account{Handle: "work1", ConfigDir: configDir},
	)

	params := &agentBuildParams{
		cityName:        "city",
		cityPath:        cityPath,
		workspace:       &config.Workspace{Provider: "test"},
		providers:       map[string]config.ProviderSpec{"test": {Command: "echo", PromptMode: "none"}},
		lookPath:        func(string) (string, error) { return "/bin/echo", nil },
		fs:              fsys.OSFS{},
		beaconTime:      time.Unix(0, 0),
		beadNames:       make(map[string]string),
		stderr:          io.Discard,
		accountRegistry: reg,
	}

	agent := &config.Agent{
		Name:    "coder",
		Account: "work1",
	}
	tp, err := resolveTemplate(params, agent, agent.QualifiedName(), nil)
	if err != nil {
		t.Fatalf("resolveTemplate: %v", err)
	}

	got := tp.Env["CLAUDE_CONFIG_DIR"]
	if got != configDir {
		t.Fatalf("CLAUDE_CONFIG_DIR = %q, want %q", got, configDir)
	}
}

// TestResolveTemplate_PreFlightFail verifies that when an agent's account has
// a config_dir that does not exist, resolveTemplate returns an error before
// creating any session. The error should mention the agent's qualified name.
func TestResolveTemplate_PreFlightFail(t *testing.T) {
	cityPath := t.TempDir()

	reg := account.TestRegistry(t,
		account.Account{Handle: "work1", ConfigDir: "/nonexistent/config/dir"},
	)

	params := &agentBuildParams{
		cityName:        "city",
		cityPath:        cityPath,
		workspace:       &config.Workspace{Provider: "test"},
		providers:       map[string]config.ProviderSpec{"test": {Command: "echo", PromptMode: "none"}},
		lookPath:        func(string) (string, error) { return "/bin/echo", nil },
		fs:              fsys.OSFS{},
		beaconTime:      time.Unix(0, 0),
		beadNames:       make(map[string]string),
		stderr:          io.Discard,
		accountRegistry: reg,
	}

	agent := &config.Agent{
		Name:    "coder",
		Account: "work1",
	}
	_, err := resolveTemplate(params, agent, agent.QualifiedName(), nil)
	if err == nil {
		t.Fatal("expected error for missing config_dir, got nil")
	}
	if !strings.Contains(err.Error(), "coder") {
		t.Fatalf("error should mention agent name %q, got: %v", "coder", err)
	}
}

// TestResolveTemplate_NoAccount verifies that when no account is configured
// (zero-value registry, no env/flag/config account), resolveTemplate does not
// inject CLAUDE_CONFIG_DIR into the environment. This ensures backward
// compatibility — existing setups without accounts continue to work unchanged.
func TestResolveTemplate_NoAccount(t *testing.T) {
	cityPath := t.TempDir()

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
		// accountRegistry is zero-value (empty Registry) — no accounts.
		// accountFlag is zero-value ("") — no --account flag.
	}

	agent := &config.Agent{
		Name: "coder",
		// Account is "" — no account configured on agent.
	}
	tp, err := resolveTemplate(params, agent, agent.QualifiedName(), nil)
	if err != nil {
		t.Fatalf("resolveTemplate: %v", err)
	}

	if val, ok := tp.Env["CLAUDE_CONFIG_DIR"]; ok {
		t.Fatalf("CLAUDE_CONFIG_DIR should not be set, got %q", val)
	}
}

// TestResolveTemplate_AccountFlag verifies that the --account flag (via
// agentBuildParams.accountFlag) overrides the agent's config Account field
// when resolving the account for CLAUDE_CONFIG_DIR injection.
func TestResolveTemplate_AccountFlag(t *testing.T) {
	cityPath := t.TempDir()
	configDir1 := filepath.Join(t.TempDir(), "work1-config")
	configDir2 := filepath.Join(t.TempDir(), "work2-config")
	if err := os.MkdirAll(configDir1, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(configDir2, 0o755); err != nil {
		t.Fatal(err)
	}

	reg := account.TestRegistry(t,
		account.Account{Handle: "work1", ConfigDir: configDir1},
		account.Account{Handle: "work2", ConfigDir: configDir2},
	)

	params := &agentBuildParams{
		cityName:        "city",
		cityPath:        cityPath,
		workspace:       &config.Workspace{Provider: "test"},
		providers:       map[string]config.ProviderSpec{"test": {Command: "echo", PromptMode: "none"}},
		lookPath:        func(string) (string, error) { return "/bin/echo", nil },
		fs:              fsys.OSFS{},
		beaconTime:      time.Unix(0, 0),
		beadNames:       make(map[string]string),
		stderr:          io.Discard,
		accountRegistry: reg,
		accountFlag:     "work2", // Flag overrides agent config.
	}

	agent := &config.Agent{
		Name:    "coder",
		Account: "work1", // Agent config says work1, but flag says work2.
	}
	tp, err := resolveTemplate(params, agent, agent.QualifiedName(), nil)
	if err != nil {
		t.Fatalf("resolveTemplate: %v", err)
	}

	got := tp.Env["CLAUDE_CONFIG_DIR"]
	if got != configDir2 {
		t.Fatalf("CLAUDE_CONFIG_DIR = %q, want %q (flag work2 should override config work1)", got, configDir2)
	}
}
