package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gastownhall/gascity/internal/mail"
)

func TestMailLifecycle(t *testing.T) {
	state := newFakeState(t)
	srv := New(state)

	// Send a message.
	body := `{"from":"mayor","to":"worker","subject":"Review needed","body":"Please check gc-456"}`
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

	// Check inbox.
	req = httptest.NewRequest("GET", "/v0/mail?agent=worker", nil)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	var inbox struct {
		Items []mail.Message `json:"items"`
		Total int            `json:"total"`
	}
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
	req = httptest.NewRequest("GET", "/v0/mail?agent=worker", nil)
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
	mp := state.mailProvs["myrig"]
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
	mp := state.mailProvs["myrig"]
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
	mp := state.mailProvs["myrig"]

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
	mp := state.mailProvs["myrig"]

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
	mp := state.mailProvs["myrig"]
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
