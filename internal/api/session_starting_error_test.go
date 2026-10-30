package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"

	"github.com/gastownhall/gascity/internal/session"
)

// TestSessionStartingIsARetryableConflict: an operator call that found the
// runtime lease held past its wait (CONTRACT O3) answers 409, retryable, in
// both error mappers.
func TestSessionStartingIsARetryableConflict(t *testing.T) {
	err := fmt.Errorf("attach: %w", session.ErrSessionStarting)
	rec := httptest.NewRecorder()
	writeSessionManagerError(rec, err)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "session_starting") {
		t.Errorf("writeSessionManagerError = %d %s, want 409 session_starting", rec.Code, rec.Body.String())
	}
	var se huma.StatusError
	if got := humaSessionManagerError(err); !errors.As(got, &se) || se.GetStatus() != http.StatusConflict || !strings.Contains(got.Error(), "session_starting") {
		t.Errorf("humaSessionManagerError = %v, want 409 session_starting", got)
	}
}

// TestSessionInactiveIsAConflict: an operation that needs a live runtime on a
// session that has none answers 409 conflict in both error mappers, not the
// 500 the store-error fallback gives an unrecognized error.
func TestSessionInactiveIsAConflict(t *testing.T) {
	err := fmt.Errorf("session gc-1: %w", session.ErrSessionInactive)
	rec := httptest.NewRecorder()
	writeSessionManagerError(rec, err)
	var problem problemDetails
	if decodeErr := json.Unmarshal(rec.Body.Bytes(), &problem); decodeErr != nil {
		t.Fatalf("decode writeSessionManagerError body %q: %v", rec.Body.String(), decodeErr)
	}
	if rec.Code != http.StatusConflict || !strings.HasPrefix(problem.Detail, "conflict: ") {
		t.Errorf("writeSessionManagerError = %d %q, want 409 with a conflict: detail", rec.Code, problem.Detail)
	}
	var se huma.StatusError
	if got := humaSessionManagerError(err); !errors.As(got, &se) || se.GetStatus() != http.StatusConflict || !strings.HasPrefix(got.Error(), "conflict: ") {
		t.Errorf("humaSessionManagerError = %v, want 409 with a conflict: detail", got)
	}
}

// TestSessionStoppingIsARetryableConflict: an operator resume refused while
// the controller's drain-ack stop is pending answers 409, retryable.
func TestSessionStoppingIsARetryableConflict(t *testing.T) {
	err := fmt.Errorf("attach: %w", session.ErrSessionStopping)
	rec := httptest.NewRecorder()
	writeSessionManagerError(rec, err)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "session_stopping") {
		t.Errorf("writeSessionManagerError = %d %s, want 409 session_stopping", rec.Code, rec.Body.String())
	}
	var se huma.StatusError
	if got := humaSessionManagerError(err); !errors.As(got, &se) || se.GetStatus() != http.StatusConflict || !strings.Contains(got.Error(), "session_stopping") {
		t.Errorf("humaSessionManagerError = %v, want 409 session_stopping", got)
	}
}

// TestConversationRetainedIsAConflict: a resume of a committed conversation
// that failed and was kept answers 409 conversation_retained in both error
// mappers, the failed start and its reset remedy intact, not the 500 the
// store-error fallback gives an unrecognized error.
func TestConversationRetainedIsAConflict(t *testing.T) {
	err := fmt.Errorf("session gc-1: %w after failed resume; run `gc session reset gc-1` to start fresh: %w", session.ErrConversationRetained, errors.New("session died during startup"))
	rec := httptest.NewRecorder()
	writeSessionManagerError(rec, err)
	var problem problemDetails
	if decodeErr := json.Unmarshal(rec.Body.Bytes(), &problem); decodeErr != nil {
		t.Fatalf("decode writeSessionManagerError body %q: %v", rec.Body.String(), decodeErr)
	}
	if rec.Code != http.StatusConflict || problem.Detail != "conversation_retained: "+err.Error() {
		t.Errorf("writeSessionManagerError = %d %q, want 409 conversation_retained: %s", rec.Code, problem.Detail, err)
	}
	var se huma.StatusError
	if got := humaSessionManagerError(err); !errors.As(got, &se) || se.GetStatus() != http.StatusConflict || got.Error() != "conversation_retained: "+err.Error() {
		t.Errorf("humaSessionManagerError = %v, want 409 conversation_retained: %s", got, err)
	}
}

// TestTurnInterruptUnsupportedIsNotImplemented: a runtime that cannot
// interrupt the session's turn answers 501 turn_interrupt_unsupported in both
// error mappers, not the 500 the store-error fallback gives.
func TestTurnInterruptUnsupportedIsNotImplemented(t *testing.T) {
	err := fmt.Errorf("stop: %w", session.ErrTurnInterruptUnsupported)
	rec := httptest.NewRecorder()
	writeSessionManagerError(rec, err)
	if rec.Code != http.StatusNotImplemented || !strings.Contains(rec.Body.String(), "turn_interrupt_unsupported") {
		t.Errorf("writeSessionManagerError = %d %s, want 501 turn_interrupt_unsupported", rec.Code, rec.Body.String())
	}
	var se huma.StatusError
	if got := humaSessionManagerError(err); !errors.As(got, &se) || se.GetStatus() != http.StatusNotImplemented || !strings.Contains(got.Error(), "turn_interrupt_unsupported") {
		t.Errorf("humaSessionManagerError = %v, want 501 turn_interrupt_unsupported", got)
	}
}
