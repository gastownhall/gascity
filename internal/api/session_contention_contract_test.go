package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	beadslib "github.com/steveyegge/beads"
)

// The session routes promise a recoverable, machine-readable response after
// store contention exhausts its own budget. In particular a CAS exhaustion
// wraps ErrVersionMismatch, whose text alone is not a Dolt error code.
func TestHumaStoreErrorContentionContract(t *testing.T) {
	cas := fmt.Errorf("%w: %w", &beads.CASRetriesExhaustedError{
		ID: "ga-session", Key: "state", Attempts: 3, LastRevision: 7,
	}, beadslib.ErrVersionMismatch)
	tests := []struct {
		name         string
		err          error
		code, prefix string
		status       int
	}{
		{"Dolt 1213", errors.New("commit: Error 1213 (40001): serialization failure"), "store-unavailable", "store_conflict: ", http.StatusServiceUnavailable},
		{"CAS exhausted", cas, "store-unavailable", "store_conflict: ", http.StatusServiceUnavailable},
		{"missing", fmt.Errorf("read session: %w", beads.ErrNotFound), "session-not-found", "not_found: ", http.StatusNotFound},
		{"unrelated", errors.New("disk full"), "internal", "internal: ", http.StatusInternalServerError},
		{"bead id is not SQLSTATE", errors.New("read ga-40001x failed"), "internal", "internal: ", http.StatusInternalServerError},
		{"ambiguous connection result", errors.New("write commit result indeterminate after connection loss (not retried to avoid double-apply)"), "internal", "internal: ", http.StatusInternalServerError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := humaStoreError(tt.err)
			var body struct {
				Status int    `json:"status"`
				Code   string `json:"code"`
				Detail string `json:"detail"`
			}
			wire, err := json.Marshal(got)
			if err != nil {
				t.Fatalf("marshal problem: %v", err)
			}
			if err := json.Unmarshal(wire, &body); err != nil {
				t.Fatalf("decode problem: %v", err)
			}
			if body.Status != tt.status || body.Code != tt.code || !strings.HasPrefix(body.Detail, tt.prefix) {
				t.Fatalf("problem = %s, want status %d, code %q, detail prefix %q", wire, tt.status, tt.code, tt.prefix)
			}
		})
	}
}

func TestSessionCloseDeleteContentionResponse(t *testing.T) {
	for _, tt := range []struct {
		name   string
		store  func(beads.Store) beads.Store
		status int
		code   string
		prefix string
	}{
		{"exhausted conflict", func(base beads.Store) beads.Store { return &alwaysTransientDeleteConflictStore{Store: base} }, http.StatusServiceUnavailable, "store-unavailable", "store_conflict: "},
		{"permanent failure", func(base beads.Store) beads.Store {
			return &nonTransientDeleteErrorStore{Store: base, err: errors.New("permission denied")}
		}, http.StatusInternalServerError, "internal", "closed but delete failed: "},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fs := newSessionFakeState(t)
			fs.cityBeadStore = tt.store(beads.NewMemStore())
			h := newTestCityHandlerWith(t, fs, New(fs))
			info := createTestSession(t, fs.cityBeadStore, fs.sp, "close delete contention")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, newPostRequest(cityURL(fs, "/session/")+info.ID+"/close?delete=true", nil))
			var body struct {
				Code   string `json:"code"`
				Detail string `json:"detail"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode response %q: %v", rec.Body.String(), err)
			}
			if rec.Code != tt.status || body.Code != tt.code || !strings.HasPrefix(body.Detail, tt.prefix) {
				t.Fatalf("response = %d %s, want status %d, code %q, detail prefix %q", rec.Code, rec.Body.String(), tt.status, tt.code, tt.prefix)
			}
		})
	}
}
