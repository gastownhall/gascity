package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

func TestResolveSessionID_BeadID(t *testing.T) {
	store := beads.NewMemStore()
	// Create a real session bead so the direct lookup succeeds.
	b, _ := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
	})

	id, err := resolveSessionID(store, b.ID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != b.ID {
		t.Errorf("got %q, want %q", id, b.ID)
	}
}

func TestResolveSessionID_Alias(t *testing.T) {
	store := beads.NewMemStore()
	b, _ := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"alias": "overseer",
		},
	})

	id, err := resolveSessionID(store, "overseer")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != b.ID {
		t.Errorf("got %q, want %q", id, b.ID)
	}
}

func TestResolveSessionID_QualifiedAlias(t *testing.T) {
	store := beads.NewMemStore()
	b, _ := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"alias": "myrig/witness",
		},
	})

	id, err := resolveSessionID(store, "myrig/witness")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != b.ID {
		t.Errorf("got %q, want %q", id, b.ID)
	}
}

func TestResolveSessionID_HistoricalAlias(t *testing.T) {
	store := beads.NewMemStore()
	b, _ := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"alias":         "sky",
			"alias_history": "mayor",
		},
	})

	id, err := resolveSessionID(store, "mayor")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != b.ID {
		t.Errorf("got %q, want %q", id, b.ID)
	}
}

func TestResolveSessionID_DoesNotResolveTemplateName(t *testing.T) {
	store := beads.NewMemStore()
	_, _ = store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession, "template:myrig/worker"},
		Metadata: map[string]string{
			"template": "myrig/worker",
		},
	})

	_, err := resolveSessionID(store, "worker")
	if err == nil {
		t.Fatal("expected template name to stay unresolved")
	}
	if !strings.Contains(err.Error(), "session not found") {
		t.Fatalf("unexpected error for template lookup: %v", err)
	}
}

func TestResolveSessionID_Ambiguous(t *testing.T) {
	store := beads.NewMemStore()
	_, _ = store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"alias": "worker",
		},
	})
	_, _ = store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"alias": "worker",
		},
	})

	_, err := resolveSessionID(store, "worker")
	if err == nil {
		t.Fatal("expected ambiguity error")
	}
	if !strings.Contains(err.Error(), "ambiguous") {
		t.Errorf("error should mention ambiguous, got: %v", err)
	}
}

func TestResolveSessionID_NotFound(t *testing.T) {
	store := beads.NewMemStore()
	_, err := resolveSessionID(store, "nonexistent")
	if err == nil {
		t.Fatal("expected not found error")
	}
	if !strings.Contains(err.Error(), "session not found") {
		t.Errorf("error should mention not found, got: %v", err)
	}
}

func TestResolveSessionID_SkipsClosedBeads(t *testing.T) {
	store := beads.NewMemStore()
	b, _ := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"template": "worker",
		},
	})
	_ = store.Close(b.ID)

	_, err := resolveSessionID(store, "worker")
	if err == nil {
		t.Fatal("expected not found for closed session")
	}
}

func TestResolveSessionIDAllowClosed_ResolvesClosedNamedSession(t *testing.T) {
	store := beads.NewMemStore()
	b, _ := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name": "sky",
		},
	})
	_ = store.Close(b.ID)

	id, err := resolveSessionIDAllowClosed(store, "sky")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != b.ID {
		t.Fatalf("got %q, want %q", id, b.ID)
	}
}

func TestResolveSessionIDAllowClosed_ResolvesClosedHistoricalAlias(t *testing.T) {
	store := beads.NewMemStore()
	b, _ := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"alias":         "sky",
			"alias_history": "mayor",
		},
	})
	_ = store.Close(b.ID)

	id, err := resolveSessionIDAllowClosed(store, "mayor")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != b.ID {
		t.Fatalf("got %q, want %q", id, b.ID)
	}
}

