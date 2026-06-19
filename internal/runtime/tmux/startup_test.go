package tmux

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/shellquote"
)

func boolPtr(b bool) *bool { return &b }

func fallbackPromptDir(tmpRoot string) string {
	return filepath.Join(tmpRoot, fmt.Sprintf(".gc-%d", os.Getuid()), "tmux-prompts")
}

// startCall records a single invocation on fakeStartOps with full arguments.
type startCall struct {
	method       string
	name         string
	workDir      string
	command      string
	env          map[string]string
	processNames []string
	rc           *RuntimeConfig
	timeout      time.Duration
}

// fakeStartOps records calls with full arguments and simulates outcomes
// for doStartSession tests.
type fakeStartOps struct {
	calls []startCall

	// createSession returns errors from this slice sequentially.
	// First call returns createErrs[0], second call returns createErrs[1], etc.
	// If the slice is exhausted, returns nil.
	createErrs []error
	createIdx  int

	isSessionRunningResult     *bool
	isRuntimeRunningResult     bool
	killErr                    error
	waitCommandErr             error
	acceptStartupDialogsErr    error
	waitReadyErr               error
	waitCommandHook            func()
	acceptStartupDialogsHook   func()
	waitReadyHook              func()
	hasSessionHook             func()
	sendKeysHook               func()
	runSetupCommandHook        func(string)
	hasSessionResult           bool
	hasSessionErr              error
	setRemainOnExitErr         error
	disableMouseAndActivityErr error
	runSetupCommandErr         error
	sendKeysErr                error
	capturePaneText            string
	capturePaneErr             error
	recordStartCrashPath       string
}

type errReader struct{}

func (errReader) Read(_ []byte) (int, error) {
	return 0, io.ErrUnexpectedEOF
}

func (f *fakeStartOps) createSession(name, workDir, command string, env map[string]string) error {
	f.calls = append(f.calls, startCall{
		method:  "createSession",
		name:    name,
		workDir: workDir,
		command: command,
		env:     env,
	})
	if f.createIdx < len(f.createErrs) {
		err := f.createErrs[f.createIdx]
		f.createIdx++
		return err
	}
	return nil
}

func (f *fakeStartOps) isSessionRunning(name string) bool {
	f.calls = append(f.calls, startCall{
		method: "isSessionRunning",
		name:   name,
	})
	if f.isSessionRunningResult == nil {
		return true
	}
	return *f.isSessionRunningResult
}

func (f *fakeStartOps) isRuntimeRunning(name string, processNames []string) bool {
	f.calls = append(f.calls, startCall{
		method:       "isRuntimeRunning",
		name:         name,
		processNames: processNames,
	})
	return f.isRuntimeRunningResult
}

func (f *fakeStartOps) killSession(name string) error {
	f.calls = append(f.calls, startCall{method: "killSession", name: name})
	return f.killErr
}

func (f *fakeStartOps) waitForCommand(_ context.Context, name string, timeout time.Duration) error {
	f.calls = append(f.calls, startCall{
		method:  "waitForCommand",
		name:    name,
		timeout: timeout,
	})
	if f.waitCommandHook != nil {
		f.waitCommandHook()
	}
	return f.waitCommandErr
}

func (f *fakeStartOps) acceptStartupDialogs(_ context.Context, name string) error {
	f.calls = append(f.calls, startCall{method: "acceptStartupDialogs", name: name})
	if f.acceptStartupDialogsHook != nil {
		f.acceptStartupDialogsHook()
	}
	return f.acceptStartupDialogsErr
}

func (f *fakeStartOps) waitForReady(_ context.Context, name string, rc *RuntimeConfig, timeout time.Duration) error {
	f.calls = append(f.calls, startCall{
		method:  "waitForReady",
		name:    name,
		rc:      rc,
		timeout: timeout,
	})
	if f.waitReadyHook != nil {
		f.waitReadyHook()
	}
	return f.waitReadyErr
}

func (f *fakeStartOps) hasSession(name string) (bool, error) {
	f.calls = append(f.calls, startCall{method: "hasSession", name: name})
	if f.hasSessionHook != nil {
		f.hasSessionHook()
	}
	return f.hasSessionResult, f.hasSessionErr
}

func (f *fakeStartOps) sendKeys(name, text string) error {
	f.calls = append(f.calls, startCall{method: "sendKeys", name: name, command: text})
	if f.sendKeysHook != nil {
		f.sendKeysHook()
	}
	return f.sendKeysErr
}

func (f *fakeStartOps) setRemainOnExit(name string) error {
	f.calls = append(f.calls, startCall{method: "setRemainOnExit", name: name})
	return f.setRemainOnExitErr
}

func (f *fakeStartOps) disableMouseAndActivity(name string) error {
	f.calls = append(f.calls, startCall{method: "disableMouseAndActivity", name: name})
	return f.disableMouseAndActivityErr
}

func (f *fakeStartOps) capturePane(name string, _ int) (string, error) {
	f.calls = append(f.calls, startCall{method: "capturePane", name: name})
	return f.capturePaneText, f.capturePaneErr
}

func (f *fakeStartOps) recordStartCrash(name, _ string) string {
	f.calls = append(f.calls, startCall{method: "recordStartCrash", name: name})
	return f.recordStartCrashPath
}

func (f *fakeStartOps) runSetupCommand(_ context.Context, cmd string, env map[string]string, timeout time.Duration) error {
	f.calls = append(f.calls, startCall{
		method:  "runSetupCommand",
		command: cmd,
		env:     env,
		timeout: timeout,
	})
	if f.runSetupCommandHook != nil {
		f.runSetupCommandHook(cmd)
	}
	if f.runSetupCommandErr != nil {
		return f.runSetupCommandErr
	}
	return nil
}

// callMethods returns just the method names for sequence assertions.
func (f *fakeStartOps) callMethods() []string {
	out := make([]string, len(f.calls))
	for i, c := range f.calls {
		out[i] = c.method
	}
	return out
}

// assertCallSequence is a helper that verifies the method call sequence.
func assertCallSequence(t *testing.T, ops *fakeStartOps, want []string) {
	t.Helper()
	got := ops.callMethods()
	if len(got) != len(want) {
		t.Fatalf("calls = %v, want %v", got, want)
	}
	for i, c := range got {
		if c != want[i] {
			t.Errorf("call %d = %q, want %q", i, c, want[i])
		}
	}
}

func containsMethod(methods []string, method string) bool {
	return methodIndex(methods, method) >= 0
}

func methodIndex(methods []string, method string) int {
	for i, got := range methods {
		if got == method {
			return i
		}
	}
	return -1
}

func callsByMethod(t *testing.T, ops *fakeStartOps, method string, wantCount int) []startCall {
	t.Helper()
	var matches []startCall
	for _, call := range ops.calls {
		if call.method == method {
			matches = append(matches, call)
		}
	}
	if len(matches) != wantCount {
		t.Fatalf("%s calls = %d, want %d; all calls = %v", method, len(matches), wantCount, ops.callMethods())
	}
	return matches
}

// ---------------------------------------------------------------------------
// doStartSession tests
// ---------------------------------------------------------------------------

func TestDoStartSession_FireAndForget(t *testing.T) {
	ops := &fakeStartOps{}

	err := doStartSession(context.Background(), ops, "test-sess", runtime.Config{
		WorkDir: "/w",
		Command: "sleep 300",
	}, DefaultConfig().SetupTimeout)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// No hints → createSession + session-level tmux options.
	assertCallSequence(t, ops, []string{"createSession", "setRemainOnExit", "disableMouseAndActivity"})

	// Verify arguments were passed through.
	c := ops.calls[0]
	if c.name != "test-sess" {
		t.Errorf("createSession name = %q, want %q", c.name, "test-sess")
	}
	if c.workDir != "/w" {
		t.Errorf("createSession workDir = %q, want %q", c.workDir, "/w")
	}
	if c.command != "sleep 300" {
		t.Errorf("createSession command = %q, want %q", c.command, "sleep 300")
	}
}

func TestDoStartSession_MouseOffDefaultDisables(t *testing.T) {
	ops := &fakeStartOps{}

	err := doStartSession(context.Background(), ops, "test-sess", runtime.Config{
		WorkDir: "/w",
		Command: "sleep 300",
	}, DefaultConfig().SetupTimeout)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	methods := ops.callMethods()
	if !containsMethod(methods, "disableMouseAndActivity") {
		t.Fatalf("disableMouseAndActivity not called; calls = %v", methods)
	}
	remainIdx := methodIndex(methods, "setRemainOnExit")
	disableIdx := methodIndex(methods, "disableMouseAndActivity")
	if remainIdx == -1 || disableIdx == -1 || disableIdx != remainIdx+1 {
		t.Fatalf("disableMouseAndActivity should immediately follow setRemainOnExit; calls = %v", methods)
	}
}

func TestDoStartSession_MouseOnSkipsDisable(t *testing.T) {
	ops := &fakeStartOps{}

	err := doStartSession(context.Background(), ops, "test-sess", runtime.Config{
		WorkDir: "/w",
		Command: "sleep 300",
		MouseOn: true,
	}, DefaultConfig().SetupTimeout)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	methods := ops.callMethods()
	if containsMethod(methods, "disableMouseAndActivity") {
		t.Fatalf("disableMouseAndActivity called with MouseOn=true; calls = %v", methods)
	}
}

func TestEnsureInstanceTokenReturnsErrorWhenReaderFails(t *testing.T) {
	oldReader := instanceTokenReader
	instanceTokenReader = errReader{}
	defer func() {
		instanceTokenReader = oldReader
	}()

	if _, err := ensureInstanceToken(nil); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("ensureInstanceToken error = %v, want %v", err, io.ErrUnexpectedEOF)
	}
}

func TestInjectSessionRuntimeHintsEnvAddsReadyPromptPrefix(t *testing.T) {
	env := injectSessionRuntimeHintsEnv(map[string]string{"GC_PROVIDER": "gemini"}, runtime.Config{
		ReadyPromptPrefix: "> ",
	})
	if got := env[sessionReadyPromptEnvKey]; got != "> " {
		t.Fatalf("%s = %q, want %q", sessionReadyPromptEnvKey, got, "> ")
	}
	if got := env["GC_PROVIDER"]; got != "gemini" {
		t.Fatalf("GC_PROVIDER = %q, want %q", got, "gemini")
	}
}

