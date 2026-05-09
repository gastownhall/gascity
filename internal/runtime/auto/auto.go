// Package auto provides a composite [runtime.Provider] that routes
// sessions to a default backend (typically tmux) or ACP based on
// per-session registration. Sessions are registered as ACP via
// [Provider.RouteACP] before [Provider.Start] is called. Unregistered
// sessions route to the default backend.
package auto

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// Provider routes session operations to a default or ACP backend
// based on per-session registration.
type Provider struct {
	defaultSP runtime.Provider
	acpSP     runtime.Provider

	mu     sync.RWMutex
	routes map[string]bool // true = ACP
}

var (
	_ runtime.Provider                      = (*Provider)(nil)
	_ runtime.DeadRuntimeSessionChecker     = (*Provider)(nil)
	_ runtime.InteractionProvider           = (*Provider)(nil)
	_ runtime.InterruptBoundaryWaitProvider = (*Provider)(nil)
	_ runtime.InterruptedTurnResetProvider  = (*Provider)(nil)
	_ runtime.TransportCapabilityProvider   = (*Provider)(nil)
)

// New creates a composite provider. defaultSP handles sessions not
// registered as ACP. acpSP handles sessions registered via RouteACP.
func New(defaultSP, acpSP runtime.Provider) *Provider {
	return &Provider{
		defaultSP: defaultSP,
		acpSP:     acpSP,
		routes:    make(map[string]bool),
	}
}

// RouteACP registers a session name to use the ACP backend.
// Must be called before Start for that session.
func (p *Provider) RouteACP(name string) {
	p.mu.Lock()
	p.routes[name] = true
	p.mu.Unlock()
}

// Unroute removes a session's routing entry. Called on Stop to avoid
// leaking entries for destroyed sessions.
func (p *Provider) Unroute(name string) {
	p.mu.Lock()
	delete(p.routes, name)
	p.mu.Unlock()
}

func (p *Provider) route(name string) runtime.Provider {
	p.mu.RLock()
	isACP := p.routes[name]
	p.mu.RUnlock()
	if isACP {
		return p.acpSP
	}
	return p.defaultSP
}

// SupportsTransport reports whether this provider can route the requested
// session transport.
func (p *Provider) SupportsTransport(transport string) bool {
	if transport != "acp" {
		return true
	}
	if provider, ok := p.acpSP.(runtime.TransportCapabilityProvider); ok {
		return provider.SupportsTransport(transport)
	}
	return false
}

// DetectTransport reports the backend currently hosting the named session.
// It returns "acp" for ACP-backed sessions and "" for default or unknown.
func (p *Provider) DetectTransport(name string) string {
	if p.defaultSP.IsRunning(name) {
		return ""
	}
	if p.acpSP.IsRunning(name) {
		return "acp"
	}
	return ""
}

// Start delegates to the routed backend.
func (p *Provider) Start(ctx context.Context, name string, cfg runtime.Config) error {
	return p.route(name).Start(ctx, name, cfg)
}

// Stop delegates to the routed backend and cleans up the route entry
// only on success. If the routed backend fails, tries the other backend
// to handle stale/missing route entries (e.g., after controller restart).
func (p *Provider) Stop(name string) error {
	primary := p.route(name)
	primaryLabel := "default"
	otherLabel := "acp"
	primaryRunning := primary.IsRunning(name)
	p.mu.RLock()
	primaryExplicitRoute := p.routes[name]
	p.mu.RUnlock()
	err := primary.Stop(name)
	if err == nil && primaryRunning {
		p.Unroute(name)
		return nil
	}
	// Fall through to the other backend in case the route is stale.
	var other runtime.Provider
	p.mu.RLock()
	if p.routes[name] {
		primaryLabel = "acp"
		otherLabel = "default"
		other = p.defaultSP
	} else {
		other = p.acpSP
	}
	p.mu.RUnlock()
	otherRunning := other.IsRunning(name)
	if err == nil {
		if primaryExplicitRoute {
			if otherRunning {
				return fmt.Errorf("%s backend: stop succeeded without liveness confirmation while %s backend still reports the session running", primaryLabel, otherLabel)
			}
			p.Unroute(name)
			return nil
		}
		err = fmt.Errorf("%w: %q", runtime.ErrSessionNotFound, name)
	}
	otherErr := other.Stop(name)
	if otherErr == nil {
		if !otherRunning {
			otherErr = fmt.Errorf("%w: %q", runtime.ErrSessionNotFound, name)
		} else if (primaryRunning || primaryExplicitRoute) && !runtime.IsSessionGone(err) {
			return fmt.Errorf("%s backend: %w", primaryLabel, err)
		}
	}
	mergedErr := runtime.MergeBackendStopErrors(
		runtime.BackendError{Label: primaryLabel, Err: err},
		runtime.BackendError{Label: otherLabel, Err: otherErr},
	)
	if mergedErr == nil {
		p.Unroute(name)
		return nil
	}
	return mergedErr
}

