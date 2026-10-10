package exec

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/tmux"
)

// execIdlePollInterval is the pause between two pane observations in
// WaitForIdle, matching the tmux provider's 200ms idle poll. A package
// variable so tests can shorten it.
var execIdlePollInterval = 200 * time.Millisecond

// execIdleRequiredObservations is how many consecutive idle pane observations
// WaitForIdle needs, mirroring the tmux provider: a single observation can
// land in the gap between two tool calls, when the prompt is visible but the
// agent is still working.
const execIdleRequiredObservations = 2

// idleBoundaryDeclared reports whether the pack declared everything an idle
// boundary needs: an activity clock (report-activity), an attachment probe
// (report-attachment), and the exec connection (proc.exec) over which the
// in-box tmux pane is observed. A pack that declares all three runs its agent
// in an in-box tmux session gc can read; a failed handshake counts as not
// declared.
func (p *Provider) idleBoundaryDeclared() bool {
	return p.handshakeCapability(runtime.ProtocolCapabilityReportActivity) &&
		p.handshakeCapability(runtime.ProtocolCapabilityReportAttachment) &&
		p.handshakeCapability(runtime.ProtocolCapabilityConnectionExec)
}

// rememberReadyPrompt records the session's configured ready-prompt prefix so
// WaitForIdle scans for the agent's own prompt rather than the default one. A
// blank prefix forgets any earlier value.
func (p *Provider) rememberReadyPrompt(name string, cfg runtime.Config) {
	if strings.TrimSpace(cfg.ReadyPromptPrefix) == "" {
		p.readyPrompts.Delete(name)
		return
	}
	p.readyPrompts.Store(name, cfg.ReadyPromptPrefix)
}

// forgetReadyPrompt drops the remembered ready-prompt prefix for name.
func (p *Provider) forgetReadyPrompt(name string) {
	p.readyPrompts.Delete(name)
}

// readyPromptPrefix returns the ready-prompt prefix remembered for name, or
// tmux.DefaultReadyPromptPrefix when this provider instance did not start the
// session (for example after an orchestrator restart).
func (p *Provider) readyPromptPrefix(name string) string {
	if v, ok := p.readyPrompts.Load(name); ok {
		if prefix, ok := v.(string); ok {
			return prefix
		}
	}
	return tmux.DefaultReadyPromptPrefix
}

// WaitForIdle waits until the in-box tmux pane shows the session's ready prompt
// with no busy indicator on two consecutive observations, and implements
// [runtime.IdleWaitProvider]. Each observation captures the last
// tmux.PromptObservationLines lines over the exec connection and applies
// tmux.PaneShowsIdlePrompt, so the boundary is the one local tmux uses.
//
// A pack that does not declare report-activity, report-attachment and
// proc.exec gets [runtime.ErrInteractionUnsupported] without any op being run.
//
// timeout is a hard upper bound: every capture runs under a context that
// expires at the deadline, and WaitForIdle returns at the deadline even if a
// capture is still in flight. On expiry the error wraps
// context.DeadlineExceeded; a canceled ctx returns ctx.Err().
func (p *Provider) WaitForIdle(ctx context.Context, name string, timeout time.Duration) error {
	if !p.idleBoundaryDeclared() {
		return runtime.ErrInteractionUnsupported
	}
	return waitForPaneIdle(ctx, p.carrier(), time.Now, name, p.readyPromptPrefix(name), timeout)
}

// waitForPaneIdle is WaitForIdle's poll loop over carrier c, reading time from
// now. It stops at the first of: the required consecutive idle observations,
// ctx cancellation, or now() reaching start+timeout.
func waitForPaneIdle(ctx context.Context, c runtime.Carrier, now func() time.Time, name, promptPrefix string, timeout time.Duration) error {
	deadline := now().Add(timeout)
	consecutive := 0
	var lastErr error
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		remaining := deadline.Sub(now())
		if remaining <= 0 {
			break
		}
		out, expired, err := boundedPeek(ctx, c, name, remaining)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if expired || !now().Before(deadline) {
			// The capture ran into the deadline: it was canceled there, and
			// an observation finished at or past the bound does not count.
			if err != nil {
				lastErr = err
			}
			break
		}
		switch {
		case err != nil:
			lastErr, consecutive = err, 0
		case tmux.PaneShowsIdlePrompt(paneLines(out), promptPrefix):
			consecutive++
			if consecutive >= execIdleRequiredObservations {
				return nil
			}
		default:
			consecutive = 0
		}
		wait := min(execIdlePollInterval, deadline.Sub(now()))
		if wait <= 0 {
			break
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	if lastErr != nil {
		return fmt.Errorf("exec provider: %q not idle within %s: %w (last capture: %w)", name, timeout, context.DeadlineExceeded, lastErr)
	}
	return fmt.Errorf("exec provider: %q not idle within %s: %w", name, timeout, context.DeadlineExceeded)
}

// boundedPeek captures the pane under a context that expires after limit and
// returns no later than that, even if the carrier does not return promptly
// on cancellation (the exec op can outlive its context while a child of the
// pack script still holds its output open). An abandoned capture finishes in
// the background and its result is dropped. expired reports that the limit
// was reached before the capture produced a usable result.
func boundedPeek(ctx context.Context, c runtime.Carrier, name string, limit time.Duration) (out string, expired bool, err error) {
	captureCtx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	type result struct {
		out string
		err error
	}
	done := make(chan result, 1)
	go func() {
		out, err := c.Peek(captureCtx, name, tmux.PromptObservationLines)
		done <- result{out, err}
	}()
	select {
	case r := <-done:
		return r.out, errors.Is(captureCtx.Err(), context.DeadlineExceeded), r.err
	case <-captureCtx.Done():
		return "", errors.Is(captureCtx.Err(), context.DeadlineExceeded), fmt.Errorf("exec provider: capturing %q: %w", name, captureCtx.Err())
	}
}

// paneLines splits captured pane output into lines; empty output has none.
func paneLines(out string) []string {
	if out == "" {
		return nil
	}
	return strings.Split(out, "\n")
}

// IsAttachedWithError reports whether a terminal is attached to the session
// via `script is-attached <name>`, separating "no client" from "could not
// tell", and implements [runtime.AttachmentObserverWithError]. A pack that did
// not declare report-attachment answers (false, nil) without running an op.
// An op failure wraps [runtime.ErrRuntimeUnavailable], which callers gating a
// destructive action treat as attached. A pack reports a session that does
// not exist by printing "false" and exiting 0.
func (p *Provider) IsAttachedWithError(name string) (bool, error) {
	if !p.handshakeCapability(runtime.ProtocolCapabilityReportAttachment) {
		return false, nil
	}
	out, err := p.run(nil, "is-attached", name)
	if err != nil {
		return false, fmt.Errorf("%w: %w", runtime.ErrRuntimeUnavailable, err)
	}
	return strings.TrimSpace(out) == "true", nil
}
