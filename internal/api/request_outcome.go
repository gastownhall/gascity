package api

import (
	"context"
	"encoding/json"
	"fmt"
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
// been recorded.
func (s *Server) acceptAsyncRequest(operation string) (requestID, eventCursor string, finish func(), err error) {
	ep := s.state.EventProvider()
	if ep == nil {
		return "", "", nil, apierr.ServiceUnavailable.Msg("events not enabled: the outcome of an async request could not be recorded")
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
	key := asyncRequestKey{cityPath: s.state.CityPath(), requestID: requestID}
	// Register before recording: a lookup that finds the marker must also find
	// the request in flight while its goroutine runs.
	asyncRequestsInFlight.Store(key, operation)
	if err := recordRequestAccepted(ep, requestID, operation); err != nil {
		asyncRequestsInFlight.Delete(key)
		return "", "", nil, apierr.ServiceUnavailable.Msg(fmt.Sprintf("recording acceptance of %s request: %v", operation, err))
	}
	return requestID, eventCursor, func() { asyncRequestsInFlight.Delete(key) }, nil
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
}

// RequestOutcome is the outcome of one async session request.
type RequestOutcome struct {
	RequestID string     `json:"request_id" doc:"The request_id from the 202 response."`
	Operation string     `json:"operation" enum:"session.create,session.message,session.submit" doc:"The operation that was accepted."`
	State     string     `json:"state" enum:"pending,succeeded,failed,outcome_unknown" doc:"pending: the request is still running. succeeded: its request.result.* event was recorded; event carries it. failed: its request.failed event was recorded; event carries it. outcome_unknown: the server that accepted the request stopped (or lost the terminal event) before recording an outcome, so the operation may or may not have taken effect; inspect the target resource before retrying."`
	Event     *WireEvent `json:"event,omitempty" doc:"The terminal event exactly as /v0/city/{cityName}/events and the event stream deliver it: request.result.session.* for succeeded, request.failed for failed. Absent while pending and for outcome_unknown."`
}

// RequestOutcomeOutput is the response for GET /v0/city/{cityName}/request/{requestId}.
type RequestOutcomeOutput struct {
	Body RequestOutcome
}

// humaHandleRequestOutcome is the Huma-typed handler for
// GET /v0/city/{cityName}/request/{requestId}.
func (s *Server) humaHandleRequestOutcome(_ context.Context, input *RequestOutcomeInput) (*RequestOutcomeOutput, error) {
	out, err := s.lookupRequestOutcome(input.RequestID)
	if err != nil {
		return nil, err
	}
	return &RequestOutcomeOutput{Body: out}, nil
}

// lookupRequestOutcome derives a request's outcome from the city event log, the
// same persisted log the event stream serves, so the answer survives a missed
// event and a supervisor restart without a second store. The in-flight
// registry only separates pending from outcome_unknown.
func (s *Server) lookupRequestOutcome(requestID string) (RequestOutcome, error) {
	ep := s.state.EventProvider()
	if ep == nil {
		return RequestOutcome{}, apierr.ServiceUnavailable.Msg("events not enabled")
	}
	requestID = strings.TrimSpace(requestID)
	// Read the registry before the log. The goroutine records its terminal
	// event before it leaves the registry, so a request seen in flight here
	// either shows its terminal event below or is still pending, and one not
	// in flight has already recorded whatever terminal event it will.
	inFlightOp, inFlight := asyncRequestsInFlight.Load(asyncRequestKey{cityPath: s.state.CityPath(), requestID: requestID})

	filter := events.Filter{Types: []string{
		events.RequestAccepted,
		events.RequestResultSessionCreate,
		events.RequestResultSessionMessage,
		events.RequestResultSessionSubmit,
		events.RequestFailed,
	}}
	var evts []events.Event
	var err error
	if ifp, ok := ep.(events.InFlightProvider); ok {
		evts, err = ifp.ListInFlight(filter)
	} else {
		evts, err = ep.List(filter)
	}
	if err != nil {
		return RequestOutcome{}, apierr.Internal.Msg(fmt.Sprintf("reading city event log: %v", err))
	}

	out := RequestOutcome{RequestID: requestID}
	var terminal *events.Event
	for i := range evts {
		e := evts[i]
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
		terminal = &e
		break
	}

	if terminal != nil {
		wire, ok := toWireEvent(*terminal)
		if !ok {
			return RequestOutcome{}, apierr.Internal.Msg(fmt.Sprintf("decoding %s event seq %d", terminal.Type, terminal.Seq))
		}
		out.Event = &wire
		return out, nil
	}
	if inFlight {
		if out.Operation == "" {
			out.Operation, _ = inFlightOp.(string)
		}
		out.State = RequestStatePending
		return out, nil
	}
	if out.Operation != "" {
		out.State = RequestStateOutcomeUnknown
		return out, nil
	}
	return RequestOutcome{}, apierr.RequestNotFound.Msg(fmt.Sprintf("request %q not found: no async session request with this id is in the retained city event log", requestID))
}
