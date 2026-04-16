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
	stream func(ctx context.Context, input *I, send sse.Sender),
) {
	// Set up the OpenAPI response schema for SSE events.
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
					Type:        huma.TypeInteger,
					Description: "The event ID.",
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

	slices.SortFunc(dataSchemas, func(b, c *huma.Schema) int {
		return strings.Compare(b.Title, c.Title)
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

	huma.Register(api, op, func(ctx context.Context, input *I) (*huma.StreamResponse, error) {
		if precheck != nil {
			if err := precheck(ctx, input); err != nil {
				return nil, err
			}
		}
		return &huma.StreamResponse{
			Body: func(hctx huma.Context) {
				hctx.SetHeader("Content-Type", "text/event-stream")
				hctx.SetHeader("Cache-Control", "no-cache")
				hctx.SetHeader("Connection", "keep-alive")

				bw := hctx.BodyWriter()
				encoder := json.NewEncoder(bw)
				flusher := findFlusher(bw)

				send := func(msg sse.Message) error {
					if msg.ID > 0 {
						fmt.Fprintf(bw, "id: %d\n", msg.ID)
					}
					event, ok := typeToEvent[derefType(reflect.TypeOf(msg.Data))]
					if !ok {
						return fmt.Errorf("unknown event type %T", msg.Data)
					}
					if event != "" && event != "message" {
						fmt.Fprintf(bw, "event: %s\n", event)
					}
					if _, err := bw.Write([]byte("data: ")); err != nil {
						return err
					}
					if err := encoder.Encode(msg.Data); err != nil {
						return err
					}
					if _, err := bw.Write([]byte("\n")); err != nil {
						return err
					}
					if flusher != nil {
						flusher.Flush()
					}
					return nil
				}

				stream(hctx.Context(), input, send)
			},
		}, nil
	})
}

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

// writeSSE writes a single SSE event to w and flushes.
//
// Deprecated: Use registerSSE or sse.Register instead. This helper is kept
// only for the global events proxy in supervisor.go which still needs raw
// ResponseWriter access. Migrating that path to registerSSE is tracked under
// Phase 2 Fix 1.
func writeSSE(w http.ResponseWriter, eventType string, id uint64, data []byte) {
	fmt.Fprintf(w, "event: %s\nid: %d\ndata: %s\n\n", eventType, id, data) //nolint:errcheck
	if err := http.NewResponseController(w).Flush(); err != nil {
		_ = err
	}
}

// writeSSEWithStringID writes an SSE event with a non-numeric ID.
//
// Deprecated: Same as writeSSE.
func writeSSEWithStringID(w http.ResponseWriter, eventType, id string, data []byte) {
	fmt.Fprintf(w, "event: %s\nid: %s\ndata: %s\n\n", eventType, id, data) //nolint:errcheck
	if err := http.NewResponseController(w).Flush(); err != nil {
		_ = err
	}
}

// writeSSEComment writes a keepalive comment line and flushes.
//
// Deprecated: Same as writeSSE. registerSSE handles keepalives via typed
// heartbeat events instead of raw comment lines.
func writeSSEComment(w http.ResponseWriter) {
	fmt.Fprintf(w, ": keepalive\n\n") //nolint:errcheck
	if err := http.NewResponseController(w).Flush(); err != nil {
		_ = err
	}
}
