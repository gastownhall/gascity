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

	"github.com/gastownhall/gascity/internal/platform/localsocket"
	"github.com/gastownhall/gascity/internal/processgroup"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/proctable"
)

// Provider manages agent sessions as child processes.
type Provider struct {
	mu       sync.Mutex
	dir      string                  // socket/meta file directory
	procs    map[string]*sessionConn // in-process tracking
	workDirs map[string]string       // session name → workDir (for CopyTo)
	ops      providerOps
	// stopping holds, for each name a Stop is taking down a held tree for, a
	// channel closed once that Stop has finished. Start and Stop wait on it,
	// so no replacement starts, and no second Stop reports success, while the
	// first is still confirming the tree gone.
	stopping        map[string]chan struct{}
	scanExecutables []string
	// fenceChecked is a test seam run between a fenced operation's check and
	// its signal.
	fenceChecked func(name string)
}

type providerOps struct {
	// start launches cmd as the owner of its process tree and returns the
	// group identity captured at spawn.
	start  func(*exec.Cmd) (int, error)
	scan   func(string, []string) ([]runtime.LiveRuntime, error)
	listen func(network, addr string) (net.Listener, error)
	dial   func(network, addr string, timeout time.Duration) (net.Conn, error)
}

const (
	// socketPathLimit is the endpoint-pathname budget, owned by localsocket
	// because the ceiling it is derived from is a platform fact rather than a
	// provider one. A private copy per package is how this provider and the
	// cmd/gc endpoints drifted apart in the first place.
	socketPathLimit       = localsocket.PathLimit
	fallbackSocketDirName = "gascity-subprocess"
	nativeSocketPathLimit = len(syscall.RawSockaddrUnix{}.Path) - 1
)

// sessionConn tracks a running child process and its control socket.
type sessionConn struct {
	cmd      *exec.Cmd
	done     chan struct{} // closed when process exits
	reaped   chan struct{} // closed once reap has finished, after done
	token    string        // the GC_INSTANCE_TOKEN this incarnation seeded
	fence    string        // the input-fence digest of this incarnation ("" when its env names none)
	listener net.Listener  // unix socket listener

	// pgid is the process-group identity captured when cmd was spawned.
	// Recording it beats looking it up later: the lookup stops answering once
	// the group leader is reaped, which is exactly when surviving descendants
	// still need to be reached.
	pgid int
}

// Compile-time check.
var (
	errPrivateSocketDirValidation                                   = errors.New("private socket directory validation failed")
	_                             runtime.Provider                  = (*Provider)(nil)
	_                             runtime.ProcessTableScanner       = (*Provider)(nil)
	_                             runtime.LivenessObserverWithError = (*Provider)(nil)
	_                             runtime.ListingAttestation        = (*Provider)(nil)
)

// NewProvider returns a subprocess [Provider] that stores socket files in
// a default temporary directory. Suitable for production use.
func NewProvider() *Provider {
	dir := defaultProviderDir()
	_ = runtime.EnsurePrivateDir(dir)
	return newProvider(dir)
}

// defaultProviderDir is the city-less state directory: one per user, because
// the path is otherwise identical for everyone on the host and [os.MkdirAll]
// succeeds on a directory someone else created first. The euid does not make
// the directory private by itself — [Provider.SetMeta] verifies ownership
// before writing — but it keeps two legitimate users off one path so that
// verification means "someone squatted" rather than "you logged in second".
func defaultProviderDir() string {
	return filepath.Join(os.TempDir(), fmt.Sprintf("gc-subprocess-%d", os.Geteuid()))
}

// NewProviderWithDir returns a subprocess [Provider] that stores socket files
// in the given directory. Useful for tests that need isolated state.
func NewProviderWithDir(dir string) *Provider {
	// Best-effort here and verified at the write path: a constructor cannot
	// report a squatted directory, and failing silently at construction would
	// hand back a Provider that writes anyway.
	_ = runtime.EnsurePrivateDir(dir)
	return newProvider(dir)
}

