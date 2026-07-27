package acp

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gastownhall/gascity/internal/execgrace"
	"github.com/gastownhall/gascity/internal/runtime"
)

const (
	// nudgePostWriteDrainTimeout caps the wait for sc.done after a Nudge stdin
	// write fails. Sized to match terminateProcess's SIGTERM grace period so a
	// Nudge racing with Stop still converges to the best-effort nil contract
	// rather than surfacing a spurious error before SIGKILL lands.
	nudgePostWriteDrainTimeout = 5 * time.Second

	// defaultSetupTimeout mirrors the tmux and herdr providers' default for
	// [session] setup_timeout.
	defaultSetupTimeout = 10 * time.Second
	// preStartOutputLimit bounds the diagnostic tail from a failed command.
	preStartOutputLimit = 4096
	// preStartWaitDelay prevents a successful command whose background child
	// inherited stdout/stderr from hanging session startup indefinitely.
	preStartWaitDelay = 2 * time.Second
	// preStartCancelGrace is the rollback-trap budget when the activity-aware
	// setup budget is enabled, matching tmux and herdr.
	preStartCancelGrace = 10 * time.Second
)

// Config holds ACP provider settings.
type Config struct {
	HandshakeTimeout  time.Duration // default 30s
	NudgeBusyTimeout  time.Duration // default 60s
	OutputBufferLines int           // default 1000
	SetupTimeout      time.Duration // default 10s; fixed deadline or output-idle budget
	SetupMaxTimeout   time.Duration // optional activity-aware absolute ceiling
	CityRoot          string        // fallback cwd when GC_DIR does not exist yet
}

func (c *Config) handshakeTimeout() time.Duration {
	if c.HandshakeTimeout <= 0 {
		return 30 * time.Second
	}
	return c.HandshakeTimeout
}

func (c *Config) nudgeBusyTimeout() time.Duration {
	if c.NudgeBusyTimeout <= 0 {
		return 60 * time.Second
	}
	return c.NudgeBusyTimeout
}

func (c *Config) outputBufferLines() int {
	if c.OutputBufferLines <= 0 {
		return defaultOutputBufferLines
	}
	return c.OutputBufferLines
}

func (c *Config) setupTimeout() time.Duration {
	if c.SetupTimeout <= 0 {
		return defaultSetupTimeout
	}
	return c.SetupTimeout
}

// Provider manages agent sessions using the Agent Client Protocol.
type Provider struct {
	mu       sync.Mutex
	dir      string                  // socket/meta file directory
	conns    map[string]*sessionConn // in-process tracking
	workDirs map[string]string       // session name → workDir (for CopyTo)
	cfg      Config
}

// Compile-time check.
var (
	_ runtime.Provider                    = (*Provider)(nil)
	_ runtime.InteractionProvider         = (*Provider)(nil)
	_ runtime.TransportCapabilityProvider = (*Provider)(nil)
)

// NewProvider returns an ACP [Provider] that stores socket files in
// a default temporary directory.
func NewProvider(cfg Config) *Provider {
	dir := filepath.Join(os.TempDir(), "gc-acp")
	_ = os.MkdirAll(dir, 0o755)
	return &Provider{
		dir:      dir,
		conns:    make(map[string]*sessionConn),
		workDirs: make(map[string]string),
		cfg:      cfg,
	}
}

// NewProviderWithDir returns an ACP [Provider] that stores socket files
// in the given directory. Useful for tests that need isolated state.
func NewProviderWithDir(dir string, cfg Config) *Provider {
	_ = os.MkdirAll(dir, 0o755)
	return &Provider{
		dir:      dir,
		conns:    make(map[string]*sessionConn),
		workDirs: make(map[string]string),
		cfg:      cfg,
	}
}

// SupportsTransport reports whether this provider can host the requested
// session transport.
func (p *Provider) SupportsTransport(transport string) bool {
	return transport == "acp"
}

