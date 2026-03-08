package tmux

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/overlay"
	"github.com/gastownhall/gascity/internal/runtime"
)

// Provider adapts [Tmux] to the [runtime.Provider] interface.
type Provider struct {
	tm       *Tmux
	cfg      Config
	mu       sync.Mutex
	workDirs map[string]string // session name → workDir (for CopyTo)
}

// Compile-time check.
var _ runtime.Provider = (*Provider)(nil)

// NewProvider returns a [Provider] backed by a real tmux installation
// with default configuration.
func NewProvider() *Provider {
	return NewProviderWithConfig(DefaultConfig())
}

// NewProviderWithConfig returns a [Provider] with the given configuration.
func NewProviderWithConfig(cfg Config) *Provider {
	return &Provider{
		tm:       NewTmuxWithConfig(cfg),
		cfg:      cfg,
		workDirs: make(map[string]string),
	}
}

// Start creates a new detached tmux session and performs a multi-step
// startup sequence to ensure agent readiness. The sequence handles zombie
// detection, command launch verification, permission warning dismissal,
// and runtime readiness polling. Steps are conditional on Config fields
// being set; an agent with no startup hints gets fire-and-forget.
func (p *Provider) Start(ctx context.Context, name string, cfg runtime.Config) error {
	// Store workDir for CopyTo.
	if cfg.WorkDir != "" {
		p.mu.Lock()
		p.workDirs[name] = cfg.WorkDir
		p.mu.Unlock()
	}

	// Copy overlays and CopyFiles before creating the tmux session.
	// Local provider: files are on the same filesystem.
	// Pack-level overlays (lower priority, merged additively).
	if cfg.WorkDir != "" {
		for _, od := range cfg.PackOverlayDirs {
			_ = overlay.CopyDir(od, cfg.WorkDir, io.Discard)
		}
	}
	// Agent-level overlay (highest priority, overwrites on conflict).
	if cfg.OverlayDir != "" && cfg.WorkDir != "" {
		_ = overlay.CopyDir(cfg.OverlayDir, cfg.WorkDir, io.Discard)
	}
	for _, cf := range cfg.CopyFiles {
		dst := cfg.WorkDir
		if cf.RelDst != "" {
			dst = filepath.Join(cfg.WorkDir, cf.RelDst)
		}
		// Skip if src and dst are the same path.
		if absSrc, err := filepath.Abs(cf.Src); err == nil {
			if absDst, err := filepath.Abs(dst); err == nil && absSrc == absDst {
				continue
			}
		}
		_ = overlay.CopyFileOrDir(cf.Src, dst, io.Discard)
	}

	return doStartSession(ctx, &tmuxStartOps{tm: p.tm}, name, cfg, p.cfg.SetupTimeout)
}

// RunLive re-applies session_live commands to a running session.
// Called by the reconciler when only session_live config has changed.
func (p *Provider) RunLive(name string, cfg runtime.Config) error {
	runSessionLive(context.Background(), &tmuxStartOps{tm: p.tm}, name, cfg, os.Stderr, p.cfg.SetupTimeout)
	return nil
}

// Stop destroys the named session and kills its entire process tree.
// Returns nil if it doesn't exist (idempotent).
func (p *Provider) Stop(name string) error {
	err := p.tm.KillSessionWithProcesses(name)
	if err != nil && (errors.Is(err, ErrSessionNotFound) || errors.Is(err, ErrNoServer)) {
		return nil // idempotent
	}
	return err
}

// Interrupt sends Ctrl-C to the named tmux session.
// Best-effort: returns nil if the session doesn't exist.
func (p *Provider) Interrupt(name string) error {
	err := p.tm.SendKeysRaw(name, "C-c")
	if err != nil && (errors.Is(err, ErrSessionNotFound) || errors.Is(err, ErrNoServer)) {
		return nil
	}
	return err
}

// IsRunning reports whether the named session exists.
func (p *Provider) IsRunning(name string) bool {
	has, err := p.tm.HasSession(name)
	return err == nil && has
}

