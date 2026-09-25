package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// These tests pin the Idempotency-Key wire contract on the session action
// endpoints (submit, messages, respond): a client whose request timed out can
// retry with the same key and get the original response back without the
// prompt or interaction response being delivered a second time.

// countSessionNudges counts provider nudges to sessionName carrying message.
func countSessionNudges(sp *runtime.Fake, sessionName, message string) int {
	n := 0
	for _, call := range sp.SnapshotCalls() {
		if call.Name != sessionName || !strings.HasPrefix(call.Method, "Nudge") {
			continue
		}
		if strings.Contains(call.Message, message) {
			n++
		}
	}
	return n
}

// countSessionResponds counts provider Respond calls to sessionName.
func countSessionResponds(sp *runtime.Fake, sessionName string) int {
	n := 0
	for _, call := range sp.SnapshotCalls() {
		if call.Name == sessionName && call.Method == "Respond" {
			n++
		}
	}
	return n
}

func postSessionAction(t *testing.T, h http.Handler, url, key, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := newPostRequest(url, strings.NewReader(body))
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func queuedSubmitCount(t *testing.T, cityPath, sessionID string) int {
	t.Helper()
	state, err := nudgequeue.LoadState(cityPath)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	n := 0
	for _, item := range state.Pending {
		if item.SessionID == sessionID {
			n++
		}
	}
	return n
}

func TestSessionSubmitIdempotentReplay(t *testing.T) {
	fs := newSessionFakeState(t)
	h := newTestCityHandler(t, fs)
	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Submit Replay")
	url := cityURL(fs, "/session/") + info.ID + "/submit"
	body := `{"message":"later please","intent":"follow_up"}`

	first := postSessionAction(t, h, url, "submit-1", body)
	if first.Code != http.StatusAccepted {
		t.Fatalf("first submit: status = %d, want 202; body = %s", first.Code, first.Body.String())
	}
	firstBody := first.Body.String()
	accepted := decodeAsyncAccepted(t, strings.NewReader(firstBody))

	replay := postSessionAction(t, h, url, "submit-1", body)
	if replay.Code != http.StatusAccepted {
		t.Fatalf("replay: status = %d, want 202; body = %s", replay.Code, replay.Body.String())
	}
	if replay.Body.String() != firstBody {
		t.Fatalf("replay body = %s, want first-response body %s", replay.Body.String(), firstBody)
	}

	success, failure := waitForSessionSubmitResult(t, fs.eventProv, accepted.RequestID)
	if success == nil {
		t.Fatalf("session submit failed: %s: %s", failure.ErrorCode, failure.ErrorMessage)
	}
	if got := queuedSubmitCount(t, fs.cityPath, info.ID); got != 1 {
		t.Fatalf("queued submits = %d, want 1 (replay re-delivered the prompt)", got)
	}
}

func TestSessionSubmitIdempotencyMismatch(t *testing.T) {
	fs := newSessionFakeState(t)
	h := newTestCityHandler(t, fs)
	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Submit Mismatch")
	url := cityURL(fs, "/session/") + info.ID + "/submit"

	first := postSessionAction(t, h, url, "submit-1", `{"message":"first","intent":"follow_up"}`)
	if first.Code != http.StatusAccepted {
		t.Fatalf("first submit: status = %d, want 202; body = %s", first.Code, first.Body.String())
	}
	accepted := decodeAsyncAccepted(t, first.Body)

	mismatch := postSessionAction(t, h, url, "submit-1", `{"message":"second","intent":"follow_up"}`)
	if mismatch.Code != http.StatusUnprocessableEntity {
		t.Fatalf("mismatch: status = %d, want 422; body = %s", mismatch.Code, mismatch.Body.String())
	}
	if !strings.Contains(mismatch.Body.String(), "idempotency") {
		t.Fatalf("mismatch body = %s, want idempotency problem", mismatch.Body.String())
	}

	if success, failure := waitForSessionSubmitResult(t, fs.eventProv, accepted.RequestID); success == nil {
		t.Fatalf("session submit failed: %s: %s", failure.ErrorCode, failure.ErrorMessage)
	}
	if got := queuedSubmitCount(t, fs.cityPath, info.ID); got != 1 {
		t.Fatalf("queued submits = %d, want 1 (mismatched body was delivered)", got)
	}
}

func TestSessionSubmitWithoutKeyDeliversEachRequest(t *testing.T) {
	fs := newSessionFakeState(t)
	h := newTestCityHandler(t, fs)
	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Submit No Key")
	url := cityURL(fs, "/session/") + info.ID + "/submit"
	body := `{"message":"again","intent":"follow_up"}`

	var requestIDs []string
	for i := 0; i < 2; i++ {
		rec := postSessionAction(t, h, url, "", body)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("submit %d: status = %d, want 202; body = %s", i, rec.Code, rec.Body.String())
		}
		requestIDs = append(requestIDs, decodeAsyncAccepted(t, rec.Body).RequestID)
	}
	if requestIDs[0] == requestIDs[1] {
		t.Fatalf("keyless submits share request_id %q, want distinct requests", requestIDs[0])
	}
	for _, id := range requestIDs {
		if success, failure := waitForSessionSubmitResult(t, fs.eventProv, id); success == nil {
			t.Fatalf("session submit %s failed: %s: %s", id, failure.ErrorCode, failure.ErrorMessage)
		}
	}
	if got := queuedSubmitCount(t, fs.cityPath, info.ID); got != 2 {
		t.Fatalf("queued submits = %d, want 2 without Idempotency-Key", got)
	}
}

