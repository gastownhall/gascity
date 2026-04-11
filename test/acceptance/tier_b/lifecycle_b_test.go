//go:build acceptance_b

// Tier B lifecycle acceptance tests.
//
// These start real cities with the subprocess session provider and
// verify agent lifecycle behavior: environment propagation, drain-ack,
// worktree pre_start, and order execution.
//
// Requires: gc binary, bd binary, subprocess provider.
// Does NOT require: tmux, dolt, inference API keys.
// Expected duration: ~2-5 minutes.
package tierb_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	helpers "github.com/gastownhall/gascity/test/acceptance/helpers"
)

var testEnvB *helpers.Env

func TestMain(m *testing.M) {
	tmpDir, err := os.MkdirTemp("", "gc-acceptance-b-*")
	if err != nil {
		panic("acceptance-b: creating temp dir: " + err.Error())
	}
	defer os.RemoveAll(tmpDir)

	gcBinary := helpers.BuildGC(tmpDir)

	gcHome := filepath.Join(tmpDir, "gc-home")
	if err := os.MkdirAll(gcHome, 0o755); err != nil {
		panic("acceptance-b: creating GC_HOME: " + err.Error())
	}
	runtimeDir := filepath.Join(tmpDir, "runtime")
	if err := os.MkdirAll(runtimeDir, 0o755); err != nil {
		panic("acceptance-b: creating XDG_RUNTIME_DIR: " + err.Error())
	}
	if err := helpers.WriteSupervisorConfig(gcHome); err != nil {
		panic("acceptance-b: " + err.Error())
	}

	testEnvB = helpers.NewEnv(gcBinary, gcHome, runtimeDir)

	code := m.Run()

	// Best-effort supervisor stop.
	helpers.RunGC(testEnvB, "", "supervisor", "stop") //nolint:errcheck
	os.Exit(code)
}

// TestLifecycle_AgentGetsCorrectEnv starts a city with a single agent
// that dumps its environment to a report file. Verifies all required
// GC_* env vars are present and correct.
func TestLifecycle_AgentGetsCorrectEnv(t *testing.T) {
	c := helpers.NewCity(t, testEnvB)
	c.Init("claude")

	reportCmd := c.WriteReportScript("envtest", true)
	c.WriteE2EConfig([]helpers.E2EAgent{
		{Name: "envtest", StartCommand: reportCmd},
	})

	c.StartWithSupervisor()

	env := c.WaitForReport("envtest", helpers.ReportTimeout())

	// Core env vars must be present.
	required := []string{
		"GC_CITY", "GC_CITY_PATH",
		"GC_CITY_RUNTIME_DIR", "GC_AGENT", "GC_SESSION_NAME",
		"GC_TEMPLATE",
	}
	for _, key := range required {
		if env[key] == "" {
			t.Errorf("%s is empty or missing", key)
		}
	}

	// GC_CITY_PATH must equal the city directory.
	if env["GC_CITY_PATH"] != c.Dir {
		t.Errorf("GC_CITY_PATH = %q, want %q", env["GC_CITY_PATH"], c.Dir)
	}

	// GT_ROOT must equal the city directory (not a rig root).
	if env["GT_ROOT"] != c.Dir {
		t.Errorf("GT_ROOT = %q, want %q", env["GT_ROOT"], c.Dir)
	}

	// GC_AGENT must match the configured agent name.
	if env["GC_AGENT"] != "envtest" {
		t.Errorf("GC_AGENT = %q, want %q", env["GC_AGENT"], "envtest")
	}
}

// TestLifecycle_RigAgentGetsBeadsDir starts a city with a rig-scoped
// agent and verifies BEADS_DIR is set to the rig's .beads directory.
// This is the end-to-end regression test for Bug 3 (2026-03-18).
func TestLifecycle_RigAgentGetsBeadsDir(t *testing.T) {
	c := helpers.NewCity(t, testEnvB)
	c.Init("claude")

	// Create a rig directory.
	rigDir := filepath.Join(c.Dir, "myrig")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}

	reportCmd := c.WriteReportScript("worker", true)

	// Write config with a rig-scoped agent.
	cityName := filepath.Base(c.Dir)
	config := "[workspace]\nname = \"" + cityName + "\"\n" +
		"\n[beads]\nprovider = \"file\"\n" +
		"\n[[rigs]]\nname = \"myrig\"\npath = \"" + rigDir + "\"\n" +
		"\n[[agent]]\nname = \"worker\"\ndir = \"myrig\"\n" +
		"max_active_sessions = 1\n" +
		"start_command = \"" + reportCmd + "\"\n"
	c.WriteConfig(config)

	c.StartWithSupervisor()

	env := c.WaitForReport("worker", helpers.ReportTimeout())

	// BEADS_DIR must point to the rig's .beads.
	wantBeads := filepath.Join(rigDir, ".beads")
	if env["BEADS_DIR"] != wantBeads {
		t.Errorf("BEADS_DIR = %q, want %q — Bug 3 regression", env["BEADS_DIR"], wantBeads)
	}

	// GC_RIG must be set.
	if env["GC_RIG"] != "myrig" {
		t.Errorf("GC_RIG = %q, want %q", env["GC_RIG"], "myrig")
	}

	// GC_RIG_ROOT must be the rig directory.
	if env["GC_RIG_ROOT"] != rigDir {
		t.Errorf("GC_RIG_ROOT = %q, want %q", env["GC_RIG_ROOT"], rigDir)
	}

	// GT_ROOT must be the CITY root, not the rig root (Bug 2 regression).
	if env["GT_ROOT"] != c.Dir {
		t.Errorf("GT_ROOT = %q, want city root %q, not rig root — Bug 2 regression",
			env["GT_ROOT"], c.Dir)
	}

	// GC_CITY_PATH must be the city root.
	if env["GC_CITY_PATH"] != c.Dir {
		t.Errorf("GC_CITY_PATH = %q, want %q", env["GC_CITY_PATH"], c.Dir)
	}
}

