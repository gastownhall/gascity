package main

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// manualCreate is a manual pending create of slowstart created ago, with
// meta added. Without a session_name it is the row Manager.CreateSession
// leaves between its two writes (the row, then session_name = s-<id>).
func manualCreate(ago time.Duration, meta ...string) beads.Bead {
	base := []string{
		"template", "slowstart", "agent_name", "slowstart", "state", "start-pending", "session_origin", "manual",
		"generation", "1", "pending_create_claim", "true", "pending_create_started_at", allocNow.Add(-ago).Format(time.RFC3339),
	}
	return sessionRow("gc-m", append(base, meta...)...)
}

// TestAllocator_UndesiredPendingCreateKeep reproduces staging J45 (stage-mc,
// v2 main b2ae0533bc, 2026-10-12 00:30:40, gcg-505857): A10 rolled back a
// manual `gc session new` 0.3s after its create, before any start. A pass
// whose census read the row between its two writes found no session name,
// so the overlay (classifyOverlaySession) left it out of desired: Drain,
// and with no name nothing is listed, so gone. armRollback's undesired-and-
// gone arm then rolled back a 0s-old create. Legacy reads the row as not
// desired too, and keeps it while its lease holds (session_reconciler.go:2368;
// SESS-065, SESS-596); v5.9r makes that keep AL1's in a running city.
//
// It also pins what bounds the keep: the never-started deadline (S6) rolls
// the row back, a suspended agent ends the lease at once, a removed agent's
// claim keeps only until the deadline (SESS-596's asymmetry: the claim path
// reads an unknown agent as leased), a claimless start-pending row (the
// unbounded start-requested path) is not kept, and an undesired pool
// pending create still rolls back (MAINT-051, §12.2 row 44). A desired
// pending create a hold puts to Sleep is not converted: legacy's keep runs
// only in its not-desired branch.
func TestAllocator_UndesiredPendingCreateKeep(t *testing.T) {
	city := func() *config.City { return diffConfig(diffAgent("slowstart", 1)) }
	suspended := func() *config.City {
		cfg := city()
		cfg.Agents[0].Suspended = true
		return cfg
	}
	removed := func() *config.City { return diffConfig(diffAgent("other", 1)) }
	for _, c := range []struct {
		name         string
		cfg          func() *config.City
		row          beads.Bead
		want         desire
		wantRollback bool
	}{
		{name: "J45: manual create before its session_name write", cfg: city, row: manualCreate(0), want: desireKeep},
		{name: "manual create once its session_name landed", cfg: city, row: manualCreate(0, "session_name", "s-gc-m"), want: desireWake},
		{name: "manual create past its never-started deadline (S6)", cfg: city, row: manualCreate(11 * time.Minute), want: desireNone, wantRollback: true},
		{name: "manual create of a suspended agent", cfg: suspended, row: manualCreate(0), want: desireDrain, wantRollback: true},
		{name: "manual create of a removed agent, inside its deadline", cfg: removed, row: manualCreate(0), want: desireKeep},
		{name: "manual create of a removed agent, past its deadline", cfg: removed, row: manualCreate(11 * time.Minute), want: desireNone, wantRollback: true},
		{name: "claimless start-pending row is not kept", cfg: city, row: manualCreate(0, "pending_create_claim", ""), want: desireDrain},
		{
			name: "a held manual create stays Sleep (legacy's keep is the not-desired branch's)", cfg: city,
			row:  manualCreate(0, "session_name", "s-gc-m", "held_until", allocNow.Add(time.Hour).Format(time.RFC3339)),
			want: desireSleep,
		},
		{
			name: "undesired pool pending create (MAINT-051)", cfg: city,
			row:  poolRow("gc-m", "slowstart", 1, "creating", "pending_create_claim", "true", "pending_create_started_at", ago(0)),
			want: desireDrain, wantRollback: true,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			in := newAllocFixture(t, c.cfg()).sessions(c.row).inputs()
			d := mustDecide(t, in)
			e := entryOf(t, d, "gc-m")
			if e.Desired != c.want {
				t.Fatalf("entry %s(%s), want %s; entries %v", e.Desired, e.Reason, c.want, entryIDs(d))
			}
			row, ok := in.Census.Rows[e.Key]
			if !ok {
				t.Fatal("gc-m is not in the census")
			}
			if _, rolled := armRollback(&rowFacts{row: row, found: true, entry: e}); rolled != c.wantRollback {
				t.Fatalf("armRollback = %v, want %v: entry %s(%s) liveness %v", rolled, c.wantRollback, e.Desired, e.Reason, e.Liveness)
			}
		})
	}
}
