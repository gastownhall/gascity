package api

import (
	"context"
	"fmt"
	"log"
	"math"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/sse"
	"github.com/gastownhall/gascity/internal/api/apierr"
	"github.com/gastownhall/gascity/internal/events"
)

const eventRotateWaitTimeout = 30 * time.Second

// humaHandleEventList is the Huma-typed handler for
// GET /v0/city/{cityName}/events (the supervisor /v0/events list is a
// separate handler on SupervisorMux).
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

	limit := 100
	if input.Limit > 0 {
		limit = input.Limit
	}
	if limit > maxPaginationLimit {
		limit = maxPaginationLimit
	}

	// One order, both paths: seq DESC (newest first). The cursor is a v1
	// sq-kind keyset token carrying the seq of the last row served; the next
	// page is the events strictly below that boundary. The old contract had a
	// window flip — no cursor returned the newest-N while any cursor walked
	// oldest-first from the head — which made walking history coherently
	// impossible. Anything other than a valid sq token is a typed 400.
	beforeSeq, err := parseEventBeforeSeq(input.Cursor)
	if err != nil {
		return nil, err
	}

	index := s.latestIndex()

	// Fetch limit+1 matching events at (first page) or strictly below (cursor
	// page) the boundary, ascending; the extra row is the has-more signal.
	scanFilter := filter
	scanFilter.BeforeSeq = beforeSeq
	evts, scanned, err := fetchEventPageAscending(ep, scanFilter, limit)
	if err != nil {
		return nil, apierr.Internal.Msg(err.Error())
	}

	// evts is ascending; the overfetched row (the oldest) signals more below.
	hasMore := false
	if len(evts) > limit {
		hasMore = true
		evts = evts[len(evts)-limit:]
	}

	// Reverse into seq DESC while projecting to the wire shape.
	wires := make([]WireEvent, 0, len(evts))
	for i := len(evts) - 1; i >= 0; i-- {
		w, ok := toWireEvent(evts[i])
		if !ok {
			continue
		}
		wires = append(wires, w)
	}

	// Total: authoritative for unfiltered reads (the log is append-only and
	// gap-free, so LatestSeq counts every event and stays constant across a
	// walk). Filtered reads report a best-effort count — the matching rows
	// this request's scan could see.
	total := scanned
	if filterIsEmpty(filter) {
		if seq, seqErr := ep.LatestSeq(); seqErr == nil {
			// LatestSeq is a uint64 counter; bound it before the int narrowing so
			// a value past the platform int range can't wrap to a negative or
			// truncated total (CodeQL go/incorrect-integer-conversion).
			if seq > uint64(math.MaxInt) {
				total = math.MaxInt
			} else {
				total = int(seq)
			}
		}
	}

	// Mint the boundary from the page's oldest fetched EVENT, not the last
	// wire row: toWireEvent drops corrupt-payload rows (logged above), and a
	// page whose whole window is corrupt would otherwise return no cursor and
	// silently strand the rest of the walk. Anchoring on evts guarantees
	// exactly `limit` seqs of progress per page regardless of projection
	// failures — corrupt rows are skipped, never re-fetched, never wedge.
	var nextCursor string
	if hasMore {
		nextCursor = encodeKeysetCursor(keysetCursor{
			Kind: cursorKindSeq,
			Seq:  evts[0].Seq,
		})
	}
	return &ListOutput[WireEvent]{
		Index: index,
		Body:  ListBody[WireEvent]{Items: wires, Total: total, NextCursor: nextCursor},
	}, nil
}

// parseEventBeforeSeq decodes the pagination cursor into a keyset seq boundary.
// An empty cursor is the first page (boundary 0 = "no boundary"). Anything that
// is not a valid sq-kind token with a non-zero seq rejects with a typed 400:
// legacy offset tokens, wrong-kind (cb) tokens, and a crafted s:0. Seq 0 is
// never minted (seqs start at 1) and 0 means "first page" here, so echoing an
// s:0 token back would serve a cursor-following client the first page forever.
func parseEventBeforeSeq(cursor string) (uint64, error) {
	if cursor == "" {
		return 0, nil
	}
	c, err := decodeKeysetCursor(cursor)
	if err != nil || c.Kind != cursorKindSeq || c.Seq == 0 {
		return 0, apierr.InvalidCursor.Msg("cursor is not a valid pagination token; re-fetch the first page")
	}
	return c.Seq, nil
}

// fetchEventPageAscending fetches up to limit+1 matching events at or below the
// filter's BeforeSeq boundary in ascending seq order; the extra row is the
// has-more signal. It returns the fetched events and scanned — the best-effort
// count of matching rows the read could see, used as the filtered Total.
//
// ListTail is the fast path: a backward scan of the ACTIVE events.jsonl only,
// never the .gz archives, so its result is trusted ONLY when it yields a full
// limit+1 rows. The active file holds the newest events, so a full tail page
// there IS the newest page below the boundary. Anything short cannot
// distinguish "log exhausted" from "active file exhausted, older matches in
// archives/rotation" and MUST fall through to the full scan — otherwise a
// rotation (or a selective filter) strands the older history behind an unminted
// cursor. The scan uses the in-flight-aware read when the provider offers one
// (listWithInFlight) so a just-rotated segment living only in a .rotating-* file
// is not skipped; the BeforeSeq predicate keeps rotation/archive handling inside
// the one battle-tested sequential reader instead of a bespoke reverse reader.
func fetchEventPageAscending(ep events.Provider, filter events.Filter, limit int) ([]events.Event, int, error) {
	fetch := limit + 1
	if tp, ok := ep.(events.TailProvider); ok {
		tail, err := tp.ListTail(filter, fetch)
		if err != nil {
			return nil, 0, err
		}
		if len(tail) == fetch {
			return tail, limit, nil
		}
	}
	all, err := listWithInFlight(ep, filter)
	if err != nil {
		return nil, 0, err
	}
	scanned := len(all)
	if len(all) > fetch {
		all = all[len(all)-fetch:]
	}
	return all, scanned, nil
}

