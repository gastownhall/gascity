package main

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/readywork"
)

// blockedFirstController is a controller holding one city store with, in
// canonical ready order, an in_progress bead with an open blocker and then a
// workable in_progress bead, both assigned to worker-1.
func blockedFirstController(t *testing.T) (*controllerState, beads.Bead, beads.Bead) {
	t.Helper()
	store := beads.NewMemStore()
	first, second := 1, 2
	gate := mustCreateReadyBead(t, store, beads.Bead{Title: "the gate", Type: "task"})
	blocked := mustCreateReadyBead(t, store, beads.Bead{Title: "blocked step", Type: "task", Priority: &first})
	workable := mustCreateReadyBead(t, store, beads.Bead{Title: "workable step", Type: "task", Priority: &second})
	status, assignee := readywork.StatusInProgress, "worker-1"
	for _, id := range []string{blocked.ID, workable.ID} {
		if err := store.Update(id, beads.UpdateOpts{Status: &status, Assignee: &assignee}); err != nil {
			t.Fatalf("claim %s: %v", id, err)
		}
	}
	if err := store.DepAdd(blocked.ID, gate.ID, "blocks"); err != nil {
		t.Fatalf("adding the blocking dep: %v", err)
	}
	cs := &controllerState{
		cfg:           &config.City{Workspace: config.Workspace{Name: "ready-flag-city"}},
		cityBeadStore: store,
		cityName:      "ready-flag-city",
		cityPath:      t.TempDir(),
	}
	return cs, blocked, workable
}

// TestControllerReadyWorkReadyDropsBlockedRowsBeforeLimit is the controller arm
// of `gc ready --status in_progress --ready --limit=1`: the blocked first row
// does not fill the bound, whether the controller is asked directly or over
// GET /v0/city/{cityName}/work/ready.
func TestControllerReadyWorkReadyDropsBlockedRowsBeforeLimit(t *testing.T) {
	cs, blocked, workable := blockedFirstController(t)
	q := readywork.Query{Status: readywork.StatusInProgress, Assignee: "worker-1", Limit: 1}

	rows, err := cs.ReadyWork(context.Background(), q)
	if err != nil {
		t.Fatalf("ReadyWork: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != blocked.ID {
		t.Fatalf("without Ready: rows = %+v, want the blocked row %s", rows, blocked.ID)
	}

	q.Ready = true
	rows, err = cs.ReadyWork(context.Background(), q)
	if err != nil {
		t.Fatalf("ReadyWork(Ready): %v", err)
	}
	if len(rows) != 1 || rows[0].ID != workable.ID {
		t.Fatalf("with Ready: rows = %+v, want the workable row %s", rows, workable.ID)
	}

	rows, err = readyWorkTestServer(t, cs).ReadyWork(context.Background(), q)
	if err != nil {
		t.Fatalf("ReadyWork over the API: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != workable.ID {
		t.Fatalf("over the API with Ready: rows = %+v, want the workable row %s", rows, workable.ID)
	}
}

// TestReadyReadyFlagRoutesThroughTheController pins the CLI half: `gc ready
// --ready` with a controller up prints the controller's filtered answer.
func TestReadyReadyFlagRoutesThroughTheController(t *testing.T) {
	newReadyCityWithRigs(t)
	cs, _, workable := blockedFirstController(t)
	client := readyWorkTestServer(t, cs)
	prev := readyAPIClient
	readyAPIClient = func(string) (*api.Client, string) { return client, "" }
	t.Cleanup(func() { readyAPIClient = prev })

	var stdout, stderr bytes.Buffer
	q := readywork.Query{Status: readywork.StatusInProgress, Assignee: "worker-1", Limit: 1, Ready: true}
	if code := cmdReady(q, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdReady = %d; stderr=%s", code, stderr.String())
	}
	var rows []readywork.ReadyBead
	if err := json.Unmarshal(stdout.Bytes(), &rows); err != nil {
		t.Fatalf("decoding %q: %v", stdout.String(), err)
	}
	if len(rows) != 1 || rows[0].ID != workable.ID {
		t.Fatalf("routed gc ready --ready printed %s, want only the workable row %s", stdout.String(), workable.ID)
	}
}

// TestGcReadyReadyFlagBindsTheQuery pins the flag the generated federated
// crash-recovery read passes, and that it reaches the reader's query.
func TestGcReadyReadyFlagBindsTheQuery(t *testing.T) {
	var opts readywork.Query
	var includeEphemeral, jsonOut bool
	cmd := newReadyCmd(nil, nil)
	cmd.ResetFlags()
	registerReadyFlags(cmd, &opts, &includeEphemeral, &jsonOut)
	if err := cmd.ParseFlags([]string{"--status", "in_progress", "--ready", "--exclude-label=hold:mayor", "--limit=1"}); err != nil {
		t.Fatalf("parsing the crash-recovery flags: %v", err)
	}
	if !opts.Ready || opts.Status != readywork.StatusInProgress || opts.Limit != 1 {
		t.Fatalf("parsed query = %+v, want Ready with status in_progress and limit 1", opts)
	}
}
