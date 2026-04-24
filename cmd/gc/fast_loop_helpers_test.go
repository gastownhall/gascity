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
	"time"
)

func skipSlowCmdGCTest(t *testing.T, reason string) {
	t.Helper()
	if os.Getenv("GC_FAST_UNIT") == "1" || testing.Short() {
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
	filtered := make([]string, 0, len(os.Environ()))
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "GC_") || strings.HasPrefix(kv, "BEADS_") {
			continue
		}
		filtered = append(filtered, kv)
	}
	return append(filtered, extra...)
}

// writeTestScript creates a shell script that exits with the given code.
// If stderrMsg is non-empty, the script writes it to stderr before exiting.
func writeTestScript(t *testing.T, _ string, exitCode int, stderrMsg string) string {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "test-beads.sh")

	content := "#!/bin/sh\n"
	if stderrMsg != "" {
		content += "echo '" + stderrMsg + "' >&2\n"
	}
	content += "exit " + itoa(exitCode) + "\n"

	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	return script
}

func itoa(n int) string {
	return []string{"0", "1", "2"}[n]
}

func listenOnRandomPort(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	return ln
}

func reserveRandomTCPPort(t *testing.T) int {
	t.Helper()
	ln := listenOnRandomPort(t)
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}

func startTCPListenerProcess(t *testing.T, port int) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("python3", "-c", `
import signal
import socket
import sys
import time
port = int(sys.argv[1])
sock = socket.socket()
sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
sock.bind(("127.0.0.1", port))
sock.listen(5)
def _stop(*_args):
    raise SystemExit(0)
signal.signal(signal.SIGTERM, _stop)
signal.signal(signal.SIGINT, _stop)
while True:
    time.sleep(1)
`, strconv.Itoa(port))
	if err := cmd.Start(); err != nil {
		t.Fatalf("start listener process: %v", err)
	}
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
	})
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return cmd
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("listener process on %d did not become ready", port)
	return nil
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
