package api

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/sse"
)

const sseKeepalive = 15 * time.Second

// cancelOnSendError wraps an sse.Sender so that on the first send
// failure it cancels the supplied context and subsequent send calls
// short-circuit to the original error. Stream loops that poll file
// watchers / tmux panes / session cursors then exit promptly via
// ctx.Done() instead of continuing to drain events onto a dead client.
//
// Returning a wrapper (rather than threading errors back through every
// closure) keeps the call-site change minimal: handlers call send() as
// before but the first write failure tears the stream down.
func cancelOnSendError(send sse.Sender, cancel context.CancelFunc) sse.Sender {
	var firstErr error
	return func(msg sse.Message) error {
		if firstErr != nil {
			return firstErr
		}
		if err := send(msg); err != nil {
			firstErr = err
			cancel()
			return err
		}
		return nil
	}
}

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
				// Derive a cancelable context from the request context and
				// plumb it into hctx so stream loops checking hctx.Context()
				// exit promptly on send-error.
				reqCtx, cancel := context.WithCancel(hctx.Context())
				defer cancel()
				hctx = hctxWithCtx(reqCtx, hctx)

				bw, encoder, flusher := beginSSEStream(hctx)
				rawSend := func(msg sse.Message) error {
					idLine := ""
					if msg.ID > 0 {
						idLine = fmt.Sprintf("id: %d\n", msg.ID)
					}
					return writeSSEFrame(bw, encoder, flusher, typeToEvent, idLine, msg.Data)
				}
				send := cancelOnSendError(rawSend, cancel)
				stream(hctx, input, send)
			},
		}, nil
	})
}

// writeSSE writes a single SSE frame and flushes.
func writeSSE(w http.ResponseWriter, eventType string, id any, data []byte) {
	fmt.Fprintf(w, "event: %s\nid: %v\ndata: %s\n\n", eventType, id, data) //nolint:errcheck
	if err := http.NewResponseController(w).Flush(); err != nil {
		_ = err
	}
}

// writeSSEComment emits a keepalive comment frame and flushes.
func writeSSEComment(w http.ResponseWriter) {
	fmt.Fprintf(w, ": keepalive\n\n") //nolint:errcheck
	if err := http.NewResponseController(w).Flush(); err != nil {
		_ = err
	}
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
				reqCtx, cancel := context.WithCancel(hctx.Context())
				defer cancel()
				hctx = hctxWithCtx(reqCtx, hctx)

				bw, encoder, flusher := beginSSEStream(hctx)
				rawSend := func(msg StringIDMessage) error {
					idLine := ""
					if msg.ID != "" {
						idLine = fmt.Sprintf("id: %s\n", msg.ID)
					}
					return writeSSEFrame(bw, encoder, flusher, typeToEvent, idLine, msg.Data)
				}
				send := stringIDCancelOnSendError(rawSend, cancel)
				stream(hctx, input, send)
			},
		}, nil
	})
}

// stringIDCancelOnSendError is the StringIDSender analog of cancelOnSendError.
// On first send failure it cancels the supplied context; subsequent calls
// short-circuit to the cached error.
func stringIDCancelOnSendError(send StringIDSender, cancel context.CancelFunc) StringIDSender {
	var firstErr error
	return func(msg StringIDMessage) error {
		if firstErr != nil {
			return firstErr
		}
		if err := send(msg); err != nil {
			firstErr = err
			cancel()
			return err
		}
		return nil
	}
}

// hctxWithCtx returns a huma.Context that reports the supplied ctx from
// its Context() method. Used by SSE registration to plumb a cancelable
// context into stream loops that poll hctx.Context().Done().
//
// Note: we cannot embed huma.Context because the interface is literally
// named Context, which collides with our override method Context(). The
// override instead delegates every method explicitly.
func hctxWithCtx(ctx context.Context, hctx huma.Context) huma.Context {
	return &hctxOverride{inner: hctx, ctx: ctx}
}

// hctxOverride wraps a huma.Context to replace only the Context() method.
// All other methods delegate to the inner huma.Context.
type hctxOverride struct {
	inner huma.Context
	ctx   context.Context
}

func (h *hctxOverride) Operation() *huma.Operation { return h.inner.Operation() }
func (h *hctxOverride) Context() context.Context   { return h.ctx }
func (h *hctxOverride) Method() string             { return h.inner.Method() }
func (h *hctxOverride) Host() string               { return h.inner.Host() }
func (h *hctxOverride) RemoteAddr() string         { return h.inner.RemoteAddr() }
func (h *hctxOverride) URL() url.URL               { return h.inner.URL() }
func (h *hctxOverride) Param(name string) string   { return h.inner.Param(name) }
func (h *hctxOverride) Query(name string) string   { return h.inner.Query(name) }
func (h *hctxOverride) Header(name string) string  { return h.inner.Header(name) }
func (h *hctxOverride) EachHeader(cb func(name, value string)) {
	h.inner.EachHeader(cb)
}
func (h *hctxOverride) BodyReader() io.Reader { return h.inner.BodyReader() }
func (h *hctxOverride) GetMultipartForm() (*multipart.Form, error) {
	return h.inner.GetMultipartForm()
}

func (h *hctxOverride) SetReadDeadline(deadline time.Time) error {
	return h.inner.SetReadDeadline(deadline)
}
func (h *hctxOverride) SetStatus(code int)              { h.inner.SetStatus(code) }
func (h *hctxOverride) Status() int                     { return h.inner.Status() }
func (h *hctxOverride) AppendHeader(name, value string) { h.inner.AppendHeader(name, value) }
func (h *hctxOverride) SetHeader(name, value string)    { h.inner.SetHeader(name, value) }
func (h *hctxOverride) BodyWriter() io.Writer           { return h.inner.BodyWriter() }
func (h *hctxOverride) TLS() *tls.ConnectionState       { return h.inner.TLS() }
func (h *hctxOverride) Version() huma.ProtoVersion      { return h.inner.Version() }

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
