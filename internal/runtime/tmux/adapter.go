package tmux

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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
	"github.com/gastownhall/gascity/internal/shellquote"
)

// Provider adapts [Tmux] to the [runtime.Provider] interface.
type Provider struct {
	tm       *Tmux
	cfg      Config
	cache    *StateCache
	mu       sync.Mutex
	workDirs map[string]string // session name → workDir (for CopyTo)
}

var instanceTokenReader = rand.Reader

// Compile-time check.
var (
	_ runtime.Provider                      = (*Provider)(nil)
	_ runtime.ImmediateNudgeProvider        = (*Provider)(nil)
	_ runtime.InterruptBoundaryWaitProvider = (*Provider)(nil)
	_ runtime.InterruptedTurnResetProvider  = (*Provider)(nil)
)

// NewProvider returns a [Provider] backed by a real tmux installation
// with default configuration.
func NewProvider() *Provider {
	return NewProviderWithConfig(DefaultConfig())
}

// NewProviderWithConfig returns a [Provider] with the given configuration.
func NewProviderWithConfig(cfg Config) *Provider {
	tm := NewTmuxWithConfig(cfg)
	ttl := cacheTTLFromEnv()
	return &Provider{
		tm:       tm,
		cfg:      cfg,
		cache:    NewStateCache(&tmuxFetcher{tm: tm}, ttl),
		workDirs: make(map[string]string),
	}
}

