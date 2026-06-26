package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/api/genclient"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/mail"
	"github.com/gastownhall/gascity/internal/session"
)

// mailPartialReadTestDeadline is load-tolerant (2s vs 5ms); 5ms fails under CI CPU saturation (ga-97aayp).
const mailPartialReadTestDeadline = 2 * time.Second

type exactRecipientMailProvider struct {
	messages map[string][]mail.Message
}

func (p *exactRecipientMailProvider) Send(from, to, subject, body string) (mail.Message, error) {
	return mail.Message{}, fmt.Errorf("unexpected Send(%q, %q, %q, %q)", from, to, subject, body)
}

func (p *exactRecipientMailProvider) Inbox(recipient string) ([]mail.Message, error) {
	return p.unread(recipient), nil
}

func (p *exactRecipientMailProvider) Get(string) (mail.Message, error) {
	return mail.Message{}, mail.ErrNotFound
}

func (p *exactRecipientMailProvider) Read(string) (mail.Message, error) {
	return mail.Message{}, mail.ErrNotFound
}

func (p *exactRecipientMailProvider) MarkRead(string) error { return nil }

func (p *exactRecipientMailProvider) MarkUnread(string) error { return nil }

func (p *exactRecipientMailProvider) Archive(string) error { return nil }

func (p *exactRecipientMailProvider) ArchiveMany(ids []string) ([]mail.ArchiveResult, error) {
	return make([]mail.ArchiveResult, len(ids)), nil
}

func (p *exactRecipientMailProvider) Delete(string) error { return nil }

func (p *exactRecipientMailProvider) DeleteMany(ids []string) ([]mail.ArchiveResult, error) {
	return make([]mail.ArchiveResult, len(ids)), nil
}

func (p *exactRecipientMailProvider) Check(recipient string) ([]mail.Message, error) {
	return p.unread(recipient), nil
}

func (p *exactRecipientMailProvider) Reply(string, string, string, string) (mail.Message, error) {
	return mail.Message{}, mail.ErrNotFound
}

func (p *exactRecipientMailProvider) Thread(string) ([]mail.Message, error) { return nil, nil }

func (p *exactRecipientMailProvider) All(recipient string) ([]mail.Message, error) {
	return append([]mail.Message(nil), p.messages[recipient]...), nil
}

func (p *exactRecipientMailProvider) Count(recipient string) (int, int, error) {
	msgs := p.messages[recipient]
	var unread int
	for _, msg := range msgs {
		if !msg.Read {
			unread++
		}
	}
	return len(msgs), unread, nil
}

func (p *exactRecipientMailProvider) unread(recipient string) []mail.Message {
	var out []mail.Message
	for _, msg := range p.messages[recipient] {
		if !msg.Read {
			out = append(out, msg)
		}
	}
	return out
}

type blockingMailProvider struct {
	exactRecipientMailProvider
	release <-chan struct{}
}

func (p *blockingMailProvider) Inbox(string) ([]mail.Message, error) {
	<-p.release
	return nil, nil
}

func (p *blockingMailProvider) Get(string) (mail.Message, error) {
	<-p.release
	return mail.Message{}, nil
}

func (p *blockingMailProvider) Count(string) (int, int, error) {
	<-p.release
	return 0, 0, nil
}

func (p *blockingMailProvider) All(string) ([]mail.Message, error) {
	<-p.release
	return nil, nil
}

func (p *blockingMailProvider) Thread(string) ([]mail.Message, error) {
	<-p.release
	return nil, nil
}

type panicMailProvider struct {
	exactRecipientMailProvider
}

func (p *panicMailProvider) Inbox(string) ([]mail.Message, error) {
	panic("mail inbox exploded")
}

func (p *panicMailProvider) Count(string) (int, int, error) {
	panic("mail count exploded")
}

func (p *panicMailProvider) All(string) ([]mail.Message, error) {
	panic("mail all exploded")
}

