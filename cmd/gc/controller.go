package main

import (
	"bufio"
	"context"
	"crypto/sha256"
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
	"sync"
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

var (
	errControllerAlreadyRunning = errors.New("controller already running")
	errControllerUnavailable    = errors.New("controller unavailable")
	errControllerUnresponsive   = errors.New("controller unresponsive")
)

type controllerCommandError struct {
	op           string
	err          error
	unavailable  bool
	unresponsive bool
}

func (e controllerCommandError) Error() string {
	if e.op == "" {
		if e.err == nil {
			return "controller command failed"
		}
		return e.err.Error()
	}
	if e.err == nil {
		return e.op
	}
	return fmt.Sprintf("%s: %v", e.op, e.err)
}

func (e controllerCommandError) Unwrap() error {
	return e.err
}

func (e controllerCommandError) Is(target error) bool {
	return (target == errControllerUnavailable && e.unavailable) ||
		(target == errControllerUnresponsive && e.unresponsive)
}

const controllerSocketPathLimit = 100

// controllerSocketPath returns the Unix socket path for controller commands.
// It preserves the legacy .gc/controller.sock location for short city paths,
// but falls back to a deterministic short temp-path when the legacy pathname
// is too close to the platform Unix-socket length limit.
func controllerSocketPath(cityPath string) string {
	canonicalCityPath := normalizePathForCompare(cityPath)
	legacy := filepath.Join(cityPath, ".gc", "controller.sock")
	canonicalLegacy := filepath.Join(canonicalCityPath, ".gc", "controller.sock")
	if len(canonicalLegacy) <= controllerSocketPathLimit {
		return legacy
	}
	sum := sha256.Sum256([]byte(canonicalCityPath))
	return filepath.Join("/tmp", "gascity-controller", fmt.Sprintf("%x.sock", sum[:16]))
}

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
	dirty *atomic.Bool,
	reloadReqCh chan reloadRequest,
	convergenceReqCh chan convergenceRequest,
	pokeCh chan struct{},
	controlDispatcherCh chan struct{},
) (net.Listener, error) {
	sockPath := controllerSocketPath(cityPath)
	if err := os.MkdirAll(filepath.Dir(sockPath), 0o700); err != nil {
		return nil, fmt.Errorf("creating controller socket dir: %w", err)
	}
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
			go handleControllerConn(conn, cityPath, cancelFn, dirty, reloadReqCh, convergenceReqCh, pokeCh, controlDispatcherCh)
		}
	}()
	return lis, nil
}

// handleControllerConn reads from a connection and dispatches commands.
// Supported commands: "stop" (shutdown), "ping" (liveness check, returns PID),
// "converge:{json}" (convergence commands routed to event loop).
func handleControllerConn(
	conn net.Conn,
	cityPath string,
	cancelFn context.CancelFunc,
	dirty *atomic.Bool,
	reloadReqCh chan reloadRequest,
	convergenceReqCh chan convergenceRequest,
	pokeCh chan struct{},
	controlDispatcherCh chan struct{},
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
		case line == "reload":
			if dirty != nil {
				dirty.Store(true)
			}
			select {
			case pokeCh <- struct{}{}:
			default:
			}
			conn.Write([]byte("ok\n")) //nolint:errcheck // best-effort ack
		case strings.HasPrefix(line, "reload:"):
			handleReloadSocketCmd(conn, line[len("reload:"):], reloadReqCh)
		case line == "control-dispatcher":
			select {
			case controlDispatcherCh <- struct{}{}:
			default:
			}
			conn.Write([]byte("ok\n")) //nolint:errcheck // best-effort ack
		case strings.HasPrefix(line, "converge:"):
			handleConvergeSocketCmd(conn, line[len("converge:"):], convergenceReqCh)
		case strings.HasPrefix(line, "trace-arm:"):
			if handleTraceSocketCmd(conn, cityPath, "start", line[len("trace-arm:"):]) {
				select {
				case pokeCh <- struct{}{}:
				default:
				}
			}
		case strings.HasPrefix(line, "trace-stop:"):
			if handleTraceSocketCmd(conn, cityPath, "stop", line[len("trace-stop:"):]) {
				select {
				case pokeCh <- struct{}{}:
				default:
				}
			}
		case line == "trace-status":
			handleTraceStatusSocketCmd(conn, cityPath)
		}
	}
}

