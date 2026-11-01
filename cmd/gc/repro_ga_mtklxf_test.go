package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

// TestReproGaMtklxfPersistentSingletonAsleepFreeableIsReusable: a persistent
// pool with max_active_sessions=1 has one canonical identity. When its only
// session sleeps with a freeable reason, claimFreshPoolSlotInfo still counts
// it as the canonical holder, so the asleep bead must be reusable or the pool
// never starts a worker again (gastownhall/gascity#8364).
func TestReproGaMtklxfPersistentSingletonAsleepFreeableIsReusable(t *testing.T) {
	t.Parallel()

	one := 1
	cfgAgent := &config.Agent{Name: "worker", Dir: "rig", MaxActiveSessions: &one}
	if !cfgAgent.UsesCanonicalSingletonPoolIdentity() {
		t.Fatal("precondition: agent must use the canonical singleton pool identity")
	}
	template := "rig/worker"

	for _, reason := range []string{"idle", "city-stop"} {
		info := session.Info{
			ID:                  "session-asleep-" + reason,
			Template:            template,
			SessionNameMetadata: "rig--worker",
			AgentName:           template,
			PoolManaged:         true,
			MetadataState:       "asleep",
			SleepReason:         reason,
		}
		bp := &agentBuildParams{}
		if got := reusablePoolSessionInfo(bp, cfgAgent, template, info, nil); !got {
			t.Errorf("reusablePoolSessionInfo(persistent singleton, asleep, sleep_reason=%s) = false; "+
				"want true — claimFreshPoolSlotInfo still treats this bead as the canonical holder, "+
				"so skipping reuse stalls the pool on every tick", reason)
		}
	}
}
