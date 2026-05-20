package main

import (
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

func writeStandaloneBdPID(t *testing.T, cityPath string, contents string) {
	t.Helper()
	beadsDir := filepath.Join(cityPath, ".beads")
	if err := os.MkdirAll(beadsDir, 0o700); err != nil {
		t.Fatalf("MkdirAll(.beads): %v", err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "dolt-server.pid"), []byte(contents), 0o600); err != nil {
		t.Fatalf("WriteFile(dolt-server.pid): %v", err)
	}
}

func TestDetectStandaloneBdDoltNoPIDFile(t *testing.T) {
	cityPath := t.TempDir()
	pid, alive, err := detectStandaloneBdDoltWithAlive(cityPath, func(int) bool {
		t.Fatal("alive should not be called when pid file is missing")
		return false
	})
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if pid != 0 || alive {
		t.Fatalf("pid=%d alive=%v, want 0/false", pid, alive)
	}
}

func TestDetectStandaloneBdDoltEmptyPIDFile(t *testing.T) {
	cityPath := t.TempDir()
	writeStandaloneBdPID(t, cityPath, "   \n")
	pid, alive, err := detectStandaloneBdDoltWithAlive(cityPath, func(int) bool {
		t.Fatal("alive should not be called when pid file is empty")
		return false
	})
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if pid != 0 || alive {
		t.Fatalf("pid=%d alive=%v, want 0/false", pid, alive)
	}
}

func TestDetectStandaloneBdDoltAlivePID(t *testing.T) {
	cityPath := t.TempDir()
	writeStandaloneBdPID(t, cityPath, "12345\n")
	calls := 0
	pid, alive, err := detectStandaloneBdDoltWithAlive(cityPath, func(p int) bool {
		calls++
		if p != 12345 {
			t.Fatalf("alive called with pid=%d, want 12345", p)
		}
		return true
	})
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if pid != 12345 || !alive {
		t.Fatalf("pid=%d alive=%v, want 12345/true", pid, alive)
	}
	if calls != 1 {
		t.Fatalf("alive called %d times, want 1", calls)
	}
}

func TestDetectStandaloneBdDoltStalePID(t *testing.T) {
	cityPath := t.TempDir()
	writeStandaloneBdPID(t, cityPath, "67890")
	pid, alive, err := detectStandaloneBdDoltWithAlive(cityPath, func(p int) bool {
		if p != 67890 {
			t.Fatalf("alive called with pid=%d, want 67890", p)
		}
		return false
	})
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if pid != 67890 {
		t.Fatalf("pid=%d, want 67890", pid)
	}
	if alive {
		t.Fatal("alive=true, want false")
	}
}

func TestDetectStandaloneBdDoltMalformedPID(t *testing.T) {
	cityPath := t.TempDir()
	writeStandaloneBdPID(t, cityPath, "not-a-number\n")
	_, _, err := detectStandaloneBdDoltWithAlive(cityPath, func(int) bool {
		t.Fatal("alive should not be called when pid file is malformed")
		return false
	})
	if err == nil {
		t.Fatal("err = nil, want non-nil for malformed pid")
	}
	if !strings.Contains(err.Error(), "parse pid") {
		t.Fatalf("err = %v, want it to mention 'parse pid'", err)
	}
}

func TestDetectStandaloneBdDoltNegativePID(t *testing.T) {
	// Non-positive PID is suspicious; we report the file's contents but
	// never treat it as a live process (Alive guards against pid <= 0
	// anyway, but skipping the call avoids a meaningless syscall).
	cityPath := t.TempDir()
	writeStandaloneBdPID(t, cityPath, "-1\n")
	pid, alive, err := detectStandaloneBdDoltWithAlive(cityPath, func(int) bool {
		t.Fatal("alive should not be called for non-positive pid")
		return false
	})
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if pid != -1 || alive {
		t.Fatalf("pid=%d alive=%v, want -1/false", pid, alive)
	}
}

func TestStandaloneBdDoltConflictErrorContainsActionableHint(t *testing.T) {
	err := standaloneBdDoltConflictError("/tmp/city", 4242)
	if err == nil {
		t.Fatal("err = nil, want non-nil")
	}
	msg := err.Error()
	for _, want := range []string{"bd dolt stop", "gc start", "/tmp/city", "4242"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error message missing %q:\n%s", want, msg)
		}
	}
}

// TestStartBeadsLifecycleRefusesLiveStandaloneBdDolt drives startBeadsLifecycle
// with a city set up to use the bd-store contract and a .beads/dolt-server.pid
// pointing at the current test process (guaranteed alive). The conflict
// detection must surface as the standalone-bd error and ensureBeadsProvider
// must not run.
func TestStartBeadsLifecycleRefusesLiveStandaloneBdDolt(t *testing.T) {
	cityPath := t.TempDir()
	// Minimal canonical setup for cityUsesBdStoreContract: write
	// .beads/metadata.json and .beads/config.yaml so the function
	// proceeds past the canonical compat/drift validation. This is the
	// same shape other tests in beads_provider_lifecycle_test.go use.
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "metadata.json"),
		[]byte(`{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":"gc"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"),
		[]byte("issue_prefix: gc\ngc.endpoint_origin: managed_city\ngc.endpoint_status: verified\ndolt.auto-start: false\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Live PID = ours.
	writeStandaloneBdPID(t, cityPath, strconv.Itoa(os.Getpid()))

	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_BEADS_SCOPE_ROOT", cityPath)
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
	}
	err := startBeadsLifecycle(cityPath, "test-city", cfg, io.Discard)
	if err == nil {
		t.Fatal("startBeadsLifecycle returned nil, want standalone-bd conflict error")
	}
	for _, want := range []string{"bd dolt stop", "gc start"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("startBeadsLifecycle err = %v, want it to mention %q", err, want)
		}
	}
}

// TestStartBeadsLifecycleIgnoresStaleStandaloneBdPID drives startBeadsLifecycle
// with a stale .beads/dolt-server.pid (PID 1 is init/launchd on every host,
// so the alive stub returns false instead). The conflict detection must
// not short-circuit on a stale file — startBeadsLifecycle proceeds to its
// next step. We do not assert success of the rest of the lifecycle here
// because that path exec's gc-beads-bd which depends on dolt being
// installed; we only care that the error, if any, is NOT the standalone-bd
// conflict error.
func TestStartBeadsLifecycleIgnoresStaleStandaloneBdPID(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "metadata.json"),
		[]byte(`{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":"gc"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"),
		[]byte("issue_prefix: gc\ngc.endpoint_origin: managed_city\ngc.endpoint_status: verified\ndolt.auto-start: false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Almost-certainly-dead PID — 2147483646 (INT_MAX-1) is the largest
	// non-special pid on a 32-bit pid_t, far outside the typical pid
	// space on any real host.
	writeStandaloneBdPID(t, cityPath, "2147483646")

	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_BEADS_SCOPE_ROOT", cityPath)
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
	}
	err := startBeadsLifecycle(cityPath, "test-city", cfg, io.Discard)
	if err != nil && strings.Contains(err.Error(), "bd-managed dolt server is already running") {
		t.Fatalf("startBeadsLifecycle incorrectly tripped conflict detection on stale pid: %v", err)
	}
}
