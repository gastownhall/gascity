//go:build integration && !windows

package credentialprovider

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/processgroup/processgrouptest"
	"github.com/gastownhall/gascity/internal/testutil"
)

const integrationCredentialJSON = `{"version":"gascity.dev/credential-provider/v1","kind":"Credential","access_token":"opaque-token","authorization_scheme":"Bearer","expires_at":"2026-07-16T12:05:00Z","audience":"manifold","scopes":["manifold:pool:acme","manifold:proxy"]}`

func TestRunCommandDoesNotInterpretArgvAsShell(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "must-not-exist")
	literal := "$(touch " + marker + ");*"
	output, err := runCommand(
		context.Background(),
		[]string{"printf", "%s", literal},
		nil,
		minimalEnvironment(os.Environ()),
	)
	if err != nil {
		t.Fatalf("runCommand: %v", err)
	}
	if got := string(output.stdout); got != literal {
		t.Fatalf("stdout = %q, want literal argv %q", got, literal)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("shell metacharacters were interpreted; marker stat error = %v", err)
	}
}

func TestRunCommandDeliversExactStdin(t *testing.T) {
	payload := []byte(`{"version":"gascity.dev/credential-provider/v1","audience":"manifold","required_scopes":["manifold:proxy"],"org":"org-acme","force_refresh":false,"interactive":false}`)
	output, err := runCommand(
		context.Background(),
		[]string{"cat"},
		payload,
		minimalEnvironment(os.Environ()),
	)
	if err != nil {
		t.Fatalf("runCommand: %v", err)
	}
	if got := string(output.stdout); got != string(payload) {
		t.Fatalf("stdout = %q, want exact stdin %q", got, payload)
	}
}

func TestRunCommandReplacesEnvironment(t *testing.T) {
	environment := []string{
		"CREDENTIAL_PROVIDER_TEST_ONLY=present",
		"PATH=" + os.Getenv("PATH"),
	}
	output, err := runCommand(
		context.Background(),
		[]string{"env"},
		nil,
		environment,
	)
	if err != nil {
		t.Fatalf("runCommand: %v", err)
	}
	got := strings.Split(strings.TrimSuffix(string(output.stdout), "\n"), "\n")
	want := append([]string(nil), environment...)
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("child environment mismatch: got %d entries, want %d (values redacted)", len(got), len(want))
	}
}

func TestRunCommandReplacesEnvironmentWithEmptySet(t *testing.T) {
	output, err := runCommand(
		context.Background(),
		[]string{"env"},
		nil,
		[]string{},
	)
	if err != nil {
		t.Fatalf("runCommand: %v", err)
	}
	if len(output.stdout) != 0 {
		t.Fatalf("child inherited environment: got %d output bytes (values redacted)", len(output.stdout))
	}
}

func TestRunCommandDrainsAndBoundsConcurrentOutputUntilCancellation(t *testing.T) {
	readyPath := t.TempDir() + "/output-complete"
	script := strings.Join([]string{
		`(head -c 2097152 /dev/zero) &`,
		`stdout_pid=$!`,
		`(head -c 2097152 /dev/zero >&2) &`,
		`stderr_pid=$!`,
		`wait "$stdout_pid"`,
		`wait "$stderr_pid"`,
		`: > "$1"`,
		`sleep 30`,
	}, "\n")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct {
		output commandOutput
		err    error
	}, 1)
	go func() {
		output, err := runCommand(
			ctx,
			[]string{"sh", "-c", script, "credential-provider-test", readyPath},
			nil,
			minimalEnvironment(os.Environ()),
		)
		done <- struct {
			output commandOutput
			err    error
		}{output: output, err: err}
	}()
	waitForFile(t, readyPath)
	cancel()

	select {
	case result := <-done:
		if result.err == nil {
			t.Fatal("runCommand succeeded after cancellation")
		}
		if !result.output.stdoutOverflow || !result.output.stderrOverflow {
			t.Fatalf("overflow = stdout:%v stderr:%v", result.output.stdoutOverflow, result.output.stderrOverflow)
		}
		if len(result.output.stdout) != maxStdoutBytes || len(result.output.stderr) != maxStderrBytes {
			t.Fatalf("captured bytes = stdout:%d stderr:%d", len(result.output.stdout), len(result.output.stderr))
		}
	case <-time.After(15 * time.Second):
		t.Fatal("runCommand deadlocked while draining overflowing output")
	}
}

