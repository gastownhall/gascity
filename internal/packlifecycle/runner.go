// Package packlifecycle runs pack-shipped lifecycle hook scripts.
//
// A pack owns services the city itself knows nothing about — a systemd
// unit, a container, an external daemon. Lifecycle hooks are how such a
// pack attaches those services to city lifecycle events, so `gc start`
// brings them up and `gc stop` takes them down without the SDK learning
// anything about the service.
//
// Hooks are convention-discovered by internal/config from a pack's
// lifecycle/ directory and executed here. Execution is best-effort: a
// failing or hanging hook is reported, never fatal, and never blocks the
// remaining hooks or the lifecycle operation that fired them.
package packlifecycle

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/citylayout"
)

// DefaultTimeout bounds how long a single hook may run before it is killed.
const DefaultTimeout = 30 * time.Second

// killGrace is how long a killed hook has to release its output pipes before
// Run stops waiting on them. Without it, a hook that spawns a longer-lived
// child would hold the pipes open past its own death and stall the caller.
const killGrace = 2 * time.Second

// Hook is one pack-shipped lifecycle script to run.
type Hook struct {
	// Event is the lifecycle event that fired the hook, e.g. "city-stop".
	Event string
	// Script is the path to the hook script.
	Script string
	// PackDir is the pack directory; it becomes the hook's working directory.
	PackDir string
	// PackName is the pack's name, used for display and pack runtime env.
	PackName string
}

// Name returns the hook's display name, "<pack>:<event>".
func (h Hook) Name() string { return h.PackName + ":" + h.Event }

// Result reports the outcome of one hook.
type Result struct {
	// Name is the hook's display name, "<pack>:<event>".
	Name string
	// Event is the lifecycle event that fired the hook.
	Event string
	// PackName is the pack that shipped the hook.
	PackName string
	// Output is the hook's combined stdout and stderr, trimmed.
	Output string
	// Err is non-nil when the hook could not be executed, exited non-zero,
	// or exceeded the timeout.
	Err error
}

// Run executes hooks in order and returns one Result per hook. Each hook is
// bounded by timeout (DefaultTimeout when timeout is not positive); a hook
// that exceeds it is killed and reported as a timeout. A hook failure never
// stops the remaining hooks — callers surface the results as warnings so a
// broken pack cannot wedge city startup or shutdown.
func Run(ctx context.Context, cityPath string, hooks []Hook, timeout time.Duration) []Result {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	var results []Result
	for _, hook := range hooks {
		results = append(results, runHook(ctx, cityPath, hook, timeout))
	}
	return results
}

func runHook(ctx context.Context, cityPath string, hook Hook, timeout time.Duration) Result {
	hookCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(hookCtx, hook.Script) //nolint:gosec // script path from pack config
	cmd.Dir = hook.PackDir
	cmd.WaitDelay = killGrace
	cmd.Env = append(cmd.Environ(), citylayout.PackRuntimeEnv(cityPath, hook.PackName)...)
	cmd.Env = append(cmd.Env,
		"GC_PACK_DIR="+hook.PackDir,
		"GC_LIFECYCLE_EVENT="+hook.Event,
	)

	out, err := cmd.CombinedOutput()
	result := Result{
		Name:     hook.Name(),
		Event:    hook.Event,
		PackName: hook.PackName,
		Output:   strings.TrimSpace(string(out)),
	}
	switch {
	case err == nil:
		return result
	case errors.Is(hookCtx.Err(), context.DeadlineExceeded):
		result.Err = fmt.Errorf("timed out after %s", timeout)
	default:
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			result.Err = fmt.Errorf("exited with status %d", exitErr.ExitCode())
		} else {
			result.Err = err
		}
	}
	return result
}