// TestSessionActionSameKeyDifferentSessionsIndependent pins per-target
// scoping on every session action: one Idempotency-Key used against two
// sessions must deliver to both, never replay the first session's response.
func TestSessionActionSameKeyDifferentSessionsIndependent(t *testing.T) {
	cases := []struct {
		action string
		body   string
		// deliveredOnce reports whether sessionID got exactly one delivery.
		deliveredOnce func(t *testing.T, fs *fakeState, info session.Info, rec *httptest.ResponseRecorder) bool
	}{
		{
			action: "submit",
			body:   `{"message":"hello","intent":"follow_up"}`,
			deliveredOnce: func(t *testing.T, fs *fakeState, info session.Info, rec *httptest.ResponseRecorder) bool {
				id := decodeAsyncAccepted(t, rec.Body).RequestID
				if success, failure := waitForSessionSubmitResult(t, fs.eventProv, id); success == nil {
					t.Fatalf("session submit %s failed: %s: %s", id, failure.ErrorCode, failure.ErrorMessage)
				}
				return queuedSubmitCount(t, fs.cityPath, info.ID) == 1
			},
		},
		{
			action: "messages",
			body:   `{"message":"hello"}`,
			deliveredOnce: func(t *testing.T, fs *fakeState, info session.Info, rec *httptest.ResponseRecorder) bool {
				id := decodeAsyncAccepted(t, rec.Body).RequestID
				if success, failure := waitForSessionMessageResult(t, fs.eventProv, id); success == nil {
					t.Fatalf("session message %s failed: %s: %s", id, failure.ErrorCode, failure.ErrorMessage)
				}
				return countSessionNudges(fs.sp, info.SessionName, "hello") == 1
			},
		},
		{
			action: "respond",
			body:   `{"request_id":"req-1","action":"approve"}`,
			deliveredOnce: func(t *testing.T, fs *fakeState, info session.Info, rec *httptest.ResponseRecorder) bool {
				var got struct {
					ID string `json:"id"`
				}
				if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
					t.Fatalf("decode respond body: %v", err)
				}
				if got.ID != info.ID {
					t.Fatalf("respond body id = %q, want %q (replayed another session's response)", got.ID, info.ID)
				}
				return countSessionResponds(fs.sp, info.SessionName) == 1
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.action, func(t *testing.T) {
			fs := newSessionFakeState(t)
			h := newTestCityHandler(t, fs)
			sessions := []session.Info{
				createTestSession(t, fs.cityBeadStore, fs.sp, "Session A"),
				createTestSession(t, fs.cityBeadStore, fs.sp, "Session B"),
			}
			if tc.action == "respond" {
				for _, info := range sessions {
					fs.sp.SetPendingInteraction(info.SessionName, &runtime.PendingInteraction{
						RequestID: "req-1",
						Kind:      "approval",
						Prompt:    "approve?",
					})
				}
			}
			for _, info := range sessions {
				rec := postSessionAction(t, h, cityURL(fs, "/session/")+info.ID+"/"+tc.action, "shared-key", tc.body)
				if rec.Code != http.StatusAccepted {
					t.Fatalf("%s to %s: status = %d, want 202; body = %s", tc.action, info.ID, rec.Code, rec.Body.String())
				}
				if !tc.deliveredOnce(t, fs, info, rec) {
					t.Fatalf("%s to %s: want exactly one delivery; calls = %#v", tc.action, info.ID, fs.sp.SnapshotCalls())
				}
			}
		})
	}
}

