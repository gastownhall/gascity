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
	"github.com/gastownhall/gascity/internal/beads"
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
	// ID is the one bead being watched. Empty when Current is set: the id is
	// then resolved once at start, as bd resolves --current before watching.
	ID string
	// Current is `show --current --watch`.
	Current bool
	// Globals are the bd global flags from anywhere in argv (before or after
	// the verb), minus --json. They travel on every bd call the watch makes,
	// so the render, the change poll and the --current lookup all read the
	// same database.
	Globals []string
}

// displayArgs is one plain render of id. bd's watchIssue ignores the show
// display flags (--json, --short, --long, --refs, --children, --thread, ...)
// and always renders the plain form, so gc drops them too.
func (r bdShowWatchRequest) displayArgs(id string) []string {
	args := append([]string{}, r.Globals...)
	if strings.HasPrefix(id, "-") {
		return append(args, "show", "--id="+id)
	}
	return append(args, "show", id)
}

// snapshotArgs reads id as JSON for change detection.
func (r bdShowWatchRequest) snapshotArgs(id string) []string {
	args := append([]string{}, r.Globals...)
	return append(args, "show", "--json", "--id="+id)
}

// currentArgs asks bd which bead --current names.
func (r bdShowWatchRequest) currentArgs(jsonOut bool) []string {
	args := append([]string{}, r.Globals...)
	args = append(args, "show", "--current")
	if jsonOut {
		args = append(args, "--json")
	}
	return args
}

// bdShowWatchDisplayOnly are show flags bd's watch mode ignores.
var bdShowWatchDisplayOnly = map[string]bool{
	"--json": true, "--short": true, "--long": true, "--refs": true,
	"--children": true, "--thread": true, "--local-time": true,
	"--include-dependents": true, "--include-comments": true, "--brief-deps": true,
}

// bdShowWatchPassthroughFlags make bd answer something other than a watch
// (help, version), so the invocation stays bd's.
var bdShowWatchPassthroughFlags = map[string]bool{
	"-h": true, "--help": true, "-V": true, "--version": true, "--as-of": true,
}

// parseBdShowWatchArgs recognizes `show|view <id> --watch` (or -w), and
// `show --current --watch`. Anything bd's watch mode would not serve either
// (several ids, no id, --as-of, --current with an id) or that names a flag gc
// does not know stays on the passthrough, where bd answers or errors itself.
func parseBdShowWatchArgs(bdArgs []string) (bdShowWatchRequest, bool) {
	verb, verbArgs, ok := bdRelocatedClassVerb(bdArgs)
	if !ok || (verb != "show" && verb != "view") {
		return bdShowWatchRequest{}, false
	}
	prefix := bdArgs[:len(bdArgs)-len(verbArgs)-1]
	globalValues := bdflags.GlobalValueFlags()
	globalBools := bdflags.GlobalBoolFlags()

	var req bdShowWatchRequest
	// keepGlobal files one global flag token (and its value, if it takes
	// one) and reports how many extra tokens it consumed.
	keepGlobal := func(args []string, i int) (int, bool) {
		name, _, hasValue := strings.Cut(args[i], "=")
		if bdShowWatchPassthroughFlags[name] {
			return 0, false
		}
		if name == "--json" {
			return 0, true
		}
		if globalValues[name] {
			if hasValue {
				req.Globals = append(req.Globals, args[i])
				return 0, true
			}
			if i+1 >= len(args) {
				return 0, false
			}
			req.Globals = append(req.Globals, args[i], args[i+1])
			return 1, true
		}
		req.Globals = append(req.Globals, args[i])
		return 0, true
	}
	for i := 0; i < len(prefix); i++ {
		skip, ok := keepGlobal(prefix, i)
		if !ok {
			return bdShowWatchRequest{}, false
		}
		i += skip
	}

	watch := false
	var ids []string
	for i := 0; i < len(verbArgs); i++ {
		arg := verbArgs[i]
		if arg == "--" {
			ids = append(ids, verbArgs[i+1:]...)
			break
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			ids = append(ids, arg)
			continue
		}
		name, value, hasValue := strings.Cut(arg, "=")
		switch {
		case name == "--watch" || name == "-w":
			watch = true
			if hasValue {
				on, err := strconv.ParseBool(value)
				if err != nil {
					return bdShowWatchRequest{}, false
				}
				watch = on
			}
		case name == "--current":
			req.Current = true
			if hasValue {
				on, err := strconv.ParseBool(value)
				if err != nil {
					return bdShowWatchRequest{}, false
				}
				req.Current = on
			}
		case name == "--id":
			if !hasValue {
				if i+1 >= len(verbArgs) {
					return bdShowWatchRequest{}, false
				}
				i++
				value = verbArgs[i]
			}
			ids = append(ids, value)
		case bdShowWatchDisplayOnly[name]:
		case globalValues[name] || globalBools[name] || bdShowWatchPassthroughFlags[name]:
			skip, ok := keepGlobal(verbArgs, i)
			if !ok {
				return bdShowWatchRequest{}, false
			}
			i += skip
		default:
			// A flag this parser does not know: leave the call to bd.
			return bdShowWatchRequest{}, false
		}
	}
	if !watch {
		return bdShowWatchRequest{}, false
	}
	switch {
	case req.Current && len(ids) == 0:
	case !req.Current && len(ids) == 1:
		req.ID = ids[0]
	default:
		return bdShowWatchRequest{}, false
	}
	return req, true
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

// bdWatchRunner runs bd the way the doBd passthrough does: same binary,
// directory and environment, each call traced, and bd's stderr scanned for
// the same two markers — the silent fallback to on-disk auto-import (a
// watch reading the wrong store is reported and stopped with
// bdSilentFallbackExitCode) and the "bd dolt start" advice (answered once with
// the gc-managed remedy).
type bdWatchRunner struct {
	bdPath    string
	dir       string
	env       []string
	cityPath  string
	scopeRoot string
	// stderr receives gc's own diagnostics, whatever the call's sink is.
	stderr io.Writer
	hinted bool
}

func (r *bdWatchRunner) run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	cmd := exec.CommandContext(ctx, r.bdPath, args...)
	cmd.Dir = r.dir
	cmd.Env = r.env
	cmd.Stdout = stdout
	scan := &headLimitedWriter{limit: bdStderrScanLimit}
	cmd.Stderr = io.MultiWriter(stderr, scan)
	start := time.Now()
	err := cmd.Run()
	exit := 0
	if err != nil {
		exit = -1
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exit = exitErr.ExitCode()
		}
	}
	beads.TraceBDCall("go:gc-bd-show-watch", r.dir, args, start, exit, err)
	switch {
	case exit > 0:
		if !r.hinted && bdOutputSuggestsConflictingDoltStart(scan.String()) && bdScopeDoltIsGcManaged(r.cityPath, r.scopeRoot) {
			r.hinted = true
			fmt.Fprintln(r.stderr, bdDoltStartConflictUserMessage) //nolint:errcheck // best-effort stderr
		}
		return exit
	case err != nil:
		if ctx.Err() == nil {
			fmt.Fprintf(r.stderr, "gc bd: %v\n", err) //nolint:errcheck // best-effort stderr
		}
		return 1
	case bdOutputIndicatesSilentFallback(scan.String()):
		fmt.Fprintln(r.stderr, bdSilentFallbackUserMessage) //nolint:errcheck // best-effort stderr
		return bdSilentFallbackExitCode
	}
	return 0
}

