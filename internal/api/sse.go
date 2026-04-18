package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/sse"
)

const sseKeepalive = 15 * time.Second

// StreamFunc is the callback signature for SSE streaming handlers
// registered via registerSSE. It receives the huma context (for setting
// custom response headers before streaming starts), the parsed input,
// and a typed Sender.
type StreamFunc[I any] func(hctx huma.Context, input *I, send sse.Sender)

// StringIDMessage is the string-ID variant of sse.Message. Used by streams
// whose cursor is a composite string (e.g. the supervisor global events
// stream, which encodes per-city cursors into a single reconnection token).
type StringIDMessage struct {
	ID   string // written as "id: <string>" on the wire
	Data any    // typed event payload; concrete type must be in the stream's eventTypeMap
}

// StringIDSender is the callback passed to the string-ID stream variant.
// Returning an error terminates the stream cleanly.
type StringIDSender func(msg StringIDMessage) error

// StringIDStreamFunc is the callback signature for SSE streams whose event
// IDs are strings rather than integers. The stream is otherwise identical
// to StreamFunc.
type StringIDStreamFunc[I any] func(hctx huma.Context, input *I, send StringIDSender)

// registerSSE registers an SSE operation like huma's sse.Register but with a
// precheck hook that can return an HTTP error before the response is committed.
//
// Why not use sse.Register directly? sse.Register's callback cannot return
// errors because response headers are already written by the time it runs.
// Some endpoints need to return 503 (service unavailable), 404, etc. based on
// runtime state before streaming starts. This wrapper runs precheck first; if
// it returns an error, that error is returned as the HTTP response. Only on
// success does SSE streaming begin.
//
// The typed eventTypeMap is used for both OpenAPI schema generation and for
// dispatching outgoing messages to the correct event: line. The map value
// type must match the concrete type passed to send.Data() / sse.Message{Data}.
func registerSSE[I any](
	api huma.API,
	op huma.Operation,
	eventTypeMap map[string]any,
	precheck func(context.Context, *I) error,
	stream StreamFunc[I],
) {
	typeToEvent := attachSSEResponseSchema(api, &op, eventTypeMap, huma.TypeInteger, "The event ID.")

	huma.Register(api, op, func(ctx context.Context, input *I) (*huma.StreamResponse, error) {
		if precheck != nil {
			if err := precheck(ctx, input); err != nil {
				return nil, err
			}
		}
		return &huma.StreamResponse{
			Body: func(hctx huma.Context) {
				bw, encoder, flusher := beginSSEStream(hctx)
				send := func(msg sse.Message) error {
					idLine := ""
					if msg.ID > 0 {
						idLine = fmt.Sprintf("id: %d\n", msg.ID)
					}
					return writeSSEFrame(bw, encoder, flusher, typeToEvent, idLine, msg.Data)
				}
				stream(hctx, input, send)
			},
		}, nil
	})
}

// registerSSEStringID is the string-ID sibling of registerSSE. It emits
// `id: <string>` on the wire so browsers echo the exact value back via
// the `Last-Event-ID` header on reconnect — a requirement for streams whose
// cursor cannot be represented as a positive integer.
//
// Huma's built-in sse.Sender uses int IDs (`sse.Message.ID int`), which
// cannot carry composite cursors like the supervisor global stream's
// per-city cursor map. This sibling is otherwise equivalent to registerSSE
// (same precheck semantics, same OpenAPI schema emission).
func registerSSEStringID[I any](
	api huma.API,
	op huma.Operation,
	eventTypeMap map[string]any,
	precheck func(context.Context, *I) error,
	stream StringIDStreamFunc[I],
) {
	typeToEvent := attachSSEResponseSchema(api, &op, eventTypeMap, huma.TypeString, "The event ID (composite cursor).")

	huma.Register(api, op, func(ctx context.Context, input *I) (*huma.StreamResponse, error) {
		if precheck != nil {
			if err := precheck(ctx, input); err != nil {
				return nil, err
			}
		}
		return &huma.StreamResponse{
			Body: func(hctx huma.Context) {
				bw, encoder, flusher := beginSSEStream(hctx)
				send := func(msg StringIDMessage) error {
					idLine := ""
					if msg.ID != "" {
						idLine = fmt.Sprintf("id: %s\n", msg.ID)
					}
					return writeSSEFrame(bw, encoder, flusher, typeToEvent, idLine, msg.Data)
				}
				stream(hctx, input, send)
			},
		}, nil
	})
}

