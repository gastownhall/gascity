package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

type noBroadAPISessionListStore struct {
	beads.Store
	t *testing.T
}

func (s noBroadAPISessionListStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if query.Label == session.LabelSession && len(query.Metadata) == 0 {
		s.t.Fatalf("API mail used broad session label scan: %+v", query)
	}
	return s.Store.List(query)
}

// Phase 0 spec coverage from engdocs/design/session-model-unification.md:
// - Surface matrix / session-targeting API
// - template: scope is factory-targeting only
// - no bare session-facing token can create from config implicitly
// - mail delivery is session-targeting, not config fallback

func TestPhase0APISessionTargetingSurfaces_RejectTemplateFactoryTargets(t *testing.T) {
	tests := []struct {
		name string
		req  func(*fakeState) *http.Request
	}{
		{
			name: "POST /close",
			req: func(fs *fakeState) *http.Request {
				return newPostRequest(cityURL(fs, "/session/template:worker/close"), nil)
			},
		},
		{
			name: "POST /wake",
			req: func(fs *fakeState) *http.Request {
				return newPostRequest(cityURL(fs, "/session/template:worker/wake"), nil)
			},
		},
		{
			name: "POST /suspend",
			req: func(fs *fakeState) *http.Request {
				return newPostRequest(cityURL(fs, "/session/template:worker/suspend"), nil)
			},
		},
		{
			name: "POST /messages",
			req: func(fs *fakeState) *http.Request {
				return newPostRequest(cityURL(fs, "/session/template:worker/messages"), strings.NewReader(`{"message":"hello"}`))
			},
		},
		{
			name: "POST /submit",
			req: func(fs *fakeState) *http.Request {
				return newPostRequest(cityURL(fs, "/session/template:worker/submit"), strings.NewReader(`{"message":"hello"}`))
			},
		},
	}

	asyncOps := map[string]bool{"POST /messages": true, "POST /submit": true}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fs := newPhase0APIOrdinaryWorkerState(t)
			srv := New(fs)
			h := newTestCityHandlerWith(t, fs, srv)

			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, tt.req(fs))

			if asyncOps[tt.name] {
				if rec.Code != http.StatusAccepted {
					t.Fatalf("%s status = %d, want 202; body=%s", tt.name, rec.Code, rec.Body.String())
				}
			} else {
				if rec.Code < 400 {
					t.Fatalf("%s accepted template:worker with status %d; body=%s", tt.name, rec.Code, rec.Body.String())
				}
			}
		})
	}
}

func TestPhase0APISessionTargetingSurfaces_BareConfigNameDoesNotCreateOrdinarySession(t *testing.T) {
	tests := []struct {
		name string
		req  func(*fakeState) *http.Request
	}{
		{
			name: "POST /close",
			req: func(fs *fakeState) *http.Request {
				return newPostRequest(cityURL(fs, "/session/worker/close"), nil)
			},
		},
		{
			name: "POST /wake",
			req: func(fs *fakeState) *http.Request {
				return newPostRequest(cityURL(fs, "/session/worker/wake"), nil)
			},
		},
		{
			name: "POST /suspend",
			req: func(fs *fakeState) *http.Request {
				return newPostRequest(cityURL(fs, "/session/worker/suspend"), nil)
			},
		},
		{
			name: "POST /messages",
			req: func(fs *fakeState) *http.Request {
				return newPostRequest(cityURL(fs, "/session/worker/messages"), strings.NewReader(`{"message":"hello"}`))
			},
		},
		{
			name: "POST /submit",
			req: func(fs *fakeState) *http.Request {
				return newPostRequest(cityURL(fs, "/session/worker/submit"), strings.NewReader(`{"message":"hello"}`))
			},
		},
	}

	asyncOps := map[string]bool{"POST /messages": true, "POST /submit": true}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fs := newPhase0APIOrdinaryWorkerState(t)
			srv := New(fs)
			h := newTestCityHandlerWith(t, fs, srv)

			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, tt.req(fs))

			if asyncOps[tt.name] {
				if rec.Code != http.StatusAccepted {
					t.Fatalf("%s status = %d, want 202; body=%s", tt.name, rec.Code, rec.Body.String())
				}
			} else {
				if rec.Code < 400 {
					t.Fatalf("%s accepted ordinary config name worker with status %d; body=%s", tt.name, rec.Code, rec.Body.String())
				}
			}
		})
	}
}