// Start spawns an ACP agent process, performs the JSON-RPC handshake, and
// optionally sends the initial nudge. Returns an error if a session with
// that name already exists or the handshake fails.
func (p *Provider) Start(ctx context.Context, name string, cfg runtime.Config) error {
	p.mu.Lock()

	// Check in-memory tracking first.
	if existing, ok := p.conns[name]; ok {
		if existing.alive() {
			p.mu.Unlock()
			return fmt.Errorf("%w: session %q", runtime.ErrSessionExists, name)
		}
		delete(p.conns, name)
	}

	// Check socket for cross-process case.
	if p.socketAlive(name) {
		p.mu.Unlock()
		return fmt.Errorf("%w: session %q", runtime.ErrSessionExists, name)
	}

	// Reserve the name with a sentinel so concurrent Start calls for the
	// same name are rejected while we perform the slow handshake outside
	// the lock. The sentinel's done channel is open (not closed), so
	// alive() returns true and duplicate checks above will reject.
	// The cancel func lets Stop abort an in-progress handshake immediately.
	hsCtx, hsCancel := context.WithCancel(ctx)
	defer hsCancel()
	sentinel := &sessionConn{done: make(chan struct{}), cancel: hsCancel, pending: make(map[int64]chan JSONRPCMessage)}
	defer close(sentinel.done)
	p.conns[name] = sentinel

	// Store workDir for CopyTo.
	if cfg.WorkDir != "" {
		p.workDirs[name] = cfg.WorkDir
	}

	p.mu.Unlock()

	// clearSentinel removes the reservation on failure.
	clearSentinel := func() {
		p.mu.Lock()
		if p.conns[name] == sentinel {
			delete(p.conns, name)
			delete(p.workDirs, name)
		}
		p.mu.Unlock()
	}

	command := cfg.Command
	if cfg.PromptSuffix != "" {
		if cfg.PromptFlag != "" {
			command = command + " " + cfg.PromptFlag + " " + cfg.PromptSuffix
		} else {
			command = command + " " + cfg.PromptSuffix
		}
	}
	if command == "" {
		clearSentinel()
		return fmt.Errorf("acp provider requires a command")
	}
	checkStartupContext := func() error {
		if err := hsCtx.Err(); err != nil {
			clearSentinel()
			return fmt.Errorf("starting session %q: %w", name, context.Cause(hsCtx))
		}
		return nil
	}
	if err := checkStartupContext(); err != nil {
		return err
	}
	if err := runtime.StageSessionWorkDir(cfg); err != nil {
		clearSentinel()
		return fmt.Errorf("staging workdir for %q: %w", name, err)
	}
	if err := checkStartupContext(); err != nil {
		return err
	}
	if err := p.runPreStart(hsCtx, cfg); err != nil {
		clearSentinel()
		return fmt.Errorf("running pre_start for %q: %w", name, err)
	}
	if err := checkStartupContext(); err != nil {
		return err
	}

	cmd := exec.Command("sh", "-c", command)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if cfg.WorkDir != "" {
		cmd.Dir = cfg.WorkDir
	}

	// Build environment: inherit parent env + apply overrides.
	cmd.Env = commandEnv(cfg.Env)

	// Set up stdio pipes for JSON-RPC.
	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		clearSentinel()
		return fmt.Errorf("creating stdin pipe for %q: %w", name, err)
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		clearSentinel()
		return fmt.Errorf("creating stdout pipe for %q: %w", name, err)
	}
	// Capture stderr to a bounded buffer for diagnostics. We use our
	// own pipe + goroutine (not cmd.Stderr) so that cmd.Wait() does not
	// block waiting for the stderr copy to finish after process kill.
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		clearSentinel()
		return fmt.Errorf("creating stderr pipe for %q: %w", name, err)
	}
	cmd.Stderr = stderrW
	var stderrBuf limitedWriter
	stderrBuf.max = 4096
	go func() {
		buf := make([]byte, 4096)
		for {
			n, readErr := stderrR.Read(buf)
			if n > 0 {
				stderrBuf.Write(buf[:n]) //nolint:errcheck
			}
			if readErr != nil {
				break
			}
		}
		stderrR.Close() //nolint:errcheck
	}()

	if err := cmd.Start(); err != nil {
		stderrW.Close() //nolint:errcheck
		clearSentinel()
		return fmt.Errorf("starting session %q: %w", name, err)
	}
	// Close the write end — child inherits it; we only read.
	stderrW.Close() //nolint:errcheck

	// Create control socket for cross-process discovery.
	processDone := make(chan struct{})
	lis, err := p.startControlSocket(name, cmd, processDone)
	if err != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
		clearSentinel()
		return fmt.Errorf("creating control socket for %q: %w", name, err)
	}

	sc := newSessionConn(cmd, stdinPipe, lis, p.cfg.outputBufferLines(), processDone)

	// Start readLoop before handshake so we can receive responses.
	go sc.readLoop(stdoutPipe)

	// Monitor process exit — clean up pending state, socket, and listener.
	// Socket cleanup happens BEFORE close(done) so that callers waiting
	// on sc.done (e.g., terminateProcess) can rely on the socket being
	// gone when done fires. Without this ordering, IsRunning can race:
	// Stop deletes the conn from the map, terminateProcess waits on done,
	// done closes, Stop returns — but the socket is still alive, so
	// IsRunning falls through to socketAlive and returns true.
	go func() {
		_ = cmd.Wait()
		sc.drainPending()
		lis.Close()                 //nolint:errcheck
		os.Remove(p.sockPath(name)) //nolint:errcheck
		_ = os.Remove(p.sockNamePath(name))
		close(processDone)
	}()

	// Perform ACP handshake with a deadline. hsCtx (created above with
	// WithCancel) is already cancellable by Stop. Add a timeout
	// child so handshake_timeout applies even when the parent has a
	// longer deadline.
	hsTimeoutCtx, hsTimeoutCancel := context.WithTimeout(hsCtx, p.cfg.handshakeTimeout())
	defer hsTimeoutCancel()

	if err := p.handshake(hsTimeoutCtx, sc, cfg.WorkDir, cfg.MCPServers); err != nil {
		// Handshake failed — kill the process. The monitor goroutine
		// handles listener/socket cleanup when the process exits.
		_ = stdinPipe.Close()
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-sc.done
		clearSentinel()
		// Include stderr tail in the error for diagnostics.
		if stderr := stderrBuf.String(); stderr != "" {
			return fmt.Errorf("acp handshake for %q: %w\nagent stderr:\n%s", name, err, stderr)
		}
		return fmt.Errorf("acp handshake for %q: %w", name, err)
	}

	// Commit under the same lock Stop uses to cancel a startup sentinel.
	// If Stop wins the lock, cancellation is visible here before the real
	// connection can replace the reservation. If Start wins, Stop observes
	// the real connection and terminates it normally. This closes the narrow
	// handoff window where Stop could otherwise cancel the old sentinel after
	// Start had already committed a live process.
	p.mu.Lock()
	startupCanceled := hsCtx.Err() != nil || p.conns[name] != sentinel
	if !startupCanceled {
		p.conns[name] = sc
	}
	p.mu.Unlock()
	if startupCanceled {
		_ = stdinPipe.Close()
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-sc.done
		clearSentinel()
		return fmt.Errorf("session %q was stopped during startup", name)
	}

	// Send initial nudge if configured (best-effort, outside lock).
	if cfg.Nudge != "" {
		_ = p.Nudge(name, runtime.TextContent(cfg.Nudge))
	}

	return nil
}

