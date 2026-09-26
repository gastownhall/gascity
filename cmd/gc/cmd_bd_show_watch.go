package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gastownhall/gascity/internal/bdflags"
)

// bdShowWatchInterval is the poll interval bd's own `show --watch` uses
// (beads v1.3.0 cmd/bd/show_display.go watchIssue: pollInterval = 2s). A var
// so tests can poll faster.
var bdShowWatchInterval = 2 * time.Second

// bdShowWatchContext is canceled by Ctrl+C or SIGTERM. A var so tests can
// stop a watch without signaling the test process.
var bdShowWatchContext = func() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// bdShowWatchHint and bdShowWatchStopped are bd's own watch-mode status lines,
// written to stderr exactly as bd writes them, so a gc-served watch reads the
// same as a bd-served one.
const (
	bdShowWatchHint    = "\nWatching for changes... (Press Ctrl+C to exit)\n"
	bdShowWatchStopped = "\nStopped watching.\n"
)

// bdShowWatchRequest is a `show --watch` invocation gc serves itself.
type bdShowWatchRequest struct {
	// ID is the one bead being watched.
	ID string
	// DisplayArgs is the original argv with the watch flag removed: one
	// plain `bd show` render per redraw, so bd keeps owning the format.
	DisplayArgs []string
	// SnapshotArgs asks bd for the same bead as JSON, for change detection.
	SnapshotArgs []string
}

// parseBdShowWatchArgs recognizes `show <id> --watch` (or -w). Anything that
// is not exactly one id, or that bd's watch mode would not serve either
// (--as-of, --current), is not recognized and stays on the passthrough, where
// bd produces its own answer or error.
func parseBdShowWatchArgs(bdArgs []string) (bdShowWatchRequest, bool) {
	verb, verbArgs, ok := bdRelocatedClassVerb(bdArgs)
	if !ok || verb != "show" {
		return bdShowWatchRequest{}, false
	}
	prefix := bdArgs[:len(bdArgs)-len(verbArgs)-1]
	globals := bdflags.GlobalValueFlags()

	watch := false
	var ids []string
	display := append([]string{}, prefix...)
	display = append(display, verb)
	for i := 0; i < len(verbArgs); i++ {
		arg := verbArgs[i]
		if arg == "--" {
			ids = append(ids, verbArgs[i+1:]...)
			display = append(display, verbArgs[i:]...)
			break
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			ids = append(ids, arg)
			display = append(display, arg)
			continue
		}
		name, value, hasValue := strings.Cut(arg, "=")
		switch name {
		case "--watch", "-w":
			if hasValue {
				on, err := strconv.ParseBool(value)
				if err != nil {
					return bdShowWatchRequest{}, false
				}
				watch = on
			} else {
				watch = true
			}
			continue
		case "--as-of", "--current":
			return bdShowWatchRequest{}, false
		case "--id":
			if !hasValue {
				if i+1 >= len(verbArgs) {
					return bdShowWatchRequest{}, false
				}
				i++
				value = verbArgs[i]
				display = append(display, arg, value)
			} else {
				display = append(display, arg)
			}
			ids = append(ids, value)
			continue
		}
		display = append(display, arg)
		if !hasValue && globals[name] && i+1 < len(verbArgs) {
			i++
			display = append(display, verbArgs[i])
		}
	}
	if !watch || len(ids) != 1 {
		return bdShowWatchRequest{}, false
	}
	snapshot := append([]string{}, prefix...)
	snapshot = append(snapshot, "show", "--json", "--id="+ids[0])
	return bdShowWatchRequest{ID: ids[0], DisplayArgs: display, SnapshotArgs: snapshot}, true
}

// bdScopeRefusesShowWatch reports whether bd would refuse `show --watch` for
// this scope. beads v1.3.0 refuses it in proxied-server mode
// (cmd/bd/proxy_capability.go proxyCommandCapabilities["show"]:
// proxy.watch.unsupported), which is the default transport for a new city.
var bdScopeRefusesShowWatch = func(cityPath string, target execStoreTarget, env []string) bool {
	for _, kv := range env {
		if value, ok := strings.CutPrefix(kv, "BEADS_DOLT_PROXIED_SERVER="); ok {
			if on, err := strconv.ParseBool(strings.TrimSpace(value)); err == nil && on {
				return true
			}
		}
	}
	return scopeUsesProxiedDoltMode(cityPath, target.ScopeRoot)
}

