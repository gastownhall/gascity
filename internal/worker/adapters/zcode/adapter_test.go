package zcode_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

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
	var out strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = io.Discard
	err := cmd.Run()
	code := 0
	if err != nil {
		var exitErr *exec.ExitError
		if !asExitError(err, &exitErr) {
			h.t.Fatalf("run adapter: %v (stdout=%s)", err, out.String())
		}
		code = exitErr.ExitCode()
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
	t    *testing.T
	cmd  *exec.Cmd
	in   io.WriteCloser
	mu   sync.Mutex
	out  strings.Builder
	done chan struct{}
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
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		h.t.Fatalf("start adapter: %v", err)
	}
	s := &session{t: h.t, cmd: cmd, in: in, done: make(chan struct{})}
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
		s.t.Fatalf("signal %v: %v", sig, err)
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
				t.Fatalf("exit code = %d, want 0", code)
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
	if _, code := s.closeAndWait(); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
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
	time.Sleep(2500 * time.Millisecond)

	h.control(map[string]string{"STUB_RC": "7"})
	s.send("this one fails")
	time.Sleep(2500 * time.Millisecond)

	h.control(map[string]string{"STUB_RC": "0"})
	s.send("this one works again")
	time.Sleep(2500 * time.Millisecond)
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
		time.Sleep(1600 * time.Millisecond)
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
	time.Sleep(3 * time.Second)
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
	time.Sleep(3 * time.Second)
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
	time.Sleep(2 * time.Second)
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
	if export.Info.Directory != h.workDir {
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

func (h *harness) readExport(sessionID string) mirrorExport {
	h.t.Helper()
	data, err := os.ReadFile(filepath.Join(h.mirrorDir, sessionID+".json"))
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
