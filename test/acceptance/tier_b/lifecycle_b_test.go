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
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
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
	helpers.RunGC(testEnvB, "", "supervisor", "stop", "--wait") //nolint:errcheck
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

	// Register the rig through the CLI, then add a rig-scoped agent. The
	// [[named_session]] entry
	// reserves a canonical session so the reconciler materializes and
	// starts the agent even without queued work — post-PR-666 templates
	// are lazy.
	c.RigAdd(rigDir, "")
	c.WriteV2AgentDir("worker",
		`dir = "myrig"`,
		"max_active_sessions = 1",
		"start_command = "+strconv.Quote(reportCmd),
	)
	c.AppendToPack("\n[[named_session]]\ntemplate = \"worker\"\ndir = \"myrig\"\nmode = \"always\"\n")

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

// TestLifecycle_DrainAckResponsiveRespawn verifies that after a sole pool
// worker calls gc runtime drain-ack, the controller socket is poked
// immediately (issue #2364) so a replacement worker is reconciled within a
// tick or two instead of after the ~4-tick (~120s) discovery cascade.
//
// Both arrival orderings — #2364 (work pre-queued before the drain) and
// #2251 (cold pool scaling from zero with work present) — ride the SAME
// drain-ack → pokeController code path (plan R5: one code change covers both
// orderings), so both subtests exercise that path end-to-end through the real
// controller socket under StartWithSupervisor.
//
// AC8 verification vehicle: the mechanically tested unit is the per-step
// respawn latency — a single drain → respawn cycle measured by the wall-clock
// gap between consecutive spawn markers. The full "no step in the chain
// regresses to 4 ticks" property is delegated to RC observation of that same
// per-step signal; automating an N-cycle loop here would add wall-clock
// flakiness without strengthening the per-step bound that already
// discriminates the fix from the bug.
//
// Detection is provider-agnostic and dolt-free: the worker stand-in appends a
// timestamped marker line each time it spawns, so the count and spacing of
// marker lines measure respawn cadence directly. The file beads provider keeps
// tier B free of an external dolt dependency, and the routed demand beads are
// written straight into the in-process store file that the reconciler's
// default scale_check reads.
func TestLifecycle_DrainAckResponsiveRespawn(t *testing.T) {
	t.Run("prequeued_respawn_2364", func(t *testing.T) {
		// Long patrol interval: the only fast path to a replacement is the
		// drain-ack poke. Without the fix the reconciler would not observe the
		// drain until the next 60s tick, so a sub-40s respawn proves the poke
		// drove an immediate reconcile.
		runResponsiveRespawn(t, "60s", 40*time.Second, 50*time.Second)
	})
	t.Run("coldpool_arrival_2251", func(t *testing.T) {
		// Short patrol: a cold pool (0 active) scales up to service the routed
		// work and the post-drain replacement both land within a couple of
		// ticks. Covers #2251's arrival ordering over the shared poke path.
		runResponsiveRespawn(t, "10s", 30*time.Second, 30*time.Second)
	})
}

