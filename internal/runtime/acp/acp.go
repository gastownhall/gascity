package acp

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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

	"github.com/gastownhall/gascity/internal/overlay"
	"github.com/gastownhall/gascity/internal/runtime"
)

// Config holds ACP provider settings.
type Config struct {
	HandshakeTimeout  time.Duration // default 30s
	NudgeBusyTimeout  time.Duration // default 60s
	OutputBufferLines int           // default 1000
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
	_ runtime.Provider            = (*Provider)(nil)
	_ runtime.InteractionProvider = (*Provider)(nil)
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
	sentinel := &sessionConn{done: make(chan struct{}), cancel: hsCancel, pending: make(map[int64]chan JSONRPCMessage)}
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

	// Copy overlay and CopyFiles before starting the process.
	if cfg.OverlayDir != "" && cfg.WorkDir != "" {
		_ = overlay.CopyDir(cfg.OverlayDir, cfg.WorkDir, io.Discard)
	}
	for _, cf := range cfg.CopyFiles {
		dst := cfg.WorkDir
		if cf.RelDst != "" {
			dst = filepath.Join(cfg.WorkDir, cf.RelDst)
		}
		if absSrc, err := filepath.Abs(cf.Src); err == nil {
			if absDst, err := filepath.Abs(dst); err == nil && absSrc == absDst {
				continue
			}
		}
		_ = overlay.CopyFileOrDir(cf.Src, dst, io.Discard)
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

	cmd := exec.Command("sh", "-c", command)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if cfg.WorkDir != "" {
		cmd.Dir = cfg.WorkDir
	}

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
	lis, err := p.startControlSocket(name, cmd)
	if err != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
		clearSentinel()
		return fmt.Errorf("creating control socket for %q: %w", name, err)
	}

	sc := newSessionConn(cmd, stdinPipe, lis, p.cfg.outputBufferLines())

	// Start readLoop before handshake so we can receive responses.
	go sc.readLoop(stdoutPipe)

	// Monitor process exit — clean up pending state, socket, and listener.
	go func() {
		_ = cmd.Wait()
		sc.drainPending()
		close(sc.done)
		lis.Close()                 //nolint:errcheck
		os.Remove(p.sockPath(name)) //nolint:errcheck
		os.Remove(p.sockNamePath(name))
	}()

	// Perform ACP handshake with a deadline. hsCtx (created above with
	// WithCancelCause) is already cancellable by Stop. Add a timeout
	// child so handshake_timeout applies even when the parent has a
	// longer deadline.
	hsTimeoutCtx, hsTimeoutCancel := context.WithTimeout(hsCtx, p.cfg.handshakeTimeout())
	defer hsTimeoutCancel()

	if err := p.handshake(hsTimeoutCtx, sc); err != nil {
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

	// Before committing the real conn, check whether Stop was called
	// during the handshake (which cancels hsCtx). If so, kill the process
	// and clean up — the caller of Stop expects the session to be gone.
	if err := hsCtx.Err(); err != nil {
		_ = stdinPipe.Close()
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-sc.done
		clearSentinel()
		return fmt.Errorf("session %q was stopped during startup", name)
	}

	p.mu.Lock()
	p.conns[name] = sc
	p.mu.Unlock()

	// Send initial nudge if configured (best-effort, outside lock).
	if cfg.Nudge != "" {
		_ = p.Nudge(name, runtime.TextContent(cfg.Nudge))
	}

	return nil
}

// handshake performs the ACP initialize → initialized → session/new sequence.
func (p *Provider) handshake(ctx context.Context, sc *sessionConn) error {
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
	newReq, _ := newSessionNewRequest()
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
	if ok {
		delete(p.conns, name)
	}
	p.mu.Unlock()

	if ok {
		if !sc.alive() {
			p.cleanupMeta(name)
			return nil
		}
		// Guard against sentinel sessionConn (nil cmd/stdin during handshake).
		// Signal the in-progress handshake to abort via the cancel func.
		if sc.cmd == nil {
			if sc.cancel != nil {
				sc.cancel()
			}
			return nil
		}
		_ = sc.stdin.Close()
		err := terminateProcess(sc)
		p.cleanupMeta(name)
		return err
	}

	// Fall back to socket (cross-process case).
	err := p.stopBySocket(name)
	p.cleanupMeta(name)
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
// become idle before sending. Returns nil if the session doesn't exist
// (best-effort).
func (p *Provider) Nudge(name string, content []runtime.ContentBlock) error {
	p.mu.Lock()
	sc, ok := p.conns[name]
	p.mu.Unlock()
	if !ok {
		return nil
	}
	if !sc.alive() {
		return nil
	}

	// Serialize nudges per-session so that waitIdle → setActivePrompt →
	// sendRequest is atomic with respect to other concurrent Nudge calls.
	sc.nudgeMu.Lock()
	defer sc.nudgeMu.Unlock()

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
		// Clear busy state on send failure.
		sc.mu.Lock()
		if sc.activePromptID == id {
			sc.activePromptID = 0
		}
		sc.mu.Unlock()
		return fmt.Errorf("sending prompt to %q: %w", name, err)
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
	return overlay.CopyFileOrDir(src, dst, io.Discard)
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
	return filepath.Join(p.dir, name+".meta."+key)
}

// cleanupMeta removes all sidecar meta files for the named session.
func (p *Provider) cleanupMeta(name string) {
	matches, _ := filepath.Glob(filepath.Join(p.dir, name+".meta.*"))
	for _, m := range matches {
		os.Remove(m) //nolint:errcheck
	}
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
func (p *Provider) startControlSocket(name string, cmd *exec.Cmd) (net.Listener, error) {
	sp := p.sockPath(name)
	namePath := p.sockNamePath(name)
	os.Remove(sp) //nolint:errcheck
	os.Remove(namePath)
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
			go handleControlConn(conn, cmd)
		}
	}()
	return lis, nil
}

// handleControlConn reads a command from the connection and acts on the process.
func handleControlConn(conn net.Conn, cmd *exec.Cmd) {
	defer conn.Close()                                     //nolint:errcheck
	conn.SetReadDeadline(time.Now().Add(10 * time.Second)) //nolint:errcheck
	scanner := bufio.NewScanner(conn)
	if !scanner.Scan() {
		return
	}
	switch scanner.Text() {
	case "stop":
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		deadline := time.After(5 * time.Second)
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		alive := true
		for alive {
			select {
			case <-deadline:
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
				alive = false
			case <-ticker.C:
				if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
					alive = false
				}
			}
		}
		conn.Write([]byte("ok\n")) //nolint:errcheck
	case "interrupt":
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGINT)
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
	var lastErr error
	for _, sp := range []string{p.sockPath(name), p.legacySockPath(name)} {
		conn, err := net.DialTimeout("unix", sp, timeout)
		if err != nil {
			lastErr = err
			continue
		}
		defer conn.Close()                        //nolint:errcheck
		conn.SetDeadline(time.Now().Add(timeout)) //nolint:errcheck
		_, err = fmt.Fprintf(conn, "%s\n", command)
		if err != nil {
			lastErr = err
			continue
		}
		scanner := bufio.NewScanner(conn)
		if scanner.Scan() && scanner.Text() == "ok" {
			return nil
		}
		if err := scanner.Err(); err != nil {
			lastErr = err
			continue
		}
		lastErr = fmt.Errorf("unexpected response from socket")
	}
	return lastErr
}

// stopBySocket connects to a session's control socket and asks it to stop.
func (p *Provider) stopBySocket(name string) error {
	err := p.sendSocketCommand(name, "stop", 7*time.Second)
	if err != nil {
		os.Remove(p.sockPath(name)) //nolint:errcheck
		os.Remove(p.sockNamePath(name))
		return nil
	}
	return nil
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

// terminateProcess sends SIGTERM then SIGKILL to a tracked process group.
func terminateProcess(sc *sessionConn) error {
	_ = syscall.Kill(-sc.cmd.Process.Pid, syscall.SIGTERM)
	select {
	case <-sc.done:
		return nil
	case <-time.After(5 * time.Second):
	}
	_ = syscall.Kill(-sc.cmd.Process.Pid, syscall.SIGKILL)
	<-sc.done
	return nil
}