func TestResolveSessionIDWithConfig_ResolvesExistingSessionName(t *testing.T) {
	store := beads.NewMemStore()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents:    []config.Agent{{Name: "mayor", MaxActiveSessions: intPtr(1)}},
		NamedSessions: []config.NamedSession{{
			Template: "mayor",
		}},
	}
	sessionName := config.NamedSessionRuntimeName(cfg.Workspace.Name, cfg.Workspace, "mayor")
	b, _ := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name":              sessionName,
			"configured_named_session":  "true",
			"configured_named_identity": "mayor",
			"configured_named_mode":     "on_demand",
		},
	})

	id, err := resolveSessionIDWithConfig(filepath.Join(t.TempDir(), "city"), cfg, store, sessionName)
	if err != nil {
		t.Fatalf("resolveSessionIDWithConfig(reserved session_name): %v", err)
	}
	if id != b.ID {
		t.Fatalf("got %q, want %q", id, b.ID)
	}
}

func TestResolveSessionIDWithConfig_ResolvesUniqueAliasBasename(t *testing.T) {
	store := beads.NewMemStore()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents:    []config.Agent{{Name: "witness", Dir: "demo"}},
		NamedSessions: []config.NamedSession{{
			Template: "witness",
			Dir:      "demo",
		}},
	}
	b, _ := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"alias":                     "demo/witness",
			"configured_named_session":  "true",
			"configured_named_identity": "demo/witness",
			"configured_named_mode":     "on_demand",
		},
	})

	id, err := resolveSessionIDWithConfig(filepath.Join(t.TempDir(), "city"), cfg, store, "witness")
	if err != nil {
		t.Fatalf("resolveSessionIDWithConfig(unique alias basename): %v", err)
	}
	if id != b.ID {
		t.Fatalf("got %q, want %q", id, b.ID)
	}
}

func TestResolveSessionIDAllowClosedWithConfig_DoesNotResolveClosedReservedAlias(t *testing.T) {
	store := beads.NewMemStore()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents:    []config.Agent{{Name: "mayor", MaxActiveSessions: intPtr(1)}},
		NamedSessions: []config.NamedSession{{
			Template: "mayor",
		}},
	}
	b, _ := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"alias":                     "mayor",
			"configured_named_session":  "true",
			"configured_named_identity": "mayor",
			"configured_named_mode":     "on_demand",
		},
	})
	_ = store.Close(b.ID)

	_, err := resolveSessionIDAllowClosedWithConfig(filepath.Join(t.TempDir(), "city"), cfg, store, "mayor")
	if !errors.Is(err, session.ErrSessionNotFound) {
		t.Fatalf("resolveSessionIDAllowClosedWithConfig(closed alias) error = %v, want session not found", err)
	}
}

func TestResolveSessionIDWithConfig_LiveAliasWinsOverReservedNamedTarget(t *testing.T) {
	store := beads.NewMemStore()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents:    []config.Agent{{Name: "mayor", MaxActiveSessions: intPtr(1)}},
		NamedSessions: []config.NamedSession{{
			Template: "mayor",
		}},
	}
	b, _ := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name": "s-gc-squatter",
			"alias":        "mayor",
		},
	})

	id, err := resolveSessionIDWithConfig(filepath.Join(t.TempDir(), "city"), cfg, store, "mayor")
	if err != nil {
		t.Fatalf("resolveSessionIDWithConfig(mayor): %v", err)
	}
	if id != b.ID {
		t.Fatalf("resolveSessionIDWithConfig(mayor) = %q, want live alias bead %q", id, b.ID)
	}
}

