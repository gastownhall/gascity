package extmsg

import "sync"

// AdapterKey uniquely identifies a registered transport adapter.
type AdapterKey struct {
	Provider  string
	AccountID string
}

// AdapterRegistry is a concurrent-safe, ephemeral registry of transport
// adapters keyed by (Provider, AccountID). Created once per controller
// lifetime and not rebuilt on config hot-reload.
//
// Registrations are in-memory only and do not survive controller restarts.
// Out-of-process adapters must re-register on reconnect. Unregister does
// not drain in-flight operations; callers that hold adapter references may
// see connection errors if the external service is torn down immediately.
type AdapterRegistry struct {
	mu       sync.RWMutex
	adapters map[AdapterKey]TransportAdapter
}

// NewAdapterRegistry creates an empty adapter registry.
func NewAdapterRegistry() *AdapterRegistry {
	return &AdapterRegistry{
		adapters: make(map[AdapterKey]TransportAdapter),
	}
}

// Register adds or replaces an adapter for the given key.
func (r *AdapterRegistry) Register(key AdapterKey, adapter TransportAdapter) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.adapters[key] = adapter
}

// Unregister removes an adapter by key.
func (r *AdapterRegistry) Unregister(key AdapterKey) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.adapters, key)
}

// Lookup returns the adapter for the given key, or nil if not registered.
func (r *AdapterRegistry) Lookup(key AdapterKey) TransportAdapter {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.adapters[key]
}

// LookupByConversation finds the adapter for a ConversationRef by deriving
// the key from ref.Provider and ref.AccountID.
func (r *AdapterRegistry) LookupByConversation(ref ConversationRef) TransportAdapter {
	return r.Lookup(AdapterKey{Provider: ref.Provider, AccountID: ref.AccountID})
}

// List returns all registered adapter keys.
func (r *AdapterRegistry) List() []AdapterKey {
	r.mu.RLock()
	defer r.mu.RUnlock()
	keys := make([]AdapterKey, 0, len(r.adapters))
	for k := range r.adapters {
		keys = append(keys, k)
	}
	return keys
}
