package api

import (
	"encoding/json"
	"reflect"

	"github.com/danielgtaylor/huma/v2"
	"github.com/gastownhall/gascity/internal/runtime"
)

// Session transcript wire types.
//
// Gas City forwards provider-native transcript frames with full
// fidelity; the producing provider is identified per-envelope via
// the Provider field (see SessionStreamRawMessageEvent,
// SessionStreamMessageEvent, sessionTranscriptGetResponse), and the
// frame JSON is emitted verbatim. Consumers parse frames using
// provider-specific logic on their side, keyed by the Provider
// identifier. We do not publish typed per-provider frame schemas
// because the frame shapes are authored outside our source tree —
// providers can change their frame shapes and Gas City's spec would
// silently lie until regenerated. Honest opacity is the right
// design.

// SessionRawMessageFrame is the wire type for one provider-native
// transcript frame. The Go level carries the original JSON bytes plus
// an optional pre-decoded Value for callers that already have the
// decoded form in hand. When Raw is non-nil we emit it verbatim —
// preserving byte-identity, map-key order, and int64 precision above
// 2^53 which would otherwise be lost via a float64 round-trip.
//
// At the OpenAPI level the schema is intentionally unconstrained
// ("any JSON value"), because the provider owns the frame shape.
type SessionRawMessageFrame struct {
	// Raw is the original JSON bytes, emitted verbatim on marshal.
	// Populated by the raw-path session stream handlers so tool-call
	// IDs, high-precision timestamps, and whitespace-sensitive fields
	// survive the round-trip.
	Raw json.RawMessage
	// Value is an optional pre-decoded frame. Used when the caller
	// only has the decoded Go form; marshals via json.Marshal(Value).
	// If Raw is non-empty it wins.
	Value any
}

// wrapRawFrameBytes wraps each provider-native frame's raw JSON bytes
// in a SessionRawMessageFrame so the wire shape is byte-identical to
// what the provider wrote. Prefer this over wrapRawFrames when the
// caller has entry.Raw available.
func wrapRawFrameBytes(values []json.RawMessage) []SessionRawMessageFrame {
	out := make([]SessionRawMessageFrame, len(values))
	for i, v := range values {
		out[i] = SessionRawMessageFrame{Raw: append(json.RawMessage(nil), v...)}
	}
	return out
}

// MarshalJSON emits Raw verbatim if set; otherwise json.Marshal(Value).
// Emits `null` when both are empty.
func (f SessionRawMessageFrame) MarshalJSON() ([]byte, error) {
	if len(f.Raw) > 0 {
		return f.Raw, nil
	}
	if f.Value == nil {
		return []byte("null"), nil
	}
	return json.Marshal(f.Value)
}

// UnmarshalJSON stashes the raw JSON into Raw so round-tripping
// through this type never alters byte-identity or precision.
func (f *SessionRawMessageFrame) UnmarshalJSON(data []byte) error {
	f.Raw = append(json.RawMessage(nil), data...)
	f.Value = nil
	return nil
}

// Schema registers and references the SessionRawMessageFrame schema.
// Implements huma.SchemaProvider.
//
// The published schema declares no type and no properties; OpenAPI
// 3.1 treats that as "any JSON value," which makes generated clients
// decode the field as raw JSON. Consumers narrow per-provider on
// their side using the Provider identifier on the enclosing envelope.
func (SessionRawMessageFrame) Schema(r huma.Registry) *huma.Schema {
	const name = "SessionRawMessageFrame"
	if _, ok := r.Map()[name]; !ok {
		r.Map()[name] = &huma.Schema{
			Title:       "Session raw transcript frame",
			Description: "Provider-native transcript frame. Gas City forwards the exact JSON the provider wrote to its session log, so the shape is provider-specific and can be any JSON value. The producing provider is identified by the Provider field on the enclosing envelope; consumers dispatch per-provider frame parsing keyed by that identifier.",
		}
	}
	return &huma.Schema{Ref: schemaRefPrefix + name}
}

// SessionStreamCommonEvent is a documentation-only union over the
// lifecycle/state events emitted on the session SSE stream
// (SessionActivityEvent, runtime.PendingInteraction, HeartbeatEvent).
// The wire shape of each variant is unchanged; this type exists purely
// to give downstream consumers a single schema name that groups the
// non-message events the stream can emit.
type SessionStreamCommonEvent struct{}

// Schema registers and references the SessionStreamCommonEvent union
// schema. Implements huma.SchemaProvider.
func (SessionStreamCommonEvent) Schema(r huma.Registry) *huma.Schema {
	const name = "SessionStreamCommonEvent"
	if _, ok := r.Map()[name]; !ok {
		variants := []reflect.Type{
			reflect.TypeOf(SessionActivityEvent{}),
			reflect.TypeOf(runtime.PendingInteraction{}),
			reflect.TypeOf(HeartbeatEvent{}),
		}
		oneOf := make([]*huma.Schema, len(variants))
		for i, t := range variants {
			oneOf[i] = r.Schema(t, true, t.Name())
		}
		r.Map()[name] = &huma.Schema{
			Title:       "Session stream lifecycle event",
			Description: "Non-message events emitted on the session SSE stream: activity transitions, pending interactions, and keepalive heartbeats. The concrete variant is identified by the SSE event name.",
			OneOf:       oneOf,
		}
	}
	return &huma.Schema{Ref: schemaRefPrefix + name}
}
