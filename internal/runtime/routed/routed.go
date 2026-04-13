// Package routed provides a composite [runtime.Provider] that routes each
// session to a named backend. Routing is explicit per session; unregistered
// sessions use the default backend.
package routed

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// Provider routes session operations to named runtime backends.
type Provider struct {
	mu         sync.RWMutex
	defaultKey string
	defaultSP  runtime.Provider
	backends   map[string]runtime.Provider
	routes     map[string]string
}

var (
	_ runtime.Provider                    = (*Provider)(nil)
	_ runtime.InteractionProvider         = (*Provider)(nil)
	_ runtime.IdleWaitProvider            = (*Provider)(nil)
	_ runtime.ImmediateNudgeProvider      = (*Provider)(nil)
	_ runtime.SleepCapabilityProvider     = (*Provider)(nil)
	_ runtime.SessionCapabilitiesProvider = (*Provider)(nil)
)

// New creates a routed provider. defaultKey is the canonical ownership name
// for sessions that use defaultSP.
func New(defaultKey string, defaultSP runtime.Provider) *Provider {
	if defaultKey == "" {
		defaultKey = "default"
	}
	p := &Provider{
		defaultKey: defaultKey,
		defaultSP:  defaultSP,
		backends:   make(map[string]runtime.Provider),
		routes:     make(map[string]string),
	}
	p.backends[defaultKey] = defaultSP
	return p
}

