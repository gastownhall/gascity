package ssh

import (
	"context"

	"github.com/gastownhall/gascity/internal/runtime"
)

// seamBackedProvider serves the legacy [runtime.Provider] through the
// de-conflated seams (via [runtime.NewProviderFromSeams]), passing SleepCapability,
// Relaunch, Capabilities, and the error-bearing probes (attachment, liveness,
// Peek) through to the underlying *Provider. The early cut-over for the ssh
// provider.
//
// ExecProvider is not passed through: ssh's Exec (over the connection) is what
// the carrier drives over internally; no production caller type-asserts it.
type seamBackedProvider struct {
	runtime.Provider
	raw *Provider
}

var (
	_ runtime.Provider                    = (*seamBackedProvider)(nil)
	_ runtime.SleepCapabilityProvider     = (*seamBackedProvider)(nil)
	_ runtime.RelaunchProvider            = (*seamBackedProvider)(nil)
	_ runtime.AttachmentObserverWithError = (*seamBackedProvider)(nil)
	_ runtime.LivenessObserverWithError   = (*seamBackedProvider)(nil)
)

// NewSeamBacked constructs an ssh provider for ep served through the seams.
func NewSeamBacked(ep Endpoint) runtime.Provider {
	return newSeamBacked(NewProvider(ep))
}

func newSeamBacked(raw *Provider) runtime.Provider {
	rt, tp := raw.Seams()
	return &seamBackedProvider{Provider: runtime.NewProviderFromSeams(rt, tp), raw: raw}
}

// SleepCapability passes through to the underlying provider (non-seam).
func (s *seamBackedProvider) SleepCapability(name string) runtime.SessionSleepCapability {
	return s.raw.SleepCapability(name)
}

// Relaunch passes through to the underlying provider's warm-box relaunch
// (respawn-pane over the ssh Conn; B2, RelaunchProvider).
func (s *seamBackedProvider) Relaunch(ctx context.Context, name string, cfg runtime.Config) error {
	return s.raw.Relaunch(ctx, name, cfg)
}

// Capabilities passes through to the underlying provider: the seams carry
// only activity and attachment reporting, and the destructive-action fence
// reads CanAttachTTY to keep ssh, whose sessions a human can attach to
// without the box reporting it, on its quiet window.
func (s *seamBackedProvider) Capabilities() runtime.ProviderCapabilities {
	return s.raw.Capabilities()
}

// IsAttachedWithError passes through to the underlying provider's single
// is-attached probe (non-seam). Without it, runtime.IsAttachedWithError would
// fall back to the seam's IsAttached, which re-resolves the box through bool
// IsRunning and drops a transport failure.
func (s *seamBackedProvider) IsAttachedWithError(name string) (bool, error) {
	return s.raw.IsAttachedWithError(name)
}

// ObserveLivenessWithError passes through to the underlying provider so an
// unreachable box reads as runtime.ErrRuntimeUnavailable, not as absent.
func (s *seamBackedProvider) ObserveLivenessWithError(name string, processNames []string) (runtime.Liveness, error) {
	return s.raw.ObserveLivenessWithError(name, processNames)
}

// Peek passes through to the underlying provider: the seam resolves the box
// through bool IsRunning first, which would turn a transport failure into an
// empty pane.
func (s *seamBackedProvider) Peek(name string, lines int) (string, error) {
	return s.raw.Peek(name, lines)
}
