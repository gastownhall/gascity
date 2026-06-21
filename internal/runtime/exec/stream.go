package exec

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// Provider implements the optional read-only streaming primitive.
var _ runtime.OutputStreamer = (*Provider)(nil)

// streamReadChunk is the read buffer for a proc.stream pipe. Output is emitted
// as it arrives (one frame per read), so this only bounds the largest single
// frame, not latency.
const streamReadChunk = 32 * 1024

// StreamOutput runs argv inside the session via the RPP `proc-stream` op and
// implements [runtime.OutputStreamer]. The command is POSIX shell-quoted onto
// the op's stdin (the same wire convention as the `exec` op), the op runs it
// read-only, and its stdout/stderr are streamed back frame-for-frame until the
// command exits or ctx is canceled.
//
// Unlike Exec, proc.stream is gated purely by the handshake: a persistent stream
// cannot be carried over the request/response exec connection, so there is no
// exit-2 "unknown op" fallback to lean on — a runtime that does not declare
// proc.stream gets [runtime.ErrStreamUnsupported] before anything is spawned, and
// the caller falls back to polling. (Because the op is declared, an exit code of
// 2 from it is the command's own result, not the unknown-op sentinel.)
func (p *Provider) StreamOutput(ctx context.Context, name string, argv []string) (<-chan runtime.StreamFrame, error) {
	if !p.handshakeCapability(runtime.ProtocolCapabilityProcStream) {
		return nil, fmt.Errorf("%w: %s proc-stream %s", runtime.ErrStreamUnsupported, p.script, name)
	}

	streamCtx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(streamCtx, p.script, "proc-stream", name)
	// WaitDelay forcibly closes the pipes shortly after the context expires, so a
	// canceled stream cannot hang behind a wrapper that keeps stdout open (e.g. a
	// `tail -F` grandchild).
	cmd.WaitDelay = 2 * time.Second
	cmd.Stdin = strings.NewReader(shellQuote(argv))

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("exec provider %s proc-stream %s: stdout pipe: %w", p.script, name, err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("exec provider %s proc-stream %s: stderr pipe: %w", p.script, name, err)
	}
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("exec provider %s proc-stream %s: %w", p.script, name, err)
	}

	frames := make(chan runtime.StreamFrame, 16)
	go func() {
		defer cancel()
		defer close(frames)

		var wg sync.WaitGroup
		wg.Add(2)
		go pumpStream(streamCtx, &wg, stdout, frames, false)
		go pumpStream(streamCtx, &wg, stderr, frames, true)

		// Reap the process in parallel with the pumps, not after them. On a clean
		// exit both pipes hit EOF and the pumps drain to completion on their own.
		// But on cancellation the process is killed while an orphaned grandchild
		// (e.g. a `tail -F`) may still hold a pipe's write end open, leaving an idle
		// pump wedged in Read; Wait's WaitDelay force-closes the pipes after the
		// process exits, which is what unblocks that read. Calling Wait only after
		// wg.Wait would deadlock the two against each other.
		waitDone := make(chan error, 1)
		go func() { waitDone <- cmd.Wait() }()
		wg.Wait()
		waitErr := <-waitDone

		select {
		case frames <- terminalFrame(ctx, p, name, waitErr):
		case <-ctx.Done():
		}
	}()

	return frames, nil
}

// pumpStream copies one pipe into frames, one frame per read, tagging frames as
// stdout or stderr. It unblocks on ctx cancellation even if the consumer has
// stopped reading, so a canceled stream never wedges on a full channel.
func pumpStream(ctx context.Context, wg *sync.WaitGroup, r io.Reader, frames chan<- runtime.StreamFrame, isErr bool) {
	defer wg.Done()
	buf := make([]byte, streamReadChunk)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			frame := runtime.StreamFrame{Stdout: chunk}
			if isErr {
				frame = runtime.StreamFrame{Stderr: chunk}
			}
			select {
			case frames <- frame:
			case <-ctx.Done():
				return
			}
		}
		if err != nil {
			return // io.EOF on clean close, or a read error when the pipe is torn down
		}
	}
}

// terminalFrame classifies the proc-stream subprocess's exit into the single
// terminal frame. Caller cancellation/timeout is the caller's own ctx error; a
// clean exit (including a non-zero command code) is an Exit frame; a signal kill
// we did not cause, or any other spawn/transport failure, is an Err frame.
func terminalFrame(ctx context.Context, p *Provider, name string, waitErr error) runtime.StreamFrame {
	if ctx.Err() != nil {
		return runtime.StreamFrame{Err: ctx.Err()}
	}
	if waitErr == nil {
		code := 0
		return runtime.StreamFrame{Exit: &code}
	}
	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) {
		// ExitCode is -1 when the process was terminated by a signal we did not
		// request (ctx was not canceled): that is a transport failure, not a clean
		// command result.
		if code := exitErr.ExitCode(); code >= 0 {
			return runtime.StreamFrame{Exit: &code}
		}
	}
	return runtime.StreamFrame{Err: fmt.Errorf("exec provider %s proc-stream %s: %w", p.script, name, waitErr)}
}
