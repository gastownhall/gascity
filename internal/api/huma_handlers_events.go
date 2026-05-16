package api

import (
	"context"
	"log"
	"strings"
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
	if d, ok, err := parseEventSince(input.Since); err != nil {
		return nil, err
	} else if ok {
		filter.Since = time.Now().Add(-d)
	}

	// Resolve the effective limit first so we can decide between the
	// bounded tail path (fast) and the full-scan pagination path (slow
	// but needed when the caller walks offsets with cursors).
	limit := 100
	if input.Limit > 0 {
		limit = input.Limit
	}
	if limit > maxPaginationLimit {
		limit = maxPaginationLimit
	}

	index := s.latestIndex()

	// Fast path: no cursor → most clients just want the N newest events.
	// Use ListTail when the provider supports it so we don't parse the
	// entire events.jsonl (which is O(file size), ~4s on 100 MB) just to
	// throw away all but the tail. Same pattern as the supervisor
	// handler's optimizedTail branch.
	if input.Cursor == "" {
		if tp, ok := ep.(events.TailProvider); ok {
			evts, err := tp.ListTail(filter, limit)
			if err != nil {
				return nil, huma.Error500InternalServerError(err.Error())
			}
			wires := toWireEvents(evts)
			// Total is best-effort here: when the caller narrowed with
			// Type/Actor/Since we cannot cheaply compute the full match
			// count, so report the returned slice length. When the
			// filter is empty, LatestSeq is authoritative since the log
			// is append-only and gap-free.
			total := len(wires)
			if filterIsEmpty(filter) {
				if seq, seqErr := ep.LatestSeq(); seqErr == nil {
					total = int(seq)
				}
			}
			return &ListOutput[WireEvent]{
				Index: index,
				Body:  ListBody[WireEvent]{Items: wires, Total: total},
			}, nil
		}
	}

	// Cursor pagination (or provider without TailProvider): we still
	// need the full materialized list to honor offset-based cursors.
	// Cap the scan at (offset+limit) matching events so this path is
	// bounded by caller pagination depth rather than file size.
	scanLimit := limit
	if input.Cursor != "" {
		scanLimit = decodeCursor(input.Cursor) + limit
	}
	filter.Limit = scanLimit

	evts, err := ep.List(filter)
	if err != nil {
		return nil, huma.Error500InternalServerError(err.Error())
	}
	wires := toWireEvents(evts)

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

	// Capture the full match count BEFORE truncating so clients can tell
	// how many items match vs. fit the page.
	total := len(wires)
	if limit < len(wires) {
		wires = wires[:limit]
	}
	return &ListOutput[WireEvent]{
		Index: index,
		Body:  ListBody[WireEvent]{Items: wires, Total: total},
	}, nil
}

func toWireEvents(evts []events.Event) []WireEvent {
	wires := make([]WireEvent, 0, len(evts))
	for _, e := range evts {
		w, ok := toWireEvent(e)
		if !ok {
			continue
		}
		wires = append(wires, w)
	}
	return wires
}

func filterIsEmpty(f events.Filter) bool {
	return f.Type == "" && f.Actor == "" && f.Subject == "" &&
		f.Since.IsZero() && f.Until.IsZero() && f.AfterSeq == 0
}

func parseEventSince(value string) (time.Duration, bool, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false, nil
	}
	d, err := time.ParseDuration(value)
	if err != nil {
		return 0, false, huma.Error400BadRequest("invalid since duration: " + err.Error())
	}
	return d, true, nil
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
// creates a watcher and streams events until the context is canceled.
// Heartbeat events are sent every 15s to keep the connection alive.
func (s *Server) streamEvents(hctx huma.Context, input *EventStreamInput, send sse.Sender) {
	ctx := hctx.Context()
	ep := s.state.EventProvider()
	afterSeq := input.resolveAfterSeq()
	if strings.TrimSpace(input.LastEventID) == "" && strings.TrimSpace(input.AfterSeq) == "" {
		seq, err := ep.LatestSeq()
		if err != nil {
			log.Printf("api: events-stream: latest seq failed: %v", err)
		} else {
			afterSeq = seq
		}
	}
	watcher, err := ep.Watch(ctx, afterSeq)
	if err != nil {
		log.Printf("api: events-stream: Watch failed after_seq=%d: %v", afterSeq, err)
		return
	}
	defer watcher.Close() //nolint:errcheck
	flushSSEHeaders(hctx)

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
