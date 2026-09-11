package exec

import (
	"context"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// seamBackedProvider serves the legacy [runtime.Provider] through the
// de-conflated seams (via [runtime.NewProviderFromSeams]), passing the optional
// interfaces production callers type-assert — DialogProvider (dialog dismissal),
// SleepCapability, and the image-checker (CheckImage) — through to the
// underlying *Provider. The early cut-over for the exec provider.
//
// ExecProvider is deliberately NOT passed through: exec's Exec op is the
// connection the carrier drives over INTERNALLY (no production caller
// type-asserts it), so seam-backed driving reaches it through the provider's own
// carrier-backed Nudge/Peek/SendKeys/Interrupt/ClearScrollback.
type seamBackedProvider struct {
	runtime.Provider
	raw *Provider
}

var (
	_ runtime.Provider                = (*seamBackedProvider)(nil)
	_ runtime.DialogProvider          = (*seamBackedProvider)(nil)
	_ runtime.SleepCapabilityProvider = (*seamBackedProvider)(nil)
	_ runtime.RelaunchProvider        = (*seamBackedProvider)(nil)
)

// NewSeamBacked wraps an exec provider for the given script so it is served
// through the seams.
func NewSeamBacked(script string) runtime.Provider {
	raw := NewProvider(script)
	rt, tp := raw.Seams()
	return &seamBackedProvider{Provider: runtime.NewProviderFromSeams(rt, tp), raw: raw}
}

// DismissKnownDialogs implements [runtime.DialogProvider] (non-seam passthrough).
func (s *seamBackedProvider) DismissKnownDialogs(ctx context.Context, name string, timeout time.Duration) error {
	return s.raw.DismissKnownDialogs(ctx, name, timeout)
}

// SleepCapability passes through to the underlying provider (non-seam).
func (s *seamBackedProvider) SleepCapability(name string) runtime.SessionSleepCapability {
	return s.raw.SleepCapability(name)
}

// CheckImage passes through (non-seam; cmd/gc start asserts an image-checker).
func (s *seamBackedProvider) CheckImage(image string) error {
	return s.raw.CheckImage(image)
}

// Relaunch passes through to the underlying provider's warm-box relaunch (B2,
// RelaunchProvider): for a separable pack it respawns the agent over the exec op;
// for a welded pack it degrades to a reprovision.
func (s *seamBackedProvider) Relaunch(ctx context.Context, name string, cfg runtime.Config) error {
	return s.raw.Relaunch(ctx, name, cfg)
}

// Capabilities preserves the raw provider's complete handshake-derived
// capability set through the production seam-backed composition.
func (s *seamBackedProvider) Capabilities() runtime.ProviderCapabilities {
	return s.raw.Capabilities()
}
