package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/mail"
	"github.com/gastownhall/gascity/internal/session"
)

func TestMailLifecycle(t *testing.T) {
	state := newSessionFakeState(t)
	srv := New(state)

	body := `{"from":"mayor","to":"myrig/worker","subject":"Review needed","body":"Please check gc-456"}`
	req := newPostRequest("/v0/mail", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("send status = %d, want %d, body: %s", rec.Code, http.StatusCreated, rec.Body.String())
	}

	var sent mail.Message
	json.NewDecoder(rec.Body).Decode(&sent) //nolint:errcheck
	if sent.Subject != "Review needed" {
		t.Errorf("Subject = %q, want %q", sent.Subject, "Review needed")
	}
	if sent.To == "" {
		t.Fatal("To is empty, want concrete materialized session ID")
	}

	// Check inbox using the same logical named target the send API accepts.
	req = httptest.NewRequest("GET", "/v0/mail?agent=myrig/worker", nil)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	var inbox struct {
		Items []mail.Message `json:"items"`
		Total int            `json:"total"`
	}
	json.NewDecoder(rec.Body).Decode(&inbox) //nolint:errcheck
	if inbox.Total != 1 {
		t.Fatalf("logical-target inbox Total = %d, want 1", inbox.Total)
	}
	if len(inbox.Items) != 1 || inbox.Items[0].ID != sent.ID {
		t.Fatalf("logical-target inbox items = %#v, want sent message %s", inbox.Items, sent.ID)
	}

	req = httptest.NewRequest("GET", "/v0/mail/count?agent=myrig/worker", nil)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	var count map[string]int
	json.NewDecoder(rec.Body).Decode(&count) //nolint:errcheck
	if count["total"] != 1 || count["unread"] != 1 {
		t.Fatalf("logical-target count = %#v, want total=1 unread=1", count)
	}

	// Named-session mail remains visible through logical queries after the
	// configured session is restarted onto a new bead.
	oldSession, err := state.cityBeadStore.Get(sent.To)
	if err != nil {
		t.Fatalf("get materialized session %s: %v", sent.To, err)
	}
	if sessionName := oldSession.Metadata["session_name"]; sessionName != "" {
		if err := state.sp.Stop(sessionName); err != nil {
			t.Fatalf("stop materialized session %s: %v", sessionName, err)
		}
	}
	if err := state.cityBeadStore.Close(sent.To); err != nil {
		t.Fatalf("close materialized session %s: %v", sent.To, err)
	}
	freshID, err := srv.resolveSessionIDMaterializingNamed(state.cityBeadStore, "myrig/worker")
	if err != nil {
		t.Fatalf("rematerialize named session: %v", err)
	}
	if freshID == sent.To {
		t.Fatalf("rematerialized session ID = %q, want a fresh bead after close", freshID)
	}

	req = httptest.NewRequest("GET", "/v0/mail?agent=myrig/worker&status=all", nil)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	json.NewDecoder(rec.Body).Decode(&inbox) //nolint:errcheck
	if inbox.Total != 1 {
		t.Fatalf("logical-target inbox after rematerialize Total = %d, want 1", inbox.Total)
	}
	if len(inbox.Items) != 1 || inbox.Items[0].ID != sent.ID {
		t.Fatalf("logical-target inbox after rematerialize items = %#v, want sent message %s", inbox.Items, sent.ID)
	}

	req = httptest.NewRequest("GET", "/v0/mail/count?agent=myrig/worker", nil)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	json.NewDecoder(rec.Body).Decode(&count) //nolint:errcheck
	if count["total"] != 1 || count["unread"] != 1 {
		t.Fatalf("logical-target count after rematerialize = %#v, want total=1 unread=1", count)
	}

	// Check inbox using the concrete materialized session ID.
	req = httptest.NewRequest("GET", "/v0/mail?agent="+sent.To, nil)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	json.NewDecoder(rec.Body).Decode(&inbox) //nolint:errcheck
	if inbox.Total != 1 {
		t.Fatalf("inbox Total = %d, want 1", inbox.Total)
	}

	// Mark read.
	req = newPostRequest("/v0/mail/"+sent.ID+"/read", nil)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("read status = %d, want %d", rec.Code, http.StatusOK)
	}

	// Inbox should be empty now (only unread).
	req = httptest.NewRequest("GET", "/v0/mail?agent="+sent.To, nil)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	json.NewDecoder(rec.Body).Decode(&inbox) //nolint:errcheck
	if inbox.Total != 0 {
		t.Errorf("inbox after read: Total = %d, want 0", inbox.Total)
	}

	// Get still works.
	req = httptest.NewRequest("GET", "/v0/mail/"+sent.ID, nil)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("get status = %d, want %d", rec.Code, http.StatusOK)
	}

	// Archive.
	req = newPostRequest("/v0/mail/"+sent.ID+"/archive", nil)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("archive status = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestMailSendAllowsHumanRecipient(t *testing.T) {
	state := newFakeState(t)
	srv := New(state)

	body := `{"from":"myrig/worker","to":"human","subject":"Done","body":"Task complete"}`
	req := newPostRequest("/v0/mail", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("send status = %d, want %d, body: %s", rec.Code, http.StatusCreated, rec.Body.String())
	}

	var sent mail.Message
	json.NewDecoder(rec.Body).Decode(&sent) //nolint:errcheck
	if sent.To != "human" {
		t.Fatalf("sent To = %q, want human", sent.To)
	}

	req = httptest.NewRequest("GET", "/v0/mail?agent=human", nil)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	var inbox struct {
		Items []mail.Message `json:"items"`
		Total int            `json:"total"`
	}
	json.NewDecoder(rec.Body).Decode(&inbox) //nolint:errcheck
	if inbox.Total != 1 {
		t.Fatalf("human inbox Total = %d, want 1", inbox.Total)
	}
	if len(inbox.Items) != 1 || inbox.Items[0].ID != sent.ID {
		t.Fatalf("human inbox items = %#v, want sent message %s", inbox.Items, sent.ID)
	}
}

