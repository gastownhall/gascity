//go:build integration

// Package dashport_test is the Go serve-level (Layer A) e2e harness for the
// dashboard. It stands up the real supervisor stack — the typed /v0 API, the
// host-side /api plane, and the embedded SPA — over a seeded event log + bead
// store via api.ServeSeededCity, then drives the exact endpoints each dashboard
// view consumes and asserts the projected JSON. It is the layer that catches the
// run-view class of regression: a projection break is visible at the Go wire
// level here even when every request still returns 200.
//
// Layer B (the Playwright render smoke) shares this package's testdata/dashport
// corpus through the same api.ServeSeededCity seam; see .dashport-plan/04-e2e.md.
package dashport_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// harness is a running seeded-city server plus the collaborators a test asserts
// against. It owns the httptest.Server lifecycle; the t.Cleanup hooks registered
// in newHarness shut the listener and then drain the plane's run tailers.
type harness struct {
	t        *testing.T
	server   *httptest.Server
	cityName string
	cityPath string
	client   *http.Client
}

// newHarness seeds a city from testdata/dashport and serves the full supervisor
// stack over an httptest.Server. The plane's per-city run tailers are started
// against the test context and drained deterministically by a t.Cleanup hook
// that calls the seam's stop function after the server is closed.
func newHarness(t *testing.T) *harness {
	t.Helper()

	fx := loadFixtures(t)

	// A two-phase start: the host-side status samplers dial the stack's own
	// loopback base URL, which is only known after httptest.NewServer binds. We
	// build the handler with an empty base URL (the run tailers read the event
	// log off disk and do not need it), which is all Layer A asserts; the status
	// endpoint itself is served by the typed /v0 plane, not the samplers.
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	handler, stop, err := serveSeededCity(ctx, fx)
	if err != nil {
		t.Fatalf("ServeSeededCity: %v", err)
	}
	// Registered before srv.Close so cleanup runs LIFO: close the server first
	// (no in-flight requests), then drain the plane's goroutines via stop, then
	// cancel the parent ctx.
	t.Cleanup(stop)

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	return &harness{
		t:        t,
		server:   srv,
		cityName: fx.CityName,
		cityPath: fx.CityPath,
		client:   srv.Client(),
	}
}

// cityURL builds a full URL for a city-scoped typed /v0 path (leading slash
// required), e.g. cityURL("/workflow/run-anchor").
func (h *harness) cityURL(path string) string {
	return h.server.URL + "/v0/city/" + h.cityName + path
}

// apiURL builds a full URL for a host-side /api plane path scoped to the city
// (leading slash on the suffix required), e.g. apiURL("/runs/summary").
func (h *harness) apiURL(suffix string) string {
	return h.server.URL + "/api/city/" + h.cityName + suffix
}

// rootURL builds a full URL against the served root (SPA + reserved prefixes).
func (h *harness) rootURL(path string) string {
	return h.server.URL + path
}

// getJSON GETs url, asserts a 200, and decodes the body into out. out must be a
// pointer to a generated Go wire type (internal/api/genclient) or a runproj
// projection struct — never map[string]any — so a wire-shape drift fails at
// compile time (the field the assertion reads no longer exists on the struct)
// rather than silently decoding to nil.
func (h *harness) getJSON(url string, out any) {
	h.t.Helper()
	resp, err := h.client.Get(url)
	if err != nil {
		h.t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close() //nolint:errcheck
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		h.t.Fatalf("GET %s: status = %d, want 200 (body: %s)", url, resp.StatusCode, truncate(body))
	}
	if out == nil {
		return
	}
	if err := json.Unmarshal(body, out); err != nil {
		h.t.Fatalf("GET %s: decode into %T: %v (body: %s)", url, out, err, truncate(body))
	}
}

// getRaw GETs url and returns the status code and raw body without decoding.
func (h *harness) getRaw(url string) (int, []byte) {
	h.t.Helper()
	resp, err := h.client.Get(url)
	if err != nil {
		h.t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close() //nolint:errcheck
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body
}

// streamStatus opens an SSE endpoint, reads at most one frame, and returns the
// response status. The stream stays open by design (it long-polls for new
// events), so the read is bounded by a short context deadline; a deadline hit
// after a 200 is success — it means the stream was serving. This mirrors the way
// the in-package SSE handler tests bound the read with a cancelable context.
func (h *harness) streamStatus(url string) int {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 750*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		h.t.Fatalf("new stream request %s: %v", url, err)
	}
	resp, err := h.client.Do(req)
	if err != nil {
		h.t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close() //nolint:errcheck
	// Drain one small chunk (or hit the deadline); either way the status is the
	// signal we assert on.
	buf := make([]byte, 256)
	_, _ = resp.Body.Read(buf)
	return resp.StatusCode
}

// status returns the status code for a method+url without a body, used for the
// reserved-prefix and CSRF invariant checks.
func (h *harness) status(method, url string) int {
	h.t.Helper()
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		h.t.Fatalf("new request %s %s: %v", method, url, err)
	}
	resp, err := h.client.Do(req)
	if err != nil {
		h.t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close() //nolint:errcheck
	return resp.StatusCode
}

func truncate(b []byte) string {
	const max = 300
	if len(b) > max {
		return string(b[:max]) + "..."
	}
	return string(b)
}
