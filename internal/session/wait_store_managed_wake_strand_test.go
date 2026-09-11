package session

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// TestManagedWakeWouldStrand_FlagsDrainedAndStoppedOnly pins the send-time
// loud-fail classification behind the sc-875uie guard. Only the completed/stopped
// states silently strand a managed nudge-wake (WakeSession accepts them with no
// conflict, yet the reconciler never resumes them). Live/resumable states keep
// the queue-a-wake behavior, and terminal/gone states are deliberately left to
// WakeSession's own conflict/not-found rejection (the enqueue→rollback→dead-letter
// path), so they must report wouldStrand=false here.
func TestManagedWakeWouldStrand_FlagsDrainedAndStoppedOnly(t *testing.T) {
	cases := []struct {
		name            string
		seedID          string // "" => do not seed the store (probe a missing id)
		status          string
		meta            map[string]string
		probeID         string
		wantWouldStrand bool
		wantState       string // asserted only when wantWouldStrand is true
	}{
		{
			name:            "drained-via-sleep-reason strands (the measured sc-875uie case)",
			seedID:          "s-drained-sr",
			status:          "open",
			meta:            map[string]string{"state": "asleep", "sleep_reason": "drained"},
			probeID:         "s-drained-sr",
			wantWouldStrand: true,
			wantState:       "drained",
		},
		{
			name:            "drained-via-state strands",
			seedID:          "s-drained",
			status:          "open",
			meta:            map[string]string{"state": "drained"},
			probeID:         "s-drained",
			wantWouldStrand: true,
			wantState:       "drained",
		},
		{
			name:            "stopped strands",
			seedID:          "s-stopped",
			status:          "open",
			meta:            map[string]string{"state": "stopped"},
			probeID:         "s-stopped",
			wantWouldStrand: true,
			wantState:       "stopped",
		},
		{
			name:            "asleep does not strand (resumable — keep queuing)",
			seedID:          "s-asleep",
			status:          "open",
			meta:            map[string]string{"state": "asleep"},
			probeID:         "s-asleep",
			wantWouldStrand: false,
		},
		{
			name:            "active does not strand",
			seedID:          "s-active",
			status:          "open",
			meta:            map[string]string{"state": "active"},
			probeID:         "s-active",
			wantWouldStrand: false,
		},
		{
			name:            "closed is left to WakeSession's conflict rejection",
			seedID:          "s-closed",
			status:          "closed",
			meta:            map[string]string{"state": "closed"},
			probeID:         "s-closed",
			wantWouldStrand: false,
		},
		{
			name:            "missing bead is left to WakeSession's not-found rejection",
			probeID:         "s-missing",
			wantWouldStrand: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var seeds []beads.Bead
			if tc.seedID != "" {
				seeds = append(seeds, sessionBeadFixture(tc.seedID, tc.status, tc.meta))
			}
			s, _ := recordingWaitStore(t, seeds...)
			state, wouldStrand, err := s.ManagedWakeWouldStrand(tc.probeID, waitStoreNow)
			if err != nil {
				t.Fatalf("ManagedWakeWouldStrand(%q): unexpected error %v", tc.probeID, err)
			}
			if wouldStrand != tc.wantWouldStrand {
				t.Fatalf("ManagedWakeWouldStrand(%q) wouldStrand = %v, want %v (state=%q)", tc.probeID, wouldStrand, tc.wantWouldStrand, state)
			}
			if tc.wantWouldStrand && state != tc.wantState {
				t.Fatalf("ManagedWakeWouldStrand(%q) state = %q, want %q", tc.probeID, state, tc.wantState)
			}
		})
	}
}

// TestManagedWakeWouldStrand_IsReadOnly asserts the probe never mutates the
// session — unlike WakeSession, which stamps wake metadata via SetMetadataBatch.
// Probing a drained session must emit zero writes.
func TestManagedWakeWouldStrand_IsReadOnly(t *testing.T) {
	s, rec := recordingWaitStore(t, sessionBeadFixture("s-drained", "open", map[string]string{"state": "asleep", "sleep_reason": "drained"}))
	if _, _, err := s.ManagedWakeWouldStrand("s-drained", waitStoreNow); err != nil {
		t.Fatalf("ManagedWakeWouldStrand: %v", err)
	}
	if writes := rec.CallsForOp("SetMetadataBatch"); len(writes) != 0 {
		t.Fatalf("ManagedWakeWouldStrand emitted %d SetMetadataBatch writes, want 0 (must be read-only)", len(writes))
	}
}
