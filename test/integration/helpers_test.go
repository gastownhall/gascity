//go:build integration

package integration

import (
	"crypto/rand"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/test/tmuxtest"
)

// agentConfig describes a single agent for setupCity.
type agentConfig struct {
	Name         string // agent name in city.toml
	StartCommand string // shell command (e.g., "sleep 3600", "bash /path/to/script.sh")
}

// usingSubprocess reports whether GC_SESSION=subprocess is set.
func usingSubprocess() bool {
	return os.Getenv("GC_SESSION") == "subprocess"
}

// uniqueCityName generates a random city name for test isolation.
func uniqueCityName() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		panic("generating random city name: " + err.Error())
	}
	hex := fmt.Sprintf("%x", b)
	parts := make([]string, 0, len(hex)+1)
	parts = append(parts, "gctest")
	for _, r := range hex {
		parts = append(parts, string(r))
	}
	return strings.Join(parts, "-")
}

// setupCity creates a city directory, initializes it, writes a city.toml
// with the given agents, and runs gc start. Returns the city directory path.
// This is the general-purpose front-door setup for all integration tests.
//
// When using tmux, pass a non-nil guard for session cleanup. When using
// subprocess, guard may be nil (cleanup is via gc stop in t.Cleanup).
func setupCity(t *testing.T, guard *tmuxtest.Guard, agents []agentConfig) string {
	t.Helper()
	env := newIsolatedCommandEnv(t, false)

	var cityName string
	if guard != nil {
		cityName = guard.CityName()
	} else {
		cityName = uniqueCityName()
	}

	cityDir := filepath.Join(t.TempDir(), cityName)

	configPath := filepath.Join(t.TempDir(), cityName+".toml")
	writeAgentsToml(t, filepath.Dir(configPath), cityName, agents)
	if err := os.Rename(filepath.Join(filepath.Dir(configPath), "city.toml"), configPath); err != nil {
		t.Fatalf("moving config template: %v", err)
	}

	// gc init --file seeds the city directly from the intended config instead
	// of creating the tutorial scaffold and mutating it afterward.
	out, err := runGCWithEnv(env, "", "init", "--skip-provider-readiness", "--file", configPath, cityDir)
	if err != nil {
		t.Fatalf("gc init --file failed: %v\noutput: %s", err, out)
	}
	registerCityCommandEnv(cityDir, env)

	waitForExpectedTmuxSessions(t, cityDir, agentNames(agents))

	// Register cleanup: gc stop on test end.
	t.Cleanup(func() {
		unregisterCityCommandEnv(cityDir)
		runGCWithEnv(env, "", "stop", cityDir)                //nolint:errcheck // best-effort cleanup
		runGCWithEnv(env, "", "supervisor", "stop", "--wait") //nolint:errcheck // best-effort cleanup
	})

	// Give sessions a moment to register.
	time.Sleep(200 * time.Millisecond)

	return cityDir
}

// setupCityNoGuard creates a city without requiring a tmuxtest.Guard.
// Used by tests that work with any session provider.
func setupCityNoGuard(t *testing.T, agents []agentConfig) string {
	t.Helper()
	return setupCity(t, nil, agents)
}

// setupRunningCity creates a city directory, initializes it, writes a
// city.toml with start_command = "sleep 3600", and runs gc start.
// Returns the city directory path.
func setupRunningCity(t *testing.T, guard *tmuxtest.Guard) string {
	t.Helper()
	return setupCity(t, guard, []agentConfig{
		{Name: "mayor", StartCommand: "sleep 3600"},
	})
}

func initCityWithManagedDoltRecovery(t *testing.T, env []string, configPath, cityDir string) {
	t.Helper()

	var (
		out          string
		err          error
		sawTransient bool
	)
	for attempt := 1; attempt <= 2; attempt++ {
		out, err = runGCDoltWithEnv(env, "", "init", "--skip-provider-readiness", "--file", configPath, cityDir)
		if err == nil {
			return
		}

		transient := isTransientManagedDoltInitFailure(out)
		alreadyInitialized := isAlreadyInitializedGCInitFailure(out)
		if !transient && !(sawTransient && alreadyInitialized) {
			t.Fatalf("gc init failed: %v\noutput: %s", err, out)
		}
		sawTransient = sawTransient || transient

		if attempt < 2 {
			t.Logf("retrying gc init after transient managed Dolt startup failure (attempt %d/2)", attempt+1)
			time.Sleep(time.Duration(attempt) * time.Second)
			continue
		}
	}

	startOut, startErr := runGCDoltWithEnv(env, "", "start", cityDir)
	if startErr != nil && !isGCStartAlreadyRunning(startOut) && !isTransientManagedDoltInitFailure(startOut) {
		t.Fatalf("gc init failed after transient managed Dolt startup failure: %v\ninit output: %s\ngc start recovery failed: %v\nstart output: %s", err, out, startErr, startOut)
	}
	if readyOut, readyErr := waitForManagedDoltCityReady(env, cityDir, 20*time.Second); readyErr == nil {
		t.Log("recovered partially initialized city after transient managed Dolt startup failure")
		return
	} else {
		t.Fatalf("gc init failed after transient managed Dolt startup failure: %v\ninit output: %s\ngc start recovery failed: %v\nstart output: %s\ncity never became ready: %v\nlast bd output: %s", err, out, startErr, startOut, readyErr, readyOut)
	}
}

