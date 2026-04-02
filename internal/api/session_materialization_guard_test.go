package api

import (
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

func TestResolveSessionIDMaterializingNamed_DoesNotMaterializeMissingMultiSessionTemplate(t *testing.T) {
	fs := newSessionFakeState(t)
	fs.cfg = &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "claude",
			Dir:               "gascity",
			Provider:          "test-agent",
			MaxActiveSessions: intPtr(5),
		}},
		Providers: map[string]config.ProviderSpec{
			"test-agent": {DisplayName: "Test Agent"},
		},
	}
	srv := New(fs)

	_, err := srv.resolveSessionIDMaterializingNamed(fs.cityBeadStore, "gascity/claude")
	if !errors.Is(err, session.ErrSessionNotFound) {
		t.Fatalf("resolveSessionIDMaterializingNamed(gascity/claude) error = %v, want ErrSessionNotFound", err)
	}

	all, err := fs.cityBeadStore.ListByLabel(session.LabelSession, 0)
	if err != nil {
		t.Fatalf("ListByLabel: %v", err)
	}
	if len(all) != 0 {
		t.Fatalf("session count = %d, want 0", len(all))
	}
}