// runPreStart runs cfg.PreStart commands on the host after workdir staging and
// before the ACP agent process is launched. Stage-2 skill/MCP materialization
// is carried in these commands, so failures are fatal rather than allowing an
// agent to start with incomplete runtime state.
func (p *Provider) runPreStart(ctx context.Context, cfg runtime.Config) error {
	for i, command := range cfg.PreStart {
		if err := p.runSetupCommand(ctx, command, cfg); err != nil {
			return fmt.Errorf("pre_start[%d]: %w", i, err)
		}
	}
	return nil
}

func (p *Provider) runSetupCommand(ctx context.Context, command string, cfg runtime.Config) error {
	timeout := p.cfg.setupTimeout()
	idle, grace := time.Duration(0), preStartWaitDelay
	if p.cfg.SetupMaxTimeout > 0 {
		idle, grace = timeout, preStartCancelGrace
	}
	mon := execgrace.NewMonitor(ctx, idle, p.cfg.SetupMaxTimeout)
	defer mon.Stop()
	runCtx := mon.Context()
	if !mon.Enabled() {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	cmd := exec.CommandContext(runCtx, "sh", "-c", command)
	dir, err := p.preStartDir(cfg)
	if err != nil {
		return err
	}
	cmd.Dir = dir
	cmd.Env = commandEnv(cfg.Env)

	var output limitedWriter
	output.max = preStartOutputLimit
	writer := mon.Writer(&output)
	cmd.Stdout, cmd.Stderr = writer, writer
	execgrace.Apply(cmd, grace)

	if err := cmd.Run(); err != nil {
		// ErrWaitDelay means the command exited successfully and only a
		// descendant kept its output pipe open past the bounded wait.
		if errors.Is(err, exec.ErrWaitDelay) {
			return nil
		}
		if ctxErr := context.Cause(runCtx); ctxErr != nil && runCtx.Err() != nil {
			err = fmt.Errorf("%w: %w", ctxErr, err)
		}
		if tail := strings.TrimSpace(output.String()); tail != "" {
			return fmt.Errorf("%w: %s", err, tail)
		}
		return err
	}
	return nil
}

// preStartDir selects a deterministic existing cwd. GC_DIR wins when the
// target workdir already exists. When a PreStart command must create it, use
// the city root; city-less callers fall back to WorkDir itself or its nearest
// existing ancestor. The provider state directory is the final safe fallback.
func (p *Provider) preStartDir(cfg runtime.Config) (string, error) {
	if dir := existingDirectory(cfg.Env["GC_DIR"]); dir != "" {
		return dir, nil
	}
	if dir := existingDirectory(p.cfg.CityRoot); dir != "" {
		return dir, nil
	}
	if dir := existingWorkDirAncestor(cfg.WorkDir); dir != "" {
		return dir, nil
	}
	if dir := existingDirectory(p.dir); dir != "" {
		return dir, nil
	}
	if dir := existingDirectory(os.TempDir()); dir != "" {
		return dir, nil
	}
	return "", fmt.Errorf("selecting pre_start cwd: no existing GC_DIR, city root, workdir ancestor, provider directory, or temp directory")
}

func existingWorkDirAncestor(workDir string) string {
	workDir = strings.TrimSpace(workDir)
	if workDir == "" {
		return ""
	}
	abs, err := filepath.Abs(workDir)
	if err != nil {
		return ""
	}
	for dir := abs; ; dir = filepath.Dir(dir) {
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		if existing := existingDirectory(dir); existing != "" {
			return existing
		}
	}
}

func existingDirectory(dir string) string {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return ""
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return ""
	}
	info, err := os.Stat(abs)
	if err != nil || !info.IsDir() {
		return ""
	}
	return abs
}

