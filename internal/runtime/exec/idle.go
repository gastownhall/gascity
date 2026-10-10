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

// execIdleProbeBudget is the WaitForIdle timeout one idle proof needs on a
// pack with an idle boundary (see IdleProbeBudget). Each pane observation is
// a full round trip to the box, so two observations plus the poll interval fit
// whenever a capture takes under about 2.4s. A package variable so tests can
// change it.
var execIdleProbeBudget = 5 * time.Second

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

// readyPromptEnvKey is the in-box tmux session environment variable that
// carries the session's ready-prompt prefix, the same key the tmux provider
// publishes, so any provider instance can recover the prefix from the box.
const readyPromptEnvKey = "GC_READY_PROMPT_PREFIX"

// rememberReadyPrompt records the session's configured ready-prompt prefix so
// WaitForIdle scans for the agent's own prompt rather than the default one. A
// blank prefix records the default.
func (p *Provider) rememberReadyPrompt(name string, cfg runtime.Config) {
	p.readyPrompts.Store(name, idlePromptPrefix(cfg.ReadyPromptPrefix))
}

// forgetReadyPrompt drops the remembered ready-prompt prefix for name.
func (p *Provider) forgetReadyPrompt(name string) {
	p.readyPrompts.Delete(name)
}

// idlePromptPrefix returns configured, or tmux.DefaultReadyPromptPrefix when
// it is blank.
func idlePromptPrefix(configured string) string {
	if strings.TrimSpace(configured) == "" {
		return tmux.DefaultReadyPromptPrefix
	}
	return configured
}

// publishReadyPrompt writes the session's ready-prompt prefix into the in-box
// tmux session environment (readyPromptEnvKey), or unsets it for a blank
// prefix, once the in-box session exists. The in-memory cache only covers the
// provider instance that started the session; this copy lets any other
// instance (a restarted orchestrator, a rebuilt provider, a CLI process)
// recover it in readyPromptPrefix. Only a pack with an idle boundary has the
// in-box tmux session and the exec connection this needs. Best effort: on
// failure other instances fall back to the default prompt.
func (p *Provider) publishReadyPrompt(ctx context.Context, name string, cfg runtime.Config) {
	if !p.idleBoundaryDeclared() {
		return
	}
	argv := []string{"tmux", "set-environment", "-t", execTmuxSession, readyPromptEnvKey, cfg.ReadyPromptPrefix}
	if strings.TrimSpace(cfg.ReadyPromptPrefix) == "" {
		argv = []string{"tmux", "set-environment", "-t", execTmuxSession, "-u", readyPromptEnvKey}
	}
	_, _, _ = p.Exec(ctx, name, argv)
}

// readyPromptPrefix returns the ready-prompt prefix for name, reading it within
// limit. A prefix this instance remembered from Start is returned without a
// round trip. Otherwise (this instance did not start the session, for example
// after an orchestrator restart) it is read from the in-box tmux session
// environment that publishReadyPrompt wrote and remembered; an unset variable
// means the default prompt and is remembered too. A failed read returns
// tmux.DefaultReadyPromptPrefix without remembering it, so the next call
// retries.
func (p *Provider) readyPromptPrefix(ctx context.Context, name string, limit time.Duration) string {
	if v, ok := p.readyPrompts.Load(name); ok {
		if prefix, ok := v.(string); ok {
			return prefix
		}
	}
	prefix, expired, err := bounded(ctx, limit, func(ctx context.Context) (string, error) {
		return p.readPublishedReadyPrompt(ctx, name)
	})
	if expired || err != nil {
		return tmux.DefaultReadyPromptPrefix
	}
	p.readyPrompts.LoadOrStore(name, prefix)
	return prefix
}

// readPublishedReadyPrompt reads readyPromptEnvKey from the in-box tmux
// session over the exec connection. It lists the whole session environment
// rather than asking for the one key, because tmux exits 1 both for an unset
// key and for a missing session, and the exec op does not carry the stderr
// that tells them apart: a "KEY=value" line is the prefix, a "-KEY" line or
// no line means unset (the default prompt), and a non-zero exit is an error.
func (p *Provider) readPublishedReadyPrompt(ctx context.Context, name string) (string, error) {
	out, code, err := p.Exec(ctx, name, []string{"tmux", "show-environment", "-t", execTmuxSession})
	if err != nil {
		return "", err
	}
	if code != 0 {
		return "", fmt.Errorf("exec provider: reading the tmux environment in %q: tmux exited %d", name, code)
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimRight(line, "\r")
		if value, ok := strings.CutPrefix(line, readyPromptEnvKey+"="); ok {
			return idlePromptPrefix(value), nil
		}
	}
	return tmux.DefaultReadyPromptPrefix, nil
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
	deadline := time.Now().Add(timeout)
	prefix := p.readyPromptPrefix(ctx, name, timeout)
	if err := ctx.Err(); err != nil {
		return err
	}
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return fmt.Errorf("exec provider: %q not idle within %s: %w (reading the ready prompt used the whole timeout)", name, timeout, context.DeadlineExceeded)
	}
	return waitForPaneIdle(ctx, p.carrier(), time.Now, name, prefix, remaining)
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
// returns no later than that (see bounded).
func boundedPeek(ctx context.Context, c runtime.Carrier, name string, limit time.Duration) (out string, expired bool, err error) {
	out, expired, err = bounded(ctx, limit, func(ctx context.Context) (string, error) {
		return c.Peek(ctx, name, tmux.PromptObservationLines)
	})
	if err != nil && expired {
		err = fmt.Errorf("exec provider: capturing %q: %w", name, err)
	}
	return out, expired, err
}

// bounded runs f under a context derived from ctx that expires after limit,
// and returns no later than that, even if f does not return promptly on
// cancellation (an exec op can outlive its context while a child of the pack
// script still holds its output open). An abandoned call finishes in the
// background and its result is dropped. expired reports that the limit was
// reached before f produced a usable result.
func bounded[T any](ctx context.Context, limit time.Duration, f func(context.Context) (T, error)) (val T, expired bool, err error) {
	callCtx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	type result struct {
		val T
		err error
	}
	done := make(chan result, 1)
	go func() {
		v, err := f(callCtx)
		done <- result{v, err}
	}()
	select {
	case r := <-done:
		return r.val, errors.Is(callCtx.Err(), context.DeadlineExceeded), r.err
	case <-callCtx.Done():
		var zero T
		return zero, errors.Is(callCtx.Err(), context.DeadlineExceeded), callCtx.Err()
	}
}

// IdleProbeBudget reports the timeout one idle proof needs on this pack and
// implements [runtime.IdleProbeBudgetProvider]: execIdleProbeBudget for a pack
// with an idle boundary, whose every pane observation is an exec round trip;
// zero otherwise, so the orchestrator keeps its default.
func (p *Provider) IdleProbeBudget() time.Duration {
	if p.idleBoundaryDeclared() {
		return execIdleProbeBudget
	}
	return 0
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
