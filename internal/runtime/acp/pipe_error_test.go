package acp

import (
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

func TestIsPipeWriteError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "io.ErrClosedPipe", err: io.ErrClosedPipe, want: true},
		{name: "EPIPE", err: syscall.EPIPE, want: true},
		{name: "os.ErrClosed", err: os.ErrClosed, want: true},
		{
			name: "wrapped os.ErrClosed from a closed os.File pipe",
			err:  fmt.Errorf("write: %w", &os.PathError{Op: "write", Path: "|1", Err: os.ErrClosed}),
			want: true,
		},
		{name: "wrapped EPIPE", err: fmt.Errorf("write: %w", syscall.EPIPE), want: true},
		{name: "unrelated error", err: errors.New("disk quota exceeded"), want: false},
		{name: "nil", err: nil, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isPipeWriteError(tt.err); got != tt.want {
				t.Fatalf("isPipeWriteError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// signalingClosedFile wraps the parent's write end of a real pipe that has
// already been closed, the state exec.Cmd.Wait leaves the StdinPipe in once
// the agent exits. Writes therefore fail with the genuine
// "write |1: file already closed" (os.ErrClosed) error. The first Write
// signals so the test can close sc.done the way the monitor goroutine does.
type signalingClosedFile struct {
	f           *os.File
	writeCalled chan struct{}
}

func (s *signalingClosedFile) Write(p []byte) (int, error) {
	n, err := s.f.Write(p)
	select {
	case <-s.writeCalled:
	default:
		close(s.writeCalled)
	}
	return n, err
}

func (s *signalingClosedFile) Close() error { return nil }

// TestNudge_ReturnsNilWhenStdinAlreadyClosedByWait pins the race where the
// agent has exited and cmd.Wait has closed the parent's stdin pipe, but the
// monitor goroutine has not yet closed sc.done. The write fails with
// os.ErrClosed, which must take the best-effort agent-exit path (wait for
// sc.done, return nil) instead of surfacing as a send error.
func TestNudge_ReturnsNilWhenStdinAlreadyClosedByWait(t *testing.T) {
	p := NewProviderWithDir(shortTempDir(t), Config{NudgeBusyTimeout: 2 * time.Second})
	name := testName()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })
	if err := w.Close(); err != nil {
		t.Fatalf("closing pipe writer: %v", err)
	}
	stdin := &signalingClosedFile{f: w, writeCalled: make(chan struct{})}

	sc := newSessionConn(nil, stdin, nil, 100, nil)
	sc.sessionID = "session-1"

	p.mu.Lock()
	p.conns[name] = sc
	p.mu.Unlock()

	go func() {
		<-stdin.writeCalled
		close(sc.done)
	}()

	done := make(chan error, 1)
	go func() {
		done <- p.Nudge(name, []runtime.ContentBlock{{Type: "text", Text: "hi"}})
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Nudge after stdin closed by Wait = %v, want nil (best-effort)", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Nudge did not return within 3s")
	}
}
