package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/reconcilekey"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// TestHandleSessionSubmitResumesOnlyWithResumeTrue is the owner ruling on
// D8 (CONTRACT v5.9): POST /submit resumes a suspended session only when the
// request carries resume: true. Without it the message queues and the
// session stays suspended, held for its operator. Kills an API submit that
// resumes by default, and one that ignores resume: true.
func TestHandleSessionSubmitResumesOnlyWithResumeTrue(t *testing.T) {
	for _, tc := range []struct {
		name   string
		body   string
		resume bool
	}{
		{name: "without resume queues", body: `{"message":"hello"}`},
		{name: "resume true resumes", body: `{"message":"hello","resume":true}`, resume: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := newSessionFakeState(t)
			h := newTestCityHandler(t, fs)

			info := createTestSession(t, fs.cityBeadStore, fs.sp, "Submit Me")
			mgr := session.NewManagerWithOptions(fs.cityBeadStore, fs.sp, session.WithCityPath(fs.cityPath))
			if err := mgr.Suspend(info.ID); err != nil {
				t.Fatalf("Suspend: %v", err)
			}

			req := newPostRequest(cityURL(fs, "/session/")+info.ID+"/submit", strings.NewReader(tc.body))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusAccepted {
				t.Fatalf("submit status = %d, want %d; body: %s", rec.Code, http.StatusAccepted, rec.Body.String())
			}
			var accepted asyncAcceptedBody
			if err := json.NewDecoder(rec.Body).Decode(&accepted); err != nil {
				t.Fatalf("decode: %v", err)
			}

			success, failure := waitForSessionSubmitResult(t, fs.eventProv, accepted.RequestID)
			if success == nil {
				t.Fatalf("session submit failed: %s: %s", failure.ErrorCode, failure.ErrorMessage)
			}
			if success.Queued == tc.resume || fs.sp.IsRunning(info.SessionName) != tc.resume {
				t.Fatalf("queued = %v, running = %v; want resumed = %v", success.Queued, fs.sp.IsRunning(info.SessionName), tc.resume)
			}
			if success.Intent != string(session.SubmitIntentDefault) {
				t.Fatalf("intent = %q, want %q", success.Intent, session.SubmitIntentDefault)
			}
			b, err := fs.cityBeadStore.Get(info.ID)
			if err != nil {
				t.Fatal(err)
			}
			if wantState := map[bool]string{false: "suspended", true: "active"}[tc.resume]; b.Metadata["state"] != wantState {
				t.Fatalf("state = %q, want %q", b.Metadata["state"], wantState)
			}
		})
	}
}

