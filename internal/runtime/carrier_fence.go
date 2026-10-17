package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
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
// Text reaches tmux through a named buffer loaded by a box shell from its
// argv, never through a tmux command string. Unlike the tmux provider, the
// carrier never adopts a fence from the box's session environment: a pane
// with no own fence (started before the carrier published one) is unproven
// until the agent is relaunched.

// carrierBufferSeq numbers the carrier's paste buffers.
var carrierBufferSeq atomic.Uint64

// carrierBufferName is a new paste buffer name, unique to this process.
func carrierBufferName() string {
	return fmt.Sprintf("gc-nudge-%d-c%d", os.Getpid(), carrierBufferSeq.Add(1))
}

// carrierLoadBufferScript loads its first argument into the buffer its
// second names, byte for byte (printf, not echo, and no tmux parsing).
const carrierLoadBufferScript = `printf '%s' "$1" | tmux load-buffer -b "$2" -`

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

// transport is what a fenced operation reports for a connection failure: a
// pack without the exec op cannot fence (ErrInputFenceUnsupported); any
// other failure left the incarnation unproven, so it is
// ErrRuntimeUnavailable, never a miss and never gone.
func (g *carrierGuard) transport(err error) error {
	if errors.Is(err, ErrExecUnsupported) {
		return fmt.Errorf("session %q: %w: %w", g.name, ErrInputFenceUnsupported, err)
	}
	return fmt.Errorf("%w: fenced input to %q: %w", ErrRuntimeUnavailable, g.name, err)
}

// verify is the operation's precheck: the carrier target's pane must be live
// and hold the guard's digest as its own fence. A box tmux that cannot
// answer for the target (a non-zero exit: no server, no session) finds it
// absent.
func (g *carrierGuard) verify(ctx context.Context) error {
	out, code, err := g.c.conn.Exec(ctx, g.name, []string{"tmux", "display-message", "-p", "-t", g.c.target, TmuxFenceReadFormat})
	if err != nil {
		return g.transport(err)
	}
	if code != 0 {
		return g.miss(FenceFoundAbsent)
	}
	r, ok := ParseTmuxFenceRead(string(out))
	switch {
	case !ok:
		return fmt.Errorf("reading input fence of %q: unexpected tmux output %q", g.name, strings.TrimSpace(string(out)))
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
// command fails; text marks input that is the operation's effect.
func (g *carrierGuard) run(ctx context.Context, inner []string, buffer string) error {
	guarded, err := TmuxGuardedInputArgs(g.pane, g.pane, g.digest, inner, buffer)
	if err != nil {
		g.deleteBuffer(buffer)
		return err
	}
	out, code, err := g.c.conn.Exec(ctx, g.name, append([]string{"tmux"}, guarded...))
	if err != nil {
		g.deleteBuffer(buffer)
		return g.transport(err)
	}
	if code != 0 {
		// tmux cannot resolve the pane id: the agent pane is gone.
		g.deleteBuffer(buffer)
		return g.miss(FenceFoundAbsent)
	}
	switch trimmed := strings.TrimSpace(string(out)); {
	case trimmed == "":
		g.applied = true
		return nil
	case strings.HasPrefix(trimmed, TmuxFenceMissPrefix):
		r, ok := ParseTmuxFenceRead(strings.TrimPrefix(trimmed, TmuxFenceMissPrefix))
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
		return fmt.Errorf("fenced input to %q: unexpected tmux output %q", g.name, trimmed)
	}
}

// paste loads text into a new buffer and pastes it, guarded.
func (g *carrierGuard) paste(ctx context.Context, text string) error {
	buffer := carrierBufferName()
	_, code, err := g.c.conn.Exec(ctx, g.name, []string{"sh", "-c", carrierLoadBufferScript, "sh", text, buffer})
	switch {
	case err != nil:
		return g.transport(err)
	case code != 0:
		return fmt.Errorf("fenced input to %q: loading a tmux paste buffer exited %d", g.name, code)
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
