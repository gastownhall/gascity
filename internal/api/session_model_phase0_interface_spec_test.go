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
		req  func() *http.Request
	}{
		{
			name: "POST /close",
			req: func() *http.Request {
				return newPostRequest("/v0/session/template:worker/close", nil)
			},
		},
		{
			name: "POST /wake",
			req: func() *http.Request {
				return newPostRequest("/v0/session/template:worker/wake", nil)
			},
		},
		{
			name: "POST /suspend",
			req: func() *http.Request {
				return newPostRequest("/v0/session/template:worker/suspend", nil)
			},
		},
		{
			name: "POST /messages",
			req: func() *http.Request {
				return newPostRequest("/v0/session/template:worker/messages", strings.NewReader(`{"message":"hello"}`))
			},
		},
		{
			name: "POST /submit",
			req: func() *http.Request {
				return newPostRequest("/v0/session/template:worker/submit", strings.NewReader(`{"message":"hello"}`))
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fs := newPhase0APIOrdinaryWorkerState(t)
			srv := New(fs)

			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, tt.req())

			if rec.Code < 400 {
				t.Fatalf("%s accepted template:worker with status %d; body=%s", tt.name, rec.Code, rec.Body.String())
			}
			if count := phase0APISessionCount(t, fs.cityBeadStore); count != 0 {
				t.Fatalf("%s materialized %d session(s) for template:worker; body=%s", tt.name, count, rec.Body.String())
			}
		})
	}
}

func TestPhase0APISessionTargetingSurfaces_BareConfigNameDoesNotCreateOrdinarySession(t *testing.T) {
	tests := []struct {
		name string
		req  func() *http.Request
	}{
		{
			name: "POST /close",
			req: func() *http.Request {
				return newPostRequest("/v0/session/worker/close", nil)
			},
		},
		{
			name: "POST /wake",
			req: func() *http.Request {
				return newPostRequest("/v0/session/worker/wake", nil)
			},
		},
		{
			name: "POST /suspend",
			req: func() *http.Request {
				return newPostRequest("/v0/session/worker/suspend", nil)
			},
		},
		{
			name: "POST /messages",
			req: func() *http.Request {
				return newPostRequest("/v0/session/worker/messages", strings.NewReader(`{"message":"hello"}`))
			},
		},
		{
			name: "POST /submit",
			req: func() *http.Request {
				return newPostRequest("/v0/session/worker/submit", strings.NewReader(`{"message":"hello"}`))
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fs := newPhase0APIOrdinaryWorkerState(t)
			srv := New(fs)

			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, tt.req())

			if rec.Code < 400 {
				t.Fatalf("%s accepted ordinary config name worker with status %d; body=%s", tt.name, rec.Code, rec.Body.String())
			}
			if count := phase0APISessionCount(t, fs.cityBeadStore); count != 0 {
				t.Fatalf("%s materialized %d session(s) for ordinary config worker; body=%s", tt.name, count, rec.Body.String())
			}
		})
	}
}

func TestPhase0APIMailSend_RejectsTemplateFactoryTarget(t *testing.T) {
	fs := newPhase0APIOrdinaryWorkerState(t)
	srv := New(fs)

	body := `{"from":"human","to":"template:worker","subject":"hello","body":"test"}`
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newPostRequest("/v0/mail", strings.NewReader(body)))

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

	body := `{"from":"human","to":"worker","subject":"hello","body":"test"}`
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newPostRequest("/v0/mail", strings.NewReader(body)))

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

func phase0APISessionCount(t *testing.T, store beads.Store) int {
	t.Helper()
	all, err := store.ListByLabel(session.LabelSession, 0)
	if err != nil {
		t.Fatalf("ListByLabel(session): %v", err)
	}
	return len(all)
}
