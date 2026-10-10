package main

import (
	"encoding/json"
	"net/http"
	"strconv"
	"testing"

	"github.com/gastownhall/gascity/internal/api/genclient"
	"github.com/gastownhall/gascity/internal/events"
)

// contractAccepted is the correlation handle every 202 body carries.
type contractAccepted struct {
	RequestID   string
	EventCursor string
}

// contractRequestFailed mirrors the request.failed payload fields the suite
// asserts on.
type contractRequestFailed struct {
	RequestID    string `json:"request_id"`
	Operation    string `json:"operation"`
	ErrorCode    string `json:"error_code"`
	ErrorMessage string `json:"error_message"`
}

func eventRequestID(e events.Event) string {
	var p struct {
		RequestID string `json:"request_id"`
	}
	if len(e.Payload) == 0 || json.Unmarshal(e.Payload, &p) != nil {
		return ""
	}
	return p.RequestID
}

// cursorSeq parses a 202 event_cursor. The contract says it is usable as the
// events stream's after_seq, so a non-numeric cursor is a contract failure.
func cursorSeq(t *testing.T, cursor string) uint64 {
	t.Helper()
	seq, err := strconv.ParseUint(cursor, 10, 64)
	if err != nil {
		t.Fatalf("event_cursor %q is not a usable after_seq: %v", cursor, err)
	}
	return seq
}

// awaitOutcome waits — event-driven, from the accepted event_cursor, exactly
// as a client following the documented flow would — for the terminal event
// correlated by request_id. It returns the success event, or the decoded
// request.failed payload when the server reported failure instead.
func (h *contractHarness) awaitOutcome(t *testing.T, acc contractAccepted, successType string) (events.Event, *contractRequestFailed) {
	t.Helper()
	if acc.RequestID == "" {
		t.Fatalf("202 body carries no request_id")
	}
	e := h.waitEvent(cursorSeq(t, acc.EventCursor), successType+"|request.failed for "+acc.RequestID, func(e events.Event) bool {
		return (e.Type == successType || e.Type == events.RequestFailed) && eventRequestID(e) == acc.RequestID
	})
	if e.Type == events.RequestFailed {
		var f contractRequestFailed
		if err := json.Unmarshal(e.Payload, &f); err != nil {
			t.Fatalf("decode request.failed payload: %v", err)
		}
		return e, &f
	}
	h.expectEventListed(t, successType, acc.RequestID)
	return e, nil
}

// awaitSuccess is awaitOutcome for operations whose documented outcome in the
// harness is success.
func (h *contractHarness) awaitSuccess(t *testing.T, acc contractAccepted, successType string) events.Event {
	t.Helper()
	e, failed := h.awaitOutcome(t, acc, successType)
	if failed != nil {
		t.Fatalf("request %s failed: %s: %s", acc.RequestID, failed.ErrorCode, failed.ErrorMessage)
	}
	return e
}

// expectEventListed re-reads the terminal event through GET /events so its
// typed envelope is validated against the spec, not just the in-process bus.
func (h *contractHarness) expectEventListed(t *testing.T, eventType, requestID string) {
	t.Helper()
	resp, err := h.client.GetV0CityByCityNameEventsWithResponse(h.ctx, contractCityName,
		&genclient.GetV0CityByCityNameEventsParams{Type: ptr(eventType)})
	expectStatus(t, "list "+eventType+" events", resp, err, http.StatusOK)
	var list struct {
		Items []struct {
			Type    string          `json:"type"`
			Payload json.RawMessage `json:"payload"`
		} `json:"items"`
	}
	if err := json.Unmarshal(resp.Body, &list); err != nil {
		t.Fatalf("decode events list: %v", err)
	}
	for _, it := range list.Items {
		if it.Type == eventType && eventRequestID(events.Event{Payload: it.Payload}) == requestID {
			return
		}
	}
	t.Fatalf("GET events?type=%s does not list request_id %s: %s", eventType, requestID, string(resp.Body))
}
