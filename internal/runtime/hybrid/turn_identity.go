package hybrid

import (
	"context"

	"github.com/gastownhall/gascity/internal/runtime"
)

var _ runtime.TurnObservationProvider = (*Provider)(nil)

// TurnCapabilities reports the routed backend's capabilities for name, and
// nothing on every axis when that backend cannot observe turns.
func (p *Provider) TurnCapabilities(name string) runtime.TurnCapabilities {
	return runtime.TurnCapabilitiesFor(p.route(name), name)
}

// TurnHistory forwards to the routed backend, or answers
// runtime.ErrInteractionUnsupported when it cannot observe turns. It never
// falls through to the other backend.
func (p *Provider) TurnHistory(ctx context.Context, name string) (runtime.TurnHistory, error) {
	if tp, ok := p.route(name).(runtime.TurnObservationProvider); ok {
		return tp.TurnHistory(ctx, name)
	}
	return runtime.TurnHistory{}, runtime.ErrInteractionUnsupported
}
