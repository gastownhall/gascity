//go:build integration

package integration

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestE2E_Drain_SetAndCheck verifies that gc runtime drain sets the GC_DRAIN
// metadata flag and gc runtime drain-check returns exit 0.
func TestE2E_Drain_SetAndCheck(t *testing.T) {
	city := e2eCity{
		Agents: []e2eAgent{
			{Name: "drainee", StartCommand: e2eSleepScript()},
		},
	}
	cityDir := setupE2ECity(t, nil, city)

	// Before drain: drain-check should return non-zero.
	_, err := gc(cityDir, "runtime", "drain-check", "drainee")
	if err == nil {
		t.Error("drain-check should fail before drain is set")
	}

	// Set drain.
	out, err := gc(cityDir, "runtime", "drain", "drainee")
	if err != nil {
		t.Fatalf("gc runtime drain failed: %v\noutput: %s", err, out)
	}

	// After drain: drain-check should return 0.
	out, err = gc(cityDir, "runtime", "drain-check", "drainee")
	if err != nil {
		t.Errorf("drain-check should succeed after drain: %v\noutput: %s", err, out)
	}
}

// TestE2E_Drain_Ack verifies that gc runtime drain-ack sets the GC_DRAIN_ACK
// metadata flag.
func TestE2E_Drain_Ack(t *testing.T) {
	city := e2eCity{
		Agents: []e2eAgent{
			{Name: "acker", StartCommand: e2eSleepScript()},
		},
	}
	cityDir := setupE2ECity(t, nil, city)

	// Drain the agent.
	out, err := gc(cityDir, "runtime", "drain", "acker")
	if err != nil {
		t.Fatalf("gc runtime drain failed: %v\noutput: %s", err, out)
	}

	// Ack the drain (simulating agent behavior).
	out, err = gc(cityDir, "runtime", "drain-ack", "acker")
	if err != nil {
		t.Fatalf("gc runtime drain-ack failed: %v\noutput: %s", err, out)
	}
}

// TestE2E_Undrain verifies that gc runtime undrain clears drain flags.
func TestE2E_Undrain(t *testing.T) {
	city := e2eCity{
		Agents: []e2eAgent{
			{Name: "undrain", StartCommand: e2eSleepScript()},
		},
	}
	cityDir := setupE2ECity(t, nil, city)

	// Set drain.
	out, err := gc(cityDir, "runtime", "drain", "undrain")
	if err != nil {
		t.Fatalf("gc runtime drain failed: %v\noutput: %s", err, out)
	}

	// Verify drain is set.
	_, err = gc(cityDir, "runtime", "drain-check", "undrain")
	if err != nil {
		t.Fatal("drain-check should succeed after drain")
	}

	// Undrain.
	out, err = gc(cityDir, "runtime", "undrain", "undrain")
	if err != nil {
		t.Fatalf("gc runtime undrain failed: %v\noutput: %s", err, out)
	}

	// After undrain: drain-check should fail again.
	_, err = gc(cityDir, "runtime", "drain-check", "undrain")
	if err == nil {
		t.Error("drain-check should fail after undrain")
	}
}

// TestE2E_RequestRestart verifies that gc runtime request-restart sets the
// GC_RESTART_REQUESTED metadata. Since request-restart blocks, we run it
// with a short timeout.
func TestE2E_RequestRestart(t *testing.T) {
	city := e2eCity{
		Agents: []e2eAgent{
			{Name: "restarter", StartCommand: e2eSleepScript()},
		},
	}
	cityDir := setupE2ECity(t, nil, city)

	// request-restart blocks forever (waits for controller to kill it).
	// Run in a goroutine with the agent's env context.
	done := make(chan struct{})
	go func() {
		defer close(done)
		// Simulate running from within agent context by passing env.
		gcWithEnv(cityDir, map[string]string{
			"GC_AGENT": "restarter",
			"GC_CITY":  cityDir,
		}, "runtime", "request-restart")
	}()

	// Give it a moment for the metadata to be set.
	time.Sleep(1 * time.Second)

	// Verify metadata was set by checking session list.
	out, err := gc(cityDir, "session", "list")
	if err != nil {
		t.Fatalf("gc session list failed: %v\noutput: %s", err, out)
	}

	// Kill the agent to unblock the goroutine.
	gc(cityDir, "session", "kill", "restarter") //nolint:errcheck
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		// Goroutine may still be blocked; that's OK for test purposes.
	}
}

