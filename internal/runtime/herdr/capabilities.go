package herdr

import (
	"context"
	"strconv"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// Optional capability interfaces herdr supports natively. (Relaunch,
// ProcessTableScanner, InterruptBoundaryWait, and DialogProvider are
// deliberately omitted from the first cut — the reconciler degrades gracefully
// when a provider lacks them: Relaunch falls back to Stop+Start, the others to
// no-op/default behavior.)
var (
	_ runtime.IdleWaitProvider       = (*Provider)(nil)
	_ runtime.ImmediateNudgeProvider = (*Provider)(nil)
)

// WaitForIdle blocks until herdr reports the agent idle or the timeout elapses,
// via herdr's native `agent wait --status idle` — vs the pane-polling tmux does.
// Either outcome (idle reached or timed out) means the caller may proceed, so
// only context cancellation surfaces as an error; the timeout is a hard bound.
func (p *Provider) WaitForIdle(ctx context.Context, name string, timeout time.Duration) error {
	ms := int(timeout / time.Millisecond)
	if ms < 1 {
		ms = 1
	}
	_, _ = p.c.run(ctx, "agent", "wait", name, "--status", "idle", "--timeout", strconv.Itoa(ms))
	return ctx.Err()
}

// NudgeNow injects input immediately. herdr's send/run already deliver without a
// wait-idle heuristic, so this is the same delivery path as Nudge.
func (p *Provider) NudgeNow(name string, content []runtime.ContentBlock) error {
	return p.Nudge(name, content)
}
