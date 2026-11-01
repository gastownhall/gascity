package api

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

// sessionPeekProbe is a runtime.Fake that records the lines each Peek asked
// for and can fail the peek of chosen sessions.
type sessionPeekProbe struct {
	*runtime.Fake
	mu    sync.Mutex
	lines map[string][]int
	fail  map[string]error
}

func newSessionPeekProbe(sp *runtime.Fake) *sessionPeekProbe {
	return &sessionPeekProbe{Fake: sp, lines: map[string][]int{}, fail: map[string]error{}}
}

func (p *sessionPeekProbe) Peek(name string, lines int) (string, error) {
	p.mu.Lock()
	p.lines[name] = append(p.lines[name], lines)
	err := p.fail[name]
	p.mu.Unlock()
	if err != nil {
		return "", err
	}
	return p.Fake.Peek(name, lines)
}

func (p *sessionPeekProbe) peeks(name string) []int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]int(nil), p.lines[name]...)
}

// sessionPeekGet serves GET /session/{target}?query through the test handler.
func sessionPeekGet(t *testing.T, fs *fakeState, target, query string) *httptest.ResponseRecorder {
	t.Helper()
	h := newTestCityHandler(t, fs)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", cityURL(fs, "/session/"+target+"?"+query), nil))
	return rec
}

func sessionPeekSetState(t *testing.T, fs *fakeState, id, state string) {
	t.Helper()
	if err := fs.cityBeadStore.SetMetadata(id, "state", state); err != nil {
		t.Fatalf("set state %s on %s: %v", state, id, err)
	}
}

func TestSessionGetPeekStrictPeeksEveryOpenUnsuspendedSession(t *testing.T) {
	cases := []struct {
		name    string
		state   string
		stopped bool
		lines   int
	}{
		{name: "active", state: "active", lines: 12},
		{name: "draining", state: "draining", lines: 12},
		{name: "creating", state: "creating", stopped: true, lines: 12},
		{name: "asleep", state: "asleep", stopped: true, lines: 12},
		{name: "active full capture", state: "active", lines: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := newSessionFakeState(t)
			probe := newSessionPeekProbe(fs.sp)
			fs.sessionProvider = probe
			info := createTestSession(t, fs.cityBeadStore, fs.sp, "Peeked")
			sessionPeekSetState(t, fs, info.ID, tc.state)
			if tc.stopped {
				if err := fs.sp.Stop(info.SessionName); err != nil {
					t.Fatalf("stop runtime: %v", err)
				}
			}
			fs.sp.SetPeekOutput(info.SessionName, "pane line 1\npane line 2\n")

			rec := sessionPeekGet(t, fs, info.ID, fmt.Sprintf("peek_strict=true&peek_lines=%d", tc.lines))
			wantStatus(t, "strict peek", rec, http.StatusOK)
			got := decodeInto[sessionResponse](t, rec)
			if got.LastOutput != "pane line 1\npane line 2\n" {
				t.Fatalf("last_output = %q, want the pane output", got.LastOutput)
			}
			if peeks := probe.peeks(info.SessionName); len(peeks) != 1 || peeks[0] != tc.lines {
				t.Fatalf("Peek lines = %v, want exactly [%d]", peeks, tc.lines)
			}
		})
	}
}