func handleReloadSocketCmd(conn net.Conn, payload string, ch chan reloadRequest) {
	if ch == nil {
		writeJSONLine(conn, reloadControlReply{
			Outcome: reloadOutcomeFailed,
			Error:   "reload control unavailable",
		})
		return
	}

	var wire reloadControlRequest
	if err := json.Unmarshal([]byte(payload), &wire); err != nil {
		writeJSONLine(conn, reloadControlReply{
			Outcome: reloadOutcomeFailed,
			Error:   fmt.Sprintf("invalid reload request: %v", err),
		})
		return
	}

	var timeout time.Duration
	if wire.Wait {
		if wire.Timeout != "" {
			parsed, err := time.ParseDuration(wire.Timeout)
			if err != nil {
				writeJSONLine(conn, reloadControlReply{
					Outcome: reloadOutcomeFailed,
					Error:   fmt.Sprintf("invalid reload timeout %q: %v", wire.Timeout, err),
				})
				return
			}
			timeout = parsed
		}
		if timeout <= 0 {
			writeJSONLine(conn, reloadControlReply{
				Outcome: reloadOutcomeFailed,
				Error:   "reload timeout must be greater than 0",
			})
			return
		}
	}

	totalDeadline := 2*controllerReloadAcceptTimeout + 5*time.Second
	if wire.Wait {
		totalDeadline += timeout
	}
	conn.SetDeadline(time.Now().Add(totalDeadline)) //nolint:errcheck // command-specific override

	req := reloadRequest{
		wait:       wire.Wait,
		timeout:    timeout,
		acceptedCh: make(chan reloadControlReply, 1),
		doneCh:     make(chan reloadControlReply, 1),
	}

	deadline := time.Now().Add(controllerReloadAcceptTimeout)
	remaining := func() time.Duration {
		d := time.Until(deadline)
		if d < 0 {
			return 0
		}
		return d
	}

	waitFor := func(ch <-chan reloadControlReply, timeout time.Duration) (reloadControlReply, bool) {
		if timeout <= 0 {
			return reloadControlReply{}, false
		}
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		select {
		case reply := <-ch:
			return reply, true
		case <-timer.C:
			return reloadControlReply{}, false
		}
	}

	acceptTimeout := remaining()
	if acceptTimeout <= 0 {
		writeJSONLine(conn, reloadControlReply{
			Outcome: reloadOutcomeBusy,
			Message: "Reload request could not be accepted because the controller is busy.",
		})
		return
	}
	timer := time.NewTimer(acceptTimeout)
	defer timer.Stop()
	select {
	case ch <- req:
	case <-timer.C:
		writeJSONLine(conn, reloadControlReply{
			Outcome: reloadOutcomeBusy,
			Message: "Reload request could not be accepted because the controller is busy.",
		})
		return
	}

	reply, ok := waitFor(req.acceptedCh, controllerReloadAcceptTimeout)
	if !ok {
		writeJSONLine(conn, reloadControlReply{
			Outcome: reloadOutcomeFailed,
			Message: "Reload request was handed to the controller but was not acknowledged in time.",
		})
		return
	}
	if reply.Outcome != reloadOutcomeAccepted {
		writeJSONLine(conn, reply)
		return
	}
	if !req.wait {
		writeJSONLine(conn, reply)
		return
	}

	finalReply, ok := waitFor(req.doneCh, req.timeout)
	if !ok {
		writeJSONLine(conn, reloadControlReply{
			Outcome: reloadOutcomeTimeout,
			Message: "Reload did not finish before timeout; it may still complete later.",
		})
		return
	}
	writeJSONLine(conn, finalReply)
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
	return sendControllerCommandWithReadTimeout(cityPath, command, 95*time.Second)
}