func TestInjectSessionRuntimeHintsEnvAddsProviderName(t *testing.T) {
	env := injectSessionRuntimeHintsEnv(nil, runtime.Config{
		ProviderName: "kimi",
	})
	if got := env["GC_PROVIDER"]; got != "kimi" {
		t.Fatalf("GC_PROVIDER = %q, want %q", got, "kimi")
	}
}

func TestInjectSessionRuntimeHintsEnvPreservesExplicitProvider(t *testing.T) {
	env := injectSessionRuntimeHintsEnv(map[string]string{"GC_PROVIDER": "custom"}, runtime.Config{
		ProviderName: "kimi",
	})
	if got := env["GC_PROVIDER"]; got != "custom" {
		t.Fatalf("GC_PROVIDER = %q, want %q", got, "custom")
	}
}

func TestDoStartSession_FullSequence(t *testing.T) {
	ops := &fakeStartOps{
		hasSessionResult: true,
	}

	cfg := runtime.Config{
		WorkDir:                "/proj",
		Command:                "claude",
		Env:                    map[string]string{"GC_AGENT": "mayor"},
		ReadyPromptPrefix:      "> ",
		ReadyDelayMs:           5000,
		ProcessNames:           []string{"claude", "node"},
		EmitsPermissionWarning: true,
	}

	err := doStartSession(context.Background(), ops, "gc-city-mayor", cfg, DefaultConfig().SetupTimeout)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	assertCallSequence(t, ops, []string{
		"createSession",
		"setRemainOnExit",
		"disableMouseAndActivity",
		"waitForCommand",
		"acceptStartupDialogs",
		"waitForReady",
		"acceptStartupDialogs",
		"hasSession",
		"isSessionRunning",
	})

	// Verify createSession got full config.
	create := ops.calls[0]
	if create.workDir != "/proj" {
		t.Errorf("createSession workDir = %q, want %q", create.workDir, "/proj")
	}
	if create.command != "claude" {
		t.Errorf("createSession command = %q, want %q", create.command, "claude")
	}
	if create.env["GC_AGENT"] != "mayor" {
		t.Errorf("createSession env = %v, want GC_AGENT=mayor", create.env)
	}

	// Verify session name flows to all ops.
	for i, c := range ops.calls {
		if c.name != "gc-city-mayor" {
			t.Errorf("call %d (%s): name = %q, want %q", i, c.method, c.name, "gc-city-mayor")
		}
	}

	// Verify waitForCommand got the right timeout.
	wfc := ops.calls[3]
	if wfc.timeout != 30*time.Second {
		t.Errorf("waitForCommand timeout = %v, want %v", wfc.timeout, 30*time.Second)
	}

	// Verify waitForReady got correct RuntimeConfig and timeout.
	wfr := ops.calls[5]
	if wfr.timeout != 10*time.Second {
		t.Errorf("waitForReady timeout = %v, want %v", wfr.timeout, 10*time.Second)
	}
	if wfr.rc == nil || wfr.rc.Tmux == nil {
		t.Fatal("waitForReady rc is nil")
	}
	if wfr.rc.Tmux.ReadyPromptPrefix != "> " {
		t.Errorf("rc.ReadyPromptPrefix = %q, want %q", wfr.rc.Tmux.ReadyPromptPrefix, "> ")
	}
	if wfr.rc.Tmux.ReadyDelayMs != 5000 {
		t.Errorf("rc.ReadyDelayMs = %d, want %d", wfr.rc.Tmux.ReadyDelayMs, 5000)
	}
	if len(wfr.rc.Tmux.ProcessNames) != 2 || wfr.rc.Tmux.ProcessNames[0] != "claude" {
		t.Errorf("rc.ProcessNames = %v, want [claude node]", wfr.rc.Tmux.ProcessNames)
	}
}

func TestDoStartSession_ReturnsContextCanceledAfterBestEffortReadyWait(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	ops := &fakeStartOps{
		hasSessionResult: true,
		waitReadyHook:    cancel,
	}

	cfg := runtime.Config{
		WorkDir:                "/proj",
		Command:                "claude",
		ReadyPromptPrefix:      "> ",
		ReadyDelayMs:           5000,
		ProcessNames:           []string{"claude"},
		EmitsPermissionWarning: true,
	}

	err := doStartSession(ctx, ops, "gc-city-mayor", cfg, DefaultConfig().SetupTimeout)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context canceled", err)
	}

	assertCallSequence(t, ops, []string{
		"createSession",
		"setRemainOnExit",
		"disableMouseAndActivity",
		"waitForCommand",
		"acceptStartupDialogs",
		"waitForReady",
	})
}

func TestDoStartSession_DoesNotRunSessionSetupAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	ops := &fakeStartOps{
		hasSessionResult: true,
		hasSessionHook:   cancel,
	}

	cfg := runtime.Config{
		Command:                "claude",
		ProcessNames:           []string{"claude"},
		ReadyPromptPrefix:      "> ",
		ReadyDelayMs:           1,
		EmitsPermissionWarning: true,
		SessionSetup:           []string{"echo setup"},
	}

	err := doStartSession(ctx, ops, "test", cfg, DefaultConfig().SetupTimeout)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context canceled", err)
	}
	for _, call := range ops.calls {
		if call.method == "runSetupCommand" {
			t.Fatalf("runSetupCommand should not execute after cancellation: %#v", ops.calls)
		}
	}
}

func TestDoStartSession_DoesNotNudgeAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	ops := &fakeStartOps{
		hasSessionResult:    true,
		runSetupCommandHook: func(_ string) { cancel() },
	}

	cfg := runtime.Config{
		Command:                "claude",
		ProcessNames:           []string{"claude"},
		ReadyPromptPrefix:      "> ",
		ReadyDelayMs:           1,
		EmitsPermissionWarning: true,
		SessionSetup:           []string{"echo setup"},
		Nudge:                  "hello",
	}

	err := doStartSession(ctx, ops, "test", cfg, DefaultConfig().SetupTimeout)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context canceled", err)
	}
	for _, call := range ops.calls {
		if call.method == "sendKeys" {
			t.Fatalf("sendKeys should not execute after cancellation: %#v", ops.calls)
		}
	}
}

func TestDoStartSession_DoesNotRunSessionLiveAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	ops := &fakeStartOps{
		hasSessionResult: true,
		sendKeysHook:     cancel,
	}

	cfg := runtime.Config{
		Command:                "claude",
		ProcessNames:           []string{"claude"},
		ReadyPromptPrefix:      "> ",
		ReadyDelayMs:           1,
		EmitsPermissionWarning: true,
		SessionSetup:           []string{"echo setup"},
		Nudge:                  "hello",
		SessionLive:            []string{"echo live"},
	}

	err := doStartSession(ctx, ops, "test", cfg, DefaultConfig().SetupTimeout)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context canceled", err)
	}
	liveCalls := 0
	for _, call := range ops.calls {
		if call.method == "runSetupCommand" && call.command == "echo live" {
			liveCalls++
		}
	}
	if liveCalls != 0 {
		t.Fatalf("session_live should not execute after cancellation: %#v", ops.calls)
	}
}

func TestDoStartSession_CreateFails(t *testing.T) {
	ops := &fakeStartOps{
		createErrs: []error{errors.New("tmux not found")},
	}

	err := doStartSession(context.Background(), ops, "test", runtime.Config{Command: "sleep 300"}, DefaultConfig().SetupTimeout)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "creating session") {
		t.Errorf("error = %q, want 'creating session'", err)
	}

	assertCallSequence(t, ops, []string{"createSession"})
}

func TestDoStartSession_CreateRetriesNoServer(t *testing.T) {
	ops := &fakeStartOps{
		createErrs: []error{ErrNoServer, nil},
	}

	err := doStartSession(context.Background(), ops, "test", runtime.Config{Command: "sleep 300"}, DefaultConfig().SetupTimeout)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	assertCallSequence(t, ops, []string{"createSession", "createSession", "setRemainOnExit", "disableMouseAndActivity"})
}

func TestDoStartSession_SessionDiesDuringStartup(t *testing.T) {
	ops := &fakeStartOps{
		hasSessionResult: false, // session died
	}

	cfg := runtime.Config{
		Command:      "claude",
		ProcessNames: []string{"claude"},
	}

	err := doStartSession(context.Background(), ops, "test", cfg, DefaultConfig().SetupTimeout)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "died during startup") {
		t.Errorf("error = %q, want 'died during startup'", err)
	}
	if !errors.Is(err, runtime.ErrSessionDiedDuringStartup) {
		t.Errorf("error = %v, want ErrSessionDiedDuringStartup", err)
	}
}

func TestDoStartSession_MissingFinalSessionDoesNotCapturePrefixSibling(t *testing.T) {
	ops := &fakeStartOps{
		hasSessionResult: false,
		capturePaneText:  "prefix sibling output must not leak",
	}

	cfg := runtime.Config{
		Command:      "codex",
		ProcessNames: []string{"codex"},
	}

	err := doStartSession(context.Background(), ops, "mayor", cfg, DefaultConfig().SetupTimeout)
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, runtime.ErrSessionDiedDuringStartup) {
		t.Fatalf("error = %v, want ErrSessionDiedDuringStartup", err)
	}
	if !strings.Contains(err.Error(), "session \"mayor\"") {
		t.Fatalf("error = %v, want session name", err)
	}
	if strings.Contains(err.Error(), "prefix sibling output") || strings.Contains(err.Error(), "last pane output") {
		t.Fatalf("error = %v, should not include pane output for missing exact session", err)
	}
	assertCallSequence(t, ops, []string{
		"createSession",
		"setRemainOnExit",
		"disableMouseAndActivity",
		"waitForCommand",
		"acceptStartupDialogs",
		"acceptStartupDialogs",
		"hasSession",
	})
}

