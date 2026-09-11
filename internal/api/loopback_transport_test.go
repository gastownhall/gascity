package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestSupervisorMuxLoopbackTransportBypassesReadAuth is the regression test for
// the read-auth review finding: the dashboard /api plane's loopback self-reads
// of the gated /v0/city/{name}/status route must keep working when read-auth is
// enabled. LoopbackTransport dispatches the trusted self-read against the
// un-gated inner handler, so it clears the gate and serves the status; the same
// read over the network listener without a grant is still rejected 401, so the
// bypass is scoped to in-process self-reads and does not weaken the gate.
func TestSupervisorMuxLoopbackTransportBypassesReadAuth(t *testing.T) {
	pub, _ := mustKeypair(t)
	sm := newTestSupervisorMux(t, map[string]*fakeState{"test-city": newFakeState(t)})
	sm.WithAnyHostAllowed().WithReadAuth(newTestReadVerifier(t, pub, time.Now()))

	const target = "/v0/city/test-city/status"

	// In-process loopback transport: the trusted self-read bypasses the gate and
	// reaches the status handler, even though no read grant is presented.
	client := &http.Client{Transport: sm.LoopbackTransport()}
	resp, err := client.Get("http://supervisor.local" + target)
	if err != nil {
		t.Fatalf("loopback get: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("loopback self-read under read-auth: status=%d want 200 (gate must be bypassed); body=%s", resp.StatusCode, body)
	}

	// The same read over the network listener without a grant is still gated,
	// proving the bypass is confined to the in-process transport.
	srv := httptest.NewServer(sm.Handler())
	defer srv.Close()
	netResp, err := http.Get(srv.URL + target)
	if err != nil {
		t.Fatalf("network get: %v", err)
	}
	defer func() { _ = netResp.Body.Close() }()
	if netResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("network read without grant: status=%d want 401 (gate must stay active)", netResp.StatusCode)
	}
}

// TestSupervisorMuxLoopbackTransportContainsPanics proves a handler panic on the
// in-process path is contained as a 500 rather than crashing the self-read
// caller's goroutine — the inner handler runs without the outer recovery
// middleware, so loopbackTransport must recover itself.
func TestSupervisorMuxLoopbackTransportContainsPanics(t *testing.T) {
	panicking := loopbackTransport{h: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	})}
	req := httptest.NewRequest(http.MethodGet, "/v0/city/acme/status", nil)
	resp, err := panicking.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip returned a transport error instead of containing the panic: %v", err)
	}
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("contained panic status=%d want 500", resp.StatusCode)
	}
}
