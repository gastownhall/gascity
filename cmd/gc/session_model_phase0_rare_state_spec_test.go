package main

import (
	"context"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// Phase 0 spec coverage from engdocs/design/session-model-unification.md:
// - Resume vs fresh rematerialization
// - Reconciler Contract / duplicate canonical repair
// - Config Drift and Restart
// - Status and Diagnostics / degraded health rule

func TestPhase0ConfigDrift_ActiveNamedSessionRestartsInPlaceWithoutCapVacancy(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "worker",
			StartCommand:      "new-cmd",
			MaxActiveSessions: intPtr(1),
		}},
		NamedSessions: []config.NamedSession{{
			Template: "worker",
			Mode:     "always",
		}},
	}

	sessionName := config.NamedSessionRuntimeName(env.cfg.Workspace.Name, env.cfg.Workspace, "worker")
	env.desiredState[sessionName] = TemplateParams{
		TemplateName:            "worker",
		InstanceName:            "worker",
		Alias:                   "worker",
		Command:                 "new-cmd",
		ConfiguredNamedIdentity: "worker",
		ConfiguredNamedMode:     "always",
	}

	oldRuntime := runtime.Config{Command: "old-cmd"}
	if err := env.sp.Start(context.Background(), sessionName, oldRuntime); err != nil {
		t.Fatalf("Start(old runtime): %v", err)
	}

	session := env.createSessionBead(sessionName, "worker")
	env.markSessionActive(&session)
	env.setSessionMetadata(&session, map[string]string{
		namedSessionMetadataKey:      "true",
		namedSessionIdentityMetadata: "worker",
		namedSessionModeMetadata:     "always",
		"session_key":                "old-provider-conversation",
		"started_config_hash":        runtime.CoreFingerprint(oldRuntime),
		"started_live_hash":          runtime.LiveFingerprint(oldRuntime),
	})

	env.reconcile([]beads.Bead{session})

	all, err := env.store.ListByLabel(sessionBeadLabel, 0)
	if err != nil {
		t.Fatalf("ListByLabel(session): %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("session bead count = %d, want same single bead during restart; beads=%v", len(all), all)
	}
	if all[0].ID != session.ID {
		t.Fatalf("restart used bead %q, want in-place restart on %q", all[0].ID, session.ID)
	}
	if all[0].Status != "open" {
		t.Fatalf("status = %q, want open while live restart is in progress", all[0].Status)
	}
	if got := all[0].Metadata["state"]; got != "creating" {
		t.Fatalf("state = %q, want creating for active config-drift restart without cap vacancy", got)
	}
	if got := all[0].Metadata["started_config_hash"]; got != "" {
		t.Fatalf("started_config_hash = %q, want cleared so next start uses fresh config", got)
	}
	if got := all[0].Metadata["continuation_reset_pending"]; got != "true" {
		t.Fatalf("continuation_reset_pending = %q, want true for unified restart path", got)
	}
}

func TestPhase0CanonicalRepair_DuplicateOpenNamedBeadsRetiresLosersNonTerminally(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:         "worker",
			StartCommand: "true",
		}},
		NamedSessions: []config.NamedSession{{
			Template: "worker",
			Mode:     "always",
		}},
	}

	sessionName := config.NamedSessionRuntimeName(env.cfg.Workspace.Name, env.cfg.Workspace, "worker")
	env.desiredState[sessionName] = TemplateParams{
		TemplateName:            "worker",
		InstanceName:            "worker",
		Alias:                   "worker",
		Command:                 "true",
		ConfiguredNamedIdentity: "worker",
		ConfiguredNamedMode:     "always",
	}

	older := env.createSessionBead("worker-older", "worker")
	env.setSessionMetadata(&older, map[string]string{
		namedSessionMetadataKey:      "true",
		namedSessionIdentityMetadata: "worker",
		namedSessionModeMetadata:     "always",
		"alias":                      "worker",
		"generation":                 "1",
		"continuity_eligible":        "true",
	})
	newer := env.createSessionBead(sessionName, "worker")
	env.setSessionMetadata(&newer, map[string]string{
		namedSessionMetadataKey:      "true",
		namedSessionIdentityMetadata: "worker",
		namedSessionModeMetadata:     "always",
		"alias":                      "worker",
		"generation":                 "2",
		"continuity_eligible":        "true",
	})

	env.reconcile([]beads.Bead{older, newer})

	all, err := env.store.ListByLabel(sessionBeadLabel, 0, beads.IncludeClosed)
	if err != nil {
		t.Fatalf("ListByLabel(session, IncludeClosed): %v", err)
	}

	var canonicalIDs []string
	var retiredLoserIDs []string
	for _, b := range all {
		if b.Metadata[namedSessionIdentityMetadata] != "worker" {
			continue
		}
		if b.Status == "closed" {
			t.Fatalf("duplicate canonical repair closed loser %s; want non-terminal retirement", b.ID)
		}
		if b.Status == "open" && !phase0RetiredCanonicalState(b.Metadata["state"]) && b.Metadata["continuity_eligible"] != "false" {
			canonicalIDs = append(canonicalIDs, b.ID)
		}
		if b.Status == "open" && phase0RetiredCanonicalState(b.Metadata["state"]) && b.Metadata["continuity_eligible"] == "false" {
			retiredLoserIDs = append(retiredLoserIDs, b.ID)
		}
	}

	if len(canonicalIDs) != 1 || canonicalIDs[0] != newer.ID {
		t.Fatalf("canonical winners = %v, want exactly newest generation bead %s", canonicalIDs, newer.ID)
	}
	if len(retiredLoserIDs) != 1 || retiredLoserIDs[0] != older.ID {
		t.Fatalf("retired losers = %v, want exactly older generation bead %s retired non-terminally", retiredLoserIDs, older.ID)
	}
}

func TestPhase0StatusText_DegradesAlwaysNamedIdentityBlockedByCitySuspend(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{
			Name:      "test-city",
			Suspended: true,
		},
		Agents: []config.Agent{{
			Name:         "worker",
			StartCommand: "true",
		}},
		NamedSessions: []config.NamedSession{{
			Template: "worker",
			Mode:     "always",
		}},
	}
	sp := runtime.NewFake()
	dops := newDrainOps(sp)

	var stdout strings.Builder
	if code := doCityStatus(sp, dops, cfg, t.TempDir(), &stdout, &strings.Builder{}); code != 0 {
		t.Fatalf("doCityStatus() = %d, want 0", code)
	}
	out := strings.ToLower(stdout.String())
	if !strings.Contains(out, "degraded") || !strings.Contains(out, "worker") || !strings.Contains(out, "blocked") {
		t.Fatalf("status output should mark blocked mode=always named identity as degraded:\n%s", stdout.String())
	}
}

func phase0RetiredCanonicalState(state string) bool {
	switch strings.TrimSpace(state) {
	case "drained", "archived", "orphaned", "suspended":
		return true
	default:
		return false
	}
}
