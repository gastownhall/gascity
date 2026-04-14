// Package specgen provides auto-generation of AsyncAPI and OpenAPI specs
// from the Go types used by the WebSocket and HTTP API.
//
// The action registry is the single source of truth — every WS action
// maps to its request/response Go types. The generator reflects on these
// types to produce JSON Schema via swaggest/go-asyncapi and
// swaggest/openapi-go, ensuring the output is spec-compliant.
//
// Usage:
//
//	go run ./cmd/specgen
//	go generate ./internal/api/specgen/...
//
//go:generate go run ../../cmd/specgen
package specgen

import (
	"reflect"
	"sort"
)

// ActionDef describes a single WebSocket action's request and response types.
type ActionDef struct {
	// Action is the dotted action name (e.g., "bead.create").
	Action string

	// Description is a short human-readable description.
	Description string

	// RequestType is the Go type for the request payload. Nil means no payload.
	RequestType reflect.Type

	// ResponseType is the Go type for the response result. Nil means no result.
	ResponseType reflect.Type

	// RequestSample is a zero-value instance of the request type for the
	// reflector. Set automatically by Register when RequestType is provided.
	RequestSample interface{}

	// ResponseSample is a zero-value instance of the response type for the
	// reflector. Set automatically by Register when ResponseType is provided.
	ResponseSample interface{}

	// IsMutation is true if the action modifies state.
	IsMutation bool
}

// Registry holds all registered WS action definitions.
type Registry struct {
	actions []ActionDef
}

// NewRegistry creates a new empty registry.
func NewRegistry() *Registry {
	return &Registry{}
}

// Register adds an action definition. If RequestType or ResponseType are set,
// it creates zero-value samples for the reflector.
func (r *Registry) Register(def ActionDef) {
	if def.RequestType != nil && def.RequestSample == nil {
		def.RequestSample = reflect.New(def.RequestType).Interface()
	}
	if def.ResponseType != nil && def.ResponseSample == nil {
		def.ResponseSample = reflect.New(def.ResponseType).Interface()
	}
	r.actions = append(r.actions, def)
}

// Actions returns all registered actions sorted by name.
func (r *Registry) Actions() []ActionDef {
	sorted := make([]ActionDef, len(r.actions))
	copy(sorted, r.actions)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Action < sorted[j].Action
	})
	return sorted
}

// EnvelopeTypes holds the actual protocol envelope types for spec generation.
// These must be populated from the api package's exported types (RequestEnvelope,
// ResponseEnvelope, etc.) to ensure the spec matches the wire format exactly.
type EnvelopeTypes struct {
	Request           interface{} // *api.RequestEnvelope
	Response          interface{} // *api.ResponseEnvelope
	Hello             interface{} // *api.HelloEnvelope
	Error             interface{} // *api.ErrorEnvelope
	Event             interface{} // *api.EventEnvelope
	SubscriptionStart interface{} // *api.SubscriptionStartPayload
	SubscriptionStop  interface{} // *api.SubscriptionStopPayload
}

// ActionNames returns sorted action names for the AsyncAPI enum.
func (r *Registry) ActionNames() []string {
	actions := r.Actions()
	names := make([]string, len(actions))
	for i, a := range actions {
		names[i] = a.Action
	}
	return names
}
