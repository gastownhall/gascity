package main

import (
	"errors"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// A released anchor must not let a stale legacy tick kill the worker between
// releasing its bead and slinging the handoff (ga-70takxcc, #9320). The previous
// bead closed during this incarnation, and neither Q1 nor a claim stamp can
// defer the cycle: the live anchor's owner must decide it.
func freshCycleAnchorOwnerEnv(t *testing.T) (*restartRequestTestEnv, beads.Bead, string, *beads.MemStore, beads.Bead) {
	t.Helper()
	// Pool template and runtime-name owners must be distinct test cases.
	env, session, name := freshCycleReproEnvIn(t, config.Workspace{
		Name: "test-city", SessionTemplate: "{{.City}}-{{.Agent}}",
	}, nil)
	if name == session.Metadata["template"] {
		t.Fatal("fixture runtime name must differ from its template")
	}
	// Production supplies the ready flag with the assigned/open snapshot;
	// omitting it would filter the anchor out before the cycle guard runs.
	env.startOptions = append(env.startOptions, withReadyAssignedFlags([]bool{true}))
	env.setSessionMetadata(&session, map[string]string{
		"state":                                "awake",
		sessionpkg.CurrentBeadIDKey:            "ga-prev",
		"awake_started_at":                     env.clk.Now().Add(-time.Hour).Format(time.RFC3339Nano),
		beadmeta.CurrentClaimBeadIDMetadataKey: "",
	})
	rig := reproMemStore()
	reproCreate(t, rig, beads.Bead{ID: "ga-prev", Title: "completed previous work", Type: "task", Assignee: "witness"})
	reproCloseForReal(t, rig, "ga-prev")
	// Control the close time with the fixture clock so row C cannot hide the
	// missing guard, regardless of the machine's wall clock.
	if err := rig.SetMetadata("ga-prev", "closed_at", env.clk.Now().Add(-time.Minute).Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	reproCreate(t, rig, beads.Bead{ID: "ga-anchor", Title: "handoff in progress", Type: "task", Assignee: "witness"})
	anchor := mustGetBead(t, rig, "ga-anchor")
	return env, session, name, rig, anchor
}

func assertFreshAnchorOwnerRetained(t *testing.T, env *restartRequestTestEnv, before beads.Bead, name string, callsBefore int) {
	t.Helper()
	for _, call := range env.sp.SnapshotCalls()[callsBefore:] {
		switch call.Method {
		case "Stop", "TerminateRuntime", "Start", "Relaunch":
			t.Errorf("runtime %s(%q) after anchor handoff; want no stop or restart", call.Method, call.Name)
		}
	}
	after := mustGetBead(t, env.store, before.ID)
	for _, key := range []string{
		"state", "session_key", sessionpkg.CurrentBeadIDKey,
		beadmeta.CurrentClaimBeadIDMetadataKey, "restart_requested",
		"continuation_reset_pending", sessionpkg.ResetCommittedAtKey,
	} {
		if after.Metadata[key] != before.Metadata[key] {
			t.Errorf("session %s = %q, want %q preserved after anchor handoff", key, after.Metadata[key], before.Metadata[key])
		}
	}
	if !env.sp.IsRunning(name) {
		t.Error("worker stopped before it could finish the handoff")
	}
}

func TestFreshCycleRepro_LiveAnchorOwnershipControlsCycle(t *testing.T) {
	for _, tc := range []struct {
		name, assignee string
		meta           map[string]string
		pool, namepool bool
		cycle          bool
	}{
		{name: "released"},
		{name: "reassigned away", assignee: "another-session"},
		{name: "blank assignee", assignee: " \t "},
		{name: "named identity still owns", assignee: "witness", cycle: true},
		{name: "padded named identity still owns", assignee: "  witness\t", cycle: true},
		{name: "session ID still owns", assignee: "$id", cycle: true},
		{name: "runtime name still owns", assignee: "$name", cycle: true},
		{name: "named identity fallback", assignee: "witness", meta: map[string]string{namedSessionIdentityMetadata: ""}, cycle: true},
		{name: "stable manual alias", assignee: "worker-alias", meta: map[string]string{"alias": "worker-alias"}, cycle: true},
		{name: "pool session ID", assignee: "$id", pool: true, cycle: true},
		{name: "pool runtime name", assignee: "$name", pool: true, cycle: true},
		{name: "stable namepool alias", assignee: "worker-one", pool: true, namepool: true, meta: map[string]string{"alias": "worker-one"}, cycle: true},
		{name: "transient pool slot excluded", assignee: "worker-1", pool: true, meta: map[string]string{"alias": "worker-1"}},
		{name: "pool template excluded", assignee: "witness", pool: true},
		{name: "alias history excluded", assignee: "old-alias", meta: map[string]string{"alias": "worker-alias", "alias_history": "old-alias"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, session, name, rig, anchor := freshCycleAnchorOwnerEnv(t)
			if tc.pool {
				env.cfg.NamedSessions = nil
				env.cfg.Agents[0].MaxActiveSessions = restartRequestTestIntPtr(2)
				env.setSessionMetadata(&session, map[string]string{
					namedSessionMetadataKey: "false", namedSessionIdentityMetadata: "",
					poolManagedMetadataKey: "true", "pool_slot": "1",
				})
				if tc.namepool {
					env.cfg.Agents[0].NamepoolNames = []string{"worker-one", "worker-two"}
				}
			}
			env.setSessionMetadata(&session, tc.meta)
			// The tick saw the anchor owned by this session before the release.
			anchor.Assignee = session.ID
			if err := rig.Update(anchor.ID, beads.UpdateOpts{Assignee: &anchor.Assignee}); err != nil {
				t.Fatal(err)
			}
			rigs := map[string]beads.Store{"gascity": rig}
			sw := testSeatWork(env.city, env.cfg, env.store, rigs)
			sw.once.Do(sw.load)
			assignee := tc.assignee
			switch assignee {
			case "$id":
				assignee = session.ID
			case "$name":
				assignee = name
			}
			if err := rig.Update(anchor.ID, beads.UpdateOpts{Assignee: &assignee}); err != nil {
				t.Fatal(err)
			}
			before := mustGetBead(t, env.store, session.ID)
			callsBefore := len(env.sp.SnapshotCalls())

			reconcileFreshCycleReproWithSeatWork(env, sw, []beads.Bead{session}, []beads.Bead{anchor}, rigs)

			if tc.cycle {
				assertCycled(t, env, session, "a genuinely different open anchor is still owned by this session")
			} else {
				assertFreshAnchorOwnerRetained(t, env, before, name, callsBefore)
			}
		})
	}
}

// A cached owner probe may locate the anchor, but its row cannot authorize a
// kill. Record only the explicit live reader's Get, not the resolver's owner
// probe. Independent status and assignee reads would observe different rows.
type freshCycleAnchorReadStore struct {
	beads.Store
	reads     int
	err       error
	afterRead func()
}

// Handles preserves cached reads and records the authoritative live Get.
func (s *freshCycleAnchorReadStore) Handles() beads.StoreHandles {
	h := beads.HandlesFor(s.Store)
	h.Live = freshCycleAnchorLiveReader{LiveReader: h.Live, probe: s}
	return h
}

// List answers live queries without invalidating a separate by-ID cache. Use the
// existing live handle so Q1 sees the backing release while the owner probe
// still encounters the primed assigned/open row.
func (s *freshCycleAnchorReadStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	if q.Live {
		return beads.LiveFor(s.Store).List(q)
	}
	return s.Store.List(q)
}

type freshCycleAnchorLiveReader struct {
	beads.LiveReader
	probe *freshCycleAnchorReadStore
}

// Get records the live anchor read and then applies the staged backing change.
func (r freshCycleAnchorLiveReader) Get(id string) (beads.Bead, error) {
	if id != "ga-anchor" {
		return r.LiveReader.Get(id)
	}
	s := r.probe
	s.reads++
	if s.err != nil {
		return beads.Bead{}, s.err
	}
	row, err := r.LiveReader.Get(id)
	if s.afterRead != nil {
		f := s.afterRead
		s.afterRead = nil
		f()
	}
	return row, err
}

func TestFreshCycleRepro_AnchorOwnershipUsesOneLiveRowPastPrimedCache(t *testing.T) {
	env, session, name, backing, anchor := freshCycleAnchorOwnerEnv(t)
	cached := beads.NewCachingStore(backing, nil)
	if err := cached.PrimeActive(); err != nil {
		t.Fatal(err)
	}
	probe := &freshCycleAnchorReadStore{Store: cached}
	rigs := map[string]beads.Store{"gascity": probe}
	sw := testSeatWork(env.city, env.cfg, env.store, rigs)
	sw.once.Do(sw.load)
	released := ""
	if err := backing.Update(anchor.ID, beads.UpdateOpts{Assignee: &released}); err != nil {
		t.Fatal(err)
	}
	stale := mustGetBead(t, cached, anchor.ID)
	if stale.Assignee != "witness" || stale.Status != "open" {
		t.Fatalf("fixture cache = %+v; want the stale assigned/open owner row", stale)
	}
	probe.reads = 0
	probe.afterRead = func() {
		// A later state change must not mix with the status of the first row.
		owner := "witness"
		if err := backing.Update(anchor.ID, beads.UpdateOpts{Assignee: &owner}); err != nil {
			t.Fatal(err)
		}
	}
	before := mustGetBead(t, env.store, session.ID)
	callsBefore := len(env.sp.SnapshotCalls())

	reconcileFreshCycleReproWithSeatWork(env, sw, []beads.Bead{session}, []beads.Bead{anchor}, rigs)

	if probe.reads != 1 {
		t.Errorf("authoritative anchor Get calls = %d, want one coherent status/assignee read", probe.reads)
	}
	assertFreshAnchorOwnerRetained(t, env, before, name, callsBefore)
}

// Legacy deliberately falls through to cycling when Q1 has ruled out a live
// claim but Q2 cannot read the anchor. v2's defer-on-error rule is separate.
func TestFreshCycleRepro_LiveAnchorReadErrorStillCycles(t *testing.T) {
	env, session, _, backing, anchor := freshCycleAnchorOwnerEnv(t)
	cached := beads.NewCachingStore(backing, nil)
	if err := cached.PrimeActive(); err != nil {
		t.Fatal(err)
	}
	probe := &freshCycleAnchorReadStore{Store: cached}
	rigs := map[string]beads.Store{"gascity": probe}
	sw := testSeatWork(env.city, env.cfg, env.store, rigs)
	sw.once.Do(sw.load)
	probe.reads = 0
	probe.err = errors.New("anchor live read unavailable")

	reconcileFreshCycleReproWithSeatWork(env, sw, []beads.Bead{session}, []beads.Bead{anchor}, rigs)

	if probe.reads == 0 {
		t.Fatal("the guard never attempted the failing live anchor read")
	}
	assertCycled(t, env, session, "legacy anchor-read errors retain the pre-existing cycle behavior")
}

// The guard's skip decision, apart from the reconciler: closed, released,
// blank and foreign anchors skip the cycle, an anchor the session owns does not
// (padding is not a different owner), and an unreadable anchor never skips.
func TestFreshAnchorCycleSkippable(t *testing.T) {
	ids := []string{"witness", "gc-sess-1"}
	for _, tc := range []struct {
		name     string
		terminal bool
		assignee string
		err      error
		want     bool
	}{
		{name: "closed anchor", terminal: true, assignee: "witness", want: true},
		{name: "released anchor", assignee: "", want: true},
		{name: "blank assignee", assignee: "", want: true},
		{name: "foreign owner", assignee: "another-session", want: true},
		{name: "own identity", assignee: "witness"},
		{name: "own session ID", assignee: "gc-sess-1"},
		{name: "unreadable anchor", err: errors.New("no such bead")},
		{name: "unreadable closed foreign anchor", terminal: true, assignee: "another-session", err: errors.New("no such bead")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := freshAnchorCycleSkippable(tc.terminal, tc.assignee, tc.err, ids); got != tc.want {
				t.Errorf("freshAnchorCycleSkippable(%v, %q, %v) = %v, want %v", tc.terminal, tc.assignee, tc.err, got, tc.want)
			}
		})
	}
}
