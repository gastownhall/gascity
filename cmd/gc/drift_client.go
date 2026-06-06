package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// httpSupervisorClient is the production SupervisorClient implementation.
// It calls the supervisor's /health endpoint over plain HTTP to discover
// build identity and to verify post-restart readiness.
//
// /health is the right endpoint for both purposes: it is the supervisor's
// canonical liveness probe (no auth, no per-city scope, fast), and it
// reports build_id which is the load-bearing field for drift detection.
type httpSupervisorClient struct {
	baseURL    string
	httpClient *http.Client
}

// newHTTPSupervisorClient returns a client targeting baseURL. The base
// URL must include scheme and authority (e.g. "http://127.0.0.1:8080");
// /health is appended at request time.
func newHTTPSupervisorClient(baseURL string) *httpSupervisorClient {
	return &httpSupervisorClient{
		baseURL: strings.TrimSuffix(baseURL, "/"),
		httpClient: &http.Client{
			Timeout: 5 * time.Second,
		},
	}
}

// Status fetches /health and projects the response onto SupervisorStatus.
// PackRoots are not yet exposed by /health (deferred follow-up); callers
// receive an empty slice and DetectPackDrift treats that as no drift.
func (c *httpSupervisorClient) Status(ctx context.Context) (SupervisorStatus, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/health", nil)
	if err != nil {
		return SupervisorStatus{}, err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return SupervisorStatus{}, err
	}
	defer resp.Body.Close() //nolint:errcheck // best-effort close
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return SupervisorStatus{}, fmt.Errorf("supervisor /health returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var body struct {
		BuildID   string `json:"build_id"`
		UptimeSec int    `json:"uptime_sec"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return SupervisorStatus{}, fmt.Errorf("decoding supervisor /health: %w", err)
	}
	return SupervisorStatus{BuildID: body.BuildID, UptimeSec: body.UptimeSec}, nil
}

// Ping issues a GET /health and returns nil iff the response is 2xx.
// Used by PollReady after RestartSupervisor to wait for the new
// supervisor process to come up.
func (c *httpSupervisorClient) Ping(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/health", nil)
	if err != nil {
		return err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close() //nolint:errcheck // best-effort close
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("supervisor /health returned %d", resp.StatusCode)
	}
	return nil
}
