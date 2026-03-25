package extmsg

import (
	"context"
	"sync"
	"testing"
)

// fakeAdapter implements TransportAdapter for testing.
type fakeAdapter struct {
	name string
	caps AdapterCapabilities
}

func (a *fakeAdapter) Name() string                      { return a.name }
func (a *fakeAdapter) Capabilities() AdapterCapabilities { return a.caps }
func (a *fakeAdapter) VerifyAndNormalizeInbound(_ context.Context, _ InboundPayload) (*ExternalInboundMessage, error) {
	return nil, ErrAdapterUnsupported
}

func (a *fakeAdapter) Publish(_ context.Context, _ PublishRequest) (*PublishReceipt, error) {
	return nil, ErrAdapterUnsupported
}

func (a *fakeAdapter) EnsureChildConversation(_ context.Context, _ ConversationRef, _ string) (*ConversationRef, error) {
	return nil, ErrAdapterUnsupported
}

func TestAdapterRegistryRegisterAndLookup(t *testing.T) {
	reg := NewAdapterRegistry()
	key := AdapterKey{Provider: "discord", AccountID: "bot-1"}
	adapter := &fakeAdapter{name: "discord-bot-1"}

	// Lookup before register returns nil.
	if got := reg.Lookup(key); got != nil {
		t.Fatalf("expected nil, got %v", got)
	}

	reg.Register(key, adapter)

	got := reg.Lookup(key)
	if got == nil {
		t.Fatal("expected adapter, got nil")
	}
	if got.Name() != "discord-bot-1" {
		t.Fatalf("expected discord-bot-1, got %s", got.Name())
	}
}

func TestAdapterRegistryLookupByConversation(t *testing.T) {
	reg := NewAdapterRegistry()
	key := AdapterKey{Provider: "slack", AccountID: "workspace-1"}
	adapter := &fakeAdapter{name: "slack-ws1"}
	reg.Register(key, adapter)

	ref := ConversationRef{
		ScopeID:        "scope",
		Provider:       "slack",
		AccountID:      "workspace-1",
		ConversationID: "channel-42",
		Kind:           ConversationRoom,
	}
	got := reg.LookupByConversation(ref)
	if got == nil || got.Name() != "slack-ws1" {
		t.Fatalf("expected slack-ws1, got %v", got)
	}

	// Different account returns nil.
	ref.AccountID = "workspace-other"
	if got := reg.LookupByConversation(ref); got != nil {
		t.Fatalf("expected nil for different account, got %v", got)
	}
}

func TestAdapterRegistryReplace(t *testing.T) {
	reg := NewAdapterRegistry()
	key := AdapterKey{Provider: "discord", AccountID: "bot-1"}
	reg.Register(key, &fakeAdapter{name: "v1"})
	reg.Register(key, &fakeAdapter{name: "v2"})

	got := reg.Lookup(key)
	if got == nil || got.Name() != "v2" {
		t.Fatalf("expected v2, got %v", got)
	}
}

func TestAdapterRegistryUnregister(t *testing.T) {
	reg := NewAdapterRegistry()
	key := AdapterKey{Provider: "discord", AccountID: "bot-1"}
	reg.Register(key, &fakeAdapter{name: "d1"})
	reg.Unregister(key)

	if got := reg.Lookup(key); got != nil {
		t.Fatalf("expected nil after unregister, got %v", got)
	}
}

func TestAdapterRegistryUnregisterMissing(_ *testing.T) {
	reg := NewAdapterRegistry()
	// Should not panic.
	reg.Unregister(AdapterKey{Provider: "x", AccountID: "y"})
}

func TestAdapterRegistryList(t *testing.T) {
	reg := NewAdapterRegistry()
	reg.Register(AdapterKey{Provider: "discord", AccountID: "a"}, &fakeAdapter{name: "d-a"})
	reg.Register(AdapterKey{Provider: "slack", AccountID: "b"}, &fakeAdapter{name: "s-b"})

	keys := reg.List()
	if len(keys) != 2 {
		t.Fatalf("expected 2 keys, got %d", len(keys))
	}

	found := make(map[AdapterKey]bool)
	for _, k := range keys {
		found[k] = true
	}
	if !found[AdapterKey{Provider: "discord", AccountID: "a"}] {
		t.Fatal("missing discord/a")
	}
	if !found[AdapterKey{Provider: "slack", AccountID: "b"}] {
		t.Fatal("missing slack/b")
	}
}

func TestAdapterRegistryConcurrency(_ *testing.T) {
	reg := NewAdapterRegistry()
	var wg sync.WaitGroup
	const n = 100

	// Concurrent register + lookup.
	for i := 0; i < n; i++ {
		wg.Add(2)
		key := AdapterKey{Provider: "p", AccountID: "a"}
		go func() {
			defer wg.Done()
			reg.Register(key, &fakeAdapter{name: "concurrent"})
		}()
		go func() {
			defer wg.Done()
			_ = reg.Lookup(key)
		}()
	}
	wg.Wait()
}
