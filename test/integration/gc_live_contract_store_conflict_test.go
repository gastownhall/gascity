//go:build integration && unix

package integration

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/api/apicontract"
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
		{"store_conflict detail with wrong code", http.StatusServiceUnavailable, `{"status":503,"code":"service-unavailable","detail":"store_conflict: lookalike"}`, false},
		{"store_conflict detail without code", http.StatusServiceUnavailable, `{"status":503,"detail":"store_conflict: lookalike"}`, false},
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

func TestLiveContractDoValidatesIntermediateStoreConflict(t *testing.T) {
	cases := []struct {
		name        string
		conflicts   int32
		finalStatus int
		finalBody   string
		accept      int
		validations int
		requests    int32
	}{
		{"each re-issued 503", 2, http.StatusOK, `{"status":"ok","id":"session-a"}`, http.StatusOK, 2, 3},
		{"accepted 503 is final", 1, http.StatusOK, `{"status":"ok"}`, http.StatusServiceUnavailable, 0, 1},
		{"other 503 is final", 0, http.StatusServiceUnavailable, `{"status":503,"code":"service-unavailable","detail":"no bead store configured"}`, http.StatusOK, 0, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec, err := os.ReadFile(filepath.Join("..", "..", "internal", "api", "openapi.json"))
			if err != nil {
				t.Fatalf("read OpenAPI spec: %v", err)
			}
			v := liveContractValidator(t, spec)
			srv, calls := storeConflictThenOKServer(t, tc.conflicts, tc.finalStatus, tc.finalBody)
			validations := 0
			validate := func(t *testing.T, spec *apicontract.Spec, req *http.Request, resp *http.Response, raw []byte) {
				t.Helper()
				if resp.StatusCode != http.StatusServiceUnavailable || string(raw) != liveStoreConflictBody {
					t.Fatalf("intermediate response = %d %s, want the declared 503", resp.StatusCode, raw)
				}
				validateLiveContractResponse(t, spec, req, resp, raw)
				validations++
			}

			liveContractDoWith(t, srv.URL, v, http.MethodPost, "/v0/city/c/session/session-a/wake", nil, nil, func(status int) bool {
				return status == tc.accept
			}, time.Now, validate)

			if validations != tc.validations {
				t.Fatalf("intermediate response validations = %d, want %d", validations, tc.validations)
			}
			if got := calls.Load(); got != tc.requests {
				t.Fatalf("requests = %d, want %d", got, tc.requests)
			}
		})
	}
}

func TestLiveContractDoStopsReissuingAfterStoreConflictBudget(t *testing.T) {
	cases := []struct {
		name     string
		elapsed  time.Duration
		status   int
		requests int32
	}{
		{"before deadline", liveContractStoreConflictBudget - time.Nanosecond, http.StatusOK, 2},
		{"at deadline", liveContractStoreConflictBudget, http.StatusOK, 2},
		{"past deadline", liveContractStoreConflictBudget + time.Nanosecond, http.StatusServiceUnavailable, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			start := time.Unix(1, 0)
			clockCalls := 0
			now := func() time.Time {
				clockCalls++
				if clockCalls == 1 {
					return start
				}
				return start.Add(tc.elapsed)
			}
			srv, calls := storeConflictThenOKServer(t, 1, http.StatusOK, `{"status":"ok"}`)

			_, resp, _ := liveContractDoWith(t, srv.URL, nil, http.MethodPost, "/v0/city/c/session/session-a/wake", nil, nil, func(status int) bool {
				return status == http.StatusOK
			}, now, validateLiveContractResponse)

			if resp.StatusCode != tc.status {
				t.Fatalf("final status = %d, want %d", resp.StatusCode, tc.status)
			}
			if got := calls.Load(); got != tc.requests {
				t.Fatalf("requests = %d, want %d", got, tc.requests)
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
