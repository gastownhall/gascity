package extmsg

import "testing"

// TestAdapterRegistry_ConcurrentSubscribesForDifferentConversationsDoNotClobber
// reproduces the reported bug: the llm-client subscribe path registers one
// adapter per (client_id, conversation_id), but a key built from Provider
// and AccountID alone collapses two different conversations for the same
// client into a single map entry. The second Register must not overwrite
// the first.
func TestAdapterRegistry_ConcurrentSubscribesForDifferentConversationsDoNotClobber(t *testing.T) {
	reg := NewAdapterRegistry()
	convA := ConversationRef{Provider: ProviderLLMClient, AccountID: "client-1", ConversationID: "conv-a"}
	convB := ConversationRef{Provider: ProviderLLMClient, AccountID: "client-1", ConversationID: "conv-b"}

	adapterA := newStubAdapter("a", convA)
	adapterB := newStubAdapter("b", convB)

	keyA := AdapterKey{Provider: convA.Provider, AccountID: convA.AccountID, ConversationID: convA.ConversationID}
	keyB := AdapterKey{Provider: convB.Provider, AccountID: convB.AccountID, ConversationID: convB.ConversationID}

	reg.Register(keyA, adapterA)
	reg.Register(keyB, adapterB)

	if got := reg.LookupByConversation(convA); got != adapterA {
		t.Fatalf("LookupByConversation(convA) = %v, want adapterA", got)
	}
	if got := reg.LookupByConversation(convB); got != adapterB {
		t.Fatalf("LookupByConversation(convB) = %v, want adapterB", got)
	}
}

// TestAdapterRegistry_UnregisterConversationScopedDoesNotEvictSiblingConversation
// covers the other half of the bug: a deferred Unregister for one
// connection must not delete a sibling connection's still-live adapter.
func TestAdapterRegistry_UnregisterConversationScopedDoesNotEvictSiblingConversation(t *testing.T) {
	reg := NewAdapterRegistry()
	convA := ConversationRef{Provider: ProviderLLMClient, AccountID: "client-1", ConversationID: "conv-a"}
	convB := ConversationRef{Provider: ProviderLLMClient, AccountID: "client-1", ConversationID: "conv-b"}

	adapterB := newStubAdapter("b", convB)

	keyA := AdapterKey{Provider: convA.Provider, AccountID: convA.AccountID, ConversationID: convA.ConversationID}
	keyB := AdapterKey{Provider: convB.Provider, AccountID: convB.AccountID, ConversationID: convB.ConversationID}

	reg.Register(keyA, newStubAdapter("a", convA))
	reg.Register(keyB, adapterB)

	reg.Unregister(keyA)

	if got := reg.LookupByConversation(convB); got != adapterB {
		t.Fatalf("LookupByConversation(convB) after sibling Unregister = %v, want adapterB still registered", got)
	}
}

// TestAdapterRegistry_LookupByConversationFallsBackToAccountWideAdapter
// guards the discord/slack-style path: an adapter registered with no
// ConversationID (one adapter serving every conversation under an account)
// must still resolve when looked up with a ConversationRef that carries a
// real ConversationID.
func TestAdapterRegistry_LookupByConversationFallsBackToAccountWideAdapter(t *testing.T) {
	reg := NewAdapterRegistry()
	accountWideKey := AdapterKey{Provider: "discord", AccountID: "acct-1"}
	adapter := newStubAdapter("discord-acct-1", ConversationRef{Provider: "discord", AccountID: "acct-1"})
	reg.Register(accountWideKey, adapter)

	ref := ConversationRef{Provider: "discord", AccountID: "acct-1", ConversationID: "thread-request"}
	if got := reg.LookupByConversation(ref); got != adapter {
		t.Fatalf("LookupByConversation(ref with ConversationID) = %v, want account-wide adapter as fallback", got)
	}

	ref2 := ConversationRef{Provider: "discord", AccountID: "acct-1", ConversationID: "thread-delivered"}
	if got := reg.LookupByConversation(ref2); got != adapter {
		t.Fatalf("LookupByConversation(ref2 with different ConversationID) = %v, want same account-wide adapter", got)
	}
}

// TestAdapterRegistry_ConversationScopedTakesPrecedenceOverAccountWide
// covers the case where both an account-wide adapter and a
// conversation-scoped adapter exist for the same (Provider, AccountID): the
// more specific registration must win.
func TestAdapterRegistry_ConversationScopedTakesPrecedenceOverAccountWide(t *testing.T) {
	reg := NewAdapterRegistry()
	accountWide := newStubAdapter("account-wide", ConversationRef{Provider: ProviderLLMClient, AccountID: "client-1"})
	scoped := newStubAdapter("scoped", ConversationRef{Provider: ProviderLLMClient, AccountID: "client-1", ConversationID: "conv-a"})

	reg.Register(AdapterKey{Provider: ProviderLLMClient, AccountID: "client-1"}, accountWide)
	reg.Register(AdapterKey{Provider: ProviderLLMClient, AccountID: "client-1", ConversationID: "conv-a"}, scoped)

	ref := ConversationRef{Provider: ProviderLLMClient, AccountID: "client-1", ConversationID: "conv-a"}
	if got := reg.LookupByConversation(ref); got != scoped {
		t.Fatalf("LookupByConversation = %v, want conversation-scoped adapter to take precedence", got)
	}
}
