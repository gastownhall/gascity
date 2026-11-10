package main

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/bazeltest"
)

func skipSlowCmdGCTest(t *testing.T, reason string) {
	t.Helper()
	if testing.Short() || strings.TrimSpace(os.Getenv("GC_FAST_UNIT")) != "0" {
		if strings.TrimSpace(os.Getenv("GC_FAST_UNIT")) == "" && !strings.Contains(reason, "test-cmd-gc-process") {
			reason += "; set GC_FAST_UNIT=0 or run make test-cmd-gc-process for full process coverage"
		}
		t.Skip(reason)
	}
}

// sanitizedBaseEnv returns os.Environ() with every GC_*/BEADS_* entry
// filtered out, followed by the given extras. Use this to build the
// `Env` for any exec.Cmd that runs the real gc-beads-bd lifecycle script
// or gc subcommands — inheriting os.Environ() raw lets GC_CITY_RUNTIME_DIR,
// GC_PACK_STATE_DIR, GC_DOLT_STATE_FILE, and friends point the child at
// the user's real registered city instead of the test's t.TempDir(),
// which silently overwrites user state on every run.
// Regression for gastownhall/gascity#938.
func sanitizedBaseEnv(extra ...string) []string {
	// Re-exec'd helper binaries must not inherit the bazel test-runner state
	// the parent owns: with the shard filter the go test runner would assign
	// the helper test to a different shard than the one requesting it and
	// exit with "no tests to run", surfacing as a readiness-pipe EOF in the
	// parent (#6638); with the coverage output path concurrent helpers would
	// clobber the parent's profile.
	base := bazeltest.HelperProcessEnv(os.Environ())
	filtered := make([]string, 0, len(base))
	for _, kv := range base {
		if strings.HasPrefix(kv, "GC_") || strings.HasPrefix(kv, "BEADS_") {
			continue
		}
		filtered = append(filtered, kv)
	}
	filtered = append(filtered,
		managedDoltTestModeEnv+"=1",
		managedDoltTestParentPIDEnv+"="+strconv.Itoa(os.Getpid()),
	)
	return append(filtered, extra...)
}

// writeTestScript creates a shell script that exits with the given code.
// If stderrMsg is non-empty, the script writes it to stderr before exiting.
func writeTestScript(t *testing.T, _ string, exitCode int, stderrMsg string) string {
	t.Helper()
	content := "#!/bin/sh\n"
	if stderrMsg != "" {
		content += "echo '" + stderrMsg + "' >&2\n"
	}
	content += "exit " + itoa(exitCode) + "\n"
	return writeNamedTestScript(t, "test-beads.sh", content)
}

func writeNamedTestScript(t *testing.T, name, content string) string {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, name)
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	return script
}

func writeManagedBdTestScript(t *testing.T, content string) string {
	t.Helper()
	return writeNamedTestScript(t, "gc-beads-bd.sh", content)
}

func itoa(n int) string {
	return []string{"0", "1", "2"}[n]
}

// listenOnRandomPort returns a live listener on a 127.0.0.1 port. It also holds
// the port's [::1] twin until t.Cleanup, so a listener a concurrent test action
// owns on [::1] cannot be mistaken for this one (see holdLoopbackPort).
func listenOnRandomPort(t *testing.T) net.Listener {
	t.Helper()
	const tries = 64
	for i := 0; i < tries; i++ {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		if holdLoopbackTwin(t, ln.Addr().(*net.TCPAddr).Port) {
			return ln
		}
		_ = ln.Close()
	}
	t.Fatalf("no port free on both loopback families in %d tries", tries)
	return nil
}

// reserveRandomTCPPort returns a loopback port number that stays held until
// t.Cleanup, so no other process is handed it between this pick and the
// caller's own bind. A fixture that picks a port number holds it.
func reserveRandomTCPPort(t *testing.T) int {
	t.Helper()
	return holdLoopbackPort(t, 0)
}

func startTCPListenerProcess(t *testing.T, port int) *exec.Cmd {
	t.Helper()
	skipSlowCmdGCTest(t, "spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage")
	cmd, err := launchTCPListenerProcess(t.TempDir(), port)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return cmd
}

func writeDoltState(cityPath string, state doltRuntimeState) error {
	stateDir := filepath.Join(cityPath, ".gc", "runtime", "packs", "dolt")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return err
	}
	data := fmt.Sprintf(`{"running":%t,"pid":%d,"port":%d,"data_dir":%q,"started_at":%q}`,
		state.Running, state.PID, state.Port, state.DataDir, state.StartedAt)
	return os.WriteFile(filepath.Join(stateDir, "dolt-state.json"), []byte(data), 0o644)
}
