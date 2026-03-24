package exec

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
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
}

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

	// Dismiss startup dialogs using the same shared Go logic as tmux.
	if cfg.EmitsPermissionWarning || len(cfg.ProcessNames) > 0 {
		_ = runtime.AcceptStartupDialogs(ctx,
			func(lines int) (string, error) { return p.Peek(name, lines) },
			func(keys ...string) error { return p.SendKeys(name, keys...) },
		)
	}

	return nil
}

// Stop destroys the named session: script stop <name>
func (p *Provider) Stop(name string) error {
	_, err := p.run(nil, "stop", name)
	return err
}

// Interrupt sends an interrupt to the session: script interrupt <name>
func (p *Provider) Interrupt(name string) error {
	_, err := p.run(nil, "interrupt", name)
	return err
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

// IsAttached always returns false — the exec provider does not support
// attach detection.
func (p *Provider) IsAttached(_ string) bool { return false }

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
	return strings.TrimSpace(out) == "true"
}

// Nudge sends a message to the session: script nudge <name>
// The message is sent on stdin. Content blocks are flattened to text.
func (p *Provider) Nudge(name string, content []runtime.ContentBlock) error {
	message := runtime.FlattenText(content)
	if message == "" {
		return nil
	}
	_, err := p.run([]byte(message), "nudge", name)
	return err
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

// Peek captures output from the session: script peek <name> <lines>
func (p *Provider) Peek(name string, lines int) (string, error) {
	return p.run(nil, "peek", name, strconv.Itoa(lines))
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

// ClearScrollback clears the scrollback: script clear-scrollback <name>
func (p *Provider) ClearScrollback(name string) error {
	_, err := p.run(nil, "clear-scrollback", name)
	return err
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

// SendKeys sends bare tmux-style keystrokes (e.g., "Enter", "Down") to the
// named session: script send-keys <name> <key1> [key2 ...]
// Used for dialog dismissal and other non-text input.
func (p *Provider) SendKeys(name string, keys ...string) error {
	args := append([]string{"send-keys", name}, keys...)
	_, err := p.run(nil, args...)
	return err
}

// RunLive re-applies session_live commands. For exec providers, runs
// commands via the adapter script. Best-effort: returns nil on failure.
func (p *Provider) RunLive(_ string, _ runtime.Config) error {
	return nil // exec providers don't support live re-apply yet
}

// Capabilities reports exec provider capabilities. The exec provider
// delegates everything to a user-supplied script and does not natively
// support attachment or activity detection.
func (p *Provider) Capabilities() runtime.ProviderCapabilities {
	return runtime.ProviderCapabilities{}
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
