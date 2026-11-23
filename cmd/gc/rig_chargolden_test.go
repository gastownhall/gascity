package main

import (
	"testing"
)

// TestRigList_CharacterizationGolden freezes the current per-lane behavior of
// `gc rig list` across the three routing lanes. It is the second command on the
// generalized harness and the first Phase-1 migration candidate.
//
// LANE CONVERGENCE (C6, see PROGRESS.md): rig list HQ Running now agrees across
// all three lanes. renderRigListFromAPI (remote+alive lanes) previously hardcoded
// HQ Running=true; both it and doRigList's serverless lane now read
// rigListHQRunning, a ping on the city's controller socket. With no controller
// in the harness all three lanes render HQ Running=false (summary.running=0) —
// the goldens freeze that convergence. In production a standalone controller and
// the supervisor (for each city it runs) both serve that socket, so the same
// probe returns true on every lane while the city runs. remote and alive still
// match (A==B), and all three now match on HQ Running.
//
// The city has no rigs, isolating the HQ-entry divergence and avoiding the
// tmux/session probe path (rigListSessionProvider is only built when rigs exist).
// The harness redacts the temp cityPath and resets the per-process builtin-import
// warning cache so this config-reading command is deterministic and lane-fair.
func TestRigList_CharacterizationGolden(t *testing.T) {
	h := newCharCity(t, charCityBasic, nil)
	h.runCharGolden(t, charCommand{
		name:  "rig-list",
		route: routeRigList,
		// rig list derives its data from config, not the bead store — no
		// store read-back applies.
	})
}