func commandEnv(overrides map[string]string) []string {
	env := os.Environ()
	keys := make([]string, 0, len(overrides))
	for key := range overrides {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		env = append(env, key+"="+overrides[key])
	}
	return env
}

// handshake performs the ACP initialize → initialized → session/new sequence.
func (p *Provider) handshake(ctx context.Context, sc *sessionConn, workDir string, mcpServers []runtime.MCPServerConfig) error {
	// Step 1: Send "initialize" request.
	initReq, _ := newInitializeRequest()
	ch, err := sc.sendRequest(initReq)
	if err != nil {
		return fmt.Errorf("sending initialize: %w", err)
	}
	select {
	case resp, ok := <-ch:
		if !ok {
			return fmt.Errorf("connection closed during initialize")
		}
		if resp.Error != nil {
			return fmt.Errorf("initialize error: %s", resp.Error.Message)
		}
	case <-ctx.Done():
		return fmt.Errorf("initialize timeout: %w", ctx.Err())
	case <-sc.done:
		return fmt.Errorf("process exited during initialize")
	}

	// Step 2: Send "initialized" notification.
	if err := sc.sendNotification(newInitializedNotification()); err != nil {
		return fmt.Errorf("sending initialized: %w", err)
	}

	// Step 3: Send "session/new" request.
	newReq, _ := newSessionNewRequest(workDir, mcpServers)
	ch, err = sc.sendRequest(newReq)
	if err != nil {
		return fmt.Errorf("sending session/new: %w", err)
	}
	select {
	case resp, ok := <-ch:
		if !ok {
			return fmt.Errorf("connection closed during session/new")
		}
		if resp.Error != nil {
			return fmt.Errorf("session/new error: %s", resp.Error.Message)
		}
		var result SessionNewResult
		if err := json.Unmarshal(resp.Result, &result); err != nil {
			return fmt.Errorf("decoding session/new result: %w", err)
		}
		sc.mu.Lock()
		sc.sessionID = result.SessionID
		sc.mu.Unlock()
	case <-ctx.Done():
		return fmt.Errorf("session/new timeout: %w", ctx.Err())
	case <-sc.done:
		return fmt.Errorf("process exited during session/new")
	}

	return nil
}

