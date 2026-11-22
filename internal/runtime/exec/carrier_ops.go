package exec

import (
	"context"
	"fmt"

	"github.com/gastownhall/gascity/internal/runtime"
)

// opsCarrier drives the session through the existing pack ops. They carry no
// incarnation fence, so every fenced entry point refuses before any op runs.
type opsCarrier struct{ p *Provider }

var _ runtime.Carrier = opsCarrier{}

func (c opsCarrier) Nudge(ctx context.Context, name string, content []runtime.ContentBlock) error {
	return c.p.nudgeOp(ctx, name, content)
}

func (c opsCarrier) Peek(ctx context.Context, name string, lines int) (string, error) {
	return c.p.peekOp(ctx, name, lines)
}

func (c opsCarrier) SendKeys(ctx context.Context, name string, keys ...string) error {
	if len(keys) == 0 {
		return nil
	}
	return c.p.sendKeysOp(ctx, name, keys...)
}

func (c opsCarrier) Interrupt(ctx context.Context, name string) error {
	return c.p.interruptOp(ctx, name)
}

func (c opsCarrier) ClearScrollback(ctx context.Context, name string) error {
	return c.p.clearScrollbackOp(ctx, name)
}

func (opsCarrier) NudgeFenced(_ context.Context, name string, _ []runtime.ContentBlock, _ runtime.InputFence) error {
	return unsupportedOpsFence(name)
}

func (opsCarrier) SendKeysFenced(_ context.Context, name string, _ runtime.InputFence, _ ...string) error {
	return unsupportedOpsFence(name)
}

func (opsCarrier) InterruptFenced(_ context.Context, name string, _ runtime.InputFence) error {
	return unsupportedOpsFence(name)
}

func (opsCarrier) FencedKeys(_ context.Context, name string, _ runtime.InputFence, _ func(func(...string) error) error) error {
	return unsupportedOpsFence(name)
}

func (opsCarrier) PublishInputFence(_ context.Context, name string, _ runtime.InputFence) error {
	return unsupportedOpsFence(name)
}

func unsupportedOpsFence(name string) error {
	return fmt.Errorf("session %q: %w", name, runtime.ErrInputFenceUnsupported)
}