func waitForManagedDoltCityReady(env []string, cityDir string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	var (
		lastOut string
		lastErr error
	)
	for time.Now().Before(deadline) {
		if port, ok := currentManagedDoltPortForTest(cityDir); ok {
			probeEnv := filterEnvMany(env,
				"GC_CITY",
				"GC_CITY_PATH",
				"GC_CITY_ROOT",
				"GC_CITY_RUNTIME_DIR",
				"GC_DOLT_PORT",
			)
			probeEnv = append(probeEnv,
				"GC_CITY="+cityDir,
				"GC_CITY_PATH="+cityDir,
				"GC_CITY_RUNTIME_DIR="+filepath.Join(cityDir, ".gc", "runtime"),
			)
			probeEnv = appendManagedDoltEndpointEnv(probeEnv, port)
			lastOut, lastErr = runCommand(cityDir, probeEnv, integrationBDCommandTimeout, bdBinary, "list", "--all", "--json", "--limit=0")
			if lastErr == nil {
				return lastOut, nil
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("timed out after %s waiting for managed Dolt city readiness", timeout)
	}
	return lastOut, lastErr
}

func isTransientManagedDoltInitFailure(out string) bool {
	msg := strings.ToLower(out)
	return strings.Contains(msg, "dolt server exited during startup") ||
		strings.Contains(msg, "did not become query-ready after 30s") ||
		strings.Contains(msg, "supervisor did not become ready") ||
		strings.Contains(msg, "supervisor did not start") ||
		strings.Contains(msg, "supervisor stopped before city became ready")
}

func isAlreadyInitializedGCInitFailure(out string) bool {
	return strings.Contains(strings.ToLower(out), "already initialized")
}

func isGCStartAlreadyRunning(out string) bool {
	return strings.Contains(strings.ToLower(out), "already running")
}

func agentNames(agents []agentConfig) []string {
	names := make([]string, 0, len(agents))
	for _, agent := range agents {
		names = append(names, agent.Name)
	}
	return names
}

func waitForExpectedTmuxSessions(t *testing.T, cityDir string, expectedAgents []string) {
	t.Helper()

	if usingSubprocess() {
		time.Sleep(500 * time.Millisecond)
		return
	}

	socketName := filepath.Base(cityDir)
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		cmd := exec.Command("tmux", "-L", socketName, "list-sessions", "-F", "#{session_name}")
		listOut, listErr := cmd.CombinedOutput()
		if listErr == nil {
			sessions := string(listOut)
			allPresent := true
			for _, agent := range expectedAgents {
				expected := strings.ReplaceAll(agent, "/", "--")
				if !strings.Contains(sessions, expected) {
					allPresent = false
					break
				}
			}
			if allPresent {
				return
			}
		}
		time.Sleep(200 * time.Millisecond)
	}

	cmd := exec.Command("tmux", "-L", socketName, "list-sessions", "-F", "#{session_name}")
	listOut, _ := cmd.CombinedOutput()
	sessionOut, _ := gc(cityDir, "session", "list", "--state", "all")
	t.Fatalf("expected tmux sessions never appeared on socket %q\nsessions:\n%s\ntmux:\n%s", socketName, sessionOut, listOut)
}

// writeAgentsToml writes a city.toml with the given agents.
func writeAgentsToml(t *testing.T, cityDir, cityName string, agents []agentConfig) {
	t.Helper()
	content := "[workspace]\nname = " + quote(cityName) + "\n\n[beads]\nprovider = \"file\"\n"
	for _, a := range agents {
		content += fmt.Sprintf("\n[[agent]]\nname = %s\nstart_command = %s\n",
			quote(a.Name), quote(a.StartCommand))
		content += fmt.Sprintf("\n[[named_session]]\ntemplate = %s\nmode = \"always\"\n",
			quote(a.Name))
	}
	tomlPath := filepath.Join(cityDir, "city.toml")
	if err := os.WriteFile(tomlPath, []byte(content), 0o644); err != nil {
		t.Fatalf("writing city.toml: %v", err)
	}
}

// agentScript returns the absolute path to a test agent script in test/agents/.
func agentScript(name string) string {
	root := findModuleRoot()
	return filepath.Join(root, "test", "agents", name)
}

// writeCityToml overwrites city.toml with a single mayor agent using the
// given start command. The city name is set to cityName.
func writeCityToml(t *testing.T, cityDir, cityName, startCommand string) {
	t.Helper()
	writeAgentsToml(t, cityDir, cityName, []agentConfig{
		{Name: "mayor", StartCommand: startCommand},
	})
}

// quote returns a TOML-safe quoted string.
func quote(s string) string {
	return strconv.Quote(s)
}

func repoRoot(t *testing.T) string {
	t.Helper()
	return findModuleRoot()
}

func filterEnvMany(env []string, prefixes ...string) []string {
	if len(prefixes) == 0 {
		return append([]string(nil), env...)
	}
	out := make([]string, 0, len(env))
	for _, entry := range env {
		keep := true
		for _, prefix := range prefixes {
			if strings.HasPrefix(entry, prefix+"=") {
				keep = false
				break
			}
		}
		if keep {
			out = append(out, entry)
		}
	}
	return out
}

// extractBeadID parses a bead ID from bd or gc output.
func extractBeadID(t *testing.T, output string) string {
	t.Helper()

	re := regexp.MustCompile(`\b(?:bd|gc|mc)-[A-Za-z0-9]+\b`)
	if match := re.FindString(output); match != "" {
		return match
	}

	for _, prefix := range []string{"Created bead: ", "Created issue: "} {
		if idx := strings.Index(output, prefix); idx >= 0 {
			rest := output[idx+len(prefix):]
			fields := strings.Fields(rest)
			if len(fields) > 0 {
				return fields[0]
			}
		}
	}

	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "bd-") || strings.HasPrefix(line, "gc-") || strings.HasPrefix(line, "mc-") {
			fields := strings.Fields(line)
			if len(fields) > 0 {
				return fields[0]
			}
		}
	}

	t.Fatalf("could not parse bead ID from output: %s", output)
	return ""
}
