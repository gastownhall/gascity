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
// Extracted from the tmux adapter as the shared core that host-side providers
// (tmux, herdr) will delegate to, so lifecycle commands run with one set of
// semantics: same GC_DIR cwd contract, same daemonizing-child tolerance, same
// failure detail. As of this commit it has no callers — tmux
// (internal/runtime/tmux/adapter.go) and herdr (internal/runtime/herdr/provider.go)
// still run their own copies.
//
// PARITY REQUIRED BEFORE WIRING: both current callers have since grown an
// execgrace layer this snapshot predates. Before either delegates here, this
// runner must regain: execgrace.NewMonitor idle/ceiling budgets under
// [session] setup_max_timeout (this version has a single fixed deadline),
// execgrace.Apply cooperative process-group interrupt so shell rollback traps
// run before SIGKILL (see adapter.go's note on stranded staged state), and
// context.Cause in the failure wrap so the reported error names which budget
// fired. Provider-specific behavior is also not covered here: tmux's
// GC_TMUX_SOCKET injection and herdr's GC_DIR-exists-else-cityRoot fallback.
func RunSetupCommand(ctx context.Context, command string, env map[string]string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	c := exec.CommandContext(ctx, "sh", "-c", command)
	if workDir := strings.TrimSpace(env["GC_DIR"]); workDir != "" {
		c.Dir = workDir
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
	if err := c.Run(); err != nil {
		// ErrWaitDelay means the command itself exited successfully and
		// only the force-closed pipes ended the wait: a setup command that
		// daemonizes a child holding inherited stdio and exits 0 succeeded.
		if errors.Is(err, exec.ErrWaitDelay) {
			return nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			err = fmt.Errorf("%w: %w", ctxErr, err)
		}
		return setupCommandFailure(err, stdout, stderr)
	}
	return nil
}

// commandOutputTail is a bounded io.Writer that keeps only the last limit
// bytes written, for folding command output into failure messages.
type commandOutputTail struct {
	limit   int
	written int
	buf     []byte
}

func newCommandOutputTail(limit int) *commandOutputTail {
	return &commandOutputTail{limit: limit}
}

func (b *commandOutputTail) Write(p []byte) (int, error) {
	b.written += len(p)
	if b.limit <= 0 {
		return len(p), nil
	}
	if len(p) >= b.limit {
		b.buf = append(b.buf[:0], p[len(p)-b.limit:]...)
		return len(p), nil
	}
	b.buf = append(b.buf, p...)
	if len(b.buf) > b.limit {
		copy(b.buf, b.buf[len(b.buf)-b.limit:])
		b.buf = b.buf[:b.limit]
	}
	return len(p), nil
}

func (b *commandOutputTail) Detail(label string) string {
	text := strings.TrimSpace(string(b.buf))
	if text == "" {
		return ""
	}
	if b.written > len(b.buf) {
		text = "... " + text
	}
	return label + ": " + text
}

func setupCommandFailure(err error, stdout, stderr *commandOutputTail) error {
	stderrDetail := stderr.Detail("stderr")
	stdoutDetail := stdout.Detail("stdout")
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
