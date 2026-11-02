package main

import (
	"path/filepath"
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

// apiCreate is an agent session the API creates (agentSessionCreateMetadata:
// session_origin ephemeral, no pool stamps) with an alias, read between its
// two writes: no session_name yet.
func apiCreate(ago time.Duration) beads.Bead {
	return sessionRow("gc-m", "template", "slowstart", "agent_name", "slowstart/mine", "alias", "mine", "state", "start-pending",
		"session_origin", "ephemeral", "generation", "1", "pending_create_claim", "true",
		"pending_create_started_at", allocNow.Add(-ago).Format(time.RFC3339))
}

// keepCase is one row's expected allocator entry and A10 verdict.
type keepCase struct {
	name         string
	cfg          func() *config.City
	row          beads.Bead
	corpse       bool               // the row's runtime is a dead pane under its name
	tweak        func(*allocInputs) // a further input, a suspended rig
	want         desire
	wantRollback bool
}

func (c keepCase) run(t *testing.T) {
	t.Helper()
	f := newAllocFixture(t, c.cfg()).sessions(c.row)
	if c.corpse {
		f.corpse(c.row.Metadata["session_name"])
	}
	in := f.inputs()
	if c.tweak != nil {
		c.tweak(&in)
	}
	d := mustDecide(t, in)
	e := entryOf(t, d, c.row.ID)
	if e.Desired != c.want || c.want == desireKeep && e.Reason != reasonPendingCreate {
		t.Fatalf("entry %s(%s), want %s; entries %v", e.Desired, e.Reason, c.want, entryIDs(d))
	}
	row, ok := in.Census.Rows[e.Key]
	if !ok {
		t.Fatalf("%s is not in the census", c.row.ID)
	}
	if _, rolled := armRollback(&rowFacts{w: &World{}, row: row, found: true, entry: e}); rolled != c.wantRollback { // a pass that read no work
		t.Fatalf("armRollback = %v, want %v: entry %s(%s) liveness %v", rolled, c.wantRollback, e.Desired, e.Reason, e.Liveness)
	}
}

// TestAllocator_UndesiredPendingCreateKeep reproduces staging J45 (stage-mc,
// v2 main b2ae0533bc, 2026-10-12 00:30:40, gcg-505857): A10 rolled back a
// manual `gc session new` 0.3s after its create, before any start. A pass
// whose census read the row between its two writes found no session name,
// so the overlay (classifyOverlaySession) left it out of desired: Drain,
// and with no name nothing is listed, so gone. armRollback's undesired-and-
// gone arm then rolled back a 0s-old create. Legacy reads the row as not
// desired too, and keeps it while its lease holds (session_reconciler.go:2368;
// SESS-065, SESS-596; its MAINT-051 sweep skips a leased claim); v5.9r makes
// that keep AL1's in a running city, for every row holding the claim: a
// manual create, an API agent create (session_origin ephemeral) and a pool
// create alike.
//
// It also pins what bounds the keep: the never-started deadline (S6), a
// started create's windows, and a dead-pane runtime past the deadline (where
// only the keep's own lease ends it) roll the row back; a suspended agent
// ends the lease at once, while a suspended rig does not (legacy's lease
// reads only the agent); a removed agent's claim keeps only until the
// deadline (SESS-596's asymmetry: the claim path reads an unknown agent as
// leased); a claimless start-pending row (the unbounded start-requested path)
// is not kept; and a lease anchor far in the future (a clock stepped back)
// extends nothing. A desired pending create a hold puts to Sleep is not
// converted: legacy's keep runs only in its not-desired branch.
func TestAllocator_UndesiredPendingCreateKeep(t *testing.T) {
	city := func() *config.City { return diffConfig(diffAgent("slowstart", 1)) }
	multi := func() *config.City { return diffConfig(diffAgent("slowstart", 3)) }
	suspended := func() *config.City {
		cfg := city()
		cfg.Agents[0].Suspended = true
		return cfg
	}
	removed := func() *config.City { return diffConfig(diffAgent("other", 1)) }
	rigPath := filepath.Join(t.TempDir(), "away")
	inRig := func() *config.City {
		cfg := diffConfig()
		a := diffAgent("helper", 1)
		a.Dir = "away"
		cfg.Agents = append(cfg.Agents, a)
		cfg.Rigs = []config.Rig{{Name: "away", Path: rigPath, Suspended: true}}
		return cfg
	}
	rigSuspended := func(in *allocInputs) { in.SuspendedRigPaths = map[string]bool{filepath.Clean(rigPath): true} }
	rigCreate := func(ago time.Duration) beads.Bead {
		return manualCreate(ago, "template", "away/helper", "agent_name", "away/helper")
	}
	// A started create: woken five minutes ago, six after its create, so
	// past its in-flight window and its attempt window.
	started := manualCreate(6*time.Minute, "session_name", "s-gc-m", "state", "creating", "last_woke_at", allocNow.Add(-5*time.Minute).Format(time.RFC3339))
	for _, c := range []keepCase{
		{name: "J45: manual create before its session_name write", cfg: city, row: manualCreate(0), want: desireKeep},
		{name: "manual create once its session_name landed", cfg: city, row: manualCreate(0, "session_name", "s-gc-m"), want: desireWake},
		{name: "manual create past its never-started deadline (S6)", cfg: city, row: manualCreate(11 * time.Minute), want: desireNone, wantRollback: true},
		{name: "API agent create (single-session) before its session_name write", cfg: city, row: apiCreate(0), want: desireKeep},
		{name: "API agent create (multi-session, alias) before its session_name write", cfg: multi, row: apiCreate(0), want: desireKeep},
		{name: "API agent create past its never-started deadline", cfg: multi, row: apiCreate(11 * time.Minute), want: desireNone, wantRollback: true},
		{name: "manual create of a suspended agent", cfg: suspended, row: manualCreate(0), want: desireDrain, wantRollback: true},
		{name: "manual create in a suspended rig, inside its lease (as legacy)", cfg: inRig, tweak: rigSuspended, row: rigCreate(0), want: desireKeep},
		{name: "manual create in a suspended rig, past its lease", cfg: inRig, tweak: rigSuspended, row: rigCreate(11 * time.Minute), want: desireNone, wantRollback: true},
		{name: "manual create of a removed agent, inside its deadline", cfg: removed, row: manualCreate(0), want: desireKeep},
		{name: "manual create of a removed agent, past its deadline", cfg: removed, row: manualCreate(11 * time.Minute), want: desireNone, wantRollback: true},
		{
			name: "removed agent's create past its deadline over a dead pane: the keep's lease ends it", cfg: removed,
			row: manualCreate(11*time.Minute, "session_name", "s-gc-m"), corpse: true, want: desireDrain, wantRollback: true,
		},
		{name: "removed agent's started create past its windows: the keep's lease ends it", cfg: removed, row: started, want: desireDrain, wantRollback: true},
		{name: "manual create stamped two hours ahead (a clock stepped back)", cfg: city, row: manualCreate(-2 * time.Hour), want: desireDrain, wantRollback: true},
		{name: "manual create stamped one minute ahead", cfg: city, row: manualCreate(-time.Minute), want: desireKeep},
		{name: "manual create stamped four minutes ahead (inside the skew bound)", cfg: city, row: manualCreate(-4 * time.Minute), want: desireKeep},
		{name: "manual create stamped six minutes ahead (past the skew bound)", cfg: city, row: manualCreate(-6 * time.Minute), want: desireDrain, wantRollback: true},
		{
			name: "started create woken two hours ahead (a clock stepped back)", cfg: removed,
			row:  manualCreate(time.Minute, "session_name", "s-gc-m", "state", "creating", "last_woke_at", allocNow.Add(2*time.Hour).Format(time.RFC3339)),
			want: desireDrain, wantRollback: true,
		},
		{name: "claimless start-pending row is not kept", cfg: city, row: manualCreate(0, "pending_create_claim", ""), want: desireDrain},
		{
			name: "a held manual create stays Sleep (legacy's keep is the not-desired branch's)", cfg: city,
			row:  manualCreate(0, "session_name", "s-gc-m", "held_until", allocNow.Add(time.Hour).Format(time.RFC3339)),
			want: desireSleep,
		},
		{
			name: "undesired pool pending create, inside its lease (MAINT-051 skips a leased claim)", cfg: city,
			row:  poolRow("gc-m", "slowstart", 1, "creating", "pending_create_claim", "true", "pending_create_started_at", ago(0)),
			want: desireKeep,
		},
		{
			name: "undesired pool pending create, past its deadline (§12.2 row 44)", cfg: city,
			row:  poolRow("gc-m", "slowstart", 1, "creating", "pending_create_claim", "true", "pending_create_started_at", ago(11*time.Minute)),
			want: desireNone, wantRollback: true,
		},
	} {
		t.Run(c.name, c.run)
	}
}

// Kills the suspended city's keep honoring a lease anchor far in the future
// (v5.9r: the skew bound applies in every mode): a pending create whose
// anchor is inside the bound keeps, one past it drains.
func TestAllocator_SuspendedCityKeepRefusesAnAnchorAhead(t *testing.T) {
	for _, tc := range []struct {
		name  string
		ahead time.Duration
		want  desire
	}{
		{"four minutes ahead", 4 * time.Minute, desireKeep},
		{"six minutes ahead", 6 * time.Minute, desireDrain},
		{"two hours ahead", 2 * time.Hour, desireDrain},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.City{Agents: []config.Agent{allocPoolAgent("worker", 3)}}
			f := newAllocFixture(t, cfg).sessions(poolRow("gc-1", "worker", 1, "start-pending", "pending_create_claim", "true",
				"pending_create_started_at", allocNow.Add(tc.ahead).Format(time.RFC3339)))
			f.in.CitySuspended = true
			d := f.decide()
			if e := entryOf(t, d, "gc-1"); e.Desired != tc.want {
				t.Fatalf("entry %s(%s), want %s", e.Desired, e.Reason, tc.want)
			}
		})
	}
}