func TestDoStartSession_ReadyDeadlineWithDeadPaneReportsProviderCrash(t *testing.T) {
	running := false
	ops := &fakeStartOps{
		waitReadyErr:           context.DeadlineExceeded,
		hasSessionResult:       true,
		isSessionRunningResult: &running,
		capturePaneText: "WARNING: proceeding, even though we could not update PATH: Operation not permitted (os error 1)\n" +
			"Error: Operation not permitted (os error 1)\n" +
			"Pane is dead",
	}

	cfg := runtime.Config{
		Command:           "codex",
		ProcessNames:      []string{"codex"},
		ReadyPromptPrefix: "› ",
	}

	err := doStartSession(context.Background(), ops, "mayor", cfg, DefaultConfig().SetupTimeout)
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, runtime.ErrSessionDiedDuringStartup) {
		t.Fatalf("error = %v, want ErrSessionDiedDuringStartup", err)
	}
	if strings.Contains(err.Error(), context.DeadlineExceeded.Error()) {
		t.Fatalf("error = %v, should not surface generic deadline after pane died", err)
	}
	for _, want := range []string{"session \"mayor\"", "Operation not permitted", "Pane is dead"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %v, want substring %q", err, want)
		}
	}
	assertCallSequence(t, ops, []string{
		"createSession",
		"setRemainOnExit",
		"disableMouseAndActivity",
		"waitForCommand",
		"acceptStartupDialogs",
		"waitForReady",
		"hasSession",
		"isSessionRunning",
		"capturePane",
		"recordStartCrash",
	})
}

func TestDoStartSession_FinalDeadPaneReportsProviderCrash(t *testing.T) {
	running := false
	ops := &fakeStartOps{
		hasSessionResult:       true,
		isSessionRunningResult: &running,
		capturePaneText:        "panic: startup failed\nPane is dead",
	}

	cfg := runtime.Config{
		Command:      "codex",
		ProcessNames: []string{"codex"},
	}

	err := doStartSession(context.Background(), ops, "mayor", cfg, DefaultConfig().SetupTimeout)
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, runtime.ErrSessionDiedDuringStartup) {
		t.Fatalf("error = %v, want ErrSessionDiedDuringStartup", err)
	}
	for _, want := range []string{"session \"mayor\"", "startup failed", "Pane is dead"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %v, want substring %q", err, want)
		}
	}
	assertCallSequence(t, ops, []string{
		"createSession",
		"setRemainOnExit",
		"disableMouseAndActivity",
		"waitForCommand",
		"acceptStartupDialogs",
		"acceptStartupDialogs",
		"hasSession",
		"isSessionRunning",
		"capturePane",
		"recordStartCrash",
	})
}

func TestDoStartSession_FinalDeadPaneCaptureErrorFallsBack(t *testing.T) {
	running := false
	ops := &fakeStartOps{
		hasSessionResult:       true,
		isSessionRunningResult: &running,
		capturePaneErr:         errors.New("capture failed"),
	}

	cfg := runtime.Config{
		Command:      "codex",
		ProcessNames: []string{"codex"},
	}

	err := doStartSession(context.Background(), ops, "mayor", cfg, DefaultConfig().SetupTimeout)
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, runtime.ErrSessionDiedDuringStartup) {
		t.Fatalf("error = %v, want ErrSessionDiedDuringStartup", err)
	}
	if !strings.Contains(err.Error(), "session \"mayor\"") {
		t.Fatalf("error = %v, want session name", err)
	}
	if strings.Contains(err.Error(), "last pane output") || strings.Contains(err.Error(), "capture failed") {
		t.Fatalf("error = %v, want fallback without pane/capture detail", err)
	}
	assertCallSequence(t, ops, []string{
		"createSession",
		"setRemainOnExit",
		"disableMouseAndActivity",
		"waitForCommand",
		"acceptStartupDialogs",
		"acceptStartupDialogs",
		"hasSession",
		"isSessionRunning",
		"capturePane",
		"recordStartCrash",
	})
}

func TestDoStartSession_DeadPaneRecordsDurableDiagnostic(t *testing.T) {
	running := false
	ops := &fakeStartOps{
		hasSessionResult:       true,
		isSessionRunningResult: &running,
		capturePaneText:        "panic: startup failed\nPane is dead",
		recordStartCrashPath:   "/city/.gc/runtime/sessions/mayor/start-stderr.log",
	}

	cfg := runtime.Config{
		Command:      "codex",
		ProcessNames: []string{"codex"},
	}

	err := doStartSession(context.Background(), ops, "mayor", cfg, DefaultConfig().SetupTimeout)
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, runtime.ErrSessionDiedDuringStartup) {
		t.Fatalf("error = %v, want ErrSessionDiedDuringStartup", err)
	}
	for _, want := range []string{"diagnostic written to", "start-stderr.log", "startup failed"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %v, want substring %q", err, want)
		}
	}
	assertCallSequence(t, ops, []string{
		"createSession",
		"setRemainOnExit",
		"disableMouseAndActivity",
		"waitForCommand",
		"acceptStartupDialogs",
		"acceptStartupDialogs",
		"hasSession",
		"isSessionRunning",
		"capturePane",
		"recordStartCrash",
	})
}

func TestDoStartSession_HasSessionError(t *testing.T) {
	ops := &fakeStartOps{
		hasSessionErr: errors.New("tmux crashed"),
	}

	cfg := runtime.Config{
		Command:      "claude",
		ProcessNames: []string{"claude"},
	}

	err := doStartSession(context.Background(), ops, "test", cfg, DefaultConfig().SetupTimeout)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "verifying session") {
		t.Errorf("error = %q, want 'verifying session'", err)
	}
}

// ---------------------------------------------------------------------------
// Individual hint tests — each hint field activates specific steps
// ---------------------------------------------------------------------------

func TestDoStartSession_ProcessNamesOnly(t *testing.T) {
	ops := &fakeStartOps{
		hasSessionResult: true,
	}

	cfg := runtime.Config{
		Command:      "codex",
		ProcessNames: []string{"codex"},
	}

	err := doStartSession(context.Background(), ops, "test", cfg, DefaultConfig().SetupTimeout)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// ProcessNames → waitForCommand + acceptStartupDialogs + hasSession.
	// No waitForReady.
	assertCallSequence(t, ops, []string{
		"createSession",
		"setRemainOnExit",
		"disableMouseAndActivity",
		"waitForCommand",
		"acceptStartupDialogs",
		"acceptStartupDialogs",
		"hasSession",
		"isSessionRunning",
	})

	// Verify isRuntimeRunning sees the process names in zombie detection path.
	// (Here create succeeded, so isRuntimeRunning isn't called.)
}

func TestDoStartSession_KimiSkipsStartupDialogAcceptance(t *testing.T) {
	ops := &fakeStartOps{
		hasSessionResult: true,
	}

	cfg := runtime.Config{
		Command:              "sh -c 'exec kimi --yolo --no-thinking'",
		ProviderName:         "wrapped-kimi",
		ProcessNames:         []string{"kimi", "python"},
		ReadyDelayMs:         5000,
		AcceptStartupDialogs: boolPtr(false),
	}

	err := doStartSession(context.Background(), ops, "test", cfg, DefaultConfig().SetupTimeout)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	assertCallSequence(t, ops, []string{
		"createSession",
		"setRemainOnExit",
		"disableMouseAndActivity",
		"waitForCommand",
		"waitForReady",
		"hasSession",
		"isSessionRunning",
	})
}

func TestDoStartSessionReturnsNudgeDeliveryError(t *testing.T) {
	ops := &fakeStartOps{
		hasSessionResult: true,
		sendKeysErr:      errors.New("command too long"),
	}

	cfg := runtime.Config{
		Command: "kimi",
		Nudge:   strings.Repeat("startup prompt\n", 100),
	}

	err := doStartSession(context.Background(), ops, "test", cfg, DefaultConfig().SetupTimeout)
	if err == nil {
		t.Fatal("expected startup nudge delivery error, got nil")
	}
	if !strings.Contains(err.Error(), "sending startup nudge") {
		t.Fatalf("error = %v, want startup nudge context", err)
	}
	if !strings.Contains(err.Error(), "command too long") {
		t.Fatalf("error = %v, want original nudge error", err)
	}

	assertCallSequence(t, ops, []string{
		"createSession",
		"setRemainOnExit",
		"disableMouseAndActivity",
		"hasSession",
		"isSessionRunning",
		"sendKeys",
	})
}

func TestDoStartSession_AcceptStartupDialogsOnly(t *testing.T) {
	ops := &fakeStartOps{
		hasSessionResult: true,
	}

	cfg := runtime.Config{
		Command:              "custom-agent",
		AcceptStartupDialogs: boolPtr(true),
	}

	err := doStartSession(context.Background(), ops, "test", cfg, DefaultConfig().SetupTimeout)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	assertCallSequence(t, ops, []string{
		"createSession",
		"setRemainOnExit",
		"disableMouseAndActivity",
		"acceptStartupDialogs",
		"acceptStartupDialogs",
		"hasSession",
		"isSessionRunning",
	})
}

