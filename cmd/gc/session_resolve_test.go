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

func TestResolveSessionID_DoesNotResolveHistoricalAlias(t *testing.T) {
	store := beads.NewMemStore()
	_, _ = store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"alias":         "sky",
			"alias_history": "mayor",
		},
	})

	_, err := resolveSessionID(store, "mayor")
	if !errors.Is(err, session.ErrSessionNotFound) {
		t.Fatalf("resolveSessionID(historical alias) = %v, want ErrSessionNotFound", err)
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

func TestResolveSessionIDAllowClosed_DoesNotResolveClosedHistoricalAlias(t *testing.T) {
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

	_, err := resolveSessionIDAllowClosed(store, "mayor")
	if !errors.Is(err, session.ErrSessionNotFound) {
		t.Fatalf("resolveSessionIDAllowClosed(historical alias) = %v, want ErrSessionNotFound", err)
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

func TestResolveSessionIDWithConfig_ResolvesQualifiedNamedAlias(t *testing.T) {
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

	id, err := resolveSessionIDWithConfig(filepath.Join(t.TempDir(), "city"), cfg, store, "demo/witness")
	if err != nil {
		t.Fatalf("resolveSessionIDWithConfig(qualified alias): %v", err)
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

func TestResolveSessionIDWithConfig_ReservedNamedTargetConflictsWithLiveAlias(t *testing.T) {
	store := beads.NewMemStore()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents:    []config.Agent{{Name: "mayor", MaxActiveSessions: intPtr(1)}},
		NamedSessions: []config.NamedSession{{
			Template: "mayor",
		}},
	}
	_, _ = store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name": "s-gc-squatter",
			"alias":        "mayor",
		},
	})

	_, err := resolveSessionIDWithConfig(filepath.Join(t.TempDir(), "city"), cfg, store, "mayor")
	if err == nil || !strings.Contains(err.Error(), "configured named session conflict") {
		t.Fatalf("resolveSessionIDWithConfig(mayor) = %v, want configured named session conflict", err)
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

func TestResolveSessionIDMaterializingNamed_AdoptsCanonicalRuntimeSessionNameBead(t *testing.T) {
	store := beads.NewMemStore()
	cityPath := t.TempDir()
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
	spec, ok := findNamedSessionSpec(cfg, config.EffectiveCityName(cfg, filepath.Base(cityPath)), "mayor")
	if !ok {
		t.Fatal("findNamedSessionSpec(mayor) = false")
	}
	sessionName := spec.SessionName
	bead, err := store.Create(beads.Bead{
		Title:  "mayor",
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name": sessionName,
			"template":     "mayor",
			"agent_name":   "mayor",
			"state":        "asleep",
		},
	})
	if err != nil {
		t.Fatalf("store.Create(): %v", err)
	}

	id, err := resolveSessionIDMaterializingNamed(cityPath, cfg, store, "mayor")
	if err != nil {
		t.Fatalf("resolveSessionIDMaterializingNamed(mayor): %v", err)
	}
	if id != bead.ID {
		t.Fatalf("resolved ID = %q, want adopted bead %q", id, bead.ID)
	}
}

func TestResolveSessionIDMaterializingNamed_DoesNotAdoptOrdinaryPoolSessionForSameTemplate(t *testing.T) {
	t.Setenv("GC_SESSION", "fake")

	store := beads.NewMemStore()
	cityPath := t.TempDir()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "claude",
			Dir:               "gascity",
			StartCommand:      "true",
			MaxActiveSessions: intPtr(3),
		}},
		NamedSessions: []config.NamedSession{{
			Template: "claude",
			Dir:      "gascity",
			Mode:     "on_demand",
		}},
	}
	ordinary, err := store.Create(beads.Bead{
		Title:  "ordinary-pool-worker",
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name": "claude-mc-ordinary",
			"template":     "gascity/claude",
			"agent_name":   "gascity/claude",
			"state":        "asleep",
		},
	})
	if err != nil {
		t.Fatalf("store.Create(): %v", err)
	}

	id, err := resolveSessionIDMaterializingNamed(cityPath, cfg, store, "gascity/claude")
	if err != nil {
		t.Fatalf("resolveSessionIDMaterializingNamed(gascity/claude): %v", err)
	}
	if id == ordinary.ID {
		t.Fatalf("resolveSessionIDMaterializingNamed(gascity/claude) adopted ordinary pool worker %q", ordinary.ID)
	}

	named, err := store.Get(id)
	if err != nil {
		t.Fatalf("store.Get(%s): %v", id, err)
	}
	if got := named.Metadata[namedSessionMetadataKey]; got != "true" {
		t.Fatalf("configured_named_session = %q, want true", got)
	}
	if got := named.Metadata["alias"]; got != "gascity/claude" {
		t.Fatalf("alias = %q, want gascity/claude", got)
	}

	preserved, err := store.Get(ordinary.ID)
	if err != nil {
		t.Fatalf("store.Get(%s): %v", ordinary.ID, err)
	}
	if preserved.Status != "open" {
		t.Fatalf("ordinary pool worker status = %q, want open", preserved.Status)
	}
	if got := preserved.Metadata[namedSessionMetadataKey]; got != "" {
		t.Fatalf("ordinary pool worker configured_named_session = %q, want empty", got)
	}
}

