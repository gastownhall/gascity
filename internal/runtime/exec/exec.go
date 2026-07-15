package exec

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// Provider implements [runtime.Provider] by delegating each operation to
// a user-supplied script via fork/exec. The script receives the operation
// name as its first argument, following the Git credential helper pattern.
//
// Exit codes: 0 = success, 1 = error (stderr has message), 2 = unknown
// operation (treated as success for forward compatibility).
type Provider struct {
	script       string
	timeout      time.Duration
	startTimeout time.Duration // used only for Start(); includes readiness polling

	// RPP handshake result, resolved lazily once per instance (see
	// handshake.go). The error is cached alongside the info so a broken
	// `protocol` op degrades probes to the zero-capability floor instead
	// of re-running on every call.
	handshakeOnce sync.Once
	handshakeInfo runtime.ProtocolInfo
	handshakeErr  error
}

type startupWatchEvent struct {
	Content string `json:"content"`
}

var startupWatchFirstEventTimeout = runtime.StartupDialogTimeout

const startupWatchCloseTimeout = 200 * time.Millisecond

// NewProvider returns an exec [Provider] that delegates to the given script.
// The script path may be absolute, relative, or a bare name resolved via
// exec.LookPath.
func NewProvider(script string) *Provider {
	return &Provider{
		script:       script,
		timeout:      30 * time.Second,
		startTimeout: 120 * time.Second,
	}
}

// run executes the script with the given args using the default timeout.
func (p *Provider) run(stdinData []byte, args ...string) (string, error) {
	return p.runWithTimeout(p.timeout, stdinData, args...)
}

// runWithTimeout executes the script with the given args and timeout,
// optionally piping stdinData to its stdin. Returns the trimmed stdout
// on success.
//
// Exit code 2 is treated as success (unknown operation — forward compatible).
// Any other non-zero exit code returns an error wrapping stderr.
func (p *Provider) runWithTimeout(dur time.Duration, stdinData []byte, args ...string) (string, error) {
	return p.runWithContext(context.Background(), dur, stdinData, args...)
}

// runWithContext executes the script using the given parent context with
// the specified timeout, optionally piping stdinData to its stdin.
func (p *Provider) runWithContext(parent context.Context, dur time.Duration, stdinData []byte, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(parent, dur)
	defer cancel()

	cmd := exec.CommandContext(ctx, p.script, args...)
	// WaitDelay ensures Go forcibly closes I/O pipes after the context
	// expires, even if grandchild processes (e.g. sleep in a shell script)
	// still hold them open.
	cmd.WaitDelay = 2 * time.Second

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if stdinData != nil {
		cmd.Stdin = bytes.NewReader(stdinData)
	}

	err := cmd.Run()
	if err != nil {
		// Check for exit code 2 → unknown operation → success.
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			if exitErr.ExitCode() == 2 {
				return "", nil
			}
		}
		errMsg := strings.TrimSpace(stderr.String())
		if errMsg == "" {
			errMsg = err.Error()
		}
		if len(args) > 0 && args[0] == "start" && strings.Contains(strings.ToLower(errMsg), "already exists") {
			return "", fmt.Errorf("%w: exec provider %s %s: %s", runtime.ErrSessionExists, p.script, strings.Join(args, " "), errMsg)
		}
		return "", fmt.Errorf("exec provider %s %s: %s", p.script, strings.Join(args, " "), errMsg)
	}

	return strings.TrimRight(stdout.String(), "\n"), nil
}

