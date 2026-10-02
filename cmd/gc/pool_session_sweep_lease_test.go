package main

import (
	"context"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// A claimed create that reached provider Start has a last_woke_at. Once its
// attempt and provider-start leases expire, only a missing runtime permits the
// undesired ephemeral pool session to be closed.
func TestSweepUndesiredPoolSessionBeads_ClaimedCreateLeaseSafety(t *testing.T) {
	const sessionName = "worker-bd-stale-create"
	tests := []struct {
		name           string
		state          string
		startedAgo     time.Duration
		lastWokeAgo    time.Duration
		completedAgo   time.Duration
		startupTimeout string
		runtimeState   string
		manual         bool
		nonEphemeral   bool
		configured     bool
		wantClosed     bool
	}{
		{
			name: "old claim with absent runtime releases pool slot", state: "creating",
			startedAgo: 10 * time.Minute, lastWokeAgo: 10 * time.Minute, wantClosed: true,
		},
		{
			name: "old claim with live runtime stays open", state: "creating",
			startedAgo: 10 * time.Minute, lastWokeAgo: 10 * time.Minute, runtimeState: "live",
		},
		{
			name: "old claim with unreadable liveness stays open", state: "creating",
			startedAgo: 10 * time.Minute, lastWokeAgo: 10 * time.Minute, runtimeState: "unavailable",
		},
		{
			name: "attempt inside first minute stays open after provider-start window", state: "asleep",
			startedAgo: 55 * time.Second, lastWokeAgo: 10 * time.Minute, startupTimeout: "1s",
		},
		{
			name: "attempt past first minute is eligible after provider-start window", state: "asleep",
			startedAgo: 65 * time.Second, lastWokeAgo: 10 * time.Minute, startupTimeout: "1s", wantClosed: true,
		},
		{
			name: "provider start still in flight stays open despite old attempt", state: "creating",
			startedAgo: 10 * time.Minute, lastWokeAgo: 60 * time.Second,
		},
		{
			name: "provider-start window expired and old attempt is eligible", state: "creating",
			startedAgo: 10 * time.Minute, lastWokeAgo: 75 * time.Second, wantClosed: true,
		},
		{
			name: "recent completed create stays open after lease expires", state: "active",
			startedAgo: 10 * time.Minute, lastWokeAgo: 10 * time.Minute, completedAgo: 90 * time.Second,
		},
		{
			name: "completed create past protection window is eligible", state: "active",
			startedAgo: 10 * time.Minute, lastWokeAgo: 10 * time.Minute, completedAgo: 150 * time.Second, wantClosed: true,
		},
		{
			name: "manual session stays open", state: "creating",
			startedAgo: 10 * time.Minute, lastWokeAgo: 10 * time.Minute, manual: true,
		},
		{
			name: "unclassified non-ephemeral session stays open", state: "creating",
			startedAgo: 10 * time.Minute, lastWokeAgo: 10 * time.Minute, nonEphemeral: true,
		},
		{
			name: "configured named session stays open", state: "creating",
			startedAgo: 10 * time.Minute, lastWokeAgo: 10 * time.Minute, configured: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			now := time.Now().UTC().Truncate(time.Second)
			metadata := map[string]string{
				"session_name":              sessionName,
				"template":                  "worker",
				"agent_name":                "worker",
				"pool_slot":                 "1",
				poolManagedMetadataKey:      boolMetadata(true),
				"state":                     tt.state,
				"pending_create_claim":      "true",
				"pending_create_started_at": now.Add(-tt.startedAgo).Format(time.RFC3339),
				"last_woke_at":              now.Add(-tt.lastWokeAgo).Format(time.RFC3339),
				"continuation_epoch":        "1",
				"generation":                "1",
			}
			if tt.completedAgo != 0 {
				metadata["creation_complete_at"] = now.Add(-tt.completedAgo).Format(time.RFC3339)
			}
			if tt.manual {
				metadata["session_origin"] = "manual"
				delete(metadata, "pool_slot")
				delete(metadata, poolManagedMetadataKey)
			}
			if tt.nonEphemeral {
				delete(metadata, "pool_slot")
				delete(metadata, poolManagedMetadataKey)
			}
			if tt.configured {
				metadata["configured_named_session"] = "true"
			}

			store := beads.NewMemStore()
			bead, err := store.Create(beads.Bead{
				Title: "worker", Type: sessionBeadType,
				Labels: []string{sessionBeadLabel, "agent:worker"}, Metadata: metadata,
			})
			if err != nil {
				t.Fatalf("Create session bead: %v", err)
			}
			// The per-attempt marker, not the original row creation time, is
			// authoritative when a session is reopened or a create is retried.
			bead.CreatedAt = now.Add(-20 * time.Minute)
			sp := runtime.Provider(runtime.NewFake())
			switch tt.runtimeState {
			case "live":
				fake := runtime.NewFake()
				if err := fake.Start(context.Background(), sessionName, runtime.Config{}); err != nil {
					t.Fatalf("Start runtime: %v", err)
				}
				sp = fake
			case "unavailable":
				sp = &sweepUnavailableLivenessProvider{Fake: runtime.NewFake()}
			}
			cfg := &config.City{Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}}}
			cfg.Session.StartupTimeout = tt.startupTimeout
			closed := sweepUndesiredPoolSessionBeads(
				"", beads.SessionStore{Store: store}, nil,
				newSessionBeadSnapshot([]beads.Bead{bead}), nil, cfg, sp, false,
			)
			got, err := store.Get(bead.ID)
			if err != nil {
				t.Fatalf("Get session bead: %v", err)
			}
			if tt.wantClosed {
				if closed != 1 || got.Status != "closed" {
					t.Fatalf("expired claim with absent runtime: closed=%d status=%q, want 1/closed", closed, got.Status)
				}
			} else if closed != 0 || got.Status == "closed" {
				t.Fatalf("protected session: closed=%d status=%q, want 0/open", closed, got.Status)
			}
		})
	}
}
