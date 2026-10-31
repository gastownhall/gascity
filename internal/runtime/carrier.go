package runtime

import (
	"context"
	"strconv"
	"strings"
	"time"
)

// Carrier drives the high-level session interactions — input delivery, output
// capture, interrupt, scrollback — over a connection to the box, separating
// HOW an interaction is realized from the connection that reaches the box. Its
// scope is those four driving verbs plus their fenced forms
// (RUNTIME-CONTRACT-009); liveness, attachment, activity, and metadata stay
// provider-specific, and higher-level orchestration (startup-dialog
// acceptance, no-wait nudge, wait-for-idle) is composed ABOVE a Carrier out of
// Peek/SendKeys (or FencedKeys), not added to it.
//
// Every unfenced op returns the underlying transport error verbatim. Whether a
// failure is fatal or best-effort is the PROVIDER facade's policy: a provider
// decides per verb which errors to discard when it delegates here (e.g.
// Kubernetes treats a missing pod as a no-op for SendKeys but propagates a
// genuine transport failure to a live pod, and propagates both for Nudge) —
// the Carrier itself never swallows. The fenced ops classify instead: a miss
// is a *FenceMismatchError, a transport failure wraps ErrRuntimeUnavailable
// (or ErrInputFenceUnsupported without the exec op), and deleting a staged
// paste buffer after a failure is best-effort.
//
// The tmux carrier ([NewTmuxCarrier]) realizes these verbs by issuing tmux
// commands over an [ExecProvider]. It is the shared driver for tmux-in-a-box
// exec-connection runtimes (Kubernetes, the SSH backend, and packs that opt
// into the tmux-box model). Such a provider must expose a name-keyed
// [ExecProvider] and owns name->box and name->target resolution plus any
// best-effort swallowing in its own adapter — the carrier knows nothing about
// pods or how the box is reached. Adopting it for a runtime that drives input
// via its own dedicated ops (e.g. an exec pack's nudge/peek subcommands) is a
// protocol change, not a silent migration. The local tmux control driver and
// the ACP stream driver realize these verbs differently and keep their own
// behavior.
type Carrier interface {
	// Nudge delivers content as input to the session, followed by a submit.
	Nudge(ctx context.Context, name string, content []ContentBlock) error
	// SendKeys sends bare keystrokes (e.g. "Enter", "C-c") without a submit.
	SendKeys(ctx context.Context, name string, keys ...string) error
	// Peek captures the last lines of output (all scrollback when lines <= 0).
	Peek(ctx context.Context, name string, lines int) (string, error)
	// Interrupt sends a soft interrupt (Ctrl-C) to the session.
	Interrupt(ctx context.Context, name string) error
	// ClearScrollback clears the session's scrollback history.
	ClearScrollback(ctx context.Context, name string) error

	// NudgeFenced delivers content as Nudge does, only to the incarnation
	// fence names (RUNTIME-CONTRACT-009): a miss is a *FenceMismatchError.
	NudgeFenced(ctx context.Context, name string, content []ContentBlock, fence InputFence) error
	// SendKeysFenced sends keys as SendKeys does, only to fence's incarnation.
	SendKeysFenced(ctx context.Context, name string, fence InputFence, keys ...string) error
	// InterruptFenced interrupts as Interrupt does, only fence's incarnation.
	InterruptFenced(ctx context.Context, name string, fence InputFence) error
	// FencedKeys runs body as one fenced operation: the fence is read once
	// before body, every key body sends goes through that read's guard, and
	// a miss after any key applied is partial. It is how a ctx-fenced
	// composite (dialog dismissal) sends its keys.
	FencedKeys(ctx context.Context, name string, fence InputFence, body func(sendKeys func(keys ...string) error) error) error
	// PublishInputFence records fence as the incarnation the session's
	// agent now runs, so fenced input to it can be proven.
	PublishInputFence(ctx context.Context, name string, fence InputFence) error
}

// tmuxCarrier drives a tmux session living inside the box by issuing tmux
// commands over an [ExecProvider] connection. target is the in-box tmux session
// the commands address (e.g. "main"); it is fixed per carrier today — a
// name->target resolver is the natural extension if one connection ever
// multiplexes sessions on distinct targets. The mapping mirrors the tmux
// commands the Kubernetes provider issued over execInPod, except Nudge, which
// pauses before its Enter and re-presses a dropped one (see Nudge). The
// provider keeps its own per-verb error policy; see [Carrier].
type tmuxCarrier struct {
	conn   ExecProvider
	target string
	// sleep waits d or until ctx ends. Tests swap it to run without real time.
	sleep func(ctx context.Context, d time.Duration) error
}

// NewTmuxCarrier returns a [Carrier] that drives the in-box tmux session
// target over conn.
func NewTmuxCarrier(conn ExecProvider, target string) Carrier {
	return &tmuxCarrier{conn: conn, target: target, sleep: sleepCtx}
}

// Nudge submit tuning, matching the local tmux runtime (internal/runtime/tmux:
// the 500ms paste debounce in NudgeSession and submitEnterMaxSends).
const (
	// nudgeSubmitDebounce is the pause between typing the text and pressing
	// Enter, and again between an Enter and the look that checks it landed.
	// An Enter sent right behind the text can be dropped: Gemini CLI 0.64
	// drops it and the text sits unsent in its input box.
	nudgeSubmitDebounce = 500 * time.Millisecond
	// nudgeSubmitMaxPresses bounds the Enters one Nudge sends.
	nudgeSubmitMaxPresses = 3
	// nudgeBoxLines is how many of the pane's last lines (blank rows at the
	// bottom dropped) count as "the input box" when checking whether an Enter
	// took the text.
	nudgeBoxLines = 10
	// nudgeTextProbeRunes is the length of the tail of the message looked for
	// in the box. A short tail survives the box wrapping a long line.
	nudgeTextProbeRunes = 24
)