// runWithTTY executes the script with the terminal inherited (for Attach).
func (p *Provider) runWithTTY(args ...string) error {
	cmd := exec.Command(p.script, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// Start creates a new session by invoking: script start <name>
// with the session config as JSON on stdin. Uses startTimeout (default
// 120s) instead of the normal timeout to allow for readiness polling.
//
// After the script returns, Start handles startup dialogs (workspace
// trust, bypass permissions) in Go using Peek + SendKeys, sharing the
// same logic as the tmux provider via [runtime.AcceptStartupDialogs].
func (p *Provider) Start(ctx context.Context, name string, cfg runtime.Config) error {
	data, err := marshalStartConfig(cfg)
	if err != nil {
		return fmt.Errorf("exec provider: marshaling start config: %w", err)
	}
	if _, err = p.runWithContext(ctx, p.startTimeout, data, "start", name); err != nil {
		return err
	}

	if err := p.dismissStartupDialogs(ctx, name, cfg); err != nil {
		if stopErr := p.Stop(name); stopErr != nil {
			return errors.Join(
				fmt.Errorf("exec provider: dismissing startup dialogs: %w", err),
				fmt.Errorf("exec provider: cleanup after startup failure: %w", stopErr),
			)
		}
		return fmt.Errorf("exec provider: dismissing startup dialogs: %w", err)
	}

	return nil
}

// supportsSeparableLaunch reports whether the pack un-welds provisioning from the
// agent launch: it must implement the box-without-agent `provision` op
// (proc.provision) AND the `exec` op (proc.exec) the controller drives the launch
// over. Otherwise the welded `start` op provisions and launches in one shot
// (compat). Gates [runtime.TransportCapabilities.SeparableLaunch]. (Un-weld B3b.)
func (p *Provider) supportsSeparableLaunch() bool {
	return p.handshakeCapability(runtime.ProtocolCapabilityProvision) &&
		p.handshakeCapability(runtime.ProtocolCapabilityConnectionExec)
}

// provisionBox creates the box for name. When the pack supports a separable
// launch it runs the `provision` op (box + staging + pre_start, NO agent); the
// agent is launched separately by launchAgent. Otherwise it falls back to the
// welded Start (which both provisions and launches). (Un-weld B3b.)
func (p *Provider) provisionBox(ctx context.Context, name string, cfg runtime.Config) error {
	if !p.supportsSeparableLaunch() {
		return p.Start(ctx, name, cfg)
	}
	data, err := marshalStartConfig(cfg)
	if err != nil {
		return fmt.Errorf("exec provider: marshaling provision config: %w", err)
	}
	_, err = p.runWithContext(ctx, p.startTimeout, data, "provision", name)
	return err
}

// launchAgent starts the agent in the in-box tmux session over the `exec` op, for
// a pack with a separable launch; for a welded pack it is a no-op (provisionBox /
// Start already launched). The launch is idempotent: it respawns the pane if the
// session already exists (relaunch into a warm box) and otherwise creates it
// (first launch after provision), then dismisses startup dialogs — the same
// Go-side orchestration the welded Start runs. The `exec` op runs in the box's
// working directory and the box env is set at provision time, so neither an
// explicit -c nor -e is needed (workdir + env are provision-half). (Un-weld B3b.)
func (p *Provider) launchAgent(ctx context.Context, name string, cfg runtime.Config) error {
	if !p.supportsSeparableLaunch() {
		return nil
	}
	command := cfg.Command
	if command == "" {
		command = defaultLaunchShell
	}
	var argv []string
	if _, code, _ := p.Exec(ctx, name, []string{"tmux", "has-session", "-t", execTmuxSession}); code == 0 {
		argv = []string{"tmux", "respawn-pane", "-k", "-t", execTmuxSession, command}
	} else {
		argv = []string{"tmux", "new-session", "-d", "-s", execTmuxSession, command}
	}
	if _, code, err := p.Exec(ctx, name, argv); err != nil {
		return fmt.Errorf("exec provider: launching agent in %q: %w", name, err)
	} else if code != 0 {
		return fmt.Errorf("exec provider: launching agent in %q: tmux exited %d", name, code)
	}
	return p.dismissStartupDialogs(ctx, name, cfg)
}

// Relaunch re-launches the agent for the reconciler's launch-only-drift path
// ([runtime.RelaunchProvider]). For a separable pack (proc.provision) it respawns
// the agent in the warm box over the exec op (launchAgent) — no reprovision. A
// welded pack has no in-place relaunch (the agent is welded into `start`), so it
// degrades to a full reprovision (Stop+Start), which is still correct.
func (p *Provider) Relaunch(ctx context.Context, name string, cfg runtime.Config) error {
	if p.supportsSeparableLaunch() {
		return p.launchAgent(ctx, name, cfg)
	}
	if err := p.Stop(name); err != nil {
		return err
	}
	return p.Start(ctx, name, cfg)
}

func (p *Provider) dismissStartupDialogs(ctx context.Context, name string, cfg runtime.Config) error {
	if cfg.AcceptStartupDialogs != nil && !*cfg.AcceptStartupDialogs {
		return nil
	}
	if cfg.AcceptStartupDialogs == nil && !cfg.EmitsPermissionWarning && len(cfg.ProcessNames) == 0 {
		return nil
	}

	dialogTimeout := runtime.StartupDialogTimeout()
	// Gate external-CLAUDE.md-import auto-acceptance to imports within this
	// session's own repository; an import that escapes the repo (a third-party
	// or system path) is left for a human rather than auto-trusted.
	trustRoot := runtime.WithTrustedImportRoot(runtime.WorkspaceImportTrustRoot(ctx, cfg.WorkDir))
	snapshots, closeWatch, ok, err := p.startStartupWatch(ctx, name, startupWatchFirstEventTimeout())
	if err != nil {
		return err
	}
	if ok {
		streamObserved, streamErr := runtime.AcceptStartupDialogsFromStreamWithStatus(ctx, dialogTimeout, snapshots,
			func(keys ...string) error { return p.SendKeys(name, keys...) },
			trustRoot,
		)
		closeErr := closeWatch()
		switch {
		case streamErr != nil:
			return streamErr
		case closeErr == nil && streamObserved:
			return nil
		default:
			return runtime.AcceptStartupDialogs(ctx,
				func(lines int) (string, error) { return p.Peek(name, lines) },
				func(keys ...string) error { return p.SendKeys(name, keys...) },
				trustRoot,
			)
		}
	}

	return runtime.AcceptStartupDialogs(ctx,
		func(lines int) (string, error) { return p.Peek(name, lines) },
		func(keys ...string) error { return p.SendKeys(name, keys...) },
		trustRoot,
	)
}

func (p *Provider) startStartupWatch(
	ctx context.Context,
	name string,
	firstEventTimeout time.Duration,
) (<-chan string, func() error, bool, error) {
	watchCtx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(watchCtx, p.script, "watch-startup", name)
	// Startup watchers are short-lived probes; tear them down quickly once the
	// dialog helper is finished so Start cannot stall behind a sleeping wrapper.
	cmd.WaitDelay = 250 * time.Millisecond

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, nil, false, fmt.Errorf("startup watcher stdout pipe: %w", err)
	}

	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, nil, false, fmt.Errorf("startup watcher start: %w", err)
	}

	type firstResult struct {
		content     string
		unsupported bool
		err         error
	}

	first := make(chan firstResult, 1)
	events := make(chan string, 1)
	done := make(chan error, 1)

	go func() {
		defer close(events)

		scanner := bufio.NewScanner(stdout)
		emitted := false
		for scanner.Scan() {
			var event startupWatchEvent
			if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
				decodeErr := fmt.Errorf("startup watcher decode: %w", err)
				if !emitted {
					first <- firstResult{err: decodeErr}
				}
				cancel()
				_ = cmd.Wait()
				done <- decodeErr
				return
			}
			if !emitted {
				emitted = true
				first <- firstResult{content: event.Content}
			}
			if err := watchCtx.Err(); err != nil {
				done <- formatStartupWatchError(stderr.String(), cmd.Wait())
				return
			}
			select {
			case events <- event.Content:
			case <-watchCtx.Done():
				done <- formatStartupWatchError(stderr.String(), cmd.Wait())
				return
			}
		}

		scanErr := scanner.Err()
		waitErr := cmd.Wait()
		if !emitted {
			if isUnknownOperation(waitErr) {
				first <- firstResult{unsupported: true}
				done <- nil
				return
			}
			if scanErr != nil {
				first <- firstResult{err: fmt.Errorf("startup watcher scan: %w", scanErr)}
				done <- scanErr
				return
			}
			if waitErr != nil {
				err := formatStartupWatchError(stderr.String(), waitErr)
				first <- firstResult{err: err}
				done <- err
				return
			}
			first <- firstResult{unsupported: true}
			done <- nil
			return
		}
		if scanErr != nil {
			done <- fmt.Errorf("startup watcher scan: %w", scanErr)
			return
		}
		done <- formatStartupWatchError(stderr.String(), waitErr)
	}()

	var (
		timeout <-chan time.Time
		timer   *time.Timer
	)
	if firstEventTimeout > 0 {
		timer = time.NewTimer(firstEventTimeout)
		timeout = timer.C
		defer timer.Stop()
	}

	var result firstResult
	select {
	case result = <-first:
	case <-timeout:
		cancel()
		_ = waitStartupWatch(done)
		return nil, nil, false, nil
	case <-ctx.Done():
		cancel()
		_ = waitStartupWatch(done)
		return nil, nil, false, ctx.Err()
	}
	if result.unsupported {
		cancel()
		_ = waitStartupWatch(done)
		return nil, nil, false, nil
	}
	if result.err != nil {
		cancel()
		_ = waitStartupWatch(done)
		return nil, nil, false, result.err
	}

	closeWatch := func() error {
		cancel()
		return waitStartupWatch(done)
	}

	return events, closeWatch, true, nil
}

