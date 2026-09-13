package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestHTTPSupervisorClient_StatusRoundTrips confirms the client surfaces
// the supervisor's reported buildID. This is the load-bearing field for
// binary-drift detection in `gc start`.
func TestHTTPSupervisorClient_StatusRoundTrips(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"status":"ok","version":"v0","build_id":"deadbeef-dirty","uptime_sec":12,"cities_total":1,"cities_running":1}`)
	}))
	t.Cleanup(srv.Close)

	c := newHTTPSupervisorClient(srv.URL)
	got, err := c.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if got.BuildID != "deadbeef-dirty" {
		t.Fatalf("BuildID = %q, want %q", got.BuildID, "deadbeef-dirty")
	}
}

// TestHTTPSupervisorClient_StatusEmptyBuildID confirms the client tolerates
// supervisors that omit build_id (older builds, or `go run` invocations
// without VCS info). DetectBinaryDrift treats empty as "unknown" rather
// than asserting drift.
func TestHTTPSupervisorClient_StatusEmptyBuildID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"status":"ok","version":"v0","uptime_sec":1,"cities_total":0,"cities_running":0}`)
	}))
	t.Cleanup(srv.Close)

	c := newHTTPSupervisorClient(srv.URL)
	got, err := c.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if got.BuildID != "" {
		t.Fatalf("BuildID = %q, want empty", got.BuildID)
	}
}

// TestHTTPSupervisorClient_StatusNon200 confirms the client surfaces a
// descriptive error when /health returns a non-2xx response.
func TestHTTPSupervisorClient_StatusNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = fmt.Fprint(w, `{"detail":"boom"}`)
	}))
	t.Cleanup(srv.Close)

	c := newHTTPSupervisorClient(srv.URL)
	_, err := c.Status(context.Background())
	if err == nil {
		t.Fatal("Status returned nil error for 500 response")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error %q does not mention the status code", err)
	}
}

// TestHTTPSupervisorClient_PingOnSuccessful200 confirms Ping returns nil
// when the supervisor is responsive.
func TestHTTPSupervisorClient_PingOnSuccessful200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"status":"ok","version":"v0","uptime_sec":1,"cities_total":0,"cities_running":0}`)
	}))
	t.Cleanup(srv.Close)

	c := newHTTPSupervisorClient(srv.URL)
	if err := c.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
}

// TestHTTPSupervisorClient_PingOnUnreachable confirms Ping returns an
// error when the supervisor is not listening. PollReady relies on this
// to keep retrying until the supervisor comes up post-restart.
func TestHTTPSupervisorClient_PingOnUnreachable(t *testing.T) {
	c := newHTTPSupervisorClient("http://127.0.0.1:1") // port 1 → connection refused
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := c.Ping(ctx); err == nil {
		t.Fatal("Ping returned nil error for unreachable supervisor")
	}
}
