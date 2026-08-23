package api

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/events"
)

func TestSupervisorHostAllowlistRejectsUnexpectedHost(t *testing.T) {
	sm := newTestSupervisorMux(t, map[string]*fakeState{})
	req := httptest.NewRequest(http.MethodGet, "http://evil.example/v0/cities", nil)
	rec := httptest.NewRecorder()

	sm.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusMisdirectedRequest {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusMisdirectedRequest, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/problem+json") {
		t.Fatalf("Content-Type = %q, want application/problem+json", got)
	}
	if !strings.Contains(rec.Body.String(), "host_not_allowed") {
		t.Fatalf("body = %q, want host_not_allowed problem detail", rec.Body.String())
	}
}

func TestSupervisorHostAllowlistRejectsEmptyHost(t *testing.T) {
	sm := newTestSupervisorMux(t, map[string]*fakeState{})
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8372/v0/cities", nil)
	req.Host = ""
	rec := httptest.NewRecorder()

	sm.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusMisdirectedRequest {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusMisdirectedRequest, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "host_not_allowed") {
		t.Fatalf("body = %q, want host_not_allowed problem detail", rec.Body.String())
	}
}

func TestSupervisorHostAllowlistRejectsMutationWithPrivateIPOrBadHost(t *testing.T) {
	cases := []struct {
		name   string
		target string
	}{
		{"private ip", "http://192.168.1.58:8372/v0/city/thriva/bead/th-123/update"},
		{"bad host", "http://evil.example:8372/v0/city/thriva/bead/th-123/update"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sm := newTestSupervisorMux(t, map[string]*fakeState{})
			req := httptest.NewRequest(http.MethodPost, tc.target, strings.NewReader("{}"))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()

			sm.Handler().ServeHTTP(rec, req)

			if rec.Code != http.StatusMisdirectedRequest {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusMisdirectedRequest, rec.Body.String())
			}
			if strings.Contains(rec.Body.String(), "csrf") {
				t.Fatalf("body = %q, host rejection must happen before CSRF handling", rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "host_not_allowed") {
				t.Fatalf("body = %q, want host_not_allowed problem detail", rec.Body.String())
			}
		})
	}
}

func TestSupervisorHostAllowlistAcceptsLoopbackAndConfiguredHost(t *testing.T) {
	cases := []struct {
		name         string
		target       string
		allowedHosts []string
	}{
		{"localhost", "http://localhost:8372/v0/cities", nil},
		{"ipv4 loopback", "http://127.0.0.1:8372/v0/cities", nil},
		{"ipv6 loopback", "http://[::1]:8372/v0/cities", nil},
		{"configured hostname", "http://thriva-dev:8372/v0/cities", []string{"thriva-dev"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sm := newTestSupervisorMux(t, map[string]*fakeState{})
			if len(tc.allowedHosts) > 0 {
				sm.WithAllowedHosts(tc.allowedHosts)
			}
			req := httptest.NewRequest(http.MethodGet, tc.target, nil)
			rec := httptest.NewRecorder()

			sm.Handler().ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
			}
		})
	}
}