// Start creates a new detached tmux session and performs a multi-step
// startup sequence to ensure agent readiness. The sequence handles zombie
// detection, command launch verification, permission warning dismissal,
// and runtime readiness polling. Steps are conditional on Config fields
// being set; an agent with no startup hints gets fire-and-forget.
func (p *Provider) Start(ctx context.Context, name string, cfg runtime.Config) error {
	var err error
	cfg.Env, err = ensureInstanceToken(cfg.Env)
	if err != nil {
		return fmt.Errorf("ensuring instance token: %w", err)
	}
	cfg.Env = injectSessionRuntimeHintsEnv(cfg.Env, cfg)

	// Store workDir for CopyTo.
	if cfg.WorkDir != "" {
		p.mu.Lock()
		p.workDirs[name] = cfg.WorkDir
		p.mu.Unlock()
	}

	// Copy overlays and CopyFiles before creating the tmux session.
	// Local provider: files are on the same filesystem.
	// V2 per-provider overlay support: CopyDirForProviders copies universal
	// files then per-provider/<provider>/ slots for ProviderName plus any
	// InstallAgentHooks entries (flattened).
	overlayProviders := append([]string{cfg.ProviderName}, cfg.InstallAgentHooks...)
	if cfg.WorkDir != "" {
		for _, od := range cfg.PackOverlayDirs {
			if err := overlay.CopyDirForProviders(od, cfg.WorkDir, overlayProviders, io.Discard); err != nil {
				return fmt.Errorf("copying pack overlay %s: %w", od, err)
			}
		}
	}
	// Agent-level overlay (highest priority; merges known settings files, overwrites others).
	if cfg.OverlayDir != "" && cfg.WorkDir != "" {
		if err := overlay.CopyDirForProviders(cfg.OverlayDir, cfg.WorkDir, overlayProviders, io.Discard); err != nil {
			return fmt.Errorf("copying overlay %s: %w", cfg.OverlayDir, err)
		}
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

	err = doStartSession(ctx, &tmuxStartOps{tm: p.tm}, name, cfg, p.cfg.SetupTimeout)
	if err == nil {
		p.cache.Invalidate()
		return nil
	}
	p.cleanupFailedStart(name, cfg)
	return err
}

func ensureInstanceToken(env map[string]string) (map[string]string, error) {
	cloned := make(map[string]string, len(env)+1)
	for k, v := range env {
		cloned[k] = v
	}
	if strings.TrimSpace(cloned["GC_INSTANCE_TOKEN"]) == "" {
		token, err := newInstanceToken()
		if err != nil {
			return nil, err
		}
		cloned["GC_INSTANCE_TOKEN"] = token
	}
	return cloned, nil
}

func injectSessionRuntimeHintsEnv(env map[string]string, cfg runtime.Config) map[string]string {
	cloned := make(map[string]string, len(env)+1)
	for k, v := range env {
		cloned[k] = v
	}
	if prompt := strings.TrimSpace(cfg.ReadyPromptPrefix); prompt != "" {
		cloned[sessionReadyPromptEnvKey] = cfg.ReadyPromptPrefix
	} else {
		delete(cloned, sessionReadyPromptEnvKey)
	}
	return cloned
}

func newInstanceToken() (string, error) {
	b := make([]byte, 16)
	if _, err := io.ReadFull(instanceTokenReader, b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func (p *Provider) cleanupFailedStart(name string, cfg runtime.Config) {
	instanceToken := strings.TrimSpace(cfg.Env["GC_INSTANCE_TOKEN"])
	if instanceToken == "" {
		// Best-effort safety guard: only managed session starts carry the
		// instance token we can use to prove ownership before killing by name.
		return
	}
	liveToken, err := p.tm.GetEnvironment(name, "GC_INSTANCE_TOKEN")
	if err != nil {
		return
	}
	if strings.TrimSpace(liveToken) != instanceToken {
		return
	}
	if err := p.tm.KillSessionWithProcesses(name); err == nil {
		p.cache.Invalidate()
	}
}

// RunLive re-applies session_live commands to a running session.
// Called by the reconciler when only session_live config has changed.
func (p *Provider) RunLive(name string, cfg runtime.Config) error {
	runSessionLive(context.Background(), &tmuxStartOps{tm: p.tm}, name, cfg, os.Stderr, p.cfg.SetupTimeout)
	return nil
}

// Stop destroys the named session and kills its entire process tree.
// Returns nil if it doesn't exist (idempotent).
// Invalidates the state cache after a successful stop so subsequent
// IsRunning calls see the updated state immediately.
func (p *Provider) Stop(name string) error {
	p.tm.CloseHiddenAttachClient(name)
	err := p.tm.KillSessionWithProcesses(name)
	if err != nil && (errors.Is(err, ErrSessionNotFound) || errors.Is(err, ErrNoServer)) {
		return nil // idempotent
	}
	if err == nil {
		// Immediately remove from cache so IsRunning reflects the kill
		// without waiting for an async refresh cycle.
		p.cache.EvictSession(name)
	}
	return err
}

// Interrupt sends Ctrl-C to the named tmux session.
// Best-effort: returns nil if the session doesn't exist.
func (p *Provider) Interrupt(name string) error {
	if p.tm.requiresHiddenAttachedInterrupt(name) && !p.tm.IsSessionAttached(name) {
		if err := p.tm.ensureHiddenAttachedClient(name); err != nil {
			return fmt.Errorf("preparing detached gemini interrupt: %w", err)
		}
	}
	if used, err := p.tm.sendHiddenAttachedKeys(name, "C-c"); used {
		if err != nil {
			return err
		}
		return nil
	}
	err := p.tm.SendKeysRaw(name, "C-c")
	if err != nil && (errors.Is(err, ErrSessionNotFound) || errors.Is(err, ErrNoServer)) {
		return nil
	}
	return err
}

// IsRunning reports whether the named session has a live (non-dead) pane.
// Uses a short-lived cache (default 2s TTL) backed by a single
// `tmux list-panes -a` call instead of per-session HasSession + IsPaneDead
// subprocess calls. Sessions with remain-on-exit corpses (pane_dead=1)
// are correctly excluded — only sessions with live panes are "running".
func (p *Provider) IsRunning(name string) bool {
	return p.cache.IsRunning(name)
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

// SleepCapability reports that tmux supports full idle sleep semantics.
func (p *Provider) SleepCapability(string) runtime.SessionSleepCapability {
	return runtime.SessionSleepCapabilityFull
}

// WaitForIdle waits for the named session to reach an idle prompt.
func (p *Provider) WaitForIdle(ctx context.Context, name string, timeout time.Duration) error {
	return p.tm.WaitForIdle(ctx, name, timeout)
}

// WaitForInterruptBoundary waits for a provider-native interrupt acknowledgement
// before the next user turn is injected.
func (p *Provider) WaitForInterruptBoundary(ctx context.Context, name string, since time.Time, timeout time.Duration) error {
	return p.tm.WaitForInterruptBoundary(ctx, name, since, timeout)
}

// ResetInterruptedTurn discards the just-interrupted Gemini user turn without
// restarting the session.
func (p *Provider) ResetInterruptedTurn(ctx context.Context, name string) error {
	if p.tm.requiresHiddenAttachedInterrupt(name) && !p.tm.IsSessionAttached(name) {
		if err := p.tm.ensureHiddenAttachedClient(name); err != nil {
			return fmt.Errorf("preparing detached gemini rewind: %w", err)
		}
	}
	if err := p.NudgeNow(name, runtime.TextContent("/rewind")); err != nil {
		return fmt.Errorf("opening gemini rewind: %w", err)
	}
	if err := p.waitForPane(ctx, name, geminiRewindDialogVisible); err != nil {
		return fmt.Errorf("waiting for gemini rewind picker: %w", err)
	}
	if err := p.SendKeys(name, "Up"); err != nil {
		return fmt.Errorf("selecting interrupted gemini turn: %w", err)
	}
	if err := sleepWithContext(ctx, 100*time.Millisecond); err != nil {
		return err
	}
	if err := p.SendKeys(name, "Enter"); err != nil {
		return fmt.Errorf("opening gemini rewind confirmation: %w", err)
	}
	if err := p.waitForPane(ctx, name, geminiRewindConfirmationVisible); err != nil {
		return fmt.Errorf("waiting for gemini rewind confirmation: %w", err)
	}
	pane, err := p.tm.CapturePane(name, 80)
	if err != nil {
		return fmt.Errorf("capturing gemini rewind confirmation: %w", err)
	}
	if !strings.Contains(pane, "No code changes to revert.") {
		if err := p.SendKeys(name, "Down"); err != nil {
			return fmt.Errorf("choosing gemini rewind-only action: %w", err)
		}
		if err := sleepWithContext(ctx, 100*time.Millisecond); err != nil {
			return err
		}
	}
	if err := p.SendKeys(name, "Enter"); err != nil {
		return fmt.Errorf("confirming gemini rewind: %w", err)
	}
	if err := p.waitForPane(ctx, name, geminiRewindComplete); err != nil {
		return fmt.Errorf("waiting for gemini rewind completion: %w", err)
	}
	if err := p.tm.WaitForIdle(ctx, name, 10*time.Second); err != nil {
		return fmt.Errorf("waiting for gemini prompt after rewind: %w", err)
	}
	return nil
}

// DismissKnownDialogs best-effort clears known trust/permissions dialogs on a
// running session using a bounded timeout.
func (p *Provider) DismissKnownDialogs(ctx context.Context, name string, timeout time.Duration) error {
	return p.tm.DismissKnownDialogs(ctx, name, timeout)
}

// Nudge sends a message to the named session to wake or redirect the agent.
// By default, waits for the agent to be idle before sending (wait-idle mode)
// to avoid interrupting active tool calls. If the agent doesn't become idle
// within NudgeIdleTimeout, sends immediately as a fallback.
// Delegates to [Tmux.NudgeSession] which handles per-session locking,
// multi-pane resolution, retry with backoff, and SIGWINCH wake.
// Best-effort: returns nil if the session doesn't exist.
func (p *Provider) Nudge(name string, content []runtime.ContentBlock) error {
	// Wait for the agent to be idle before sending, unless disabled.
	// This prevents interrupting active tool calls — the prompt is visible
	// in scrollback during inter-tool-call gaps, so immediate send-keys
	// would inject text mid-execution. See upstream dfd945e9/6bc898ce.
	if idleTimeout := p.tm.cfg.NudgeIdleTimeout; idleTimeout > 0 {
		// Best-effort wait — if it fails (session gone, timeout), proceed
		// with the nudge anyway. The message may arrive during active work,
		// but Claude's cooperative queue will handle it at the next turn.
		_ = p.tm.WaitForIdle(context.Background(), name, idleTimeout)
	}
	return p.NudgeNow(name, content)
}

// NudgeNow sends a message immediately without performing a wait-idle check.
func (p *Provider) NudgeNow(name string, content []runtime.ContentBlock) error {
	var parts []string
	for _, b := range content {
		switch b.Type {
		case "file_path":
			if b.Path != "" {
				base := filepath.Base(b.Path)
				if _, err := os.Stat(b.Path); err != nil {
					parts = append(parts, "[File not found: ./"+base+"]")
				} else if err := p.CopyTo(name, b.Path, base); err != nil {
					parts = append(parts, "[File staging failed: ./"+base+": "+err.Error()+"]")
				} else {
					parts = append(parts, "[File staged: ./"+base+"]")
				}
			}
		default: // "text"
			if b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
	}
	message := strings.Join(parts, "\n")
	if message == "" {
		return nil
	}

	if used, err := p.tm.sendHiddenAttachedText(name, message); used {
		if err != nil {
			return err
		}
		return nil
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

func (p *Provider) waitForPane(ctx context.Context, name string, match func(string) bool) error {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return err
		}
		pane, err := p.tm.CapturePane(name, 80)
		if err == nil && match(pane) {
			return nil
		}
		if err := sleepWithContext(ctx, 100*time.Millisecond); err != nil {
			return err
		}
	}
	return ErrIdleTimeout
}

func geminiRewindDialogVisible(pane string) bool {
	return strings.Contains(pane, "Cancel rewind and stay here") || strings.Contains(pane, "> Rewind")
}

func geminiRewindConfirmationVisible(pane string) bool {
	return strings.Contains(pane, "Confirm Rewind")
}

func geminiRewindComplete(pane string) bool {
	return !strings.Contains(pane, "Confirm Rewind") &&
		!strings.Contains(pane, "Cancel rewind and stay here") &&
		!strings.Contains(pane, "> Rewind") &&
		!strings.Contains(pane, "Rewinding...")
}

func sleepWithContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// SendKeys sends bare keystrokes to the named session. Each key is sent
// as a separate tmux send-keys invocation (e.g., "Enter", "Down", "C-c").
// Best-effort: returns nil if the session doesn't exist.
func (p *Provider) SendKeys(name string, keys ...string) error {
	if used, err := p.tm.sendHiddenAttachedKeys(name, keys...); used {
		if err != nil {
			return err
		}
		return nil
	}
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
	return overlay.CopyFileOrDir(src, dst, io.Discard)
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
	isSessionRunning(name string) bool
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

func (o *tmuxStartOps) isSessionRunning(name string) bool {
	return o.tm.IsSessionRunning(name)
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
	return o.tm.NudgeSession(name, text)
}

func (o *tmuxStartOps) setRemainOnExit(name string) error {
	return o.tm.SetRemainOnExit(name, true)
}

func (o *tmuxStartOps) runSetupCommand(ctx context.Context, cmd string, env map[string]string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	c := exec.CommandContext(ctx, "sh", "-c", cmd)
	if workDir := strings.TrimSpace(env["GC_DIR"]); workDir != "" {
		c.Dir = workDir
	}
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
	if err := ctx.Err(); err != nil {
		return err
	}

	// Step 0: Run pre-start commands (directory/worktree preparation).
	if err := runPreStart(ctx, ops, name, cfg, setupTimeout); err != nil {
		return fmt.Errorf("running pre_start: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	// Step 1: Ensure fresh session (zombie detection).
	if err := ensureFreshSession(ops, name, cfg); err != nil {
		return err
	}

	// Enable remain-on-exit for crash forensics. Best-effort.
	_ = ops.setRemainOnExit(name)
	if err := ctx.Err(); err != nil {
		return err
	}

	hasHints := cfg.ReadyPromptPrefix != "" || cfg.ReadyDelayMs > 0 ||
		len(cfg.ProcessNames) > 0 || cfg.EmitsPermissionWarning ||
		cfg.Nudge != "" || len(cfg.PreStart) > 0 || len(cfg.SessionSetup) > 0 || cfg.SessionSetupScript != "" ||
		len(cfg.SessionLive) > 0

	if !hasHints {
		// Fire-and-forget: caller may SendImmediate before the agent is
		// fully interactive. This is an accepted narrow race — it only
		// occurs when no readiness hints are configured, and the message
		// lands in tmux scrollback where the agent picks it up at its
		// next turn boundary.
		return nil
	}

	// Step 2: Wait for agent command to appear (not still in shell).
	if len(cfg.ProcessNames) > 0 {
		_ = ops.waitForCommand(ctx, name, 30*time.Second) // best-effort, non-fatal
		if err := ctx.Err(); err != nil {
			return err
		}
	}

	// Step 3: Accept startup dialogs (workspace trust + bypass permissions).
	// Always attempted when process names are set, since any Claude-like
	// agent may show a trust dialog regardless of EmitsPermissionWarning.
	if len(cfg.ProcessNames) > 0 || cfg.EmitsPermissionWarning {
		_ = ops.acceptStartupDialogs(ctx, name) // best-effort
		if err := ctx.Err(); err != nil {
			return err
		}
	}

	// Step 4: Wait for runtime readiness.
	if cfg.ReadyPromptPrefix != "" || cfg.ReadyDelayMs > 0 {
		rc := &RuntimeConfig{Tmux: &RuntimeTmuxConfig{
			ReadyPromptPrefix: cfg.ReadyPromptPrefix,
			ReadyDelayMs:      cfg.ReadyDelayMs,
			ProcessNames:      cfg.ProcessNames,
		}}
		_ = ops.waitForReady(ctx, name, rc, 60*time.Second) // best-effort
		if err := ctx.Err(); err != nil {
			return err
		}
	}

	// Some CLIs surface trust or permissions dialogs only after their initial
	// ready screen. Re-run dialog acceptance after readiness so late dialogs do
	// not strand the session in an unusable startup state.
	if len(cfg.ProcessNames) > 0 || cfg.EmitsPermissionWarning {
		_ = ops.acceptStartupDialogs(ctx, name) // best-effort
		if err := ctx.Err(); err != nil {
			return err
		}
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
	if err := ctx.Err(); err != nil {
		return err
	}
	runSessionSetup(ctx, ops, name, cfg, os.Stderr, setupTimeout)

	// Step 6: Send nudge text if configured.
	if err := ctx.Err(); err != nil {
		return err
	}
	if cfg.Nudge != "" {
		_ = ops.sendKeys(name, cfg.Nudge) // best-effort
	}

	// Step 6.5: Run session_live commands (idempotent, re-applicable).
	if err := ctx.Err(); err != nil {
		return err
	}
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
// Used for directory/worktree preparation. Failures are fatal because
// launching into an unprepared workDir can point agents at the wrong repo or
// skip required bootstrap state entirely.
func runPreStart(ctx context.Context, ops startOps, _ string, cfg runtime.Config, setupTimeout time.Duration) error {
	if len(cfg.PreStart) == 0 {
		return nil
	}
	setupEnv := make(map[string]string, len(cfg.Env))
	for k, v := range cfg.Env {
		setupEnv[k] = v
	}
	for i, cmd := range cfg.PreStart {
		if err := ops.runSetupCommand(ctx, cmd, setupEnv, setupTimeout); err != nil {
			return fmt.Errorf("pre_start[%d]: %w", i, err)
		}
	}
	return nil
}

// ensureFreshSession creates a session, handling stale tmux state.
// If the session already exists, returns an error (duplicate detection).
// Exceptions:
//   - dead panes (remain-on-exit corpses) are recycled even without ProcessNames
//   - if ProcessNames are configured and the agent is dead (zombie), the
//     zombie session is killed and recreated
//
// maxInlinePromptLen is the threshold above which prompts are written to a
// temp file and read back via $(cat ...) inside the tmux session. tmux
// new-session passes the command through a fixed-size protocol buffer
// (~2KB) so large prompts cause "command too long" errors.
const maxInlinePromptLen = 1024

func ensureFreshSession(ops startOps, name string, cfg runtime.Config) error {
	fullCommand := cfg.Command
	promptFile := ""
	if cfg.PromptSuffix != "" {
		if len(cfg.PromptSuffix) > maxInlinePromptLen {
			// Large prompt — write to temp file and use $(cat ...) expansion
			// inside the tmux session's shell to avoid the protocol limit and
			// prevent the quoted prompt from leaking into the exec command
			// line (which triggers ENAMETOOLONG / exit 126 when the total
			// command overflows kernel argv/exec buffers).
			var err error
			promptFile, err = writePromptFile(cfg.WorkDir, name, cfg.PromptSuffix)
			if err != nil {
				// No silent fallback: the inline path would produce the
				// "File name too long" tmux pane death that this helper
				// exists to prevent. Surface the failure so the reconciler
				// records it and the operator can diagnose the cause.
				return fmt.Errorf("writing prompt temp file for session %q: %w", name, err)
			}
			fullCommand = longPromptCommand(cfg.Command, cfg.PromptFlag, promptFile)
		} else {
			if cfg.PromptFlag != "" {
				fullCommand = fullCommand + " " + cfg.PromptFlag + " " + cfg.PromptSuffix
			} else {
				fullCommand = fullCommand + " " + cfg.PromptSuffix
			}
		}
	}
	err := ops.createSession(name, cfg.WorkDir, fullCommand, cfg.Env)
	if err == nil {
		return nil // created successfully
	}
	if errors.Is(err, ErrNoServer) {
		time.Sleep(50 * time.Millisecond)
		err = ops.createSession(name, cfg.WorkDir, fullCommand, cfg.Env)
		if err == nil {
			return nil
		}
	}
	if !errors.Is(err, ErrSessionExists) {
		return cleanupPromptFileOnError(promptFile, fmt.Errorf("creating session: %w", err))
	}

	// Session exists but the pane is already dead (e.g. remain-on-exit corpse).
	// Safe to recycle even when ProcessNames are unavailable.
	if !ops.isSessionRunning(name) {
		if err := ops.killSession(name); err != nil {
			return cleanupPromptFileOnError(promptFile, fmt.Errorf("killing dead session: %w", err))
		}
		if err := recreateSessionAfterCleanup(ops, name, cfg.WorkDir, fullCommand, cfg.Env, promptFile); err != nil {
			return cleanupPromptFileOnError(promptFile, fmt.Errorf("creating session after dead-session cleanup: %w", err))
		}
		return nil
	}

	// Session exists — without process names we can't distinguish a zombie
	// from a healthy session, so treat it as a duplicate.
	if len(cfg.ProcessNames) == 0 {
		return cleanupPromptFileOnError(promptFile, fmt.Errorf("%w: session %q", runtime.ErrSessionExists, name))
	}

	// We have process names — check if the agent is alive.
	if ops.isRuntimeRunning(name, cfg.ProcessNames) {
		return cleanupPromptFileOnError(promptFile, fmt.Errorf("%w: session %q", runtime.ErrSessionExists, name))
	}

	// Zombie: tmux alive but agent dead. Kill and recreate.
	if err := ops.killSession(name); err != nil {
		return cleanupPromptFileOnError(promptFile, fmt.Errorf("killing zombie session: %w", err))
	}
	if err := recreateSessionAfterCleanup(ops, name, cfg.WorkDir, fullCommand, cfg.Env, promptFile); err != nil {
		return cleanupPromptFileOnError(promptFile, fmt.Errorf("creating session after zombie cleanup: %w", err))
	}
	return nil
}

func longPromptCommand(command, promptFlag, promptFile string) string {
	quotedPromptFile := shellquote.Quote(promptFile)
	var script string
	if promptFlag != "" {
		script = fmt.Sprintf(`__gc_prompt="$(cat %s && printf .)"; __gc_status=$?; rm -f %s; [ "$__gc_status" -eq 0 ] || exit "$__gc_status"; __gc_prompt="${__gc_prompt%%.}"; exec %s %s "$__gc_prompt"`,
			quotedPromptFile, quotedPromptFile, command, promptFlag)
	} else {
		script = fmt.Sprintf(`__gc_prompt="$(cat %s && printf .)"; __gc_status=$?; rm -f %s; [ "$__gc_status" -eq 0 ] || exit "$__gc_status"; __gc_prompt="${__gc_prompt%%.}"; exec %s "$__gc_prompt"`,
			quotedPromptFile, quotedPromptFile, command)
	}
	return "sh -c " + shellquote.Quote(script)
}

func removePromptFile(promptFile string) error {
	if promptFile == "" {
		return nil
	}
	if err := os.Remove(promptFile); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("removing prompt temp file %q: %w", promptFile, err)
	}
	return nil
}

func cleanupPromptFileOnError(promptFile string, err error) error {
	if promptFile == "" || err == nil {
		return err
	}
	if removeErr := removePromptFile(promptFile); removeErr != nil {
		return errors.Join(err, removeErr)
	}
	return err
}

func recreateSessionAfterCleanup(ops startOps, name, workDir, command string, env map[string]string, promptFile string) error {
	err := ops.createSession(name, workDir, command, env)
	if errors.Is(err, ErrNoServer) {
		time.Sleep(50 * time.Millisecond)
		err = ops.createSession(name, workDir, command, env)
	}
	if errors.Is(err, ErrSessionExists) {
		_ = removePromptFile(promptFile)
		return nil // race: another process created it
	}
	return err
}

// writePromptFile writes a shell-quoted prompt string to a temp file for
// the tmux session's shell to read back via $(cat ...). The file contains
// the raw prompt text (unquoted) so shell expansion yields a single argv
// element.
//
// Preferred location is <workDir>/.gc/tmp (visible from inside the worktree
// and cleaned up with the agent's scratch space). A non-empty WorkDir must
// exist and be a directory, because tmux may otherwise start the pane in the
// wrong checkout. If WorkDir is empty, or WorkDir exists but its .gc/tmp path
// is unusable, this falls back to a gc-scoped directory under os.TempDir().
// The fallback is load-bearing: without it a failed MkdirAll used to trigger
// a silent "inline the prompt into the tmux command line" path that produced
// "cannot execute: File name too long" pane deaths for large prompts.
func writePromptFile(workDir, agentName, shellQuotedPrompt string) (string, error) {
	// Strip surrounding single quotes from shell-quoted string.
	raw := shellQuotedPrompt
	if len(raw) >= 2 && raw[0] == '\'' && raw[len(raw)-1] == '\'' {
		raw = raw[1 : len(raw)-1]
		raw = strings.ReplaceAll(raw, `'\''`, `'`)
	}

	// Try workDir-scoped path first so the prompt file sits next to the
	// session's scratch state. An unusable workDir is not fatal; we still
	// want a valid argv-via-file path to avoid the inline fallback.
	var candidateErrs []error
	if workDir != "" {
		info, err := os.Stat(workDir)
		if err != nil {
			return "", fmt.Errorf("workdir unavailable: %w", err)
		}
		if !info.IsDir() {
			return "", fmt.Errorf("workdir %q is not a directory", workDir)
		}
		dir := filepath.Join(workDir, ".gc", "tmp")
		path, err := writePromptToDir(dir, agentName, raw)
		if err == nil {
			return path, nil
		}
		candidateErrs = append(candidateErrs, fmt.Errorf("workdir tmp: %w", err))
	}
	osTmpDir := filepath.Join(os.TempDir(), fmt.Sprintf(".gc-%d", os.Getuid()), "tmux-prompts")
	path, err := writePromptToDir(osTmpDir, agentName, raw)
	if err == nil {
		return path, nil
	}
	candidateErrs = append(candidateErrs, fmt.Errorf("os tmp: %w", err))
	return "", errors.Join(candidateErrs...)
}

// writePromptToDir creates the target directory and writes the prompt to
// a new temp file inside it. Returns the temp file path on success.
func writePromptToDir(dir, agentName, raw string) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(dir, "prompt-"+agentName+"-*.txt")
	if err != nil {
		return "", err
	}
	if _, err := f.WriteString(raw); err != nil {
		if closeErr := f.Close(); closeErr != nil {
			return "", errors.Join(err, closeErr)
		}
		if removeErr := os.Remove(f.Name()); removeErr != nil {
			return "", errors.Join(err, removeErr)
		}
		return "", err
	}
	if err := f.Close(); err != nil {
		if removeErr := os.Remove(f.Name()); removeErr != nil {
			return "", errors.Join(err, removeErr)
		}
		return "", err
	}
	return f.Name(), nil
}
