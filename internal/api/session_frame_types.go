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
// transcript frame. The Go level carries an arbitrary JSON value
// and marshals verbatim. At the OpenAPI level the schema is
// intentionally unconstrained ("any JSON value").
type SessionRawMessageFrame struct {
	// Value is the provider-native frame. Marshaled verbatim; the schema
	// is declared via Schema(r).
	Value any
}

// wrapRawFrames wraps each provider-native frame value in a
// SessionRawMessageFrame so the wire shape is preserved while the Go
// slice type carries the documented schema.
func wrapRawFrames(values []any) []SessionRawMessageFrame {
	out := make([]SessionRawMessageFrame, len(values))
	for i, v := range values {
		out[i] = SessionRawMessageFrame{Value: v}
	}
	return out
}

// MarshalJSON emits the underlying Value so the wire shape matches what
// the provider wrote to its session log.
func (f SessionRawMessageFrame) MarshalJSON() ([]byte, error) {
	if f.Value == nil {
		return []byte("null"), nil
	}
	return json.Marshal(f.Value)
}

// UnmarshalJSON stashes the raw JSON into Value so round-tripping
// through this type does not alter any fields.
func (f *SessionRawMessageFrame) UnmarshalJSON(data []byte) error {
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	f.Value = v
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
