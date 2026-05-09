package main

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/pidutil"
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

func requireDeletedPathHeld(t *testing.T, pid int, targetPath string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !processHoldsDeletedPath(pid, targetPath) {
		if time.Now().After(deadline) {
			t.Fatalf("process %d did not hold deleted %s", pid, targetPath)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func symlinkedCityPaths(t *testing.T) (aliasCity, realCity string) {
	t.Helper()
	root := t.TempDir()
	realParent := filepath.Join(root, "real")
	if err := os.MkdirAll(realParent, 0o755); err != nil {
		t.Fatal(err)
	}
	aliasParent := filepath.Join(root, "alias")
	if err := os.Symlink(realParent, aliasParent); err != nil {
		t.Skip("symlinks not supported")
	}
	aliasCity = filepath.Join(aliasParent, "bright-lights")
	realCity = filepath.Join(realParent, "bright-lights")
	if err := os.MkdirAll(realCity, 0o755); err != nil {
		t.Fatal(err)
	}
	return aliasCity, realCity
}

func TestDoltStateRuntimeLayoutCmdUsesCanonicalPaths(t *testing.T) {
	cityPath := t.TempDir()
	wantCityPath := normalizePathForCompare(cityPath)
	var stdout, stderr bytes.Buffer
	code := run([]string{"dolt-state", "runtime-layout", "--city", cityPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run() = %d, stderr = %s", code, stderr.String())
	}
	got := parseDoltRuntimeLayoutOutput(t, stdout.String())
	wantPack := citylayout.PackStateDir(wantCityPath, "dolt")
	want := map[string]string{
		"GC_PACK_STATE_DIR":   wantPack,
		"GC_DOLT_DATA_DIR":    filepath.Join(wantCityPath, ".beads", "dolt"),
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

func TestResolveManagedDoltRuntimeLayoutCanonicalizesSymlinkedCityPath(t *testing.T) {
	aliasCity, realCity := symlinkedCityPaths(t)
	wantCityPath := normalizePathForCompare(realCity)

	layout, err := resolveManagedDoltRuntimeLayout(aliasCity)
	if err != nil {
		t.Fatalf("resolveManagedDoltRuntimeLayout: %v", err)
	}

	packStateDir := citylayout.PackStateDir(wantCityPath, "dolt")
	want := managedDoltRuntimeLayout{
		PackStateDir: packStateDir,
		DataDir:      filepath.Join(wantCityPath, ".beads", "dolt"),
		LogFile:      filepath.Join(packStateDir, "dolt.log"),
		StateFile:    filepath.Join(packStateDir, "dolt-provider-state.json"),
		PIDFile:      filepath.Join(packStateDir, "dolt.pid"),
		LockFile:     filepath.Join(packStateDir, "dolt.lock"),
		ConfigFile:   filepath.Join(packStateDir, "dolt-config.yaml"),
	}
	if layout != want {
		t.Fatalf("resolveManagedDoltRuntimeLayout() = %+v, want %+v", layout, want)
	}
}

func TestValidDoltRuntimeStateAcceptsSymlinkEquivalentDataDir(t *testing.T) {
	aliasCity, realCity := symlinkedCityPaths(t)
	layout, err := resolveManagedDoltRuntimeLayout(realCity)
	if err != nil {
		t.Fatalf("resolveManagedDoltRuntimeLayout: %v", err)
	}

	port := reserveRandomTCPPort(t)
	listener := startTCPListenerProcessInDir(t, port, layout.DataDir)
	defer func() {
		_ = listener.Process.Kill()
		_ = listener.Wait()
	}()

	cases := []struct {
		name     string
		cityPath string
		dataDir  string
	}{
		{
			name:     "real state data dir against aliased city path",
			cityPath: aliasCity,
			dataDir:  layout.DataDir,
		},
		{
			name:     "aliased state data dir against real city path",
			cityPath: realCity,
			dataDir:  filepath.Join(aliasCity, ".beads", "dolt"),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			state := doltRuntimeState{
				Running:   true,
				PID:       listener.Process.Pid,
				Port:      port,
				DataDir:   tc.dataDir,
				StartedAt: time.Now().UTC().Format(time.RFC3339),
			}
			if !validDoltRuntimeState(state, tc.cityPath) {
				t.Fatalf("validDoltRuntimeState(%q, %q) = false, want true", tc.dataDir, tc.cityPath)
			}
		})
	}
}

func TestRepairedManagedDoltRuntimeStateAcceptsSymlinkEquivalentDataDir(t *testing.T) {
	aliasCity, realCity := symlinkedCityPaths(t)
	layout, err := resolveManagedDoltRuntimeLayout(realCity)
	if err != nil {
		t.Fatalf("resolveManagedDoltRuntimeLayout: %v", err)
	}

	port := reserveRandomTCPPort(t)
	listener := startTCPListenerProcessInDir(t, port, layout.DataDir)
	defer func() {
		_ = listener.Process.Kill()
		_ = listener.Wait()
	}()

	cases := []struct {
		name    string
		dataDir string
	}{
		{
			name:    "real data dir",
			dataDir: layout.DataDir,
		},
		{
			name:    "aliased data dir",
			dataDir: filepath.Join(aliasCity, ".beads", "dolt"),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repaired, ok := repairedManagedDoltRuntimeState(aliasCity, layout, doltRuntimeState{
				Port:    port,
				DataDir: tc.dataDir,
			})
			if !ok {
				t.Fatalf("repairedManagedDoltRuntimeState(%q) = not ok, want ok", tc.dataDir)
			}
			if !repaired.Running {
				t.Fatal("repaired state Running = false, want true")
			}
			if repaired.PID != listener.Process.Pid {
				t.Fatalf("repaired state PID = %d, want %d", repaired.PID, listener.Process.Pid)
			}
			if repaired.Port != port {
				t.Fatalf("repaired state Port = %d, want %d", repaired.Port, port)
			}
			if repaired.DataDir != layout.DataDir {
				t.Fatalf("repaired state DataDir = %q, want %q", repaired.DataDir, layout.DataDir)
			}
			if strings.TrimSpace(repaired.StartedAt) == "" {
				t.Fatal("repaired state StartedAt is empty")
			}
		})
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

func TestChooseManagedDoltPortUsesCanonicalSeedForSymlinkedCityPath(t *testing.T) {
	aliasCity, realCity := symlinkedCityPaths(t)

	if got, want := deterministicManagedDoltPortSeed(aliasCity), deterministicManagedDoltPortSeed(realCity); got != want {
		t.Fatalf("deterministicManagedDoltPortSeed(alias) = %d, want %d", got, want)
	}

	aliasPort, err := chooseManagedDoltPort(aliasCity, "")
	if err != nil {
		t.Fatalf("chooseManagedDoltPort(alias): %v", err)
	}
	realPort, err := chooseManagedDoltPort(realCity, "")
	if err != nil {
		t.Fatalf("chooseManagedDoltPort(real): %v", err)
	}
	if aliasPort != realPort {
		t.Fatalf("chooseManagedDoltPort(alias) = %q, want %q", aliasPort, realPort)
	}
}

func TestDoltStateAllocatePortCmdReusesLiveProviderState(t *testing.T) {
	cityPath := t.TempDir()
	stateFile := filepath.Join(t.TempDir(), "dolt-provider-state.json")
	layout, err := resolveManagedDoltRuntimeLayout(cityPath)
	if err != nil {
		t.Fatalf("resolveManagedDoltRuntimeLayout: %v", err)
	}
	port := reserveRandomTCPPort(t)
	listener := startTCPListenerProcessInDir(t, port, layout.DataDir)
	if err := writeDoltRuntimeStateFile(stateFile, doltRuntimeState{
		Running:   true,
		PID:       listener.Process.Pid,
		Port:      port,
		DataDir:   layout.DataDir,
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
}

func TestStartTCPListenerProcessInDirRegistersCleanup(t *testing.T) {
	skipSlowCmdGCTest(t, "spawns a TCP listener process and verifies cleanup; run make test-cmd-gc-process for full coverage")
	port := reserveRandomTCPPort(t)
	dir := t.TempDir()
	var proc *exec.Cmd

	t.Run("listener", func(t *testing.T) {
		proc = startTCPListenerProcessInDir(t, port, dir)
		if !pidutil.Alive(proc.Process.Pid) {
			t.Fatalf("listener pid %d is not alive after start", proc.Process.Pid)
		}
	})

	if proc == nil {
		t.Fatal("listener process handle was not captured")
	}
	if proc.ProcessState == nil {
		t.Fatal("listener cleanup did not wait for process exit")
	}
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 200*time.Millisecond)
	if err == nil {
		_ = conn.Close()
		t.Fatal("listener port is still reachable after cleanup")
	}
}

func TestManagedDoltExistingStatePortReturnsPublishedPortBeforeListenerReady(t *testing.T) {
	cityPath := t.TempDir()
	layout, err := resolveManagedDoltRuntimeLayout(cityPath)
	if err != nil {
		t.Fatalf("resolveManagedDoltRuntimeLayout: %v", err)
	}
	if err := os.MkdirAll(layout.DataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeDoltRuntimeStateFile(layout.StateFile, doltRuntimeState{
		Running:   true,
		PID:       os.Getpid(),
		Port:      43129,
		DataDir:   layout.DataDir,
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("writeDoltRuntimeStateFile: %v", err)
	}
	if got := managedDoltExistingStatePort(cityPath, layout, os.Getpid()); got != 43129 {
		t.Fatalf("managedDoltExistingStatePort = %d, want 43129 before listener is reachable", got)
	}
}

func TestValidDoltRuntimeStateRequiresExpectedDataDir(t *testing.T) {
	cityPath := t.TempDir()
	if got := validDoltRuntimeState(doltRuntimeState{
		Running: true,
		PID:     os.Getpid(),
		Port:    43130,
		DataDir: "",
	}, cityPath); got {
		t.Fatal("validDoltRuntimeState = true, want false when data_dir is empty")
	}
}

func TestValidDoltRuntimeStateRejectsAlivePIDThatDoesNotOwnManagedDolt(t *testing.T) {
	cityPath := t.TempDir()
	layout, err := resolveManagedDoltRuntimeLayout(cityPath)
	if err != nil {
		t.Fatalf("resolveManagedDoltRuntimeLayout: %v", err)
	}
	if err := os.MkdirAll(layout.DataDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(data dir): %v", err)
	}

	port := reserveRandomTCPPort(t)
	listener := startTCPListenerProcessInDir(t, port, layout.DataDir)
	defer func() {
		_ = listener.Process.Kill()
		_ = listener.Wait()
	}()

	if got := validDoltRuntimeState(doltRuntimeState{
		Running:   true,
		PID:       os.Getpid(),
		Port:      port,
		DataDir:   layout.DataDir,
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}, cityPath); got {
		t.Fatal("validDoltRuntimeState = true, want false when pid is alive but does not own managed Dolt")
	}
}

func TestDoltStateAllocatePortCmdRepairsStaleProviderStateFromOwnedLivePortHolder(t *testing.T) {
	cityPath := t.TempDir()
	stateFile := filepath.Join(t.TempDir(), "dolt-provider-state.json")
	layout, err := resolveManagedDoltRuntimeLayout(cityPath)
	if err != nil {
		t.Fatalf("resolveManagedDoltRuntimeLayout: %v", err)
	}
	if err := os.MkdirAll(layout.DataDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(data dir): %v", err)
	}

	port := reserveRandomTCPPort(t)
	listener := startTCPListenerProcessInDir(t, port, layout.DataDir)
	defer func() {
		_ = listener.Process.Kill()
		_ = listener.Wait()
	}()

	if err := writeDoltRuntimeStateFile(stateFile, doltRuntimeState{
		Running:   true,
		PID:       999999,
		Port:      port,
		DataDir:   layout.DataDir,
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
	if state.PID != listener.Process.Pid {
		t.Fatalf("repaired state pid = %d, want %d", state.PID, listener.Process.Pid)
	}
}

func TestDoltStateAllocatePortCmdRepairsStoppedProviderStateFromOwnedLivePortHolder(t *testing.T) {
	cityPath := t.TempDir()
	stateFile := filepath.Join(t.TempDir(), "dolt-provider-state.json")
	layout, err := resolveManagedDoltRuntimeLayout(cityPath)
	if err != nil {
		t.Fatalf("resolveManagedDoltRuntimeLayout: %v", err)
	}
	if err := os.MkdirAll(layout.DataDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(data dir): %v", err)
	}

	port := reserveRandomTCPPort(t)
	listener := startTCPListenerProcessInDir(t, port, layout.DataDir)
	defer func() {
		_ = listener.Process.Kill()
		_ = listener.Wait()
	}()

	if err := writeDoltRuntimeStateFile(stateFile, doltRuntimeState{
		Running:   false,
		PID:       0,
		Port:      port,
		DataDir:   layout.DataDir,
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
	if state.PID != listener.Process.Pid {
		t.Fatalf("repaired state pid = %d, want %d", state.PID, listener.Process.Pid)
	}
}

func TestDoltStateAllocatePortCmdRepairsMissingProviderStateFromPublishedHint(t *testing.T) {
	cityPath := t.TempDir()
	stateFile := filepath.Join(t.TempDir(), "dolt-provider-state.json")
	layout, err := resolveManagedDoltRuntimeLayout(cityPath)
	if err != nil {
		t.Fatalf("resolveManagedDoltRuntimeLayout: %v", err)
	}
	if err := os.MkdirAll(layout.DataDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(data dir): %v", err)
	}

	port := reserveRandomTCPPort(t)
	listener := startTCPListenerProcessInDir(t, port, layout.DataDir)
	defer func() {
		_ = listener.Process.Kill()
		_ = listener.Wait()
	}()

	if err := writeDoltRuntimeStateFile(managedDoltStatePath(cityPath), doltRuntimeState{
		Running:   false,
		PID:       0,
		Port:      port,
		DataDir:   layout.DataDir,
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("writeDoltRuntimeStateFile(published): %v", err)
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
		t.Fatalf("readDoltRuntimeStateFile(provider): %v", err)
	}
	if !state.Running {
		t.Fatalf("repaired state running = false, want true")
	}
	if state.Port != port {
		t.Fatalf("repaired state port = %d, want %d", state.Port, port)
	}
	if state.PID != listener.Process.Pid {
		t.Fatalf("repaired state pid = %d, want %d", state.PID, listener.Process.Pid)
	}

	if _, err := os.Stat(layout.StateFile); !os.IsNotExist(err) {
		t.Fatalf("canonical provider state was touched for non-canonical --state-file: %v", err)
	}
}

func TestDoltStateAllocatePortCmdRepairsMissingCanonicalProviderStateFromPublishedHint(t *testing.T) {
	cityPath := t.TempDir()
	layout, err := resolveManagedDoltRuntimeLayout(cityPath)
	if err != nil {
		t.Fatalf("resolveManagedDoltRuntimeLayout: %v", err)
	}
	if err := os.MkdirAll(layout.DataDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(data dir): %v", err)
	}

	port := reserveRandomTCPPort(t)
	listener := startTCPListenerProcessInDir(t, port, layout.DataDir)
	defer func() {
		_ = listener.Process.Kill()
		_ = listener.Wait()
	}()

	if err := writeDoltRuntimeStateFile(managedDoltStatePath(cityPath), doltRuntimeState{
		Running:   false,
		PID:       0,
		Port:      port,
		DataDir:   layout.DataDir,
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("writeDoltRuntimeStateFile(published): %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"dolt-state", "allocate-port", "--city", cityPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run() = %d, stderr = %s", code, stderr.String())
	}
	if got := strings.TrimSpace(stdout.String()); got != strconv.Itoa(port) {
		t.Fatalf("allocate-port = %q, want %d", got, port)
	}

	state, err := readDoltRuntimeStateFile(layout.StateFile)
	if err != nil {
		t.Fatalf("readDoltRuntimeStateFile(provider): %v", err)
	}
	if !state.Running {
		t.Fatalf("repaired state running = false, want true")
	}
	if state.Port != port {
		t.Fatalf("repaired state port = %d, want %d", state.Port, port)
	}
	if state.PID != listener.Process.Pid {
		t.Fatalf("repaired state pid = %d, want %d", state.PID, listener.Process.Pid)
	}
}

func TestDoltStateAllocatePortCmdRepairsStaleWrongPortProviderStateFromPublishedHint(t *testing.T) {
	cityPath := t.TempDir()
	stateFile := filepath.Join(t.TempDir(), "dolt-provider-state.json")
	layout, err := resolveManagedDoltRuntimeLayout(cityPath)
	if err != nil {
		t.Fatalf("resolveManagedDoltRuntimeLayout: %v", err)
	}
	if err := os.MkdirAll(layout.DataDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(data dir): %v", err)
	}

	stalePort := reserveRandomTCPPort(t)
	if err := writeDoltRuntimeStateFile(stateFile, doltRuntimeState{
		Running:   true,
		PID:       999999,
		Port:      stalePort,
		DataDir:   layout.DataDir,
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("writeDoltRuntimeStateFile(provider): %v", err)
	}

	port := reserveRandomTCPPort(t)
	listener := startTCPListenerProcessInDir(t, port, layout.DataDir)
	defer func() {
		_ = listener.Process.Kill()
		_ = listener.Wait()
	}()

	if err := writeDoltRuntimeStateFile(managedDoltStatePath(cityPath), doltRuntimeState{
		Running:   false,
		PID:       0,
		Port:      port,
		DataDir:   layout.DataDir,
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("writeDoltRuntimeStateFile(published): %v", err)
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
		t.Fatalf("readDoltRuntimeStateFile(provider): %v", err)
	}
	if !state.Running {
		t.Fatalf("repaired state running = false, want true")
	}
	if state.Port != port {
		t.Fatalf("repaired state port = %d, want %d", state.Port, port)
	}
	if state.PID != listener.Process.Pid {
		t.Fatalf("repaired state pid = %d, want %d", state.PID, listener.Process.Pid)
	}
	if _, err := os.Stat(layout.StateFile); !os.IsNotExist(err) {
		t.Fatalf("canonical provider state was touched for non-canonical --state-file: %v", err)
	}
}

func TestDoltStateAllocatePortCmdIgnoresMalformedPublishedHint(t *testing.T) {
	cityPath := t.TempDir()
	stateFile := filepath.Join(t.TempDir(), "dolt-provider-state.json")
	publishedPath := managedDoltStatePath(cityPath)
	if err := os.MkdirAll(filepath.Dir(publishedPath), 0o755); err != nil {
		t.Fatalf("MkdirAll(published dir): %v", err)
	}
	if err := os.WriteFile(publishedPath, []byte("{not-json"), 0o644); err != nil {
		t.Fatalf("write malformed published hint: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"dolt-state", "allocate-port", "--city", cityPath, "--state-file", stateFile}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run() = %d, stderr = %s", code, stderr.String())
	}
	if _, err := strconv.Atoi(strings.TrimSpace(stdout.String())); err != nil {
		t.Fatalf("allocate-port output %q is not a port: %v", stdout.String(), err)
	}
	if _, err := os.Stat(stateFile); !os.IsNotExist(err) {
		t.Fatalf("provider state was written from malformed hint: %v", err)
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
	port := reserveRandomTCPPort(t)
	proc := startTCPListenerProcessInDir(t, port, layout.DataDir)
	defer func() {
		_ = proc.Process.Kill()
		_ = proc.Wait()
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
		Port:      port,
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
	listener := startTCPListenerProcessInDir(t, port, layout.DataDir)
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
	listener := startTCPListenerProcessInDir(t, port, layout.DataDir)
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
	deletedPath := filepath.Join(layout.DataDir, "held.db")
	proc := startOpenFileAndTCPListenerProcess(t, deletedPath, port, layout.DataDir)
	defer func() {
		_ = proc.Process.Kill()
		_, _ = proc.Process.Wait()
	}()
	if err := os.Remove(deletedPath); err != nil {
		t.Fatalf("Remove(%s): %v", deletedPath, err)
	}
	requireDeletedPathHeld(t, proc.Process.Pid, deletedPath)
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

func TestDoltStateExistingManagedCmdRejectsForeignListenerBackedOnlyByStateDataDir(t *testing.T) {
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
	code := run([]string{"dolt-state", "existing-managed", "--city", cityPath, "--host", "127.0.0.1", "--port", strconv.Itoa(port), "--user", "root", "--timeout-ms", "100"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run() = %d, stderr = %s", code, stderr.String())
	}
	got := parseDoltStateOutput(t, stdout.String())
	if got["managed_pid"] != strconv.Itoa(listener.Process.Pid) {
		t.Fatalf("managed_pid = %q, want %d", got["managed_pid"], listener.Process.Pid)
	}
	if got["managed_owned"] != "false" {
		t.Fatalf("managed_owned = %q, want false", got["managed_owned"])
	}
	if got["port_holder_owned"] != "false" {
		t.Fatalf("port_holder_owned = %q, want false", got["port_holder_owned"])
	}
	if got["ready"] != "false" {
		t.Fatalf("ready = %q, want false", got["ready"])
	}
	if got["reusable"] != "false" {
		t.Fatalf("reusable = %q, want false", got["reusable"])
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
	listener := startTCPListenerProcessInDir(t, port, layout.DataDir)
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

func TestDoltStateExistingManagedCmdFallsBackToPublishedRuntimeState(t *testing.T) {
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
	listener := startTCPListenerProcessInDir(t, port, layout.DataDir)
	defer func() {
		_ = listener.Process.Kill()
		_ = listener.Wait()
	}()
	state := doltRuntimeState{
		Running:   true,
		PID:       listener.Process.Pid,
		Port:      port,
		DataDir:   layout.DataDir,
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if err := writeDoltRuntimeStateFile(managedDoltStatePath(cityPath), state); err != nil {
		t.Fatalf("writeDoltRuntimeStateFile(published): %v", err)
	}
	if _, err := os.Stat(layout.StateFile); !os.IsNotExist(err) {
		t.Fatalf("provider state should be absent, stat err = %v", err)
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
	deletedPath := filepath.Join(layout.DataDir, "held.db")
	proc := startOpenFileAndTCPListenerProcess(t, deletedPath, port, "")
	defer func() {
		_ = proc.Process.Kill()
		_, _ = proc.Process.Wait()
	}()
	if err := os.Remove(deletedPath); err != nil {
		t.Fatalf("Remove(%s): %v", deletedPath, err)
	}
	requireDeletedPathHeld(t, proc.Process.Pid, deletedPath)
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
	skipSlowCmdGCTest(t, "spawns managed dolt holder processes; run make test-cmd-gc-process for full coverage")
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

func startTCPListenerProcessInDir(t *testing.T, port int, dir string) *exec.Cmd {
	t.Helper()
	skipSlowCmdGCTest(t, "spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", dir, err)
	}
	cmd := exec.Command("python3", "-c", `
import os
import signal
import socket
import sys
import time
port = int(sys.argv[1])
os.chdir(sys.argv[2])
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
`, strconv.Itoa(port), dir)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start listener process in %s: %v", dir, err)
	}
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
	})
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return cmd
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = cmd.Process.Kill()
	_, _ = cmd.Process.Wait()
	t.Fatalf("listener process on %d in %s did not become ready", port, dir)
	return nil
}

func startLockedDelayedTCPListenerProcessInDir(t *testing.T, lockFile string, port int, dir string, delay time.Duration) *exec.Cmd {
	t.Helper()
	skipSlowCmdGCTest(t, "spawns a delayed TCP listener process to emulate managed dolt recovery; run make test-cmd-gc-process for full coverage")
	if err := os.MkdirAll(filepath.Dir(lockFile), 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", filepath.Dir(lockFile), err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", dir, err)
	}
	readyFile := filepath.Join(t.TempDir(), "lock-held.ready")
	cmd := exec.Command("python3", "-c", `
import fcntl
import os
import signal
import socket
import sys
import time

lock_file = sys.argv[1]
ready_file = sys.argv[2]
port = int(sys.argv[3])
data_dir = sys.argv[4]
delay_s = float(sys.argv[5])

os.makedirs(os.path.dirname(lock_file), exist_ok=True)
os.makedirs(data_dir, exist_ok=True)
lock = open(lock_file, "a+")
fcntl.flock(lock.fileno(), fcntl.LOCK_EX)
with open(ready_file, "w", encoding="utf-8") as f:
    f.write("locked\n")
os.chdir(data_dir)
time.sleep(delay_s)
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
`, lockFile, readyFile, strconv.Itoa(port), dir, strconv.FormatFloat(delay.Seconds(), 'f', 3, 64))
	if err := cmd.Start(); err != nil {
		t.Fatalf("start locked delayed listener process in %s: %v", dir, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(readyFile); err == nil {
			return cmd
		}
		time.Sleep(25 * time.Millisecond)
	}
	_ = cmd.Process.Kill()
	_, _ = cmd.Process.Wait()
	t.Fatalf("locked delayed listener process on %d in %s did not acquire lifecycle lock", port, dir)
	return nil
}

func startUnixSocketProcess(t *testing.T, socketPath string) *exec.Cmd {
	t.Helper()
	readyPath := filepath.Join(t.TempDir(), "ready")
	proc := exec.Command("python3", "-c", `
import os
import socket
import sys
import time
path = sys.argv[1]
ready_path = sys.argv[2]
if os.path.exists(path):
    os.remove(path)
sock = socket.socket(socket.AF_UNIX)
sock.bind(path)
sock.listen(1)
with open(ready_path, "w") as f:
    f.write("ready\n")
while True:
    time.sleep(1)
`, socketPath, readyPath)
	if err := proc.Start(); err != nil {
		t.Fatalf("start unix socket process: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(socketPath); err == nil {
			if _, readyErr := os.Stat(readyPath); readyErr == nil {
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
	readyPath := filepath.Join(t.TempDir(), "ready")
	proc := exec.Command("python3", "-c", `
import os
import sys
import time
path = sys.argv[1]
ready_path = sys.argv[2]
f = open(path, "a+")
f.write("held")
f.flush()
with open(ready_path, "w") as f_ready:
    f_ready.write("ready\n")
while True:
    time.sleep(1)
`, path, readyPath)
	if err := proc.Start(); err != nil {
		t.Fatalf("start open-file process: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			if _, readyErr := os.Stat(readyPath); readyErr == nil {
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

func startOpenFileAndTCPListenerProcess(t *testing.T, path string, port int, dir string) *exec.Cmd {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	readyPath := filepath.Join(t.TempDir(), "ready")
	proc := exec.Command("python3", "-c", `
import os
import signal
import socket
import sys
import time
path = sys.argv[1]
port = int(sys.argv[2])
ready_path = sys.argv[3]
f = open(path, "a+")
f.write("held")
f.flush()
sock = socket.socket()
sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
sock.bind(("127.0.0.1", port))
sock.listen(5)
with open(ready_path, "w") as f_ready:
    f_ready.write("ready\n")
def _stop(*_args):
    raise SystemExit(0)
signal.signal(signal.SIGTERM, _stop)
signal.signal(signal.SIGINT, _stop)
while True:
    time.sleep(1)
`, path, strconv.Itoa(port), readyPath)
	if strings.TrimSpace(dir) != "" {
		proc.Dir = dir
	}
	if err := proc.Start(); err != nil {
		t.Fatalf("start open-file listener process: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			if _, readyErr := os.Stat(readyPath); readyErr == nil {
				conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 200*time.Millisecond)
				if err == nil {
					_ = conn.Close()
					return proc
				}
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	_ = proc.Process.Kill()
	_ = proc.Wait()
	t.Fatalf("open file listener %s on %d did not become ready", path, port)
	return nil
}

func processHoldsDeletedPath(pid int, targetPath string) bool {
	fdDir := filepath.Join("/proc", strconv.Itoa(pid), "fd")
	entries, err := os.ReadDir(fdDir)
	if err == nil {
		for _, entry := range entries {
			target, readErr := os.Readlink(filepath.Join(fdDir, entry.Name()))
			if readErr != nil || !strings.Contains(target, " (deleted)") {
				continue
			}
			if samePath(strings.TrimSuffix(target, " (deleted)"), targetPath) {
				return true
			}
		}
	}
	for _, target := range deletedDataInodeTargetsFromLsof(pid) {
		if samePath(target, targetPath) {
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
case "$*" in
  *"sql -r csv -q SHOW DATABASES"*)
    printf 'Database\ngascity\ninformation_schema\nmysql\ndolt_cluster\n__gc_probe\n'
    exit 0
    ;;
  *"CREATE TABLE IF NOT EXISTS"*"__gc_read_only_probe"*)
    echo 'database is read only' >&2
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
	code := run([]string{"dolt-state", "read-only-check", "--host", "127.0.0.1", "--port", "3311", "--user", "root"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run() = %d, stderr = %s", code, stderr.String())
	}
	invocation, err := os.ReadFile(invocationFile)
	if err != nil {
		t.Fatalf("ReadFile(invocation): %v", err)
	}
	text := string(invocation)
	bt := "`"
	assertNoManagedDoltProbeDrop(t, "read-only-check invocation", text)
	assertNoManagedDoltProbeLegacyTarget(t, "read-only-check invocation", text)
	wantWrite := "REPLACE INTO " + bt + "gascity" + bt + "." + bt + managedDoltProbeTable + bt + " VALUES (1)"
	if !strings.Contains(text, wantWrite) {
		t.Fatalf("read-only-check invocation = %s, want %q", text, wantWrite)
	}
}

func TestDoltStateReadOnlyCheckCmdReturnsErrExitWhenWritable(t *testing.T) {
	binDir := t.TempDir()
	invocationFile := filepath.Join(t.TempDir(), "dolt-invocation.txt")
	writeFakeDoltSQLBinary(t, binDir, invocationFile, `#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$INVOCATION_FILE"
case "$*" in
  *"sql -r csv -q SHOW DATABASES"*)
    printf 'Database\ngascity\n'
    exit 0
    ;;
  *)
    exit 0
    ;;
esac
`)
	t.Setenv("INVOCATION_FILE", invocationFile)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	var stdout, stderr bytes.Buffer
	code := run([]string{"dolt-state", "read-only-check", "--host", "127.0.0.1", "--port", "3311", "--user", "root"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("run() = %d, want 1; stderr = %s", code, stderr.String())
	}
	invocation, err := os.ReadFile(invocationFile)
	if err != nil {
		t.Fatalf("ReadFile(invocation): %v", err)
	}
	assertNoManagedDoltProbeLegacyTarget(t, "read-only-check writable invocation", string(invocation))
}

func TestDoltStateReadOnlyCheckCmdNoUserDatabaseReturnsDiagnostic(t *testing.T) {
	binDir := t.TempDir()
	invocationFile := filepath.Join(t.TempDir(), "dolt-invocation.txt")
	writeFakeDoltSQLBinary(t, binDir, invocationFile, `#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$INVOCATION_FILE"
case "$*" in
  *"sql -r csv -q SHOW DATABASES"*)
    printf 'Database\ninformation_schema\nmysql\ndolt_cluster\nperformance_schema\nsys\n__gc_probe\n'
    exit 0
    ;;
  *"CREATE TABLE IF NOT EXISTS"*"__gc_read_only_probe"*)
    echo "unexpected write probe without a user database" >&2
    exit 2
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
	code := run([]string{"dolt-state", "read-only-check", "--host", "127.0.0.1", "--port", "3311", "--user", "root"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("run() = %d, want 1; stdout = %s stderr = %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "no user database") {
		t.Fatalf("stderr = %q, want no-user-database diagnostic", stderr.String())
	}
	invocation, err := os.ReadFile(invocationFile)
	if err != nil {
		t.Fatalf("ReadFile(invocation): %v", err)
	}
	if strings.Contains(string(invocation), "CREATE TABLE IF NOT EXISTS") {
		t.Fatalf("read-only-check ran write probe without user database:\n%s", invocation)
	}
}

func TestDoltStateResetProbeCmdDropsManagedProbeDatabase(t *testing.T) {
	binDir := t.TempDir()
	invocationFile := filepath.Join(t.TempDir(), "dolt-invocation.txt")
	writeFakeDoltSQLBinary(t, binDir, invocationFile, `#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$INVOCATION_FILE"
case "$*" in
  *"sql -r csv -q SHOW DATABASES"*)
    printf 'Database\ngascity\ninformation_schema\nbeads\n__gc_probe\n'
    exit 0
    ;;
  *"DROP DATABASE IF EXISTS __gc_probe"*)
    exit 0
    ;;
  *"DROP TABLE IF EXISTS"*"__gc_read_only_probe"*)
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
	code := run([]string{"dolt-state", "reset-probe", "--host", "127.0.0.1", "--port", "3311", "--user", "root", "--force"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run() = %d, stderr = %s", code, stderr.String())
	}
	invocation, err := os.ReadFile(invocationFile)
	if err != nil {
		t.Fatalf("ReadFile(invocation): %v", err)
	}
	text := string(invocation)
	if !strings.Contains(text, "DROP DATABASE IF EXISTS "+managedDoltProbeDatabase) {
		t.Fatalf("reset-probe invocation = %s, want managed probe drop", text)
	}
	for _, want := range []string{"DROP TABLE IF EXISTS `gascity`.`" + managedDoltProbeTable + "`", "DROP TABLE IF EXISTS `beads`.`" + managedDoltProbeTable + "`"} {
		if !strings.Contains(text, want) {
			t.Fatalf("reset-probe invocation = %s, want %q", text, want)
		}
	}
}

func TestDoltStateResetProbeCmdRequiresForce(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"dolt-state", "reset-probe", "--host", "127.0.0.1", "--port", "3311", "--user", "root"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("run() = %d, want 1; stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "refusing to reset health probe artifacts without --force") ||
		!strings.Contains(stderr.String(), managedDoltProbeDatabase) ||
		!strings.Contains(stderr.String(), "legacy bead store") {
		t.Fatalf("stderr = %q, want force warning with legacy bead store context", stderr.String())
	}
}

func TestDoltStateResetProbeCmdUsesDirectConnectionWithPassword(t *testing.T) {
	binDir := t.TempDir()
	invocationFile := filepath.Join(t.TempDir(), "dolt-invocation.txt")
	writeFakeDoltSQLBinary(t, binDir, invocationFile, `#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$INVOCATION_FILE"
exit 42
`)
	t.Setenv("GC_DOLT_PASSWORD", "secret")
	t.Setenv("INVOCATION_FILE", invocationFile)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	oldResetDirect := managedDoltResetProbeDirectFn
	called := false
	managedDoltResetProbeDirectFn = func(host, port, user string) error {
		called = true
		if host != "127.0.0.1" || port != "3311" || user != "root" {
			t.Fatalf("managedDoltResetProbeDirectFn(%q, %q, %q), want requested connection", host, port, user)
		}
		return nil
	}
	t.Cleanup(func() {
		managedDoltResetProbeDirectFn = oldResetDirect
	})

	var stdout, stderr bytes.Buffer
	code := run([]string{"dolt-state", "reset-probe", "--host", "127.0.0.1", "--port", "3311", "--user", "root", "--force"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run() = %d, stderr = %s", code, stderr.String())
	}
	if !called {
		t.Fatalf("managedDoltResetProbeDirectFn was not called")
	}
	if _, err := os.Stat(invocationFile); !os.IsNotExist(err) {
		t.Fatalf("dolt CLI invocation file exists after password reset path: err=%v", err)
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
  *"sql -r csv -q SHOW DATABASES"*)
    printf 'Database\ngascity\ninformation_schema\nmysql\ndolt_cluster\n__gc_probe\n'
    exit 0
    ;;
  *"CREATE TABLE IF NOT EXISTS"*"__gc_read_only_probe"*)
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
	assertNoManagedDoltProbeDrop(t, "health-check read-only probe", text)
	assertNoManagedDoltProbeLegacyTarget(t, "health-check read-only probe", text)
	bt := "`"
	wantWrite := "REPLACE INTO " + bt + "gascity" + bt + "." + bt + managedDoltProbeTable + bt + " VALUES (1)"
	if !strings.Contains(text, wantWrite) {
		t.Fatalf("health-check probe = %s, want %q", text, wantWrite)
	}
	for _, want := range []string{"--host 127.0.0.1", "--port 3311", "--user root", "SELECT active_branch()", "information_schema.PROCESSLIST", "SHOW DATABASES"} {
		if strings.Contains(text, want) == false {
			t.Fatalf("dolt invocation missing %q: %s", want, text)
		}
	}
}

func TestDoltStateHealthCheckCmdNoUserDatabaseReportsUnknown(t *testing.T) {
	binDir := t.TempDir()
	invocationFile := filepath.Join(t.TempDir(), "dolt-invocation.txt")
	writeFakeDoltSQLBinary(t, binDir, invocationFile, `#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$INVOCATION_FILE"
case "$*" in
  *"sql -q SELECT active_branch()"*)
    exit 0
    ;;
  *"sql -r csv -q SHOW DATABASES"*)
    printf 'Database\ninformation_schema\nmysql\ndolt_cluster\nperformance_schema\nsys\n__gc_probe\n'
    exit 0
    ;;
  *"sql -r csv -q SELECT COUNT(*) AS cnt FROM information_schema.PROCESSLIST"*)
    printf 'cnt\n0\n'
    exit 0
    ;;
  *"CREATE TABLE IF NOT EXISTS"*)
    echo "unexpected write probe without a user database" >&2
    exit 2
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
		t.Fatalf("run() = %d, stdout = %s stderr = %s", code, stdout.String(), stderr.String())
	}
	got := parseDoltStateOutput(t, stdout.String())
	if got["query_ready"] != "true" {
		t.Fatalf("query_ready = %q, want true", got["query_ready"])
	}
	if got["read_only"] != "unknown" {
		t.Fatalf("read_only = %q, want unknown", got["read_only"])
	}
	if got["connection_count"] != "0" {
		t.Fatalf("connection_count = %q, want 0", got["connection_count"])
	}
	invocation, err := os.ReadFile(invocationFile)
	if err != nil {
		t.Fatalf("ReadFile(invocation): %v", err)
	}
	if strings.Contains(string(invocation), "CREATE TABLE IF NOT EXISTS") {
		t.Fatalf("health-check ran write probe without user database:\n%s", invocation)
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
	if strings.Contains(text, "CREATE TABLE IF NOT EXISTS") && strings.Contains(text, managedDoltProbeTable) {
		t.Fatalf("health-check unexpectedly ran read-only probe: %s", text)
	}
	if strings.Contains(text, "SHOW DATABASES") {
		t.Fatalf("health-check unexpectedly enumerated databases without --check-read-only: %s", text)
	}
	assertNoManagedDoltProbeLegacyTarget(t, "health-check skip-read-only probe", text)
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

func TestDoltStateHealthCheckCmdReturnsErrExitWhenReadOnlyProbeFails(t *testing.T) {
	binDir := t.TempDir()
	invocationFile := filepath.Join(t.TempDir(), "dolt-invocation.txt")
	writeFakeDoltSQLBinary(t, binDir, invocationFile, `#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$INVOCATION_FILE"
case "$*" in
  *"sql -q SELECT active_branch()"*)
    exit 0
    ;;
  *"sql -r csv -q SHOW DATABASES"*)
    printf 'Database\ngascity\n'
    exit 0
    ;;
  *"CREATE TABLE IF NOT EXISTS"*"__gc_read_only_probe"*)
    echo 'probe exploded' >&2
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
	code := run([]string{"dolt-state", "health-check", "--host", "127.0.0.1", "--port", "3311", "--user", "root", "--check-read-only"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("run() = %d, want 1; stdout = %s stderr = %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "health-check") {
		t.Fatalf("stderr = %q, want health-check failure", stderr.String())
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
	deletedPath := filepath.Join(layout.DataDir, "held.db")
	proc := startOpenFileAndTCPListenerProcess(t, deletedPath, port, "")
	t.Cleanup(func() {
		_ = proc.Process.Kill()
		_, _ = proc.Process.Wait()
	})
	if err := os.Remove(deletedPath); err != nil {
		t.Fatalf("Remove(%s): %v", deletedPath, err)
	}
	requireDeletedPathHeld(t, proc.Process.Pid, deletedPath)

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

	port := reserveRandomTCPPort(t)
	proc := startTCPListenerProcessInDir(t, port, layout.DataDir)
	defer func() {
		_ = proc.Process.Kill()
		_ = proc.Wait()
	}()
	if err := os.WriteFile(layout.PIDFile, []byte(strconv.Itoa(proc.Process.Pid)+"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(pid): %v", err)
	}
	state := doltRuntimeState{
		Running:   true,
		PID:       proc.Process.Pid,
		Port:      port,
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
	code := run([]string{"dolt-state", "stop-managed", "--city", cityPath, "--port", strconv.Itoa(port)}, &stdout, &stderr)
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
	if state.Port != port {
		t.Fatalf("state.Port = %d, want %d", state.Port, port)
	}
}

func TestDoltStateStopManagedCmdCleansStaleStateWhenNoPID(t *testing.T) {
	cityPath := t.TempDir()
	layout, err := resolveManagedDoltRuntimeLayout(cityPath)
	if err != nil {
		t.Fatalf("resolveManagedDoltRuntimeLayout: %v", err)
	}
	stalePort := reserveRandomTCPPort(t)
	if err := os.MkdirAll(filepath.Dir(layout.PIDFile), 0o755); err != nil {
		t.Fatalf("MkdirAll(runtime dir): %v", err)
	}
	if err := os.WriteFile(layout.PIDFile, []byte("999999\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(pid): %v", err)
	}
	if err := writeDoltRuntimeStateFile(layout.StateFile, doltRuntimeState{
		Running:   true,
		PID:       999999,
		Port:      stalePort,
		DataDir:   layout.DataDir,
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("writeDoltRuntimeStateFile: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"dolt-state", "stop-managed", "--city", cityPath, "--port", strconv.Itoa(stalePort)}, &stdout, &stderr)
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
	if state.Port != stalePort {
		t.Fatalf("state.Port = %d, want %d", state.Port, stalePort)
	}
}

func TestDoltStateStopManagedCmdDoesNotKillImposterPortHolder(t *testing.T) {
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

	var stdout, stderr bytes.Buffer
	code := run([]string{"dolt-state", "stop-managed", "--city", cityPath, "--port", strconv.Itoa(port)}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run() = %d, stderr = %s", code, stderr.String())
	}

	got := parseDoltStateOutput(t, stdout.String())
	if got["had_pid"] != "false" {
		t.Fatalf("had_pid = %q, want false for imposter port-holder", got["had_pid"])
	}
	if !managedStopPIDAlive(listener.Process.Pid) {
		t.Fatalf("imposter pid %d was killed by stop-managed", listener.Process.Pid)
	}
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), time.Second)
	if err != nil {
		t.Fatalf("imposter listener on %d stopped accepting connections: %v", port, err)
	}
	_ = conn.Close()

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
	if state.Port != port {
		t.Fatalf("state.Port = %d, want %d", state.Port, port)
	}
}

func TestDoltStateRecoverManagedCmdReportsReadOnlyAndRestarts(t *testing.T) {
	skipSlowCmdGCTest(t, "spawns managed dolt recovery processes; run make test-cmd-gc-process for full coverage")
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

	port := reserveRandomTCPPort(t)
	original := startTCPListenerProcessInDir(t, port, layout.DataDir)
	defer func() {
		_ = original.Process.Kill()
		_ = original.Wait()
	}()
	if err := os.WriteFile(layout.PIDFile, []byte(strconv.Itoa(original.Process.Pid)+"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(pid): %v", err)
	}
	if err := writeDoltRuntimeStateFile(layout.StateFile, doltRuntimeState{
		Running:   true,
		PID:       original.Process.Pid,
		Port:      port,
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
    config_file=$3
    port=$(awk '/port:/ {print $2; exit}' "$config_file")
    data_dir=$(awk '/data_dir:/ {print $2; exit}' "$config_file" | tr -d '"')
    exec python3 - "$port" "$data_dir" <<'INNERPY'
import os
import signal
import socket
import sys
import time

port = int(sys.argv[1])
data_dir = sys.argv[2]
if data_dir:
    os.chdir(data_dir)
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
  *"sql -r csv -q SHOW DATABASES"*)
    printf 'Database\ngascity\n'
    exit 0
    ;;
  *"CREATE TABLE IF NOT EXISTS"*"__gc_read_only_probe"*)
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
	code := run([]string{"dolt-state", "recover-managed", "--city", cityPath, "--host", "127.0.0.1", "--port", strconv.Itoa(port), "--user", "root", "--timeout-ms", "5000"}, &stdout, &stderr)
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

func TestDoltStateRecoverManagedCmdNoUserDatabaseHealthSucceeds(t *testing.T) {
	skipSlowCmdGCTest(t, "spawns managed dolt recovery processes; run make test-cmd-gc-process for full coverage")
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

	port := reserveRandomTCPPort(t)
	original := startTCPListenerProcessInDir(t, port, layout.DataDir)
	defer func() {
		_ = original.Process.Kill()
		_ = original.Wait()
	}()
	if err := os.WriteFile(layout.PIDFile, []byte(strconv.Itoa(original.Process.Pid)+"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(pid): %v", err)
	}
	if err := writeDoltRuntimeStateFile(layout.StateFile, doltRuntimeState{
		Running:   true,
		PID:       original.Process.Pid,
		Port:      port,
		DataDir:   layout.DataDir,
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("writeDoltRuntimeStateFile: %v", err)
	}

	binDir := t.TempDir()
	invocationFile := filepath.Join(t.TempDir(), "dolt-invocation.txt")
	writeFakeDoltSQLBinary(t, binDir, invocationFile, `#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$INVOCATION_FILE"
case "$*" in
  "sql-server --config "*)
    config_file=$3
    port=$(awk '/port:/ {print $2; exit}' "$config_file")
    data_dir=$(awk '/data_dir:/ {print $2; exit}' "$config_file" | tr -d '"')
    exec python3 - "$port" "$data_dir" <<'INNERPY'
import os
import signal
import socket
import sys
import time

port = int(sys.argv[1])
data_dir = sys.argv[2]
if data_dir:
    os.chdir(data_dir)
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
    printf 'cnt\n0\n'
    ;;
  *"SELECT active_branch()"*)
    exit 0
    ;;
  *"sql -r csv -q SHOW DATABASES"*)
    printf 'Database\ninformation_schema\nmysql\ndolt_cluster\nperformance_schema\nsys\n__gc_probe\n'
    exit 0
    ;;
  *"CREATE TABLE IF NOT EXISTS"*)
    echo "unexpected write probe without a user database" >&2
    exit 2
    ;;
  *)
    echo "unexpected command: $*" >&2
    exit 2
    ;;
esac
`)
	t.Setenv("INVOCATION_FILE", invocationFile)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Cleanup(func() {
		if state, err := readDoltRuntimeStateFile(layout.StateFile); err == nil && state.PID > 0 {
			_ = terminateManagedDoltPID(state.PID)
		}
	})

	var stdout, stderr bytes.Buffer
	code := run([]string{"dolt-state", "recover-managed", "--city", cityPath, "--host", "127.0.0.1", "--port", strconv.Itoa(port), "--user", "root", "--timeout-ms", "5000"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run() = %d, stdout = %s stderr = %s", code, stdout.String(), stderr.String())
	}
	got := parseDoltStateOutput(t, stdout.String())
	if got["diagnosed_read_only"] != "false" {
		t.Fatalf("diagnosed_read_only = %q, want false", got["diagnosed_read_only"])
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
	invocation, err := os.ReadFile(invocationFile)
	if err != nil {
		t.Fatalf("ReadFile(invocation): %v", err)
	}
	if strings.Contains(string(invocation), "CREATE TABLE IF NOT EXISTS") {
		t.Fatalf("recover-managed ran write probe without user database:\n%s", invocation)
	}
}

func TestRecoverManagedDoltProcessReturnsWhenConcurrentStarterBecomesReady(t *testing.T) {
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

	port := reserveRandomTCPPort(t)
	starter := startLockedDelayedTCPListenerProcessInDir(t, layout.LockFile, port, layout.DataDir, 600*time.Millisecond)
	defer func() {
		_ = starter.Process.Kill()
		_ = starter.Wait()
	}()

	if err := os.WriteFile(layout.PIDFile, []byte(strconv.Itoa(starter.Process.Pid)+"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(pid): %v", err)
	}
	if err := writeDoltRuntimeStateFile(layout.StateFile, doltRuntimeState{
		Running:   true,
		PID:       starter.Process.Pid,
		Port:      port,
		DataDir:   layout.DataDir,
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("writeDoltRuntimeStateFile: %v", err)
	}

	t.Setenv("GC_DOLT_PASSWORD", "test-password")
	oldQueryProbeDirect := managedDoltQueryProbeDirectFn
	oldReadOnlyDirect := managedDoltReadOnlyStateDirectFn
	oldConnectionCountDirect := managedDoltConnectionCountDirectFn
	managedDoltQueryProbeDirectFn = func(_, _, _ string) error { return nil }
	managedDoltReadOnlyStateDirectFn = func(_, _, _ string) (string, error) { return "false", nil }
	managedDoltConnectionCountDirectFn = func(_, _, _ string) (string, error) { return "1", nil }
	defer func() {
		managedDoltQueryProbeDirectFn = oldQueryProbeDirect
		managedDoltReadOnlyStateDirectFn = oldReadOnlyDirect
		managedDoltConnectionCountDirectFn = oldConnectionCountDirect
	}()

	report, err := recoverManagedDoltProcess(cityPath, "127.0.0.1", strconv.Itoa(port), "root", "warning", 3*time.Second)
	if err != nil {
		t.Fatalf("recoverManagedDoltProcess() error = %v", err)
	}
	if !report.HadPID {
		t.Fatalf("recoverManagedDoltProcess().HadPID = false, want true")
	}
	if !report.Ready {
		t.Fatalf("recoverManagedDoltProcess().Ready = false, want true")
	}
	if !report.Healthy {
		t.Fatalf("recoverManagedDoltProcess().Healthy = false, want true")
	}
	if report.PID != starter.Process.Pid {
		t.Fatalf("recoverManagedDoltProcess().PID = %d, want %d", report.PID, starter.Process.Pid)
	}

	probeLock, _, err := openManagedDoltLifecycleLock(cityPath)
	if err != nil {
		t.Fatalf("openManagedDoltLifecycleLock: %v", err)
	}
	locked, err := tryManagedDoltLifecycleLock(probeLock)
	if err != nil {
		_ = probeLock.Close()
		t.Fatalf("tryManagedDoltLifecycleLock: %v", err)
	}
	if locked {
		releaseManagedDoltLifecycleLock(probeLock)
		t.Fatal("recoverManagedDoltProcess() returned after concurrent starter released the lifecycle lock, want success while the concurrent starter still owns it")
	}
	_ = probeLock.Close()

	published, err := readDoltRuntimeStateFile(managedDoltStatePath(cityPath))
	if err != nil {
		t.Fatalf("read published dolt runtime state: %v", err)
	}
	if !published.Running {
		t.Fatalf("published.Running = false, want true")
	}
	if published.PID != starter.Process.Pid {
		t.Fatalf("published.PID = %d, want %d", published.PID, starter.Process.Pid)
	}
	if published.Port != port {
		t.Fatalf("published.Port = %d, want %d", published.Port, port)
	}
}

func TestRecoverManagedDoltProcessReusesHealthyManagedServerOnReboundPort(t *testing.T) {
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

	oldPort := reserveRandomTCPPort(t)
	newPort := reserveRandomTCPPort(t)
	for newPort == oldPort {
		newPort = reserveRandomTCPPort(t)
	}
	listener := startTCPListenerProcessInDir(t, newPort, layout.DataDir)
	defer func() {
		_ = listener.Process.Kill()
		_ = listener.Wait()
	}()

	if err := os.WriteFile(layout.PIDFile, []byte(strconv.Itoa(listener.Process.Pid)+"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(pid): %v", err)
	}
	state := doltRuntimeState{
		Running:   true,
		PID:       listener.Process.Pid,
		Port:      newPort,
		DataDir:   layout.DataDir,
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if err := writeDoltRuntimeStateFile(layout.StateFile, state); err != nil {
		t.Fatalf("writeDoltRuntimeStateFile(layout): %v", err)
	}
	if err := writeDoltRuntimeStateFile(managedDoltStatePath(cityPath), state); err != nil {
		t.Fatalf("writeDoltRuntimeStateFile(published): %v", err)
	}

	binDir := t.TempDir()
	invocationFile := filepath.Join(t.TempDir(), "dolt-invocation")
	writeFakeDoltSQLBinary(t, binDir, invocationFile, `#!/bin/sh
set -eu
case "${1:-}" in
  config)
    exit 0
    ;;
  *)
    printf 'dolt %s\n' "$*" >> "$INVOCATION_FILE"
    exit 1
    ;;
esac
`)
	t.Setenv("PATH", strings.Join([]string{binDir, os.Getenv("PATH")}, string(os.PathListSeparator)))
	t.Setenv("GC_DOLT_PASSWORD", "test-password")

	oldQueryProbeDirect := managedDoltQueryProbeDirectFn
	oldReadOnlyDirect := managedDoltReadOnlyStateDirectFn
	oldConnectionCountDirect := managedDoltConnectionCountDirectFn
	managedDoltQueryProbeDirectFn = func(_, port, _ string) error {
		if port != strconv.Itoa(newPort) {
			return fmt.Errorf("unexpected query probe port %s", port)
		}
		return nil
	}
	managedDoltReadOnlyStateDirectFn = func(_, port, _ string) (string, error) {
		if port != strconv.Itoa(newPort) {
			return "", fmt.Errorf("unexpected read-only probe port %s", port)
		}
		return "false", nil
	}
	managedDoltConnectionCountDirectFn = func(_, port, _ string) (string, error) {
		if port != strconv.Itoa(newPort) {
			return "", fmt.Errorf("unexpected connection-count probe port %s", port)
		}
		return "1", nil
	}
	defer func() {
		managedDoltQueryProbeDirectFn = oldQueryProbeDirect
		managedDoltReadOnlyStateDirectFn = oldReadOnlyDirect
		managedDoltConnectionCountDirectFn = oldConnectionCountDirect
	}()

	report, err := recoverManagedDoltProcess(cityPath, "127.0.0.1", strconv.Itoa(oldPort), "root", "warning", 2*time.Second)
	if err != nil {
		t.Fatalf("recoverManagedDoltProcess() error = %v", err)
	}
	if !report.HadPID {
		t.Fatalf("recoverManagedDoltProcess().HadPID = false, want true")
	}
	if !report.Ready {
		t.Fatalf("recoverManagedDoltProcess().Ready = false, want true")
	}
	if !report.Healthy {
		t.Fatalf("recoverManagedDoltProcess().Healthy = false, want true")
	}
	if report.PID != listener.Process.Pid {
		t.Fatalf("recoverManagedDoltProcess().PID = %d, want %d", report.PID, listener.Process.Pid)
	}
	if report.Port != newPort {
		t.Fatalf("recoverManagedDoltProcess().Port = %d, want %d", report.Port, newPort)
	}
	if !managedStopPIDAlive(listener.Process.Pid) {
		t.Fatalf("listener pid %d no longer alive after recovery", listener.Process.Pid)
	}
	if invocation, err := os.ReadFile(invocationFile); err == nil && strings.TrimSpace(string(invocation)) != "" {
		t.Fatalf("recoverManagedDoltProcess() unexpectedly launched dolt:\n%s", string(invocation))
	}
	published, err := readDoltRuntimeStateFile(managedDoltStatePath(cityPath))
	if err != nil {
		t.Fatalf("read published dolt runtime state: %v", err)
	}
	if published.PID != listener.Process.Pid || published.Port != newPort {
		t.Fatalf("published state = %+v, want pid=%d port=%d", published, listener.Process.Pid, newPort)
	}
}

func TestDoltStateRecoverManagedCmdClearsPublishedStateWhenPreflightCleanupFails(t *testing.T) {
	skipSlowCmdGCTest(t, "spawns managed dolt recovery processes; run make test-cmd-gc-process for full coverage")
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

	port := reserveRandomTCPPort(t)
	original := startTCPListenerProcessInDir(t, port, layout.DataDir)
	defer func() {
		_ = original.Process.Kill()
		_ = original.Wait()
	}()
	if err := os.WriteFile(layout.PIDFile, []byte(strconv.Itoa(original.Process.Pid)+"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(pid): %v", err)
	}
	state := doltRuntimeState{
		Running:   true,
		PID:       original.Process.Pid,
		Port:      port,
		DataDir:   layout.DataDir,
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if err := writeDoltRuntimeStateFile(layout.StateFile, state); err != nil {
		t.Fatalf("writeDoltRuntimeStateFile(layout): %v", err)
	}
	if err := writeDoltRuntimeStateFile(managedDoltStatePath(cityPath), state); err != nil {
		t.Fatalf("writeDoltRuntimeStateFile(published): %v", err)
	}

	oldPreflight := managedDoltPreflightCleanupFn
	managedDoltPreflightCleanupFn = func(string) error {
		return errors.New("preflight cleanup failed")
	}
	defer func() { managedDoltPreflightCleanupFn = oldPreflight }()

	var stdout, stderr bytes.Buffer
	code := run([]string{"dolt-state", "recover-managed", "--city", cityPath, "--host", "127.0.0.1", "--port", strconv.Itoa(port), "--user", "root", "--timeout-ms", "5000"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("run() = %d, want 1; stdout = %s stderr = %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "preflight cleanup failed") {
		t.Fatalf("stderr = %q, want preflight cleanup failure", stderr.String())
	}
	if managedStopPIDAlive(original.Process.Pid) {
		t.Fatalf("original pid %d still alive after failed recovery", original.Process.Pid)
	}
	stateAfter, err := readDoltRuntimeStateFile(layout.StateFile)
	if err != nil {
		t.Fatalf("readDoltRuntimeStateFile(after failure): %v", err)
	}
	if stateAfter.Running {
		t.Fatalf("stateAfter.Running = true after failed preflight cleanup: %+v", stateAfter)
	}
	if stateAfter.PID != 0 {
		t.Fatalf("stateAfter.PID = %d, want 0 after failed preflight cleanup", stateAfter.PID)
	}
	if stateAfter.Port != port {
		t.Fatalf("stateAfter.Port = %d, want %d after failed preflight cleanup", stateAfter.Port, port)
	}
	if _, err := os.Stat(layout.PIDFile); !os.IsNotExist(err) {
		t.Fatalf("pid file still present after failed preflight cleanup: err=%v", err)
	}
	if _, err := os.Stat(managedDoltStatePath(cityPath)); !os.IsNotExist(err) {
		t.Fatalf("published managed state still present after failed preflight cleanup: err=%v", err)
	}
}

func TestDoltStateRecoverManagedCmdFailsWhenPostStartHealthFails(t *testing.T) {
	skipSlowCmdGCTest(t, "spawns managed dolt recovery processes; run make test-cmd-gc-process for full coverage")
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

	port := reserveRandomTCPPort(t)
	original := startTCPListenerProcessInDir(t, port, layout.DataDir)
	defer func() {
		_ = original.Process.Kill()
		_ = original.Wait()
	}()
	if err := os.WriteFile(layout.PIDFile, []byte(strconv.Itoa(original.Process.Pid)+"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(pid): %v", err)
	}
	if err := writeDoltRuntimeStateFile(layout.StateFile, doltRuntimeState{
		Running:   true,
		PID:       original.Process.Pid,
		Port:      port,
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
    config_file=$3
    port=$(awk '/port:/ {print $2; exit}' "$config_file")
    data_dir=$(awk '/data_dir:/ {print $2; exit}' "$config_file" | tr -d '"')
    exec python3 - "$port" "$data_dir" <<'INNERPY'
import os
import signal
import socket
import sys
import time

port = int(sys.argv[1])
data_dir = sys.argv[2]
if data_dir:
    os.chdir(data_dir)
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
    if [ "$count" -le 4 ]; then
      exit 0
    fi
    echo "final health probe failed" >&2
    exit 1
    ;;
  *"sql -r csv -q SHOW DATABASES"*)
    printf 'Database\ngascity\n'
    exit 0
    ;;
  *"CREATE TABLE IF NOT EXISTS"*"__gc_read_only_probe"*)
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
	code := run([]string{"dolt-state", "recover-managed", "--city", cityPath, "--host", "127.0.0.1", "--port", strconv.Itoa(port), "--user", "root", "--timeout-ms", "5000"}, &stdout, &stderr)
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
	failedPID, err := strconv.Atoi(got["pid"])
	if err != nil {
		t.Fatalf("parse pid %q: %v", got["pid"], err)
	}
	if failedPID <= 0 {
		t.Fatalf("pid = %q, want replacement pid", got["pid"])
	}
	if managedStopPIDAlive(failedPID) {
		t.Fatalf("replacement pid %d still alive after failed recovery", failedPID)
	}
	state, err := readDoltRuntimeStateFile(layout.StateFile)
	if err != nil {
		t.Fatalf("readDoltRuntimeStateFile(after failure): %v", err)
	}
	if state.Running {
		t.Fatalf("state.Running = true after failed recovery: %+v", state)
	}
	if state.PID != 0 {
		t.Fatalf("state.PID = %d, want 0 after failed recovery", state.PID)
	}
	if _, err := os.Stat(layout.PIDFile); !os.IsNotExist(err) {
		t.Fatalf("pid file still present after failed recovery: err=%v", err)
	}
	if _, err := os.Stat(managedDoltStatePath(cityPath)); !os.IsNotExist(err) {
		t.Fatalf("published managed state still present after failed recovery: err=%v", err)
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
