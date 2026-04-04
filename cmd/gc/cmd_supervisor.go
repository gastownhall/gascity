package main

import (
	"bufio"
	"context"
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

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/convergence"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/hooks"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/supervisor"
	"github.com/gastownhall/gascity/internal/telemetry"
	"github.com/gastownhall/gascity/internal/workspacesvc"
	"github.com/spf13/cobra"
)

func newSupervisorCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "supervisor",
		Short: "Manage the machine-wide supervisor",
		Long: `Manage the machine-wide supervisor.

The supervisor manages all registered cities from a single process,
hosting a unified API server. Use "gc init", "gc start", or "gc register"
to add cities.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(
		newSupervisorRunCmd(stdout, stderr),
		newSupervisorStartCmd(stdout, stderr),
		newSupervisorStopCmd(stdout, stderr),
		newSupervisorStatusCmd(stdout, stderr),
		newSupervisorReloadCmd(stdout, stderr),
		newSupervisorLogsCmd(stdout, stderr),
		newSupervisorInstallCmd(stdout, stderr),
		newSupervisorUninstallCmd(stdout, stderr),
	)
	return cmd
}

func newSupervisorStartCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "start",
		Short: "Start the machine-wide supervisor in the background",
		Long: `Start the machine-wide supervisor in the background.

This forks "gc supervisor run", verifies it became ready, and returns.`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if doSupervisorStart(stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	return cmd
}

func newSupervisorStopCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "stop",
		Short: "Stop the machine-wide supervisor",
		Long:  `Stop the running machine-wide supervisor and all its cities.`,
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if stopSupervisor(stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	return cmd
}

func newSupervisorStatusCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Check if the supervisor is running",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if supervisorStatus(stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	return cmd
}

// acquireSupervisorLock takes an exclusive flock on the supervisor lock file.
func acquireSupervisorLock() (*os.File, error) {
	dir := supervisor.RuntimeDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("creating runtime dir: %w", err)
	}
	path := filepath.Join(dir, "supervisor.lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("opening supervisor lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close() //nolint:errcheck
		return nil, fmt.Errorf("supervisor already running")
	}
	return f, nil
}

func guardSupervisorSocketDir(dir string) {
	if !isTestBinary() {
		return
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	hostGC := filepath.Join(home, ".gc")
	if strings.HasPrefix(dir, hostGC+string(filepath.Separator)) || dir == hostGC {
		panic("supervisorSocketPath: refusing to connect to host supervisor socket during tests")
	}
}

func supervisorSocketPathForDir(dir string) string {
	guardSupervisorSocketDir(dir)
	return filepath.Join(dir, "supervisor.sock")
}

func supervisorSocketPathCandidates() []string {
	paths := []string{supervisorSocketPathForDir(supervisor.RuntimeDir())}
	defaultPath := supervisorSocketPathForDir(supervisor.DefaultHome())
	if defaultPath != paths[0] {
		paths = append(paths, defaultPath)
	}
	return paths
}

// supervisorSocketPath returns the path to the supervisor control socket.
//
// Guard: in test binaries, the resolved path must not point to the host's
// real runtime directory. The DefaultHome/RuntimeDir guards catch most
// cases, but this adds defense-in-depth for the socket specifically.
func supervisorSocketPath() string {
	return supervisorSocketPathCandidates()[0]
}

// startSupervisorSocket creates a Unix domain socket at the given path
// and handles ping/stop commands. Unlike startControllerSocket (which
// constructs its own path), this binds to the exact path provided.
type reconcileRequest struct {
	done chan struct{}
}

var (
	supervisorReloadQueueTimeout = 5 * time.Second
	supervisorReloadWaitTimeout  = 5 * time.Minute
)

func startSupervisorSocket(sockPath string, cancelFn context.CancelFunc, reconcileCh chan reconcileRequest) (net.Listener, error) {
	os.Remove(sockPath) //nolint:errcheck // remove stale socket from previous crash
	lis, err := net.Listen("unix", sockPath)
	if err != nil {
		return nil, fmt.Errorf("listening on supervisor socket: %w", err)
	}
	go func() {
		for {
			conn, err := lis.Accept()
			if err != nil {
				// Permanent close — exit loop.
				if errors.Is(err, net.ErrClosed) {
					return
				}
				// Transient error — log and continue.
				fmt.Fprintf(os.Stderr, "gc supervisor: socket accept: %v\n", err) //nolint:errcheck
				continue
			}
			go handleSupervisorConn(conn, cancelFn, reconcileCh)
		}
	}()
	return lis, nil
}

// handleSupervisorConn reads from a connection and dispatches commands.
// Supported: "stop" (shutdown), "ping" (liveness check, returns PID),
// "reload" (trigger immediate reconciliation of all cities).
func handleSupervisorConn(conn net.Conn, cancelFn context.CancelFunc, reconcileCh chan reconcileRequest) {
	defer conn.Close()                                     //nolint:errcheck
	conn.SetReadDeadline(time.Now().Add(60 * time.Second)) //nolint:errcheck
	scanner := bufio.NewScanner(conn)
	if scanner.Scan() {
		switch scanner.Text() {
		case "stop":
			cancelFn()
			conn.Write([]byte("ok\n")) //nolint:errcheck
		case "ping":
			fmt.Fprintf(conn, "%d\n", os.Getpid()) //nolint:errcheck
		case "reload":
			req := reconcileRequest{done: make(chan struct{})}
			select {
			case reconcileCh <- req:
			case <-time.After(supervisorReloadQueueTimeout):
				conn.Write([]byte("busy\n")) //nolint:errcheck
				return
			}
			select {
			case <-req.done:
				conn.Write([]byte("ok\n")) //nolint:errcheck
			case <-time.After(supervisorReloadWaitTimeout):
				conn.Write([]byte("timeout\n")) //nolint:errcheck
			}
		}
	}
}

// supervisorAlive checks whether the supervisor is running by pinging
// the control socket. Returns the PID if alive, 0 otherwise.
func supervisorAlive() int {
	_, pid := runningSupervisorSocket()
	return pid
}

func runningSupervisorSocket() (string, int) {
	for _, sockPath := range supervisorSocketPathCandidates() {
		if pid := supervisorAliveAtPath(sockPath); pid != 0 {
			return sockPath, pid
		}
	}
	return "", 0
}

func supervisorAliveAtPath(sockPath string) int {
	conn, err := net.DialTimeout("unix", sockPath, 500*time.Millisecond)
	if err != nil {
		return 0
	}
	defer conn.Close()                                    //nolint:errcheck
	conn.Write([]byte("ping\n"))                          //nolint:errcheck
	conn.SetReadDeadline(time.Now().Add(2 * time.Second)) //nolint:errcheck
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

// stopSupervisor sends a stop command to the running supervisor.
// It also unloads the platform service (without removing the unit file)
// so launchd/systemd doesn't immediately restart the supervisor.
func stopSupervisor(stdout, stderr io.Writer) int {
	// Unload the platform service first so the service manager doesn't
	// restart the supervisor after we send the stop command.
	unloadSupervisorService()

	sockPath, _ := runningSupervisorSocket()
	if sockPath == "" {
		fmt.Fprintln(stderr, "gc supervisor stop: supervisor is not running") //nolint:errcheck
		return 1
	}
	conn, err := net.DialTimeout("unix", sockPath, 2*time.Second)
	if err != nil {
		fmt.Fprintln(stderr, "gc supervisor stop: supervisor is not running") //nolint:errcheck
		return 1
	}
	defer conn.Close()                                     //nolint:errcheck
	conn.Write([]byte("stop\n"))                           //nolint:errcheck
	conn.SetReadDeadline(time.Now().Add(10 * time.Second)) //nolint:errcheck
	buf := make([]byte, 64)
	n, _ := conn.Read(buf)
	if n > 0 && string(buf[:n]) == "ok\n" {
		fmt.Fprintln(stdout, "Supervisor stopping...") //nolint:errcheck
		return 0
	}
	fmt.Fprintln(stderr, "gc supervisor stop: no acknowledgment from supervisor") //nolint:errcheck
	return 1
}

// supervisorStatus checks and reports whether the supervisor is running.
func supervisorStatus(stdout, _ io.Writer) int {
	pid := supervisorAlive()
	if pid > 0 {
		fmt.Fprintf(stdout, "Supervisor is running (PID %d)\n", pid) //nolint:errcheck
		return 0
	}
	fmt.Fprintln(stdout, "Supervisor is not running") //nolint:errcheck
	return 1
}

func newSupervisorReloadCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "reload",
		Short: "Trigger immediate reconciliation of all cities",
		Long: `Send a reload signal to the running supervisor, causing it to
