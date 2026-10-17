package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
)

// The box carrier's fenced input (RUNTIME-CONTRACT-009): the same guard the
// local tmux provider runs, issued over the box connection. A fenced
// operation reads the box tmux's agent pane once ("display-message" of
// TmuxFenceReadFormat on the carrier's target) and needs it live and holding
// the fence as its own value; then every input command runs inside one
// if-shell -F on that pane's id and fence (TmuxGuardedInputArgs), so a pane
// respawned as another incarnation between the read and a send takes nothing.
// Each of these tmux commands runs in a box shell that reports tmux's own
// failure (carrierTmuxFailed), so a command the box did not answer, whether
// the connection exited non-zero or tmux in the box failed, is a
// *CarrierFenceUnreadError and never an absent miss: absence is only what a
// live box tmux answers. An Exec error instead (a spawn or transport failure,
// a timeout, a cancel) is transport's plain ErrRuntimeUnavailable, which does
// not say whether part of the input went in first. Text reaches tmux through
// a named buffer loaded by a box shell from its argv, never through a tmux
// command string. Unlike the tmux provider, the carrier never adopts a fence
// from the box's session environment: a pane with no own fence (started
// before the carrier published one) is unproven until the agent is relaunched.

// ErrCarrierFenceUnread marks a fenced carrier command the box did not
// answer (CarrierFenceUnreadError).
var ErrCarrierFenceUnread = errors.New("the box did not answer a fenced input command")

// CarrierFenceUnreadError is a fenced carrier command the box did not answer:
// the connection's own non-zero exit (ssh 255, kubectl or docker exec 1), or
// tmux in the box failing (no server, a wrong socket, no tmux binary). It
// cannot tell an absent incarnation from an outage, so it is
// ErrRuntimeUnavailable and ErrCarrierFenceUnread, never a miss and never
// gone; the provider that owns the connection may still tell an absent box
// apart (exec asks its pack's is-running). Partial reports that some of the
// operation's effect was applied first, as FenceMismatchError's does.
type CarrierFenceUnreadError struct {
	Session string
	Partial bool
	// Detail names the unanswered command and how it failed.
	Detail string
}

// Error names the session and the unanswered command; never a token.
func (e *CarrierFenceUnreadError) Error() string {
	partial := ""
	if e.Partial {
		partial = ", after partial input"
	}
	return fmt.Sprintf("%s: fenced input to %q: %s: %s%s", ErrRuntimeUnavailable.Error(), e.Session, ErrCarrierFenceUnread.Error(), e.Detail, partial)
}

// Unwrap is ErrRuntimeUnavailable and ErrCarrierFenceUnread.
func (e *CarrierFenceUnreadError) Unwrap() []error {
	return []error{ErrRuntimeUnavailable, ErrCarrierFenceUnread}
}

// carrierBufferSeq numbers the carrier's paste buffers.
var carrierBufferSeq atomic.Uint64

// carrierBufferName is a new paste buffer name, unique to this process.
func carrierBufferName() string {
	return fmt.Sprintf("gc-nudge-%d-c%d", os.Getpid(), carrierBufferSeq.Add(1))
}

// carrierTmuxFailed starts the last line a carrier box script prints when
// its tmux fails, followed by tmux's exit status. The script itself then
// exits 0, so a non-zero exit is the connection's own.
const carrierTmuxFailed = "gc-tmux-failed:"

// carrierTmuxReport ends a carrier box script: it drops tmux's stderr and
// reports a tmux that failed as carrierTmuxFailed and its exit status.
const carrierTmuxReport = ` 2>/dev/null || echo "` + carrierTmuxFailed + `$?"`

// carrierTmuxScript runs tmux with the script's arguments.
const carrierTmuxScript = `tmux "$@"` + carrierTmuxReport

// carrierLoadBufferScript loads its first argument into the buffer its
// second names, byte for byte (printf, not echo, and no tmux parsing).
const carrierLoadBufferScript = `printf '%s' "$1" | tmux load-buffer -b "$2" -` + carrierTmuxReport

// carrierGuard is one fenced operation on one box session: the digest and
// the pane its read proved, and whether any of its effect was applied.
type carrierGuard struct {
	c       *tmuxCarrier
	name    string
	digest  string
	pane    string
	applied bool
}

