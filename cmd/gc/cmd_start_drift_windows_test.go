//go:build windows

package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

const testSupervisorSignalTermSocketCommand = "signal_term"

func TestSpawnDetachedSupervisorWindowsCarriesPreserveSessionsOnSignal(t *testing.T) {
	t.Setenv("GC_HOME", shortTempDir(t, "gc-home-"))
	snapshot := filepath.Join(shortTempDir(t, "drift-env-"), "supervisor.env")
	t.Setenv("GC_TEST_SUPERVISOR_ENV_SNAPSHOT", snapshot)
	t.Setenv("GC_TEST_SUPERVISOR_ENV_CONTROL", "kept")
	t.Setenv(supervisorPreserveSessionsOnSignalEnv, "0")
	for _, key := range managedDoltSessionScopedEnvKeys {
		t.Setenv(key, "stamped")
	}
	script := filepath.Join(shortTempDir(t, "drift-env-script-"), "write-env.ps1")
	scriptBody := fmt.Sprintf(`[Environment]::GetEnvironmentVariables('Process').GetEnumerator() | ForEach-Object { "$($_.Name)=$($_.Value)" } | Set-Content -LiteralPath '%s'
`, strings.ReplaceAll(snapshot, "'", "''"))
	if err := os.WriteFile(script, []byte(scriptBody), 0o600); err != nil {
		t.Fatalf("writing env snapshot script: %v", err)
	}

	powershell := requireWindowsPowerShell(t)
	if err := spawnDetachedSupervisor(powershell, "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", script); err != nil {
		t.Fatalf("spawnDetachedSupervisor: %v", err)
	}
	env := readDetachedSupervisorEnvSnapshot(t, snapshot)
	for _, key := range managedDoltSessionScopedEnvKeys {
		if value, ok := env[key]; ok {
			t.Errorf("detached supervisor inherited %s=%q from the spawning session", key, value)
		}
	}
	if got := env[supervisorPreserveSessionsOnSignalEnv]; got != "1" {
		t.Fatalf("detached supervisor %s = %q, want 1", supervisorPreserveSessionsOnSignalEnv, got)
	}
	if got := env["GC_TEST_SUPERVISOR_ENV_CONTROL"]; got != "kept" {
		t.Errorf("detached supervisor lost unrelated env; GC_TEST_SUPERVISOR_ENV_CONTROL=%q", got)
	}
}

func readDetachedSupervisorEnvSnapshot(t *testing.T, path string) map[string]string {
	t.Helper()
	deadline := time.After(10 * time.Second)
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		data, err := os.ReadFile(path)
		if err == nil {
			env := map[string]string{}
			for _, line := range strings.Split(string(data), "\n") {
				if key, value, ok := strings.Cut(strings.TrimRight(line, "\r"), "="); ok && key != "" {
					env[key] = value
				}
			}
			return env
		}
		select {
		case <-deadline:
			t.Fatalf("supervisor child did not write its environment: %v", err)
		case <-tick.C:
		}
	}
}

func TestReadSupervisorExePathWindowsReturnsOwnImagePath(t *testing.T) {
	got, err := readSupervisorExePath(os.Getpid())
	if err != nil {
		t.Fatalf("readSupervisorExePath(self): %v", err)
	}
	want, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	assertSameWindowsPath(t, got, want)
}

func TestReadSupervisorExePathWindowsUsesLaunchPathAfterRenameAside(t *testing.T) {
	source := requireWindowsPowerShell(t)
	dir := t.TempDir()
	launchPath := filepath.Join(dir, "drift-helper.exe")
	copyFileForWindowsDriftTest(t, source, launchPath)

	cmd, done := startWindowsSleepProcess(t, launchPath)
	t.Cleanup(func() { cleanupWindowsDriftHelper(cmd, done) })
	asidePath := filepath.Join(dir, "drift-helper.exe.old")
	if err := os.Rename(launchPath, asidePath); err != nil {
		t.Fatalf("renaming running helper aside: %v", err)
	}
	copyFileForWindowsDriftTest(t, source, launchPath)

	got, err := readSupervisorExePath(cmd.Process.Pid)
	if err != nil {
		t.Fatalf("readSupervisorExePath(helper): %v", err)
	}
	assertSameWindowsPath(t, got, launchPath)
}

