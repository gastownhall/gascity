package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

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
