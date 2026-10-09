package session

import (
	"context"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
)

// wakeRow is an active session row carrying a pending explicit wake.
func wakeRow(t *testing.T, store beads.Store) beads.Bead {
	t.Helper()
	b, err := store.Create(beads.Bead{Title: "worker", Type: BeadType, Labels: []string{LabelSession}, Metadata: map[string]string{
		"session_name":      "s-wake",
		"template":          "worker",
		"provider":          "claude",
		"state":             string(StateActive),
		"wake_request":      string(WakeCauseExplicit),
		"wake_requested_at": "2026-10-08T11:00:00Z",
	}})
	if err != nil {
		t.Fatalf("creating session bead: %v", err)
	}
	return b
}

// TestOperatorSuspendClearsPendingWake is D7 rule 3: `gc session suspend`
// (its fallback and the API both reach Manager.Suspend) supersedes a pending
// wake in its own write; the city stop sweep leaves it for the next start.
func TestOperatorSuspendClearsPendingWake(t *testing.T) {
	for name, tc := range map[string]struct {
		suspend func(m *Manager, id string) error
		cleared bool
	}{
		"operator": {suspend: (*Manager).Suspend, cleared: true},
		"shutdown": {suspend: (*Manager).SuspendForShutdown},
	} {
		t.Run(name, func(t *testing.T) {
			store := beads.NewMemStore()
			sp := runtime.NewFake()
			mgr := NewManagerWithOptions(store, sp)
			b := wakeRow(t, store)
			if err := sp.Start(context.Background(), "s-wake", runtime.Config{}); err != nil {
				t.Fatal(err)
			}
			if err := tc.suspend(mgr, b.ID); err != nil {
				t.Fatalf("suspend: %v", err)
			}
			got, err := store.Get(b.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Metadata["state"] != string(StateSuspended) {
				t.Fatalf("state = %q, want suspended", got.Metadata["state"])
			}
			if cleared := got.Metadata["wake_request"] == "" && got.Metadata["wake_requested_at"] == ""; cleared != tc.cleared {
				t.Fatalf("wake request %q/%q, want cleared=%v", got.Metadata["wake_request"], got.Metadata["wake_requested_at"], tc.cleared)
			}
		})
	}
}
