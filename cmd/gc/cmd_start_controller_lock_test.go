package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// newForegroundStartLockCity writes a city whose bead store is an exec
// provider script. The script appends each op it is asked to run to the
// returned log, followed by the controller-lock state when the op ran. Its
// "start" op leaves a long-lived child behind, the way a real provider leaves
// its server running, and records the child's PID; the helper kills it on
// cleanup. startExit is the exit status the script returns for "start".
func newForegroundStartLockCity(t *testing.T, startExit string) (cityPath, opsLog string) {
	t.Helper()
	cityPath = t.TempDir()
	clearInheritedBeadsEnv(t)
	t.Chdir(t.TempDir())

	if err := os.MkdirAll(filepath.Join(cityPath, citylayout.RuntimeRoot), 0o755); err != nil {
		t.Fatalf("scaffold runtime root: %v", err)
	}
	opsLog = filepath.Join(cityPath, "provider-ops")
	lockPath := filepath.Join(cityPath, ".gc", "controller.lock")
	childPID := filepath.Join(cityPath, "provider-child.pid")
	t.Cleanup(func() {
		data, err := os.ReadFile(childPID)
		if err != nil {
			return
		}
		if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && pid > 0 {
			syscall.Kill(pid, syscall.SIGKILL) //nolint:errcheck // best-effort fixture cleanup
		}
	})
	script := filepath.Join(cityPath, "provider.sh")
	body := "#!/bin/sh\n" +
		"state=free\n" +
		"if command -v flock >/dev/null 2>&1 && ! flock -n \"" + lockPath + "\" true 2>/dev/null; then state=held; fi\n" +
		"printf '%s lock=%s\\n' \"$1\" \"$state\" >> \"" + opsLog + "\"\n" +
		"if [ \"$1\" = start ]; then\n" +
		"  sleep 60 </dev/null >/dev/null 2>&1 &\n" +
		"  echo $! > \"" + childPID + "\"\n" +
		"  exit " + startExit + "\n" +
		"fi\n" +
		"exit 0\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil { //nolint:gosec // test fixture must be executable
		t.Fatalf("write provider script: %v", err)
	}
	cityTOML := "[workspace]\nname = \"lock-order-city\"\n\n" +
		"[beads]\nprovider = \"exec:" + script + "\"\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityTOML), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}

	oldBuild := buildSessionProviderByName
	t.Cleanup(func() { buildSessionProviderByName = oldBuild })
	buildSessionProviderByName = func(_ *config.City, _ string, _ config.SessionConfig, _, _ string) (runtime.Provider, error) {
		return runtime.NewFake(), nil
	}
	return cityPath, opsLog
}

func readProviderOps(t *testing.T, opsLog string) []string {
	t.Helper()
	data, err := os.ReadFile(opsLog)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("read provider ops: %v", err)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

// A foreground start that cannot become the controller must not touch the
// bead-store provider. gc stop holds the controller lock while it retires the
// provider; a `gc start --foreground` that started the provider first and
// only then found the lock taken restarted the provider underneath that stop
// and exited, leaving whatever it had started behind.
func TestStartForegroundLeavesBeadsProviderAloneWhileControllerLockHeld(t *testing.T) {
	cityPath, opsLog := newForegroundStartLockCity(t, "0")

	// Stands in for gc stop retiring the provider (or a live controller).
	held, err := acquireControllerLock(cityPath)
	if err != nil {
		t.Fatalf("acquireControllerLock: %v", err)
	}
	defer held.Close() //nolint:errcheck // test cleanup

	var stdout, stderr bytes.Buffer
	if code := doStartStandalone([]string{cityPath}, true, &stdout, &stderr); code != 1 {
		t.Fatalf("doStartStandalone exit = %d, want 1\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), errControllerAlreadyRunning.Error()) {
		t.Errorf("stderr = %q, want the controller-lock refusal", stderr.String())
	}
	if ops := readProviderOps(t, opsLog); len(ops) != 0 {
		t.Fatalf("provider ops = %q, want none: a start that cannot take the controller lock ran the bead-store provider", ops)
	}
}

// The foreground start takes the controller lock before it starts the
// provider and releases it when startup fails. The provider's server outlives
// the start, so the lock descriptor must not leak into it: an inherited lock
// would keep every later controller out for as long as the server runs.
func TestStartForegroundStartsBeadsProviderUnderControllerLock(t *testing.T) {
	if _, err := exec.LookPath("flock"); err != nil {
		t.Skip("flock binary not available to probe the controller lock from the provider script")
	}
	cityPath, opsLog := newForegroundStartLockCity(t, "1")

	var stdout, stderr bytes.Buffer
	if code := doStartStandalone([]string{cityPath}, true, &stdout, &stderr); code != 1 {
		t.Fatalf("doStartStandalone exit = %d, want 1 (provider start fails)\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
	}
	ops := readProviderOps(t, opsLog)
	if len(ops) == 0 || ops[0] != "start lock=held" {
		t.Fatalf("provider ops = %q, want the first op to be start with the controller lock held", ops)
	}

	lock, err := acquireControllerLock(cityPath)
	if err != nil {
		t.Fatalf("controller lock still held after the failed start returned (leaked into the provider's child?): %v", err)
	}
	lock.Close() //nolint:errcheck // probe cleanup
}
