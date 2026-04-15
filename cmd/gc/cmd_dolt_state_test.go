package main

import (
	"bytes"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/citylayout"
)

func parseDoltStateOutput(t *testing.T, out string) map[string]string {
	t.Helper()
	values := map[string]string{}
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) != 2 {
			t.Fatalf("state output line %q missing tab separator", line)
		}
		values[parts[0]] = parts[1]
	}
	return values
}

func parseDoltRuntimeLayoutOutput(t *testing.T, out string) map[string]string {
	t.Helper()
	values := map[string]string{}
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) != 2 {
			t.Fatalf("runtime-layout line %q missing tab separator", line)
		}
		values[parts[0]] = parts[1]
	}
	return values
}

func TestDoltStateRuntimeLayoutCmdUsesCanonicalPaths(t *testing.T) {
	cityPath := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := run([]string{"dolt-state", "runtime-layout", "--city", cityPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run() = %d, stderr = %s", code, stderr.String())
	}
	got := parseDoltRuntimeLayoutOutput(t, stdout.String())
	wantPack := citylayout.PackStateDir(cityPath, "dolt")
	want := map[string]string{
		"GC_PACK_STATE_DIR":   wantPack,
		"GC_DOLT_DATA_DIR":    filepath.Join(cityPath, ".beads", "dolt"),
		"GC_DOLT_LOG_FILE":    filepath.Join(wantPack, "dolt.log"),
		"GC_DOLT_STATE_FILE":  filepath.Join(wantPack, "dolt-provider-state.json"),
		"GC_DOLT_PID_FILE":    filepath.Join(wantPack, "dolt.pid"),
		"GC_DOLT_LOCK_FILE":   filepath.Join(wantPack, "dolt.lock"),
		"GC_DOLT_CONFIG_FILE": filepath.Join(wantPack, "dolt-config.yaml"),
	}
	for key, wantValue := range want {
		if got[key] != wantValue {
			t.Fatalf("%s = %q, want %q; output=%q", key, got[key], wantValue, stdout.String())
		}
	}
}

func TestDoltStateRuntimeLayoutCmdHonorsProjectedOverrides(t *testing.T) {
	cityPath := t.TempDir()
	t.Setenv("GC_CITY_RUNTIME_DIR", "/runtime-root")
	t.Setenv("GC_DOLT_DATA_DIR", "/data-root")
	t.Setenv("GC_DOLT_LOG_FILE", "/logs/dolt.log")
	t.Setenv("GC_DOLT_STATE_FILE", "/state/dolt-provider-state.json")
	t.Setenv("GC_DOLT_PID_FILE", "/state/dolt.pid")
	t.Setenv("GC_DOLT_LOCK_FILE", "/state/dolt.lock")
	t.Setenv("GC_DOLT_CONFIG_FILE", "/state/dolt-config.yaml")

	var stdout, stderr bytes.Buffer
	code := run([]string{"dolt-state", "runtime-layout", "--city", cityPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run() = %d, stderr = %s", code, stderr.String())
	}
	got := parseDoltRuntimeLayoutOutput(t, stdout.String())
	want := map[string]string{
		"GC_PACK_STATE_DIR":   filepath.Join("/runtime-root", "packs", "dolt"),
		"GC_DOLT_DATA_DIR":    "/data-root",
		"GC_DOLT_LOG_FILE":    "/logs/dolt.log",
		"GC_DOLT_STATE_FILE":  "/state/dolt-provider-state.json",
		"GC_DOLT_PID_FILE":    "/state/dolt.pid",
		"GC_DOLT_LOCK_FILE":   "/state/dolt.lock",
		"GC_DOLT_CONFIG_FILE": "/state/dolt-config.yaml",
	}
	for key, wantValue := range want {
		if got[key] != wantValue {
			t.Fatalf("%s = %q, want %q; output=%q", key, got[key], wantValue, stdout.String())
		}
	}
}

func TestDoltStateAllocatePortCmdHonorsEnvOverride(t *testing.T) {
	cityPath := t.TempDir()
	t.Setenv("GC_DOLT_PORT", "4406")

	var stdout, stderr bytes.Buffer
	code := run([]string{"dolt-state", "allocate-port", "--city", cityPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run() = %d, stderr = %s", code, stderr.String())
	}
	if got := strings.TrimSpace(stdout.String()); got != "4406" {
		t.Fatalf("allocate-port = %q, want 4406", got)
	}
}

func TestDoltStateAllocatePortCmdReusesLiveProviderState(t *testing.T) {
	cityPath := t.TempDir()
	stateFile := filepath.Join(t.TempDir(), "dolt-provider-state.json")
	proc := exec.Command("sleep", "30")
	if err := proc.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	defer func() {
		_ = proc.Process.Kill()
		_, _ = proc.Process.Wait()
	}()
	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", "43123"))
	if err != nil {
		t.Fatalf("listen on provider-state port: %v", err)
	}
	defer listener.Close() //nolint:errcheck
	if err := writeDoltRuntimeStateFile(stateFile, doltRuntimeState{
		Running:   true,
		PID:       proc.Process.Pid,
		Port:      43123,
		DataDir:   filepath.Join(cityPath, ".beads", "dolt"),
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("writeDoltRuntimeStateFile: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"dolt-state", "allocate-port", "--city", cityPath, "--state-file", stateFile}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run() = %d, stderr = %s", code, stderr.String())
	}
	if got := strings.TrimSpace(stdout.String()); got != "43123" {
		t.Fatalf("allocate-port = %q, want 43123", got)
	}
}

func TestDoltStateAllocatePortCmdRepairsStaleProviderStateFromOwnedLivePortHolder(t *testing.T) {
	cityPath := t.TempDir()
	stateFile := filepath.Join(t.TempDir(), "dolt-provider-state.json")

	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", "0"))
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close() //nolint:errcheck
	port := listener.Addr().(*net.TCPAddr).Port

	if err := writeDoltRuntimeStateFile(stateFile, doltRuntimeState{
		Running:   true,
		PID:       999999,
		Port:      port,
		DataDir:   filepath.Join(cityPath, ".beads", "dolt"),
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("writeDoltRuntimeStateFile: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"dolt-state", "allocate-port", "--city", cityPath, "--state-file", stateFile}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run() = %d, stderr = %s", code, stderr.String())
	}
	if got := strings.TrimSpace(stdout.String()); got != strconv.Itoa(port) {
		t.Fatalf("allocate-port = %q, want %d", got, port)
	}

	state, err := readDoltRuntimeStateFile(stateFile)
	if err != nil {
		t.Fatalf("readDoltRuntimeStateFile: %v", err)
	}
	if !state.Running {
		t.Fatalf("repaired state running = false, want true")
	}
	if state.Port != port {
		t.Fatalf("repaired state port = %d, want %d", state.Port, port)
	}
	if state.PID != os.Getpid() {
		t.Fatalf("repaired state pid = %d, want %d", state.PID, os.Getpid())
	}
}

func TestDoltStateAllocatePortCmdRepairsStoppedProviderStateFromOwnedLivePortHolder(t *testing.T) {
	cityPath := t.TempDir()
	stateFile := filepath.Join(t.TempDir(), "dolt-provider-state.json")

	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", "0"))
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close() //nolint:errcheck
	port := listener.Addr().(*net.TCPAddr).Port

	if err := writeDoltRuntimeStateFile(stateFile, doltRuntimeState{
		Running:   false,
		PID:       0,
		Port:      port,
		DataDir:   filepath.Join(cityPath, ".beads", "dolt"),
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("writeDoltRuntimeStateFile: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"dolt-state", "allocate-port", "--city", cityPath, "--state-file", stateFile}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run() = %d, stderr = %s", code, stderr.String())
	}
	if got := strings.TrimSpace(stdout.String()); got != strconv.Itoa(port) {
		t.Fatalf("allocate-port = %q, want %d", got, port)
	}

	state, err := readDoltRuntimeStateFile(stateFile)
	if err != nil {
		t.Fatalf("readDoltRuntimeStateFile: %v", err)
	}
	if !state.Running {
		t.Fatalf("repaired state running = false, want true")
	}
	if state.Port != port {
		t.Fatalf("repaired state port = %d, want %d", state.Port, port)
	}
	if state.PID != os.Getpid() {
		t.Fatalf("repaired state pid = %d, want %d", state.PID, os.Getpid())
	}
}

func TestDoltStateAllocatePortCmdSkipsOccupiedSeedPort(t *testing.T) {
	cityPath := t.TempDir()

	var firstOut, firstErr bytes.Buffer
	code := run([]string{"dolt-state", "allocate-port", "--city", cityPath}, &firstOut, &firstErr)
	if code != 0 {
		t.Fatalf("initial run() = %d, stderr = %s", code, firstErr.String())
	}
	firstPort, err := strconv.Atoi(strings.TrimSpace(firstOut.String()))
	if err != nil {
		t.Fatalf("parse initial port: %v", err)
	}
	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(firstPort)))
	if err != nil {
		t.Fatalf("listen on seed port %d: %v", firstPort, err)
	}
	defer listener.Close() //nolint:errcheck

	var secondOut, secondErr bytes.Buffer
	code = run([]string{"dolt-state", "allocate-port", "--city", cityPath}, &secondOut, &secondErr)
	if code != 0 {
		t.Fatalf("second run() = %d, stderr = %s", code, secondErr.String())
	}
	secondPort, err := strconv.Atoi(strings.TrimSpace(secondOut.String()))
	if err != nil {
		t.Fatalf("parse second port: %v", err)
	}
	if secondPort == firstPort {
		t.Fatalf("allocate-port reused occupied seed port %d", firstPort)
	}
}

func TestDoltStateAllocatePortCmdIgnoresInvalidProviderState(t *testing.T) {
	cityPath := t.TempDir()
	cases := []struct {
		name  string
		state doltRuntimeState
	}{
		{
			name: "running false",
			state: doltRuntimeState{
				Running:   false,
				PID:       os.Getpid(),
				Port:      43124,
				DataDir:   filepath.Join(cityPath, ".beads", "dolt"),
				StartedAt: time.Now().UTC().Format(time.RFC3339),
			},
		},
		{
			name: "wrong data dir",
			state: doltRuntimeState{
				Running:   true,
				PID:       os.Getpid(),
				Port:      43125,
				DataDir:   filepath.Join(t.TempDir(), ".beads", "dolt"),
				StartedAt: time.Now().UTC().Format(time.RFC3339),
			},
		},
		{
			name: "unreachable port",
			state: doltRuntimeState{
				Running:   true,
				PID:       os.Getpid(),
				Port:      43126,
				DataDir:   filepath.Join(cityPath, ".beads", "dolt"),
				StartedAt: time.Now().UTC().Format(time.RFC3339),
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stateFile := filepath.Join(t.TempDir(), "dolt-provider-state.json")
			if err := writeDoltRuntimeStateFile(stateFile, tc.state); err != nil {
				t.Fatalf("writeDoltRuntimeStateFile: %v", err)
			}
			var stdout, stderr bytes.Buffer
			code := run([]string{"dolt-state", "allocate-port", "--city", cityPath, "--state-file", stateFile}, &stdout, &stderr)
			if code != 0 {
				t.Fatalf("run() = %d, stderr = %s", code, stderr.String())
			}
			if got := strings.TrimSpace(stdout.String()); got == strconv.Itoa(tc.state.Port) {
				t.Fatalf("allocate-port reused invalid provider-state port %d", tc.state.Port)
			}
		})
	}
}

func TestDoltStateInspectManagedCmdUsesPIDFileAndStateOwnership(t *testing.T) {
	cityPath := t.TempDir()
	layout, err := resolveManagedDoltRuntimeLayout(cityPath)
	if err != nil {
		t.Fatalf("resolveManagedDoltRuntimeLayout: %v", err)
	}
	proc := exec.Command("sleep", "30")
	if err := proc.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	defer func() {
		_ = proc.Process.Kill()
		_, _ = proc.Process.Wait()
	}()
	if err := os.MkdirAll(filepath.Dir(layout.PIDFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(layout.PIDFile, []byte(strconv.Itoa(proc.Process.Pid)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeDoltRuntimeStateFile(layout.StateFile, doltRuntimeState{
		Running:   true,
		PID:       proc.Process.Pid,
		Port:      43127,
		DataDir:   layout.DataDir,
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("writeDoltRuntimeStateFile: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"dolt-state", "inspect-managed", "--city", cityPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run() = %d, stderr = %s", code, stderr.String())
	}
	got := parseDoltStateOutput(t, stdout.String())
	if got["managed_pid"] != strconv.Itoa(proc.Process.Pid) {
		t.Fatalf("managed_pid = %q, want %d", got["managed_pid"], proc.Process.Pid)
	}
	if got["managed_source"] != "pid-file" {
		t.Fatalf("managed_source = %q, want pid-file", got["managed_source"])
	}
	if got["managed_owned"] != "true" {
		t.Fatalf("managed_owned = %q, want true", got["managed_owned"])
	}
	if got["managed_deleted_inodes"] != "false" {
		t.Fatalf("managed_deleted_inodes = %q, want false", got["managed_deleted_inodes"])
	}
}

func TestDoltStateInspectManagedCmdDetectsDeletedInodes(t *testing.T) {
	cityPath := t.TempDir()
	layout, err := resolveManagedDoltRuntimeLayout(cityPath)
	if err != nil {
		t.Fatalf("resolveManagedDoltRuntimeLayout: %v", err)
	}
	proc := exec.Command("python3", "-c", `
import os, signal, sys, time
path = sys.argv[1]
os.makedirs(path, exist_ok=True)
stale = os.path.join(path, "stale-open.txt")
f = open(stale, "w+")
f.write("stale")
f.flush()
os.unlink(stale)
signal.signal(signal.SIGTERM, lambda *_: sys.exit(0))
signal.signal(signal.SIGINT, lambda *_: sys.exit(0))
while True:
    time.sleep(1)
`, layout.DataDir)
	if err := proc.Start(); err != nil {
		t.Fatalf("start python: %v", err)
	}
	defer func() {
		_ = proc.Process.Kill()
		_, _ = proc.Process.Wait()
	}()
	if err := os.MkdirAll(filepath.Dir(layout.PIDFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(layout.PIDFile, []byte(strconv.Itoa(proc.Process.Pid)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeDoltRuntimeStateFile(layout.StateFile, doltRuntimeState{
		Running:   true,
		PID:       proc.Process.Pid,
		Port:      43128,
		DataDir:   layout.DataDir,
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("writeDoltRuntimeStateFile: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"dolt-state", "inspect-managed", "--city", cityPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run() = %d, stderr = %s", code, stderr.String())
	}
	got := parseDoltStateOutput(t, stdout.String())
	if got["managed_deleted_inodes"] != "true" {
		t.Fatalf("managed_deleted_inodes = %q, want true", got["managed_deleted_inodes"])
	}
}

func TestDoltStateInspectManagedCmdReportsPortHolderOwnership(t *testing.T) {
	if _, err := exec.LookPath("lsof"); err != nil {
		t.Skip("lsof not installed")
	}
	cityPath := t.TempDir()
	layout, err := resolveManagedDoltRuntimeLayout(cityPath)
	if err != nil {
		t.Fatalf("resolveManagedDoltRuntimeLayout: %v", err)
	}
	port := reserveRandomTCPPort(t)
	listener := startTCPListenerProcess(t, port)
	defer func() {
		_ = listener.Process.Kill()
		_ = listener.Wait()
	}()
	if err := writeDoltRuntimeStateFile(layout.StateFile, doltRuntimeState{
		Running:   true,
		PID:       listener.Process.Pid,
		Port:      port,
		DataDir:   layout.DataDir,
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("writeDoltRuntimeStateFile: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"dolt-state", "inspect-managed", "--city", cityPath, "--port", strconv.Itoa(port)}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run() = %d, stderr = %s", code, stderr.String())
	}
	got := parseDoltStateOutput(t, stdout.String())
	if got["port_holder_pid"] != strconv.Itoa(listener.Process.Pid) {
		t.Fatalf("port_holder_pid = %q, want %d", got["port_holder_pid"], listener.Process.Pid)
	}
	if got["port_holder_owned"] != "true" {
		t.Fatalf("port_holder_owned = %q, want true", got["port_holder_owned"])
	}
}

func TestDoltStateProbeManagedCmdReportsRunningOwnedHolder(t *testing.T) {
	if _, err := exec.LookPath("lsof"); err != nil {
		t.Skip("lsof not installed")
	}
	cityPath := t.TempDir()
	layout, err := resolveManagedDoltRuntimeLayout(cityPath)
	if err != nil {
		t.Fatalf("resolveManagedDoltRuntimeLayout: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(layout.StateFile), 0o755); err != nil {
		t.Fatalf("MkdirAll(runtime dir): %v", err)
	}
	port := reserveRandomTCPPort(t)
	listener := startTCPListenerProcess(t, port)
	defer func() {
		_ = listener.Process.Kill()
		_ = listener.Wait()
	}()
	if err := writeDoltRuntimeStateFile(layout.StateFile, doltRuntimeState{
		Running:   true,
		PID:       listener.Process.Pid,
		Port:      port,
		DataDir:   layout.DataDir,
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("writeDoltRuntimeStateFile: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"dolt-state", "probe-managed", "--city", cityPath, "--host", "0.0.0.0", "--port", strconv.Itoa(port)}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run() = %d, stderr = %s", code, stderr.String())
	}
	got := parseDoltStateOutput(t, stdout.String())
	if got["running"] != "true" {
		t.Fatalf("running = %q, want true", got["running"])
	}
	if got["port_holder_owned"] != "true" {
		t.Fatalf("port_holder_owned = %q, want true", got["port_holder_owned"])
	}
	if got["port_holder_deleted_inodes"] != "false" {
		t.Fatalf("port_holder_deleted_inodes = %q, want false", got["port_holder_deleted_inodes"])
	}
	if got["tcp_reachable"] != "true" {
		t.Fatalf("tcp_reachable = %q, want true", got["tcp_reachable"])
	}
	if got["port_holder_pid"] != strconv.Itoa(listener.Process.Pid) {
		t.Fatalf("port_holder_pid = %q, want %d", got["port_holder_pid"], listener.Process.Pid)
	}
}

func TestDoltStateProbeManagedCmdReportsImposterHolder(t *testing.T) {
	if _, err := exec.LookPath("lsof"); err != nil {
		t.Skip("lsof not installed")
	}
	cityPath := t.TempDir()
	port := reserveRandomTCPPort(t)
	listener := startTCPListenerProcess(t, port)
	defer func() {
		_ = listener.Process.Kill()
		_ = listener.Wait()
	}()

	var stdout, stderr bytes.Buffer
	code := run([]string{"dolt-state", "probe-managed", "--city", cityPath, "--host", "127.0.0.1", "--port", strconv.Itoa(port)}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run() = %d, stderr = %s", code, stderr.String())
	}
	got := parseDoltStateOutput(t, stdout.String())
	if got["running"] != "false" {
		t.Fatalf("running = %q, want false", got["running"])
	}
	if got["port_holder_owned"] != "false" {
		t.Fatalf("port_holder_owned = %q, want false", got["port_holder_owned"])
	}
	if got["port_holder_deleted_inodes"] != "false" {
		t.Fatalf("port_holder_deleted_inodes = %q, want false", got["port_holder_deleted_inodes"])
	}
	if got["tcp_reachable"] != "true" {
		t.Fatalf("tcp_reachable = %q, want true", got["tcp_reachable"])
	}
	if got["port_holder_pid"] != strconv.Itoa(listener.Process.Pid) {
		t.Fatalf("port_holder_pid = %q, want %d", got["port_holder_pid"], listener.Process.Pid)
	}
}

func TestDoltStateProbeManagedCmdReportsDeletedOwnedHolder(t *testing.T) {
	if _, err := exec.LookPath("lsof"); err != nil {
		t.Skip("lsof not installed")
	}
	cityPath := t.TempDir()
	layout, err := resolveManagedDoltRuntimeLayout(cityPath)
	if err != nil {
		t.Fatalf("resolveManagedDoltRuntimeLayout: %v", err)
	}
	if err := os.MkdirAll(layout.DataDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(data dir): %v", err)
	}
	port := reserveRandomTCPPort(t)
	proc := exec.Command("python3", "-c", `
import os
import signal
import socket
import sys
import time

port = int(sys.argv[1])
deleted_path = sys.argv[2]
os.makedirs(os.path.dirname(deleted_path), exist_ok=True)
f = open(deleted_path, "a+")
f.write("held")
f.flush()
os.unlink(deleted_path)
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
`, strconv.Itoa(port), filepath.Join(layout.DataDir, "held.db"))
	if err := proc.Start(); err != nil {
		t.Fatalf("start deleted-inode listener: %v", err)
	}
	defer func() {
		_ = proc.Process.Kill()
		_, _ = proc.Process.Wait()
	}()
	deadline := time.Now().Add(5 * time.Second)
	for !processHasDeletedDataInodes(proc.Process.Pid, layout.DataDir) {

		if time.Now().After(deadline) {
			t.Fatalf("process %d did not hold deleted data inodes under %s", proc.Process.Pid, layout.DataDir)
		}
		time.Sleep(25 * time.Millisecond)
	}
	if err := writeDoltRuntimeStateFile(layout.StateFile, doltRuntimeState{
		Running:   true,
		PID:       proc.Process.Pid,
		Port:      port,
		DataDir:   layout.DataDir,
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("writeDoltRuntimeStateFile: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"dolt-state", "probe-managed", "--city", cityPath, "--host", "127.0.0.1", "--port", strconv.Itoa(port)}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run() = %d, stderr = %s", code, stderr.String())
	}
	got := parseDoltStateOutput(t, stdout.String())
	if got["running"] != "false" {
		t.Fatalf("running = %q, want false", got["running"])
	}
	if got["port_holder_owned"] != "true" {
		t.Fatalf("port_holder_owned = %q, want true", got["port_holder_owned"])
	}
	if got["port_holder_deleted_inodes"] != "true" {
		t.Fatalf("port_holder_deleted_inodes = %q, want true", got["port_holder_deleted_inodes"])
	}
	if got["tcp_reachable"] != "true" {
		t.Fatalf("tcp_reachable = %q, want true", got["tcp_reachable"])
	}
	if got["port_holder_pid"] != strconv.Itoa(proc.Process.Pid) {
		t.Fatalf("port_holder_pid = %q, want %d", got["port_holder_pid"], proc.Process.Pid)
	}
}

func TestDoltStateExistingManagedCmdReportsReusableOwnedServer(t *testing.T) {
	if _, err := exec.LookPath("lsof"); err != nil {
		t.Skip("lsof not installed")
	}
	cityPath := t.TempDir()
	layout, err := resolveManagedDoltRuntimeLayout(cityPath)
	if err != nil {
		t.Fatalf("resolveManagedDoltRuntimeLayout: %v", err)
	}
	binDir := t.TempDir()
	invocationFile := filepath.Join(t.TempDir(), "dolt-invocation.txt")
	writeFakeDoltSQLBinary(t, binDir, invocationFile, `#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$INVOCATION_FILE"
case "$*" in
  *"sql -q SELECT active_branch()"*)
    exit 0
    ;;
  *)
    echo "unexpected command: $*" >&2
    exit 2
    ;;
esac
`)
	t.Setenv("INVOCATION_FILE", invocationFile)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	port := reserveRandomTCPPort(t)
	listener := startTCPListenerProcess(t, port)
	defer func() {
		_ = listener.Process.Kill()
		_ = listener.Wait()
	}()
	if err := writeDoltRuntimeStateFile(layout.StateFile, doltRuntimeState{
		Running:   true,
		PID:       listener.Process.Pid,
		Port:      port,
		DataDir:   layout.DataDir,
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("writeDoltRuntimeStateFile: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"dolt-state", "existing-managed", "--city", cityPath, "--host", "0.0.0.0", "--port", strconv.Itoa(port), "--user", "root", "--timeout-ms", "1000"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run() = %d, stderr = %s", code, stderr.String())
	}
	got := parseDoltStateOutput(t, stdout.String())
	if got["managed_pid"] != strconv.Itoa(listener.Process.Pid) {
		t.Fatalf("managed_pid = %q, want %d", got["managed_pid"], listener.Process.Pid)
	}
	if got["managed_owned"] != "true" {
		t.Fatalf("managed_owned = %q, want true", got["managed_owned"])
	}
	if got["state_port"] != strconv.Itoa(port) {
		t.Fatalf("state_port = %q, want %d", got["state_port"], port)
	}
	if got["ready"] != "true" {
		t.Fatalf("ready = %q, want true", got["ready"])
	}
	if got["reusable"] != "true" {
		t.Fatalf("reusable = %q, want true", got["reusable"])
	}
	if got["deleted_inodes"] != "false" {
		t.Fatalf("deleted_inodes = %q, want false", got["deleted_inodes"])
	}
}

func TestDoltStateExistingManagedCmdReportsDeletedInodes(t *testing.T) {
	if _, err := exec.LookPath("lsof"); err != nil {
		t.Skip("lsof not installed")
	}
	cityPath := t.TempDir()
	layout, err := resolveManagedDoltRuntimeLayout(cityPath)
	if err != nil {
		t.Fatalf("resolveManagedDoltRuntimeLayout: %v", err)
	}
	if err := os.MkdirAll(layout.DataDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(data dir): %v", err)
	}
	binDir := t.TempDir()
	invocationFile := filepath.Join(t.TempDir(), "dolt-invocation.txt")
	writeFakeDoltSQLBinary(t, binDir, invocationFile, `#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$INVOCATION_FILE"
case "$*" in
  *"sql -q SELECT active_branch()"*)
    exit 0
    ;;
  *)
    echo "unexpected command: $*" >&2
    exit 2
    ;;
esac
`)
	t.Setenv("INVOCATION_FILE", invocationFile)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	port := reserveRandomTCPPort(t)
	proc := exec.Command("python3", "-c", `
import os
import signal
import socket
import sys
import time

port = int(sys.argv[1])
deleted_path = sys.argv[2]
os.makedirs(os.path.dirname(deleted_path), exist_ok=True)
f = open(deleted_path, "a+")
f.write("held")
f.flush()
os.unlink(deleted_path)
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
`, strconv.Itoa(port), filepath.Join(layout.DataDir, "held.db"))
	if err := proc.Start(); err != nil {
		t.Fatalf("start deleted-inode listener: %v", err)
	}
	defer func() {
		_ = proc.Process.Kill()
		_, _ = proc.Process.Wait()
	}()
	deadline := time.Now().Add(5 * time.Second)
	for !processHasDeletedDataInodes(proc.Process.Pid, layout.DataDir) {

		if time.Now().After(deadline) {
			t.Fatalf("process %d did not hold deleted data inodes under %s", proc.Process.Pid, layout.DataDir)
		}
		time.Sleep(25 * time.Millisecond)
	}
	if err := writeDoltRuntimeStateFile(layout.StateFile, doltRuntimeState{
		Running:   true,
		PID:       proc.Process.Pid,
		Port:      port,
		DataDir:   layout.DataDir,
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("writeDoltRuntimeStateFile: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"dolt-state", "existing-managed", "--city", cityPath, "--host", "127.0.0.1", "--port", strconv.Itoa(port), "--user", "root", "--timeout-ms", "1000"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run() = %d, stderr = %s", code, stderr.String())
	}
	got := parseDoltStateOutput(t, stdout.String())
	if got["deleted_inodes"] != "true" {
		t.Fatalf("deleted_inodes = %q, want true", got["deleted_inodes"])
	}
	if got["reusable"] != "false" {
		t.Fatalf("reusable = %q, want false", got["reusable"])
	}
}

func TestDoltStatePreflightCleanCmdRemovesStaleArtifacts(t *testing.T) {
	if _, err := exec.LookPath("lsof"); err != nil {
		t.Skip("lsof not installed")
	}
	cityPath := t.TempDir()
	layout, err := resolveManagedDoltRuntimeLayout(cityPath)
	if err != nil {
		t.Fatalf("resolveManagedDoltRuntimeLayout: %v", err)
	}

	phantomDir := filepath.Join(layout.DataDir, "phantom", ".dolt", "noms")
	if err := os.MkdirAll(phantomDir, 0o755); err != nil {
		t.Fatal(err)
	}
	staleLock := filepath.Join(layout.DataDir, "stale", ".dolt", "noms", "LOCK")
	if err := os.MkdirAll(filepath.Dir(staleLock), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staleLock, []byte("stale\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	healthyManifest := filepath.Join(layout.DataDir, "healthy", ".dolt", "noms", "manifest")
	if err := os.MkdirAll(filepath.Dir(healthyManifest), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(healthyManifest, []byte("ok\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	socketPath := filepath.Join("/tmp", "dolt-gc-preflight-"+strconv.FormatInt(time.Now().UnixNano(), 10)+".sock")
	staleSocket := startUnixSocketProcess(t, socketPath)
	if err := staleSocket.Process.Kill(); err != nil {
		t.Fatalf("kill stale socket holder: %v", err)
	}
	_ = staleSocket.Wait()
	t.Cleanup(func() { _ = os.Remove(socketPath) })
	if _, err := os.Stat(socketPath); err != nil {
		t.Fatalf("stale socket precondition missing: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"dolt-state", "preflight-clean", "--city", cityPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run() = %d, stderr = %s", code, stderr.String())
	}
	if _, err := os.Stat(socketPath); !os.IsNotExist(err) {
		t.Fatalf("socket %s still present after preflight clean, stat err = %v", socketPath, err)
	}
	if _, err := os.Stat(staleLock); !os.IsNotExist(err) {
		t.Fatalf("LOCK %s still present after preflight clean, stat err = %v", staleLock, err)
	}
	quarantined, err := filepath.Glob(filepath.Join(layout.DataDir, ".quarantine", "*-phantom*"))
	if err != nil {
		t.Fatalf("Glob(quarantine): %v", err)
	}
	if len(quarantined) != 1 {
		t.Fatalf("quarantined phantom databases = %d, want 1 (%v)", len(quarantined), quarantined)
	}
	if _, err := os.Stat(filepath.Join(layout.DataDir, "healthy", ".dolt", "noms", "manifest")); err != nil {
		t.Fatalf("healthy manifest removed unexpectedly: %v", err)
	}
}

func TestDoltStatePreflightCleanCmdPreservesLiveArtifacts(t *testing.T) {
	if _, err := exec.LookPath("lsof"); err != nil {
		t.Skip("lsof not installed")
	}
	cityPath := t.TempDir()
	layout, err := resolveManagedDoltRuntimeLayout(cityPath)
	if err != nil {
		t.Fatalf("resolveManagedDoltRuntimeLayout: %v", err)
	}

	liveManifest := filepath.Join(layout.DataDir, "live", ".dolt", "noms", "manifest")
	if err := os.MkdirAll(filepath.Dir(liveManifest), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(liveManifest, []byte("ok\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	liveLock := filepath.Join(layout.DataDir, "live", ".dolt", "noms", "LOCK")
	liveLockHolder := startOpenFileProcess(t, liveLock)
	defer func() {
		_ = liveLockHolder.Process.Kill()
		_ = liveLockHolder.Wait()
	}()

	socketPath := filepath.Join("/tmp", "dolt-gc-preflight-live-"+strconv.FormatInt(time.Now().UnixNano(), 10)+".sock")
	liveSocket := startUnixSocketProcess(t, socketPath)
	defer func() {
		_ = liveSocket.Process.Kill()
		_ = liveSocket.Wait()
	}()
	t.Cleanup(func() { _ = os.Remove(socketPath) })

	var stdout, stderr bytes.Buffer
	code := run([]string{"dolt-state", "preflight-clean", "--city", cityPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run() = %d, stderr = %s", code, stderr.String())
	}
	if _, err := os.Stat(liveLock); err != nil {
		t.Fatalf("live LOCK removed unexpectedly: %v", err)
	}
	if _, err := os.Stat(socketPath); err != nil {
		t.Fatalf("live socket removed unexpectedly: %v", err)
	}
}

func startUnixSocketProcess(t *testing.T, socketPath string) *exec.Cmd {
	t.Helper()
	proc := exec.Command("python3", "-c", `
import os
import socket
import sys
import time
path = sys.argv[1]
if os.path.exists(path):
    os.remove(path)
sock = socket.socket(socket.AF_UNIX)
sock.bind(path)
sock.listen(1)
while True:
    time.sleep(1)
`, socketPath)
	if err := proc.Start(); err != nil {
		t.Fatalf("start unix socket process: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(socketPath); err == nil {
			if open, openErr := fileOpenedByAnyProcess(socketPath); openErr == nil && open {
				return proc
			}
		}
		if time.Now().After(deadline) {
			_ = proc.Process.Kill()
			_ = proc.Wait()
			t.Fatalf("unix socket %s did not become visible to lsof", socketPath)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func startOpenFileProcess(t *testing.T, path string) *exec.Cmd {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	proc := exec.Command("python3", "-c", `
import os
import sys
import time
path = sys.argv[1]
f = open(path, "a+")
f.write("held")
f.flush()
while True:
    time.sleep(1)
`, path)
	if err := proc.Start(); err != nil {
		t.Fatalf("start open-file process: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			if open, openErr := fileOpenedByAnyProcess(path); openErr == nil && open {
				return proc
			}
		}
		if time.Now().After(deadline) {
			_ = proc.Process.Kill()
			_ = proc.Wait()
			t.Fatalf("open file %s did not become visible to lsof", path)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func processHoldsDeletedPath(pid int, targetPath string) bool {
	fdDir := filepath.Join("/proc", strconv.Itoa(pid), "fd")
	entries, err := os.ReadDir(fdDir)
	if err != nil {
		return false
	}
	for _, entry := range entries {
		target, readErr := os.Readlink(filepath.Join(fdDir, entry.Name()))
		if readErr != nil || !strings.Contains(target, " (deleted)") {
			continue
		}
		if samePath(strings.TrimSuffix(target, " (deleted)"), targetPath) {
			return true
		}
	}
	return false
}

func TestProcessHasDeletedDataInodesIgnoresDeletedNomsLock(t *testing.T) {
	cityPath := t.TempDir()
	layout, err := resolveManagedDoltRuntimeLayout(cityPath)
	if err != nil {
		t.Fatalf("resolveManagedDoltRuntimeLayout: %v", err)
	}
	lockPath := filepath.Join(layout.DataDir, "hq", ".dolt", "noms", "LOCK")
	proc := startOpenFileProcess(t, lockPath)
	defer func() {
		_ = proc.Process.Kill()
		_ = proc.Wait()
	}()
	if err := os.Remove(lockPath); err != nil {
		t.Fatalf("Remove(lock): %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !processHoldsDeletedPath(proc.Process.Pid, lockPath) {
		if time.Now().After(deadline) {
			t.Fatalf("process %d did not hold deleted %s", proc.Process.Pid, lockPath)
		}
		time.Sleep(25 * time.Millisecond)
	}
	if processHasDeletedDataInodes(proc.Process.Pid, layout.DataDir) {
		t.Fatalf("processHasDeletedDataInodes(%d, %q) = true, want false for deleted noms LOCK", proc.Process.Pid, layout.DataDir)
	}
}

func TestDoltStateQueryProbeCmdUsesDoltHelper(t *testing.T) {
	binDir := t.TempDir()
	invocationFile := filepath.Join(t.TempDir(), "dolt-invocation.txt")
	writeFakeDoltSQLBinary(t, binDir, invocationFile, `#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$INVOCATION_FILE"
case "$*" in
  *"sql -q SELECT active_branch()"*)
    exit 0
    ;;
  *)
    echo "unexpected command: $*" >&2
    exit 2
    ;;
esac
`)
	t.Setenv("INVOCATION_FILE", invocationFile)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	var stdout, stderr bytes.Buffer
	code := run([]string{"dolt-state", "query-probe", "--host", "0.0.0.0", "--port", "3311", "--user", "root"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run() = %d, stderr = %s", code, stderr.String())
	}
	invocation, err := os.ReadFile(invocationFile)
	if err != nil {
		t.Fatalf("ReadFile(invocation): %v", err)
	}
	text := string(invocation)
	for _, want := range []string{"--host 127.0.0.1", "--port 3311", "--user root", "sql -q SELECT active_branch()"} {
		if !strings.Contains(text, want) {
			t.Fatalf("dolt invocation missing %q: %s", want, text)
		}
	}
}

func TestDoltStateReadOnlyCheckCmdDetectsReadOnly(t *testing.T) {
	binDir := t.TempDir()
	invocationFile := filepath.Join(t.TempDir(), "dolt-invocation.txt")
	writeFakeDoltSQLBinary(t, binDir, invocationFile, `#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$INVOCATION_FILE"
echo 'database is read only' >&2
exit 1
`)
	t.Setenv("INVOCATION_FILE", invocationFile)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	var stdout, stderr bytes.Buffer
	code := run([]string{"dolt-state", "read-only-check", "--host", "127.0.0.1", "--port", "3311", "--user", "root"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run() = %d, stderr = %s", code, stderr.String())
	}
}

func TestDoltStateReadOnlyCheckCmdReturnsErrExitWhenWritable(t *testing.T) {
	binDir := t.TempDir()
	invocationFile := filepath.Join(t.TempDir(), "dolt-invocation.txt")
	writeFakeDoltSQLBinary(t, binDir, invocationFile, `#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$INVOCATION_FILE"
exit 0
`)
	t.Setenv("INVOCATION_FILE", invocationFile)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	var stdout, stderr bytes.Buffer
	code := run([]string{"dolt-state", "read-only-check", "--host", "127.0.0.1", "--port", "3311", "--user", "root"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("run() = %d, want 1; stderr = %s", code, stderr.String())
	}
}

func TestDoltStateHealthCheckCmdReportsReadOnlyAndConnectionCount(t *testing.T) {
	binDir := t.TempDir()
	invocationFile := filepath.Join(t.TempDir(), "dolt-invocation.txt")
	writeFakeDoltSQLBinary(t, binDir, invocationFile, `#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$INVOCATION_FILE"
case "$*" in
  *"sql -q SELECT active_branch()"*)
    exit 0
    ;;
  *"sql -q CREATE DATABASE IF NOT EXISTS __gc_probe; USE __gc_probe; CREATE TABLE IF NOT EXISTS __probe (k INT PRIMARY KEY); REPLACE INTO __probe VALUES (1); DROP TABLE __probe; DROP DATABASE __gc_probe;"*)
    echo 'database is read only' >&2
    exit 1
    ;;
  *"sql -r csv -q SELECT COUNT(*) AS cnt FROM information_schema.PROCESSLIST"*)
    printf 'cnt\n812\n'
    exit 0
    ;;
  *)
    echo "unexpected command: $*" >&2
    exit 2
    ;;
esac
`)
	t.Setenv("INVOCATION_FILE", invocationFile)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	var stdout, stderr bytes.Buffer
	code := run([]string{"dolt-state", "health-check", "--host", "0.0.0.0", "--port", "3311", "--user", "root", "--check-read-only"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run() = %d, stderr = %s", code, stderr.String())
	}
	got := parseDoltStateOutput(t, stdout.String())
	if got["query_ready"] != "true" {
		t.Fatalf("query_ready = %q, want true", got["query_ready"])
	}
	if got["read_only"] != "true" {
		t.Fatalf("read_only = %q, want true", got["read_only"])
	}
	if got["connection_count"] != "812" {
		t.Fatalf("connection_count = %q, want 812", got["connection_count"])
	}
	invocation, err := os.ReadFile(invocationFile)
	if err != nil {
		t.Fatalf("ReadFile(invocation): %v", err)
	}
	text := string(invocation)
	for _, want := range []string{"--host 127.0.0.1", "--port 3311", "--user root", "SELECT active_branch()", "information_schema.PROCESSLIST"} {
		if strings.Contains(text, want) == false {
			t.Fatalf("dolt invocation missing %q: %s", want, text)
		}
	}
}

func TestDoltStateHealthCheckCmdSkipsReadOnlyAndBestEffortCount(t *testing.T) {
	binDir := t.TempDir()
	invocationFile := filepath.Join(t.TempDir(), "dolt-invocation.txt")
	writeFakeDoltSQLBinary(t, binDir, invocationFile, `#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$INVOCATION_FILE"
case "$*" in
  *"sql -q SELECT active_branch()"*)
    exit 0
    ;;
  *"sql -r csv -q SELECT COUNT(*) AS cnt FROM information_schema.PROCESSLIST"*)
    exit 1
    ;;
  *)
    echo "unexpected command: $*" >&2
    exit 2
    ;;
esac
`)
	t.Setenv("INVOCATION_FILE", invocationFile)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	var stdout, stderr bytes.Buffer
	code := run([]string{"dolt-state", "health-check", "--host", "127.0.0.1", "--port", "3311", "--user", "root"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run() = %d, stderr = %s", code, stderr.String())
	}
	got := parseDoltStateOutput(t, stdout.String())
	if got["read_only"] != "false" {
		t.Fatalf("read_only = %q, want false", got["read_only"])
	}
	if got["connection_count"] != "" {
		t.Fatalf("connection_count = %q, want blank", got["connection_count"])
	}
	invocation, err := os.ReadFile(invocationFile)
	if err != nil {
		t.Fatalf("ReadFile(invocation): %v", err)
	}
	text := string(invocation)
	if strings.Contains(text, "CREATE DATABASE IF NOT EXISTS __gc_probe") {
		t.Fatalf("health-check unexpectedly ran read-only probe: %s", text)
	}
	for _, want := range []string{"SELECT active_branch()", "information_schema.PROCESSLIST"} {
		if strings.Contains(text, want) == false {
			t.Fatalf("dolt invocation missing %q: %s", want, text)
		}
	}
}

func TestDoltStateHealthCheckCmdReturnsErrExitWhenProbeFails(t *testing.T) {
	binDir := t.TempDir()
	invocationFile := filepath.Join(t.TempDir(), "dolt-invocation.txt")
	writeFakeDoltSQLBinary(t, binDir, invocationFile, `#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$INVOCATION_FILE"
echo 'query failed' >&2
exit 1
`)
	t.Setenv("INVOCATION_FILE", invocationFile)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	var stdout, stderr bytes.Buffer
	code := run([]string{"dolt-state", "health-check", "--host", "127.0.0.1", "--port", "3311", "--user", "root"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("run() = %d, want 1; stderr = %s", code, stderr.String())
	}
}

func TestDoltStateWaitReadyCmdReturnsReady(t *testing.T) {
	cityPath := t.TempDir()
	binDir := t.TempDir()
	invocationFile := filepath.Join(t.TempDir(), "dolt-invocation.txt")
	writeFakeDoltSQLBinary(t, binDir, invocationFile, `#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$INVOCATION_FILE"
case "$*" in
  *"sql -q SELECT active_branch()"*)
    exit 0
    ;;
  *)
    echo "unexpected command: $*" >&2
    exit 2
    ;;
esac
`)
	t.Setenv("INVOCATION_FILE", invocationFile)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	port := reserveRandomTCPPort(t)
	listener := startTCPListenerProcess(t, port)
	t.Cleanup(func() {
		_ = listener.Process.Kill()
		_, _ = listener.Process.Wait()
	})

	var stdout, stderr bytes.Buffer
	code := run([]string{
		"dolt-state", "wait-ready",
		"--city", cityPath,
		"--host", "0.0.0.0",
		"--port", strconv.Itoa(port),
		"--user", "root",
		"--pid", strconv.Itoa(listener.Process.Pid),
		"--timeout-ms", "1000",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run() = %d, stderr = %s", code, stderr.String())
	}
	got := parseDoltStateOutput(t, stdout.String())
	if got["ready"] != "true" {
		t.Fatalf("ready = %q, want true", got["ready"])
	}
	if got["pid_alive"] != "true" {
		t.Fatalf("pid_alive = %q, want true", got["pid_alive"])
	}
	if got["deleted_inodes"] != "false" {
		t.Fatalf("deleted_inodes = %q, want false", got["deleted_inodes"])
	}
}

func TestDoltStateWaitReadyCmdDetectsDeletedInodes(t *testing.T) {
	cityPath := t.TempDir()
	layout, err := resolveManagedDoltRuntimeLayout(cityPath)
	if err != nil {
		t.Fatalf("resolveManagedDoltRuntimeLayout: %v", err)
	}
	if err := os.MkdirAll(layout.DataDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(data dir): %v", err)
	}

	binDir := t.TempDir()
	invocationFile := filepath.Join(t.TempDir(), "dolt-invocation.txt")
	writeFakeDoltSQLBinary(t, binDir, invocationFile, `#!/bin/sh
set -eu
printf '%s
' "$*" >> "$INVOCATION_FILE"
case "$*" in
  *"sql -q SELECT active_branch()"*)
    exit 0
    ;;
  *)
    echo "unexpected command: $*" >&2
    exit 2
    ;;
esac
`)
	t.Setenv("INVOCATION_FILE", invocationFile)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	port := reserveRandomTCPPort(t)
	proc := exec.Command("python3", "-c", `
import os
import signal
import socket
import sys
import time

port = int(sys.argv[1])
deleted_path = sys.argv[2]
os.makedirs(os.path.dirname(deleted_path), exist_ok=True)
f = open(deleted_path, "a+")
f.write("held")
f.flush()
os.unlink(deleted_path)
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
`, strconv.Itoa(port), filepath.Join(layout.DataDir, "held.db"))
	if err := proc.Start(); err != nil {
		t.Fatalf("start deleted-inode listener: %v", err)
	}
	t.Cleanup(func() {
		_ = proc.Process.Kill()
		_, _ = proc.Process.Wait()
	})
	deadline := time.Now().Add(5 * time.Second)
	for !processHasDeletedDataInodes(proc.Process.Pid, layout.DataDir) {

		if time.Now().After(deadline) {
			t.Fatalf("process %d did not hold deleted data inodes under %s", proc.Process.Pid, layout.DataDir)
		}
		time.Sleep(25 * time.Millisecond)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{
		"dolt-state", "wait-ready",
		"--city", cityPath,
		"--host", "127.0.0.1",
		"--port", strconv.Itoa(port),
		"--user", "root",
		"--pid", strconv.Itoa(proc.Process.Pid),
		"--timeout-ms", "1000",
		"--check-deleted",
	}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("run() = %d, want 1; stdout = %s stderr = %s", code, stdout.String(), stderr.String())
	}
	got := parseDoltStateOutput(t, stdout.String())
	if got["ready"] != "false" {
		t.Fatalf("ready = %q, want false", got["ready"])
	}
	if got["pid_alive"] != "true" {
		t.Fatalf("pid_alive = %q, want true", got["pid_alive"])
	}
	if got["deleted_inodes"] != "true" {
		t.Fatalf("deleted_inodes = %q, want true", got["deleted_inodes"])
	}
}

func TestDoltStateStopManagedCmdStopsManagedPID(t *testing.T) {
	cityPath := t.TempDir()
	layout, err := resolveManagedDoltRuntimeLayout(cityPath)
	if err != nil {
		t.Fatalf("resolveManagedDoltRuntimeLayout: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(layout.PIDFile), 0o755); err != nil {
		t.Fatalf("MkdirAll(runtime dir): %v", err)
	}

	proc := exec.Command("sleep", "30")
	if err := proc.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	defer func() {
		_ = proc.Process.Kill()
		_, _ = proc.Process.Wait()
	}()
	if err := os.WriteFile(layout.PIDFile, []byte(strconv.Itoa(proc.Process.Pid)+"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(pid): %v", err)
	}
	state := doltRuntimeState{
		Running:   true,
		PID:       proc.Process.Pid,
		Port:      3311,
		DataDir:   layout.DataDir,
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if err := writeDoltRuntimeStateFile(layout.StateFile, state); err != nil {
		t.Fatalf("writeDoltRuntimeStateFile: %v", err)
	}
	if err := writeDoltRuntimeStateFile(managedDoltStatePath(cityPath), state); err != nil {
		t.Fatalf("write published dolt runtime state: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"dolt-state", "stop-managed", "--city", cityPath, "--port", "3311"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run() = %d, stderr = %s", code, stderr.String())
	}
	got := parseDoltStateOutput(t, stdout.String())
	if got["had_pid"] != "true" {
		t.Fatalf("had_pid = %q, want true", got["had_pid"])
	}
	if got["forced"] != "false" {
		t.Fatalf("forced = %q, want false", got["forced"])
	}
	deadline := time.Now().Add(5 * time.Second)
	for managedStopPIDAlive(proc.Process.Pid) {
		if time.Now().After(deadline) {
			t.Fatalf("pid %d still alive after stop-managed", proc.Process.Pid)
		}
		time.Sleep(25 * time.Millisecond)
	}
	if _, err := os.Stat(layout.PIDFile); !os.IsNotExist(err) {
		t.Fatalf("pid file still present, err = %v", err)
	}
	if _, err := os.Stat(managedDoltStatePath(cityPath)); !os.IsNotExist(err) {
		t.Fatalf("published dolt runtime state still present, err = %v", err)
	}
	state, err = readDoltRuntimeStateFile(layout.StateFile)
	if err != nil {
		t.Fatalf("readDoltRuntimeStateFile: %v", err)
	}
	if state.Running {
		t.Fatalf("state.Running = true, want false")
	}
	if state.PID != 0 {
		t.Fatalf("state.PID = %d, want 0", state.PID)
	}
	if state.Port != 3311 {
		t.Fatalf("state.Port = %d, want 3311", state.Port)
	}
}

func TestDoltStateStopManagedCmdCleansStaleStateWhenNoPID(t *testing.T) {
	cityPath := t.TempDir()
	layout, err := resolveManagedDoltRuntimeLayout(cityPath)
	if err != nil {
		t.Fatalf("resolveManagedDoltRuntimeLayout: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(layout.PIDFile), 0o755); err != nil {
		t.Fatalf("MkdirAll(runtime dir): %v", err)
	}
	if err := os.WriteFile(layout.PIDFile, []byte("999999\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(pid): %v", err)
	}
	if err := writeDoltRuntimeStateFile(layout.StateFile, doltRuntimeState{
		Running:   true,
		PID:       999999,
		Port:      3311,
		DataDir:   layout.DataDir,
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("writeDoltRuntimeStateFile: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"dolt-state", "stop-managed", "--city", cityPath, "--port", "3311"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run() = %d, stderr = %s", code, stderr.String())
	}
	got := parseDoltStateOutput(t, stdout.String())
	if got["had_pid"] != "false" {
		t.Fatalf("had_pid = %q, want false", got["had_pid"])
	}
	if _, err := os.Stat(layout.PIDFile); !os.IsNotExist(err) {
		t.Fatalf("pid file still present, err = %v", err)
	}
	state, err := readDoltRuntimeStateFile(layout.StateFile)
	if err != nil {
		t.Fatalf("readDoltRuntimeStateFile: %v", err)
	}
	if state.Running {
		t.Fatalf("state.Running = true, want false")
	}
	if state.PID != 0 {
		t.Fatalf("state.PID = %d, want 0", state.PID)
	}
}

func TestDoltStateRecoverManagedCmdReportsReadOnlyAndRestarts(t *testing.T) {
	cityPath := t.TempDir()
	layout, err := resolveManagedDoltRuntimeLayout(cityPath)
	if err != nil {
		t.Fatalf("resolveManagedDoltRuntimeLayout: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(layout.PIDFile), 0o755); err != nil {
		t.Fatalf("MkdirAll(runtime dir): %v", err)
	}
	if err := os.MkdirAll(layout.DataDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(data dir): %v", err)
	}

	original := exec.Command("sleep", "30")
	if err := original.Start(); err != nil {
		t.Fatalf("start original sleep: %v", err)
	}
	defer func() {
		_ = original.Process.Kill()
		_, _ = original.Process.Wait()
	}()
	if err := os.WriteFile(layout.PIDFile, []byte(strconv.Itoa(original.Process.Pid)+"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(pid): %v", err)
	}
	if err := writeDoltRuntimeStateFile(layout.StateFile, doltRuntimeState{
		Running:   true,
		PID:       original.Process.Pid,
		Port:      3311,
		DataDir:   layout.DataDir,
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("writeDoltRuntimeStateFile: %v", err)
	}

	binDir := t.TempDir()
	invocationFile := filepath.Join(t.TempDir(), "dolt-invocation.txt")
	readOnlyOnce := filepath.Join(t.TempDir(), "read-only-once")
	if err := os.WriteFile(readOnlyOnce, []byte("1\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(read-only-once): %v", err)
	}
	writeFakeDoltSQLBinary(t, binDir, invocationFile, `#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$INVOCATION_FILE"
case "$*" in
  "sql-server --config "*)
    config_file=${*#sql-server --config }
    port=$(awk '/port:/ {print $2; exit}' "$config_file")
    exec python3 - "$port" <<'INNERPY'
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
INNERPY
    ;;
  *"SELECT COUNT(*) AS cnt FROM information_schema.PROCESSLIST"*)
    printf 'cnt\n1\n'
    ;;
  *"SELECT active_branch()"*)
    exit 0
    ;;
  *"CREATE DATABASE IF NOT EXISTS __gc_probe;"*)
    if [ -f "$READ_ONLY_ONCE" ]; then
      rm -f "$READ_ONLY_ONCE"
      echo "read only" >&2
      exit 1
    fi
    exit 0
    ;;
  *)
    echo "unexpected command: $*" >&2
    exit 2
    ;;
esac
`)
	t.Setenv("INVOCATION_FILE", invocationFile)
	t.Setenv("READ_ONLY_ONCE", readOnlyOnce)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Cleanup(func() {
		if state, err := readDoltRuntimeStateFile(layout.StateFile); err == nil && state.PID > 0 {
			_ = terminateManagedDoltPID(state.PID)
		}
	})

	var stdout, stderr bytes.Buffer
	code := run([]string{"dolt-state", "recover-managed", "--city", cityPath, "--host", "127.0.0.1", "--port", "3311", "--user", "root", "--timeout-ms", "1000"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run() = %d, stdout = %s stderr = %s", code, stdout.String(), stderr.String())
	}
	got := parseDoltStateOutput(t, stdout.String())
	if got["diagnosed_read_only"] != "true" {
		t.Fatalf("diagnosed_read_only = %q, want true", got["diagnosed_read_only"])
	}
	if got["had_pid"] != "true" {
		t.Fatalf("had_pid = %q, want true", got["had_pid"])
	}
	if got["ready"] != "true" {
		t.Fatalf("ready = %q, want true", got["ready"])
	}
	if got["healthy"] != "true" {
		t.Fatalf("healthy = %q, want true", got["healthy"])
	}
	state, err := readDoltRuntimeStateFile(layout.StateFile)
	if err != nil {
		t.Fatalf("readDoltRuntimeStateFile: %v", err)
	}
	published, err := readDoltRuntimeStateFile(managedDoltStatePath(cityPath))
	if err != nil {
		t.Fatalf("read published dolt runtime state: %v", err)
	}
	if !state.Running {
		t.Fatalf("state.Running = false, want true")
	}
	if !published.Running {
		t.Fatalf("published.Running = false, want true")
	}
	if state.PID == 0 || state.PID == original.Process.Pid {
		t.Fatalf("state.PID = %d, want a new managed pid", state.PID)
	}
	if published.PID != state.PID {
		t.Fatalf("published.PID = %d, want %d", published.PID, state.PID)
	}
	if published.Port != state.Port {
		t.Fatalf("published.Port = %d, want %d", published.Port, state.Port)
	}
	if managedStopPIDAlive(original.Process.Pid) {
		t.Fatalf("original pid %d still alive after recovery", original.Process.Pid)
	}
}

func TestDoltStateRecoverManagedCmdFailsWhenPostStartHealthFails(t *testing.T) {
	cityPath := t.TempDir()
	layout, err := resolveManagedDoltRuntimeLayout(cityPath)
	if err != nil {
		t.Fatalf("resolveManagedDoltRuntimeLayout: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(layout.PIDFile), 0o755); err != nil {
		t.Fatalf("MkdirAll(runtime dir): %v", err)
	}
	if err := os.MkdirAll(layout.DataDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(data dir): %v", err)
	}

	original := exec.Command("sleep", "30")
	if err := original.Start(); err != nil {
		t.Fatalf("start original sleep: %v", err)
	}
	defer func() {
		_ = original.Process.Kill()
		_, _ = original.Process.Wait()
	}()
	if err := os.WriteFile(layout.PIDFile, []byte(strconv.Itoa(original.Process.Pid)+"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(pid): %v", err)
	}
	if err := writeDoltRuntimeStateFile(layout.StateFile, doltRuntimeState{
		Running:   true,
		PID:       original.Process.Pid,
		Port:      3311,
		DataDir:   layout.DataDir,
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("writeDoltRuntimeStateFile: %v", err)
	}

	binDir := t.TempDir()
	invocationFile := filepath.Join(t.TempDir(), "dolt-invocation.txt")
	activeBranchCount := filepath.Join(t.TempDir(), "active-branch-count")
	writeFakeDoltSQLBinary(t, binDir, invocationFile, `#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$INVOCATION_FILE"
case "$*" in
  "sql-server --config "*)
    config_file=${*#sql-server --config }
    port=$(awk '/port:/ {print $2; exit}' "$config_file")
    exec python3 - "$port" <<'INNERPY'
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
INNERPY
    ;;
  *"SELECT COUNT(*) AS cnt FROM information_schema.PROCESSLIST"*)
    printf 'cnt\n1\n'
    ;;
  *"SELECT active_branch()"*)
    count=0
    if [ -f "$ACTIVE_BRANCH_COUNT" ]; then
      count=$(cat "$ACTIVE_BRANCH_COUNT")
    fi
    count=$((count + 1))
    printf '%s\n' "$count" > "$ACTIVE_BRANCH_COUNT"
    if [ "$count" -le 3 ]; then
      exit 0
    fi
    echo "final health probe failed" >&2
    exit 1
    ;;
  *"CREATE DATABASE IF NOT EXISTS __gc_probe;"*)
    exit 0
    ;;
  *)
    echo "unexpected command: $*" >&2
    exit 2
    ;;
esac
`)
	t.Setenv("INVOCATION_FILE", invocationFile)
	t.Setenv("ACTIVE_BRANCH_COUNT", activeBranchCount)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Cleanup(func() {
		if state, err := readDoltRuntimeStateFile(layout.StateFile); err == nil && state.PID > 0 {
			_ = terminateManagedDoltPID(state.PID)
		}
	})

	var stdout, stderr bytes.Buffer
	code := run([]string{"dolt-state", "recover-managed", "--city", cityPath, "--host", "127.0.0.1", "--port", "3311", "--user", "root", "--timeout-ms", "1000"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("run() = %d, want 1; stdout = %s stderr = %s", code, stdout.String(), stderr.String())
	}
	got := parseDoltStateOutput(t, stdout.String())
	if got["had_pid"] != "true" {
		t.Fatalf("had_pid = %q, want true", got["had_pid"])
	}
	if got["ready"] != "true" {
		t.Fatalf("ready = %q, want true", got["ready"])
	}
	if got["healthy"] != "false" {
		t.Fatalf("healthy = %q, want false", got["healthy"])
	}
	if !strings.Contains(stderr.String(), "recover-managed") {
		t.Fatalf("stderr = %q, want recover-managed failure", stderr.String())
	}
}

func writeFakeDoltSQLBinary(t *testing.T, binDir, invocationFile, body string) {
	t.Helper()
	script := strings.ReplaceAll(body, "$INVOCATION_FILE", invocationFile)
	path := filepath.Join(binDir, "dolt")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile(fake dolt): %v", err)
	}
}
