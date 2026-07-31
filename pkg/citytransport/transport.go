package citytransport

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
	"time"
)

// maxResponseBytes caps how much of a response body we read. A server that
// streams unbounded bytes at a producer must not be able to exhaust it.
const maxResponseBytes = 1 << 20

// Doer is the minimal HTTP surface the transport needs. Tests supply a stub;
// production supplies an *http.Client.
type Doer interface {
	Do(req *http.Request) (*http.Response, error)
}

// Config configures a Client. BaseURL and TokenProvider are required.
type Config struct {
	// BaseURL is the API root, e.g. https://bts.example/api/v1.
	BaseURL string
	// TokenProvider supplies the bearer per attempt, so a file-backed token can
	// rotate out of band mid-flight. It is consulted on EVERY attempt, which is
	// what makes an in-flight rotation recoverable by retry alone: a rotation
	// changes the credential and nothing else — not the source identity, not
	// the epoch, not any cursor.
	TokenProvider func(context.Context) (string, error)
	// MaxAttempts bounds total attempts per upload, including the first
	// (default 4). Retries are bounded so a failing sink applies backpressure
	// instead of spinning.
	MaxAttempts int
	// BaseBackoff is the first retry delay; each subsequent retry doubles it
	// (default 250ms).
	BaseBackoff time.Duration
	// MaxBackoff caps the per-retry delay (default 30s).
	MaxBackoff time.Duration
	HTTP       Doer
	// Sleep is the delay hook, injectable so tests do not wait in real time.
	Sleep func(context.Context, time.Duration) error
}

// Client posts uploads. It performs no ordering, no policy evaluation, and no
// checkpoint bookkeeping; a Send either returns a decoded Ack or a typed error.
type Client struct {
	cfg Config
}

// NewClient builds a Client, applying defaults and validating required config.
func NewClient(cfg Config) (*Client, error) {
	if strings.TrimSpace(cfg.BaseURL) == "" {
		return nil, errors.New("citytransport: BaseURL is required")
	}
	if _, err := url.Parse(cfg.BaseURL); err != nil {
		return nil, fmt.Errorf("citytransport: BaseURL: %w", err)
	}
	if cfg.TokenProvider == nil {
		return nil, errors.New("citytransport: TokenProvider is required")
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = 4
	}
	if cfg.BaseBackoff <= 0 {
		cfg.BaseBackoff = 250 * time.Millisecond
	}
	if cfg.MaxBackoff <= 0 {
		cfg.MaxBackoff = 30 * time.Second
	}
	if cfg.HTTP == nil {
		cfg.HTTP = &http.Client{Timeout: 30 * time.Second}
	}
	if cfg.Sleep == nil {
		cfg.Sleep = sleepCtx
	}
	return &Client{cfg: cfg}, nil
}

// Send posts one upload for cityHash and returns the decoded acknowledgement.
//
// cityHash is the salted partition key; the cleartext city name must never be
// passed here. Every failure path returns a typed error and no Ack, so a caller
// that only advances state on a nil error can never advance on a failure.
func (c *Client) Send(ctx context.Context, cityHash string, up Upload) (Ack, error) {
	if strings.TrimSpace(cityHash) == "" {
		return Ack{}, errors.New("citytransport: cityHash is required")
	}
	body, err := EncodeUpload(up)
	if err != nil {
		return Ack{}, fmt.Errorf("citytransport: encode upload: %w", err)
	}
	endpoint := strings.TrimRight(c.cfg.BaseURL, "/") + "/city/" + url.PathEscape(cityHash) + "/events"

	backoff := c.cfg.BaseBackoff
	var lastErr error
	for attempt := 1; attempt <= c.cfg.MaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return Ack{}, fmt.Errorf("%w: %w", ErrCanceled, err)
		}
		ack, retryable, err := c.attempt(ctx, endpoint, body)
		if err == nil {
			return ack, nil
		}
		lastErr = err
		if !retryable {
			return Ack{}, err
		}
		if attempt == c.cfg.MaxAttempts {
			break
		}
		if err := c.cfg.Sleep(ctx, backoff); err != nil {
			return Ack{}, fmt.Errorf("%w: %w", ErrCanceled, err)
		}
		if backoff *= 2; backoff > c.cfg.MaxBackoff {
			backoff = c.cfg.MaxBackoff
		}
	}
	return Ack{}, fmt.Errorf("%w after %d attempts: %w", ErrRetriesExhausted, c.cfg.MaxAttempts, lastErr)
}

// attempt performs one request. It reports whether the failure is worth
// retrying: transport faults and 5xx/429 are; a 4xx problem is a decision the
// server already made, and a malformed body is a protocol break.
func (c *Client) attempt(ctx context.Context, endpoint string, body []byte) (Ack, bool, error) {
	token, err := c.cfg.TokenProvider(ctx)
	if err != nil {
		// Retryable: a token file mid-rotation reads as an error for a moment.
		return Ack{}, true, fmt.Errorf("%w: %w", ErrCredential, err)
	}
	if strings.TrimSpace(token) == "" {
		return Ack{}, true, fmt.Errorf("%w: empty token", ErrCredential)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return Ack{}, false, fmt.Errorf("citytransport: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, application/problem+json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := c.cfg.HTTP.Do(req)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return Ack{}, false, fmt.Errorf("%w: %w", ErrCanceled, ctxErr)
		}
		return Ack{}, true, fmt.Errorf("citytransport: post: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBytes))
		_ = resp.Body.Close()
	}()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return Ack{}, true, fmt.Errorf("citytransport: read response: %w", err)
	}

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		ack, err := DecodeAck(raw)
		if err != nil {
			// A 2xx we cannot decode is NOT a success. Returning it as an error
			// is what stops a caller from treating an unparseable body as an
			// acknowledgement.
			return Ack{}, false, err
		}
		return ack, false, nil
	case resp.StatusCode == http.StatusTooManyRequests, resp.StatusCode >= 500:
		return Ack{}, true, decodeProblem(raw, resp.StatusCode)
	default:
		return Ack{}, false, decodeProblem(raw, resp.StatusCode)
	}
}

// decodeProblem turns an error response into a *Problem. A body that is not a
// problem document still yields a Problem carrying the status, so callers always
// get the same type.
func decodeProblem(raw []byte, status int) error {
	var p Problem
	dec := json.NewDecoder(bytes.NewReader(raw))
	// Problems are extensible by RFC 9457, so unknown members are tolerated here
	// — unlike the closed Ack/Upload schemas.
	if err := dec.Decode(&p); err != nil || p.Status == 0 && p.Title == "" {
		return &Problem{
			Type:   "about:blank",
			Title:  http.StatusText(status),
			Status: status,
			Detail: truncate(string(raw), 512),
		}
	}
	if p.Status == 0 {
		p.Status = status
	}
	return &p
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