// tmux runs `tmux <args...>` in the box over the connection and returns its
// standard output and exit code. A non-zero command exit is reported via err
// only when the connection itself surfaces it; callers that are best-effort
// discard err.
func (c *tmuxCarrier) tmux(ctx context.Context, name string, args ...string) ([]byte, int, error) {
	return c.conn.Exec(ctx, name, append([]string{"tmux"}, args...))
}

// Nudge types content into the pane, waits nudgeSubmitDebounce, and presses
// Enter. When the box showed the typed text before the Enter and still shows
// it unchanged after, the Enter was dropped, so it presses again, up to
// nudgeSubmitMaxPresses Enters. This is the local tmux runtime's debounce and
// re-Enter (internal/runtime/tmux NudgeSession) for tmux-in-a-box runtimes.
// Waiting for a booting agent's prompt stays above the carrier: the
// provider's ready_prompt_prefix and ready_delay_ms gate the first nudge.
func (c *tmuxCarrier) Nudge(ctx context.Context, name string, content []ContentBlock) error {
	message := FlattenText(content)
	if message == "" {
		return nil
	}
	// Type the literal text, then submit — the two-step send-keys the k8s
	// provider uses (a single send-keys would interpret the text as key names).
	// If typing fails, the error surfaces and Enter is skipped: the caller
	// learns delivery failed (the pane may hold a half-typed, unsubmitted line).
	// [TmuxLiteralArgs] keeps the text byte-exact (E5).
	if _, _, err := c.tmux(ctx, name, append([]string{"send-keys", "-t", c.target, "-l"}, TmuxLiteralArgs(message)...)...); err != nil {
		return err
	}
	// Let the TUI take the text before the Enter (see nudgeSubmitDebounce).
	if err := c.sleep(ctx, nudgeSubmitDebounce); err != nil {
		return err
	}
	// Look at the box once before the first Enter. Re-pressing is only safe
	// when we can see the text sitting there; a box we cannot read (a peek
	// error, a paste shown as a label, an empty fake) gets one Enter, as
	// before.
	before, err := c.Peek(ctx, name, nudgeBoxLines)
	box := paneBox(before)
	watch := err == nil && boxShowsText(box, message)
	for press := 1; ; press++ {
		if _, _, err := c.tmux(ctx, name, "send-keys", "-t", c.target, "Enter"); err != nil {
			return err
		}
		if !watch || press == nudgeSubmitMaxPresses {
			return nil
		}
		// From here an Enter has gone out, so every exit reports success: a
		// caller that retried on an error would type the text twice.
		if c.sleep(ctx, nudgeSubmitDebounce) != nil {
			return nil
		}
		after, err := c.Peek(ctx, name, nudgeBoxLines)
		// A box that changed took the Enter, or moved for some other reason
		// and cannot be read; either way, stop. Only a box still showing
		// exactly the typed text gets another Enter, so a turn that started
		// is never pressed again.
		if err != nil || paneBox(after) != box {
			return nil
		}
	}
}

// paneBox is the last nudgeBoxLines lines of a pane capture, blank rows at the
// bottom dropped: the part of the screen that holds a TUI's input box.
func paneBox(pane string) string {
	lines := strings.Split(strings.TrimRight(pane, " \n"), "\n")
	if len(lines) > nudgeBoxLines {
		lines = lines[len(lines)-nudgeBoxLines:]
	}
	return strings.Join(lines, "\n")
}

// boxShowsText reports whether box shows the end of message: the last
// nudgeTextProbeRunes runes of its last non-blank line.
func boxShowsText(box, message string) bool {
	lines := strings.Split(strings.TrimSpace(message), "\n")
	probe := []rune(strings.TrimSpace(lines[len(lines)-1]))
	if len(probe) > nudgeTextProbeRunes {
		probe = probe[len(probe)-nudgeTextProbeRunes:]
	}
	return len(probe) > 0 && strings.Contains(box, string(probe))
}

// sleepCtx waits d, or until ctx ends.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *tmuxCarrier) SendKeys(ctx context.Context, name string, keys ...string) error {
	// k8s issues a bare no-op `send-keys -t <target>` for empty keys; we
	// short-circuit to issue nothing (behaviorally identical — no keystrokes).
	if len(keys) == 0 {
		return nil
	}
	_, _, err := c.tmux(ctx, name, append([]string{"send-keys", "-t", c.target}, keys...)...)
	return err
}

func (c *tmuxCarrier) Peek(ctx context.Context, name string, lines int) (string, error) {
	start := "-"
	if lines > 0 {
		start = "-" + strconv.Itoa(lines)
	}
	out, _, err := c.tmux(ctx, name, "capture-pane", "-t", c.target, "-p", "-S", start)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func (c *tmuxCarrier) Interrupt(ctx context.Context, name string) error {
	_, _, err := c.tmux(ctx, name, "send-keys", "-t", c.target, "C-c")
	return err
}

func (c *tmuxCarrier) ClearScrollback(ctx context.Context, name string) error {
	_, _, err := c.tmux(ctx, name, "clear-history", "-t", c.target)
	return err
}