// runBdShowWatch serves `show <id> --watch` for a scope where bd refuses it,
// the way bd serves it where it does not (beads v1.3.0 watchIssue): render
// once, then poll every interval and re-render when the bead's
// id:status:updated_at snapshot changes, until ctx is canceled (Ctrl+C).
func runBdShowWatch(ctx context.Context, req bdShowWatchRequest, run bdWatchRunFunc, interval time.Duration, stdout, stderr io.Writer) int {
	id := req.ID
	if req.Current {
		// bd resolves --current once, then watches that bead.
		resolved, ok := bdShowWatchCurrentID(ctx, req, run)
		if !ok {
			// Let bd say why, in its own words and exit code.
			if code := run(ctx, req.currentArgs(false), stdout, stderr); code != 0 {
				return code
			}
			fmt.Fprintln(stderr, "gc bd: could not resolve --current for watch mode") //nolint:errcheck // best-effort stderr
			return 1
		}
		id = resolved
	}

	// Snapshot before rendering: a change that lands in between is then seen
	// by the next poll and redrawn, never lost.
	last, code := bdShowWatchSnapshot(ctx, req, id, run)
	if code == bdSilentFallbackExitCode {
		return code
	}
	if code := run(ctx, req.displayArgs(id), stdout, stderr); code != 0 {
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
			snap, code := bdShowWatchSnapshot(ctx, req, id, run)
			if code == bdSilentFallbackExitCode {
				return code
			}
			if code != 0 || snap == last {
				continue
			}
			last = snap
			if code := run(ctx, req.displayArgs(id), stdout, stderr); code == bdSilentFallbackExitCode {
				return code
			}
			if ctx.Err() != nil {
				continue
			}
			fmt.Fprint(stderr, bdShowWatchHint) //nolint:errcheck // best-effort stderr
		}
	}
}

// bdShowWatchRow is the part of bd's show --json row the watch reads.
type bdShowWatchRow struct {
	ID        string `json:"id"`
	Status    string `json:"status"`
	UpdatedAt string `json:"updated_at"`
}

// bdShowWatchSnapshot reads the bead as JSON and reduces it to bd's own watch
// snapshot (id:status:updated_at). A failed read returns bd's non-zero code
// and is skipped by the caller, as bd skips it.
func bdShowWatchSnapshot(ctx context.Context, req bdShowWatchRequest, id string, run bdWatchRunFunc) (string, int) {
	var out bytes.Buffer
	if code := run(ctx, req.snapshotArgs(id), &out, io.Discard); code != 0 {
		return "", code
	}
	var rows []bdShowWatchRow
	if err := json.Unmarshal(out.Bytes(), &rows); err == nil && len(rows) > 0 {
		return rows[0].ID + ":" + rows[0].Status + ":" + rows[0].UpdatedAt, 0
	}
	raw := bytes.TrimSpace(out.Bytes())
	if len(raw) == 0 {
		return "", 1
	}
	return string(raw), 0
}

// bdShowWatchCurrentID asks bd which bead --current names.
func bdShowWatchCurrentID(ctx context.Context, req bdShowWatchRequest, run bdWatchRunFunc) (string, bool) {
	var out bytes.Buffer
	if code := run(ctx, req.currentArgs(true), &out, io.Discard); code != 0 {
		return "", false
	}
	var rows []bdShowWatchRow
	if err := json.Unmarshal(out.Bytes(), &rows); err != nil || len(rows) == 0 || strings.TrimSpace(rows[0].ID) == "" {
		return "", false
	}
	return rows[0].ID, true
}

// serveBdShowWatch is doBd's hook: it runs the gc-side watch until Ctrl+C or
// SIGTERM.
func serveBdShowWatch(req bdShowWatchRequest, runner *bdWatchRunner, stdout, stderr io.Writer) int {
	ctx, stop := bdShowWatchContext()
	defer stop()
	return runBdShowWatch(ctx, req, runner.run, bdShowWatchInterval, stdout, stderr)
}
