package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestValidateCommandAllowsForceOnSling(t *testing.T) {
	// --force is dangerous in general (bypasses singleton + idempotency
	// checks) but legitimate on `sling` specifically. The dashboard
	// permits --force only on explicitly-allowed commands.
	if _, err := ValidateCommand("sling BL-42 mayor --force"); err != nil {
		t.Fatalf("ValidateCommand(sling --force): %v, want ok", err)
	}
}

func TestValidateCommandRejectsForceOnNonAllowedCommands(t *testing.T) {
	// Regression: PR #852 removed --force from BlockedPatterns so sling
	// could use it. Without scoping, any future whitelisted command could
	// accidentally inherit remote-execution risk when combined with
	// --force. The scoping check rejects --force on commands not in
	// ForceAllowedCommands.
	for _, cmd := range []string{
		"convoy refresh convoy-1 --force",
		"unsling BL-42 --force",
		"hook attach BL-42 --force",
	} {
		if _, err := ValidateCommand(cmd); err == nil {
			t.Fatalf("ValidateCommand(%q) = nil, want rejection of --force", cmd)
		}
	}
}

func TestValidateCommandMarksBeadQueriesAPIOnly(t *testing.T) {
	meta, err := ValidateCommand("list")
	if err != nil {
		t.Fatalf("ValidateCommand(list): %v", err)
	}
	if meta.Binary != "api" {
		t.Fatalf("list binary = %q, want api", meta.Binary)
	}

	meta, err = ValidateCommand("show bead-1")
	if err != nil {
		t.Fatalf("ValidateCommand(show): %v", err)
	}
	if meta.Binary != "api" {
		t.Fatalf("show binary = %q, want api", meta.Binary)
	}
}

func TestAPIHandlerRunExecutesListViaAPI(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v0/beads" {
			t.Fatalf("path = %q, want /v0/beads", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[{"id":"bead-1"}],"total":1}`))
	}))
	defer srv.Close()

	h := NewAPIHandler("/tmp/city", "test-city", srv.URL, "", 5*time.Second, 10*time.Second, "csrf-token")
	req := httptest.NewRequest(http.MethodPost, "/api/run", strings.NewReader(`{"command":"list"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Dashboard-Token", "csrf-token")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var resp CommandResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !resp.Success {
		t.Fatalf("success = false, error=%q", resp.Error)
	}
	if !strings.Contains(resp.Output, `"id": "bead-1"`) {
		t.Fatalf("output %q does not contain API bead payload", resp.Output)
	}
}

func TestAPIHandlerRunAPIOnlyCommandFailsClosedWhenAPIUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	h := NewAPIHandler("/tmp/city", "test-city", srv.URL, "", 5*time.Second, 10*time.Second, "csrf-token")
	req := httptest.NewRequest(http.MethodPost, "/api/run", strings.NewReader(`{"command":"list"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Dashboard-Token", "csrf-token")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusBadGateway, rec.Body.String())
	}
	var resp CommandResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Success {
		t.Fatalf("success = true, want false")
	}
	if !strings.Contains(resp.Error, "Failed to execute API-backed command") {
		t.Fatalf("error %q missing API-backed failure prefix", resp.Error)
	}
	if strings.Contains(resp.Error, "executable file not found") {
		t.Fatalf("error %q shows subprocess fallback; want fail-closed API error", resp.Error)
	}
}

func TestAPIHandlerRunExecutesShowViaAPI(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v0/bead/bead-1" {
			t.Fatalf("path = %q, want /v0/bead/bead-1", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"bead-1","title":"Example"}`))
	}))
	defer srv.Close()

	h := NewAPIHandler("/tmp/city", "test-city", srv.URL, "", 5*time.Second, 10*time.Second, "csrf-token")
	req := httptest.NewRequest(http.MethodPost, "/api/run", strings.NewReader(`{"command":"show bead-1"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Dashboard-Token", "csrf-token")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var resp CommandResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !resp.Success {
		t.Fatalf("success = false, error=%q", resp.Error)
	}
	if !strings.Contains(resp.Output, `"title": "Example"`) {
		t.Fatalf("output %q does not contain API bead payload", resp.Output)
	}
}

func TestAPIHandlerRunExecutesSlingViaAPIWithForce(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v0/sling" {
			t.Fatalf("path = %q, want /v0/sling", r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if got := body["bead"]; got != "BL-42" {
			t.Fatalf("bead = %#v, want BL-42", got)
		}
		if got := body["target"]; got != "myrig/worker" {
			t.Fatalf("target = %#v, want myrig/worker", got)
		}
		if got := body["formula"]; got != "graph-work" {
			t.Fatalf("formula = %#v, want graph-work", got)
		}
		if got := body["force"]; got != true {
			t.Fatalf("force = %#v, want true", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"slung","workflow_id":"wf-1","root_bead_id":"wf-1","mode":"attached"}`))
	}))
	defer srv.Close()

	h := NewAPIHandler("/tmp/city", "test-city", srv.URL, "", 5*time.Second, 10*time.Second, "csrf-token")
	req := httptest.NewRequest(http.MethodPost, "/api/run", strings.NewReader(`{"command":"sling BL-42 myrig/worker --force --formula=graph-work","confirmed":true}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Dashboard-Token", "csrf-token")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var resp CommandResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !resp.Success {
		t.Fatalf("success = false, error=%q", resp.Error)
	}
	if !strings.Contains(resp.Output, `"workflow_id": "wf-1"`) {
		t.Fatalf("output %q missing workflow payload", resp.Output)
	}
}

func TestAPIHandlerRunShowFailsClosedWhenAPIUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	h := NewAPIHandler("/tmp/city", "test-city", srv.URL, "", 5*time.Second, 10*time.Second, "csrf-token")
	req := httptest.NewRequest(http.MethodPost, "/api/run", strings.NewReader(`{"command":"show bead-1"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Dashboard-Token", "csrf-token")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusBadGateway, rec.Body.String())
	}
	var resp CommandResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Success {
		t.Fatalf("success = true, want false")
	}
	if !strings.Contains(resp.Error, "Failed to execute API-backed command") {
		t.Fatalf("error %q missing API-backed failure prefix", resp.Error)
	}
	if strings.Contains(resp.Error, "executable file not found") {
		t.Fatalf("error %q shows subprocess fallback; want fail-closed API error", resp.Error)
	}
}
