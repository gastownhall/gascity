package main

import (
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

func TestResolveSessionIDMaterializingNamed_DoesNotMaterializeMissingMultiSessionTemplate(t *testing.T) {
	store := beads.NewMemStore()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "claude",
			Dir:               "gascity",
			StartCommand:      "true",
			MaxActiveSessions: intPtr(5),
		}},
	}

	_, err := resolveSessionIDMaterializingNamed(t.TempDir(), cfg, store, "gascity/claude")
	if !errors.Is(err, session.ErrSessionNotFound) {
		t.Fatalf("resolveSessionIDMaterializingNamed(gascity/claude) error = %v, want ErrSessionNotFound", err)
	}

	all, err := store.ListByLabel(session.LabelSession, 0)
	if err != nil {
		t.Fatalf("ListByLabel: %v", err)
	}
	if len(all) != 0 {
		t.Fatalf("session count = %d, want 0", len(all))
	}
}

func TestSessionWithinDesiredConfig_ManualPoolSessionIsNotConfigEligible(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "pooled", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(3)},
		},
	}

	agent, eligible := sessionWithinDesiredConfig(makeBead("b1", map[string]string{
		"template":       "pooled",
		"session_name":   "manual-pooled",
		"manual_session": "true",
	}), cfg, map[string]int{"pooled": 2})

	if agent == nil {
		t.Fatal("sessionWithinDesiredConfig returned nil agent")
	}
	// Manual sessions on multi-session (implicit) agents are config-eligible
	// so they get WakeConfig and survive the reconciler.
	if !eligible {
		t.Fatal("manual pool session should be config-eligible")
	}
}
