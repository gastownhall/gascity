package api

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/gastownhall/gascity/internal/api/apierr"
	"github.com/gastownhall/gascity/internal/events"
)

// Request outcome states reported by GET /v0/city/{cityName}/request/{requestId}.
const (
	RequestStatePending        = "pending"
	RequestStateSucceeded      = "succeeded"
	RequestStateFailed         = "failed"
	RequestStateOutcomeUnknown = "outcome_unknown"
)

// sessionRequestResultOperations maps each async session success event type to
// the operation it completes. These, request.failed and request.accepted are
// the only event types the outcome lookup reads.
var sessionRequestResultOperations = map[string]string{
	events.RequestResultSessionCreate:  RequestOperationSessionCreate,
	events.RequestResultSessionMessage: RequestOperationSessionMessage,
	events.RequestResultSessionSubmit:  RequestOperationSessionSubmit,
}

func isSessionRequestOperation(op string) bool {
	switch op {
	case RequestOperationSessionCreate, RequestOperationSessionMessage, RequestOperationSessionSubmit:
		return true
	}
	return false
}

// asyncRequestKey names one accepted async request. Request IDs are random,
// but the city path keeps a lookup on one city from reporting another city's
// request.
type asyncRequestKey struct {
	cityPath  string
	requestID string
}

// asyncRequestsInFlight holds every async session request whose goroutine is
// still running in this process, mapped to its operation. It is process-wide,
// not per Server, because a per-city Server is rebuilt when city state changes
// while the goroutines it started keep running. A restart empties it, which is
// how the lookup tells a request still being worked on from one whose owner
// exited before recording an outcome.
var asyncRequestsInFlight sync.Map

// acceptAsyncRequest admits one async session request. It mints the request_id,
// captures the event cursor the 202 returns, and records the request.accepted
// marker in the city event log, so a client holding the request_id can read its
// outcome from GET /request/{requestId} even if it never sees the terminal
// event. A request whose acceptance cannot be recorded is refused with 503
// before any work starts: a 202 must never hand out an id nothing can resolve.
//
// The caller runs finish when its goroutine exits, after the terminal event has
// been emitted.
func (s *Server) acceptAsyncRequest(operation string) (requestID, eventCursor string, finish func(), err error) {
	ep, err := s.asyncRequestEventLog()
	if err != nil {
		return "", "", nil, err
	}
	requestID, err = newRequestID()
	if err != nil {
		return "", "", nil, apierr.Internal.Msg(err.Error())
	}
	// Capture the cursor first: resuming from it then replays the
	// request.accepted marker and every event the request produces.
	eventCursor, err = s.currentCityEventCursor()
	if err != nil {
		return "", "", nil, apierr.Internal.Msg(err.Error())
	}
	finish, err = s.recordAsyncAcceptance(ep, requestID, operation)
	if err != nil {
		return "", "", nil, err
	}
	return requestID, eventCursor, finish, nil
}

// asyncRequestEventLog is the city event log an async session request's
// acceptance is recorded in. A city without one refuses the request with 503
// before any work starts.
func (s *Server) asyncRequestEventLog() (events.Recorder, error) {
	ep := s.state.EventProvider()
	if ep == nil {
		return nil, apierr.ServiceUnavailable.Msg("events not enabled: the outcome of an async request could not be recorded")
	}
	return ep, nil
}

// recordAsyncAcceptance registers requestID in flight and records its
// request.accepted marker in ep. A marker the log refuses refuses the request
// with 503. /submit and /messages, whose request_id is their op's id when the
// operation journal records them, record it once admitted (acceptMessageOp).
//
// The caller runs finish when its goroutine exits, after the terminal event has
// been emitted.
func (s *Server) recordAsyncAcceptance(ep events.Recorder, requestID, operation string) (finish func(), err error) {
	key := asyncRequestKey{cityPath: s.state.CityPath(), requestID: requestID}
	// Register before recording: a lookup that finds the marker must also find
	// the request in flight while its goroutine runs.
	asyncRequestsInFlight.Store(key, operation)
	if err := recordRequestAccepted(ep, requestID, operation); err != nil {
		asyncRequestsInFlight.Delete(key)
		return nil, apierr.ServiceUnavailable.Msg(fmt.Sprintf("recording acceptance of %s request: %v", operation, err))
	}
	return func() { asyncRequestsInFlight.Delete(key) }, nil
}

