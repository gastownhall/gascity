package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Phase 0 spec coverage from engdocs/design/session-model-unification.md:
// - provider compatibility boundary
// - Phase 1 internal unification with compatibility veneers

func TestPhase0ProviderCompatibility_CreateKeepsResponseKindButDoesNotPersistSpecialSessionKind(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)

	req := newPostRequest(cityURL(fs, "/sessions"), strings.NewReader(`{"kind":"provider","name":"test-agent"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusAccepted, rec.Body.String())
	}

	result := waitForRequestResult(t, fs.eventProv, "session.create", 5*time.Second)
	if result.Status != "succeeded" {
		t.Fatalf("request.result status = %q, want succeeded", result.Status)
	}

	bead, err := fs.cityBeadStore.Get(result.ResourceID)
	if err != nil {
		t.Fatalf("Get(%s): %v", result.ResourceID, err)
	}
	if got := bead.Metadata["mc_session_kind"]; got != "" {
		t.Fatalf("mc_session_kind = %q, want empty", got)
	}
}
