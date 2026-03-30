package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/convergence"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/supervisor"
	"github.com/gastownhall/gascity/internal/telemetry"
	"github.com/gastownhall/gascity/internal/workspacesvc"
)

var errControllerAlreadyRunning = errors.New("controller already running")

// acquireControllerLock takes an exclusive flock on .gc/controller.lock.
// Returns the locked file (caller must defer Close) or an error if another
// controller is already running.
func acquireControllerLock(cityPath string) (*os.File, error) {
	path := filepath.Join(cityPath, ".gc", "controller.lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("opening controller lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close() //nolint:errcheck // closing after flock failure
		return nil, errControllerAlreadyRunning
	}
	return f, nil
}

// startControllerSocket listens on a Unix socket at .gc/controller.sock.
// When a client sends "stop\n", cancelFn is called to shut down the
// controller loop. convergenceReqCh is used to route convergence commands
// to the event loop for serialized processing. Returns the listener for cleanup.
func startControllerSocket(
	cityPath string,
	cancelFn context.CancelFunc,
	convergenceReqCh chan convergenceRequest,
	pokeCh chan struct{},
	workflowControlCh chan struct{},
) (net.Listener, error) {
	sockPath := filepath.Join(cityPath, ".gc", "controller.sock")
	// Remove stale socket from a previous crash.
	os.Remove(sockPath) //nolint:errcheck // stale socket cleanup
	lis, err := net.Listen("unix", sockPath)
	if err != nil {
		return nil, fmt.Errorf("listening on controller socket: %w", err)
	}
	go func() {
		for {
			conn, err := lis.Accept()
			if err != nil {
				return // listener closed
			}
			go handleControllerConn(conn, cancelFn, convergenceReqCh, pokeCh, workflowControlCh)
		}
	}()
	return lis, nil
}

// handleControllerConn reads from a connection and dispatches commands.
// Supported commands: "stop" (shutdown), "ping" (liveness check, returns PID),
// "converge:{json}" (convergence commands routed to event loop).
func handleControllerConn(
	conn net.Conn,
	cancelFn context.CancelFunc,
	convergenceReqCh chan convergenceRequest,
	pokeCh chan struct{},
	workflowControlCh chan struct{},
) {
	defer conn.Close()                                 //nolint:errcheck // best-effort cleanup
	conn.SetDeadline(time.Now().Add(95 * time.Second)) //nolint:errcheck // symmetric read+write deadline; 5s margin over 30s enqueue + 60s reply
	scanner := bufio.NewScanner(conn)
	// Increase scanner buffer for convergence commands which may carry large payloads.
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	if scanner.Scan() {
		line := scanner.Text()
		switch {
		case line == "stop":
			cancelFn()
			conn.Write([]byte("ok\n")) //nolint:errcheck // best-effort ack
		case line == "ping":
			fmt.Fprintf(conn, "%d\n", os.Getpid()) //nolint:errcheck // best-effort
		case line == "poke":
			// Non-blocking send: triggers immediate reconciler tick for
			// event-driven wake after sling assigns work.
			select {
			case pokeCh <- struct{}{}:
			default: // poke already pending
			}
			conn.Write([]byte("ok\n")) //nolint:errcheck // best-effort ack
		case line == "workflow-control":
			select {
			case workflowControlCh <- struct{}{}:
			default:
			}
			conn.Write([]byte("ok\n")) //nolint:errcheck // best-effort ack
		case strings.HasPrefix(line, "converge:"):
			handleConvergeSocketCmd(conn, line[len("converge:"):], convergenceReqCh)
		}
	}
}

// handleConvergeSocketCmd parses a convergence JSON request, enqueues it
// to the event loop, and writes the reply back to the connection.
func handleConvergeSocketCmd(conn net.Conn, payload string, ch chan convergenceRequest) {
	var req convergenceRequest
	if err := json.Unmarshal([]byte(payload), &req); err != nil {
		writeJSONLine(conn, convergenceReply{Error: fmt.Sprintf("invalid request: %v", err)})
		return
	}
	req.replyCh = make(chan convergenceReply, 1)
	// Send to event loop with a timeout to prevent hanging if the loop is stuck.
	select {
	case ch <- req:
	case <-time.After(30 * time.Second):
		writeJSONLine(conn, convergenceReply{Error: "controller busy (request timeout)"})
		return
	}
	// Wait for reply.
	select {
	case reply := <-req.replyCh:
		writeJSONLine(conn, reply)
	case <-time.After(60 * time.Second):
		writeJSONLine(conn, convergenceReply{Error: "controller did not respond in time"})
	}
}

