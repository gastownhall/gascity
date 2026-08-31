package zcode_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/pathutil"
	zcodeadapter "github.com/gastownhall/gascity/internal/worker/adapters/zcode"
)

// The adapter is exercised end-to-end with the real bash script and a stubbed
// CLI: ZCODE_NODE_BIN points at a python stub that records its argv and emits
// canned JSON. No network, no ZCode bundle, no host HOME.
const nodeStub = `#!/usr/bin/env python3
import json, os, sys, time

# The adapter runs "$NODE_BIN --version" for its node floor check.
if len(sys.argv) > 1 and sys.argv[1] == "--version":
    print("v99.0.0")
    sys.exit(0)

# argv[1] is the bundle path; everything after it is the real call.
args = sys.argv[2:]
with open(os.environ["STUB_LOG"], "a", encoding="utf-8") as fh:
    fh.write(json.dumps(args) + "\n")

home = os.environ.get("HOME", "")
if home:
    os.makedirs(home, exist_ok=True)
    with open(os.path.join(home, "child-home"), "w", encoding="utf-8") as fh:
        fh.write(home + "\n")

# A control file lets a test flip behavior between turns of a running adapter,
# which the parent's environment cannot do once the child is launched.
ctl = os.environ.get("STUB_CTL")
if ctl and os.path.exists(ctl):
    with open(ctl, encoding="utf-8") as fh:
        for key, _, value in (ln.partition("=") for ln in fh.read().split()):
            if key:
                os.environ[key] = value

# ZCode installs its own SIGINT handler and keeps working through one, so the
# adapter must escalate. STUB_IGNORE_INT reproduces that.
if os.environ.get("STUB_IGNORE_INT"):
    import signal
    signal.signal(signal.SIGINT, signal.SIG_IGN)

sleep_for = float(os.environ.get("STUB_SLEEP", "0"))
if sleep_for:
    deadline = time.time() + sleep_for
    while time.time() < deadline:
        time.sleep(0.1)
    done = os.environ.get("STUB_DONE_FILE")
    if done:
        with open(done, "w", encoding="utf-8") as fh:
            fh.write("finished\n")

if os.environ.get("STUB_BAD_JSON"):
    sys.stderr.write("the cli complained\n")
    sys.stdout.write("this is not json{{{")
    sys.exit(0)

rc = int(os.environ.get("STUB_RC", "0"))
if rc:
    sys.stderr.write("stub failure\n")
    sys.exit(rc)

# Fail only when resuming, to exercise the stale-sid recovery path.
if os.environ.get("STUB_FAIL_ON_RESUME") and any(a.startswith("--resume=") for a in args):
    sys.stderr.write("no such session\n")
    sys.exit(1)

print(json.dumps({
    "sessionId": os.environ.get("STUB_SID", "sess_stub"),
    "response": os.environ.get("STUB_RESPONSE", "ok"),
    "usage": {"inputTokens": 11, "outputTokens": 3, "totalTokens": 14},
    "projection": {"turnCount": 1, "totalTokenCount": 14},
}))
`

// installedAdapter materializes the adapter once per test binary. Installing
// per-test raced the parallel tests' own fork/exec: a sibling goroutine forking
// while the script's write fd is still open makes the exec fail ETXTBSY.
var (
	adapterOnce sync.Once
	adapterPath string
	adapterErr  error
)

func installedAdapter(t *testing.T) string {
	t.Helper()
	adapterOnce.Do(func() {
		dir, err := os.MkdirTemp("", "zcode-adapter-*")
		if err != nil {
			adapterErr = err
			return
		}
		adapterPath, adapterErr = zcodeadapter.Install(dir)
	})
	if adapterErr != nil {
		t.Fatalf("install adapter: %v", adapterErr)
	}
	return adapterPath
}

type harness struct {
	t         *testing.T
	home      string
	adapter   string
	logPath   string
	ctlPath   string
	mirrorDir string
	workDir   string
	env       map[string]string
	// stderr holds the last run's stderr. The adapter's die_config sites all
	// exit 78 and only stderr says which precondition tripped (ga-xx7x9).
	stderr string
}

