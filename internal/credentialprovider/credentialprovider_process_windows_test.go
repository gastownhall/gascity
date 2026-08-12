//go:build integration && windows

package credentialprovider

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/testutil"
	"golang.org/x/sys/windows"
)

const (
	windowsHelperTestPattern = "^TestCredentialProviderWindowsProcessHelper$"
	windowsHelperWaitMode    = "provider-wait"
	windowsHelperExitMode    = "provider-exit"
	windowsHelperBlockMode   = "descendant-block"
)

func TestCredentialProviderWindowsJobKillsDescendants(t *testing.T) {
	pidPath := t.TempDir() + `\descendant.pid`
	expiresAt := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	provider, err := New([]string{
		windowsTestExecutable(t),
		"-test.run=" + windowsHelperTestPattern,
		"--",
		windowsHelperWaitMode,
		pidPath,
		expiresAt,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() {
		_, mintErr := provider.Mint(ctx, validCredentialRequest())
		done <- mintErr
	}()

	pid := waitForWindowsPIDFile(t, pidPath, done, func(mintErr error) string {
		if mintErr == nil {
			return "Mint completed successfully"
		}
		return fmt.Sprintf("Mint returned error: %v", mintErr)
	})
	process, err := windows.OpenProcess(
		windows.SYNCHRONIZE|windows.PROCESS_TERMINATE|windows.PROCESS_QUERY_LIMITED_INFORMATION,
		false,
		uint32(pid),
	)
	if err != nil {
		t.Fatalf("open descendant process %d: %v", pid, err)
	}
	t.Cleanup(func() {
		_ = windows.TerminateProcess(process, 1)
		_ = windows.CloseHandle(process)
	})
	cancel()

	select {
	case mintErr := <-done:
		if !errors.Is(mintErr, context.Canceled) {
			t.Fatalf("Mint error = %v, want context cancellation", mintErr)
		}
	case <-time.After(testutil.ExecRaceTimeout):
		t.Fatal("Mint did not return after cancellation")
	}
	event, err := windows.WaitForSingleObject(process, uint32(testutil.ExecRaceTimeout/time.Millisecond))
	if err != nil {
		t.Fatalf("wait for descendant process %d: %v", pid, err)
	}
	if event != windows.WAIT_OBJECT_0 {
		t.Fatalf("descendant wait result = %#x, want WAIT_OBJECT_0", event)
	}
}

func TestCredentialProviderWindowsJobCloseKillsDescendantsAfterParentExit(t *testing.T) {
	dir := t.TempDir()
	pidPath := dir + `\descendant.pid`
	releasePath := dir + `\release-parent`
	expiresAt := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	response := windowsCredentialResponse(expiresAt)
	executable := windowsTestExecutable(t)
	type commandResult struct {
		output commandOutput
		err    error
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan commandResult, 1)
	go func() {
		output, runErr := runCommand(
			ctx,
			[]string{
				executable,
				"-test.run=" + windowsHelperTestPattern,
				"--",
				windowsHelperExitMode,
				pidPath,
				releasePath,
				expiresAt,
			},
			nil,
			minimalEnvironment(os.Environ()),
		)
		done <- commandResult{output: output, err: runErr}
	}()

	pid := waitForWindowsPIDFile(t, pidPath, done, func(result commandResult) string {
		class := "startup-or-control failure"
		switch {
		case result.err == nil:
			class = "success"
		case errors.Is(result.err, context.Canceled):
			class = "canceled"
		case errors.Is(result.err, context.DeadlineExceeded):
			class = "deadline"
		case errors.Is(result.err, exec.ErrWaitDelay):
			class = "pipe wait deadline"
		default:
			var exitErr *exec.ExitError
			if errors.As(result.err, &exitErr) {
				class = "process exit failure"
			}
		}
		return fmt.Sprintf(
			"class=%s err=%v stdout_bytes=%d stderr_bytes=%d stdout_overflow=%t stderr_overflow=%t",
			class,
			result.err,
			len(result.output.stdout),
			len(result.output.stderr),
			result.output.stdoutOverflow,
			result.output.stderrOverflow,
		)
	})
	process, err := windows.OpenProcess(
		windows.SYNCHRONIZE|windows.PROCESS_TERMINATE|windows.PROCESS_QUERY_LIMITED_INFORMATION,
		false,
		uint32(pid),
	)
	if err != nil {
		t.Fatalf("open descendant process %d: %v", pid, err)
	}
	t.Cleanup(func() {
		_ = windows.TerminateProcess(process, 1)
		_ = windows.CloseHandle(process)
	})
	if err := os.WriteFile(releasePath, []byte("release"), 0o600); err != nil {
		t.Fatalf("release provider parent: %v", err)
	}

	select {
	case result := <-done:
		if !errors.Is(result.err, exec.ErrWaitDelay) {
			t.Fatalf("runCommand error = %v, want exec.ErrWaitDelay", result.err)
		}
		if got, want := string(result.output.stdout), response+"\n"; got != want {
			t.Fatalf("stdout = %q, want exact response %q", got, want)
		}
	case <-time.After(testutil.ExecRaceTimeout):
		t.Fatal("runCommand did not bound descendant-held response pipes after the provider parent exited")
	}
	event, err := windows.WaitForSingleObject(process, uint32(testutil.ExecRaceTimeout/time.Millisecond))
	if err != nil {
		t.Fatalf("wait for descendant process %d: %v", pid, err)
	}
	if event != windows.WAIT_OBJECT_0 {
		t.Fatalf("descendant wait result = %#x, want WAIT_OBJECT_0", event)
	}
}

func TestCredentialProviderWindowsProcessHelper(t *testing.T) {
	arguments := flag.Args()
	if len(arguments) == 0 {
		t.Skip("subprocess helper")
	}
	switch arguments[0] {
	case windowsHelperWaitMode:
		if len(arguments) != 3 {
			t.Fatal("provider wait helper received invalid arguments")
		}
		descendant := startWindowsTestDescendant(t)
		writeWindowsTestPID(t, arguments[1], descendant.Process.Pid)
		writeWindowsCredentialResponse(t, arguments[2])
		if err := descendant.Wait(); err != nil {
			t.Fatalf("descendant exited before provider cancellation: %v", err)
		}
		t.Fatal("descendant exited before provider cancellation")
	case windowsHelperExitMode:
		if len(arguments) != 4 {
			t.Fatal("provider exit helper received invalid arguments")
		}
		descendant := startWindowsTestDescendant(t)
		writeWindowsTestPID(t, arguments[1], descendant.Process.Pid)
		if err := descendant.Process.Release(); err != nil {
			t.Fatalf("release descendant process handle: %v", err)
		}
		waitForWindowsRelease(t, arguments[2])
		writeWindowsCredentialResponse(t, arguments[3])
		os.Exit(0)
	case windowsHelperBlockMode:
		if len(arguments) != 1 {
			t.Fatal("descendant helper received invalid arguments")
		}
		event, err := windows.CreateEvent(nil, 0, 0, nil)
		if err != nil {
			t.Fatalf("create descendant wait event: %v", err)
		}
		defer windows.CloseHandle(event)
		waitResult, err := windows.WaitForSingleObject(event, windows.INFINITE)
		t.Fatalf("descendant wait returned event=%#x err=%v", waitResult, err)
	default:
		t.Skip("subprocess helper")
	}
}

func windowsTestExecutable(t *testing.T) string {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("resolve test executable: %v", err)
	}
	return executable
}

func startWindowsTestDescendant(t *testing.T) *exec.Cmd {
	t.Helper()
	command := exec.Command(
		windowsTestExecutable(t),
		"-test.run="+windowsHelperTestPattern,
		"--",
		windowsHelperBlockMode,
	)
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if err := command.Start(); err != nil {
		t.Fatalf("start descendant helper: %v", err)
	}
	t.Cleanup(func() {
		_ = command.Process.Kill()
	})
	return command
}

func writeWindowsTestPID(t *testing.T, path string, pid int) {
	t.Helper()
	if err := os.WriteFile(path, []byte(strconv.Itoa(pid)), 0o600); err != nil {
		t.Fatalf("write descendant pid: %v", err)
	}
}

func waitForWindowsRelease(t *testing.T, path string) {
	t.Helper()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		_, err := os.Stat(path)
		if err == nil {
			return
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("stat provider release file: %v", err)
		}
		<-ticker.C
	}
}

