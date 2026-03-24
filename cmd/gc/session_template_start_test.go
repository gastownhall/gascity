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