// IsAttached reports whether a user terminal is connected to the named session.
func (p *Provider) IsAttached(name string) bool {
	return p.tm.IsSessionAttached(name)
}

// ProcessAlive reports whether the named session has a live agent
// process matching one of the given names in its process tree.
// Returns true if processNames is empty (no check possible).
func (p *Provider) ProcessAlive(name string, processNames []string) bool {
	if len(processNames) == 0 {
		return true
	}
	return p.tm.IsRuntimeRunning(name, processNames)
}

// Capabilities reports tmux provider capabilities.
// Tmux supports both attachment detection and activity reporting.
func (p *Provider) Capabilities() runtime.ProviderCapabilities {
	return runtime.ProviderCapabilities{
		CanReportAttachment: true,
		CanReportActivity:   true,
	}
}

// Nudge sends a message to the named session to wake or redirect the agent.
// By default, waits for the agent to be idle before sending (wait-idle mode)
// to avoid interrupting active tool calls. If the agent doesn't become idle
// within NudgeIdleTimeout, sends immediately as a fallback.
// Delegates to [Tmux.NudgeSession] which handles per-session locking,
// multi-pane resolution, retry with backoff, and SIGWINCH wake.
// Best-effort: returns nil if the session doesn't exist.
func (p *Provider) Nudge(name, message string) error {
	// Wait for the agent to be idle before sending, unless disabled.
	// This prevents interrupting active tool calls — the prompt is visible
	// in scrollback during inter-tool-call gaps, so immediate send-keys
	// would inject text mid-execution. See upstream dfd945e9/6bc898ce.
	if idleTimeout := p.tm.cfg.NudgeIdleTimeout; idleTimeout > 0 {
		// Best-effort wait — if it fails (session gone, timeout), proceed
		// with the nudge anyway. The message may arrive during active work,
		// but Claude's cooperative queue will handle it at the next turn.
		_ = p.tm.WaitForIdle(name, idleTimeout)
	}

	err := p.tm.NudgeSession(name, message)
	if err != nil && (errors.Is(err, ErrSessionNotFound) || errors.Is(err, ErrNoServer)) {
		return nil
	}
	return err
}

// SetMeta stores a key-value pair in the named session's tmux environment.
func (p *Provider) SetMeta(name, key, value string) error {
	return p.tm.SetEnvironment(name, key, value)
}

// GetMeta retrieves a value from the named session's tmux environment.
// Returns ("", nil) if the key is not set. Propagates session-not-found
// and no-server errors so callers can distinguish "key absent" from
// "session gone."
func (p *Provider) GetMeta(name, key string) (string, error) {
	val, err := p.tm.GetEnvironment(name, key)
	if err != nil {
		if errors.Is(err, ErrSessionNotFound) || errors.Is(err, ErrNoServer) {
			return "", err
		}
		return "", nil // key not set
	}
	return val, nil
}

// RemoveMeta removes a key from the named session's tmux environment.
func (p *Provider) RemoveMeta(name, key string) error {
	return p.tm.RemoveEnvironment(name, key)
}

// Peek captures the last N lines of output from the named session.
// If lines <= 0, captures all available scrollback.
func (p *Provider) Peek(name string, lines int) (string, error) {
	if lines <= 0 {
		return p.tm.CapturePaneAll(name)
	}
	return p.tm.CapturePane(name, lines)
}

// ListRunning returns all tmux session names matching the given prefix.
func (p *Provider) ListRunning(prefix string) ([]string, error) {
	all, err := p.tm.ListSessions()
	if err != nil {
		return nil, err
	}
	var matched []string
	for _, name := range all {
		if strings.HasPrefix(name, prefix) {
			matched = append(matched, name)
		}
	}
	return matched, nil
}

// GetLastActivity returns the time of the last I/O activity in the named
// session. Delegates to [Tmux.GetSessionActivity].
func (p *Provider) GetLastActivity(name string) (time.Time, error) {
	return p.tm.GetSessionActivity(name)
}

// ClearScrollback clears the scrollback history of the named session.
// Delegates to [Tmux.ClearHistory].
func (p *Provider) ClearScrollback(name string) error {
	return p.tm.ClearHistory(name)
}