func TestReadSupervisorExePathWindowsRefusesMissingLaunchPathAfterRenameAside(t *testing.T) {
	source := requireWindowsPowerShell(t)
	dir := t.TempDir()
	launchPath := filepath.Join(dir, "drift-helper.exe")
	copyFileForWindowsDriftTest(t, source, launchPath)

	cmd, done := startWindowsSleepProcess(t, launchPath)
	t.Cleanup(func() { cleanupWindowsDriftHelper(cmd, done) })
	asidePath := filepath.Join(dir, "drift-helper.exe.old")
	if err := os.Rename(launchPath, asidePath); err != nil {
		t.Fatalf("renaming running helper aside: %v", err)
	}

	got, err := readSupervisorExePath(cmd.Process.Pid)
	if err == nil {
		t.Fatalf("readSupervisorExePath returned %q after launch path disappeared; want error", got)
	}
	for _, want := range []string{launchPath, filepath.Base(asidePath), "cannot be inspected"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %v, want detail %q", err, want)
		}
	}
}

func TestReadSupervisorExePathWindowsFailsWhenCommandLineUnreadable(t *testing.T) {
	old := readSupervisorCommandLineHook
	readSupervisorCommandLineHook = func(int) (string, error) {
		return "", os.ErrPermission
	}
	t.Cleanup(func() { readSupervisorCommandLineHook = old })

	got, err := readSupervisorExePath(os.Getpid())
	if err == nil {
		t.Fatalf("readSupervisorExePath succeeded with unreadable command line: %q", got)
	}
	if !strings.Contains(err.Error(), "command line") {
		t.Fatalf("error = %v, want command-line detail", err)
	}
}

func TestReadSupervisorExePathWindowsAllowsRelativeArgv0MatchingImage(t *testing.T) {
	imagePath, err := querySupervisorProcessImagePath(os.Getpid())
	if err != nil {
		t.Fatalf("querySupervisorProcessImagePath(self): %v", err)
	}
	base := filepath.Base(imagePath)
	old := readSupervisorCommandLineHook
	readSupervisorCommandLineHook = func(int) (string, error) {
		return base + " supervisor run", nil
	}
	t.Cleanup(func() { readSupervisorCommandLineHook = old })

	got, err := readSupervisorExePath(os.Getpid())
	if err != nil {
		t.Fatalf("readSupervisorExePath(relative matching argv0): %v", err)
	}
	assertSameWindowsPath(t, got, imagePath)
}

func TestSupervisorRelativeArgv0MatchesImageAddsExe(t *testing.T) {
	if !supervisorRelativeArgv0MatchesImage(`C:\tools\gc.exe`, "gc") {
		t.Fatal("relative argv0 without extension did not match image with .exe")
	}
	if supervisorRelativeArgv0MatchesImage(`C:\tools\gc.exe.old`, "gc") {
		t.Fatal("relative argv0 matched a renamed-aside image")
	}
}

func TestReadSupervisorExePathWindowsRejectsRelativeArgv0DifferentFromImage(t *testing.T) {
	imagePath, err := querySupervisorProcessImagePath(os.Getpid())
	if err != nil {
		t.Fatalf("querySupervisorProcessImagePath(self): %v", err)
	}
	old := readSupervisorCommandLineHook
	readSupervisorCommandLineHook = func(int) (string, error) {
		return "definitely-not-" + filepath.Base(imagePath) + " supervisor run", nil
	}
	t.Cleanup(func() { readSupervisorCommandLineHook = old })

	got, err := readSupervisorExePath(os.Getpid())
	if err == nil {
		t.Fatalf("readSupervisorExePath accepted mismatched relative argv0: %q", got)
	}
	if !strings.Contains(err.Error(), filepath.Base(imagePath)) {
		t.Fatalf("error = %v, want image name detail", err)
	}
}

