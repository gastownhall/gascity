package t3bridge

import (
	"context"
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
	_ runtime.Provider                        = (*seamBackedProvider)(nil)
	_ runtime.SleepCapabilityProvider         = (*seamBackedProvider)(nil)
	_ runtime.LivenessObserverWithError       = (*seamBackedProvider)(nil)
	_ runtime.TurnInterruptCapabilityProvider = (*seamBackedProvider)(nil)
	_ runtime.KeyedNudgeProvider              = (*seamBackedProvider)(nil)
	_ runtime.SessionBindingProvider          = (*seamBackedProvider)(nil)
	_ runtime.TurnFenceProvider               = (*seamBackedProvider)(nil)
	_ runtime.TurnObservationProvider         = (*seamBackedProvider)(nil)
	_ runtime.FencedInputProvider             = (*seamBackedProvider)(nil)
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

// TurnInterruptMode passes through to the underlying provider (non-seam).
func (s *seamBackedProvider) TurnInterruptMode(name string) runtime.TurnInterruptMode {
	return s.raw.TurnInterruptMode(name)
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

// Nudge delivers through the raw bridge rather than the generic seam adapter,
// which answers nil for a session it cannot attach to. The bridge reports
// runtime.ErrRuntimeUnavailable when T3 cannot be read and
// runtime.ErrSessionNotFound when no thread is bound, so the nudge queue
// retries instead of acknowledging a nudge that never arrived.
func (s *seamBackedProvider) Nudge(name string, content []runtime.ContentBlock) error {
	return s.raw.Nudge(name, content)
}

// Interrupt reports the same typed errors as Nudge instead of the seam
// adapter's silent nil for a session it cannot attach to.
func (s *seamBackedProvider) Interrupt(name string) error {
	return s.raw.Interrupt(name)
}

// NudgeKeyed passes the caller's delivery key through to the raw bridge,
// which derives T3's idempotency ids from it.
func (s *seamBackedProvider) NudgeKeyed(name string, content []runtime.ContentBlock, key string) (runtime.DeliveryReceipt, error) {
	return s.raw.NudgeKeyed(name, content, key)
}

// LookupDelivery passes through to the raw bridge.
func (s *seamBackedProvider) LookupDelivery(ctx context.Context, name, key string) (runtime.DeliveryReceipt, error) {
	return s.raw.LookupDelivery(ctx, name, key)
}

// SessionBinding reports the T3 thread and project bound to name.
func (s *seamBackedProvider) SessionBinding(name string) (runtime.SessionBinding, error) {
	return s.raw.SessionBinding(name)
}

// ObserveTurn passes through to the raw bridge.
func (s *seamBackedProvider) ObserveTurn(ctx context.Context, name string) (runtime.TurnState, error) {
	return s.raw.ObserveTurn(ctx, name)
}

// InterruptTurn passes through to the raw bridge.
func (s *seamBackedProvider) InterruptTurn(ctx context.Context, name, turnID string) (runtime.TurnInterrupt, error) {
	return s.raw.InterruptTurn(ctx, name, turnID)
}

// WaitTurnEnded passes through to the raw bridge.
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

// InputFencing passes through to the raw bridge (non-seam).
func (s *seamBackedProvider) InputFencing(name string) runtime.InputFencing {
	return s.raw.InputFencing(name)
}

// NudgeFenced passes through to the raw bridge (non-seam).
func (s *seamBackedProvider) NudgeFenced(ctx context.Context, name string, content []runtime.ContentBlock, n runtime.FencedNudge) error {
	return s.raw.NudgeFenced(ctx, name, content, n)
}

// SendKeysFenced passes through to the raw bridge (non-seam).
func (s *seamBackedProvider) SendKeysFenced(ctx context.Context, name string, fence runtime.InputFence, keys ...string) error {
	return s.raw.SendKeysFenced(ctx, name, fence, keys...)
}

// InterruptFenced passes through to the raw bridge (non-seam).
func (s *seamBackedProvider) InterruptFenced(ctx context.Context, name string, fence runtime.InputFence) error {
	return s.raw.InterruptFenced(ctx, name, fence)
}