func sendControllerCommandWithReadTimeout(cityPath, command string, readTimeout time.Duration) ([]byte, error) {
	sockPath := controllerSocketPath(cityPath)
	conn, err := net.DialTimeout("unix", sockPath, 2*time.Second)
	if err != nil {
		return nil, controllerCommandError{
			op:          "connecting to controller",
			err:         fmt.Errorf("%w (is the controller running?)", err),
			unavailable: true,
		}
	}
	defer conn.Close()                                     //nolint:errcheck
	conn.SetWriteDeadline(time.Now().Add(5 * time.Second)) //nolint:errcheck
	conn.SetReadDeadline(time.Now().Add(readTimeout))      //nolint:errcheck
	if _, err := conn.Write([]byte(command + "\n")); err != nil {
		return nil, fmt.Errorf("sending command: %w", err)
	}
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return nil, controllerCommandError{
				op:           "reading response",
				err:          err,
				unresponsive: true,
			}
		}
		return nil, controllerCommandError{
			op:           "reading response",
			err:          io.ErrUnexpectedEOF,
			unresponsive: true,
		}
	}
	return []byte(strings.TrimSpace(scanner.Text())), nil
}

// controllerAlive checks whether a controller is running by connecting
// to the controller.sock and sending a "ping". Returns the PID if alive,
// or 0 if not reachable.
func controllerAlive(cityPath string) int {
	sockPath := controllerSocketPath(cityPath)
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

// watchConfigTargets starts an fsnotify watcher on the given config paths and
// sets dirty to true after a debounce window. Config source directories are
// watched shallowly to handle vim/emacs rename-swap atomic saves; pack and
// convention roots are watched recursively because fsnotify is non-recursive.
// Returns a cleanup function. If the watcher cannot be created, returns a
// no-op cleanup (degraded to tick-only, no file watching).
type configWatchRegistrar struct {
	watcher        *fsnotify.Watcher
	stderr         io.Writer
	mu             sync.Mutex
	recursiveRoots map[string]struct{}
	discoveryRoots map[string]struct{}
}

func newConfigWatchRegistrar(watcher *fsnotify.Watcher, stderr io.Writer) *configWatchRegistrar {
	return &configWatchRegistrar{
		watcher:        watcher,
		stderr:         stderr,
		recursiveRoots: make(map[string]struct{}),
		discoveryRoots: make(map[string]struct{}),
	}
}

func (r *configWatchRegistrar) addPath(root string, recursive bool, done <-chan struct{}) bool {
	select {
	case <-done:
		return false
	default:
	}
	info, err := os.Stat(root)
	if err != nil {
		fmt.Fprintf(r.stderr, "config watcher: cannot stat %s: %v\n", root, err) //nolint:errcheck // best-effort stderr
		return false
	}
	if !r.addOne(root, done) {
		return false
	}
	if !info.IsDir() {
		return true
	}
	if !recursive {
		return true
	}
	walkRoot := root
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		walkRoot = resolved
	}
	walkErr := filepath.WalkDir(walkRoot, func(path string, d os.DirEntry, walkErr error) error {
		select {
		case <-done:
			return filepath.SkipAll
		default:
		}
		if walkErr != nil {
			fmt.Fprintf(r.stderr, "config watcher: walk %s: %v\n", path, walkErr) //nolint:errcheck // best-effort stderr
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		if samePath(path, root) {
			return nil
		}
		if path != root && shouldIgnoreConfigWatchEvent(path) {
			return filepath.SkipDir
		}
		r.addOne(path, done)
		return nil
	})
	if walkErr != nil {
		fmt.Fprintf(r.stderr, "config watcher: walk %s: %v\n", walkRoot, walkErr) //nolint:errcheck // best-effort stderr
	}
	return true
}

func (r *configWatchRegistrar) addOne(path string, done <-chan struct{}) bool {
	select {
	case <-done:
		return false
	default:
	}
	if err := r.watcher.Add(path); err != nil {
		if errors.Is(err, syscall.ENOSPC) {
			fmt.Fprintf(r.stderr, "config watcher: cannot watch %s: inotify watch limit reached; increase fs.inotify.max_user_watches or reduce watched pack size: %v\n", path, err) //nolint:errcheck // best-effort stderr
			return false
		}
		fmt.Fprintf(r.stderr, "config watcher: cannot watch %s: %v\n", path, err) //nolint:errcheck // best-effort stderr
		return false
	}
	return true
}

func (r *configWatchRegistrar) markRecursiveRoot(root string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.recursiveRoots[normalizePathForCompare(root)] = struct{}{}
}

func (r *configWatchRegistrar) unmarkRecursiveRoot(root string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.recursiveRoots, normalizePathForCompare(root))
}