func (p *panicMailProvider) Get(string) (mail.Message, error) {
	panic("mail get exploded")
}

func (p *panicMailProvider) Thread(string) ([]mail.Message, error) {
	panic("mail thread exploded")
}

type threadMailProvider struct {
	exactRecipientMailProvider
	messages []mail.Message
}

func (p *threadMailProvider) Thread(string) ([]mail.Message, error) {
	return append([]mail.Message(nil), p.messages...), nil
}

type multiRecipientInboxMailProvider struct {
	exactRecipientMailProvider
	calls [][]string
}

func (p *multiRecipientInboxMailProvider) Inbox(recipient string) ([]mail.Message, error) {
	return nil, fmt.Errorf("fallback Inbox(%q) should not be called", recipient)
}

func (p *multiRecipientInboxMailProvider) InboxRecipients(recipients []string) ([]mail.Message, error) {
	p.calls = append(p.calls, append([]string(nil), recipients...))
	return []mail.Message{{ID: "multi-1", To: strings.Join(recipients, ",")}}, nil
}

type multiProviderFakeState struct {
	*fakeState
	providers map[string]mail.Provider
}

func (f *multiProviderFakeState) MailProvider(rig string) mail.Provider {
	return f.providers[rig]
}

func (f *multiProviderFakeState) MailProviders() map[string]mail.Provider {
	return f.providers
}

func TestMailInboxForRecipientsUsesMultiRecipientProvider(t *testing.T) {
	mp := &multiRecipientInboxMailProvider{}

	msgs, err := mailInboxForRecipients(mp, []string{"sky", "mayor", "sky"})
	if err != nil {
		t.Fatalf("mailInboxForRecipients: %v", err)
	}
	if len(msgs) != 1 || msgs[0].ID != "multi-1" {
		t.Fatalf("messages = %#v, want multi-recipient result", msgs)
	}
	if len(mp.calls) != 1 {
		t.Fatalf("InboxRecipients calls = %d, want 1", len(mp.calls))
	}
	want := []string{"sky", "mayor"}
	if fmt.Sprint(mp.calls[0]) != fmt.Sprint(want) {
		t.Fatalf("InboxRecipients recipients = %#v, want %#v", mp.calls[0], want)
	}
}

func TestMailInboxForRecipientsFallbackDedupesMessages(t *testing.T) {
	mp := &exactRecipientMailProvider{messages: map[string][]mail.Message{
		"sky":   {{ID: "msg-1", To: "sky"}, {ID: "shared", To: "sky"}},
		"mayor": {{ID: "shared", To: "mayor"}, {ID: "msg-2", To: "mayor"}},
	}}

	msgs, err := mailInboxForRecipients(mp, []string{"sky", "mayor", "sky"})
	if err != nil {
		t.Fatalf("mailInboxForRecipients: %v", err)
	}
	if len(msgs) != 3 {
		t.Fatalf("messages = %#v, want 3 deduped fallback messages", msgs)
	}
}

func TestUniqueMailRecipientsDoesNotMutateInput(t *testing.T) {
	recipients := []string{"sky", "sky", "mayor"}
	original := append([]string(nil), recipients...)

	got := uniqueMailRecipients(recipients)

	if !reflect.DeepEqual(got, []string{"sky", "mayor"}) {
		t.Fatalf("uniqueMailRecipients = %#v, want sky/mayor", got)
	}
	if !reflect.DeepEqual(recipients, original) {
		t.Fatalf("input mutated to %#v, want %#v", recipients, original)
	}
}

func TestUniqueNonEmptyMailRecipientsDoesNotMutateInput(t *testing.T) {
	recipients := []string{" sky ", "", "mayor"}
	original := append([]string(nil), recipients...)

	got := uniqueNonEmptyMailRecipients(recipients)

	if !reflect.DeepEqual(got, []string{"sky", "mayor"}) {
		t.Fatalf("uniqueNonEmptyMailRecipients = %#v, want sky/mayor", got)
	}
	if !reflect.DeepEqual(recipients, original) {
		t.Fatalf("input mutated to %#v, want %#v", recipients, original)
	}
}

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

