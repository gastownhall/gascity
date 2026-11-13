// Package registry resolves runtime provider selection names (the
// `session = "<name>"` values from city configuration) to factories that
// construct [runtime.Provider] values. It is the seam that lets provider
// wiring move out of a hardcoded switch: builtin providers register here
// from cmd/gc, and pack-declared runtimes will register here during city
// composition (ga-h504e5).
//
// Resolution order: exact name, then longest matching registered prefix
// (e.g. "exec:"), then the fallback. Registration collisions are errors —
// a pack-declared runtime must never silently shadow a builtin.
package registry

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// ErrUnknownRuntime reports a selection name with no registered factory,
// prefix match, or fallback.
var ErrUnknownRuntime = errors.New("unknown runtime provider")

// Factory constructs a runtime.Provider for the given selection name.
// Prefix factories receive the full selection name including the prefix
// (e.g. "exec:/path/to/script") and own its parsing.
type Factory func(name string, sc config.SessionConfig, cityName, cityPath string) (runtime.Provider, error)

// HostCapabilitiesFunc reports what the runtime behind a selection name
// guarantees about the controller's host. Like a prefix [Factory], it
// receives the full selection name, so a prefix registration can answer per
// selection (an exec: script reports through its own handshake).
type HostCapabilitiesFunc func(name, cityName, cityPath string) runtime.HostCapabilities

// StaticHostCapabilities returns a HostCapabilitiesFunc that reports caps for
// every selection.
func StaticHostCapabilities(caps runtime.HostCapabilities) HostCapabilitiesFunc {
	return func(_, _, _ string) runtime.HostCapabilities { return caps }
}

// Option configures one registration.
type Option func(*entry)

// WithHostCapabilities declares the registration's host capabilities. A
// registration without it reports the zero [runtime.HostCapabilities].
func WithHostCapabilities(f HostCapabilitiesFunc) Option {
	return func(e *entry) { e.host = f }
}

// entry is one registration: the factory and what its runtime declares.
type entry struct {
	factory Factory
	host    HostCapabilitiesFunc
}

func newEntry(f Factory, opts []Option) entry {
	e := entry{factory: f}
	for _, opt := range opts {
		opt(&e)
	}
	return e
}

// Registry maps runtime selection names to provider factories.
// The zero value is not usable; call [New].
type Registry struct {
	mu       sync.RWMutex
	exact    map[string]entry
	prefixes map[string]entry
	fallback entry
}

// New returns an empty Registry.
func New() *Registry {
	return &Registry{
		exact:    make(map[string]entry),
		prefixes: make(map[string]entry),
	}
}

// Register binds an exact selection name to a factory. Duplicate names,
// blank names, and nil factories are errors.
func (r *Registry) Register(name string, f Factory, opts ...Option) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("registering runtime provider: name is empty")
	}
	if f == nil {
		return fmt.Errorf("registering runtime provider %q: factory is nil", name)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.exact[name]; exists {
		return fmt.Errorf("registering runtime provider %q: name already registered", name)
	}
	r.exact[name] = newEntry(f, opts)
	return nil
}

// RegisterPrefix binds a selection-name prefix (which must end in ':',
// e.g. "exec:") to a factory. The factory receives the full selection
// name. Duplicate prefixes, malformed prefixes, and nil factories are
// errors.
func (r *Registry) RegisterPrefix(prefix string, f Factory, opts ...Option) error {
	if prefix == "" || !strings.HasSuffix(prefix, ":") {
		return fmt.Errorf("registering runtime provider prefix %q: prefix must end with ':'", prefix)
	}
	if f == nil {
		return fmt.Errorf("registering runtime provider prefix %q: factory is nil", prefix)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.prefixes[prefix]; exists {
		return fmt.Errorf("registering runtime provider prefix %q: prefix already registered", prefix)
	}
	r.prefixes[prefix] = newEntry(f, opts)
	return nil
}

// SetFallback binds the factory used when no exact name or prefix
// matches. The current production fallback is tmux (see
// cmd/gc/runtime_registry.go); without a fallback, unknown names return
// [ErrUnknownRuntime].
func (r *Registry) SetFallback(f Factory, opts ...Option) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fallback = newEntry(f, opts)
}

// New resolves a selection name and constructs its provider.
func (r *Registry) New(name string, sc config.SessionConfig, cityName, cityPath string) (runtime.Provider, error) {
	f := r.lookup(name).factory
	if f == nil {
		return nil, fmt.Errorf("%w: %q", ErrUnknownRuntime, name)
	}
	p, err := f(name, sc, cityName, cityPath)
	if err != nil {
		return nil, fmt.Errorf("constructing runtime provider %q: %w", name, err)
	}
	return p, nil
}

// HostCapabilities reports what the runtime a selection name resolves to
// guarantees about the controller's host. It resolves exactly as [New] does —
// exact name, longest prefix, then the fallback — so it describes the
// runtime that would actually run. A selection that resolves to nothing, or
// to a registration without [WithHostCapabilities], reports the zero value.
func (r *Registry) HostCapabilities(name, cityName, cityPath string) runtime.HostCapabilities {
	host := r.lookup(name).host
	if host == nil {
		return runtime.HostCapabilities{}
	}
	return host(name, cityName, cityPath)
}

func (r *Registry) lookup(name string) entry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if e, ok := r.exact[name]; ok {
		return e
	}
	var bestPrefix string
	var best entry
	for prefix, e := range r.prefixes {
		if strings.HasPrefix(name, prefix) && len(prefix) > len(bestPrefix) {
			bestPrefix, best = prefix, e
		}
	}
	if best.factory != nil {
		return best
	}
	return r.fallback
}

// Resolves reports whether name resolves to an exact or prefix registration,
// that is, to something other than the fallback.
func (r *Registry) Resolves(name string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if _, ok := r.exact[name]; ok {
		return true
	}
	for prefix := range r.prefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// Clone returns a registry with the receiver's registrations that shares
// no mutable state with it. City composition clones the builtin registry
// and registers pack-declared runtimes on the copy, so concurrent cities
// in one process never observe each other's registrations and the builtin
// registry itself is never mutated after construction.
func (r *Registry) Clone() *Registry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	c := New()
	for name, e := range r.exact {
		c.exact[name] = e
	}
	for prefix, e := range r.prefixes {
		c.prefixes[prefix] = e
	}
	c.fallback = r.fallback
	return c
}

// Has reports whether an exact selection name is registered.
func (r *Registry) Has(name string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.exact[name]
	return ok
}

// Prefixes returns the registered prefix selections, sorted.
func (r *Registry) Prefixes() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	prefixes := make([]string, 0, len(r.prefixes))
	for prefix := range r.prefixes {
		prefixes = append(prefixes, prefix)
	}
	sort.Strings(prefixes)
	return prefixes
}

// Names returns the registered exact selection names, sorted.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.exact))
	for name := range r.exact {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
