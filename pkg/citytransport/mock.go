package citytransport

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"sync"
	"time"
)

// MockDoer is a scripted Doer for fault injection. Each entry in Responses is
// consumed by one attempt in order; once exhausted the last entry repeats, so a
// permanently-failing sink is expressible with a single entry.
//
// It is exported (not test-only) because the mapping layer's fault drills need
// the same scripted transport, and duplicating it there would let the two copies
// drift.
type MockDoer struct {
	Responses []MockResponse

	mu       sync.Mutex
	attempts int
	// Bodies records every request body seen, so a test can assert what was (or
	// was not) offered.
	Bodies [][]byte
}

// MockResponse is one scripted attempt: either a transport error or a status
// plus body.
type MockResponse struct {
	Err    error
	Status int
	Body   string
	// Delay, if set, blocks the attempt so a test can drive cancellation.
	Delay time.Duration
}

// Do implements Doer.
func (m *MockDoer) Do(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		body, _ = io.ReadAll(req.Body)
	}

	m.mu.Lock()
	idx := m.attempts
	m.attempts++
	m.Bodies = append(m.Bodies, body)
	if len(m.Responses) == 0 {
		m.mu.Unlock()
		return nil, io.ErrUnexpectedEOF
	}
	if idx >= len(m.Responses) {
		idx = len(m.Responses) - 1
	}
	r := m.Responses[idx]
	m.mu.Unlock()

	if r.Delay > 0 {
		select {
		case <-time.After(r.Delay):
		case <-req.Context().Done():
			return nil, req.Context().Err()
		}
	}
	if r.Err != nil {
		return nil, r.Err
	}
	status := r.Status
	if status == 0 {
		status = http.StatusOK
	}
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(bytes.NewReader([]byte(r.Body))),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Request:    req,
	}, nil
}

// Attempts returns how many requests the mock has served.
func (m *MockDoer) Attempts() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.attempts
}

// StaticToken is a TokenProvider returning a fixed credential.
func StaticToken(tok string) func(context.Context) (string, error) {
	return func(context.Context) (string, error) { return tok, nil }
}

// RotatingToken is a TokenProvider that walks a list of credentials, one per
// call, repeating the last. It models an out-of-band rotation landing between
// two attempts of the same upload.
func RotatingToken(tokens ...string) func(context.Context) (string, error) {
	var mu sync.Mutex
	i := 0
	return func(context.Context) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		if len(tokens) == 0 {
			return "", nil
		}
		t := tokens[i]
		if i < len(tokens)-1 {
			i++
		}
		return t, nil
	}
}

// NoSleep is a Sleep hook that returns immediately unless ctx is done, so retry
// tests do not spend wall-clock time in backoff.
func NoSleep(ctx context.Context, _ time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}
