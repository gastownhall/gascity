package extmsg

import "sync"

// AdapterKey uniquely identifies a registered transport adapter.
//
// ConversationID is the zero value ("") for adapters shared across every
// conversation under an account (the discord/slack-style HTTPAdapter path,
// registered once per Provider+AccountID via the admin API) and is set to a
// specific conversation for adapters scoped to exactly one conversation
// (the llm-client/connected-client SSE subscribe path, which registers one
// adapter per subscribe call). See LookupByConversation for how the two
// forms resolve through the same map.
type AdapterKey struct {
	Provider       string
	AccountID      string
	ConversationID string
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

// LookupByConversation finds the adapter for a ConversationRef. It first
// tries the conversation-scoped key (ref.ConversationID included), which
// matches an llm-client adapter registered for exactly this conversation;
// if none is registered, it falls back to the account-wide key (no
// ConversationID), which matches a discord/slack-style adapter registered
// once per account and shared across every conversation under it.
func (r *AdapterRegistry) LookupByConversation(ref ConversationRef) TransportAdapter {
	scoped := AdapterKey{Provider: ref.Provider, AccountID: ref.AccountID, ConversationID: ref.ConversationID}
	if a := r.Lookup(scoped); a != nil {
		return a
	}
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