func TestShouldAcceptStartupDialogsProviderResolution(t *testing.T) {
	tests := []struct {
		name string
		cfg  runtime.Config
		want bool
	}{
		{
			name: "explicit runtime config skips startup dialogs",
			cfg: runtime.Config{
				ProviderName:         "custom-kimi",
				Command:              "sh -c 'kimi --yolo'",
				ProcessNames:         []string{"kimi"},
				AcceptStartupDialogs: boolPtr(false),
			},
			want: false,
		},
		{
			name: "explicit runtime config accepts startup dialogs",
			cfg: runtime.Config{
				ProviderName:         "custom-provider",
				ProcessNames:         []string{"custom"},
				AcceptStartupDialogs: boolPtr(true),
			},
			want: true,
		},
		{
			name: "empty command keeps conservative dialog acceptance",
			cfg: runtime.Config{
				ProcessNames: []string{"unknown"},
			},
			want: true,
		},
		{
			name: "explicit non-kimi accepts startup dialogs",
			cfg: runtime.Config{
				ProviderName: "codex",
				ProcessNames: []string{"codex"},
			},
			want: true,
		},
		{
			name: "no startup dialog hint skips acceptance",
			cfg:  runtime.Config{},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldAcceptStartupDialogs(tt.cfg); got != tt.want {
				t.Fatalf("shouldAcceptStartupDialogs() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDoStartSession_ReadyPromptPrefixOnly(t *testing.T) {
	ops := &fakeStartOps{
		hasSessionResult: true,
	}

	cfg := runtime.Config{
		Command:           "gemini",
		ReadyPromptPrefix: "❯ ",
	}

	err := doStartSession(context.Background(), ops, "test", cfg, DefaultConfig().SetupTimeout)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// ReadyPromptPrefix → waitForReady + hasSession.
	// No waitForCommand (no ProcessNames), no acceptBypassWarning.
	assertCallSequence(t, ops, []string{
		"createSession",
		"setRemainOnExit",
		"disableMouseAndActivity",
		"waitForReady",
		"hasSession",
		"isSessionRunning",
	})

	// Verify RuntimeConfig carries the prefix.
	wfr := ops.calls[3]
	if wfr.rc.Tmux.ReadyPromptPrefix != "❯ " {
		t.Errorf("rc.ReadyPromptPrefix = %q, want %q", wfr.rc.Tmux.ReadyPromptPrefix, "❯ ")
	}
}

func TestDoStartSession_ReadyDelayOnly(t *testing.T) {
	ops := &fakeStartOps{
		hasSessionResult: true,
	}

	cfg := runtime.Config{
		Command:      "gemini",
		ReadyDelayMs: 3000,
	}

	err := doStartSession(context.Background(), ops, "test", cfg, DefaultConfig().SetupTimeout)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	assertCallSequence(t, ops, []string{
		"createSession",
		"setRemainOnExit",
		"disableMouseAndActivity",
		"waitForReady",
		"hasSession",
		"isSessionRunning",
	})

	// Verify RuntimeConfig carries the delay.
	wfr := ops.calls[3]
	if wfr.rc.Tmux.ReadyDelayMs != 3000 {
		t.Errorf("rc.ReadyDelayMs = %d, want %d", wfr.rc.Tmux.ReadyDelayMs, 3000)
	}
}

func TestDoStartSession_TreatsDeadlineAfterReadyAsSuccessWhenSessionAlive(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	ops := &fakeStartOps{
		hasSessionResult: true,
		waitReadyHook: func() {
			// Block until context expires so ctx.Err() is guaranteed non-nil when
			// the hook returns. time.Sleep(N) races with the context timer under
			// high parallel load: if the timer goroutine is delayed, ctx.Err() can
			// return nil after the sleep, causing an extra acceptStartupDialogs call.
			<-ctx.Done()
		},
	}

	cfg := runtime.Config{
		WorkDir:                "/proj",
		Command:                "claude",
		ReadyPromptPrefix:      "> ",
		ReadyDelayMs:           5000,
		ProcessNames:           []string{"claude"},
		EmitsPermissionWarning: true,
	}

	err := doStartSession(ctx, ops, "gc-city-mayor", cfg, DefaultConfig().SetupTimeout)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}

	assertCallSequence(t, ops, []string{
		"createSession",
		"setRemainOnExit",
		"disableMouseAndActivity",
		"waitForCommand",
		"acceptStartupDialogs",
		"waitForReady",
		"hasSession",
		"isSessionRunning",
	})
}

func TestDoStartSession_TreatsDeadlineAfterPostReadyAsSuccessWhenSessionAlive(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	postReadyCalls := 0
	ops := &fakeStartOps{
		hasSessionResult: true,
		acceptStartupDialogsHook: func() {
			postReadyCalls++
			if postReadyCalls == 2 {
				time.Sleep(5 * time.Millisecond)
			}
		},
	}

	cfg := runtime.Config{
		WorkDir:                "/proj",
		Command:                "claude",
		ReadyPromptPrefix:      "> ",
		ReadyDelayMs:           5000,
		ProcessNames:           []string{"claude"},
		EmitsPermissionWarning: true,
	}

	err := doStartSession(ctx, ops, "gc-city-mayor", cfg, DefaultConfig().SetupTimeout)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}

	assertCallSequence(t, ops, []string{
		"createSession",
		"setRemainOnExit",
		"disableMouseAndActivity",
		"waitForCommand",
		"acceptStartupDialogs",
		"waitForReady",
		"acceptStartupDialogs",
		"hasSession",
		"isSessionRunning",
	})
}

func TestDoStartSession_EmitsPermissionWarningOnly(t *testing.T) {
	ops := &fakeStartOps{
		hasSessionResult: true,
	}

	cfg := runtime.Config{
		Command:                "claude",
		EmitsPermissionWarning: true,
	}

	err := doStartSession(context.Background(), ops, "test", cfg, DefaultConfig().SetupTimeout)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// EmitsPermissionWarning → acceptStartupDialogs + hasSession.
	// No waitForCommand (no ProcessNames), no waitForReady (no prefix/delay).
	assertCallSequence(t, ops, []string{
		"createSession",
		"setRemainOnExit",
		"disableMouseAndActivity",
		"acceptStartupDialogs",
		"acceptStartupDialogs",
		"hasSession",
		"isSessionRunning",
	})
}

func TestDoStartSession_ProcessNamesAndReadyPrefix(t *testing.T) {
	ops := &fakeStartOps{
		hasSessionResult: true,
	}

	cfg := runtime.Config{
		Command:           "claude",
		ProcessNames:      []string{"claude"},
		ReadyPromptPrefix: "> ",
	}

	err := doStartSession(context.Background(), ops, "test", cfg, DefaultConfig().SetupTimeout)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Both ProcessNames and ReadyPromptPrefix — acceptStartupDialogs always runs.
	assertCallSequence(t, ops, []string{
		"createSession",
		"setRemainOnExit",
		"disableMouseAndActivity",
		"waitForCommand",
		"acceptStartupDialogs",
		"waitForReady",
		"acceptStartupDialogs",
		"hasSession",
		"isSessionRunning",
	})
}

func TestDoStartSession_CursorReadinessHintsTriggerRuntimeWait(t *testing.T) {
	ops := &fakeStartOps{
		hasSessionResult: true,
	}

	cfg := runtime.Config{
		Command:           "cursor-agent",
		ProcessNames:      []string{"cursor-agent"},
		ReadyPromptPrefix: "\u2192 ",
		ReadyDelayMs:      10000,
	}

	err := doStartSession(context.Background(), ops, "test", cfg, DefaultConfig().SetupTimeout)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	assertCallSequence(t, ops, []string{
		"createSession",
		"setRemainOnExit",
		"disableMouseAndActivity",
		"waitForCommand",
		"acceptStartupDialogs",
		"waitForReady",
		"acceptStartupDialogs",
		"hasSession",
		"isSessionRunning",
	})

	wfr := ops.calls[5]
	if wfr.rc.Tmux.ReadyPromptPrefix != "\u2192 " {
		t.Errorf("rc.ReadyPromptPrefix = %q, want %q", wfr.rc.Tmux.ReadyPromptPrefix, "\u2192 ")
	}
	if wfr.rc.Tmux.ReadyDelayMs != 10000 {
		t.Errorf("rc.ReadyDelayMs = %d, want %d", wfr.rc.Tmux.ReadyDelayMs, 10000)
	}
	if len(wfr.rc.Tmux.ProcessNames) != 1 || wfr.rc.Tmux.ProcessNames[0] != "cursor-agent" {
		t.Errorf("rc.ProcessNames = %v, want [cursor-agent]", wfr.rc.Tmux.ProcessNames)
	}
}

func TestDoStartSession_ProcessNamesAndReadyDelayRechecksDialogs(t *testing.T) {
	ops := &fakeStartOps{
		hasSessionResult: true,
	}

	cfg := runtime.Config{
		Command:      "codex",
		ProcessNames: []string{"codex"},
		ReadyDelayMs: 3000,
	}

	err := doStartSession(context.Background(), ops, "test", cfg, DefaultConfig().SetupTimeout)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	assertCallSequence(t, ops, []string{
		"createSession",
		"setRemainOnExit",
		"disableMouseAndActivity",
		"waitForCommand",
		"acceptStartupDialogs",
		"waitForReady",
		"acceptStartupDialogs",
		"hasSession",
		"isSessionRunning",
	})
}

func TestDoStartSession_SetRemainOnExit(t *testing.T) {
	// Even fire-and-forget agents get remain-on-exit.
	ops := &fakeStartOps{}

	err := doStartSession(context.Background(), ops, "test-sess", runtime.Config{
		WorkDir: "/w",
		Command: "sleep 300",
	}, DefaultConfig().SetupTimeout)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	assertCallSequence(t, ops, []string{"createSession", "setRemainOnExit", "disableMouseAndActivity"})

	// Verify session name passed through.
	c := ops.calls[1]
	if c.name != "test-sess" {
		t.Errorf("setRemainOnExit name = %q, want %q", c.name, "test-sess")
	}
}

func TestDoStartSession_SetRemainOnExitErrorIgnored(t *testing.T) {
	// setRemainOnExit error is best-effort — startup still succeeds.
	ops := &fakeStartOps{
		setRemainOnExitErr: errors.New("tmux option not supported"),
	}

	err := doStartSession(context.Background(), ops, "test", runtime.Config{
		WorkDir: "/w",
		Command: "sleep 300",
	}, DefaultConfig().SetupTimeout)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	assertCallSequence(t, ops, []string{"createSession", "setRemainOnExit", "disableMouseAndActivity"})
}

func TestStartupReadyProbeTimeoutUsesReadyDelayBudget(t *testing.T) {
	cfg := runtime.Config{
		ReadyPromptPrefix: "> ",
		ReadyDelayMs:      2500,
	}
	if got, want := startupReadyProbeTimeout(cfg), 7500*time.Millisecond; got != want {
		t.Fatalf("startupReadyProbeTimeout() = %v, want %v", got, want)
	}
}

func TestStartupReadyProbeTimeoutFallsBackForPromptOnly(t *testing.T) {
	cfg := runtime.Config{
		ReadyPromptPrefix: "> ",
	}
	if got, want := startupReadyProbeTimeout(cfg), 15*time.Second; got != want {
		t.Fatalf("startupReadyProbeTimeout() = %v, want %v", got, want)
	}
}

func TestDoStartSession_OneShotLifecycleSkipsPostStartNudgeChecks(t *testing.T) {
	ops := &fakeStartOps{
		hasSessionResult:   false,
		setRemainOnExitErr: ErrNoServer,
	}

	err := doStartSession(context.Background(), ops, "test", runtime.Config{
		WorkDir:   "/w",
		Command:   "true",
		Lifecycle: runtime.LifecycleOneShot,
		Nudge:     "start working",
	}, DefaultConfig().SetupTimeout)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	assertCallSequence(t, ops, []string{"createSession", "setRemainOnExit", "disableMouseAndActivity"})
}

// ---------------------------------------------------------------------------
// ensureFreshSession tests
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// Session setup tests
// ---------------------------------------------------------------------------

func TestDoStartSession_SessionSetupRunsAfterAlive(t *testing.T) {
	ops := &fakeStartOps{
		hasSessionResult: true,
	}

	cfg := runtime.Config{
		Command:      "claude",
		ProcessNames: []string{"claude"},
		SessionSetup: []string{
			"tmux set-option -t test status-style 'bg=blue'",
			"tmux set-option -t test mouse on",
		},
	}

	err := doStartSession(context.Background(), ops, "test", cfg, DefaultConfig().SetupTimeout)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Setup commands run between hasSession and sendKeys (no nudge here).
	assertCallSequence(t, ops, []string{
		"createSession",
		"setRemainOnExit",
		"disableMouseAndActivity",
		"waitForCommand",
		"acceptStartupDialogs",
		"acceptStartupDialogs",
		"hasSession",
		"isSessionRunning",
		"runSetupCommand",
		"runSetupCommand",
	})

	setupCalls := callsByMethod(t, ops, "runSetupCommand", 2)
	cmd1 := setupCalls[0]
	if cmd1.command != "tmux set-option -t test status-style 'bg=blue'" {
		t.Errorf("setup cmd[0] = %q, want status-style command", cmd1.command)
	}
	cmd2 := setupCalls[1]
	if cmd2.command != "tmux set-option -t test mouse on" {
		t.Errorf("setup cmd[1] = %q, want mouse command", cmd2.command)
	}

	// Verify GC_SESSION env var.
	if cmd1.env["GC_SESSION"] != "test" {
		t.Errorf("GC_SESSION = %q, want %q", cmd1.env["GC_SESSION"], "test")
	}
}

func TestDoStartSession_SessionSetupScriptRunsAfterCommands(t *testing.T) {
	ops := &fakeStartOps{
		hasSessionResult: true,
	}

	cfg := runtime.Config{
		Command:            "claude",
		ProcessNames:       []string{"claude"},
		SessionSetup:       []string{"tmux set mouse on"},
		SessionSetupScript: "/city/scripts/setup.sh",
		Nudge:              "start working",
	}

	err := doStartSession(context.Background(), ops, "test", cfg, DefaultConfig().SetupTimeout)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Order: create, remain, wait, dialogs, hasSession, setup cmd, setup script, nudge.
	assertCallSequence(t, ops, []string{
		"createSession",
		"setRemainOnExit",
		"disableMouseAndActivity",
		"waitForCommand",
		"acceptStartupDialogs",
		"acceptStartupDialogs",
		"hasSession",
		"isSessionRunning",
		"runSetupCommand",
		"runSetupCommand",
		"sendKeys",
	})

	setupCalls := callsByMethod(t, ops, "runSetupCommand", 2)
	nudgeCalls := callsByMethod(t, ops, "sendKeys", 1)

	// First runSetupCommand = inline command.
	if setupCalls[0].command != "tmux set mouse on" {
		t.Errorf("setup[0] = %q, want inline command", setupCalls[0].command)
	}
	// Second runSetupCommand = script.
	if setupCalls[1].command != "/city/scripts/setup.sh" {
		t.Errorf("setup[1] = %q, want script", setupCalls[1].command)
	}
	// sendKeys = nudge.
	if nudgeCalls[0].command != "start working" {
		t.Errorf("nudge = %q, want %q", nudgeCalls[0].command, "start working")
	}
}

func TestDoStartSession_NoSetupConfigured(t *testing.T) {
	ops := &fakeStartOps{
		hasSessionResult: true,
	}

	cfg := runtime.Config{
		Command:      "claude",
		ProcessNames: []string{"claude"},
	}

	err := doStartSession(context.Background(), ops, "test", cfg, DefaultConfig().SetupTimeout)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// No setup commands should appear.
	for _, c := range ops.calls {
		if c.method == "runSetupCommand" {
			t.Error("unexpected runSetupCommand call with no setup configured")
		}
	}
}

func TestDoStartSession_SessionSetupFailureNonFatal(t *testing.T) {
	ops := &fakeStartOps{
		hasSessionResult:   true,
		runSetupCommandErr: errors.New("tmux option not supported"),
	}

	cfg := runtime.Config{
		Command:      "claude",
		ProcessNames: []string{"claude"},
		SessionSetup: []string{"tmux bad-command"},
		Nudge:        "continue",
	}

	err := doStartSession(context.Background(), ops, "test", cfg, DefaultConfig().SetupTimeout)
	if err != nil {
		t.Fatalf("setup failure should be non-fatal, got: %v", err)
	}

	// Nudge should still run after failed setup.
	methods := ops.callMethods()
	last := methods[len(methods)-1]
	if last != "sendKeys" {
		t.Errorf("last call = %q, want sendKeys (nudge after setup failure)", last)
	}
}

func TestDoStartSession_SetupOnlyTriggersHints(t *testing.T) {
	// session_setup alone should trigger the hints path (not fire-and-forget).
	ops := &fakeStartOps{
		hasSessionResult: true,
	}

	cfg := runtime.Config{
		Command:      "sleep 300",
		SessionSetup: []string{"tmux set mouse on"},
	}

	err := doStartSession(context.Background(), ops, "test", cfg, DefaultConfig().SetupTimeout)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should include hasSession (verify alive) and runSetupCommand.
	var hasSetup, hasVerify bool
	for _, c := range ops.calls {
		if c.method == "runSetupCommand" {
			hasSetup = true
		}
		if c.method == "hasSession" {
			hasVerify = true
		}
	}
	if !hasVerify {
		t.Error("expected hasSession call (verify alive)")
	}
	if !hasSetup {
		t.Error("expected runSetupCommand call")
	}
}

func TestDoStartSession_SetupScriptOnlyTriggersHints(t *testing.T) {
	ops := &fakeStartOps{
		hasSessionResult: true,
	}

	cfg := runtime.Config{
		Command:            "sleep 300",
		SessionSetupScript: "/city/scripts/setup.sh",
	}

	err := doStartSession(context.Background(), ops, "test", cfg, DefaultConfig().SetupTimeout)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var hasSetup bool
	for _, c := range ops.calls {
		if c.method == "runSetupCommand" {
			hasSetup = true
		}
	}
	if !hasSetup {
		t.Error("expected runSetupCommand call for script")
	}
}

func TestDoStartSession_PreStartRunsBeforeCreate(t *testing.T) {
	ops := &fakeStartOps{
		hasSessionResult: true,
	}

	cfg := runtime.Config{
		Command:  "claude",
		WorkDir:  "/proj",
		PreStart: []string{"setup-worktree"},
	}

	err := doStartSession(context.Background(), ops, "test", cfg, DefaultConfig().SetupTimeout)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	assertCallSequence(t, ops, []string{"runSetupCommand", "createSession", "setRemainOnExit", "disableMouseAndActivity", "hasSession", "isSessionRunning"})

	pre := ops.calls[0]
	if pre.command != "setup-worktree" {
		t.Errorf("pre_start command = %q, want %q", pre.command, "setup-worktree")
	}
	if pre.timeout != DefaultConfig().SetupTimeout {
		t.Errorf("pre_start timeout = %v, want %v", pre.timeout, DefaultConfig().SetupTimeout)
	}
}

func TestDoStartSession_PreStartFailureIsFatal(t *testing.T) {
	ops := &fakeStartOps{
		runSetupCommandErr: errors.New("context canceled"),
	}

	cfg := runtime.Config{
		Command:  "claude",
		WorkDir:  "/proj",
		PreStart: []string{"setup-worktree"},
	}

	err := doStartSession(context.Background(), ops, "test", cfg, DefaultConfig().SetupTimeout)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "running pre_start") {
		t.Fatalf("error = %q, want running pre_start", err)
	}

	assertCallSequence(t, ops, []string{"runSetupCommand"})
}

func TestRunSetupCommandIncludesStderrOnFailure(t *testing.T) {
	ops := &tmuxStartOps{tm: &Tmux{}}

	err := ops.runSetupCommand(
		context.Background(),
		"printf 'OpenBao read failed for secret/path' >&2; exit 3",
		map[string]string{},
		time.Second,
	)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "exit status 3") {
		t.Fatalf("error = %q, want exit status", err)
	}
	if !strings.Contains(err.Error(), "stderr: OpenBao read failed for secret/path") {
		t.Fatalf("error = %q, want stderr detail", err)
	}
}

