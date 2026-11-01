package main

import (
	"io"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// The legacy desired-state build keeps a parked seat desired (so it sleeps,
// never closing as an orphan) while its blocked pin takes no cap headroom:
// with a city cap of one, new demand still gets its seat (ga-ylpbsy.16, D1).
func TestBuildDesiredState_ParkedSeatTakesNoCapHeadroom(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	now := time.Now().UTC()
	parked, err := store.Create(beads.Bead{
		Title: "worker-1", Type: sessionBeadType, Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"template": "worker", "agent_name": "worker-1", "pool_slot": "1", "pool_managed": "true",
			"state": "asleep", "sleep_reason": "idle", "slept_at": now.Add(-time.Hour).Format(time.RFC3339),
			"generation": "2", "instance_token": "tok-2",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetMetadata(parked.ID, "session_name", PoolSessionName("worker", parked.ID)); err != nil {
		t.Fatal(err)
	}
	blocker, err := store.Create(beads.Bead{Title: "review loop", Type: "task", Status: "open"})
	if err != nil {
		t.Fatal(err)
	}
	pinned, err := store.Create(beads.Bead{
		Title: "finalize", Type: "task", Status: "open", Assignee: parked.ID,
		Metadata: map[string]string{"gc.routed_to": "worker"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DepAdd(pinned.ID, blocker.ID, "blocks"); err != nil {
		t.Fatal(err)
	}
	cityMax := 1
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city", MaxActiveSessions: &cityMax},
		Agents:    []config.Agent{{Name: "worker", StartCommand: "true", ScaleCheck: "printf 1"}},
	}

	ds := buildDesiredState("test-city", cityPath, now, cfg, runtime.NewFake(), store, io.Discard)

	if _, ok := ds.State[PoolSessionName("worker", parked.ID)]; !ok {
		t.Fatalf("desired %v, want the parked seat kept desired", desiredNames(ds))
	}
	if len(ds.State) != 2 {
		t.Fatalf("desired %v, want the parked seat and one new seat under a city cap of one", desiredNames(ds))
	}
}

func desiredNames(ds DesiredStateResult) []string {
	var out []string
	for name := range ds.State {
		out = append(out, name)
	}
	return out
}