func waitStartupWatch(done <-chan error) error {
	select {
	case err := <-done:
		if err == nil || errors.Is(err, context.Canceled) || isCanceledStartupWatchError(err) {
			return nil
		}
		return err
	case <-time.After(startupWatchCloseTimeout):
		return nil
	}
}

func isCanceledStartupWatchError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "signal: killed") ||
		strings.Contains(msg, "signal: terminated") ||
		strings.Contains(msg, "exit status 137") ||
		strings.Contains(msg, "exit status 143")
}

func isUnknownOperation(err error) bool {
	var exitErr *exec.ExitError
	return errors.As(err, &exitErr) && exitErr.ExitCode() == 2
}

func formatStartupWatchError(stderr string, err error) error {
	if err == nil {
		return nil
	}
	stderr = strings.TrimSpace(stderr)
	if stderr == "" {
		return err
	}
	return fmt.Errorf("startup watcher: %s", stderr)
}

// DismissKnownDialogs best-effort clears known trust/permissions dialogs on a
// running session using a bounded timeout.
//
// Unlike the startup path (dismissStartupDialogs), this mid-session clear is
// deliberately not given a trusted import root: exec has no reliable
// mid-session work-dir lookup for a running box (the workdir is provision-half
// and is not persisted to queryable meta), so there is no exec analog of tmux's
// GetPaneWorkDir here. Leaving the root empty means the external-CLAUDE.md-import
// modal fails closed on this path — it is left for a human rather than
// auto-accepted — which is the safe asymmetry: the common startup case is
// gated, and the rare mid-session re-surface (resume/reattach) never
// auto-trusts an unverified import.
func (p *Provider) DismissKnownDialogs(ctx context.Context, name string, timeout time.Duration) error {
	return runtime.AcceptStartupDialogsWithTimeout(ctx, timeout,
		func(lines int) (string, error) { return p.Peek(name, lines) },
		func(keys ...string) error { return p.SendKeys(name, keys...) },
	)
}