func TestDefaultRestartHelpersWindowsKillRequestsSignalTermSocketShutdown(t *testing.T) {
	gcHome := shortTempDir(t, "gc-home-")
	runtimeDir := shortTempDir(t, "gc-run-")
	t.Setenv("GC_HOME", gcHome)
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)

	supervisorPID := os.Getpid()
	var signalRequested atomic.Bool
	var stopRequested atomic.Bool
	startTestSupervisorSocket(t, filepath.Join(gcHome, "supervisor.sock"), func(cmd string) string {
		switch cmd {
		case "ping":
			return strconv.Itoa(supervisorPID) + "\n"
		case testSupervisorSignalTermSocketCommand:
			signalRequested.Store(true)
			return "ok\n"
		case "stop":
			stopRequested.Store(true)
			return "ok\n"
		}
		return ""
	})

	helpers := defaultRestartHelpers()
	if err := helpers.Kill(supervisorPID); err != nil {
		t.Fatalf("default restart Kill(%d): %v", supervisorPID, err)
	}
	if !signalRequested.Load() {
		t.Fatal("default restart Kill did not send the signal-equivalent shutdown request")
	}
	if stopRequested.Load() {
		t.Fatal("default restart Kill fell back to destructive stop against a current supervisor")
	}
}

func TestDefaultRestartHelpersWindowsKillFallsBackForOldSupervisor(t *testing.T) {
	gcHome := shortTempDir(t, "gc-home-")
	runtimeDir := shortTempDir(t, "gc-run-")
	t.Setenv("GC_HOME", gcHome)
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)

	oldUnload := unloadSupervisorServiceHook
	unloadSupervisorServiceHook = func() error { return nil }
	t.Cleanup(func() { unloadSupervisorServiceHook = oldUnload })

	var stdout bytes.Buffer
	oldNotice := driftRestartNoticeWriter
	driftRestartNoticeWriter = &stdout
	t.Cleanup(func() { driftRestartNoticeWriter = oldNotice })

	supervisorPID := os.Getpid()
	var signalRequested atomic.Bool
	var stopRequested atomic.Bool
	startTestSupervisorSocket(t, filepath.Join(gcHome, "supervisor.sock"), func(cmd string) string {
		switch cmd {
		case "ping":
			return strconv.Itoa(supervisorPID) + "\n"
		case testSupervisorSignalTermSocketCommand:
			signalRequested.Store(true)
			return ""
		case "stop":
			stopRequested.Store(true)
			return "ok\n"
		}
		return ""
	})

	if err := defaultRestartHelpers().Kill(supervisorPID); err != nil {
		t.Fatalf("default restart Kill(%d): %v", supervisorPID, err)
	}
	if !signalRequested.Load() || !stopRequested.Load() {
		t.Fatalf("signal requested = %v, stop requested = %v; want both", signalRequested.Load(), stopRequested.Load())
	}
	if !strings.Contains(stdout.String(), "predates session-preserving restart") {
		t.Fatalf("notice = %q, want compatibility warning", stdout.String())
	}
}

func TestDefaultRestartHelpersWindowsKillDoesNotFallbackAfterUnexpectedSignalAck(t *testing.T) {
	gcHome := shortTempDir(t, "gc-home-")
	runtimeDir := shortTempDir(t, "gc-run-")
	t.Setenv("GC_HOME", gcHome)
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)

	supervisorPID := os.Getpid()
	var signalRequested atomic.Bool
	var stopRequested atomic.Bool
	startTestSupervisorSocket(t, filepath.Join(gcHome, "supervisor.sock"), func(cmd string) string {
		switch cmd {
		case "ping":
			return strconv.Itoa(supervisorPID) + "\n"
		case testSupervisorSignalTermSocketCommand:
			signalRequested.Store(true)
			return "not-ok\n"
		case "stop":
			stopRequested.Store(true)
			return "ok\n"
		}
		return ""
	})

	err := defaultRestartHelpers().Kill(supervisorPID)
	if err == nil {
		t.Fatal("default restart Kill succeeded after an unexpected signal_term acknowledgment")
	}
	if !signalRequested.Load() {
		t.Fatal("default restart Kill did not send the signal-equivalent shutdown request")
	}
	if stopRequested.Load() {
		t.Fatal("default restart Kill fell back to destructive stop after a current supervisor replied unexpectedly")
	}
	if !strings.Contains(err.Error(), "unexpected acknowledgment") {
		t.Fatalf("error = %v, want unexpected acknowledgment detail", err)
	}
}

