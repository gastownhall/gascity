// Package subprocess implements [runtime.Provider] using child processes.
//
// Each session runs as a detached child process (via os/exec) with no
// terminal attached. This is the lightweight alternative to the tmux
// provider — useful for CI, testing, and environments where tmux is
// unavailable.
//
// Process tracking uses two layers:
//   - In-memory: for the same gc process (Start followed by Stop/IsRunning)
//   - Unix sockets: for cross-process persistence (gc start → gc stop).
//     Each session gets a per-session unix socket (<name>.sock) that serves
//     as both proof of liveness and control channel (stop/interrupt/ping).
//
// Limitations compared to tmux:
//   - No interactive attach (Attach always returns an error)
//   - No startup hint support (fire-and-forget only)
package subprocess

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// Provider manages agent sessions as child processes.
type Provider struct {
	mu       sync.Mutex
	dir      string                  // socket/meta file directory
	procs    map[string]*sessionConn // in-process tracking
	workDirs map[string]string       // session name → workDir (for CopyTo)
}

const socketPathLimit = 100

// sessionConn tracks a running child process and its control socket.
type sessionConn struct {
	cmd      *exec.Cmd
	done     chan struct{} // closed when process exits
	listener net.Listener  // unix socket listener
}

// Compile-time check.
var _ runtime.Provider = (*Provider)(nil)

// NewProvider returns a subprocess [Provider] that stores socket files in
// a default temporary directory. Suitable for production use.
func NewProvider() *Provider {
	dir := filepath.Join(os.TempDir(), "gc-subprocess")
	_ = os.MkdirAll(dir, 0o755)
	return &Provider{dir: dir, procs: make(map[string]*sessionConn), workDirs: make(map[string]string)}
}

// NewProviderWithDir returns a subprocess [Provider] that stores socket files
// in the given directory. Useful for tests that need isolated state.
func NewProviderWithDir(dir string) *Provider {
	_ = os.MkdirAll(dir, 0o755)
	return &Provider{dir: dir, procs: make(map[string]*sessionConn), workDirs: make(map[string]string)}
}

// Start spawns a child process for the given session name and config.
// Returns an error if a session with that name is already running.
// Startup hints (ReadyPromptPrefix, ProcessNames, etc.) are ignored —
// all sessions are fire-and-forget.
func (p *Provider) Start(_ context.Context, name string, cfg runtime.Config) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Check in-memory tracking first.
	if existing, ok := p.procs[name]; ok {
		if existing.alive() {
			return fmt.Errorf("%w: session %q", runtime.ErrSessionExists, name)
		}
		delete(p.procs, name)
	}

	// Check socket for cross-process case.
	if p.socketAlive(name) {
		return fmt.Errorf("%w: session %q", runtime.ErrSessionExists, name)
	}

	// Store workDir for CopyTo.
	if cfg.WorkDir != "" {
		p.workDirs[name] = cfg.WorkDir
	}
	clearWorkDir := func() {
		delete(p.workDirs, name)
	}

	if err := runtime.StageSessionWorkDir(cfg); err != nil {
		clearWorkDir()
		return fmt.Errorf("staging workdir for %q: %w", name, err)
	}

	command := cfg.Command
	if command == "" {
		command = "sh"
	}

	cmd := exec.Command("sh", "-c", command)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if cfg.WorkDir != "" {
		cmd.Dir = cfg.WorkDir
	}
	// Managed subprocess sessions are background workers. If stdout/stderr are
	// left nil, they inherit the caller's descriptors, which can keep parent
	// CombinedOutput pipes open long after the spawning gc command has returned.
	// Use /dev/null instead of io.Discard so exec doesn't create copy goroutines
	// that can block on grandchildren inheriting the pipe.
	nullFile, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		clearWorkDir()
		return fmt.Errorf("opening %s for %q: %w", os.DevNull, name, err)
	}
	cmd.Stdout = nullFile
	cmd.Stderr = nullFile

	// Build environment: inherit parent env + apply overrides.
	env := os.Environ()
	if len(cfg.Env) > 0 {
		keys := make([]string, 0, len(cfg.Env))
		for k := range cfg.Env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			env = append(env, k+"="+cfg.Env[k])
		}
	}
	cmd.Env = env

	if err := cmd.Start(); err != nil {
		_ = nullFile.Close()
		clearWorkDir()
		return fmt.Errorf("starting session %q: %w", name, err)
	}
	_ = nullFile.Close()

	// Create control socket for cross-process discovery.
	done := make(chan struct{})
	lis, err := p.startControlSocket(name, cmd, done)
	if err != nil {
		// Socket creation failed — kill the process and bail.
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		clearWorkDir()
		return fmt.Errorf("creating control socket for %q: %w", name, err)
	}
	if err := p.persistStartMetadata(name, cfg.Env); err != nil {
		lis.Close() //nolint:errcheck
		_ = os.Remove(p.sockPath(name))
		_ = os.Remove(p.sockNamePath(name))
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		clearWorkDir()
		return fmt.Errorf("storing metadata for %q: %w", name, err)
	}

	go func() {
		_ = cmd.Wait()
		// Clean up socket before signaling done so ListRunning
		// never sees a stale socket after Stop returns.
		lis.Close()                 //nolint:errcheck
		os.Remove(p.sockPath(name)) //nolint:errcheck
		_ = os.Remove(p.sockNamePath(name))
		p.clearSessionMeta(name)
		close(done)
	}()

	p.procs[name] = &sessionConn{cmd: cmd, done: done, listener: lis}
	return nil
}

