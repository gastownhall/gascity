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
// SessionEventProvider (SubscribeSessionEvents), TurnEventProvider
// (SubscribeTurnEvents), TurnFenceProvider and TurnObservationProvider (turn
// reads and the turn-scoped interrupt), ProcessTableScanner (orphan reaping),
// LivenessObserverWithError and ListingAttestation — through to the
// underlying *Provider. The early cut-over for the acp provider.
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
	_ runtime.ProcessTableScanner         = (*seamBackedProvider)(nil)
	_ runtime.LivenessObserverWithError   = (*seamBackedProvider)(nil)
	_ runtime.ListingAttestation          = (*seamBackedProvider)(nil)
	_ runtime.IdentitySidecarProvider     = (*seamBackedProvider)(nil)
	_ runtime.IdentitySidecarProvider     = (*Provider)(nil)
	_ runtime.TurnEventProvider           = (*seamBackedProvider)(nil)
	_ runtime.TurnFenceProvider           = (*seamBackedProvider)(nil)
	_ runtime.TurnObservationProvider     = (*seamBackedProvider)(nil)
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

// ObserveTurn implements [runtime.TurnFenceProvider] (non-seam passthrough).
func (s *seamBackedProvider) ObserveTurn(ctx context.Context, name string) (runtime.TurnState, error) {
	return s.raw.ObserveTurn(ctx, name)
}

// InterruptTurn implements [runtime.TurnFenceProvider] (non-seam passthrough).
func (s *seamBackedProvider) InterruptTurn(ctx context.Context, name, turnID string) (runtime.TurnInterrupt, error) {
	return s.raw.InterruptTurn(ctx, name, turnID)
}

// WaitTurnEnded implements [runtime.TurnFenceProvider] (non-seam passthrough).
func (s *seamBackedProvider) WaitTurnEnded(ctx context.Context, name, turnID string, timeout time.Duration) (runtime.TurnState, error) {
	return s.raw.WaitTurnEnded(ctx, name, turnID, timeout)
}

// TurnCapabilities implements [runtime.TurnObservationProvider] (non-seam
// passthrough).
func (s *seamBackedProvider) TurnCapabilities(name string) runtime.TurnCapabilities {
	return s.raw.TurnCapabilities(name)
}

// TurnHistory implements [runtime.TurnObservationProvider] (non-seam
// passthrough).
func (s *seamBackedProvider) TurnHistory(ctx context.Context, name string) (runtime.TurnHistory, error) {
	return s.raw.TurnHistory(ctx, name)
}

// FindRuntimesBySessionID implements [runtime.ProcessTableScanner] (non-seam
// passthrough).
func (s *seamBackedProvider) FindRuntimesBySessionID(id string) ([]runtime.LiveRuntime, error) {
	return s.raw.FindRuntimesBySessionID(id)
}

// TerminateRuntime implements [runtime.ProcessTableScanner] (non-seam
// passthrough).
func (s *seamBackedProvider) TerminateRuntime(r runtime.LiveRuntime) error {
	return s.raw.TerminateRuntime(r)
}

// ObserveLivenessWithError passes the underlying provider's three-outcome
// liveness through (non-seam); the seams expose only a bool liveness read.
func (s *seamBackedProvider) ObserveLivenessWithError(name string, processNames []string) (runtime.Liveness, error) {
	return s.raw.ObserveLivenessWithError(name, processNames)
}

// LocalIdentitySidecar passes the underlying provider's sidecar declaration
// through; the seams route GetMeta to the same sidecar.
func (s *seamBackedProvider) LocalIdentitySidecar() bool { return s.raw.LocalIdentitySidecar() }

// LivenessReadsFresh forwards [runtime.FreshByConstruction].
func (s *seamBackedProvider) LivenessReadsFresh() bool { return s.raw.LivenessReadsFresh() }

// ListRunningComplete passes the underlying provider's listing attestation
// through; the seams route ListRunning to the same raw listing.
func (s *seamBackedProvider) ListRunningComplete() bool {
	return s.raw.ListRunningComplete()
}