func TestDefaultRestartHelpersWindowsKillDoesNotFallbackAfterSignalAckTimeout(t *testing.T) {
	gcHome := shortTempDir(t, "gc-home-")
	runtimeDir := shortTempDir(t, "gc-run-")
	t.Setenv("GC_HOME", gcHome)
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)

	oldTimeout := supervisorSocketAckTimeout
	supervisorSocketAckTimeout = 50 * time.Millisecond
	t.Cleanup(func() { supervisorSocketAckTimeout = oldTimeout })

	supervisorPID := os.Getpid()
	var signalRequested atomic.Bool
	var stopRequested atomic.Bool
	releaseSignalConn := make(chan struct{})
	startBlockingSignalTermSupervisorSocket(t, filepath.Join(gcHome, "supervisor.sock"), supervisorPID, &signalRequested, &stopRequested, releaseSignalConn)
	t.Cleanup(func() { close(releaseSignalConn) })

	err := defaultRestartHelpers().Kill(supervisorPID)
	if err == nil {
		t.Fatal("default restart Kill succeeded after signal_term acknowledgment timed out")
	}
	if !signalRequested.Load() {
		t.Fatal("default restart Kill did not send the signal-equivalent shutdown request")
	}
	if stopRequested.Load() {
		t.Fatal("default restart Kill fell back to destructive stop after the current supervisor did not acknowledge before the deadline")
	}
}

func TestDefaultRestartHelpersWindowsKillRefusesMismatchedSupervisorSocket(t *testing.T) {
	gcHome := shortTempDir(t, "gc-home-")
	runtimeDir := shortTempDir(t, "gc-run-")
	t.Setenv("GC_HOME", gcHome)
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)

	const requestedPID = 4242
	var stopRequested atomic.Bool
	startTestSupervisorSocket(t, filepath.Join(gcHome, "supervisor.sock"), func(cmd string) string {
		switch cmd {
		case "ping":
			return "9999\n"
		case "stop":
			stopRequested.Store(true)
			return "ok\n"
		}
		return ""
	})

	err := defaultRestartHelpers().Kill(requestedPID)
	if err == nil {
		t.Fatal("default restart Kill succeeded against a socket reporting another PID")
	}
	if stopRequested.Load() {
		t.Fatal("default restart Kill sent stop to a socket reporting another PID")
	}
	if !strings.Contains(err.Error(), "PID 4242") || !strings.Contains(err.Error(), "PID 9999") {
		t.Fatalf("error = %v, want both requested and observed PIDs", err)
	}
}

func TestDefaultRestartHelpersWindowsKillRefusesSocketPeerPIDMismatch(t *testing.T) {
	gcHome := shortTempDir(t, "gc-home-")
	runtimeDir := shortTempDir(t, "gc-run-")
	t.Setenv("GC_HOME", gcHome)
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)

	const requestedPID = 4242
	var commandRequested atomic.Bool
	startTestSupervisorSocket(t, filepath.Join(gcHome, "supervisor.sock"), func(cmd string) string {
		switch cmd {
		case "ping":
			return "4242\n"
		case testSupervisorSignalTermSocketCommand, "stop":
			commandRequested.Store(true)
			return "ok\n"
		}
		return ""
	})

	err := defaultRestartHelpers().Kill(requestedPID)
	if err == nil {
		t.Fatal("default restart Kill succeeded after the socket peer PID check failed")
	}
	if commandRequested.Load() {
		t.Fatal("default restart Kill sent a shutdown command after the peer PID check failed")
	}
	if !strings.Contains(err.Error(), "PID 4242") || !strings.Contains(err.Error(), fmt.Sprintf("PID %d", os.Getpid())) {
		t.Fatalf("error = %v, want requested and peer PIDs", err)
	}
}