// bdWatchRunFunc runs one bd invocation and returns its exit code.
type bdWatchRunFunc func(ctx context.Context, args []string, stdout, stderr io.Writer) int

// newBdWatchRunner runs bd the same way the passthrough does: same binary,
// directory and environment.
func newBdWatchRunner(bdPath, dir string, env []string) bdWatchRunFunc {
	return func(ctx context.Context, args []string, stdout, stderr io.Writer) int {
		cmd := exec.CommandContext(ctx, bdPath, args...)
		cmd.Dir = dir
		cmd.Env = env
		cmd.Stdout = stdout
		cmd.Stderr = stderr
		err := cmd.Run()
		if err == nil {
			return 0
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() > 0 {
			return exitErr.ExitCode()
		}
		if ctx.Err() == nil {
			fmt.Fprintf(stderr, "gc bd: %v\n", err) //nolint:errcheck // best-effort stderr
		}
		return 1
	}
}

// runBdShowWatch serves `show <id> --watch` for a scope where bd refuses it,
// the way bd serves it where it does not (beads v1.3.0 watchIssue): render
// once, then poll every interval and re-render when the bead's
// id:status:updated_at snapshot changes, until ctx is canceled (Ctrl+C).
func runBdShowWatch(ctx context.Context, req bdShowWatchRequest, run bdWatchRunFunc, interval time.Duration, stdout, stderr io.Writer) int {
	// Snapshot before rendering: a change that lands in between is then seen
	// by the next poll and redrawn, never lost.
	last, _ := bdShowWatchSnapshot(ctx, req, run)
	if code := run(ctx, req.DisplayArgs, stdout, stderr); code != 0 {
		if ctx.Err() != nil {
			fmt.Fprint(stderr, bdShowWatchStopped) //nolint:errcheck // best-effort stderr
			return 0
		}
		return code
	}
	fmt.Fprint(stderr, bdShowWatchHint) //nolint:errcheck // best-effort stderr

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			fmt.Fprint(stderr, bdShowWatchStopped) //nolint:errcheck // best-effort stderr
			return 0
		case <-ticker.C:
			snap, ok := bdShowWatchSnapshot(ctx, req, run)
			if !ok || snap == last {
				continue
			}
			last = snap
			run(ctx, req.DisplayArgs, stdout, stderr)
			if ctx.Err() != nil {
				continue
			}
			fmt.Fprint(stderr, bdShowWatchHint) //nolint:errcheck // best-effort stderr
		}
	}
}

// bdShowWatchSnapshot reads the bead as JSON and reduces it to bd's own watch
// snapshot (id:status:updated_at). A failed read reports !ok and is skipped,
// as bd skips it.
func bdShowWatchSnapshot(ctx context.Context, req bdShowWatchRequest, run bdWatchRunFunc) (string, bool) {
	var out bytes.Buffer
	if code := run(ctx, req.SnapshotArgs, &out, io.Discard); code != 0 {
		return "", false
	}
	var rows []struct {
		ID        string `json:"id"`
		Status    string `json:"status"`
		UpdatedAt string `json:"updated_at"`
	}
	if err := json.Unmarshal(out.Bytes(), &rows); err == nil && len(rows) > 0 {
		return rows[0].ID + ":" + rows[0].Status + ":" + rows[0].UpdatedAt, true
	}
	raw := bytes.TrimSpace(out.Bytes())
	if len(raw) == 0 {
		return "", false
	}
	return string(raw), true
}

// serveBdShowWatch is doBd's hook: it runs the gc-side watch until Ctrl+C or
// SIGTERM.
func serveBdShowWatch(req bdShowWatchRequest, bdPath, dir string, env []string, stdout, stderr io.Writer) int {
	ctx, stop := bdShowWatchContext()
	defer stop()
	return runBdShowWatch(ctx, req, newBdWatchRunner(bdPath, dir, env), bdShowWatchInterval, stdout, stderr)
}