// attachSSEResponseSchema populates op.Responses with the text/event-stream
// media block for the given event map. Returns the reverse-lookup map
// from concrete payload type → SSE event name so the send function can
// write the correct `event:` line at runtime.
func attachSSEResponseSchema(
	api huma.API,
	op *huma.Operation,
	eventTypeMap map[string]any,
	idSchemaType string,
	idSchemaDesc string,
) map[reflect.Type]string {
	if op.Responses == nil {
		op.Responses = map[string]*huma.Response{}
	}
	if op.Responses["200"] == nil {
		op.Responses["200"] = &huma.Response{}
	}
	if op.Responses["200"].Content == nil {
		op.Responses["200"].Content = map[string]*huma.MediaType{}
	}

	typeToEvent := make(map[reflect.Type]string, len(eventTypeMap))
	dataSchemas := make([]*huma.Schema, 0, len(eventTypeMap))
	for k, v := range eventTypeMap {
		vt := derefType(reflect.TypeOf(v))
		typeToEvent[vt] = k
		required := []string{"data"}
		if k != "" && k != "message" {
			required = append(required, "event")
		}
		s := &huma.Schema{
			Title: "Event " + k,
			Type:  huma.TypeObject,
			Properties: map[string]*huma.Schema{
				"id": {
					Type:        idSchemaType,
					Description: idSchemaDesc,
				},
				"event": {
					Type:        huma.TypeString,
					Description: "The event name.",
					Extensions: map[string]any{
						"const": k,
					},
				},
				"data": api.OpenAPI().Components.Schemas.Schema(vt, true, k),
				"retry": {
					Type:        huma.TypeInteger,
					Description: "The retry time in milliseconds.",
				},
			},
			Required: required,
		}
		dataSchemas = append(dataSchemas, s)
	}

	slices.SortFunc(dataSchemas, func(a, b *huma.Schema) int {
		return strings.Compare(a.Title, b.Title)
	})

	op.Responses["200"].Content["text/event-stream"] = &huma.MediaType{
		Schema: &huma.Schema{
			Title:       "Server Sent Events",
			Description: "Each oneOf object represents one possible SSE message.",
			Type:        huma.TypeArray,
			Items: &huma.Schema{
				Extensions: map[string]any{
					"oneOf": dataSchemas,
				},
			},
		},
	}

	return typeToEvent
}

// beginSSEStream sets the standard SSE headers on the huma response and
// returns the underlying writer + JSON encoder + flusher the send
// function will use per frame.
func beginSSEStream(hctx huma.Context) (bw any, encoder *json.Encoder, flusher http.Flusher) {
	hctx.SetHeader("Content-Type", "text/event-stream")
	hctx.SetHeader("Cache-Control", "no-cache")
	hctx.SetHeader("Connection", "keep-alive")
	body := hctx.BodyWriter()
	return body, json.NewEncoder(body), findFlusher(body)
}

// writeSSEFrame emits one SSE frame (id/event/data/blank line) to bw and
// flushes. Returns the first I/O error so the caller can terminate the
// stream on client disconnect.
func writeSSEFrame(
	bw any,
	encoder *json.Encoder,
	flusher http.Flusher,
	typeToEvent map[reflect.Type]string,
	idLine string,
	data any,
) error {
	w, ok := bw.(interface {
		Write([]byte) (int, error)
	})
	if !ok {
		return fmt.Errorf("sse: body writer does not implement io.Writer")
	}
	if idLine != "" {
		if _, err := w.Write([]byte(idLine)); err != nil {
			return err
		}
	}
	event, ok := typeToEvent[derefType(reflect.TypeOf(data))]
	if !ok {
		return fmt.Errorf("unknown event type %T", data)
	}
	if event != "" && event != "message" {
		if _, err := fmt.Fprintf(anyWriter{w}, "event: %s\n", event); err != nil {
			return err
		}
	}
	if _, err := w.Write([]byte("data: ")); err != nil {
		return err
	}
	if err := encoder.Encode(data); err != nil {
		return err
	}
	if _, err := w.Write([]byte("\n")); err != nil {
		return err
	}
	if flusher != nil {
		flusher.Flush()
	}
	return nil
}

// anyWriter adapts an io.Writer-like `any` so fmt.Fprintf can target it.
type anyWriter struct {
	w interface {
		Write([]byte) (int, error)
	}
}

func (a anyWriter) Write(p []byte) (int, error) { return a.w.Write(p) }

// derefType follows pointers until it finds a non-pointer type.
func derefType(t reflect.Type) reflect.Type {
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t
}

// findFlusher unwraps writers to find one that supports http.Flusher.
func findFlusher(w any) http.Flusher {
	type unwrapper interface {
		Unwrap() http.ResponseWriter
	}
	for {
		if f, ok := w.(http.Flusher); ok {
			return f
		}
		if u, ok := w.(unwrapper); ok {
			w = u.Unwrap()
			continue
		}
		return nil
	}
}
