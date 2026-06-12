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

// Registry maps runtime selection names to provider factories.
// The zero value is not usable; call [New].
type Registry struct {
	mu       sync.RWMutex
	exact    map[string]Factory
	prefixes map[string]Factory
	fallback Factory
}

// New returns an empty Registry.
func New() *Registry {
	return &Registry{
		exact:    make(map[string]Factory),
		prefixes: make(map[string]Factory),
	}
}

// Register binds an exact selection name to a factory. Duplicate names,
// blank names, and nil factories are errors.
func (r *Registry) Register(name string, f Factory) error {
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
	r.exact[name] = f
	return nil
}

// RegisterPrefix binds a selection-name prefix (which must end in ':',
// e.g. "exec:") to a factory. The factory receives the full selection
// name. Duplicate prefixes, malformed prefixes, and nil factories are
// errors.
func (r *Registry) RegisterPrefix(prefix string, f Factory) error {
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
	r.prefixes[prefix] = f
	return nil
}

// SetFallback binds the factory used when no exact name or prefix
// matches. The current production fallback is tmux (see
// cmd/gc/runtime_registry.go); without a fallback, unknown names return
// [ErrUnknownRuntime].
func (r *Registry) SetFallback(f Factory) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fallback = f
}

// New resolves a selection name and constructs its provider.
func (r *Registry) New(name string, sc config.SessionConfig, cityName, cityPath string) (runtime.Provider, error) {
	f := r.lookup(name)
	if f == nil {
		return nil, fmt.Errorf("%w: %q", ErrUnknownRuntime, name)
	}
	p, err := f(name, sc, cityName, cityPath)
	if err != nil {
		return nil, fmt.Errorf("constructing runtime provider %q: %w", name, err)
	}
	return p, nil
}

func (r *Registry) lookup(name string) Factory {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if f, ok := r.exact[name]; ok {
		return f
	}
	var bestPrefix string
	var best Factory
	for prefix, f := range r.prefixes {
		if strings.HasPrefix(name, prefix) && len(prefix) > len(bestPrefix) {
			bestPrefix, best = prefix, f
		}
	}
	if best != nil {
		return best
	}
	return r.fallback
}

// Has reports whether an exact selection name is registered.
func (r *Registry) Has(name string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.exact[name]
	return ok
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
