package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/mail"
	"github.com/gastownhall/gascity/internal/session"
)

func TestMailLifecycle(t *testing.T) {
	state := newFakeState(t)
	h := newTestCityHandler(t, state)

	// Send a message. Bare "worker" resolves to "myrig/worker" (the qualified name).
	body := `{"from":"mayor","to":"worker","subject":"Review needed","body":"Please check gc-456"}`
	req := newPostRequest(cityURL(state, "/mail"), bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("send status = %d, want %d, body: %s", rec.Code, http.StatusCreated, rec.Body.String())
	}

	var sent mail.Message
	json.NewDecoder(rec.Body).Decode(&sent) //nolint:errcheck
	if sent.Subject != "Review needed" {
		t.Errorf("Subject = %q, want %q", sent.Subject, "Review needed")
	}
	if sent.To != "myrig/worker" {
		t.Errorf("To = %q, want %q (bare name should resolve to qualified)", sent.To, "myrig/worker")
	}

	// Check inbox using the resolved qualified name.
	req = httptest.NewRequest("GET", cityURL(state, "/mail?agent=myrig/worker"), nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var inbox struct {
		Items []mail.Message `json:"items"`
		Total int            `json:"total"`
	}
	json.NewDecoder(rec.Body).Decode(&inbox) //nolint:errcheck
	if inbox.Total != 1 {
		t.Fatalf("inbox Total = %d, want 1", inbox.Total)
	}

	// Mark read.
	req = newPostRequest(cityURL(state, "/mail/")+sent.ID+"/read", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("read status = %d, want %d", rec.Code, http.StatusOK)
	}

	// Inbox should be empty now (only unread).
	req = httptest.NewRequest("GET", cityURL(state, "/mail?agent=myrig/worker"), nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	json.NewDecoder(rec.Body).Decode(&inbox) //nolint:errcheck
	if inbox.Total != 0 {
		t.Errorf("inbox after read: Total = %d, want 0", inbox.Total)
	}

	// Get still works.
	req = httptest.NewRequest("GET", cityURL(state, "/mail/")+sent.ID, nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("get status = %d, want %d", rec.Code, http.StatusOK)
	}
	var readMsg mail.Message
	json.NewDecoder(rec.Body).Decode(&readMsg) //nolint:errcheck
	if !readMsg.Read {
		t.Fatalf("get after read: Read = false, want true")
	}

	// Archive.
	req = newPostRequest(cityURL(state, "/mail/")+sent.ID+"/archive", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("archive status = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestMailMarkUnread(t *testing.T) {
	state := newFakeState(t)
	h := newTestCityHandler(t, state)

	body := `{"from":"mayor","to":"worker","subject":"Unread test","body":"check this"}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newPostRequest(cityURL(state, "/mail"), bytes.NewBufferString(body)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("send status = %d, want %d; body: %s", rec.Code, http.StatusCreated, rec.Body.String())
	}
	var sent mail.Message
	json.NewDecoder(rec.Body).Decode(&sent) //nolint:errcheck

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, newPostRequest(cityURL(state, "/mail/")+sent.ID+"/read", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("read status = %d, want %d", rec.Code, http.StatusOK)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, newPostRequest(cityURL(state, "/mail/")+sent.ID+"/mark-unread", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("mark-unread status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", cityURL(state, "/mail?agent=myrig/worker"), nil))
	var inbox struct {
		Items []mail.Message `json:"items"`
		Total int            `json:"total"`
	}
	json.NewDecoder(rec.Body).Decode(&inbox) //nolint:errcheck
	if inbox.Total != 1 {
		t.Fatalf("inbox after mark-unread: Total = %d, want 1 (message should reappear)", inbox.Total)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", cityURL(state, "/mail/")+sent.ID, nil))
	var unread mail.Message
	json.NewDecoder(rec.Body).Decode(&unread) //nolint:errcheck
	if unread.Read {
		t.Fatalf("get after mark-unread: Read = true, want false")
	}
}

func TestMailSendValidation(t *testing.T) {
	state := newFakeState(t)
	h := newTestCityHandler(t, state)

	// Missing required fields (to, subject).
	body := `{"from":"mayor"}`
	req := newPostRequest(cityURL(state, "/mail"), bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want %d; body = %s", rec.Code, http.StatusUnprocessableEntity, rec.Body.String())
	}

	// Huma validation errors follow RFC 9457: status, title, detail, errors[].
	// Each validation error is an entry in the errors array with a location
	// like "body.to" identifying the offending field.
	var apiErr struct {
		Status int `json:"status"`
		Errors []struct {
			Location string `json:"location"`
			Message  string `json:"message"`
		} `json:"errors"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&apiErr); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// Each missing required field yields one entry in the errors array.
	// Expect errors for both "to" and "subject" — Huma reports them with
	// location "body" and the field name in the message.
	var hasToErr, hasSubjectErr bool
	for _, e := range apiErr.Errors {
		if strings.Contains(e.Message, "to") {
			hasToErr = true
		}
		if strings.Contains(e.Message, "subject") {
			hasSubjectErr = true
		}
	}
	if !hasToErr {
		t.Errorf("missing validation error for 'to' field; errors = %+v", apiErr.Errors)
	}
	if !hasSubjectErr {
		t.Errorf("missing validation error for 'subject' field; errors = %+v", apiErr.Errors)
	}
}

func TestMailCount(t *testing.T) {
	state := newFakeState(t)
	mp := state.cityMailProv
	mp.Send("a", "b", "msg1", "body1") //nolint:errcheck
	mp.Send("a", "b", "msg2", "body2") //nolint:errcheck
	h := newTestCityHandler(t, state)

	req := httptest.NewRequest("GET", cityURL(state, "/mail/count?agent=b"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var resp map[string]int
	json.NewDecoder(rec.Body).Decode(&resp) //nolint:errcheck
	if resp["unread"] != 2 {
		t.Errorf("unread = %d, want 2", resp["unread"])
	}
}

func TestMailInboxSeesHistoricalAliasSessionAddedAfterInitialMiss(t *testing.T) {
	state := newFakeState(t)
	h := newTestCityHandler(t, state)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", cityURL(state, "/mail?agent=old-worker"), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("initial inbox status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	store := state.stores["myrig"]
	if _, err := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"alias":         "worker",
			"alias_history": "old-worker",
		},
	}); err != nil {
		t.Fatalf("Create session: %v", err)
	}
	if _, err := state.cityMailProv.Send("human", "worker", "Fresh session", "visible after initial miss"); err != nil {
		t.Fatalf("Send: %v", err)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", cityURL(state, "/mail?agent=old-worker"), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("second inbox status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var inbox struct {
		Items []mail.Message `json:"items"`
		Total int            `json:"total"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&inbox); err != nil {
		t.Fatalf("decode inbox: %v", err)
	}
	if inbox.Total != 1 {
		t.Fatalf("second inbox Total = %d, want 1", inbox.Total)
	}
	if len(inbox.Items) != 1 || inbox.Items[0].Body != "visible after initial miss" {
		t.Fatalf("second inbox items = %#v, want visible historical-alias message", inbox.Items)
	}
}

func TestMailDelete(t *testing.T) {
	state := newFakeState(t)
	mp := state.cityMailProv
	msg, _ := mp.Send("mayor", "worker", "To delete", "content")
	h := newTestCityHandler(t, state)

	req := httptest.NewRequest("DELETE", cityURL(state, "/mail/")+msg.ID, nil)
	req.Header.Set("X-GC-Request", "true")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("delete status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	// After delete (soft delete/archive), message should no longer appear in inbox.
	req = httptest.NewRequest("GET", cityURL(state, "/mail?agent=worker"), nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var inbox struct {
		Items []mail.Message `json:"items"`
		Total int            `json:"total"`
	}
	json.NewDecoder(rec.Body).Decode(&inbox) //nolint:errcheck
	if inbox.Total != 0 {
		t.Errorf("inbox after delete: Total = %d, want 0", inbox.Total)
	}
}

func TestMailDeleteNotFound(t *testing.T) {
	state := newFakeState(t)
	h := newTestCityHandler(t, state)

	req := httptest.NewRequest("DELETE", cityURL(state, "/mail/nonexistent"), nil)
	req.Header.Set("X-GC-Request", "true")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestMailListStatusAll(t *testing.T) {
	state := newFakeState(t)
	mp := state.cityMailProv

	// Send two messages to worker.
	mp.Send("mayor", "worker", "First", "body1")  //nolint:errcheck
	mp.Send("mayor", "worker", "Second", "body2") //nolint:errcheck

	h := newTestCityHandler(t, state)

	// Default (no status) returns only unread — both should appear.
	req := httptest.NewRequest("GET", cityURL(state, "/mail?agent=worker"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var resp struct {
		Items []mail.Message `json:"items"`
		Total int            `json:"total"`
	}
	json.NewDecoder(rec.Body).Decode(&resp) //nolint:errcheck
	if resp.Total != 2 {
		t.Fatalf("unread Total = %d, want 2", resp.Total)
	}

	// Mark the first message as read.
	mp.MarkRead(resp.Items[0].ID) //nolint:errcheck

	// Default (unread) should now return 1.
	req = httptest.NewRequest("GET", cityURL(state, "/mail?agent=worker&status=unread"), nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	json.NewDecoder(rec.Body).Decode(&resp) //nolint:errcheck
	if resp.Total != 1 {
		t.Fatalf("unread after mark-read Total = %d, want 1", resp.Total)
	}

	// status=all should return both (read + unread).
	req = httptest.NewRequest("GET", cityURL(state, "/mail?agent=worker&status=all"), nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=all returned %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	json.NewDecoder(rec.Body).Decode(&resp) //nolint:errcheck
	if resp.Total != 2 {
		t.Errorf("status=all Total = %d, want 2", resp.Total)
	}
}

func TestMailListStatusAllAcrossRigs(t *testing.T) {
	state := newFakeState(t)
	mp := state.cityMailProv

	mp.Send("mayor", "worker", "Msg1", "body1") //nolint:errcheck
	msg2, _ := mp.Send("mayor", "worker", "Msg2", "body2")
	mp.MarkRead(msg2.ID) //nolint:errcheck

	h := newTestCityHandler(t, state)

	// status=all without rig param aggregates across all rigs.
	req := httptest.NewRequest("GET", cityURL(state, "/mail?agent=worker&status=all"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=all returned %d, want %d", rec.Code, http.StatusOK)
	}

	var resp struct {
		Items []mail.Message `json:"items"`
		Total int            `json:"total"`
	}
	json.NewDecoder(rec.Body).Decode(&resp) //nolint:errcheck
	if resp.Total != 2 {
		t.Errorf("status=all across rigs Total = %d, want 2", resp.Total)
	}
}

func TestMailListStatusInvalid(t *testing.T) {
	state := newFakeState(t)
	h := newTestCityHandler(t, state)

	req := httptest.NewRequest("GET", cityURL(state, "/mail?status=bogus"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status=bogus returned %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestMailReply(t *testing.T) {
	state := newFakeState(t)
	mp := state.cityMailProv
	msg, _ := mp.Send("mayor", "worker", "Initial", "content")
	h := newTestCityHandler(t, state)

	body := `{"from":"worker","subject":"Re: Initial","body":"Done!"}`
	req := newPostRequest(cityURL(state, "/mail/")+msg.ID+"/reply", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("reply status = %d, want %d, body: %s", rec.Code, http.StatusCreated, rec.Body.String())
	}

	var reply mail.Message
	json.NewDecoder(rec.Body).Decode(&reply) //nolint:errcheck
	if reply.ThreadID == "" {
		t.Error("reply has no ThreadID")
	}
}

func TestMailListIncludesRig(t *testing.T) {
	state := newFakeState(t)
	mp := state.cityMailProv
	mp.Send("alice", "bob", "Hi", "hello") //nolint:errcheck
	h := newTestCityHandler(t, state)

	// List without rig filter — aggregation path.
	req := httptest.NewRequest("GET", cityURL(state, "/mail?status=all"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var resp struct {
		Items []mail.Message `json:"items"`
	}
	json.NewDecoder(rec.Body).Decode(&resp) //nolint:errcheck
	if len(resp.Items) == 0 {
		t.Fatal("expected at least 1 message")
	}
	if resp.Items[0].Rig != "test-city" {
		t.Errorf("Items[0].Rig = %q, want %q", resp.Items[0].Rig, "test-city")
	}

	// List with rig filter — single-rig path. The rig param is used as the tag.
	req = httptest.NewRequest("GET", cityURL(state, "/mail?rig=test-city&status=all"), nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	json.NewDecoder(rec.Body).Decode(&resp) //nolint:errcheck
	if len(resp.Items) == 0 {
		t.Fatal("expected at least 1 message")
	}
	if resp.Items[0].Rig != "test-city" {
		t.Errorf("Items[0].Rig = %q, want %q (single-rig path)", resp.Items[0].Rig, "test-city")
	}
}

func TestMailThreadIncludesRig(t *testing.T) {
	state := newFakeState(t)
	mp := state.cityMailProv
	msg, _ := mp.Send("alice", "bob", "Thread test", "body")

	// Reply to create a thread.
	mp.Reply(msg.ID, "bob", "Re: Thread test", "reply body") //nolint:errcheck

	h := newTestCityHandler(t, state)

	req := httptest.NewRequest("GET", cityURL(state, "/mail/thread/")+msg.ThreadID+"?rig=test-city", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var resp struct {
		Items []mail.Message `json:"items"`
	}
	json.NewDecoder(rec.Body).Decode(&resp) //nolint:errcheck
	if len(resp.Items) == 0 {
		t.Fatal("expected thread messages")
	}
	for i, m := range resp.Items {
		if m.Rig != "test-city" {
			t.Errorf("Items[%d].Rig = %q, want %q", i, m.Rig, "test-city")
		}
	}
}

func TestMailSendIdempotentReplayIncludesRig(t *testing.T) {
	state := newFakeState(t)
	h := newTestCityHandler(t, state)

	body := `{"rig":"test-city","from":"alice","to":"worker","subject":"Hi","body":"hello"}`
	req := newPostRequest(cityURL(state, "/mail"), bytes.NewBufferString(body))
	req.Header.Set("Idempotency-Key", "mail-send-1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("first send status = %d, want %d, body: %s", rec.Code, http.StatusCreated, rec.Body.String())
	}

	req = newPostRequest(cityURL(state, "/mail"), bytes.NewBufferString(body))
	req.Header.Set("Idempotency-Key", "mail-send-1")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("replayed send status = %d, want %d, body: %s", rec.Code, http.StatusCreated, rec.Body.String())
	}

	var msg mail.Message
	json.NewDecoder(rec.Body).Decode(&msg) //nolint:errcheck
	if msg.Rig != "test-city" {
		t.Fatalf("replayed send Rig = %q, want %q", msg.Rig, "test-city")
	}
}

func TestMailGetWithoutRigHintIncludesResolvedRig(t *testing.T) {
	state := newFakeState(t)
	mp := state.cityMailProv
	msg, _ := mp.Send("alice", "bob", "Hi", "hello")
	h := newTestCityHandler(t, state)

	req := httptest.NewRequest("GET", cityURL(state, "/mail/")+msg.ID, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("get status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var got mail.Message
	json.NewDecoder(rec.Body).Decode(&got) //nolint:errcheck
	if got.Rig != "test-city" {
		t.Fatalf("get Rig = %q, want %q", got.Rig, "test-city")
	}
}

func TestMailMutationEventsUseResolvedRigWithoutHint(t *testing.T) {
	state := newFakeState(t)
	ep := state.eventProv.(*events.Fake)
	mp := state.cityMailProv
	msg, _ := mp.Send("alice", "bob", "Hi", "hello")
	h := newTestCityHandler(t, state)

	req := newPostRequest(cityURL(state, "/mail/")+msg.ID+"/read", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("read status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if len(ep.Events) == 0 {
		t.Fatal("expected read event")
	}

	var payload struct {
		Rig string `json:"rig"`
	}
	if err := json.Unmarshal(ep.Events[len(ep.Events)-1].Payload, &payload); err != nil {
		t.Fatalf("unmarshal read payload: %v", err)
	}
	if payload.Rig != "test-city" {
		t.Fatalf("read event rig = %q, want %q", payload.Rig, "test-city")
	}
}

func TestMailReplyWithoutRigHintUsesResolvedRig(t *testing.T) {
	state := newFakeState(t)
	ep := state.eventProv.(*events.Fake)
	mp := state.cityMailProv
	msg, _ := mp.Send("alice", "bob", "Hi", "hello")
	h := newTestCityHandler(t, state)

	body := `{"from":"bob","subject":"Re: Hi","body":"reply"}`
	req := newPostRequest(cityURL(state, "/mail/")+msg.ID+"/reply", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("reply status = %d, want %d, body: %s", rec.Code, http.StatusCreated, rec.Body.String())
	}

	var reply mail.Message
	json.NewDecoder(rec.Body).Decode(&reply) //nolint:errcheck
	if reply.Rig != "test-city" {
		t.Fatalf("reply Rig = %q, want %q", reply.Rig, "test-city")
	}

	if len(ep.Events) == 0 {
		t.Fatal("expected reply event")
	}

	var payload struct {
		Rig     string       `json:"rig"`
		Message mail.Message `json:"message"`
	}
	if err := json.Unmarshal(ep.Events[len(ep.Events)-1].Payload, &payload); err != nil {
		t.Fatalf("unmarshal reply payload: %v", err)
	}
	if payload.Rig != "test-city" {
		t.Fatalf("reply event rig = %q, want %q", payload.Rig, "test-city")
	}
	if payload.Message.Rig != "test-city" {
		t.Fatalf("reply event message rig = %q, want %q", payload.Message.Rig, "test-city")
	}
}