// Interrupt delegates to the routed backend.
func (p *Provider) Interrupt(name string) error {
	return p.route(name).Interrupt(name)
}

// IsRunning checks the routed backend first. If it reports not running,
// falls through to the other backend to handle route table inconsistencies.
func (p *Provider) IsRunning(name string) bool {
	if p.route(name).IsRunning(name) {
		return true
	}
	// Fall through: check the other backend in case routing is stale.
	p.mu.RLock()
	isACP := p.routes[name]
	p.mu.RUnlock()
	if isACP {
		return p.defaultSP.IsRunning(name)
	}
	return p.acpSP.IsRunning(name)
}

// IsDeadRuntimeSession checks both backends for a positive dead-artifact
// report because ListRunning is also merged across both backends.
func (p *Provider) IsDeadRuntimeSession(name string) (bool, error) {
	primary := p.route(name)
	if dead, err := providerDeadRuntimeSession(primary, name); dead || err != nil {
		return dead, err
	}
	p.mu.RLock()
	isACP := p.routes[name]
	p.mu.RUnlock()
	if isACP {
		return providerDeadRuntimeSession(p.defaultSP, name)
	}
	return providerDeadRuntimeSession(p.acpSP, name)
}

func providerDeadRuntimeSession(sp runtime.Provider, name string) (bool, error) {
	checker, ok := sp.(runtime.DeadRuntimeSessionChecker)
	if !ok {
		return false, nil
	}
	return checker.IsDeadRuntimeSession(name)
}

// IsAttached delegates to the routed backend.
func (p *Provider) IsAttached(name string) bool {
	return p.route(name).IsAttached(name)
}

// Attach delegates to the routed backend. ACP sessions return an error.
func (p *Provider) Attach(name string) error {
	p.mu.RLock()
	isACP := p.routes[name]
	p.mu.RUnlock()
	if isACP {
		return fmt.Errorf("agent %q uses ACP transport (no terminal to attach to)", name)
	}
	return p.defaultSP.Attach(name)
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

// ListRunning queries both backends and returns best-effort results plus a
// partial-list error when one backend fails.
func (p *Provider) ListRunning(prefix string) ([]string, error) {
	defaultList, dErr := p.defaultSP.ListRunning(prefix)
	acpList, aErr := p.acpSP.ListRunning(prefix)
	return runtime.MergeBackendListResults(
		runtime.BackendListResult{Label: "default", Names: defaultList, Err: dErr},
		runtime.BackendListResult{Label: "acp", Names: acpList, Err: aErr},
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
// A capability is reported only if both default and ACP support it.
func (p *Provider) Capabilities() runtime.ProviderCapabilities {
	dc := p.defaultSP.Capabilities()
	ac := p.acpSP.Capabilities()
	return runtime.ProviderCapabilities{
		CanReportAttachment: dc.CanReportAttachment && ac.CanReportAttachment,
		CanReportActivity:   dc.CanReportActivity && ac.CanReportActivity,
	}
}

// SleepCapability reports idle sleep capability for the routed backend.
func (p *Provider) SleepCapability(name string) runtime.SessionSleepCapability {
	if scp, ok := p.route(name).(runtime.SleepCapabilityProvider); ok {
		return scp.SleepCapability(name)
	}
	return runtime.SessionSleepCapabilityDisabled
}