// Stop destroys the named session: script stop <name>
func (p *Provider) Stop(name string) error {
	_, err := p.run(nil, "stop", name)
	return err
}

// execTmuxSession is the in-box tmux session the carrier addresses. An exec-pack
// runtime runs one agent per box in a tmux session named "main" (the tmux-in-box
// convention shared with the Kubernetes provider), so the driving verbs are
// reproduced as tmux commands over the exec op rather than dedicated
// nudge/peek/send-keys/interrupt/clear-scrollback wire ops.
const execTmuxSession = "main"

// defaultLaunchShell is the in-box command launchAgent runs when the config
// carries no Command (a holding shell, matching the welded packs' default).
const defaultLaunchShell = "/bin/sh"

// carrier drives the in-box tmux session over this provider's own exec op.
func (p *Provider) carrier() runtime.Carrier {
	return runtime.NewTmuxCarrier(p, execTmuxSession)
}

// The dedicated-wire-op driving helpers. The public driving methods
// (Nudge/Peek/SendKeys/Interrupt/ClearScrollback) try the tmux carrier over the
// exec op first and fall back to these when the runtime does not implement exec
// (ErrExecUnsupported): a pack that ships exec + tmux-in-box is
// driven over the carrier, while a pack that only implements the older dedicated
// driving ops (the gc-session-k8s reference) keeps working unchanged. The
// startup readiness + dialog-dismissal subsystem calls the same public methods,
// so it inherits the same carrier-then-fallback behavior. (watch-startup, a
// streaming op the request/response exec connection cannot carry, remains a
// direct wire op.)
func (p *Provider) nudgeOp(name string, content []runtime.ContentBlock) error {
	message := runtime.FlattenText(content)
	if message == "" {
		return nil
	}
	_, err := p.run([]byte(message), "nudge", name)
	return err
}