// Stop terminates the named session. Returns nil if it doesn't exist
// (idempotent). Sends SIGTERM first, then SIGKILL after a grace period.
func (p *Provider) Stop(name string) error {
	p.mu.Lock()
	sc, ok := p.procs[name]
	if ok {
		delete(p.procs, name)
	}
	p.mu.Unlock()

	// Try in-memory process first.
	if ok {
		if !sc.alive() {
			return nil
		}
		return terminateSessionConn(sc)
	}

	// Fall back to socket (cross-process case: gc stop after gc start).
	return p.stopBySocket(name)
}

// Interrupt sends SIGINT to the named session's process.
// Best-effort: returns nil if the session doesn't exist.
func (p *Provider) Interrupt(name string) error {
	p.mu.Lock()
	sc, ok := p.procs[name]
	p.mu.Unlock()
	if ok {
		return runtime.SignalProcessGroup(sc.cmd, syscall.SIGINT)
	}

	// Fall back to socket (cross-process case).
	// Swallow connection errors — if the socket doesn't exist the session
	// is dead, which is the same as "interrupt succeeded" (idempotent).
	err := p.sendSocketCommand(name, "interrupt", 2*time.Second)
	if err != nil {
		return nil // session not running — best-effort
	}
	return nil
}

// IsRunning reports whether the named session has a live process.
func (p *Provider) IsRunning(name string) bool {
	p.mu.Lock()
	sc, ok := p.procs[name]
	p.mu.Unlock()

	if ok {
		return sc.alive()
	}

	// Fall back to socket liveness check.
	return p.socketAlive(name)
}

// IsAttached always returns false — subprocess has no terminal concept.
func (p *Provider) IsAttached(_ string) bool { return false }

// Attach is not supported by the subprocess provider.
func (p *Provider) Attach(_ string) error {
	return fmt.Errorf("subprocess provider does not support attach")
}

// ProcessAlive reports whether the named session is still running.
// The subprocess provider cannot inspect the process tree, so it
// delegates to IsRunning: if the session is alive, the agent process
// is assumed alive. Returns true when processNames is empty (per
// the Provider contract).
func (p *Provider) ProcessAlive(name string, processNames []string) bool {
	if len(processNames) == 0 {
		return true
	}
	return p.IsRunning(name)
}

// Nudge is not supported by the subprocess provider — there is no
// interactive terminal to send messages to. Returns nil (best-effort).
func (p *Provider) Nudge(_ string, _ []runtime.ContentBlock) error {
	return nil
}

// SendKeys is not supported by the subprocess provider — there is no
// interactive terminal to send keystrokes to. Returns nil (best-effort).
func (p *Provider) SendKeys(_ string, _ ...string) error {
	return nil
}

// RunLive is not supported by the subprocess provider. Returns nil.
func (p *Provider) RunLive(_ string, _ runtime.Config) error {
	return nil
}

// Peek is not supported by the subprocess provider — there is no
// terminal with scrollback to capture. Returns an empty string.
func (p *Provider) Peek(_ string, _ int) (string, error) {
	return "", nil
}

// SetMeta stores a key-value pair for the named session in a sidecar file.
func (p *Provider) SetMeta(name, key, value string) error {
	return os.WriteFile(p.metaPath(name, key), []byte(value), 0o644)
}

// GetMeta retrieves a metadata value from a sidecar file.
// Returns ("", nil) if the key is not set.
func (p *Provider) GetMeta(name, key string) (string, error) {
	data, err := os.ReadFile(p.metaPath(name, key))
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	return string(data), nil
}

