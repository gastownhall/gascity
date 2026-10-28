package api

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"

	"github.com/gastownhall/gascity/internal/runtime"
)

// TestRuntimeUnavailableIsServiceUnavailable: a session operation the runtime
// could not observe (a nudge or interrupt while the runtime is unreachable)
// is retryable, so both error mappers answer 503 runtime_unavailable rather
// than the 500 the store-error fallback gives an unrecognized error.
func TestRuntimeUnavailableIsServiceUnavailable(t *testing.T) {
	err := fmt.Errorf("nudging gc-1: %w: snapshot unavailable", runtime.ErrRuntimeUnavailable)
	rec := httptest.NewRecorder()
	writeSessionManagerError(rec, err)
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "runtime_unavailable") {
		t.Errorf("writeSessionManagerError = %d %s, want 503 runtime_unavailable", rec.Code, rec.Body.String())
	}
	var se huma.StatusError
	if got := humaSessionManagerError(err); !errors.As(got, &se) || se.GetStatus() != http.StatusServiceUnavailable || !strings.HasPrefix(got.Error(), "runtime_unavailable: ") {
		t.Errorf("humaSessionManagerError = %v, want 503 runtime_unavailable", got)
	}
}

// TestNudgeRejectedIsConflict: a nudge the runtime definitively refused (its
// state forbids the turn) delivered nothing and is no server fault, so both
// error mappers answer 409 nudge_rejected rather than the 500 the store-error
// fallback gives an unrecognized error.
func TestNudgeRejectedIsConflict(t *testing.T) {
	err := fmt.Errorf("nudging gc-1: %w: t3 refused the turn for %q: %w", runtime.ErrNudgeRejected, "gc-1", errors.New("Orchestration command invariant failed (thread.turn.start)"))
	rec := httptest.NewRecorder()
	writeSessionManagerError(rec, err)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "nudge_rejected") {
		t.Errorf("writeSessionManagerError = %d %s, want 409 nudge_rejected", rec.Code, rec.Body.String())
	}
	var se huma.StatusError
	if got := humaSessionManagerError(err); !errors.As(got, &se) || se.GetStatus() != http.StatusConflict || !strings.HasPrefix(got.Error(), "nudge_rejected: ") {
		t.Errorf("humaSessionManagerError = %v, want 409 nudge_rejected", got)
	}
}