// TestSessionSubmitIdempotencyIntentIsPartOfBody pins that intent is hashed:
// reusing a key with a different intent is a different request, not a replay.
func TestSessionSubmitIdempotencyIntentIsPartOfBody(t *testing.T) {
	fs := newSessionFakeState(t)
	h := newTestCityHandler(t, fs)
	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Submit Intent")
	url := cityURL(fs, "/session/") + info.ID + "/submit"

	first := postSessionAction(t, h, url, "submit-1", `{"message":"same text","intent":"follow_up"}`)
	if first.Code != http.StatusAccepted {
		t.Fatalf("first submit: status = %d, want 202; body = %s", first.Code, first.Body.String())
	}
	accepted := decodeAsyncAccepted(t, first.Body)

	mismatch := postSessionAction(t, h, url, "submit-1", `{"message":"same text","intent":"interrupt_now"}`)
	if mismatch.Code != http.StatusUnprocessableEntity {
		t.Fatalf("intent-only change: status = %d, want 422; body = %s", mismatch.Code, mismatch.Body.String())
	}
	if success, failure := waitForSessionSubmitResult(t, fs.eventProv, accepted.RequestID); success == nil {
		t.Fatalf("session submit failed: %s: %s", failure.ErrorCode, failure.ErrorMessage)
	}
}

// TestSessionSubmitAndMessagesKeepSeparateScopes pins that /submit and
// /messages on the same session do not share a key scope: with no intent the
// two bodies are identical, so a shared scope would replay submit's 202.
func TestSessionSubmitAndMessagesKeepSeparateScopes(t *testing.T) {
	fs := newSessionFakeState(t)
	h := newTestCityHandler(t, fs)
	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Submit Then Message")
	base := cityURL(fs, "/session/") + info.ID
	body := `{"message":"scoped text"}`

	submit := postSessionAction(t, h, base+"/submit", "shared-key", body)
	if submit.Code != http.StatusAccepted {
		t.Fatalf("submit: status = %d, want 202; body = %s", submit.Code, submit.Body.String())
	}
	submitID := decodeAsyncAccepted(t, submit.Body).RequestID

	message := postSessionAction(t, h, base+"/messages", "shared-key", body)
	if message.Code != http.StatusAccepted {
		t.Fatalf("messages: status = %d, want 202; body = %s", message.Code, message.Body.String())
	}
	messageID := decodeAsyncAccepted(t, message.Body).RequestID
	if submitID == messageID {
		t.Fatalf("messages replayed submit's request_id %q, want separate scopes", submitID)
	}
	if success, failure := waitForSessionSubmitResult(t, fs.eventProv, submitID); success == nil {
		t.Fatalf("session submit failed: %s: %s", failure.ErrorCode, failure.ErrorMessage)
	}
	if success, failure := waitForSessionMessageResult(t, fs.eventProv, messageID); success == nil {
		t.Fatalf("session message failed: %s: %s", failure.ErrorCode, failure.ErrorMessage)
	}
}

func TestSessionActionIdempotencyPathEscapesTarget(t *testing.T) {
	if got, want := sessionActionIdempotencyPath("a/b", "submit"), "/v0/session/a%2Fb/submit"; got != want {
		t.Fatalf("sessionActionIdempotencyPath = %q, want %q", got, want)
	}
	if got, want := sessionActionIdempotencyPath("gc-1", "respond"), "/v0/session/gc-1/respond"; got != want {
		t.Fatalf("sessionActionIdempotencyPath = %q, want %q", got, want)
	}
}

