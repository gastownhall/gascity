package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// A min=1, max=1 persistent pool whose row was stopped by a restart request
// (restartHandoff: asleep, reset committed, no sleep reason) must be taken
// back by the floor on the next tick, not stall on its own runtime name.
func TestBuildDesiredState_MinFloorPoolRowAfterRestartHandoffIsReused(t *testing.T) {
	store := beads.NewMemStore()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "worker",
			Dir:               "repo",
			StartCommand:      "true",
			ScaleCheck:        "printf 0",
			MinActiveSessions: intPtr(1),
			MaxActiveSessions: intPtr(1),
		}},
	}
	now := time.Date(2026, 10, 30, 5, 0, 0, 0, time.UTC)
	cityPath := t.TempDir()
	sp := runtime.NewFake()

	var stderr bytes.Buffer
	first := buildDesiredState("test-city", cityPath, now, cfg, sp, store, &stderr)
	if len(first.State) != 1 {
		t.Fatalf("tick 1 desired = %d, want 1; stderr=%q", len(first.State), stderr.String())
	}
	rows, err := store.List(beads.ListQuery{Label: sessionBeadLabel})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("tick 1 session beads = %d, want 1", len(rows))
	}
	row := rows[0]

	handoff := restartHandoff("", true, finalizeFacts{Now: now.Add(time.Minute)})
	if err := store.SetMetadataBatch(row.ID, handoff); err != nil {
		t.Fatal(err)
	}

	stderr.Reset()
	second := buildDesiredState("test-city", cityPath, now.Add(2*time.Minute), cfg, sp, store, &stderr)
	if strings.Contains(stderr.String(), "slot stalled on its own runtime name") {
		t.Fatalf("tick 2 stalled on the handed-off row's runtime name: %s", stderr.String())
	}
	after, err := store.List(beads.ListQuery{Label: sessionBeadLabel})
	if err != nil {
		t.Fatal(err)
	}
	open := 0
	for _, b := range after {
		if b.Status != "closed" {
			open++
		}
	}
	if open != 1 {
		t.Fatalf("tick 2 open session beads = %d, want 1 (the handed-off row reused)", open)
	}
	reused := false
	for _, tp := range second.State {
		if tp.Env["GC_SESSION_ID"] == row.ID {
			reused = true
		}
	}
	if !reused {
		t.Fatalf("tick 2 did not keep handed-off row %s desired; desired=%d stderr=%q", row.ID, len(second.State), stderr.String())
	}
}
