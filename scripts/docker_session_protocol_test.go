package scripts_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	gcruntime "github.com/gastownhall/gascity/internal/runtime"
	runtimeexec "github.com/gastownhall/gascity/internal/runtime/exec"
	"github.com/gastownhall/gascity/internal/testutil"
)

const (
	dockerProtocolContainerID         = "fake-container-id"
	dockerProtocolObservationInterval = 10 * time.Millisecond
)

func TestDockerSessionProtocol(t *testing.T) {
	root := repoRoot(t)
	adapter := filepath.Join(root, "scripts", "gc-session-docker")
	fakeSource := filepath.Join(root, "scripts", "testdata", "docker-session", "docker")

	run := func(ctx context.Context, fixture *dockerProtocolFixture, executable string, args []string, stdin []byte) ([]byte, error) {
		cmd := exec.CommandContext(ctx, executable, args...)
		cmd.Dir = root
		cmd.Env = fixture.env()
		cmd.Stdin = bytes.NewReader(stdin)
		return cmd.CombinedOutput()
	}

	t.Run("failed_start_removes_created_container", func(t *testing.T) {
		fixture := newDockerProtocolFixture(t, fakeSource)
		fixture.allowImage(t)
		fixture.writeState(t, "tmux-missing", "")

		config := dockerProtocolStartConfig(t, fixture.workDir, "")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		out, err := run(ctx, fixture, adapter, []string{"start", fixture.containerName}, config)
		if err == nil {
			t.Fatal("start succeeded without tmux, want exit 1")
		}
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
			t.Fatalf("start error = %v, want exit 1\noutput:\n%s", err, out)
		}
		if !strings.Contains(string(out), "tmux not found in image 'gc-protocol-test:latest'") {
			t.Errorf("start diagnostic = %q, want missing-tmux context", out)
		}

		if fixture.containerExists() {
			t.Errorf("container %q remains after failed start", fixture.containerName)
		}

		// Rollback force-removes the created container by immutable ID and does
		// not gate removal behind a graceful "stop -t 10": a slow stop would
		// outrun the exec provider's cancellation grace and leak the container.
		calls := fixture.calls(t)
		runAt := -1
		for i, call := range calls {
			if len(call) > 0 && call[0] == "run" {
				runAt = i
				break
			}
		}
		if runAt < 0 {
			t.Fatalf("docker run was not observed; calls:\n%s", formatDockerProtocolCalls(calls))
		}
		cleanup := dockerProtocolCleanupCallsAfterRun(calls)
		if len(cleanup) != 1 || !reflect.DeepEqual(cleanup[0], []string{"rm", "-f", dockerProtocolContainerID}) {
			t.Errorf("failed-start cleanup = %v, want exactly one immutable-ID rm -f; calls:\n%s",
				cleanup, formatDockerProtocolCalls(calls))
		}
	})

	t.Run("cleanup_failure_preserves_original_start_error", func(t *testing.T) {
		fixture := newDockerProtocolFixture(t, fakeSource)
		fixture.allowImage(t)
		fixture.writeState(t, "fail-mkdir-status", "23\n")
		fixture.writeState(t, "fail-rm-status", "41\n")

		config := dockerProtocolStartConfig(t, fixture.workDir, "")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		out, err := run(ctx, fixture, adapter, []string{"start", fixture.containerName}, config)
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 23 {
			t.Fatalf("start error = %v, want injected exit 23\noutput:\n%s", err, out)
		}
		if !strings.Contains(string(out), "failed to remove container '"+fixture.containerName+"'") {
			t.Errorf("start diagnostic = %q, want contextual remove warning", out)
		}
		if !strings.Contains(string(out), "injected rm failure (status 41)") {
			t.Errorf("start diagnostic = %q, want cleanup failure detail", out)
		}
	})

	t.Run("trailing_space_prompt_matches_trimmed_capture", func(t *testing.T) {
		fixture := newDockerProtocolFixture(t, fakeSource)
		fixture.allowImage(t)
		fixture.writeState(t, "prompt-output", ">\n"+strings.Repeat("\n", 20))

		config := dockerProtocolStartConfig(t, fixture.workDir, "> ")
		ctx, cancel := context.WithTimeout(context.Background(), testutil.ExecRaceTimeout)
		defer cancel()
		started := time.Now()
		out, err := run(ctx, fixture, adapter, []string{"start", fixture.containerName}, config)
		if err != nil {
			calls := fixture.calls(t)
			t.Fatalf("start did not observe the trimmed prompt: %v (context: %v, elapsed: %s)\noutput:\n%s\ndocker calls:\n%s",
				err, ctx.Err(), time.Since(started).Round(time.Millisecond), out, formatDockerProtocolCalls(calls))
		}
		if !fixture.containerExists() {
			t.Fatalf("successful start removed container %q; cleanup guard was not disarmed", fixture.containerName)
		}

		calls := fixture.calls(t)
		wantCapture := dockerProtocolCaptureCall(fixture.containerName, 120)
		if captureCalls := dockerProtocolCallCount(calls, wantCapture); captureCalls != 1 {
			t.Fatalf("exact 120-line prompt observations = %d, want exactly 1; calls:\n%s",
				captureCalls, formatDockerProtocolCalls(calls))
		}
		if cleanupCalls := dockerProtocolCleanupCallsAfterRun(calls); len(cleanupCalls) != 0 {
			t.Fatalf("successful start invoked cleanup guard: %v\ncalls:\n%s", cleanupCalls, formatDockerProtocolCalls(calls))
		}
	})

	t.Run("prompt_semantics_conform_to_native_tmux", func(t *testing.T) {
		tests := []struct {
			name   string
			prefix string
			output string
		}{
			{name: "regular content", prefix: "> ", output: "> ready"},
			{name: "non-breaking space", prefix: "❯ ", output: "❯\u00a0"},
			{name: "box border", prefix: "❯ ", output: "│ ❯\u00a0"},
			{name: "configured border prefix", prefix: "│ ", output: "│ prompt"},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				fixture := newDockerProtocolFixture(t, fakeSource)
				fixture.allowImage(t)
				fixture.writeState(t, "prompt-output", tt.output+"\n")

				config := dockerProtocolStartConfig(t, fixture.workDir, tt.prefix)
				ctx, cancel := context.WithTimeout(context.Background(), testutil.ExecRaceTimeout)
				defer cancel()
				out, err := run(ctx, fixture, adapter, []string{"start", fixture.containerName}, config)
				if err != nil {
					t.Fatalf("start did not observe prompt %q for prefix %q: %v\noutput:\n%s\ncalls:\n%s",
						tt.output, tt.prefix, err, out, formatDockerProtocolCalls(fixture.calls(t)))
				}
				calls := fixture.calls(t)
				if got := dockerProtocolCallCount(calls, dockerProtocolCaptureCall(fixture.containerName, 120)); got != 1 {
					t.Fatalf("prompt observations = %d, want exactly 1; calls:\n%s", got, formatDockerProtocolCalls(calls))
				}
			})
		}
	})

	t.Run("context_cancellation_rolls_back_created_container", func(t *testing.T) {
		fixture := newDockerProtocolFixture(t, fakeSource)
		fixture.allowImage(t)
		fixture.writeState(t, "prompt-output", "not ready >\n")

		provider := runtimeexec.NewProvider(fixture.adapterWrapper(t, adapter))
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		startResult := make(chan error, 1)
		go func() {
			startResult <- provider.Start(ctx, fixture.containerName, gcruntime.Config{
				Command:           "sleep infinity",
				WorkDir:           fixture.workDir,
				ReadyPromptPrefix: "> ",
				Env: map[string]string{
					"GC_DOCKER_HOME_MOUNT": "false",
					"GC_DOCKER_IMAGE":      "gc-protocol-test:latest",
				},
			})
		}()

		captureCall := dockerProtocolCaptureCall(fixture.containerName, 120)
		observationCtx, stopObservation := context.WithTimeout(context.Background(), testutil.ExecRaceTimeout)
		waitForDockerProtocolCall(observationCtx, t, fixture, captureCall)
		stopObservation()
		cancel()

		completionCtx, stopCompletion := context.WithTimeout(context.Background(), testutil.ExecRaceTimeout)
		defer stopCompletion()
		var err error
		select {
		case err = <-startResult:
		case <-completionCtx.Done():
			t.Fatalf("Start did not return within %s after cancellation", testutil.ExecRaceTimeout)
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Start error = %v, want context.Canceled", err)
		}

		calls := fixture.calls(t)
		if dockerProtocolCallCount(calls, captureCall) == 0 {
			t.Fatalf("cancellation started before prompt observation; calls:\n%s", formatDockerProtocolCalls(calls))
		}
		if fixture.containerExists() {
			t.Errorf("container %q remains after start context cancellation; calls:\n%s",
				fixture.containerName, formatDockerProtocolCalls(calls))
		}
		cleanup := dockerProtocolCleanupCallsAfterRun(calls)
		if len(cleanup) != 1 || !reflect.DeepEqual(cleanup[0], []string{"rm", "-f", dockerProtocolContainerID}) {
			t.Errorf("immutable-ID cleanup calls = %v, want a single force-remove; calls:\n%s",
				cleanup, formatDockerProtocolCalls(calls))
		}
	})

	t.Run("unsupported_command_fails_closed", func(t *testing.T) {
		fixture := newDockerProtocolFixture(t, fakeSource)
		wantArgs := []string{"unsupported-op", "two words", "line one\nline two", ""}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		out, err := run(ctx, fixture, fixture.fakeDocker, wantArgs, nil)
		if err == nil {
			t.Fatal("unsupported fake Docker command succeeded")
		}
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 97 {
			t.Fatalf("unsupported command error = %v, want exit 97\noutput:\n%s", err, out)
		}
		if !strings.Contains(string(out), "fake docker: unsupported argv:") {
			t.Fatalf("unsupported command output = %q, want decoded argv diagnostic", out)
		}
		calls := fixture.calls(t)
		if len(calls) != 1 || !reflect.DeepEqual(calls[0], wantArgs) {
			t.Fatalf("lossless argv trace = %#v, want %#v", calls, wantArgs)
		}
	})

	t.Run("malformed_known_commands_fail_closed", func(t *testing.T) {
		tests := []struct {
			name     string
			args     []string
			seedTmux bool
		}{
			{name: "zero arguments", args: []string{}},
			{name: "run missing detached and init flags", args: []string{"run", "--name", "gc-protocol-test", "image", "sleep", "infinity"}},
			{
				name:     "capture targets wrong session",
				seedTmux: true,
				args: []string{
					"exec", "-e", "TMUX_TMPDIR=/run/gc-tmux", "gc-protocol-test",
					"tmux", "-u", "capture-pane", "-p", "-t", "agent", "-S", "-120",
				},
			},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				fixture := newDockerProtocolFixture(t, fakeSource)
				if tt.seedTmux {
					fixture.writeState(t, filepath.Join("containers", fixture.containerName), "running\n")
					fixture.writeState(t, filepath.Join("tmux", fixture.containerName), "running\n")
				}
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				out, err := run(ctx, fixture, fixture.fakeDocker, tt.args, nil)
				var exitErr *exec.ExitError
				if !errors.As(err, &exitErr) || exitErr.ExitCode() != 97 {
					t.Fatalf("malformed command error = %v, want exit 97\noutput:\n%s", err, out)
				}
				if !strings.Contains(string(out), "fake docker: unsupported argv:") {
					t.Fatalf("malformed command output = %q, want decoded argv diagnostic", out)
				}
				calls := fixture.calls(t)
				if len(calls) != 1 || !reflect.DeepEqual(calls[0], tt.args) {
					t.Fatalf("lossless argv trace = %#v, want %#v", calls, tt.args)
				}
			})
		}
	})
}