func TestMailNamedSessionQueryExcludesOrdinaryBackingTemplateSessions(t *testing.T) {
	state := newSessionFakeState(t)
	srv := New(state)

	namedID, err := srv.resolveSessionIDMaterializingNamed(state.cityBeadStore, "myrig/worker")
	if err != nil {
		t.Fatalf("materialize named session: %v", err)
	}
	ordinary, err := state.cityBeadStore.Create(beads.Bead{
		Title:  "ordinary worker",
		Type:   session.BeadType,
		Status: "open",
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name": "s-gc-ordinary-worker",
			"template":     "myrig/worker",
			"agent_name":   "myrig/worker",
			"state":        "active",
		},
	})
	if err != nil {
		t.Fatalf("create ordinary backing-template session: %v", err)
	}
	namedMsg, err := state.cityMailProv.Send("human", namedID, "named", "named body")
	if err != nil {
		t.Fatalf("send named message: %v", err)
	}
	legacyMsg, err := state.cityMailProv.Send("human", "myrig/worker", "legacy named", "legacy body")
	if err != nil {
		t.Fatalf("send legacy logical named message: %v", err)
	}
	ordinaryMsg, err := state.cityMailProv.Send("human", ordinary.ID, "ordinary", "ordinary body")
	if err != nil {
		t.Fatalf("send ordinary message: %v", err)
	}

	req := httptest.NewRequest("GET", "/v0/mail?agent=myrig/worker&status=all", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	var inbox struct {
		Items []mail.Message `json:"items"`
		Total int            `json:"total"`
	}
	json.NewDecoder(rec.Body).Decode(&inbox) //nolint:errcheck
	if inbox.Total != 2 {
		t.Fatalf("logical named inbox Total = %d, want 2; items=%#v", inbox.Total, inbox.Items)
	}
	gotIDs := make(map[string]bool, len(inbox.Items))
	for _, item := range inbox.Items {
		gotIDs[item.ID] = true
	}
	if !gotIDs[namedMsg.ID] || !gotIDs[legacyMsg.ID] || gotIDs[ordinaryMsg.ID] {
		t.Fatalf("logical named inbox item IDs = %#v, want named %s and legacy %s only", gotIDs, namedMsg.ID, legacyMsg.ID)
	}

	req = httptest.NewRequest("GET", "/v0/mail/count?agent=myrig/worker", nil)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	var count map[string]int
	json.NewDecoder(rec.Body).Decode(&count) //nolint:errcheck
	if count["total"] != 2 || count["unread"] != 2 {
		t.Fatalf("logical named count = %#v, want total=2 unread=2", count)
	}
}