func TestPhase0APIMailSend_RejectsTemplateFactoryTarget(t *testing.T) {
	fs := newPhase0APIOrdinaryWorkerState(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)

	body := `{"from":"human","to":"template:worker","subject":"hello","body":"test"}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newPostRequest(cityURL(fs, "/mail"), strings.NewReader(body)))

	if rec.Code == http.StatusCreated {
		t.Fatalf("POST /v0/mail accepted template:worker as recipient; body=%s", rec.Body.String())
	}
	msgs, err := fs.cityMailProv.Inbox("template:worker")
	if err != nil {
		t.Fatalf("Inbox(template:worker): %v", err)
	}
	if len(msgs) != 0 {
		t.Fatalf("template:worker recipient got %d message(s), want 0", len(msgs))
	}
}

func TestPhase0APIMailSend_BareConfigNameDoesNotResolveAsRecipient(t *testing.T) {
	fs := newPhase0APIOrdinaryWorkerState(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)

	body := `{"from":"human","to":"worker","subject":"hello","body":"test"}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newPostRequest(cityURL(fs, "/mail"), strings.NewReader(body)))

	if rec.Code == http.StatusCreated {
		t.Fatalf("POST /v0/mail accepted ordinary config name as recipient; body=%s", rec.Body.String())
	}
	msgs, err := fs.cityMailProv.Inbox("worker")
	if err != nil {
		t.Fatalf("Inbox(worker): %v", err)
	}
	if len(msgs) != 0 {
		t.Fatalf("ordinary config recipient got %d message(s), want 0", len(msgs))
	}
}

func TestPhase0APIMailSend_BareNamedSessionUsesConfiguredMailboxWithoutMaterializing(t *testing.T) {
	fs := newPhase0APINamedWorkerState(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)

	body := `{"from":"human","to":"worker","subject":"hello","body":"test"}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newPostRequest(cityURL(fs, "/mail"), strings.NewReader(body)))

	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /v0/mail status = %d, want %d; body=%s", rec.Code, http.StatusCreated, rec.Body.String())
	}
	msgs, err := fs.cityMailProv.Inbox("myrig/worker")
	if err != nil {
		t.Fatalf("Inbox(myrig/worker): %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("configured named mailbox got %d message(s), want 1", len(msgs))
	}
	if got := msgs[0].To; got != "myrig/worker" {
		t.Fatalf("message To = %q, want configured mailbox identity myrig/worker", got)
	}
	if count := phase0APISessionCount(t, fs.cityBeadStore); count != 0 {
		t.Fatalf("POST /v0/mail materialized %d session(s) for configured named recipient", count)
	}
}

func TestPhase0APIMailSend_BareNamedSessionUsesExistingLiveMailboxWithoutMaterializing(t *testing.T) {
	fs := newPhase0APINamedWorkerState(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	live, err := fs.cityBeadStore.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			apiNamedSessionMetadataKey: "true",
			apiNamedSessionIdentityKey: "myrig/worker",
			apiNamedSessionModeKey:     "always",
			"alias":                    "live-worker",
			"session_name":             "s-gc-test-city-worker",
			"state":                    "asleep",
		},
	})
	if err != nil {
		t.Fatalf("create live named session: %v", err)
	}

	body := `{"from":"human","to":"worker","subject":"hello","body":"test"}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newPostRequest(cityURL(fs, "/mail"), strings.NewReader(body)))

	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /v0/mail status = %d, want %d; body=%s", rec.Code, http.StatusCreated, rec.Body.String())
	}
	msgs, err := fs.cityMailProv.Inbox("live-worker")
	if err != nil {
		t.Fatalf("Inbox(live-worker): %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("live named mailbox got %d message(s), want 1", len(msgs))
	}
	if got := msgs[0].To; got != "live-worker" {
		t.Fatalf("message To = %q, want existing live mailbox live-worker", got)
	}
	if count := phase0APISessionCount(t, fs.cityBeadStore); count != 1 {
		t.Fatalf("POST /v0/mail left %d session(s), want only existing live session %s", count, live.ID)
	}
}