func TestDriftStopRefusesReplacement(t *testing.T) {
	gcHome := shortTempDir(t, "drift-")
	t.Setenv("GC_HOME", gcHome)
	path := supervisorSocketPath()
	old, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("Listen(unix, %q): %v", path, err)
	}
	t.Cleanup(func() {
		old.Close()       //nolint:errcheck
		os.Remove(path)   //nolint:errcheck
		os.Remove(gcHome) //nolint:errcheck
	})
	if err := old.(*net.UnixListener).SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("SetDeadline: %v", err)
	}

	result := make(chan error, 1)
	go func() { result <- requestSupervisorStopForPID(4242) }()
	conn, err := old.Accept()
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	defer conn.Close() //nolint:errcheck
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("SetDeadline(conn): %v", err)
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatalf("ReadString(ping): %v", err)
	}
	if line != "ping\n" {
		t.Fatalf("unexpected probe: %q", line)
	}
	if err := old.Close(); err != nil {
		t.Fatalf("Close(old): %v", err)
	}

	var stopped atomic.Bool
	startTestSupervisorSocket(t, path, func(cmd string) string {
		switch cmd {
		case "ping":
			return "9999\n"
		case testSupervisorSignalTermSocketCommand, "stop":
			stopped.Store(true)
			return "ok\n"
		}
		return ""
	})
	if _, err := io.WriteString(conn, "4242\n"); err != nil {
		t.Fatalf("WriteString(probe response): %v", err)
	}
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("stop request succeeded after the socket owner changed")
		}
		if stopped.Load() {
			t.Fatal("stop request reached the replacement supervisor")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stop request hung")
	}
}

func startBlockingSignalTermSupervisorSocket(t *testing.T, sockPath string, pid int, signalRequested, stopRequested *atomic.Bool, release <-chan struct{}) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(sockPath), 0o700); err != nil {
		t.Fatalf("MkdirAll(%q): %v", filepath.Dir(sockPath), err)
	}
	lis, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("Listen(unix, %q): %v", sockPath, err)
	}
	t.Cleanup(func() {
		lis.Close()         //nolint:errcheck
		os.Remove(sockPath) //nolint:errcheck
	})

	go func() {
		for {
			conn, err := lis.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close() //nolint:errcheck
				buf := make([]byte, 64)
				n, err := conn.Read(buf)
				if err != nil || n == 0 {
					return
				}
				switch strings.TrimSpace(string(buf[:n])) {
				case "ping":
					fmt.Fprintf(conn, "%d\n", pid) //nolint:errcheck
				case testSupervisorSignalTermSocketCommand:
					signalRequested.Store(true)
					<-release
				case "stop":
					stopRequested.Store(true)
					io.WriteString(conn, "ok\n") //nolint:errcheck
				}
			}(conn)
		}
	}()
}

