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
// of its fan-out (hookWorkQueryStores) one after another, and each query runs
// each discovery tier as its own standalone `bd` process. In a healthy city with
// four rigs and no work anywhere, the number of serial `bd` launches times a
// healthy per-call cost (#5533 measured 0.3-0.9s per standalone `bd` against a
// responsive Dolt; 0.6s is the middle) must stay below the managed hook budget
// (defaultHookRunTimeout), so the hook answers before its deadline instead of
// being killed with exit 124.
//
// The fake bd sleeps per call, so the wall-clock assertion is real: a store leg
// that runs its ~10 launches concurrently with the other four answers in ~2s,
// while the serial fan-out the bug report traced needs ~10s.
func TestHookNoWorkDiscoveryFitsHookBudget(t *testing.T) {
	const perCallCost = 600 * time.Millisecond

	cityDir := t.TempDir()
	binDir := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "bd.log")
	bdScript := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$BD_LOG\"\nsleep 0.2\nprintf '[]'\n"
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
		t.Fatalf("stores = %d, want the city store plus four rig stores", len(stores))
	}
	runner := hookDiscoveryRunner(context.Background(), stores, shellWorkQueryWithEnvContext, hookVisibility{}, nil, io.Discard)
	started := time.Now()
	out, err := runner(agent.EffectiveWorkQuery(), cityDir)
	elapsed := time.Since(started)
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
	calls := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(calls) == 0 {
		t.Fatalf("bd log is empty: no discovery launches were recorded")
	}
	// Concurrent legs pay the LONGEST single store's serial chain, not the
	// five-store sum: at #5533's 0.6s per launch the serial fan-out cost ~30s
	// against the 15s budget, while each store's own chain is ~6s. Otherwise
	// the number of launches alone would overrun the budget forever, no
	// matter how the legs are scheduled.
	longestChain := time.Duration((len(calls)+len(stores)-1)/len(stores)) * perCallCost
	if longestChain >= defaultHookRunTimeout {
		t.Fatalf("no-work discovery launches %d bd processes across %d stores: the longest store leg's chain is %s at %s per launch, at or over the %s hook budget",
			len(calls), len(stores), longestChain, perCallCost, defaultHookRunTimeout)
	}
	// The serial sum of the five stores' queries (~50 launches at 0.2s real
	// per-call cost ≈ 10s) must not be paid: concurrent legs answer in the
	// slowest single store's time, and the wall clock must stay under half.
	// This is the assertion the serial fan-out (#5533's shape) cannot pass.
	serialSum := time.Duration(len(calls)) * 200 * time.Millisecond
	if elapsed >= serialSum/2 {
		t.Fatalf("discovery wall clock %s, want under half the %s five-store serial sum (concurrent store legs)", elapsed.Round(time.Millisecond), serialSum)
	}
}
