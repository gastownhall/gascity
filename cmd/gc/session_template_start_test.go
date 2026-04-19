package main

import (
	"io"
	"path/filepath"
	"strings"
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
	if reopened.Metadata["pending_create_claim"] != "true" {
		t.Fatalf("pending_create_claim = %q, want true", reopened.Metadata["pending_create_claim"])
	}
}

func TestEnsureSessionForTemplate_PoolTemplateWithoutAliasUsesGeneratedWorkDirIdentity(t *testing.T) {
	t.Setenv("GC_SESSION", "fake")

	cityPath := t.TempDir()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs:      []config.Rig{{Name: "demo", Path: filepath.Join(cityPath, "repos", "demo")}},
		Agents: []config.Agent{{
			Name:              "ant",
			Dir:               "demo",
			StartCommand:      "true",
			WorkDir:           ".gc/worktrees/{{.Rig}}/ants/{{.AgentBase}}",
			MinActiveSessions: intPtr(0),
			MaxActiveSessions: intPtr(4),
		}},
	}
	store := beads.NewMemStore()

	firstName, err := ensureSessionForTemplate(cityPath, cfg, store, "demo/ant", io.Discard)
	if err != nil {
		t.Fatalf("ensureSessionForTemplate(first) = %v", err)
	}
	secondName, err := ensureSessionForTemplate(cityPath, cfg, store, "demo/ant", io.Discard)
	if err != nil {
		t.Fatalf("ensureSessionForTemplate(second) = %v", err)
	}
	if firstName == secondName {
		t.Fatalf("session names should be unique, got %q for both", firstName)
	}

	all, err := store.ListByLabel(session.LabelSession, 0)
	if err != nil {
		t.Fatalf("ListByLabel: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("session bead count = %d, want 2", len(all))
	}
	seenWorkDir := make(map[string]bool, len(all))
	for _, bead := range all {
		sessionName := bead.Metadata["session_name"]
		if sessionName == "" {
			t.Fatal("session_name should be populated")
		}
		if got := bead.Metadata["session_name_explicit"]; got != boolMetadata(true) {
			t.Fatalf("session_name_explicit = %q, want %q", got, boolMetadata(true))
		}
		if !strings.HasPrefix(sessionName, "ant-adhoc-") {
			t.Fatalf("session_name = %q, want ant-adhoc-*", sessionName)
		}
		workDir := bead.Metadata["work_dir"]
		if filepath.Dir(workDir) != filepath.Join(cityPath, ".gc", "worktrees", "demo", "ants") {
			t.Fatalf("work_dir(%q) parent = %q, want %q", sessionName, filepath.Dir(workDir), filepath.Join(cityPath, ".gc", "worktrees", "demo", "ants"))
		}
		base := filepath.Base(workDir)
		if base == "ant" {
			t.Fatalf("work_dir(%q) base = %q, want unique generated identity", sessionName, base)
		}
		if !strings.HasPrefix(base, "ant-adhoc-") {
			t.Fatalf("work_dir(%q) base = %q, want ant-adhoc-*", sessionName, base)
		}
		if seenWorkDir[workDir] {
			t.Fatalf("duplicate work_dir %q for aliasless pooled sessions", workDir)
		}
		seenWorkDir[workDir] = true
		if got := bead.Metadata["agent_name"]; got != "demo/"+sessionName {
			t.Fatalf("agent_name(%q) = %q, want %q", sessionName, got, "demo/"+sessionName)
		}
	}
}

func TestEnsureSessionForTemplate_RebrandedSingletonKeepsTemplateWorkDirIdentity(t *testing.T) {
	t.Setenv("GC_SESSION", "fake")

	cityPath := t.TempDir()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs:      []config.Rig{{Name: "demo", Path: filepath.Join(cityPath, "repos", "demo")}},
		Agents: []config.Agent{{
			Name:              "witness",
			Dir:               "demo",
			StartCommand:      "true",
			WorkDir:           ".gc/worktrees/{{.Rig}}/{{.AgentBase}}",
			MaxActiveSessions: intPtr(1),
		}},
		NamedSessions: []config.NamedSession{{
			Name:     "boot",
			Template: "witness",
			Dir:      "demo",
		}},
	}
	store := beads.NewMemStore()

	sessionName, err := ensureSessionForTemplate(cityPath, cfg, store, "demo/boot", io.Discard)
	if err != nil {
		t.Fatalf("ensureSessionForTemplate = %v", err)
	}
	if sessionName == "" {
		t.Fatal("session name should be populated")
	}

	all, err := store.ListByLabel(session.LabelSession, 0)
	if err != nil {
		t.Fatalf("ListByLabel: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("session bead count = %d, want 1", len(all))
	}
	wantWorkDir := filepath.Join(cityPath, ".gc", "worktrees", "demo", "witness")
	if got := all[0].Metadata["work_dir"]; got != wantWorkDir {
		t.Fatalf("work_dir = %q, want %q", got, wantWorkDir)
	}
}