// recordRequestAccepted appends the request.accepted marker, acknowledged when
// the provider can acknowledge appends. A provider that cannot (an exec script)
// gets a best-effort Record; while the request runs, the in-flight registry
// still answers pending without the marker.
func recordRequestAccepted(ep events.Recorder, requestID, operation string) error {
	raw, err := json.Marshal(RequestAcceptedPayload{RequestID: requestID, Operation: operation})
	if err != nil {
		return fmt.Errorf("marshal %s: %w", events.RequestAccepted, err)
	}
	e := events.Event{Type: events.RequestAccepted, Actor: "api", Payload: raw}
	if ack, ok := ep.(events.AckRecorder); ok {
		return ack.RecordAck(e)
	}
	ep.Record(e)
	return nil
}

// RequestOutcomeInput is the input for GET /v0/city/{cityName}/request/{requestId}.
type RequestOutcomeInput struct {
	CityScope
	RequestID string `path:"requestId" doc:"The request_id from an async session request's 202 response."`
	AfterSeq  string `query:"after_seq" required:"true" pattern:"^[0-9]+$" doc:"The event_cursor from the same 202 response. The request's request.accepted marker and terminal event both follow it, so the lookup reads only the events after it."`
}

// RequestOutcome is the outcome of one async session request.
type RequestOutcome struct {
	RequestID string     `json:"request_id" doc:"The request_id from the 202 response."`
	Operation string     `json:"operation" enum:"session.create,session.message,session.submit" doc:"The operation that was accepted."`
	State     string     `json:"state" enum:"pending,succeeded,failed,outcome_unknown" doc:"pending: the request is still running, or its terminal event is held until the city event log takes it. succeeded: its request.result.* event was recorded; event carries it. failed: its request.failed event was recorded; event carries it. outcome_unknown: the server that accepted the request stopped (or lost the terminal event) before recording an outcome, so the operation may or may not have taken effect; inspect the target resource before retrying."`
	Event     *WireEvent `json:"event,omitempty" doc:"The terminal event exactly as /v0/city/{cityName}/events and the event stream deliver it: request.result.session.* for succeeded, request.failed for failed. Absent while pending and for outcome_unknown."`
}

// RequestOutcomeOutput is the response for GET /v0/city/{cityName}/request/{requestId}.
type RequestOutcomeOutput struct {
	Body RequestOutcome
}

// humaHandleRequestOutcome is the Huma-typed handler for
// GET /v0/city/{cityName}/request/{requestId}.
func (s *Server) humaHandleRequestOutcome(ctx context.Context, input *RequestOutcomeInput) (*RequestOutcomeOutput, error) {
	afterSeq, err := strconv.ParseUint(input.AfterSeq, 10, 64)
	if err != nil {
		return nil, apierr.ValidationFailed.Msg(fmt.Sprintf("after_seq %q is not an event sequence number: pass the event_cursor of the request's 202 response", input.AfterSeq))
	}
	out, err := s.lookupRequestOutcome(ctx, input.RequestID, afterSeq)
	if err != nil {
		return nil, err
	}
	return &RequestOutcomeOutput{Body: out}, nil
}

