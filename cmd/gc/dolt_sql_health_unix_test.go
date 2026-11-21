//go:build unix

package main

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// TestRunManagedDoltSQLEmptyPasswordMasksDefaultProfileAgainstRealServer pins,
// against a real dolt, why an empty managed password still passes an explicit
// empty --password: without one, dolt injects the password of a `default` CLI
// profile, and the passwordless managed server then refuses the probe.
func TestRunManagedDoltSQLEmptyPasswordMasksDefaultProfileAgainstRealServer(t *testing.T) {
	_, port, _, cleanup := startRealDoltServer(t, "", "")
	defer cleanup()
	portText := fmt.Sprintf("%d", port)
	// A dolt CLI cold start on a loaded host can outlast the runner's
	// production bound, and this test pins argv, not latency.
	prevTimeout := managedDoltSQLCommandTimeout
	managedDoltSQLCommandTimeout = 30 * time.Second
	t.Cleanup(func() { managedDoltSQLCommandTimeout = prevTimeout })

	// startRealDoltServer points DOLT_ROOT_PATH at a per-test home, so the
	// profile lands in this test's global dolt config only.
	runExternal(t, "", "dolt", "profile", "add", "--password", "not-the-managed-password", "default")
	controlEnv := withEnvValue(os.Environ(), "DOLT_CLI_PASSWORD", "")
	stdout, stderr, err := runGCBeadsBdCommand(t, controlEnv, "dolt", "--host", "127.0.0.1", "--port", portText, "--user", "root", "--no-tls", "sql", "-q", "SELECT 1")
	if err == nil || !strings.Contains(stdout+stderr, "Access denied") {
		t.Fatalf("control: dolt without --password = %v, want the default profile's password refused, or this test proves nothing:\n%s%s", err, stdout, stderr)
	}

	if _, err := runManagedDoltSQL("127.0.0.1", portText, "root", "-r", "csv", "-q", "SELECT 1"); err != nil {
		t.Fatalf("runManagedDoltSQL() with an empty managed password error = %v, want the default profile's password masked", err)
	}
}