func newHarness(t *testing.T, stubEnv map[string]string) *harness {
	t.Helper()

	root := t.TempDir()
	home := filepath.Join(root, "home")
	workDir := filepath.Join(root, "work")
	mirrorDir := filepath.Join(root, "transcripts")
	for _, dir := range []string{home, workDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}

	adapter := installedAdapter(t)

	stub := filepath.Join(root, "stub-node")
	if err := os.WriteFile(stub, []byte(nodeStub), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	bundle := filepath.Join(root, "zcode.cjs")
	if err := os.WriteFile(bundle, []byte("// stub bundle\n"), 0o644); err != nil {
		t.Fatalf("write bundle: %v", err)
	}

	h := &harness{
		t:         t,
		home:      home,
		adapter:   adapter,
		logPath:   filepath.Join(root, "stub.log"),
		ctlPath:   filepath.Join(root, "stub.ctl"),
		mirrorDir: mirrorDir,
		workDir:   workDir,
		env: map[string]string{
			"HOME":                    home,
			"XDG_STATE_HOME":          filepath.Join(home, ".local", "state"),
			"PATH":                    os.Getenv("PATH"),
			"ZCODE_CJS":               bundle,
			"ZCODE_API_KEY":           "dummy-not-a-real-key",
			"ZCODE_MODEL":             "glm-test",
			"ZCODE_NODE_BIN":          stub,
			"GC_ZCODE_TRANSCRIPT_DIR": mirrorDir,
			"GC_SESSION":              "test-session",
			"STUB_LOG":                filepath.Join(root, "stub.log"),
			"STUB_CTL":                filepath.Join(root, "stub.ctl"),
		},
	}
	for k, v := range stubEnv {
		h.env[k] = v
	}
	return h
}

func (h *harness) envList() []string {
	out := make([]string, 0, len(h.env))
	for k, v := range h.env {
		out = append(out, k+"="+v)
	}
	return out
}

func (h *harness) command() *exec.Cmd {
	cmd := exec.Command(h.adapter)
	cmd.Dir = h.workDir
	cmd.Env = h.envList()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd
}

// run feeds stdin, waits for exit, and returns combined stdout plus the exit
// code.
func (h *harness) run(stdin string) (string, int) {
	h.t.Helper()

	cmd := h.command()
	cmd.Stdin = strings.NewReader(stdin)
	var out, errOut strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	err := cmd.Run()
	h.stderr = errOut.String()
	code := 0
	if err != nil {
		var exitErr *exec.ExitError
		if !asExitError(err, &exitErr) {
			h.t.Fatalf("run adapter: %v (stdout=%s, stderr=%s)", err, out.String(), h.stderr)
		}
		code = exitErr.ExitCode()
	}
	if code != 0 && h.stderr != "" {
		h.t.Logf("adapter exited %d: %s", code, strings.TrimSpace(h.stderr))
	}
	return out.String(), code
}

func asExitError(err error, target **exec.ExitError) bool {
	e := &exec.ExitError{}
	if errors.As(err, &e) {
		*target = e
		return true
	}
	return false
}

// session starts the adapter with a live stdin pipe so a test can drive turns
// and signals independently.
type session struct {
	t      *testing.T
	cmd    *exec.Cmd
	in     io.WriteCloser
	mu     sync.Mutex
	out    strings.Builder
	errOut strings.Builder
	done   chan struct{}
}

func (h *harness) start() *session {
	h.t.Helper()

	cmd := h.command()
	in, err := cmd.StdinPipe()
	if err != nil {
		h.t.Fatalf("stdin pipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		h.t.Fatalf("stdout pipe: %v", err)
	}
	s := &session{t: h.t, cmd: cmd, in: in, done: make(chan struct{})}
	cmd.Stderr = &s.errOut
	if err := cmd.Start(); err != nil {
		h.t.Fatalf("start adapter: %v", err)
	}
	go func() {
		defer close(s.done)
		buf := make([]byte, 4096)
		for {
			n, err := stdout.Read(buf)
			if n > 0 {
				s.mu.Lock()
				s.out.Write(buf[:n])
				s.mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()
	h.t.Cleanup(func() {
		if s.cmd.ProcessState == nil && s.cmd.Process != nil {
			_ = s.cmd.Process.Kill()
		}
	})
	return s
}

func (s *session) send(line string) {
	s.t.Helper()
	s.sendRaw(line + "\n")
}

// sendRaw writes exactly what it is given, newlines included (or not).
func (s *session) sendRaw(text string) {
	s.t.Helper()
	if _, err := io.WriteString(s.in, text); err != nil && !strings.Contains(err.Error(), "broken pipe") {
		s.t.Fatalf("write prompt: %v", err)
	}
}

func (s *session) signal(sig syscall.Signal) {
	s.t.Helper()
	if err := syscall.Kill(-s.cmd.Process.Pid, sig); err != nil {
		// An adapter that already died leaves a zombie process group, and
		// signaling one reports EPERM on macOS rather than ESRCH — which reads
		// as a permissions problem and hides the real story. Print what the
		// adapter said before it went, which is where the cause actually is.
		s.t.Fatalf("signal %v: %v\nadapter output so far:\n%s", sig, err, s.output())
	}
}

// adapterWaitBudget bounds every lifecycle wait in this suite. Generous on
// purpose: it is a deadlock guard, not a timing assertion, and a loaded machine
// must not turn a slow turn into a failure.
const adapterWaitBudget = 20 * time.Second

// waitForTurns blocks until the adapter has completed n turns, counted by its
// ready markers (startup prints one, then one per finished turn). This is the
// lifecycle signal that replaces sleeping for "long enough" — it is both faster
// and immune to a loaded machine stretching a turn past a fixed delay.
func (s *session) waitForTurns(n int) {
	s.t.Helper()
	deadline := time.Now().Add(adapterWaitBudget)
	for strings.Count(s.output(), "zcode-repl ready") < n+1 {
		if time.Now().After(deadline) {
			s.t.Fatalf("only %d/%d turns completed within %s:\n%s",
				strings.Count(s.output(), "zcode-repl ready")-1, n, adapterWaitBudget, s.output())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// waitForFailures blocks until the adapter has reported n failed turns, or the
// shell has exited — the bail path exits without printing anything further.
func (s *session) waitForFailures(n int) {
	s.t.Helper()
	deadline := time.Now().Add(adapterWaitBudget)
	for strings.Count(s.output(), "zcode-repl error rc=") < n {
		if !s.alive() || time.Now().After(deadline) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// waitForOutput blocks until needle appears in the adapter's stdout.
func (s *session) waitForOutput(needle string, timeout time.Duration) {
	s.t.Helper()
	deadline := time.Now().Add(timeout)
	for !strings.Contains(s.output(), needle) {
		if time.Now().After(deadline) {
			s.t.Fatalf("%q never appeared within %s:\n%s", needle, timeout, s.output())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (s *session) alive() bool {
	return s.cmd.ProcessState == nil && syscall.Kill(-s.cmd.Process.Pid, 0) == nil
}

func (s *session) output() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.out.String()
}

// closeAndWait closes stdin and waits for exit, returning stdout and the code.
func (s *session) closeAndWait() (string, int) {
	s.t.Helper()
	_ = s.in.Close()
	return s.wait()
}

func (s *session) wait() (string, int) {
	s.t.Helper()
	code := 0
	if err := s.cmd.Wait(); err != nil {
		var exitErr *exec.ExitError
		if !asExitError(err, &exitErr) {
			s.t.Fatalf("wait adapter: %v", err)
		}
		code = exitErr.ExitCode()
	}
	<-s.done
	// errOut is only safe to read once Wait has joined exec's copier.
	if code != 0 && s.errOut.Len() > 0 {
		s.t.Logf("adapter exited %d: %s", code, strings.TrimSpace(s.errOut.String()))
	}
	return s.output(), code
}

func (h *harness) control(values map[string]string) {
	h.t.Helper()
	parts := make([]string, 0, len(values))
	for k, v := range values {
		parts = append(parts, k+"="+v)
	}
	if err := os.WriteFile(h.ctlPath, []byte(strings.Join(parts, " ")), 0o644); err != nil {
		h.t.Fatalf("write control file: %v", err)
	}
}

func (h *harness) calls() [][]string {
	h.t.Helper()
	data, err := os.ReadFile(h.logPath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		h.t.Fatalf("read stub log: %v", err)
	}
	var out [][]string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var call []string
		if err := json.Unmarshal([]byte(line), &call); err != nil {
			h.t.Fatalf("parse stub log line %q: %v", line, err)
		}
		out = append(out, call)
	}
	return out
}

func (h *harness) prompts() []string {
	h.t.Helper()
	var out []string
	for _, call := range h.calls() {
		for _, arg := range call {
			if strings.HasPrefix(arg, "--prompt=") {
				out = append(out, strings.TrimPrefix(arg, "--prompt="))
			}
		}
	}
	return out
}

func (h *harness) resetLog() {
	h.t.Helper()
	if err := os.Remove(h.logPath); err != nil && !os.IsNotExist(err) {
		h.t.Fatalf("reset stub log: %v", err)
	}
}

func (h *harness) sidPath(key string) string {
	epoch := h.env["GC_CONTINUATION_EPOCH"]
	if epoch == "" {
		epoch = "1"
	}
	return filepath.Join(h.home, ".local", "state", "gascity", "zcode", "sids", key+"#"+epoch)
}

// sid reads the persisted provider session id for the harness's session key.
func (h *harness) sid() string {
	h.t.Helper()
	data, err := os.ReadFile(h.sidPath("test-session"))
	if err != nil {
		h.t.Fatalf("read sid: %v", err)
	}
	return strings.TrimSpace(string(data))
}

// Behavior 1: a single pasted burst is one turn, not one turn per line.
func TestBurstCoalescesIntoOneTurn(t *testing.T) {
	t.Parallel()

	h := newHarness(t, nil)
	body := "line one\nline two\nline three"
	_, code := h.run(body + "\n")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if got := h.calls(); len(got) != 1 {
		t.Fatalf("expected ONE turn, got %d: %v", len(got), got)
	}
	if got := h.prompts(); len(got) != 1 || got[0] != body {
		t.Fatalf("prompts = %q, want [%q]", got, body)
	}
}

func TestIdleSeparatedPromptsStaySeparate(t *testing.T) {
	t.Parallel()

	h := newHarness(t, nil)
	s := h.start()
	s.send("first prompt")
	time.Sleep(2500 * time.Millisecond)
	s.send("second prompt")
	time.Sleep(2500 * time.Millisecond)
	if _, code := s.closeAndWait(); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}

	want := []string{"first prompt", "second prompt"}
	if got := h.prompts(); !equalStrings(got, want) {
		t.Fatalf("prompts = %q, want %q", got, want)
	}
}

// Behavior 2: --opt=value single-argv forms (node:util parseArgs rejects the
// two-argv form when the value starts with a dash).
func TestDashLeadingPromptUsesSingleArgvForm(t *testing.T) {
	t.Parallel()

	h := newHarness(t, nil)
	body := "- a markdown bullet\n--flag-looking line\n- another"
	_, code := h.run(body + "\n")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}

	call := h.calls()[0]
	if !containsString(call, "--prompt="+body) {
		t.Fatalf("call %q missing --prompt=<body>", call)
	}
	if containsString(call, "--prompt") {
		t.Fatalf("call %q used the ambiguous two-argv --prompt form", call)
	}
}

func TestResumeUsesSingleArgvForm(t *testing.T) {
	t.Parallel()

	h := newHarness(t, map[string]string{"STUB_SID": "sess_abc"})
	h.run("first\n")
	h.resetLog()
	h.run("second\n")

	call := h.calls()[0]
	if !containsString(call, "--resume=sess_abc") {
		t.Fatalf("call %q missing --resume=sess_abc", call)
	}
	if containsString(call, "--resume") {
		t.Fatalf("call %q used the ambiguous two-argv --resume form", call)
	}
}

// The adapter's six die_config sites all exit 78, so a bare exit code cannot
// say which precondition tripped. TestControlBytesAreStripped flaked at 78
// under a saturated parallel sweep and blocked a push with nothing to go on,
// because the harness discarded stderr (ga-xx7x9).
func TestHarnessKeepsTheAdapterStderrThatNamesAFailure(t *testing.T) {
	t.Parallel()

	h := newHarness(t, nil)
	delete(h.env, "ZCODE_API_KEY")

	if _, code := h.run("hello\n"); code != 78 {
		t.Fatalf("exit code = %d, want 78 (EX_CONFIG)", code)
	}
	if !strings.Contains(h.stderr, "ZCODE_API_KEY is unset") {
		t.Fatalf("harness dropped the reason for exit 78; stderr = %q", h.stderr)
	}
}

// Five of the six preconditions are pure env/filesystem tests; only the node
// floor check forks, and a fork that fails under load is not a version verdict.
func TestNodeProbeSpawnFailureIsNotReportedAsAnOldNode(t *testing.T) {
	t.Parallel()

	killed := filepath.Join(t.TempDir(), "killed-node")
	if err := os.WriteFile(killed, []byte("#!/bin/sh\nkill -9 $$\n"), 0o755); err != nil {
		t.Fatalf("write killed-node: %v", err)
	}
	h := newHarness(t, map[string]string{"ZCODE_NODE_BIN": killed})

	if _, code := h.run("hello\n"); code != 78 {
		t.Fatalf("exit code = %d, want 78 (EX_CONFIG)", code)
	}
	if !strings.Contains(h.stderr, "could not run") {
		t.Fatalf("a killed probe was not named as a spawn failure; stderr = %q", h.stderr)
	}
	// 137 = 128+SIGKILL, the OOM signature the saturated-box flake would carry.
	if !strings.Contains(h.stderr, "exit 137") {
		t.Fatalf("stderr lost the probe's exit status; stderr = %q", h.stderr)
	}

	// The other half: a node that really is too old must still be a config error,
	// or the guard above would wave through the case the floor check exists for.
	old := filepath.Join(t.TempDir(), "old-node")
	if err := os.WriteFile(old, []byte("#!/bin/sh\necho v18.0.0\n"), 0o755); err != nil {
		t.Fatalf("write old-node: %v", err)
	}
	h = newHarness(t, map[string]string{"ZCODE_NODE_BIN": old})

	if _, code := h.run("hello\n"); code != 78 {
		t.Fatalf("exit code = %d, want 78 (EX_CONFIG)", code)
	}
	if !strings.Contains(h.stderr, "is v18.0.0") {
		t.Fatalf("a genuinely old node was not reported as a version problem; stderr = %q", h.stderr)
	}
}

// Behavior 3: gc's pre-Enter Escape and stray control bytes never reach the
// model.
func TestControlBytesAreStripped(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		stdin string
		want  []string
	}{
		"trailing escape":   {"hello world\x1b\n", []string{"hello world"}},
		"embedded escape":   {"alpha\x1bbeta\x1b\n", []string{"alphabeta"}},
		"control-only line": {"\x1b\n", nil},
		"blank lines":       {"\n   \nreal prompt\n", []string{"real prompt"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			h := newHarness(t, nil)
			_, code := h.run(tc.stdin)
			if code != 0 {
				t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, strings.TrimSpace(h.stderr))
			}
			if got := h.prompts(); !equalStrings(got, tc.want) {
				t.Fatalf("prompts = %q, want %q", got, tc.want)
			}
		})
	}
}

// tmux delivers prompts over its send-keys literal limit as a bracketed paste,
// so the body arrives wrapped in ESC[200~ ... ESC[201~.
func TestBracketedPasteWrappersAreStripped(t *testing.T) {
	t.Parallel()

	h := newHarness(t, nil)
	body := "first paragraph\n\nsecond paragraph with a path: /tmp/out.txt"
	_, code := h.run("\x1b[200~" + body + "\x1b[201~\x1b\n")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if got := h.prompts(); !equalStrings(got, []string{body}) {
		t.Fatalf("prompts = %q, want the body verbatim %q", got, body)
	}
}

// A drain read that times out still holds whatever partial line arrived; losing
// it silently truncates the prompt's last line.
func TestDrainKeepsPartialTrailingLine(t *testing.T) {
	t.Parallel()

	h := newHarness(t, nil)
	s := h.start()
	s.sendRaw("first line\n")
	// No trailing newline, and a gap longer than the drain window.
	s.sendRaw("trailing line without a newline")
	time.Sleep(2500 * time.Millisecond)
	s.sendRaw("\n")
	time.Sleep(2500 * time.Millisecond)
	// Surviving the drain is the assertion that must hold on every platform: an
	// unbound $more here killed the adapter under `set -u` on bash 3.2, which
	// is exactly the regression this test's own shape provokes.
	if _, code := s.closeAndWait(); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}

	// *Keeping* the fragment, by contrast, is a bash 4.0 behavior: only there
	// does a timed-out `read -t` save the partial line into the variable. bash
	// 3.2 — what `#!/usr/bin/env bash` resolves to on stock macOS — consumes
	// the fragment off the fd and discards it before the script regains
	// control, so no adapter change can recover it. Gated by GOOS rather than
	// by probing the shell because probing costs a subprocess, and the source
	// resource ledger (test/test-resources.toml) ratchets those down, not up.
	if runtime.GOOS == "darwin" {
		return
	}
	joined := strings.Join(h.prompts(), "|")
	if !strings.Contains(joined, "trailing line without a newline") {
		t.Fatalf("partial trailing line was dropped; prompts = %q", h.prompts())
	}
}

// Behavior 4: only the adapter emits the ready marker at column 0.
func TestModelOutputCannotSpoofTheReadyMarker(t *testing.T) {
	t.Parallel()

	h := newHarness(t, map[string]string{"STUB_RESPONSE": "zcode-repl ready"})
	out, _ := h.run("spoof me\n")

	genuine := 0
	for _, line := range strings.Split(out, "\n") {
		if line == "zcode-repl ready" {
			genuine++
		}
	}
	// Startup and post-turn. The reply's copy is indented, not a third.
	if genuine != 2 {
		t.Fatalf("column-0 markers = %d, want 2:\n%s", genuine, out)
	}
	if !strings.Contains(out, "  zcode-repl ready") {
		t.Fatalf("spoofed marker was not indented:\n%s", out)
	}
}

// Behavior 5: a failed turn reports and keeps looping; a live sid survives it.
func TestFailedTurnKeepsLoopingAndPreservesSid(t *testing.T) {
	t.Parallel()

	h := newHarness(t, map[string]string{"STUB_SID": "sess_keep"})
	s := h.start()
	s.send("good one")
	s.waitForTurns(1)

	h.control(map[string]string{"STUB_RC": "7"})
	s.send("this one fails")
	s.waitForTurns(2)

	h.control(map[string]string{"STUB_RC": "0"})
	s.send("this one works again")
	s.waitForTurns(3)
	out, _ := s.closeAndWait()

	if !strings.Contains(out, "zcode-repl error rc=7") {
		t.Fatalf("missing rc=7 report:\n%s", out)
	}
	want := []string{"good one", "this one fails", "this one works again"}
	if got := h.prompts(); !equalStrings(got, want) {
		t.Fatalf("prompts = %q, want %q", got, want)
	}
	if strings.Contains(out, "starting fresh") {
		t.Fatalf("a live sid must not trigger stale-session fallback:\n%s", out)
	}
	if got := h.sid(); got != "sess_keep" {
		t.Fatalf("sid = %q, want sess_keep", got)
	}
}

func TestFailingTurnPrintsErrorAndMarker(t *testing.T) {
	t.Parallel()

	h := newHarness(t, map[string]string{"STUB_RC": "3"})
	out, code := h.run("one\n")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if !strings.Contains(out, "zcode-repl error rc=3") {
		t.Fatalf("missing rc report:\n%s", out)
	}
	if !strings.Contains(out, "zcode-repl stderr: stub failure") {
		t.Fatalf("missing stderr excerpt:\n%s", out)
	}
	if !strings.HasSuffix(strings.TrimRight(out, "\n"), "zcode-repl ready") {
		t.Fatalf("a recoverable failure must end ready:\n%s", out)
	}
}

func TestUnparsableResponseIsReported(t *testing.T) {
	t.Parallel()

	h := newHarness(t, map[string]string{"STUB_BAD_JSON": "1"})
	out, code := h.run("bad json please\n")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if !strings.Contains(out, "zcode-repl error rc=0 (unparsable response)") {
		t.Fatalf("missing unparsable-response report:\n%s", out)
	}
	// rc was 0, so the failure branch never ran: the CLI's stderr is the only
	// evidence of why the turn produced nothing usable.
	if !strings.Contains(out, "zcode-repl stderr: the cli complained") {
		t.Fatalf("unparsable response did not surface the CLI stderr:\n%s", out)
	}
}

// A headless turn is silent until it completes, so the pane would otherwise
// still show the previous turn's ready marker as its last line while busy.
func TestTurnInFlightIsAnnouncedBeforeTheReadyMarker(t *testing.T) {
	t.Parallel()

	h := newHarness(t, nil)
	out, _ := h.run("do some work\n")

	busy := strings.Index(out, "zcode-repl turn in flight")
	if busy < 0 {
		t.Fatalf("no in-flight announcement:\n%s", out)
	}
	if last := strings.LastIndex(out, "zcode-repl ready"); last < busy {
		t.Fatalf("in-flight line must precede the turn's ready marker:\n%s", out)
	}
	// The startup marker still comes first, so a pane that has never run a turn
	// reads as ready, not busy.
	if first := strings.Index(out, "zcode-repl ready"); first > busy {
		t.Fatalf("startup marker must precede the first in-flight line:\n%s", out)
	}
}

// Behavior 6: five consecutive failures exit 1 with no trailing marker, so
// liveness and readiness both go red.
func TestFiveConsecutiveFailuresBailWithoutMarker(t *testing.T) {
	t.Parallel()

	h := newHarness(t, map[string]string{"STUB_RC": "4"})
	s := h.start()
	for i := 0; i < 6; i++ {
		if !s.alive() {
			break
		}
		s.send(fmt.Sprintf("failing prompt %d", i))
		// The fifth failure bails without a trailing marker, so count reported
		// failures rather than completed turns and stop once the shell is gone.
		s.waitForFailures(i + 1)
	}
	out, code := s.wait()

	if code != 1 {
		t.Fatalf("exit code = %d, want 1:\n%s", code, out)
	}
	if !strings.HasSuffix(strings.TrimRight(out, "\n"), "zcode-repl fatal: 5 consecutive turn failures") {
		t.Fatalf("bail line must be last (no ready marker after it):\n%s", out)
	}
}

// Behavior 7: INT interrupts the turn, TERM ends the session.
func TestInterruptMidTurnContinuesTheLoop(t *testing.T) {
	t.Parallel()

	h := newHarness(t, map[string]string{"STUB_SLEEP": "5", "STUB_SID": "sess_int"})
	s := h.start()
	s.send("slow turn")
	time.Sleep(3 * time.Second) // inside the stub's sleep
	s.signal(syscall.SIGINT)
	time.Sleep(2 * time.Second)

	if !s.alive() {
		t.Fatalf("adapter exited on SIGINT; it must absorb it:\n%s", s.output())
	}
	h.control(map[string]string{"STUB_SLEEP": "0"})
	s.send("after the interrupt")
	s.waitForTurns(2)
	out, _ := s.closeAndWait()

	if !strings.Contains(out, "zcode-repl error rc=") {
		t.Fatalf("interrupted turn was not reported:\n%s", out)
	}
	if !containsString(h.prompts(), "after the interrupt") {
		t.Fatalf("adapter stopped accepting work after SIGINT: %q", h.prompts())
	}
	if strings.Contains(out, "starting fresh") {
		t.Fatalf("an interrupted turn is not a stale-session signal:\n%s", out)
	}
}

// The CLI ignoring SIGINT must not leave an orphaned turn running: the adapter
// escalates until the turn's process group is gone.
func TestInterruptKillsASigintIgnoringTurn(t *testing.T) {
	t.Parallel()

	h := newHarness(t, map[string]string{"STUB_SLEEP": "30", "STUB_IGNORE_INT": "1"})
	doneFile := filepath.Join(h.workDir, "turn-finished")
	h.env["STUB_DONE_FILE"] = doneFile

	s := h.start()
	s.send("a turn that ignores interrupts")
	// Interrupt the instant the pane says busy. That is what an observer
	// watching for the in-flight line does, so the announcement must not
	// precede the child it describes.
	s.waitForOutput("zcode-repl turn in flight", 20*time.Second)
	s.signal(syscall.SIGINT)

	// The adapter must report the turn as failed rather than block until the
	// stub's own deadline.
	deadline := time.Now().Add(15 * time.Second)
	for !strings.Contains(s.output(), "zcode-repl error rc=") {
		if time.Now().After(deadline) {
			t.Fatalf("adapter did not report an interrupted turn within 15s:\n%s", s.output())
		}
		time.Sleep(250 * time.Millisecond)
	}
	if !s.alive() {
		t.Fatalf("adapter exited on SIGINT; it must absorb it:\n%s", s.output())
	}
	if _, err := os.Stat(doneFile); err == nil {
		t.Fatal("the interrupted turn ran to completion; it must be killed, not orphaned")
	}
	if !strings.Contains(s.output(), "zcode-repl interrupting turn") {
		t.Fatalf("the interrupt was not announced in the pane:\n%s", s.output())
	}

	s.signal(syscall.SIGTERM)
	if _, code := s.wait(); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	// Still absent after the adapter is gone: nothing survived to finish.
	if _, err := os.Stat(doneFile); err == nil {
		t.Fatal("an orphaned turn outlived the adapter and completed")
	}
}

func TestInterruptWhileIdleDoesNotExit(t *testing.T) {
	t.Parallel()

	h := newHarness(t, nil)
	s := h.start()
	time.Sleep(2 * time.Second) // idle, blocked in read
	s.signal(syscall.SIGINT)
	time.Sleep(2 * time.Second)

	if !s.alive() {
		t.Fatalf("adapter exited on an idle SIGINT:\n%s", s.output())
	}
	s.send("still working")
	s.waitForTurns(1)
	if _, code := s.closeAndWait(); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}

	if got := h.prompts(); !equalStrings(got, []string{"still working"}) {
		t.Fatalf("prompts = %q, want [still working]", got)
	}
}

func TestTerminateExitsCleanly(t *testing.T) {
	t.Parallel()

	h := newHarness(t, nil)
	s := h.start()
	s.waitForOutput("zcode-repl ready", 20*time.Second)
	s.signal(syscall.SIGTERM)
	if _, code := s.wait(); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
}

// Behavior 8: sid persistence across restarts, with the unvalidated gate.
func TestSidRoundTripsAcrossRestarts(t *testing.T) {
	t.Parallel()

	h := newHarness(t, map[string]string{"STUB_SID": "sess_persisted"})
	h.run("first process\n")

	if got := h.sid(); got != "sess_persisted" {
		t.Fatalf("sid = %q, want sess_persisted", got)
	}

	h.resetLog()
	h.run("second process\n")
	if call := h.calls()[0]; !containsString(call, "--resume=sess_persisted") {
		t.Fatalf("second process did not resume on its first turn: %q", call)
	}
}

func TestStaleSidFallsBackToAFreshSession(t *testing.T) {
	t.Parallel()

	h := newHarness(t, map[string]string{
		"STUB_SID":            "sess_new",
		"STUB_FAIL_ON_RESUME": "1",
	})
	sidPath := h.sidPath("test-session")
	if err := os.MkdirAll(filepath.Dir(sidPath), 0o755); err != nil {
		t.Fatalf("mkdir sid dir: %v", err)
	}
	if err := os.WriteFile(sidPath, []byte("sess_stale\n"), 0o600); err != nil {
		t.Fatalf("seed stale sid: %v", err)
	}

	out, code := h.run("recover please\n")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if !strings.Contains(out, "zcode-repl: resume of sess_stale failed, starting fresh") {
		t.Fatalf("missing stale-sid recovery notice:\n%s", out)
	}
	want := []string{"recover please", "recover please"}
	if got := h.prompts(); !equalStrings(got, want) {
		t.Fatalf("prompts = %q, want the same prompt retried once: %q", got, want)
	}
}

// A conversation reset must not resume: gc bumps GC_CONTINUATION_EPOCH when it
// commits one, and a plain restart leaves it alone.
func TestContinuationEpochScopesTheSid(t *testing.T) {
	t.Parallel()

	h := newHarness(t, map[string]string{"STUB_SID": "sess_epoch_1"})
	h.env["GC_CONTINUATION_EPOCH"] = "1"
	h.run("first conversation\n")
	if got := h.sid(); got != "sess_epoch_1" {
		t.Fatalf("sid = %q, want sess_epoch_1", got)
	}

	// Restart at the same epoch resumes.
	h.resetLog()
	h.run("same conversation\n")
	if call := h.calls()[0]; !containsString(call, "--resume=sess_epoch_1") {
		t.Fatalf("restart at the same epoch did not resume: %q", call)
	}

	// Reset bumps the epoch: the first turn must be a fresh session.
	h.resetLog()
	h.env["GC_CONTINUATION_EPOCH"] = "2"
	h.env["STUB_SID"] = "sess_epoch_2"
	h.run("fresh conversation\n")
	for _, arg := range h.calls()[0] {
		if strings.HasPrefix(arg, "--resume=") {
			t.Fatalf("reset still resumed the prior conversation: %q", arg)
		}
	}
	if got := h.sid(); got != "sess_epoch_2" {
		t.Fatalf("post-reset sid = %q, want sess_epoch_2", got)
	}
	// The superseded epoch's sid file is pruned, not left to accumulate.
	stale := filepath.Join(h.home, ".local", "state", "gascity", "zcode", "sids", "test-session#1")
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("superseded epoch sid file still present: %v", err)
	}
}

func TestSessionKeyComesFromGCSession(t *testing.T) {
	t.Parallel()

	h := newHarness(t, map[string]string{"STUB_SID": "sess_keyed"})
	h.env["GC_SESSION"] = "gascity/gc.worker-9"
	h.run("key me\n")

	// Path-unsafe characters are folded to underscores.
	if _, err := os.Stat(h.sidPath("gascity_gc.worker-9")); err != nil {
		t.Fatalf("sid file not written under the folded session key: %v", err)
	}
}

// Behavior 9: the export mirror the sessionlog zcode reader consumes.
func TestExportMirrorAccumulatesTurns(t *testing.T) {
	t.Parallel()

	h := newHarness(t, map[string]string{"STUB_SID": "sess_mirror", "STUB_RESPONSE": "the anchor"})
	h.run("first turn\n")

	export := h.readExport("sess_mirror")
	if export.Info.ID != "sess_mirror" {
		t.Fatalf("info.id = %q, want sess_mirror", export.Info.ID)
	}
	// The adapter reports the shell's $PWD, which bash derives from getcwd()
	// because the harness hands it an env with no PWD to inherit — so it is the
	// physical path. h.workDir is whatever t.TempDir() handed out, which on
	// macOS is the /var symlink to the same directory. Same directory, two
	// spellings: compare by path identity, not by string.
	if !pathutil.SamePath(export.Info.Directory, h.workDir) {
		t.Fatalf("info.directory = %q, want %q", export.Info.Directory, h.workDir)
	}
	if len(export.Messages) != 2 {
		t.Fatalf("messages = %d, want 2 (user + assistant)", len(export.Messages))
	}
	first := export.Messages[0]
	if first.Info.Role != "user" || first.Parts[0].Text != "first turn" {
		t.Fatalf("first message = %+v, want the user prompt", first)
	}
	second := export.Messages[1]
	if second.Info.Role != "assistant" || second.Parts[0].Text != "the anchor" {
		t.Fatalf("second message = %+v, want the assistant reply", second)
	}
	if second.Info.ParentID != first.Info.ID {
		t.Fatalf("assistant parentID = %q, want %q", second.Info.ParentID, first.Info.ID)
	}
	if second.Info.Time.Created == 0 {
		t.Fatalf("assistant timestamp is unset")
	}
	if second.Info.Usage == nil || second.Info.Usage["totalTokens"] == nil {
		t.Fatalf("usage was not recorded: %+v", second.Info.Usage)
	}

	// A second process appends rather than truncating.
	h.env["STUB_RESPONSE"] = "the recall"
	h.run("second turn\n")

	export = h.readExport("sess_mirror")
	if len(export.Messages) != 4 {
		t.Fatalf("messages = %d, want 4 after a second turn", len(export.Messages))
	}
	if got := export.Messages[2].Parts[0].Text; got != "second turn" {
		t.Fatalf("third message = %q, want the second prompt", got)
	}
	if export.Messages[2].Info.ParentID != export.Messages[1].Info.ID {
		t.Fatalf("append did not chain parentID across turns")
	}
}

// A turn must be visible in the mirror while it runs, not only once it lands:
// nothing can observe — or interrupt — a turn that leaves no trace until it
// returns.
func TestExportMirrorPublishesTheUserTurnBeforeTheReply(t *testing.T) {
	t.Parallel()

	h := newHarness(t, map[string]string{"STUB_SID": "sess_pending"})
	h.run("establish the session\n")

	// Second turn: the session id is known, so the prompt lands in the mirror
	// before the reply does.
	h.env["STUB_SLEEP"] = "30"
	s := h.start()
	s.send("the long turn")
	s.waitForOutput("zcode-repl turn in flight", 20*time.Second)

	deadline := time.Now().Add(10 * time.Second)
	for {
		export := h.readExport("sess_pending")
		if len(export.Messages) == 3 {
			last := export.Messages[2]
			if last.Info.Role != "user" || last.Parts[0].Text != "the long turn" {
				t.Fatalf("pending entry = %+v, want the in-flight user turn", last)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("in-flight turn never appeared in the mirror")
		}
		time.Sleep(200 * time.Millisecond)
	}

	// Interrupting now leaves the user turn unpaired — exactly what an
	// interrupted turn looks like for every other family.
	s.signal(syscall.SIGINT)
	s.waitForOutput("zcode-repl error rc=", 15*time.Second)
	s.signal(syscall.SIGTERM)
	if _, code := s.wait(); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	// The interrupted turn is closed out, not left dangling: a trailing user
	// message would read as "still in flight" to every consumer of the mirror.
	export := h.readExport("sess_pending")
	if len(export.Messages) != 4 {
		t.Fatalf("messages = %d, want the interrupted turn recorded and closed", len(export.Messages))
	}
	if got := export.Messages[2].Info.ParentID; got != export.Messages[1].Info.ID {
		t.Fatalf("pending user parentID = %q, want %q", got, export.Messages[1].Info.ID)
	}
	if got := export.Messages[3].Info.Role; got != "assistant" {
		t.Fatalf("interrupted turn tail role = %q, want assistant", got)
	}
}

// A conversation reset must move the prior conversation out of the directory the
// model browses — that adjacency is the leak that was actually observed (GLM
// reproduced a pre-reset anchor byte-exact out of a stale mirror sitting beside
// the fresh one). It must NOT destroy it: gc reads the pre-reset transcript
// after the reset is issued, so the record has to survive somewhere resolvable.
func TestResetArchivesTheSupersededEpochsState(t *testing.T) {
	t.Parallel()

	h := newHarness(t, map[string]string{"STUB_SID": "sess_epoch_one"})
	h.env["GC_CONTINUATION_EPOCH"] = "1"
	h.run("first conversation\n")

	oldMirror := filepath.Join(h.mirrorDir, h.epochScope(), "sess_epoch_one.json")
	if _, err := os.Stat(oldMirror); err != nil {
		t.Fatalf("epoch 1 mirror missing: %v", err)
	}
	oldHome := filepath.Join(h.home, ".local", "state", "gascity", "zcode", "homes", h.epochScope())
	if _, err := os.Stat(oldHome); err != nil {
		t.Fatalf("epoch 1 CLI home missing: %v", err)
	}

	h.env["GC_CONTINUATION_EPOCH"] = "2"
	h.env["STUB_SID"] = "sess_epoch_two"
	h.run("fresh conversation\n")

	// Gone from the live tree the model reads...
	if _, err := os.Stat(oldMirror); !os.IsNotExist(err) {
		t.Fatalf("superseded epoch's transcript stayed adjacent to the fresh one: %v", err)
	}
	// ...but preserved in the archive, still keyed by its own scope.
	archived := filepath.Join(h.home, ".local", "state", "gascity", "zcode",
		"archived-transcripts", "test-session#1", "sess_epoch_one.json")
	if _, err := os.Stat(archived); err != nil {
		t.Fatalf("superseded epoch's transcript was destroyed rather than archived: %v", err)
	}
	// The CLI's own database has no preservation contract and is the copy the
	// CLI itself would reattach to, so it is still deleted.
	if _, err := os.Stat(oldHome); !os.IsNotExist(err) {
		t.Fatalf("superseded epoch's CLI state survived the reset: %v", err)
	}
	if _, err := os.Stat(filepath.Join(h.mirrorDir, h.epochScope(), "sess_epoch_two.json")); err != nil {
		t.Fatalf("epoch 2 mirror missing: %v", err)
	}
	// The live tree carries exactly one scope: the current one.
	entries, err := os.ReadDir(h.mirrorDir)
	if err != nil {
		t.Fatalf("read live mirror root: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != h.epochScope() {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("live mirror root = %v, want only the current scope %q", names, h.epochScope())
	}
}

// The CLI child must not write its session database into the pane's HOME, or a
// reset cannot orphan it.
func TestCLIChildGetsAPerEpochHome(t *testing.T) {
	t.Parallel()

	h := newHarness(t, map[string]string{"STUB_SID": "sess_home"})
	h.run("a turn\n")

	want := filepath.Join(h.home, ".local", "state", "gascity", "zcode", "homes", h.epochScope())
	data, err := os.ReadFile(filepath.Join(want, "child-home"))
	if err != nil {
		t.Fatalf("CLI child did not run with the per-epoch HOME: %v", err)
	}
	if got := strings.TrimSpace(string(data)); got != want {
		t.Fatalf("child HOME = %q, want %q", got, want)
	}
}

// The first turn of a session is the boot/priming turn — the one gc is most
// likely to interrupt — and it has no session id yet to key a mirror by. It
// still must not vanish.
func TestCancelledFirstTurnStaysVisibleAndIsAdopted(t *testing.T) {
	t.Parallel()

	h := newHarness(t, map[string]string{"STUB_SLEEP": "30", "STUB_SID": "sess_after_cancel"})
	s := h.start()
	s.send("the boot prompt")
	s.waitForOutput("zcode-repl turn in flight", 20*time.Second)
	s.signal(syscall.SIGINT)
	s.waitForOutput("zcode-repl error rc=", 15*time.Second)

	pending := h.readExport(h.pendingSessionID())
	if len(pending.Messages) != 2 || pending.Messages[0].Parts[0].Text != "the boot prompt" {
		t.Fatalf("canceled first turn left no usable trace: %+v", pending.Messages)
	}
	if got := pending.Messages[1].Info.Role; got != "assistant" {
		t.Fatalf("canceled first turn was left open (tail role %q)", got)
	}

	// The next successful turn adopts it, so the conversation reads in order.
	h.env["STUB_SLEEP"] = "0"
	h.control(map[string]string{"STUB_SLEEP": "0"})
	s.send("the turn that lands")
	deadline := time.Now().Add(20 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(h.mirrorDir, h.epochScope(), "sess_after_cancel.json")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("session mirror never appeared:\n%s", s.output())
		}
		time.Sleep(200 * time.Millisecond)
	}
	s.signal(syscall.SIGTERM)
	if _, code := s.wait(); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}

	export := h.readExport("sess_after_cancel")
	if len(export.Messages) < 4 {
		t.Fatalf("adopted history = %d messages, want the closed canceled turn plus the pair", len(export.Messages))
	}
	if got := export.Messages[0].Parts[0].Text; got != "the boot prompt" {
		t.Fatalf("first message = %q, want the canceled boot prompt carried forward", got)
	}
	if got := export.Messages[0].Info.SessionID; got != "sess_after_cancel" {
		t.Fatalf("adopted message sessionID = %q, want sess_after_cancel", got)
	}
	if _, err := os.Stat(filepath.Join(h.mirrorDir, h.epochScope(), h.pendingSessionID()+".json")); !os.IsNotExist(err) {
		t.Fatalf("placeholder export survived adoption: %v", err)
	}
}

// A turn that fails or is interrupted must CLOSE in the mirror. The user
// message was published when the turn started, so leaving it unpaired reads as
// "a turn is in flight" forever while the pane sits idle at its marker.
func TestFailedTurnClosesTheMirrorEntry(t *testing.T) {
	t.Parallel()

	h := newHarness(t, map[string]string{"STUB_SID": "sess_closes"})
	// One process: the first turn validates the sid, so the failing turn takes
	// the plain failure path rather than the stale-sid recovery path.
	s := h.start()
	s.send("establish the session")
	s.waitForTurns(1)
	h.control(map[string]string{"STUB_RC": "1"})
	s.send("a turn that fails")
	s.waitForTurns(2)
	if _, code := s.closeAndWait(); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}

	export := h.readExport("sess_closes")
	last := export.Messages[len(export.Messages)-1]
	if last.Info.Role != "assistant" {
		t.Fatalf("mirror tail role = %q, want assistant so the turn reads as finished", last.Info.Role)
	}
	if !strings.Contains(last.Parts[0].Text, "rc=1") {
		t.Fatalf("closing entry = %q, want the failure outcome", last.Parts[0].Text)
	}
	if prev := export.Messages[len(export.Messages)-2]; prev.Info.Role != "user" {
		t.Fatalf("failed turn's prompt was not recorded: %+v", prev)
	}
}

// The same invariant for an interrupt, which takes the identical path.
func TestInterruptedTurnClosesTheMirrorEntry(t *testing.T) {
	t.Parallel()

	h := newHarness(t, map[string]string{"STUB_SID": "sess_int_closes"})
	h.run("establish the session\n")

	h.env["STUB_SLEEP"] = "30"
	s := h.start()
	s.send("a turn to interrupt")
	s.waitForOutput("zcode-repl turn in flight", 20*time.Second)
	s.signal(syscall.SIGINT)
	s.waitForOutput("zcode-repl error rc=", 15*time.Second)
	s.signal(syscall.SIGTERM)
	if _, code := s.wait(); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}

	export := h.readExport("sess_int_closes")
	last := export.Messages[len(export.Messages)-1]
	if last.Info.Role != "assistant" {
		t.Fatalf("mirror tail role = %q, want assistant after an interrupt", last.Info.Role)
	}
	if !strings.Contains(last.Parts[0].Text, "interrupted") {
		t.Fatalf("closing entry = %q, want the interrupt outcome", last.Parts[0].Text)
	}
}

// Entry ids are identities to gc: HistorySnapshot.Cursor.AfterEntryID is one,
// and history comparison short-circuits on id equality. Positional ids alone
// made every conversation emit the same sequence, so two unrelated
// conversations compared as the same history and a cursor from one addressed a
// position in the other.
func TestMirrorEntryIDsAreUniqueAcrossConversations(t *testing.T) {
	t.Parallel()

	h := newHarness(t, map[string]string{"STUB_SID": "sess_first"})
	h.run("turn one\n")
	h.run("turn two\n")
	first := h.readExport("sess_first")

	// A second conversation, same length, same prompts, same scope.
	h.env["GC_CONTINUATION_EPOCH"] = "2"
	h.env["STUB_SID"] = "sess_second"
	h.run("turn one\n")
	h.run("turn two\n")
	second := h.readExport("sess_second")

	if len(first.Messages) == 0 || len(first.Messages) != len(second.Messages) {
		t.Fatalf("expected two equal-length conversations, got %d and %d",
			len(first.Messages), len(second.Messages))
	}
	seen := map[string]string{}
	for _, m := range first.Messages {
		seen[m.Info.ID] = "first"
	}
	for _, m := range second.Messages {
		if owner, clash := seen[m.Info.ID]; clash {
			t.Fatalf("entry id %q appears in both the %s and second conversation", m.Info.ID, owner)
		}
	}
	// And each id still names its own session, so an id is self-describing.
	for _, m := range first.Messages {
		if !strings.HasPrefix(m.Info.ID, "sess_first-") {
			t.Fatalf("entry id %q is not namespaced by its session", m.Info.ID)
		}
	}
}

// Config preconditions fail closed with EX_CONFIG, never a half-live pane.
func TestMissingConfigExitsSeventyEight(t *testing.T) {
	t.Parallel()

	for name, drop := range map[string]string{
		"missing bundle": "ZCODE_CJS",
		"missing key":    "ZCODE_API_KEY",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			h := newHarness(t, nil)
			delete(h.env, drop)
			_, code := h.run("anything\n")
			if code != 78 {
				t.Fatalf("exit code = %d, want 78 (EX_CONFIG)", code)
			}
			if len(h.calls()) != 0 {
				t.Fatalf("adapter called the CLI despite missing %s", drop)
			}
		})
	}
}