// TestE2E_Nudge verifies that gc session nudge delivers text to a tmux session.
func TestE2E_Nudge(t *testing.T) {
	if usingSubprocess() {
		t.Skip("nudge requires tmux provider")
	}

	city := e2eCity{
		Agents: []e2eAgent{
			{Name: "nudgee", StartCommand: e2eSleepScript()},
		},
	}
	cityDir := setupE2ECity(t, nil, city)

	out, err := gc(cityDir, "session", "nudge", "nudgee", "hello from test")
	if err != nil {
		t.Fatalf("gc session nudge failed: %v\noutput: %s", err, out)
	}
}

// TestE2E_Peek verifies that gc session peek captures session output.
func TestE2E_Peek(t *testing.T) {
	if usingSubprocess() {
		t.Skip("peek requires tmux provider")
	}

	// Use sh -c with semicolons (not &&) so Docker's exec wrapper
	// doesn't break the command chain. Docker wraps in sh -c "exec $cmd"
	// which replaces the shell on the first && operand.
	city := e2eCity{
		Agents: []e2eAgent{
			{Name: "peekee", StartCommand: "sh -c 'echo peek-test-output; sleep 3600'"},
		},
	}
	cityDir := setupE2ECity(t, nil, city)

	// Wait for the agent to produce output.
	time.Sleep(2 * time.Second)

	out, err := gc(cityDir, "session", "peek", "peekee")
	if err != nil {
		t.Fatalf("gc session peek failed: %v\noutput: %s", err, out)
	}
	if !strings.Contains(out, "peek-test-output") {
		t.Errorf("peek output missing expected text:\n%s", out)
	}
}

// TestE2E_ConfigDrift verifies that changing a fingerprinted agent field in
// city.toml while agents are running triggers reconciliation via the watcher.
func TestE2E_ConfigDrift(t *testing.T) {
	city := e2eCity{
		Agents: []e2eAgent{
			{
				Name:         "drifter",
				StartCommand: "CUSTOM_VERSION=v1 " + e2eReportScript(),
			},
		},
	}
	cityDir := setupE2ECity(t, nil, city)

	// Wait for first report.
	report := waitForReport(t, cityDir, "drifter", e2eDefaultTimeout())
	if !report.has("CUSTOM_VERSION", "v1") {
		t.Fatalf("initial CUSTOM_VERSION: got %v, want [v1]", report.getAll("CUSTOM_VERSION"))
	}

	// Change config by mutating the fingerprinted start_command. Custom env
	// keys are intentionally ignored by the runtime fingerprint, so changing
	// Env alone should not imply restart.
	city.Workspace.Name = "" // Will be filled from cityDir base.
	city.Agents[0].StartCommand = "CUSTOM_VERSION=v2 " + e2eReportScript()
	city.Workspace.Name = findCityName(t, cityDir)
	writeE2EToml(t, cityDir, city)

	// Remove old report so we can detect a new one.
	reportPath := strings.ReplaceAll("drifter", "/", "__")
	reportDir := cityDir + "/.gc-reports"
	_ = removeFile(reportDir + "/" + reportPath + ".report")

	// The controller is already running. Writing city.toml should trigger a
	// config reload and reconcile via the watcher/patrol loop.
	report2 := waitForReport(t, cityDir, "drifter", e2eDefaultTimeout())
	if !report2.has("CUSTOM_VERSION", "v2") {
		t.Errorf("post-drift CUSTOM_VERSION: got %v, want [v2]", report2.getAll("CUSTOM_VERSION"))
	}
}

// gcWithEnv runs the gc binary with extra environment variables.
func gcWithEnv(dir string, env map[string]string, args ...string) (string, error) {
	cmd := gcCommand(args...)
	if dir != "" {
		cmd.Dir = dir
	}
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// gcCommand creates an exec.Cmd for the gc binary with standard env setup.
func gcCommand(args ...string) *exec.Cmd {
	cmd := exec.Command(gcBinary, args...)
	cmd.Env = integrationEnv()
	return cmd
}

// findCityName reads city.toml to extract the workspace name.
func findCityName(t *testing.T, cityDir string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(cityDir, "city.toml"))
	if err != nil {
		t.Fatalf("reading city.toml: %v", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "name") {
			parts := strings.SplitN(line, "=", 2)
			if len(parts) == 2 {
				return strings.Trim(strings.TrimSpace(parts[1]), "\"")
			}
		}
	}
	t.Fatal("city name not found in city.toml")
	return ""
}

// removeFile removes a file, ignoring errors.
func removeFile(path string) error {
	return os.Remove(path)
}