func TestPhase0APIMailSend_BareNamedSessionUsesTargetedLiveMailboxLookup(t *testing.T) {
	fs := newPhase0APINamedWorkerState(t)
	baseStore := fs.cityBeadStore
	if _, err := baseStore.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			apiNamedSessionMetadataKey: "true",
			apiNamedSessionIdentityKey: "myrig/worker",
			apiNamedSessionModeKey:     "always",
			"alias":                    "live-worker",
			"session_name":             "s-gc-test-city-worker",
			"state":                    "asleep",
		},
	}); err != nil {
		t.Fatalf("create live named session: %v", err)
	}
	fs.cityBeadStore = noBroadAPISessionListStore{Store: baseStore, t: t}
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)

	body := `{"from":"human","to":"worker","subject":"hello","body":"test"}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newPostRequest(cityURL(fs, "/mail"), strings.NewReader(body)))

	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /v0/mail status = %d, want %d; body=%s", rec.Code, http.StatusCreated, rec.Body.String())
	}
	msgs, err := fs.cityMailProv.Inbox("live-worker")
	if err != nil {
		t.Fatalf("Inbox(live-worker): %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("live named mailbox got %d message(s), want 1", len(msgs))
	}
}

func TestPhase0APIMailQuery_BareNamedSessionUsesTargetedRecipientLookup(t *testing.T) {
	fs := newPhase0APINamedWorkerState(t)
	baseStore := fs.cityBeadStore
	live, err := baseStore.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			apiNamedSessionMetadataKey: "true",
			apiNamedSessionIdentityKey: "myrig/worker",
			apiNamedSessionModeKey:     "always",
			"alias":                    "live-worker",
			"session_name":             "s-gc-test-city-worker",
			"state":                    "asleep",
		},
	})
	if err != nil {
		t.Fatalf("create live named session: %v", err)
	}
	closed, err := baseStore.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			apiNamedSessionMetadataKey: "true",
			apiNamedSessionIdentityKey: "myrig/worker",
			apiNamedSessionModeKey:     "always",
			"session_name":             "s-gc-test-city-worker-old",
			"state":                    "closed",
		},
	})
	if err != nil {
		t.Fatalf("create closed named session: %v", err)
	}
	if err := baseStore.Close(closed.ID); err != nil {
		t.Fatalf("close named session: %v", err)
	}
	fs.cityBeadStore = noBroadAPISessionListStore{Store: baseStore, t: t}
	srv := New(fs)

	recipients := srv.resolveMailQueryRecipientsWithContext(t.Context(), "worker")
	want := []string{live.ID, closed.ID, "live-worker", "myrig/worker", "s-gc-test-city-worker", "s-gc-test-city-worker-old", "worker"}
	if strings.Join(recipients, ",") != strings.Join(want, ",") {
		t.Fatalf("recipients = %#v, want %#v", recipients, want)
	}
}

func TestPhase0APIMailQuery_MaterializedNamedSessionKeepsConfiguredMailbox(t *testing.T) {
	fs := newPhase0APINamedWorkerState(t)
	baseStore := fs.cityBeadStore
	for _, msg := range []struct {
		to   string
		body string
	}{
		{to: "myrig/worker", body: "configured mailbox before materialization"},
		{to: "worker", body: "raw compatibility mailbox"},
		{to: "unrelated", body: "must not leak"},
	} {
		if _, err := fs.cityMailProv.Send("human", msg.to, "test", msg.body); err != nil {
			t.Fatalf("Send(%q): %v", msg.to, err)
		}
	}
	if _, err := baseStore.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			apiNamedSessionMetadataKey: "true",
			apiNamedSessionIdentityKey: "myrig/worker",
			apiNamedSessionModeKey:     "always",
			"alias":                    "live-worker",
			"session_name":             "s-gc-test-city-worker",
			"state":                    "asleep",
		},
	}); err != nil {
		t.Fatalf("create live named session: %v", err)
	}
	fs.cityBeadStore = noBroadAPISessionListStore{Store: baseStore, t: t}
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", cityURL(fs, "/mail?agent=worker"), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("inbox status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var inbox struct {
		Items []struct {
			To   string `json:"to"`
			Body string `json:"body"`
		} `json:"items"`
		Total int `json:"total"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&inbox); err != nil {
		t.Fatalf("decode inbox: %v", err)
	}
	if inbox.Total != 2 {
		t.Fatalf("inbox Total = %d, want 2; items=%#v", inbox.Total, inbox.Items)
	}
	seen := map[string]bool{}
	for _, item := range inbox.Items {
		if item.To == "unrelated" || item.Body == "must not leak" {
			t.Fatalf("inbox leaked unrelated recipient message: %#v", inbox.Items)
		}
		seen[item.To] = true
	}
	for _, to := range []string{"myrig/worker", "worker"} {
		if !seen[to] {
			t.Fatalf("inbox missing message for %q: %#v", to, inbox.Items)
		}
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", cityURL(fs, "/mail/count?agent=worker"), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("count status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var count struct {
		Total  int `json:"total"`
		Unread int `json:"unread"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&count); err != nil {
		t.Fatalf("decode count: %v", err)
	}
	if count.Total != inbox.Total || count.Unread != inbox.Total {
		t.Fatalf("count = (%d total, %d unread), want (%d total, %d unread)", count.Total, count.Unread, inbox.Total, inbox.Total)
	}
}

func TestPhase0APIMailQuery_UnmaterializedNamedSessionUsesConfiguredMailboxOnly(t *testing.T) {
	fs := newPhase0APINamedWorkerState(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)

	for _, msg := range []struct {
		to   string
		body string
	}{
		{to: "myrig/worker", body: "configured mailbox"},
		{to: "worker", body: "raw compatibility mailbox"},
		{to: "unrelated", body: "must not leak"},
	} {
		if _, err := fs.cityMailProv.Send("human", msg.to, "test", msg.body); err != nil {
			t.Fatalf("Send(%q): %v", msg.to, err)
		}
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", cityURL(fs, "/mail?agent=worker"), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("inbox status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var inbox struct {
		Items []struct {
			To   string `json:"to"`
			Body string `json:"body"`
		} `json:"items"`
		Total int `json:"total"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&inbox); err != nil {
		t.Fatalf("decode inbox: %v", err)
	}
	if inbox.Total != 2 {
		t.Fatalf("inbox Total = %d, want 2; items=%#v", inbox.Total, inbox.Items)
	}
	for _, item := range inbox.Items {
		if item.To == "unrelated" || item.Body == "must not leak" {
			t.Fatalf("inbox leaked unrelated recipient message: %#v", inbox.Items)
		}
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", cityURL(fs, "/mail/count?agent=worker"), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("count status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var count struct {
		Total  int `json:"total"`
		Unread int `json:"unread"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&count); err != nil {
		t.Fatalf("decode count: %v", err)
	}
	if count.Total != inbox.Total || count.Unread != inbox.Total {
		t.Fatalf("count = (%d total, %d unread), want (%d total, %d unread)", count.Total, count.Unread, inbox.Total, inbox.Total)
	}
}

func TestPhase0APIResolver_BareConfigNameDoesNotMaterializeOrdinarySession(t *testing.T) {
	fs := newPhase0APIOrdinaryWorkerState(t)
	srv := New(fs)

	id, err := srv.resolveSessionIDMaterializingNamed(fs.cityBeadStore, "worker")
	if err == nil {
		t.Fatalf("resolveSessionIDMaterializingNamed(worker) = %q, want error", id)
	}
	if count := phase0APISessionCount(t, fs.cityBeadStore); count != 0 {
		t.Fatalf("resolver materialized %d session(s) for ordinary config worker", count)
	}
}

func newPhase0APIOrdinaryWorkerState(t *testing.T) *fakeState {
	t.Helper()
	fs := newSessionFakeState(t)
	fs.cfg = &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "worker",
			Provider:          "test-agent",
			MaxActiveSessions: intPtr(1),
		}},
		Providers: map[string]config.ProviderSpec{
			"test-agent": {DisplayName: "Test Agent"},
		},
	}
	return fs
}

func newPhase0APINamedWorkerState(t *testing.T) *fakeState {
	t.Helper()
	fs := newSessionFakeState(t)
	fs.cfg = &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "worker",
			Dir:               "myrig",
			Provider:          "test-agent",
			MaxActiveSessions: intPtr(1),
		}},
		NamedSessions: []config.NamedSession{{
			Template: "worker",
			Dir:      "myrig",
		}},
		Rigs: []config.Rig{{
			Name: "myrig",
			Path: "/tmp/myrig",
		}},
		Providers: map[string]config.ProviderSpec{
			"test-agent": {DisplayName: "Test Agent"},
		},
	}
	return fs
}

func phase0APISessionCount(t *testing.T, store beads.Store) int {
	t.Helper()
	all, err := store.ListByLabel(session.LabelSession, 0)
	if err != nil {
		t.Fatalf("ListByLabel(session): %v", err)
	}
	return len(all)
}
