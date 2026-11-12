package main

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

// TestRollbackCloseIsGuarded: TestPendingCreateFailureRollsBack's rollback
// close over a backing whose atomic closer resolves without a test adapter
// on the write's stack (exactBacking over the atomic-close MemStore), so the
// disposition guard judges the close's class keys: they must be minted.
func TestRollbackCloseIsGuarded(t *testing.T) {
	pending := []string{"pending_create_claim", "true", "pending_create_started_at", gatherNow.Add(-time.Minute).Format(time.RFC3339), "wait_hold", "true"}
	row := poolRow("gc-1", "worker", 1, "creating", append([]string{"generation", "3", "instance_token", "tok"}, pending...)...)
	m := beads.NewAtomicCloseMemStore()
	if _, err := m.Create(row); err != nil {
		t.Fatal(err)
	}
	k := newTxKitOn(t, m, exactBacking{m})
	k.it.Kind = intentStart
	k.it.Deadline = gatherNow.Add(70 * time.Second)
	k.p.Runtime = &startHookLeaf{recordingLeaf: k.leaf}
	k.on(seamAfterCall, func() {})
	k.on(seamAfterCall, func() { k.mutate(func(r *simRuntime) { r.zombie = true }) })
	s := k.runKind()
	b, _ := k.backing.Get("gc-1")
	if !s.Closed || b.Status != "closed" || b.Metadata["state"] != string(session.StateFailedCreate) || b.Metadata["wait_hold"] != "" {
		t.Fatalf("settlement %+v, row %s %v; want the rollback close with the Retire cell", s, b.Status, b.Metadata)
	}
}