type dockerProtocolFixture struct {
	stateDir      string
	binDir        string
	fakeDocker    string
	homeDir       string
	workDir       string
	containerName string
}

func newDockerProtocolFixture(t *testing.T, fakeSource string) *dockerProtocolFixture {
	t.Helper()
	root := t.TempDir()
	binDir := filepath.Join(root, "bin")
	stateDir := filepath.Join(root, "state")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("create fake Docker bin directory: %v", err)
	}
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatalf("create fake Docker state directory: %v", err)
	}
	source, err := os.ReadFile(fakeSource)
	if err != nil {
		t.Fatalf("read fake Docker executable: %v", err)
	}
	fakeDocker := filepath.Join(binDir, "docker")
	if err := os.WriteFile(fakeDocker, source, 0o755); err != nil {
		t.Fatalf("install fake Docker executable: %v", err)
	}
	return &dockerProtocolFixture{
		stateDir:      stateDir,
		binDir:        binDir,
		fakeDocker:    fakeDocker,
		homeDir:       t.TempDir(),
		workDir:       t.TempDir(),
		containerName: "gc-protocol-test",
	}
}

func (f *dockerProtocolFixture) env() []string {
	env := make([]string, 0, len(os.Environ())+3)
	for _, entry := range os.Environ() {
		if strings.HasPrefix(entry, "PATH=") ||
			strings.HasPrefix(entry, "HOME=") ||
			strings.HasPrefix(entry, "GC_TEST_DOCKER_STATE_DIR=") {
			continue
		}
		env = append(env, entry)
	}
	return append(env,
		"PATH="+f.binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"HOME="+f.homeDir,
		"GC_TEST_DOCKER_STATE_DIR="+f.stateDir,
	)
}

