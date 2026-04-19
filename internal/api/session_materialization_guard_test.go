package api

import (
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
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

func TestResolveSessionIDWithConfig_RejectsOrphanedNamedSessionBead(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)

	b, err := fs.cityBeadStore.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name":              "test-city--worker",
			"alias":                     "myrig/worker",
			"configured_named_session":  "true",
			"configured_named_identity": "myrig/worker",
			"configured_named_mode":     "on_demand",
			"continuity_eligible":       "true",
			"state":                     "active",
			"template":                  "myrig/worker",
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	fs.cfg.NamedSessions = nil

	if id, err := session.ResolveSessionID(fs.cityBeadStore, "myrig/worker"); err != nil || id != b.ID {
		t.Fatalf("session.ResolveSessionID = %q, %v, want %q and nil", id, err, b.ID)
	}
	_, err = srv.resolveSessionIDWithConfig(fs.cityBeadStore, "myrig/worker")
	if !errors.Is(err, session.ErrSessionNotFound) {
		t.Fatalf("resolveSessionIDWithConfig(myrig/worker) = %v, want ErrSessionNotFound", err)
	}
}
