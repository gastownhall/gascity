// Package hybrid provides a composite [runtime.Provider] that routes
// operations to a local or remote backend based on session name.
package hybrid

import (
	"context"
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
	_ runtime.InteractionProvider           = (*Provider)(nil)
	_ runtime.InterruptBoundaryWaitProvider = (*Provider)(nil)
	_ runtime.InterruptedTurnResetProvider  = (*Provider)(nil)
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

// ListRunning queries both backends and merges results. If one backend
// errors, results from the other are still returned (best-effort).
func (p *Provider) ListRunning(prefix string) ([]string, error) {
	local, lErr := p.local.ListRunning(prefix)
	remote, rErr := p.remote.ListRunning(prefix)
	if lErr != nil && rErr != nil {
		return nil, lErr
	}
	return append(local, remote...), nil
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
func (p *Provider) Capabilities() runtime.ProviderCapabilities {
	lc := p.local.Capabilities()
	rc := p.remote.Capabilities()
	return runtime.ProviderCapabilities{
		CanReportAttachment: lc.CanReportAttachment && rc.CanReportAttachment,
		CanReportActivity:   lc.CanReportActivity && rc.CanReportActivity,
	}
}

// SleepCapability reports idle sleep capability for the routed backend.
func (p *Provider) SleepCapability(name string) runtime.SessionSleepCapability {
	if scp, ok := p.route(name).(runtime.SleepCapabilityProvider); ok {
		return scp.SleepCapability(name)
	}
	return runtime.SessionSleepCapabilityDisabled
}
