// Package execgrace provides cooperative cancellation and optional
// activity-aware timeouts for [os/exec.Cmd].
//
// It generalizes two patterns that already exist piecemeal in the codebase:
//
//   - Graceful cancellation ([Apply]): the command runs in its own process
//     group and context cancellation interrupts that group first, so shell
//     rollback traps (and any foreground child blocking them) get a chance to
//     run before cancellation escalates to a forced kill. Without this, Go's
//     default [os/exec.CommandContext] cancel is Process.Kill — SIGKILL — which
//     is untrappable: a setup script killed mid-flight can never restore state
//     it staged aside (the worktree-setup data-loss class). This is the same
//     protection [internal/runtime/exec]'s interruptThenKill introduced for
//     adapter scripts, lifted to a reusable home.
//
//   - Activity-aware deadlines ([Monitor]): a fixed wall-clock timeout cannot
//     distinguish a hung command from a slow-but-healthy one. A Monitor cancels
//     its context only after the command has produced no output for a
//     configurable idle window, with an independent absolute ceiling as the
//     runaway backstop (a command that streams output forever must still die).
//     Both dimensions are opt-in: a zero idle and zero ceiling yield a Monitor
//     that passes the parent context and writers through untouched, so call
//     sites keep their existing fixed-deadline behavior unless they ask for
//     more.
package execgrace

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"
)

// ErrIdle is the cancellation cause when a [Monitor]'s idle window elapses
// with no output from the command. Retrieve it with [context.Cause].
var ErrIdle = errors.New("command produced no output within the idle timeout")

// ErrCeiling is the cancellation cause when a [Monitor]'s absolute ceiling
// elapses regardless of output. Retrieve it with [context.Cause].
var ErrCeiling = errors.New("command exceeded the maximum runtime ceiling")

// Apply configures cmd for cooperative cancellation and returns the flag that
// records whether a cancellation action was delivered.
//
// It places the command in its own process group (POSIX; no-op on Windows),
// replaces the default context-cancel behavior (SIGKILL) with
// [InterruptThenKill], and raises cmd.WaitDelay to grace when grace is larger
// than the current value. WaitDelay bounds how long Wait allows the
// interrupted process — its rollback traps included — and any grandchildren
// holding the I/O pipes before Go forcibly terminates them, so grace is
// effectively the trap budget. A zero grace leaves WaitDelay untouched.
//
// The returned flag serves callers that need cancellation to win over the
// command's own exit status (see [internal/runtime/exec]); callers that only
// need the graceful signal ordering may ignore it.
func Apply(cmd *exec.Cmd, grace time.Duration) *atomic.Bool {
	setProcessGroup(cmd)
	accepted := new(atomic.Bool)
	cmd.Cancel = InterruptThenKill(cmd, accepted)
	if grace > cmd.WaitDelay {
		cmd.WaitDelay = grace
	}
	return accepted
}

// InterruptThenKill builds an [os/exec.Cmd.Cancel] that first interrupts the
// command's process group so a cooperative command — and any foreground child
// blocking its rollback trap — can roll back before cancellation becomes a
// forced kill, recording in accepted whether cancellation was delivered so the
// caller can let it win over the command's own exit status. Platforms without
// process groups or os.Interrupt (such as Windows) fall back to Kill.
func InterruptThenKill(cmd *exec.Cmd, accepted *atomic.Bool) func() error {
	return func() error {
		err := interruptProcessGroup(cmd)
		if err == nil {
			accepted.Store(true)
			return nil
		}
		if errors.Is(err, os.ErrProcessDone) {
			return err
		}
		err = cmd.Process.Kill()
		if err == nil {
			accepted.Store(true)
		}
		return err
	}
}

// Monitor derives a context that is canceled when a command stops making
// observable progress (no output for idle) or exhausts an absolute runtime
// ceiling. Wrap the command's stdout/stderr with [Monitor.Writer] so output
// feeds the idle clock.
//
// A Monitor with neither dimension enabled is inert: Context returns the
// parent unchanged and Writer returns writers unchanged, preserving the call
// site's existing behavior with zero overhead.
type Monitor struct {
	ctx    context.Context
	cancel context.CancelCauseFunc

	idle time.Duration
	last atomic.Int64 // UnixNano of the most recent output (or start)

	mu        sync.Mutex
	idleTimer *time.Timer
	ceilTimer *time.Timer
	stopped   bool
}

// NewMonitor returns a Monitor over parent. idle > 0 enables the
// no-output-for-idle cancellation (cause [ErrIdle]); ceiling > 0 enables the
// absolute wall-clock cancellation (cause [ErrCeiling]). Either may be zero to
// disable that dimension; when both are zero the Monitor is inert and
// [Monitor.Context] returns parent itself.
//
// Callers must arrange for [Monitor.Stop] to run once the command finishes
// (typically via defer) to release the Monitor's timers.
func NewMonitor(parent context.Context, idle, ceiling time.Duration) *Monitor {
	m := &Monitor{idle: idle}
	if idle <= 0 && ceiling <= 0 {
		m.ctx = parent
		return m
	}
	m.ctx, m.cancel = context.WithCancelCause(parent)
	m.last.Store(time.Now().UnixNano())
	if idle > 0 {
		m.idleTimer = time.AfterFunc(idle, m.checkIdle)
	}
	if ceiling > 0 {
		m.ceilTimer = time.AfterFunc(ceiling, func() { m.cancel(ErrCeiling) })
	}
	return m
}

// Context returns the monitored context. Pass it to
// [os/exec.CommandContext]. For an inert Monitor this is the parent context
// itself.
func (m *Monitor) Context() context.Context { return m.ctx }

// Enabled reports whether the Monitor is actively enforcing at least one
// dimension. Call sites that need a fixed fallback deadline when monitoring
// is disabled can branch on this.
func (m *Monitor) Enabled() bool { return m.cancel != nil }

// Writer wraps inner so each write refreshes the idle clock. For an inert
// Monitor (or one without the idle dimension) it returns inner unchanged.
// The returned writer is safe for the concurrent use os/exec makes of
// separate stdout/stderr pipes: the clock is a single atomic store and all
// forwarding is delegated to inner.
func (m *Monitor) Writer(inner io.Writer) io.Writer {
	if m.cancel == nil || m.idle <= 0 {
		return inner
	}
	return &activityWriter{m: m, inner: inner}
}

// Stop releases the Monitor's timers. It does not cancel the derived context
// on its own; a context already canceled (by either dimension or the parent)
// stays canceled with its recorded cause. Safe to call multiple times.
func (m *Monitor) Stop() {
	if m.cancel == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stopped {
		return
	}
	m.stopped = true
	if m.idleTimer != nil {
		m.idleTimer.Stop()
	}
	if m.ceilTimer != nil {
		m.ceilTimer.Stop()
	}
	// Release the cause-context's resources; a prior cancellation (with its
	// cause) wins because CancelCauseFunc is first-cause-sticky.
	m.cancel(context.Canceled)
}

// checkIdle fires when the idle window may have elapsed. Output since the
// timer was armed re-arms it for the remainder; true silence cancels with
// [ErrIdle].
func (m *Monitor) checkIdle() {
	since := time.Since(time.Unix(0, m.last.Load()))
	if since >= m.idle {
		m.cancel(ErrIdle)
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stopped {
		return
	}
	m.idleTimer.Reset(m.idle - since)
}

type activityWriter struct {
	m     *Monitor
	inner io.Writer
}

func (w *activityWriter) Write(p []byte) (int, error) {
	w.m.last.Store(time.Now().UnixNano())
	return w.inner.Write(p)
}