// listWithInFlight returns all events matching filter, folding in events still
// stranded in an in-flight rotation file when the provider is an
// [events.InFlightProvider]. Plain List reads archives + the active file, so
// during a rotation's compression window it misses the just-rotated .rotating-*
// segment; the in-flight-aware read closes that gap so a descending keyset walk
// cannot skip a whole seq range. Providers with no in-flight window fall back to
// List unchanged.
func listWithInFlight(ep events.Provider, filter events.Filter) ([]events.Event, error) {
	if ip, ok := ep.(events.InFlightProvider); ok {
		return ip.ListInFlight(filter)
	}
	return ep.List(filter)
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
		return 0, false, apierr.InvalidRequest.Msg("invalid since duration: " + err.Error())
	}
	return d, true, nil
}

// humaHandleEventEmit is the Huma-typed handler for POST /v0/events.
// Body validation (Type and Actor required) is enforced by struct tags
// on EventEmitInput.
func (s *Server) humaHandleEventEmit(_ context.Context, input *EventEmitInput) (*EventEmitOutput, error) {
	// Idempotency: append at most once per Idempotency-Key — the log is
	// append-only, so a timed-out retry would otherwise double-emit (and
	// double any projection built over the log). The cached value is just the
	// status constant; the whole point is skipping the duplicate Record.
	status, err := withIdempotency(s.idem, "/v0/events", input.IdempotencyKey, input.Body,
		func() (string, error) {
			ep := s.state.EventProvider()
			if ep == nil {
				return "", apierr.ServiceUnavailable.Msg("events not enabled")
			}
			ep.Record(events.Event{
				Type:    input.Body.Type,
				Actor:   input.Body.Actor,
				Subject: input.Body.Subject,
				Message: input.Body.Message,
			})
			return "recorded", nil
		})
	if err != nil {
		return nil, err
	}

	resp := &EventEmitOutput{}
	resp.Body.Status = status
	return resp, nil
}

// humaHandleEventRotate is the Huma-typed handler for POST
// /v0/city/{cityName}/events/rotate.
func (s *Server) humaHandleEventRotate(ctx context.Context, input *EventRotateInput) (*EventRotateOutput, error) {
	ep := s.state.EventProvider()
	rec, ok := ep.(*events.FileRecorder)
	if !ok {
		return nil, apierr.MethodNotAllowed.Msg(
			fmt.Sprintf("rotation is only supported for the file-backed events provider; current provider is '%s'", eventProviderName(s.state, ep)),
		)
	}

	result, err := rec.ForceRotate()
	if err != nil {
		return nil, apierr.Internal.Msg("rotation failed: " + err.Error())
	}

	compressionStatus := "pending"
	if input.Wait && result.Rotated && result.Done != nil {
		timer := time.NewTimer(eventRotateWaitTimeout)
		defer timer.Stop()
		select {
		case <-result.Done:
			compressionStatus = "complete"
		case <-timer.C:
			compressionStatus = "pending"
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	return &EventRotateOutput{Body: eventRotateResponseFromResult(result, compressionStatus)}, nil
}

func eventProviderName(state State, ep events.Provider) string {
	if state != nil {
		if cfg := state.Config(); cfg != nil {
			if provider := strings.TrimSpace(cfg.Events.Provider); provider != "" {
				return provider
			}
		}
	}
	switch ep.(type) {
	case nil:
		return "none"
	case *events.Fake:
		return "fake"
	case *events.FileRecorder:
		return "file"
	default:
		return fmt.Sprintf("%T", ep)
	}
}

func eventRotateResponseFromResult(result events.RotationResult, compressionStatus string) EventRotateResponse {
	if !result.Rotated {
		return EventRotateResponse{
			Rotated: false,
			Reason:  result.Reason,
		}
	}
	return EventRotateResponse{
		Rotated: true,
		Archive: &EventRotateArchive{
			Path:              result.ArchivePath,
			FirstSeq:          result.FirstSeq,
			LastSeq:           result.LastSeq,
			CompressionStatus: compressionStatus,
		},
		AnchorEvent: &EventRotateAnchor{
			Seq:  result.AnchorSeq,
			Type: events.EventsRotated,
			Ts:   result.AnchorTimestamp.UTC(),
		},
	}
}

// checkEventStream is the precheck for GET /v0/events/stream. It runs before
// the response is committed so it can return proper HTTP errors.
func (s *Server) checkEventStream(_ context.Context, _ *EventStreamInput) error {
	if s.state.EventProvider() == nil {
		return apierr.ServiceUnavailable.Msg("events not enabled")
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
		// Head-start (no resume cursor): stream from now. Fail closed on a
		// LatestSeq error rather than fall through to afterSeq=0, which Watch now
		// treats as "replay the entire retained history" (across archives) — a
		// head-start client must not get a full-history flood. The client can
		// reconnect.
		seq, err := ep.LatestSeq()
		if err != nil {
			log.Printf("api: events-stream: latest seq failed, refusing head-start replay: %v", err)
			return
		}
		afterSeq = seq
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