// Register adds or replaces a named backend.
func (p *Provider) Register(key string, provider runtime.Provider) error {
	if key == "" {
		return fmt.Errorf("routed provider: backend key is required")
	}
	if provider == nil {
		return fmt.Errorf("routed provider: backend %q is nil", key)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.backends[key] = provider
	return nil
}

// Route registers a session name to use backend key. An empty key or the
// default key removes the explicit route.
func (p *Provider) Route(name, key string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if key == "" || key == p.defaultKey {
		delete(p.routes, name)
		return
	}
	p.routes[name] = key
}

// Unroute removes a session route.
func (p *Provider) Unroute(name string) {
	p.mu.Lock()
	delete(p.routes, name)
	p.mu.Unlock()
}

func (p *Provider) route(name string) (runtime.Provider, string) {
	p.mu.RLock()
	key := p.routes[name]
	if key == "" {
		key = p.defaultKey
	}
	sp := p.backends[key]
	if sp == nil {
		if key == p.defaultKey {
			sp = p.defaultSP
		} else {
			sp = missingBackend{key: key}
		}
	}
	p.mu.RUnlock()
	return sp, key
}

func (p *Provider) routeKey(name string) string {
	_, key := p.route(name)
	return key
}

func (p *Provider) backendSnapshot() map[string]runtime.Provider {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make(map[string]runtime.Provider, len(p.backends))
	for key, sp := range p.backends {
		out[key] = sp
	}
	return out
}

func (p *Provider) runningBackend(name string) (runtime.Provider, string, bool) {
	primary, primaryKey := p.route(name)
	if primary.IsRunning(name) {
		return primary, primaryKey, true
	}
	backends := p.backendSnapshot()
	keys := make([]string, 0, len(backends))
	for key := range backends {
		if key != primaryKey {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	for _, key := range keys {
		if backends[key].IsRunning(name) {
			return backends[key], key, true
		}
	}
	return primary, primaryKey, false
}

func (p *Provider) liveBackend(name string) (runtime.Provider, string) {
	if sp, key, ok := p.runningBackend(name); ok {
		return sp, key
	}
	return p.route(name)
}

// DetectTransport reports the non-default backend that appears to own name.
// It is kept for compatibility with session transport detection.
func (p *Provider) DetectTransport(name string) string {
	if _, key, ok := p.runningBackend(name); ok && key != p.defaultKey {
		return key
	}
	if key := p.routeKey(name); key != p.defaultKey {
		return key
	}
	backends := p.backendSnapshot()
	keys := make([]string, 0, len(backends))
	for key := range backends {
		if key != p.defaultKey {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	for _, key := range keys {
		if backends[key].IsRunning(name) {
			return key
		}
	}
	return ""
}

// Start delegates to the routed backend.
func (p *Provider) Start(ctx context.Context, name string, cfg runtime.Config) error {
	sp, _ := p.route(name)
	return sp.Start(ctx, name, cfg)
}

// Stop delegates to the backend that is actually running the session, probing
// other backends to recover from stale route tables after controller restarts.
func (p *Provider) Stop(name string) error {
	if sp, spKey, ok := p.runningBackend(name); ok {
		if err := sp.Stop(name); err != nil {
			if p.stopOtherRunningBackend(name, spKey) {
				p.Unroute(name)
				return nil
			}
			return err
		}
		p.Unroute(name)
		return nil
	}
	primary, _ := p.route(name)
	if err := primary.Stop(name); err != nil {
		return err
	}
	p.Unroute(name)
	return nil
}

func (p *Provider) stopOtherRunningBackend(name, skipKey string) bool {
	backends := p.backendSnapshot()
	keys := make([]string, 0, len(backends))
	for key := range backends {
		if key != skipKey {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	for _, key := range keys {
		if !backends[key].IsRunning(name) {
			continue
		}
		if backends[key].Stop(name) == nil {
			return true
		}
	}
	return false
}

// Interrupt delegates to the routed backend.
func (p *Provider) Interrupt(name string) error {
	sp, _ := p.liveBackend(name)
	return sp.Interrupt(name)
}

// IsRunning checks the routed backend first, then falls through to other
// backends to tolerate stale or missing routes.
func (p *Provider) IsRunning(name string) bool {
	_, _, ok := p.runningBackend(name)
	return ok
}

// IsAttached checks the routed backend first, then falls through to the backend
// that is actually running the session.
func (p *Provider) IsAttached(name string) bool {
	sp, _ := p.liveBackend(name)
	return sp.IsAttached(name)
}

// Attach delegates to the routed backend. ACP sessions remain explicitly
// non-attachable for the existing user-facing contract.
func (p *Provider) Attach(name string) error {
	sp, key := p.liveBackend(name)
	if key == "acp" {
		return fmt.Errorf("agent %q uses ACP transport (no terminal to attach to)", name)
	}
	return sp.Attach(name)
}

// ProcessAlive delegates to the routed backend.
func (p *Provider) ProcessAlive(name string, processNames []string) bool {
	if len(processNames) == 0 {
		return true
	}
	sp, _ := p.liveBackend(name)
	return sp.ProcessAlive(name, processNames)
}

// Nudge delegates to the routed backend.
func (p *Provider) Nudge(name string, content []runtime.ContentBlock) error {
	sp, _ := p.liveBackend(name)
	return sp.Nudge(name, content)
}

// WaitForIdle delegates when the routed backend supports idle waiting.
func (p *Provider) WaitForIdle(ctx context.Context, name string, timeout time.Duration) error {
	sp, _ := p.liveBackend(name)
	if wp, ok := sp.(runtime.IdleWaitProvider); ok {
		return wp.WaitForIdle(ctx, name, timeout)
	}
	return runtime.ErrInteractionUnsupported
}

// NudgeNow delegates when the routed backend supports immediate nudges.
func (p *Provider) NudgeNow(name string, content []runtime.ContentBlock) error {
	sp, _ := p.liveBackend(name)
	if np, ok := sp.(runtime.ImmediateNudgeProvider); ok {
		return np.NudgeNow(name, content)
	}
	return sp.Nudge(name, content)
}

// Pending delegates when the routed backend supports interactions.
func (p *Provider) Pending(name string) (*runtime.PendingInteraction, error) {
	sp, _ := p.liveBackend(name)
	if ip, ok := sp.(runtime.InteractionProvider); ok {
		return ip.Pending(name)
	}
	return nil, runtime.ErrInteractionUnsupported
}

// Respond delegates when the routed backend supports interactions.
func (p *Provider) Respond(name string, response runtime.InteractionResponse) error {
	sp, _ := p.liveBackend(name)
	if ip, ok := sp.(runtime.InteractionProvider); ok {
		return ip.Respond(name, response)
	}
	return runtime.ErrInteractionUnsupported
}

// SetMeta delegates to the routed backend.
func (p *Provider) SetMeta(name, key, value string) error {
	sp, _ := p.liveBackend(name)
	return sp.SetMeta(name, key, value)
}

// GetMeta delegates to the routed backend.
func (p *Provider) GetMeta(name, key string) (string, error) {
	sp, _ := p.liveBackend(name)
	return sp.GetMeta(name, key)
}

// RemoveMeta delegates to the routed backend.
func (p *Provider) RemoveMeta(name, key string) error {
	sp, _ := p.liveBackend(name)
	return sp.RemoveMeta(name, key)
}

// Peek delegates to the routed backend.
func (p *Provider) Peek(name string, lines int) (string, error) {
	sp, _ := p.liveBackend(name)
	return sp.Peek(name, lines)
}

// ListRunning queries all backends and merges successful results. It only
// returns an error when every backend fails.
func (p *Provider) ListRunning(prefix string) ([]string, error) {
	backends := p.backendSnapshot()
	keys := make([]string, 0, len(backends))
	for key := range backends {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	seen := make(map[string]bool)
	var merged []string
	var errs []error
	success := 0
	for _, key := range keys {
		names, err := backends[key].ListRunning(prefix)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s backend: %w", key, err))
			continue
		}
		success++
		for _, name := range names {
			if seen[name] {
				continue
			}
			seen[name] = true
			merged = append(merged, name)
		}
	}
	if len(errs) > 0 {
		if success == 0 {
			return nil, errors.Join(errs...)
		}
	}
	return merged, nil
}

// GetLastActivity delegates to the routed backend.
func (p *Provider) GetLastActivity(name string) (time.Time, error) {
	sp, _ := p.liveBackend(name)
	return sp.GetLastActivity(name)
}

// ClearScrollback delegates to the routed backend.
func (p *Provider) ClearScrollback(name string) error {
	sp, _ := p.liveBackend(name)
	return sp.ClearScrollback(name)
}

// CopyTo delegates to the routed backend.
func (p *Provider) CopyTo(name, src, relDst string) error {
	sp, _ := p.liveBackend(name)
	return sp.CopyTo(name, src, relDst)
}

// SendKeys delegates to the routed backend.
func (p *Provider) SendKeys(name string, keys ...string) error {
	sp, _ := p.liveBackend(name)
	return sp.SendKeys(name, keys...)
}

// RunLive delegates to the routed backend.
func (p *Provider) RunLive(name string, cfg runtime.Config) error {
	sp, _ := p.liveBackend(name)
	return sp.RunLive(name, cfg)
}

// Capabilities reports the intersection of all backend capabilities for
// global callers that cannot name a session-specific route.
func (p *Provider) Capabilities() runtime.ProviderCapabilities {
	backends := p.backendSnapshot()
	if len(backends) == 0 {
		return runtime.ProviderCapabilities{}
	}
	keys := make([]string, 0, len(backends))
	for key := range backends {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	caps := backends[keys[0]].Capabilities()
	for _, key := range keys[1:] {
		next := backends[key].Capabilities()
		caps.CanReportAttachment = caps.CanReportAttachment && next.CanReportAttachment
		caps.CanReportActivity = caps.CanReportActivity && next.CanReportActivity
	}
	return caps
}

// SessionCapabilities reports the routed backend's capabilities.
func (p *Provider) SessionCapabilities(name string) runtime.ProviderCapabilities {
	sp, _ := p.liveBackend(name)
	return sp.Capabilities()
}

// SleepCapability reports idle sleep capability for the routed backend.
func (p *Provider) SleepCapability(name string) runtime.SessionSleepCapability {
	sp, _ := p.liveBackend(name)
	if scp, ok := sp.(runtime.SleepCapabilityProvider); ok {
		return scp.SleepCapability(name)
	}
	caps := sp.Capabilities()
	switch {
	case caps.CanReportActivity && caps.CanReportAttachment:
		return runtime.SessionSleepCapabilityFull
	case caps.CanReportActivity:
		return runtime.SessionSleepCapabilityTimedOnly
	default:
		return runtime.SessionSleepCapabilityDisabled
	}
}

type missingBackend struct {
	key string
}

func (m missingBackend) err() error {
	return fmt.Errorf("routed provider: backend %q is not registered", m.key)
}

func (m missingBackend) Start(context.Context, string, runtime.Config) error { return m.err() }
func (m missingBackend) Stop(string) error                                   { return m.err() }
func (m missingBackend) Interrupt(string) error                              { return m.err() }
func (m missingBackend) IsRunning(string) bool                               { return false }
func (m missingBackend) IsAttached(string) bool                              { return false }
func (m missingBackend) Attach(string) error                                 { return m.err() }
func (m missingBackend) ProcessAlive(string, []string) bool                  { return false }
func (m missingBackend) Nudge(string, []runtime.ContentBlock) error          { return m.err() }
func (m missingBackend) SetMeta(string, string, string) error                { return m.err() }
func (m missingBackend) GetMeta(string, string) (string, error)              { return "", m.err() }
func (m missingBackend) RemoveMeta(string, string) error                     { return m.err() }
func (m missingBackend) Peek(string, int) (string, error)                    { return "", m.err() }
func (m missingBackend) ListRunning(string) ([]string, error)                { return nil, m.err() }
func (m missingBackend) GetLastActivity(string) (time.Time, error)           { return time.Time{}, m.err() }
func (m missingBackend) ClearScrollback(string) error                        { return m.err() }
func (m missingBackend) CopyTo(string, string, string) error                 { return m.err() }
func (m missingBackend) SendKeys(string, ...string) error                    { return m.err() }
func (m missingBackend) RunLive(string, runtime.Config) error                { return m.err() }
func (m missingBackend) Capabilities() runtime.ProviderCapabilities {
	return runtime.ProviderCapabilities{}
}
