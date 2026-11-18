// Package setupcmd runs one host-side session lifecycle shell command
// (pre_start, session_setup, session_setup_script, session_live) — "in gc's
// process via sh -c", per the Config field contracts — under a bounded,
// cooperatively canceled budget.
//
// It lives beside, not in, the root runtime package because it depends on
// internal/execgrace and internal/execenv, and the root package is held to
// standard-library imports (RUNTIME-INV-001).
package setupcmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/execenv"
	"github.com/gastownhall/gascity/internal/execgrace"
	"github.com/gastownhall/gascity/internal/runtime"
)

const (
	// outputLimit bounds the captured output tail folded into a failure.
	outputLimit = 4096
	// waitDelay force-closes the capture pipe shortly after the command exits
	// (or the fixed deadline fires), so a command that daemonizes a child
	// holding inherited stdio cannot hang the wait; it is also the TERM-to-KILL
	// grace under the fixed deadline.
	waitDelay = 2 * time.Second
	// cancelGrace is the TERM-to-KILL rollback-trap budget under the
	// activity-aware budget.
	cancelGrace = 10 * time.Second
)

// Options is the budget and working-directory policy for one [Run].
type Options struct {
	// Timeout must be positive. With MaxTimeout unset it is a fixed
	// wall-clock deadline ([session] setup_timeout); with MaxTimeout set it
	// bounds output silence instead.
	Timeout time.Duration
	// MaxTimeout, when positive, switches to the activity-aware budget: the
	// command fails after Timeout without output, or after MaxTimeout in
	// total ([session] setup_max_timeout).
	MaxTimeout time.Duration
	// FallbackDir is the working directory when env["GC_DIR"] is set but does
	// not exist yet — a pre_start often creates it. Empty leaves the command
	// in the caller's working directory.
	FallbackDir string
	// Clock is the activity-aware budget's time source; nil means the real
	// clock. Tests inject one to drive the idle window deterministically. The
	// fixed deadline always runs on real time.
	Clock execgrace.Clock
}

// Run executes command with `sh -c` under opts' budget. The command's
// environment is the process environment ([execenv.Environ]) overlaid with env
// (last wins); its working directory is env["GC_DIR"] when that exists, else
// opts.FallbackDir.
//
// On budget expiry the command's process group receives SIGTERM, so shell
// rollback traps run, and is force-killed after a grace ([execgrace.Apply]).
// The returned error then wraps the budget's [context.Cause]:
// [context.DeadlineExceeded] for the fixed deadline, [execgrace.ErrIdle] or
// [execgrace.ErrCeiling] for the activity-aware one.
//
// A failure carries the last bytes of the command's combined stdout and
// stderr, scrubbed of [runtime.SetupCommandSecrets] before being cut, because
// the error is durable (logs, event bus, bead notes) and a `set -x` trace or a
// failing curl echoes credentials back. A command that exits 0 while a
// daemonized child still holds its stdio succeeds.
func Run(ctx context.Context, command string, env map[string]string, opts Options) error {
	if opts.Timeout <= 0 {
		return fmt.Errorf("setup command timeout must be positive, got %v", opts.Timeout)
	}
	// Read the base env before arming the deadline: execenv.Environ waits out
	// an in-flight process-env projection, and that wait is not the command's.
	baseEnv := execenv.Environ()
	idle, grace := time.Duration(0), waitDelay
	if opts.MaxTimeout > 0 {
		idle, grace = opts.Timeout, cancelGrace
	}
	mon := execgrace.NewMonitorWithClock(ctx, idle, opts.MaxTimeout, opts.Clock)
	defer mon.Stop()
	runCtx := mon.Context()
	if !mon.Enabled() {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
	}
	c := exec.CommandContext(runCtx, "sh", "-c", command)
	c.Dir = workDir(env, opts.FallbackDir)
	c.Env = baseEnv
	for k, v := range env {
		c.Env = append(c.Env, k+"="+v)
	}
	out := newOutputTail(outputLimit)
	w := mon.Writer(out)
	c.Stdout, c.Stderr = w, w
	execgrace.Apply(c, grace)
	if err := c.Run(); err != nil {
		// ErrWaitDelay means the command itself exited successfully and only
		// the force-closed pipe ended the wait.
		if errors.Is(err, exec.ErrWaitDelay) {
			return nil
		}
		if runCtx.Err() != nil {
			if cause := context.Cause(runCtx); cause != nil {
				err = fmt.Errorf("%w: %w", cause, err)
			}
		}
		if tail := out.render(runtime.SetupCommandSecrets(env)); tail != "" {
			return fmt.Errorf("%w: %s", err, tail)
		}
		return err
	}
	return nil
}

// workDir resolves the command's working directory: GC_DIR when it exists,
// else fallback. The empty string means the caller's working directory.
func workDir(env map[string]string, fallback string) string {
	dir := strings.TrimSpace(env["GC_DIR"])
	if dir == "" {
		return ""
	}
	if _, err := os.Stat(dir); err == nil {
		return dir
	}
	return fallback
}

// outputTail is a bounded io.Writer over a command's output. It retains
// [runtime.OutputTailRetention] bytes rather than limit so redaction runs on
// whole values before the reported window is cut out of them. Once it has
// discarded output, what it retains can begin inside a credential, so render
// strips that first ([runtime.TrimDecapitatedSecret]).
type outputTail struct {
	limit   int
	retain  int
	written int
	buf     []byte
}

func newOutputTail(limit int) *outputTail {
	return &outputTail{limit: limit, retain: runtime.OutputTailRetention(limit)}
}

func (b *outputTail) Write(p []byte) (int, error) {
	b.written += len(p)
	if len(p) >= b.retain {
		b.buf = append(b.buf[:0], p[len(p)-b.retain:]...)
		return len(p), nil
	}
	b.buf = append(b.buf, p...)
	if len(b.buf) > b.retain {
		copy(b.buf, b.buf[len(b.buf)-b.retain:])
		b.buf = b.buf[:b.retain]
	}
	return len(p), nil
}

// render returns the last limit bytes with secrets scrubbed, trimmed.
func (b *outputTail) render(secrets []string) string {
	text := string(b.buf)
	if b.written > len(b.buf) {
		text = runtime.TrimDecapitatedSecret(text, secrets)
	}
	text, _ = runtime.RedactSecretsTail(text, b.limit, secrets)
	return strings.TrimSpace(text)
}
