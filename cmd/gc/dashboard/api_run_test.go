package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

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
