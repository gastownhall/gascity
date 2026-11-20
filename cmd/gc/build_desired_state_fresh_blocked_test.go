package main

import (
	"io"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// A wake_mode="fresh" pool agent whose asleep session holds an open step that
// is blocked (and so not ready) must not get a fresh session planned for that
// step: the new session cannot claim it, drains, and is respawned every tick
// (gastownhall/gascity#8172).
func TestBuildDesiredState_FreshWakeSkipsBlockedAssignedStep(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	now := time.Now().UTC()
	asleep, err := store.Create(beads.Bead{
		Title: "worker-1", Type: sessionBeadType, Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"template": "worker", "agent_name": "worker-1", "pool_slot": "1", "pool_managed": "true",
			"state": "asleep", "sleep_reason": "drained", "slept_at": now.Add(-time.Hour).Format(time.RFC3339),
			"generation": "2", "instance_token": "tok-2",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetMetadata(asleep.ID, "session_name", PoolSessionName("worker", asleep.ID)); err != nil {
		t.Fatal(err)
	}
	blocker, err := store.Create(beads.Bead{Title: "review", Type: "task", Status: "open"})
	if err != nil {
		t.Fatal(err)
	}
	step, err := store.Create(beads.Bead{
		Title: "finalize", Type: "task", Status: "open", Assignee: asleep.ID,
		Metadata: map[string]string{"gc.routed_to": "worker"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DepAdd(step.ID, blocker.ID, "blocks"); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents:    []config.Agent{{Name: "worker", StartCommand: "true", ScaleCheck: "printf 0", WakeMode: "fresh"}},
	}

	ds := buildDesiredState("test-city", cityPath, now, cfg, runtime.NewFake(), store, io.Discard)

	for name := range ds.State {
		if name != PoolSessionName("worker", asleep.ID) {
			t.Fatalf("desired %v: planned fresh session %q for blocked step %s, want no fresh session", desiredNames(ds), name, step.ID)
		}
	}
}