func TestResolveSessionIDMaterializingNamed_RuntimeSessionNameWrongTemplateConflicts(t *testing.T) {
	store := beads.NewMemStore()
	cityPath := t.TempDir()
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
	spec, ok := findNamedSessionSpec(cfg, config.EffectiveCityName(cfg, filepath.Base(cityPath)), "mayor")
	if !ok {
		t.Fatal("findNamedSessionSpec(mayor) = false")
	}
	sessionName := spec.SessionName
	other, err := store.Create(beads.Bead{
		Title:  "other",
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name": sessionName,
			"template":     "other",
			"agent_name":   "other",
			"state":        "asleep",
		},
	})
	if err != nil {
		t.Fatalf("store.Create(): %v", err)
	}
	snapshot, err := loadSessionBeadSnapshot(store)
	if err != nil {
		t.Fatalf("loadSessionBeadSnapshot(): %v", err)
	}
	if bead, conflict := findNamedSessionConflict(snapshot, spec); !conflict {
		t.Fatalf("findNamedSessionConflict() = false, want conflict; snapshot=%#v", snapshot.Open())
	} else if bead.Metadata["template"] != "other" {
		t.Fatalf("findNamedSessionConflict() bead template = %q, want other", bead.Metadata["template"])
	}

	id, err := resolveSessionIDMaterializingNamed(cityPath, cfg, store, "mayor")
	if err == nil || !strings.Contains(err.Error(), "conflicts with configured named session") {
		t.Fatalf(
			"resolveSessionIDMaterializingNamed(mayor) = id %q err %v, want configured named session conflict (wrong bead %q)",
			id,
			err,
			other.ID,
		)
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
	// Explicit gc session close retires the canonical identifiers first.
	// Materialization should therefore mint a fresh canonical bead instead
	// of reviving the deliberately retired runtime identity.
	if id == info.ID {
		t.Fatalf("resolveSessionIDMaterializingNamed(mayor) = %q, want fresh bead after explicit close", id)
	}
	bead, err := store.Get(id)
	if err != nil {
		t.Fatalf("store.Get(%s): %v", id, err)
	}
	if got := bead.Metadata[namedSessionIdentityMetadata]; got != "mayor" {
		t.Fatalf("configured_named_identity = %q, want mayor", got)
	}
	if bead.Status != "open" {
		t.Fatalf("status = %q, want open", bead.Status)
	}
}

func TestResolveSessionIDMaterializingNamed_UsesQualifiedNamedTarget(t *testing.T) {
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

	id, err := resolveSessionIDMaterializingNamed(t.TempDir(), cfg, store, "demo/witness")
	if err != nil {
		t.Fatalf("resolveSessionIDMaterializingNamed(demo/witness): %v", err)
	}
	bead, err := store.Get(id)
	if err != nil {
		t.Fatalf("store.Get(%s): %v", id, err)
	}
	if bead.Metadata["alias"] != "demo/witness" {
		t.Fatalf("alias = %q, want demo/witness", bead.Metadata["alias"])
	}
}

