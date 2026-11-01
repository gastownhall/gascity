package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"testing"

	"github.com/gastownhall/gascity/internal/session"
)

// sessionPeekTransport serves client requests in process through h, so the
// client tests open no listener.
type sessionPeekTransport struct{ h http.Handler }

func (tr sessionPeekTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	tr.h.ServeHTTP(rec, req)
	return rec.Result(), nil
}

// sessionPeekClient is an "alpha" city client over h. Its base URL's host is
// one the test handler's Host allowlist accepts.
func sessionPeekClient(h http.Handler) *Client {
	return NewCityScopedClientWithHTTPClient("http://example.com", "alpha", &http.Client{Transport: sessionPeekTransport{h: h}})
}

// writeSessionPeekProblem writes a canned problem+json answer.
func writeSessionPeekProblem(w http.ResponseWriter, status int, code, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	body := struct {
		Status int    `json:"status"`
		Code   string `json:"code,omitempty"`
		Detail string `json:"detail,omitempty"`
	}{Status: status, Code: code, Detail: detail}
	_ = json.NewEncoder(w).Encode(body)
}

func TestClientPeekSessionRequestsStrictPeek(t *testing.T) {
	cases := []struct {
		lines int
		wire  string
	}{
		{lines: 25, wire: "25"},
		{lines: 0, wire: "0"},
		{lines: -3, wire: "0"},
		{lines: 20000, wire: "10000"},
	}
	for _, tc := range cases {
		t.Run(strconv.Itoa(tc.lines), func(t *testing.T) {
			h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v0/city/alpha/session/worker-1" {
					t.Errorf("path = %q, want /v0/city/alpha/session/worker-1", r.URL.Path)
				}
				q := r.URL.Query()
				if q.Get("peek") != "true" || q.Get("peek_strict") != "true" || q.Get("peek_lines") != tc.wire {
					t.Errorf("query = %q, want peek=true peek_strict=true peek_lines=%s", r.URL.RawQuery, tc.wire)
				}
				w.Header().Set("X-GC-Cache-Age-S", "0.5")
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(sessionResponse{ID: "gc-xyz", State: "active", SessionName: "worker-1", LastOutput: "hello\n"})
			})
			got, err := sessionPeekClient(h).PeekSession("worker-1", tc.lines)
			if err != nil {
				t.Fatalf("PeekSession: %v", err)
			}
			if got.Body.ID != "gc-xyz" || got.Body.LastOutput != "hello\n" {
				t.Fatalf("body = %+v", got.Body)
			}
			if got.AgeSeconds != 0.5 {
				t.Fatalf("AgeSeconds = %v, want 0.5", got.AgeSeconds)
			}
		})
	}
}

func TestClientPeekSessionReturnsTheLocalPeekErrors(t *testing.T) {
	fs := newSessionFakeState(t)
	fs.cityName = "alpha"
	suspended := createTestSession(t, fs.cityBeadStore, fs.sp, "Suspended")
	sessionPeekSetState(t, fs, suspended.ID, "suspended")
	retired := createTestSession(t, fs.cityBeadStore, fs.sp, "Retired")
	if err := fs.cityBeadStore.SetMetadata(retired.ID, "alias", "retired-probe"); err != nil {
		t.Fatalf("set alias: %v", err)
	}
	if err := fs.cityBeadStore.Close(retired.ID); err != nil {
		t.Fatalf("close: %v", err)
	}
	c := sessionPeekClient(newTestCityHandler(t, fs))
	remote := &Client{isRemote: true}

	_, err := c.PeekSession(suspended.ID, 50)
	if !errors.Is(err, session.ErrSessionInactive) {
		t.Fatalf("suspended err = %v, want ErrSessionInactive", err)
	}
	if want := "session is not active: " + suspended.ID; err.Error() != want {
		t.Fatalf("suspended err = %q, want %q", err.Error(), want)
	}
	if ShouldFallbackForRead(c, err) || ShouldFallbackForRead(remote, err) {
		t.Fatalf("suspended err %v must not fall back", err)
	}

	_, err = c.PeekSession("retired-probe", 50)
	if !errors.Is(err, session.ErrSessionNotFound) {
		t.Fatalf("closed alias err = %v, want ErrSessionNotFound", err)
	}
	if want := `session not found: "retired-probe"`; err.Error() != want {
		t.Fatalf("closed alias err = %q, want %q", err.Error(), want)
	}
	if ShouldFallbackForRead(c, err) {
		t.Fatalf("closed alias err %v must not fall back", err)
	}
}

func TestClientPeekSessionKeepsOtherProblemsAsAPIErrors(t *testing.T) {
	serve := func(status int, code, detail string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writeSessionPeekProblem(w, status, code, detail)
		})
	}
	c := sessionPeekClient(serve(http.StatusConflict, "session-conflict", "ambiguous: x"))
	_, err := c.PeekSession("x", 5)
	if errors.Is(err, session.ErrSessionInactive) || err == nil || err.Error() != "API error: ambiguous: x" {
		t.Fatalf("409 session-conflict err = %v, want API error: ambiguous: x", err)
	}

	c = sessionPeekClient(serve(http.StatusServiceUnavailable, "", "cache_not_live: priming"))
	if _, err = c.PeekSession("x", 5); !ShouldFallbackForRead(c, err) {
		t.Fatalf("503 cache_not_live err = %v, want a fallbackable read error", err)
	}

	c = sessionPeekClient(serve(http.StatusInternalServerError, "internal", "boom"))
	if _, err = c.PeekSession("x", 5); !IsServerError(err) {
		t.Fatalf("500 err = %v, want a server error", err)
	}

	c = sessionPeekClient(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	if _, err = c.PeekSession("x", 5); err == nil || errors.Is(err, session.ErrSessionNotFound) {
		t.Fatalf("bare 404 err = %v, want a plain error", err)
	}
}

func TestSessionPeekLinesClampMatchesTheWireMaximum(t *testing.T) {
	field, ok := reflect.TypeOf(SessionGetInput{}).FieldByName("PeekLines")
	if !ok {
		t.Fatal("SessionGetInput has no PeekLines field")
	}
	if got, want := field.Tag.Get("maximum"), strconv.Itoa(maxSessionPeekLines); got != want {
		t.Fatalf("PeekLines maximum = %q, client clamp = %q", got, want)
	}
}
