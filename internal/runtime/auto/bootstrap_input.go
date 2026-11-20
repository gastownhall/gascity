package auto

import (
	"context"

	"github.com/gastownhall/gascity/internal/runtime"
)

var _ runtime.BootstrapInputObserver = (*Provider)(nil)

// ObserveBootstrapInput consults only the backend selected for this session.
func (p *Provider) ObserveBootstrapInput(ctx context.Context, name string, req runtime.BootstrapInputRequest) (*runtime.BootstrapInputProof, error) {
	return runtime.ObserveBootstrapInput(ctx, p, name, req)
}
