package api

import (
	"encoding/json"
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

	req := newPostRequest("/v0/sessions", strings.NewReader(`{"kind":"provider","name":"test-agent"}`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusCreated, rec.Body.String())
	}

	var resp sessionResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Kind != "provider" {
		t.Fatalf("resp.Kind = %q, want provider", resp.Kind)
	}

	bead, err := fs.cityBeadStore.Get(resp.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", resp.ID, err)
	}
	if got := bead.Metadata["mc_session_kind"]; got != "" {
		t.Fatalf("mc_session_kind = %q, want empty", got)
	}
}