// Stop terminates the named session. Returns nil if it doesn't exist
// (idempotent). Sends SIGTERM first, then SIGKILL after a grace period.
func (p *Provider) Stop(name string) error {
	p.mu.Lock()
	sc, ok := p.conns[name]
	// Keep an in-progress startup sentinel reserved until Start observes the
	// cancellation and unwinds. Deleting it here would let a replacement Start
	// run PreStart against the same workdir while the canceled command's
	// rollback trap is still active. Deliver cancellation while holding the
	// provider lock so Start cannot commit between sentinel lookup and cancel.
	if ok && sc.cmd == nil {
		if sc.cancel != nil {
			sc.cancel()
		}
		p.mu.Unlock()
		return nil
	}
	if ok {
		delete(p.conns, name)
	}
	p.mu.Unlock()

	if ok {
		if !sc.alive() {
			p.cleanupMeta(name)
			return nil
		}
		_ = sc.stdin.Close()
		err := terminateProcess(sc)
		if err == nil || runtime.IsSessionGone(err) {
			p.cleanupMeta(name)
			return nil
		}
		return err
	}

	// Fall back to socket (cross-process case).
	err := p.stopBySocket(name)
	if err == nil || runtime.IsSessionGone(err) {
		p.cleanupMeta(name)
		return nil
	}
	return err
}

// Interrupt sends SIGINT to the named session's process.
// Best-effort: returns nil if the session doesn't exist.
func (p *Provider) Interrupt(name string) error {
	p.mu.Lock()
	sc, ok := p.conns[name]
	p.mu.Unlock()
	if ok {
		// Guard against sentinel sessionConn (nil cmd during handshake).
		if sc.cmd == nil {
			return nil
		}
		return syscall.Kill(-sc.cmd.Process.Pid, syscall.SIGINT)
	}

	// Fall back to socket (cross-process case).
	_ = p.sendSocketCommand(name, "interrupt", 2*time.Second)
	return nil
}

// IsRunning reports whether the named session has a live process.
func (p *Provider) IsRunning(name string) bool {
	p.mu.Lock()
	sc, ok := p.conns[name]
	p.mu.Unlock()

	if ok {
		return sc.alive()
	}
	return p.socketAlive(name)
}

// IsAttached always returns false — ACP sessions have no terminal.
func (p *Provider) IsAttached(_ string) bool { return false }

// Attach is not supported by the ACP provider.
func (p *Provider) Attach(_ string) error {
	return fmt.Errorf("acp provider does not support attach")
}

// ProcessAlive delegates to IsRunning. Returns true when processNames is
// empty (per the Provider contract).
func (p *Provider) ProcessAlive(name string, processNames []string) bool {
	if len(processNames) == 0 {
		return true
	}
	return p.IsRunning(name)
}

