package ssh

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// validTmuxName mirrors the local tmux provider's guard: a session name must be
// a safe tmux target. tmux treats "." as a pane and ":" as a window separator,
// so a session created with -s "a.b" cannot be addressed with -t "a.b" — and
// the carrier addresses this session by its name, so reject such names up front.
var validTmuxName = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// ErrInvalidSessionName reports a session name that is not a safe tmux target.
var ErrInvalidSessionName = errors.New("invalid session name")

// Provider is a [runtime.Provider] that runs each session as a tmux session on
// an existing remote host reached over SSH. It does NOT provision the box (the
// host pre-exists); it connects, starts a tmux session, and drives it through
// the shared tmux carrier over the ssh exec connection — the same carrier the
// Kubernetes provider uses. One host runs many sessions, each its own tmux
// session named by the session name (which is the carrier's tmux target).
//
// v0 scope: lifecycle (start/stop/is-running/list), the driving verbs, meta,
// activity, attach, and startup orchestration (PreStart / SessionSetup /
// SessionSetupScript / SessionLive / post-start liveness / initial Nudge). Still
// deferred: startup-dialog dismissal (EmitsPermissionWarning /
// AcceptStartupDialogs), live SessionLive re-apply via RunLive (a no-op, like
// k8s), and CopyTo.
type Provider struct {
	conn            *Conn
	postStartSettle time.Duration // settle before the post-start liveness recheck
}

var (
	_ runtime.Provider                = (*Provider)(nil)
	_ runtime.ExecProvider            = (*Provider)(nil)
	_ runtime.SleepCapabilityProvider = (*Provider)(nil)
)

// defaultPostStartSettle is how long Start waits before re-checking that a
// managed session survived startup (the box already exists, so a short settle
// suffices to catch an agent that dies immediately, e.g. a stale --resume key).
const defaultPostStartSettle = time.Second

// NewProvider returns an ssh Provider for the box at ep.
func NewProvider(ep Endpoint) *Provider {
	return &Provider{conn: New(ep), postStartSettle: defaultPostStartSettle}
}

// Exec runs argv on the box — the connection primitive (see [Conn.Exec]).
func (p *Provider) Exec(ctx context.Context, name string, argv []string) ([]byte, int, error) {
	return p.conn.Exec(ctx, name, argv)
}

// carrier drives the named session's tmux over the connection. The in-box tmux
// target is the session name itself (one host, many named sessions).
func (p *Provider) carrier(name string) runtime.Carrier {
	return runtime.NewTmuxCarrier(p.conn, name)
}

// tmux runs a tmux subcommand over the connection and returns trimmed stdout,
// the exit code, and a transport error (if any).
func (p *Provider) tmux(ctx context.Context, name string, args ...string) (string, int, error) {
	out, code, err := p.conn.Exec(ctx, name, append([]string{"tmux"}, args...))
	return strings.TrimRight(string(out), "\n"), code, err
}

const defaultRemoteShell = "/bin/sh"

