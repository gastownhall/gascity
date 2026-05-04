package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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

	accepted := decodeAsyncAccepted(t, rec.Body)
	success, failure := waitForSessionCreateResult(t, fs.eventProv, accepted.RequestID)
	if success == nil {
		t.Fatalf("session create failed: %s: %s", failure.ErrorCode, failure.ErrorMessage)
	}

	bead, err := fs.cityBeadStore.Get(success.Session.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", success.Session.ID, err)
	}
	if got := bead.Metadata["real_world_app_session_kind"]; got != "" {
		t.Fatalf("real_world_app_session_kind = %q, want empty", got)
	}
}
