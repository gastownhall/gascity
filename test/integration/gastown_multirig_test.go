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
// The city starts from a file-backed schema-2 source so config-only multi-rig
// tests do not depend on the developer machine's bd/dolt toolchain before the
// test overwrites city.toml with its scenario-specific fixture.
func setupMultiRigCity(t *testing.T, rigCount int) (cityDir string, rigDirs []string) {
	t.Helper()
	env := newIsolatedCommandEnv(t, false)
	cityName := uniqueCityName()
	cityDir = filepath.Join(t.TempDir(), cityName)
	sourceDir := filepath.Join(t.TempDir(), cityName+"-source")
	require.NoError(t, os.MkdirAll(sourceDir, 0o755))

	var source strings.Builder
	fmt.Fprintf(&source, "[workspace]\nname = %s\n", quote(cityName))
	fmt.Fprintf(&source, "\n[beads]\nprovider = \"file\"\n")
	require.NoError(t, os.WriteFile(filepath.Join(sourceDir, "city.toml"), []byte(source.String()), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(sourceDir, "pack.toml"), []byte(packTomlWithCoreImport(t, cityName)), 0o644))

	// Create the city scaffold inside an isolated supervisor env so
	// multi-rig tests do not contend with the suite-global supervisor.
	out, err := runGCWithEnv(env, "", "init", "--skip-provider-readiness", "--from", sourceDir, cityDir)
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

// writeMultiRigToml writes a schema-2 city plus site bindings for the given
// rig directories. City-scoped agents are declared under agents/<name>/agent.toml.
// Rig-scoped agents come from a local pack imported by each rig so duplicate
// agent names across rigs stay representable without legacy inline [[agent]].
func writeMultiRigToml(t *testing.T, cityDir, cityName string, rigDirs []string, agents []gasTownAgent) {
	t.Helper()

	var cityAgents []gasTownAgent
	rigAgentsByDir := make(map[string][]gasTownAgent)
	rigPackAgentNames := make(map[string]gasTownAgent)
	for _, a := range agents {
		if a.Dir == "" {
			cityAgents = append(cityAgents, a)
			continue
		}
		rigAgentsByDir[a.Dir] = append(rigAgentsByDir[a.Dir], a)
		if _, ok := rigPackAgentNames[a.Name]; !ok {
			rigPackAgentNames[a.Name] = gasTownAgent{Name: a.Name}
		}
	}
	if len(rigPackAgentNames) > 0 {
		writeMultiRigAgentPack(t, cityDir, rigPackAgentNames)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "[workspace]\nname = %s\n", quote(cityName))
	fmt.Fprintf(&b, "\n[beads]\nprovider = \"file\"\n")
	fmt.Fprintf(&b, "\n[daemon]\npatrol_interval = \"100ms\"\n")

	for i := range rigDirs {
		rigName := fmt.Sprintf("rig-%d", i)
		fmt.Fprintf(&b, "\n[[rigs]]\nname = %s\n", quote(rigName))
		if rigAgents := rigAgentsByDir[rigName]; len(rigAgents) > 0 {
			fmt.Fprintf(&b, "\n[rigs.imports.multirig_agents]\nsource = \"packs/multirig-agents\"\n")
			for _, a := range rigAgents {
				fmt.Fprintf(&b, "\n[[rigs.patches]]\nagent = %s\n", quote(a.Name))
				if a.StartCommand != "" {
					fmt.Fprintf(&b, "start_command = %s\n", quote(a.StartCommand))
				}
				if a.Suspended {
					b.WriteString("suspended = true\n")
				}
			}
		}
	}

	for _, a := range agents {
		if a.Pool != nil {
			continue
		}
		template := a.Name
		if a.Dir != "" {
			template = "multirig_agents." + a.Name
		}
		fmt.Fprintf(&b, "\n[[named_session]]\ntemplate = %s\nmode = \"always\"\n", quote(template))
		if a.Dir != "" {
			fmt.Fprintf(&b, "name = %s\n", quote(a.Name))
			fmt.Fprintf(&b, "dir = %s\n", quote(a.Dir))
		}
	}

	tomlPath := filepath.Join(cityDir, "city.toml")
	require.NoError(t, os.WriteFile(tomlPath, []byte(b.String()), 0o644))
	writeGasTownAgentFiles(t, cityDir, cityAgents)

	var site strings.Builder
	fmt.Fprintf(&site, "workspace_name = %s\n", quote(cityName))
	for i, rd := range rigDirs {
		rigName := fmt.Sprintf("rig-%d", i)
		fmt.Fprintf(&site, "\n[[rig]]\nname = %s\npath = %s\n", quote(rigName), quote(rd))
	}
	require.NoError(t, os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(cityDir, ".gc", "site.toml"), []byte(site.String()), 0o644))
}

