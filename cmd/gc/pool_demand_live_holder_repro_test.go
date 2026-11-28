package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// ga-6ab279: a live rig pool session holding an in-progress claim on a
// city-store bead routed to its template must count as pool demand, so a
// queued bead gets a second session instead of being realized onto the
// busy holder.
func TestBuildDesiredState_RigPoolLiveHolderCityStoreClaimCountsAsDemand(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "riga")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatalf("create rig dir: %v", err)
	}
	cityStore := beads.NewMemStore()
	rigStore := beads.NewMemStore()
	holder, err := cityStore.Create(beads.Bead{
		Title:  "working rig worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "template:riga/worker"},
		Metadata: map[string]string{
			"template":     "riga/worker",
			"session_name": "worker-gc-1",
			"state":        "active",
			"pool_managed": "true",
		},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	held, err := cityStore.Create(beads.Bead{
		Title:    "held city work",
		Type:     "task",
		Assignee: holder.ID,
		Metadata: map[string]string{"gc.routed_to": "riga/worker"},
	})
	if err != nil {
		t.Fatalf("create held bead: %v", err)
	}
	if err := cityStore.Update(held.ID, beads.UpdateOpts{Status: stringPtr("in_progress")}); err != nil {
		t.Fatalf("set held in_progress: %v", err)
	}
	if _, err := cityStore.Create(beads.Bead{
		Title:    "queued city work",
		Type:     "task",
		Metadata: map[string]string{"gc.routed_to": "riga/worker"},
	}); err != nil {
		t.Fatalf("create queued bead: %v", err)
	}
	sessionSnapshot, err := loadSessionBeadSnapshot(cityStore)
	if err != nil {
		t.Fatalf("load session snapshot: %v", err)
	}
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "worker-gc-1", runtime.Config{}); err != nil {
		t.Fatalf("start fake holder: %v", err)
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs:      []config.Rig{{Name: "riga", Path: rigPath}},
		Agents: []config.Agent{{
			Name:              "worker",
			Dir:               "riga",
			StartCommand:      "true",
			MaxActiveSessions: intPtr(5),
			ScaleCheck:        printCountCommand(1),
		}},
	}

	dsResult := buildDesiredStateWithSessionBeads(
		"test-city", cityPath, time.Now().UTC(), cfg, sp,
		cityStore, map[string]beads.Store{"riga": rigStore}, sessionSnapshot, nil, io.Discard,
	)
	var names []string
	for name, tp := range dsResult.State {
		if tp.TemplateName == "riga/worker" {
			names = append(names, name)
		}
	}
	if len(names) != 2 {
		t.Fatalf("riga/worker desired sessions = %v (%d), want 2: the busy holder plus a new session for the queued bead", names, len(names))
	}
}
