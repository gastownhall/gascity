package api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"strconv"

	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/session"
)

func newRequestID() (string, error) {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generating request ID: %w", err)
	}
	return "req-" + hex.EncodeToString(b), nil
}

func (s *Server) currentCityEventCursor() (string, error) {
	ep := s.state.EventProvider()
	if ep == nil {
		return "0", nil
	}
	seq, err := ep.LatestSeq()
	if err != nil {
		return "", fmt.Errorf("capturing city event cursor: %w", err)
	}
	return strconv.FormatUint(seq, 10), nil
}

// EmitTypedEvent records a typed async result event to the given recorder.
func EmitTypedEvent(rec events.Recorder, eventType, subject string, payload events.Payload) {
	e, err := typedEvent(eventType, subject, payload)
	if err != nil {
		log.Printf("api: %v", err)
		return
	}
	rec.Record(e)
}

// typedEvent is the typed async result event EmitTypedEvent records.
func typedEvent(eventType, subject string, payload events.Payload) (events.Event, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return events.Event{}, fmt.Errorf("marshal %s: %w", eventType, err)
	}
	return events.Event{
		Type:    eventType,
		Actor:   "api",
		Subject: subject,
		Payload: raw,
	}, nil
}

// EmitRequestFailed records a request.failed event to the given recorder.
func EmitRequestFailed(rec events.Recorder, requestID, operation, errorCode, errorMessage string) {
	EmitTypedEvent(rec, events.RequestFailed, "", RequestFailedPayload{
		RequestID:    requestID,
		Operation:    operation,
		ErrorCode:    errorCode,
		ErrorMessage: errorMessage,
	})
}

func (s *Server) emitAsyncResult(eventType, subject string, payload events.Payload) {
	rec := s.state.EventProvider()
	if rec == nil {
		log.Printf("api: no event provider for %s result %s", eventType, requestIDFromPayload(payload))
		return
	}
	EmitTypedEvent(rec, eventType, subject, payload)
}

// emitSessionRequestResult records operation's session create, /submit or
// /messages terminal event through its city's shared op state
// (session_ops.go): a request that outlived the Server it started on reports
// to the event log the supervisor serves the city with now, not the one the
// city's restart closed, and an event that log refuses is kept until a log
// takes it. A Server with no op state records to its own event log.
func (s *Server) emitSessionRequestResult(operation, eventType, subject string, payload events.Payload) {
	if s.ops == nil {
		s.emitAsyncResult(eventType, subject, payload)
		return
	}
	e, err := typedEvent(eventType, subject, payload)
	if err != nil {
		log.Printf("api: %v", err)
		return
	}
	s.ops.city.emit(s, requestIDFromPayload(payload), operation, e)
}

func (s *Server) emitRequestFailed(requestID, operation, errorCode, errorMessage string) {
	s.emitAsyncResult(events.RequestFailed, "", RequestFailedPayload{
		RequestID:    requestID,
		Operation:    operation,
		ErrorCode:    errorCode,
		ErrorMessage: errorMessage,
	})
}

func (s *Server) recoverAsRequestFailed(requestID, operation string) {
	if r := recover(); r != nil {
		s.emitRequestFailed(requestID, operation, "internal_error", fmt.Sprintf("panic: %v", r))
	}
}

// recoverSessionCreate turns a panic in a session create goroutine into its
// request.failed{internal_error} event.
func (s *Server) recoverSessionCreate(requestID string) {
	if r := recover(); r != nil {
		s.emitSessionCreateFailed(requestID, "internal_error", fmt.Sprintf("panic: %v", r))
	}
}

func requestIDFromPayload(payload events.Payload) string {
	switch p := payload.(type) {
	case CityCreateSucceededPayload:
		return p.RequestID
	case CityUnregisterSucceededPayload:
		return p.RequestID
	case SessionCreateSucceededPayload:
		return p.RequestID
	case SessionMessageSucceededPayload:
		return p.RequestID
	case SessionSubmitSucceededPayload:
		return p.RequestID
	case RigCreateSucceededPayload:
		return p.RequestID
	case RigProvisionProgressPayload:
		return p.RequestID
	case RequestFailedPayload:
		return p.RequestID
	case RequestAcceptedPayload:
		return p.RequestID
	default:
		return ""
	}
}

// emitSessionCreateSucceeded records a request.result.session.create event.
func (s *Server) emitSessionCreateSucceeded(requestID string, resp sessionResponse) {
	s.emitSessionRequestResult(RequestOperationSessionCreate, events.RequestResultSessionCreate, resp.ID, SessionCreateSucceededPayload{
		RequestID: requestID,
		Session:   resp,
	})
}

