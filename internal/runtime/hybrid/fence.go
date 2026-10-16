package hybrid

import (
	"context"

	"github.com/gastownhall/gascity/internal/runtime"
)

var _ runtime.FencedInputProvider = (*Provider)(nil)

// InputFencing reports how the backend name routes to fences its input.
func (p *Provider) InputFencing(name string) runtime.InputFencing {
	return runtime.InputFencingFor(p.route(name), name)
}

// NudgeFenced forwards to the routed backend, or refuses with
// runtime.ErrInputFenceUnsupported when it cannot fence name. It never falls
// through to the other backend.
func (p *Provider) NudgeFenced(ctx context.Context, name string, content []runtime.ContentBlock, n runtime.FencedNudge) error {
	fp, err := runtime.FencingBackend(p.route(name), name)
	if err != nil {
		return err
	}
	return fp.NudgeFenced(ctx, name, content, n)
}

// SendKeysFenced forwards as NudgeFenced does.
func (p *Provider) SendKeysFenced(ctx context.Context, name string, fence runtime.InputFence, keys ...string) error {
	fp, err := runtime.FencingBackend(p.route(name), name)
	if err != nil {
		return err
	}
	return fp.SendKeysFenced(ctx, name, fence, keys...)
}

// InterruptFenced forwards as NudgeFenced does.
func (p *Provider) InterruptFenced(ctx context.Context, name string, fence runtime.InputFence) error {
	fp, err := runtime.FencingBackend(p.route(name), name)
	if err != nil {
		return err
	}
	return fp.InterruptFenced(ctx, name, fence)
}
