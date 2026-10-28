package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// hookRunSealChildEnv turns the re-executed test binary into a wrapped child
// that frames a payload in seals as nudge drain does, prints an unsealed tail,
// and reports that it has committed (as nudge drain does when it acks). Set to
// hookRunSealChildWedge, the child then wedges past the wrapper's deadline;
// set to hookRunSealChildExit, it exits 0. TestMain runs it before any package
// setup, so killing it leaks nothing.
const hookRunSealChildEnv = "GC_TEST_HOOK_RUN_SEAL_CHILD"

const (
	hookRunSealChildWedge = "wedge"
	hookRunSealChildExit  = "exit"
)

// hookRunSealChildWatchdog bounds the wedged child only in case the wrapper
// never kills it; the test itself ends on the child's commit report.
const hookRunSealChildWatchdog = time.Minute

const (
	hookRunSealChildPayload   = "nudge payload printed before the commit\n"
	hookRunSealChildTail      = "partial output printed after the seal"
	hookRunSealChildCommitted = "committed"
)

func maybeRunHookRunSealChild() {
	mode := os.Getenv(hookRunSealChildEnv)
	if mode != hookRunSealChildWedge && mode != hookRunSealChildExit {
		return
	}
	if err := sealHookRunOutput(os.Stdout); err != nil {
		os.Exit(97)
	}
	if _, err := io.WriteString(os.Stdout, hookRunSealChildPayload); err != nil {
		os.Exit(97)
	}
	if err := sealHookRunOutput(os.Stdout); err != nil {
		os.Exit(97)
	}
	if _, err := io.WriteString(os.Stdout, hookRunSealChildTail); err != nil {
		os.Exit(97)
	}
	if _, err := io.WriteString(os.Stderr, hookRunSealChildCommitted+"\n"); err != nil {
		os.Exit(97)
	}
	if mode == hookRunSealChildExit {
		os.Exit(0)
	}
	// Wedge after the commit until the wrapper kills the process.
	<-time.After(hookRunSealChildWatchdog)
	os.Exit(0)
}

// signalWriter records everything written to it and closes seen once the
// output contains want.
type signalWriter struct {
	mu   sync.Mutex
	buf  bytes.Buffer
	want string
	seen chan struct{}
	once sync.Once
}

func newSignalWriter(want string) *signalWriter {
	return &signalWriter{want: want, seen: make(chan struct{})}
}

func (w *signalWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf.Write(p)
	if strings.Contains(w.buf.String(), w.want) {
		w.once.Do(func() { close(w.seen) })
	}
	return len(p), nil
}

func (w *signalWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

func TestHookRunResult(t *testing.T) {
	const (
		seal            = "SEAL"
		timeoutExitCode = 99
	)
	// In out, "|" stands for a complete seal as a child writes it. A NUL
	// followed by part of the seal is a seal write that failed part-way.
	tests := []struct {
		name      string
		childCode int
		timedOut  bool
		captured  bool
		out       string
		wantOut   string
		wantCode  int
	}{
		{name: "clean exit forwards everything without seals", captured: true, out: "one|two|three", wantOut: "onetwothree"},
		{name: "child status passes through", childCode: 7, captured: true, out: "partial", wantOut: "partial", wantCode: 7},
		{name: "clean exit drops a frame a failed write left open", captured: true, out: "before|partial payl", wantOut: "before"},
		{name: "clean exit drops a frame whose closing seal was cut short", captured: true, out: "before|payload\x00SE", wantOut: "before"},
		{name: "clean exit drops an opening seal that was cut short", captured: true, out: "before\x00S", wantOut: "before"},
		{name: "failed child keeps closed frames only", childCode: 3, captured: true, out: "|payload|after|torn", wantOut: "payloadafter", wantCode: 3},
		{name: "timeout drops unsealed output", timedOut: true, captured: true, out: "partial", wantCode: timeoutExitCode},
		{name: "timeout keeps a closed frame and drops the tail", timedOut: true, captured: true, out: "|payload|tail", wantOut: "payload", wantCode: timeoutExitCode},
		{name: "timeout drops a frame that never closed", timedOut: true, captured: true, out: "|one||two", wantOut: "one", wantCode: timeoutExitCode},
		{name: "failed read keeps only sealed output and fails open", captured: false, out: "|payload|tail", wantOut: "payload", wantCode: timeoutExitCode},
		{name: "failed read inside a seal leaks no fragment", captured: false, out: "|payload|tail\x00SE", wantOut: "payload", wantCode: timeoutExitCode},
		{name: "failed read before any seal forwards nothing", captured: false, out: "payload\x00SE", wantCode: timeoutExitCode},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := []byte(strings.ReplaceAll(tt.out, "|", "\x00"+seal))
			gotOut, gotCode := hookRunResult(tt.childCode, tt.timedOut, tt.captured, out, seal, timeoutExitCode)
			if string(gotOut) != tt.wantOut || gotCode != tt.wantCode {
				t.Errorf("hookRunResult(%d, timedOut=%t, captured=%t, %q) = (%q, %d), want (%q, %d)",
					tt.childCode, tt.timedOut, tt.captured, out, gotOut, gotCode, tt.wantOut, tt.wantCode)
			}
		})
	}
}

