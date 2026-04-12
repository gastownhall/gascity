//go:build integration

package integration

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Reconciler convergence tests
//
// These tests exercise the reconciler's core loop — the state machine that
// reads config, compares with running sessions, and creates/kills sessions
// to converge. Each test starts a city, lets the reconciler tick, and
// verifies the desired state is reached.
// ---------------------------------------------------------------------------

// renderReconcilerToml returns a city.toml configured for fast reconciler
// ticks and the file bead store (no dolt dependency). The patrol_interval
// is set to 100ms so convergence happens quickly in tests.
func renderReconcilerToml(cityName string, agentBlocks string) string {
	return fmt.Sprintf(`[workspace]
name = %s

[beads]
provider = "file"

[daemon]
patrol_interval = "100ms"

%s
%s
`, quote(cityName), agentBlocks, reconcilerNamedSessions(agentBlocks))
}

var reconcilerAgentNamePattern = regexp.MustCompile(`(?m)^name = "([^"]+)"$`)

func reconcilerNamedSessions(agentBlocks string) string {
	parts := strings.Split(agentBlocks, "[[agent]]")
	var named strings.Builder
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" || strings.Contains(part, "[agent.pool]") {
			continue
		}
		match := reconcilerAgentNamePattern.FindStringSubmatch(part)
		if len(match) != 2 {
			continue
		}
		fmt.Fprintf(&named, "\n[[named_session]]\ntemplate = %s\nmode = \"always\"\n", quote(match[1]))
	}
	return named.String()
}

func writeReconcilerToml(t *testing.T, cityDir, cityName string, agentBlocks string) {
	t.Helper()

	tomlPath := filepath.Join(cityDir, "city.toml")
	if err := os.WriteFile(tomlPath, []byte(renderReconcilerToml(cityName, agentBlocks)), 0o644); err != nil {
		t.Fatalf("writing city.toml: %v", err)
	}
}

// setupReconcilerCity initializes a city with custom agent config, starts
// it, and registers cleanup. Returns the city directory path.
//
// Uses the init -> overwrite -> start pattern (no intermediate stop) to
// avoid a race where gc stop is not fully synchronous and gc start can
// fail with "standalone controller already running".
func setupReconcilerCity(t *testing.T, agentBlocks string) string {
	t.Helper()
	env := newIsolatedCommandEnv(t, false)

	cityName := uniqueCityName()
	cityDir := filepath.Join(t.TempDir(), cityName)
	configPath := filepath.Join(t.TempDir(), cityName+".toml")
	if err := os.WriteFile(configPath, []byte(renderReconcilerToml(cityName, agentBlocks)), 0o644); err != nil {
		t.Fatalf("writing init config: %v", err)
	}

	// Copy e2e scripts into the city before init so the first launch sees the
	// final filesystem layout.
	copyE2EScripts(t, cityDir)

	out, err := runGCWithEnv(env, "", "init", "--skip-provider-readiness", "--file", configPath, cityDir)
	if err != nil {
		t.Fatalf("gc init --file failed: %v\noutput: %s", err, out)
	}
	registerCityCommandEnv(cityDir, env)

	t.Cleanup(func() {
		unregisterCityCommandEnv(cityDir)
		runGCWithEnv(env, "", "stop", cityDir)      //nolint:errcheck // best-effort cleanup
		runGCWithEnv(env, "", "supervisor", "stop") //nolint:errcheck // best-effort cleanup
		fixRootOwnedFiles(cityDir)
	})

	return cityDir
}