// writeJSONLine marshals v as JSON and writes it as a single line to w.
func writeJSONLine(w net.Conn, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	data = append(data, '\n')
	w.Write(data) //nolint:errcheck // best-effort
}

// sendControllerCommand sends a command string to controller.sock and
// returns the raw response bytes. Used by CLI commands that need to
// route through the controller.
func sendControllerCommand(cityPath, command string) ([]byte, error) {
	sockPath := filepath.Join(cityPath, ".gc", "controller.sock")
	conn, err := net.DialTimeout("unix", sockPath, 2*time.Second)
	if err != nil {
		return nil, fmt.Errorf("connecting to controller: %w (is the controller running?)", err)
	}
	defer conn.Close()                                     //nolint:errcheck
	conn.SetWriteDeadline(time.Now().Add(5 * time.Second)) //nolint:errcheck
	conn.SetReadDeadline(time.Now().Add(95 * time.Second)) //nolint:errcheck // Must exceed server-side 30s enqueue + 60s reply
	if _, err := conn.Write([]byte(command + "\n")); err != nil {
		return nil, fmt.Errorf("sending command: %w", err)
	}
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return nil, fmt.Errorf("reading response: %w", err)
		}
		return nil, fmt.Errorf("reading response: connection closed")
	}
	return scanner.Bytes(), nil
}

// controllerAlive checks whether a controller is running by connecting
// to the controller.sock and sending a "ping". Returns the PID if alive,
// or 0 if not reachable.
func controllerAlive(cityPath string) int {
	sockPath := filepath.Join(cityPath, ".gc", "controller.sock")
	conn, err := net.DialTimeout("unix", sockPath, 500*time.Millisecond)
	if err != nil {
		return 0
	}
	defer conn.Close()                                    //nolint:errcheck // best-effort
	conn.Write([]byte("ping\n"))                          //nolint:errcheck // best-effort
	conn.SetReadDeadline(time.Now().Add(2 * time.Second)) //nolint:errcheck // best-effort
	buf := make([]byte, 64)
	n, err := conn.Read(buf)
	if err != nil || n == 0 {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(buf[:n])))
	if err != nil {
		return 0
	}
	return pid
}

// debounceDelay is the coalesce window for filesystem events. Multiple
// events within this window (vim atomic saves, git checkouts) produce a
// single dirty signal. Tests may override this for faster response.
var debounceDelay = 200 * time.Millisecond

// watchConfigDirs starts an fsnotify watcher on the given directories and
// sets dirty to true after a debounce window. Watches directories instead
// of individual files to handle vim/emacs rename-swap atomic saves.
// Returns a cleanup function. If the watcher cannot be created, returns a
// no-op cleanup (degraded to tick-only, no file watching).
func watchConfigDirs(dirs []string, dirty *atomic.Bool, stderr io.Writer) func() {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		fmt.Fprintf(stderr, "gc start: config watcher: %v (reload on tick only)\n", err) //nolint:errcheck // best-effort stderr
		return func() {}
	}
	for _, dir := range dirs {
		if err := watcher.Add(dir); err != nil {
			fmt.Fprintf(stderr, "gc start: config watcher: cannot watch %s: %v\n", dir, err) //nolint:errcheck // best-effort stderr
		}
	}
	go func() {
		var debounce *time.Timer
		for {
			select {
			case _, ok := <-watcher.Events:
				if !ok {
					return
				}
				// Debounce: reset timer on each event, fire after quiet period.
				if debounce != nil {
					debounce.Stop()
				}
				debounce = time.AfterFunc(debounceDelay, func() {
					dirty.Store(true)
				})
			case _, ok := <-watcher.Errors:
				if !ok {
					return
				}
			}
		}
	}()
	return func() { watcher.Close() } //nolint:errcheck // best-effort cleanup
}

// reloadResult holds the result of a config reload attempt.
type reloadResult struct {
	Cfg      *config.City
	Prov     *config.Provenance
	Revision string
}