// Start launches the session command in a new tmux session named name on the
// box and runs the configured startup orchestration:
//
//   - PreStart commands run on the box BEFORE session creation; a failure
//     aborts (the agent never launches into an unprepared workdir).
//   - new-session creates the tmux session. A name that already exists fails
//     with [runtime.ErrSessionExists] (has-session precheck, and re-checked if
//     new-session fails, since the connection drops tmux's stderr on a non-zero
//     exit). Env is injected via tmux -e (requires remote tmux >= 3.2).
//   - SessionSetup, SessionSetupScript (piped to a remote sh), and SessionLive
//     run on the box after creation, best-effort. Every setup step runs with the
//     session WorkDir as cwd and the session env (cfg.Env + GC_SESSION) exported,
//     matching the local tmux adapter and the k8s in-pod environment.
//   - When the config carries managed startup hints and is not one-shot, a
//     bounded settle + has-session recheck detects an agent that dies
//     immediately (e.g. a stale --resume key), returning
//     [runtime.ErrSessionDiedDuringStartup]; on death the tmux session is killed
//     (SessionSetup filesystem side effects on the box are not rolled back).
//   - The initial cfg.Nudge, if any, is delivered last. There is no
//     ReadyPromptPrefix / ReadyDelayMs ready-prompt wait before it (matching the
//     k8s provider), so a caller needing a ready gate must not rely on it.
//
// Deferred: startup-dialog dismissal (EmitsPermissionWarning /
// AcceptStartupDialogs), CopyTo, and live SessionLive re-apply on drift (RunLive
// is a no-op, matching k8s; SessionLive is applied once at startup).
func (p *Provider) Start(ctx context.Context, name string, cfg runtime.Config) error {
	if !validTmuxName.MatchString(name) {
		return fmt.Errorf("%w %q: must match %s", ErrInvalidSessionName, name, validTmuxName.String())
	}
	if p.hasSession(ctx, name) {
		return fmt.Errorf("%w: ssh session %q", runtime.ErrSessionExists, name)
	}

	// Setup steps run on the box with the session WorkDir as cwd and the session
	// env (cfg.Env + GC_SESSION) exported — matching the local tmux adapter's
	// runSetupCommand and the k8s in-pod environment.
	prelude := setupPrelude(cfg, name)

	// PreStart prepares the target filesystem; a failure aborts startup.
	for _, cmd := range cfg.PreStart {
		if cmd == "" {
			continue
		}
		out, code, err := p.conn.Exec(ctx, name, []string{"sh", "-c", prelude + cmd})
		if err != nil {
			return fmt.Errorf("ssh start %q: pre_start: %w", name, err)
		}
		if code != 0 {
			return fmt.Errorf("ssh start %q: pre_start %q exited %d: %s", name, cmd, code, strings.TrimSpace(string(out)))
		}
	}

	args := []string{"new-session", "-d", "-s", name}
	if cfg.WorkDir != "" {
		args = append(args, "-c", cfg.WorkDir)
	}
	for _, k := range sortedKeys(cfg.Env) {
		args = append(args, "-e", k+"="+cfg.Env[k])
	}
	command := cfg.Command
	if command == "" {
		command = defaultRemoteShell
	}
	args = append(args, command)
	if _, code, err := p.tmux(ctx, name, args...); err != nil {
		return fmt.Errorf("ssh start %q: %w", name, err)
	} else if code != 0 {
		// Tighten the sentinel under the precheck TOCTOU: if the session now
		// exists, new-session failed because it was a duplicate.
		if p.hasSession(ctx, name) {
			return fmt.Errorf("%w: ssh session %q", runtime.ErrSessionExists, name)
		}
		return fmt.Errorf("ssh start %q: tmux new-session exited %d", name, code)
	}

	// SessionSetup, SessionSetupScript, and SessionLive run on the box,
	// best-effort, with the same cwd + env prelude.
	for _, cmd := range cfg.SessionSetup {
		if cmd != "" {
			_, _, _ = p.conn.Exec(ctx, name, []string{"sh", "-c", prelude + cmd})
		}
	}
	if cfg.SessionSetupScript != "" {
		// k8s/tmux log a warning on a read error; the ssh provider has no stderr
		// yet, so a missing/unreadable script path is skipped silently (NOTE).
		if script, err := os.ReadFile(cfg.SessionSetupScript); err == nil {
			_, _, _ = p.conn.execScript(ctx, []byte(prelude+"\n"+string(script)))
		}
	}
	for _, cmd := range cfg.SessionLive {
		if cmd != "" {
			_, _, _ = p.conn.Exec(ctx, name, []string{"sh", "-c", prelude + cmd})
		}
	}

	// Post-start liveness: detect an agent that dies immediately after startup.
	if p.requiresPostStartLiveness(cfg) {
		if p.postStartSettle > 0 {
			select {
			case <-ctx.Done():
				return fmt.Errorf("ssh start %q: %w", name, ctx.Err())
			case <-time.After(p.postStartSettle):
			}
		}
		if !p.hasSession(ctx, name) {
			_ = p.Stop(name)
			return fmt.Errorf("%w: ssh session %q died immediately after startup", runtime.ErrSessionDiedDuringStartup, name)
		}
	}

	if cfg.Nudge != "" {
		_ = p.Nudge(name, runtime.TextContent(cfg.Nudge))
	}
	return nil
}

// requiresPostStartLiveness mirrors the k8s policy: a non-one-shot session that
// carries managed startup hints gets the settle + has-session liveness recheck.
func (p *Provider) requiresPostStartLiveness(cfg runtime.Config) bool {
	return cfg.Lifecycle != runtime.LifecycleOneShot && runtime.HasManagedStartupHints(cfg)
}