func (p *Provider) peekOp(name string, lines int) (string, error) {
	return p.run(nil, "peek", name, strconv.Itoa(lines))
}

func (p *Provider) sendKeysOp(name string, keys ...string) error {
	_, err := p.run(nil, append([]string{"send-keys", name}, keys...)...)
	return err
}

func (p *Provider) interruptOp(name string) error {
	_, err := p.run(nil, "interrupt", name)
	return err
}

func (p *Provider) clearScrollbackOp(name string) error {
	_, err := p.run(nil, "clear-scrollback", name)
	return err
}

// Interrupt sends a soft interrupt (Ctrl-C) to the in-box tmux session over the
// exec connection, falling back to the dedicated interrupt op when the runtime
// does not implement exec.
func (p *Provider) Interrupt(name string) error {
	if err := p.carrier().Interrupt(context.Background(), name); !errors.Is(err, runtime.ErrExecUnsupported) {
		return err
	}
	return p.interruptOp(name)
}

// IsRunning checks if the session is alive: script is-running <name>
// Returns true only if stdout is "true". Errors → false.
func (p *Provider) IsRunning(name string) bool {
	out, err := p.run(nil, "is-running", name)
	if err != nil {
		return false
	}
	return strings.TrimSpace(out) == "true"
}

// IsAttached reports terminal attachment via `script is-attached <name>`
// when the executable declared the report-attachment capability in its
// protocol handshake; otherwise it is always false. Op errors read as
// not attached.
func (p *Provider) IsAttached(name string) bool {
	if !p.handshakeCapability(runtime.ProtocolCapabilityReportAttachment) {
		return false
	}
	out, err := p.run(nil, "is-attached", name)
	if err != nil {
		return false
	}
	return out == "true"
}

// Attach connects the terminal to the session: script attach <name>
func (p *Provider) Attach(name string) error {
	return p.runWithTTY("attach", name)
}

// ProcessAlive checks for a live agent process: script process-alive <name>
// Process names are sent on stdin, one per line.
// Returns true if processNames is empty (per interface contract).
func (p *Provider) ProcessAlive(name string, processNames []string) bool {
	if len(processNames) == 0 {
		return true
	}
	stdin := []byte(strings.Join(processNames, "\n"))
	out, err := p.run(stdin, "process-alive", name)
	if err != nil {
		return false
	}
	// A runtime that does not implement process-alive answers exit 2, which run
	// maps to empty output. Treat unimplemented/unknown as ALIVE (liveness for
	// such runtimes is gated by IsRunning) — never as a spurious "dead" that
	// would make ObserveLiveness reap a live session. Only an explicit "false"
	// reports a dead agent.
	s := strings.TrimSpace(out)
	return s == "" || s == "true"
}

// Nudge delivers content as input to the in-box tmux session (typed, then
// submitted) over the exec connection, falling back to the dedicated nudge op
// when the runtime does not implement exec. Empty content is a no-op.
func (p *Provider) Nudge(name string, content []runtime.ContentBlock) error {
	if err := p.carrier().Nudge(context.Background(), name, content); !errors.Is(err, runtime.ErrExecUnsupported) {
		return err
	}
	return p.nudgeOp(name, content)
}

