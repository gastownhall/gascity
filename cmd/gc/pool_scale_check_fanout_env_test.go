package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// effectiveBeadsDir reproduces what runShellCommand hands the probe process:
// cmd.Env = mergeRuntimeEnv(os.Environ(), env). mergeRuntimeEnv STRIPS any
// inherited BEADS_DIR and re-applies the override, so this is the one and
// only BEADS_DIR the probe's bd sees.
func effectiveBeadsDir(env map[string]string) string {
	for _, kv := range mergeRuntimeEnv(os.Environ(), env) {
		if strings.HasPrefix(kv, "BEADS_DIR=") {
			return strings.TrimPrefix(kv, "BEADS_DIR=")
		}
	}
	return ""
}

func TestEachFanOutProbePinsItsOwnStore(t *testing.T) {
	cityPath := t.TempDir()
	rigAPath := filepath.Join(cityPath, "riga")
	rigBPath := filepath.Join(cityPath, "rigb")
	cfg := &config.City{Rigs: []config.Rig{
		{Name: "riga", Path: rigAPath},
		{Name: "rigb", Path: rigBPath},
	}}
	agentCfg := &config.Agent{Name: "worker"} // Dir == "" => city-scoped

	cityEnv := map[string]string{
		"BEADS_DIR":   filepath.Join(cityPath, ".beads"),
		"GC_RIG":      "",
		"GC_RIG_ROOT": "",
	}

	probes := cityScopedFanOutProbes(cityPath, cfg, agentCfg, cityPath, cityEnv, map[string]bool{})

	want := map[string]string{
		"city": filepath.Join(cityPath, ".beads"),
		"riga": filepath.Join(rigAPath, ".beads"),
		"rigb": filepath.Join(rigBPath, ".beads"),
	}
	for _, p := range probes {
		if got := effectiveBeadsDir(p.env); got != want[p.ref] {
			t.Errorf("probe %q: effective BEADS_DIR = %q, want %q", p.ref, got, want[p.ref])
		}
	}
}

func TestFanOutSumDoesNotMultiplyCityDemand(t *testing.T) {
	cityPath := t.TempDir()
	rigAPath := filepath.Join(cityPath, "riga")
	rigBPath := filepath.Join(cityPath, "rigb")
	cfg := &config.City{Rigs: []config.Rig{
		{Name: "riga", Path: rigAPath},
		{Name: "rigb", Path: rigBPath},
	}}
	agentCfg := &config.Agent{Name: "worker"}
	cityEnv := map[string]string{"BEADS_DIR": filepath.Join(cityPath, ".beads")}

	// The runner models bd honestly: it resolves the store from the
	// effective BEADS_DIR, NOT from dir (which is what every existing
	// fan-out test assumes, and why they all pass on the buggy code).
	countsByStore := map[string]string{
		filepath.Join(cityPath, ".beads"): "7",
		filepath.Join(rigAPath, ".beads"): "3",
		filepath.Join(rigBPath, ".beads"): "5",
	}
	runner := func(_, _ string, env map[string]string) (string, error) {
		n, ok := countsByStore[effectiveBeadsDir(env)]
		if !ok {
			return "0", nil
		}
		return n, nil
	}

	probes := cityScopedFanOutProbes(cityPath, cfg, agentCfg, cityPath, cityEnv, map[string]bool{})
	sem := make(chan struct{}, len(probes))
	sp := scaleParams{Min: 0, Max: 100, Check: "check"}

	got, errs := evaluatePoolFanOutSum("worker", sp, probes, runner, sem, true)
	if len(errs) != 0 {
		t.Fatalf("errs = %v, want none", errs)
	}
	if got != 15 {
		t.Fatalf("federated demand = %d, want 15 (7 city + 3 riga + 5 rigb); "+
			"%d = 3 x 7 means every leg read the CITY store", got, got)
	}
}
