package apicontract

import (
	"bytes"
	"fmt"
	"io"
	"mime"
	"net/http"
	"sort"
	"sync"
)

// InProcessTransport is an http.RoundTripper that serves every request from an
// http.Handler in the same process — no listener, no loopback socket — while
// validating each response against a Spec and recording which operationIds
// were exercised. Streaming (text/event-stream) responses are returned as soon
// as the handler commits its headers, so SSE clients observe frames as the
// handler flushes them; their bodies are not schema-validated.
//
// Validation failures and requests that match no operation are collected
// rather than returned as transport errors, so a contract test can call the
// generated client normally and assert on Failures at the end.
type InProcessTransport struct {
	spec    *Spec
	handler http.Handler
	// RemoteAddr is stamped on every served request (default 127.0.0.1:0),
	// matching what a loopback listener would report.
	RemoteAddr string

	mu        sync.Mutex
	exercised map[string]int
	failures  []error
}

// NewInProcessTransport returns a transport that serves requests from h and
// validates responses against spec.
func NewInProcessTransport(spec *Spec, h http.Handler) *InProcessTransport {
	return &InProcessTransport{spec: spec, handler: h, RemoteAddr: "127.0.0.1:0", exercised: map[string]int{}}
}

// RoundTrip implements http.RoundTripper.
func (t *InProcessTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	served := req.Clone(req.Context())
	served.RemoteAddr = t.RemoteAddr
	served.RequestURI = req.URL.RequestURI()
	if served.Host == "" {
		served.Host = req.URL.Host
	}
	if served.Body == nil {
		served.Body = http.NoBody
	}

	op, matched := t.spec.Match(req.Method, req.URL.Path)
	if !matched {
		t.fail(&ViolationError{Err: fmt.Errorf("%s %s matches no operation in the OpenAPI document", req.Method, req.URL.Path)})
	}

	pr, pw := io.Pipe()
	w := newStreamWriter(pw)
	go func() {
		defer func() {
			w.commit()
			_ = pw.Close()
		}()
		t.handler.ServeHTTP(w, served)
	}()
	<-w.ready

	resp := &http.Response{
		Status:        fmt.Sprintf("%d %s", w.status, http.StatusText(w.status)),
		StatusCode:    w.status,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        w.committed,
		Request:       req,
		ContentLength: -1,
	}
	if matched {
		t.record(op.ID)
	}
	if isEventStream(resp.Header.Get("Content-Type")) {
		resp.Body = pr
		return resp, nil
	}
	body, err := io.ReadAll(pr)
	_ = pr.Close()
	if err != nil {
		return nil, fmt.Errorf("reading in-process response for %s %s: %w", req.Method, req.URL.Path, err)
	}
	if matched {
		if verr := t.spec.ValidateResponse(req, resp, body); verr != nil {
			t.fail(&ViolationError{OperationID: op.ID, Err: verr})
		}
	}
	resp.ContentLength = int64(len(body))
	resp.Body = io.NopCloser(bytes.NewReader(body))
	return resp, nil
}

// ViolationError is one response that did not match the spec, or a request
// that matched no operation (OperationID empty).
type ViolationError struct {
	OperationID string
	Err         error
}

// Error implements error.
func (e *ViolationError) Error() string {
	if e.OperationID == "" {
		return e.Err.Error()
	}
	return "[" + e.OperationID + "] " + e.Err.Error()
}

// Unwrap returns the underlying validation error.
func (e *ViolationError) Unwrap() error { return e.Err }

// Exercised returns operationId -> number of responses observed.
func (t *InProcessTransport) Exercised() map[string]int {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make(map[string]int, len(t.exercised))
	for k, v := range t.exercised {
		out[k] = v
	}
	return out
}

// ExercisedIDs returns the sorted set of exercised operationIds.
func (t *InProcessTransport) ExercisedIDs() []string {
	ex := t.Exercised()
	ids := make([]string, 0, len(ex))
	for id := range ex {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// Failures returns every spec violation and unmatched request observed so far.
func (t *InProcessTransport) Failures() []error {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]error, len(t.failures))
	copy(out, t.failures)
	return out
}

func (t *InProcessTransport) record(id string) {
	t.mu.Lock()
	t.exercised[id]++
	t.mu.Unlock()
}

func (t *InProcessTransport) fail(err error) {
	t.mu.Lock()
	t.failures = append(t.failures, err)
	t.mu.Unlock()
}

func isEventStream(contentType string) bool {
	mt, _, err := mime.ParseMediaType(contentType)
	return err == nil && mt == "text/event-stream"
}

// streamWriter is an http.ResponseWriter + http.Flusher whose body flows
// through a pipe. Headers are committed (snapshotted and published on ready)
// at the first Write, Flush, or handler return.
type streamWriter struct {
	header    http.Header
	committed http.Header
	status    int
	pw        *io.PipeWriter
	ready     chan struct{}
	once      sync.Once
}

func newStreamWriter(pw *io.PipeWriter) *streamWriter {
	return &streamWriter{header: http.Header{}, pw: pw, ready: make(chan struct{})}
}

func (w *streamWriter) Header() http.Header { return w.header }

func (w *streamWriter) WriteHeader(code int) {
	select {
	case <-w.ready:
		return
	default:
	}
	if w.status == 0 {
		w.status = code
	}
}

func (w *streamWriter) Write(p []byte) (int, error) {
	w.commit()
	return w.pw.Write(p)
}

func (w *streamWriter) Flush() { w.commit() }

func (w *streamWriter) commit() {
	w.once.Do(func() {
		if w.status == 0 {
			w.status = http.StatusOK
		}
		w.committed = w.header.Clone()
		close(w.ready)
	})
}