// emitSessionCreateFailed records a request.failed event for session.create.
func (s *Server) emitSessionCreateFailed(requestID, errorCode, errorMessage string) {
	s.emitSessionCreateFailedFor(requestID, "", errorCode, errorMessage)
}

// emitSessionCreateFailedFor records a session.create failure for a session
// the create had already written, naming it so the client can find it; an
// empty sessionID names none.
func (s *Server) emitSessionCreateFailedFor(requestID, sessionID, errorCode, errorMessage string) {
	s.emitSessionRequestResult(RequestOperationSessionCreate, events.RequestFailed, "", RequestFailedPayload{
		RequestID:    requestID,
		Operation:    RequestOperationSessionCreate,
		ErrorCode:    errorCode,
		ErrorMessage: errorMessage,
		SessionID:    sessionID,
	})
}

// emitSessionMessageSucceeded records a request.result.session.message event.
// op is the operation the request was recorded as, nil when it was not.
func (s *Server) emitSessionMessageSucceeded(requestID, sessionID string, out messageOutcome, op *ConversationOp) {
	s.emitSessionRequestResult(RequestOperationSessionMessage, events.RequestResultSessionMessage, sessionID, SessionMessageSucceededPayload{
		RequestID:          requestID,
		SessionID:          sessionID,
		Queued:             out.Queued,
		WillStart:          out.willStart,
		WillNotStartReason: out.willNotStartReason,
		Op:                 op,
	})
}

// emitSessionMessageFailed records a request.failed event for session.message.
func (s *Server) emitSessionMessageFailed(requestID, errorCode, errorMessage string, op *ConversationOp) {
	s.emitRequestFailedOp(requestID, RequestOperationSessionMessage, errorCode, errorMessage, op)
}

// emitSessionSubmitSucceeded records a request.result.session.submit event.
func (s *Server) emitSessionSubmitSucceeded(requestID, sessionID string, out messageOutcome, intent string, op *ConversationOp) {
	s.emitSessionRequestResult(RequestOperationSessionSubmit, events.RequestResultSessionSubmit, sessionID, SessionSubmitSucceededPayload{
		RequestID:          requestID,
		SessionID:          sessionID,
		Queued:             out.Queued,
		Intent:             intent,
		WillStart:          out.willStart,
		WillNotStartReason: out.willNotStartReason,
		Interrupt:          submitInterruptWire(out.Interrupt),
		Op:                 op,
	})
}

// emitSessionSubmitFailed records a request.failed event for session.submit.
func (s *Server) emitSessionSubmitFailed(requestID, errorCode, errorMessage string, op *ConversationOp) {
	s.emitRequestFailedOp(requestID, RequestOperationSessionSubmit, errorCode, errorMessage, op)
}

// emitRequestFailedOp records a /submit or /messages request.failed event
// carrying the operation the failed request was recorded as; a nil op is the
// legacy payload.
func (s *Server) emitRequestFailedOp(requestID, operation, errorCode, errorMessage string, op *ConversationOp) {
	s.emitSessionRequestResult(operation, events.RequestFailed, "", RequestFailedPayload{
		RequestID:    requestID,
		Operation:    operation,
		ErrorCode:    errorCode,
		ErrorMessage: errorMessage,
		Op:           op,
	})
}

// submitInterruptWire projects a turn-fenced interrupt_now's report onto the
// submit result; nil when no fence ran.
func submitInterruptWire(r *session.TurnInterruptReport) *SessionSubmitInterrupt {
	if r == nil {
		return nil
	}
	return &SessionSubmitInterrupt{Outcome: string(r.Outcome), TurnID: r.TurnID, Degradation: r.Degradation}
}

// emitRigCreateSucceeded records a request.result.rig.create event — the
// terminal success of an async server-side rig add.
func (s *Server) emitRigCreateSucceeded(requestID, rig, prefix, defaultBranch string) {
	s.emitAsyncResult(events.RequestResultRigCreate, rig, RigCreateSucceededPayload{
		RequestID:     requestID,
		Rig:           rig,
		Prefix:        prefix,
		DefaultBranch: defaultBranch,
	})
}

// emitRigProvisionProgress records a rig.provision.progress event for one
// provisioning step. It is best-effort telemetry: the caller wraps it in a
// recover so an event-bus hiccup never fails a rig add.
func (s *Server) emitRigProvisionProgress(requestID, rig, step, detail string, warn bool) {
	s.emitAsyncResult(events.RigProvisionProgress, rig, RigProvisionProgressPayload{
		RequestID: requestID,
		Rig:       rig,
		Step:      step,
		Detail:    detail,
		Warn:      warn,
	})
}
