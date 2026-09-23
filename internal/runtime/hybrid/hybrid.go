// Package hybrid provides a composite [runtime.Provider] that routes
// operations to a local or remote backend based on session name.
package hybrid

import (
	"context"
	"fmt"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// Provider routes session operations to a local or remote provider
// based on a name-matching function.
type Provider struct {
	local    runtime.Provider
	remote   runtime.Provider
	isRemote func(name string) bool
}

var (
	_ runtime.Provider                      = (*Provider)(nil)
	_ runtime.DeadRuntimeSessionChecker     = (*Provider)(nil)
	_ runtime.InteractionProvider           = (*Provider)(nil)
	_ runtime.IdleSnapshotProvider          = (*Provider)(nil)
	_ runtime.InterruptBoundaryWaitProvider = (*Provider)(nil)
	_ runtime.InterruptedTurnResetProvider  = (*Provider)(nil)
	_ runtime.RelaunchProvider              = (*Provider)(nil)
	_ runtime.LivenessObserver              = (*Provider)(nil)
	_ runtime.LivenessObserverWithError     = (*Provider)(nil)
	_ runtime.SessionEventProvider          = (*Provider)(nil)
)

// New creates a hybrid provider. isRemote returns true for sessions
// that should be managed by the remote provider.
func New(local, remote runtime.Provider, isRemote func(string) bool) *Provider {
	return &Provider{local: local, remote: remote, isRemote: isRemote}
}

func (p *Provider) route(name string) runtime.Provider {
	if p.isRemote(name) {
		return p.remote
	}
	return p.local
}

// Start delegates to the routed backend.
func (p *Provider) Start(ctx context.Context, name string, cfg runtime.Config) error {
	return p.route(name).Start(ctx, name, cfg)
}

// Stop delegates to the routed backend.
func (p *Provider) Stop(name string) error {
	return p.route(name).Stop(name)
}

// Interrupt delegates to the routed backend.
func (p *Provider) Interrupt(name string) error {
	return p.route(name).Interrupt(name)
}

// IsRunning delegates to the routed backend.
func (p *Provider) IsRunning(name string) bool {
	return p.route(name).IsRunning(name)
}

// IsDeadRuntimeSession delegates to the routed backend when it can positively
// distinguish live sessions from visible dead artifacts.
func (p *Provider) IsDeadRuntimeSession(name string) (bool, error) {
	checker, ok := p.route(name).(runtime.DeadRuntimeSessionChecker)
	if !ok {
		return false, nil
	}
	return checker.IsDeadRuntimeSession(name)
}

// IsAttached delegates to the routed backend.
func (p *Provider) IsAttached(name string) bool {
	return p.route(name).IsAttached(name)
}

// Attach delegates to the routed backend.
func (p *Provider) Attach(name string) error {
	return p.route(name).Attach(name)
}

// ProcessAlive delegates to the routed backend.
func (p *Provider) ProcessAlive(name string, processNames []string) bool {
	return p.route(name).ProcessAlive(name, processNames)
}

// ObserveLiveness delegates to the routed backend through runtime.ObserveLiveness
// so the backend's native LivenessObserver fast-path is preserved (e.g. herdr's
// agent-status liveness) instead of collapsing to the generic
// IsRunning+ProcessAlive fold.
func (p *Provider) ObserveLiveness(name string, processNames []string) runtime.Liveness {
	return runtime.ObserveLiveness(p.route(name), name, processNames)
}

// ObserveLivenessWithError forwards the optional error-bearing observation to
// the selected backend without collapsing uncertainty into false.
func (p *Provider) ObserveLivenessWithError(name string, processNames []string) (runtime.Liveness, error) {
	return runtime.ObserveLivenessWithError(p.route(name), name, processNames)
}

// Nudge delegates to the routed backend.
func (p *Provider) Nudge(name string, content []runtime.ContentBlock) error {
	return p.route(name).Nudge(name, content)
}

// WaitForIdle delegates to the routed backend when it supports explicit
// idle-boundary waiting.
func (p *Provider) WaitForIdle(ctx context.Context, name string, timeout time.Duration) error {
	if wp, ok := p.route(name).(runtime.IdleWaitProvider); ok {
		return wp.WaitForIdle(ctx, name, timeout)
	}
	return runtime.ErrInteractionUnsupported
}

// SnapshotIdle delegates to the routed backend when it can take a
// point-in-time idle observation. Like WaitForIdle this must be forwarded
// explicitly: this Provider enumerates the optional interfaces it supports
// rather than embedding a backend, so a local tmux session would otherwise
// lose SnapshotIdle for every session in a local/remote split city.
func (p *Provider) SnapshotIdle(name string) (bool, error) {
	if sp, ok := p.route(name).(runtime.IdleSnapshotProvider); ok {
		return sp.SnapshotIdle(name)
	}
	return false, runtime.ErrInteractionUnsupported
}

// NudgeNow delegates to the routed backend when it supports immediate
// injection without an internal wait-idle step.
func (p *Provider) NudgeNow(name string, content []runtime.ContentBlock) error {
	if np, ok := p.route(name).(runtime.ImmediateNudgeProvider); ok {
		return np.NudgeNow(name, content)
	}
	return p.route(name).Nudge(name, content)
}

// ResetInterruptedTurn delegates to the routed backend when it supports
// provider-native interrupted-turn discard semantics.
func (p *Provider) ResetInterruptedTurn(ctx context.Context, name string) error {
	if rp, ok := p.route(name).(runtime.InterruptedTurnResetProvider); ok {
		return rp.ResetInterruptedTurn(ctx, name)
	}
	return runtime.ErrInteractionUnsupported
}

// Relaunch forwards a warm-box agent relaunch to the routed backend when it
// supports one, so the reconciler's RelaunchProvider type-assert is not masked
// by the hybrid router.
func (p *Provider) Relaunch(ctx context.Context, name string, cfg runtime.Config) error {
	if rp, ok := p.route(name).(runtime.RelaunchProvider); ok {
		return rp.Relaunch(ctx, name, cfg)
	}
	return runtime.ErrRelaunchUnsupported
}

// WaitForInterruptBoundary delegates to the routed backend when it can confirm
// a provider-native interrupt boundary before the next turn is injected.
func (p *Provider) WaitForInterruptBoundary(ctx context.Context, name string, since time.Time, timeout time.Duration) error {
	if wp, ok := p.route(name).(runtime.InterruptBoundaryWaitProvider); ok {
		return wp.WaitForInterruptBoundary(ctx, name, since, timeout)
	}
	return runtime.ErrInteractionUnsupported
}

// Pending delegates to the routed backend when it supports structured
// interactions.
func (p *Provider) Pending(name string) (*runtime.PendingInteraction, error) {
	if ip, ok := p.route(name).(runtime.InteractionProvider); ok {
		return ip.Pending(name)
	}
	return nil, runtime.ErrInteractionUnsupported
}

// Respond delegates to the routed backend when it supports structured
// interactions.
func (p *Provider) Respond(name string, response runtime.InteractionResponse) error {
	if ip, ok := p.route(name).(runtime.InteractionProvider); ok {
		return ip.Respond(name, response)
	}
	return runtime.ErrInteractionUnsupported
}

// SetMeta delegates to the routed backend.
func (p *Provider) SetMeta(name, key, value string) error {
	return p.route(name).SetMeta(name, key, value)
}

// GetMeta delegates to the routed backend.
func (p *Provider) GetMeta(name, key string) (string, error) {
	return p.route(name).GetMeta(name, key)
}

// RemoveMeta delegates to the routed backend.
func (p *Provider) RemoveMeta(name, key string) error {
	return p.route(name).RemoveMeta(name, key)
}

// Peek delegates to the routed backend.
func (p *Provider) Peek(name string, lines int) (string, error) {
	return p.route(name).Peek(name, lines)
}

// ListRunning queries both backends and returns best-effort results plus a
// partial-list error when one backend fails.
func (p *Provider) ListRunning(prefix string) ([]string, error) {
	local, lErr := p.local.ListRunning(prefix)
	remote, rErr := p.remote.ListRunning(prefix)
	return runtime.MergeBackendListResults(
		runtime.BackendListResult{Label: "local", Names: local, Err: lErr},
		runtime.BackendListResult{Label: "remote", Names: remote, Err: rErr},
	)
}

// GetLastActivity delegates to the routed backend.
func (p *Provider) GetLastActivity(name string) (time.Time, error) {
	return p.route(name).GetLastActivity(name)
}

// ClearScrollback delegates to the routed backend.
func (p *Provider) ClearScrollback(name string) error {
	return p.route(name).ClearScrollback(name)
}

// CopyTo delegates to the routed backend.
func (p *Provider) CopyTo(name, src, relDst string) error {
	return p.route(name).CopyTo(name, src, relDst)
}

// SendKeys delegates to the routed backend.
func (p *Provider) SendKeys(name string, keys ...string) error {
	return p.route(name).SendKeys(name, keys...)
}

// RunLive delegates to the routed backend.
func (p *Provider) RunLive(name string, cfg runtime.Config) error {
	return p.route(name).RunLive(name, cfg)
}

// Capabilities returns the intersection of both backends' capabilities.
// A capability is reported only if both local and remote support it.
// NeedsClaimBackstop is a need, not an ability, so it unions instead: if
// either backend requires the stalled-claim backstop, the composite does too.
func (p *Provider) Capabilities() runtime.ProviderCapabilities {
	lc := p.local.Capabilities()
	rc := p.remote.Capabilities()
	return runtime.ProviderCapabilities{
		CanReportAttachment: lc.CanReportAttachment && rc.CanReportAttachment,
		CanReportActivity:   lc.CanReportActivity && rc.CanReportActivity,
		CanStream:           lc.CanStream && rc.CanStream,
		CanAttachTTY:        lc.CanAttachTTY && rc.CanAttachTTY,
		NeedsClaimBackstop:  lc.NeedsClaimBackstop || rc.NeedsClaimBackstop,
	}
}

// SleepCapability reports idle sleep capability for the routed backend.
func (p *Provider) SleepCapability(name string) runtime.SessionSleepCapability {
	if scp, ok := p.route(name).(runtime.SleepCapabilityProvider); ok {
		return scp.SleepCapability(name)
	}
	return runtime.SessionSleepCapabilityDisabled
}

// SubscribeSessionEvents merges the session-event streams of every backend
// that implements runtime.SessionEventProvider. EventCapableRoute reports
// event-capability per session, backend by backend, so a caller may rely on
// events arriving for a session routed to EITHER backend. Forwarding only
// one backend's stream (the pre-fix behavior) would silently drop events for
// sessions served by the other backend whenever both are event-capable,
// while still telling that caller (via EventCapableRoute) that those
// sessions were covered. Wrapping an event-capable local backend (e.g.
// herdr) behind hybrid without this method would fail the
// runtime.SessionEventProvider type assertion in cmd/gc's
// sessionEventPump.restart and silently drop the whole event-driven
// reconcile poke, falling back to patrol polling with no underlying
// capability loss to explain it.
func (p *Provider) SubscribeSessionEvents(ctx context.Context) (<-chan runtime.SessionEvent, error) {
	lSEP, lok := p.local.(runtime.SessionEventProvider)
	rSEP, rok := p.remote.(runtime.SessionEventProvider)
	if !lok && !rok {
		return nil, fmt.Errorf("neither local nor remote backend implements SubscribeSessionEvents")
	}
	if lok && !rok {
		return lSEP.SubscribeSessionEvents(ctx)
	}
	if rok && !lok {
		return rSEP.SubscribeSessionEvents(ctx)
	}

	lCh, err := lSEP.SubscribeSessionEvents(ctx)
	if err != nil {
		return nil, fmt.Errorf("local backend: %w", err)
	}
	rCh, err := rSEP.SubscribeSessionEvents(ctx)
	if err != nil {
		return nil, fmt.Errorf("remote backend: %w", err)
	}
	return mergeSessionEvents(ctx, lCh, rCh), nil
}

// mergeSessionEvents fans two session-event streams into one, closing the
// output only when ctx is done or both inputs close. It never closes the
// output merely because one side has gone quiet: a consumer computing
// liveness off the single merged stream would otherwise see the WHOLE
// composite die whenever either backend went idle for
// runtime.SessionEventStaleAfter, even though the other backend kept
// delivering — a non-self-healing outage.
func mergeSessionEvents(ctx context.Context, a, b <-chan runtime.SessionEvent) <-chan runtime.SessionEvent {
	out := make(chan runtime.SessionEvent)
	go func() {
		defer close(out)
		for a != nil || b != nil {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-a:
				if !ok {
					a = nil
					continue
				}
				select {
				case out <- ev:
				case <-ctx.Done():
					return
				}
			case ev, ok := <-b:
				if !ok {
					b = nil
					continue
				}
				select {
				case out <- ev:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out
}

// EventCapableRoute reports whether the backend routed for name implements
// runtime.SessionEventProvider. SubscribeSessionEvents above merges both
// backends' streams whenever both are event-capable, so a session routed to
// either backend gets real events whenever this reports true.
func (p *Provider) EventCapableRoute(name string) bool {
	_, ok := p.route(name).(runtime.SessionEventProvider)
	return ok
}