func TestHandleSessionSubmitUsesImmediateDefaultForCodex(t *testing.T) {
	fs := newSessionFakeState(t)
	h := newTestCityHandler(t, fs)

	mgr := session.NewManagerWithOptions(fs.cityBeadStore, fs.sp, session.WithCityPath(fs.cityPath))
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{Template: "helper", Title: "Codex Submit", Command: "codex", WorkDir: t.TempDir(), Provider: "codex", Env: nil, Resume: session.ProviderResume{}, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.Suspend(info.ID); err != nil {
		t.Fatalf("Suspend: %v", err)
	}

	req := newPostRequest(cityURL(fs, "/session/")+info.ID+"/submit", strings.NewReader(`{"message":"hello","resume":true}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("submit status = %d, want %d; body: %s", rec.Code, http.StatusAccepted, rec.Body.String())
	}
	var accepted asyncAcceptedBody
	if err := json.NewDecoder(rec.Body).Decode(&accepted); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if accepted.RequestID == "" {
		t.Fatal("missing request_id")
	}

	success, failure := waitForSessionSubmitResult(t, fs.eventProv, accepted.RequestID)
	if success == nil {
		t.Fatalf("session submit failed: %s: %s", failure.ErrorCode, failure.ErrorMessage)
	}
}

func TestHandleSessionSubmitFollowUpQueuesMessage(t *testing.T) {
	fs := newSessionFakeState(t)
	h := newTestCityHandler(t, fs)

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Queue Me")

	req := newPostRequest(cityURL(fs, "/session/")+info.ID+"/submit", strings.NewReader(`{"message":"later please","intent":"follow_up"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("submit status = %d, want %d; body: %s", rec.Code, http.StatusAccepted, rec.Body.String())
	}
	var accepted asyncAcceptedBody
	if err := json.NewDecoder(rec.Body).Decode(&accepted); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if accepted.RequestID == "" {
		t.Fatal("missing request_id")
	}

	success, failure := waitForSessionSubmitResult(t, fs.eventProv, accepted.RequestID)
	if success == nil {
		t.Fatalf("session submit failed: %s: %s", failure.ErrorCode, failure.ErrorMessage)
	}

	state, err := nudgequeue.LoadState(fs.cityPath)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if len(state.Pending) != 1 {
		t.Fatalf("pending queued submits = %d, want 1", len(state.Pending))
	}
	item := state.Pending[0]
	if item.SessionID != info.ID {
		t.Fatalf("SessionID = %q, want %q", item.SessionID, info.ID)
	}
	if item.Message != "later please" {
		t.Fatalf("Message = %q, want %q", item.Message, "later please")
	}
}

func TestHandleSessionGetIncludesSubmissionCapabilities(t *testing.T) {
	fs := newSessionFakeState(t)
	h := newTestCityHandler(t, fs)

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Capabilities")
	if err := fs.cityBeadStore.Update(info.ID, beads.UpdateOpts{
		Metadata: map[string]string{
			"pool_managed": "true",
			"pool_slot":    "1",
		},
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", cityURL(fs, "/session/")+info.ID, nil)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("get status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var resp sessionResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.SubmissionCapabilities.SupportsFollowUp {
		t.Fatal("SupportsFollowUp = false, want true")
	}
	if !resp.SubmissionCapabilities.SupportsInterruptNow {
		t.Fatal("SupportsInterruptNow = false, want true")
	}
}

func TestHandleSessionStopUsesSoftEscapeForCodex(t *testing.T) {
	fs := newSessionFakeState(t)
	h := newTestCityHandler(t, fs)

	mgr := session.NewManagerWithOptions(fs.cityBeadStore, fs.sp)
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{Template: "helper", Title: "Codex", Command: "codex", WorkDir: t.TempDir(), Provider: "codex", Env: nil, Resume: session.ProviderResume{}, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := fs.cityBeadStore.Update(info.ID, beads.UpdateOpts{
		Metadata: map[string]string{"pool_managed": "true"},
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	rec := httptest.NewRecorder()
	req := newPostRequest(cityURL(fs, "/session/")+info.ID+"/stop", nil)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("stop status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var sawEscape, sawInterrupt bool
	for _, call := range fs.sp.Calls {
		if call.Method == "SendKeys" && call.Name == info.SessionName && call.Message == "Escape" {
			sawEscape = true
		}
		if call.Method == "Interrupt" && call.Name == info.SessionName {
			sawInterrupt = true
		}
	}
	if !sawEscape {
		t.Fatalf("calls = %#v, want SendKeys(Escape)", fs.sp.Calls)
	}
	if sawInterrupt {
		t.Fatalf("calls = %#v, did not want Interrupt for codex stop", fs.sp.Calls)
	}
}

// TestHandleSessionMessageReportsQueued: POST /messages without resume: true
// to a held session queues the message, and its result says so (queued).
// Kills a result that reports a queued message as delivered.
func TestHandleSessionMessageReportsQueued(t *testing.T) {
	fs := newSessionFakeState(t)
	h := newTestCityHandler(t, fs)
	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Held")
	if err := session.NewManagerWithOptions(fs.cityBeadStore, fs.sp, session.WithCityPath(fs.cityPath)).Suspend(info.ID); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newPostRequest(cityURL(fs, "/session/")+info.ID+"/messages", strings.NewReader(`{"message":"hello"}`)))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d; body: %s", rec.Code, rec.Body.String())
	}
	accepted := decodeAsyncAccepted(t, rec.Body)
	success, failure := waitForSessionMessageResult(t, fs.eventProv, accepted.RequestID)
	if success == nil {
		t.Fatalf("message failed: %s: %s", failure.ErrorCode, failure.ErrorMessage)
	}
	if !success.Queued {
		t.Fatal("queued = false for a message queued on a held session")
	}
}

// submitWithoutResume posts message to id without resume: true and returns
// the result.
func submitWithoutResume(t *testing.T, fs *fakeState, h http.Handler, id, body string) *SessionSubmitSucceededPayload {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newPostRequest(cityURL(fs, "/session/")+id+"/submit", strings.NewReader(body)))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("submit status = %d; body: %s", rec.Code, rec.Body.String())
	}
	var accepted asyncAcceptedBody
	if err := json.NewDecoder(rec.Body).Decode(&accepted); err != nil {
		t.Fatalf("decode: %v", err)
	}
	success, failure := waitForSessionSubmitResult(t, fs.eventProv, accepted.RequestID)
	if success == nil {
		t.Fatalf("session submit failed: %s: %s", failure.ErrorCode, failure.ErrorMessage)
	}
	return success
}

// willStartState is a controller that names why it will not start.
type willStartState struct {
	*fakeState
	refusal   string
	uncertain bool
}

func (s *willStartState) WakeStartRefusal(session.Info) (string, bool) {
	return s.refusal, !s.uncertain
}

// TestHandleSessionSubmitWithoutResumeLeavesStartToController is D8 rule 1
// for the API (CONTRACT v5.9 D8 7(e); review findings C-1, C-2, C-6):
// without resume: true, a message to a session that is not running never
// starts it in the controller's process. It queues, and the result says
// whether the controller will start it: on an unheld asleep row the wake is
// recorded and the session handed to the controller (will_start true, or
// false with the controller's reason and the resume hint); on a held row
// nothing is written or enqueued (will_start false, the hold named). Kills
// an in-process start, a wake over a hold, an enqueue without a wake, and a
// silent success for a session that will not start.
func TestHandleSessionSubmitWithoutResumeLeavesStartToController(t *testing.T) {
	for name, tc := range map[string]struct {
		held      bool
		refusal   string
		uncertain bool
		willStart bool
		reason    string
	}{
		"unheld asleep":                   {willStart: true},
		"unheld, will not start":          {refusal: "rig \"r\" is suspended", reason: "resume: true"},
		"unheld pool seat, demand unseen": {uncertain: true},
		"suspended":                       {held: true, reason: "an operator holds the session"},
	} {
		t.Run(name, func(t *testing.T) {
			fs := newSessionFakeState(t)
			state := &willStartState{fakeState: fs, refusal: tc.refusal, uncertain: tc.uncertain}
			h := newTestCityHandlerWith(t, state, New(state))
			info := createTestSession(t, fs.cityBeadStore, fs.sp, "Dormant")
			if err := session.NewManagerWithOptions(fs.cityBeadStore, fs.sp, session.WithCityPath(fs.cityPath)).Suspend(info.ID); err != nil {
				t.Fatalf("Suspend: %v", err)
			}
			if !tc.held {
				if err := fs.cityBeadStore.SetMetadataBatch(info.ID, map[string]string{"state": "asleep", "suspended_at": "", "held_until": "", "sleep_intent": ""}); err != nil {
					t.Fatal(err)
				}
			}
			startsBefore := fs.sp.CountCalls("Start", info.SessionName)
			success := submitWithoutResume(t, fs, h, info.ID, `{"message":"hello"}`)
			if !success.Queued || fs.sp.CountCalls("Start", info.SessionName) != startsBefore {
				t.Fatalf("queued = %v, starts = %d; want queued and no start", success.Queued, fs.sp.CountCalls("Start", info.SessionName)-startsBefore)
			}
			if tc.uncertain {
				if success.WillStart != nil {
					t.Fatalf("will_start = %v, want unknown for a seat waiting on demand", *success.WillStart)
				}
			} else if success.WillStart == nil || *success.WillStart != tc.willStart || !strings.Contains(success.WillNotStartReason, tc.reason) {
				t.Fatalf("will_start = %v, reason %q; want %v and %q", success.WillStart, success.WillNotStartReason, tc.willStart, tc.reason)
			}
			b, err := fs.cityBeadStore.Get(info.ID)
			if err != nil {
				t.Fatal(err)
			}
			if woken := b.Metadata["wake_request"] != ""; woken == tc.held {
				t.Fatalf("wake_request = %q on a row held=%v", b.Metadata["wake_request"], tc.held)
			}
			if enqueued := slices.Contains(fs.enqueuedKeys(), reconcilekey.Session(info.ID)); enqueued == tc.held {
				t.Fatalf("enqueued = %v on a row held=%v", enqueued, tc.held)
			}
		})
	}
}

// TestHandleSessionSubmitRecordsNoWakeForOtherQueues is review finding C-1:
// only a queue the resume policy caused on a dormant row records a wake. A
// follow_up queued behind a running session's turn, and a message to a row
// that is still creating, record none, and the follow_up pokes nothing.
// Kills a wake or poke for any queued message (the creating-row residue).
func TestHandleSessionSubmitRecordsNoWakeForOtherQueues(t *testing.T) {
	t.Run("follow_up to a running session", func(t *testing.T) {
		fs := newSessionFakeState(t)
		h := newTestCityHandler(t, fs)
		info := createTestSession(t, fs.cityBeadStore, fs.sp, "Running")
		success := submitWithoutResume(t, fs, h, info.ID, `{"message":"later","intent":"follow_up"}`)
		if b, _ := fs.cityBeadStore.Get(info.ID); !success.Queued || success.WillStart != nil || b.Metadata["wake_request"] != "" {
			t.Fatalf("queued = %v, will_start = %v, wake_request = %q; want a plain queue", success.Queued, success.WillStart, b.Metadata["wake_request"])
		}
		if slices.Contains(fs.enqueuedKeys(), reconcilekey.Session(info.ID)) {
			t.Fatal("a follow_up queued behind a running turn poked the controller")
		}
	})
	t.Run("message while creating", func(t *testing.T) {
		fs := newSessionFakeState(t)
		h := newTestCityHandler(t, fs)
		info := createTestSession(t, fs.cityBeadStore, fs.sp, "Creating")
		_ = fs.sp.Stop(info.SessionName)
		if err := fs.cityBeadStore.SetMetadataBatch(info.ID, map[string]string{"state": "creating"}); err != nil {
			t.Fatal(err)
		}
		success := submitWithoutResume(t, fs, h, info.ID, `{"message":"second"}`)
		if b, _ := fs.cityBeadStore.Get(info.ID); !success.Queued || success.WillStart != nil || b.Metadata["wake_request"] != "" {
			t.Fatalf("queued = %v, will_start = %v, wake_request = %q; want queued, unknown, no wake on a creating row", success.Queued, success.WillStart, b.Metadata["wake_request"])
		}
	})
	t.Run("active row whose runtime died", func(t *testing.T) {
		fs := newSessionFakeState(t)
		h := newTestCityHandler(t, fs)
		info := createTestSession(t, fs.cityBeadStore, fs.sp, "Dead")
		_ = fs.sp.Stop(info.SessionName)
		success := submitWithoutResume(t, fs, h, info.ID, `{"message":"hello"}`)
		if b, _ := fs.cityBeadStore.Get(info.ID); !success.Queued || success.WillStart != nil || b.Metadata["wake_request"] != "" {
			t.Fatalf("queued = %v, will_start = %v, wake_request = %q; want queued and unknown, not a claimed start", success.Queued, success.WillStart, b.Metadata["wake_request"])
		}
	})
}

// TestDeferralFor: a lost wake CAS still pokes but claims nothing; a held
// row neither pokes nor starts; only a recorded wake asks the refuser; a
// row that is not dormant pokes and claims nothing. Kills a contended wake
// treated as a failure (no poke) or as a start.
func TestDeferralFor(t *testing.T) {
	for _, tc := range []struct {
		outcome session.WakeRequestOutcome
		err     error
		want    deferral
	}{
		{err: fmt.Errorf("x: %w", session.ErrWakeRequestContended), want: deferUnknown},
		{err: errors.New("store down"), want: deferFailed},
		{outcome: session.WakeHeld, want: deferHeld},
		{outcome: session.WakeRecorded, want: deferAsk},
		{outcome: session.WakeNotDormant, want: deferUnknown},
	} {
		if got := deferralFor(tc.outcome, tc.err); got != tc.want {
			t.Errorf("deferralFor(%v, %v) = %d, want %d", tc.outcome, tc.err, got, tc.want)
		}
	}
}

// TestBackgroundMessageNeverStartsInProcess is review finding C-4: extmsg's
// background send routes like ResumeViaController. A message to an asleep
// session is queued, the wake recorded and the session handed to the
// controller; nothing starts in the controller's process. Kills a background
// send that starts the runtime itself.
func TestBackgroundMessageNeverStartsInProcess(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Asleep")
	if err := session.NewManagerWithOptions(fs.cityBeadStore, fs.sp, session.WithCityPath(fs.cityPath)).Suspend(info.ID); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	if err := fs.cityBeadStore.SetMetadataBatch(info.ID, map[string]string{"state": "asleep", "suspended_at": "", "held_until": "", "sleep_intent": ""}); err != nil {
		t.Fatal(err)
	}
	startsBefore := fs.sp.CountCalls("Start", info.SessionName)
	if err := srv.sendBackgroundMessageToSession(context.Background(), fs.cityBeadStore, info.ID, "ping"); err != nil {
		t.Fatalf("sendBackgroundMessageToSession: %v", err)
	}
	b, err := fs.cityBeadStore.Get(info.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fs.sp.CountCalls("Start", info.SessionName) != startsBefore || b.Metadata["wake_request"] == "" ||
		!slices.Contains(fs.enqueuedKeys(), reconcilekey.Session(info.ID)) {
		t.Fatalf("starts = %d, wake_request = %q, enqueued = %v; want the start left to the controller",
			fs.sp.CountCalls("Start", info.SessionName)-startsBefore, b.Metadata["wake_request"], fs.enqueuedKeys())
	}
}
