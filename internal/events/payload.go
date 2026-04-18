package events

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sync"
)

// Payload is the sealed interface implemented by every typed event
// payload. Typed payloads flow into the event bus as []byte (the
// domain-agnostic storage shape, see Event.Payload) and back out at the
// SSE projection layer, which uses the registry below to decode each
// event's bytes into the concrete Go type before emitting it on the
// typed /v0/events/stream wire schema (Principle 7).
//
// IsEventPayload is a marker method with no semantic meaning; its
// purpose is to seal the interface so map[string]any and other
// ad-hoc shapes cannot satisfy it accidentally. The method is
// exported (rather than unexported) because payload types live in
// multiple packages (internal/api, internal/extmsg, etc.) and Go
// requires an exported marker for cross-package implementations.
type Payload interface {
	IsEventPayload()
}

// NoPayload is the registered shape for event types that carry no
// structured payload beyond the Event envelope's Actor / Subject /
// Message / Seq / Type / Ts fields. Use it when an event type is
// semantically identified by its envelope alone and does not need
// additional per-variant fields.
type NoPayload struct{}

func (NoPayload) IsEventPayload() {}

// payloadRegistry holds the event-type → sample Payload mapping used
// by the SSE projection to decode bytes back into typed Go values.
// It is populated at init time by callers of RegisterPayload.
var (
	payloadRegistryMu sync.RWMutex
	payloadRegistry   = map[string]Payload{}
)

// RegisterPayload associates an event-type constant with a sample
// Payload value. The sample's Go type is used to JSON-decode every
// emission of this event type at the SSE projection layer. Panics if
// the same event type is registered twice with different sample types
// (sameness by reflect.Type) — the registry is a compile-visible
// schema and conflicting entries are a bug, not a runtime condition.
func RegisterPayload(eventType string, sample Payload) {
	payloadRegistryMu.Lock()
	defer payloadRegistryMu.Unlock()
	if existing, ok := payloadRegistry[eventType]; ok {
		if reflect.TypeOf(existing) != reflect.TypeOf(sample) {
			panic(fmt.Sprintf("events: payload for %q already registered as %T, cannot re-register as %T",
				eventType, existing, sample))
		}
		return
	}
	payloadRegistry[eventType] = sample
}

// LookupPayload returns the registered sample Payload for eventType,
// or (nil, false) if the event type has no typed payload registered
// yet. The sample is a zero value of the Go type; use reflect.New on
// reflect.TypeOf(sample) to allocate a fresh instance for decoding.
func LookupPayload(eventType string) (Payload, bool) {
	payloadRegistryMu.RLock()
	defer payloadRegistryMu.RUnlock()
	p, ok := payloadRegistry[eventType]
	return p, ok
}

// RegisteredPayloadTypes returns a snapshot of the event-type →
// sample Payload mapping. Used by the SSE projection to build the
// Huma eventTypeMap so the OpenAPI spec emits a oneOf of typed
// variants for /v0/events/stream and /v0/city/{cityName}/events/stream.
func RegisteredPayloadTypes() map[string]Payload {
	payloadRegistryMu.RLock()
	defer payloadRegistryMu.RUnlock()
	out := make(map[string]Payload, len(payloadRegistry))
	for k, v := range payloadRegistry {
		out[k] = v
	}
	return out
}

// DecodePayload JSON-decodes the raw bytes for an event into the
// registered Go type. Returns the decoded value, true if a typed
// payload was registered, and any decode error. When the event type
// is not registered, returns (nil, false, nil) so callers can fall
// back to passing the raw bytes through the opaque envelope path.
func DecodePayload(eventType string, raw json.RawMessage) (any, bool, error) {
	sample, ok := LookupPayload(eventType)
	if !ok {
		return nil, false, nil
	}
	if len(raw) == 0 {
		// Zero-length payload for a registered type: return the zero
		// value. NoPayload registrations always hit this path.
		return reflect.New(reflect.TypeOf(sample)).Elem().Interface(), true, nil
	}
	instance := reflect.New(reflect.TypeOf(sample))
	if err := json.Unmarshal(raw, instance.Interface()); err != nil {
		return nil, true, fmt.Errorf("events: decode %q payload: %w", eventType, err)
	}
	return instance.Elem().Interface(), true, nil
}
