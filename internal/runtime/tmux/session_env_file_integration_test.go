//go:build integration

package tmux

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestSecretEnvIsAbsentFromProcCmdline is the end-to-end proof against a real
// tmux: launch a session whose env holds a canary and then read the process
// table the way an unprivileged local user would.
//
// The inert-env session is the negative control. Without it the scan could pass
// because it never worked, not because the leak is closed: an inert value still
// rides -e, so it MUST show up in the same scan that finds nothing for the
// secret one. Each session gets its own socket so each `tmux new-session` forks
// its own long-lived server — the process that inherits the client argv and
// holds it for the session's whole life.
func TestSecretEnvIsAbsentFromProcCmdline(t *testing.T) {
	if !hasTmux() {
		t.Skip("tmux not installed")
	}
	if _, err := os.Stat("/proc/self/cmdline"); err != nil {
		t.Skip("no /proc")
	}

	stamp := time.Now().UnixNano()
	control := fmt.Sprintf("gc-argv-control-canary-%d", stamp)
	secret := fmt.Sprintf("gc-argv-secret-canary-%d", stamp)

	newTmux := func(socket string) *Tmux {
		cfg := DefaultConfig()
		cfg.SocketName = socket
		return NewTmuxWithConfig(cfg)
	}
	controlTm := newTmux(privateSocketName("argvctl"))
	secretTm := newTmux(privateSocketName("argvsec"))

	// GC_RIG is on the argv allow list; ANTHROPIC_AUTH_TOKEN is not.
	if err := controlTm.NewSessionWithCommandAndEnv("gcargvctl", "", "sleep 120",
		map[string]string{"GC_RIG": control}); err != nil {
		t.Fatalf("control NewSessionWithCommandAndEnv: %v", err)
	}
	t.Cleanup(func() { _ = controlTm.KillSession("gcargvctl") })

	if err := secretTm.NewSessionWithCommandAndEnv("gcargvsec", "", "sleep 120",
		map[string]string{"ANTHROPIC_AUTH_TOKEN": secret}); err != nil {
		t.Fatalf("secret NewSessionWithCommandAndEnv: %v", err)
	}
	t.Cleanup(func() { _ = secretTm.KillSession("gcargvsec") })

	if !procCmdlineContains(t, control) {
		t.Fatal("negative control failed: an -e value did NOT reach /proc/*/cmdline, so this scan proves nothing")
	}
	if procCmdlineContains(t, secret) {
		t.Error("secret env value is readable in /proc/*/cmdline")
	}

	// The value still has to arrive, or we have closed the leak by breaking the
	// feature. Check both the tmux session environment (GC_INSTANCE_TOKEN
	// readback depends on it) and a process launched inside the session.
	got, err := secretTm.GetEnvironment("gcargvsec", "ANTHROPIC_AUTH_TOKEN")
	if err != nil {
		t.Fatalf("GetEnvironment: %v", err)
	}
	if got != secret {
		t.Errorf("session env did not receive the value (len %d, want %d)", len(got), len(secret))
	}

	out := filepath.Join(t.TempDir(), "child-env")
	if _, err := secretTm.run("new-window", "-d", "-t", "gcargvsec",
		"sh -c 'printf %s \"$ANTHROPIC_AUTH_TOKEN\" > "+out+"'"); err != nil {
		t.Fatalf("new-window: %v", err)
	}
	var body []byte
	for range 50 {
		if body, err = os.ReadFile(out); err == nil && len(body) > 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if string(body) != secret {
		t.Errorf("process in the session did not inherit the value (len %d, want %d)", len(body), len(secret))
	}

	// No staged directory may outlive the call that needed it.
	if leftovers := stagedDirs(t); len(leftovers) != 0 {
		t.Errorf("staged directories survived session creation: %v", leftovers)
	}
}

// procCmdlineContains reports whether any process's command line contains
// needle. It reads only the boolean out — a caller must never log what it
// found, which is the whole point of the check.
func procCmdlineContains(t *testing.T, needle string) bool {
	t.Helper()
	entries, err := os.ReadDir("/proc")
	if err != nil {
		t.Fatalf("read /proc: %v", err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		body, err := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		if err != nil {
			continue // process exited, or not ours to read
		}
		if strings.Contains(string(body), needle) {
			return true
		}
	}
	return false
}

// stagedDirs lists the staged tmux command directories still present in the
// temp dir.
func stagedDirs(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(os.TempDir())
	if err != nil {
		t.Fatalf("read temp dir: %v", err)
	}
	var found []string
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), stagedDirPrefix) {
			found = append(found, e.Name())
		}
	}
	return found
}