func (f *dockerProtocolFixture) adapterWrapper(t *testing.T, adapter string) string {
	t.Helper()
	wrapper := filepath.Join(filepath.Dir(f.binDir), "provider")
	contents := fmt.Sprintf("#!/usr/bin/env bash\nexport PATH=%s\nexport HOME=%s\nexport GC_TEST_DOCKER_STATE_DIR=%s\nexec %s \"$@\"\n",
		shellSingleQuote(f.binDir+string(os.PathListSeparator)+os.Getenv("PATH")),
		shellSingleQuote(f.homeDir),
		shellSingleQuote(f.stateDir),
		shellSingleQuote(adapter),
	)
	if err := os.WriteFile(wrapper, []byte(contents), 0o755); err != nil {
		t.Fatalf("write Docker adapter wrapper: %v", err)
	}
	return wrapper
}

func shellSingleQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func (f *dockerProtocolFixture) allowImage(t *testing.T) {
	t.Helper()
	f.writeState(t, "images", "gc-protocol-test:latest\n")
}

func (f *dockerProtocolFixture) writeState(t *testing.T, name, content string) {
	t.Helper()
	statePath := filepath.Join(f.stateDir, name)
	if err := os.MkdirAll(filepath.Dir(statePath), 0o755); err != nil {
		t.Fatalf("create fake Docker state parent for %s: %v", name, err)
	}
	if err := os.WriteFile(statePath, []byte(content), 0o644); err != nil {
		t.Fatalf("write fake Docker state %s: %v", name, err)
	}
}