func TestRunSetupCommandFallsBackToStdoutDetail(t *testing.T) {
	ops := &tmuxStartOps{tm: &Tmux{}}

	err := ops.runSetupCommand(
		context.Background(),
		"printf 'wrote state to /tmp/x'; exit 4",
		map[string]string{},
		time.Second,
	)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "exit status 4") {
		t.Fatalf("error = %q, want exit status", err)
	}
	if !strings.Contains(err.Error(), "stdout: wrote state to /tmp/x") {
		t.Fatalf("error = %q, want stdout detail", err)
	}
}

func TestRunSetupCommandIncludesBothStreamDetails(t *testing.T) {
	ops := &tmuxStartOps{tm: &Tmux{}}

	err := ops.runSetupCommand(
		context.Background(),
		"printf 'actionable stdout'; printf 'noisy stderr' >&2; exit 5",
		map[string]string{},
		time.Second,
	)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "stderr: noisy stderr") {
		t.Fatalf("error = %q, want stderr detail", err)
	}
	if !strings.Contains(err.Error(), "stdout: actionable stdout") {
		t.Fatalf("error = %q, want stdout detail", err)
	}
}

func TestRunSetupCommandTimeoutMatchesDeadlineExceeded(t *testing.T) {
	ops := &tmuxStartOps{tm: &Tmux{}}

	err := ops.runSetupCommand(
		context.Background(),
		"echo started; sleep 30",
		map[string]string{},
		500*time.Millisecond,
	)
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %q, want errors.Is DeadlineExceeded", err)
	}
	if !strings.Contains(err.Error(), "stdout: started") {
		t.Fatalf("error = %q, want partial output captured before timeout", err)
	}
}