// SetMeta stores a key-value pair: script set-meta <name> <key>
// The value is sent on stdin.
func (p *Provider) SetMeta(name, key, value string) error {
	_, err := p.run([]byte(value), "set-meta", name, key)
	return err
}

// GetMeta retrieves a metadata value: script get-meta <name> <key>
// Returns ("", nil) if stdout is empty.
func (p *Provider) GetMeta(name, key string) (string, error) {
	return p.run(nil, "get-meta", name, key)
}

// RemoveMeta removes a metadata key: script remove-meta <name> <key>
func (p *Provider) RemoveMeta(name, key string) error {
	_, err := p.run(nil, "remove-meta", name, key)
	return err
}

// Peek captures the last `lines` of the in-box tmux pane (all scrollback when
// lines <= 0) over the exec connection, falling back to the dedicated peek op
// when the runtime does not implement exec.
func (p *Provider) Peek(name string, lines int) (string, error) {
	out, err := p.carrier().Peek(context.Background(), name, lines)
	if errors.Is(err, runtime.ErrExecUnsupported) {
		return p.peekOp(name, lines)
	}
	return out, err
}

// ListRunning returns sessions matching a prefix: script list-running <prefix>
// Returns one name per stdout line. Empty stdout → empty slice (not nil).
func (p *Provider) ListRunning(prefix string) ([]string, error) {
	out, err := p.run(nil, "list-running", prefix)
	if err != nil {
		return nil, err
	}
	if out == "" {
		return []string{}, nil
	}
	return strings.Split(out, "\n"), nil
}

// ClearScrollback clears the in-box tmux session's scrollback over the exec
// connection, falling back to the dedicated clear-scrollback op when the runtime
// does not implement exec.
func (p *Provider) ClearScrollback(name string) error {
	if err := p.carrier().ClearScrollback(context.Background(), name); !errors.Is(err, runtime.ErrExecUnsupported) {
		return err
	}
	return p.clearScrollbackOp(name)
}

// CheckImage verifies that a container image exists locally by invoking:
// script check-image <image>. Non-container providers return exit 2 (unknown
// operation), which runWithTimeout treats as success — making this a safe
// no-op for tmux-only setups.
func (p *Provider) CheckImage(image string) error {
	_, err := p.run(nil, "check-image", image)
	return err
}

// CopyTo copies src into the named session at relDst: script copy-to <name> <src> <relDst>
// Best-effort: returns nil on error.
func (p *Provider) CopyTo(name, src, relDst string) error {
	_, err := p.run(nil, "copy-to", name, src, relDst)
	return err
}

// SendKeys sends bare tmux-style keystrokes (e.g. "Enter", "Down") to the in-box
// tmux session over the exec connection; used for dialog dismissal and other
// non-text input. Falls back to the dedicated send-keys op when the runtime does
// not implement exec.
func (p *Provider) SendKeys(name string, keys ...string) error {
	if err := p.carrier().SendKeys(context.Background(), name, keys...); !errors.Is(err, runtime.ErrExecUnsupported) {
		return err
	}
	return p.sendKeysOp(name, keys...)
}

// RunLive re-applies session_live commands. For exec providers, runs
// commands via the adapter script. Best-effort: returns nil on failure.
func (p *Provider) RunLive(_ string, _ runtime.Config) error {
	return nil // exec providers don't support live re-apply yet
}

// Capabilities reports exec provider capabilities as declared by the
// executable's protocol handshake (zero capabilities for scripts without
// a `protocol` op, or when the handshake failed — the failure stays
// observable via Protocol).
func (p *Provider) Capabilities() runtime.ProviderCapabilities {
	return runtime.ProviderCapabilities{
		CanReportAttachment: p.handshakeCapability(runtime.ProtocolCapabilityReportAttachment),
		CanReportActivity:   p.handshakeCapability(runtime.ProtocolCapabilityReportActivity),
		CanStream:           p.handshakeCapability(runtime.ProtocolCapabilityProcStream),
		CanAttachTTY:        p.handshakeCapability(runtime.ProtocolCapabilityTTYAttach),
	}
}