// runResponsiveRespawn drives one responsive-respawn scenario: it stands up a
// min=0/max=1 pool with pre-queued routed work, lets the worker stand-in
// spawn-mark-drain-exit, and asserts that a replacement spawns within
// maxRespawnLatency of the first worker. firstMarkerTimeout bounds the initial
// cold scale-up. patrolInterval tunes how strongly the poke is isolated from
// the patrol tick (a long interval makes the poke the only fast path).
func runResponsiveRespawn(t *testing.T, patrolInterval string, maxRespawnLatency, firstMarkerTimeout time.Duration) {
	t.Helper()
	c := helpers.NewCity(t, testEnvB)
	c.Init("claude")

	scriptCmd, markersPath := writeRespawnMarkerScript(t, c)
	writeRespawnPoolConfig(c, scriptCmd, patrolInterval)
	writePrequeuedRoutedBeads(t, c, "worker", 4)

	c.StartWithSupervisor()

	// The cold pool must scale from zero to service the routed work.
	if !c.WaitForCondition(func() bool {
		return len(readRespawnMarkers(markersPath)) >= 1
	}, firstMarkerTimeout) {
		out, _ := c.GC("status", "--city", c.Dir)
		t.Fatalf("pool worker never spawned to service pre-queued routed work within %s\nstatus:\n%s", firstMarkerTimeout, out)
	}

	// A replacement must spawn after the first worker drain-acked. Poll past
	// the assertion bound so a marker landing right at the boundary is still
	// observed before we measure it.
	deadline := time.Now().Add(maxRespawnLatency + 15*time.Second)
	for time.Now().Before(deadline) {
		if len(readRespawnMarkers(markersPath)) >= 2 {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}

	markers := readRespawnMarkers(markersPath)
	if len(markers) < 2 {
		out, _ := c.GC("status", "--city", c.Dir)
		t.Fatalf("replacement worker did not spawn after drain-ack (poke not honored?); markers=%v\nstatus:\n%s", markers, out)
	}

	latency := time.Duration((markers[1] - markers[0]) * float64(time.Second))
	if latency > maxRespawnLatency {
		t.Fatalf("respawn latency %s exceeds responsive bound %s (markers=%v, patrol=%s)",
			latency, maxRespawnLatency, markers, patrolInterval)
	}
	t.Logf("observed %d spawns; first respawn latency %s (bound %s, patrol %s)",
		len(markers), latency, maxRespawnLatency, patrolInterval)
}

// writeRespawnMarkerScript writes the pool-worker stand-in: it appends a
// timestamped marker line, calls the no-arg gc runtime drain-ack (which pokes
// the controller via GC_CITY_PATH from the agent env), then exits. Returns the
// start_command and the markers file path.
func writeRespawnMarkerScript(t *testing.T, c *helpers.City) (scriptCmd, markersPath string) {
	t.Helper()
	scriptsDir := filepath.Join(c.Dir, ".gc", "scripts")
	if err := os.MkdirAll(scriptsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	markersPath = filepath.Join(c.Dir, ".gc", "respawn-markers.txt")
	script := fmt.Sprintf(`#!/bin/sh
# Pool-worker stand-in for the responsive-respawn acceptance test. Each spawned
# worker appends one timestamped marker line, drain-acks (poking the controller
# so a replacement is reconciled immediately rather than on the next patrol
# tick), then exits. No set -e: a failing drain-ack must not skip the marker.
MARKERS=%q
printf 'RUN %%s\n' "$(date +%%s.%%N)" >> "$MARKERS"
gc runtime drain-ack 2>/dev/null || true
sleep 1
exit 0
`, markersPath)
	scriptPath := filepath.Join(scriptsDir, "respawn-marker.sh")
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return "bash " + scriptPath, markersPath
}

// writeRespawnPoolConfig writes city.toml (workspace + file beads provider +
// tuned daemon patrol interval) and a single city-scoped pool agent
// (min=0/max=1, wake_mode=fresh) under the directory-based agents/worker/
// surface. Inline [[agent]] tables in city.toml are a rejected PackV1 surface
// under schema-2 enforcement, so the agent must live in agents/<name>/agent.toml.
func writeRespawnPoolConfig(c *helpers.City, scriptCmd, patrolInterval string) {
	cityName := filepath.Base(c.Dir)
	c.WriteConfig(fmt.Sprintf(`[workspace]
name = %q

[beads]
provider = "file"

[daemon]
patrol_interval = %q
`, cityName, patrolInterval))
	c.WriteV2AgentDir("worker",
		fmt.Sprintf("start_command = %q", scriptCmd),
		`wake_mode = "fresh"`,
		"min_active_sessions = 0",
		"max_active_sessions = 1",
	)
}

// writePrequeuedRoutedBeads seeds the file beads store with n open, unassigned
// task beads routed to the given pool template. The reconciler's default
// scale_check counts these as pool demand (it reads the in-process store's
// Ready() set and matches metadata gc.routed_to), so a min=0/max=1 pool scales
// up to service them. A surplus of beads keeps demand positive across several
// drain → respawn cycles regardless of whether the reconciler assigns a bead
// per spawn.
func writePrequeuedRoutedBeads(t *testing.T, c *helpers.City, template string, n int) {
	t.Helper()
	type fileBead struct {
		ID        string            `json:"id"`
		Title     string            `json:"title"`
		Status    string            `json:"status"`
		Type      string            `json:"issue_type"`
		Priority  int               `json:"priority"`
		CreatedAt string            `json:"created_at"`
		Metadata  map[string]string `json:"metadata"`
	}
	store := struct {
		Seq   int        `json:"seq"`
		Beads []fileBead `json:"beads"`
	}{Seq: n}
	now := time.Now().UTC().Format(time.RFC3339)
	for i := 1; i <= n; i++ {
		store.Beads = append(store.Beads, fileBead{
			ID:        fmt.Sprintf("prequeued-%d", i),
			Title:     fmt.Sprintf("pre-queued pool work %d", i),
			Status:    "open",
			Type:      "task",
			Priority:  2,
			CreatedAt: now,
			Metadata:  map[string]string{"gc.routed_to": template},
		})
	}
	data, err := json.MarshalIndent(store, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(c.Dir, ".gc", "beads.json"), data, 0o644); err != nil {
		t.Fatalf("writing pre-queued beads.json: %v", err)
	}
}

// readRespawnMarkers parses the marker file into spawn timestamps (epoch
// seconds). A missing file yields no markers.
func readRespawnMarkers(path string) []float64 {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var ts []float64
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "RUN ") {
			continue
		}
		v, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimPrefix(line, "RUN ")), 64)
		if err != nil {
			continue
		}
		ts = append(ts, v)
	}
	return ts
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
