package api

import (
	"context"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/sse"
	"github.com/gastownhall/gascity/internal/events"
)

// humaHandleEventList is the Huma-typed handler for GET /v0/events.
func (s *Server) humaHandleEventList(ctx context.Context, input *EventListInput) (*ListOutput[events.Event], error) {
	bp := input.toBlockingParams()
	if bp.isBlocking() {
		waitForChange(ctx, s.state.EventProvider(), bp)
	}

	ep := s.state.EventProvider()
	if ep == nil {
		return &ListOutput[events.Event]{
			Index: 0,
			Body:  ListBody[events.Event]{Items: []events.Event{}, Total: 0},
		}, nil
	}

	filter := events.Filter{
		Type:  input.Type,
		Actor: input.Actor,
	}
	if input.Since != "" {
		if d, err := time.ParseDuration(input.Since); err == nil {
			filter.Since = time.Now().Add(-d)
		}
	}

	evts, err := ep.List(filter)
	if err != nil {
		return nil, huma.Error500InternalServerError(err.Error())
	}
	if evts == nil {
		evts = []events.Event{}
	}

	index := s.latestIndex()

	// Pagination support.
	limit := 100
	if input.Limit > 0 {
		limit = input.Limit
	}
	if input.Cursor != "" {
		pp := pageParams{
			Offset: decodeCursor(input.Cursor),
			Limit:  limit,
		}
		page, total, nextCursor := paginate(evts, pp)
		if page == nil {
			page = []events.Event{}
		}
		return &ListOutput[events.Event]{
			Index: index,
			Body:  ListBody[events.Event]{Items: page, Total: total, NextCursor: nextCursor},
		}, nil
	}

	if limit < len(evts) {
		evts = evts[:limit]
	}
	return &ListOutput[events.Event]{
		Index: index,
		Body:  ListBody[events.Event]{Items: evts, Total: len(evts)},
	}, nil
}

// humaHandleEventEmit is the Huma-typed handler for POST /v0/events.
func (s *Server) humaHandleEventEmit(_ context.Context, input *EventEmitInput) (*EventEmitOutput, error) {
	ep := s.state.EventProvider()
	if ep == nil {
		return nil, huma.Error503ServiceUnavailable("events not enabled")
	}

	if input.Body.Type == "" {
		return nil, huma.Error400BadRequest("type is required")
	}
	if input.Body.Actor == "" {
		return nil, huma.Error400BadRequest("actor is required")
	}

	ep.Record(events.Event{
		Type:    input.Body.Type,
		Actor:   input.Body.Actor,
		Subject: input.Body.Subject,
		Message: input.Body.Message,
	})

	resp := &EventEmitOutput{}
	resp.Body.Status = "recorded"
	return resp, nil
}

// registerEventStreamRoute wires up GET /v0/events/stream via registerSSE so
// the SSE event schema is documented in the OpenAPI spec.
//
// The typed event map declares two event types that clients may receive:
//   - "event": an eventStreamEnvelope with the actual event plus optional
//     workflow projection
//   - "heartbeat": a periodic keepalive event used to hold the connection
//     open through intermediate proxies
func (s *Server) registerEventStreamRoute() {
	registerSSE(s.humaAPI, huma.Operation{
		OperationID: "stream-events",
		Method:      http.MethodGet,
		Path:        "/v0/events/stream",
		Summary:     "Stream city events in real time",
		Description: "Server-Sent Events stream of city events with optional workflow projections. " +
			"Supports reconnection via Last-Event-ID header or after_seq query param.",
	}, map[string]any{
		"event":     eventStreamEnvelope{},
		"heartbeat": HeartbeatEvent{},
	}, s.checkEventStream, s.streamEvents)
}

// checkEventStream is the precheck for GET /v0/events/stream. It runs before
// the response is committed so it can return proper HTTP errors.
func (s *Server) checkEventStream(_ context.Context, _ *EventStreamInput) error {
	if s.state.EventProvider() == nil {
		return huma.Error503ServiceUnavailable("events not enabled")
	}
	return nil
}

// streamEvents is the SSE streaming callback for GET /v0/events/stream. The
// precheck has already verified the event provider exists. This function
// creates a watcher and streams events until the context is cancelled.
// Heartbeat events are sent every 15s to keep the connection alive.
func (s *Server) streamEvents(ctx context.Context, input *EventStreamInput, send sse.Sender) {
	ep := s.state.EventProvider()
	afterSeq := input.resolveAfterSeq()
	watcher, err := ep.Watch(ctx, afterSeq)
	if err != nil {
		return
	}
	defer watcher.Close() //nolint:errcheck

	keepalive := time.NewTicker(sseKeepalive)
	defer keepalive.Stop()

	type result struct {
		event events.Event
		err   error
	}
	ch := make(chan result, 1)

	readNext := func() {
		go func() {
			e, err := watcher.Next()
			select {
			case ch <- result{event: e, err: err}:
			case <-ctx.Done():
			}
		}()
	}

	readNext()

	for {
		select {
		case <-ctx.Done():
			return
		case r := <-ch:
			if r.err != nil {
				return
			}
			envelope := eventStreamEnvelope{
				Event:    r.event,
				Workflow: projectWorkflowEvent(s.state, r.event),
			}
			if err := send(sse.Message{ID: int(r.event.Seq), Data: envelope}); err != nil {
				return
			}
			readNext()
		case t := <-keepalive.C:
			if err := send.Data(HeartbeatEvent{Timestamp: t.UTC().Format(time.RFC3339)}); err != nil {
				return
			}
		}
	}
}