func TestSessionMessageIdempotentReplay(t *testing.T) {
	fs := newSessionFakeState(t)
	h := newTestCityHandler(t, fs)
	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Message Replay")
	url := cityURL(fs, "/session/") + info.ID + "/messages"
	body := `{"message":"hello once"}`

	first := postSessionAction(t, h, url, "msg-1", body)
	if first.Code != http.StatusAccepted {
		t.Fatalf("first message: status = %d, want 202; body = %s", first.Code, first.Body.String())
	}
	firstBody := first.Body.String()
	accepted := decodeAsyncAccepted(t, strings.NewReader(firstBody))

	replay := postSessionAction(t, h, url, "msg-1", body)
	if replay.Code != http.StatusAccepted {
		t.Fatalf("replay: status = %d, want 202; body = %s", replay.Code, replay.Body.String())
	}
	if replay.Body.String() != firstBody {
		t.Fatalf("replay body = %s, want first-response body %s", replay.Body.String(), firstBody)
	}

	success, failure := waitForSessionMessageResult(t, fs.eventProv, accepted.RequestID)
	if success == nil {
		t.Fatalf("session message failed: %s: %s", failure.ErrorCode, failure.ErrorMessage)
	}
	if got := countSessionNudges(fs.sp, info.SessionName, "hello once"); got != 1 {
		t.Fatalf("nudges = %d, want 1 (replay re-delivered the message); calls = %#v", got, fs.sp.SnapshotCalls())
	}
}

func TestSessionMessageIdempotencyMismatch(t *testing.T) {
	fs := newSessionFakeState(t)
	h := newTestCityHandler(t, fs)
	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Message Mismatch")
	url := cityURL(fs, "/session/") + info.ID + "/messages"

	first := postSessionAction(t, h, url, "msg-1", `{"message":"first text"}`)
	if first.Code != http.StatusAccepted {
		t.Fatalf("first message: status = %d, want 202; body = %s", first.Code, first.Body.String())
	}
	accepted := decodeAsyncAccepted(t, first.Body)

	mismatch := postSessionAction(t, h, url, "msg-1", `{"message":"second text"}`)
	if mismatch.Code != http.StatusUnprocessableEntity {
		t.Fatalf("mismatch: status = %d, want 422; body = %s", mismatch.Code, mismatch.Body.String())
	}

	if success, failure := waitForSessionMessageResult(t, fs.eventProv, accepted.RequestID); success == nil {
		t.Fatalf("session message failed: %s: %s", failure.ErrorCode, failure.ErrorMessage)
	}
	if got := countSessionNudges(fs.sp, info.SessionName, "second text"); got != 0 {
		t.Fatalf("mismatched message delivered %d times, want 0", got)
	}
}

func TestSessionMessageWithoutKeyDeliversEachRequest(t *testing.T) {
	fs := newSessionFakeState(t)
	h := newTestCityHandler(t, fs)
	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Message No Key")
	url := cityURL(fs, "/session/") + info.ID + "/messages"

	var requestIDs []string
	for i := 0; i < 2; i++ {
		rec := postSessionAction(t, h, url, "", `{"message":"hello twice"}`)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("message %d: status = %d, want 202; body = %s", i, rec.Code, rec.Body.String())
		}
		requestIDs = append(requestIDs, decodeAsyncAccepted(t, rec.Body).RequestID)
	}
	for _, id := range requestIDs {
		if success, failure := waitForSessionMessageResult(t, fs.eventProv, id); success == nil {
			t.Fatalf("session message %s failed: %s: %s", id, failure.ErrorCode, failure.ErrorMessage)
		}
	}
	if got := countSessionNudges(fs.sp, info.SessionName, "hello twice"); got != 2 {
		t.Fatalf("nudges = %d, want 2 without Idempotency-Key", got)
	}
}

