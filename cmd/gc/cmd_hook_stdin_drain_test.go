package main

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

// trackingReader counts how many bytes were read from the wrapped reader, so a
// test can assert gc hook run fully consumed the provider's hook stdin.
type trackingReader struct {
	r    io.Reader
	read int
}

func (t *trackingReader) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	t.read += n
	return n, err
}

// TestHookRunConsumesStdinWhenWrappedCommandIgnoresIt is the regression for the
// fleet-wide "UserPromptSubmit hook (failed): failed to write hook stdin:
// Broken pipe (os error 32)" on every codex prompt submit. gc hook run forwards
// the provider's stdin to the wrapped command (e.g. `nudge drain --inject`);
// when that command exits — on its fast path or on the timeout — before
// consuming the payload, gc hook run returned and closed the pipe under codex's
// in-flight write, killing nudge-drain and mail-check injection silently.
//
// gc hook run must fully consume its stdin so the provider's write always
// completes, regardless of whether the wrapped command reads it. The wrapped
// executable here is /bin/true, which exits 0 without reading stdin.
func TestHookRunConsumesStdinWhenWrappedCommandIgnoresIt(t *testing.T) {
	orig := hookRunExecutable
	hookRunExecutable = func() (string, error) { return "/bin/true", nil }
	t.Cleanup(func() { hookRunExecutable = orig })

	payload := strings.Repeat("x", 8192)
	tr := &trackingReader{r: strings.NewReader(payload)}
	var stdout, stderr bytes.Buffer

	code := cmdHookRun(
		[]string{"nudge", "drain", "--inject"},
		hookRunOptions{Timeout: 5 * time.Second, TimeoutExitCode: 0},
		tr, &stdout, &stderr,
	)

	if code != 0 {
		t.Fatalf("cmdHookRun = %d, want 0; stderr=%q", code, stderr.String())
	}
	if tr.read < len(payload) {
		t.Fatalf("gc hook run consumed only %d/%d bytes of the provider's stdin; a wrapped command that ignores stdin must not leave the provider's write unconsumed (that is the EPIPE)", tr.read, len(payload))
	}
}

// blockingReader delivers a finite prefix, then blocks on the next Read until
// release is closed — modeling a provider pipe that writes less than the 1 MiB
// drain limit of hook stdin and never closes it (no EOF). The test closes
// release on cleanup so the gc hook run drain goroutine cannot leak past it.
type blockingReader struct {
	prefix  []byte
	release <-chan struct{}
}

func (b *blockingReader) Read(p []byte) (int, error) {
	if len(b.prefix) > 0 {
		n := copy(p, b.prefix)
		b.prefix = b.prefix[n:]
		return n, nil
	}
	<-b.release
	return 0, io.EOF
}

// TestHookRunReturnsWithinTimeoutWhenStdinNeverEOFs pins the fix for the review
// finding that the pre-spawn stdin drain was not bounded by the hard timeout. gc
// hook run buffers provider stdin before running the wrapped command, and
// os.Stdin has no read deadline, so a provider that writes less than 1 MiB
// without closing stdin (no EOF) blocked io.ReadAll forever — before cmd.Run()
// and past the advertised timeout — freezing the prompt-submit hot path. The
// drain must stay bounded by ctx so gc hook run always fails open within the
// configured timeout instead of wedging before it spawns the child.
func TestHookRunReturnsWithinTimeoutWhenStdinNeverEOFs(t *testing.T) {
	orig := hookRunExecutable
	hookRunExecutable = func() (string, error) { return "/bin/true", nil }
	t.Cleanup(func() { hookRunExecutable = orig })

	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	stdin := &blockingReader{prefix: []byte(`{"transcript_path":"/tmp/t.jsonl"}`), release: release}
	var stdout, stderr bytes.Buffer

	const timeout = 200 * time.Millisecond
	done := make(chan int, 1)
	start := time.Now()
	go func() {
		done <- cmdHookRun(
			[]string{"nudge", "drain", "--inject"},
			hookRunOptions{Timeout: timeout, TimeoutExitCode: 124},
			stdin, &stdout, &stderr,
		)
	}()

	select {
	case code := <-done:
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Fatalf("cmdHookRun returned after %s, want ~%s: the pre-spawn stdin drain is not bounded by the hard timeout", elapsed, timeout)
		}
		if code != 124 {
			t.Fatalf("cmdHookRun = %d, want 124 (fail-open timeout); stderr=%q", code, stderr.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("cmdHookRun did not return within 10s: the pre-spawn stdin drain blocked past the hard timeout (the regression)")
	}
}

// TestHookRunSkipsStdinDrainForTerminal pins the fix for the iteration-2 review
// finding that the pre-spawn drain omitted readHookStdin's terminal guard. A
// char-device stdin (an interactive or inherited terminal on os.Stdin) never
// reaches EOF on its own, so draining it unblocks only when the hard timeout
// fires — a manual `gc hook run -- <cmd>` then returns the fail-open timeout code
// without ever running <cmd>. drainHookStdin must skip a terminal exactly like
// readHookStdin and run the child immediately with empty stdin, while still
// buffering and timeout-bounding pipe-based provider stdin.
//
// A PTY master from /dev/ptmx is the terminal proxy: it is a char-device
// *os.File whose Read blocks forever with no EOF while no slave writes to it,
// which is exactly the shape of os.Stdin on a real terminal. The wrapped
// executable is /bin/true, which exits 0 without reading stdin.
func TestHookRunSkipsStdinDrainForTerminal(t *testing.T) {
	tty, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		t.Skipf("cannot open /dev/ptmx as a terminal-stdin proxy: %v", err)
	}
	t.Cleanup(func() { _ = tty.Close() })
	// Guard the guard: the proxy only proves anything if it is actually a
	// char-device that blocks, matching a real terminal on os.Stdin.
	st, err := tty.Stat()
	if err != nil || st.Mode()&os.ModeCharDevice == 0 {
		t.Skipf("/dev/ptmx is not a char device here (mode=%v err=%v); cannot model terminal stdin", st.Mode(), err)
	}

	orig := hookRunExecutable
	hookRunExecutable = func() (string, error) { return "/bin/true", nil }
	t.Cleanup(func() { hookRunExecutable = orig })

	var stdout, stderr bytes.Buffer
	const timeout = 3 * time.Second
	done := make(chan int, 1)
	start := time.Now()
	go func() {
		done <- cmdHookRun(
			[]string{"nudge", "drain", "--inject"},
			hookRunOptions{Timeout: timeout, TimeoutExitCode: 124},
			tty, &stdout, &stderr,
		)
	}()

	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("cmdHookRun = %d, want 0: a terminal-backed gc hook run must skip the stdin drain and run the child, not drain the terminal until the timeout; stderr=%q", code, stderr.String())
		}
		if elapsed := time.Since(start); elapsed >= timeout {
			t.Fatalf("cmdHookRun returned after %s (>= the %s timeout): terminal stdin was drained to the deadline instead of skipped", elapsed, timeout)
		}
	case <-time.After(timeout + 5*time.Second):
		t.Fatalf("cmdHookRun did not return: terminal stdin was drained past the hard timeout instead of being skipped (the regression)")
	}
}
