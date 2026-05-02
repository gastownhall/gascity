package api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"strconv"

	"github.com/gastownhall/gascity/internal/events"
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
	raw, err := json.Marshal(payload)
	if err != nil {
		log.Printf("api: marshal %s: %v", eventType, err)
		return
	}
	rec.Record(events.Event{
		Type:    eventType,
		Actor:   "api",
		Subject: subject,
		Payload: raw,
	})
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
	case RequestFailedPayload:
		return p.RequestID
	default:
		return ""
	}
}

// emitSessionCreateSucceeded records a request.result.session.create event.
func (s *Server) emitSessionCreateSucceeded(requestID string, resp sessionResponse) {
	s.emitAsyncResult(events.RequestResultSessionCreate, resp.ID, SessionCreateSucceededPayload{
		RequestID: requestID,
		Session:   resp,
	})
}

// emitSessionCreateFailed records a request.failed event for session.create.
func (s *Server) emitSessionCreateFailed(requestID, errorCode, errorMessage string) {
	s.emitRequestFailed(requestID, RequestOperationSessionCreate, errorCode, errorMessage)
}

// emitSessionMessageSucceeded records a request.result.session.message event.
func (s *Server) emitSessionMessageSucceeded(requestID, sessionID string) {
	s.emitAsyncResult(events.RequestResultSessionMessage, sessionID, SessionMessageSucceededPayload{
		RequestID: requestID,
		SessionID: sessionID,
	})
}

// emitSessionMessageFailed records a request.failed event for session.message.
func (s *Server) emitSessionMessageFailed(requestID, errorCode, errorMessage string) {
	s.emitRequestFailed(requestID, RequestOperationSessionMessage, errorCode, errorMessage)
}

// emitSessionSubmitSucceeded records a request.result.session.submit event.
func (s *Server) emitSessionSubmitSucceeded(requestID, sessionID string, queued bool, intent string) {
	s.emitAsyncResult(events.RequestResultSessionSubmit, sessionID, SessionSubmitSucceededPayload{
		RequestID: requestID,
		SessionID: sessionID,
		Queued:    queued,
		Intent:    intent,
	})
}

// emitSessionSubmitFailed records a request.failed event for session.submit.
func (s *Server) emitSessionSubmitFailed(requestID, errorCode, errorMessage string) {
	s.emitRequestFailed(requestID, RequestOperationSessionSubmit, errorCode, errorMessage)
}
