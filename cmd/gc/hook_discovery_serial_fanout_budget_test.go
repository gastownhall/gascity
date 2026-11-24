package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
)

// TestHookNoWorkDiscoveryFitsHookBudget pins issue #5533: an idle `gc hook`
// discovers work by running the agent's default work query against every store
// of its fan-out (hookWorkQueryStores), and each query runs each discovery tier
// as its own standalone `bd` process. In a healthy city with four rigs and no
// work anywhere, discovery must cost about its slowest store, not the sum of
// all five, so the hook answers well inside the managed hook budget
// (defaultHookRunTimeout) instead of being killed with exit 124.
//
// The fake bd sleeps a fixed perCall on every call. Read one store at a time,
// five stores of about ten launches each take about ten seconds; read
// concurrently they take about two.
func TestHookNoWorkDiscoveryFitsHookBudget(t *testing.T) {
	const (
		perCall = 200 * time.Millisecond
		// healthyCallCost is #5533's measured per-launch cost of a standalone
		// bd against a responsive Dolt (0.3-0.9s): the middle of that range.
		healthyCallCost = 600 * time.Millisecond
		// maxLaunchesPerStore ratchets one store's chain of bd launches on the
		// no-work path; cutting it further is ga-4qdfn's work.
		maxLaunchesPerStore = 10
		maxWall             = 5 * time.Second
	)

	cityDir := t.TempDir()
	binDir := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "bd.log")
	bdScript := "#!/bin/sh\nprintf '%s\\n' \"$PWD\" >> \"$BD_LOG\"\nsleep 0.2\nprintf '[]'\n"
	if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte(bdScript), 0o755); err != nil {
		t.Fatalf("write fake bd: %v", err)
	}

	var rigs []config.Rig
	for _, name := range []string{"sl", "syst", "te", "wr"} {
		path := filepath.Join(cityDir, "rigs", name)
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatalf("mkdir rig %s: %v", name, err)
		}
		rigs = append(rigs, config.Rig{Name: name, Path: path})
	}
	cfg := &config.City{Rigs: rigs}
	agent := &config.Agent{Name: "worker", Scope: "city"}

	storeEnv := map[string]string{
		"PATH":            binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"BD_LOG":          logPath,
		"GC_SESSION_ID":   "gc-session-1",
		"GC_SESSION_NAME": "worker-1",
		"GC_ALIAS":        "worker",
	}
	prev := hookStoreQueryEnv
	hookStoreQueryEnv = func(_ string, _ *config.City, _ *config.Agent) (map[string]string, error) {
		env := make(map[string]string, len(storeEnv))
		for k, v := range storeEnv {
			env[k] = v
		}
		return env, nil
	}
	t.Cleanup(func() { hookStoreQueryEnv = prev })

	stores := hookWorkQueryStores(cityDir, cfg, agent, "worker", cityDir, mergeRuntimeEnv(os.Environ(), storeEnv), nil, nil)
	if len(stores) != 5 {
		t.Fatalf("stores = %d, want the city store and four rig stores", len(stores))
	}
	runner := hookDiscoveryRunner(context.Background(), stores, shellWorkQueryWithEnvContext, hookVisibility{}, nil, io.Discard)
	start := time.Now()
	out, err := runner(agent.EffectiveWorkQuery(), cityDir)
	wall := time.Since(start)
	if err != nil {
		t.Fatalf("discovery: %v", err)
	}
	if strings.TrimSpace(out) != "[]" {
		t.Fatalf("discovery output = %q, want no-work []", out)
	}

	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read bd log: %v", err)
	}
	perStore := map[string]int{}
	for _, dir := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		perStore[dir]++
	}
	if len(perStore) != len(stores) {
		t.Fatalf("bd ran in %d store dirs, want %d: %v", len(perStore), len(stores), perStore)
	}
	slowest := 0
	for dir, n := range perStore {
		if n > maxLaunchesPerStore {
			t.Errorf("store %s launched bd %d times, want at most %d", dir, n, maxLaunchesPerStore)
		}
		slowest = max(slowest, n)
	}
	if cost := time.Duration(slowest) * healthyCallCost; cost >= defaultHookRunTimeout {
		t.Errorf("the slowest store's %d launches at %s each cost %s, at or over the %s hook budget", slowest, healthyCallCost, cost, defaultHookRunTimeout)
	}
	if wall >= maxWall {
		t.Fatalf("no-work discovery across %d stores took %s at %s per bd call (launches per store %v), want under %s: the stores are read one after another",
			len(stores), wall.Round(time.Millisecond), perCall, perStore, maxWall)
	}
}