type mirrorExport struct {
	Info struct {
		ID        string `json:"id"`
		Directory string `json:"directory"`
	} `json:"info"`
	Messages []struct {
		Info struct {
			ID        string         `json:"id"`
			SessionID string         `json:"sessionID"`
			Role      string         `json:"role"`
			ParentID  string         `json:"parentID"`
			Usage     map[string]any `json:"usage"`
			Time      struct {
				Created int64 `json:"created"`
			} `json:"time"`
		} `json:"info"`
		Parts []struct {
			ID   string `json:"id"`
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"parts"`
	} `json:"messages"`
}

// pendingSessionID mirrors the adapter's scope-derived placeholder id for turns
// canceled before a session id existed.
func (h *harness) pendingSessionID() string {
	return "pending-" + regexp.MustCompile(`[^A-Za-z0-9._-]`).ReplaceAllString(h.epochScope(), "_")
}

// epochScope mirrors the adapter's per-epoch mirror directory, which is how a
// conversation reset orphans the prior conversation's plaintext.
func (h *harness) epochScope() string {
	epoch := h.env["GC_CONTINUATION_EPOCH"]
	if epoch == "" {
		epoch = "1"
	}
	return h.env["GC_SESSION"] + "#" + epoch
}

func (h *harness) readExport(sessionID string) mirrorExport {
	h.t.Helper()
	data, err := os.ReadFile(filepath.Join(h.mirrorDir, h.epochScope(), sessionID+".json"))
	if err != nil {
		h.t.Fatalf("read export mirror: %v", err)
	}
	var export mirrorExport
	if err := json.Unmarshal(data, &export); err != nil {
		h.t.Fatalf("parse export mirror: %v", err)
	}
	return export
}

func containsString(haystack []string, needle string) bool {
	for _, item := range haystack {
		if item == needle {
			return true
		}
	}
	return false
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// extractPyFunc slices a top-level function definition out of the embedded
// adapter script so a test can exercise it in isolation, without the script's
// argv dispatch running. Top-level defs are separated by blank lines.
func extractPyFunc(t *testing.T, name string) string {
	t.Helper()
	src := string(zcodeadapter.Script())
	start := strings.Index(src, "\ndef "+name+"(")
	if start < 0 {
		t.Fatalf("function %q not found in adapter script", name)
	}
	start++ // step past the newline onto the def
	body := src[start:]
	end := strings.Index(body, "\n\ndef ")
	if end < 0 {
		t.Fatalf("could not bound function %q in adapter script", name)
	}
	return body[:end]
}

// TestLastUserTextScansBackwardForTheLastUserMessage pins the helper's
// documented contract: it returns the text of the LAST user message anywhere in
// the history, not only when the final message happens to be the user's. The
// dedup guard in mode_reply relies on that backward scan to avoid re-publishing
// a user turn mode_prompt already wrote; a version that inspects only the final
// message returns None the moment an assistant reply sits at the tail.
func TestLastUserTextScansBackwardForTheLastUserMessage(t *testing.T) {
	t.Parallel()

	driver := extractPyFunc(t, "last_user_text") + "\n\n" +
		"import json, sys\n" +
		"val = last_user_text(json.load(open(sys.argv[1])))\n" +
		"sys.stdout.write('NONE' if val is None else val)\n"
	driverPath := filepath.Join(t.TempDir(), "driver.py")
	if err := os.WriteFile(driverPath, []byte(driver), 0o644); err != nil {
		t.Fatalf("write driver: %v", err)
	}

	cases := []struct {
		name     string
		messages string
		want     string
	}{
		{
			name:     "user before assistant tail",
			messages: `[{"info":{"role":"user"},"parts":[{"text":"hello"}]},{"info":{"role":"assistant"},"parts":[{"text":"hi"}]}]`,
			want:     "hello",
		},
		{
			name:     "most recent of several user turns",
			messages: `[{"info":{"role":"user"},"parts":[{"text":"first"}]},{"info":{"role":"assistant"},"parts":[{"text":"a"}]},{"info":{"role":"user"},"parts":[{"text":"second"}]},{"info":{"role":"assistant"},"parts":[{"text":"b"}]}]`,
			want:     "second",
		},
		{
			name:     "no user message",
			messages: `[{"info":{"role":"assistant"},"parts":[{"text":"only"}]}]`,
			want:     "NONE",
		},
		{
			name:     "empty history",
			messages: `[]`,
			want:     "NONE",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			exportPath := filepath.Join(t.TempDir(), "export.json")
			if err := os.WriteFile(exportPath, []byte(`{"info":{"id":"s"},"messages":`+tc.messages+`}`), 0o644); err != nil {
				t.Fatalf("write export: %v", err)
			}
			out, err := exec.Command("python3", driverPath, exportPath).Output()
			if err != nil {
				t.Fatalf("run driver: %v", err)
			}
			if got := string(out); got != tc.want {
				t.Fatalf("last_user_text = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestUnparsableResponseClosesTheMirrorEntry is the rc=0 twin of
// TestFailedTurnClosesTheMirrorEntry. An unparsable reply still finishes the
// turn, so the user message mode_prompt published when the turn started must be
// closed out — a trailing user tail reads as "still in flight" to every
// consumer of the mirror even though the pane is idle at its marker.
func TestUnparsableResponseClosesTheMirrorEntry(t *testing.T) {
	t.Parallel()

	h := newHarness(t, map[string]string{"STUB_SID": "sess_unparsable"})
	h.run("establish the session\n")

	h.env["STUB_BAD_JSON"] = "1"
	out, code := h.run("go dark\n")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if !strings.Contains(out, "zcode-repl error rc=0 (unparsable response)") {
		t.Fatalf("missing unparsable-response report:\n%s", out)
	}

	export := h.readExport("sess_unparsable")
	if len(export.Messages) != 4 {
		t.Fatalf("messages = %d, want 4 (turn 1 pair + published + closed-out unparsable turn):\n%+v", len(export.Messages), export.Messages)
	}
	if third := export.Messages[2]; third.Info.Role != "user" || third.Parts[0].Text != "go dark" {
		t.Fatalf("third message = %+v, want the published user turn", third)
	}
	last := export.Messages[3]
	if last.Info.Role != "assistant" {
		t.Fatalf("tail role = %q, want assistant so the turn reads as finished", last.Info.Role)
	}
	if !strings.Contains(last.Parts[0].Text, "unparsable response") {
		t.Fatalf("tail note = %q, want the unparsable-response outcome", last.Parts[0].Text)
	}
}