func TestHandleSupervisorConnSignalTermUsesSignalShutdownMode(t *testing.T) {
	t.Setenv(supervisorPreserveSessionsOnSignalEnv, "1")
	client, server := net.Pipe()
	defer client.Close() //nolint:errcheck
	defer server.Close() //nolint:errcheck

	gotMode := make(chan supervisorShutdownMode, 1)
	gotTrigger := make(chan shutdownTrigger, 1)
	handlerDone := make(chan struct{})
	go func() {
		defer close(handlerDone)
		handleSupervisorConn(server, func(mode supervisorShutdownMode, trigger shutdownTrigger) bool {
			gotMode <- mode
			gotTrigger <- trigger
			return false
		}, nil, nil)
	}()

	if _, err := client.Write([]byte(testSupervisorSignalTermSocketCommand + "\n")); err != nil {
		t.Fatalf("Write(signal command): %v", err)
	}
	select {
	case mode := <-gotMode:
		if mode != supervisorShutdownPreserveSessions {
			t.Fatalf("mode = %v, want preserve-sessions", mode)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for signal-equivalent shutdown")
	}
	trigger := <-gotTrigger
	if trigger.Source != "socket_signal" {
		t.Fatalf("trigger.Source = %q, want socket_signal", trigger.Source)
	}
	if trigger.Signal != syscall.SIGTERM.String() {
		t.Fatalf("trigger.Signal = %q, want %q", trigger.Signal, syscall.SIGTERM.String())
	}
	ack := make([]byte, len("ok\n"))
	client.SetReadDeadline(time.Now().Add(time.Second)) //nolint:errcheck
	if _, err := io.ReadFull(client, ack); err != nil {
		t.Fatalf("ReadFull(ok): %v", err)
	}
	if string(ack) != "ok\n" {
		t.Fatalf("ack = %q, want ok", string(ack))
	}
	select {
	case <-handlerDone:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for handler")
	}
}

func TestPrintUnreadableSupervisorRestartErrorWindowsNoProcWording(t *testing.T) {
	var stderr bytes.Buffer
	printUnreadableSupervisorRestartError(&stderr, 1234, os.ErrPermission)
	out := stderr.String()
	if strings.Contains(out, "/proc/") || strings.Contains(out, "different user") {
		t.Fatalf("Windows unreadable message used Unix wording:\n%s", out)
	}
	for _, want := range []string{"Windows", "PID 1234", "--no-auto-restart"} {
		if !strings.Contains(out, want) {
			t.Fatalf("Windows unreadable message missing %q:\n%s", want, out)
		}
	}
}

func startWindowsSleepProcess(t *testing.T, exe string) (*exec.Cmd, <-chan error) {
	t.Helper()
	cmd := exec.Command(exe, "-NoProfile", "-Command", "Start-Sleep -Seconds 600")
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting %q sleep helper: %v", exe, err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	waitForWindowsDriftHelper(t, cmd.Process.Pid, done)
	return cmd, done
}

func waitForWindowsDriftHelper(t *testing.T, pid int, done <-chan error) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if !pidGone(pid) {
			return
		}
		select {
		case err := <-done:
			t.Fatalf("helper exited before becoming live: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("helper PID %d did not become live", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func cleanupWindowsDriftHelper(cmd *exec.Cmd, done <-chan error) {
	if cmd.ProcessState != nil {
		return
	}
	_ = cmd.Process.Kill()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
	}
}

func copyFileForWindowsDriftTest(t *testing.T, source, dest string) {
	t.Helper()
	in, err := os.Open(source)
	if err != nil {
		t.Fatalf("opening %q: %v", source, err)
	}
	defer in.Close() //nolint:errcheck
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o755)
	if err != nil {
		t.Fatalf("creating %q: %v", dest, err)
	}
	defer out.Close() //nolint:errcheck
	if _, err := io.Copy(out, in); err != nil {
		t.Fatalf("copying %q to %q: %v", source, dest, err)
	}
}

func requireWindowsPowerShell(t *testing.T) string {
	t.Helper()
	exe, err := exec.LookPath("powershell.exe")
	if err != nil {
		t.Fatalf("powershell.exe is required for this Windows process test: %v", err)
	}
	return exe
}

func assertSameWindowsPath(t *testing.T, got, want string) {
	t.Helper()
	gotAbs, err := filepath.Abs(got)
	if err != nil {
		t.Fatalf("resolving got path %q: %v", got, err)
	}
	wantAbs, err := filepath.Abs(want)
	if err != nil {
		t.Fatalf("resolving want path %q: %v", want, err)
	}
	if !strings.EqualFold(filepath.Clean(gotAbs), filepath.Clean(wantAbs)) {
		t.Fatalf("path = %q, want %q", got, want)
	}
}