// RemoveMeta removes a metadata sidecar file.
func (p *Provider) RemoveMeta(name, key string) error {
	err := os.Remove(p.metaPath(name, key))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func (p *Provider) persistStartMetadata(name string, env map[string]string) error {
	p.clearSessionMeta(name)
	for key, value := range env {
		if err := p.SetMeta(name, key, value); err != nil {
			p.clearSessionMeta(name)
			return err
		}
	}
	return nil
}

// GetLastActivity returns zero time — subprocess provider does not
// support activity tracking.
func (p *Provider) GetLastActivity(_ string) (time.Time, error) {
	return time.Time{}, nil
}

// ClearScrollback is a no-op for subprocess sessions (no scrollback buffer).
func (p *Provider) ClearScrollback(_ string) error {
	return nil
}

// CopyTo copies src into the named session's working directory at relDst.
// Best-effort: returns nil if session unknown or src missing.
func (p *Provider) CopyTo(name, src, relDst string) error {
	p.mu.Lock()
	wd := p.workDirs[name]
	p.mu.Unlock()
	if wd == "" {
		return nil
	}
	if _, err := os.Stat(src); err != nil {
		return nil
	}
	dst := wd
	if relDst != "" {
		dst = filepath.Join(wd, relDst)
	}
	return runtime.StagePath(src, dst)
}

// ListRunning returns the names of all running sessions whose names
// match the given prefix, discovered via socket files.
func (p *Provider) ListRunning(prefix string) ([]string, error) {
	dirs := []string{p.dir}
	if fallback := p.fallbackDir(); fallback != p.dir {
		dirs = append(dirs, fallback)
	}
	seen := make(map[string]bool)
	var names []string
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		for _, e := range entries {
			n := e.Name()
			if !strings.HasSuffix(n, ".sock") {
				continue
			}
			sn := p.socketNameForEntry(dir, strings.TrimSuffix(n, ".sock"))
			if !strings.HasPrefix(sn, prefix) || seen[sn] {
				continue
			}
			if p.socketAlive(sn) {
				seen[sn] = true
				names = append(names, sn)
			}
		}
	}
	return names, nil
}

func (p *Provider) metaPath(name, key string) string {
	return filepath.Join(p.dir, metaFilePrefix(name)+".meta."+metaFileKey(key))
}

func (p *Provider) clearSessionMeta(name string) {
	matches, err := filepath.Glob(filepath.Join(p.dir, metaFilePrefix(name)+".meta.*"))
	if err != nil {
		return
	}
	for _, path := range matches {
		_ = os.Remove(path)
	}
}

func metaFilePrefix(name string) string {
	return "m" + metaFileKey(name)
}

