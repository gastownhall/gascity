package main

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// TestLegacyDoesNotBeginUserHoldDrainOnKeyedOwnedSuspend pins the F4 contract for
// ga-f7v2ft.125: once the suspend patch is durable, the keyed suspend family is
// the sole converger. A legacy tick whose snapshot already carries the suspend
// must not begin its own user-hold drain — that path converges to
// asleep + sleep_reason=user-hold with sleep_intent CLEARED (SleepPatch), a
// different end state than the suspend the CLI reported, and the agent gets a
// Ctrl-C interrupt instead of the keyed token-bound stop.
//
// The production guard is the forward pass's top-of-loop keyed-ownership
// exclusion, not a check at the drain arm: legacyStartExcluded carries
// resolveExactSessionStartOrDrainAckStopOwnership, whose exactUserHoldSuspendCurrent
// arm marks the suspended row, and the excluded row never reaches wakeTargets at
// all. Swapping the predicate for the suspend-blind resolveExactSessionStartOwnership
// makes keyed_active fail, which is what pins the arm as load-bearing.
func TestLegacyDoesNotBeginUserHoldDrainOnKeyedOwnedSuspend(t *testing.T) {
	seed := func(env *reconcilerTestEnv) beads.Bead {
		env.cfg = &config.City{
			Agents:        []config.Agent{{Name: "reviewer", StartCommand: "true"}},
			NamedSessions: []config.NamedSession{{Name: "reviewer", Template: "reviewer", Mode: "on_demand"}},
		}
		env.addDesired("reviewer", "reviewer", true)
		bead := env.createSessionBead("reviewer", "reviewer")
		env.setSessionMetadata(&bead, map[string]string{
			"state":                  string(sessionpkg.StateSuspended),
			"sleep_intent":           "user-hold",
			"held_until":             env.clk.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339),
			"pin_awake":              "true",
			"configured_named":       "true",
			"named_session_identity": "test-city/reviewer",
			"named_session_mode":     "on_demand",
		})
		return bead
	}

	t.Run("keyed_active", func(t *testing.T) {
		env := newReconcilerTestEnv()
		bead := seed(env)
		env.startOptions = append(env.startOptions, withLegacyStartExclusion(func(info sessionpkg.Info) bool {
			return resolveExactSessionStartOrDrainAckStopOwnership(info, env.cfg, env.clk.Now().UTC())
		}))
		env.reconcile([]beads.Bead{bead})
		if drain := env.dt.get(bead.ID); drain != nil {
			t.Fatalf("legacy began a %q drain on a keyed-owned suspend: %+v", drain.reason, drain)
		}
	})

	t.Run("legacy_only", func(t *testing.T) {
		env := newReconcilerTestEnv()
		bead := seed(env)
		env.reconcile([]beads.Bead{bead})
		drain := env.dt.get(bead.ID)
		if drain == nil || drain.reason != "user-hold" {
			t.Fatalf("legacy-only drain = %+v, want the pre-existing user-hold drain", drain)
		}
	})
}