// Stop kills the tmux session; idempotent (kill-session on a missing session
// exits non-zero, which is swallowed).
func (p *Provider) Stop(name string) error {
	_, _, _ = p.tmux(context.Background(), name, "kill-session", "-t", name)
	return nil
}

// hasSession reports whether the tmux session exists, using the given context.
func (p *Provider) hasSession(ctx context.Context, name string) bool {
	_, code, err := p.tmux(ctx, name, "has-session", "-t", name)
	return err == nil && code == 0
}

// IsRunning reports whether the tmux session exists on the box.
func (p *Provider) IsRunning(name string) bool {
	return p.hasSession(context.Background(), name)
}

// IsAttached reports whether a terminal is attached to the tmux session.
func (p *Provider) IsAttached(name string) bool {
	out, code, err := p.tmux(context.Background(), name, "display-message", "-t", name, "-p", "#{session_attached}")
	if err != nil || code != 0 {
		return false
	}
	return strings.TrimSpace(out) == "1"
}

// Attach connects the local terminal to the remote tmux session over ssh -t.
func (p *Provider) Attach(name string) error {
	cmd := exec.Command("ssh", attachArgs(p.conn.ep, name)...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// ProcessAlive reports whether any of processNames is running on the box.
// Returns true when processNames is empty (per the interface contract).
func (p *Provider) ProcessAlive(name string, processNames []string) bool {
	if len(processNames) == 0 {
		return true
	}
	for _, pname := range processNames {
		if pname == "" {
			continue
		}
		// Bracket the first character so the regex cannot match the literal
		// text in the invoking shell's own command line: over ssh the op runs
		// as `sh -c "'pgrep' '-f' '<pat>'"`, and on dash the wrapper shell
		// survives carrying <pname> in its argv, which a bare `pgrep -f <pname>`
		// would self-match (a false positive for any name). "[c]laude" cannot
		// match the literal "[c]laude" text yet still matches a running "claude".
		pat := "[" + pname[:1] + "]" + pname[1:]
		_, code, err := p.conn.Exec(context.Background(), name, []string{"pgrep", "-f", pat})
		if err == nil && code == 0 {
			return true
		}
	}
	return false
}

// Nudge delivers content to the session (best-effort).
func (p *Provider) Nudge(name string, content []runtime.ContentBlock) error {
	_ = p.carrier(name).Nudge(context.Background(), name, content)
	return nil
}

// SendKeys sends bare keystrokes to the session (best-effort).
func (p *Provider) SendKeys(name string, keys ...string) error {
	_ = p.carrier(name).SendKeys(context.Background(), name, keys...)
	return nil
}

// Peek captures the last N lines of the session pane (best-effort: empty on failure).
func (p *Provider) Peek(name string, lines int) (string, error) {
	out, _ := p.carrier(name).Peek(context.Background(), name, lines)
	return out, nil
}

// Interrupt sends Ctrl-C to the session (best-effort).
func (p *Provider) Interrupt(name string) error {
	_ = p.carrier(name).Interrupt(context.Background(), name)
	return nil
}

// ClearScrollback clears the session's scrollback (best-effort).
func (p *Provider) ClearScrollback(name string) error {
	_ = p.carrier(name).ClearScrollback(context.Background(), name)
	return nil
}

// SetMeta stores a key-value pair in the tmux session environment.
func (p *Provider) SetMeta(name, key, value string) error {
	_, _, _ = p.tmux(context.Background(), name, "set-environment", "-t", name, key, value)
	return nil
}

// GetMeta retrieves a value from the tmux session environment ("" if unset).
func (p *Provider) GetMeta(name, key string) (string, error) {
	out, code, err := p.tmux(context.Background(), name, "show-environment", "-t", name, key)
	if err != nil || code != 0 {
		return "", nil
	}
	out = strings.TrimSpace(out)
	if strings.HasPrefix(out, "-") { // "-KEY" means explicitly unset
		return "", nil
	}
	if _, val, ok := strings.Cut(out, "="); ok {
		return val, nil
	}
	return "", nil
}

// RemoveMeta removes a key from the tmux session environment.
func (p *Provider) RemoveMeta(name, key string) error {
	_, _, _ = p.tmux(context.Background(), name, "set-environment", "-t", name, "-u", key)
	return nil
}

// ListRunning returns the names of tmux sessions whose names have the prefix.
func (p *Provider) ListRunning(prefix string) ([]string, error) {
	out, code, err := p.tmux(context.Background(), "", "list-sessions", "-F", "#{session_name}")
	if err != nil {
		return nil, err
	}
	if code != 0 || out == "" {
		return []string{}, nil // no server / no sessions
	}
	var names []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line != "" && (prefix == "" || strings.HasPrefix(line, prefix)) {
			names = append(names, line)
		}
	}
	return names, nil
}

// GetLastActivity returns the session's last-activity time (zero if unknown).
func (p *Provider) GetLastActivity(name string) (time.Time, error) {
	out, code, err := p.tmux(context.Background(), name, "display-message", "-t", name, "-p", "#{session_activity}")
	if err != nil || code != 0 {
		return time.Time{}, nil
	}
	secs, perr := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if perr != nil {
		return time.Time{}, nil
	}
	return time.Unix(secs, 0), nil
}

// CopyTo is not yet supported by the v0 ssh provider (best-effort no-op).
func (p *Provider) CopyTo(_, _, _ string) error { return nil }

// RunLive is a no-op for the ssh provider (session_live re-apply unsupported).
func (p *Provider) RunLive(_ string, _ runtime.Config) error { return nil }

// Capabilities reports what this provider can observe. Activity is reported
// via tmux #{session_activity}. Attachment is observable (IsAttached queries
// #{session_attached}) but — matching the Kubernetes provider — is left
// undeclared so idle-sleep resolves to timed-only on a remote box; revisit
// once attachment-aware sleep is validated against a real remote.
func (p *Provider) Capabilities() runtime.ProviderCapabilities {
	return runtime.ProviderCapabilities{
		CanReportActivity: true, // tmux #{session_activity}
	}
}

// SleepCapability reports timed-only idle sleep, matching the Kubernetes
// provider: a remote tmux box supports timer-based sleep but cannot guarantee
// interactive prompt-boundary safety.
func (p *Provider) SleepCapability(string) runtime.SessionSleepCapability {
	return runtime.SessionSleepCapabilityTimedOnly
}

// attachArgs builds the ssh argv for an interactive attach: force a PTY (-t),
// the host-key/key/port options, the destination, and the remote tmux-attach
// command shell-quoted into a SINGLE argument — so a session name with shell
// metacharacters cannot inject a remote command (ssh otherwise hands the
// post-destination args to the remote shell as one unquoted string). BatchMode
// is deliberately omitted (unlike the non-interactive [sshArgs]) so an
// operator-initiated attach can still answer a key-passphrase or host-key prompt.
func attachArgs(ep Endpoint, name string) []string {
	args := []string{"-t", "-o", "StrictHostKeyChecking=accept-new"}
	if ep.KnownHostsPath != "" {
		args = append(args, "-o", "UserKnownHostsFile="+ep.KnownHostsPath)
	}
	if ep.KeyPath != "" {
		args = append(args, "-i", ep.KeyPath)
	}
	if ep.Port != 0 {
		args = append(args, "-p", strconv.Itoa(ep.Port))
	}
	return append(args, "--", ep.target(), shellQuote([]string{"tmux", "attach", "-t", name}))
}

// setupPrelude builds a sh prefix that cd's into the session WorkDir and
// exports the session env (cfg.Env plus GC_SESSION), so PreStart / SessionSetup
// / SessionLive commands and the setup script run with the same cwd and
// environment the local tmux adapter (runSetupCommand) and the k8s in-pod path
// provide. The remote command or script body is appended after this prefix; a
// failed cd aborts with exit 1 (which fails PreStart and is discarded by the
// best-effort steps).
func setupPrelude(cfg runtime.Config, name string) string {
	var b strings.Builder
	if cfg.WorkDir != "" {
		b.WriteString("cd " + shellQuote([]string{cfg.WorkDir}) + " || exit 1\n")
	}
	for _, k := range sortedKeys(cfg.Env) {
		b.WriteString("export " + k + "=" + shellQuote([]string{cfg.Env[k]}) + "\n")
	}
	b.WriteString("export GC_SESSION=" + shellQuote([]string{name}) + "\n")
	return b.String()
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
