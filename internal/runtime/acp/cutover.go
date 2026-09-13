package acp

import "github.com/gastownhall/gascity/internal/runtime"

// seamBackedProvider serves the legacy [runtime.Provider] through the
// de-conflated seams (via [runtime.NewProviderFromSeams]), passing the optional
// interfaces production callers type-assert — InteractionProvider (pending /
// respond), TransportCapabilityProvider (SupportsTransport), and SleepCapability
// — through to the underlying *Provider. The early cut-over for the acp provider.
type seamBackedProvider struct {
	runtime.Provider
	raw *Provider
}

var (
	_ runtime.Provider                    = (*seamBackedProvider)(nil)
	_ runtime.InteractionProvider         = (*seamBackedProvider)(nil)
	_ runtime.TransportCapabilityProvider = (*seamBackedProvider)(nil)
	_ runtime.SleepCapabilityProvider     = (*seamBackedProvider)(nil)
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