func TestSessionRespondIdempotentReplay(t *testing.T) {
	fs := newSessionFakeState(t)
	h := newTestCityHandler(t, fs)
	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Respond Replay")
	fs.sp.SetPendingInteraction(info.SessionName, &runtime.PendingInteraction{
		RequestID: "req-1",
		Kind:      "approval",
		Prompt:    "approve?",
	})
	url := cityURL(fs, "/session/") + info.ID + "/respond"
	body := `{"request_id":"req-1","action":"approve"}`

	first := postSessionAction(t, h, url, "respond-1", body)
	if first.Code != http.StatusAccepted {
		t.Fatalf("first respond: status = %d, want 202; body = %s", first.Code, first.Body.String())
	}
	replay := postSessionAction(t, h, url, "respond-1", body)
	if replay.Code != http.StatusAccepted {
		t.Fatalf("replay: status = %d, want 202; body = %s", replay.Code, replay.Body.String())
	}
	if replay.Body.String() != first.Body.String() {
		t.Fatalf("replay body = %s, want first-response body %s", replay.Body.String(), first.Body.String())
	}
	if got := countSessionResponds(fs.sp, info.SessionName); got != 1 {
		t.Fatalf("Respond calls = %d, want 1 (replay re-sent the response)", got)
	}

	mismatch := postSessionAction(t, h, url, "respond-1", `{"request_id":"req-1","action":"deny"}`)
	if mismatch.Code != http.StatusUnprocessableEntity {
		t.Fatalf("mismatch: status = %d, want 422; body = %s", mismatch.Code, mismatch.Body.String())
	}
	if got := countSessionResponds(fs.sp, info.SessionName); got != 1 {
		t.Fatalf("Respond calls after mismatch = %d, want 1", got)
	}
}

func TestSessionRespondFailureReleasesKey(t *testing.T) {
	fs := newSessionFakeState(t)
	h := newTestCityHandler(t, fs)
	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Respond Retry")
	url := cityURL(fs, "/session/") + info.ID + "/respond"
	body := `{"request_id":"req-1","action":"approve"}`

	// No pending interaction yet: the respond fails and must not pin the key.
	failed := postSessionAction(t, h, url, "respond-1", body)
	if failed.Code != http.StatusConflict {
		t.Fatalf("respond without pending: status = %d, want 409; body = %s", failed.Code, failed.Body.String())
	}

	fs.sp.SetPendingInteraction(info.SessionName, &runtime.PendingInteraction{
		RequestID: "req-1",
		Kind:      "approval",
		Prompt:    "approve?",
	})
	retry := postSessionAction(t, h, url, "respond-1", body)
	if retry.Code != http.StatusAccepted {
		t.Fatalf("retry after failure: status = %d, want 202; body = %s", retry.Code, retry.Body.String())
	}
}

// blockingRespondProvider holds Respond open until release is closed so a
// concurrent same-key request observes the in-flight claim.
type blockingRespondProvider struct {
	*runtime.Fake
	entered chan struct{}
	release chan struct{}
}

func (p *blockingRespondProvider) Respond(name string, response runtime.InteractionResponse) error {
	close(p.entered)
	<-p.release
	return p.Fake.Respond(name, response)
}

func TestSessionRespondInFlightReturnsConflict(t *testing.T) {
	fs := newSessionFakeState(t)
	blocking := &blockingRespondProvider{Fake: fs.sp, entered: make(chan struct{}), release: make(chan struct{})}
	fs.sessionProvider = blocking
	h := newTestCityHandler(t, fs)
	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Respond In Flight")
	fs.sp.SetPendingInteraction(info.SessionName, &runtime.PendingInteraction{
		RequestID: "req-1",
		Kind:      "approval",
		Prompt:    "approve?",
	})
	url := cityURL(fs, "/session/") + info.ID + "/respond"
	body := `{"request_id":"req-1","action":"approve"}`

	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { firstDone <- postSessionAction(t, h, url, "respond-1", body) }()
	<-blocking.entered

	inFlight := postSessionAction(t, h, url, "respond-1", body)
	if inFlight.Code != http.StatusConflict || !strings.Contains(inFlight.Body.String(), "idempotency-in-flight") {
		t.Fatalf("in-flight repeat: status = %d body = %s, want 409 idempotency-in-flight", inFlight.Code, inFlight.Body.String())
	}

	close(blocking.release)
	first := <-firstDone
	if first.Code != http.StatusAccepted {
		t.Fatalf("first respond: status = %d, want 202; body = %s", first.Code, first.Body.String())
	}
	replay := postSessionAction(t, h, url, "respond-1", body)
	if replay.Code != http.StatusAccepted || replay.Body.String() != first.Body.String() {
		t.Fatalf("replay after completion: status = %d body = %s, want 202 %s", replay.Code, replay.Body.String(), first.Body.String())
	}
	if got := countSessionResponds(fs.sp, info.SessionName); got != 1 {
		t.Fatalf("Respond calls = %d, want 1", got)
	}
}
