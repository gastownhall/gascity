//go:build unix

package main

// systemd warm refresh and process-group self-protection are both POSIX
// mechanisms: the sentinel is placed in its own process group with Setpgid,
// and the refusal under test is 'do not signal my own group'.
import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/processgroup/processgrouptest"
	"github.com/gastownhall/gascity/internal/supervisor"
)

func startWorkspaceServiceSentinel(t *testing.T, gcHome, cityPath, serviceName string) workspaceServiceSentinel {
	t.Helper()
	processgrouptest.RequireRealProcessSignals(t)

	stateRoot := filepath.Join(cityPath, ".gc", "services", serviceName)
	socketPath := filepath.Join(t.TempDir(), serviceName+".sock")
	cmd := exec.Command("sh", "-c", "trap 'exit 0' TERM; while :; do sleep 1; done")
	cmd.Env = append(os.Environ(),
		"GC_HOME="+gcHome,
		"GC_CITY_PATH="+cityPath,
		"GC_SERVICE_NAME="+serviceName,
		"GC_SERVICE_STATE_ROOT="+stateRoot,
		"GC_SERVICE_SOCKET="+socketPath,
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start workspace-service sentinel %q: %v", serviceName, err)
	}
	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil {
		t.Fatalf("Getpgid(%d): %v", cmd.Process.Pid, err)
	}
	waitCh := make(chan error, 1)
	go func() {
		waitCh <- cmd.Wait()
	}()
	t.Cleanup(func() {
		if processGroupAlive(pgid) {
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
		}
		select {
		case <-waitCh:
		case <-time.After(time.Second):
			t.Logf("workspace-service sentinel pgid %d did not exit before cleanup timeout", pgid)
		}
	})
	if !processGroupAlive(pgid) {
		t.Fatalf("workspace-service sentinel pgid %d is not alive", pgid)
	}
	return workspaceServiceSentinel{pgid: pgid}
}

