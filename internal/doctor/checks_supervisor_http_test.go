package doctor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"syscall"
	"testing"

	"github.com/gastownhall/gascity/internal/supervisor"
)

type mockHTTPDoer struct {
	resp *http.Response
	err  error
}

func (m *mockHTTPDoer) Do(*http.Request) (*http.Response, error) {
	return m.resp, m.err
}

func httpOKResponse() *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader("")),
	}
}

func httpStatusResponse(code int) *http.Response {
	return &http.Response{
		StatusCode: code,
		Body:       io.NopCloser(strings.NewReader("")),
	}
}

// makeSupervisorHTTPCheck builds a SupervisorHTTPCheck with injected config
// loader and HTTP client. port=0 exercises the PortOrDefault() path (returns 8372).
func makeSupervisorHTTPCheck(supervisorRunning bool, port int, doer httpDoer) *SupervisorHTTPCheck {
	cfg := supervisor.Config{Supervisor: supervisor.Section{Port: port}}
	return &SupervisorHTTPCheck{
		supervisorRunning: supervisorRunning,
		loadConfig:        func(_ string) (supervisor.Config, error) { return cfg, nil },
		configPath:        func() string { return "unused" },
		client:            doer,
	}
}

func TestSupervisorHTTPCheck_Name(t *testing.T) {
	c := NewSupervisorHTTPCheck(false)
	if c.Name() != "supervisor-http-api" {
		t.Errorf("Name() = %q, want %q", c.Name(), "supervisor-http-api")
	}
}

func TestSupervisorHTTPCheck_CanFix(t *testing.T) {
	c := NewSupervisorHTTPCheck(false)
	if c.CanFix() {
		t.Error("CanFix() = true, want false")
	}
}

func TestSupervisorHTTPCheck_WarmupEligible(t *testing.T) {
	c := NewSupervisorHTTPCheck(false)
	if c.WarmupEligible() {
		t.Error("WarmupEligible() = true, want false")
	}
}

// TestSupervisorHTTPCheck_SkipWhenSupervisorNotRunning verifies the check
// returns OK with a skip message when the unix socket is unreachable, so the
// operator sees one clear problem (socket failure) rather than two.
func TestSupervisorHTTPCheck_SkipWhenSupervisorNotRunning(t *testing.T) {
	c := makeSupervisorHTTPCheck(false, 8372, nil)
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Errorf("status = %d, want OK; msg = %s", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "skipped") {
		t.Errorf("message = %q, want 'skipped'", r.Message)
	}
}

// TestSupervisorHTTPCheck_OKReachableOnConfiguredPort verifies a 2xx response
// on a non-default port produces the expected success message with the port number.
func TestSupervisorHTTPCheck_OKReachableOnConfiguredPort(t *testing.T) {
	const port = 9001
	c := makeSupervisorHTTPCheck(true, port, &mockHTTPDoer{resp: httpOKResponse()})
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Fatalf("status = %d, want OK; msg = %s", r.Status, r.Message)
	}
	want := fmt.Sprintf("supervisor socket OK, HTTP API reachable on port %d", port)
	if r.Message != want {
		t.Errorf("message = %q, want %q", r.Message, want)
	}
}

// TestSupervisorHTTPCheck_OKReachableUsesPortOrDefault verifies that when no
// port is configured (Port=0), PortOrDefault() returns 8372 and the message
// reflects that default.
func TestSupervisorHTTPCheck_OKReachableUsesPortOrDefault(t *testing.T) {
	c := makeSupervisorHTTPCheck(true, 0, &mockHTTPDoer{resp: httpOKResponse()})
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Fatalf("status = %d, want OK; msg = %s", r.Status, r.Message)
	}
	want := fmt.Sprintf("supervisor socket OK, HTTP API reachable on port %d", 8372)
	if r.Message != want {
		t.Errorf("message = %q, want %q", r.Message, want)
	}
}

// TestSupervisorHTTPCheck_ErrorConnectionRefused verifies a connection-refused
// error produces a StatusError with "connection refused" and the port number.
func TestSupervisorHTTPCheck_ErrorConnectionRefused(t *testing.T) {
	const port = 9001
	c := makeSupervisorHTTPCheck(true, port, &mockHTTPDoer{err: syscall.ECONNREFUSED})
	r := c.Run(&CheckContext{})
	if r.Status != StatusError {
		t.Fatalf("status = %d, want Error; msg = %s", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "connection refused") {
		t.Errorf("message = %q, want 'connection refused'", r.Message)
	}
	if !strings.Contains(r.Message, fmt.Sprintf("%d", port)) {
		t.Errorf("message = %q, want port %d", r.Message, port)
	}
}

// TestSupervisorHTTPCheck_ErrorTimeout verifies that a context.DeadlineExceeded
// produces a StatusError with "timeout" and the port number.
func TestSupervisorHTTPCheck_ErrorTimeout(t *testing.T) {
	const port = 9001
	c := makeSupervisorHTTPCheck(true, port, &mockHTTPDoer{err: context.DeadlineExceeded})
	r := c.Run(&CheckContext{})
	if r.Status != StatusError {
		t.Fatalf("status = %d, want Error; msg = %s", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "timeout") {
		t.Errorf("message = %q, want 'timeout'", r.Message)
	}
	if !strings.Contains(r.Message, fmt.Sprintf("%d", port)) {
		t.Errorf("message = %q, want port %d", r.Message, port)
	}
}

// TestSupervisorHTTPCheck_ErrorNonTwoXX verifies that a non-2xx HTTP status
// (e.g., 503) produces a StatusError with the status code and port.
func TestSupervisorHTTPCheck_ErrorNonTwoXX(t *testing.T) {
	const port = 9001
	c := makeSupervisorHTTPCheck(true, port, &mockHTTPDoer{resp: httpStatusResponse(http.StatusServiceUnavailable)})
	r := c.Run(&CheckContext{})
	if r.Status != StatusError {
		t.Fatalf("status = %d, want Error; msg = %s", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, fmt.Sprintf("non-2xx HTTP %d", http.StatusServiceUnavailable)) {
		t.Errorf("message = %q, want 'non-2xx HTTP 503'", r.Message)
	}
	if !strings.Contains(r.Message, fmt.Sprintf("%d", port)) {
		t.Errorf("message = %q, want port %d", r.Message, port)
	}
}

// TestSupervisorHTTPCheck_ErrorConfigLoad verifies that a config-load failure
// produces a StatusError even before the HTTP probe.
func TestSupervisorHTTPCheck_ErrorConfigLoad(t *testing.T) {
	configErr := fmt.Errorf("permission denied reading supervisor.toml")
	c := &SupervisorHTTPCheck{
		supervisorRunning: true,
		loadConfig:        func(_ string) (supervisor.Config, error) { return supervisor.Config{}, configErr },
		configPath:        func() string { return "unused" },
		client:            nil, // must not be called
	}
	r := c.Run(&CheckContext{})
	if r.Status != StatusError {
		t.Fatalf("status = %d, want Error; msg = %s", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "permission denied") {
		t.Errorf("message = %q, want config error text", r.Message)
	}
}