func newProvider(dir string) *Provider {
	return &Provider{
		dir:      dir,
		procs:    make(map[string]*sessionConn),
		workDirs: make(map[string]string),
		ops: providerOps{
			scan: proctable.ScanBySessionIDForExecutables,
			start: func(cmd *exec.Cmd) (int, error) {
				// Sessions outlive the CLI invocation that started them and
				// are stopped through the control socket, so releasing
				// ownership must not take them down.
				return processgroup.StartCommand(cmd, processgroup.SurviveRelease)
			},
			listen: net.Listen,
			dial:   net.DialTimeout,
		},
		stopping: make(map[string]chan struct{}),
	}
}

// Start spawns a child process for the given session name and config.
// Returns an error if a session with that name is already running.
// Startup hints (ReadyPromptPrefix, ProcessNames, etc.) are ignored —
// all sessions are fire-and-forget.
func (p *Provider) Start(_ context.Context, name string, cfg runtime.Config) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	euid := os.Geteuid()
	if err := p.awaitStopLocked(name); err != nil {
		return err
	}

	// Check in-memory tracking first.
	if existing, ok := p.procs[name]; ok {
		if existing.alive() {
			return fmt.Errorf("%w: session %q", runtime.ErrSessionExists, name)
		}
		// The leader has exited, but a tree still held has members that
		// outlived it; a replacement must not start beside them.
		// p.mu is held, and reap takes it before it finishes, so this must not
		// wait for reap the way terminateSessionConn does.
		if processgroup.HoldsTree(existing.cmd) {
			if err := processgroup.TerminateManagedCommand(existing.cmd, existing.pgid, existing.done, runtime.ManagedProcessStopGrace, processgroup.Options{}); err != nil {
				return fmt.Errorf("stopping what the previous session %q left running: %w", name, err)
			}
		}
		delete(p.procs, name)
	}

	// Check socket for cross-process case.
	if p.socketAliveAt(name, euid) {
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

	if reason := runtime.ShellCommandUnavailable(cfg.Command); reason != "" {
		clearWorkDir()
		return fmt.Errorf("session %q command refused: %s", name, reason)
	}
	cmd, err := runtime.ShellSessionCommand(cfg.Command)
	if err != nil {
		clearWorkDir()
		return fmt.Errorf("preparing command for %q: %w", name, err)
	}
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

	// Build environment: inherit parent env + apply overrides. An empty override
	// spells withholding, matching the tmux adapter: remove the inherited entry
	// instead of passing KEY=, so namespace isolation remains real to children.
	env := os.Environ()
	if len(cfg.Env) > 0 {
		keys := make([]string, 0, len(cfg.Env))
		for k := range cfg.Env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			env = envWithoutKey(env, k)
			if cfg.Env[k] == "" {
				continue
			}
			env = append(env, k+"="+cfg.Env[k])
		}
	}
	cmd.Env = env

	// Validate immediately before process creation so hostile pre-creation
	// fails without spawning a child or touching stale socket artifacts.
	socketDir := p.socketDirForEUID(euid)
	if err := p.ensureSocketDir(socketDir, euid); err != nil {
		_ = nullFile.Close()
		clearWorkDir()
		return fmt.Errorf("preparing control socket for %q: %w", name, err)
	}
	pgid, err := p.ops.start(cmd)
	if err != nil {
		_ = nullFile.Close()
		clearWorkDir()
		return fmt.Errorf("starting session %q: %w", name, err)
	}
	_ = nullFile.Close()

	// Seed the identity sidecar before the control socket exists, so no
	// reader can see a live socket that carries no identity.
	if err := p.persistStartMetadata(name, cfg.Env); err != nil {
		_ = processgroup.KillCommandGroup(cmd, pgid, processgroup.Options{})
		_ = cmd.Wait()
		clearWorkDir()
		return fmt.Errorf("storing metadata for %q: %w", name, err)
	}

	// Create control socket for cross-process discovery.
	sc := &sessionConn{cmd: cmd, done: make(chan struct{}), reaped: make(chan struct{}), token: cfg.Env["GC_INSTANCE_TOKEN"], pgid: pgid}
	if f, ok := runtime.InputFenceFromEnv(cfg.Env); ok {
		sc.fence = f.Digest()
	}
	lis, err := p.startControlSocket(name, sc, socketDir, euid)
	if err != nil {
		// Socket creation failed — reap the owned tree and bail.
		_ = processgroup.KillCommandGroup(cmd, pgid, processgroup.Options{})
		_ = cmd.Wait()
		p.clearSessionMeta(name)
		clearWorkDir()
		return fmt.Errorf("creating control socket for %q: %w", name, err)
	}

	sc.listener = lis
	go p.reap(name, sc, socketDir, euid)

	p.procs[name] = sc
	return nil
}