// TestRunSetupCommandBackgroundChildSucceedsBounded is the regression for
// setup commands that daemonize a child inheriting stdio: without
// Cmd.WaitDelay the capture pipes never reach EOF and Run blocks until the
// descendant exits, far past setup_timeout. The command itself exits 0, so
// it must be reported as success once the pipes are force-closed.
func TestRunSetupCommandBackgroundChildSucceedsBounded(t *testing.T) {
	ops := &tmuxStartOps{tm: &Tmux{}}

	start := time.Now()
	err := ops.runSetupCommand(
		context.Background(),
		"sleep 30 & exit 0",
		map[string]string{},
		5*time.Second,
	)
	elapsed := time.Since(start)
	if elapsed >= 10*time.Second {
		t.Fatalf("runSetupCommand blocked %v on a background child holding stdio", elapsed)
	}
	if err != nil {
		t.Fatalf("daemonizing setup command exiting 0 should succeed, got %v", err)
	}
}

func TestRunSetupCommandBackgroundChildFailureBounded(t *testing.T) {
	ops := &tmuxStartOps{tm: &Tmux{}}

	start := time.Now()
	err := ops.runSetupCommand(
		context.Background(),
		"printf 'daemon prestart broke' >&2; sleep 30 & exit 7",
		map[string]string{},
		5*time.Second,
	)
	elapsed := time.Since(start)
	if elapsed >= 10*time.Second {
		t.Fatalf("runSetupCommand blocked %v on a background child holding stdio", elapsed)
	}
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "exit status 7") {
		t.Fatalf("error = %q, want exit status", err)
	}
	if !strings.Contains(err.Error(), "stderr: daemon prestart broke") {
		t.Fatalf("error = %q, want stderr detail", err)
	}
}

func TestCommandOutputTail(t *testing.T) {
	cases := []struct {
		name   string
		limit  int
		writes []string
		label  string
		want   string
	}{
		{name: "no output", limit: 8, writes: nil, label: "stderr", want: ""},
		{name: "whitespace only", limit: 8, writes: []string{" \n\t "}, label: "stderr", want: ""},
		{name: "under limit", limit: 8, writes: []string{"abc"}, label: "stderr", want: "stderr: abc"},
		{name: "exact limit has no marker", limit: 4, writes: []string{"abcd"}, label: "stderr", want: "stderr: abcd"},
		{name: "oversized single write keeps tail", limit: 4, writes: []string{"abcdefgh"}, label: "stderr", want: "stderr: ... efgh"},
		{name: "rollover across writes", limit: 4, writes: []string{"abc", "def"}, label: "stderr", want: "stderr: ... cdef"},
		{name: "many small writes", limit: 3, writes: []string{"a", "b", "c", "d", "e"}, label: "stdout", want: "stdout: ... cde"},
		{name: "zero limit drops content", limit: 0, writes: []string{"abc"}, label: "stderr", want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tail := newCommandOutputTail(tc.limit)
			for _, w := range tc.writes {
				n, err := tail.Write([]byte(w))
				if err != nil {
					t.Fatalf("Write(%q) error: %v", w, err)
				}
				if n != len(w) {
					t.Fatalf("Write(%q) = %d, want %d", w, n, len(w))
				}
			}
			if got := tail.Detail(tc.label); got != tc.want {
				t.Fatalf("Detail(%q) = %q, want %q", tc.label, got, tc.want)
			}
		})
	}
}

func TestDoStartSession_SetupEnvPassthrough(t *testing.T) {
	ops := &fakeStartOps{
		hasSessionResult: true,
	}

	cfg := runtime.Config{
		Command:      "claude",
		ProcessNames: []string{"claude"},
		Env:          map[string]string{"GC_AGENT": "mayor", "GC_CITY": "/city"},
		SessionSetup: []string{"echo setup"},
	}

	err := doStartSession(context.Background(), ops, "test-sess", cfg, DefaultConfig().SetupTimeout)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Find runSetupCommand call.
	for _, c := range ops.calls {
		if c.method == "runSetupCommand" {
			if c.env["GC_SESSION"] != "test-sess" {
				t.Errorf("GC_SESSION = %q, want %q", c.env["GC_SESSION"], "test-sess")
			}
			if c.env["GC_AGENT"] != "mayor" {
				t.Errorf("GC_AGENT = %q, want %q", c.env["GC_AGENT"], "mayor")
			}
			if c.env["GC_CITY"] != "/city" {
				t.Errorf("GC_CITY = %q, want %q", c.env["GC_CITY"], "/city")
			}
			return
		}
	}
	t.Error("no runSetupCommand call found")
}

// ---------------------------------------------------------------------------
// ensureFreshSession tests
// ---------------------------------------------------------------------------

