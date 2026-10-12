package main

import (
	"maps"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

// Kills: drain-ack stop-pending, an empty state or failed-create counted as
// unknown; a shared name counted without a pool slot, or a lone slot row; a
// named claimant counted when it is failed-create or its identity is not
// configured; and a fold that is not first-leg-wins, so a migrated copy of one
// row reads as a second claimant of its slot name. These are C4.5 item 1b's
// conditions plus the P3 spec's duplicate named identities (PR L/M).
func TestCountV2SessionMigration(t *testing.T) {
	cfg := chatCity("always")
	cfg.Agents = append(cfg.Agents, allocPoolAgent("worker", 3))
	slotRow := func(id string) beads.Bead {
		return poolRow(id, "worker", 2, "creating", "session_name", "worker-2-pool")
	}
	for _, tc := range []struct {
		name                string
		rows, rig           []beads.Bead
		unknown, slot, dups map[string]int
	}{
		{name: "known states", rows: []beads.Bead{
			poolRow("gc-1", "worker", 1, "active"),
			poolRow("gc-2", "worker", 2, ""),
			poolRow("gc-3", "worker", 3, string(session.StateFailedCreate)),
			poolRow("gc-4", "worker", 1, "draining", "state_reason", session.DrainAckStopPendingReason),
		}},
		{name: "unknown states", rows: []beads.Bead{
			poolRow("gc-1", "worker", 1, "archived"),
			poolRow("gc-2", "worker", 2, "archived"),
			poolRow("gc-3", "worker", 3, "draining"),
		}, unknown: map[string]int{"archived": 2, "draining": 1}},
		{name: "shared slot name", rows: []beads.Bead{
			slotRow("gc-1"), slotRow("gc-2"), slotRow("gc-3"),
			poolRow("gc-4", "worker", 1, "active"),
			sessionRow("gc-5", "template", "worker", "state", "active", "session_name", "shared"),
			sessionRow("gc-6", "template", "worker", "state", "active", "session_name", "shared"),
		}, slot: map[string]int{"worker-2-pool": 3}},
		{name: "migrated copy is one row", rows: []beads.Bead{slotRow("gc-1")}, rig: []beads.Bead{slotRow("gc-1")}},
		{name: "duplicate named", rows: []beads.Bead{
			chatRow("gc-1", "1"), chatRow("gc-2", "2"),
			chatRow("gc-3", "1", "configured_named_identity", "ghost"),
			chatRow("gc-4", "1", "configured_named_identity", "ghost"),
		}, dups: map[string]int{"chat": 2}},
		{name: "failed-create claimant", rows: []beads.Bead{
			chatRow("gc-1", "1"), chatRow("gc-2", "2", "state", string(session.StateFailedCreate)),
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := *cfg
			cityPath := t.TempDir()
			c.Rigs = []config.Rig{{Name: "fixture", Path: t.TempDir()}}
			m, err := readV2SessionMigration(cityPath, "test-city", &c, censusStore(tc.rows...), map[string]beads.Store{"fixture": censusStore(tc.rig...)})
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			for _, got := range []struct {
				what      string
				got, want map[string]int
			}{{"unknown states", m.UnknownStates, tc.unknown}, {"shared slot names", m.SharedSlotNames, tc.slot}, {"duplicate named", m.DuplicateNamed, tc.dups}} {
				if len(got.got)+len(got.want) > 0 && !maps.Equal(got.got, got.want) {
					t.Errorf("%s = %v, want %v", got.what, got.got, got.want)
				}
			}
			if (m.refusal() == nil) != (len(tc.unknown)+len(tc.slot)+len(tc.dups) == 0) {
				t.Errorf("refusal = %v for %+v", m.refusal(), m)
			}
		})
	}
}