// tryReloadConfig attempts to reload city.toml with includes and patches.
// Returns the new config, provenance, and revision on success, or an error
// on failure (parse error, validation error, cityName changed). Callers
// should keep the old config on error. Warnings are written to stderr;
// strict mode (default) makes them fatal — use --no-strict to disable.
func tryReloadConfig(tomlPath, lockedCityName, cityRoot string, stderr io.Writer) (*reloadResult, error) {
	// Auto-fetch remote packs before full config load (mirrors cmd_start).
	if quickCfg, qErr := config.Load(fsys.OSFS{}, tomlPath); qErr == nil && len(quickCfg.Packs) > 0 {
		if fErr := config.FetchPacks(quickCfg.Packs, cityRoot); fErr != nil {
			return nil, fmt.Errorf("fetching packs: %w", fErr)
		}
	}

	newCfg, prov, err := config.LoadWithIncludes(fsys.OSFS{}, tomlPath, extraConfigFiles...)
	if err != nil {
		return nil, fmt.Errorf("parsing city.toml: %w", err)
	}
	applyFeatureFlags(newCfg)
	if strictMode && len(prov.Warnings) > 0 {
		for _, w := range prov.Warnings {
			fmt.Fprintf(stderr, "gc start: strict: %s\n", w) //nolint:errcheck // best-effort stderr
		}
		fmt.Fprintln(stderr, "gc start: use --no-strict to disable strict checking") //nolint:errcheck // best-effort stderr
		return nil, fmt.Errorf("strict mode: %d collision warning(s)", len(prov.Warnings))
	}
	for _, w := range prov.Warnings {
		fmt.Fprintf(stderr, "gc start: warning: %s\n", w) //nolint:errcheck // best-effort stderr
	}
	if err := config.ValidateAgents(newCfg.Agents); err != nil {
		return nil, fmt.Errorf("validating agents: %w", err)
	}
	if err := config.ValidateServices(newCfg.Services); err != nil {
		return nil, fmt.Errorf("validating services: %w", err)
	}
	if err := workspacesvc.ValidateRuntimeSupport(newCfg.Services); err != nil {
		return nil, fmt.Errorf("validating services: %w", err)
	}
	newName := newCfg.Workspace.Name
	if newName == "" {
		newName = filepath.Base(filepath.Dir(tomlPath))
	}
	if newName != lockedCityName {
		return nil, fmt.Errorf("workspace.name changed from %q to %q (restart controller to apply)", lockedCityName, newName)
	}
	rev := config.Revision(fsys.OSFS{}, prov, newCfg, cityRoot)
	return &reloadResult{Cfg: newCfg, Prov: prov, Revision: rev}, nil
}

// gracefulStopAll performs two-pass graceful shutdown:
//  1. Send Interrupt (Ctrl-C) to all sessions
//  2. Wait shutdown_timeout
//  3. Stop (force-kill) any survivors
func gracefulStopAll(
	names []string,
	sp runtime.Provider,
	timeout time.Duration,
	rec events.Recorder,
	cfg *config.City,
	store beads.Store,
	stdout, stderr io.Writer,
) {
	if timeout <= 0 || len(names) == 0 {
		// Immediate kill (no grace period).
		stopTargetsBounded(stopTargetsForNames(names, cfg, store, stderr), cfg, sp, rec, "gc", stdout, stderr)
		return
	}
	targets := stopTargetsForNames(names, cfg, store, stderr)
	targetByName := make(map[string]stopTarget, len(targets))
	for _, target := range targets {
		targetByName[target.name] = target
	}

	// Pass 1: interrupt all in a single bounded broadcast wave.
	// This is intentionally flat: interrupts are a best-effort graceful hint,
	// while pass 2 keeps reverse dependency ordering for any survivors.
	// The configured timeout is the post-dispatch grace window; dispatch
	// latency is intentionally outside that budget so every interrupted
	// session still gets the full graceful-exit wait once nudged.
	sent := interruptTargetsBounded(targets, sp, stderr)
	fmt.Fprintf(stdout, "Sent interrupt to %d/%d agent(s), waiting %s...\n", //nolint:errcheck // best-effort stdout
		sent, len(names), timeout)

	// Poll until all agents exit or timeout expires (avoid sleeping full duration).
	pollInterval := 500 * time.Millisecond
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		allExited := true
		for _, name := range names {
			if sp.IsRunning(name) {
				allExited = false
				break
			}
		}
		if allExited {
			break
		}
		remaining := time.Until(deadline)
		if remaining < pollInterval {
			time.Sleep(remaining)
		} else {
			time.Sleep(pollInterval)
		}
	}

	// Pass 2: kill survivors.
	var survivors []string
	for _, name := range names {
		if !sp.IsRunning(name) {
			fmt.Fprintf(stdout, "Agent '%s' exited gracefully\n", name) //nolint:errcheck // best-effort stdout
			subject := name
			if target, ok := targetByName[name]; ok && target.subject != "" {
				subject = target.subject
			}
			rec.Record(events.Event{
				Type: events.SessionStopped, Actor: "gc", Subject: subject,
			})
			continue
		}
		survivors = append(survivors, name)
	}
	stopTargetsBounded(filterStopTargets(targets, survivors), cfg, sp, rec, "gc", stdout, stderr)
}