func (f *dockerProtocolFixture) containerExists() bool {
	_, err := os.Stat(filepath.Join(f.stateDir, "containers", f.containerName))
	return err == nil
}

func (f *dockerProtocolFixture) calls(t *testing.T) [][]string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(f.stateDir, "calls"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read fake Docker calls: %v", err)
	}
	calls := make([][]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".argv") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(f.stateDir, "calls", entry.Name()))
		if err != nil {
			t.Fatalf("read fake Docker call %s: %v", entry.Name(), err)
		}
		newline := bytes.IndexByte(data, '\n')
		if newline < 0 {
			t.Fatalf("fake Docker call %s has no argc header", entry.Name())
		}
		argc, err := strconv.Atoi(string(data[:newline]))
		if err != nil || argc < 0 {
			t.Fatalf("fake Docker call %s argc = %q: %v", entry.Name(), data[:newline], err)
		}
		parts := bytes.Split(data[newline+1:], []byte{0})
		if len(parts) != argc+1 || len(parts[argc]) != 0 {
			t.Fatalf("fake Docker call %s payload has %d fields, want %d plus terminator", entry.Name(), len(parts), argc)
		}
		call := make([]string, argc)
		for i := 0; i < argc; i++ {
			call[i] = string(parts[i])
		}
		calls = append(calls, call)
	}
	return calls
}

