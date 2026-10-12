package api

import (
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
