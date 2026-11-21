package main

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/configedit"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/testutil"
)

const cityConfigLockTestCity = "[workspace]\nname = \"city1\"\n\n[providers.claude]\nbase = \"builtin:claude\"\n\n[[agent]]\nname = \"worker\"\nprovider = \"claude\"\n"

// holdCityConfigLock takes the city config lock as another process would and
// returns its release, which the test cleanup also runs.
func holdCityConfigLock(t *testing.T, cityDir string) func() {
	t.Helper()
	release, err := configedit.LockCity(fsys.OSFS{}, cityDir, nil)
	if err != nil {
		t.Fatalf("LockCity: %v", err)
	}
	t.Cleanup(release)
	return release
}

// appendAgentToCityToml stands in for another process's edit: it appends an
// inline agent to city.toml.
func appendAgentToCityToml(t *testing.T, cityDir, name string) {
	t.Helper()
	path := filepath.Join(cityDir, "city.toml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading city.toml: %v", err)
	}
	data = append(data, []byte("\n[[agent]]\nname = \""+name+"\"\nprovider = \"claude\"\n")...)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("writing city.toml: %v", err)
	}
}

func newCityConfigLockControllerState(t *testing.T, cityDir string) *controllerState {
	t.Helper()
	t.Setenv("GC_BEADS", "file")
	cfg, err := config.Load(fsys.OSFS{}, filepath.Join(cityDir, "city.toml"))
	if err != nil {
		t.Fatalf("loading city.toml: %v", err)
	}
	cs := newControllerState(context.Background(), cfg, runtime.NewFake(), events.NewFake(), "city1", cityDir)
	cs.pokeCh = make(chan struct{}, 1)
	cs.configDirty = &atomic.Bool{}
	return cs
}

func loadAgentsByName(t *testing.T, cityDir string) map[string]config.Agent {
	t.Helper()
	cfg, err := config.Load(fsys.OSFS{}, filepath.Join(cityDir, "city.toml"))
	if err != nil {
		t.Fatalf("loading city.toml: %v", err)
	}
	agents := make(map[string]config.Agent, len(cfg.Agents))
	for _, a := range cfg.Agents {
		agents[a.Name] = a
	}
	return agents
}

func awaitErr(t *testing.T, ch <-chan error, what string) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatalf("%s did not finish after the city config lock was released", what)
		return nil
	}
}

// An API config mutation that finds another process mid-edit waits for that
// edit and applies its own change on top, so city.toml keeps both.
func TestControllerConfigMutationWaitsForCityConfigLock(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(cityConfigLockTestCity), 0o644); err != nil {
		t.Fatal(err)
	}
	cs := newCityConfigLockControllerState(t, cityDir)

	release := holdCityConfigLock(t, cityDir)
	done := make(chan error, 1)
	go func() { done <- cs.SuspendAgent("worker") }()
	testutil.AwaitFlockWaiters(t, configedit.CityLockFile(cityDir), 1)
	appendAgentToCityToml(t, cityDir, "builder")
	release()
	if err := awaitErr(t, done, "SuspendAgent"); err != nil {
		t.Fatalf("SuspendAgent: %v", err)
	}

	agents := loadAgentsByName(t, cityDir)
	if _, ok := agents["builder"]; !ok {
		t.Error("agent builder written by the other process was lost")
	}
	if !agents["worker"].Suspended {
		t.Error("worker is not suspended: the API mutation was lost")
	}
}