func TestEnsureFreshSession_Success(t *testing.T) {
	ops := &fakeStartOps{}

	cfg := runtime.Config{
		WorkDir: "/proj",
		Command: "claude",
		Env:     map[string]string{"GC_AGENT": "mayor"},
	}
	err := ensureFreshSession(ops, "gc-test", cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	assertCallSequence(t, ops, []string{"createSession"})

	// Verify config passed through.
	c := ops.calls[0]
	if c.name != "gc-test" {
		t.Errorf("name = %q, want %q", c.name, "gc-test")
	}
	if c.workDir != "/proj" {
		t.Errorf("workDir = %q, want %q", c.workDir, "/proj")
	}
	if c.command != "claude" {
		t.Errorf("command = %q, want %q", c.command, "claude")
	}
	if c.env["GC_AGENT"] != "mayor" {
		t.Errorf("env = %v, want GC_AGENT=mayor", c.env)
	}
}

func TestEnsureFreshSession_ZombieDetection(t *testing.T) {
	running := true
	ops := &fakeStartOps{
		isSessionRunningResult: &running,
		createErrs:             []error{ErrSessionExists},
		isRuntimeRunningResult: false, // zombie
	}

	cfg := runtime.Config{
		WorkDir:      "/proj",
		Command:      "claude",
		ProcessNames: []string{"claude", "node"},
	}
	err := ensureFreshSession(ops, "gc-test", cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	assertCallSequence(t, ops, []string{
		"createSession",
		"isSessionRunning",
		"isRuntimeRunning",
		"killSession",
		"createSession",
	})

	// Verify isRuntimeRunning received the ProcessNames from config.
	irt := ops.calls[2]
	if len(irt.processNames) != 2 || irt.processNames[0] != "claude" || irt.processNames[1] != "node" {
		t.Errorf("isRuntimeRunning processNames = %v, want [claude node]", irt.processNames)
	}

	// Verify recreate (second createSession) passes same config as initial.
	first := ops.calls[0]
	second := ops.calls[4]
	if first.workDir != second.workDir {
		t.Errorf("recreate workDir = %q, initial = %q", second.workDir, first.workDir)
	}
	if first.command != second.command {
		t.Errorf("recreate command = %q, initial = %q", second.command, first.command)
	}
}

func TestEnsureFreshSession_HealthyExisting(t *testing.T) {
	running := true
	ops := &fakeStartOps{
		isSessionRunningResult: &running,
		createErrs:             []error{ErrSessionExists},
		isRuntimeRunningResult: true, // alive
	}

	err := ensureFreshSession(ops, "test", runtime.Config{
		Command:      "claude",
		ProcessNames: []string{"claude"},
	})
	if err == nil {
		t.Fatal("expected error for duplicate session")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("error = %q, want 'already exists'", err)
	}

	// Should not kill or recreate.
	assertCallSequence(t, ops, []string{"createSession", "isSessionRunning", "isRuntimeRunning"})
}

func TestEnsureFreshSession_DuplicateNoProcessNames(t *testing.T) {
	running := true
	ops := &fakeStartOps{
		isSessionRunningResult: &running,
		createErrs:             []error{ErrSessionExists},
	}

	// Without ProcessNames, a live session is still treated as a duplicate.
	err := ensureFreshSession(ops, "test", runtime.Config{
		Command: "sleep 300",
	})
	if err == nil {
		t.Fatal("expected error for duplicate session")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("error = %q, want 'already exists'", err)
	}

	// Should not call isRuntimeRunning or kill.
	assertCallSequence(t, ops, []string{"createSession", "isSessionRunning"})
}

func TestEnsureFreshSession_DeadPaneWithoutProcessNames(t *testing.T) {
	running := false
	ops := &fakeStartOps{
		isSessionRunningResult: &running,
		createErrs:             []error{ErrSessionExists},
	}

	err := ensureFreshSession(ops, "test", runtime.Config{
		Command: "sleep 300",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	assertCallSequence(t, ops, []string{
		"createSession",
		"isSessionRunning",
		"killSession",
		"createSession",
	})
}

func TestEnsureFreshSession_ZombieKillFails(t *testing.T) {
	running := true
	ops := &fakeStartOps{
		isSessionRunningResult: &running,
		createErrs:             []error{ErrSessionExists},
		isRuntimeRunningResult: false, // zombie
		killErr:                errors.New("permission denied"),
	}

	err := ensureFreshSession(ops, "test", runtime.Config{
		Command:      "claude",
		ProcessNames: []string{"claude"},
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "killing zombie session") {
		t.Errorf("error = %q, want 'killing zombie session'", err)
	}
}

func TestEnsureFreshSession_RecreateRace(t *testing.T) {
	// After zombie kill, recreate gets ErrSessionExists from a concurrent process.
	running := true
	ops := &fakeStartOps{
		isSessionRunningResult: &running,
		createErrs:             []error{ErrSessionExists, ErrSessionExists},
		isRuntimeRunningResult: false, // zombie
	}

	err := ensureFreshSession(ops, "test", runtime.Config{
		Command:      "claude",
		ProcessNames: []string{"claude"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v (race should be tolerated)", err)
	}
}

func TestEnsureFreshSession_RecreateFails(t *testing.T) {
	running := true
	ops := &fakeStartOps{
		isSessionRunningResult: &running,
		createErrs:             []error{ErrSessionExists, errors.New("out of memory")},
		isRuntimeRunningResult: false, // zombie
	}

	err := ensureFreshSession(ops, "test", runtime.Config{
		Command:      "claude",
		ProcessNames: []string{"claude"},
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "creating session after zombie cleanup") {
		t.Errorf("error = %q, want 'creating session after zombie cleanup'", err)
	}
}

func TestEnsureFreshSession_DeadPaneCleanupRetriesNoServer(t *testing.T) {
	running := false
	ops := &fakeStartOps{
		isSessionRunningResult: &running,
		createErrs:             []error{ErrSessionExists, ErrNoServer, nil},
	}

	err := ensureFreshSession(ops, "test", runtime.Config{
		Command: "sleep 300",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertCallSequence(t, ops, []string{"createSession", "isSessionRunning", "killSession", "createSession", "createSession"})
}

// ---------------------------------------------------------------------------
// ensureFreshSession prompt suffix tests
// ---------------------------------------------------------------------------

// TestEnsureFreshSession_PromptSuffixAppendedToCommand verifies that
// PromptSuffix is appended to the command as a positional argument.
// This is the behavior that caused OpenCode to crash: the prompt text
// (beacon + instructions) was passed as argv[1], which OpenCode interprets
// as a project directory path.
func TestEnsureFreshSession_PromptSuffixAppendedToCommand(t *testing.T) {
	ops := &fakeStartOps{}

	cfg := runtime.Config{
		WorkDir:      "/proj",
		Command:      "opencode",
		PromptSuffix: "'You are an agent. Do work.'",
	}
	err := ensureFreshSession(ops, "gc-test-prompt", cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	assertCallSequence(t, ops, []string{"createSession"})

	// The command passed to createSession should have the prompt appended.
	c := ops.calls[0]
	if c.command != "opencode 'You are an agent. Do work.'" {
		t.Errorf("createSession command = %q, want %q", c.command, "opencode 'You are an agent. Do work.'")
	}
}

// TestEnsureFreshSession_PromptSuffixWithFlagPrefix verifies that when
// PromptFlag is set, the flag is prepended to PromptSuffix in the
// command. This is the correct behavior for providers that accept
// prompts via named flags.
func TestEnsureFreshSession_PromptSuffixWithFlagPrefix(t *testing.T) {
	ops := &fakeStartOps{}

	cfg := runtime.Config{
		WorkDir:      "/proj",
		Command:      "myprovider",
		PromptSuffix: "'You are an agent.'",
		PromptFlag:   "--prompt",
	}
	err := ensureFreshSession(ops, "gc-test-flag", cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	c := ops.calls[0]
	want := "myprovider --prompt 'You are an agent.'"
	if c.command != want {
		t.Errorf("createSession command = %q, want %q", c.command, want)
	}
}

// TestEnsureFreshSession_EmptyPromptSuffix verifies that when PromptSuffix
// is empty (PromptMode "none"), the command is passed through unchanged.
// This is the correct behavior for OpenCode and Codex.
func TestEnsureFreshSession_EmptyPromptSuffix(t *testing.T) {
	ops := &fakeStartOps{}

	cfg := runtime.Config{
		WorkDir: "/proj",
		Command: "opencode",
	}
	err := ensureFreshSession(ops, "gc-test-none", cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	c := ops.calls[0]
	if c.command != "opencode" {
		t.Errorf("createSession command = %q, want %q — no prompt should be appended", c.command, "opencode")
	}
}

// TestEnsureFreshSession_LongPromptSuffixUsesFileExpansion verifies that
// prompts exceeding maxInlinePromptLen are written to a temp file and
// loaded via $(cat ...) shell expansion to avoid tmux protocol limits.
func TestEnsureFreshSession_LongPromptSuffixUsesFileExpansion(t *testing.T) {
	ops := &fakeStartOps{}

	longPrompt := "'" + strings.Repeat("x", maxInlinePromptLen+100) + "'"
	cfg := runtime.Config{
		WorkDir:      t.TempDir(),
		Command:      "claude --dangerously-skip-permissions",
		PromptSuffix: longPrompt,
	}
	err := ensureFreshSession(ops, "gc-test-long", cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	c := ops.calls[0]
	// Should use sh -c with $(cat ...) expansion rather than inline.
	if !strings.HasPrefix(c.command, "sh -c '") {
		t.Errorf("long prompt should use sh -c wrapper, got %q", c.command)
	}
	if !strings.Contains(c.command, "$(cat ") {
		t.Errorf("long prompt should use $(cat ...) file expansion, got %q", c.command)
	}
}

// TestEnsureFreshSession_LongPromptWithFlagUsesFileExpansion verifies that
// the flag-mode file-expansion path preserves the flag as a separate
// argument. Without this fix, the flag would be lost when the prompt
// spills to a temp file.
func TestEnsureFreshSession_LongPromptWithFlagUsesFileExpansion(t *testing.T) {
	ops := &fakeStartOps{}

	longPrompt := "'" + strings.Repeat("x", maxInlinePromptLen+100) + "'"
	cfg := runtime.Config{
		WorkDir:      t.TempDir(),
		Command:      "myprovider",
		PromptSuffix: longPrompt,
		PromptFlag:   "--prompt",
	}
	err := ensureFreshSession(ops, "gc-test-flag-long", cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	c := ops.calls[0]
	// Should use sh -c with $(cat ...) expansion.
	if !strings.HasPrefix(c.command, "sh -c '") {
		t.Fatalf("long prompt should use sh -c wrapper, got %q", c.command)
	}
	// The flag must appear as a separate token before the loaded prompt.
	if !strings.Contains(c.command, `--prompt "$__gc_prompt"`) {
		t.Errorf("flag-mode long prompt should pass the loaded prompt after --prompt, got %q", c.command)
	}
}

func longPromptScriptFromCommand(t *testing.T, command string) string {
	t.Helper()
	args := shellquote.Split(command)
	if len(args) != 3 || args[0] != "sh" || args[1] != "-c" {
		t.Fatalf("long-prompt command should be sh -c <script>, got args %#v from %q", args, command)
	}
	return args[2]
}

func promptFileFromLongPromptCommand(t *testing.T, command string) string {
	t.Helper()
	script := longPromptScriptFromCommand(t, command)
	const marker = `$(cat `
	start := strings.Index(script, marker)
	if start < 0 {
		t.Fatalf("long-prompt script missing cat expansion: %q", script)
	}
	start += len(marker)
	end := strings.Index(script[start:], ` && printf .)`)
	if end < 0 {
		t.Fatalf("long-prompt script has unterminated prompt path: %q", script)
	}
	args := shellquote.Split(script[start : start+end])
	if len(args) != 1 {
		t.Fatalf("long-prompt script has invalid prompt path expression %q parsed as %#v", script[start:start+end], args)
	}
	return args[0]
}

func TestEnsureFreshSession_LongPromptRemovesPromptFileBeforeExec(t *testing.T) {
	ops := &fakeStartOps{}

	longPrompt := "'" + strings.Repeat("x", maxInlinePromptLen+100) + "'"
	cfg := runtime.Config{
		WorkDir:      t.TempDir(),
		Command:      "claude --dangerously-skip-permissions",
		PromptSuffix: longPrompt,
	}
	if err := ensureFreshSession(ops, "gc-test-clean-before-exec", cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	c := ops.calls[0]
	script := longPromptScriptFromCommand(t, c.command)
	readIdx := strings.Index(script, "$(cat ")
	rmIdx := strings.Index(script, "rm -f ")
	execIdx := strings.Index(script, "exec ")
	if readIdx < 0 || rmIdx < 0 || execIdx < 0 {
		t.Fatalf("long-prompt script missing read/remove/exec sequence: %q", script)
	}
	if readIdx >= rmIdx || rmIdx >= execIdx {
		t.Fatalf("prompt file must be read and removed before exec replaces the shell, got %q", script)
	}
}

func TestLongPromptCommandPreservesTrailingNewlines(t *testing.T) {
	tmp := t.TempDir()
	promptDir := filepath.Join(tmp, "dir$HOME")
	if err := os.MkdirAll(promptDir, 0o700); err != nil {
		t.Fatalf("create prompt dir: %v", err)
	}
	promptFile := filepath.Join(promptDir, "prompt.txt")
	outFile := filepath.Join(tmp, "out.txt")
	rawPrompt := "first line\nsecond line\n\n"
	if err := os.WriteFile(promptFile, []byte(rawPrompt), 0o600); err != nil {
		t.Fatalf("write prompt: %v", err)
	}

	receiver := "sh -c " + shellquote.Quote(`printf %s "$1" > "$0"`) + " " + shellquote.Quote(outFile)
	command := longPromptCommand(receiver, "", promptFile)
	cmd := exec.Command("sh", "-c", command)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("running long prompt command: %v\n%s", err, output)
	}

	got, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	if string(got) != rawPrompt {
		t.Fatalf("prompt payload mismatch:\ngot  %q\nwant %q", string(got), rawPrompt)
	}
	if _, err := os.Stat(promptFile); !os.IsNotExist(err) {
		t.Fatalf("prompt file should be removed by wrapper, stat err = %v", err)
	}
}

func TestEnsureFreshSession_LongPromptShellWrapperQuotesScript(t *testing.T) {
	ops := &fakeStartOps{}

	tmpRoot := filepath.Join(t.TempDir(), "o'brien")
	if err := os.MkdirAll(tmpRoot, 0o700); err != nil {
		t.Fatalf("setup: %v", err)
	}
	t.Setenv("TMPDIR", tmpRoot)

	cfg := runtime.Config{
		WorkDir:      "",
		Command:      "claude",
		PromptSuffix: "'" + strings.Repeat("x", maxInlinePromptLen+100) + "'",
	}
	if err := ensureFreshSession(ops, "gc-test-quoted-tempdir", cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	promptFile := promptFileFromLongPromptCommand(t, ops.calls[0].command)
	if !strings.HasPrefix(promptFile, tmpRoot+string(os.PathSeparator)) {
		t.Fatalf("quoted wrapper should preserve temp path prefix %q, got %q", tmpRoot, promptFile)
	}
}

func TestEnsureFreshSession_CreateSessionFailureRemovesPromptFile(t *testing.T) {
	ops := &fakeStartOps{createErrs: []error{errors.New("tmux create failed")}}

	workDir := t.TempDir()
	longPrompt := "'" + strings.Repeat("x", maxInlinePromptLen+100) + "'"
	cfg := runtime.Config{
		WorkDir:      workDir,
		Command:      "claude",
		PromptSuffix: longPrompt,
	}
	err := ensureFreshSession(ops, "gc-test-create-fails", cfg)
	if err == nil {
		t.Fatal("expected createSession error")
	}

	promptFile := promptFileFromLongPromptCommand(t, ops.calls[0].command)
	if _, statErr := os.Stat(promptFile); !os.IsNotExist(statErr) {
		t.Fatalf("prompt file should be removed after createSession failure, stat err = %v", statErr)
	}
}

func TestEnsureFreshSession_RecreateRaceRemovesUnusedPromptFile(t *testing.T) {
	running := true
	ops := &fakeStartOps{
		isSessionRunningResult: &running,
		createErrs:             []error{ErrSessionExists, ErrSessionExists},
		isRuntimeRunningResult: false,
	}

	cfg := runtime.Config{
		WorkDir:      t.TempDir(),
		Command:      "claude",
		PromptSuffix: "'" + strings.Repeat("x", maxInlinePromptLen+100) + "'",
		ProcessNames: []string{"claude"},
	}
	if err := ensureFreshSession(ops, "gc-test-recreate-race", cfg); err != nil {
		t.Fatalf("unexpected error: %v (race should be tolerated)", err)
	}

	promptFile := promptFileFromLongPromptCommand(t, ops.calls[0].command)
	if _, statErr := os.Stat(promptFile); !os.IsNotExist(statErr) {
		t.Fatalf("unused prompt file should be removed after tolerated recreate race, stat err = %v", statErr)
	}
}

// TestEnsureFreshSession_LongPromptUnusableWorkDirReturnsError verifies that
// a non-empty invalid WorkDir remains fatal. Falling back to OS temp for the
// prompt file would let real tmux start the pane in its default directory,
// which can put agents in the wrong checkout.
func TestEnsureFreshSession_LongPromptUnusableWorkDirReturnsError(t *testing.T) {
	ops := &fakeStartOps{}

	// A deep path whose ancestors can't be created (os.MkdirAll fails on a
	// path that descends into a regular file).
	tmp := t.TempDir()
	regularFile := filepath.Join(tmp, "not-a-dir")
	if err := os.WriteFile(regularFile, []byte("sentinel"), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}
	unusableWorkDir := filepath.Join(regularFile, "worktree-that-cannot-exist")

	longPromptRaw := strings.Repeat("x", maxInlinePromptLen+100)
	longPrompt := "'" + longPromptRaw + "'"
	cfg := runtime.Config{
		WorkDir:      unusableWorkDir,
		Command:      "claude --dangerously-skip-permissions",
		PromptSuffix: longPrompt,
	}
	err := ensureFreshSession(ops, "gc-test-unusable-workdir", cfg)
	if err == nil {
		t.Fatal("expected invalid workdir error")
	}
	if !strings.Contains(err.Error(), "workdir unavailable") {
		t.Fatalf("expected workdir unavailable error, got %v", err)
	}
	if len(ops.calls) != 0 {
		t.Fatalf("createSession should not be called for invalid WorkDir, calls = %#v", ops.calls)
	}
}

func TestEnsureFreshSession_LongPromptValidWorkDirUnusableTmpFallsBackToOSTemp(t *testing.T) {
	ops := &fakeStartOps{}
	tmpRoot := t.TempDir()
	t.Setenv("TMPDIR", tmpRoot)

	workDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(workDir, ".gc"), []byte("not a dir"), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}

	longPromptRaw := strings.Repeat("x", maxInlinePromptLen+100)
	cfg := runtime.Config{
		WorkDir:      workDir,
		Command:      "claude --dangerously-skip-permissions",
		PromptSuffix: "'" + longPromptRaw + "'",
	}
	if err := ensureFreshSession(ops, "gc-test-workdir-tmp-fallback", cfg); err != nil {
		t.Fatalf("expected OS temp dir fallback, got error: %v", err)
	}

	c := ops.calls[0]
	if strings.Contains(c.command, longPromptRaw) {
		t.Errorf("raw prompt leaked into tmux command, command = %q", c.command)
	}
	promptFile := promptFileFromLongPromptCommand(t, c.command)
	expectedDir := fallbackPromptDir(tmpRoot)
	if !strings.HasPrefix(promptFile, expectedDir+string(os.PathSeparator)) {
		t.Errorf("expected OS fallback prompt under %q, got %q", expectedDir, promptFile)
	}
}

// TestEnsureFreshSession_LongPromptEmptyWorkDirFallsBackToOSTemp verifies
// that when WorkDir is empty the long-prompt path still writes to OS temp
// instead of silently falling back to inline embedding.
func TestEnsureFreshSession_LongPromptEmptyWorkDirFallsBackToOSTemp(t *testing.T) {
	ops := &fakeStartOps{}
	tmpRoot := t.TempDir()
	t.Setenv("TMPDIR", tmpRoot)

	longPromptRaw := strings.Repeat("y", maxInlinePromptLen+100)
	longPrompt := "'" + longPromptRaw + "'"
	cfg := runtime.Config{
		WorkDir:      "",
		Command:      "claude",
		PromptSuffix: longPrompt,
	}
	err := ensureFreshSession(ops, "gc-test-empty-workdir", cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	c := ops.calls[0]
	if !strings.HasPrefix(c.command, "sh -c '") {
		t.Fatalf("long prompt with empty workdir should use sh -c wrapper, got %q", c.command)
	}
	if strings.Contains(c.command, longPromptRaw) {
		t.Errorf("raw prompt leaked into tmux command, command = %q", c.command)
	}
	promptFile := promptFileFromLongPromptCommand(t, c.command)
	expectedDir := fallbackPromptDir(tmpRoot)
	if !strings.HasPrefix(promptFile, expectedDir+string(os.PathSeparator)) {
		t.Errorf("expected OS fallback prompt under %q, got %q", expectedDir, promptFile)
	}
}

func TestEnsureFreshSession_LongPromptFileWriteFailureDoesNotCreateSession(t *testing.T) {
	ops := &fakeStartOps{}

	tmp := t.TempDir()
	regularFile := filepath.Join(tmp, "not-a-dir")
	if err := os.WriteFile(regularFile, []byte("sentinel"), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}
	t.Setenv("TMPDIR", regularFile)

	cfg := runtime.Config{
		WorkDir:      filepath.Join(regularFile, "worktree-that-cannot-exist"),
		Command:      "claude",
		PromptSuffix: "'" + strings.Repeat("x", maxInlinePromptLen+100) + "'",
	}
	err := ensureFreshSession(ops, "gc-test-no-prompt-file", cfg)
	if err == nil {
		t.Fatal("expected prompt file write failure")
	}
	if len(ops.calls) != 0 {
		t.Fatalf("createSession should not be called when prompt file creation fails, calls = %#v", ops.calls)
	}
}

// TestEnsureFreshSession_LongPromptWorkDirPreferredOverOSTemp verifies that
// when the configured WorkDir is usable, the prompt file lands inside it
// (not OS temp). This preserves the session-scoped lifetime of the file so
// it gets cleaned up alongside the session.
func TestEnsureFreshSession_LongPromptWorkDirPreferredOverOSTemp(t *testing.T) {
	ops := &fakeStartOps{}

	workDir := t.TempDir()
	longPrompt := "'" + strings.Repeat("z", maxInlinePromptLen+100) + "'"
	cfg := runtime.Config{
		WorkDir:      workDir,
		Command:      "claude",
		PromptSuffix: longPrompt,
	}
	err := ensureFreshSession(ops, "gc-test-prefer-workdir", cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	c := ops.calls[0]
	// The prompt file path appears inside the sh -c wrapper. It should be
	// rooted at workDir/.gc/tmp rather than os.TempDir.
	expectedDir := filepath.Join(workDir, ".gc", "tmp")
	if !strings.Contains(c.command, expectedDir) {
		t.Errorf("expected prompt file under %q, got command %q", expectedDir, c.command)
	}
}

func TestTmuxStartOpsRunSetupCommandUsesGC_DIRAsWorkingDirectory(t *testing.T) {
	tmpDir := t.TempDir()
	ops := &tmuxStartOps{tm: &Tmux{cfg: DefaultConfig()}}

	if err := ops.runSetupCommand(context.Background(), "touch prestart-marker", map[string]string{
		"GC_DIR": tmpDir,
	}, time.Second); err != nil {
		t.Fatalf("runSetupCommand: %v", err)
	}

	if _, err := os.Stat(filepath.Join(tmpDir, "prestart-marker")); err != nil {
		t.Fatalf("prestart-marker not created in GC_DIR: %v", err)
	}
}

func TestPaneDeadInfoParsesStatusAndSignal(t *testing.T) {
	tm := NewTmux()
	exec := &fakeExecutor{out: "139|SIGSEGV\n"}
	tm.exec = exec

	status, signal := tm.PaneDeadInfo("mayor")
	if status != "139" || signal != "SIGSEGV" {
		t.Fatalf("PaneDeadInfo = (%q, %q), want (139, SIGSEGV)", status, signal)
	}
	if len(exec.calls) == 0 {
		t.Fatal("no tmux call recorded")
	}
	last := exec.calls[len(exec.calls)-1]
	if joined := strings.Join(last, " "); !strings.Contains(joined, "#{pane_dead_status}|#{pane_dead_signal}") {
		t.Fatalf("display-message args = %v, want pane_dead format", last)
	}
}

func TestPaneDeadInfoErrorReturnsEmpty(t *testing.T) {
	tm := NewTmux()
	tm.exec = &fakeExecutor{err: errors.New("no such pane")}
	if status, signal := tm.PaneDeadInfo("mayor"); status != "" || signal != "" {
		t.Fatalf("PaneDeadInfo = (%q, %q), want empty on error", status, signal)
	}
}

func TestRecordStartCrashWritesDurableArtifact(t *testing.T) {
	dir := t.TempDir()
	tm := NewTmux()
	tm.exec = &fakeExecutor{out: "139|SIGSEGV\n"}
	o := &tmuxStartOps{tm: tm, runtimeDir: dir}

	path := o.recordStartCrash("mayor", "panic: startup failed\nPane is dead")
	want := filepath.Join(dir, "sessions", "mayor", "start-stderr.log")
	if path != want {
		t.Fatalf("path = %q, want %q", path, want)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading artifact: %v", err)
	}
	for _, sub := range []string{"session: mayor", "exit-status: 139", "signal: SIGSEGV", "panic: startup failed", "Pane is dead"} {
		if !strings.Contains(string(data), sub) {
			t.Fatalf("artifact = %q, want substring %q", data, sub)
		}
	}
}

func TestRecordStartCrashDisabledWhenNoRuntimeDir(t *testing.T) {
	tm := NewTmux()
	tm.exec = &fakeExecutor{out: "139|SIGSEGV\n"}
	o := &tmuxStartOps{tm: tm, runtimeDir: ""}
	if path := o.recordStartCrash("mayor", "x"); path != "" {
		t.Fatalf("path = %q, want empty when runtimeDir unset", path)
	}
}
