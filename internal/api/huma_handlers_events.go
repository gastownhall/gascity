package api

import (
	"context"
	"log"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
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

// humaHandleEventStream is the Huma-typed handler for GET /v0/events/stream.
// It returns a StreamResponse whose Body callback performs SSE streaming,
// reusing the existing streamProjectedEventsWithWatcher function.
func (s *Server) humaHandleEventStream(_ context.Context, input *EventStreamInput) (*huma.StreamResponse, error) {
	ep := s.state.EventProvider()
	if ep == nil {
		return nil, huma.Error503ServiceUnavailable("events not enabled")
	}

	afterSeq := input.resolveAfterSeq()

	// Create watcher before committing 200 — allows returning 503 on failure.
	watcher, err := ep.Watch(context.Background(), afterSeq)
	if err != nil {
		return nil, huma.Error503ServiceUnavailable("failed to start event watcher: " + err.Error())
	}

	return &huma.StreamResponse{
		Body: func(ctx huma.Context) {
			ctx.SetHeader("Content-Type", "text/event-stream")
			ctx.SetHeader("Cache-Control", "no-cache")
			ctx.SetHeader("Connection", "keep-alive")

			w := ctx.BodyWriter()
			rw, ok := w.(http.ResponseWriter)
			if !ok {
				log.Printf("api: event stream writer does not implement http.ResponseWriter")
				watcher.Close() //nolint:errcheck
				return
			}

			streamProjectedEventsWithWatcher(ctx.Context(), rw, watcher, s.state)
		},
	}, nil
}
