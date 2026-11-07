package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// pendingLookProvider is a fake provider whose look for a pending
// interaction is look.
type pendingLookProvider struct {
	*runtime.Fake
	look func(name string) (*runtime.PendingInteraction, error)
}

func (p *pendingLookProvider) Pending(name string) (*runtime.PendingInteraction, error) {
	return p.look(name)
}

// hangingPendingLook is a look that answers only once the test ends.
func hangingPendingLook(t *testing.T) func(string) (*runtime.PendingInteraction, error) {
	t.Helper()
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	return func(string) (*runtime.PendingInteraction, error) {
		<-release
		return nil, nil
	}
}

// pendingLookCases are the looks a running session's runtime cannot answer:
// a runtime it cannot reach, another error, a panic, and one that hangs
// past the caller's deadline (ctx).
func pendingLookCases(t *testing.T) []struct {
	name    string
	look    func(string) (*runtime.PendingInteraction, error)
	timeout time.Duration
	detail  string
} {
	return []struct {
		name    string
		look    func(string) (*runtime.PendingInteraction, error)
		timeout time.Duration
		detail  string
	}{
		{name: "runtime unreachable", look: func(string) (*runtime.PendingInteraction, error) {
			return nil, fmt.Errorf("capturing pane: tmux server unreachable: %w", runtime.ErrRuntimeUnavailable)
		}, detail: "tmux server unreachable"},
		{name: "look fails", look: func(string) (*runtime.PendingInteraction, error) {
			return nil, errors.New("capturing pane: exit status 1")
		}, detail: "exit status 1"},
		{name: "look panics", look: func(string) (*runtime.PendingInteraction, error) {
			panic("pending look blew up")
		}, detail: "the look panicked: pending look blew up"},
		{name: "look hangs", look: hangingPendingLook(t), timeout: 200 * time.Millisecond, detail: "the look did not answer in time"},
	}
}

// wantPendingCheckRefusal fails unless rec is the 503 pending_check_unavailable
// refusal naming detail.
func wantPendingCheckRefusal(t *testing.T, rec *httptest.ResponseRecorder, detail string) {
	t.Helper()
	var problem struct {
		Code   string `json:"code"`
		Detail string `json:"detail"`
	}
	if rec.Code != http.StatusServiceUnavailable || json.Unmarshal(rec.Body.Bytes(), &problem) != nil {
		t.Fatalf("status = %d, want 503; body: %s", rec.Code, rec.Body.String())
	}
	if problem.Code != "service-unavailable" || !strings.HasPrefix(problem.Detail, pendingCheckCode+": could not check session ") ||
		!strings.Contains(problem.Detail, detail) || !strings.HasSuffix(problem.Detail, "retry") {
		t.Fatalf("problem = %+v, want service-unavailable %s: naming %q and saying to retry", problem, pendingCheckCode, detail)
	}
}

// TestSessionSubmitAndMessageRefuseALookThatCannotAnswer: the API's front
// door (rejectPendingInteraction) is lanesubmit.CheckPending. A running
// session whose look for a pending interaction fails, panics, or does not
// answer in time is refused with a retryable 503 pending_check_unavailable,
// and nothing reaches the session. Kills a front door that accepts text the
// runtime never said nothing blocks, and an unbounded look.
func TestSessionSubmitAndMessageRefuseALookThatCannotAnswer(t *testing.T) {
	for _, row := range pendingLookCases(t) {
		for _, tc := range []struct{ path, body string }{
			{"/submit", `{"message":"hello?"}`},
			{"/submit", `{"message":"hello?","resume":true}`},
			{"/messages", `{"message":"hello?"}`},
		} {
			t.Run(row.name+" "+tc.path+" "+tc.body, func(t *testing.T) {
				fs := newSessionFakeState(t)
				h := newTestCityHandler(t, fs)
				info := createTestSession(t, fs.cityBeadStore, fs.sp, "Unanswered")
				fs.sessionProvider = &pendingLookProvider{Fake: fs.sp, look: row.look}
				fs.sp.Calls = nil

				req := newPostRequest(cityURL(fs, "/session/")+info.ID+tc.path, strings.NewReader(tc.body))
				if row.timeout > 0 {
					ctx, cancel := context.WithTimeout(req.Context(), row.timeout)
					defer cancel()
					req = req.WithContext(ctx)
				}
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, req)
				wantPendingCheckRefusal(t, rec, row.detail)
				for _, call := range fs.sp.Calls {
					if (call.Method == "Nudge" || call.Method == "NudgeNow" || call.Method == "SendKeys") && call.Name == info.SessionName {
						t.Fatalf("session received %s though its look did not answer", call.Method)
					}
				}
			})
		}
	}
}

// TestSubmitUnderLaneRefusesALookThatCannotAnswer: under lane authority the
// front door is the same check, run for a request no op under its key
// answers: a look that cannot answer is 503 pending_check_unavailable and
// writes no op.
func TestSubmitUnderLaneRefusesALookThatCannotAnswer(t *testing.T) {
	for _, row := range pendingLookCases(t) {
		if row.timeout > 0 {
			// The hang is pinned through the API's handler above; the lane
			// runs the same check.
			continue
		}
		t.Run(row.name, func(t *testing.T) {
			e, _ := laneMessageEnv(t, nil)
			shortSettle(t)
			info := e.session(t, "Lane unanswered")
			e.fs.sessionProvider = &pendingLookProvider{Fake: e.fs.sp, look: row.look}
			for _, path := range []string{"/submit", "/messages"} {
				rec := e.post(t, "/session/"+info.ID+path, "k-unanswered", `{"message":"hi"}`)
				wantPendingCheckRefusal(t, rec, row.detail)
			}
			if rows := e.opRows(t); len(rows) != 0 {
				t.Fatalf("op rows = %d, want none written", len(rows))
			}
		})
	}
}

// The front door's look is bounded: a hanging look with no caller deadline
// is refused once lanesubmit.PendingCheckTimeout passes, not waited on.
func TestRejectPendingInteractionIsBounded(t *testing.T) {
	if testing.Short() {
		t.Skip("waits out lanesubmit.PendingCheckTimeout")
	}
	fs := newSessionFakeState(t)
	srv := New(fs)
	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Hangs")
	fs.sessionProvider = &pendingLookProvider{Fake: fs.sp, look: hangingPendingLook(t)}
	done := make(chan error, 1)
	go func() { done <- srv.rejectPendingInteraction(context.Background(), fs.cityBeadStore, info.ID) }()
	select {
	case err := <-done:
		if err == nil || !strings.HasPrefix(err.Error(), pendingCheckCode+": ") {
			t.Fatalf("err = %v, want %s", err, pendingCheckCode)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("rejectPendingInteraction did not return; the look is unbounded")
	}
}
