package acp

import (
	"context"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// seamBackedProvider serves the legacy [runtime.Provider] through the
// de-conflated seams (via [runtime.NewProviderFromSeams]), passing the optional
// interfaces production callers type-assert — InteractionProvider (pending /
// respond), TransportCapabilityProvider (SupportsTransport), SleepCapability,
// IdleWaitProvider (WaitForIdle), IdleSnapshotProvider (SnapshotIdle),
// SessionEventProvider (SubscribeSessionEvents), and TurnEventProvider
// (SubscribeTurnEvents) —
// through to the underlying *Provider. The early cut-over for the acp provider.
type seamBackedProvider struct {
	runtime.Provider
	raw *Provider
}

var (
	_ runtime.Provider                    = (*seamBackedProvider)(nil)
	_ runtime.InteractionProvider         = (*seamBackedProvider)(nil)
	_ runtime.TransportCapabilityProvider = (*seamBackedProvider)(nil)
	_ runtime.SleepCapabilityProvider     = (*seamBackedProvider)(nil)
	_ runtime.IdleWaitProvider            = (*seamBackedProvider)(nil)
	_ runtime.IdleSnapshotProvider        = (*seamBackedProvider)(nil)
	_ runtime.SessionEventProvider        = (*seamBackedProvider)(nil)
	_ runtime.TurnEventProvider           = (*seamBackedProvider)(nil)
)

// NewSeamBacked constructs an acp provider served through the seams.
func NewSeamBacked(cfg Config) runtime.Provider { return seamBack(NewProvider(cfg)) }

// NewSeamBackedWithDir is NewProviderWithDir served through the seams.
func NewSeamBackedWithDir(dir string, cfg Config) runtime.Provider {
	return seamBack(NewProviderWithDir(dir, cfg))
}

func seamBack(raw *Provider) *seamBackedProvider {
	rt, tp := raw.Seams()
	return &seamBackedProvider{Provider: runtime.NewProviderFromSeams(rt, tp), raw: raw}
}

// Pending implements [runtime.InteractionProvider] (non-seam passthrough).
func (s *seamBackedProvider) Pending(name string) (*runtime.PendingInteraction, error) {
	return s.raw.Pending(name)
}

// Respond implements [runtime.InteractionProvider] (non-seam passthrough).
func (s *seamBackedProvider) Respond(name string, response runtime.InteractionResponse) error {
	return s.raw.Respond(name, response)
}

// SupportsTransport implements [runtime.TransportCapabilityProvider] (non-seam).
func (s *seamBackedProvider) SupportsTransport(transport string) bool {
	return s.raw.SupportsTransport(transport)
}

// SleepCapability passes through to the underlying provider (non-seam).
func (s *seamBackedProvider) SleepCapability(name string) runtime.SessionSleepCapability {
	return s.raw.SleepCapability(name)
}

// WaitForIdle implements [runtime.IdleWaitProvider] (non-seam passthrough).
func (s *seamBackedProvider) WaitForIdle(ctx context.Context, name string, timeout time.Duration) error {
	return s.raw.WaitForIdle(ctx, name, timeout)
}

// SnapshotIdle implements [runtime.IdleSnapshotProvider] (non-seam passthrough).
func (s *seamBackedProvider) SnapshotIdle(name string) (bool, error) {
	return s.raw.SnapshotIdle(name)
}

// SubscribeSessionEvents implements [runtime.SessionEventProvider] (non-seam
// passthrough).
func (s *seamBackedProvider) SubscribeSessionEvents(ctx context.Context) (<-chan runtime.SessionEvent, error) {
	return s.raw.SubscribeSessionEvents(ctx)
}

// SubscribeTurnEvents implements [runtime.TurnEventProvider] (non-seam
// passthrough).
func (s *seamBackedProvider) SubscribeTurnEvents(ctx context.Context) (<-chan runtime.TurnEvent, error) {
	return s.raw.SubscribeTurnEvents(ctx)
}