func TestMailListRigStoreSlowReturnsTyped503(t *testing.T) {
	state := newFakeState(t)
	release := make(chan struct{})
	state.cityMailProv = &blockingMailProvider{release: release}
	oldDeadline := mailReadDeadline
	mailReadDeadline = 5 * time.Millisecond
	t.Cleanup(func() {
		mailReadDeadline = oldDeadline
		close(release)
	})
	h := newTestCityHandler(t, state)

	req := httptest.NewRequest("GET", cityURL(state, "/mail?agent=worker&rig=myrig"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assertStoreSlowProblem(t, rec)
}

func TestMailCountRigStoreSlowReturnsTyped503(t *testing.T) {
	state := newFakeState(t)
	release := make(chan struct{})
	state.cityMailProv = &blockingMailProvider{release: release}
	oldDeadline := mailReadDeadline
	mailReadDeadline = 5 * time.Millisecond
	t.Cleanup(func() {
		mailReadDeadline = oldDeadline
		close(release)
	})
	h := newTestCityHandler(t, state)

	req := httptest.NewRequest("GET", cityURL(state, "/mail/count?agent=worker&rig=myrig"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assertStoreSlowProblem(t, rec)
}

func TestMailGetRigStoreSlowReturnsTyped503(t *testing.T) {
	state := newFakeState(t)
	release := make(chan struct{})
	state.cityMailProv = &blockingMailProvider{release: release}
	oldDeadline := mailReadDeadline
	mailReadDeadline = 5 * time.Millisecond
	t.Cleanup(func() {
		mailReadDeadline = oldDeadline
		close(release)
	})
	h := newTestCityHandler(t, state)

	req := httptest.NewRequest("GET", cityURL(state, "/mail/msg-1?rig=myrig"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assertStoreSlowProblem(t, rec)
}

func TestMailListRigProviderPanicReturns500(t *testing.T) {
	state := newFakeState(t)
	state.cityMailProv = &panicMailProvider{}
	h := newTestCityHandler(t, state)

	req := httptest.NewRequest("GET", cityURL(state, "/mail?agent=worker&rig=myrig"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusInternalServerError, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "mail provider read panicked") {
		t.Fatalf("body missing recovered panic detail: %s", rec.Body.String())
	}
}

func TestMailListAllRigsProviderPanicReturnsPartial(t *testing.T) {
	state := &multiProviderFakeState{
		fakeState: newFakeState(t),
		providers: map[string]mail.Provider{
			"fast": &exactRecipientMailProvider{messages: map[string][]mail.Message{
				"worker": {{ID: "fast-1", To: "worker", Subject: "ready"}},
			}},
			"panic": &panicMailProvider{},
		},
	}
	h := newTestCityHandler(t, state)

	req := httptest.NewRequest("GET", cityURL(state, "/mail?agent=worker"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var body struct {
		Items         []mail.Message `json:"items"`
		Partial       bool           `json:"partial"`
		PartialErrors []string       `json:"partial_errors"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v; body: %s", err, rec.Body.String())
	}
	if len(body.Items) != 1 || body.Items[0].Rig != "fast" {
		t.Fatalf("items = %+v, want one fast-rig message", body.Items)
	}
	if !body.Partial {
		t.Fatalf("partial = false, want true; errors = %v", body.PartialErrors)
	}
	if len(body.PartialErrors) != 1 || !strings.Contains(body.PartialErrors[0], "mail provider read panicked") {
		t.Fatalf("partial_errors = %v, want recovered panic detail", body.PartialErrors)
	}
}

func TestMailListAllRigsStoreSlowReturnsPartial(t *testing.T) {
	release := make(chan struct{})
	state := &multiProviderFakeState{
		fakeState: newFakeState(t),
		providers: map[string]mail.Provider{
			"fast": &exactRecipientMailProvider{messages: map[string][]mail.Message{
				"worker": {{ID: "fast-1", To: "worker", Subject: "ready"}},
			}},
			"slow": &blockingMailProvider{release: release},
		},
	}
	oldDeadline := mailReadDeadline
	mailReadDeadline = mailPartialReadTestDeadline
	t.Cleanup(func() {
		mailReadDeadline = oldDeadline
		close(release)
	})
	h := newTestCityHandler(t, state)

	req := httptest.NewRequest("GET", cityURL(state, "/mail?agent=worker"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assertPartialMailListStoreSlow(t, rec)
}

func TestMailListAllStatusStoreSlowReturnsPartial(t *testing.T) {
	release := make(chan struct{})
	state := &multiProviderFakeState{
		fakeState: newFakeState(t),
		providers: map[string]mail.Provider{
			"fast": &exactRecipientMailProvider{messages: map[string][]mail.Message{
				"worker": {{ID: "fast-1", To: "worker", Subject: "ready", Read: true}},
			}},
			"slow": &blockingMailProvider{release: release},
		},
	}
	oldDeadline := mailReadDeadline
	mailReadDeadline = mailPartialReadTestDeadline
	t.Cleanup(func() {
		mailReadDeadline = oldDeadline
		close(release)
	})
	h := newTestCityHandler(t, state)

	req := httptest.NewRequest("GET", cityURL(state, "/mail?agent=worker&status=all"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assertPartialMailListStoreSlow(t, rec)
}

func TestMailCountAllRigsStoreSlowReturnsPartial(t *testing.T) {
	release := make(chan struct{})
	state := &multiProviderFakeState{
		fakeState: newFakeState(t),
		providers: map[string]mail.Provider{
			"fast": &exactRecipientMailProvider{messages: map[string][]mail.Message{
				"worker": {{ID: "fast-1", To: "worker", Subject: "ready"}},
			}},
			"slow": &blockingMailProvider{release: release},
		},
	}
	oldDeadline := mailReadDeadline
	mailReadDeadline = mailPartialReadTestDeadline
	t.Cleanup(func() {
		mailReadDeadline = oldDeadline
		close(release)
	})
	h := newTestCityHandler(t, state)

	req := httptest.NewRequest("GET", cityURL(state, "/mail/count?agent=worker"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var body struct {
		Total         int      `json:"total"`
		Unread        int      `json:"unread"`
		Partial       bool     `json:"partial"`
		PartialErrors []string `json:"partial_errors"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v; body: %s", err, rec.Body.String())
	}
	if body.Total != 1 || body.Unread != 1 {
		t.Fatalf("counts = total:%d unread:%d, want total:1 unread:1", body.Total, body.Unread)
	}
	assertPartialStoreSlow(t, body.Partial, body.PartialErrors)
}

func TestMailListAllRigsStoreSlowAllFailedReturnsTyped503(t *testing.T) {
	releaseA := make(chan struct{})
	releaseB := make(chan struct{})
	state := &multiProviderFakeState{
		fakeState: newFakeState(t),
		providers: map[string]mail.Provider{
			"slow-a": &blockingMailProvider{release: releaseA},
			"slow-b": &blockingMailProvider{release: releaseB},
		},
	}
	oldDeadline := mailReadDeadline
	mailReadDeadline = 5 * time.Millisecond
	t.Cleanup(func() {
		mailReadDeadline = oldDeadline
		close(releaseA)
		close(releaseB)
	})
	h := newTestCityHandler(t, state)

	req := httptest.NewRequest("GET", cityURL(state, "/mail?agent=worker"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assertStoreSlowProblem(t, rec)
}

func TestMailListAllStatusStoreSlowAllFailedReturnsTyped503(t *testing.T) {
	releaseA := make(chan struct{})
	releaseB := make(chan struct{})
	state := &multiProviderFakeState{
		fakeState: newFakeState(t),
		providers: map[string]mail.Provider{
			"slow-a": &blockingMailProvider{release: releaseA},
			"slow-b": &blockingMailProvider{release: releaseB},
		},
	}
	oldDeadline := mailReadDeadline
	mailReadDeadline = 5 * time.Millisecond
	t.Cleanup(func() {
		mailReadDeadline = oldDeadline
		close(releaseA)
		close(releaseB)
	})
	h := newTestCityHandler(t, state)

	req := httptest.NewRequest("GET", cityURL(state, "/mail?agent=worker&status=all"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assertStoreSlowProblem(t, rec)
}

func TestMailCountAllRigsStoreSlowAllFailedReturnsTyped503(t *testing.T) {
	releaseA := make(chan struct{})
	releaseB := make(chan struct{})
	state := &multiProviderFakeState{
		fakeState: newFakeState(t),
		providers: map[string]mail.Provider{
			"slow-a": &blockingMailProvider{release: releaseA},
			"slow-b": &blockingMailProvider{release: releaseB},
		},
	}
	oldDeadline := mailReadDeadline
	mailReadDeadline = 5 * time.Millisecond
	t.Cleanup(func() {
		mailReadDeadline = oldDeadline
		close(releaseA)
		close(releaseB)
	})
	h := newTestCityHandler(t, state)

	req := httptest.NewRequest("GET", cityURL(state, "/mail/count?agent=worker"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assertStoreSlowProblem(t, rec)
}

func TestMailThreadRigStoreSlowReturnsTyped503(t *testing.T) {
	state := newFakeState(t)
	release := make(chan struct{})
	state.cityMailProv = &blockingMailProvider{release: release}
	oldDeadline := mailReadDeadline
	mailReadDeadline = 5 * time.Millisecond
	t.Cleanup(func() {
		mailReadDeadline = oldDeadline
		close(release)
	})
	h := newTestCityHandler(t, state)

	req := httptest.NewRequest("GET", cityURL(state, "/mail/thread/thread-1?rig=myrig"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assertStoreSlowProblem(t, rec)
}

func TestMailThreadAllRigsStoreSlowReturnsPartial(t *testing.T) {
	release := make(chan struct{})
	state := &multiProviderFakeState{
		fakeState: newFakeState(t),
		providers: map[string]mail.Provider{
			"fast": &threadMailProvider{messages: []mail.Message{{ID: "fast-1", ThreadID: "thread-1"}}},
			"slow": &blockingMailProvider{release: release},
		},
	}
	oldDeadline := mailReadDeadline
	mailReadDeadline = mailPartialReadTestDeadline
	t.Cleanup(func() {
		mailReadDeadline = oldDeadline
		close(release)
	})
	h := newTestCityHandler(t, state)

	req := httptest.NewRequest("GET", cityURL(state, "/mail/thread/thread-1"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assertPartialMailListStoreSlow(t, rec)
}

func TestMailThreadAllRigsStoreSlowAllFailedReturnsTyped503(t *testing.T) {
	releaseA := make(chan struct{})
	releaseB := make(chan struct{})
	state := &multiProviderFakeState{
		fakeState: newFakeState(t),
		providers: map[string]mail.Provider{
			"slow-a": &blockingMailProvider{release: releaseA},
			"slow-b": &blockingMailProvider{release: releaseB},
		},
	}
	oldDeadline := mailReadDeadline
	mailReadDeadline = 5 * time.Millisecond
	t.Cleanup(func() {
		mailReadDeadline = oldDeadline
		close(releaseA)
		close(releaseB)
	})
	h := newTestCityHandler(t, state)

	req := httptest.NewRequest("GET", cityURL(state, "/mail/thread/thread-1"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assertStoreSlowProblem(t, rec)
}

func TestClientMailListAllRigsMultipleStoreSlowReturnsTyped503BeforeClientTimeout(t *testing.T) {
	releaseA := make(chan struct{})
	releaseB := make(chan struct{})
	releaseC := make(chan struct{})
	state := &multiProviderFakeState{
		fakeState: newFakeState(t),
		providers: map[string]mail.Provider{
			"slow-a": &blockingMailProvider{release: releaseA},
			"slow-b": &blockingMailProvider{release: releaseB},
			"slow-c": &blockingMailProvider{release: releaseC},
		},
	}
	oldDeadline := mailReadDeadline
	mailReadDeadline = 100 * time.Millisecond
	ts := httptest.NewServer(newTestCityHandler(t, state))
	t.Cleanup(ts.Close)
	t.Cleanup(func() {
		mailReadDeadline = oldDeadline
		close(releaseA)
		close(releaseB)
		close(releaseC)
	})
	c := newTestCityScopedClientWithTimeout(t, ts.URL, state.CityName(), 250*time.Millisecond)

	_, err := c.ListMailInbox("worker", "")
	if err == nil {
		t.Fatal("ListMailInbox succeeded, want typed store_slow error")
	}
	if !IsStoreSlowError(err) {
		t.Fatalf("ListMailInbox error = %v, want typed store_slow before client timeout", err)
	}
	if ShouldFallbackForRead(err) {
		t.Fatalf("ShouldFallbackForRead = true for typed store_slow error: %v", err)
	}
}

func newTestCityScopedClientWithTimeout(t *testing.T, baseURL, cityName string, timeout time.Duration) *Client {
	t.Helper()
	cw, err := genclient.NewClientWithResponses(
		baseURL,
		genclient.WithHTTPClient(&http.Client{Timeout: timeout}),
		genclient.WithRequestEditorFn(func(_ context.Context, req *http.Request) error {
			req.Header.Set("X-GC-Request", "true")
			return nil
		}),
	)
	if err != nil {
		t.Fatalf("NewClientWithResponses: %v", err)
	}
	return &Client{cw: cw, baseURL: baseURL, cityName: cityName}
}

func assertPartialMailListStoreSlow(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var body struct {
		Items         []mail.Message `json:"items"`
		Partial       bool           `json:"partial"`
		PartialErrors []string       `json:"partial_errors"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v; body: %s", err, rec.Body.String())
	}
	if len(body.Items) != 1 || body.Items[0].Rig != "fast" {
		t.Fatalf("items = %+v, want one fast-rig message", body.Items)
	}
	assertPartialStoreSlow(t, body.Partial, body.PartialErrors)
}

func assertPartialStoreSlow(t *testing.T, partial bool, partialErrors []string) {
	t.Helper()
	if !partial {
		t.Fatalf("partial = false, want true; errors = %v", partialErrors)
	}
	if len(partialErrors) == 0 {
		t.Fatal("partial_errors empty, want store_slow entry")
	}
	for _, msg := range partialErrors {
		if strings.Contains(msg, "store_slow:") {
			return
		}
	}
	t.Fatalf("partial_errors = %v, want store_slow entry", partialErrors)
}

func assertStoreSlowProblem(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusServiceUnavailable, rec.Body.String())
	}
	var problem struct {
		Detail string `json:"detail"`
		Status int    `json:"status"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&problem); err != nil {
		t.Fatalf("decode problem: %v; body: %s", err, rec.Body.String())
	}
	if problem.Status != http.StatusServiceUnavailable {
		t.Fatalf("problem status = %d, want %d", problem.Status, http.StatusServiceUnavailable)
	}
	if !strings.HasPrefix(problem.Detail, "store_slow:") {
		t.Fatalf("detail = %q, want store_slow prefix", problem.Detail)
	}
}

func TestMailInboxAndCountResolveSessionMailboxAddresses(t *testing.T) {
	state := newFakeState(t)
	state.cityBeadStore = state.stores["myrig"]
	sessionBead, err := state.cityBeadStore.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"alias":         "sky",
			"alias_history": "mayor,witness",
		},
	})
	if err != nil {
		t.Fatalf("Create session: %v", err)
	}
	for _, msg := range []struct {
		to   string
		body string
	}{
		{to: "sky", body: "current alias"},
		{to: sessionBead.ID, body: "session id"},
		{to: "mayor", body: "historical alias"},
	} {
		if _, err := state.cityMailProv.Send("human", msg.to, "", msg.body); err != nil {
			t.Fatalf("Send(%q): %v", msg.to, err)
		}
	}
	h := newTestCityHandler(t, state)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", cityURL(state, "/mail?agent=sky"), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("inbox status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var inbox struct {
		Items []mail.Message `json:"items"`
		Total int            `json:"total"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&inbox); err != nil {
		t.Fatalf("decode inbox: %v", err)
	}
	if inbox.Total != 3 {
		t.Fatalf("inbox Total = %d, want 3; items=%#v", inbox.Total, inbox.Items)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", cityURL(state, "/mail/count?agent=sky"), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("count status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var count struct {
		Total  int `json:"total"`
		Unread int `json:"unread"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&count); err != nil {
		t.Fatalf("decode count: %v", err)
	}
	if count.Total != 3 || count.Unread != 3 {
		t.Fatalf("count = (%d total, %d unread), want (3 total, 3 unread)", count.Total, count.Unread)
	}
}

func TestMailAPIQueriesAllResolvedSessionMailboxAddresses(t *testing.T) {
	state := newFakeState(t)
	state.cityBeadStore = state.stores["myrig"]
	sessionBead, err := state.cityBeadStore.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"alias":         "sky",
			"alias_history": "mayor,witness",
			"session_name":  "runtime-sky",
		},
	})
	if err != nil {
		t.Fatalf("Create session: %v", err)
	}
	state.cityMailProv = &exactRecipientMailProvider{messages: map[string][]mail.Message{
		"sky":            {{ID: "msg-current", From: "human", To: "sky", Body: "current alias"}},
		sessionBead.ID:   {{ID: "msg-id", From: "human", To: sessionBead.ID, Body: "session id"}},
		"mayor":          {{ID: "msg-history", From: "human", To: "mayor", Body: "historical alias"}},
		"runtime-sky":    {{ID: "msg-runtime", From: "human", To: "runtime-sky", Body: "runtime session name"}},
		"unrelated-user": {{ID: "msg-other", From: "human", To: "unrelated-user", Body: "other"}},
	}}
	h := newTestCityHandler(t, state)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", cityURL(state, "/mail?agent=sky"), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("inbox status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var inbox struct {
		Items []mail.Message `json:"items"`
		Total int            `json:"total"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&inbox); err != nil {
		t.Fatalf("decode inbox: %v", err)
	}
	if inbox.Total != 4 {
		t.Fatalf("inbox Total = %d, want 4 resolved mailbox addresses; items=%#v", inbox.Total, inbox.Items)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", cityURL(state, "/mail/count?agent=sky"), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("count status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var count struct {
		Total  int `json:"total"`
		Unread int `json:"unread"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&count); err != nil {
		t.Fatalf("decode count: %v", err)
	}
	if count.Total != 4 || count.Unread != 4 {
		t.Fatalf("count = (%d total, %d unread), want (4 total, 4 unread)", count.Total, count.Unread)
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

func TestMailDeleteMissingIsIdempotent(t *testing.T) {
	state := newFakeState(t)
	h := newTestCityHandler(t, state)

	req := httptest.NewRequest("DELETE", cityURL(state, "/mail/nonexistent"), nil)
	req.Header.Set("X-GC-Request", "true")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
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