func TestResolveSessionIDMaterializingNamed_QualifiedAliasBasenameDoesNotStealNamedTarget(t *testing.T) {
	t.Setenv("GC_SESSION", "fake")

	store := beads.NewMemStore()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{
			{Name: "mayor", StartCommand: "true"},
			{Name: "mayor", Dir: "ops", StartCommand: "true"},
		},
		NamedSessions: []config.NamedSession{{
			Template: "mayor",
		}},
	}
	ordinary, _ := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name": "s-gc-ops-mayor",
			"alias":        "ops/mayor",
		},
	})

	id, err := resolveSessionIDMaterializingNamed(t.TempDir(), cfg, store, "mayor")
	if err != nil {
		t.Fatalf("resolveSessionIDMaterializingNamed(mayor): %v", err)
	}
	if id == ordinary.ID {
		t.Fatalf("resolveSessionIDMaterializingNamed(mayor) returned qualified alias basename match %q; want canonical named session", id)
	}
	bead, err := store.Get(id)
	if err != nil {
		t.Fatalf("store.Get(%s): %v", id, err)
	}
	if bead.Metadata["alias"] != "mayor" {
		t.Fatalf("alias = %q, want mayor", bead.Metadata["alias"])
	}
	if bead.Metadata[namedSessionMetadataKey] != "true" {
		t.Fatalf("configured_named_session = %q, want true", bead.Metadata[namedSessionMetadataKey])
	}
}

func TestResolveSessionIDAllowClosedWithConfig_ReservedNamedTargetIgnoresClosedHistoricalBead(t *testing.T) {
	store := beads.NewMemStore()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents:    []config.Agent{{Name: "mayor", MaxActiveSessions: intPtr(1)}},
		NamedSessions: []config.NamedSession{{
			Template: "mayor",
		}},
	}
	b, _ := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"alias":         "sky",
			"alias_history": "mayor",
		},
	})
	_ = store.Close(b.ID)

	_, err := resolveSessionIDAllowClosedWithConfig(filepath.Join(t.TempDir(), "city"), cfg, store, "mayor")
	if !errors.Is(err, session.ErrSessionNotFound) {
		t.Fatalf("resolveSessionIDAllowClosedWithConfig(mayor) error = %v, want session not found", err)
	}
}

func TestResolveSessionIDMaterializingNamed_MaterializesConfiguredNamedSession(t *testing.T) {
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

	id, err := resolveSessionIDMaterializingNamed(t.TempDir(), cfg, store, "mayor")
	if err != nil {
		t.Fatalf("resolveSessionIDMaterializingNamed(mayor): %v", err)
	}
	bead, err := store.Get(id)
	if err != nil {
		t.Fatalf("store.Get(%s): %v", id, err)
	}
	if bead.Metadata["alias"] != "mayor" {
		t.Fatalf("alias = %q, want mayor", bead.Metadata["alias"])
	}
	if bead.Metadata[namedSessionMetadataKey] != "true" {
		t.Fatalf("configured_named_session = %q, want true", bead.Metadata[namedSessionMetadataKey])
	}
}

func TestResolveSessionIDMaterializingNamed_RecreatesClosedConfiguredNamedSession(t *testing.T) {
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
	mgr := session.NewManager(store, runtime.NewFake())
	info, err := mgr.CreateAliasedNamedWithTransportAndMetadata(
		context.Background(),
		"mayor",
		config.NamedSessionRuntimeName(cfg.EffectiveCityName(), cfg.Workspace, "mayor"),
		"mayor",
		"Mayor",
		"true",
		t.TempDir(),
		"shell",
		"",
		nil,
		session.ProviderResume{},
		runtime.Config{},
		map[string]string{
			namedSessionMetadataKey:      "true",
			namedSessionIdentityMetadata: "mayor",
			namedSessionModeMetadata:     "on_demand",
		},
	)
	if err != nil {
		t.Fatalf("CreateAliasedNamedWithTransportAndMetadata: %v", err)
	}
	if err := mgr.Close(info.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}

	id, err := resolveSessionIDMaterializingNamed(t.TempDir(), cfg, store, "mayor")
	if err != nil {
		t.Fatalf("resolveSessionIDMaterializingNamed(mayor): %v", err)
	}
	// The fix reopens the closed bead (preserving bead ID for reference
	// continuity) instead of creating a new one.
	if id != info.ID {
		t.Fatalf("resolveSessionIDMaterializingNamed(mayor) = %q, want %q (should reopen closed bead)", id, info.ID)
	}
	bead, err := store.Get(id)
	if err != nil {
		t.Fatalf("store.Get(%s): %v", id, err)
	}
	// The reopened bead retains its configured_named_identity.
	// Alias and session_name may have been retired during Close();
	// the reconciler re-stamps them on the next tick.
	if got := bead.Metadata[namedSessionIdentityMetadata]; got != "mayor" {
		t.Fatalf("configured_named_identity = %q, want mayor", got)
	}
	if bead.Status != "open" {
		t.Fatalf("status = %q, want open (bead should be reopened)", bead.Status)
	}
}