// waitForSession polls gc session list until the given agent name appears
// or the timeout expires.
func waitForSession(t *testing.T, cityDir, agentName string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		out, err := gc(cityDir, "session", "list")
		if err == nil && strings.Contains(out, agentName) {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	out, _ := gc(cityDir, "session", "list")
	t.Fatalf("session %q not found within %s\nsession list:\n%s", agentName, timeout, out)
}

// waitForSessionCount polls gc session list until at least count sessions
// matching the given prefix appear or the timeout expires.
func waitForSessionCount(t *testing.T, cityDir, prefix string, count int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		out, err := gc(cityDir, "session", "list")
		if err == nil {
			n := countSessionsByPrefix(out, prefix)
			if n >= count {
				return
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	out, _ := gc(cityDir, "session", "list")
	t.Fatalf("wanted %d sessions matching %q, got %d within %s\nsession list:\n%s",
		count, prefix, countSessionsByPrefix(out, prefix), timeout, out)
}

// countSessionsByPrefix counts lines in session list output that contain
// the given prefix string.
func countSessionsByPrefix(sessionList, prefix string) int {
	count := 0
	for _, line := range strings.Split(sessionList, "\n") {
		if strings.Contains(line, prefix) {
			count++
		}
	}
	return count
}

// assertNoSession waits for waitTime then verifies the agent does NOT
// appear in the session list.
func assertNoSession(t *testing.T, cityDir, agentName string, waitTime time.Duration) {
	t.Helper()
	time.Sleep(waitTime)
	out, _ := gc(cityDir, "session", "list")
	if strings.Contains(out, agentName) {
		t.Errorf("session %q should not be running but found in:\n%s", agentName, out)
	}
}

// TestGastown_Reconciler_AlwaysSessionStarts verifies that the reconciler
// starts a named agent session without any external trigger. The agent has
// a long-lived start_command; the reconciler should create and maintain
// its session.
func TestGastown_Reconciler_AlwaysSessionStarts(t *testing.T) {
	agentBlocks := fmt.Sprintf(`[[agent]]
name = "worker"
start_command = %s
`, quote("bash $GC_CITY/.gc/scripts/stuck-agent.sh"))

	cityDir := setupReconcilerCity(t, agentBlocks)

	// The reconciler should converge: the agent session must appear.
	waitForSession(t, cityDir, "worker", 30*time.Second)
}

// TestGastown_Reconciler_SessionRestartsAfterExit verifies that the
// reconciler restarts an agent whose session exits. We use a short-lived
// script that writes a counter file and exits immediately. The reconciler
// should detect the dead session and restart it.
func TestGastown_Reconciler_SessionRestartsAfterExit(t *testing.T) {
	// The restart script appends a line to a marker file on each invocation,
	// then exits. The reconciler should keep restarting it. Using atomic
	// append (echo >> file) instead of read-modify-write avoids races when
	// the reconciler restarts the agent while a previous instance is still
	// writing the counter.
	markerDir := t.TempDir()
	script := fmt.Sprintf(`bash -c 'echo 1 >> "%s/restart-count"; exit 0'`,
		markerDir)

	agentBlocks := fmt.Sprintf(`[[agent]]
name = "shortlived"
start_command = %s
`, quote(script))

	_ = setupReconcilerCity(t, agentBlocks)

	// Wait for the reconciler to restart the agent at least twice.
	// With patrol_interval=100ms, this should happen quickly.
	// Count restarts by counting lines in the marker file.
	markerFile := filepath.Join(markerDir, "restart-count")
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(markerFile)
		if err == nil {
			lines := strings.Count(strings.TrimSpace(string(data)), "\n") + 1
			if len(strings.TrimSpace(string(data))) > 0 && lines >= 2 {
				t.Logf("agent restarted %d times", lines)
				return
			}
		}
		time.Sleep(500 * time.Millisecond)
	}

	data, _ := os.ReadFile(markerFile)
	t.Fatalf("agent was not restarted at least twice within 30s; marker file content: %q", string(data))
}

// TestGastown_Reconciler_SuspendedAgentSkipped verifies that the reconciler
// does NOT start an agent with suspended = true. The agent should never
// appear in the session list.
func TestGastown_Reconciler_SuspendedAgentSkipped(t *testing.T) {
	agentBlocks := fmt.Sprintf(`[[agent]]
name = "sleeper"
start_command = %s
suspended = true
`, quote("bash $GC_CITY/.gc/scripts/stuck-agent.sh"))

	cityDir := setupReconcilerCity(t, agentBlocks)

	// Wait several reconciler ticks and verify the agent never starts.
	// With patrol_interval=100ms, 3 seconds is ~30 ticks.
	assertNoSession(t, cityDir, "sleeper", 3*time.Second)
}

// TestGastown_Reconciler_PoolScaling verifies that the reconciler scales
// a pool agent to the count returned by the pool check command. With
// min=1, max=3, and check returning "2", the reconciler should converge
// to exactly 2 pool instances.
func TestGastown_Reconciler_PoolScaling(t *testing.T) {
	agentBlocks := fmt.Sprintf(`[[agent]]
name = "scaler"
start_command = %s

[agent.pool]
min = 1
max = 3
check = "echo 2"
`, quote("bash $GC_CITY/.gc/scripts/stuck-agent.sh"))

	cityDir := setupReconcilerCity(t, agentBlocks)

	// Wait for at least 2 pool instances (scaler-1 and scaler-2).
	waitForSessionCount(t, cityDir, "scaler", 2, 30*time.Second)

	// Verify exactly 2, not 3 (check returns 2, max is 3).
	out, err := gc(cityDir, "session", "list")
	if err != nil {
		t.Fatalf("gc session list failed: %v\noutput: %s", err, out)
	}
	count := countSessionsByPrefix(out, "scaler")
	if count != 2 {
		t.Errorf("expected exactly 2 pool instances, got %d\nsession list:\n%s", count, out)
	}
}

// TestGastown_Reconciler_MultipleAgentsConverge verifies that the
// reconciler converges multiple independent agents to the running state.
// All agents should be present in the session list after convergence.
func TestGastown_Reconciler_MultipleAgentsConverge(t *testing.T) {
	sleepCmd := quote("bash $GC_CITY/.gc/scripts/stuck-agent.sh")
	agentBlocks := fmt.Sprintf(`[[agent]]
name = "alpha"
start_command = %s

[[agent]]
name = "beta"
start_command = %s

[[agent]]
name = "gamma"
start_command = %s
`, sleepCmd, sleepCmd, sleepCmd)

	cityDir := setupReconcilerCity(t, agentBlocks)

	// All three agents should converge to running.
	for _, name := range []string{"alpha", "beta", "gamma"} {
		waitForSession(t, cityDir, name, 30*time.Second)
	}

	// Verify all three are present simultaneously.
	out, err := gc(cityDir, "session", "list")
	if err != nil {
		t.Fatalf("gc session list failed: %v\noutput: %s", err, out)
	}
	for _, name := range []string{"alpha", "beta", "gamma"} {
		if !strings.Contains(out, name) {
			t.Errorf("agent %q not found in session list:\n%s", name, out)
		}
	}

	// Verify gc stop succeeds cleanly.
	out, err = gc("", "stop", cityDir)
	if err != nil {
		t.Fatalf("gc stop failed: %v\noutput: %s", err, out)
	}
}
