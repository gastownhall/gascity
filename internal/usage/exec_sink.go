package usage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"time"
)

// DefaultExecSinkTimeout bounds a single ExecSink.Record invocation. The [Sink]
// contract requires Record not to block the caller's hot path; callers
// (controller reconcile tick, worker op-finish) may pass a long-lived context,
// so ExecSink derives its own per-fact deadline instead of trusting the caller's
// context to be bounded. A hung script is then surfaced as a timeout error —
// handled like any other failed write (logged, retried on the next tick) — not
// an indefinite stall.
const DefaultExecSinkTimeout = 5 * time.Second

// execSinkKillGrace bounds post-timeout cleanup. When the per-fact deadline
// fires, the script is killed; if it (or a child it spawned) outlives the signal
// or keeps the stderr pipe open, WaitDelay makes Run close the pipes and return
// after this grace instead of blocking on the descendant.
const execSinkKillGrace = 2 * time.Second

// ExecSink records each fact by invoking an external script with the fact's
// JSON on stdin (one JSON object, newline-terminated). It is the out-of-process
// injection seam — the script is the integration point for an external
// aggregator, exchanged over a JSON wire contract rather than a linked Go API
// (mirroring the events exec provider).
//
// Each Record invocation is bounded by the sink's per-fact timeout
// ([DefaultExecSinkTimeout] unless overridden) so a slow or hung script cannot
// block a latency-sensitive caller. The script must therefore return promptly;
// callers with high-frequency facts should still prefer the durable LocalSink
// (which an external drainer can tail) and reserve ExecSink for low-frequency
// facts.
type ExecSink struct {
	script  string
	timeout time.Duration
}

// NewExecSink returns an ExecSink that invokes script for each fact, bounding
// every invocation by [DefaultExecSinkTimeout].
func NewExecSink(script string) *ExecSink {
	return &ExecSink{script: script, timeout: DefaultExecSinkTimeout}
}

// NewExecSinkWithTimeout returns an ExecSink with an explicit per-fact timeout.
// A non-positive timeout disables the internal bound, leaving cancellation to
// the caller's context. It is primarily for tests and tuning.
func NewExecSinkWithTimeout(script string, timeout time.Duration) *ExecSink {
	return &ExecSink{script: script, timeout: timeout}
}

// Record marshals f to JSON and feeds it to the script on stdin, bounded by the
// sink's per-fact timeout so a hung script cannot block the caller indefinitely.
func (s *ExecSink) Record(ctx context.Context, f Fact) error {
	line, err := json.Marshal(f)
	if err != nil {
		return err
	}
	if s.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.timeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, s.script)
	cmd.WaitDelay = execSinkKillGrace
	cmd.Stdin = bytes.NewReader(append(line, '\n'))
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("usage exec sink %q timed out after %s: %w", s.script, s.timeout, ctx.Err())
		}
		return fmt.Errorf("usage exec sink %q: %w: %s", s.script, err, stderr.String())
	}
	return nil
}