func TestResolveSessionIDMaterializingNamed_UsesCityUniqueBareNamedTarget(t *testing.T) {
	t.Setenv("GC_SESSION", "fake")

	store := beads.NewMemStore()
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
	}

	id, err := resolveSessionIDMaterializingNamed(t.TempDir(), cfg, store, "witness")
	if err != nil {
		t.Fatalf("resolveSessionIDMaterializingNamed(witness): %v", err)
	}
	bead, err := store.Get(id)
	if err != nil {
		t.Fatalf("store.Get(%s): %v", id, err)
	}
	if bead.Metadata["alias"] != "demo/witness" {
		t.Fatalf("alias = %q, want demo/witness", bead.Metadata["alias"])
	}
}

func TestResolveSessionIDMaterializingNamed_TemplatePrefixBypassesNamedSessionAlias(t *testing.T) {
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
	cityPath := t.TempDir()

	canonicalID, err := resolveSessionIDMaterializingNamed(cityPath, cfg, store, "mayor")
	if err != nil {
		t.Fatalf("resolveSessionIDMaterializingNamed(mayor): %v", err)
	}
	freshID, err := resolveSessionIDMaterializingNamed(cityPath, cfg, store, "template:mayor")
	if err != nil {
		t.Fatalf("resolveSessionIDMaterializingNamed(template:mayor): %v", err)
	}
	if freshID == canonicalID {
		t.Fatalf("template:mayor returned canonical session %q; want fresh session", freshID)
	}
	bead, err := store.Get(freshID)
	if err != nil {
		t.Fatalf("store.Get(%s): %v", freshID, err)
	}
	if bead.Metadata["alias"] != "" {
		t.Fatalf("alias = %q, want empty", bead.Metadata["alias"])
	}
	if bead.Metadata[namedSessionMetadataKey] != "" {
		t.Fatalf("configured_named_session = %q, want empty", bead.Metadata[namedSessionMetadataKey])
	}
}

func TestResolveSessionIDMaterializingNamed_ReusesExistingQualifiedTemplateSession(t *testing.T) {
	store := beads.NewMemStore()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:         "claude",
			Dir:          "gascity",
			StartCommand: "true",
		}},
	}
	existing, err := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession, "template:gascity/claude"},
		Metadata: map[string]string{
			"template":             "gascity/claude",
			"session_name":         "s-gc-existing",
			"state":                "creating",
			"pending_create_claim": "true",
			"manual_session":       "true",
		},
	})
	if err != nil {
		t.Fatalf("create existing session: %v", err)
	}

	id, err := resolveSessionIDMaterializingNamed(t.TempDir(), cfg, store, "gascity/claude")
	if err != nil {
		t.Fatalf("resolveSessionIDMaterializingNamed(gascity/claude): %v", err)
	}
	if id != existing.ID {
		t.Fatalf("got %q, want existing session %q", id, existing.ID)
	}

	all, err := store.ListByLabel(session.LabelSession, 0)
	if err != nil {
		t.Fatalf("ListByLabel: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("session count = %d, want 1", len(all))
	}
}
