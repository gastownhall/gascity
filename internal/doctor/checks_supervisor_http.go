package doctor

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"syscall"
	"time"

	"github.com/gastownhall/gascity/internal/supervisor"
)

// httpDoer abstracts the HTTP client so tests can inject a mock.
type httpDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// SupervisorHTTPCheck verifies that the supervisor HTTP API is reachable on
// its configured port. The check is skipped when the supervisor unix socket
// is already known to be down so the operator sees one clear problem rather
// than two.
type SupervisorHTTPCheck struct {
	supervisorRunning bool
	loadConfig        func(string) (supervisor.Config, error)
	configPath        func() string
	client            httpDoer
}

// NewSupervisorHTTPCheck returns a check configured to probe the supervisor
// HTTP API. supervisorRunning should come from the unix-socket probe result.
func NewSupervisorHTTPCheck(supervisorRunning bool) *SupervisorHTTPCheck {
	return &SupervisorHTTPCheck{
		supervisorRunning: supervisorRunning,
		loadConfig:        supervisor.LoadConfig,
		configPath:        supervisor.ConfigPath,
		client:            &http.Client{Timeout: 3 * time.Second},
	}
}

// Name returns the check identifier.
func (c *SupervisorHTTPCheck) Name() string { return "supervisor-http-api" }

// CanFix reports that this check does not support automatic remediation.
func (c *SupervisorHTTPCheck) CanFix() bool { return false }

// Fix is a no-op; CanFix returns false.
func (c *SupervisorHTTPCheck) Fix(_ *CheckContext) error { return nil }

// Run checks that the supervisor HTTP API is reachable on its configured port.
func (c *SupervisorHTTPCheck) Run(_ *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name()}
	if !c.supervisorRunning {
		r.Status = StatusOK
		r.Message = "supervisor socket not running — HTTP API check skipped"
		return r
	}

	cfg, err := c.loadConfig(c.configPath())
	if err != nil {
		r.Status = StatusError
		r.Message = fmt.Sprintf("cannot load supervisor config: %v", err)
		return r
	}
	port := cfg.Supervisor.PortOrDefault()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
		fmt.Sprintf("http://127.0.0.1:%d/v0/cities", port), nil)
	if err != nil {
		r.Status = StatusError
		r.Message = fmt.Sprintf("supervisor HTTP API on port %d: %v", port, err)
		return r
	}

	resp, err := c.client.Do(req)
	if err != nil {
		if isConnectionRefused(err) {
			r.Status = StatusError
			r.Message = fmt.Sprintf("supervisor HTTP API on port %d: connection refused", port)
			return r
		}
		if isTimeout(err) {
			r.Status = StatusError
			r.Message = fmt.Sprintf("supervisor HTTP API on port %d: timeout", port)
			return r
		}
		r.Status = StatusError
		r.Message = fmt.Sprintf("supervisor HTTP API on port %d: %v", port, err)
		return r
	}
	defer resp.Body.Close() //nolint:errcheck // best-effort body close

	if resp.StatusCode/100 != 2 {
		r.Status = StatusError
		r.Message = fmt.Sprintf("supervisor HTTP API on port %d: non-2xx HTTP %d", port, resp.StatusCode)
		return r
	}

	r.Status = StatusOK
	r.Message = fmt.Sprintf("supervisor socket OK, HTTP API reachable on port %d", port)
	return r
}

func isConnectionRefused(err error) bool {
	if errors.Is(err, syscall.ECONNREFUSED) {
		return true
	}
	return strings.Contains(err.Error(), "connection refused")
}

func isTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr interface{ Timeout() bool }
	if errors.As(err, &netErr) {
		return netErr.Timeout()
	}
	return false
}