// SendKeys sends bare keystrokes to the named session. Each key is sent
// as a separate tmux send-keys invocation (e.g., "Enter", "Down", "C-c").
// Best-effort: returns nil if the session doesn't exist.
func (p *Provider) SendKeys(name string, keys ...string) error {
	for _, k := range keys {
		err := p.tm.SendKeysRaw(name, k)
		if err != nil && (errors.Is(err, ErrSessionNotFound) || errors.Is(err, ErrNoServer)) {
			return nil // best-effort
		}
		if err != nil {
			return err
		}
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
		return nil // unknown session
	}
	if _, err := os.Stat(src); err != nil {
		return nil // src missing
	}
	dst := wd
	if relDst != "" {
		dst = filepath.Join(wd, relDst)
	}
	return overlay.CopyDir(src, dst, io.Discard)
}

// Attach connects the user's terminal to the named tmux session.
// This hands stdin/stdout/stderr to tmux and blocks until detach.
func (p *Provider) Attach(name string) error {
	args := []string{"-u"}
	if p.cfg.SocketName != "" {
		args = append(args, "-L", p.cfg.SocketName)
	}
	args = append(args, "attach-session", "-t", name)
	cmd := exec.Command("tmux", args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// Tmux returns the underlying [Tmux] instance for advanced operations
// that are not part of the [runtime.Provider] interface.
func (p *Provider) Tmux() *Tmux {
	return p.tm
}

// ---------------------------------------------------------------------------
// Multi-step startup orchestration
// ---------------------------------------------------------------------------

// startOps abstracts tmux operations needed by the startup sequence.
// This enables unit testing without a real tmux server.
type startOps interface {
	createSession(name, workDir, command string, env map[string]string) error
	isRuntimeRunning(name string, processNames []string) bool
	killSession(name string) error
	waitForCommand(ctx context.Context, name string, timeout time.Duration) error
	acceptStartupDialogs(ctx context.Context, name string) error
	waitForReady(ctx context.Context, name string, rc *RuntimeConfig, timeout time.Duration) error
	hasSession(name string) (bool, error)
	sendKeys(name, text string) error
	setRemainOnExit(name string) error
	runSetupCommand(ctx context.Context, cmd string, env map[string]string, timeout time.Duration) error
}

// tmuxStartOps adapts [*Tmux] to the [startOps] interface.
type tmuxStartOps struct{ tm *Tmux }

func (o *tmuxStartOps) createSession(name, workDir, command string, env map[string]string) error {
	if command != "" || len(env) > 0 {
		return o.tm.NewSessionWithCommandAndEnv(name, workDir, command, env)
	}
	return o.tm.NewSession(name, workDir)
}

func (o *tmuxStartOps) isRuntimeRunning(name string, processNames []string) bool {
	return o.tm.IsRuntimeRunning(name, processNames)
}

func (o *tmuxStartOps) killSession(name string) error {
	return o.tm.KillSessionWithProcesses(name)
}

func (o *tmuxStartOps) waitForCommand(ctx context.Context, name string, timeout time.Duration) error {
	return o.tm.WaitForCommand(ctx, name, supportedShells, timeout)
}

func (o *tmuxStartOps) acceptStartupDialogs(ctx context.Context, name string) error {
	return o.tm.AcceptStartupDialogs(ctx, name)
}

func (o *tmuxStartOps) waitForReady(ctx context.Context, name string, rc *RuntimeConfig, timeout time.Duration) error {
	return o.tm.WaitForRuntimeReady(ctx, name, rc, timeout)
}

func (o *tmuxStartOps) hasSession(name string) (bool, error) {
	return o.tm.HasSession(name)
}

func (o *tmuxStartOps) sendKeys(name, text string) error {
	return o.tm.SendKeys(name, text)
}

func (o *tmuxStartOps) setRemainOnExit(name string) error {
	return o.tm.SetRemainOnExit(name, true)
}

func (o *tmuxStartOps) runSetupCommand(ctx context.Context, cmd string, env map[string]string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	c := exec.CommandContext(ctx, "sh", "-c", cmd)
	c.Env = os.Environ()
	for k, v := range env {
		c.Env = append(c.Env, k+"="+v)
	}
	// Expose the tmux socket name so session_setup scripts can use
	// "tmux -L $GC_TMUX_SOCKET" to reach the correct server.
	if o.tm.cfg.SocketName != "" {
		c.Env = append(c.Env, "GC_TMUX_SOCKET="+o.tm.cfg.SocketName)
	}
	return c.Run()
}

// doStartSession is the pure startup orchestration logic.
// Testable via fakeStartOps without a real tmux server.
// The setupTimeout parameter controls the per-command timeout for
// session_setup, session_setup_script, and pre_start commands.
func doStartSession(ctx context.Context, ops startOps, name string, cfg runtime.Config, setupTimeout time.Duration) error {
	// Step 0: Run pre-start commands (directory/worktree preparation).
	runPreStart(ctx, ops, name, cfg, os.Stderr, setupTimeout)

	// Step 1: Ensure fresh session (zombie detection).
	if err := ensureFreshSession(ops, name, cfg); err != nil {
		return err
	}

	// Enable remain-on-exit for crash forensics. Best-effort.
	_ = ops.setRemainOnExit(name)

	hasHints := cfg.ReadyPromptPrefix != "" || cfg.ReadyDelayMs > 0 ||
		len(cfg.ProcessNames) > 0 || cfg.EmitsPermissionWarning ||
		cfg.Nudge != "" || len(cfg.PreStart) > 0 || len(cfg.SessionSetup) > 0 || cfg.SessionSetupScript != "" ||
		len(cfg.SessionLive) > 0

	if !hasHints {
		return nil // fire-and-forget
	}

	// Step 2: Wait for agent command to appear (not still in shell).
	if len(cfg.ProcessNames) > 0 {
		_ = ops.waitForCommand(ctx, name, 30*time.Second) // best-effort, non-fatal
	}

	// Step 3: Accept startup dialogs (workspace trust + bypass permissions).
	// Always attempted when process names are set, since any Claude-like
	// agent may show a trust dialog regardless of EmitsPermissionWarning.
	if len(cfg.ProcessNames) > 0 || cfg.EmitsPermissionWarning {
		_ = ops.acceptStartupDialogs(ctx, name) // best-effort
	}

	// Step 4: Wait for runtime readiness.
	if cfg.ReadyPromptPrefix != "" || cfg.ReadyDelayMs > 0 {
		rc := &RuntimeConfig{Tmux: &RuntimeTmuxConfig{
			ReadyPromptPrefix: cfg.ReadyPromptPrefix,
			ReadyDelayMs:      cfg.ReadyDelayMs,
			ProcessNames:      cfg.ProcessNames,
		}}
		_ = ops.waitForReady(ctx, name, rc, 60*time.Second) // best-effort
	}

	// Step 5: Verify session survived startup.
	alive, err := ops.hasSession(name)
	if err != nil {
		return fmt.Errorf("verifying session: %w", err)
	}
	if !alive {
		return fmt.Errorf("session %q died during startup", name)
	}

	// Step 5.5: Run session setup commands and script.
	runSessionSetup(ctx, ops, name, cfg, os.Stderr, setupTimeout)

	// Step 6: Send nudge text if configured.
	if cfg.Nudge != "" {
		_ = ops.sendKeys(name, cfg.Nudge) // best-effort
	}

	// Step 6.5: Run session_live commands (idempotent, re-applicable).
	runSessionLive(ctx, ops, name, cfg, os.Stderr, setupTimeout)

	return nil
}

// runSessionSetup runs session_setup commands then session_setup_script.
// Non-fatal: warnings on failure, session still works.
func runSessionSetup(ctx context.Context, ops startOps, name string, cfg runtime.Config, stderr io.Writer, setupTimeout time.Duration) {
	if len(cfg.SessionSetup) == 0 && cfg.SessionSetupScript == "" {
		return
	}

	// Build env vars for setup commands/script.
	setupEnv := make(map[string]string, len(cfg.Env)+1)
	for k, v := range cfg.Env {
		setupEnv[k] = v
	}
	setupEnv["GC_SESSION"] = name

	// Run inline commands in order.
	for i, cmd := range cfg.SessionSetup {
		if err := ops.runSetupCommand(ctx, cmd, setupEnv, setupTimeout); err != nil {
			_, _ = fmt.Fprintf(stderr, "gc: session_setup[%d] warning: %v\n", i, err)
		}
	}

	// Run script if configured.
	if cfg.SessionSetupScript != "" {
		if err := ops.runSetupCommand(ctx, cfg.SessionSetupScript, setupEnv, setupTimeout); err != nil {
			_, _ = fmt.Fprintf(stderr, "gc: session_setup_script warning: %v\n", err)
		}
	}
}

// runSessionLive runs session_live commands (idempotent, re-applicable).
// Called at startup after nudge, and by the reconciler on live-only drift.
// Non-fatal: warnings on failure, session still works.
func runSessionLive(ctx context.Context, ops startOps, name string, cfg runtime.Config, stderr io.Writer, setupTimeout time.Duration) {
	if len(cfg.SessionLive) == 0 {
		return
	}

	// Build env vars for live commands.
	setupEnv := make(map[string]string, len(cfg.Env)+1)
	for k, v := range cfg.Env {
		setupEnv[k] = v
	}
	setupEnv["GC_SESSION"] = name

	for i, cmd := range cfg.SessionLive {
		if err := ops.runSetupCommand(ctx, cmd, setupEnv, setupTimeout); err != nil {
			_, _ = fmt.Fprintf(stderr, "gc: session_live[%d] warning: %v\n", i, err)
		}
	}
}

// runPreStart runs pre_start commands before session creation.
// Used for directory/worktree preparation. Non-fatal: warnings on failure.
func runPreStart(ctx context.Context, ops startOps, _ string, cfg runtime.Config, stderr io.Writer, setupTimeout time.Duration) {
	if len(cfg.PreStart) == 0 {
		return
	}
	setupEnv := make(map[string]string, len(cfg.Env))
	for k, v := range cfg.Env {
		setupEnv[k] = v
	}
	for i, cmd := range cfg.PreStart {
		if err := ops.runSetupCommand(ctx, cmd, setupEnv, setupTimeout); err != nil {
			_, _ = fmt.Fprintf(stderr, "gc: pre_start[%d] warning: %v\n", i, err)
		}
	}
}

// ensureFreshSession creates a session, handling zombies.
// If the session already exists, returns an error (duplicate detection).
// Exception: if ProcessNames are configured and the agent is dead (zombie),
// kills the zombie session and recreates it.
func ensureFreshSession(ops startOps, name string, cfg runtime.Config) error {
	err := ops.createSession(name, cfg.WorkDir, cfg.Command, cfg.Env)
	if err == nil {
		return nil // created successfully
	}
	if !errors.Is(err, ErrSessionExists) {
		return fmt.Errorf("creating session: %w", err)
	}

	// Session exists — without process names we can't distinguish a zombie
	// from a healthy session, so treat it as a duplicate.
	if len(cfg.ProcessNames) == 0 {
		return fmt.Errorf("%w: session %q", runtime.ErrSessionExists, name)
	}

	// We have process names — check if the agent is alive.
	if ops.isRuntimeRunning(name, cfg.ProcessNames) {
		return fmt.Errorf("%w: session %q", runtime.ErrSessionExists, name)
	}

	// Zombie: tmux alive but agent dead. Kill and recreate.
	if err := ops.killSession(name); err != nil {
		return fmt.Errorf("killing zombie session: %w", err)
	}
	err = ops.createSession(name, cfg.WorkDir, cfg.Command, cfg.Env)
	if errors.Is(err, ErrSessionExists) {
		return nil // race: another process created it
	}
	if err != nil {
		return fmt.Errorf("creating session after zombie cleanup: %w", err)
	}
	return nil
}
