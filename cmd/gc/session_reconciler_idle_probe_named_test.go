package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// A named, interactive-resume, assigned-work-only session must never be
// selected as an idle-probe target: beginIdleRespawnDrainIfIdle refuses to
// drain named sessions (they're materialized by the named-session loop, not
// pool respawn), so a probe started on one is never consumed by any clear
// site and permanently blocks all future probes for that session ID via the
// dt.idleProbes[id] != nil guard. This drives two ticks to prove the
// exclusion is steady, not a first-tick coincidence.
func TestSelectIdleProbeTargets_ExcludesNamedAssignedWorkOnly(t *testing.T) {
	info := sessionpkg.Info{
		ID:                     "s1",
		SessionNameMetadata:    "release-operator",
		ConfiguredNamedSession: true,
	}
	target := wakeTarget{info: info, alive: true}
	policy := resolvedSessionSleepPolicy{
		Class:      config.SessionSleepInteractiveResume,
		Effective:  "60s",
		Capability: runtime.SessionSleepCapabilityFull,
	}
	wakeEvals := map[string]wakeEvaluation{
		"s1": {Reason: "assigned-work", Reasons: []WakeReason{WakeWork}, Policy: policy},
	}
	wakeTargets := []wakeTarget{target}
	infoByID := infoByIDForTargets(wakeTargets)
	dt := newDrainTracker()

	first := selectIdleProbeTargets(wakeTargets, wakeEvals, dt, infoByID)
	if first["s1"] {
		t.Fatalf("tick 1: named assigned-work-only session must not be idle-probe-eligible, got %v", first)
	}
	if _, ok := dt.idleProbe("s1"); ok {
		t.Fatal("tick 1: no probe should have been started for a named session")
	}

	second := selectIdleProbeTargets(wakeTargets, wakeEvals, dt, infoByID)
	if second["s1"] {
		t.Fatalf("tick 2: named assigned-work-only session must still not be idle-probe-eligible, got %v", second)
	}
	if _, ok := dt.idleProbe("s1"); ok {
		t.Fatal("tick 2: no probe should have been started for a named session")
	}
}