// reap cleans up after sc's process exits. The socket goes first, so
// ListRunning never sees a stale socket after Stop returns. done closes next,
// so the runtime reads not alive before its identity sidecar is cleared.
//
// The sidecar is cleared only while it is still this incarnation's. Under p.mu
// no newer Start in this provider can be mid-seed, so a name this provider now
// tracks for a newer incarnation is left alone; a sidecar another provider has
// reseeded carries a different token. The token check is a read before the
// remove, so it narrows the cross-process race rather than closing it.
func (p *Provider) reap(name string, sc *sessionConn, socketDir string, euid int) {
	defer close(sc.reaped)
	_ = sc.cmd.Wait()
	sc.listener.Close() //nolint:errcheck
	_ = p.removeSocketArtifactsAt(name, socketDir, euid)
	close(sc.done)
	// The leader exiting is not the tree exiting. Ownership ends here only if
	// nothing is left; otherwise it is kept, because Stop can still arrive
	// through p.procs and the owned tree is the only way to reach helpers the
	// agent left running.
	_ = processgroup.ReleaseCommandIfEmpty(sc.cmd)

	p.mu.Lock()
	defer p.mu.Unlock()
	if cur, ok := p.procs[name]; ok && cur != sc {
		return
	}
	if token, err := p.GetMeta(name, "GC_INSTANCE_TOKEN"); err != nil || token != sc.token {
		return
	}
	p.clearSessionMeta(name)
}

func envWithoutKey(env []string, key string) []string {
	prefix := key + "="
	out := make([]string, 0, len(env))
	for _, entry := range env {
		if !strings.HasPrefix(entry, prefix) {
			out = append(out, entry)
		}
	}
	return out
}

// Stop terminates the named session. Returns nil if it doesn't exist
// (idempotent). Sends SIGTERM first, then SIGKILL after a grace period.
func (p *Provider) Stop(name string) error {
	euid := os.Geteuid()
	p.mu.Lock()
	if err := p.awaitStopLocked(name); err != nil {
		p.mu.Unlock()
		return err
	}
	sc, ok := p.procs[name]
	var stopped chan struct{}
	if ok {
		delete(p.procs, name)
		// A held tree can outlive its leader, and with it the control socket
		// that would otherwise keep a replacement out, so the name stays
		// reserved until the tree is confirmed gone.
		if processgroup.HoldsTree(sc.cmd) {
			stopped = make(chan struct{})
			p.stopping[name] = stopped
		}
	}
	p.mu.Unlock()

	// Try in-memory process first.
	if ok {
		if testHookBeforeStopTeardown != nil {
			testHookBeforeStopTeardown(name)
		}
		err := terminateTracked(sc)
		p.mu.Lock()
		// Keep the session reachable while its tree is still held, so a
		// retry acts on the same tree rather than finding nothing.
		if err != nil && processgroup.HoldsTree(sc.cmd) {
			if _, replaced := p.procs[name]; !replaced {
				p.procs[name] = sc
			}
		}
		if stopped != nil {
			delete(p.stopping, name)
			close(stopped)
		}
		p.mu.Unlock()
		return err
	}

	// Fall back to socket (cross-process case: gc stop after gc start).
	return p.stopBySocketAt(name, euid)
}