// TestLifecycle_DrainAckStopsSession verifies that an agent calling
// gc runtime drain-ack causes the reconciler to stop the session
// without immediately re-waking it.
func TestLifecycle_DrainAckStopsSession(t *testing.T) {
	c := helpers.NewCity(t, testEnvB)
	c.Init("claude")

	// Agent that reports then drain-acks.
	reportCmd := c.WriteReportScript("draintest", true)
	c.WriteE2EConfig([]helpers.E2EAgent{
		{Name: "draintest", StartCommand: reportCmd},
	})

	c.StartWithSupervisor()

	// Wait for the agent to run and report.
	c.WaitForReport("draintest", helpers.ReportTimeout())

	// The agent called drain-ack and exited. Verify the session is
	// eventually stopped by checking gc status.
	deadline := helpers.ReportTimeout()
	stopped := c.WaitForCondition(func() bool {
		out, err := c.GC("status", "--city", c.Dir)
		if err != nil {
			return false // status command failed, keep polling
		}
		// Agent must be explicitly in a stopped/asleep state, or absent
		// from the output entirely. Don't match generic substrings.
		lines := strings.Split(out, "\n")
		for _, line := range lines {
			if strings.Contains(line, "draintest") {
				return strings.Contains(line, "asleep") || strings.Contains(line, "stopped")
			}
		}
		// Agent not mentioned at all — it was stopped and cleaned up.
		return true
	}, deadline)

	if !stopped {
		out, _ := c.GC("status", "--city", c.Dir)
		t.Fatalf("agent not stopped after drain-ack within %s:\n%s", deadline, out)
	}
}

// TestLifecycle_PackMaterializationOnStart verifies that gc start
// materializes gastown packs even if they were deleted after init.
// This is the end-to-end regression test for Bug 4 (2026-03-18).
func TestLifecycle_PackMaterializationOnStart(t *testing.T) {
	c := helpers.NewCity(t, testEnvB)
	c.InitFrom(filepath.Join(helpers.ExamplesDir(), "gastown"))

	// gc init --from now completes startup registration, so stop and
	// unregister before exercising the explicit gc start path below.
	if out, err := c.GC("stop", c.Dir); err != nil {
		t.Fatalf("gc stop after init-from failed: %v\n%s", err, out)
	}
	if out, err := c.GC("unregister", c.Dir); err != nil {
		t.Fatalf("gc unregister after init-from failed: %v\n%s", err, out)
	}

	systemGastownPack := filepath.Join(".gc", "system", "packs", "gastown", "pack.toml")
	systemMaintenancePack := filepath.Join(".gc", "system", "packs", "maintenance", "pack.toml")

	// Verify managed system packs exist after init.
	if !c.HasFile(systemGastownPack) {
		t.Fatal(".gc/system/packs/gastown/pack.toml not materialized after init")
	}

	// Delete managed packs to simulate partial init failure.
	if err := os.RemoveAll(filepath.Join(c.Dir, ".gc", "system", "packs")); err != nil {
		t.Fatal(err)
	}

	// gc start registers with the supervisor, which materializes managed packs
	// during registration (before config load).
	out, err := c.GC("start", c.Dir)
	if err != nil {
		t.Logf("gc start returned error (may be expected): %v\n%s", err, out)
	}

	// Wait for the supervisor to materialize managed packs (reconcile tick).
	found := c.WaitForCondition(func() bool {
		return c.HasFile(systemGastownPack)
	}, 60*time.Second)

	if !found {
		t.Fatal(".gc/system/packs/gastown/pack.toml not re-materialized on start — Bug 4 regression")
	}
	if !c.HasFile(systemMaintenancePack) {
		t.Fatal(".gc/system/packs/maintenance/pack.toml not re-materialized on start")
	}
}
