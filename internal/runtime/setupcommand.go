package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

const (
	// setupCommandOutputLimit bounds how much stdout/stderr tail is retained
	// per stream and folded into a setup-command failure message.
	setupCommandOutputLimit = 4096
	// setupCommandWaitDelay is how long after the command exits (or the
	// timeout fires) Go forcibly closes the capture pipes, so background
	// descendants that inherited stdio cannot block the wait indefinitely.
	setupCommandWaitDelay = 2 * time.Second
)

// RunSetupCommand executes one session lifecycle shell command (pre_start,
// session_setup, session_setup_script, session_live) host-side — "in gc's
// process via sh -c", per the Config field contracts — with a per-command
// timeout. The command's working directory is env["GC_DIR"] when set; env is
// appended to the inherited process environment (last wins). On failure, a
// bounded tail of the command's stdout/stderr is folded into the returned
// error so operators can see why a setup command failed without hunting for
// logs.
//
// Extracted from the tmux adapter as the shared core that local providers can
// delegate to, so lifecycle commands keep one GC_DIR cwd contract, bounded
// output detail, daemonizing-child tolerance, and cooperative cancellation.
// Provider-specific runners (tmux and herdr) retain their optional
// activity-aware setup_max_timeout and transport-specific environment. This
// shared path uses its explicit per-command timeout as the ceiling.
func RunSetupCommand(ctx context.Context, command string, env map[string]string, timeout time.Duration) error {
	return runSetupCommand(ctx, command, env, timeout, false)
}

// RunPreStart executes one pre_start command with the same bounded output and
// cancellation semantics as the other lifecycle commands. A pre_start is
// allowed to create cfg.WorkDir, so a missing GC_DIR is preserved in the
// environment but is not used as the child cwd until it exists.
func RunPreStart(ctx context.Context, command string, env map[string]string, timeout time.Duration) error {
	return runSetupCommand(ctx, command, env, timeout, true)
}

func runSetupCommand(ctx context.Context, command string, env map[string]string, timeout time.Duration, allowMissingWorkDir bool) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	c := exec.CommandContext(ctx, "sh", "-c", command)
	if workDir := strings.TrimSpace(env["GC_DIR"]); workDir != "" {
		if _, err := os.Stat(workDir); err == nil || !allowMissingWorkDir || !os.IsNotExist(err) {
			c.Dir = workDir
		}
	}
	c.Env = os.Environ()
	for k, v := range env {
		c.Env = append(c.Env, k+"="+v)
	}
	stdout := newCommandOutputTail(setupCommandOutputLimit)
	stderr := newCommandOutputTail(setupCommandOutputLimit)
	c.Stdout = stdout
	c.Stderr = stderr
	// WaitDelay ensures Go forcibly closes the capture pipes after the
	// command exits or the timeout fires, even if background descendants
	// spawned by the command still hold them open.
	c.WaitDelay = setupCommandWaitDelay
	// A context cancellation must interrupt the command group before the
	// forced kill so setup scripts can run rollback traps and descendants cannot
	// continue mutating a workdir after Start has returned.
	finishSetupCommandCancellation := applySetupCommandCancellation(c, setupCommandWaitDelay)
	runErr := c.Run()
	// If cancellation made the command return early, synchronously terminate
	// the whole process group before returning to the provider. A shell may run
	// its rollback trap and exit while a redirected descendant ignores SIGINT;
	// merely stopping the escalation timer here would leave that descendant
	// mutating the workdir after Start reports failure.
	if ctx.Err() != nil {
		finishSetupCommandCancellation(true)
	} else {
		finishSetupCommandCancellation(false)
	}
	if runErr != nil {
		// ErrWaitDelay means the command itself exited successfully and
		// only the force-closed pipes ended the wait: a setup command that
		// daemonizes a child holding inherited stdio and exits 0 succeeded.
		if errors.Is(runErr, exec.ErrWaitDelay) && ctx.Err() == nil {
			return nil
		}
		err := runErr
		if ctxErr := context.Cause(ctx); ctxErr != nil && ctx.Err() != nil {
			err = fmt.Errorf("%w: %w", ctxErr, err)
		}
		return setupCommandFailure(err, stdout, stderr, SetupCommandSecrets(env))
	}
	return nil
}

// commandOutputTail is a bounded io.Writer that reports only the last limit
// bytes written, for folding command output into failure messages. It retains
// [OutputTailRetention] bytes rather than limit so redaction runs on a whole
// value before the reported window is cut out of it.
type commandOutputTail struct {
	limit   int
	retain  int
	written int
	buf     []byte
}

func newCommandOutputTail(limit int) *commandOutputTail {
	return &commandOutputTail{limit: limit, retain: OutputTailRetention(limit)}
}

func (b *commandOutputTail) Write(p []byte) (int, error) {
	b.written += len(p)
	if b.retain <= 0 {
		return len(p), nil
	}
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

// Detail renders the tail with secrets scrubbed. Redaction is this type's job
// rather than the caller's because only it knows the retained buffer is wider
// than the window it reports, and [RedactSecretsTail] has to see the wider one.
func (b *commandOutputTail) Detail(label string, secrets []string) string {
	text, trimmed := RedactSecretsTail(string(b.buf), b.limit, secrets)
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	if trimmed || b.written > len(b.buf) {
		text = "... " + text
	}
	return label + ": " + text
}

// setupCommandFailure folds a bounded tail of both streams into the failure.
// The tails are scrubbed because this error is durable — it reaches logs, the
// event bus and bead notes — and a setup command echoing a credential it was
// handed (a `set -x` trace, a failing curl printing its header) would otherwise
// park that credential there permanently.
func setupCommandFailure(err error, stdout, stderr *commandOutputTail, secrets []string) error {
	stderrDetail := stderr.Detail("stderr", secrets)
	stdoutDetail := stdout.Detail("stdout", secrets)
	switch {
	case stderrDetail != "" && stdoutDetail != "":
		return fmt.Errorf("%w; %s; %s", err, stderrDetail, stdoutDetail)
	case stderrDetail != "":
		return fmt.Errorf("%w; %s", err, stderrDetail)
	case stdoutDetail != "":
		return fmt.Errorf("%w; %s", err, stdoutDetail)
	default:
		return err
	}
}
