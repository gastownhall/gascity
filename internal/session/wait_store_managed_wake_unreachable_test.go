package session

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// TestManagedWakeUnreachable_FlagsTerminalAndStranding pins the drain-time
// dead-letter classification behind the sc-875uie sweep. Unlike the send-time
// ManagedWakeWouldStrand probe (drained/stopped only), the maintenance-time sweep
// runs after the raced session has often already reached closed, so it must flag
// BOTH the terminal wake-conflict states (closed / archived-without-continuity) AND
// the stranding states (drained / stopped) as unreachable — while still leaving a
// genuinely resumable session (asleep / active / archived-with-continuity) alone so
// a queued nudge it will still consume is never dropped.
func TestManagedWakeUnreachable_FlagsTerminalAndStranding(t *testing.T) {
	cases := []struct {
		name            string
		seedID          string // "" => do not seed the store (probe a missing id)
		status          string
		meta            map[string]string
		probeID         string
		wantUnreachable bool
		wantState       string // asserted only when wantUnreachable is true
	}{
		{
			name:            "closed is unreachable (the drained pool worker the reconciler closed)",
			seedID:          "s-closed",
			status:          "closed",
			meta:            map[string]string{"state": "closed"},
			probeID:         "s-closed",
			wantUnreachable: true,
			wantState:       "closed",
		},
		{
			name:            "drained is unreachable (the measured sc-875uie race)",
			seedID:          "s-drained",
			status:          "open",
			meta:            map[string]string{"state": "asleep", "sleep_reason": "drained"},
			probeID:         "s-drained",
			wantUnreachable: true,
			wantState:       "drained",
		},
		{
			name:            "stopped is unreachable",
			seedID:          "s-stopped",
			status:          "open",
			meta:            map[string]string{"state": "stopped"},
			probeID:         "s-stopped",
			wantUnreachable: true,
			wantState:       "stopped",
		},
		{
			name:            "archived-without-continuity is unreachable",
			seedID:          "s-arch-no",
			status:          "open",
			meta:            map[string]string{"state": "archived", "continuity_eligible": "false"},
			probeID:         "s-arch-no",
			wantUnreachable: true,
			wantState:       "archived",
		},
		{
			name:            "archived-with-continuity stays reachable (still resumable)",
			seedID:          "s-arch-yes",
			status:          "open",
			meta:            map[string]string{"state": "archived", "continuity_eligible": "true"},
			probeID:         "s-arch-yes",
			wantUnreachable: false,
		},
		{
			name:            "asleep stays reachable (a queued nudge is still consumed on wake)",
			seedID:          "s-asleep",
			status:          "open",
			meta:            map[string]string{"state": "asleep"},
			probeID:         "s-asleep",
			wantUnreachable: false,
		},
		{
			name:            "active stays reachable",
			seedID:          "s-active",
			status:          "open",
			meta:            map[string]string{"state": "active"},
			probeID:         "s-active",
			wantUnreachable: false,
		},
		{
			name:            "missing bead is not swept (transient-race safe; left to the fence-mismatch path)",
			probeID:         "s-missing",
			wantUnreachable: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var seeds []beads.Bead
			if tc.seedID != "" {
				seeds = append(seeds, sessionBeadFixture(tc.seedID, tc.status, tc.meta))
			}
			s, _ := recordingWaitStore(t, seeds...)
			state, unreachable, err := s.ManagedWakeUnreachable(tc.probeID, waitStoreNow)
			if err != nil {
				t.Fatalf("ManagedWakeUnreachable(%q): unexpected error %v", tc.probeID, err)
			}
			if unreachable != tc.wantUnreachable {
				t.Fatalf("ManagedWakeUnreachable(%q) unreachable = %v, want %v (state=%q)", tc.probeID, unreachable, tc.wantUnreachable, state)
			}
			if tc.wantUnreachable && state != tc.wantState {
				t.Fatalf("ManagedWakeUnreachable(%q) state = %q, want %q", tc.probeID, state, tc.wantState)
			}
		})
	}
}

// TestManagedWakeUnreachable_IsReadOnly asserts the sweep probe never mutates the
// session it classifies — probing a terminal session must emit zero writes.
func TestManagedWakeUnreachable_IsReadOnly(t *testing.T) {
	s, rec := recordingWaitStore(t, sessionBeadFixture("s-closed", "closed", map[string]string{"state": "closed"}))
	if _, _, err := s.ManagedWakeUnreachable("s-closed", waitStoreNow); err != nil {
		t.Fatalf("ManagedWakeUnreachable: %v", err)
	}
	if writes := rec.CallsForOp("SetMetadataBatch"); len(writes) != 0 {
		t.Fatalf("ManagedWakeUnreachable emitted %d SetMetadataBatch writes, want 0 (must be read-only)", len(writes))
	}
}