func (r *configWatchRegistrar) markDiscoveryRoot(root string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.discoveryRoots[normalizePathForCompare(root)] = struct{}{}
}

func (r *configWatchRegistrar) watchesRecursively(path string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for root := range r.recursiveRoots {
		if pathIsWithin(root, path) {
			return true
		}
	}
	return false
}

func (r *configWatchRegistrar) isConventionRootCreate(path string) bool {
	parent := filepath.Dir(path)
	base := filepath.Base(filepath.Clean(path))
	if !isConventionDiscoveryDirName(base) {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for root := range r.discoveryRoots {
		if samePath(root, parent) {
			return true
		}
	}
	return false
}

func pathIsWithin(root, path string) bool {
	root = normalizePathForCompare(root)
	path = normalizePathForCompare(path)
	if samePath(root, path) {
		return true
	}
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	if err != nil {
		return false
	}
	return rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func isConventionDiscoveryDirName(base string) bool {
	for _, name := range config.ConventionDiscoveryDirNames() {
		if base == name {
			return true
		}
	}
	return false
}

func watchConfigTargets(targets []config.WatchTarget, dirty *atomic.Bool, pokeCh chan struct{}, stderr io.Writer) func() {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		fmt.Fprintf(stderr, "gc start: config watcher: %v (reload on tick only)\n", err) //nolint:errcheck // best-effort stderr
		return func() {}
	}
	registrar := newConfigWatchRegistrar(watcher, stderr)

	markDirty := func() {
		dirty.Store(true)
		if pokeCh != nil {
			select {
			case pokeCh <- struct{}{}:
			default:
			}
		}
	}

	done := make(chan struct{})
	eventLoopDone := make(chan struct{})
	var registrationWG sync.WaitGroup
	var enqueueMu sync.Mutex
	enqueueRecursiveWatch := func(root string, trackRoot bool) {
		enqueueMu.Lock()
		select {
		case <-done:
			enqueueMu.Unlock()
			return
		default:
		}
		if trackRoot {
			registrar.markRecursiveRoot(root)
		}
		registrationWG.Add(1)
		enqueueMu.Unlock()
		go func() {
			defer registrationWG.Done()
			if ok := registrar.addPath(root, true, done); !ok && trackRoot {
				registrar.unmarkRecursiveRoot(root)
			}
		}()
	}

	// fsnotify is non-recursive. Watch config source directories shallowly,
	// but recurse through pack and convention roots where config-bearing
	// files live below pre-existing subdirectories. Regression guard:
	// gastownhall/gascity#780.
	for _, target := range targets {
		if target.DiscoverConventions {
			registrar.markDiscoveryRoot(target.Path)
		}
		if target.Recursive {
			registrar.markRecursiveRoot(target.Path)
		}
		if ok := registrar.addPath(target.Path, target.Recursive, done); !ok && target.Recursive {
			registrar.unmarkRecursiveRoot(target.Path)
		}
	}
	go func() {
		defer close(eventLoopDone)
		var debounce *time.Timer
		defer func() {
			if debounce != nil {
				debounce.Stop()
			}
		}()
		for {
			select {
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}
				if shouldIgnoreConfigWatchEvent(event.Name) {
					continue
				}
				if event.Op&(fsnotify.Create|fsnotify.Rename) != 0 {
					if info, err := os.Stat(event.Name); err == nil && info.IsDir() {
						// Convention roots may appear after startup, and nested
						// dirs inside recursive roots may be pre-populated. Queue
						// the walk so fsnotify consumption stays fast.
						if registrar.isConventionRootCreate(event.Name) {
							enqueueRecursiveWatch(event.Name, true)
						} else if registrar.watchesRecursively(event.Name) {
							enqueueRecursiveWatch(event.Name, false)
						}
					}
				}
				// Debounce: reset timer on each event, fire after quiet period.
				if debounce != nil {
					debounce.Stop()
				}
				debounce = time.AfterFunc(debounceDelay, func() {
					markDirty()
				})
			case _, ok := <-watcher.Errors:
				if !ok {
					return
				}
			}
		}
	}()
	var cleanupOnce sync.Once
	return func() {
		cleanupOnce.Do(func() {
			enqueueMu.Lock()
			close(done)
			enqueueMu.Unlock()
			registrationWG.Wait()
			watcher.Close() //nolint:errcheck // best-effort cleanup
			<-eventLoopDone
		})
	}
}