// testHookBeforeStopTeardown, when set, is called by Stop once it has taken a
// tracked session, before tearing it down.
var testHookBeforeStopTeardown func(name string)

// stopWaitTimeout bounds how long Start or Stop waits for a Stop of the same
// session already in progress: a cooperative grace, then a forced
// termination and its confirmation.
const stopWaitTimeout = 3*runtime.ManagedProcessStopGrace + 10*time.Second

// awaitStopLocked waits for a Stop of name already in progress to finish,
// releasing p.mu while it waits. p.mu must be held, and is held on return.
func (p *Provider) awaitStopLocked(name string) error {
	deadline := time.Now().Add(stopWaitTimeout)
	for {
		stopped, stopping := p.stopping[name]
		if !stopping {
			return nil
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return fmt.Errorf("session %q is still being stopped after %s", name, stopWaitTimeout)
		}
		p.mu.Unlock()
		timer := time.NewTimer(remaining)
		select {
		case <-stopped:
		case <-timer.C:
		}
		timer.Stop()
		p.mu.Lock()
	}
}

// terminateTracked stops a session this provider started. A leader that has
// exited leaves nothing to stop unless its tree is still held, which means
// members outlived it.
func terminateTracked(sc *sessionConn) error {
	if !sc.alive() && !processgroup.HoldsTree(sc.cmd) {
		<-sc.reaped
		return nil
	}
	return terminateSessionConn(sc)
}

// Interrupt sends SIGINT to the named session's process.
// Best-effort: returns nil if the session doesn't exist.
func (p *Provider) Interrupt(name string) error {
	euid := os.Geteuid()
	p.mu.Lock()
	sc, ok := p.procs[name]
	p.mu.Unlock()
	if ok {
		return processgroup.InterruptCommand(sc.cmd, sc.pgid, processgroup.Options{})
	}

	// Fall back to socket (cross-process case). A missing socket is the same
	// as "interrupt succeeded"; validation failures must remain visible.
	err := p.sendSocketCommandAt(name, "interrupt", 2*time.Second, euid)
	if errors.Is(err, errPrivateSocketDirValidation) {
		return err
	}
	return nil
}

// IsRunning reports whether the named session has a live process.
func (p *Provider) IsRunning(name string) bool {
	obs, _ := p.ObserveLivenessWithError(name, nil)
	return obs.Running
}