func writeMultiRigAgentPack(t *testing.T, cityDir string, agents map[string]gasTownAgent) {
	t.Helper()

	packDir := filepath.Join(cityDir, "packs", "multirig-agents")
	require.NoError(t, os.MkdirAll(packDir, 0o755))
	pack := "[pack]\nname = \"multirig-agents\"\nschema = 2\n"
	require.NoError(t, os.WriteFile(filepath.Join(packDir, "pack.toml"), []byte(pack), 0o644))

	for _, a := range agents {
		agentDir := filepath.Join(packDir, "agents", a.Name)
		require.NoError(t, os.MkdirAll(agentDir, 0o755))
		var b strings.Builder
		b.WriteString("scope = \"rig\"\n")
		if a.StartCommand != "" {
			fmt.Fprintf(&b, "start_command = %s\n", quote(a.StartCommand))
		}
		require.NoError(t, os.WriteFile(filepath.Join(agentDir, "agent.toml"), []byte(b.String()), 0o644))
	}
}

func installFakeBDForCity(t *testing.T, cityDir string) {
	t.Helper()

	shimDir := t.TempDir()
	script := filepath.Join(shimDir, "bd")
	content := `#!/bin/sh
set -eu
store="${BEADS_DIR:?}/fake-beads"
mkdir -p "$store"
case "${1:-}" in
  create)
    title="${2:?missing title}"
    id="${GC_BEADS_PREFIX:-bd}-fake"
    printf '%s' "$title" > "$store/$id"
    printf 'Created issue: %s\n' "$id"
    ;;
  show)
    id="${2:?missing id}"
    if [ ! -f "$store/$id" ]; then
      printf 'Error: issue not found: %s\n' "$id" >&2
      exit 1
    fi
    printf 'ID: %s\n' "$id"
    printf 'Title: %s\n' "$(cat "$store/$id")"
    ;;
  *)
    printf 'unsupported fake bd command: %s\n' "$*" >&2
    exit 2
    ;;
esac
`
	require.NoError(t, os.WriteFile(script, []byte(content), 0o755))

	loaded, ok := cityCommandEnv.Load(cityDir)
	require.True(t, ok, "city command env should be registered for %s", cityDir)
	env := append([]string(nil), loaded.([]string)...)
	envMap := parseEnvList(env)
	env = replaceEnv(env, "PATH", prependPath(shimDir, envMap["PATH"]))
	registerCityCommandEnv(cityDir, env)
}

func seedConfiguredFakeBDWorkspace(t *testing.T, dir, prefix string) {
	t.Helper()

	beadsDir := filepath.Join(dir, ".beads")
	require.NoError(t, os.MkdirAll(beadsDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(beadsDir, "config.yaml"), []byte("issue_prefix: "+prefix+"\n"), 0o644))
	metadata := fmt.Sprintf(`{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":%q}`+"\n", prefix)
	require.NoError(t, os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte(metadata), 0o644))
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
	writeMultiRigToml(t, cityDir, cityName, rigDirs, agents)
	installFakeBDForCity(t, cityDir)

	// Seed bd store markers after city.toml exists, then exercise only
	// Gas City's configured rig route rather than direct cwd-based bd calls.
	prefix0 := "r0"
	prefix1 := "r1"
	seedConfiguredFakeBDWorkspace(t, rigDirs[0], prefix0)
	seedConfiguredFakeBDWorkspace(t, rigDirs[1], prefix1)
	assert.NotEqual(t, prefix0, prefix1, "rig bead prefixes should differ")

	// Create a bead through Gas City's configured rig route.
	out, err := gc(cityDir, "bd", "--rig", "rig-0", "create", "multi-rig bead test alpha")
	require.NoError(t, err, "bd create in rig-0: %s", out)
	beadID := extractBeadID(t, out)

	// The bead ID should carry rig-0's prefix.
	require.NotEmpty(t, beadID, "bead ID should not be empty")
	assert.True(t, strings.HasPrefix(beadID, prefix0),
		"bead ID %q should start with rig-0 prefix %q", beadID, prefix0)

	// Verify the bead is visible through rig-0's configured route.
	out, err = gc(cityDir, "bd", "--rig", "rig-0", "show", beadID)
	require.NoError(t, err, "bd show from rig-0: %s", out)
	assert.Contains(t, out, "multi-rig bead test alpha",
		"bead should be visible from rig-0")

	// Verify the same bead is not visible through rig-1's configured route.
	out, err = gc(cityDir, "bd", "--rig", "rig-1", "show", beadID)
	require.Error(t, err, "bd show from rig-1 should fail for bead %s; output: %s", beadID, out)
	assert.NotContains(t, out, "multi-rig bead test alpha",
		"bead should not be visible from rig-1")
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
