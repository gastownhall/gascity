package main

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

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

func writeManagedBdCityFixture(t *testing.T, cityPath string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"test-city\"\n"), 0o644); err != nil {
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
}

func startStandaloneBdDoltLikeProcess(t *testing.T, dataDir string) *exec.Cmd {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("requires bash for exec -a")
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatalf("MkdirAll(dataDir): %v", err)
	}
	fifo := filepath.Join(dataDir, "sql-server")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatalf("Mkfifo(sql-server): %v", err)
	}
	cmd := exec.Command("bash", "-c", "exec -a dolt cat sql-server")
	cmd.Dir = dataDir
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start fake dolt sql-server: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	// 30s: must outlast a worst-case processArgsPSTimeout (10s) ps fallback plus
	// process-exec/proc-reflection latency under heavy parallel CI load.
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if cmd.Process.Signal(syscall.Signal(0)) == nil && processLooksLikeDoltSQLServer(cmd.Process.Pid, dataDir) {
			return cmd
		}
		time.Sleep(10 * time.Millisecond)
	}
	args, _ := processArgs(cmd.Process.Pid)
	t.Fatalf("fake dolt sql-server did not become inspectable; pid=%d args=%q", cmd.Process.Pid, args)
	return cmd
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
	pid, alive, err := detectStandaloneBdDoltWith(cityPath, func(p int) bool {
		calls++
		if p != 12345 {
			t.Fatalf("alive called with pid=%d, want 12345", p)
		}
		return true
	}, func(p int, dataDir string) bool {
		if p != 12345 {
			t.Fatalf("process match called with pid=%d, want 12345", p)
		}
		if dataDir != filepath.Join(cityPath, ".beads", "dolt") {
			t.Fatalf("process match called with dataDir=%q, want city dolt dir", dataDir)
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

func TestDetectStandaloneBdDoltLiveUnrelatedPID(t *testing.T) {
	cityPath := t.TempDir()
	writeStandaloneBdPID(t, cityPath, "12345\n")
	pid, alive, err := detectStandaloneBdDoltWith(cityPath, func(p int) bool {
		if p != 12345 {
			t.Fatalf("alive called with pid=%d, want 12345", p)
		}
		return true
	}, func(p int, dataDir string) bool {
		if p != 12345 {
			t.Fatalf("process match called with pid=%d, want 12345", p)
		}
		if dataDir != filepath.Join(cityPath, ".beads", "dolt") {
			t.Fatalf("process match called with dataDir=%q, want city dolt dir", dataDir)
		}
		return false
	})
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if pid != 12345 {
		t.Fatalf("pid=%d, want 12345", pid)
	}
	if alive {
		t.Fatal("alive=true for live non-dolt pid, want false")
	}
}

func TestDetectStandaloneBdDoltLiveDoltForDifferentDataDirDoesNotConflict(t *testing.T) {
	cityPath := t.TempDir()
	otherCityPath := t.TempDir()
	proc := startStandaloneBdDoltLikeProcess(t, filepath.Join(otherCityPath, ".beads", "dolt"))
	writeStandaloneBdPID(t, cityPath, strconv.Itoa(proc.Process.Pid))
	pid, alive, err := detectStandaloneBdDolt(cityPath)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if pid != proc.Process.Pid {
		t.Fatalf("pid=%d, want %d", pid, proc.Process.Pid)
	}
	if alive {
		t.Fatal("alive=true for dolt sql-server with different data dir, want false")
	}
}

func TestDetectStandaloneBdDoltCurrentProcessPIDDoesNotConflict(t *testing.T) {
	cityPath := t.TempDir()
	writeStandaloneBdPID(t, cityPath, strconv.Itoa(os.Getpid()))
	pid, alive, err := detectStandaloneBdDolt(cityPath)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if pid != os.Getpid() {
		t.Fatalf("pid=%d, want current process pid %d", pid, os.Getpid())
	}
	if alive {
		t.Fatal("alive=true for current test process, want false because it is not dolt sql-server")
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
	for _, want := range []string{"bd dolt stop", "/tmp/city", "4242"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error message missing %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "gc start") {
		t.Fatalf("error message should not hardcode command-specific retry guidance:\n%s", msg)
	}
}

func TestParseDoltPSCommandLineHandlesSpacedExecutablePath(t *testing.T) {
	argv := parseDoltPSCommandLine("/Applications/Dolt Tools/bin/dolt sql-server --config /tmp/city/.gc/runtime/packs/dolt/dolt-config.yaml")
	if !looksLikeDoltSQLServer(argv) {
		t.Fatalf("parseDoltPSCommandLine did not preserve dolt sql-server shape: %#v", argv)
	}
	if len(argv) != 4 || argv[2] != "--config" || argv[3] != "/tmp/city/.gc/runtime/packs/dolt/dolt-config.yaml" {
		t.Fatalf("parseDoltPSCommandLine argv = %#v, want config preserved", argv)
	}
}

// TestStartBeadsLifecycleRefusesLiveStandaloneBdDolt drives startBeadsLifecycle
// with a city set up to use the bd-store contract and a .beads/dolt-server.pid
// pointing at the current test process (guaranteed alive). The conflict
// detection must surface as the standalone-bd error and ensureBeadsProvider
// must not run.
func TestStartBeadsLifecycleRefusesLiveStandaloneBdDolt(t *testing.T) {
	cityPath := t.TempDir()
	writeManagedBdCityFixture(t, cityPath)
	proc := startStandaloneBdDoltLikeProcess(t, filepath.Join(cityPath, ".beads", "dolt"))
	writeStandaloneBdPID(t, cityPath, strconv.Itoa(proc.Process.Pid))

	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_BEADS_SCOPE_ROOT", cityPath)
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
	}
	err := startBeadsLifecycle(cityPath, "test-city", cfg, io.Discard)
	if err == nil {
		t.Fatal("startBeadsLifecycle returned nil, want standalone-bd conflict error")
	}
	for _, want := range []string{"bd dolt stop"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("startBeadsLifecycle err = %v, want it to mention %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "gc start") {
		t.Fatalf("startBeadsLifecycle err should not hardcode retry command: %v", err)
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
	writeManagedBdCityFixture(t, cityPath)
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

func TestInitDirIfReadyDetectsStandaloneBdDoltAtProviderConvergence(t *testing.T) {
	cityPath := t.TempDir()
	writeManagedBdCityFixture(t, cityPath)
	materializeBuiltinPacksForTest(t, cityPath)

	callLog := filepath.Join(cityPath, "provider-start.log")
	script := gcBeadsBdScriptPath(cityPath)
	content := "#!/bin/sh\n" +
		"echo \"$1\" >> \"" + callLog + "\"\n" +
		"echo 'provider should not start' >&2\n" +
		"exit 1\n"
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}

	proc := startStandaloneBdDoltLikeProcess(t, filepath.Join(cityPath, ".beads", "dolt"))
	writeStandaloneBdPID(t, cityPath, strconv.Itoa(proc.Process.Pid))

	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_BEADS_SCOPE_ROOT", cityPath)
	deferred, err := initDirIfReady(cityPath, cityPath, "gc")
	if err == nil {
		t.Fatal("initDirIfReady returned nil, want standalone-bd conflict error")
	}
	if deferred {
		t.Fatal("initDirIfReady deferred = true, want false")
	}
	for _, want := range []string{"bd dolt stop"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("initDirIfReady err = %v, want it to mention %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "gc start") {
		t.Fatalf("initDirIfReady err should not hardcode retry command: %v", err)
	}
	if _, statErr := os.Stat(callLog); !os.IsNotExist(statErr) {
		t.Fatalf("provider script should not have started, stat err = %v", statErr)
	}
}
