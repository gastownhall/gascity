package events

import (
	"encoding/json"
	"testing"
)

type samplePayload struct {
	A string `json:"a"`
}

func (samplePayload) IsEventPayload() {}

func TestRegisterAndLookup(t *testing.T) {
	const evt = "test.register.lookup"
	// Clean up after the test to avoid polluting global registry.
	t.Cleanup(func() {
		payloadRegistryMu.Lock()
		delete(payloadRegistry, evt)
		payloadRegistryMu.Unlock()
	})

	RegisterPayload(evt, samplePayload{})
	got, ok := LookupPayload(evt)
	if !ok {
		t.Fatalf("expected registered event %q to be found", evt)
	}
	if _, ok := got.(samplePayload); !ok {
		t.Fatalf("expected samplePayload, got %T", got)
	}
}

func TestDecodePayloadRegistered(t *testing.T) {
	const evt = "test.decode.registered"
	t.Cleanup(func() {
		payloadRegistryMu.Lock()
		delete(payloadRegistry, evt)
		payloadRegistryMu.Unlock()
	})
	RegisterPayload(evt, samplePayload{})

	raw := json.RawMessage(`{"a":"hello"}`)
	got, registered, err := DecodePayload(evt, raw)
	if err != nil {
		t.Fatalf("decode err: %v", err)
	}
	if !registered {
		t.Fatalf("expected registered=true")
	}
	sp, ok := got.(samplePayload)
	if !ok {
		t.Fatalf("expected samplePayload, got %T", got)
	}
	if sp.A != "hello" {
		t.Fatalf("A = %q, want hello", sp.A)
	}
}

func TestDecodePayloadUnregistered(t *testing.T) {
	raw := json.RawMessage(`{"anything":true}`)
	got, registered, err := DecodePayload("test.never.registered", raw)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if registered {
		t.Fatalf("expected registered=false")
	}
	if got != nil {
		t.Fatalf("expected nil payload, got %v", got)
	}
}

func TestDecodePayloadEmptyBytesZeroValue(t *testing.T) {
	const evt = "test.decode.empty"
	t.Cleanup(func() {
		payloadRegistryMu.Lock()
		delete(payloadRegistry, evt)
		payloadRegistryMu.Unlock()
	})
	RegisterPayload(evt, NoPayload{})

	got, registered, err := DecodePayload(evt, nil)
	if err != nil {
		t.Fatalf("decode err: %v", err)
	}
	if !registered {
		t.Fatalf("expected registered=true")
	}
	if _, ok := got.(NoPayload); !ok {
		t.Fatalf("expected NoPayload, got %T", got)
	}
}

func TestRegisterConflictPanics(t *testing.T) {
	const evt = "test.conflict"
	t.Cleanup(func() {
		payloadRegistryMu.Lock()
		delete(payloadRegistry, evt)
		payloadRegistryMu.Unlock()
	})
	RegisterPayload(evt, samplePayload{})

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on conflicting re-registration")
		}
	}()
	RegisterPayload(evt, NoPayload{})
}

func TestRegisterSameTypeIdempotent(t *testing.T) {
	const evt = "test.idempotent"
	t.Cleanup(func() {
		payloadRegistryMu.Lock()
		delete(payloadRegistry, evt)
		payloadRegistryMu.Unlock()
	})
	RegisterPayload(evt, samplePayload{})
	// Second call with same type must not panic.
	RegisterPayload(evt, samplePayload{})
}