func metaFileKey(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

// --- Unix socket helpers ---

func (p *Provider) legacySockPath(name string) string {
	return filepath.Join(p.dir, name+".sock")
}

func (p *Provider) sockKey(name string) string {
	sum := sha256.Sum256([]byte(name))
	return "s" + hex.EncodeToString(sum[:4])
}

func (p *Provider) fallbackDir() string {
	sum := sha256.Sum256([]byte(filepath.Clean(p.dir)))
	return filepath.Join(os.TempDir(), "gascity-subprocess", hex.EncodeToString(sum[:8]))
}

func (p *Provider) socketDir() string {
	candidate := filepath.Join(p.dir, p.sockKey("probe")+".sock")
	if len(candidate) <= socketPathLimit {
		return p.dir
	}
	return p.fallbackDir()
}

func (p *Provider) sockPath(name string) string {
	return filepath.Join(p.socketDir(), p.sockKey(name)+".sock")
}

func (p *Provider) sockNamePath(name string) string {
	return filepath.Join(p.socketDir(), p.sockKey(name)+".name")
}

func (p *Provider) socketNameForEntry(dir, key string) string {
	data, err := os.ReadFile(filepath.Join(dir, key+".name"))
	if err != nil {
		return key
	}
	name := strings.TrimSpace(string(data))
	if name == "" {
		return key
	}
	return name
}

// startControlSocket creates a unix socket for the session and starts
// a goroutine to handle commands. The socket handler supports:
//   - "stop" — SIGTERM then SIGKILL to the whole session process group; replies "ok"
//   - "interrupt" — SIGINT to the whole session process group; replies "ok"
//   - "ping" — replies "ok"
//   - "pid" — replies with the PID (diagnostics)
func (p *Provider) startControlSocket(name string, cmd *exec.Cmd, done <-chan struct{}) (net.Listener, error) {
	sp := p.sockPath(name)
	namePath := p.sockNamePath(name)
	if err := os.MkdirAll(filepath.Dir(sp), 0o755); err != nil {
		return nil, err
	}
	// Remove stale socket from a previous crash.
	os.Remove(sp) //nolint:errcheck
	_ = os.Remove(namePath)
	if err := os.WriteFile(namePath, []byte(name), 0o644); err != nil {
		return nil, err
	}
	lis, err := net.Listen("unix", sp)
	if err != nil {
		os.Remove(namePath) //nolint:errcheck
		return nil, err
	}
	go func() {
		for {
			conn, err := lis.Accept()
			if err != nil {
				return // listener closed
			}
			go handleSessionConn(conn, cmd, done)
		}
	}()
	return lis, nil
}

// handleSessionConn reads a command from the connection and acts on the process.
func handleSessionConn(conn net.Conn, cmd *exec.Cmd, done <-chan struct{}) {
	defer conn.Close()                                     //nolint:errcheck
	conn.SetReadDeadline(time.Now().Add(10 * time.Second)) //nolint:errcheck
	scanner := bufio.NewScanner(conn)
	if !scanner.Scan() {
		return
	}
	switch scanner.Text() {
	case "stop":
		_ = runtime.TerminateManagedProcess(cmd, done, runtime.ManagedProcessStopGrace)
		conn.Write([]byte("ok\n")) //nolint:errcheck
	case "interrupt":
		_ = runtime.SignalProcessGroup(cmd, syscall.SIGINT)
		conn.Write([]byte("ok\n")) //nolint:errcheck
	case "ping":
		conn.Write([]byte("ok\n")) //nolint:errcheck
	case "pid":
		fmt.Fprintf(conn, "%d\n", cmd.Process.Pid) //nolint:errcheck
	}
}

// socketAlive checks if a session is alive by pinging its control socket.
func (p *Provider) socketAlive(name string) bool {
	return p.sendSocketCommand(name, "ping", 500*time.Millisecond) == nil
}

// sendSocketCommand connects to the session's control socket, sends a
// command, and waits for "ok". Returns nil on success.
func (p *Provider) sendSocketCommand(name, command string, timeout time.Duration) error {
	var (
		lastErr            error
		firstActionableErr error
	)
	for _, sp := range []string{p.sockPath(name), p.legacySockPath(name)} {
		err := func(sockPath string) error {
			conn, err := net.DialTimeout("unix", sockPath, timeout)
			if err != nil {
				return err
			}
			defer conn.Close()                        //nolint:errcheck
			conn.SetDeadline(time.Now().Add(timeout)) //nolint:errcheck
			if _, err := fmt.Fprintf(conn, "%s\n", command); err != nil {
				return err
			}
			scanner := bufio.NewScanner(conn)
			if scanner.Scan() && scanner.Text() == "ok" {
				return nil
			}
			if err := scanner.Err(); err != nil {
				return err
			}
			return fmt.Errorf("unexpected response from socket")
		}(sp)
		if err == nil {
			return nil
		}
		if !isUnavailableSocketError(err) && firstActionableErr == nil {
			firstActionableErr = err
		}
		lastErr = err
	}
	if firstActionableErr != nil {
		return firstActionableErr
	}
	return lastErr
}

// stopBySocket connects to a session's control socket and asks it to stop.
func (p *Provider) stopBySocket(name string) error {
	err := p.sendSocketCommand(name, "stop", 7*time.Second)
	if err != nil {
		if isUnavailableSocketError(err) {
			// Socket doesn't exist or can't connect — session is dead (idempotent).
			// Clean up stale socket file if it exists.
			os.Remove(p.sockPath(name)) //nolint:errcheck
			_ = os.Remove(p.sockNamePath(name))
			return nil
		}
		return err
	}
	return nil
}

func isUnavailableSocketError(err error) bool {
	return errors.Is(err, os.ErrNotExist) ||
		errors.Is(err, syscall.ENOENT) ||
		errors.Is(err, syscall.ECONNREFUSED)
}

// --- In-memory process helpers ---

// terminateSessionConn sends SIGTERM then SIGKILL to an in-memory tracked process.
func terminateSessionConn(sc *sessionConn) error {
	return runtime.TerminateManagedProcess(sc.cmd, sc.done, runtime.ManagedProcessStopGrace)
}

// Capabilities reports subprocess provider capabilities. The subprocess
// provider has no terminal and no activity tracking.
func (p *Provider) Capabilities() runtime.ProviderCapabilities {
	return runtime.ProviderCapabilities{}
}

// SleepCapability reports that subprocess sessions support timed-only idle
// sleep. They are headless and cannot provide prompt-boundary guarantees.
func (p *Provider) SleepCapability(string) runtime.SessionSleepCapability {
	return runtime.SessionSleepCapabilityTimedOnly
}

// alive reports whether the process is still running.
func (sc *sessionConn) alive() bool {
	select {
	case <-sc.done:
		return false
	default:
		return true
	}
}
