//go:build unix

package main

// The stand-in for a live standalone dolt is a shell process that signals
// readiness through a FIFO. Both the shell and the FIFO are Unix mechanisms;
// the remaining cases in dolt_standalone_conflict_test.go need neither and
// run everywhere.
import (
	"github.com/gastownhall/gascity/internal/config"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

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
	cmd := exec.Command("bash", "-c", `exec -a dolt cat sql-server -- --data-dir "$1"`, "fake-dolt", dataDir)
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