func shouldIgnoreConfigWatchEvent(path string) bool {
	clean := filepath.Clean(path)
	if clean == "" || clean == "." {
		return false
	}
	sepGC := string(filepath.Separator) + ".gc"
	sepBeads := string(filepath.Separator) + ".beads"
	return clean == ".gc" ||
		clean == ".beads" ||
		strings.HasSuffix(clean, sepGC) ||
		strings.HasSuffix(clean, sepBeads) ||
		strings.Contains(clean, sepGC+string(filepath.Separator)) ||
		strings.Contains(clean, sepBeads+string(filepath.Separator))
}

// reloadResult holds the result of a config reload attempt.
type reloadResult struct {
	Cfg      *config.City
	Prov     *config.Provenance
	Revision string
	Warnings []string
}

type reloadWarningError struct {
	err      error
	warnings []string
}

func (e reloadWarningError) Error() string {
	return e.err.Error()
}

func (e reloadWarningError) Unwrap() error {
	return e.err
}

func (e reloadWarningError) ReloadWarnings() []string {
	return append([]string(nil), e.warnings...)
}

type reloadWarningCarrier interface {
	ReloadWarnings() []string
}

const reloadStrictWarningHint = "use --no-strict to disable strict checking"

func reloadWarningsFromError(err error) []string {
	var carrier reloadWarningCarrier
	if errors.As(err, &carrier) {
		return carrier.ReloadWarnings()
	}
	return nil
}