func TestInstallSupervisorSystemdWarmRefreshStopsWorkspaceServicesBeforeStart(t *testing.T) {
	processgrouptest.RequireRealProcessSignals(t)

	if goruntime.GOOS != "linux" {
		t.Skip("systemd path only applies on linux")
	}
	homeDir := t.TempDir()
	gcHome := filepath.Join(t.TempDir(), "gc-home")
	t.Setenv("HOME", homeDir)
	t.Setenv("GC_HOME", gcHome)

	data := &supervisorServiceData{
		GCPath:        "/tmp/gc-new",
		LogPath:       "/tmp/gc-home/supervisor.log",
		GCHome:        gcHome,
		XDGRuntimeDir: "/tmp/gc-run",
		Path:          "/usr/local/bin:/usr/bin:/bin",
	}
	path := supervisorSystemdServicePath()
	unitName := supervisorSystemdServiceName()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("old unit\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cityPath := filepath.Join(t.TempDir(), "city")
	if err := os.MkdirAll(cityPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := supervisor.NewRegistry(supervisor.RegistryPath()).Register(cityPath, "bright-lights"); err != nil {
		t.Fatalf("Register(%q): %v", cityPath, err)
	}
	stateRoot := filepath.Join(cityPath, ".gc", "services", "bridge")
	socketPath := filepath.Join(t.TempDir(), "bridge.sock")
	cmd := exec.Command("sh", "-c", "trap 'exit 0' TERM; while :; do sleep 1; done")
	cmd.Env = append(os.Environ(),
		"GC_HOME="+gcHome,
		"GC_CITY_PATH="+cityPath,
		"GC_SERVICE_NAME=bridge",
		"GC_SERVICE_STATE_ROOT="+stateRoot,
		"GC_SERVICE_SOCKET="+socketPath,
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start workspace-service sentinel: %v", err)
	}
	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil {
		t.Fatalf("Getpgid(%d): %v", cmd.Process.Pid, err)
	}
	waitCh := make(chan error, 1)
	go func() {
		waitCh <- cmd.Wait()
	}()
	t.Cleanup(func() {
		if processGroupAlive(pgid) {
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
		}
		select {
		case <-waitCh:
		case <-time.After(time.Second):
			t.Logf("workspace-service sentinel pgid %d did not exit before cleanup timeout", pgid)
		}
	})
	if !processGroupAlive(pgid) {
		t.Fatalf("workspace-service sentinel pgid %d is not alive before warm refresh", pgid)
	}
	scope, err := supervisorWorkspaceServiceCleanupScopeFromRegistry(gcHome)
	if err != nil {
		t.Fatalf("supervisorWorkspaceServiceCleanupScopeFromRegistry: %v", err)
	}
	procs, err := findSupervisorWorkspaceServiceProcesses(scope)
	if err != nil {
		t.Fatalf("findSupervisorWorkspaceServiceProcesses: %v", err)
	}
	if !slices.ContainsFunc(procs, func(proc supervisorWorkspaceServiceProcess) bool { return proc.pgid == pgid }) {
		t.Fatalf("workspace-service discovery procs = %#v, want pgid %d", procs, pgid)
	}

	oldRun := supervisorSystemctlRun
	oldActive := supervisorSystemctlActive
	var (
		calls              []string
		startBeforeCleanup bool
	)
	supervisorSystemctlRun = func(args ...string) error {
		call := strings.Join(args, " ")
		calls = append(calls, call)
		if call == "--user start "+unitName && processGroupAlive(pgid) {
			startBeforeCleanup = true
		}
		return nil
	}
	supervisorSystemctlActive = func(service string) bool {
		if service != unitName {
			return false
		}
		for _, call := range calls {
			if call == "--user kill --kill-who=main --signal=SIGTERM "+unitName {
				return false
			}
		}
		return true
	}
	stubSupervisorRunningPreserveSignalReady(t, true)
	t.Cleanup(func() {
		supervisorSystemctlRun = oldRun
		supervisorSystemctlActive = oldActive
	})

	var stdout, stderr bytes.Buffer
	if code := installSupervisorSystemd(data, &stdout, &stderr); code != 0 {
		t.Fatalf("installSupervisorSystemd code = %d, want 0; stderr=%q", code, stderr.String())
	}
	if startBeforeCleanup {
		t.Fatalf("systemctl start ran before workspace-service pgid %d was stopped; calls=%v", pgid, calls)
	}
	if err := waitForProcessGroupExit(pgid, time.Second); err != nil {
		t.Fatalf("workspace-service cleanup: %v", err)
	}
}

func TestInstallSupervisorSystemdWarmRefreshLeavesUnregisteredWorkspaceServices(t *testing.T) {
	if goruntime.GOOS != "linux" {
		t.Skip("systemd path only applies on linux")
	}
	homeDir := t.TempDir()
	gcHome := filepath.Join(t.TempDir(), "gc-home")
	t.Setenv("HOME", homeDir)
	t.Setenv("GC_HOME", gcHome)

	registeredCity := filepath.Join(t.TempDir(), "registered-city")
	unregisteredCity := filepath.Join(t.TempDir(), "unregistered-city")
	if err := os.MkdirAll(registeredCity, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(unregisteredCity, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := supervisor.NewRegistry(supervisor.RegistryPath()).Register(registeredCity, "registered-city"); err != nil {
		t.Fatalf("Register(%q): %v", registeredCity, err)
	}

	data := &supervisorServiceData{
		GCPath:        "/tmp/gc-new",
		LogPath:       "/tmp/gc-home/supervisor.log",
		GCHome:        gcHome,
		XDGRuntimeDir: "/tmp/gc-run",
		Path:          "/usr/local/bin:/usr/bin:/bin",
	}
	path := supervisorSystemdServicePath()
	unitName := supervisorSystemdServiceName()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("old unit\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	registered := startWorkspaceServiceSentinel(t, gcHome, registeredCity, "bridge")
	unregistered := startWorkspaceServiceSentinel(t, gcHome, unregisteredCity, "other-bridge")

	oldRun := supervisorSystemctlRun
	oldActive := supervisorSystemctlActive
	var calls []string
	supervisorSystemctlRun = func(args ...string) error {
		calls = append(calls, strings.Join(args, " "))
		return nil
	}
	supervisorSystemctlActive = func(service string) bool {
		if service != unitName {
			return false
		}
		for _, call := range calls {
			if call == "--user kill --kill-who=main --signal=SIGTERM "+unitName {
				return false
			}
		}
		return true
	}
	stubSupervisorRunningPreserveSignalReady(t, true)
	t.Cleanup(func() {
		supervisorSystemctlRun = oldRun
		supervisorSystemctlActive = oldActive
	})

	var stdout, stderr bytes.Buffer
	if code := installSupervisorSystemd(data, &stdout, &stderr); code != 0 {
		t.Fatalf("installSupervisorSystemd code = %d, want 0; stderr=%q", code, stderr.String())
	}
	if err := waitForProcessGroupExit(registered.pgid, time.Second); err != nil {
		t.Fatalf("registered workspace-service cleanup: %v", err)
	}
	if !processGroupAlive(unregistered.pgid) {
		t.Fatalf("unregistered workspace-service pgid %d was stopped by warm-refresh cleanup", unregistered.pgid)
	}
}

func TestTerminateProcessGroupRefusesCurrentProcessGroup(t *testing.T) {
	if err := terminateProcessGroup(syscall.Getpgrp(), time.Millisecond); err == nil {
		t.Fatal("terminateProcessGroup current process group error = nil, want refusal")
	}
}
