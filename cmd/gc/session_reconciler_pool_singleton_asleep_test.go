package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

// A persistent canonical singleton pool (max_active_sessions = 1) whose only session sleeps with a freeable reason must reuse
// that session through the wake path. Skipping it while claimFreshPoolSlotInfo still counts it as the canonical holder left the
// pool stalled on its own runtime name every tick.
func TestReusablePoolSessionInfo_PersistentSingletonAsleepFreeableIsReusable(t *testing.T) {
	t.Parallel()

	one := 1
	singleton := &config.Agent{Name: "ops", Dir: "rig", MaxActiveSessions: &one}
	template := "rig/ops"
	asleep := func(id, reason string) session.Info {
		return session.Info{
			ID:                  id,
			Template:            template,
			SessionNameMetadata: "rig--ops",
			Alias:               "rig/ops",
			PoolManaged:         true,
			MetadataState:       "asleep",
			SleepReason:         reason,
		}
	}

	for _, reason := range []string{"idle", "city-stop"} {
		if got := reusablePoolSessionInfo(&agentBuildParams{}, singleton, template, asleep("s-"+reason, reason), nil); !got {
			t.Fatalf("persistent singleton, asleep, sleep_reason=%s: reusable = false, want true", reason)
		}
	}

	// controls: a deliberate hold, held work, a closed bead and a non-singleton persistent pool keep today's behavior
	if reusablePoolSessionInfo(&agentBuildParams{}, singleton, template, asleep("s-hold", "quarantine"), nil) {
		t.Fatalf("persistent singleton, asleep, sleep_reason=quarantine: reusable = true, want false")
	}
	withWork := &agentBuildParams{assignedWorkBeads: []beads.Bead{{ID: "w", Status: "in_progress", Assignee: "rig/ops"}}}
	if reusablePoolSessionInfo(withWork, singleton, template, asleep("s-work", "idle"), nil) {
		t.Fatalf("persistent singleton, asleep with in-progress work: reusable = true, want false")
	}
	closed := asleep("s-closed", "idle")
	closed.Closed = true
	if reusablePoolSessionInfo(&agentBuildParams{}, singleton, template, closed, nil) {
		t.Fatalf("closed bead: reusable = true, want false (a closed bead frees the slot)")
	}
	many := 3
	pool := &config.Agent{Name: "ops", Dir: "rig", MaxActiveSessions: &many}
	if reusablePoolSessionInfo(&agentBuildParams{}, pool, template, asleep("s-pool", "idle"), nil) {
		t.Fatalf("persistent pool of 3: reusable = true, want false (fresh identity, unchanged)")
	}
}