func TestResolveSessionIDMaterializingNamed_PrefersReopenableCanonicalClosedBead(t *testing.T) {
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

	retired, err := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			namedSessionMetadataKey:      "true",
			namedSessionIdentityMetadata: "mayor",
			namedSessionModeMetadata:     "on_demand",
		},
	})
	if err != nil {
		t.Fatalf("Create(retired): %v", err)
	}
	if err := store.Close(retired.ID); err != nil {
		t.Fatalf("Close(retired): %v", err)
	}

	sessionName := config.NamedSessionRuntimeName(cfg.EffectiveCityName(), cfg.Workspace, "mayor")
	canonical, err := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name":               sessionName,
			"alias":                      "mayor",
			"close_reason":               "suspended",
			"closed_at":                  "2026-04-04T10:00:00Z",
			namedSessionMetadataKey:      "true",
			namedSessionIdentityMetadata: "mayor",
			namedSessionModeMetadata:     "on_demand",
		},
	})
	if err != nil {
		t.Fatalf("Create(canonical): %v", err)
	}
	if err := store.Close(canonical.ID); err != nil {
		t.Fatalf("Close(canonical): %v", err)
	}

	id, err := resolveSessionIDMaterializingNamed(cityPath, cfg, store, "mayor")
	if err != nil {
		t.Fatalf("resolveSessionIDMaterializingNamed(mayor): %v", err)
	}
	if id != canonical.ID {
		t.Fatalf("resolveSessionIDMaterializingNamed(mayor) = %q, want canonical %q", id, canonical.ID)
	}

	reopened, err := store.Get(canonical.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", canonical.ID, err)
	}
	if reopened.Status != "open" {
		t.Fatalf("status = %q, want open", reopened.Status)
	}
	if reopened.Metadata["close_reason"] != "" {
		t.Fatalf("close_reason = %q, want empty", reopened.Metadata["close_reason"])
	}
}

func TestResolveSessionIDMaterializingNamed_RejectsTemplatePrefixOnSessionSurface(t *testing.T) {
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
	_, err = resolveSessionIDMaterializingNamed(cityPath, cfg, store, "template:mayor")
	if !errors.Is(err, session.ErrSessionNotFound) {
		t.Fatalf("resolveSessionIDMaterializingNamed(template:mayor) = %v, want ErrSessionNotFound", err)
	}
	all, err := store.ListByLabel(session.LabelSession, 0)
	if err != nil {
		t.Fatalf("ListByLabel: %v", err)
	}
	if len(all) != 1 || all[0].ID != canonicalID {
		t.Fatalf("session beads after template: target = %#v, want only canonical %q", all, canonicalID)
	}
}

func TestResolveSessionIDMaterializingNamed_DoesNotResolveQualifiedTemplateSession(t *testing.T) {
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

	_, err = resolveSessionIDMaterializingNamed(t.TempDir(), cfg, store, "gascity/claude")
	if !errors.Is(err, session.ErrSessionNotFound) {
		t.Fatalf("resolveSessionIDMaterializingNamed(gascity/claude) = %v, want ErrSessionNotFound", err)
	}

	all, err := store.ListByLabel(session.LabelSession, 0)
	if err != nil {
		t.Fatalf("ListByLabel: %v", err)
	}
	if len(all) != 1 || all[0].ID != existing.ID {
		t.Fatalf("session beads = %#v, want existing session %q only", all, existing.ID)
	}
}

// Regression test for #423: passing nil stderr to the reopen path must not
// panic. The defensive guard in materializeSessionForTemplateWithOptions
// and reopenClosedConfiguredNamedSessionBead should normalise nil to
// io.Discard.
func TestResolveSessionIDMaterializingNamed_NilStderrDoesNotPanic(t *testing.T) {
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

	sessionName := config.NamedSessionRuntimeName(cfg.EffectiveCityName(), cfg.Workspace, "mayor")
	canonical, err := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name":               sessionName,
			"alias":                      "mayor",
			"close_reason":               "suspended",
			"closed_at":                  "2026-04-04T10:00:00Z",
			namedSessionMetadataKey:      "true",
			namedSessionIdentityMetadata: "mayor",
			namedSessionModeMetadata:     "on_demand",
		},
	})
	if err != nil {
		t.Fatalf("Create(canonical): %v", err)
	}
	if err := store.Close(canonical.ID); err != nil {
		t.Fatalf("Close(canonical): %v", err)
	}

	// Exercise the reopen path — before #423 this would SIGSEGV.
	id, err := resolveSessionIDMaterializingNamed(cityPath, cfg, store, "mayor")
	if err != nil {
		t.Fatalf("resolveSessionIDMaterializingNamed: %v", err)
	}
	if id != canonical.ID {
		t.Fatalf("got %q, want canonical %q", id, canonical.ID)
	}
}
