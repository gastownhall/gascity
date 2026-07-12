package main

import (
	"bytes"
	"io"
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
