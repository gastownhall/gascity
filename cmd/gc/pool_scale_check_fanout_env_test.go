package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// TestEachProbePinsItsOwnBeadsDir is the core regression for ga-jrjkuo:
// cityScopedFanOutProbes must give each rig probe its own BEADS_DIR pinned to
// that rig's store, not the caller's (city) BEADS_DIR reused for every probe.
func TestEachProbePinsItsOwnBeadsDir(t *testing.T) {
	cityPath := t.TempDir()
	rigAPath := filepath.Join(cityPath, "riga")
	rigBPath := filepath.Join(cityPath, "rigb")
	cfg := &config.City{Rigs: []config.Rig{
		{Name: "riga", Path: rigAPath},
		{Name: "rigb", Path: rigBPath},
	}}
	agentCfg := &config.Agent{Name: "worker"}
	ownEnv := map[string]string{"BEADS_DIR": filepath.Join(cityPath, ".beads")}

	probes := cityScopedFanOutProbes(cityPath, cfg, agentCfg, cityPath, ownEnv, nil)

	wantFor := map[string]string{
		"city": filepath.Join(cityPath, ".beads"),
		"riga": filepath.Join(rigAPath, ".beads"),
		"rigb": filepath.Join(rigBPath, ".beads"),
	}
	for _, p := range probes {
		want, ok := wantFor[p.ref]
		if !ok {
			t.Fatalf("unexpected probe ref %q", p.ref)
		}
		if got := p.env["BEADS_DIR"]; got != want {
			t.Errorf("probe %q: BEADS_DIR = %q, want %q", p.ref, got, want)
		}
	}
}

// TestProbesDoNotShareOneEnvMap guards against every probe aliasing the same
// env map: mutating one probe's env must never be visible through another
// probe's env, since concurrent goroutines in evaluatePoolFanOutSum each read
// their own probe's env without synchronization.
func TestProbesDoNotShareOneEnvMap(t *testing.T) {
	cityPath := t.TempDir()
	cfg := &config.City{Rigs: []config.Rig{
		{Name: "riga", Path: filepath.Join(cityPath, "riga")},
	}}
	ownEnv := map[string]string{"BEADS_DIR": filepath.Join(cityPath, ".beads")}

	probes := cityScopedFanOutProbes(cityPath, cfg, &config.Agent{Name: "worker"}, cityPath, ownEnv, nil)
	if len(probes) != 2 {
		t.Fatalf("len(probes) = %d, want 2", len(probes))
	}
	probes[0].env["SENTINEL"] = "written-via-city-probe"
	if probes[1].env["SENTINEL"] == "written-via-city-probe" {
		t.Errorf("rig probe %q shares the city probe's env map", probes[1].ref)
	}
}

// TestProbeSubprocessResolvesItsOwnStore falsifies the (former) doc-comment
// premise that the probe command's working directory selects the store: bd
// resolves its store from BEADS_DIR, not cwd, so two probes with the same env
// but different dirs would resolve the SAME store. Asserted at the subprocess
// boundary rather than on the map so it stays a regression guard regardless
// of how the env is built.
func TestProbeSubprocessResolvesItsOwnStore(t *testing.T) {
	cityPath := t.TempDir()
	rigAPath := filepath.Join(cityPath, "riga")
	if err := os.MkdirAll(rigAPath, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{Rigs: []config.Rig{{Name: "riga", Path: rigAPath}}}
	ownEnv := map[string]string{"BEADS_DIR": filepath.Join(cityPath, ".beads")}

	probes := cityScopedFanOutProbes(cityPath, cfg, &config.Agent{Name: "worker"}, cityPath, ownEnv, nil)

	seen := map[string]string{}
	for _, p := range probes {
		out, err := shellScaleCheck(`printf '%s' "$BEADS_DIR"`, p.dir, p.env)
		if err != nil {
			t.Fatalf("probe %q: %v", p.ref, err)
		}
		seen[p.ref] = strings.TrimSpace(out)
	}
	if seen["city"] == seen["riga"] {
		t.Errorf("both probes resolved BEADS_DIR=%q: cwd does not select the store", seen["city"])
	}
}
