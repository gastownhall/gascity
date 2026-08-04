package t3bridge

import (
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// seamBackedProvider serves the legacy [runtime.Provider] through the
// de-conflated seams (via [runtime.NewProviderFromSeams]), passing SleepCapability
// through to the underlying *Provider. The early cut-over for the t3bridge
// provider (whose Transport is the bespoke "t3" turn protocol, not the carrier).
type seamBackedProvider struct {
	runtime.Provider
	raw *Provider
}

var (
	_ runtime.Provider                  = (*seamBackedProvider)(nil)
	_ runtime.SleepCapabilityProvider   = (*seamBackedProvider)(nil)
	_ runtime.LivenessObserverWithError = (*seamBackedProvider)(nil)
)

// NewSeamBacked constructs a t3bridge provider served through the seams.
func NewSeamBacked() runtime.Provider {
	raw := NewProvider()
	rt, tp := raw.Seams()
	return &seamBackedProvider{Provider: runtime.NewProviderFromSeams(rt, tp), raw: raw}
}

// SleepCapability passes through to the underlying provider (non-seam).
func (s *seamBackedProvider) SleepCapability(name string) runtime.SessionSleepCapability {
	return s.raw.SleepCapability(name)
}

// ObserveLivenessWithError passes the raw bridge's snapshot-aware observation
// through the production seam-backed provider. The embedded legacy provider's
// IsRunning signature remains unchanged.
func (s *seamBackedProvider) ObserveLivenessWithError(name string, processNames []string) (runtime.Liveness, error) {
	return s.raw.ObserveLivenessWithError(name, processNames)
}

// GetLastActivity preserves the raw bridge's error-bearing snapshot boundary.
// The generic seam adapter first opens a live Place, whose legacy bool liveness
// surface cannot distinguish a bridge outage from absence.
func (s *seamBackedProvider) GetLastActivity(name string) (time.Time, error) {
	return s.raw.GetLastActivity(name)
}