// fenced runs body as one fenced operation on session name: it validates f
// and reads the agent pane's fence before any input.
func (c *tmuxCarrier) fenced(ctx context.Context, name string, f InputFence, body func(*carrierGuard) error) error {
	if err := f.Validate(); err != nil {
		return err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	g := &carrierGuard{c: c, name: name, digest: f.Digest()}
	if err := g.verify(ctx); err != nil {
		return err
	}
	return body(g)
}

// miss is the operation's mismatch finding found.
func (g *carrierGuard) miss(found string) error {
	return &FenceMismatchError{Session: g.name, Found: found, Partial: g.applied}
}

// unread is the operation's error for a command the box did not answer.
func (g *carrierGuard) unread(detail string) error {
	return &CarrierFenceUnreadError{Session: g.name, Partial: g.applied, Detail: detail}
}

// transport is what a fenced operation reports for a connection failure: a
// pack without the exec op cannot fence (ErrInputFenceUnsupported); any
// other failure left the incarnation unproven, so it is
// ErrRuntimeUnavailable, never a miss and never gone. It does not carry
// Partial: input applied before the failure goes unreported.
func (g *carrierGuard) transport(err error) error {
	if errors.Is(err, ErrExecUnsupported) {
		return fmt.Errorf("session %q: %w: %w", g.name, ErrInputFenceUnsupported, err)
	}
	return fmt.Errorf("%w: fenced input to %q: %w", ErrRuntimeUnavailable, g.name, err)
}

// tmux runs tmux with args in a box shell (carrierTmuxScript) and returns
// what it printed (answer).
func (g *carrierGuard) tmux(ctx context.Context, step string, args ...string) (string, error) {
	out, code, err := g.c.conn.Exec(ctx, g.name, append([]string{"sh", "-c", carrierTmuxScript, "sh"}, args...))
	return g.answer(step, out, code, err)
}

// answer is the trimmed output of a carrier box script the connection ran
// for step. A connection failure is transport's; the connection's non-zero
// exit, or a last line reporting tmux's failure (carrierTmuxFailed), is a
// command the box did not answer (unread).
func (g *carrierGuard) answer(step string, out []byte, code int, err error) (string, error) {
	switch {
	case err != nil:
		return "", g.transport(err)
	case code != 0:
		return "", g.unread(fmt.Sprintf("%s: the box connection exited %d", step, code))
	}
	trimmed := strings.TrimSpace(string(out))
	status, reported := strings.CutPrefix(trimmed[strings.LastIndexByte(trimmed, '\n')+1:], carrierTmuxFailed)
	if _, parseErr := strconv.ParseUint(status, 10, 8); reported && parseErr == nil {
		return "", g.unread(fmt.Sprintf("%s: tmux in the box exited %s", step, status))
	}
	return trimmed, nil
}

// verify is the operation's precheck: the carrier target's pane must be live
// and hold the guard's digest as its own fence. A box tmux answering with no
// pane for the target finds it absent; a read the box did not answer is
// unread, and one answered with something else cannot tell either, so it is
// ErrRuntimeUnavailable.
func (g *carrierGuard) verify(ctx context.Context) error {
	out, err := g.tmux(ctx, "reading the fence", "display-message", "-p", "-t", g.c.target, TmuxFenceReadFormat)
	if err != nil {
		return err
	}
	r, ok := ParseTmuxFenceRead(out)
	switch {
	case !ok:
		return fmt.Errorf("%w: reading input fence of %q: unexpected tmux output %q", ErrRuntimeUnavailable, g.name, out)
	case r.Absent() || r.Dead:
		return g.miss(r.Found())
	case r.Own() != g.digest:
		return g.miss(r.Found())
	}
	g.pane = r.Pane
	return nil
}

// run runs inner as one guarded input command on the proven pane. buffer is
// the paste buffer inner consumes, deleted when the guard misses or the
// command fails; text marks input that is the operation's effect. A guarded
// command the box did not answer (tmux in the box failing, as when its server
// died after the read) is unread, never a miss: tmux answers a pane it
// cannot resolve through the guard's false branch.
func (g *carrierGuard) run(ctx context.Context, inner []string, buffer string) error {
	guarded, err := TmuxGuardedInputArgs(g.pane, g.pane, g.digest, inner, buffer)
	if err != nil {
		g.deleteBuffer(buffer)
		return err
	}
	out, err := g.tmux(ctx, "a guarded input command", guarded...)
	if err != nil {
		g.deleteBuffer(buffer)
		return err
	}
	switch {
	case out == "":
		g.applied = true
		return nil
	case strings.HasPrefix(out, TmuxFenceMissPrefix):
		r, ok := ParseTmuxFenceRead(strings.TrimPrefix(out, TmuxFenceMissPrefix))
		switch {
		case !ok:
			return g.miss(FenceFoundUnproven)
		case r.Absent() || r.Pane != g.pane:
			// No session, or tmux evaluated the window's active pane: the
			// agent pane is gone.
			return g.miss(FenceFoundAbsent)
		}
		return g.miss(r.Found())
	default:
		return fmt.Errorf("%w: fenced input to %q: unexpected tmux output %q", ErrRuntimeUnavailable, g.name, out)
	}
}

// paste loads text into a new buffer and pastes it, guarded.
func (g *carrierGuard) paste(ctx context.Context, text string) error {
	buffer := carrierBufferName()
	out, code, err := g.c.conn.Exec(ctx, g.name, []string{"sh", "-c", carrierLoadBufferScript, "sh", text, buffer})
	if _, err = g.answer("loading a paste buffer", out, code, err); err != nil {
		return err
	}
	return g.run(ctx, []string{"paste-buffer", "-d", "-r", "-b", buffer, "-t", g.pane}, buffer)
}

// deleteBuffer removes buffer, best-effort.
func (g *carrierGuard) deleteBuffer(buffer string) {
	if buffer != "" {
		_, _, _ = g.c.conn.Exec(context.Background(), g.name, []string{"tmux", "delete-buffer", "-b", buffer})
	}
}

// NudgeFenced delivers content as Nudge does (the text, then Enter), with
// both conditional on the agent pane holding fence.
func (c *tmuxCarrier) NudgeFenced(ctx context.Context, name string, content []ContentBlock, fence InputFence) error {
	return c.fenced(ctx, name, fence, func(g *carrierGuard) error {
		message := FlattenText(content)
		if message == "" {
			return nil
		}
		if err := g.paste(ctx, message); err != nil {
			return err
		}
		return g.run(ctx, []string{"send-keys", "-t", g.pane, "Enter"}, "")
	})
}

// SendKeysFenced sends keys (tmux key names only) as SendKeys does, in one
// guarded command.
func (c *tmuxCarrier) SendKeysFenced(ctx context.Context, name string, fence InputFence, keys ...string) error {
	for _, k := range keys {
		if !TmuxKeyName(k) {
			return fmt.Errorf("fenced send-keys to %q: %q is not a key name", name, k)
		}
	}
	return c.FencedKeys(ctx, name, fence, func(sendKeys func(keys ...string) error) error {
		return sendKeys(keys...)
	})
}

// FencedKeys runs body as one fenced operation whose sendKeys sends tmux key
// names as SendKeys does, each send guarded on the one fence read.
func (c *tmuxCarrier) FencedKeys(ctx context.Context, name string, fence InputFence, body func(sendKeys func(keys ...string) error) error) error {
	return c.fenced(ctx, name, fence, func(g *carrierGuard) error {
		return body(func(keys ...string) error {
			for _, k := range keys {
				if !TmuxKeyName(k) {
					return fmt.Errorf("fenced send-keys to %q: %q is not a key name", name, k)
				}
			}
			if len(keys) == 0 {
				return nil
			}
			return g.run(ctx, append([]string{"send-keys", "-t", g.pane}, keys...), "")
		})
	})
}

// InterruptFenced sends Ctrl-C as Interrupt does, guarded.
func (c *tmuxCarrier) InterruptFenced(ctx context.Context, name string, fence InputFence) error {
	return c.fenced(ctx, name, fence, func(g *carrierGuard) error {
		return g.run(ctx, []string{"send-keys", "-t", g.pane, "C-c"}, "")
	})
}

// PublishInputFence sets fence as the fence of the carrier target's pane.
func (c *tmuxCarrier) PublishInputFence(ctx context.Context, name string, fence InputFence) error {
	if err := fence.Validate(); err != nil {
		return err
	}
	_, code, err := c.tmux(ctx, name, TmuxPublishInputFenceArgs(c.target, fence)...)
	switch {
	case err != nil:
		return fmt.Errorf("publishing input fence of %q: %w", name, err)
	case code != 0:
		return fmt.Errorf("publishing input fence of %q: tmux exited %d", name, code)
	}
	return nil
}
