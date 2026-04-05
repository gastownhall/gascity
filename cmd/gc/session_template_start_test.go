package main

import (
	"io"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

func TestEnsureSessionForTemplate_CreatesFreshSessionForTemplateFallback(t *testing.T) {
	t.Setenv("GC_SESSION", "fake")

	store := beads.NewMemStore()
	_, _ = store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"template":     "mayor",
			"session_name": "s-gc-old",
			"alias":        "old-chat",
		},
	})

	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:         "mayor",
			StartCommand: "true",
		}},
	}

	sessionName, err := ensureSessionForTemplate(t.TempDir(), cfg, store, "mayor", io.Discard)
	if err != nil {
		t.Fatalf("ensureSessionForTemplate(mayor): %v", err)
	}
	if sessionName == "s-gc-old" {
		t.Fatalf("ensureSessionForTemplate reused existing ordinary chat %q; want fresh session", sessionName)
	}

	all, err := store.ListByLabel(session.LabelSession, 0)
	if err != nil {
		t.Fatalf("ListByLabel: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("session bead count = %d, want 2", len(all))
	}
}

func TestEnsureSessionForTemplate_ReopensClosedNamedSessionWithCleanMetadata(t *testing.T) {
	t.Setenv("GC_SESSION", "fake")

	store := beads.NewMemStore()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:         "mayor",
			StartCommand: "true",
		}},
		NamedSessions: []config.NamedSession{{
			Template: "mayor",
		}},
	}
	sessionName := config.NamedSessionRuntimeName(cfg.EffectiveCityName(), cfg.Workspace, "mayor")
	bead, err := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name":               sessionName,
			"alias":                      "mayor",
			"close_reason":               "suspended",
			"closed_at":                  "2026-04-04T10:00:00Z",
			"pending_create_claim":       "true",
			namedSessionMetadataKey:      "true",
			namedSessionIdentityMetadata: "mayor",
			namedSessionModeMetadata:     "on_demand",
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.Close(bead.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}

	gotName, err := ensureSessionForTemplate(t.TempDir(), cfg, store, "mayor", io.Discard)
	if err != nil {
		t.Fatalf("ensureSessionForTemplate(mayor): %v", err)
	}
	if gotName != sessionName {
		t.Fatalf("sessionName = %q, want %q", gotName, sessionName)
	}

	reopened, err := store.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", bead.ID, err)
	}
	if reopened.Status != "open" {
		t.Fatalf("status = %q, want open", reopened.Status)
	}
	if reopened.Metadata["close_reason"] != "" {
		t.Fatalf("close_reason = %q, want empty", reopened.Metadata["close_reason"])
	}
	if reopened.Metadata["closed_at"] != "" {
		t.Fatalf("closed_at = %q, want empty", reopened.Metadata["closed_at"])
	}
	if reopened.Metadata["pending_create_claim"] != "" {
		t.Fatalf("pending_create_claim = %q, want empty", reopened.Metadata["pending_create_claim"])
	}
}