// tryReloadConfig attempts to reload city.toml with includes and patches.
// Returns the new config, provenance, revision, and non-fatal warnings on
// success, or an error on failure (parse error, validation error, workspace
// name changed). Some failures after composition also return warning metadata
// via the result and error; callers should report those details while keeping
// the old config. Strict mode (default) makes composition warnings fatal.
func tryReloadConfig(tomlPath, lockedWorkspaceName, cityRoot string) (*reloadResult, error) {
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
	reloadWarnings := append([]string(nil), prov.Warnings...)
	resultWithWarnings := func(warnings []string) *reloadResult {
		return &reloadResult{
			Cfg:      newCfg,
			Prov:     prov,
			Warnings: append([]string(nil), warnings...),
		}
	}
	failWithWarnings := func(err error) (*reloadResult, error) {
		if len(reloadWarnings) == 0 {
			return nil, err
		}
		return resultWithWarnings(reloadWarnings), reloadWarningError{
			err:      err,
			warnings: reloadWarnings,
		}
	}
	if strictMode && len(prov.Warnings) > 0 {
		warnings := append(append([]string(nil), reloadWarnings...), reloadStrictWarningHint)
		result := resultWithWarnings(warnings)
		return result, reloadWarningError{
			err:      fmt.Errorf("strict mode: %d collision warning(s)", len(prov.Warnings)),
			warnings: warnings,
		}
	}
	if err := config.ValidateAgents(newCfg.Agents); err != nil {
		return failWithWarnings(fmt.Errorf("validating agents: %w", err))
	}
	if err := config.ValidateServices(newCfg.Services); err != nil {
		return failWithWarnings(fmt.Errorf("validating services: %w", err))
	}
	if err := workspacesvc.ValidateRuntimeSupport(newCfg.Services); err != nil {
		return failWithWarnings(fmt.Errorf("validating services: %w", err))
	}
	newName := newCfg.Workspace.Name
	if newName == "" {
		newName = filepath.Base(filepath.Dir(tomlPath))
	}
	if newName != lockedWorkspaceName {
		return failWithWarnings(fmt.Errorf("workspace.name changed from %q to %q (restart controller to apply)", lockedWorkspaceName, newName))
	}
	rev := config.Revision(fsys.OSFS{}, prov, newCfg, cityRoot)
	return &reloadResult{Cfg: newCfg, Prov: prov, Revision: rev, Warnings: reloadWarnings}, nil
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
		stopTargetsBounded(stopTargetsForNames(names, cfg, store, stderr), cfg, store, sp, rec, "gc", stdout, stderr)
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
	sent := interruptTargetsBounded(targets, cfg, store, sp, stderr)
	fmt.Fprintf(stdout, "Sent interrupt to %d/%d agent(s), waiting %s...\n", //nolint:errcheck // best-effort stdout
		sent, len(names), timeout)

	// Poll until all agents exit or timeout expires (avoid sleeping full duration).
	pollInterval := 500 * time.Millisecond
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		allExited := true
		if runningSet, ok := runningSessionSet(sp, names); ok {
			allExited = len(runningSet) == 0
		} else {
			for _, name := range names {
				running, err := workerSessionTargetRunningWithConfig("", nil, sp, nil, name)
				if err == nil && running {
					allExited = false
					break
				}
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
	runningSet, listed := runningSessionSet(sp, names)
	for _, name := range names {
		running := false
		if listed {
			running = runningSet[name]
		} else {
			running, _ = workerSessionTargetRunningWithConfig("", nil, sp, nil, name)
		}
		if !running {
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
	stopTargetsBounded(filterStopTargets(targets, survivors), cfg, store, sp, rec, "gc", stdout, stderr)
}

func runningSessionSet(sp runtime.Provider, names []string) (map[string]bool, bool) {
	running, err := sp.ListRunning("")
	if err != nil {
		return nil, false
	}
	if len(names) == 0 {
		return map[string]bool{}, true
	}
	wanted := make(map[string]bool, len(names))
	for _, name := range names {
		wanted[name] = true
	}
	result := make(map[string]bool, len(names))
	for _, name := range running {
		if wanted[name] {
			result[name] = true
		}
	}
	return result, true
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
	watchTargets []config.WatchTarget,
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
		cityPath:            cityPath,
		cityName:            cityName,
		tomlPath:            tomlPath,
		watchTargets:        watchTargets,
		cfg:                 loopCfg,
		sp:                  sp,
		buildFn:             buildFn,
		dops:                dops,
		ct:                  ct,
		it:                  it,
		wg:                  wg,
		od:                  od,
		rec:                 rec,
		cs:                  cs,
		poolSessions:        poolSessions,
		poolDeathHandlers:   poolDeathHandlers,
		suspendedNames:      suspendedNames,
		pokeCh:              make(chan struct{}, 1),
		controlDispatcherCh: make(chan struct{}, 1),
		logPrefix:           "gc start",
		stdout:              stdout,
		stderr:              stderr,
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
// stops all agents. Returns an exit code. initialWatchTargets is the set of
// paths to watch for config changes (from initial provenance).
func runController(
	cityPath string,
	tomlPath string,
	cfg *config.City,
	configRev string,
	buildFn func(*config.City, runtime.Provider, beads.Store) DesiredStateResult,
	buildFnWithSessionBeads func(*config.City, runtime.Provider, beads.Store, map[string]beads.Store, *sessionBeadSnapshot, *sessionReconcilerTraceCycle) DesiredStateResult,
	sp runtime.Provider,
	dops drainOps,
	poolSessions map[string]time.Duration,
	poolDeathHandlers map[string]poolDeathInfo,
	initialWatchTargets []config.WatchTarget,
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
	reloadReqCh := make(chan reloadRequest)
	pokeCh := make(chan struct{}, 1)
	controlDispatcherCh := make(chan struct{}, 1)
	configDirty := &atomic.Bool{}

	sockPath := controllerSocketPath(cityPath)
	lis, err := startControllerSocket(cityPath, cancel, configDirty, reloadReqCh, convergenceReqCh, pokeCh, controlDispatcherCh)
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
		WatchTargets:            initialWatchTargets,
		ConfigRev:               configRev,
		ConfigDirty:             configDirty,
		Cfg:                     cfg,
		SP:                      sp,
		Publication:             supervisor.PublicationConfig{},
		BuildFn:                 buildFn,
		BuildFnWithSessionBeads: buildFnWithSessionBeads,
		Dops:                    dops,
		Rec:                     rec,
		PoolSessions:            poolSessions,
		PoolDeathHandlers:       poolDeathHandlers,
		ReloadReqCh:             reloadReqCh,
		ConvergenceReqCh:        convergenceReqCh,
		PokeCh:                  pokeCh,
		ControlDispatcherCh:     controlDispatcherCh,
		Stdout:                  stdout,
		Stderr:                  stderr,
	})

	// Install controller-managed bead stores even when the HTTP API is
	// disabled. Standalone runtime still needs cached city/rig stores for
	// session-bead sync and rig-scoped wake decisions.
	cs := newControllerState(ctx, cfg, sp, eventProv, cityName, cityPath)
	cs.ct = cr.crashTrack()
	cs.pokeCh = pokeCh
	cs.services = cr.svc
	cs.startBeadEventWatcher(ctx)
	cr.setControllerState(cs)

	// Start API server if configured. Standalone city mode wraps the
	// single city in a SupervisorMux so every endpoint is served at its
	// real scoped path (/v0/city/{cityName}/...) — matching the
	// published OpenAPI contract. Clients should use NewCityScopedClient
	// with the city name (accessible via the supervisor's /v0/cities).
	if cfg.API.Port > 0 {
		bind := cfg.API.BindOrDefault()
		nonLocal := bind != "127.0.0.1" && bind != "localhost" && bind != "::1"
		readOnly := nonLocal && !cfg.API.AllowMutations
		if readOnly {
			fmt.Fprintf(stderr, "api: binding to %s — mutation endpoints disabled (non-localhost)\n", bind) //nolint:errcheck
		}
		apiMux := api.NewSupervisorMux(&singleCityStateResolver{state: cs}, readOnly, "controller", time.Now())
		addr := net.JoinHostPort(bind, strconv.Itoa(cfg.API.Port))
		apiLis, apiErr := net.Listen("tcp", addr)
		if apiErr != nil {
			fmt.Fprintf(stderr, "api: WARNING: listen %s failed: %v — continuing without API server\n", addr, apiErr) //nolint:errcheck // best-effort stderr
		} else {
			go func() {
				if err := apiMux.Serve(apiLis); err != nil && !errors.Is(err, http.ErrServerClosed) {
					fmt.Fprintf(stderr, "api: %v\n", err) //nolint:errcheck // best-effort stderr
				}
			}()
			defer func() {
				shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				apiMux.Shutdown(shutCtx) //nolint:errcheck // best-effort cleanup
			}()
			fmt.Fprintf(stdout, "API server listening on http://%s\n", addr) //nolint:errcheck // best-effort stdout
		}
	}

	runPoolOnBoot(cfg, cityPath, shellRunHook, stderr)
	cr.run(ctx)
	cr.shutdown()

	rec.Record(events.Event{Type: events.ControllerStopped, Actor: "gc"})
	telemetry.RecordControllerLifecycle(context.Background(), "stopped")
	fmt.Fprintln(stdout, "Controller stopped.") //nolint:errcheck // best-effort stdout
	return 0
}

// singleCityStateResolver adapts a single api.State into an api.CityResolver
// so the standalone `gc controller` mode can run its single city behind a
// SupervisorMux. The resulting HTTP surface matches supervisor-mode exactly:
// every per-city operation is served at /v0/city/{cityName}/... . No bare
// /v0/foo alias exists.
type singleCityStateResolver struct {
	state api.State
}

func (r *singleCityStateResolver) ListCities() []api.CityInfo {
	return []api.CityInfo{{
		Name:    r.state.CityName(),
		Path:    r.state.CityPath(),
		Running: true,
	}}
}

func (r *singleCityStateResolver) CityState(name string) api.State {
	if name == r.state.CityName() {
		return r.state
	}
	return nil
}
