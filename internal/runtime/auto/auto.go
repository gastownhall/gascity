// Package auto provides a composite [runtime.Provider] that routes
// sessions to a default backend or per-session override backends based
// on registration. Sessions are registered via [Provider.Route] before
// [Provider.Start] is called. Unregistered sessions route to the
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

// Provider routes session operations to a default backend or
// per-session override backends based on registration.
type Provider struct {
	defaultSP runtime.Provider

	mu        sync.RWMutex
	routes    map[string]string           // session name → provider key
	providers map[string]runtime.Provider // provider key → provider instance
}

var (
	_ runtime.Provider            = (*Provider)(nil)
	_ runtime.InteractionProvider = (*Provider)(nil)
)

// New creates a composite provider. defaultSP handles sessions not
// registered with a specific backend. Additional backends are
// registered via [Provider.AddBackend] and sessions are routed to
// them via [Provider.Route].
func New(defaultSP runtime.Provider) *Provider {
	return &Provider{
		defaultSP: defaultSP,
		routes:    make(map[string]string),
		providers: make(map[string]runtime.Provider),
	}
}

// AddBackend registers a named backend provider. The key is used in
// [Provider.Route] and [Provider.RouteACP] calls to associate
// sessions with this backend.
func (p *Provider) AddBackend(key string, sp runtime.Provider) {
	p.mu.Lock()
	p.providers[key] = sp
	p.mu.Unlock()
}

// Route registers a session name to use the named backend. Returns an
// error if providerKey has not been registered via [Provider.AddBackend].
// Must be called before Start for that session.
func (p *Provider) Route(name, providerKey string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.providers[providerKey]; !ok {
		return fmt.Errorf("auto: provider key %q not registered (call AddBackend first)", providerKey)
	}
	p.routes[name] = providerKey
	return nil
}

// RouteACP registers a session name to use the "acp" backend. Returns
// an error if the "acp" backend has not been registered via AddBackend.
func (p *Provider) RouteACP(name string) error {
	return p.Route(name, "acp")
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
	key := p.routes[name]
	sp := p.providers[key]
	p.mu.RUnlock()
	if sp != nil {
		return sp
	}
	return p.defaultSP
}

// allBackends returns all registered backend providers (including default).
// Used by ListRunning and fallthrough logic.
func (p *Provider) allBackends() []runtime.Provider {
	p.mu.RLock()
	defer p.mu.RUnlock()
	seen := make(map[runtime.Provider]bool)
	result := []runtime.Provider{p.defaultSP}
	seen[p.defaultSP] = true
	for _, sp := range p.providers {
		if !seen[sp] {
			result = append(result, sp)
			seen[sp] = true
		}
	}
	return result
}

// DetectTransport reports the backend currently hosting the named session.
// It returns the provider key for override-backed sessions and "" for
// default or unknown. Used by the session manager to backfill transport
// metadata on legacy session beads. The route table is only trusted when
// the routed backend reports the session as running; otherwise all
// backends are probed.
func (p *Provider) DetectTransport(name string) string {
	p.mu.RLock()
	key := p.routes[name]
	routed := p.providers[key]
	p.mu.RUnlock()
	if routed != nil && routed.IsRunning(name) {
		return key
	}
	if p.defaultSP.IsRunning(name) {
		return ""
	}
	// Probe non-default backends for sessions with stale/missing routes.
	p.mu.RLock()
	defer p.mu.RUnlock()
	for k, sp := range p.providers {
		if sp.IsRunning(name) {
			return k
		}
	}
	return ""
}

// Start delegates to the routed backend.
func (p *Provider) Start(ctx context.Context, name string, cfg runtime.Config) error {
	return p.route(name).Start(ctx, name, cfg)
}

// Stop delegates to the routed backend and cleans up the route entry
// only on success. If the routed backend fails, tries all other backends
// to handle stale/missing route entries (e.g., after controller restart).
func (p *Provider) Stop(name string) error {
	primary := p.route(name)
	err := primary.Stop(name)
	if err == nil {
		p.Unroute(name)
		return nil
	}
	// Fall through to other backends in case the route is stale.
	for _, backend := range p.allBackends() {
		if backend == primary {
			continue
		}
		if otherErr := backend.Stop(name); otherErr == nil {
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
	// Fall through: check other backends in case routing is stale.
	primary := p.route(name)
	for _, backend := range p.allBackends() {
		if backend == primary {
			continue
		}
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

// ListRunning queries all backends and merges results.
func (p *Provider) ListRunning(prefix string) ([]string, error) {
	backends := p.allBackends()
	var merged []string
	var errs []error
	for _, backend := range backends {
		names, err := backend.ListRunning(prefix)
		merged = append(merged, names...)
		if err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) == len(backends) {
		return nil, errors.Join(errs...)
	}
	if len(errs) > 0 {
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
func (p *Provider) Capabilities() runtime.ProviderCapabilities {
	caps := p.defaultSP.Capabilities()
	for _, backend := range p.allBackends() {
		bc := backend.Capabilities()
		caps.CanReportAttachment = caps.CanReportAttachment && bc.CanReportAttachment
		caps.CanReportActivity = caps.CanReportActivity && bc.CanReportActivity
	}
	return caps
}

// SleepCapability reports idle sleep capability for the routed backend.
func (p *Provider) SleepCapability(name string) runtime.SessionSleepCapability {
	if scp, ok := p.route(name).(runtime.SleepCapabilityProvider); ok {
		return scp.SleepCapability(name)
	}
	return runtime.SessionSleepCapabilityDisabled
}
