package api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
)

func newRequestID() (string, error) {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generating request ID: %w", err)
	}
	return "req-" + hex.EncodeToString(b), nil
}

// emitRequestResult records a request.result event to the city's event
// provider.
func (s *Server) emitRequestResult(_ beads.Store, requestID, operation, status, resourceID, errorCode, errorMessage string) {
	payload, err := json.Marshal(RequestResultPayload{
		RequestID:    requestID,
		Operation:    operation,
		Status:       status,
		ResourceID:   resourceID,
		ErrorCode:    errorCode,
		ErrorMessage: errorMessage,
	})
	if err != nil {
		log.Printf("api: marshal request.result: %v", err)
		return
	}
	rec := s.state.EventProvider()
	if rec == nil {
		log.Printf("api: no event provider for request.result %s", requestID)
		return
	}
	rec.Record(events.Event{
		Type:    events.RequestResult,
		Actor:   "api",
		Subject: resourceID,
		Payload: payload,
	})
}
