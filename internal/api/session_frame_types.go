package api

import (
	"encoding/json"
	"reflect"

	"github.com/danielgtaylor/huma/v2"
	"github.com/gastownhall/gascity/internal/runtime"
)

// Provider-native session transcript frame types. These describe the JSON
// shapes streamed to clients on the raw session SSE endpoint via the
// Messages field on SessionStreamRawMessageEvent. The supervisor forwards
// whatever the provider wrote to its session log, so the wire shape is
// determined by the provider, not by the supervisor. These structs mirror
// the shapes already recognized by internal/sessionlog so consumers of
// openapi.json can code against a named schema instead of "arbitrary JSON".

// CodexRawEntry is the outer wrapper of one line in a Codex rollout log.
type CodexRawEntry struct {
	Timestamp string          `json:"timestamp"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}

// CodexEventMsg is the payload of a Codex `event_msg` entry.
type CodexEventMsg struct {
	Type    string `json:"type" doc:"user_message, agent_message, agent_reasoning, token_count."`
	Message string `json:"message,omitempty" doc:"Message text for user_message/agent_message entries."`
	Text    string `json:"text,omitempty" doc:"Text for agent_reasoning entries."`
}

// CodexResponseItem is one item inside a Codex `response_item` entry.
type CodexResponseItem struct {
	Type    string              `json:"type" doc:"message, reasoning, function_call, function_call_output."`
	Role    string              `json:"role,omitempty"`
	Content []CodexTextContent  `json:"content,omitempty"`
	Summary []CodexTextContent  `json:"summary,omitempty"`
	CallID  string              `json:"call_id,omitempty"`
	Name    string              `json:"name,omitempty"`
	Output  string              `json:"output,omitempty"`
}

// CodexTextContent is a text fragment inside a Codex response item.
type CodexTextContent struct {
	Text string `json:"text"`
}

// GeminiThought is a Gemini "thought" transcript entry.
type GeminiThought struct {
	Subject     string `json:"subject"`
	Description string `json:"description"`
}

// GeminiToolCall is a Gemini tool-call transcript entry.
type GeminiToolCall struct {
	ID     string                       `json:"id"`
	Name   string                       `json:"name"`
	Args   json.RawMessage              `json:"args"`
	Result []GeminiToolCallResultEntry  `json:"result"`
}

// GeminiToolCallResultEntry is one entry in a Gemini tool-call result list.
type GeminiToolCallResultEntry struct {
	FunctionResponse GeminiFunctionResponse `json:"functionResponse"`
}

// GeminiFunctionResponse is the per-call function-response payload in a
// Gemini tool-call result entry.
type GeminiFunctionResponse struct {
	ID       string                        `json:"id"`
	Response GeminiFunctionResponseOutput  `json:"response"`
}

// GeminiFunctionResponseOutput wraps the textual output of a Gemini
// function-response payload.
type GeminiFunctionResponseOutput struct {
	Output string `json:"output"`
}

// SessionRawMessageFrame is the discriminated union surfaced in the
// Messages field of a raw session transcript event. In practice the
// supervisor forwards whatever JSON the provider wrote to its log, so
// at the Go level a frame carries an arbitrary JSON value and marshals
// verbatim. At the OpenAPI level the schema is a oneOf over the known
// provider frame shapes so consumers can generate typed clients.
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

// Schema registers and references the SessionRawMessageFrame schema.
// Implements huma.SchemaProvider.
//
// The supervisor forwards whatever JSON the provider wrote to its
// session log — objects, arrays, nulls, primitives are all possible.
// The published schema therefore declares no type and no properties;
// OpenAPI 3.1 treats that as "any JSON value," which makes generated
// clients decode the field as a raw JSON value rather than a
// narrowly-typed map. Consumers that want to narrow further can import
// the per-provider frame types (CodexRawEntry, CodexEventMsg, etc.)
// directly — they live as exported Go types in this package and in
// internal/sessionlog.
func (SessionRawMessageFrame) Schema(r huma.Registry) *huma.Schema {
	const name = "SessionRawMessageFrame"
	if _, ok := r.Map()[name]; !ok {
		r.Map()[name] = &huma.Schema{
			Title:       "Session raw transcript frame",
			Description: "Provider-native transcript frame. The supervisor forwards the exact JSON the provider wrote to its session log, so the shape is provider-specific and can be any JSON value (object, array, null, primitive).",
		}
	}
	return &huma.Schema{Ref: schemaRefPrefix + name}
}
