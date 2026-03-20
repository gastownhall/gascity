// Package auto provides a composite [runtime.Provider] that routes
// sessions to a default backend or named override backends based on
// per-session registration. Sessions are registered via [Provider.Route]
// before [Provider.Start] is called. Unregistered sessions route to the
// default backend.
package auto

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// Provider routes session operations to a default or named override backend
// based on per-session registration.
type Provider struct {
	defaultSP runtime.Provider
	backends  map[string]runtime.Provider // provider name → provider

	mu     sync.RWMutex
	routes map[string]string // session name → provider name
}

var (
	_ runtime.Provider            = (*Provider)(nil)
	_ runtime.InteractionProvider = (*Provider)(nil)
)

// New creates a composite provider. defaultSP handles sessions not
// registered with an override. backends maps provider names to providers
// for sessions registered via Route.
func New(defaultSP runtime.Provider, backends map[string]runtime.Provider) *Provider {
	if backends == nil {
		backends = make(map[string]runtime.Provider)
	}
	return &Provider{
		defaultSP: defaultSP,
		backends:  backends,
		routes:    make(map[string]string),
	}
}

// Route registers a session name to use the named backend provider.
// Must be called before Start for that session.
func (p *Provider) Route(sessionName, providerName string) {
	p.mu.Lock()
	p.routes[sessionName] = providerName
	p.mu.Unlock()
}

// RouteACP registers a session name to use the ACP backend.
// Backward-compatible shim for Route(name, "acp").
func (p *Provider) RouteACP(name string) {
	p.Route(name, "acp")
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
	provName, routed := p.routes[name]
	p.mu.RUnlock()
	if routed {
		if backend, ok := p.backends[provName]; ok {
			return backend
		}
	}
	return p.defaultSP
}

// DetectTransport reports the backend currently hosting the named session.
// It returns the provider name for override-backed sessions and "" for
// default or unknown.
func (p *Provider) DetectTransport(name string) string {
	if p.defaultSP.IsRunning(name) {
		return ""
	}
	for provName, backend := range p.backends {
		if backend.IsRunning(name) {
			return provName
		}
	}
	return ""
}

// Start delegates to the routed backend.
func (p *Provider) Start(ctx context.Context, name string, cfg runtime.Config) error {
	return p.route(name).Start(ctx, name, cfg)
}

// Stop delegates to the routed backend and cleans up the route entry
// only on success. If the routed backend fails, tries other backends
// to handle stale/missing route entries (e.g., after controller restart).
func (p *Provider) Stop(name string) error {
	primary := p.route(name)
	err := primary.Stop(name)
	if err == nil {
		p.Unroute(name)
		return nil
	}
	// Fall through to other backends in case the route is stale.
	for _, backend := range p.backends {
		if backend == primary {
			continue
		}
		if otherErr := backend.Stop(name); otherErr == nil {
			p.Unroute(name)
			return nil
		}
	}
	// Try default if primary wasn't the default.
	if primary != p.defaultSP {
		if otherErr := p.defaultSP.Stop(name); otherErr == nil {
			p.Unroute(name)
			return nil
		}
	}
	return err // return original error if all fail
}

// Interrupt delegates to the routed backend.
func (p *Provider) Interrupt(name string) error {
	return p.route(name).Interrupt(name)
}

// IsRunning checks the routed backend first. If it reports not running,
// falls through to other backends to handle route table inconsistencies.
func (p *Provider) IsRunning(name string) bool {
	if p.route(name).IsRunning(name) {
		return true
	}
	// Fall through: check all backends in case routing is stale.
	if p.defaultSP.IsRunning(name) {
		return true
	}
	for _, backend := range p.backends {
		if backend.IsRunning(name) {
			return true
		}
	}
	return false
}

// IsAttached delegates to the routed backend.
func (p *Provider) IsAttached(name string) bool {
	return p.route(name).IsAttached(name)
}

// Attach delegates to the routed backend. Non-attachable backends
// (those that don't support terminal attachment) return an error.
func (p *Provider) Attach(name string) error {
	p.mu.RLock()
	provName, routed := p.routes[name]
	p.mu.RUnlock()
	if routed && !isAttachableProvider(provName) {
		return fmt.Errorf("agent %q uses %s transport (no terminal to attach to)", name, provName)
	}
	return p.route(name).Attach(name)
}

// isAttachableProvider reports whether a provider name supports terminal
// attachment. tmux and hybrid providers are attachable; others are not.
func isAttachableProvider(provName string) bool {
	switch provName {
	case "tmux", "hybrid", "":
		return true
	default:
		return false
	}
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
func (p *Provider) WaitForIdle(name string, timeout time.Duration) error {
	if wp, ok := p.route(name).(runtime.IdleWaitProvider); ok {
		return wp.WaitForIdle(name, timeout)
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

// ListRunning queries all backends and merges results. If any backend
// fails, partial results are returned along with the error so callers
// can distinguish complete vs partial results.
func (p *Provider) ListRunning(prefix string) ([]string, error) {
	var merged []string
	var errs []error
	defaultList, dErr := p.defaultSP.ListRunning(prefix)
	merged = append(merged, defaultList...)
	if dErr != nil {
		errs = append(errs, fmt.Errorf("default backend: %w", dErr))
	}
	for provName, backend := range p.backends {
		list, err := backend.ListRunning(prefix)
		merged = append(merged, list...)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s backend: %w", provName, err))
		}
	}
	if len(errs) > 0 {
		if len(errs) == 1+len(p.backends) {
			// All backends failed — return nil results.
			return nil, errors.Join(errs...)
		}
		return merged, errors.Join(errs...)
	}
	return merged, nil
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

// Capabilities returns the intersection of all backends' capabilities.
// A capability is reported only if all backends support it.
func (p *Provider) Capabilities() runtime.ProviderCapabilities {
	caps := p.defaultSP.Capabilities()
	for _, backend := range p.backends {
		bc := backend.Capabilities()
		caps.CanReportAttachment = caps.CanReportAttachment && bc.CanReportAttachment
		caps.CanReportActivity = caps.CanReportActivity && bc.CanReportActivity
	}
	return caps
}