func TestCredentialProviderWholeResponseTimeout(t *testing.T) {
	dir := t.TempDir()
	pidPath := dir + "/descendant.pid"
	readyPath := dir + "/response-written"
	script := strings.Join([]string{
		`(trap '' HUP; sleep 30) &`,
		`child=$!`,
		`printf '%s' "$child" > "$1"`,
		`printf '%s\n' '` + integrationCredentialJSON + `'`,
		`: > "$2"`,
		`wait "$child"`,
	}, "\n")
	provider, err := New([]string{"sh", "-c", script, "credential-provider-test", pidPath, readyPath})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	provider.now = func() time.Time { return credentialTestNow }
	t.Cleanup(func() { processgrouptest.KillFromPIDFile(t, pidPath) })

	done := make(chan error, 1)
	go func() {
		_, mintErr := provider.Mint(context.Background(), validCredentialRequest())
		done <- mintErr
	}()
	waitForFile(t, readyPath)

	select {
	case mintErr := <-done:
		if !errors.Is(mintErr, context.DeadlineExceeded) {
			t.Fatalf("Mint error = %v, want context deadline", mintErr)
		}
	case <-time.After(helperTimeout + testutil.ExecRaceTimeout):
		t.Fatal("Mint did not honor the whole-response deadline")
	}

	waitForProcessGone(t, pidPath)
}

func TestCredentialProviderParentCancellationCleanup(t *testing.T) {
	pidPath := t.TempDir() + "/descendant.pid"
	script := strings.Join([]string{
		`sleep 30 &`,
		`child=$!`,
		`printf '%s' "$child" > "$1"`,
		`wait "$child"`,
	}, "\n")
	provider, err := New([]string{"sh", "-c", script, "credential-provider-test", pidPath})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { processgrouptest.KillFromPIDFile(t, pidPath) })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, mintErr := provider.Mint(ctx, validCredentialRequest())
		done <- mintErr
	}()
	waitForFile(t, pidPath)
	cancel()
	select {
	case mintErr := <-done:
		if !errors.Is(mintErr, context.Canceled) {
			t.Fatalf("Mint error = %v, want parent cancellation", mintErr)
		}
	case <-time.After(helperTimeout + testutil.ExecRaceTimeout):
		t.Fatal("Mint did not honor parent cancellation")
	}
	waitForProcessGone(t, pidPath)
}

func TestCredentialProviderDescendantPipeCleanupFailsClosed(t *testing.T) {
	dir := t.TempDir()
	pidPath := dir + "/descendant.pid"
	releasePath := dir + "/release-parent"
	script := strings.Join([]string{
		`(trap '' HUP; sleep 30) &`,
		`child=$!`,
		`printf '%s' "$child" > "$1"`,
		`while [ ! -e "$2" ]; do sleep 0.01; done`,
		`printf '%s\n' '` + integrationCredentialJSON + `'`,
		`exit 0`,
	}, "\n")
	provider, err := New([]string{"sh", "-c", script, "credential-provider-test", pidPath, releasePath})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	provider.now = func() time.Time { return credentialTestNow }
	t.Cleanup(func() { processgrouptest.KillFromPIDFile(t, pidPath) })

	done := make(chan error, 1)
	go func() {
		_, mintErr := provider.Mint(context.Background(), validCredentialRequest())
		done <- mintErr
	}()
	waitForFile(t, pidPath)
	if err := os.WriteFile(releasePath, []byte("release"), 0o600); err != nil {
		t.Fatalf("release provider parent: %v", err)
	}
	select {
	case mintErr := <-done:
		if mintErr == nil {
			t.Fatal("Mint accepted a response whose descendant held the response pipes open")
		}
	case <-time.After(helperTimeout + testutil.ExecRaceTimeout):
		t.Fatal("Mint did not bound descendant-held response pipes")
	}
	waitForProcessGone(t, pidPath)
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.After(testutil.ExecRaceTimeout)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("stat readiness file %s: %v", path, err)
		}
		select {
		case <-ticker.C:
		case <-deadline:
			t.Fatalf("timed out waiting for readiness file %s", path)
		}
	}
}

func waitForProcessGone(t *testing.T, pidPath string) {
	t.Helper()
	rawPID, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatalf("read descendant pid: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(rawPID)))
	if err != nil || pid <= 1 {
		t.Fatalf("descendant pid = %q: %v", rawPID, err)
	}

	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.After(testutil.ExecRaceTimeout)
	for {
		err := syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return
		}
		if err != nil && !errors.Is(err, syscall.EPERM) {
			t.Fatalf("probe descendant %d: %v", pid, err)
		}
		select {
		case <-ticker.C:
		case <-deadline:
			t.Fatalf("descendant process %d survived provider cancellation", pid)
		}
	}
}
