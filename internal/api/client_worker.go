package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
)

// Client-side legs for the worker lifecycle family (cr-gdeav.5.4 draft).
//
// These requests are hand-built rather than generated because the four routes
// are new: the generated client is a projection of the committed OpenAPI spec,
// so the ops appear there only once the spec is regenerated (the EDGE PR's
// `make check-generated-docs-drift` step). Everything that matters about the
// transport is still the remote client's: the CSRF header, the live bearer, the
// per-request city-write grant, and the REST transport with its re-auth
// RoundTripper — which is why this file uses c.restClient instead of building
// an http.Client of its own.

// WorkerVerbRequest is the body the claim / heartbeat / release legs send. The
// claimant is the caller's identity (BEADS_ACTOR on the CLI), not the transport
// credential, so a shared bearer cannot silently become the owner of a bead.
type WorkerVerbRequest struct {
	SessionID string `json:"session_id,omitempty"`
	Assignee  string `json:"assignee"`
	BeadID    string `json:"bead_id"`
}

// WorkerCloseRequest is the body the typed close sends. Outcome carries the
// ADR-0009 disposition; Commit/Branch only accompany a shipped close.
type WorkerCloseRequest struct {
	// SessionID and Assignee carry the worker's ownership into the close, which
	// is what lets the server refuse a stranger's close instead of writing
	// whatever work record the caller typed.
	SessionID string `json:"session_id,omitempty"`
	Assignee  string `json:"assignee"`
	BeadID    string `json:"bead_id"`
	Outcome   string `json:"outcome"`
	Commit    string `json:"commit,omitempty"`
	Branch    string `json:"branch,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

// WorkerVerbResult is what a worker verb reports back. Status is the verb's own
// result vocabulary (claimed / renewed / released / skipped / closed) and Bead
// is the row as the server persisted it, so the CLI can answer with the same
// shape its local leg does instead of reading again.
type WorkerVerbResult struct {
	Status   string
	Bead     beads.Bead
	Revision string // precondition token in plain form ("" when the store has none)
	// Skipped reports a release that found a different holder — a result, not
	// an error, which is why it is a field rather than an err.
	Skipped bool
}

// WorkerClaim acquires a bead for assignee over POST /worker/claim.
func (c *Client) WorkerClaim(ctx context.Context, req WorkerVerbRequest) (WorkerVerbResult, error) {
	return c.doWorkerVerb(ctx, http.MethodPost, "/worker/claim", req)
}

// WorkerHeartbeat refreshes the lease over POST /worker/heartbeat.
func (c *Client) WorkerHeartbeat(ctx context.Context, req WorkerVerbRequest) (WorkerVerbResult, error) {
	return c.doWorkerVerb(ctx, http.MethodPost, "/worker/heartbeat", req)
}

// WorkerRelease hands a claim back over DELETE /worker/claim. A different
// holder is Skipped with a nil error.
func (c *Client) WorkerRelease(ctx context.Context, req WorkerVerbRequest) (WorkerVerbResult, error) {
	return c.doWorkerVerb(ctx, http.MethodDelete, "/worker/claim", req)
}

// WorkerClose closes with a typed work record over POST /worker/close.
func (c *Client) WorkerClose(ctx context.Context, req WorkerCloseRequest) (WorkerVerbResult, error) {
	return c.doWorkerVerb(ctx, http.MethodPost, "/worker/close", req)
}

// workerVerbResponse is the server's response envelope.
type workerVerbResponse struct {
	Status string     `json:"status"`
	Bead   beads.Bead `json:"bead"`
}

func (c *Client) doWorkerVerb(ctx context.Context, method, tail string, payload any) (WorkerVerbResult, error) {
	var out WorkerVerbResult
	if err := c.requireCityScope(); err != nil {
		return out, err
	}
	if !c.isRemote || c.restClient == nil {
		return out, fmt.Errorf("api: worker lifecycle verbs are remote-only; build the client with NewRemoteCityScopedClient")
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return out, fmt.Errorf("api: encoding worker request: %w", err)
	}
	endpoint, err := url.JoinPath(strings.TrimRight(c.baseURL, "/"), "/v0/city/", c.cityName, strings.TrimPrefix(tail, "/"))
	if err != nil {
		return out, fmt.Errorf("api: building worker URL for %q: %w", tail, err)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
	if err != nil {
		return out, fmt.Errorf("api: building worker request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GC-Request", "true")
	if tok, err := c.bearerToken(); err != nil {
		return out, err
	} else if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	if err := c.attachCityWriteGrant(req); err != nil {
		return out, err
	}

	resp, err := c.restClient.Do(req)
	if err != nil {
		return out, fmt.Errorf("api: %s %s: %w", method, tail, err)
	}
	defer resp.Body.Close() //nolint:errcheck // read below
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return out, fmt.Errorf("api: reading %s %s response: %w", method, tail, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return out, &WorkerVerbHTTPError{Method: method, Path: tail, StatusCode: resp.StatusCode, Body: snippet(raw)}
	}
	var parsed workerVerbResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return out, fmt.Errorf("api: decoding %s %s output: %w (%s)", method, tail, err, snippet(raw))
	}
	out.Status = parsed.Status
	out.Bead = parsed.Bead
	out.Skipped = parsed.Status == "skipped"
	out.Revision = resp.Header.Get("X-GC-Revision")
	return out, nil
}

// WorkerVerbHTTPError is a non-2xx answer from a worker route. The status code
// stays a NUMBER because the caller branches on it: a 409 lost claim is a
// normal unwind (exit non-zero, say who holds it), a 501 means the city's store
// cannot serve the verb at all, and a 401/403 is a grant problem — flattening
// them into one string would throw away the only signal that separates them.
type WorkerVerbHTTPError struct {
	Method     string
	Path       string
	StatusCode int
	Body       string
}

func (e *WorkerVerbHTTPError) Error() string {
	return fmt.Sprintf("api: %s %s: %d %s: %s", e.Method, e.Path, e.StatusCode, http.StatusText(e.StatusCode), e.Body)
}

// WorkerVerbStatusCode reports the HTTP status a worker verb came back with,
// and whether the failure was an HTTP answer at all.
func WorkerVerbStatusCode(err error) (int, bool) {
	var httpErr *WorkerVerbHTTPError
	if errors.As(err, &httpErr) {
		return httpErr.StatusCode, true
	}
	return 0, false
}

func snippet(raw []byte) string {
	s := strings.TrimSpace(string(raw))
	if len(s) > 400 {
		return s[:400] + "\u2026"
	}
	return s
}