// Nudge sends a session/prompt to the named session. Waits for the agent to
// become idle before sending. Returns ErrSessionNotFound when this provider
// instance does not own the in-memory ACP connection. Returns nil if the
// agent process exits during the send (best-effort).
func (p *Provider) Nudge(name string, content []runtime.ContentBlock) error {
	p.mu.Lock()
	sc, ok := p.conns[name]
	p.mu.Unlock()
	if !ok {
		return fmt.Errorf("%w: ACP provider does not own session %q", runtime.ErrSessionNotFound, name)
	}
	if !sc.alive() {
		return nil
	}

	// Serialize nudges per-session so that waitIdle → setActivePrompt →
	// sendRequest is atomic with respect to other concurrent Nudge calls.
	sc.nudgeMu.Lock()
	defer sc.nudgeMu.Unlock()

	// Re-check liveness under the lock. If an earlier Nudge observed the
	// process exit and returned nil while we were queued on nudgeMu, skip
	// the marshal+write work instead of tripping through the recovery path.
	if !sc.alive() {
		return nil
	}

	// Wait for agent to become idle.
	if !sc.waitIdle(p.cfg.nudgeBusyTimeout()) {
		return fmt.Errorf("agent %q busy, timed out waiting for idle", name)
	}

	sc.mu.Lock()
	sessID := sc.sessionID
	sc.mu.Unlock()
	if sessID == "" {
		return fmt.Errorf("session %q has no ACP session ID", name)
	}

	msg, id := newSessionPromptRequest(sessID, content)

	// Set busy state BEFORE sendRequest so that dispatch can match the
	// response ID and clear it. If we set it after, a fast agent could
	// respond before setActivePrompt runs, leaving busy set permanently.
	sc.setActivePrompt(id)

	ch, err := sc.sendRequest(msg)
	if err != nil {
		sc.clearActivePrompt(id)
		// Non-pipe failures (e.g., marshal errors) have nothing to do with
		// the agent lifecycle, so surface them immediately rather than
		// stalling the caller on sc.done.
		if !isPipeWriteError(err) {
			return fmt.Errorf("sending prompt to %q: %w", name, err)
		}
		// Pipe write failed — the agent process is exiting (e.g., a prior
		// Interrupt delivered SIGINT and the agent died, or Stop closed
		// our stdin end between the alive() check and the write).
		// Sync on the existing lifecycle event: cmd.Wait() → drainPending →
		// close(sc.done). Once that fires, this is identical to the
		// !sc.alive() case above, so honor the best-effort contract by
		// returning nil. The bound matches terminateProcess's SIGTERM grace
		// period; the common path returns in microseconds.
		select {
		case <-sc.done:
			// A chronically flapping agent would otherwise be silent here;
			// a single stderr line lets ops distinguish "nothing happened"
			// from "agent died mid-write."
			fmt.Fprintf(os.Stderr, "acp: nudge to %q skipped (agent exiting): %v\n", name, err)
			return nil
		case <-time.After(nudgePostWriteDrainTimeout):
			return fmt.Errorf("sending prompt to %q: %w", name, err)
		}
	}

	// Drain the response channel in the background. If the agent
	// returns a JSON-RPC error, log it rather than silently dropping.
	go func() {
		resp, ok := <-ch
		if !ok {
			return // connection closed, drainPending already cleaned up
		}
		if resp.Error != nil {
			// Best we can do: log via stderr. The prompt was sent, so
			// the error is informational, not actionable by the caller.
			fmt.Fprintf(os.Stderr, "acp: prompt error for %q: %s\n", name, resp.Error.Message)
		}
	}()

	return nil
}

// Pending reports structured pending interactions. ACP only tracks whether an
// outbound prompt is in flight; that busy state is not a user-facing blocking
// interaction, so the provider intentionally reports this capability as
// unsupported.
func (p *Provider) Pending(_ string) (*runtime.PendingInteraction, error) {
	return nil, runtime.ErrInteractionUnsupported
}

// Respond resolves a pending structured interaction. ACP does not currently
// expose those interactions over the protocol, so responses are unsupported.
func (p *Provider) Respond(_ string, _ runtime.InteractionResponse) error {
	return runtime.ErrInteractionUnsupported
}

// SendKeys is a no-op for ACP sessions (no terminal).
func (p *Provider) SendKeys(_ string, _ ...string) error {
	return nil
}

// RunLive is a no-op for ACP sessions.
func (p *Provider) RunLive(_ string, _ runtime.Config) error {
	return nil
}

