//go:build integration

package integration

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func waitForSessionTargets(t *testing.T, cityDir string, targets []string, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		out, err := gc(cityDir, "session", "list", "--state", "all")
		if err == nil {
			allPresent := true
			for _, target := range targets {
				if !strings.Contains(out, target) {
					allPresent = false
					break
				}
			}
			if allPresent {
				return
			}
		}
		time.Sleep(300 * time.Millisecond)
	}

	out, _ := gc(cityDir, "session", "list", "--state", "all")
	t.Fatalf("expected session targets never appeared:\n%s", out)
}

func waitForActiveSessionTargets(t *testing.T, cityDir string, targets []string, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		out, err := gc(cityDir, "session", "list")
		if err == nil {
			allPresent := true
			for _, target := range targets {
				if !strings.Contains(out, target) {
					allPresent = false
					break
				}
			}
			if allPresent {
				return
			}
		}
		time.Sleep(300 * time.Millisecond)
	}

	out, _ := gc(cityDir, "session", "list")
	t.Fatalf("expected active session targets never appeared:\n%s", out)
}

// setupMultiRigCity creates a city scaffold with the given number of rig
// directories under an isolated integration GC_HOME. Returns the city
// directory and a slice of rig directory paths.
//
// The city starts with the default minimal scaffold. Callers overwrite
// city.toml afterward and use gc restart/start against the isolated
// supervisor-managed city registered for this path.
func setupMultiRigCity(t *testing.T, rigCount int) (cityDir string, rigDirs []string) {
	t.Helper()
	env := newIsolatedCommandEnv(t, false)
	cityName := uniqueCityName()
	cityDir = filepath.Join(t.TempDir(), cityName)

	// Create the city scaffold inside an isolated supervisor env so
	// multi-rig tests do not contend with the suite-global supervisor.
	out, err := runGCWithEnv(env, "", "init", "--skip-provider-readiness", cityDir)
	require.NoError(t, err, "gc init: %s", out)
	registerCityCommandEnv(cityDir, env)

	rigDirs = make([]string, rigCount)
	for i := 0; i < rigCount; i++ {
		rigDirs[i] = filepath.Join(t.TempDir(), fmt.Sprintf("rig-%d", i))
		require.NoError(t, os.MkdirAll(rigDirs[i], 0o755))
		registerCityCommandEnv(rigDirs[i], env)
	}

	t.Cleanup(func() {
		unregisterCityCommandEnv(cityDir)
		for _, rigDir := range rigDirs {
			unregisterCityCommandEnv(rigDir)
		}
		runGCWithEnv(env, "", "stop", cityDir)                //nolint:errcheck // best-effort cleanup
		runGCWithEnv(env, "", "supervisor", "stop", "--wait") //nolint:errcheck // best-effort cleanup
		deadline := time.Now().Add(10 * time.Second)
		for {
			if err := os.RemoveAll(cityDir); err == nil || time.Now().After(deadline) {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
	})

	return cityDir, rigDirs
}

// writeMultiRigToml writes a city.toml that references the given rig directories.
// Each rig gets a [[rigs]] entry and a rig-scoped worker agent.
func writeMultiRigToml(t *testing.T, cityDir, cityName string, rigDirs []string, agents []gasTownAgent) {
	t.Helper()

	var b strings.Builder
	fmt.Fprintf(&b, "[workspace]\nname = %s\n", quote(cityName))
	fmt.Fprintf(&b, "\n[beads]\nprovider = \"file\"\n")
	fmt.Fprintf(&b, "\n[daemon]\npatrol_interval = \"100ms\"\n")

	for i, rd := range rigDirs {
		rigName := fmt.Sprintf("rig-%d", i)
		fmt.Fprintf(&b, "\n[[rigs]]\nname = %s\npath = %s\n",
			quote(rigName), quote(rd))
	}

	for _, a := range agents {
		fmt.Fprintf(&b, "\n[[agent]]\nname = %s\n", quote(a.Name))
		fmt.Fprintf(&b, "start_command = %s\n", quote(a.StartCommand))
		if a.Dir != "" {
			fmt.Fprintf(&b, "dir = %s\n", quote(a.Dir))
		}
		if a.Suspended {
			fmt.Fprintf(&b, "suspended = true\n")
		}
		if len(a.Env) > 0 {
			b.WriteString("\n[agent.env]\n")
			for k, v := range a.Env {
				fmt.Fprintf(&b, "%s = %s\n", k, quote(v))
			}
		}
		if a.Pool != nil {
			fmt.Fprintf(&b, "min_active_sessions = %d\n", a.Pool.Min)
			fmt.Fprintf(&b, "max_active_sessions = %d\n", a.Pool.Max)
			fmt.Fprintf(&b, "scale_check = %s\n", quote(a.Pool.Check))
		}
	}

	for _, a := range agents {
		if a.Pool != nil {
			continue
		}
		fmt.Fprintf(&b, "\n[[named_session]]\ntemplate = %s\nmode = \"always\"\n", quote(a.Name))
		if a.Dir != "" {
			fmt.Fprintf(&b, "dir = %s\n", quote(a.Dir))
		}
	}

	tomlPath := filepath.Join(cityDir, "city.toml")
	require.NoError(t, os.WriteFile(tomlPath, []byte(b.String()), 0o644))
}

// TestGastown_MultiRig_ConfigLoads creates a city with 2 rigs and verifies
// that gc config show reports both rigs.
func TestGastown_MultiRig_ConfigLoads(t *testing.T) {
	cityDir, rigDirs := setupMultiRigCity(t, 2)
	cityName := uniqueCityName()

	agents := []gasTownAgent{
		{Name: "worker", StartCommand: "sleep 3600"},
	}
	writeMultiRigToml(t, cityDir, cityName, rigDirs, agents)

	// Verify gc config show works and mentions both rigs.
	out, err := gc(cityDir, "config", "show")
	require.NoError(t, err, "gc config show: %s", out)
	assert.Contains(t, out, "rig-0", "config show should list rig-0")
	assert.Contains(t, out, "rig-1", "config show should list rig-1")

	// Verify gc config show --validate passes.
	out, err = gc(cityDir, "config", "show", "--validate")
	require.NoError(t, err, "gc config show --validate: %s", out)
}

// TestGastown_MultiRig_AgentsIsolated creates a city with 2 rigs, each having
// a rig-scoped worker agent. Starts the city and uses the report-script
// pattern to verify that each agent receives the correct GC_RIG env var.
func TestGastown_MultiRig_AgentsIsolated(t *testing.T) {
	cityDir, rigDirs := setupMultiRigCity(t, 2)
	cityName := uniqueCityName()

	// Write report scripts into each rig directory.
	// Each script dumps env vars to a report file keyed by agent name.
	reportDir := filepath.Join(cityDir, ".gc-reports")
	require.NoError(t, os.MkdirAll(reportDir, 0o755))

	scriptContent := `#!/bin/bash
set -euo pipefail
SAFE_NAME="${GC_AGENT//\//__}"
REPORT_DIR="${GC_CITY}/.gc-reports"
mkdir -p "$REPORT_DIR"
REPORT="${REPORT_DIR}/${SAFE_NAME}.report"
{
    echo "STATUS=started"
    echo "CWD=$(pwd)"
    env | grep "^GC_" | sort || true
    echo "STATUS=complete"
} > "$REPORT" 2>&1
sleep 3600
`
	for i, rd := range rigDirs {
		scriptPath := filepath.Join(rd, "report.sh")
		require.NoError(t, os.WriteFile(scriptPath, []byte(scriptContent), 0o755),
			"writing report script for rig-%d", i)
	}

	agents := []gasTownAgent{
		{
			Name:         "worker",
			StartCommand: fmt.Sprintf("bash %s", filepath.Join(rigDirs[0], "report.sh")),
			Dir:          "rig-0",
		},
		{
			Name:         "worker",
			StartCommand: fmt.Sprintf("bash %s", filepath.Join(rigDirs[1], "report.sh")),
			Dir:          "rig-1",
		},
	}
	writeMultiRigToml(t, cityDir, cityName, rigDirs, agents)

	out, err := gc("", "restart", cityDir)
	require.NoError(t, err, "gc restart: %s", out)
	waitForSessionTargets(t, cityDir, []string{"rig-0/worker", "rig-1/worker"}, 30*time.Second)

	// Wait for both reports.
	deadline := time.Now().Add(30 * time.Second)
	reportNames := []string{"rig-0__worker", "rig-1__worker"}
	for _, name := range reportNames {
		reportPath := filepath.Join(reportDir, name+".report")
		for time.Now().Before(deadline) {
			data, readErr := os.ReadFile(reportPath)
			if readErr == nil && strings.Contains(string(data), "STATUS=complete") {
				break
			}
			time.Sleep(300 * time.Millisecond)
		}
		data, readErr := os.ReadFile(reportPath)
		require.NoError(t, readErr, "report for %s not found", name)
		require.Contains(t, string(data), "STATUS=complete",
			"report for %s did not complete: %s", name, string(data))
	}

	// Verify each agent received the correct GC_RIG.
	for i, name := range reportNames {
		data, err := os.ReadFile(filepath.Join(reportDir, name+".report"))
		require.NoError(t, err)
		content := string(data)
		expectedRig := fmt.Sprintf("GC_RIG=rig-%d", i)
		assert.Contains(t, content, expectedRig,
			"agent %s should have %s in env", name, expectedRig)
	}
}

// TestGastown_MultiRig_BeadIsolation creates a city with 2 rigs, each with
// its own beads database. Creates a bead in rig-0 and verifies the bead ID
// carries a rig-specific prefix.
func TestGastown_MultiRig_BeadIsolation(t *testing.T) {
	cityDir, rigDirs := setupMultiRigCity(t, 2)
	cityName := uniqueCityName()

	agents := []gasTownAgent{
		{Name: "worker", StartCommand: "sleep 3600"},
	}

	// Initialize beads in each rig directory with unique prefixes.
	prefix0 := initBd(t, rigDirs[0])
	prefix1 := initBd(t, rigDirs[1])
	assert.NotEqual(t, prefix0, prefix1, "rig bead prefixes should differ")

	// Create a bead from rig-0's directory.
	out, err := bd(rigDirs[0], "create", "multi-rig bead test alpha")
	require.NoError(t, err, "bd create in rig-0: %s", out)
	beadID := extractBeadID(t, out)

	// The bead ID should carry rig-0's prefix.
	require.NotEmpty(t, beadID, "bead ID should not be empty")
	assert.True(t, strings.HasPrefix(beadID, prefix0),
		"bead ID %q should start with rig-0 prefix %q", beadID, prefix0)

	// Verify the bead is visible from rig-0.
	out, err = bd(rigDirs[0], "show", beadID)
	require.NoError(t, err, "bd show from rig-0: %s", out)
	assert.Contains(t, out, "multi-rig bead test alpha",
		"bead should be visible from rig-0")

	writeMultiRigToml(t, cityDir, cityName, rigDirs, agents)
}

// TestGastown_MultiRig_IndependentLifecycle starts a city with 2 rigs, stops
// it, restarts it, and verifies both rigs come back cleanly.
func TestGastown_MultiRig_IndependentLifecycle(t *testing.T) {
	cityDir, rigDirs := setupMultiRigCity(t, 2)
	cityName := uniqueCityName()

	agents := []gasTownAgent{
		{Name: "worker", StartCommand: "sleep 3600", Dir: "rig-0"},
		{Name: "worker", StartCommand: "sleep 3600", Dir: "rig-1"},
	}
	writeMultiRigToml(t, cityDir, cityName, rigDirs, agents)

	out, err := gc("", "restart", cityDir)
	require.NoError(t, err, "gc restart: %s", out)
	waitForSessionTargets(t, cityDir, []string{"rig-0/worker", "rig-1/worker"}, 30*time.Second)

	// Let agents settle.
	time.Sleep(500 * time.Millisecond)

	// Verify both agents appear in session list.
	out, err = gc(cityDir, "session", "list")
	require.NoError(t, err, "gc session list: %s", out)
	assert.Contains(t, out, "worker", "session list should show worker agents")

	// Stop the city.
	out, err = gc("", "stop", cityDir)
	require.NoError(t, err, "gc stop: %s", out)

	time.Sleep(300 * time.Millisecond)

	// Restart the city.
	out, err = gc("", "start", cityDir)
	require.NoError(t, err, "gc start (restart): %s", out)

	waitForActiveSessionTargets(t, cityDir, []string{"rig-0/worker", "rig-1/worker"}, 30*time.Second)

	// Verify both agents are back.
	out, err = gc(cityDir, "session", "list")
	require.NoError(t, err, "gc session list after restart: %s", out)
	assert.Contains(t, out, "worker",
		"session list after restart should show worker agents")

	// Verify config still shows both rigs.
	out, err = gc(cityDir, "config", "show")
	require.NoError(t, err, "gc config show after restart: %s", out)
	assert.Contains(t, out, "rig-0", "config should still have rig-0 after restart")
	assert.Contains(t, out, "rig-1", "config should still have rig-1 after restart")
}