// ObserveLivenessWithError implements [runtime.LivenessObserverWithError]. An
// in-process session answers from its process; any other answers from its
// control socket (see probeSessionSocket), so a probe that cannot tell returns
// an error wrapping [runtime.ErrRuntimeUnavailable] instead of absence.
// Process names are ignored, as in ProcessAlive.
func (p *Provider) ObserveLivenessWithError(name string, _ []string) (runtime.Liveness, error) {
	euid := os.Geteuid()
	p.mu.Lock()
	sc, ok := p.procs[name]
	p.mu.Unlock()

	if ok {
		alive := sc.alive()
		return runtime.Liveness{Running: alive, Alive: alive}, nil
	}
	present, err := p.probeSessionSocket(name, euid)
	return runtime.Liveness{Running: present, Alive: present}, err
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

// FindRuntimesBySessionID implements [runtime.ProcessTableScanner].
func (p *Provider) FindRuntimesBySessionID(id string) ([]runtime.LiveRuntime, error) {
	p.mu.Lock()
	executables := append([]string(nil), p.scanExecutables...)
	p.mu.Unlock()
	found, scanErr := p.ops.scan(id, executables)

	p.mu.Lock()
	trackedBySessionID := make(map[string]string)
	for name, sc := range p.procs {
		if sc == nil || sc.cmd == nil || !sc.alive() {
			continue
		}
		if sessionID := envValue(sc.cmd.Env, "GC_SESSION_ID"); sessionID != "" {
			trackedBySessionID[sessionID] = name
		}
	}
	p.mu.Unlock()

	for i := range found {
		if name, ok := trackedBySessionID[found[i].SessionID]; ok {
			found[i].IsTracked = true
			found[i].ProviderName = name
		}
	}
	return found, scanErr
}

// SetScanExecutables supplies the city's runtime executable names for Windows
// unreadable-process classification. It does not filter readable roots.
func (p *Provider) SetScanExecutables(executables []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.scanExecutables = append([]string(nil), executables...)
}

// TerminateRuntime implements [runtime.ProcessTableScanner].
func (p *Provider) TerminateRuntime(r runtime.LiveRuntime) error {
	if r.PID <= 1 {
		return fmt.Errorf("subprocess: invalid PID %d for session %s", r.PID, r.SessionID)
	}
	if err := proctable.KillByPIDIdentity(r.PID, r.StartIdentity); err != nil {
		return fmt.Errorf("subprocess: terminate runtime PID %d for session %s: %w", r.PID, r.SessionID, err)
	}
	return nil
}

func envValue(env []string, key string) string {
	prefix := key + "="
	for i := len(env) - 1; i >= 0; i-- {
		value := env[i]
		if strings.HasPrefix(value, prefix) {
			return value[len(prefix):]
		}
	}
	return ""
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
//
// The sidecar is owner-only. Even after [Provider.persistStartMetadata] filters
// the session environment it still holds the incarnation fence token, and a
// fence that can be read or forged is how a stale process talks its way past
// drain. Ownership is verified on every write rather than trusted from
// construction, and the mode is set explicitly rather than left to
// [os.WriteFile]'s perm argument, which is consulted only at create — a host
// upgrading from an older binary keeps its 0644 files otherwise, which is
// exactly the host that already has credentials on disk.
func (p *Provider) SetMeta(name, key, value string) error {
	if err := runtime.EnsurePrivateDir(p.dir); err != nil {
		return err
	}
	return runtime.WritePrivateFile(p.metaPath(name, key), []byte(value))
}

// LocalIdentitySidecar implements [runtime.IdentitySidecarProvider]: GetMeta
// reads the session's local sidecar file.
func (p *Provider) LocalIdentitySidecar() bool { return true }

// LivenessReadsFresh implements [runtime.FreshByConstruction].
func (p *Provider) LivenessReadsFresh() bool { return true }

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

// persistStartMetadata seeds the session's sidecar from its environment so that
// GetMeta answers the identity and fence reads the reconciler makes while the
// session is still starting.
//
// It seeds the classified half of the environment, not all of it: the sidecar
// is a durable file store that outlives the session, so writing every variable
// leaves the agent's API keys on disk with no reader that ever wants them back.
// [runtime.SplitEnvForMetaSeed] keeps the keys a GetMeta consumer reads.
func (p *Provider) persistStartMetadata(name string, env map[string]string) error {
	seed, _ := runtime.SplitEnvForMetaSeed(env)
	p.clearSessionMeta(name)
	for _, key := range runtime.MetaSeedKeys(seed) {
		if err := p.SetMeta(name, key, seed[key]); err != nil {
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
// match the given prefix, discovered via socket files. A socket that cannot be
// classified (see probeSessionSocket) leaves its name out, so the names come
// back with a [runtime.PartialListError] rather than as a complete list.
func (p *Provider) ListRunning(prefix string) ([]string, error) {
	euid := os.Geteuid()
	dirs := []string{p.dir}
	if fallback := p.fallbackDirForEUID(euid); fallback != p.dir {
		dirs = append(dirs, fallback)
	}
	seen := make(map[string]bool)
	var (
		names    []string
		failures []error
	)
	for _, dir := range dirs {
		if err := p.validateSocketDir(dir, euid); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
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
			present, err := p.probeSessionSocket(sn, euid)
			if errors.Is(err, errPrivateSocketDirValidation) {
				return nil, err
			}
			if err != nil {
				failures = append(failures, err)
				continue
			}
			if present {
				seen[sn] = true
				names = append(names, sn)
			}
		}
	}
	if len(failures) > 0 {
		return names, &runtime.PartialListError{Err: errors.Join(failures...)}
	}
	return names, nil
}

// ListRunningComplete implements [runtime.ListingAttestation]: a socket that
// cannot be classified makes ListRunning partial, so an error-free result
// lists every running session.
func (p *Provider) ListRunningComplete() bool { return true }

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
	return p.fallbackDirForEUID(os.Geteuid())
}

func (p *Provider) fallbackDirForEUID(euid int) string {
	legacy := filepath.Join(os.TempDir(), fallbackSocketDirName, p.fallbackLeaf())
	probe := filepath.Join(legacy, p.sockKey("probe")+".sock")
	if len(probe) <= nativeSocketPathLimit {
		return legacy
	}
	return p.privateFallbackDir(euid)
}

func (p *Provider) fallbackLeaf() string {
	sum := sha256.Sum256([]byte(filepath.Clean(p.dir)))
	return hex.EncodeToString(sum[:8])
}

// privateFallbackRoot is the owner-only root holding hashed fallback socket
// directories for one user.
//
// Which directory can hold a short endpoint pathname is a platform question,
// so localsocket owns the root. This used to join the literal "/tmp", which is
// not a temporary directory on Windows: filepath.Join turns it into a
// drive-relative \tmp on whatever volume the process happens to be running
// from, outside every runtime directory Gas City creates and protects.
func privateFallbackRoot(euid int) string {
	return localsocket.FallbackDir(privateFallbackNamespace(euid))
}

func (p *Provider) privateFallbackDir(euid int) string {
	return filepath.Join(privateFallbackRoot(euid), p.fallbackLeaf())
}

func (p *Provider) isPrivateFallbackDir(dir string, euid int) bool {
	return dir == p.privateFallbackDir(euid)
}

func validatePrivateSocketDir(path string, euid int) error {
	return validatePrivateSocketDirPlatform(path, euid)
}

func ensurePrivateSocketDir(path string, euid int) error {
	if err := os.Mkdir(path, 0o700); err != nil && !os.IsExist(err) {
		return fmt.Errorf("creating private socket directory %q: %w", path, err)
	}
	return validatePrivateSocketDir(path, euid)
}

func (p *Provider) ensureSocketDir(dir string, euid int) error {
	if !p.isPrivateFallbackDir(dir, euid) {
		return os.MkdirAll(dir, 0o755)
	}
	if err := ensurePrivateSocketDir(filepath.Dir(dir), euid); err != nil {
		return err
	}
	return ensurePrivateSocketDir(dir, euid)
}

func (p *Provider) validateSocketDir(dir string, euid int) error {
	if !p.isPrivateFallbackDir(dir, euid) {
		return nil
	}
	if err := validatePrivateSocketDir(filepath.Dir(dir), euid); err != nil {
		return err
	}
	return validatePrivateSocketDir(dir, euid)
}

func (p *Provider) socketDir() string {
	return p.socketDirForEUID(os.Geteuid())
}

func (p *Provider) socketDirForEUID(euid int) string {
	candidate := filepath.Join(p.dir, p.sockKey("probe")+".sock")
	if len(candidate) <= socketPathLimit {
		return p.dir
	}
	return p.fallbackDirForEUID(euid)
}

func (p *Provider) sockPath(name string) string {
	return filepath.Join(p.socketDir(), p.sockKey(name)+".sock")
}

func (p *Provider) sockNamePath(name string) string {
	return filepath.Join(p.socketDir(), p.sockKey(name)+".name")
}

func (p *Provider) removeSocketArtifactsAt(name, dir string, euid int) error {
	if err := p.validateSocketDir(dir, euid); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	key := p.sockKey(name)
	_ = os.Remove(filepath.Join(dir, key+".sock"))
	_ = os.Remove(filepath.Join(dir, key+".name"))
	return nil
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
//   - "stop" — cooperative stop, then force-reap of the owned tree; replies
//     "ok" on confirmed teardown and "err: <reason>" otherwise
//   - "interrupt" — interrupt the whole session process group; replies "ok" or
//     "err: <reason>"
//   - "ping" — replies "ok"
//   - "pid" — replies with the PID (diagnostics)
func (p *Provider) startControlSocket(name string, sc *sessionConn, dir string, euid int) (net.Listener, error) {
	if err := p.ensureSocketDir(dir, euid); err != nil {
		return nil, err
	}
	key := p.sockKey(name)
	sp := filepath.Join(dir, key+".sock")
	namePath := filepath.Join(dir, key+".name")
	// Remove stale socket from a previous crash.
	os.Remove(sp) //nolint:errcheck
	_ = os.Remove(namePath)
	if err := os.WriteFile(namePath, []byte(name), 0o644); err != nil {
		return nil, err
	}
	lis, err := p.ops.listen("unix", sp)
	if err != nil {
		_ = os.Remove(namePath)
		return nil, err
	}
	go func() {
		for {
			conn, err := lis.Accept()
			if err != nil {
				return // listener closed
			}
			go handleSessionConn(conn, sc)
		}
	}()
	return lis, nil
}

// handleSessionConn reads a command from the connection and acts on the process.
//
// A failed or unsupported operation answers "err: <reason>" rather than "ok".
// The caller is a remote process deciding whether the session is gone; telling
// it the stop succeeded when the tree is still running hands it a false clear,
// and it has no other way to find out.
func handleSessionConn(conn net.Conn, sc *sessionConn) {
	defer conn.Close()                                     //nolint:errcheck
	conn.SetReadDeadline(time.Now().Add(10 * time.Second)) //nolint:errcheck
	scanner := bufio.NewScanner(conn)
	if !scanner.Scan() {
		return
	}
	reply := func(err error) {
		if err != nil {
			fmt.Fprintf(conn, "err: %v\n", err) //nolint:errcheck
			return
		}
		conn.Write([]byte("ok\n")) //nolint:errcheck
	}
	switch scanner.Text() {
	case "stop":
		reply(terminateSessionConn(sc))
	case "interrupt":
		reply(processgroup.InterruptCommand(sc.cmd, sc.pgid, processgroup.Options{}))
	case "ping":
		conn.Write([]byte("ok\n")) //nolint:errcheck
	case "pid":
		fmt.Fprintf(conn, "%d\n", sc.cmd.Process.Pid) //nolint:errcheck
	}
}

// socketAlive reports whether the session's control socket accepts a
// connection. A socket that cannot be classified reads as not alive.
func (p *Provider) socketAlive(name string) bool {
	return p.socketAliveAt(name, os.Geteuid())
}

func (p *Provider) socketAliveAt(name string, euid int) bool {
	present, _ := p.probeSessionSocket(name, euid)
	return present
}

// socketProbeTimeout bounds one control-socket dial in probeSessionSocket.
const socketProbeTimeout = 500 * time.Millisecond

// probeSessionSocket classifies the named session's control socket, trying the
// hashed path and then the legacy name-based one. It is the one answer behind
// IsRunning, ObserveLivenessWithError and ListRunning:
//   - (true, nil): a path accepted the connection. The owner closes its
//     listener when the process exits, so the session is running even when it
//     is too busy to answer a ping.
//   - (false, nil): every path is missing, refuses connections, or is too long
//     to bind (see [runtime.ClassifyControlSocketDial] for what refused means
//     off Linux).
//   - (false, err): the private socket directory failed validation, or no
//     path connected and one failed another way (a dial timeout, EACCES); err
//     wraps [runtime.ErrRuntimeUnavailable].
func (p *Provider) probeSessionSocket(name string, euid int) (bool, error) {
	socketDir := p.socketDirForEUID(euid)
	paths := make([]string, 0, 2)
	switch err := p.validateSocketDir(socketDir, euid); {
	case err == nil:
		paths = append(paths, filepath.Join(socketDir, p.sockKey(name)+".sock"))
	case !os.IsNotExist(err):
		return false, fmt.Errorf("%w: %w: %w", runtime.ErrRuntimeUnavailable, errPrivateSocketDirValidation, err)
	}
	paths = append(paths, p.legacySockPath(name))
	var unknown error
	for _, sp := range paths {
		if runtime.UnixSocketPathTooLong(sp) {
			continue
		}
		conn, err := p.ops.dial("unix", sp, socketProbeTimeout)
		switch runtime.ClassifyControlSocketDial(err) {
		case runtime.ControlSocketPresent:
			_ = conn.Close()
			return true, nil
		case runtime.ControlSocketUnknown:
			if unknown == nil {
				unknown = err
			}
		}
	}
	if unknown != nil {
		return false, fmt.Errorf("%w: subprocess control socket for %q: %w", runtime.ErrRuntimeUnavailable, name, unknown)
	}
	return false, nil
}

// sendSocketCommand connects to the session's control socket, sends a
// command, and waits for "ok". Returns nil on success.
func (p *Provider) sendSocketCommand(name, command string, timeout time.Duration) error {
	return p.sendSocketCommandAt(name, command, timeout, os.Geteuid())
}

func (p *Provider) sendSocketCommandAt(name, command string, timeout time.Duration, euid int) error {
	socketDir := p.socketDirForEUID(euid)
	var (
		lastErr            error
		firstActionableErr error
	)
	canonicalAvailable := true
	if err := p.validateSocketDir(socketDir, euid); err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("%w: %w", errPrivateSocketDirValidation, err)
		}
		canonicalAvailable = false
		lastErr = err
	}
	legacyPath := p.legacySockPath(name)
	canonicalPath := filepath.Join(socketDir, p.sockKey(name)+".sock")
	paths := make([]string, 0, 2)
	if canonicalAvailable {
		paths = append(paths, canonicalPath)
	}
	paths = append(paths, legacyPath)
	for _, sp := range paths {
		err := func(sockPath string) error {
			conn, err := p.ops.dial("unix", sockPath, timeout)
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
		// The canonical hashed path above is always addressable. An older
		// name-based path can exceed sockaddr_un and cannot contain a live
		// compatibility socket; retain the canonical result in that case.
		if sp == legacyPath && len(legacyPath) > nativeSocketPathLimit && errors.Is(err, syscall.EINVAL) {
			continue
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
	return p.stopBySocketAt(name, os.Geteuid())
}

func (p *Provider) stopBySocketAt(name string, euid int) error {
	err := p.sendSocketCommandAt(name, "stop", 7*time.Second, euid)
	if err != nil {
		if isUnavailableSocketError(err) {
			// Socket doesn't exist or can't connect — session is dead (idempotent).
			// Clean up stale socket file if it exists.
			return p.removeSocketArtifactsAt(name, p.socketDirForEUID(euid), euid)
		}
		return err
	}
	return nil
}

// --- In-memory process helpers ---

// terminateSessionConn requests a cooperative stop and, after the grace period,
// force-reaps the owned process tree, then returns once reap has finished, so
// a stopped session's sidecar is gone when Stop (or a socket "stop") returns.
func terminateSessionConn(sc *sessionConn) error {
	err := processgroup.TerminateManagedCommand(sc.cmd, sc.pgid, sc.done, runtime.ManagedProcessStopGrace, processgroup.Options{})
	<-sc.reaped
	return err
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