func TestSessionGetPeekStrictRefusesSuspendedAndClosedSessions(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, fs *fakeState, id string)
	}{
		{name: "suspended", setup: func(t *testing.T, fs *fakeState, id string) {
			sessionPeekSetState(t, fs, id, "suspended")
		}},
		{name: "closed", setup: func(t *testing.T, fs *fakeState, id string) {
			if err := fs.cityBeadStore.Close(id); err != nil {
				t.Fatalf("close: %v", err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := newSessionFakeState(t)
			probe := newSessionPeekProbe(fs.sp)
			fs.sessionProvider = probe
			info := createTestSession(t, fs.cityBeadStore, fs.sp, "Refused")
			tc.setup(t, fs, info.ID)

			rec := sessionPeekGet(t, fs, info.ID, "peek_strict=true")
			wantStatus(t, "strict peek", rec, http.StatusConflict)
			body := decodeInto[struct {
				Type   string `json:"type"`
				Code   string `json:"code"`
				Detail string `json:"detail"`
			}](t, rec)
			if body.Code != "session-inactive" || body.Type != "urn:gascity:error:session-inactive" {
				t.Fatalf("problem = %+v, want session-inactive", body)
			}
			if want := "session is not active: " + info.ID; body.Detail != want {
				t.Fatalf("detail = %q, want %q", body.Detail, want)
			}
			if peeks := probe.peeks(info.SessionName); len(peeks) != 0 {
				t.Fatalf("Peek lines = %v, want no peek", peeks)
			}
		})
	}
}

func TestSessionGetPeekStrictReportsAFailedPeek(t *testing.T) {
	fs := newSessionFakeState(t)
	probe := newSessionPeekProbe(fs.sp)
	fs.sessionProvider = probe
	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Broken")
	probe.fail[info.SessionName] = errors.New("pane capture failed")

	rec := sessionPeekGet(t, fs, info.ID, "peek_strict=true")
	wantStatus(t, "strict peek", rec, http.StatusInternalServerError)
	body := decodeInto[struct {
		Code   string `json:"code"`
		Detail string `json:"detail"`
	}](t, rec)
	if body.Code != "internal" {
		t.Fatalf("code = %q, want internal", body.Code)
	}
	if !strings.Contains(body.Detail, "pane capture failed") || !strings.Contains(body.Detail, info.ID) {
		t.Fatalf("detail = %q, want the peek error and the session id", body.Detail)
	}
}

func TestSessionGetPeekStrictFindsAClosedSessionOnlyByID(t *testing.T) {
	fs := newSessionFakeState(t)
	fs.sessionProvider = newSessionPeekProbe(fs.sp)
	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Retired")
	if err := fs.cityBeadStore.SetMetadata(info.ID, "alias", "retired-probe"); err != nil {
		t.Fatalf("set alias: %v", err)
	}
	if err := fs.cityBeadStore.Close(info.ID); err != nil {
		t.Fatalf("close: %v", err)
	}

	rec := sessionPeekGet(t, fs, "retired-probe", "peek_strict=true")
	wantStatus(t, "strict peek by closed alias", rec, http.StatusNotFound)
	body := decodeInto[struct {
		Code   string `json:"code"`
		Detail string `json:"detail"`
	}](t, rec)
	if body.Code != "session-not-found" {
		t.Fatalf("code = %q, want session-not-found", body.Code)
	}
	if want := `not_found: session not found: "retired-probe"`; body.Detail != want {
		t.Fatalf("detail = %q, want %q", body.Detail, want)
	}

	rec = sessionPeekGet(t, fs, "retired-probe", "")
	wantStatus(t, "default get by closed alias", rec, http.StatusOK)
	if got := decodeInto[sessionResponse](t, rec); got.ID != info.ID {
		t.Fatalf("id = %q, want %q", got.ID, info.ID)
	}
}

func TestSessionGetPreviewPeekStaysBestEffort(t *testing.T) {
	fs := newSessionFakeState(t)
	probe := newSessionPeekProbe(fs.sp)
	fs.sessionProvider = probe

	draining := createTestSession(t, fs.cityBeadStore, fs.sp, "Draining")
	sessionPeekSetState(t, fs, draining.ID, "draining")
	fs.sp.SetPeekOutput(draining.SessionName, "draining output\n")
	rec := sessionPeekGet(t, fs, draining.ID, "peek=true")
	wantStatus(t, "preview draining", rec, http.StatusOK)
	if got := decodeInto[sessionResponse](t, rec); got.LastOutput != "" {
		t.Fatalf("draining last_output = %q, want none", got.LastOutput)
	}
	if peeks := probe.peeks(draining.SessionName); len(peeks) != 0 {
		t.Fatalf("draining Peek lines = %v, want no peek", peeks)
	}

	broken := createTestSession(t, fs.cityBeadStore, fs.sp, "Broken")
	probe.fail[broken.SessionName] = errors.New("pane capture failed")
	rec = sessionPeekGet(t, fs, broken.ID, "peek=true")
	wantStatus(t, "preview failed peek", rec, http.StatusOK)
	if got := decodeInto[sessionResponse](t, rec); got.LastOutput != "" {
		t.Fatalf("broken last_output = %q, want none", got.LastOutput)
	}

	active := createTestSession(t, fs.cityBeadStore, fs.sp, "Active")
	fs.sp.SetPeekOutput(active.SessionName, "active output\n")
	rec = sessionPeekGet(t, fs, active.ID, "peek=true")
	wantStatus(t, "preview active", rec, http.StatusOK)
	if got := decodeInto[sessionResponse](t, rec); got.LastOutput != "active output\n" {
		t.Fatalf("active last_output = %q, want the pane output", got.LastOutput)
	}
	if peeks := probe.peeks(active.SessionName); len(peeks) != 1 || peeks[0] != 5 {
		t.Fatalf("active Peek lines = %v, want [5]", peeks)
	}

	suspended := createTestSession(t, fs.cityBeadStore, fs.sp, "Suspended")
	sessionPeekSetState(t, fs, suspended.ID, "suspended")
	rec = sessionPeekGet(t, fs, suspended.ID, "peek=true")
	wantStatus(t, "preview suspended", rec, http.StatusOK)
}