// controllerLoop is a compatibility shim that wraps CityRuntime.run().
// Tests and runController use this entry point to preserve the existing
// call signature during the Phase 0 extraction. It will be removed once
// callers migrate to CityRuntime directly.
//
//nolint:unparam // compatibility shim — many params are nil in tests but varied in runController
func controllerLoop(
	ctx context.Context,
	interval time.Duration, // overrides cfg patrol interval when non-zero (used by tests)
	cfg *config.City,
	cityName string,
	tomlPath string,
	watchDirs []string,
	buildFn func(*config.City, runtime.Provider, beads.Store) DesiredStateResult,
	sp runtime.Provider,
	dops drainOps,
	ct crashTracker,
	it idleTracker,
	wg wispGC,
	od orderDispatcher,
	rec events.Recorder,
	poolSessions map[string]time.Duration,
	poolDeathHandlers map[string]poolDeathInfo,
	suspendedNames map[string]bool,
	cs *controllerState, // nil when API disabled
	stdout, stderr io.Writer,
) {
	var cityPath string
	if tomlPath != "" {
		cityPath = filepath.Dir(tomlPath)
	}
	// Allow callers (tests) to override the patrol interval without
	// mutating the caller's config.
	loopCfg := cfg
	if interval > 0 {
		cfgCopy := *cfg
		cfgCopy.Daemon.PatrolInterval = interval.String()
		loopCfg = &cfgCopy
	}
	cr := &CityRuntime{
		cityPath:          cityPath,
		cityName:          cityName,
		tomlPath:          tomlPath,
		watchDirs:         watchDirs,
		cfg:               loopCfg,
		sp:                sp,
		buildFn:           buildFn,
		dops:              dops,
		ct:                ct,
		it:                it,
		wg:                wg,
		od:                od,
		rec:               rec,
		cs:                cs,
		poolSessions:      poolSessions,
		poolDeathHandlers: poolDeathHandlers,
		suspendedNames:    suspendedNames,
		pokeCh:            make(chan struct{}, 1),
		workflowControlCh: make(chan struct{}, 1),
		logPrefix:         "gc start",
		stdout:            stdout,
		stderr:            stderr,
	}
	cr.run(ctx)
}

// shortRev returns the first 12 characters of a revision hash.
func shortRev(rev string) string {
	if len(rev) > 12 {
		return rev[:12]
	}
	return rev
}

// configReloadSummary returns a human-readable summary of what changed
// between config reloads.
func configReloadSummary(oldAgents, oldRigs, newAgents, newRigs int) string {
	var parts []string
	switch {
	case newAgents > oldAgents:
		parts = append(parts, fmt.Sprintf("%d agents (+%d)", newAgents, newAgents-oldAgents))
	case newAgents < oldAgents:
		parts = append(parts, fmt.Sprintf("%d agents (-%d)", newAgents, oldAgents-newAgents))
	default:
		parts = append(parts, fmt.Sprintf("%d agents", newAgents))
	}
	switch {
	case newRigs > oldRigs:
		parts = append(parts, fmt.Sprintf("%d rigs (+%d)", newRigs, newRigs-oldRigs))
	case newRigs < oldRigs:
		parts = append(parts, fmt.Sprintf("%d rigs (-%d)", newRigs, oldRigs-newRigs))
	default:
		parts = append(parts, fmt.Sprintf("%d rigs", newRigs))
	}
	return strings.Join(parts, ", ")
}

