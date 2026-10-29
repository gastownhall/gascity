//go:build integration

package integration

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// liveStoreConflictBody is the declared, retryable 503 the supervisor answered
// a session wake with under host load, verbatim from the response that failed
// TestGCLiveContract_BeadsAndEvents in the ga-o01pz0 gate (ga-19ii7x).
const liveStoreConflictBody = `{"type":"urn:gascity:error:store-unavailable","title":"Store Unavailable","status":503,"detail":"store_conflict: commit update wisp: Error 1213 (40001): serialization failure: this transaction conflicts with a committed transaction from another client, try restarting transaction.","code":"store-unavailable"}`

func TestLiveContractStoreConflictIsOnlyTheDeclaredRetryable503(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{"observed wake store_conflict", http.StatusServiceUnavailable, liveStoreConflictBody, true},
		{"store_conflict detail on a 500", http.StatusInternalServerError, liveStoreConflictBody, false},
		{"other 503 cause", http.StatusServiceUnavailable, `{"status":503,"code":"service-unavailable","detail":"no bead store configured"}`, false},
		{"store-unavailable without store_conflict", http.StatusServiceUnavailable, `{"status":503,"code":"store-unavailable","detail":"store unreachable"}`, false},
		{"session conflict", http.StatusConflict, `{"status":409,"code":"session-conflict","detail":"store_conflict: lookalike"}`, false},
		{"non-JSON 503", http.StatusServiceUnavailable, "store_conflict", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := liveContractStoreConflict(tc.status, []byte(tc.body)); got != tc.want {
				t.Fatalf("liveContractStoreConflict(%d, %s) = %v, want %v", tc.status, tc.body, got, tc.want)
			}
		})
	}
}

// storeConflictThenOKServer answers the first conflicts requests with the
// declared store_conflict 503 and every later one with status/body.
func storeConflictThenOKServer(t *testing.T, conflicts int32, status int, body string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		if calls.Add(1) <= conflicts {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(liveStoreConflictBody))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

// A mutation that loses its store write to a concurrent writer gets the
// declared store_conflict 503; the contract client re-issues it rather than
// failing the run, which is what failed TestGCLiveContract_BeadsAndEvents.
func TestLiveContractRequestReissuesDeclaredStoreConflict(t *testing.T) {
	srv, calls := storeConflictThenOKServer(t, 2, http.StatusOK, `{"status":"ok","id":"rwacd-wisp-oas"}`)

	wake := liveContractJSON[struct {
		ID string `json:"id"`
	}](t, srv.URL, nil, http.MethodPost, "/v0/city/c/session/rwacd-wisp-oas/wake", nil, http.StatusOK)

	if wake.ID != "rwacd-wisp-oas" {
		t.Fatalf("wake id = %q, want rwacd-wisp-oas", wake.ID)
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("requests = %d, want 3 (two store conflicts, then success)", got)
	}
}

func TestLiveContractRequestOneOfReissuesDeclaredStoreConflict(t *testing.T) {
	srv, calls := storeConflictThenOKServer(t, 1, http.StatusConflict, `{"status":409,"code":"session-conflict","detail":"no_pending: none"}`)

	liveContractRequestOneOf(t, srv.URL, nil, http.MethodPost, "/v0/city/c/session/s/respond", map[string]string{"action": "deny"}, []int{http.StatusConflict, http.StatusNotImplemented})

	if got := calls.Load(); got != 2 {
		t.Fatalf("requests = %d, want 2 (one store conflict, then the accepted 409)", got)
	}
}

// Only the declared store_conflict is re-issued, and never when the caller
// accepts the status it carries: any other response is final on first sight.
func TestLiveContractDoReturnsFinalResponsesWithoutReissue(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		wanted func(int) bool
	}{
		{"other 503 cause", http.StatusServiceUnavailable, `{"status":503,"code":"service-unavailable","detail":"no bead store configured"}`, func(int) bool { return false }},
		{"internal error", http.StatusInternalServerError, `{"status":500,"code":"internal","detail":"internal: boom"}`, func(int) bool { return false }},
		{"accepted store_conflict", http.StatusServiceUnavailable, liveStoreConflictBody, func(status int) bool { return status == http.StatusServiceUnavailable }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, calls := storeConflictThenOKServer(t, 0, tc.status, tc.body)

			_, resp, raw := liveContractDo(t, srv.URL, nil, http.MethodPost, "/v0/city/c/session/s/wake", nil, nil, tc.wanted)

			if resp.StatusCode != tc.status || string(raw) != tc.body {
				t.Fatalf("response = %d %s, want %d %s", resp.StatusCode, raw, tc.status, tc.body)
			}
			if got := calls.Load(); got != 1 {
				t.Fatalf("requests = %d, want 1 (final response, no re-issue)", got)
			}
		})
	}
}