immediately re-read the registry and reconcile all cities. Use this
after killing a child process to force the supervisor to detect the
change and restart it without waiting for the next patrol tick.`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if reloadSupervisor(stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	return cmd
}

// reloadSupervisor sends a reload command to the running supervisor.
func reloadSupervisor(stdout, stderr io.Writer) int {
	sockPath, _ := runningSupervisorSocket()
	if sockPath == "" {
		fmt.Fprintln(stderr, "gc supervisor reload: supervisor is not running; start it with 'gc supervisor start'") //nolint:errcheck
		return 1
	}
	conn, err := net.DialTimeout("unix", sockPath, 2*time.Second)
	if err != nil {
		fmt.Fprintln(stderr, "gc supervisor reload: supervisor is not running; start it with 'gc supervisor start'") //nolint:errcheck
		return 1
	}
	defer conn.Close()                                                                //nolint:errcheck
	conn.Write([]byte("reload\n"))                                                    //nolint:errcheck
	conn.SetReadDeadline(time.Now().Add(supervisorReloadWaitTimeout + 5*time.Second)) //nolint:errcheck
	buf := make([]byte, 64)
	n, _ := conn.Read(buf)
	resp := strings.TrimSpace(string(buf[:n]))
	switch resp {
	case "ok":
		fmt.Fprintln(stdout, "Reconciliation triggered.") //nolint:errcheck
		return 0
	case "busy":
		fmt.Fprintln(stderr, "gc supervisor reload: reconcile queue is busy; try again shortly") //nolint:errcheck
		return 1
	case "timeout":
		fmt.Fprintln(stderr, "gc supervisor reload: reconcile did not finish before timeout") //nolint:errcheck
		return 1
	}
	fmt.Fprintln(stderr, "gc supervisor reload: supervisor not responding (may be shutting down); try 'gc supervisor start'") //nolint:errcheck
	return 1
}

// managedCity tracks a running CityRuntime inside the supervisor.
type managedCity struct {
	cr         *CityRuntime
	name       string // city name at launch — used for name-drift detection
	started    bool
	status     string
	cancel     context.CancelFunc
	done       chan struct{} // closed when the city goroutine exits
	closer     io.Closer     // FileRecorder (or nil); closed on city stop
	tombstoned atomic.Bool   // set before Remove() in shutdown paths for teardown safety
}

// managedCityStopTimeout returns the grace period for a city stop.
// Only ShutdownTimeoutDuration is used — startup and drift-drain timeouts
// are intentionally excluded because they govern unrelated lifecycle phases.
// The 5s nil-config fallback matches ShutdownTimeoutDuration's own default.
func managedCityStopTimeout(mc *managedCity) time.Duration {
	if mc == nil || mc.cr == nil || mc.cr.cfg == nil {
		return 5 * time.Second
	}
	return mc.cr.cfg.Daemon.ShutdownTimeoutDuration()
}

func stopManagedCity(mc *managedCity, cityPath string, stderr io.Writer) {
	if mc == nil {
		return
	}
	mc.cancel()
	timeout := managedCityStopTimeout(mc)
	if timeout > 0 {
		select {
		case <-mc.done:
			if err := shutdownBeadsProvider(cityPath); err != nil {
				fmt.Fprintf(stderr, "gc supervisor: city '%s': bead store: %v\n", mc.name, err) //nolint:errcheck
			}
			if mc.closer != nil {
				mc.closer.Close() //nolint:errcheck
			}
			return
		case <-time.After(timeout):
			fmt.Fprintf(stderr, "gc supervisor: city '%s' did not exit within %s after cancel; forcing shutdown\n", mc.name, timeout) //nolint:errcheck
		}
	}
	if mc.cr != nil {
		func() {
			defer func() { recover() }() //nolint:errcheck
			mc.cr.shutdown()
		}()
	}
	if timeout > 0 {
		select {
		case <-mc.done:
		case <-time.After(timeout):
			fmt.Fprintf(stderr, "gc supervisor: city '%s' did not exit within %s after forced shutdown\n", mc.name, timeout) //nolint:errcheck
		}
	}
	if err := shutdownBeadsProvider(cityPath); err != nil {
		fmt.Fprintf(stderr, "gc supervisor: city '%s': bead store: %v\n", mc.name, err) //nolint:errcheck
	}
	if mc.closer != nil {
		mc.closer.Close() //nolint:errcheck
	}
}

// runSupervisor is the main supervisor loop. It acquires the lock,
// starts a control socket, reads the registry, starts CityRuntimes,
// and runs until canceled.
func runSupervisor(stdout, stderr io.Writer) int {
	if pid := supervisorAlive(); pid != 0 {
		fmt.Fprintf(stderr, "gc supervisor: supervisor already running (PID %d)\n", pid) //nolint:errcheck
		return 1
	}

	lock, err := acquireSupervisorLock()
	if err != nil {
		fmt.Fprintf(stderr, "gc supervisor: %v\n", err) //nolint:errcheck
		return 1
	}
	defer lock.Close() //nolint:errcheck

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Reconcile channel — triggers immediate reconciliation from SIGHUP
	// or the "reload" socket command.
	reconcileCh := make(chan reconcileRequest, 1)

	// Signal handler: SIGINT/SIGTERM → shutdown, SIGHUP → immediate reconcile.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigCh)
	go func() {
		for {
			select {
			case sig := <-sigCh:
				if sig == syscall.SIGHUP {
					fmt.Fprintln(stderr, "SIGHUP received, triggering reconciliation...") //nolint:errcheck
					select {
					case reconcileCh <- reconcileRequest{}:
					default: // reconcile already pending
					}
					continue
				}
				// SIGINT/SIGTERM → shutdown.
				cancel()
				return
			case <-ctx.Done():
				return
			}
		}
	}()

	// Load supervisor config.
	supCfg, err := supervisor.LoadConfig(supervisor.ConfigPath())
	if err != nil {
		fmt.Fprintf(stderr, "gc supervisor: config: %v\n", err) //nolint:errcheck
		return 1
	}

	reg := supervisor.NewRegistry(supervisor.RegistryPath())

	// Track managed cities via atomic-snapshot registry. API reads are
	// lock-free (atomic pointer load); mutations go through citiesMu.
	registry := newCityRegistry()

	// Start API server with city-namespaced routing (Phase 2).
	startedAt := time.Now()
	bind := supCfg.Supervisor.BindOrDefault()
	port := supCfg.Supervisor.PortOrDefault()
	nonLocal := bind != "127.0.0.1" && bind != "localhost" && bind != "::1"
	readOnly := nonLocal && !supCfg.Supervisor.AllowMutations
	if readOnly {
		fmt.Fprintf(stderr, "gc supervisor: binding to %s — mutation endpoints disabled (non-localhost)\n", bind) //nolint:errcheck
	}
	apiMux := api.NewSupervisorMux(registry, readOnly, version, startedAt)
	addr := net.JoinHostPort(bind, strconv.Itoa(port))
	apiLis, apiErr := net.Listen("tcp", addr)
	if apiErr != nil {
		fmt.Fprintf(stderr, "gc supervisor: api: listen %s failed: %v\n", addr, apiErr) //nolint:errcheck
		return 1
	}
	go func() {
		if err := apiMux.Serve(apiLis); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(stderr, "gc supervisor: api: %v\n", err) //nolint:errcheck
		}
	}()
	defer func() {
		shutCtx, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		apiMux.Shutdown(shutCtx) //nolint:errcheck
	}()
	fmt.Fprintf(stdout, "Supervisor API listening on http://%s\n", addr) //nolint:errcheck

	// Control socket — uses supervisor-specific path, not the per-city controller socket.
	sockPath := supervisorSocketPath()
	if err := os.MkdirAll(filepath.Dir(sockPath), 0o700); err != nil {
		fmt.Fprintf(stderr, "gc supervisor: creating socket dir: %v\n", err) //nolint:errcheck
		return 1
	}
	lis, err := startSupervisorSocket(sockPath, cancel, reconcileCh)
	if err != nil {
		fmt.Fprintf(stderr, "gc supervisor: %v\n", err) //nolint:errcheck
		return 1
	}
	defer lis.Close()         //nolint:errcheck
	defer os.Remove(sockPath) //nolint:errcheck

	fmt.Fprintln(stdout, "Supervisor started.") //nolint:errcheck

	// Reconciliation loop.
	interval := supCfg.Supervisor.PatrolIntervalDuration()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// safeReconcile wraps reconcileCities with panic recovery so a bug
	// in the reconciliation loop doesn't crash the entire supervisor.
	safeReconcile := func() {
		defer func() {
			if r := recover(); r != nil {
				fmt.Fprintf(stderr, "gc supervisor: reconcile panicked: %v\n", r) //nolint:errcheck
			}
		}()
		reconcileCities(reg, registry, supCfg.Publication, stdout, stderr)
	}

	// Initial reconcile.
	safeReconcile()

	for {
		select {
		case <-ticker.C:
			safeReconcile()
		case req := <-reconcileCh:
			safeReconcile()
			// Also poke all running cities so they immediately reconcile
			// their agents (e.g. after a child process was killed).
			snap := registry.Snapshot()
			for _, v := range snap.all {
				if v.Started && v.cs != nil {
					v.cs.Poke()
				}
			}
			if req.done != nil {
				close(req.done)
			}
		case <-ctx.Done():
			// Shutdown all cities. Collect under lock, then stop outside
			// to avoid blocking API requests during graceful shutdown.
			var toStop map[string]*managedCity
			registry.BatchUpdate(func(
				cities map[string]*managedCity,
				_ map[string]cityInitProgress,
				_ map[string]*initFailRecord,
				_ map[string]*panicRecord,
			) {
				toStop = make(map[string]*managedCity, len(cities))
				for k, v := range cities {
					v.tombstoned.Store(true)
					toStop[k] = v
					delete(cities, k)
				}
			})
			for name, mc := range toStop {
				fmt.Fprintf(stdout, "Stopping city '%s'...\n", name) //nolint:errcheck
				stopManagedCity(mc, name, stderr)
				fmt.Fprintf(stdout, "City '%s' stopped.\n", name) //nolint:errcheck
			}
			fmt.Fprintln(stdout, "Supervisor stopped.") //nolint:errcheck
			return 0
		}
	}
}

// panicRecord tracks consecutive panic count and next-eligible-restart time
// for crash-loop backoff on consistently-failing cities.
type panicRecord struct {
	count   int
	backoff time.Time // don't restart until after this time
}

// initFailRecord tracks consecutive initialization failure count and
// backoff for cities that fail prepareCityForSupervisor or config load.
// The configMod field lets us reset backoff when the user fixes their config.
type initFailRecord struct {
	count     int
	backoff   time.Time
	configMod time.Time // mtime of city.toml at last failure
	lastError string    // last error message for user-facing feedback
}

// reconcileCities compares the registry against running cities and
// starts/stops as needed. All state access goes through the cityRegistry.
func reconcileCities(
	reg *supervisor.Registry,
	cr *cityRegistry,
	publication supervisor.PublicationConfig,
	stdout, stderr io.Writer,
) {
	entries, err := reg.List()
	if err != nil {
		fmt.Fprintf(stderr, "gc supervisor: registry: %v\n", err) //nolint:errcheck
		return
	}

	// Build desired set.
	desired := make(map[string]supervisor.CityEntry, len(entries))
	for _, e := range entries {
		desired[e.Path] = e
	}

	// Stop cities no longer in registry. Collect under lock, stop outside
	// to avoid blocking API requests during graceful shutdown.
	var toStop []*managedCity
	var toStopPaths []string
	cr.BatchUpdate(func(
		cities map[string]*managedCity,
		_ map[string]cityInitProgress,
		_ map[string]*initFailRecord,
		_ map[string]*panicRecord,
	) {
		for path, mc := range cities {
			if _, ok := desired[path]; !ok {
				mc.tombstoned.Store(true)
				toStop = append(toStop, mc)
				toStopPaths = append(toStopPaths, path)
				delete(cities, path)
			}
		}
	})

	for i, mc := range toStop {
		name := filepath.Base(toStopPaths[i])
		fmt.Fprintf(stdout, "Unregistered city '%s', stopping...\n", name) //nolint:errcheck
		stopManagedCity(mc, toStopPaths[i], stderr)
		// Clear backoff so re-registering starts immediately.
		cr.BatchUpdate(func(
			_ map[string]*managedCity,
			_ map[string]cityInitProgress,
			initFailures map[string]*initFailRecord,
			panicHistory map[string]*panicRecord,
		) {
			delete(panicHistory, toStopPaths[i])
			delete(initFailures, toStopPaths[i])
		})
		fmt.Fprintf(stdout, "City '%s' stopped.\n", name) //nolint:errcheck
	}

	// Clear panicHistory and initFailures for any path no longer in the
	// desired set. This handles the case where a city panicked or failed
	// init (self-removed from cities + recorded backoff) and was then
	// unregistered — without this, re-registering the fixed city would
	// inherit the old backoff.
	cr.BatchUpdate(func(
		_ map[string]*managedCity,
		_ map[string]cityInitProgress,
		initFailures map[string]*initFailRecord,
		panicHistory map[string]*panicRecord,
	) {
		for path := range panicHistory {
			if _, ok := desired[path]; !ok {
				delete(panicHistory, path)
			}
		}
		for path := range initFailures {
			if _, ok := desired[path]; !ok {
				delete(initFailures, path)
			}
		}
	})

	// Detect name drift: if a running city's registry name changed,
	// schedule a stop/restart so live routing matches registry identity.
	var nameDriftPaths []string
	var nameDriftCities []*managedCity
	cr.BatchUpdate(func(
		cities map[string]*managedCity,
		_ map[string]cityInitProgress,
		_ map[string]*initFailRecord,
		_ map[string]*panicRecord,
	) {
		for path, mc := range cities {
			if entry, ok := desired[path]; ok {
				if entry.EffectiveName() != mc.name {
					nameDriftPaths = append(nameDriftPaths, path)
					nameDriftCities = append(nameDriftCities, mc)
					delete(cities, path)
				}
			}
		}
	})
	for i, mc := range nameDriftCities {
		fmt.Fprintf(stdout, "City name changed at '%s', restarting...\n", nameDriftPaths[i]) //nolint:errcheck
		stopManagedCity(mc, nameDriftPaths[i], stderr)
	}

	// Start new cities (and name-drifted restarts). Build list under lock,
	// then release lock for I/O-heavy initialization (config loading, bead
	// lifecycle, formula materialization, etc.).
	var toStart []supervisor.CityEntry
	cr.ReadCallback(func(
		cities map[string]*managedCity,
		initStatus map[string]cityInitProgress,
		_ map[string]*initFailRecord,
		_ map[string]*panicRecord,
	) {
		for path, entry := range desired {
			if _, running := cities[path]; !running {
				if _, initializing := initStatus[path]; initializing {
					continue
				}
				toStart = append(toStart, entry)
			}
		}
	})

	for _, entry := range toStart {
		path := entry.Path
		name := entry.EffectiveName()

		// Crash-loop backoff: skip cities that panicked recently.
		skipBackoff := func() bool {
			var skip bool
			cr.ReadCallback(func(
				_ map[string]*managedCity,
				_ map[string]cityInitProgress,
				_ map[string]*initFailRecord,
				panicHistory map[string]*panicRecord,
			) {
				pr := panicHistory[path]
				skip = pr != nil && time.Now().Before(pr.backoff)
			})
			return skip
		}()
		if skipBackoff {
			continue
		}

		// Init failure backoff: skip cities whose init failed recently,
		// unless the config file has been modified (user may have fixed it).
		tomlPath := filepath.Join(path, "city.toml")
		var skipInit bool
		var ifr *initFailRecord
		cr.ReadCallback(func(
			_ map[string]*managedCity,
			_ map[string]cityInitProgress,
			initFailures map[string]*initFailRecord,
			_ map[string]*panicRecord,
		) {
			rec := initFailures[path]
			if rec != nil && time.Now().Before(rec.backoff) {
				skipInit = true
				cp := *rec
				ifr = &cp
			}
		})
		if skipInit {
			// Check if config was modified since last failure.
			if info, err := os.Stat(tomlPath); err != nil || !info.ModTime().After(ifr.configMod) {
				continue
			}
			// Config changed — reset backoff and retry.
			cr.BatchUpdate(func(
				_ map[string]*managedCity,
				_ map[string]cityInitProgress,
				initFailures map[string]*initFailRecord,
				_ map[string]*panicRecord,
			) {
				delete(initFailures, path)
			})
		}

		// recordInitFailure logs the error and records backoff state.
		recordInitFailure := func(cityName, msg string) {
			fmt.Fprintf(stderr, "gc supervisor: city '%s': %s (skipping)\n", cityName, msg) //nolint:errcheck
			var configMod time.Time
			if info, stErr := os.Stat(tomlPath); stErr == nil {
				configMod = info.ModTime()
			}
			cr.BatchUpdate(func(
				_ map[string]*managedCity,
				_ map[string]cityInitProgress,
				initFailures map[string]*initFailRecord,
				_ map[string]*panicRecord,
			) {
				ifrec := initFailures[path]
				if ifrec == nil {
					ifrec = &initFailRecord{}
					initFailures[path] = ifrec
				}
				ifrec.count++
				exp := ifrec.count - 1
				if exp > 5 {
					exp = 5
				}
				delay := time.Duration(10<<exp) * time.Second
				if delay > 5*time.Minute {
					delay = 5 * time.Minute
				}
				ifrec.backoff = time.Now().Add(delay)
				ifrec.configMod = configMod
				ifrec.lastError = msg
				fmt.Fprintf(stderr, "gc supervisor: city '%s': init failure #%d, next retry in %s\n", cityName, ifrec.count, delay) //nolint:errcheck
			})
		}

		// Quick-parse city.toml for pre-load tasks (same as doStart).
		quickCfg, qErr := config.Load(fsys.OSFS{}, tomlPath)

		// Materialize gastown packs before full config load if needed.
		if qErr == nil && usesGastownPack(quickCfg) {
			if err := MaterializeGastownPacks(path); err != nil {
				fmt.Fprintf(stderr, "gc supervisor: city '%s': materializing gastown packs: %v\n", name, err) //nolint:errcheck
			}
		}

		// Auto-fetch remote packs before full config load (same as doStart).
		if qErr == nil && len(quickCfg.Packs) > 0 {
			if fErr := config.FetchPacks(quickCfg.Packs, path); fErr != nil {
				recordInitFailure(name, fmt.Sprintf("fetching packs: %v", fErr))
				continue
			}
		}

		// Load city config with provenance so WatchDirs covers included files.
		// System packs are appended as extra includes for normal pack expansion.
		cfg, prov, loadErr := config.LoadWithIncludes(fsys.OSFS{}, tomlPath, builtinPackIncludes(path)...)
		if loadErr != nil {
			recordInitFailure(name, loadErr.Error())
			continue
		}

		// Use registered name as authoritative identity. Warn if live
		// config has a different workspace.name (name drift).
		cityName := name // from entry.EffectiveName()
		if liveName := cfg.Workspace.Name; liveName != "" && liveName != cityName {
			fmt.Fprintf(stderr, "gc supervisor: city '%s': workspace.name changed to %q (re-register to update)\n", //nolint:errcheck
				cityName, liveName)
		}

		// Track initialization progress for the API.
		cr.BatchUpdate(func(
			_ map[string]*managedCity,
			initStatus map[string]cityInitProgress,
			_ map[string]*initFailRecord,
			_ map[string]*panicRecord,
		) {
			initStatus[path] = cityInitProgress{name: cityName, status: "loading_config"}
		})

		// Run critical city initialization (same steps as cmd_start.go).
		if err := prepareCityForSupervisor(path, cityName, cfg, stderr, func(status string) {
			cr.BatchUpdate(func(
				_ map[string]*managedCity,
				initStatus map[string]cityInitProgress,
				_ map[string]*initFailRecord,
				_ map[string]*panicRecord,
			) {
				initStatus[path] = cityInitProgress{name: cityName, status: status}
			})
		}); err != nil {
			cr.BatchUpdate(func(
				_ map[string]*managedCity,
				initStatus map[string]cityInitProgress,
				_ map[string]*initFailRecord,
				_ map[string]*panicRecord,
			) {
				delete(initStatus, path)
			})
			recordInitFailure(cityName, fmt.Sprintf("init: %v", err))
			continue
		}

		runPostPrepareStep := func(status string, fn func() error) error {
			cr.BatchUpdate(func(
				_ map[string]*managedCity,
				initStatus map[string]cityInitProgress,
				_ map[string]*initFailRecord,
				_ map[string]*panicRecord,
			) {
				initStatus[path] = cityInitProgress{name: cityName, status: status}
			})
			started := time.Now()
			err := fn()
			if dur := time.Since(started); dur > time.Second {
				fmt.Fprintf(stderr, "gc supervisor: city '%s': %s took %s\n", cityName, status, dur.Round(10*time.Millisecond)) //nolint:errcheck
			}
			return err
		}

		// Warn if city has its own API port.
		if cfg.API.Port > 0 {
			fmt.Fprintf(stderr, "gc supervisor: city '%s' has [api] port=%d which is ignored under supervisor mode\n", //nolint:errcheck
				cityName, cfg.API.Port)
		}

		var sp runtime.Provider
		spErr := runPostPrepareStep("creating_session_provider", func() error {
			var err error
			sp, err = newSessionProviderByName(
				effectiveProviderName(cfg.Session.Provider), cfg.Session, cityName, path)
			return err
		})
		if spErr != nil {
			cr.BatchUpdate(func(
				_ map[string]*managedCity,
				initStatus map[string]cityInitProgress,
				_ map[string]*initFailRecord,
				_ map[string]*panicRecord,
			) {
				delete(initStatus, path)
			})
			recordInitFailure(cityName, fmt.Sprintf("session provider: %v", spErr))
			continue
		}

		// Fail-fast image pre-check for container providers (same as doStart).
		if err := runPostPrepareStep("checking_agent_images", func() error {
			return checkAgentImages(sp, cfg.Agents, stderr)
		}); err != nil {
			cr.BatchUpdate(func(
				_ map[string]*managedCity,
				initStatus map[string]cityInitProgress,
				_ map[string]*initFailRecord,
				_ map[string]*panicRecord,
			) {
				delete(initStatus, path)
			})
			recordInitFailure(cityName, err.Error())
			continue
		}

		rec := events.Discard
		var eventProv events.Provider
		evPath := filepath.Join(path, ".gc", "events.jsonl")
		fr, frErr := events.NewFileRecorder(evPath, stderr)
		if frErr == nil {
			rec = fr
			eventProv = fr
		}

		dops := newDrainOps(sp)
		poolSessions := computePoolSessions(cfg, cityName, path, sp)
		poolDeathHandlers := computePoolDeathHandlers(cfg, cityName, path, sp)
		watchDirs := config.WatchDirs(prov, cfg, path)
		configRev := config.Revision(fsys.OSFS{}, prov, cfg, path)
		pokeCh := make(chan struct{}, 1)
		cityCtx, cityCancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		mc := &managedCity{name: cityName, cancel: cityCancel, done: done, closer: fr}

		convergenceReqCh := make(chan convergenceRequest, 16)
		controlDispatcherCh := make(chan struct{}, 1)

		var cityRuntime *CityRuntime
		if err := runPostPrepareStep("building_city_runtime", func() error {
			cityRuntime = newCityRuntime(CityRuntimeParams{
				CityPath:                path,
				CityName:                cityName,
				TomlPath:                tomlPath,
				WatchDirs:               watchDirs,
				ConfigRev:               configRev,
				Cfg:                     cfg,
				SP:                      sp,
				Publication:             publication,
				BuildFn:                 supervisorBuildAgentsFn(path, cityName, stderr),
				BuildFnWithSessionBeads: supervisorBuildAgentsFnWithSessionBeads(path, cityName, stderr),
				Dops:                    dops,
				Rec:                     rec,
				PoolSessions:            poolSessions,
				PoolDeathHandlers:       poolDeathHandlers,
				ConvergenceReqCh:        convergenceReqCh,
				PokeCh:                  pokeCh,
				ControlDispatcherCh:     controlDispatcherCh,
				OnStarted: func() {
					cr.UpdateCallback(path, func(m *managedCity) {
						m.started = true
					})
				},
				OnStatus: func(status string) {
					cr.UpdateCallback(path, func(m *managedCity) {
						m.status = status
					})
				},
				LogPrefix: "gc supervisor",
				Stdout:    stdout,
				Stderr:    stderr,
			})
			return nil
		}); err != nil {
			recordInitFailure(cityName, fmt.Sprintf("city runtime: %v", err))
			continue
		}
		mc.cr = cityRuntime

		// Wire API state.
		var cs *controllerState
		if err := runPostPrepareStep("opening_controller_state", func() error {
			cs = newControllerState(cfg, sp, eventProv, cityName, path)
			return nil
		}); err != nil {
			recordInitFailure(cityName, fmt.Sprintf("controller state: %v", err))
			continue
		}
		cs.ct = cityRuntime.crashTrack()
		cs.pokeCh = pokeCh
		cs.services = cityRuntime.svc
		cs.startBeadEventWatcher(cityCtx)
		cityRuntime.setControllerState(cs)

		// Run pool on_boot hooks (same as runController does).
		if err := runPostPrepareStep("running_pool_on_boot", func() error {
			runPoolOnBoot(cfg, path, shellScaleCheck, stderr)
			return nil
		}); err != nil {
			recordInitFailure(cityName, fmt.Sprintf("pool on_boot: %v", err))
			continue
		}

		// Insert into map BEFORE launching goroutine to prevent races
		// where an early panic deletes a non-existent entry, leaving a
		// zombie after the post-launch insertion.
		alreadyRunning := publishManagedCity(cr, path, mc)
		if alreadyRunning {
			cr.BatchUpdate(func(
				_ map[string]*managedCity,
				initStatus map[string]cityInitProgress,
				_ map[string]*initFailRecord,
				_ map[string]*panicRecord,
			) {
				delete(initStatus, path)
			})
			cityCancel()
			cityRuntime.shutdown()
			if fr != nil {
				fr.Close() //nolint:errcheck
			}
			continue
		}

		// Acquire controller lock to prevent split-brain with standalone
		// controllers (mirrors runController in controller.go).
		lock, lockErr := acquireControllerLock(path)
		if lockErr != nil {
			fmt.Fprintf(stderr, "gc supervisor: city '%s': controller lock: %v\n", cityName, lockErr) //nolint:errcheck
			cityCancel()
			cityRuntime.shutdown()
			if fr != nil {
				fr.Close() //nolint:errcheck
			}
			cr.BatchUpdate(func(
				cities map[string]*managedCity,
				_ map[string]cityInitProgress,
				_ map[string]*initFailRecord,
				_ map[string]*panicRecord,
			) {
				delete(cities, path)
			})
			recordInitFailure(cityName, fmt.Sprintf("controller lock: %v", lockErr))
			continue
		}

		// Start controller socket AFTER the alreadyRunning check so we
		// never destroy a live city's socket or leak a listener.
		sockPath := filepath.Join(path, ".gc", "controller.sock")
		lis, lisErr := startControllerSocket(path, cityCancel, convergenceReqCh, pokeCh, controlDispatcherCh)
		if lisErr != nil {
			fmt.Fprintf(stderr, "gc supervisor: city '%s': controller socket: %v\n", cityName, lisErr) //nolint:errcheck
			lock.Close()                                                                               //nolint:errcheck // no socket to race with
			cityCancel()
			cityRuntime.shutdown()
			if fr != nil {
				fr.Close() //nolint:errcheck
			}
			cr.BatchUpdate(func(
				cities map[string]*managedCity,
				_ map[string]cityInitProgress,
				_ map[string]*initFailRecord,
				_ map[string]*panicRecord,
			) {
				delete(cities, path)
			})
			recordInitFailure(cityName, fmt.Sprintf("controller socket: %v", lisErr))
			continue
		}

		// Generate controller token for convergence ACL
		// (mirrors runController in controller.go).
		controllerToken, tokenErr := convergence.GenerateToken()
		if tokenErr != nil {
			fmt.Fprintf(stderr, "gc supervisor: city '%s': controller token: %v\n", cityName, tokenErr) //nolint:errcheck
			lis.Close()                                                                                 //nolint:errcheck
			os.Remove(sockPath)                                                                         //nolint:errcheck
			lock.Close()                                                                                //nolint:errcheck // lock released last
			cityCancel()
			cityRuntime.shutdown()
			if fr != nil {
				fr.Close() //nolint:errcheck
			}
			cr.BatchUpdate(func(
				cities map[string]*managedCity,
				_ map[string]cityInitProgress,
				_ map[string]*initFailRecord,
				_ map[string]*panicRecord,
			) {
				delete(cities, path)
			})
			recordInitFailure(cityName, fmt.Sprintf("controller token: %v", tokenErr))
			continue
		}
		_ = controllerToken // available for future waves via function parameters
		if err := convergence.WriteToken(path, controllerToken); err != nil {
			fmt.Fprintf(stderr, "gc supervisor: city '%s': writing controller token: %v\n", cityName, err) //nolint:errcheck
			lis.Close()                                                                                    //nolint:errcheck
			os.Remove(sockPath)                                                                            //nolint:errcheck
			lock.Close()                                                                                   //nolint:errcheck // lock released last
			cityCancel()
			cityRuntime.shutdown()
			if fr != nil {
				fr.Close() //nolint:errcheck
			}
			cr.BatchUpdate(func(
				cities map[string]*managedCity,
				_ map[string]cityInitProgress,
				_ map[string]*initFailRecord,
				_ map[string]*panicRecord,
			) {
				delete(cities, path)
			})
			recordInitFailure(cityName, fmt.Sprintf("controller token write: %v", err))
			continue
		}

		// Capture the socket's os.FileInfo so the goroutine can perform
		// ownership-safe socket removal on exit via os.SameFile — a
		// replacement city that re-bound the same path won't have its
		// socket unlinked. Uses os.SameFile for cross-platform safety.
		sockInfo, _ := os.Stat(sockPath)

		// Disable automatic socket unlinking on listener close so our
		// ownership-safe removal logic is the sole path for cleanup.
		// Without this, l.Close() unconditionally unlinks the socket
		// file, defeating the SameFile check.
		if ul, ok := lis.(*net.UnixListener); ok {
			ul.SetUnlinkOnClose(false)
		}

		go func(n, p string, cityFr *events.FileRecorder, l net.Listener, sock string, origSockInfo os.FileInfo, lk *os.File) {
			// Recovery and close(done) defer is pushed FIRST so it
			// executes LAST (Go LIFO), preserving the invariant that
			// completion is signaled only after all resource cleanup.
			defer func() {
				if r := recover(); r != nil {
					fmt.Fprintf(stderr, "gc supervisor: city '%s' panicked: %v\n", n, r) //nolint:errcheck
					// Gracefully stop agents so they aren't orphaned.
					// Wrap in recovery to prevent nested panic from crashing
					// the entire supervisor.
					func() {
						defer func() { recover() }() //nolint:errcheck
						cityRuntime.shutdown()
					}()
					if err := shutdownBeadsProvider(p); err != nil {
						fmt.Fprintf(stderr, "gc supervisor: city '%s': bead store: %v\n", n, err) //nolint:errcheck
					}
					// Close the file recorder (only on panic — normal exit
					// leaves it for the external caller via mc.closer).
					if cityFr != nil {
						cityFr.Close() //nolint:errcheck
					}
					// Record panic for crash-loop backoff and remove from
					// cities map in a single batch update.
					cr.BatchUpdate(func(
						cities map[string]*managedCity,
						_ map[string]cityInitProgress,
						_ map[string]*initFailRecord,
						panicHistory map[string]*panicRecord,
					) {
						pr := panicHistory[p]
						if pr == nil {
							pr = &panicRecord{}
							panicHistory[p] = pr
						}
						pr.count++
						// Exponential backoff: 10s, 20s, 40s, ... capped at 5 min.
						exp := pr.count - 1
						if exp > 5 {
							exp = 5 // prevent int overflow at high panic counts
						}
						delay := time.Duration(10<<exp) * time.Second
						if delay > 5*time.Minute {
							delay = 5 * time.Minute
						}
						pr.backoff = time.Now().Add(delay)
						fmt.Fprintf(stderr, "gc supervisor: city '%s' panic #%d, next retry in %s\n", n, pr.count, delay) //nolint:errcheck
						delete(cities, p)
					})
				} else {
					// Normal exit (context canceled) — reset panic counter
					// and remove from map in a single critical section.
					cr.BatchUpdate(func(
						cities map[string]*managedCity,
						_ map[string]cityInitProgress,
						_ map[string]*initFailRecord,
						panicHistory map[string]*panicRecord,
					) {
						delete(panicHistory, p)
						delete(cities, p)
					})
				}
				// Signal completion last — ensures all cleanup is done before
				// waiters (shutdown/unregister paths) proceed.
				close(done)
			}()
			// Resource cleanup defers pushed AFTER recovery/done so they
			// execute BEFORE it in LIFO order: resources are released,
			// then done is closed.
			defer lk.Close()                 //nolint:errcheck // release controller lock (last released)
			defer convergence.RemoveToken(p) //nolint:errcheck // best-effort cleanup
			defer func() {
				// Ownership-safe socket removal: only unlink if the
				// on-disk file is the same one we created, so a
				// replacement city's socket is never destroyed.
				if origSockInfo != nil {
					if cur, err := os.Stat(sock); err == nil {
						if os.SameFile(origSockInfo, cur) {
							os.Remove(sock) //nolint:errcheck
						}
					}
				}
			}()
			defer l.Close() //nolint:errcheck // close listener (after socket removal)
			defer telemetry.RecordControllerLifecycle(context.Background(), "stopped")
			cityRuntime.run(cityCtx)
		}(cityName, path, fr, lis, sockPath, sockInfo, lock)

		rec.Record(events.Event{Type: events.ControllerStarted, Actor: "gc"})
		telemetry.RecordControllerLifecycle(context.Background(), "started")
		fmt.Fprintf(stdout, "Launching city '%s' (%s)\n", cityName, path) //nolint:errcheck
	}

	// Reconcile the global rig index from all registered cities.
	reconcileRigIndex(reg, stderr)
}

// reconcileRigIndex rebuilds the [[rigs]] section of cities.toml from the
// rig definitions in each registered city's city.toml.
func reconcileRigIndex(reg *supervisor.Registry, stderr io.Writer) {
	cities, err := reg.List()
	if err != nil {
		return
	}

	var mappings []supervisor.RigCityMapping
	var loadFailed bool
	for _, c := range cities {
		cfg, err := loadCityConfig(c.Path)
		if err != nil {
			// Abort reconciliation if any city can't be loaded — a partial
			// snapshot would cause ReconcileRigs to drop rigs from the
			// errored city.
			fmt.Fprintf(stderr, "gc supervisor: skipping rig reconcile: city %q config error: %v\n", c.EffectiveName(), err) //nolint:errcheck
			loadFailed = true
			break
		}
		for _, rig := range cfg.Rigs {
			rigPath := rig.Path
			if !filepath.IsAbs(rigPath) {
				rigPath = filepath.Join(c.Path, rigPath)
			}
			rigPath = filepath.Clean(rigPath)
			mappings = append(mappings, supervisor.RigCityMapping{
				RigPath:  rigPath,
				RigName:  rig.Name,
				CityPath: c.Path,
			})
		}
	}

	if loadFailed {
		return
	}
	if err := reg.ReconcileRigs(mappings); err != nil {
		fmt.Fprintf(stderr, "gc supervisor: reconciling rig index: %v\n", err) //nolint:errcheck
	}
}

func publishManagedCity(cr *cityRegistry, path string, mc *managedCity) bool {
	var alreadyRunning bool
	cr.BatchUpdate(func(
		cities map[string]*managedCity,
		initStatus map[string]cityInitProgress,
		initFailures map[string]*initFailRecord,
		_ map[string]*panicRecord,
	) {
		// Re-check: another goroutine might have added this city while we
		// were initializing outside the lock.
		if _, running := cities[path]; running {
			alreadyRunning = true
			return
		}
		// The controller state and per-city API are fully wired at this point.
		// Mark the city started before the first reconcile so slow bead scans
		// don't keep supervisor startup and API availability blocked.
		mc.started = true
		mc.status = "starting_agents"
		cities[path] = mc
		delete(initStatus, path)
		delete(initFailures, path) // clear backoff on successful init
	})
	return alreadyRunning
}

// prepareCityForSupervisor runs the critical city initialization steps
// that cmd_start.go performs before runController. Without these, cities
// would have no formulas, no bead stores, and no resolved rig paths.
func prepareCityForSupervisor(cityPath, cityName string, cfg *config.City, stderr io.Writer, progress func(string)) error {
	runStep := func(status string, fn func() error) error {
		if progress != nil && status != "" {
			progress(status)
		}
		started := time.Now()
		err := fn()
		if dur := time.Since(started); dur > time.Second {
			fmt.Fprintf(stderr, "gc supervisor: city '%s': %s took %s\n", cityName, status, dur.Round(10*time.Millisecond)) //nolint:errcheck
		}
		return err
	}

	// Validate rigs.
	if err := config.ValidateRigs(cfg.Rigs, config.EffectiveHQPrefix(cfg)); err != nil {
		return fmt.Errorf("validate rigs: %w", err)
	}
	if err := config.ValidateServices(cfg.Services); err != nil {
		return fmt.Errorf("validate services: %w", err)
	}
	if err := workspacesvc.ValidateRuntimeSupport(cfg.Services); err != nil {
		return fmt.Errorf("validate services: %w", err)
	}

	// Materialize the gc-beads-bd script.
	if _, err := MaterializeBeadsBdScript(cityPath); err != nil {
		fmt.Fprintf(stderr, "gc supervisor: city '%s': materializing gc-beads-bd: %v\n", cityName, err) //nolint:errcheck
		// Non-fatal.
	}

	// Materialize builtin packs (system packs are auto-included via LoadWithIncludes).
	if err := MaterializeBuiltinPacks(cityPath); err != nil {
		fmt.Fprintf(stderr, "gc supervisor: city '%s': builtin packs: %v\n", cityName, err) //nolint:errcheck
		// Non-fatal.
	}

	// Materialize builtin prompts and formulas.
	if err := materializeBuiltinPrompts(cityPath); err != nil {
		fmt.Fprintf(stderr, "gc supervisor: city '%s': builtin prompts: %v\n", cityName, err) //nolint:errcheck
	}
	if err := materializeBuiltinFormulas(cityPath); err != nil {
		fmt.Fprintf(stderr, "gc supervisor: city '%s': builtin formulas: %v\n", cityName, err) //nolint:errcheck
	}
	ensureInitArtifacts(cityPath, cfg, stderr, "gc supervisor")

	// Resolve rig paths and start bead store lifecycle.
	resolveRigPaths(cityPath, cfg.Rigs)
	if err := runStep("starting_bead_store", func() error {
		return startBeadsLifecycle(cityPath, cityName, cfg, stderr)
	}); err != nil {
		return fmt.Errorf("beads lifecycle: %w", err)
	}

	// Post-startup bead provider health check.
	if err := runStep("checking_bead_store_health", func() error {
		return healthBeadsProvider(cityPath)
	}); err != nil {
		fmt.Fprintf(stderr, "gc supervisor: city '%s': beads health: %v\n", cityName, err) //nolint:errcheck
		// Non-fatal.
	}

	// Materialize system formulas into the city formulas/ directory.
	if err := runStep("materializing_system_formulas", func() error {
		_, sysErr := MaterializeSystemFormulas(systemFormulasFS, "system_formulas", cityPath)
		return sysErr
	}); err != nil {
		fmt.Fprintf(stderr, "gc supervisor: city '%s': system formulas: %v\n", cityName, err) //nolint:errcheck
	}

	// Resolve formula symlinks.
	if progress != nil {
		progress("resolving_formulas")
	}
	if len(cfg.FormulaLayers.City) > 0 {
		if err := runStep("resolving_city_formulas", func() error {
			return ResolveFormulas(cityPath, cfg.FormulaLayers.City)
		}); err != nil {
			fmt.Fprintf(stderr, "gc supervisor: city '%s': city formulas: %v\n", cityName, err) //nolint:errcheck
		}
	}
	for _, r := range cfg.Rigs {
		layers, ok := cfg.FormulaLayers.Rigs[r.Name]
		if !ok || len(layers) == 0 {
			// Rigs without explicit formula layers inherit city formulas
			// so pool agents can use default sling formulas (mol-do-work).
			layers = cfg.FormulaLayers.City
		}
		if len(layers) > 0 {
			status := fmt.Sprintf("resolving_rig_formulas:%s", r.Name)
			if err := runStep(status, func() error {
				return ResolveFormulas(r.Path, layers)
			}); err != nil {
				fmt.Fprintf(stderr, "gc supervisor: city '%s': rig %q formulas: %v\n", cityName, r.Name, err) //nolint:errcheck
			}
		}
	}

	// Materialize Claude skill stubs.
	if cfg.Workspace.Provider == "claude" {
		dirs := []string{cityPath}
		for _, r := range cfg.Rigs {
			if r.Path != "" {
				dirs = append(dirs, r.Path)
			}
		}
		if err := runStep("materializing_skill_stubs", func() error {
			return materializeSkillStubs(dirs...)
		}); err != nil {
			fmt.Fprintf(stderr, "gc supervisor: city '%s': skill stubs: %v\n", cityName, err) //nolint:errcheck
		}
	}

	// Validate agents.
	if err := runStep("validating_agents", func() error {
		return config.ValidateAgents(cfg.Agents)
	}); err != nil {
		return fmt.Errorf("validate agents: %w", err)
	}

	// Validate install_agent_hooks (workspace + all agents).
	if err := runStep("validating_hooks", func() error {
		if ih := cfg.Workspace.InstallAgentHooks; len(ih) > 0 {
			if err := hooks.Validate(ih); err != nil {
				return fmt.Errorf("workspace hooks: %w", err)
			}
		}
		for _, a := range cfg.Agents {
			if len(a.InstallAgentHooks) > 0 {
				if err := hooks.Validate(a.InstallAgentHooks); err != nil {
					return fmt.Errorf("agent %q hooks: %w", a.QualifiedName(), err)
				}
			}
		}
		return nil
	}); err != nil {
		return err
	}

	return nil
}

// effectiveProviderName returns the provider name respecting GC_SESSION env override.
func effectiveProviderName(configured string) string {
	if v := os.Getenv("GC_SESSION"); v != "" {
		return v
	}
	return configured
}

// supervisorBuildAgentsFn returns a buildFn suitable for CityRuntimeParams.
// It delegates to buildDesiredState with a stable beacon timestamp.
func supervisorBuildAgentsFn(cityPath, cityName string, stderr io.Writer) func(*config.City, runtime.Provider, beads.Store) DesiredStateResult {
	beaconTime := time.Now()
	return func(c *config.City, sp runtime.Provider, store beads.Store) DesiredStateResult {
		return buildDesiredState(cityName, cityPath, beaconTime, c, sp, store, stderr)
	}
}

func supervisorBuildAgentsFnWithSessionBeads(cityPath, cityName string, stderr io.Writer) func(*config.City, runtime.Provider, beads.Store, map[string]beads.Store, *sessionBeadSnapshot, *sessionReconcilerTraceCycle) DesiredStateResult {
	beaconTime := time.Now()
	return func(c *config.City, sp runtime.Provider, store beads.Store, rigStores map[string]beads.Store, sessionBeads *sessionBeadSnapshot, trace *sessionReconcilerTraceCycle) DesiredStateResult {
		return buildDesiredStateWithSessionBeads(cityName, cityPath, beaconTime, c, sp, store, rigStores, sessionBeads, trace, stderr)
	}
}

// cityInitProgress tracks the initialization status of a city that is
// being prepared but has not yet been inserted into the cities map.
type cityInitProgress struct {
	name   string
	status string
}

// Compile-time check that *cityRegistry satisfies api.CityResolver.
var _ api.CityResolver = (*cityRegistry)(nil)