// SleepCapability reports that exec-backed sessions support timed-only idle
// sleep via controller-driven lifecycle decisions.
func (p *Provider) SleepCapability(string) runtime.SessionSleepCapability {
	return runtime.SessionSleepCapabilityTimedOnly
}

// GetLastActivity returns the last activity time: script get-last-activity <name>
// Expects RFC3339 on stdout, or empty for unsupported. Malformed → zero time.
func (p *Provider) GetLastActivity(name string) (time.Time, error) {
	out, err := p.run(nil, "get-last-activity", name)
	if err != nil {
		return time.Time{}, err
	}
	if out == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339, out)
	if err != nil {
		// Malformed timestamp → zero time, no error.
		return time.Time{}, nil
	}
	return t, nil
}

// Provider implements the optional connection primitive.
var (
	_ runtime.ExecProvider     = (*Provider)(nil)
	_ runtime.RelaunchProvider = (*Provider)(nil)
)

// Exec runs argv inside the session via the RPP `exec` op and implements
// [runtime.ExecProvider]. argv is POSIX shell-quoted onto the op's stdin (the
// v0 wire op carries the command on stdin and the runtime runs it, e.g. via
// `sh -c "$(cat)"`), and the op's exit code is the command's exit code. A
// runtime whose script does not implement exec (exit 2) yields
// [runtime.ErrExecUnsupported]; the driving methods then fall back to the
// dedicated nudge/peek/send-keys/interrupt/clear-scrollback wire ops.
//
// Because the v0 `exec` op uses stdin for the command itself, the command's
// own stdin is not separately available; the driving ops reproduced over Exec
// (tmux send-keys / capture-pane / …) do not need it.
func (p *Provider) Exec(ctx context.Context, name string, argv []string) ([]byte, int, error) {
	command := shellQuote(argv)
	cmdCtx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	cmd := exec.CommandContext(cmdCtx, p.script, "exec", name)
	cmd.WaitDelay = 2 * time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.Stdin = strings.NewReader(command)

	err := cmd.Run()
	if err != nil {
		// A context timeout/cancellation kills the process, so cmd.Run reports
		// an *ExitError (signal: killed) with a -1 code. Classify that as a
		// transport failure BEFORE reading any exit code, so a timed-out op is
		// never misreported as a clean command result.
		if cmdCtx.Err() != nil {
			return nil, -1, fmt.Errorf("exec provider %s exec %s: %w", p.script, name, cmdCtx.Err())
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			if exitErr.ExitCode() == 2 {
				// Exit 2 is overloaded: the RPP "unknown op" sentinel AND a
				// possible in-box command exit (the exec op forwards the command's
				// exit code). A runtime that DECLARES the exec connection in its
				// handshake implements the op, so exit 2 is the command's own exit
				// code; only an UNDECLARED runtime means "op unimplemented" — fall
				// back to the dedicated driving ops.
				if p.handshakeCapability(runtime.ProtocolCapabilityConnectionExec) {
					return stdout.Bytes(), 2, nil
				}
				return nil, 0, fmt.Errorf("%w: %s exec %s", runtime.ErrExecUnsupported, p.script, name)
			}
			// A non-zero (non-2) exit is the command's own result, not a
			// transport failure: return the output and the code, no error.
			return stdout.Bytes(), exitErr.ExitCode(), nil
		}
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, -1, fmt.Errorf("exec provider %s exec %s: %s", p.script, name, msg)
	}
	return stdout.Bytes(), 0, nil
}

// shellQuote renders argv as a single POSIX shell command string (each
// argument single-quoted, embedded single quotes escaped as '\”), so a
// runtime's `exec` handler can run it verbatim via `sh -c`.
func shellQuote(argv []string) string {
	quoted := make([]string, len(argv))
	for i, a := range argv {
		quoted[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
	}
	return strings.Join(quoted, " ")
}
