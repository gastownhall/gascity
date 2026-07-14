package api

import (
	"bytes"
	"io"
	"net/http"
)

// LoopbackTransport returns an http.RoundTripper that serves a request against
// the supervisor's own un-gated inner handler in-process, without a network
// hop. It exists for the supervisor's server-side self-reads — the dashboard
// /api plane's status and run-view samplers — which must read the supervisor's
// own typed /v0/city/{name}/... routes over loopback to build the /api/*
// responses.
//
// Those self-reads intentionally bypass the read-auth gate. The gate exists to
// stop city reads from network position; a self-read is the supervisor reading
// its own state to serve the /api/* plane, which is itself documented as
// outside the read-auth gate (an authority fronting the whole listener protects
// /api/* — and the self-read is behind that same boundary). Routing the
// self-read back through the network listener would instead hand it a read-auth
// 401 whenever read-auth is enabled, silently degrading the dashboard health and
// run views. Dispatching against the inner handler keeps the self-read on the
// same typed handlers without the edge middleware (auth, host allow-listing,
// CORS) that only applies to external callers.
func (sm *SupervisorMux) LoopbackTransport() http.RoundTripper {
	return loopbackTransport{h: http.HandlerFunc(sm.ServeHTTP)}
}

// loopbackTransport dispatches a request against an in-process handler instead
// of dialing the network. It is the mechanism behind SupervisorMux.LoopbackTransport.
type loopbackTransport struct{ h http.Handler }

// RoundTrip serves req against the wrapped handler and returns the recorded
// response. It never returns a transport error: the wrapped handler always
// produces a response, and a handler panic is contained as a 500 (the inner
// handler runs without the outer recovery middleware, so containing it here
// keeps a self-read from crashing the caller's goroutine).
func (t loopbackTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	rec := &loopbackRecorder{header: make(http.Header)}
	func() {
		defer func() {
			if r := recover(); r != nil && !rec.wroteHeader {
				rec.status = http.StatusInternalServerError
			}
		}()
		t.h.ServeHTTP(rec, req)
	}()
	status := rec.status
	if status == 0 {
		status = http.StatusOK
	}
	return &http.Response{
		StatusCode: status,
		Status:     http.StatusText(status),
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
		Header:     rec.header,
		Body:       io.NopCloser(bytes.NewReader(rec.body.Bytes())),
		Request:    req,
	}, nil
}

// loopbackRecorder is a minimal http.ResponseWriter that buffers the status,
// headers, and body an in-process handler writes, so loopbackTransport can
// project them onto an *http.Response. It captures only what the self-read
// callers consume (status code and body); it deliberately does not reinvent
// httptest.ResponseRecorder's full machinery.
type loopbackRecorder struct {
	header      http.Header
	body        bytes.Buffer
	status      int
	wroteHeader bool
}

func (r *loopbackRecorder) Header() http.Header { return r.header }

func (r *loopbackRecorder) WriteHeader(status int) {
	if !r.wroteHeader {
		r.status = status
		r.wroteHeader = true
	}
}

func (r *loopbackRecorder) Write(b []byte) (int, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	return r.body.Write(b)
}

// Flush is a no-op that lets handlers which probe for http.Flusher (streaming
// writers) succeed; the buffered body is already complete when RoundTrip reads it.
func (r *loopbackRecorder) Flush() {}
