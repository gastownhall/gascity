package main

import (
	"context"
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// Phase 0 spec coverage from engdocs/design/session-model-unification.md:
// - Namespace Model / Session Namespace
// - Historical alias policy
// - Config Namespace
// - Sessions / session_origin canonical writes
// - Phase 0 red matrix and gap analysis

func TestPhase0SessionResolution_NormalSessionTargetingIgnoresHistoricalAlias(t *testing.T) {
	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"alias":         "sky",
			"alias_history": "mayor",
		},
	}); err != nil {
		t.Fatalf("create session bead: %v", err)
	}

	_, err := resolveSessionID(store, "mayor")
	if !errors.Is(err, session.ErrSessionNotFound) {
		t.Fatalf("resolveSessionID(mayor) error = %v, want ErrSessionNotFound", err)
	}
}

func TestPhase0SessionResolution_NormalSessionTargetingIgnoresAgentNameFallback(t *testing.T) {
	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name": "s-gc-worker",
			"agent_name":   "mayor",
			"template":     "reviewer",
		},
	}); err != nil {
		t.Fatalf("create session bead: %v", err)
	}

	_, err := resolveSessionID(store, "mayor")
	if !errors.Is(err, session.ErrSessionNotFound) {
		t.Fatalf("resolveSessionID(mayor) error = %v, want ErrSessionNotFound", err)
	}
}

func TestPhase0SessionResolution_ConfiguredNamedConflictFailsClosed(t *testing.T) {
	store := beads.NewMemStore()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents:    []config.Agent{{Name: "mayor", StartCommand: "true", MaxActiveSessions: intPtr(1)}},
		NamedSessions: []config.NamedSession{{
			Template: "mayor",
		}},
	}
	if _, err := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name": "s-gc-squatter",
			"alias":        "mayor",
		},
	}); err != nil {
		t.Fatalf("create squatter bead: %v", err)
	}

	_, err := resolveSessionIDWithConfig(t.TempDir(), cfg, store, "mayor")
	if !errors.Is(err, errNamedSessionConflict) {
		t.Fatalf("resolveSessionIDWithConfig(mayor) error = %v, want errNamedSessionConflict", err)
	}
}

func TestPhase0SessionResolution_DoesNotImplicitlyMaterializeSingletonConfig(t *testing.T) {
	t.Setenv("GC_SESSION", "phase0")

	store := beads.NewMemStore()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "mayor",
			StartCommand:      "true",
			MaxActiveSessions: intPtr(1),
		}},
	}

	_, err := resolveSessionIDMaterializingNamed(t.TempDir(), cfg, store, "mayor")
	if !errors.Is(err, session.ErrSessionNotFound) {
		t.Fatalf("resolveSessionIDMaterializingNamed(mayor) error = %v, want ErrSessionNotFound", err)
	}

	all, err := store.ListByLabel(session.LabelSession, 0)
	if err != nil {
		t.Fatalf("ListByLabel: %v", err)
	}
	if len(all) != 0 {
		t.Fatalf("session count = %d, want 0", len(all))
	}
}

func TestPhase0SessionResolution_RigScopedBareNamedIdentityRequiresAmbientRig(t *testing.T) {
	t.Setenv("GC_SESSION", "phase0")
	t.Setenv("GC_DIR", t.TempDir())

	store := beads.NewMemStore()
	rigPath := t.TempDir()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:         "witness",
			Dir:          "demo",
			StartCommand: "true",
		}},
		NamedSessions: []config.NamedSession{{
			Template: "witness",
			Dir:      "demo",
		}},
		Rigs: []config.Rig{{
			Name: "demo",
			Path: rigPath,
		}},
	}

	_, err := resolveSessionIDMaterializingNamed(t.TempDir(), cfg, store, "witness")
	if !errors.Is(err, session.ErrSessionNotFound) {
		t.Fatalf("resolveSessionIDMaterializingNamed(witness) error = %v, want ErrSessionNotFound without ambient rig", err)
	}

	all, err := store.ListByLabel(session.LabelSession, 0)
	if err != nil {
		t.Fatalf("ListByLabel: %v", err)
	}
	if len(all) != 0 {
		t.Fatalf("session count = %d, want 0", len(all))
	}
}

func TestPhase0CanonicalMetadata_ManualCreateWritesSessionOrigin(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := session.NewManager(store, sp)

	info, err := mgr.Create(context.Background(), "worker", "Worker", "echo test", t.TempDir(), "test-provider", nil, session.ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	bead, err := store.Get(info.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", info.ID, err)
	}
	if got := bead.Metadata["session_origin"]; got != "manual" {
		t.Fatalf("session_origin = %q, want manual", got)
	}
	if got := bead.Metadata["manual_session"]; got != "" {
		t.Fatalf("manual_session = %q, want empty", got)
	}
}

func TestPhase0CanonicalMetadata_NamedMaterializationWritesNamedOriginWithoutLegacyManualFlag(t *testing.T) {
	t.Setenv("GC_SESSION", "phase0")

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

	id, err := resolveSessionIDMaterializingNamed(t.TempDir(), cfg, store, "mayor")
	if err != nil {
		t.Fatalf("resolveSessionIDMaterializingNamed(mayor): %v", err)
	}

	bead, err := store.Get(id)
	if err != nil {
		t.Fatalf("Get(%s): %v", id, err)
	}
	if got := bead.Metadata["session_origin"]; got != "named" {
		t.Fatalf("session_origin = %q, want named", got)
	}
	if got := bead.Metadata["manual_session"]; got != "" {
		t.Fatalf("manual_session = %q, want empty", got)
	}
}

func TestPhase0CanonicalMetadata_TemplateFactoryMaterializationWritesEphemeralOriginWithoutLegacyPoolFlags(t *testing.T) {
	t.Setenv("GC_SESSION", "phase0")

	store := beads.NewMemStore()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "worker",
			StartCommand:      "true",
			MinActiveSessions: intPtr(0),
			MaxActiveSessions: intPtr(3),
		}},
	}

	id, err := ensureSessionIDForTemplateWithOptions(t.TempDir(), cfg, store, "worker", nil, ensureSessionForTemplateOptions{forceFresh: true})
	if err != nil {
		t.Fatalf("ensureSessionIDForTemplateWithOptions(worker): %v", err)
	}

	bead, err := store.Get(id)
	if err != nil {
		t.Fatalf("Get(%s): %v", id, err)
	}
	if got := bead.Metadata["session_origin"]; got != "manual" {
		t.Fatalf("session_origin = %q, want manual", got)
	}
	if got := bead.Metadata["manual_session"]; got != "" {
		t.Fatalf("manual_session = %q, want empty", got)
	}
	if got := bead.Metadata["pool_managed"]; got != "" {
		t.Fatalf("pool_managed = %q, want empty", got)
	}
	if got := bead.Metadata["pool_slot"]; got != "" {
		t.Fatalf("pool_slot = %q, want empty", got)
	}
}
