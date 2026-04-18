package api

import (
	"context"
	"log"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/sse"
	"github.com/gastownhall/gascity/internal/events"
)

// humaHandleEventList is the Huma-typed handler for GET /v0/events.
func (s *Server) humaHandleEventList(ctx context.Context, input *EventListInput) (*ListOutput[WireEvent], error) {
	bp := input.toBlockingParams()
	if bp.isBlocking() {
		waitForChange(ctx, s.state.EventProvider(), bp)
	}

	ep := s.state.EventProvider()
	if ep == nil {
		return &ListOutput[WireEvent]{
			Index: 0,
			Body:  ListBody[WireEvent]{Items: []WireEvent{}, Total: 0},
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

	wires := make([]WireEvent, 0, len(evts))
	for _, e := range evts {
		wires = append(wires, toWireEvent(e))
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
		page, total, nextCursor := paginate(wires, pp)
		if page == nil {
			page = []WireEvent{}
		}
		return &ListOutput[WireEvent]{
			Index: index,
			Body:  ListBody[WireEvent]{Items: page, Total: total, NextCursor: nextCursor},
		}, nil
	}

	if limit < len(wires) {
		wires = wires[:limit]
	}
	return &ListOutput[WireEvent]{
		Index: index,
		Body:  ListBody[WireEvent]{Items: wires, Total: len(wires)},
	}, nil
}

// humaHandleEventEmit is the Huma-typed handler for POST /v0/events.
// Body validation (Type and Actor required) is enforced by struct tags
// on EventEmitInput.
func (s *Server) humaHandleEventEmit(_ context.Context, input *EventEmitInput) (*EventEmitOutput, error) {
	ep := s.state.EventProvider()
	if ep == nil {
		return nil, huma.Error503ServiceUnavailable("events not enabled")
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
func (s *Server) streamEvents(hctx huma.Context, input *EventStreamInput, send sse.Sender) {
	ctx := hctx.Context()
	ep := s.state.EventProvider()
	afterSeq := input.resolveAfterSeq()
	watcher, err := ep.Watch(ctx, afterSeq)
	if err != nil {
		log.Printf("api: events-stream: Watch failed after_seq=%d: %v", afterSeq, err)
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
				log.Printf("api: events-stream: watcher Next failed: %v", r.err)
				return
			}
			envelope, decodeErr := wireEventFrom(r.event, projectWorkflowEvent(s.state, r.event))
			if decodeErr != nil {
				// Strict registry policy (Principle 7): any event type
				// without a registered payload is a programming error.
				// Skip the emission so the client's connection isn't
				// poisoned with an invalid variant, and log for
				// diagnosis; the registry-coverage test in
				// event_payloads_coverage_test.go prevents this at CI.
				log.Printf("api: events-stream skip %s seq=%d: %v", r.event.Type, r.event.Seq, decodeErr)
				readNext()
				continue
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