func dockerProtocolStartConfig(t *testing.T, workDir, readyPrefix string) []byte {
	t.Helper()
	config := struct {
		Command           string            `json:"command"`
		WorkDir           string            `json:"work_dir"`
		ReadyPromptPrefix string            `json:"ready_prompt_prefix,omitempty"`
		Env               map[string]string `json:"env"`
	}{
		Command:           "sleep infinity",
		WorkDir:           workDir,
		ReadyPromptPrefix: readyPrefix,
		Env: map[string]string{
			"GC_DOCKER_HOME_MOUNT": "false",
			"GC_DOCKER_IMAGE":      "gc-protocol-test:latest",
		},
	}
	data, err := json.Marshal(config)
	if err != nil {
		t.Fatalf("marshal Docker start config: %v", err)
	}
	return data
}

func dockerProtocolCaptureCall(containerName string, observationLines int) []string {
	return []string{
		"exec", "-e", "TMUX_TMPDIR=/run/gc-tmux", containerName,
		"tmux", "-u", "capture-pane", "-p", "-t", "main", "-S", fmt.Sprintf("-%d", observationLines),
	}
}

func dockerProtocolCallCount(calls [][]string, want []string) int {
	count := 0
	for _, call := range calls {
		if reflect.DeepEqual(call, want) {
			count++
		}
	}
	return count
}

func waitForDockerProtocolCall(ctx context.Context, t *testing.T, fixture *dockerProtocolFixture, want []string) {
	t.Helper()
	ticker := time.NewTicker(dockerProtocolObservationInterval)
	defer ticker.Stop()

	var calls [][]string
	for {
		calls = fixture.calls(t)
		if dockerProtocolCallCount(calls, want) > 0 {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("waiting for exact Docker call %q: %v; last calls:\n%s",
				want, ctx.Err(), formatDockerProtocolCalls(calls))
		case <-ticker.C:
		}
	}
}

// dockerProtocolCleanupCallsAfterRun returns the immutable-ID cleanup calls
// (a graceful "stop -t 10" or a force "rm -f") observed after the first
// "docker run", so tests can assert exactly how a created container is torn
// down. It still matches the graceful stop so a regression that reintroduces
// it before the force-remove is caught.
func dockerProtocolCleanupCallsAfterRun(calls [][]string) [][]string {
	runAt := -1
	for i, call := range calls {
		if len(call) > 0 && call[0] == "run" {
			runAt = i
			break
		}
	}
	if runAt < 0 {
		return nil
	}

	var cleanup [][]string
	for _, call := range calls[runAt+1:] {
		if reflect.DeepEqual(call, []string{"stop", "-t", "10", dockerProtocolContainerID}) ||
			reflect.DeepEqual(call, []string{"rm", "-f", dockerProtocolContainerID}) {
			cleanup = append(cleanup, call)
		}
	}
	return cleanup
}

func formatDockerProtocolCalls(calls [][]string) string {
	var formatted strings.Builder
	for i, call := range calls {
		fmt.Fprintf(&formatted, "%02d: %q\n", i, call)
	}
	return formatted.String()
}