// TestHookRunSpoolIsGoneOnceClosed pins that capturing a child's output
// leaves no file behind once the wrapper is done with it.
func TestHookRunSpoolIsGoneOnceClosed(t *testing.T) {
	spool, err := createHookRunSpool()
	if err != nil {
		t.Fatalf("createHookRunSpool: %v", err)
	}
	name := spool.out.Name()
	if _, err := io.WriteString(spool.out, "payload"); err != nil {
		t.Fatalf("writing spool: %v", err)
	}
	got, err := spool.read()
	if err != nil || string(got) != "payload" {
		t.Fatalf("spool.read = (%q, %v), want (%q, nil)", got, err, "payload")
	}
	var stderr bytes.Buffer
	spool.close(&stderr)
	if stderr.Len() != 0 {
		t.Fatalf("spool.close reported %q, want nothing", stderr.String())
	}
	if _, err := os.Stat(name); !os.IsNotExist(err) {
		t.Fatalf("spool %s still exists after close (stat err=%v)", name, err)
	}
}

// useTestBinaryAsHookRunChild makes gc hook run re-execute the test binary as
// a seal child in the given mode.
func useTestBinaryAsHookRunChild(t *testing.T, mode string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	previous := hookRunExecutable
	hookRunExecutable = func() (string, error) { return exe, nil }
	t.Cleanup(func() { hookRunExecutable = previous })
	t.Setenv(hookRunSealChildEnv, mode)
}

// TestHookRunFailsOpenWithoutStartingTheChildWhenNoSpool pins that gc hook
// run does not start the wrapped command when it cannot create its output
// spool. Capturing through a pipe instead could lose output the child commits
// after printing: WaitDelay can close the pipe before its bytes are read. A
// command that never ran has committed nothing, so failing open loses nothing.
func TestHookRunFailsOpenWithoutStartingTheChildWhenNoSpool(t *testing.T) {
	useTestBinaryAsHookRunChild(t, hookRunSealChildExit)
	missing := filepath.Join(t.TempDir(), "missing")
	for _, name := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(name, missing)
	}
	const timeoutExitCode = 5

	var stdout, stderr bytes.Buffer
	code := runHookChild(context.Background(), []string{"nudge", "drain", "--inject"}, 15*time.Second, timeoutExitCode, nil, &stdout, &stderr)

	if code != timeoutExitCode {
		t.Fatalf("runHookChild code = %d, want fail-open %d; stderr=%s", code, timeoutExitCode, stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want nothing", stdout.String())
	}
	if strings.Contains(stderr.String(), hookRunSealChildCommitted) {
		t.Fatalf("the child ran without an output spool; stderr=%s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "not running the command") {
		t.Fatalf("stderr = %q, want the spool failure reported", stderr.String())
	}
}

// TestHookRunForwardsSealedOutputWhenDeadlineFollowsCommit is the regression
// for wrapped nudge drains losing nudges: drain prints the nudges, acks them,
// and only then does the wrapper's deadline fire. The wrapper used to discard
// everything a timed-out child printed, so acked nudges never reached the
// provider. Output the child sealed before committing must be forwarded; the
// unsealed tail after it may be partial and must still be dropped.
func TestHookRunForwardsSealedOutputWhenDeadlineFollowsCommit(t *testing.T) {
	useTestBinaryAsHookRunChild(t, hookRunSealChildWedge)

	// The deadline is a safety net only; the test ends the run as soon as the
	// child reports its commit.
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	stderr := newSignalWriter(hookRunSealChildCommitted)
	go func() {
		select {
		case <-stderr.seen:
			cancel()
		case <-ctx.Done():
		}
	}()

	var stdout bytes.Buffer
	code := runHookChild(ctx, []string{"nudge", "drain", "--inject"}, 15*time.Second, 0, nil, &stdout, stderr)

	select {
	case <-stderr.seen:
	default:
		t.Fatalf("child never reported its commit; stderr=%s", stderr.String())
	}
	if code != 0 {
		t.Fatalf("runHookChild code = %d, want fail-open 0; stderr=%s", code, stderr.String())
	}
	if got := stdout.String(); got != hookRunSealChildPayload {
		t.Fatalf("stdout = %q, want the sealed payload %q; stderr=%s", got, hookRunSealChildPayload, stderr.String())
	}
	if !strings.Contains(stderr.String(), "timed out after 15s") {
		t.Fatalf("stderr = %q, want timeout diagnostic", stderr.String())
	}
}