// lookupRequestOutcome derives a request's outcome from the city event log
// after afterSeq, the same persisted log the event stream serves, so the
// answer survives a missed event and a supervisor restart without a second
// store. The in-flight registry and the city's kept terminal events only
// separate pending from outcome_unknown.
func (s *Server) lookupRequestOutcome(ctx context.Context, requestID string, afterSeq uint64) (RequestOutcome, error) {
	ep := s.state.EventProvider()
	if ep == nil {
		return RequestOutcome{}, apierr.ServiceUnavailable.Msg("events not enabled")
	}
	requestID = strings.TrimSpace(requestID)
	// Read the registry before the log. The goroutine emits its terminal
	// event before it leaves the registry, so a request seen in flight here
	// either shows its terminal event below or is still pending, and one not
	// in flight has emitted whatever terminal event it will: recorded, kept
	// by its city because the log refused it, or lost.
	inFlightOp, inFlight := asyncRequestsInFlight.Load(asyncRequestKey{cityPath: s.state.CityPath(), requestID: requestID})
	out, err := readRequestOutcome(ctx, ep, requestID, afterSeq)
	switch {
	case err != nil, out.State != "":
		return out, err
	case inFlight:
		if out.Operation == "" {
			out.Operation, _ = inFlightOp.(string)
		}
		out.State = RequestStatePending
		return out, nil
	case out.Operation == "":
		return RequestOutcome{}, apierr.RequestNotFound.Msg(fmt.Sprintf("request %q not found: no async session request with this id is in the retained city event log after seq %d", requestID, afterSeq))
	}
	// An accepted request whose goroutine has finished with no terminal event
	// in the log may have one kept, the log having refused it. The kept events
	// are recorded now if the log takes them; one it still refuses is held,
	// so the request is pending, not outcome_unknown, which would invite a
	// duplicate resend. One recorded now, by this flush or a concurrent one,
	// is read again.
	if s.ops != nil {
		if s.ops.city.flushKeeps(s, requestID) {
			out.State = RequestStatePending
			return out, nil
		}
		if again, err := readRequestOutcome(ctx, ep, requestID, afterSeq); err != nil || again.State != "" {
			return again, err
		}
	}
	out.State = RequestStateOutcomeUnknown
	return out, nil
}

// readRequestOutcome scans ep's events after afterSeq for requestID's
// request.accepted marker and its first terminal event. The outcome carries the
// operation the events name, and a State and Event only when it found the
// terminal event.
func readRequestOutcome(ctx context.Context, ep events.Provider, requestID string, afterSeq uint64) (RequestOutcome, error) {
	filter := events.Filter{AfterSeq: afterSeq, Types: []string{
		events.RequestAccepted,
		events.RequestResultSessionCreate,
		events.RequestResultSessionMessage,
		events.RequestResultSessionSubmit,
		events.RequestFailed,
	}}
	evts, err := listRequestEvents(ctx, ep, filter)
	if err != nil {
		return RequestOutcome{}, apierr.Internal.Msg(fmt.Sprintf("reading city event log: %v", err))
	}

	out := RequestOutcome{RequestID: requestID}
	for _, e := range evts {
		var p struct {
			RequestID string `json:"request_id"`
			Operation string `json:"operation"`
		}
		if json.Unmarshal(e.Payload, &p) != nil || p.RequestID != requestID {
			continue
		}
		switch e.Type {
		case events.RequestAccepted:
			if isSessionRequestOperation(p.Operation) && out.Operation == "" {
				out.Operation = p.Operation
			}
			continue
		case events.RequestFailed:
			if !isSessionRequestOperation(p.Operation) {
				continue
			}
			out.Operation = p.Operation
			out.State = RequestStateFailed
		default:
			out.Operation = sessionRequestResultOperations[e.Type]
			out.State = RequestStateSucceeded
		}
		wire, ok := toWireEvent(e)
		if !ok {
			return RequestOutcome{}, apierr.Internal.Msg(fmt.Sprintf("decoding %s event seq %d", e.Type, e.Seq))
		}
		out.Event = &wire
		return out, nil
	}
	return out, nil
}

// listRequestEvents reads filter's events for the outcome lookup, folding in a
// segment still in flight in a rotation (listWithInFlight), and stops once
// ctx is done when the provider can.
func listRequestEvents(ctx context.Context, ep events.Provider, filter events.Filter) ([]events.Event, error) {
	if cp, ok := ep.(events.InFlightContextProvider); ok {
		return cp.ListInFlightContext(ctx, filter)
	}
	return listWithInFlight(ep, filter)
}
