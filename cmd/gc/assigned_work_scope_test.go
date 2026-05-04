package main

import (
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

func TestFilterAssignedWorkBeadsForSessionWakeKeepsOnlyReachableAssigneeSources(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "riga")
	cfg := &config.City{
		Rigs: []config.Rig{{Name: "riga", Path: rigPath}},
		Agents: []config.Agent{{
			Name: "worker",
			Dir:  "riga",
		}},
		NamedSessions: []config.NamedSession{{
			Template: "worker",
			Dir:      "riga",
			Mode:     "on_demand",
		}},
	}
	sessions := []beads.Bead{{
		ID:     "session-1",
		Status: "open",
		Type:   sessionBeadType,
		Metadata: map[string]string{
			"template":                  "riga/worker",
			"session_name":              "worker-session",
			"configured_named_identity": "riga/worker",
		},
	}}
	work := []beads.Bead{
		{ID: "city-named", Status: "open", Assignee: "riga/worker"},
		{ID: "rig-named", Status: "open", Assignee: "riga/worker"},
		{ID: "city-session", Status: "in_progress", Assignee: "session-1"},
		{ID: "rig-session", Status: "in_progress", Assignee: "session-1"},
	}
	storeRefs := []string{"", "riga", "", "riga"}

	got := filterAssignedWorkBeadsForSessionWake(cfg, cityPath, sessions, work, storeRefs)

	if len(got) != 2 {
		t.Fatalf("filtered work length = %d, want 2: %#v", len(got), got)
	}
	if got[0].ID != "rig-named" || got[1].ID != "rig-session" {
		t.Fatalf("filtered work IDs = [%s %s], want [rig-named rig-session]", got[0].ID, got[1].ID)
	}
}

func TestFilterAssignedWorkBeadsForPoolDemandKeepsDirectAssigneeAfterTemplateFallback(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{{
			Name: "worker",
		}},
	}
	sessions := []beads.Bead{{
		ID:     "session-1",
		Status: "open",
		Type:   sessionBeadType,
		Metadata: map[string]string{
			"template":     "worker",
			"session_name": "worker-session",
		},
	}}
	work := []beads.Bead{{
		ID:       "direct-assigned",
		Status:   "in_progress",
		Assignee: "session-1",
		Metadata: map[string]string{},
	}}

	got := filterAssignedWorkBeadsForPoolDemand(cfg, "", sessions, work, []string{""})

	if len(got) != 1 || got[0].ID != "direct-assigned" {
		t.Fatalf("filtered work = %#v, want direct-assigned work preserved through template fallback", got)
	}
}

func TestFilterAssignedWorkBeadsForPoolDemandDropsDirectAssigneeFromUnreachableStore(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "riga")
	cfg := &config.City{
		Rigs: []config.Rig{{Name: "riga", Path: rigPath}},
		Agents: []config.Agent{{
			Name: "worker",
		}},
	}
	sessions := []beads.Bead{{
		ID:     "session-1",
		Status: "open",
		Type:   sessionBeadType,
		Metadata: map[string]string{
			"template":     "worker",
			"session_name": "worker-session",
		},
	}}
	work := []beads.Bead{{
		ID:       "rig-direct-assigned",
		Status:   "in_progress",
		Assignee: "session-1",
		Metadata: map[string]string{},
	}}

	got := filterAssignedWorkBeadsForPoolDemand(cfg, cityPath, sessions, work, []string{"riga"})

	if len(got) != 0 {
		t.Fatalf("filtered work = %#v, want unreachable rig-store direct assignment dropped", got)
	}
}

func TestSessionHasOpenAssignedWorkUsesOnlyReachableStore(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "riga")
	cfg := &config.City{
		Rigs: []config.Rig{{Name: "riga", Path: rigPath}},
		Agents: []config.Agent{{
			Name: "worker",
			Dir:  "riga",
		}},
	}
	cityStore := beads.NewMemStore()
	rigStore := beads.NewMemStore()
	session := beads.Bead{
		ID:     "session-1",
		Type:   sessionBeadType,
		Status: "open",
		Metadata: map[string]string{
			"template":     "riga/worker",
			"session_name": "worker-session",
		},
	}
	if _, err := cityStore.Create(beads.Bead{
		ID:       "city-work",
		Type:     "task",
		Status:   "open",
		Assignee: session.ID,
	}); err != nil {
		t.Fatalf("Create city work: %v", err)
	}

	has, err := sessionHasOpenAssignedWorkForReachableStore(cityPath, cfg, cityStore, map[string]beads.Store{"riga": rigStore}, session)
	if err != nil {
		t.Fatalf("sessionHasOpenAssignedWorkForReachableStore: %v", err)
	}
	if has {
		t.Fatal("city-store assigned work should not count for a rig-scoped session")
	}

	if _, err := rigStore.Create(beads.Bead{
		ID:       "rig-work",
		Type:     "task",
		Status:   "open",
		Assignee: session.ID,
	}); err != nil {
		t.Fatalf("Create rig work: %v", err)
	}
	has, err = sessionHasOpenAssignedWorkForReachableStore(cityPath, cfg, cityStore, map[string]beads.Store{"riga": rigStore}, session)
	if err != nil {
		t.Fatalf("sessionHasOpenAssignedWorkForReachableStore: %v", err)
	}
	if !has {
		t.Fatal("rig-store assigned work should count for a rig-scoped session")
	}
}