// TestDashboardSurfacesServedBehindHostAllowlist is the mount-topology guard
// for the dashboard half of #2723. The allowlist logic itself is pinned by
// TestIsAllowedSupervisorHost; what this test pins is the SupervisorMux-internal
// wiring — surfaces attached via WithStaticHandler (SPA "/" catch-all) and
// WithAPIPlane ("/api/" plane) are served INSIDE the host gate that
// SupervisorMux.Handler() wraps around the whole mux, so remounting either
// ahead of withHostAllowing within the mux fails here. It exercises only
// SupervisorMux with fake handlers and cannot see cmd/gc topology: a refactor
// that serves the real dashboard from a separate listener would bypass this
// guard entirely (production attach site: attachDashboard in
// cmd/gc/supervisor_dashboard.go).
func TestDashboardSurfacesServedBehindHostAllowlist(t *testing.T) {
	spa := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("spa-shell"))
	})
	plane := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("api-plane"))
	})
	handler := newTestSupervisorMux(t, map[string]*fakeState{}).
		WithStaticHandler(spa).
		WithAPIPlane(plane).
		Handler()

	cases := []struct {
		name     string
		path     string
		host     string
		wantCode int
		wantBody string
	}{
		{"spa loopback ipv4", "/", "127.0.0.1:8080", http.StatusOK, "spa-shell"},
		{"spa deep route loopback", "/city/thriva/agents", "127.0.0.1:8080", http.StatusOK, "spa-shell"},
		{"spa rebinding host rejected", "/", "evil.example:8080", http.StatusMisdirectedRequest, "host_not_allowed"},
		{"plane loopback", "/api/host/cities", "127.0.0.1:8080", http.StatusOK, "api-plane"},
		{"plane rebinding host rejected", "/api/host/cities", "evil.example:8080", http.StatusMisdirectedRequest, "host_not_allowed"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "http://"+tc.host+tc.path, nil)
			rec := httptest.NewRecorder()

			handler.ServeHTTP(rec, req)

			if rec.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, tc.wantCode, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tc.wantBody) {
				t.Fatalf("body = %q, want it to contain %q", rec.Body.String(), tc.wantBody)
			}
			if tc.wantCode == http.StatusMisdirectedRequest && strings.Contains(rec.Body.String(), "spa-shell") {
				t.Fatalf("body = %q, SPA content must not leak on a rejected Host", rec.Body.String())
			}
		})
	}
}

// TestSupervisorHostAllowlistRawRequestLine covers the request-line smuggling
// cases httptest.NewRequest cannot represent, against a real net/http server
// over raw TCP. Per RFC 9112 an absolute-form request-target overrides the
// Host header (Go binds r.Host to the URI authority), so a loopback Host
// header cannot be smuggled past the allowlist alongside an attacker
// authority; duplicate Host headers are rejected by net/http with 400 before
// the handler runs.
func TestSupervisorHostAllowlistRawRequestLine(t *testing.T) {
	sm := newTestSupervisorMux(t, map[string]*fakeState{})
	srv := httptest.NewServer(sm.Handler())
	defer srv.Close()

	cases := []struct {
		name     string
		request  string
		wantCode string
	}{
		{
			"absolute-uri attacker authority beats loopback host header",
			"GET http://evil.example/v0/cities HTTP/1.1\r\nHost: 127.0.0.1\r\nConnection: close\r\n\r\n",
			"421",
		},
		{
			"absolute-uri loopback authority beats attacker host header",
			"GET http://127.0.0.1/v0/cities HTTP/1.1\r\nHost: evil.example\r\nConnection: close\r\n\r\n",
			"200",
		},
		{
			// Pins net/http stdlib behavior (400 before the handler runs),
			// not repo code — kept as one canary so a Go release that
			// relaxes duplicate-Host handling surfaces here.
			"duplicate host headers rejected before handler",
			"GET /v0/cities HTTP/1.1\r\nHost: evil.example\r\nHost: 127.0.0.1\r\nConnection: close\r\n\r\n",
			"400",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conn, err := net.Dial("tcp", srv.Listener.Addr().String())
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer conn.Close() //nolint:errcheck // test cleanup
			if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
				t.Fatalf("set deadline: %v", err)
			}
			if _, err := conn.Write([]byte(tc.request)); err != nil {
				t.Fatalf("write request: %v", err)
			}
			raw, err := io.ReadAll(conn)
			if err != nil {
				t.Fatalf("read response: %v", err)
			}
			statusLine, _, _ := strings.Cut(string(raw), "\r\n")
			if !strings.Contains(statusLine, " "+tc.wantCode+" ") {
				t.Fatalf("status line = %q, want code %s; full response:\n%s", statusLine, tc.wantCode, raw)
			}
		})
	}
}

