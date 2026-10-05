package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// launchdStartFixture writes a plist whose ProgramArguments launch
// plistBinary and stubs launchctl so a kickstart brings the supervisor up.
func launchdStartFixture(t *testing.T, plistBinary string, launchctlErr error) (plistPath string, calls *[]string) {
	t.Helper()
	plistPath = filepath.Join(t.TempDir(), "com.gascity.supervisor.plist")
	content := "<plist><dict><key>ProgramArguments</key><array><string>" + plistBinary + "</string><string>supervisor</string><string>run</string></array></dict></plist>"
	if err := os.WriteFile(plistPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	recorded := []string{}
	started := false
	oldRun, oldAlive := supervisorLaunchctlRun, supervisorAliveHook
	t.Cleanup(func() { supervisorLaunchctlRun, supervisorAliveHook = oldRun, oldAlive })
	supervisorLaunchctlRun = func(args ...string) error {
		recorded = append(recorded, strings.Join(args, " "))
		if launchctlErr != nil && args[0] == "load" {
			return launchctlErr
		}
		if args[0] == "kickstart" {
			started = true
		}
		return nil
	}
	supervisorAliveHook = func() int {
		if started {
			return 4321
		}
		return 0
	}
	t.Setenv("GC_HOME", t.TempDir())
	return plistPath, &recorded
}

func fakeGCBinary(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gc")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestStartSupervisorViaInstalledLaunchdStartsJob(t *testing.T) {
	gcPath := fakeGCBinary(t)
	plistPath, calls := launchdStartFixture(t, gcPath, nil)

	var stdout, stderr bytes.Buffer
	handled, code := startSupervisorViaInstalledLaunchd(plistPath, gcPath, &stdout, &stderr, false)
	if !handled || code != 0 {
		t.Fatalf("handled=%v code=%d stderr=%s, want launchd start", handled, code, stderr.String())
	}
	joined := strings.Join(*calls, "\n")
	for _, want := range []string{"unload " + plistPath, "load " + plistPath, "kickstart -p gui/"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("launchctl calls missing %q:\n%s", want, joined)
		}
	}
	if !strings.Contains(stdout.String(), "Supervisor started (PID 4321)") {
		t.Fatalf("stdout = %q, want started message", stdout.String())
	}
}

func TestStartSupervisorViaInstalledLaunchdNoPlistFallsBack(t *testing.T) {
	_, calls := launchdStartFixture(t, "/unused/gc", nil)

	var stdout, stderr bytes.Buffer
	handled, _ := startSupervisorViaInstalledLaunchd(filepath.Join(t.TempDir(), "missing.plist"), fakeGCBinary(t), &stdout, &stderr, false)
	if handled {
		t.Fatal("handled = true with no plist installed, want fork fallback")
	}
	if len(*calls) != 0 || stderr.Len() != 0 {
		t.Fatalf("calls=%v stderr=%q, want a silent fallback", *calls, stderr.String())
	}
}

// A plist installed from another gc installation must not be started by this
// gc: that would silently run the other binary.
func TestStartSupervisorViaInstalledLaunchdForeignBinaryFallsBack(t *testing.T) {
	other := fakeGCBinary(t)
	plistPath, calls := launchdStartFixture(t, other, nil)
	gcPath := fakeGCBinary(t)

	var stdout, stderr bytes.Buffer
	handled, _ := startSupervisorViaInstalledLaunchd(plistPath, gcPath, &stdout, &stderr, false)
	if handled {
		t.Fatal("handled = true for a plist that runs a different gc binary")
	}
	if len(*calls) != 0 {
		t.Fatalf("launchctl called for a foreign plist: %v", *calls)
	}
	for _, want := range []string{"runs " + other, "supervisor install --force"} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("stderr missing %q:\n%s", want, stderr.String())
		}
	}
}

// Without a GUI session (SSH, CI) launchctl cannot start the job; gc
// supervisor start must fall back to the fork, as before, with a warning.
func TestStartSupervisorViaInstalledLaunchdFallsBackWhenLaunchctlFails(t *testing.T) {
	gcPath := fakeGCBinary(t)
	plistPath, _ := launchdStartFixture(t, gcPath, errors.New("Load failed: 5: Input/output error"))

	var stdout, stderr bytes.Buffer
	handled, _ := startSupervisorViaInstalledLaunchd(plistPath, gcPath, &stdout, &stderr, false)
	if handled {
		t.Fatal("handled = true after a launchctl failure, want the fork fallback")
	}
	for _, want := range []string{"Input/output error", "starting the supervisor outside launchd instead", supervisorOmitProviderCredsEnv} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("stderr missing %q:\n%s", want, stderr.String())
		}
	}
}

func TestStartSupervisorViaInstalledLaunchdUnparseablePlistFallsBack(t *testing.T) {
	_, calls := launchdStartFixture(t, "/unused/gc", nil)
	plistPath := filepath.Join(t.TempDir(), "broken.plist")
	if err := os.WriteFile(plistPath, []byte("<plist><dict></dict></plist>"), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	handled, _ := startSupervisorViaInstalledLaunchd(plistPath, fakeGCBinary(t), &stdout, &stderr, false)
	if handled || len(*calls) != 0 {
		t.Fatalf("handled=%v calls=%v, want an unparseable plist treated as foreign", handled, *calls)
	}
	if !strings.Contains(stderr.String(), "(unreadable ProgramArguments)") {
		t.Fatalf("stderr = %q, want unparseable-plist warning", stderr.String())
	}
}

// Covers the macOS wiring in gc supervisor start: with this binary's plist
// installed, it starts the launchd job and never forks.
func TestDoSupervisorStartOnDarwinStartsInstalledLaunchdJob(t *testing.T) {
	pinRealHome(t)
	t.Setenv("GC_HOME", shortTempDir(t, "gc-home-"))
	t.Setenv("XDG_RUNTIME_DIR", shortTempDir(t, "gc-run-"))
	t.Setenv(supervisorSystemdUnitEnv, "")
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	plistPath, calls := launchdStartFixture(t, self, nil)
	oldGOOS, oldPlist := supervisorRuntimeGOOS, supervisorStartLaunchdPlistPath
	t.Cleanup(func() { supervisorRuntimeGOOS, supervisorStartLaunchdPlistPath = oldGOOS, oldPlist })
	supervisorRuntimeGOOS = "darwin"
	supervisorStartLaunchdPlistPath = func() string { return plistPath }

	var stdout, stderr bytes.Buffer
	if code := doSupervisorStart(&stdout, &stderr); code != 0 {
		t.Fatalf("doSupervisorStart = %d; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(strings.Join(*calls, "\n"), "kickstart -p gui/") {
		t.Fatalf("launchctl calls = %v, want the installed job kickstarted", *calls)
	}
	if !strings.Contains(stdout.String(), "Supervisor started (PID 4321)") {
		t.Fatalf("stdout = %q, want started message from the launchd job", stdout.String())
	}
	if strings.Contains(stderr.String(), "outside launchd") {
		t.Fatalf("supervisor was forked outside launchd:\n%s", stderr.String())
	}
}
