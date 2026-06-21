package exec

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// writeStreamScript writes a minimal RPP script that declares the proc.stream
// capability and whose `proc-stream` op runs the shell-quoted command from
// stdin (`sh -c "$(cat)"`). When declareCapability is false the handshake omits
// proc.stream, so StreamOutput must refuse before spawning. Unlisted ops exit 2.
func writeStreamScript(t *testing.T, declareCapability bool) string {
	t.Helper()
	caps := `["proc.stream"]`
	if !declareCapability {
		caps = `[]`
	}
	protocol := `  protocol) printf '{"version":0,"capabilities":` + caps + `}\n' ;;`
	path := filepath.Join(t.TempDir(), "gc-runtime-stream")
	script := "#!/bin/sh\nop=\"$1\"; name=\"$2\"\ncase \"$op\" in\n" +
		protocol + "\n" +
		`  proc-stream) sh -c "$(cat)" ;;` + "\n" +
		"  *) exit 2 ;;\nesac\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// collectStream drains a frame channel until it closes, returning the
// concatenated stdout and stderr bytes and the terminal frame (Exit or Err).
func collectStream(t *testing.T, frames <-chan runtime.StreamFrame) (stdout, stderr []byte, terminal runtime.StreamFrame) {
	t.Helper()
	for f := range frames {
		switch {
		case f.Stdout != nil:
			stdout = append(stdout, f.Stdout...)
		case f.Stderr != nil:
			stderr = append(stderr, f.Stderr...)
		default:
			terminal = f
		}
	}
	return stdout, stderr, terminal
}

func TestStreamOutput_StreamsStdoutThenExit(t *testing.T) {
	p := NewProvider(writeStreamScript(t, true))
	frames, err := p.StreamOutput(context.Background(), "s", []string{"sh", "-c", "printf 'a\\n'; printf 'b\\n'"})
	if err != nil {
		t.Fatalf("StreamOutput: %v", err)
	}
	stdout, _, terminal := collectStream(t, frames)
	if got := string(stdout); got != "a\nb\n" {
		t.Errorf("stdout = %q, want %q", got, "a\nb\n")
	}
	if terminal.Err != nil {
		t.Fatalf("terminal Err = %v, want a clean Exit", terminal.Err)
	}
	if terminal.Exit == nil || *terminal.Exit != 0 {
		t.Errorf("terminal Exit = %v, want 0", terminal.Exit)
	}
}

func TestStreamOutput_SeparatesStderr(t *testing.T) {
	p := NewProvider(writeStreamScript(t, true))
	frames, err := p.StreamOutput(context.Background(), "s", []string{"sh", "-c", "printf out; printf err >&2"})
	if err != nil {
		t.Fatalf("StreamOutput: %v", err)
	}
	stdout, stderr, _ := collectStream(t, frames)
	if string(stdout) != "out" {
		t.Errorf("stdout = %q, want %q", stdout, "out")
	}
	if string(stderr) != "err" {
		t.Errorf("stderr = %q, want %q", stderr, "err")
	}
}

func TestStreamOutput_UnsupportedWithoutCapability(t *testing.T) {
	// The script implements proc-stream but does NOT declare the capability:
	// StreamOutput must refuse up front (no exit-2 fallback for a stream op).
	p := NewProvider(writeStreamScript(t, false))
	frames, err := p.StreamOutput(context.Background(), "s", []string{"echo", "hi"})
	if !errors.Is(err, runtime.ErrStreamUnsupported) {
		t.Errorf("err = %v, want runtime.ErrStreamUnsupported", err)
	}
	if frames != nil {
		t.Error("frames channel should be nil when the stream is unsupported")
	}
}

func TestStreamOutput_NonZeroExitIsTerminalExitNotErr(t *testing.T) {
	p := NewProvider(writeStreamScript(t, true))
	frames, err := p.StreamOutput(context.Background(), "s", []string{"sh", "-c", "printf x; exit 7"})
	if err != nil {
		t.Fatalf("StreamOutput: %v", err)
	}
	stdout, _, terminal := collectStream(t, frames)
	if string(stdout) != "x" {
		t.Errorf("stdout = %q, want %q", stdout, "x")
	}
	if terminal.Err != nil {
		t.Fatalf("terminal Err = %v; a non-zero command exit must be a clean Exit", terminal.Err)
	}
	if terminal.Exit == nil || *terminal.Exit != 7 {
		t.Errorf("terminal Exit = %v, want 7 (the command's own exit code)", terminal.Exit)
	}
}

func TestStreamOutput_CallerCancelClosesPromptly(t *testing.T) {
	// A long-lived tail: cancellation must tear it down and close the channel
	// without the test hanging.
	p := NewProvider(writeStreamScript(t, true))
	ctx, cancel := context.WithCancel(context.Background())
	frames, err := p.StreamOutput(ctx, "s", []string{"sh", "-c", "while :; do printf 'tick\\n'; sleep 0.1; done"})
	if err != nil {
		t.Fatalf("StreamOutput: %v", err)
	}

	// Wait for at least one live frame, then cancel.
	select {
	case f := <-frames:
		if f.Stdout == nil {
			t.Fatalf("first frame = %+v, want a stdout tick", f)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no frame within 3s; stream never produced output")
	}
	cancel()

	// The channel must close promptly (WaitDelay bounds the teardown at ~2s).
	deadline := time.After(5 * time.Second)
	for {
		select {
		case _, ok := <-frames:
			if !ok {
				return // closed: success
			}
		case <-deadline:
			t.Fatal("frames channel did not close within 5s of cancel")
		}
	}
}
