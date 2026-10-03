package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/nudgequeue"
)

// TestHookClaimContinuationNudgeUsesClaimingSession verifies the production
// enqueue path with the assignee identities emitted by hook claims. The queue
// address remains the assignee, while the fence and poller use the concrete
// session that owns that identity.
func TestHookClaimContinuationNudgeUsesClaimingSession(t *testing.T) {
	tests := []struct {
		name       string
		alias      string
		transport  string
		dispatcher string
		wantPoller bool
	}{
		{name: "alias", alias: "gascity/worker-3", transport: "tmux", wantPoller: true},
		{name: "session bead ID", transport: "tmux", wantPoller: true},
		{name: "supervisor dispatcher", alias: "gascity/worker-3", transport: "tmux", dispatcher: "supervisor"},
		{name: "ACP transport", alias: "gascity/worker-3", transport: "acp"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cityPath := t.TempDir()
			cityTOML := "[workspace]\nname = \"test-city\"\n"
			if tt.dispatcher != "" {
				cityTOML += "[daemon]\nnudge_dispatcher = \"" + tt.dispatcher + "\"\n"
			}
			if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityTOML), 0o644); err != nil {
				t.Fatalf("write city.toml: %v", err)
			}
			t.Setenv("GC_CITY", cityPath)

			store := beads.NewMemStore()
			runtimeName := "test-city--worker-3"
			sessionBead, err := store.Create(beads.Bead{
				Type:   sessionBeadType,
				Status: "open",
				Labels: []string{sessionBeadLabel},
				Metadata: map[string]string{
					"session_name":       runtimeName,
					"alias":              tt.alias,
					"transport":          tt.transport,
					"continuation_epoch": "7",
				},
			})
			if err != nil {
				t.Fatalf("create session bead: %v", err)
			}
			assignee := tt.alias
			if assignee == "" {
				assignee = sessionBead.ID
			}

			previousOpen := openNudgeBeadStore
			openNudgeBeadStore = func(string) beads.NudgesStore {
				return beads.NudgesStore{Store: store}
			}
			t.Cleanup(func() { openNudgeBeadStore = previousOpen })

			type pollerCall struct{ city, key, session string }
			var pollerCalls []pollerCall
			previousStart := startNudgePoller
			startNudgePoller = func(city, key, session string) error {
				pollerCalls = append(pollerCalls, pollerCall{city, key, session})
				return nil
			}
			t.Cleanup(func() { startNudgePoller = previousStart })

			hookContinuationNudgeEnqueue(assignee)

			state, err := nudgequeue.LoadState(cityPath)
			if err != nil {
				t.Fatalf("load nudge queue: %v", err)
			}
			if len(state.Pending) != 1 || len(state.InFlight) != 0 || len(state.Dead) != 0 {
				t.Fatalf("nudge queue pending/in-flight/dead = %d/%d/%d, want 1/0/0", len(state.Pending), len(state.InFlight), len(state.Dead))
			}
			item := state.Pending[0]
			if item.Agent != assignee || item.Source != "hook-claim-continuation" {
				t.Errorf("queued agent/source = %q/%q, want %q/hook-claim-continuation", item.Agent, item.Source, assignee)
			}
			if item.SessionID != sessionBead.ID || item.ContinuationEpoch != "7" {
				t.Errorf("queued fence = %q/%q, want %q/7", item.SessionID, item.ContinuationEpoch, sessionBead.ID)
			}
			if !tt.wantPoller {
				if len(pollerCalls) != 0 {
					t.Errorf("started poller for %s: %+v", tt.name, pollerCalls)
				}
				return
			}
			want := pollerCall{cityPath, sessionBead.ID, runtimeName}
			if len(pollerCalls) != 1 || pollerCalls[0] != want {
				t.Errorf("poller calls = %+v, want [%+v]", pollerCalls, want)
			}
		})
	}
}