// runController runs the persistent controller loop. It acquires a lock,
// opens a control socket, runs the reconciliation loop, and on shutdown
// stops all agents. Returns an exit code. initialWatchDirs is the set of
// directories to watch for config changes (from initial provenance).
func runController(
	cityPath string,
	tomlPath string,
	cfg *config.City,
	configRev string,
	buildFn func(*config.City, runtime.Provider, beads.Store) DesiredStateResult,
	buildFnWithSessionBeads func(*config.City, runtime.Provider, beads.Store, *sessionBeadSnapshot) DesiredStateResult,
	sp runtime.Provider,
	dops drainOps,
	poolSessions map[string]time.Duration,
	poolDeathHandlers map[string]poolDeathInfo,
	initialWatchDirs []string,
	rec events.Recorder,
	eventProv events.Provider,
	stdout, stderr io.Writer,
) int {
	lock, err := acquireControllerLock(cityPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc start: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	defer lock.Close() //nolint:errcheck // best-effort cleanup

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Signal handler: SIGINT/SIGTERM → cancel.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		cancel()
	}()

	convergenceReqCh := make(chan convergenceRequest, 16)
	pokeCh := make(chan struct{}, 1)
	workflowControlCh := make(chan struct{}, 1)

	sockPath := filepath.Join(cityPath, ".gc", "controller.sock")
	lis, err := startControllerSocket(cityPath, cancel, convergenceReqCh, pokeCh, workflowControlCh)
	if err != nil {
		fmt.Fprintf(stderr, "gc start: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	defer lis.Close()         //nolint:errcheck // best-effort cleanup
	defer os.Remove(sockPath) //nolint:errcheck // best-effort cleanup

	// Generate and write the controller token for convergence loop ACL.
	// The token is written to .gc/controller.token and kept in memory only.
	// It is NOT set in os.Environ() to prevent leaking to child processes
	// (exec scripts, git commands, order hooks). Future waves that need
	// the token from controller code use convergence.ReadToken() or pass it
	// explicitly through function parameters.
	controllerToken, err := convergence.GenerateToken()
	if err != nil {
		fmt.Fprintf(stderr, "gc start: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	_ = controllerToken // available for future waves via function parameters
	if err := convergence.WriteToken(cityPath, controllerToken); err != nil {
		fmt.Fprintf(stderr, "gc start: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	defer convergence.RemoveToken(cityPath) //nolint:errcheck // best-effort cleanup

	cityName := cfg.Workspace.Name
	if cityName == "" {
		cityName = filepath.Base(cityPath)
	}
	rec.Record(events.Event{Type: events.ControllerStarted, Actor: "gc"})
	telemetry.RecordControllerLifecycle(context.Background(), "started")
	fmt.Fprintln(stdout, "Controller started.") //nolint:errcheck // best-effort stdout

	cr := newCityRuntime(CityRuntimeParams{
		CityPath:                cityPath,
		CityName:                cityName,
		TomlPath:                tomlPath,
		WatchDirs:               initialWatchDirs,
		ConfigRev:               configRev,
		Cfg:                     cfg,
		SP:                      sp,
		Publication:             supervisor.PublicationConfig{},
		BuildFn:                 buildFn,
		BuildFnWithSessionBeads: buildFnWithSessionBeads,
		Dops:                    dops,
		Rec:                     rec,
		PoolSessions:            poolSessions,
		PoolDeathHandlers:       poolDeathHandlers,
		ConvergenceReqCh:        convergenceReqCh,
		PokeCh:                  pokeCh,
		WorkflowControlCh:       workflowControlCh,
		Stdout:                  stdout,
		Stderr:                  stderr,
	})

	// Start API server if configured.
	if cfg.API.Port > 0 {
		cs := newControllerState(cfg, sp, eventProv, cityName, cityPath)
		cs.ct = cr.crashTrack()
		cs.pokeCh = pokeCh
		cs.services = cr.svc
		cr.setControllerState(cs)
		bind := cfg.API.BindOrDefault()
		nonLocal := bind != "127.0.0.1" && bind != "localhost" && bind != "::1"
		var apiSrv *api.Server
		if nonLocal && !cfg.API.AllowMutations {
			apiSrv = api.NewReadOnly(cs)
			fmt.Fprintf(stderr, "api: binding to %s — mutation endpoints disabled (non-localhost)\n", bind) //nolint:errcheck
		} else {
			apiSrv = api.New(cs)
		}
		addr := net.JoinHostPort(bind, strconv.Itoa(cfg.API.Port))
		apiLis, apiErr := net.Listen("tcp", addr)
		if apiErr != nil {
			fmt.Fprintf(stderr, "api: WARNING: listen %s failed: %v — continuing without API server\n", addr, apiErr) //nolint:errcheck // best-effort stderr
		} else {
			go func() {
				if err := apiSrv.Serve(apiLis); err != nil && !errors.Is(err, http.ErrServerClosed) {
					fmt.Fprintf(stderr, "api: %v\n", err) //nolint:errcheck // best-effort stderr
				}
			}()
			defer func() {
				shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				apiSrv.Shutdown(shutCtx) //nolint:errcheck // best-effort cleanup
			}()
			fmt.Fprintf(stdout, "API server listening on http://%s\n", addr) //nolint:errcheck // best-effort stdout
		}
	}

	runPoolOnBoot(cfg, cityPath, shellScaleCheck, stderr)
	cr.run(ctx)
	cr.shutdown()

	rec.Record(events.Event{Type: events.ControllerStopped, Actor: "gc"})
	telemetry.RecordControllerLifecycle(context.Background(), "stopped")
	fmt.Fprintln(stdout, "Controller stopped.") //nolint:errcheck // best-effort stdout
	return 0
}