func TestMailSendValidation(t *testing.T) {
	state := newFakeState(t)
	srv := New(state)

	// Missing required fields.
	body := `{"from":"mayor"}`
	req := newPostRequest("/v0/mail", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}

	var apiErr Error
	json.NewDecoder(rec.Body).Decode(&apiErr) //nolint:errcheck
	if len(apiErr.Details) != 2 {
		t.Errorf("Details count = %d, want 2", len(apiErr.Details))
	}
}

func TestMailCount(t *testing.T) {
	state := newFakeState(t)
	mp := state.cityMailProv
	mp.Send("a", "b", "msg1", "body1") //nolint:errcheck
	mp.Send("a", "b", "msg2", "body2") //nolint:errcheck
	srv := New(state)

	req := httptest.NewRequest("GET", "/v0/mail/count?agent=b", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	var resp map[string]int
	json.NewDecoder(rec.Body).Decode(&resp) //nolint:errcheck
	if resp["unread"] != 2 {
		t.Errorf("unread = %d, want 2", resp["unread"])
	}
}

func TestMailDelete(t *testing.T) {
	state := newFakeState(t)
	mp := state.cityMailProv
	msg, _ := mp.Send("mayor", "worker", "To delete", "content")
	srv := New(state)

	req := httptest.NewRequest("DELETE", "/v0/mail/"+msg.ID, nil)
	req.Header.Set("X-GC-Request", "true")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("delete status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	// After delete (soft delete/archive), message should no longer appear in inbox.
	req = httptest.NewRequest("GET", "/v0/mail?agent=worker", nil)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

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
	srv := New(state)

	req := httptest.NewRequest("DELETE", "/v0/mail/nonexistent", nil)
	req.Header.Set("X-GC-Request", "true")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

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

	srv := New(state)

	// Default (no status) returns only unread — both should appear.
	req := httptest.NewRequest("GET", "/v0/mail?agent=worker", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

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
	req = httptest.NewRequest("GET", "/v0/mail?agent=worker&status=unread", nil)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	json.NewDecoder(rec.Body).Decode(&resp) //nolint:errcheck
	if resp.Total != 1 {
		t.Fatalf("unread after mark-read Total = %d, want 1", resp.Total)
	}

	// status=all should return both (read + unread).
	req = httptest.NewRequest("GET", "/v0/mail?agent=worker&status=all", nil)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

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

	srv := New(state)

	// status=all without rig param aggregates across all rigs.
	req := httptest.NewRequest("GET", "/v0/mail?agent=worker&status=all", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

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
	srv := New(state)

	req := httptest.NewRequest("GET", "/v0/mail?status=bogus", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status=bogus returned %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestMailReply(t *testing.T) {
	state := newFakeState(t)
	mp := state.cityMailProv
	msg, _ := mp.Send("mayor", "worker", "Initial", "content")
	srv := New(state)

	body := `{"from":"worker","subject":"Re: Initial","body":"Done!"}`
	req := newPostRequest("/v0/mail/"+msg.ID+"/reply", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

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
	srv := New(state)

	// List without rig filter — aggregation path.
	req := httptest.NewRequest("GET", "/v0/mail?status=all", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

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
	req = httptest.NewRequest("GET", "/v0/mail?rig=test-city&status=all", nil)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

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

	srv := New(state)

	req := httptest.NewRequest("GET", "/v0/mail/thread/"+msg.ThreadID+"?rig=test-city", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

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
	state := newSessionFakeState(t)
	srv := New(state)

	body := `{"rig":"test-city","from":"alice","to":"myrig/worker","subject":"Hi","body":"hello"}`
	req := newPostRequest("/v0/mail", bytes.NewBufferString(body))
	req.Header.Set("Idempotency-Key", "mail-send-1")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("first send status = %d, want %d, body: %s", rec.Code, http.StatusCreated, rec.Body.String())
	}

	req = newPostRequest("/v0/mail", bytes.NewBufferString(body))
	req.Header.Set("Idempotency-Key", "mail-send-1")
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

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
	srv := New(state)

	req := httptest.NewRequest("GET", "/v0/mail/"+msg.ID, nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

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
	srv := New(state)

	req := newPostRequest("/v0/mail/"+msg.ID+"/read", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

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
	srv := New(state)

	body := `{"from":"bob","subject":"Re: Hi","body":"reply"}`
	req := newPostRequest("/v0/mail/"+msg.ID+"/reply", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

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