func writeWindowsCredentialResponse(t *testing.T, expiresAt string) {
	t.Helper()
	if _, err := fmt.Fprintln(os.Stdout, windowsCredentialResponse(expiresAt)); err != nil {
		t.Fatalf("write credential response: %v", err)
	}
}

func windowsCredentialResponse(expiresAt string) string {
	return fmt.Sprintf(
		`{"version":"%s","kind":"Credential","access_token":"opaque-token","authorization_scheme":"Bearer","expires_at":"%s","audience":"manifold","scopes":["manifold:pool:acme","manifold:proxy"]}`,
		ProtocolVersion,
		expiresAt,
	)
}

func waitForWindowsPIDFile[T any](t *testing.T, path string, done <-chan T, describe func(T) string) int {
	t.Helper()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.NewTimer(testutil.ExecRaceTimeout + commandWaitDelay + commandKillGrace)
	defer deadline.Stop()
	for {
		raw, err := os.ReadFile(path)
		if err == nil {
			pid, parseErr := strconv.Atoi(strings.TrimSpace(string(raw)))
			if parseErr != nil || pid <= 1 {
				t.Fatalf("descendant pid = %q: %v", raw, parseErr)
			}
			return pid
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("read descendant pid: %v", err)
		}
		select {
		case result := <-done:
			t.Fatalf("operation completed before descendant pid file %s: %s", path, describe(result))
		case <-ticker.C:
		case <-deadline.C:
			select {
			case result := <-done:
				t.Fatalf("operation completed before descendant pid file %s: %s", path, describe(result))
			default:
			}
			t.Fatalf("timed out waiting for descendant pid file %s", path)
		}
	}
}