// Peek returns the last N lines of captured output from session/update
// notifications.
func (p *Provider) Peek(name string, lines int) (string, error) {
	p.mu.Lock()
	sc, ok := p.conns[name]
	p.mu.Unlock()
	if !ok {
		return "", nil
	}
	return sc.peekLines(lines), nil
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

// GetLastActivity returns the time of the last session/update notification.
func (p *Provider) GetLastActivity(name string) (time.Time, error) {
	p.mu.Lock()
	sc, ok := p.conns[name]
	p.mu.Unlock()
	if !ok {
		return time.Time{}, nil
	}
	return sc.getLastActivity(), nil
}

// ClearScrollback clears the output buffer.
func (p *Provider) ClearScrollback(name string) error {
	p.mu.Lock()
	sc, ok := p.conns[name]
	p.mu.Unlock()
	if ok {
		sc.clearOutput()
	}
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
	entries, err := os.ReadDir(p.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var names []string
	for _, e := range entries {
		n := e.Name()
		if !strings.HasSuffix(n, ".sock") {
			continue
		}
		sn := p.socketNameForEntry(strings.TrimSuffix(n, ".sock"))
		if !strings.HasPrefix(sn, prefix) {
			continue
		}
		if p.socketAlive(sn) {
			names = append(names, sn)
		}
	}
	return names, nil
}

func (p *Provider) metaPath(name, key string) string {
	return filepath.Join(p.dir, metaFilePrefix(name)+".meta."+metaFileKey(key))
}

// cleanupMeta removes all sidecar meta files for the named session.
func (p *Provider) cleanupMeta(name string) {
	matches, _ := filepath.Glob(filepath.Join(p.dir, metaFilePrefix(name)+".meta.*"))
	for _, m := range matches {
		os.Remove(m) //nolint:errcheck
	}
}

func metaFilePrefix(name string) string {
	return "m" + metaFileKey(name)
}

func metaFileKey(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

// --- Unix socket helpers (same as subprocess) ---

func (p *Provider) legacySockPath(name string) string {
	return filepath.Join(p.dir, name+".sock")
}

func (p *Provider) sockKey(name string) string {
	sum := sha256.Sum256([]byte(name))
	return "s" + hex.EncodeToString(sum[:4])
}

func (p *Provider) sockPath(name string) string {
	return filepath.Join(p.dir, p.sockKey(name)+".sock")
}

func (p *Provider) sockNamePath(name string) string {
	return filepath.Join(p.dir, p.sockKey(name)+".name")
}

func (p *Provider) socketNameForEntry(key string) string {
	data, err := os.ReadFile(filepath.Join(p.dir, key+".name"))
	if err != nil {
		return key
	}
	name := strings.TrimSpace(string(data))
	if name == "" {
		return key
	}
	return name
}

// startControlSocket creates a unix socket for cross-process commands.
func (p *Provider) startControlSocket(name string, cmd *exec.Cmd, done <-chan struct{}) (net.Listener, error) {
	sp := p.sockPath(name)
	namePath := p.sockNamePath(name)
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
				return
			}
			go handleControlConn(conn, cmd, done)
		}
	}()
	return lis, nil
}

// handleControlConn reads a command from the connection and acts on the process.
func handleControlConn(conn net.Conn, cmd *exec.Cmd, done <-chan struct{}) {
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
	for _, sp := range []string{p.sockPath(name), p.legacySockPath(name)} {
		conn, err := net.DialTimeout("unix", sp, 500*time.Millisecond)
		if err != nil {
			continue
		}
		_ = conn.Close()
		if p.sendSocketCommand(name, "ping", 500*time.Millisecond) == nil {
			return true
		}
	}
	return false
}

// sendSocketCommand connects to the session's control socket and sends a command.
func (p *Provider) sendSocketCommand(name, command string, timeout time.Duration) error {
	var (
		lastErr            error
		firstActionableErr error
	)
	for _, sp := range []string{p.sockPath(name), p.legacySockPath(name)} {
		err := func(path string) error {
			conn, err := net.DialTimeout("unix", path, timeout)
			if err != nil {
				return err
			}
			defer conn.Close()                        //nolint:errcheck
			conn.SetDeadline(time.Now().Add(timeout)) //nolint:errcheck
			_, err = fmt.Fprintf(conn, "%s\n", command)
			if err != nil {
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

// Capabilities reports ACP provider capabilities. The ACP provider has
// no terminal and does not natively support attachment or activity detection.
func (p *Provider) Capabilities() runtime.ProviderCapabilities {
	return runtime.ProviderCapabilities{}
}

// SleepCapability reports that ACP sessions support timed-only idle sleep.
func (p *Provider) SleepCapability(string) runtime.SessionSleepCapability {
	return runtime.SessionSleepCapabilityTimedOnly
}

// isPipeWriteError reports whether err originated from writing to a closed
// stdin pipe — the signal that the agent process exited between our alive()
// check and the write. Other sendRequest failures (marshal errors, etc.) are
// unrelated to lifecycle and should surface immediately.
func isPipeWriteError(err error) bool {
	return errors.Is(err, io.ErrClosedPipe) || errors.Is(err, syscall.EPIPE)
}

// terminateProcess sends SIGTERM then SIGKILL to a tracked process group.
func terminateProcess(sc *sessionConn) error {
	return runtime.TerminateManagedProcess(sc.cmd, sc.done, runtime.ManagedProcessStopGrace)
}
