package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/runtime"
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

func TestSessionSubmitSameKeyDifferentSessionsIndependent(t *testing.T) {
	fs := newSessionFakeState(t)
	h := newTestCityHandler(t, fs)
	a := createTestSession(t, fs.cityBeadStore, fs.sp, "Submit A")
	b := createTestSession(t, fs.cityBeadStore, fs.sp, "Submit B")
	body := `{"message":"hello","intent":"follow_up"}`

	var requestIDs []string
	for _, id := range []string{a.ID, b.ID} {
		rec := postSessionAction(t, h, cityURL(fs, "/session/")+id+"/submit", "shared-key", body)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("submit to %s: status = %d, want 202; body = %s", id, rec.Code, rec.Body.String())
		}
		requestIDs = append(requestIDs, decodeAsyncAccepted(t, rec.Body).RequestID)
	}
	if requestIDs[0] == requestIDs[1] {
		t.Fatalf("same key on two sessions replayed request_id %q, want independent scopes", requestIDs[0])
	}
	for _, id := range requestIDs {
		if success, failure := waitForSessionSubmitResult(t, fs.eventProv, id); success == nil {
			t.Fatalf("session submit %s failed: %s: %s", id, failure.ErrorCode, failure.ErrorMessage)
		}
	}
	if queuedSubmitCount(t, fs.cityPath, a.ID) != 1 || queuedSubmitCount(t, fs.cityPath, b.ID) != 1 {
		t.Fatalf("want one queued submit per session")
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