func TestSupervisorRequestAuditRecordsBoundedPayload(t *testing.T) {
	recorder := events.NewFake()
	resolver := &fakeCityResolver{
		cities:             map[string]*fakeState{},
		supervisorRecorder: recorder,
	}
	sm := NewSupervisorMux(resolver, nil, false, "test", "", time.Now())
	longPath := "/v0/" + strings.Repeat("x", 400)
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8372"+longPath+"?secret=not-recorded", nil)
	req.RemoteAddr = "192.168.1.20:49152"
	req.Header.Set("Origin", "http://localhost:8080")
	rec := httptest.NewRecorder()

	sm.Handler().ServeHTTP(rec, req)

	if len(recorder.Events) != 1 {
		t.Fatalf("recorded events = %d, want 1", len(recorder.Events))
	}
	event := recorder.Events[0]
	if event.Type != events.SupervisorRequest {
		t.Fatalf("event type = %q, want %q", event.Type, events.SupervisorRequest)
	}
	if event.Actor != "api" {
		t.Fatalf("event actor = %q, want api", event.Actor)
	}
	var payload SupervisorRequestPayload
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if payload.Method != http.MethodGet {
		t.Fatalf("method = %q, want %q", payload.Method, http.MethodGet)
	}
	if payload.Status != rec.Code {
		t.Fatalf("status = %d, want response code %d", payload.Status, rec.Code)
	}
	if len([]rune(payload.Path)) > 256 {
		t.Fatalf("path length = %d, want <= 256", len([]rune(payload.Path)))
	}
	if strings.Contains(payload.Path, "secret") {
		t.Fatalf("path = %q, must not include query string", payload.Path)
	}
	if payload.RemoteAddrClass != "private" {
		t.Fatalf("remote addr class = %q, want private", payload.RemoteAddrClass)
	}
	if payload.Host != "127.0.0.1" {
		t.Fatalf("host = %q, want 127.0.0.1", payload.Host)
	}
	if !payload.OriginAllowed {
		t.Fatal("origin_allowed = false, want true")
	}
	if payload.Phase != supervisorRequestPhaseComplete {
		t.Fatalf("phase = %q, want %q", payload.Phase, supervisorRequestPhaseComplete)
	}
	// G9: the audit record carries the server-minted X-GC-Request-Id that was
	// also echoed to the client, so the two can be correlated.
	minted := rec.Header().Get("X-GC-Request-Id")
	if minted == "" {
		t.Fatal("response is missing the minted X-GC-Request-Id header")
	}
	if payload.RequestID != minted {
		t.Fatalf("payload request_id = %q, want the minted header %q", payload.RequestID, minted)
	}
}

func TestSupervisorRequestAuditRecordsEventStreamStartBeforeClose(t *testing.T) {
	recorder := events.NewFake()
	resolver := &fakeCityResolver{
		cities:             map[string]*fakeState{},
		supervisorRecorder: recorder,
	}
	sm := NewSupervisorMux(resolver, nil, false, "test", "", time.Now())

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8372/v0/events/stream", nil).WithContext(ctx)
	req.Header.Set("Accept", "text/event-stream")
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		sm.Handler().ServeHTTP(rec, req)
	}()

	start := waitForSupervisorRequestPhase(t, recorder, supervisorRequestPhaseStart)
	if start.Status != 0 {
		t.Fatalf("start status = %d, want 0 before response status is known", start.Status)
	}
	if start.DurationMs != 0 {
		t.Fatalf("start duration_ms = %d, want 0", start.DurationMs)
	}
	if start.Path != "/v0/events/stream" {
		t.Fatalf("start path = %q, want /v0/events/stream", start.Path)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("stream did not close after context cancellation")
	}
	complete := waitForSupervisorRequestPhase(t, recorder, supervisorRequestPhaseComplete)
	if complete.Status != http.StatusOK {
		t.Fatalf("complete status = %d, want %d", complete.Status, http.StatusOK)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("response status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
}

func waitForSupervisorRequestPhase(t *testing.T, provider events.Provider, phase string) SupervisorRequestPayload {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		evts, err := provider.List(events.Filter{Type: events.SupervisorRequest})
		if err != nil {
			t.Fatalf("list supervisor request events: %v", err)
		}
		for _, event := range evts {
			var payload SupervisorRequestPayload
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				t.Fatalf("unmarshal payload: %v", err)
			}
			if payload.Phase == phase {
				return payload
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for supervisor.request phase %q", phase)
	return SupervisorRequestPayload{}
}
